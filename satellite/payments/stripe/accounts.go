// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package stripe

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stripe/stripe-go/v81"
	"github.com/zeebo/errs"

	"storj.io/common/currency"
	"storj.io/common/storj"
	"storj.io/common/uuid"
	"storj.io/storj/satellite/accounting"
	"storj.io/storj/satellite/payments"
	"storj.io/storj/satellite/payments/coinpayments"
)

const invoiceReferenceCustomFieldName = "Reference"

// ensures that accounts implements payments.Accounts.
var _ payments.Accounts = (*accounts)(nil)

// accounts is an implementation of payments.Accounts.
//
// architecture: Service
type accounts struct {
	service *Service
}

// CreditCards exposes all needed functionality to manage account credit cards.
func (accounts *accounts) CreditCards() payments.CreditCards {
	return &creditCards{service: accounts.service}
}

// PaymentIntents exposes all needed functionality to manage credit cards charging.
func (accounts *accounts) PaymentIntents() payments.PaymentIntents {
	return &paymentIntents{service: accounts.service}
}

// WebhookEvents exposes all needed functionality to handle a stripe webhookEvents event.
func (accounts *accounts) WebhookEvents() payments.WebhookEvents {
	return &webhookEvents{service: accounts.service}
}

// Balances exposes all needed functionality to manage account balances.
func (accounts *accounts) Balances() payments.Balances {
	return &balances{service: accounts.service}
}

// Invoices exposes all needed functionality to manage account invoices.
func (accounts *accounts) Invoices() payments.Invoices {
	return &invoices{service: accounts.service}
}

// Setup creates a payment account for the user.
// If account is already set up it will return nil.
func (accounts *accounts) Setup(ctx context.Context, userID uuid.UUID, email string, signupPromoCode string) (couponType payments.CouponType, err error) {
	defer mon.Task()(&ctx, userID, email)(&err)

	couponType = payments.FreeTierCoupon

	_, err = accounts.service.db.Customers().GetCustomerID(ctx, userID)
	if err == nil {
		return couponType, nil
	}

	params := &stripe.CustomerParams{
		Params: stripe.Params{Context: ctx},
		Email:  stripe.String(email),
	}

	if signupPromoCode == "" {

		params.Coupon = stripe.String(accounts.service.stripeConfig.StripeFreeTierCouponID)

		customer, err := accounts.service.stripeClient.Customers().New(params)
		if err != nil {
			stripeErr := &stripe.Error{}
			if errors.As(err, &stripeErr) {
				err = errs.Wrap(errors.New(stripeErr.Msg))
			}
			return couponType, Error.Wrap(err)
		}

		// TODO: delete customer from stripe, if db insertion fails
		return couponType, Error.Wrap(accounts.service.db.Customers().Insert(ctx, userID, customer.ID))
	}

	promoCodeIter := accounts.service.stripeClient.PromoCodes().List(&stripe.PromotionCodeListParams{
		ListParams: stripe.ListParams{Context: ctx},
		Code:       stripe.String(signupPromoCode),
	})

	var promoCode *stripe.PromotionCode

	if promoCodeIter.Next() {
		promoCode = promoCodeIter.PromotionCode()
	} else {
		couponType = payments.NoCoupon
	}

	// If signup promo code is provided, apply this on account creation.
	// If a free tier coupon is provided with no signup promo code, apply this on account creation.
	if promoCode != nil && promoCode.Coupon != nil {
		params.Coupon = stripe.String(promoCode.Coupon.ID)
		couponType = payments.SignupCoupon
	} else if accounts.service.stripeConfig.StripeFreeTierCouponID != "" {
		params.Coupon = stripe.String(accounts.service.stripeConfig.StripeFreeTierCouponID)
	}

	customer, err := accounts.service.stripeClient.Customers().New(params)
	if err != nil {
		stripeErr := &stripe.Error{}
		if errors.As(err, &stripeErr) {
			err = errs.Wrap(errors.New(stripeErr.Msg))
		}
		return couponType, Error.Wrap(err)
	}

	// TODO: delete customer from stripe, if db insertion fails
	return couponType, Error.Wrap(accounts.service.db.Customers().Insert(ctx, userID, customer.ID))
}

// ShouldSkipMinimumCharge returns true if, for the given user, we should
// not apply a minimum charge (either because they have a positive token balance,
// they have legacy token TXs or because they have an active package plan).
func (accounts *accounts) ShouldSkipMinimumCharge(ctx context.Context, cusID string, userID uuid.UUID) (bool, error) {
	// 1) Check token balance.
	monetaryBalance, err := accounts.service.billingDB.GetBalance(ctx, userID)
	if err != nil {
		return false, err
	}

	// Truncate to cents, since Stripe only invoices at cent precision.
	tokenBalance := currency.AmountFromDecimal(
		monetaryBalance.AsDecimal().Truncate(2),
		currency.USDollars,
	)
	if tokenBalance.BaseUnits() > 0 {
		return true, nil // there is still a positive balance, skip minimum charge.
	}

	// 2) Check if the user has any complete legacy STORJ token transactions.
	txInfos, err := accounts.service.db.Transactions().ListAccount(ctx, userID)
	if err != nil {
		return false, err
	}

	foundLegacyTX := false
	for _, tx := range txInfos {
		if tx.Status == coinpayments.StatusCompleted {
			foundLegacyTX = true
			break
		}
	}
	if foundLegacyTX {
		// If the user has legacy STORJ token transactions, we should skip minimum charge.
		return true, nil
	}

	// 3) Check package plan info.
	packagePlanInfo, purchaseDate, err := accounts.GetPackageInfo(ctx, userID)
	if err != nil {
		return false, err
	}
	// We check for plan expiration one step before creating an invoice so there is no need to do it here again.
	// We should just make sure that service.removeExpiredCredit is set to true.
	if packagePlanInfo != nil && purchaseDate != nil && accounts.service.pricingConfig.MinimumChargeDate != nil {
		if purchaseDate.After(*accounts.service.pricingConfig.MinimumChargeDate) {
			return false, nil // User has a package plan, but it was purchased after the minimum charge date, so we should not skip.
		}

		if cusID == "" {
			cusID, err = accounts.service.db.Customers().GetCustomerID(ctx, userID)
			if err != nil {
				return false, err
			}
		}

		// Stripe returns list ordered by most recent, so ending balance of the first item is current balance.
		list := accounts.service.stripeClient.CustomerBalanceTransactions().List(&stripe.CustomerBalanceTransactionListParams{
			Customer:   stripe.String(cusID),
			ListParams: stripe.ListParams{Context: ctx, Limit: stripe.Int64(1)},
		})

		var hasCredit bool

		for list.Next() {
			tx := list.CustomerBalanceTransaction()
			// The customer's `balance` after the transaction was applied.
			// A negative value decreases the amount due on the customer's next invoice.
			// Which means that if the balance is negative, the customer has credit.
			if tx.EndingBalance < 0 {
				hasCredit = true
				break
			}
		}

		return hasCredit, nil // If the user has purchased a package plan before the minimum charge date, we should skip if they have credit.
	}

	// Otherwise, no reason to skip.
	return false, nil
}

// EnsureUserHasCustomer creates a stripe customer for userID if non exists.
func (accounts *accounts) EnsureUserHasCustomer(ctx context.Context, userID uuid.UUID, email string, signupPromoCode string) (err error) {
	defer mon.Task()(&ctx, userID, email)(&err)

	_, err = accounts.service.db.Customers().GetCustomerID(ctx, userID)
	if err != nil {
		if !errors.Is(err, ErrNoCustomer) {
			return Error.Wrap(err)
		}

		_, err = accounts.Setup(ctx, userID, email, signupPromoCode)
		if err != nil {
			return Error.Wrap(err)
		}
	}

	return nil
}

// ChangeEmail changes a customer's email address.
func (accounts *accounts) ChangeEmail(ctx context.Context, userID uuid.UUID, email string) (err error) {
	defer mon.Task()(&ctx, userID, email)(&err)

	cusID, err := accounts.service.db.Customers().GetCustomerID(ctx, userID)
	if err != nil {
		return Error.Wrap(err)
	}

	params := &stripe.CustomerParams{
		Params: stripe.Params{Context: ctx},
		Email:  stripe.String(email),
	}

	_, err = accounts.service.stripeClient.Customers().Update(cusID, params)
	if err != nil {
		stripeErr := &stripe.Error{}
		if errors.As(err, &stripeErr) {
			err = errs.Wrap(errors.New(stripeErr.Msg))
		}
		return Error.Wrap(err)
	}

	return nil
}

// SaveBillingAddress saves billing address for a user and returns the updated billing information.
func (accounts *accounts) SaveBillingAddress(ctx context.Context, userID uuid.UUID, address payments.BillingAddress) (_ *payments.BillingInformation, err error) {
	defer mon.Task()(&ctx)(&err)

	customerID, err := accounts.service.db.Customers().GetCustomerID(ctx, userID)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	customerParams := &stripe.CustomerParams{
		Params: stripe.Params{
			Context: ctx,
		},
		Name: &address.Name,
		Address: &stripe.AddressParams{
			Line1:      stripe.String(address.Line1),
			Line2:      stripe.String(address.Line2),
			City:       stripe.String(address.City),
			PostalCode: stripe.String(address.PostalCode),
			State:      stripe.String(address.State),
			Country:    stripe.String(string(address.Country.Code)),
		},
	}
	customerParams.AddExpand("tax_ids")

	customer, err := accounts.service.stripeClient.Customers().Update(customerID, customerParams)
	if err != nil {
		stripeErr := &stripe.Error{}
		if errors.As(err, &stripeErr) {
			err = errs.Wrap(errors.New(stripeErr.Msg))
		}
		return nil, Error.Wrap(err)
	}

	return accounts.unpackBillingInformation(*customer)
}

// AddTaxID adds a new tax ID for a user and returns the updated billing information.
func (accounts *accounts) AddTaxID(ctx context.Context, userID uuid.UUID, taxID payments.TaxID) (_ *payments.BillingInformation, err error) {
	defer mon.Task()(&ctx)(&err)

	customerID, err := accounts.service.db.Customers().GetCustomerID(ctx, userID)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	taxIDParams := stripe.TaxIDParams{
		Params: stripe.Params{
			Context: ctx,
		},
		Customer: &customerID,
		Type:     stripe.String(string(taxID.Tax.Code)),
		Value:    &taxID.Value,
	}
	_, err = accounts.service.stripeClient.TaxIDs().New(&taxIDParams)
	if err != nil {
		stripeErr := &stripe.Error{}
		if errors.As(err, &stripeErr) {
			if stripeErr.Code == stripe.ErrorCodeTaxIDInvalid {
				err = Error.Wrap(payments.ErrInvalidTaxID.New("Tax validation error: %s", stripeErr.Msg))
			} else {
				err = errs.Wrap(errors.New(stripeErr.Msg))
			}
		}
		return nil, Error.Wrap(err)
	}

	params := &stripe.CustomerParams{
		Params: stripe.Params{Context: ctx},
	}
	params.AddExpand("tax_ids")
	customer, err := accounts.service.stripeClient.Customers().Get(customerID, params)
	if err != nil {
		return nil, Error.Wrap(err)
	}
	return accounts.unpackBillingInformation(*customer)
}

// AddDefaultInvoiceReference adds a new default invoice reference to be displayed on each invoice and returns the updated billing information.
func (accounts *accounts) AddDefaultInvoiceReference(ctx context.Context, userID uuid.UUID, reference string) (_ *payments.BillingInformation, err error) {
	defer mon.Task()(&ctx)(&err)

	reference = strings.TrimSpace(reference)

	if len(reference) > 140 {
		return nil, Error.New("invoice reference is too long")
	}

	customerID, err := accounts.service.db.Customers().GetCustomerID(ctx, userID)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	customerParams := &stripe.CustomerParams{Params: stripe.Params{Context: ctx}}
	customer, err := accounts.service.stripeClient.Customers().Get(customerID, customerParams)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	customFieldMap := make(map[string]string)
	if customer.InvoiceSettings != nil && customer.InvoiceSettings.CustomFields != nil {
		for _, field := range customer.InvoiceSettings.CustomFields {
			customFieldMap[field.Name] = field.Value
		}
	}

	if reference != "" {
		customFieldMap[invoiceReferenceCustomFieldName] = reference
	} else {
		delete(customFieldMap, invoiceReferenceCustomFieldName)
	}

	// Ensure we don't exceed the custom field limit.
	if len(customFieldMap) > 4 {
		return nil, Error.New("cannot have more than 4 invoice custom fields")
	}

	var customFields []*stripe.CustomerInvoiceSettingsCustomFieldParams
	for name, value := range customFieldMap {
		f := &stripe.CustomerInvoiceSettingsCustomFieldParams{
			Name:  stripe.String(name),
			Value: stripe.String(value),
		}
		customFields = append(customFields, f)
	}

	customerParams.InvoiceSettings = &stripe.CustomerInvoiceSettingsParams{}

	if len(customFields) > 0 {
		customerParams.InvoiceSettings.CustomFields = customFields
	} else {
		// Use AddExtra to clear 'invoice_settings[custom_fields]'.
		customerParams.AddExtra("invoice_settings[custom_fields]", "")
	}

	customerParams.AddExpand("tax_ids")

	customer, err = accounts.service.stripeClient.Customers().Update(customerID, customerParams)
	if err != nil {
		stripeErr := &stripe.Error{}
		if errors.As(err, &stripeErr) {
			err = errs.Wrap(errors.New(stripeErr.Msg))
		}
		return nil, Error.Wrap(err)
	}

	return accounts.unpackBillingInformation(*customer)
}

// RemoveTaxID removes a tax ID from a user and returns the updated billing information.
func (accounts *accounts) RemoveTaxID(ctx context.Context, userID uuid.UUID, id string) (_ *payments.BillingInformation, err error) {
	defer mon.Task()(&ctx)(&err)

	customerID, err := accounts.service.db.Customers().GetCustomerID(ctx, userID)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	_, err = accounts.service.stripeClient.TaxIDs().Del(id, &stripe.TaxIDParams{
		Params: stripe.Params{
			Context: ctx,
		},
		Customer: &customerID,
	})
	if err != nil {
		stripeErr := &stripe.Error{}
		if errors.As(err, &stripeErr) {
			err = errs.Wrap(errors.New(stripeErr.Msg))
		}
		return nil, Error.Wrap(err)
	}

	params := &stripe.CustomerParams{
		Params: stripe.Params{Context: ctx},
	}
	params.AddExpand("tax_ids")
	customer, err := accounts.service.stripeClient.Customers().Get(customerID, params)
	if err != nil {
		return nil, Error.Wrap(err)
	}
	return accounts.unpackBillingInformation(*customer)
}

// GetBillingInformation gets the billing information for a user.
func (accounts *accounts) GetBillingInformation(ctx context.Context, userID uuid.UUID) (info *payments.BillingInformation, err error) {
	defer mon.Task()(&ctx)(&err)

	customerID, err := accounts.service.db.Customers().GetCustomerID(ctx, userID)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	params := &stripe.CustomerParams{
		Params: stripe.Params{Context: ctx},
	}
	params.AddExpand("tax_ids")
	customer, err := accounts.service.stripeClient.Customers().Get(customerID, params)
	if err != nil {
		stripeErr := &stripe.Error{}
		if errors.As(err, &stripeErr) {
			err = errs.Wrap(errors.New(stripeErr.Msg))
		}
		return nil, Error.Wrap(err)
	}

	return accounts.unpackBillingInformation(*customer)
}

func (accounts *accounts) unpackBillingInformation(customer stripe.Customer) (info *payments.BillingInformation, err error) {
	// use customer.address to determine if the customer has custom billing information.
	hasNoAddress := customer.Address == nil || customer.Address == (&stripe.Address{})
	hasNoTaxInfo := customer.TaxIDs == nil || len(customer.TaxIDs.Data) == 0
	hasNoCustomFields := customer.InvoiceSettings == nil || customer.InvoiceSettings.CustomFields == nil

	if hasNoAddress && hasNoTaxInfo && hasNoCustomFields {
		return &payments.BillingInformation{}, nil
	}

	var (
		address   *payments.BillingAddress
		reference string
	)
	taxIDs := make([]payments.TaxID, 0)

	if !hasNoAddress {
		stripeAddr := customer.Address
		countryCode := payments.CountryCode(stripeAddr.Country)
		var country payments.TaxCountry
		for _, taxCountry := range payments.TaxCountries {
			if taxCountry.Code == countryCode {
				country = taxCountry
				break
			}
		}
		if (country == payments.TaxCountry{}) {
			// if country is not found in the list of tax countries, use the country code as the name
			country.Name = stripeAddr.Country
			country.Code = countryCode
		}
		address = &payments.BillingAddress{
			Name:       customer.Name,
			Line1:      stripeAddr.Line1,
			Line2:      stripeAddr.Line2,
			City:       stripeAddr.City,
			PostalCode: stripeAddr.PostalCode,
			State:      stripeAddr.State,
			Country:    country,
		}
	}
	if !hasNoTaxInfo {
		for _, taxID := range customer.TaxIDs.Data {
			var tax payments.Tax
			for _, t := range payments.Taxes {
				if t.Code == taxID.Type {
					tax = t
					break
				}
			}
			taxIDs = append(taxIDs, payments.TaxID{
				ID:    taxID.ID,
				Tax:   tax,
				Value: taxID.Value,
			})
		}
	}
	if !hasNoCustomFields {
		for _, field := range customer.InvoiceSettings.CustomFields {
			if field.Name == invoiceReferenceCustomFieldName {
				reference = field.Value
				break
			}
		}
	}

	return &payments.BillingInformation{
		Address:          address,
		TaxIDs:           taxIDs,
		InvoiceReference: reference,
	}, nil
}

// UpdatePackage updates a customer's package plan information.
func (accounts *accounts) UpdatePackage(ctx context.Context, userID uuid.UUID, packagePlan *string, timestamp *time.Time) (err error) {
	defer mon.Task()(&ctx)(&err)

	_, err = accounts.service.db.Customers().UpdatePackage(ctx, userID, packagePlan, timestamp)
	if err != nil {
		return Error.Wrap(err)
	}

	return nil
}

// GetPackageInfo returns the package plan and time of purchase for a user.
func (accounts *accounts) GetPackageInfo(ctx context.Context, userID uuid.UUID) (packagePlan *string, purchaseTime *time.Time, err error) {
	defer mon.Task()(&ctx)(&err)

	packagePlan, purchaseTime, err = accounts.service.db.Customers().GetPackageInfo(ctx, userID)
	if err != nil {
		return nil, nil, Error.Wrap(err)
	}

	return
}

// ProductCharges returns how much money current user will be charged for each project split by product.
func (accounts *accounts) ProductCharges(ctx context.Context, userID uuid.UUID, since, before time.Time) (charges payments.ProductChargesResponse, err error) {
	defer mon.Task()(&ctx, userID, since, before)(&err)

	charges = make(payments.ProductChargesResponse)

	projects, err := accounts.service.projectsDB.GetOwnActive(ctx, userID)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	for _, project := range projects {
		productUsages := make(map[int32]accounting.ProjectUsage)
		productInfos := make(map[int32]payments.ProductUsagePriceModel)

		err = accounts.service.getAndProcessUsages(ctx, project.ID, productUsages, productInfos, since, before)
		if err != nil {
			return nil, Error.Wrap(err)
		}

		productIDs := getSortedProductIDs(productUsages)

		productCharges := make(map[int32]payments.ProductCharge)

		for _, productID := range productIDs {
			usage := productUsages[productID]
			info := productInfos[productID]

			usage.Egress = applyEgressDiscount(usage, info.ProjectUsagePriceModel)
			price := accounts.service.calculateProjectUsagePrice(usage, info.ProjectUsagePriceModel)

			productCharges[productID] = payments.ProductCharge{
				ProjectUsage: usage,

				ProductUsagePriceModel: info,

				EgressMBCents:       price.Egress.IntPart(),
				SegmentMonthCents:   price.Segments.IntPart(),
				StorageMBMonthCents: price.Storage.IntPart(),
			}
		}

		if len(productCharges) == 0 {
			name := "Global"
			if product, ok := accounts.service.pricingConfig.ProductPriceMap[0]; ok {
				name = product.ProductName
			}

			productCharges[0] = payments.ProductCharge{
				ProjectUsage: accounting.ProjectUsage{Since: since, Before: before},
				ProductUsagePriceModel: payments.ProductUsagePriceModel{
					ProductName:            name,
					ProjectUsagePriceModel: accounts.service.pricingConfig.UsagePrices,
				},
			}
		}

		charges[project.PublicID] = productCharges
	}

	return charges, nil
}

// ProjectCharges returns how much money current user will be charged for each project.
func (accounts *accounts) ProjectCharges(ctx context.Context, userID uuid.UUID, since, before time.Time) (charges payments.ProjectChargesResponse, err error) {
	defer mon.Task()(&ctx, userID, since, before)(&err)

	charges = make(payments.ProjectChargesResponse)

	projects, err := accounts.service.projectsDB.GetOwnActive(ctx, userID)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	for _, project := range projects {
		usages, err := accounts.service.usageDB.GetProjectTotalByPartnerAndPlacement(ctx, project.ID, accounts.service.partnerNames, since, before, false)
		if err != nil {
			return nil, Error.Wrap(err)
		}

		partnerCharges := make(map[string]payments.ProjectCharge)

		for key, usage := range usages {
			if key == "" {
				return nil, Error.New("invalid usage key format")
			}

			// Split the key to extract partner and placement
			parts := strings.Split(key, "|")
			partner := parts[0]

			priceModel := accounts.GetProjectUsagePriceModel(partner)
			usage.Egress = applyEgressDiscount(usage, priceModel)
			price := accounts.service.calculateProjectUsagePrice(usage, priceModel)

			partnerCharges[key] = payments.ProjectCharge{
				ProjectUsage: usage,

				EgressMBCents:       price.Egress.IntPart(),
				SegmentMonthCents:   price.Segments.IntPart(),
				StorageMBMonthCents: price.Storage.IntPart(),
			}
		}

		// to return unpartnered empty charge if there's no usage
		if len(partnerCharges) == 0 {
			partnerCharges[""] = payments.ProjectCharge{
				ProjectUsage: accounting.ProjectUsage{Since: since, Before: before},
			}
		}

		charges[project.PublicID] = partnerCharges
	}

	return charges, nil
}

// GetProjectUsagePriceModel returns the project usage price model for a partner name.
func (accounts *accounts) GetProjectUsagePriceModel(partner string) payments.ProjectUsagePriceModel {
	if override, ok := accounts.service.pricingConfig.UsagePriceOverrides[partner]; ok {
		return override
	}
	return accounts.service.pricingConfig.UsagePrices
}

// GetPartnerPlacementPriceModel returns the productID and related usage price model for a partner and placement.
func (accounts *accounts) GetPartnerPlacementPriceModel(partner string, placement storj.PlacementConstraint) (productID int32, _ payments.ProductUsagePriceModel, _ error) {
	productID, ok := accounts.service.pricingConfig.PartnerPlacementMap.GetProductByPartnerAndPlacement(partner, int(placement))
	if !ok {
		productID, _ = accounts.service.pricingConfig.PlacementProductMap.GetProductByPlacement(int(placement))
	}
	if price, ok := accounts.service.pricingConfig.ProductPriceMap[productID]; ok {
		return productID, price, nil
	}
	return 0, payments.ProductUsagePriceModel{}, ErrPricingNotfound.New("pricing not found for partner %s and placement %d", partner, placement)
}

// GetProductName returns the product name for a given product ID.
func (accounts *accounts) GetProductName(productID int32) (string, error) {
	if price, ok := accounts.service.pricingConfig.ProductPriceMap[productID]; ok {
		return price.ProductName, nil
	}
	return "", ErrPricingNotfound.New("pricing not found for product %d", productID)
}

// GetPartnerNames returns the partners relevant to billing.
func (accounts *accounts) GetPartnerNames() []string {
	return accounts.service.partnerNames
}

// ProductIdAndPriceForUsageKey returns the product ID and usage price model for a given usage key.
func (accounts *accounts) ProductIdAndPriceForUsageKey(key string) (int32, payments.ProductUsagePriceModel) {
	return accounts.service.productIdAndPriceForUsageKey(key)
}

// GetPartnerPlacements returns the placements for a partner. It also includes the
// placements for the default product price config that have not been overridden
// for the partner.
func (accounts *accounts) GetPartnerPlacements(partner string) []storj.PlacementConstraint {
	placements := make([]storj.PlacementConstraint, 0)
	placementMap, ok := accounts.service.pricingConfig.PartnerPlacementMap[partner]
	if !ok {
		placementMap = make(payments.PlacementProductIdMap)
	}
	for i, i2 := range accounts.service.pricingConfig.PlacementProductMap {
		if _, ok = placementMap[i]; !ok {
			placementMap[i] = i2
		}
	}
	for placement := range placementMap {
		placements = append(placements, storj.PlacementConstraint(placement))
	}
	sort.SliceStable(placements, func(i, j int) bool {
		return placements[i] < placements[j]
	})
	return placements
}

// CheckProjectInvoicingStatus returns error if for the given project there are outstanding project records and/or usage
// which have not been applied/invoiced yet (meaning sent over to stripe).
func (accounts *accounts) CheckProjectInvoicingStatus(ctx context.Context, projectID uuid.UUID) (err error) {
	defer mon.Task()(&ctx)(&err)

	year, month, _ := accounts.service.nowFn().UTC().Date()
	firstOfMonth := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)

	// Check if an invoice project record exists already
	err = accounts.service.db.ProjectRecords().Check(ctx, projectID, firstOfMonth.AddDate(0, -1, 0), firstOfMonth)
	if errs.Is(err, ErrProjectRecordExists) {
		record, err := accounts.service.db.ProjectRecords().Get(ctx, projectID, firstOfMonth.AddDate(0, -1, 0), firstOfMonth)
		if err != nil {
			return err
		}
		// state = 0 means unapplied and not invoiced yet.
		if record.State == 0 {
			return errs.New("unapplied project invoice record exist")
		}
		// Record has been applied, so project can be deleted.
		return nil
	}
	if err != nil {
		return err
	}

	return nil
}

// CheckProjectUsageStatus returns error if for the given project there is some usage for current or previous month.
func (accounts *accounts) CheckProjectUsageStatus(ctx context.Context, projectID uuid.UUID) (currentUsageExists, invoicingIncomplete bool, currentMonthPrice decimal.Decimal, err error) {
	defer mon.Task()(&ctx)(&err)

	year, month, _ := accounts.service.nowFn().UTC().Date()
	firstOfMonth := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)

	if accounts.service.config.DeleteProjectCostThreshold == 0 {
		// check current month usage and do not allow deletion if usage exists
		currentUsage, err := accounts.service.usageDB.GetProjectTotal(ctx, projectID, firstOfMonth, accounts.service.nowFn())
		if err != nil {
			return false, false, decimal.Zero, err
		}
		if currentUsage.Storage > 0 || currentUsage.Egress > 0 || currentUsage.SegmentCount > 0 {
			return true, false, decimal.Zero, payments.ErrUnbilledUsageCurrentMonth
		}

		// check usage for last month, if exists, ensure we have an invoice item created.
		lastMonthUsage, err := accounts.service.usageDB.GetProjectTotal(ctx, projectID, firstOfMonth.AddDate(0, -1, 0), firstOfMonth.AddDate(0, 0, -1))
		if err != nil {
			return false, false, decimal.Zero, err
		}
		if lastMonthUsage.Storage > 0 || lastMonthUsage.Egress > 0 || lastMonthUsage.SegmentCount > 0 {
			err = accounts.service.db.ProjectRecords().Check(ctx, projectID, firstOfMonth.AddDate(0, -1, 0), firstOfMonth)
			if !errs.Is(err, ErrProjectRecordExists) {
				return false, true, decimal.Zero, payments.ErrUnbilledUsageLastMonth
			}
		}

		return false, false, decimal.Zero, nil
	}

	getCostTotal := func(start, before time.Time) (decimal.Decimal, error) {
		usages, err := accounts.service.usageDB.GetProjectTotalByPartnerAndPlacement(ctx, projectID, accounts.service.partnerNames, start, before, false)
		if err != nil {
			return decimal.Zero, err
		}

		total := decimal.Zero
		for key, usage := range usages {
			if key == "" {
				return decimal.Zero, errs.New("invalid usage key format")
			}

			_, priceModel := accounts.service.productIdAndPriceForUsageKey(key)
			usage.Egress = applyEgressDiscount(usage, priceModel.ProjectUsagePriceModel)
			price := accounts.service.calculateProjectUsagePrice(usage, priceModel.ProjectUsagePriceModel)

			total = total.Add(price.Total())
		}
		return total, nil
	}

	currentMonthPrice, err = getCostTotal(firstOfMonth, accounts.service.nowFn())
	if err != nil {
		return false, false, decimal.Zero, err
	}

	if currentMonthPrice.GreaterThanOrEqual(decimal.NewFromInt(accounts.service.config.DeleteProjectCostThreshold)) {
		return true, false, currentMonthPrice, payments.ErrUnbilledUsageCurrentMonth
	}

	previousMonthPrice, err := getCostTotal(firstOfMonth.AddDate(0, -1, 0), firstOfMonth.AddDate(0, 0, -1))
	if err != nil {
		return false, false, decimal.Zero, err
	}

	if previousMonthPrice.GreaterThanOrEqual(decimal.NewFromInt(accounts.service.config.DeleteProjectCostThreshold)) {
		err := accounts.service.db.ProjectRecords().Check(ctx, projectID, firstOfMonth.AddDate(0, -1, 0), firstOfMonth)
		switch {
		case errs.Is(err, ErrProjectRecordExists):
			// there’s already a record for last month → nothing to do, fall through.
		case err != nil:
			// some unexpected DB error → propagate it.
			return false, false, currentMonthPrice, err
		default:
			// err == nil → no record exists for last month → unbilled usage.
			return false, true, currentMonthPrice, payments.ErrUnbilledUsageLastMonth
		}
	}

	return false, false, currentMonthPrice, err
}

// Charges returns list of all credit card charges related to account.
func (accounts *accounts) Charges(ctx context.Context, userID uuid.UUID) (_ []payments.Charge, err error) {
	defer mon.Task()(&ctx, userID)(&err)

	customerID, err := accounts.service.db.Customers().GetCustomerID(ctx, userID)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	params := &stripe.ChargeListParams{
		ListParams: stripe.ListParams{Context: ctx},
		Customer:   stripe.String(customerID),
	}
	params.Filters.AddFilter("limit", "", "100")

	iter := accounts.service.stripeClient.Charges().List(params)

	var charges []payments.Charge
	for iter.Next() {
		charge := iter.Charge()

		// ignore all non credit card charges
		if charge.PaymentMethodDetails.Type != stripe.ChargePaymentMethodDetailsTypeCard {
			continue
		}
		if charge.PaymentMethodDetails.Card == nil {
			continue
		}

		charges = append(charges, payments.Charge{
			ID:     charge.ID,
			Amount: charge.Amount,
			CardInfo: payments.CardInfo{
				ID:       charge.PaymentMethod,
				Brand:    string(charge.PaymentMethodDetails.Card.Brand),
				LastFour: charge.PaymentMethodDetails.Card.Last4,
			},
			CreatedAt: time.Unix(charge.Created, 0).UTC(),
		})
	}

	if err = iter.Err(); err != nil {
		return nil, Error.Wrap(err)
	}

	return charges, nil
}

// StorjTokens exposes all storj token related functionality.
func (accounts *accounts) StorjTokens() payments.StorjTokens {
	return &storjTokens{service: accounts.service}
}

// Coupons exposes all needed functionality to manage coupons.
func (accounts *accounts) Coupons() payments.Coupons {
	return &coupons{service: accounts.service}
}
