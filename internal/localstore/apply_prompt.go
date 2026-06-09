package localstore

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// applyPromptTx materializes a domain.Mutation with EntityType==EntityPrompt
// into user_prompts / prompt_tombstones within the caller-supplied transaction.
// The caller owns Begin / Commit / Rollback.
//
// # Dispatch contract
//
// This function is the ONLY write path for prompt mutations.  It is invoked
// from both localWriteLocked and ApplyPulled BEFORE domain.Decide / applyTx,
// so a prompt mutation can NEVER reach the memories apply path (which carries
// a defense-in-depth guard that would reject it with a routing error).
//
// # Idempotency / INV5
//
// The upsert branch uses ON CONFLICT(sync_id) DO UPDATE, making a re-applied
// upsert a no-op at the SQL layer.  It also writes to applied_mutations
// (INSERT OR IGNORE) to satisfy the same INV5 contract that memories use —
// this makes re-pulled prompts consistent with memories: Decide's
// MutationApplied check (used by the memory path) would detect the row and
// produce NoOp; prompts bypass Decide, so we write the marker directly here.
//
// # Tombstone / resurrection guard
//
// Delete: DELETE the live user_prompts row and UPSERT prompt_tombstones keyed
// by sync_id.  deleted_at = m.UpdatedAt (the mutation's logical timestamp).
//
// Upsert: check prompt_tombstones for this sync_id.  If a tombstone exists and
// the incoming mutation is STALE (m.UpdatedAt <= tombstone.deleted_at) the
// write is a no-op — the prompt was deleted more recently than this upsert.
// If the upsert is FRESH it wins: DELETE the tombstone (revive) then upsert
// the user_prompts row.
func applyPromptTx(tx *sql.Tx, m domain.Mutation) error {
	switch m.Op {
	case domain.OpUpsert:
		return applyPromptUpsertTx(tx, m)
	case domain.OpDelete:
		return applyPromptDeleteTx(tx, m)
	default:
		return fmt.Errorf("applyPromptTx: unknown op %q for prompt mutation (sync_id=%s)", m.Op, m.SyncID)
	}
}

// applyPromptUpsertTx handles the upsert branch of applyPromptTx.
func applyPromptUpsertTx(tx *sql.Tx, m domain.Mutation) error {
	// Symmetric with applyPromptDeleteTx: never ON CONFLICT(sync_id) on an empty
	// sync_id (which would merge all empty-sync_id prompts into one row). Reachable
	// only via a malformed external/pulled mutation; AddPrompt always sets one.
	if m.SyncID == "" {
		return fmt.Errorf("applyPromptUpsertTx: empty sync_id")
	}

	// Check for a tombstone. If one exists, apply the staleness guard.
	var tombDeletedAtStr string
	err := tx.QueryRow(
		`SELECT deleted_at FROM prompt_tombstones WHERE sync_id = ?`, m.SyncID,
	).Scan(&tombDeletedAtStr)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("applyPromptUpsertTx: read tombstone: %w", err)
	}

	tombstoneExists := err == nil
	if tombstoneExists {
		if isStalePromptUpsert(m, tombDeletedAtStr) {
			// The tombstone is newer than this upsert — resurrection denied.
			// Still record the mutation as applied for INV5 idempotency.
			return recordAppliedMutation(tx, m.MutationID)
		}
		// The upsert is fresher — delete the tombstone to revive the prompt.
		if _, err := tx.Exec(`DELETE FROM prompt_tombstones WHERE sync_id = ?`, m.SyncID); err != nil {
			return fmt.Errorf("applyPromptUpsertTx: delete tombstone: %w", err)
		}
	}

	// Upsert the user_prompts row.  created_at is set from m.UpdatedAt (the
	// prompt's logical creation time carried in the mutation) on INSERT; the
	// DO UPDATE preserves the original created_at (mutable fields only).
	createdAt := m.UpdatedAt.UTC().Format(time.RFC3339Nano)
	_, err = tx.Exec(`
		INSERT INTO user_prompts (sync_id, session_id, content, project, writer_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(sync_id) DO UPDATE SET
		  session_id = excluded.session_id,
		  content    = excluded.content,
		  project    = excluded.project,
		  writer_id  = excluded.writer_id`,
		m.SyncID, m.SessionID, m.Content, m.Project, m.WriterID, createdAt,
	)
	if err != nil {
		return fmt.Errorf("applyPromptUpsertTx: upsert: %w", err)
	}

	return recordAppliedMutation(tx, m.MutationID)
}

// applyPromptDeleteTx handles the delete branch of applyPromptTx.
func applyPromptDeleteTx(tx *sql.Tx, m domain.Mutation) error {
	if m.SyncID == "" {
		return fmt.Errorf("applyPromptDeleteTx: empty sync_id")
	}

	// Remove the live row (no-op if it never existed or was already gone).
	if _, err := tx.Exec(`DELETE FROM user_prompts WHERE sync_id = ?`, m.SyncID); err != nil {
		return fmt.Errorf("applyPromptDeleteTx: delete row: %w", err)
	}

	// Upsert the tombstone keyed by sync_id.  ON CONFLICT updates all mutable
	// fields so a re-applied delete (idempotent re-pull) is a no-op at the SQL
	// layer, mirroring the memory tombstone pattern in execWriteTombstone.
	deletedAt := m.UpdatedAt.UTC().Format(time.RFC3339Nano)
	_, err := tx.Exec(`
		INSERT INTO prompt_tombstones (sync_id, session_id, project, deleted_at, deleted_by)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(sync_id) DO UPDATE SET
		  session_id = excluded.session_id,
		  project    = excluded.project,
		  deleted_at = excluded.deleted_at,
		  deleted_by = excluded.deleted_by`,
		m.SyncID, m.SessionID, m.Project, deletedAt, m.WriterID,
	)
	if err != nil {
		return fmt.Errorf("applyPromptDeleteTx: upsert tombstone: %w", err)
	}

	return recordAppliedMutation(tx, m.MutationID)
}

// isStalePromptUpsert returns true when the incoming upsert should be rejected
// because a tombstone for the same sync_id is at least as recent.
//
// The comparison is: m.UpdatedAt <= tombstone.deleted_at (lexicographic on
// RFC3339Nano / SQLite datetime strings is equivalent to chronological order
// because both formats are monotone-ASCII-sortable when stored in UTC).
// An empty or unparseable tombstone timestamp is treated as "always fresh" —
// the upsert wins (fail open toward data preservation).
func isStalePromptUpsert(m domain.Mutation, tombDeletedAtStr string) bool {
	tombDeletedAt := parseTime(tombDeletedAtStr)
	if tombDeletedAt.IsZero() {
		// Unparseable tombstone timestamp: treat as stale-guard disabled (upsert wins).
		return false
	}
	// Stale: incoming upsert is NOT strictly newer than the tombstone.
	return !m.UpdatedAt.After(tombDeletedAt)
}

// recordAppliedMutation writes mutation_id to applied_mutations (INSERT OR
// IGNORE) so that both the memory path (via Decide.MutationApplied) and the
// prompt path share the same INV5 idempotency table.  A nil or empty
// mutation_id is silently skipped (normalizeMutation guarantees it is set on
// all real writes; guard here is defense-in-depth).
func recordAppliedMutation(tx *sql.Tx, mutationID string) error {
	if mutationID == "" {
		return nil
	}
	_, err := tx.Exec(
		`INSERT OR IGNORE INTO applied_mutations(mutation_id) VALUES (?)`,
		mutationID,
	)
	if err != nil {
		return fmt.Errorf("recordAppliedMutation: %w", err)
	}
	return nil
}
