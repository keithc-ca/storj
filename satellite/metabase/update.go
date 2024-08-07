// Copyright (C) 2020 Storj Labs, Inc.
// See LICENSE for copying information.

package metabase

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"cloud.google.com/go/spanner"
	"github.com/zeebo/errs"
	"google.golang.org/api/iterator"

	"storj.io/common/storj"
	"storj.io/common/uuid"
	"storj.io/storj/shared/dbutil/spannerutil"
)

const (
	noLockWithExpirationErrMsg = "Object Lock settings must not be placed on an object with an expiration date"
	noLockOnUncommittedErrMsg  = "Object Lock settings must only be placed on committed objects"
	noShortenRetentionErrMsg   = "retention period cannot be shortened"
	noRemoveRetentionErrMsg    = "an active retention configuration cannot be removed"
)

var (
	// ErrValueChanged is returned when the current value of the key does not match the oldValue in UpdateSegmentPieces.
	ErrValueChanged = errs.Class("value changed")
	// ErrObjectExpiration is used when an object's expiration prevents an operation from succeeding.
	ErrObjectExpiration = errs.Class("object expiration")
	// ErrObjectStatus is used when an object's status prevents an operation from succeeding.
	ErrObjectStatus = errs.Class("object status")
)

// UpdateSegmentPieces contains arguments necessary for updating segment pieces.
type UpdateSegmentPieces struct {
	// Name of the database adapter to use for this segment. If empty (""), check all adapters
	// until the segment is found.
	DBAdapterName string

	StreamID uuid.UUID
	Position SegmentPosition

	OldPieces Pieces

	NewRedundancy storj.RedundancyScheme
	NewPieces     Pieces

	NewRepairedAt time.Time // sets new time of last segment repair (optional).
}

// UpdateSegmentPieces updates pieces for specified segment. If provided old pieces
// won't match current database state update will fail.
func (db *DB) UpdateSegmentPieces(ctx context.Context, opts UpdateSegmentPieces) (err error) {
	defer mon.Task()(&ctx)(&err)

	if opts.StreamID.IsZero() {
		return ErrInvalidRequest.New("StreamID missing")
	}

	if err := opts.OldPieces.Verify(); err != nil {
		if ErrInvalidRequest.Has(err) {
			return ErrInvalidRequest.New("OldPieces: %v", errs.Unwrap(err))
		}
		return err
	}

	if opts.NewRedundancy.IsZero() {
		return ErrInvalidRequest.New("NewRedundancy zero")
	}

	// its possible that in this method we will have less pieces
	// than optimal shares (e.g. after repair)
	if len(opts.NewPieces) < int(opts.NewRedundancy.RepairShares) {
		return ErrInvalidRequest.New("number of new pieces is less than new redundancy repair shares value")
	}

	if err := opts.NewPieces.Verify(); err != nil {
		if ErrInvalidRequest.Has(err) {
			return ErrInvalidRequest.New("NewPieces: %v", errs.Unwrap(err))
		}
		return err
	}

	oldPieces, err := db.aliasCache.EnsurePiecesToAliases(ctx, opts.OldPieces)
	if err != nil {
		return Error.New("unable to convert pieces to aliases: %w", err)
	}

	newPieces, err := db.aliasCache.EnsurePiecesToAliases(ctx, opts.NewPieces)
	if err != nil {
		return Error.New("unable to convert pieces to aliases: %w", err)
	}

	var resultPieces AliasPieces
	for _, adapter := range db.adapters {
		if opts.DBAdapterName == "" || opts.DBAdapterName == adapter.Name() {
			resultPieces, err = adapter.UpdateSegmentPieces(ctx, opts, oldPieces, newPieces)
			if err != nil {
				if ErrSegmentNotFound.Has(err) {
					continue
				}
				return err
			}
			// segment was found
			break
		}
	}
	if resultPieces == nil {
		return ErrSegmentNotFound.New("segment missing")
	}

	if !EqualAliasPieces(newPieces, resultPieces) {
		return ErrValueChanged.New("segment remote_alias_pieces field was changed")
	}

	mon.Meter("segment_update").Mark(1)

	return nil
}

// UpdateSegmentPieces updates pieces for specified segment, if pieces matches oldPieces.
func (p *PostgresAdapter) UpdateSegmentPieces(ctx context.Context, opts UpdateSegmentPieces, oldPieces, newPieces AliasPieces) (resultPieces AliasPieces, err error) {
	updateRepairAt := !opts.NewRepairedAt.IsZero()

	err = p.db.QueryRowContext(ctx, `
		UPDATE segments SET
			remote_alias_pieces = CASE
				WHEN remote_alias_pieces = $3 THEN $4
				ELSE remote_alias_pieces
			END,
			redundancy = CASE
				WHEN remote_alias_pieces = $3 THEN $5
				ELSE redundancy
			END,
			repaired_at = CASE
				WHEN remote_alias_pieces = $3 AND $7 = true THEN $6
				ELSE repaired_at
			END
		WHERE
			stream_id     = $1 AND
			position      = $2
		RETURNING remote_alias_pieces
		`, opts.StreamID, opts.Position, oldPieces, newPieces, redundancyScheme{&opts.NewRedundancy}, opts.NewRepairedAt, updateRepairAt).
		Scan(&resultPieces)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSegmentNotFound.New("segment missing")
		}
		return nil, Error.New("unable to update segment pieces: %w", err)
	}
	return resultPieces, nil
}

// UpdateSegmentPieces updates pieces for specified segment, if pieces matches oldPieces.
func (s *SpannerAdapter) UpdateSegmentPieces(ctx context.Context, opts UpdateSegmentPieces, oldPieces, newPieces AliasPieces) (resultPieces AliasPieces, err error) {
	updateRepairAt := !opts.NewRepairedAt.IsZero()

	_, err = s.client.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		resultPieces, err = spannerutil.CollectRow(tx.Query(ctx, spanner.Statement{
			SQL: `
				UPDATE segments SET
					remote_alias_pieces = CASE
						WHEN remote_alias_pieces = @old_pieces THEN @new_pieces
						ELSE remote_alias_pieces
					END,
					redundancy = CASE
						WHEN remote_alias_pieces = @old_pieces THEN @redundancy
						ELSE redundancy
					END,
					repaired_at = CASE
						WHEN remote_alias_pieces = @old_pieces AND @update_repaired_at = true THEN @new_repaired_at
						ELSE repaired_at
					END
				WHERE
					stream_id     = @stream_id AND
					position      = @position
				THEN RETURN remote_alias_pieces
			`,
			Params: map[string]any{
				"stream_id":          opts.StreamID,
				"position":           opts.Position,
				"old_pieces":         oldPieces,
				"new_pieces":         newPieces,
				"redundancy":         redundancyScheme{&opts.NewRedundancy},
				"new_repaired_at":    opts.NewRepairedAt,
				"update_repaired_at": updateRepairAt,
			},
		}), func(row *spanner.Row, item *AliasPieces) error {
			err = row.Columns(item)
			if err != nil {
				return Error.New("unable to decode result pieces: %w", err)
			}
			return nil
		})

		if err != nil {
			if errors.Is(err, iterator.Done) {
				return ErrSegmentNotFound.New("segment missing")
			}
			return Error.New("unable to update segment pieces: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, Error.Wrap(err)
	}
	return resultPieces, nil
}

// SetObjectExactVersionRetention contains arguments necessary for setting
// the retention configuration of an exact version of an object.
type SetObjectExactVersionRetention struct {
	ObjectLocation
	Version Version

	Retention Retention
}

// Verify verifies the request fields.
func (opts *SetObjectExactVersionRetention) Verify() (err error) {
	if err = opts.ObjectLocation.Verify(); err != nil {
		return err
	}
	if err = opts.Retention.Verify(); err != nil {
		return ErrInvalidRequest.Wrap(err)
	}
	return nil
}

// SetObjectExactVersionRetention sets the retention configuration of an exact version of an object.
func (db *DB) SetObjectExactVersionRetention(ctx context.Context, opts SetObjectExactVersionRetention) (err error) {
	defer mon.Task()(&ctx)(&err)

	if err := opts.Verify(); err != nil {
		return err
	}

	return db.ChooseAdapter(opts.ProjectID).SetObjectExactVersionRetention(ctx, opts)
}

// SetObjectExactVersionRetention sets the retention configuration of an exact version of an object.
func (p *PostgresAdapter) SetObjectExactVersionRetention(ctx context.Context, opts SetObjectExactVersionRetention) (err error) {
	defer mon.Task()(&ctx)(&err)

	var info preUpdateRetentionInfo

	err = p.db.QueryRowContext(ctx, `
		SELECT status, expires_at, retention_mode, retain_until
		FROM objects
		WHERE
			(project_id, bucket_name, object_key, version) = ($1, $2, $3, $4)
		`, opts.ProjectID, opts.BucketName, opts.ObjectKey, opts.Version,
	).Scan(
		&info.Status,
		&info.ExpiresAt,
		retentionModeWrapper{&info.Retention.Mode},
		timeWrapper{&info.Retention.RetainUntil},
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrObjectNotFound.New("")
		}
		return Error.New("unable to query object info before setting retention: %w", err)
	}

	if err = info.verify(opts.Retention); err != nil {
		return errs.Wrap(err)
	}

	return errs.Wrap(p.setObjectExactVersionRetention(ctx, opts))
}

func (p *PostgresAdapter) setObjectExactVersionRetention(ctx context.Context, opts SetObjectExactVersionRetention) (err error) {
	defer mon.Task()(&ctx)(&err)

	res, err := p.db.ExecContext(ctx, `
		UPDATE objects
		SET
			retention_mode = $5,
			retain_until   = $6
		WHERE
			(project_id, bucket_name, object_key, version) = ($1, $2, $3, $4)
		`, opts.ProjectID, []byte(opts.BucketName), opts.ObjectKey, opts.Version,
		retentionModeWrapper{&opts.Retention.Mode}, timeWrapper{&opts.Retention.RetainUntil},
	)
	if err != nil {
		return Error.New("unable to update object retention configuration: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return Error.New("unable to get number of affected objects: %w", err)
	}
	if affected == 0 {
		return ErrObjectNotFound.New("")
	}

	return nil
}

// SetObjectExactVersionRetention sets the retention configuration of an exact version of an object.
func (s *SpannerAdapter) SetObjectExactVersionRetention(ctx context.Context, opts SetObjectExactVersionRetention) (err error) {
	defer mon.Task()(&ctx)(&err)

	result, err := spannerutil.CollectRow(s.client.Single().Query(ctx, spanner.Statement{
		SQL: `
			SELECT status, expires_at, retention_mode, retain_until
			FROM objects
			WHERE
				(project_id, bucket_name, object_key, version) = (@project_id, @bucket_name, @object_key, @version)
		`,
		Params: map[string]interface{}{
			"project_id":  opts.ProjectID,
			"bucket_name": opts.BucketName,
			"object_key":  opts.ObjectKey,
			"version":     opts.Version,
		},
	}), func(row *spanner.Row, item *preUpdateRetentionInfo) error {
		return Error.Wrap(row.Columns(
			&item.Status,
			&item.ExpiresAt,
			retentionModeWrapper{&item.Retention.Mode},
			timeWrapper{&item.Retention.RetainUntil},
		))
	})
	if err != nil {
		if errors.Is(err, iterator.Done) {
			return ErrObjectNotFound.New("")
		}
		return Error.New("unable to query object info before setting retention: %w", err)
	}

	if err = result.verify(opts.Retention); err != nil {
		return errs.Wrap(err)
	}

	return errs.Wrap(s.setObjectExactVersionRetention(ctx, opts))
}

func (s *SpannerAdapter) setObjectExactVersionRetention(ctx context.Context, opts SetObjectExactVersionRetention) (err error) {
	defer mon.Task()(&ctx)(&err)

	var affected int64
	_, err = s.client.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		affected, err = tx.Update(ctx, spanner.Statement{
			SQL: `
				UPDATE objects
				SET
					retention_mode = @retention_mode,
					retain_until   = @retain_until
				WHERE
					(project_id, bucket_name, object_key, version) = (@project_id, @bucket_name, @object_key, @version)
			`,
			Params: map[string]interface{}{
				"project_id":     opts.ProjectID,
				"bucket_name":    opts.BucketName,
				"object_key":     opts.ObjectKey,
				"version":        opts.Version,
				"retention_mode": retentionModeWrapper{&opts.Retention.Mode},
				"retain_until":   timeWrapper{&opts.Retention.RetainUntil},
			},
		})
		return errs.Wrap(err)
	})
	if err != nil {
		return Error.New("unable to update object retention configuration: %w", err)
	}

	if affected == 0 {
		return ErrObjectNotFound.New("")
	}

	return nil
}

// SetObjectLastCommittedRetention contains arguments necessary for setting
// the retention configuration of the most recently committed version of an object.
type SetObjectLastCommittedRetention struct {
	ObjectLocation
	Retention Retention
}

// Verify verifies the request fields.
func (opts SetObjectLastCommittedRetention) Verify() (err error) {
	if err = opts.ObjectLocation.Verify(); err != nil {
		return err
	}
	if err = opts.Retention.Verify(); err != nil {
		return ErrInvalidRequest.Wrap(err)
	}
	return nil
}

// SetObjectLastCommittedRetention sets the retention configuration
// of the most recently committed version of an object.
func (db *DB) SetObjectLastCommittedRetention(ctx context.Context, opts SetObjectLastCommittedRetention) (err error) {
	defer mon.Task()(&ctx)(&err)

	if err := opts.Verify(); err != nil {
		return err
	}

	return db.ChooseAdapter(opts.ProjectID).SetObjectLastCommittedRetention(ctx, opts)
}

// SetObjectLastCommittedRetention sets the retention configuration
// of the most recently committed version of an object.
func (p *PostgresAdapter) SetObjectLastCommittedRetention(ctx context.Context, opts SetObjectLastCommittedRetention) (err error) {
	defer mon.Task()(&ctx)(&err)

	var (
		version Version
		info    preUpdateRetentionInfo
	)
	err = p.db.QueryRowContext(ctx, `
		SELECT version, expires_at, retention_mode, retain_until
		FROM objects
		WHERE
			(project_id, bucket_name, object_key) = ($1, $2, $3)
			AND status IN `+statusesCommitted+`
		ORDER BY version DESC
		LIMIT 1
		`, opts.ProjectID, opts.BucketName, opts.ObjectKey,
	).Scan(
		&version,
		&info.ExpiresAt,
		retentionModeWrapper{&info.Retention.Mode},
		timeWrapper{&info.Retention.RetainUntil},
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrObjectNotFound.New("")
		}
		return Error.New("unable to query object info before setting retention: %w", err)
	}

	if err = info.verifyWithoutStatus(opts.Retention); err != nil {
		return errs.Wrap(err)
	}

	return errs.Wrap(p.setObjectExactVersionRetention(ctx, SetObjectExactVersionRetention{
		ObjectLocation: opts.ObjectLocation,
		Version:        version,
		Retention:      opts.Retention,
	}))
}

// SetObjectLastCommittedRetention sets the retention configuration
// of the most recently committed version of an object.
func (s *SpannerAdapter) SetObjectLastCommittedRetention(ctx context.Context, opts SetObjectLastCommittedRetention) (err error) {
	defer mon.Task()(&ctx)(&err)

	type info struct {
		version Version
		preUpdateRetentionInfo
	}

	result, err := spannerutil.CollectRow(s.client.Single().Query(ctx, spanner.Statement{
		SQL: `
			SELECT version, expires_at, retention_mode, retain_until
			FROM objects
			WHERE
				(project_id, bucket_name, object_key) = (@project_id, @bucket_name, @object_key)
				AND status IN ` + statusesCommitted + `
			ORDER BY version DESC
			LIMIT 1
		`,
		Params: map[string]interface{}{
			"project_id":  opts.ProjectID,
			"bucket_name": opts.BucketName,
			"object_key":  opts.ObjectKey,
		},
	}), func(row *spanner.Row, item *info) error {
		return Error.Wrap(row.Columns(
			&item.version,
			&item.ExpiresAt,
			retentionModeWrapper{&item.Retention.Mode},
			timeWrapper{&item.Retention.RetainUntil},
		))
	})
	if err != nil {
		if errors.Is(err, iterator.Done) {
			return ErrObjectNotFound.New("")
		}
		return Error.New("unable to query object info before setting retention: %w", err)
	}

	if err = result.verifyWithoutStatus(opts.Retention); err != nil {
		return errs.Wrap(err)
	}

	return Error.Wrap(s.setObjectExactVersionRetention(ctx, SetObjectExactVersionRetention{
		ObjectLocation: opts.ObjectLocation,
		Version:        result.version,
		Retention:      opts.Retention,
	}))
}

// preUpdateRetentionInfo contains information about an object that is collected
// before updating the object's retention configuration.
type preUpdateRetentionInfo struct {
	Status    ObjectStatus
	ExpiresAt *time.Time
	Retention Retention
}

// verify returns an error if the object's retention shouldn't be updated.
func (info *preUpdateRetentionInfo) verify(newRetention Retention) error {
	if !info.Status.IsCommitted() {
		return ErrObjectStatus.New(noLockOnUncommittedErrMsg)
	}
	return errs.Wrap(info.verifyWithoutStatus(newRetention))
}

// verifyWithoutStatus returns an error if the object's retention shouldn't be updated,
// ignoring the status.
func (info *preUpdateRetentionInfo) verifyWithoutStatus(newRetention Retention) error {
	if info.ExpiresAt != nil {
		return ErrObjectExpiration.New(noLockWithExpirationErrMsg)
	}

	if err := info.Retention.Verify(); err != nil {
		return errs.Wrap(err)
	}

	if info.Retention.Active() {
		switch {
		case !newRetention.Enabled():
			return ErrObjectLock.New(noRemoveRetentionErrMsg)
		case newRetention.RetainUntil.Before(info.Retention.RetainUntil):
			return ErrObjectLock.New(noShortenRetentionErrMsg)
		}
	}

	return nil
}
