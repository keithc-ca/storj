// Copyright (C) 2020 Storj Labs, Inc.
// See LICENSE for copying information.

package metabase

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"cloud.google.com/go/spanner"
	pgxerrcode "github.com/jackc/pgerrcode"
	"github.com/zeebo/errs"
	"google.golang.org/grpc/codes"

	"storj.io/common/memory"
	"storj.io/common/storj"
	"storj.io/common/uuid"
	"storj.io/storj/shared/dbutil/pgutil/pgerrcode"
	"storj.io/storj/shared/dbutil/txutil"
	"storj.io/storj/shared/tagsql"
)

// ValidatePlainSize determines whether we disable PlainSize validation for old uplinks.
const ValidatePlainSize = false

const defaultZombieDeletionPeriod = 24 * time.Hour

var (
	// ErrObjectNotFound is used to indicate that the object does not exist.
	ErrObjectNotFound = errs.Class("object not found")
	// ErrInvalidRequest is used to indicate invalid requests.
	ErrInvalidRequest = errs.Class("metabase: invalid request")
	// ErrFailedPrecondition is used to indicate that some conditions in the request has failed.
	ErrFailedPrecondition = errs.Class("metabase: failed precondition")
	// ErrConflict is used to indicate conflict with the request.
	ErrConflict = errs.Class("metabase: conflict")
)

type commitObjectTransactionAdapter interface {
	updateSegmentOffsets(ctx context.Context, streamID uuid.UUID, updates []segmentToCommit) (err error)
	finalizeObjectCommit(ctx context.Context, opts CommitObject, nextStatus ObjectStatus, nextVersion Version, finalSegments []segmentInfoForCommit, totalPlainSize int64, totalEncryptedSize int64, fixedSegmentSize int32, object *Object) error
	finalizeInlineObjectCommit(ctx context.Context, object *Object, segment *Segment) (err error)

	precommitTransactionAdapter
}

// BeginObjectNextVersion contains arguments necessary for starting an object upload.
type BeginObjectNextVersion struct {
	ObjectStream

	ExpiresAt              *time.Time
	ZombieDeletionDeadline *time.Time

	EncryptedUserData
	Encryption storj.EncryptionParameters

	Retention Retention // optional
	LegalHold bool

	// supported only by Spanner.
	MaxCommitDelay *time.Duration
}

// Verify verifies get object request fields.
func (opts *BeginObjectNextVersion) Verify() error {
	if err := opts.ObjectStream.Verify(); err != nil {
		return err
	}

	if opts.Version != NextVersion {
		return ErrInvalidRequest.New("Version should be metabase.NextVersion")
	}

	err := opts.EncryptedUserData.Verify()
	if err != nil {
		return err
	}

	if err := opts.Retention.Verify(); err != nil {
		return ErrInvalidRequest.Wrap(err)
	}

	if opts.ExpiresAt != nil {
		switch {
		case opts.Retention.Enabled():
			return ErrInvalidRequest.New("ExpiresAt must not be set if Retention is set")
		case opts.LegalHold:
			return ErrInvalidRequest.New("ExpiresAt must not be set if LegalHold is set")
		}
	}

	return nil
}

// BeginObjectNextVersion adds a pending object to the database, with automatically assigned version.
func (db *DB) BeginObjectNextVersion(ctx context.Context, opts BeginObjectNextVersion) (object Object, err error) {
	defer mon.Task()(&ctx)(&err)

	if err := opts.Verify(); err != nil {
		return Object{}, err
	}

	if opts.ZombieDeletionDeadline == nil {
		deadline := time.Now().Add(defaultZombieDeletionPeriod)
		opts.ZombieDeletionDeadline = &deadline
	}

	object = Object{
		ObjectStream: ObjectStream{
			ProjectID:  opts.ProjectID,
			BucketName: opts.BucketName,
			ObjectKey:  opts.ObjectKey,
			StreamID:   opts.StreamID,
		},
		ExpiresAt:              opts.ExpiresAt,
		Encryption:             opts.Encryption,
		ZombieDeletionDeadline: opts.ZombieDeletionDeadline,
		Retention:              opts.Retention,
		LegalHold:              opts.LegalHold,
	}

	err = db.ChooseAdapter(opts.ProjectID).BeginObjectNextVersion(ctx, opts, &object)
	if err != nil {
		return Object{}, Error.New("unable to insert object: %w", err)
	}

	mon.Meter("object_begin").Mark(1)

	return object, nil
}

// BeginObjectNextVersion implements Adapter.
func (p *PostgresAdapter) BeginObjectNextVersion(ctx context.Context, opts BeginObjectNextVersion, object *Object) error {
	return p.db.QueryRowContext(ctx, `
			INSERT INTO objects (
				project_id, bucket_name, object_key, version, stream_id,
				expires_at, encryption,
				zombie_deletion_deadline,
				encrypted_metadata, encrypted_metadata_nonce, encrypted_metadata_encrypted_key, encrypted_etag,
				retention_mode, retain_until
			) VALUES (
				$1, $2, $3,
					coalesce((
						SELECT version + 1
						FROM objects
						WHERE (project_id, bucket_name, object_key) = ($1, $2, $3)
						ORDER BY version DESC
						LIMIT 1
					), 1),
				$4, $5, $6,
				$7,
				$8, $9, $10, $11,
				$12, $13
			)
			RETURNING status, version, created_at
		`, opts.ProjectID, opts.BucketName, opts.ObjectKey, opts.StreamID,
		opts.ExpiresAt, encryptionParameters{&opts.Encryption},
		opts.ZombieDeletionDeadline,
		opts.EncryptedMetadata, opts.EncryptedMetadataNonce, opts.EncryptedMetadataEncryptedKey, opts.EncryptedETag,
		lockModeWrapper{
			retentionMode: &opts.Retention.Mode,
			legalHold:     &opts.LegalHold,
		}, timeWrapper{&opts.Retention.RetainUntil},
	).Scan(&object.Status, &object.Version, &object.CreatedAt)
}

// BeginObjectNextVersion implements Adapter.
func (s *SpannerAdapter) BeginObjectNextVersion(ctx context.Context, opts BeginObjectNextVersion, object *Object) error {
	_, err := s.client.ReadWriteTransactionWithOptions(ctx, func(ctx context.Context, txn *spanner.ReadWriteTransaction) error {
		enc, err := encryptionParameters{&opts.Encryption}.Value()
		if err != nil {
			return Error.Wrap(err)
		}

		return Error.Wrap(txn.Query(ctx, spanner.Statement{
			SQL: `INSERT objects (
					project_id, bucket_name, object_key, version, stream_id,
					expires_at, encryption,
					zombie_deletion_deadline,
					encrypted_metadata, encrypted_metadata_nonce, encrypted_metadata_encrypted_key, encrypted_etag,
					retention_mode, retain_until
				) VALUES (
					@project_id, @bucket_name, @object_key,
					coalesce(
						(SELECT version + 1
						FROM objects
						WHERE (project_id, bucket_name, object_key) = (@project_id, @bucket_name, @object_key)
						ORDER BY version DESC
						LIMIT 1)
					,1),
					@stream_id, @expires_at,
					@encryption, @zombie_deletion_deadline,
					@encrypted_metadata, @encrypted_metadata_nonce, @encrypted_metadata_encrypted_key, @encrypted_etag,
					@retention_mode, @retain_until
				)
				THEN RETURN status,version,created_at`,
			Params: map[string]interface{}{
				"project_id":                       opts.ProjectID.Bytes(),
				"bucket_name":                      opts.BucketName,
				"object_key":                       opts.ObjectKey,
				"stream_id":                        opts.StreamID.Bytes(),
				"expires_at":                       opts.ExpiresAt,
				"encryption":                       enc,
				"zombie_deletion_deadline":         opts.ZombieDeletionDeadline,
				"encrypted_metadata":               opts.EncryptedMetadata,
				"encrypted_metadata_nonce":         opts.EncryptedMetadataNonce,
				"encrypted_metadata_encrypted_key": opts.EncryptedMetadataEncryptedKey,
				"encrypted_etag":                   opts.EncryptedETag,
				"retention_mode": lockModeWrapper{
					retentionMode: &opts.Retention.Mode,
					legalHold:     &opts.LegalHold,
				},
				"retain_until": timeWrapper{&opts.Retention.RetainUntil},
			},
		}).Do(func(row *spanner.Row) error {
			return Error.Wrap(row.Columns(&object.Status, &object.Version, &object.CreatedAt))
		}))
	}, spanner.TransactionOptions{
		CommitOptions: spanner.CommitOptions{
			MaxCommitDelay: opts.MaxCommitDelay,
		},
		TransactionTag: "begin-object-next-version",
	})
	return err
}

// BeginObjectExactVersion contains arguments necessary for starting an object upload.
type BeginObjectExactVersion struct {
	ObjectStream

	ExpiresAt              *time.Time
	ZombieDeletionDeadline *time.Time

	EncryptedUserData
	Encryption storj.EncryptionParameters

	Retention Retention // optional
	LegalHold bool

	// TestingBypassVerify makes the (*DB).TestingBeginObjectExactVersion method skip
	// validation of this struct's fields. This is useful for inserting intentionally
	// malformed or unexpected data into the database and testing that we handle it properly.
	TestingBypassVerify bool
}

// Verify verifies get object reqest fields.
func (opts *BeginObjectExactVersion) Verify() error {
	if err := opts.ObjectStream.Verify(); err != nil {
		return err
	}

	if opts.Version == NextVersion {
		return ErrInvalidRequest.New("Version should not be metabase.NextVersion")
	}

	err := opts.EncryptedUserData.Verify()
	if err != nil {
		return err
	}

	if err := opts.Retention.Verify(); err != nil {
		return ErrInvalidRequest.Wrap(err)
	}

	if opts.ExpiresAt != nil {
		switch {
		case opts.Retention.Enabled():
			return ErrInvalidRequest.New("ExpiresAt must not be set if Retention is set")
		case opts.LegalHold:
			return ErrInvalidRequest.New("ExpiresAt must not be set if LegalHold is set")
		}
	}

	return nil
}

// TestingBeginObjectExactVersion adds a pending object to the database, with specific version.
func (db *DB) TestingBeginObjectExactVersion(ctx context.Context, opts BeginObjectExactVersion) (committed Object, err error) {
	defer mon.Task()(&ctx)(&err)

	if !opts.TestingBypassVerify {
		if err := opts.Verify(); err != nil {
			return Object{}, err
		}
	}

	if opts.ZombieDeletionDeadline == nil {
		deadline := time.Now().Add(defaultZombieDeletionPeriod)
		opts.ZombieDeletionDeadline = &deadline
	}

	object := Object{
		ObjectStream: ObjectStream{
			ProjectID:  opts.ProjectID,
			BucketName: opts.BucketName,
			ObjectKey:  opts.ObjectKey,
			Version:    opts.Version,
			StreamID:   opts.StreamID,
		},
		ExpiresAt:              opts.ExpiresAt,
		Encryption:             opts.Encryption,
		ZombieDeletionDeadline: opts.ZombieDeletionDeadline,
		Retention:              opts.Retention,
		LegalHold:              opts.LegalHold,
	}

	err = db.ChooseAdapter(opts.ProjectID).TestingBeginObjectExactVersion(ctx, opts, &object)
	if err != nil {
		if ErrObjectAlreadyExists.Has(err) {
			return Object{}, err
		}
		return Object{}, Error.New("unable to commit object: %w", err)
	}

	mon.Meter("object_begin").Mark(1)

	return object, nil
}

// TestingBeginObjectExactVersion implements Adapter.
func (p *PostgresAdapter) TestingBeginObjectExactVersion(ctx context.Context, opts BeginObjectExactVersion, object *Object) error {
	err := p.db.QueryRowContext(ctx, `
		INSERT INTO objects (
			project_id, bucket_name, object_key, version, stream_id,
			expires_at, encryption,
			zombie_deletion_deadline,
			encrypted_metadata, encrypted_metadata_nonce, encrypted_metadata_encrypted_key, encrypted_etag,
			retention_mode, retain_until
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7,
			$8,
			$9, $10, $11, $12,
			$13, $14
		)
		RETURNING status, created_at
		`, opts.ProjectID, opts.BucketName, opts.ObjectKey, opts.Version, opts.StreamID,
		opts.ExpiresAt, encryptionParameters{&opts.Encryption},
		opts.ZombieDeletionDeadline,
		opts.EncryptedMetadata, opts.EncryptedMetadataNonce, opts.EncryptedMetadataEncryptedKey, opts.EncryptedETag,
		lockModeWrapper{
			retentionMode: &opts.Retention.Mode,
			legalHold:     &opts.LegalHold,
		}, timeWrapper{&opts.Retention.RetainUntil},
	).Scan(
		&object.Status, &object.CreatedAt,
	)
	if err != nil {
		if code := pgerrcode.FromError(err); code == pgxerrcode.UniqueViolation {
			return Error.Wrap(ErrObjectAlreadyExists.New(""))
		}
	}
	return err
}

// TestingBeginObjectExactVersion implements Adapter.
func (s *SpannerAdapter) TestingBeginObjectExactVersion(ctx context.Context, opts BeginObjectExactVersion, object *Object) error {
	_, err := s.client.ReadWriteTransactionWithOptions(ctx, func(ctx context.Context, txn *spanner.ReadWriteTransaction) error {
		err := txn.Query(ctx, spanner.Statement{
			SQL: `INSERT INTO objects (
				project_id, bucket_name, object_key, version, stream_id,
				expires_at, encryption,
				zombie_deletion_deadline,
				encrypted_metadata, encrypted_metadata_nonce, encrypted_metadata_encrypted_key, encrypted_etag,
				retention_mode, retain_until
			) VALUES (
				@project_id, @bucket_name, @object_key, @version, @stream_id,
				@expires_at, @encryption,
				@zombie_deletion_deadline,
				@encrypted_metadata, @encrypted_metadata_nonce, @encrypted_metadata_encrypted_key, @encrypted_etag,
				@retention_mode, @retain_until
			) THEN RETURN status, created_at`,
			Params: map[string]interface{}{
				"project_id":                       opts.ProjectID,
				"bucket_name":                      opts.BucketName,
				"object_key":                       opts.ObjectKey,
				"version":                          opts.Version,
				"stream_id":                        opts.StreamID,
				"expires_at":                       opts.ExpiresAt,
				"encryption":                       &encryptionParameters{&opts.Encryption},
				"zombie_deletion_deadline":         opts.ZombieDeletionDeadline,
				"encrypted_metadata":               opts.EncryptedMetadata,
				"encrypted_metadata_nonce":         opts.EncryptedMetadataNonce,
				"encrypted_metadata_encrypted_key": opts.EncryptedMetadataEncryptedKey,
				"encrypted_etag":                   opts.EncryptedETag,
				"retention_mode": lockModeWrapper{
					retentionMode: &opts.Retention.Mode,
					legalHold:     &opts.LegalHold,
				},
				"retain_until": timeWrapper{&opts.Retention.RetainUntil},
			},
		}).Do(func(row *spanner.Row) error {
			return Error.Wrap(row.Columns(&object.Status, &object.CreatedAt))
		})

		if err != nil {
			if errCode := spanner.ErrCode(err); errCode == codes.AlreadyExists {
				return Error.Wrap(ErrObjectAlreadyExists.New(""))
			}
			return Error.Wrap(err)
		}

		return nil
	}, spanner.TransactionOptions{
		TransactionTag: "testing-begin-object-exact-version",
	})
	return err
}

// BeginSegment contains options to verify, whether a new segment upload can be started.
type BeginSegment struct {
	ObjectStream

	Position SegmentPosition

	// TODO: unused field, can remove
	RootPieceID storj.PieceID

	Pieces Pieces

	ObjectExistsChecked bool
}

// BeginSegment verifies, whether a new segment upload can be started.
func (db *DB) BeginSegment(ctx context.Context, opts BeginSegment) (err error) {
	defer mon.Task()(&ctx)(&err)

	if err := opts.ObjectStream.Verify(); err != nil {
		return err
	}

	if err := opts.Pieces.Verify(); err != nil {
		return err
	}

	if opts.RootPieceID.IsZero() {
		return ErrInvalidRequest.New("RootPieceID missing")
	}

	if !opts.ObjectExistsChecked {
		// NOTE: Find a way to safely remove this. This isn't strictly necessary,
		// since we can also fail this in CommitSegment.
		// We should prevent creating segments for non-partial objects.

		// Verify that object exists and is partial.
		exists, err := db.ChooseAdapter(opts.ProjectID).PendingObjectExists(ctx, opts)
		if err != nil {
			return Error.New("unable to query object status: %w", err)
		}
		if !exists {
			return ErrPendingObjectMissing.New("")
		}
	}

	mon.Meter("segment_begin").Mark(1)

	return nil
}

// PendingObjectExists checks whether an object already exists.
func (p *PostgresAdapter) PendingObjectExists(ctx context.Context, opts BeginSegment) (exists bool, err error) {
	err = p.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM objects
			WHERE (project_id, bucket_name, object_key, version, stream_id) = ($1, $2, $3, $4, $5) AND
				status = `+statusPending+`
		)`,
		opts.ProjectID, opts.BucketName, opts.ObjectKey, opts.Version, opts.StreamID).Scan(&exists)
	return exists, err
}

// PendingObjectExists checks whether an object already exists.
func (s *SpannerAdapter) PendingObjectExists(ctx context.Context, opts BeginSegment) (exists bool, err error) {
	err = s.client.Single().QueryWithOptions(ctx, spanner.Statement{
		SQL: `
			SELECT EXISTS (
				SELECT 1
				FROM objects
				WHERE
					project_id      = @project_id
					AND bucket_name = @bucket_name
					AND object_key  = @object_key
					AND version     = @version
					AND stream_id   = @stream_id
					AND status      = ` + statusPending + `
			)
		`,
		Params: map[string]interface{}{
			"project_id":  opts.ProjectID,
			"bucket_name": opts.BucketName,
			"object_key":  opts.ObjectKey,
			"version":     opts.Version,
			"stream_id":   opts.StreamID,
		},
	}, spanner.QueryOptions{RequestTag: "pending-object-exists"}).Do(func(row *spanner.Row) error {
		return Error.Wrap(row.Columns(&exists))
	})
	return exists, Error.Wrap(err)
}

// CommitSegment contains all necessary information about the segment.
type CommitSegment struct {
	ObjectStream

	Position    SegmentPosition
	RootPieceID storj.PieceID

	ExpiresAt *time.Time

	EncryptedKeyNonce []byte
	EncryptedKey      []byte

	PlainOffset   int64 // offset in the original data stream
	PlainSize     int32 // size before encryption
	EncryptedSize int32 // segment size after encryption

	EncryptedETag []byte

	Redundancy storj.RedundancyScheme

	Pieces Pieces

	Placement storj.PlacementConstraint

	// supported only by Spanner.
	MaxCommitDelay *time.Duration

	TestingUseMutations bool
}

// CommitSegment commits segment to the database.
func (db *DB) CommitSegment(ctx context.Context, opts CommitSegment) (err error) {
	defer mon.Task()(&ctx)(&err)

	if err := opts.ObjectStream.Verify(); err != nil {
		return err
	}

	if err := opts.Pieces.Verify(); err != nil {
		return err
	}

	switch {
	case opts.RootPieceID.IsZero():
		return ErrInvalidRequest.New("RootPieceID missing")
	case len(opts.EncryptedKey) == 0:
		return ErrInvalidRequest.New("EncryptedKey missing")
	case len(opts.EncryptedKeyNonce) == 0:
		return ErrInvalidRequest.New("EncryptedKeyNonce missing")
	case opts.EncryptedSize <= 0:
		return ErrInvalidRequest.New("EncryptedSize negative or zero")
	case opts.PlainSize <= 0 && ValidatePlainSize:
		return ErrInvalidRequest.New("PlainSize negative or zero")
	case opts.PlainOffset < 0:
		return ErrInvalidRequest.New("PlainOffset negative")
	case opts.Redundancy.IsZero():
		return ErrInvalidRequest.New("Redundancy zero")
	}

	if len(opts.Pieces) < int(opts.Redundancy.OptimalShares) {
		return ErrInvalidRequest.New("number of pieces is less than redundancy optimal shares value")
	}

	aliasPieces, err := db.aliasCache.EnsurePiecesToAliases(ctx, opts.Pieces)
	if err != nil {
		return Error.New("unable to convert pieces to aliases: %w", err)
	}

	err = db.ChooseAdapter(opts.ProjectID).CommitPendingObjectSegment(ctx, opts, aliasPieces)
	if err != nil {
		if ErrPendingObjectMissing.Has(err) {
			return err
		}
		return Error.New("unable to insert segment: %w", err)
	}

	mon.Meter("segment_commit").Mark(1)
	mon.IntVal("segment_commit_encrypted_size").Observe(int64(opts.EncryptedSize))

	return nil
}

// CommitPendingObjectSegment commits segment to the database.
func (p *PostgresAdapter) CommitPendingObjectSegment(ctx context.Context, opts CommitSegment, aliasPieces AliasPieces) (err error) {
	defer mon.Task()(&ctx)(&err)

	// Verify that object exists and is partial.
	_, err = p.db.ExecContext(ctx, `
		INSERT INTO segments (
			stream_id, position, expires_at,
			root_piece_id, encrypted_key_nonce, encrypted_key,
			encrypted_size, plain_offset, plain_size, encrypted_etag,
			redundancy,
			remote_alias_pieces,
			placement
		) VALUES (
			(
				SELECT stream_id
				FROM objects
				WHERE (project_id, bucket_name, object_key, version, stream_id) = ($12, $13, $14, $15, $16) AND
					status = `+statusPending+`
			), $1, $2,
			$3, $4, $5,
			$6, $7, $8, $9,
			$10,
			$11,
			$17
		)
		ON CONFLICT(stream_id, position)
		DO UPDATE SET
			expires_at = $2,
			root_piece_id = $3, encrypted_key_nonce = $4, encrypted_key = $5,
			encrypted_size = $6, plain_offset = $7, plain_size = $8, encrypted_etag = $9,
			redundancy = $10,
			remote_alias_pieces = $11,
			placement = $17,
			-- clear fields in case it was inline segment before
			inline_data = NULL
		`, opts.Position, opts.ExpiresAt,
		opts.RootPieceID, opts.EncryptedKeyNonce, opts.EncryptedKey,
		opts.EncryptedSize, opts.PlainOffset, opts.PlainSize, opts.EncryptedETag,
		opts.Redundancy,
		aliasPieces,
		opts.ProjectID, opts.BucketName, opts.ObjectKey, opts.Version, opts.StreamID,
		opts.Placement,
	)
	if err != nil {
		if code := pgerrcode.FromError(err); code == pgxerrcode.NotNullViolation {
			return ErrPendingObjectMissing.New("")
		}
	}

	return Error.Wrap(err)
}

// CommitPendingObjectSegment commits segment to the database.
func (p *CockroachAdapter) CommitPendingObjectSegment(ctx context.Context, opts CommitSegment, aliasPieces AliasPieces) (err error) {
	defer mon.Task()(&ctx)(&err)

	// Verify that object exists and is partial.
	_, err = p.db.ExecContext(ctx, `
			UPSERT INTO segments (
				stream_id, position,
				expires_at, root_piece_id, encrypted_key_nonce, encrypted_key,
				encrypted_size, plain_offset, plain_size, encrypted_etag,
				redundancy,
				remote_alias_pieces,
				placement,
				-- clear fields in case it was inline segment before
				inline_data
			) VALUES (
				(
					SELECT stream_id
					FROM objects
					WHERE (project_id, bucket_name, object_key, version, stream_id) = ($12, $13, $14, $15, $16) AND
						status = `+statusPending+`
				), $1, $2,
				$3, $4, $5,
				$6, $7, $8, $9,
				$10,
				$11,
				$17,
				NULL
			)`, opts.Position, opts.ExpiresAt,
		opts.RootPieceID, opts.EncryptedKeyNonce, opts.EncryptedKey,
		opts.EncryptedSize, opts.PlainOffset, opts.PlainSize, opts.EncryptedETag,
		opts.Redundancy,
		aliasPieces,
		opts.ProjectID, opts.BucketName, opts.ObjectKey, opts.Version, opts.StreamID,
		opts.Placement,
	)
	if err != nil {
		if code := pgerrcode.FromError(err); code == pgxerrcode.NotNullViolation {
			return ErrPendingObjectMissing.New("")
		}
	}

	return Error.Wrap(err)
}

// CommitPendingObjectSegment commits segment to the database.
func (s *SpannerAdapter) CommitPendingObjectSegment(ctx context.Context, opts CommitSegment, aliasPieces AliasPieces) (err error) {
	defer mon.Task()(&ctx)(&err)

	if opts.TestingUseMutations {
		return s.commitPendingObjectSegmentWithMutations(ctx, opts, aliasPieces)
	}

	var numRows int64
	_, err = s.client.ReadWriteTransactionWithOptions(ctx, func(ctx context.Context, txn *spanner.ReadWriteTransaction) error {
		stmt := spanner.Statement{
			SQL: `
				INSERT OR UPDATE INTO segments (
					stream_id, position,
					expires_at, root_piece_id, encrypted_key_nonce, encrypted_key,
					encrypted_size, plain_offset, plain_size, encrypted_etag,
					redundancy,
					remote_alias_pieces,
					placement,
					-- clear column in case it was inline segment before
					inline_data
				) VALUES (
					(
						SELECT stream_id
						FROM objects
						WHERE (project_id, bucket_name, object_key, version, stream_id) = (@project_id, @bucket_name, @object_key, @version, @stream_id) AND
							status = ` + statusPending + `
					), @position,
					@expires_at, @root_piece_id, @encrypted_key_nonce, @encrypted_key,
					@encrypted_size, @plain_offset, @plain_size, @encrypted_etag,
					@redundancy,
					@alias_pieces,
					@placement,
					NULL
				)
			`,
			Params: map[string]interface{}{
				"position":            opts.Position,
				"expires_at":          opts.ExpiresAt,
				"root_piece_id":       opts.RootPieceID,
				"encrypted_key_nonce": opts.EncryptedKeyNonce,
				"encrypted_key":       opts.EncryptedKey,
				"encrypted_size":      int64(opts.EncryptedSize),
				"plain_offset":        opts.PlainOffset,
				"plain_size":          int64(opts.PlainSize),
				"encrypted_etag":      opts.EncryptedETag,
				"redundancy":          opts.Redundancy,
				"alias_pieces":        aliasPieces,
				"project_id":          opts.ProjectID,
				"bucket_name":         opts.BucketName,
				"object_key":          opts.ObjectKey,
				"version":             opts.Version,
				"stream_id":           opts.StreamID,
				"placement":           opts.Placement,
			},
		}
		numRows, err = txn.Update(ctx, stmt)
		return err
	}, spanner.TransactionOptions{
		CommitOptions: spanner.CommitOptions{
			MaxCommitDelay: opts.MaxCommitDelay,
		},
		TransactionTag: "commit-pending-object-segment",
	})
	if err != nil {
		if spanner.ErrCode(err) == codes.FailedPrecondition {
			// TODO(spanner) dirty hack to distinguish FailedPrecondition errors.
			// Another issue is that emulator returns different message than real spanner instance.
			if strings.Contains(err.Error(), "column: segments.stream_id") ||
				strings.Contains(err.Error(), "stream_id must not be NULL in table segments") {
				return ErrPendingObjectMissing.New("")
			}
			return ErrFailedPrecondition.Wrap(err)
		}
		return Error.Wrap(err)
	}
	if numRows < 1 {
		return ErrPendingObjectMissing.New("")
	}
	return nil
}

func (s *SpannerAdapter) commitPendingObjectSegmentWithMutations(ctx context.Context, opts CommitSegment, aliasPieces AliasPieces) (err error) {
	defer mon.Task()(&ctx)(&err)

	_, err = s.client.ReadWriteTransactionWithOptions(ctx, func(ctx context.Context, txn *spanner.ReadWriteTransaction) error {
		row, err := txn.ReadRow(ctx,
			"objects",
			spanner.Key{opts.ProjectID, opts.BucketName, opts.ObjectKey, int64(opts.Version)},
			[]string{"stream_id", "status"},
		)
		if err != nil {
			if errors.Is(err, spanner.ErrRowNotFound) {
				return ErrPendingObjectMissing.New("")
			}
			return ErrFailedPrecondition.Wrap(err)
		}

		var streamID uuid.UUID
		var status int64
		err = row.Columns(&streamID, &status)
		if err != nil {
			return Error.Wrap(err)
		}

		if streamID != opts.StreamID || status != int64(Pending) {
			return ErrPendingObjectMissing.New("")
		}

		err = txn.BufferWrite([]*spanner.Mutation{
			spanner.InsertOrUpdate("segments",
				[]string{
					"stream_id", "position", "expires_at", "root_piece_id", "encrypted_key_nonce",
					"encrypted_key", "encrypted_size", "plain_offset", "plain_size", "encrypted_etag",
					"redundancy", "remote_alias_pieces", "placement",
					"inline_data",
				},
				[]any{
					opts.StreamID, opts.Position, opts.ExpiresAt, opts.RootPieceID, opts.EncryptedKeyNonce,
					opts.EncryptedKey, int64(opts.EncryptedSize), opts.PlainOffset, int64(opts.PlainSize), opts.EncryptedETag,
					opts.Redundancy, aliasPieces, opts.Placement,
					// clear column in case it was inline segment before
					nil,
				},
			),
		})
		if err != nil {
			return Error.Wrap(err)
		}

		return nil
	}, spanner.TransactionOptions{
		CommitOptions: spanner.CommitOptions{
			MaxCommitDelay: opts.MaxCommitDelay,
		},
		TransactionTag: "commit-pending-object-segment-mutations",
	})

	return Error.Wrap(err)
}

// CommitInlineSegment contains all necessary information about the segment.
type CommitInlineSegment struct {
	ObjectStream

	Position SegmentPosition

	ExpiresAt *time.Time

	EncryptedKeyNonce []byte
	EncryptedKey      []byte

	PlainOffset   int64 // offset in the original data stream
	PlainSize     int32 // size before encryption
	EncryptedETag []byte

	InlineData []byte

	// supported only by Spanner.
	MaxCommitDelay *time.Duration
}

// Verify verifies commit inline segment reqest fields.
func (opts CommitInlineSegment) Verify() error {
	switch {
	case len(opts.EncryptedKey) == 0:
		return ErrInvalidRequest.New("EncryptedKey missing")
	case len(opts.EncryptedKeyNonce) == 0:
		return ErrInvalidRequest.New("EncryptedKeyNonce missing")
	case opts.PlainSize <= 0 && ValidatePlainSize:
		return ErrInvalidRequest.New("PlainSize negative or zero")
	case opts.PlainOffset < 0:
		return ErrInvalidRequest.New("PlainOffset negative")
	}
	return nil
}

// CommitInlineSegment commits inline segment to the database.
func (db *DB) CommitInlineSegment(ctx context.Context, opts CommitInlineSegment) (err error) {
	defer mon.Task()(&ctx)(&err)

	if err := opts.ObjectStream.Verify(); err != nil {
		return err
	}

	if err := opts.Verify(); err != nil {
		return err
	}

	// TODO: do we have a lower limit for inline data?
	// TODO should we move check for max inline segment from metainfo here
	err = db.ChooseAdapter(opts.ProjectID).CommitInlineSegment(ctx, opts)
	if err != nil {
		if ErrPendingObjectMissing.Has(err) {
			return err
		}
		return Error.New("unable to insert segment: %w", err)
	}
	mon.Meter("segment_commit").Mark(1)
	mon.IntVal("segment_commit_encrypted_size").Observe(int64(len(opts.InlineData)))

	return nil
}

// CommitInlineSegment commits inline segment to the database.
func (p *PostgresAdapter) CommitInlineSegment(ctx context.Context, opts CommitInlineSegment) (err error) {
	_, err = p.db.ExecContext(ctx, `
			INSERT INTO segments (
				stream_id, position, expires_at,
				root_piece_id, encrypted_key_nonce, encrypted_key,
				encrypted_size, plain_offset, plain_size, encrypted_etag,
				inline_data
			) VALUES (
				(
					SELECT stream_id
					FROM objects
					WHERE (project_id, bucket_name, object_key, version, stream_id) = ($11, $12, $13, $14, $15) AND
						status = `+statusPending+`
				),
				$1, $2,
				$3, $4, $5,
				$6, $7, $8, $9,
				$10
			)
			ON CONFLICT(stream_id, position)
			DO UPDATE SET
				expires_at = $2,
				root_piece_id = $3, encrypted_key_nonce = $4, encrypted_key = $5,
				encrypted_size = $6, plain_offset = $7, plain_size = $8, encrypted_etag = $9,
				inline_data = $10,
				-- clear columns in case it was remote segment before
				redundancy = 0, remote_alias_pieces = NULL
		`, opts.Position, opts.ExpiresAt,
		storj.PieceID{}, opts.EncryptedKeyNonce, opts.EncryptedKey,
		len(opts.InlineData), opts.PlainOffset, opts.PlainSize, opts.EncryptedETag,
		opts.InlineData,
		opts.ProjectID, opts.BucketName, opts.ObjectKey, opts.Version, opts.StreamID,
	)
	if err != nil {
		if code := pgerrcode.FromError(err); code == pgxerrcode.NotNullViolation {
			return ErrPendingObjectMissing.New("")
		}
	}

	return Error.Wrap(err)
}

// CommitInlineSegment commits inline segment to the database.
func (p *CockroachAdapter) CommitInlineSegment(ctx context.Context, opts CommitInlineSegment) (err error) {
	_, err = p.db.ExecContext(ctx, `
			UPSERT INTO segments (
				stream_id, position, expires_at,
				root_piece_id, encrypted_key_nonce, encrypted_key,
				encrypted_size, plain_offset, plain_size, encrypted_etag,
				inline_data,
				-- clear columns in case it was remote segment before
				redundancy, remote_alias_pieces
			) VALUES (
				(
					SELECT stream_id
					FROM objects
					WHERE (project_id, bucket_name, object_key, version, stream_id) = ($11, $12, $13, $14, $15) AND
						status = `+statusPending+`
				),
				$1, $2,
				$3, $4, $5,
				$6, $7, $8, $9,
				$10,
				0, NULL
			)
		`, opts.Position, opts.ExpiresAt,
		storj.PieceID{}, opts.EncryptedKeyNonce, opts.EncryptedKey,
		len(opts.InlineData), opts.PlainOffset, opts.PlainSize, opts.EncryptedETag,
		opts.InlineData,
		opts.ProjectID, opts.BucketName, opts.ObjectKey, opts.Version, opts.StreamID,
	)
	if err != nil {
		if code := pgerrcode.FromError(err); code == pgxerrcode.NotNullViolation {
			return ErrPendingObjectMissing.New("")
		}
	}

	return Error.Wrap(err)
}

// CommitInlineSegment commits inline segment to the database.
func (s *SpannerAdapter) CommitInlineSegment(ctx context.Context, opts CommitInlineSegment) (err error) {
	_, err = s.client.ReadWriteTransactionWithOptions(ctx, func(ctx context.Context, txn *spanner.ReadWriteTransaction) error {
		_, err := txn.Update(ctx, spanner.Statement{
			SQL: `
				INSERT OR UPDATE INTO segments (
					stream_id, position, expires_at,
					root_piece_id, encrypted_key_nonce, encrypted_key,
					encrypted_size, plain_offset, plain_size, encrypted_etag,
					inline_data,
					-- clear columns in case it was remote segment before
					 redundancy, remote_alias_pieces
				) VALUES (
					(
						SELECT stream_id
						FROM objects
						WHERE (project_id, bucket_name, object_key, version, stream_id) = (@project_id, @bucket_name, @object_key, @version, @stream_id) AND
							status = ` + statusPending + `
					), @position, @expires_at,
					@root_piece_id, @encrypted_key_nonce, @encrypted_key,
					@encrypted_size, @plain_offset, @plain_size, @encrypted_etag,
					@inline_data,
					0, NULL
				)
			`,
			Params: map[string]interface{}{
				"position":            opts.Position,
				"expires_at":          opts.ExpiresAt,
				"root_piece_id":       storj.PieceID{},
				"encrypted_key_nonce": opts.EncryptedKeyNonce,
				"encrypted_key":       opts.EncryptedKey,
				"encrypted_size":      len(opts.InlineData),
				"plain_offset":        opts.PlainOffset,
				"plain_size":          int64(opts.PlainSize),
				"encrypted_etag":      opts.EncryptedETag,
				"inline_data":         opts.InlineData,
				"project_id":          opts.ProjectID.Bytes(),
				"bucket_name":         opts.BucketName,
				"object_key":          opts.ObjectKey,
				"version":             opts.Version,
				"stream_id":           opts.StreamID,
			},
		})
		return Error.Wrap(err)
	}, spanner.TransactionOptions{
		CommitOptions: spanner.CommitOptions{
			MaxCommitDelay: opts.MaxCommitDelay,
		},
		TransactionTag: "commit-inline-segment",
	})
	if err != nil {
		if code := spanner.ErrCode(err); code == codes.FailedPrecondition {
			return ErrPendingObjectMissing.New("")
		}
	}
	return Error.Wrap(err)
}

// CommitObject contains arguments necessary for committing an object.
type CommitObject struct {
	ObjectStream

	Encryption storj.EncryptionParameters

	// OverrideEncryptedMedata flag controls if we want to set metadata fields with CommitObject
	// it's possible to set metadata with BeginObject request so we need to
	// be explicit if we would like to set it with CommitObject which will
	// override any existing metadata.
	OverrideEncryptedMetadata bool
	EncryptedUserData

	DisallowDelete bool

	// Versioned indicates whether an object is allowed to have multiple versions.
	Versioned bool

	// supported only by Spanner.
	MaxCommitDelay *time.Duration

	// IfNoneMatch is an optional field for conditional writes.
	IfNoneMatch IfNoneMatch
}

// Verify verifies request fields.
func (c *CommitObject) Verify() error {
	if err := c.ObjectStream.Verify(); err != nil {
		return err
	}

	if c.Encryption.CipherSuite != storj.EncUnspecified && c.Encryption.BlockSize <= 0 {
		return ErrInvalidRequest.New("Encryption.BlockSize is negative or zero")
	}

	if c.OverrideEncryptedMetadata {
		err := c.EncryptedUserData.Verify()
		if err != nil {
			return err
		}
	}

	return c.IfNoneMatch.Verify()
}

// WithTx provides a TransactionAdapter for the context of a database transaction.
func (p *PostgresAdapter) WithTx(ctx context.Context, opts TransactionOptions, f func(context.Context, TransactionAdapter) error) error {
	return txutil.WithTx(ctx, p.db, nil, func(ctx context.Context, tx tagsql.Tx) error {
		txAdapter := &postgresTransactionAdapter{postgresAdapter: p, tx: tx}
		return f(ctx, txAdapter)
	})
}

// WithTx provides a TransactionAdapter for the context of a database transaction.
func (s *SpannerAdapter) WithTx(ctx context.Context, opts TransactionOptions, f func(context.Context, TransactionAdapter) error) error {
	transactionTag := opts.TransactionTag
	if transactionTag == "" {
		transactionTag = "metabase-withtx"
	}
	_, err := s.client.ReadWriteTransactionWithOptions(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
		txAdapter := &spannerTransactionAdapter{spannerAdapter: s, tx: tx}
		return f(ctx, txAdapter)
	}, spanner.TransactionOptions{
		CommitOptions: spanner.CommitOptions{
			MaxCommitDelay: opts.MaxCommitDelay,
		},
		TransactionTag: transactionTag,
	})
	return err
}

// CommitObject adds a pending object to the database. If another committed object is under target location
// it will be deleted.
func (db *DB) CommitObject(ctx context.Context, opts CommitObject) (object Object, err error) {
	defer mon.Task()(&ctx)(&err)

	if err := opts.Verify(); err != nil {
		return Object{}, err
	}

	var precommit PrecommitConstraintResult
	err = db.ChooseAdapter(opts.ProjectID).WithTx(ctx, TransactionOptions{
		MaxCommitDelay: opts.MaxCommitDelay,
		TransactionTag: "commit-object",
	}, func(ctx context.Context, adapter TransactionAdapter) error {
		segments, err := adapter.fetchSegmentsForCommit(ctx, opts.StreamID)
		if err != nil {
			return Error.New("failed to fetch segments: %w", err)
		}

		if err = db.validateParts(segments); err != nil {
			return err
		}

		finalSegments := convertToFinalSegments(segments)
		if err := adapter.updateSegmentOffsets(ctx, opts.StreamID, finalSegments); err != nil {
			return Error.New("failed to update segments: %w", err)
		}

		// TODO: would we even need this when we make main index plain_offset?
		fixedSegmentSize := int32(0)
		if len(finalSegments) > 0 {
			fixedSegmentSize = finalSegments[0].PlainSize
			for i, seg := range finalSegments {
				if seg.Position.Part != 0 || seg.Position.Index != uint32(i) {
					fixedSegmentSize = -1
					break
				}
				if i < len(finalSegments)-1 && seg.PlainSize != fixedSegmentSize {
					fixedSegmentSize = -1
					break
				}
			}
		}

		var totalPlainSize, totalEncryptedSize int64
		for _, seg := range finalSegments {
			totalPlainSize += int64(seg.PlainSize)
			totalEncryptedSize += int64(seg.EncryptedSize)
		}

		nextStatus := committedWhereVersioned(opts.Versioned)
		precommit, err = db.PrecommitConstraint(ctx, PrecommitConstraint{
			Location:       opts.Location(),
			Versioned:      opts.Versioned,
			DisallowDelete: opts.DisallowDelete,
			CheckExistence: opts.IfNoneMatch.All(),
		}, adapter)
		if err != nil {
			return err
		}

		nextVersion := opts.Version
		if nextVersion < precommit.HighestVersion {
			nextVersion = precommit.HighestVersion + 1
		}

		err = adapter.finalizeObjectCommit(ctx, opts, nextStatus, nextVersion, segments, totalPlainSize, totalEncryptedSize, fixedSegmentSize, &object)
		if err != nil {
			return err
		}

		object.StreamID = opts.StreamID
		object.ProjectID = opts.ProjectID
		object.BucketName = opts.BucketName
		object.ObjectKey = opts.ObjectKey
		object.Version = nextVersion
		object.Status = nextStatus
		object.SegmentCount = int32(len(segments))
		object.TotalPlainSize = totalPlainSize
		object.TotalEncryptedSize = totalEncryptedSize
		object.FixedSegmentSize = fixedSegmentSize
		return nil
	})
	if err != nil {
		return Object{}, err
	}

	precommit.submitMetrics()

	mon.Meter("object_commit").Mark(1)
	mon.IntVal("object_commit_segments").Observe(int64(object.SegmentCount))
	mon.IntVal("object_commit_encrypted_size").Observe(object.TotalEncryptedSize)

	return object, nil
}

func (ptx *postgresTransactionAdapter) finalizeObjectCommit(ctx context.Context, opts CommitObject, nextStatus ObjectStatus, nextVersion Version, finalSegments []segmentInfoForCommit, totalPlainSize int64, totalEncryptedSize int64, fixedSegmentSize int32, object *Object) (err error) {
	defer mon.Task()(&ctx)(&err)

	args := []interface{}{
		opts.ProjectID, opts.BucketName, opts.ObjectKey, opts.Version, opts.StreamID,
		nextStatus,
		len(finalSegments),
		totalPlainSize,
		totalEncryptedSize,
		fixedSegmentSize,
		encryptionParameters{&opts.Encryption},
	}

	args = append(args, nextVersion)

	metadataColumns := ""
	if opts.OverrideEncryptedMetadata {
		args = append(args,
			opts.EncryptedMetadataNonce,
			opts.EncryptedMetadata,
			opts.EncryptedMetadataEncryptedKey,
			opts.EncryptedETag,
		)
		metadataColumns = `,
				encrypted_metadata_nonce         = $13,
				encrypted_metadata               = $14,
				encrypted_metadata_encrypted_key = $15,
				encrypted_etag                   = $16
			`
	}
	err = ptx.tx.QueryRowContext(ctx, `
			UPDATE objects SET
				version = $12,
				status = $6,
				segment_count = $7,

				total_plain_size     = $8,
				total_encrypted_size = $9,
				fixed_segment_size   = $10,
				zombie_deletion_deadline = NULL,

				-- TODO should we allow to override existing encryption parameters or return error if don't match with opts?
				encryption = CASE
					WHEN objects.encryption = 0 AND $11 <> 0 THEN $11
					WHEN objects.encryption = 0 AND $11 = 0 THEN NULL
					ELSE objects.encryption
				END
				`+metadataColumns+`
			WHERE (project_id, bucket_name, object_key, version, stream_id) = ($1, $2, $3, $4, $5) AND
				status       = `+statusPending+`
			RETURNING
				created_at, expires_at,
				encrypted_metadata, encrypted_metadata_encrypted_key, encrypted_metadata_nonce, encrypted_etag,
				encryption,
				retention_mode, retain_until
			`, args...).Scan(
		&object.CreatedAt, &object.ExpiresAt,
		&object.EncryptedMetadata, &object.EncryptedMetadataEncryptedKey, &object.EncryptedMetadataNonce, &object.EncryptedETag,
		encryptionParameters{&object.Encryption},
		lockModeWrapper{
			retentionMode: &object.Retention.Mode,
			legalHold:     &object.LegalHold,
		},
		timeWrapper{&object.Retention.RetainUntil},
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrObjectNotFound.Wrap(Error.New("object with specified version and pending status is missing"))
		} else if code := pgerrcode.FromError(err); code == pgxerrcode.NotNullViolation {
			// TODO maybe we should check message if 'encryption' label is there
			return ErrInvalidRequest.New("Encryption is missing")
		}
		return Error.New("failed to update object: %w", err)
	}
	if err := object.Retention.Verify(); err != nil {
		return Error.Wrap(err)
	}
	if object.ExpiresAt != nil && (object.LegalHold || object.Retention.Enabled()) {
		return Error.New("object expiration must not be set if Object Lock configuration is set")
	}

	return nil
}

func (stx *spannerTransactionAdapter) finalizeObjectCommit(ctx context.Context, opts CommitObject, nextStatus ObjectStatus, nextVersion Version, finalSegments []segmentInfoForCommit, totalPlainSize int64, totalEncryptedSize int64, fixedSegmentSize int32, object *Object) (err error) {
	defer mon.Task()(&ctx)(&err)

	requestedEncryptionParameters := opts.Encryption
	var (
		deleted                 bool
		oldUserData             EncryptedUserData
		oldEncryptionParameters storj.EncryptionParameters
	)
	lockMode := lockModeWrapper{
		retentionMode: &object.Retention.Mode,
		legalHold:     &object.LegalHold,
	}
	retainUntil := timeWrapper{&object.Retention.RetainUntil}

	// We can not simply UPDATE the row, because we are changing the 'version' column,
	// which is part of the primary key. Spanner does not allow changing a primary key
	// column on an existing row. We must DELETE then INSERT a new row.
	err = stx.tx.QueryWithOptions(ctx, spanner.Statement{
		SQL: `
				DELETE FROM objects
				WHERE
					project_id      = @project_id
					AND bucket_name = @bucket_name
					AND object_key  = @object_key
					AND version     = @version
					AND stream_id   = @stream_id
					AND status      = ` + statusPending + `
				THEN RETURN
					created_at, expires_at,
					encrypted_metadata, encrypted_metadata_encrypted_key, encrypted_metadata_nonce, encrypted_etag,
					encryption,
					retention_mode, retain_until
			`,
		Params: map[string]interface{}{
			"project_id":  opts.ProjectID,
			"bucket_name": opts.BucketName,
			"object_key":  opts.ObjectKey,
			"version":     opts.Version,
			"stream_id":   opts.StreamID,
		},
	}, spanner.QueryOptions{RequestTag: "finalize-object-commit"}).Do(func(row *spanner.Row) error {
		deleted = true
		return Error.Wrap(row.Columns(
			&object.CreatedAt, &object.ExpiresAt,
			&oldUserData.EncryptedMetadata, &oldUserData.EncryptedMetadataEncryptedKey, &oldUserData.EncryptedMetadataNonce, &oldUserData.EncryptedETag,
			encryptionParameters{&oldEncryptionParameters}, lockMode, retainUntil,
		))
	})
	if err != nil {
		return Error.New("failed to delete old object row: %w", err)
	}
	if !deleted {
		return ErrObjectNotFound.Wrap(Error.New("object with specified version and pending status is missing"))
	}
	if err := object.Retention.Verify(); err != nil {
		return Error.Wrap(err)
	}
	if object.ExpiresAt != nil && (object.LegalHold || object.Retention.Enabled()) {
		return Error.New("object expiration must not be set if Object Lock configuration is set")
	}

	// TODO should we allow to override existing encryption parameters or return error if don't match with opts?
	var encryptionArg *storj.EncryptionParameters
	if oldEncryptionParameters.IsZero() && !requestedEncryptionParameters.IsZero() {
		encryptionArg = &requestedEncryptionParameters
	} else if oldEncryptionParameters.IsZero() && requestedEncryptionParameters.IsZero() {
		return ErrInvalidRequest.New("Encryption is missing")
	} else {
		encryptionArg = &oldEncryptionParameters
	}
	if opts.OverrideEncryptedMetadata {
		oldUserData = opts.EncryptedUserData
	}

	// Create insert mutation for objects table
	objectInsert := spanner.Insert("objects",
		[]string{
			"project_id", "bucket_name", "object_key", "version",
			"stream_id", "created_at", "expires_at", "status", "segment_count",
			"encrypted_metadata_nonce", "encrypted_metadata", "encrypted_metadata_encrypted_key", "encrypted_etag",
			"total_plain_size", "total_encrypted_size", "fixed_segment_size",
			"encryption", "zombie_deletion_deadline",
			"retention_mode", "retain_until",
		},
		[]any{
			opts.ProjectID, opts.BucketName, opts.ObjectKey, nextVersion,
			opts.StreamID, object.CreatedAt, object.ExpiresAt, nextStatus, len(finalSegments),
			oldUserData.EncryptedMetadataNonce, oldUserData.EncryptedMetadata, oldUserData.EncryptedMetadataEncryptedKey, oldUserData.EncryptedETag,
			totalPlainSize, totalEncryptedSize, int64(fixedSegmentSize),
			encryptionParameters{encryptionArg}, nil, // zombie_deletion_deadline is NULL
			lockMode, retainUntil,
		})

	err = stx.tx.BufferWrite([]*spanner.Mutation{objectInsert})
	if err != nil {
		if code := spanner.ErrCode(err); code == codes.FailedPrecondition {
			// TODO maybe we should check message if 'encryption' label is there
			return ErrInvalidRequest.New("Encryption is missing (%w)", err)
		}
		return Error.New("failed to update object: %w", err)
	}
	object.Encryption = *encryptionArg
	object.EncryptedUserData = oldUserData
	return nil
}

func (db *DB) validateParts(segments []segmentInfoForCommit) error {
	partSize := make(map[uint32]memory.Size)

	var lastPart uint32
	for _, segment := range segments {
		partSize[segment.Position.Part] += memory.Size(segment.PlainSize)
		if lastPart < segment.Position.Part {
			lastPart = segment.Position.Part
		}
	}

	if len(partSize) > db.config.MaxNumberOfParts {
		return ErrFailedPrecondition.New("exceeded maximum number of parts: %d", db.config.MaxNumberOfParts)
	}

	for part, size := range partSize {
		// Last part has no minimum size.
		if part == lastPart {
			continue
		}

		if size < db.config.MinPartSize {
			return ErrFailedPrecondition.New("size of part number %d is below minimum threshold, got: %s, min: %s", part, size, db.config.MinPartSize)
		}
	}

	return nil
}

// CommitInlineObject contains arguments necessary for committing an inline object.
type CommitInlineObject struct {
	ObjectStream
	CommitInlineSegment CommitInlineSegment

	ExpiresAt *time.Time

	EncryptedUserData
	Encryption storj.EncryptionParameters

	Retention Retention // optional
	LegalHold bool

	DisallowDelete bool

	// Versioned indicates whether an object is allowed to have multiple versions.
	Versioned bool

	// IfNoneMatch is an optional field for conditional writes.
	IfNoneMatch IfNoneMatch
}

// Verify verifies reqest fields.
func (c *CommitInlineObject) Verify() error {
	if err := c.ObjectStream.Verify(); err != nil {
		return err
	}

	if err := c.CommitInlineSegment.Verify(); err != nil {
		return err
	}

	if c.Encryption.CipherSuite != storj.EncUnspecified && c.Encryption.BlockSize <= 0 {
		return ErrInvalidRequest.New("Encryption.BlockSize is negative or zero")
	}

	err := c.EncryptedUserData.Verify()
	if err != nil {
		return err
	}

	if err := c.Retention.Verify(); err != nil {
		return ErrInvalidRequest.Wrap(err)
	}

	if c.ExpiresAt != nil {
		switch {
		case c.Retention.Enabled():
			return ErrInvalidRequest.New("ExpiresAt must not be set if Retention is set")
		case c.LegalHold:
			return ErrInvalidRequest.New("ExpiresAt must not be set if LegalHold is set")
		}
	}

	return c.IfNoneMatch.Verify()
}

// CommitInlineObject adds full inline object to the database. If another committed object is under target location
// it will be deleted.
func (db *DB) CommitInlineObject(ctx context.Context, opts CommitInlineObject) (object Object, err error) {
	defer mon.Task()(&ctx)(&err)

	if err := opts.Verify(); err != nil {
		return Object{}, err
	}

	var precommit PrecommitConstraintResult
	err = db.ChooseAdapter(opts.ProjectID).WithTx(ctx, TransactionOptions{
		TransactionTag: "commit-inline-object",
	}, func(ctx context.Context, adapter TransactionAdapter) error {
		precommit, err = db.PrecommitConstraint(ctx, PrecommitConstraint{
			Location:       opts.Location(),
			Versioned:      opts.Versioned,
			DisallowDelete: opts.DisallowDelete,
			CheckExistence: opts.IfNoneMatch.All(),
		}, adapter)
		if err != nil {
			return err
		}

		nextVersion := precommit.HighestVersion + 1
		nextStatus := committedWhereVersioned(opts.Versioned)

		object.StreamID = opts.StreamID
		object.ProjectID = opts.ProjectID
		object.BucketName = opts.BucketName
		object.ObjectKey = opts.ObjectKey
		object.Version = nextVersion
		object.Status = nextStatus
		object.SegmentCount = 1
		object.TotalPlainSize = int64(opts.CommitInlineSegment.PlainSize)
		object.TotalEncryptedSize = int64(int32(len(opts.CommitInlineSegment.InlineData)))
		object.ExpiresAt = opts.ExpiresAt
		object.Encryption = opts.Encryption
		object.EncryptedUserData = opts.EncryptedUserData
		object.Retention = opts.Retention
		object.LegalHold = opts.LegalHold

		segment := &Segment{
			StreamID:          opts.StreamID,
			Position:          opts.CommitInlineSegment.Position,
			ExpiresAt:         opts.ExpiresAt,
			EncryptedKey:      opts.CommitInlineSegment.EncryptedKey,
			EncryptedKeyNonce: opts.CommitInlineSegment.EncryptedKeyNonce,
			EncryptedETag:     opts.CommitInlineSegment.EncryptedETag,
			PlainSize:         opts.CommitInlineSegment.PlainSize,
			EncryptedSize:     int32(len(opts.CommitInlineSegment.InlineData)),
			InlineData:        opts.CommitInlineSegment.InlineData,
		}

		return adapter.finalizeInlineObjectCommit(ctx, &object, segment)
	})
	if err != nil {
		return Object{}, err
	}

	precommit.submitMetrics()

	mon.Meter("object_commit").Mark(1)
	mon.IntVal("object_commit_segments").Observe(int64(object.SegmentCount))
	mon.IntVal("object_commit_encrypted_size").Observe(object.TotalEncryptedSize)

	return object, nil
}

func (ptx *postgresTransactionAdapter) finalizeInlineObjectCommit(ctx context.Context, object *Object, segment *Segment) (err error) {
	defer mon.Task()(&ctx)(&err)

	// TODO should we put this into single query
	err = ptx.tx.QueryRowContext(ctx, `
		INSERT INTO objects (
			project_id, bucket_name, object_key, version, stream_id,
			status, segment_count, expires_at, encryption,
			total_plain_size, total_encrypted_size,
			zombie_deletion_deadline,
			encrypted_metadata, encrypted_metadata_nonce, encrypted_metadata_encrypted_key, encrypted_etag,
			retention_mode, retain_until
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9,
			$10, $11,
			$12,
			$13, $14, $15, $16,
			$17, $18
		)
		RETURNING created_at`,
		object.ProjectID, object.BucketName, object.ObjectKey, object.Version, object.StreamID,
		object.Status, object.SegmentCount, object.ExpiresAt, encryptionParameters{&object.Encryption},
		object.TotalPlainSize, object.TotalEncryptedSize,
		nil,
		object.EncryptedMetadata, object.EncryptedMetadataNonce, object.EncryptedMetadataEncryptedKey, object.EncryptedETag,
		lockModeWrapper{
			retentionMode: &object.Retention.Mode,
			legalHold:     &object.LegalHold,
		}, timeWrapper{&object.Retention.RetainUntil},
	).Scan(&object.CreatedAt)
	if err != nil {
		return Error.New("failed to create object: %w", err)
	}

	// TODO consider not inserting segment if inline data is empty

	_, err = ptx.tx.ExecContext(ctx, `
		INSERT INTO segments (
			stream_id, position, expires_at,
			root_piece_id, encrypted_key_nonce, encrypted_key,
			encrypted_size, encrypted_etag, plain_size, plain_offset,
			inline_data
		) VALUES (
			$1, $2, $3,
			$4, $5, $6,
			$7, $8, $9, 0, -- plain_offset is 0
			$10
		)
		`, segment.StreamID, segment.Position, segment.ExpiresAt,
		storj.PieceID{}, segment.EncryptedKeyNonce, segment.EncryptedKey,
		segment.EncryptedSize, segment.EncryptedETag, segment.PlainSize,
		segment.InlineData,
	)
	if err != nil {
		return Error.New("failed to create segment: %w", err)
	}

	return nil
}

func (stx *spannerTransactionAdapter) finalizeInlineObjectCommit(ctx context.Context, object *Object, segment *Segment) (err error) {
	defer mon.Task()(&ctx)(&err)

	// TODO(spanner) should we perform these two inserts as a Migration
	err = stx.tx.QueryWithOptions(ctx, spanner.Statement{
		SQL: `
			INSERT INTO objects (
				project_id, bucket_name, object_key, version, stream_id,
				status, segment_count, expires_at, encryption,
				total_plain_size, total_encrypted_size,
				zombie_deletion_deadline,
				encrypted_metadata, encrypted_metadata_nonce, encrypted_metadata_encrypted_key, encrypted_etag,
				retention_mode, retain_until
			) VALUES (
				@project_id, @bucket_name, @object_key, @version, @stream_id,
				@status, @segment_count, @expires_at, @encryption_parameters,
				@total_plain_size, @total_encrypted_size,
				@zombie_deletion_deadline,
				@encrypted_metadata, @encrypted_metadata_nonce, @encrypted_metadata_encrypted_key, @encrypted_etag,
				@retention_mode, @retain_until
			)
			THEN RETURN created_at
		`,
		Params: map[string]interface{}{
			"project_id":                       object.ProjectID,
			"bucket_name":                      object.BucketName,
			"object_key":                       []byte(object.ObjectKey),
			"version":                          object.Version,
			"stream_id":                        object.StreamID,
			"status":                           object.Status,
			"segment_count":                    int64(object.SegmentCount),
			"expires_at":                       object.ExpiresAt,
			"encryption_parameters":            encryptionParameters{&object.Encryption},
			"total_plain_size":                 object.TotalPlainSize,
			"total_encrypted_size":             object.TotalEncryptedSize,
			"zombie_deletion_deadline":         nil,
			"encrypted_metadata":               object.EncryptedMetadata,
			"encrypted_metadata_nonce":         object.EncryptedMetadataNonce,
			"encrypted_metadata_encrypted_key": object.EncryptedMetadataEncryptedKey,
			"encrypted_etag":                   object.EncryptedETag,
			"retention_mode": lockModeWrapper{
				retentionMode: &object.Retention.Mode,
				legalHold:     &object.LegalHold,
			},
			"retain_until": timeWrapper{&object.Retention.RetainUntil},
		},
	}, spanner.QueryOptions{RequestTag: "finalize-inline-object-commit"}).Do(func(row *spanner.Row) error {
		err := row.Columns(&object.CreatedAt)
		if err != nil {
			return Error.New("failed to read object created_at: %w", err)
		}
		return nil
	})
	if err != nil {
		return Error.New("failed to create object: %w", err)
	}

	// TODO consider not inserting segment if inline data is empty
	_, err = stx.tx.UpdateWithOptions(ctx, spanner.Statement{
		SQL: `
			INSERT INTO segments (
				stream_id, position, expires_at,
				root_piece_id, encrypted_key_nonce, encrypted_key,
				encrypted_size, encrypted_etag, plain_size, plain_offset,
				inline_data
			) VALUES (
				@stream_id, @position, @expires_at,
				@root_piece_id, @encrypted_key_nonce, @encrypted_key,
				@encrypted_size, @encrypted_etag, @plain_size, 0, -- plain_offset is 0
				@inline_data
			)
		`,
		Params: map[string]interface{}{
			"stream_id":           segment.StreamID,
			"position":            segment.Position,
			"expires_at":          segment.ExpiresAt,
			"root_piece_id":       storj.PieceID{},
			"encrypted_key_nonce": segment.EncryptedKeyNonce,
			"encrypted_key":       segment.EncryptedKey,
			"encrypted_size":      int64(segment.EncryptedSize),
			"encrypted_etag":      segment.EncryptedETag,
			"plain_size":          int64(segment.PlainSize),
			"inline_data":         segment.InlineData,
		},
	}, spanner.QueryOptions{RequestTag: "finalize-inline-object-commit-segments"})
	if err != nil {
		return Error.New("failed to create segment: %w", err)
	}

	return nil
}
