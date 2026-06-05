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
	if _, err := s.pool.Exec(ctx, sql, writerID); err != nil {
		return fmt.Errorf("DeactivateWriterKey %q: %w", writerID, err)
	}
	return nil
}
