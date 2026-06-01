package localstore

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// Apply executes a domain.Action against the local SQLite store inside a single
// transaction. It is the pull-apply adapter: callers (the sync loop) invoke
// Decide() first, then pass the returned Action here.
//
// NoOp is a valid input — Apply returns nil immediately.
// For all other actions, the applied_mutations row is written so future
// Decide() calls detect idempotent re-apply (Invariant 5).
func Apply(db *sql.DB, a domain.Action, m domain.Mutation) error {
	if a == domain.NoOp {
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

	switch a {
	case domain.ActionInsert:
		err = execInsert(tx, m)
	case domain.ActionUpdate:
		err = execUpdate(tx, m)
	case domain.ActionWriteTombstone:
		err = execWriteTombstone(tx, m)
	default:
		err = fmt.Errorf("Apply: unknown action %d", a)
	}
	if err != nil {
		return err
	}

	// Record applied mutation for idempotency (INV 5).
	if m.MutationID != "" {
		_, err = tx.Exec(
			`INSERT OR IGNORE INTO applied_mutations(mutation_id) VALUES (?)`,
			m.MutationID,
		)
		if err != nil {
			return fmt.Errorf("Apply: record applied: %w", err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("Apply: commit: %w", err)
	}
	return nil
}

func execInsert(tx *sql.Tx, m domain.Mutation) error {
	_, err := tx.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content,
		   project, scope, topic_key, parent_sync_id, status,
		   version, seq, writer_id, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.SyncID, m.SessionID, string(m.EntityType), m.Type, m.Title, m.Content,
		m.Project, m.Scope, nullStr(m.TopicKey), nullStr(m.ParentSyncID), nullStr(m.Status),
		m.Version, m.Seq, m.WriterID,
		m.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("execInsert: %w", err)
	}
	return nil
}

func execUpdate(tx *sql.Tx, m domain.Mutation) error {
	_, err := tx.Exec(`
		UPDATE memories
		SET title=?, content=?, type=?, status=?, topic_key=?, parent_sync_id=?,
		    version=?, seq=?, writer_id=?, updated_at=?
		WHERE sync_id=?`,
		m.Title, m.Content, m.Type, nullStr(m.Status), nullStr(m.TopicKey), nullStr(m.ParentSyncID),
		m.Version, m.Seq, m.WriterID,
		m.UpdatedAt.UTC().Format(time.RFC3339Nano),
		m.SyncID,
	)
	if err != nil {
		return fmt.Errorf("execUpdate: %w", err)
	}
	return nil
}

func execWriteTombstone(tx *sql.Tx, m domain.Mutation) error {
	now := m.UpdatedAt.UTC().Format(time.RFC3339Nano)

	// Set deleted_at on the memories row.
	_, err := tx.Exec(
		`UPDATE memories SET deleted_at=?, version=?, writer_id=? WHERE sync_id=?`,
		now, m.Version, m.WriterID, m.SyncID,
	)
	if err != nil {
		return fmt.Errorf("execWriteTombstone: update memories: %w", err)
	}

	// Insert tombstone row (atomically in same tx).
	_, err = tx.Exec(`
		INSERT OR REPLACE INTO memory_tombstones
		  (sync_id, project, scope, topic_key, deleted_at, deleted_by, version)
		VALUES (?,?,?,?,?,?,?)`,
		m.SyncID, m.Project, m.Scope, nullStr(m.TopicKey),
		now, m.WriterID, m.Version,
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
