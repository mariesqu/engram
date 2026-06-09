package centralstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mariesqu/engram/internal/domain"
)

// applyPromptDecisionQ materializes a domain.Mutation with
// EntityType==EntityPrompt into central_user_prompts / central_prompt_tombstones
// within the caller-supplied transaction. The caller (Apply) owns Begin /
// Commit / Rollback.
//
// # Dispatch contract
//
// This function is invoked from Apply AFTER insertMutationQ assigns the central
// journal seq and the durable applied-marker (central_mutations.mutation_id
// UNIQUE). The journal entry is always written regardless of whether the
// materialized state changes (stale guards, idempotent re-applies, etc.) —
// the seq/journal write is entity-agnostic and runs unconditionally first.
//
// # Staleness guard (upsert path)
//
// SELECT deleted_at FROM central_prompt_tombstones WHERE sync_id=$1.
//
//   - No tombstone row: proceed unconditionally to upsert.
//   - Tombstone exists AND m.UpdatedAt <= tombstone.deleted_at (equal = stale):
//     resurrection denied — no-op (the prompt was deleted at least as recently as
//     this write). The journal row is already committed by insertMutationQ; this
//     function returns nil so the outer tx can commit cleanly.
//   - Tombstone exists AND m.UpdatedAt is strictly AFTER tombstone.deleted_at:
//     fresh write wins — DELETE the tombstone, then upsert the live row.
//
// # TIMESTAMPTZ precision
//
// central_prompt_tombstones.deleted_at and central_user_prompts.created_at are
// TIMESTAMPTZ; Postgres scans them as time.Time values with microsecond precision
// (Postgres stores to microseconds, not nanoseconds). The staleness comparison
// uses time.Time.After which compares the full time.Time value — including any
// sub-microsecond nanosecond component present on the incoming domain.Mutation
// (which always uses time.Now().UTC() / RFC3339Nano). In practice all mutations
// carry time.Now() timestamps whose nanosecond component is stripped by Postgres
// on the round-trip; the equal-is-stale rule is therefore conservative: an
// incoming upsert at the SAME microsecond as the tombstone is treated as stale
// (matches the local store's "equal is stale" rule in isStalePromptUpsert).
func applyPromptDecisionQ(ctx context.Context, tx pgx.Tx, m domain.Mutation) error {
	switch m.Op {
	case domain.OpUpsert:
		return applyPromptUpsertQ(ctx, tx, m)
	case domain.OpDelete:
		return applyPromptDeleteQ(ctx, tx, m)
	default:
		return fmt.Errorf("applyPromptDecisionQ: unknown op %q (sync_id=%s)", m.Op, m.SyncID)
	}
}

// applyPromptUpsertQ handles the upsert branch of applyPromptDecisionQ.
func applyPromptUpsertQ(ctx context.Context, tx pgx.Tx, m domain.Mutation) error {
	// Empty sync_id guard — symmetric with the local store's applyPromptUpsertTx.
	// A malformed mutation with an empty sync_id must never reach the central table;
	// ON CONFLICT(sync_id) with '' would merge all empty-sync_id rows into one.
	if m.SyncID == "" {
		return fmt.Errorf("applyPromptUpsertQ: empty sync_id")
	}

	// Check for an existing tombstone. If one exists and this upsert is stale,
	// short-circuit with nil (the journal row is already committed; return success).
	var tombDeletedAt time.Time
	err := tx.QueryRow(ctx,
		`SELECT deleted_at FROM central_prompt_tombstones WHERE sync_id = $1`,
		m.SyncID,
	).Scan(&tombDeletedAt)

	if err != nil && err != pgx.ErrNoRows {
		return fmt.Errorf("applyPromptUpsertQ: read tombstone: %w", err)
	}

	tombstoneExists := err == nil
	if tombstoneExists {
		// Stale: incoming write is not strictly newer than the tombstone.
		// equal = stale, matching the local isStalePromptUpsert semantics.
		if !m.UpdatedAt.After(tombDeletedAt) {
			return nil // resurrection denied; journal row already committed above
		}
		// Fresh write wins — delete the tombstone to fully revive the prompt.
		if _, err := tx.Exec(ctx,
			`DELETE FROM central_prompt_tombstones WHERE sync_id = $1`,
			m.SyncID,
		); err != nil {
			return fmt.Errorf("applyPromptUpsertQ: delete tombstone: %w", err)
		}
	}

	// Upsert the central_user_prompts row. created_at is set from m.UpdatedAt
	// (the prompt's logical creation time) on INSERT; the ON CONFLICT DO UPDATE
	// preserves the original created_at (only mutable fields are updated).
	const upsertQ = `
		INSERT INTO central_user_prompts (sync_id, session_id, content, project, writer_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (sync_id) DO UPDATE SET
		  session_id = EXCLUDED.session_id,
		  content    = EXCLUDED.content,
		  project    = EXCLUDED.project,
		  writer_id  = EXCLUDED.writer_id`
	if _, err := tx.Exec(ctx, upsertQ,
		m.SyncID,
		m.SessionID,
		m.Content,
		m.Project,
		m.WriterID,
		m.UpdatedAt.UTC(),
	); err != nil {
		return fmt.Errorf("applyPromptUpsertQ: upsert: %w", err)
	}
	return nil
}

// applyPromptDeleteQ handles the delete branch of applyPromptDecisionQ.
func applyPromptDeleteQ(ctx context.Context, tx pgx.Tx, m domain.Mutation) error {
	if m.SyncID == "" {
		return fmt.Errorf("applyPromptDeleteQ: empty sync_id")
	}

	// Remove the live row (no-op if it never existed or was already deleted).
	if _, err := tx.Exec(ctx,
		`DELETE FROM central_user_prompts WHERE sync_id = $1`,
		m.SyncID,
	); err != nil {
		return fmt.Errorf("applyPromptDeleteQ: delete row: %w", err)
	}

	// Upsert the tombstone keyed by sync_id. ON CONFLICT (sync_id) DO UPDATE
	// makes a re-applied delete (idempotent re-pull) a no-op at the SQL layer,
	// mirroring the local store's applyPromptDeleteTx.
	const tombstoneQ = `
		INSERT INTO central_prompt_tombstones (sync_id, session_id, project, deleted_at, deleted_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (sync_id) DO UPDATE SET
		  session_id = EXCLUDED.session_id,
		  project    = EXCLUDED.project,
		  deleted_at = EXCLUDED.deleted_at,
		  deleted_by = EXCLUDED.deleted_by`
	if _, err := tx.Exec(ctx, tombstoneQ,
		m.SyncID,
		m.SessionID,
		m.Project,
		m.UpdatedAt.UTC(),
		m.WriterID,
	); err != nil {
		return fmt.Errorf("applyPromptDeleteQ: upsert tombstone: %w", err)
	}
	return nil
}
