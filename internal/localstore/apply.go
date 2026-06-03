package localstore

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// Apply executes a domain.Decision against the local SQLite store inside a
// single transaction. It is the pull-apply adapter: callers (the sync loop)
// invoke Decide() first, then pass the returned Decision here.
//
// NoOp is a valid input — Apply returns nil immediately.
// For all other actions, the applied_mutations row is written so future
// Decide() calls detect idempotent re-apply (Invariant 5).
//
// The Decision contract enriches the bare Action with:
//   - TargetSyncID: the resolved row's sync_id (may differ from m.SyncID when
//     resolved via topic_key — fixes the P1-a silent write-loss bug).
//   - Undelete: when true, the adapter clears deleted_at on the memories row
//     AND removes the memory_tombstones entry, making the record live again
//     (fixes the P1-b tombstone-undelete omission).
func Apply(db *sql.DB, d domain.Decision, m domain.Mutation) error {
	if d.Action == domain.NoOp {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("Apply: begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if err = applyTx(tx, d, m); err != nil {
		return err
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("Apply: commit: %w", err)
	}
	return nil
}

// applyTx executes the decision switch and applied_mutations INSERT against an
// existing transaction. It is the tx-aware core used by both Apply (pull-apply)
// and LocalWrite (local write + outbox enqueue in ONE atomic transaction).
//
// The caller is responsible for beginning and committing (or rolling back) the
// transaction. applyTx itself never calls Begin, Commit, or Rollback.
func applyTx(tx *sql.Tx, d domain.Decision, m domain.Mutation) error {
	var err error

	switch d.Action {
	case domain.ActionInsert:
		// Decide returns ActionInsert only when cur == nil — no memories row exists.
		// This includes the tombstone-only state (a tombstone exists but the memories
		// row was never written or was already purged). In that state,
		// execUndeleteUpdate would UPDATE 0 rows and silently drop the write.
		// We always INSERT here; the execClearTombstone block below removes any
		// stale tombstone, leaving the new row live and fully visible.
		err = execInsert(tx, m)
	case domain.ActionUpdate:
		// P1-a fix: use d.TargetSyncID (the resolved row) — not m.SyncID.
		err = execUpdate(tx, d.TargetSyncID, m)
		if err == nil && d.Undelete {
			// The resolved row was soft-deleted; clear it.
			err = execClearDeletedAt(tx, d.TargetSyncID)
		}
	case domain.ActionWriteTombstone:
		// P1-a fix (cross-writer delete case): use d.TargetSyncID (the resolved row's sync_id, set by Decide)
		// rather than m.SyncID. For same-writer deletes these are identical; for
		// cross-writer deletes TargetSyncID is cur.SyncID (the row found via
		// FindByTopic), ensuring the UPDATE and tombstone INSERT address the actual
		// stored row and not a non-existent incoming sync_id.
		err = execWriteTombstone(tx, d.TargetSyncID, m)
	default:
		err = fmt.Errorf("applyTx: unknown action %d", d.Action)
	}
	if err != nil {
		return err
	}

	// P1-b fix: when undeleting, remove the tombstone row so the record is
	// fully live and FindByTopic / SearchMemories return it again.
	if d.Undelete {
		if err = execClearTombstone(tx, d.TargetSyncID, m); err != nil {
			return err
		}
	}

	// Record applied mutation for idempotency (INV 5).
	if m.MutationID != "" {
		_, err = tx.Exec(
			`INSERT OR IGNORE INTO applied_mutations(mutation_id) VALUES (?)`,
			m.MutationID,
		)
		if err != nil {
			return fmt.Errorf("applyTx: record applied: %w", err)
		}
	}

	return nil
}

func execInsert(tx *sql.Tx, m domain.Mutation) error {
	_, err := tx.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content,
		   project, scope, topic_key, parent_sync_id, status,
		   version, seq, writer_id, last_write_mutation_id, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.SyncID, m.SessionID, string(m.EntityType), m.Type, m.Title, m.Content,
		m.Project, m.Scope, nullStr(m.TopicKey), nullStr(m.ParentSyncID), nullStr(m.Status),
		m.Version, m.Seq, m.WriterID, m.MutationID,
		m.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("execInsert: %w", err)
	}
	return nil
}

// execUpdate overwrites the existing record identified by targetSyncID.
// P1-a fix: targetSyncID is the RESOLVED row's sync_id from Decision.TargetSyncID,
// which may differ from m.SyncID when resolved via FindByTopic.
func execUpdate(tx *sql.Tx, targetSyncID string, m domain.Mutation) error {
	_, err := tx.Exec(`
		UPDATE memories
		SET title=?, content=?, type=?, status=?, topic_key=?, parent_sync_id=?,
		    version=?, seq=?, writer_id=?, last_write_mutation_id=?, updated_at=?
		WHERE sync_id=?`,
		m.Title, m.Content, m.Type, nullStr(m.Status), nullStr(m.TopicKey), nullStr(m.ParentSyncID),
		m.Version, m.Seq, m.WriterID, m.MutationID,
		m.UpdatedAt.UTC().Format(time.RFC3339Nano),
		targetSyncID,
	)
	if err != nil {
		return fmt.Errorf("execUpdate: %w", err)
	}
	return nil
}

// execClearDeletedAt clears the soft-delete flag on the row identified by
// targetSyncID, making it visible to FindByTopic and SearchMemories again.
func execClearDeletedAt(tx *sql.Tx, targetSyncID string) error {
	_, err := tx.Exec(
		`UPDATE memories SET deleted_at=NULL WHERE sync_id=?`,
		targetSyncID,
	)
	if err != nil {
		return fmt.Errorf("execClearDeletedAt: %w", err)
	}
	return nil
}

// execClearTombstone removes the tombstone entry for the revived record.
// Looks up by sync_id (primary) and also by topic_key+project+scope when a
// topic_key is present, ensuring stale topic-keyed tombstones are also gone.
func execClearTombstone(tx *sql.Tx, targetSyncID string, m domain.Mutation) error {
	// Remove by sync_id first (covers both topic-keyed and topic-less records).
	_, err := tx.Exec(
		`DELETE FROM memory_tombstones WHERE sync_id=?`,
		targetSyncID,
	)
	if err != nil {
		return fmt.Errorf("execClearTombstone (by sync_id): %w", err)
	}
	// Also remove any topic-key tombstone that covers the same identity.
	if m.TopicKey != nil && *m.TopicKey != "" {
		_, err = tx.Exec(
			`DELETE FROM memory_tombstones WHERE topic_key=? AND project=? AND scope=?`,
			*m.TopicKey, m.Project, m.Scope,
		)
		if err != nil {
			return fmt.Errorf("execClearTombstone (by topic_key): %w", err)
		}
	}
	return nil
}

// execWriteTombstone soft-deletes the memories row identified by targetSyncID
// and writes the corresponding tombstone row.
//
// targetSyncID is the RESOLVED row's sync_id from Decision.TargetSyncID.
// For same-writer deletes this equals m.SyncID; for cross-writer deletes
// (where FindByTopic resolved a different row Y) this is cur.SyncID (Y).
// Using targetSyncID for BOTH the UPDATE and the tombstone INSERT ensures the
// correct row is addressed and the tombstone covers the actual deleted identity.
//
// Metadata (project, scope, topic_key, version, writer_id) always comes from m
// because the DELETE mutation carries the authoritative deletion context.
func execWriteTombstone(tx *sql.Tx, targetSyncID string, m domain.Mutation) error {
	now := m.UpdatedAt.UTC().Format(time.RFC3339Nano)

	// Set deleted_at on the resolved memories row. seq on the memories row is
	// retained (it carries the last-applied central seq for cursor/audit purposes)
	// but is NOT updated here — it is not used in the LWW tiebreaker. The winning
	// delete's content-addressed mutation_id IS stamped (last_write_mutation_id)
	// because that is the final LWW tiebreaker (see writeWins doc comment).
	_, err := tx.Exec(
		`UPDATE memories SET deleted_at=?, version=?, writer_id=?, last_write_mutation_id=? WHERE sync_id=?`,
		now, m.Version, m.WriterID, m.MutationID, targetSyncID,
	)
	if err != nil {
		return fmt.Errorf("execWriteTombstone: update memories: %w", err)
	}

	// Insert tombstone row keyed by targetSyncID so FindTombstone by sync_id
	// and by topic_key both resolve to the correct deleted identity.
	// deleted_by (writer_id) and last_write_mutation_id (the winning delete's
	// content-addressed id) are the identity tiebreaker fields used by writeWins;
	// both are replica-identical (see writeWins doc comment), unlike sync_id (PK).
	_, err = tx.Exec(`
		INSERT OR REPLACE INTO memory_tombstones
		  (sync_id, project, scope, topic_key, deleted_at, deleted_by, version, last_write_mutation_id)
		VALUES (?,?,?,?,?,?,?,?)`,
		targetSyncID, m.Project, m.Scope, nullStr(m.TopicKey),
		now, m.WriterID, m.Version, m.MutationID,
	)
	if err != nil {
		return fmt.Errorf("execWriteTombstone: insert tombstone: %w", err)
	}
	return nil
}

// nullStr converts a *string to sql.NullString for nullable column binding.
func nullStr(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *p, Valid: true}
}
