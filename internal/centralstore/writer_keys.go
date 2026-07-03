package centralstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrWriterKeyNotFound is returned by WriterKey when no active key exists for
// the given writer_id. Callers should treat this as an auth rejection (403/401)
// rather than an internal error.
var ErrWriterKeyNotFound = errors.New("centralstore: writer key not found or inactive")

// UpsertWriterKey inserts or replaces the HMAC key for writerID. If a row for
// writerID already exists, the secret is updated, updated_at is set to now(),
// and active is set to true (re-activates a previously revoked key). This is the
// provision-and-rotate operation: calling it a second time with a new secret
// rotates the key without needing a separate delete step.
func (s *Store) UpsertWriterKey(ctx context.Context, writerID string, secret []byte) error {
	const sql = `
		INSERT INTO cloud_writer_keys (writer_id, secret)
		VALUES ($1, $2)
		ON CONFLICT (writer_id) DO UPDATE SET
			secret     = EXCLUDED.secret,
			updated_at = now(),
			active     = true`
	if _, err := s.pool.Exec(ctx, sql, writerID, secret); err != nil {
		return fmt.Errorf("UpsertWriterKey %q: %w", writerID, err)
	}
	return nil
}

// WriterKey returns the raw HMAC key for writerID. It returns ErrWriterKeyNotFound
// when no row exists for writerID or when the row's active flag is false (revoked).
// The caller uses the returned bytes directly with wireauth.Verify.
func (s *Store) WriterKey(ctx context.Context, writerID string) ([]byte, error) {
	const sql = `
		SELECT secret FROM cloud_writer_keys
		WHERE writer_id = $1 AND active = true`
	var secret []byte
	err := s.pool.QueryRow(ctx, sql, writerID).Scan(&secret)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrWriterKeyNotFound
		}
		return nil, fmt.Errorf("WriterKey %q: %w", writerID, err)
	}
	return secret, nil
}

// WriterPurgeEpoch returns the current purge_epoch for writerID. It uses the
// SAME not-found semantics as WriterKey: ErrWriterKeyNotFound when no row
// exists for writerID or when the row's active flag is false (revoked) — a
// revoked writer must not be able to probe or influence the purge epoch.
func (s *Store) WriterPurgeEpoch(ctx context.Context, writerID string) (int64, error) {
	const sql = `
		SELECT purge_epoch FROM cloud_writer_keys
		WHERE writer_id = $1 AND active = true`
	var epoch int64
	err := s.pool.QueryRow(ctx, sql, writerID).Scan(&epoch)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrWriterKeyNotFound
		}
		return 0, fmt.Errorf("WriterPurgeEpoch %q: %w", writerID, err)
	}
	return epoch, nil
}

// BumpPurgeEpoch increments purge_epoch by 1 for the active writer identified
// by writerID and returns the NEW value. This is the trigger for a remote
// purge: the next time that writer's daemon starts (or otherwise checks
// /v1/state), it will see remote_epoch > honored_epoch and purge its local
// SYNCED data before re-pulling from central.
//
// Returns ErrWriterKeyNotFound when no active row exists for writerID —
// bumping a nonexistent or revoked writer's epoch is a no-op the caller must
// not mistake for success.
func (s *Store) BumpPurgeEpoch(ctx context.Context, writerID string) (int64, error) {
	const sql = `
		UPDATE cloud_writer_keys
		SET purge_epoch = purge_epoch + 1,
		    updated_at  = now()
		WHERE writer_id = $1 AND active = true
		RETURNING purge_epoch`
	var epoch int64
	err := s.pool.QueryRow(ctx, sql, writerID).Scan(&epoch)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrWriterKeyNotFound
		}
		return 0, fmt.Errorf("BumpPurgeEpoch %q: %w", writerID, err)
	}
	return epoch, nil
}

// BumpAllPurgeEpochs increments purge_epoch by 1 for every ACTIVE writer whose
// writer_id is NOT in except, and returns the number of writers affected. This
// backs `engram keys purge --all [--except <id>...]` — a central-wide purge
// broadcast that spares any writer explicitly excluded (e.g. the writer that
// requested the purge, which does not need to purge itself).
//
// except may be nil or empty (no exclusions). A nil/empty except purges every
// active writer. Passing a writer_id not currently active is harmless (the
// UPDATE's WHERE active = true already excludes it; listing it in except is a
// no-op either way).
//
// except is normalized to a non-nil (possibly empty) slice before binding: a
// NIL slice encodes as SQL NULL, and `x = ANY(NULL)` evaluates to NULL (not
// false), which would make `NOT (x = ANY(NULL))` also NULL — silently
// excluding EVERY row from the WHERE clause instead of none. An empty (non-nil)
// slice encodes as `ARRAY[]::text[]`, for which `x = ANY(ARRAY[])` is false and
// `NOT false` is true for every row — the correct "no exclusions" behavior.
func (s *Store) BumpAllPurgeEpochs(ctx context.Context, except []string) (int, error) {
	if except == nil {
		except = []string{}
	}
	const sql = `
		UPDATE cloud_writer_keys
		SET purge_epoch = purge_epoch + 1,
		    updated_at  = now()
		WHERE active = true AND NOT (writer_id = ANY($1))`
	tag, err := s.pool.Exec(ctx, sql, except)
	if err != nil {
		return 0, fmt.Errorf("BumpAllPurgeEpochs: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// DeactivateWriterKey sets active = false for writerID, revoking the key. After
// deactivation, WriterKey returns ErrWriterKeyNotFound for that writerID. The
// row is retained in the table so the audit trail (created_at, updated_at) is
// preserved. To re-activate or rotate, call UpsertWriterKey with a new secret.
func (s *Store) DeactivateWriterKey(ctx context.Context, writerID string) error {
	const sql = `
		UPDATE cloud_writer_keys
		SET active     = false,
		    updated_at = now()
		WHERE writer_id = $1`
	tag, err := s.pool.Exec(ctx, sql, writerID)
	if err != nil {
		return fmt.Errorf("DeactivateWriterKey %q: %w", writerID, err)
	}
	if tag.RowsAffected() == 0 {
		// No row matched writerID — nothing was revoked. Surface this so callers
		// don't mistake a no-op for a successful revocation.
		return ErrWriterKeyNotFound
	}
	return nil
}
