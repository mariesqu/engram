package localstore

// created_at_backfill.go implements FUP-005c's local half: applying a page of
// original-creation-time entries the syncer fetched from central's
// /v1/created-at, and tracking per-project completion so the backfill runs
// exactly once per project (see created_at_backfill's schema note, v16→v17).

import (
	"fmt"

	"github.com/mariesqu/engram/internal/domain"
)

// BackfillCreatedAt applies a page of entries: for each sync_id with a LIVE
// local row, sets created_at to entry.CreatedAt ONLY when that candidate is
// STRICTLY OLDER than the row's current created_at — "the older of the two"
// (FUP-005c). A sync_id this node has never pulled (no local row) is silently
// skipped: there is nothing here for it to correct. entries is
// []domain.CreatedAtEntry — the same shared, transport-agnostic type
// centralstore.Store.OriginalCreatedAt and remote.Client.OriginalCreatedAt
// both return (see domain.CreatedAtEntry's doc comment), so the syncer's
// backfill driver hands this straight through with no local conversion.
//
// review_after is deliberately left UNTOUCHED. FUP-005c's brief asks that it
// be recomputed "only for rows that were never reviewed" — but this store has
// no reviewed/never-reviewed MARKER independent of review_after itself:
// MarkReviewed (review.go) overwrites review_after with the exact same
// decay-window formula execInsert uses, so a freshly-inserted row and a
// freshly-re-reviewed row are indistinguishable from the stored value alone.
// Recomputing here without that marker could silently UNDO an operator's own
// mark_reviewed call. Since no such marker exists, this backfill does not
// touch review_after at all, matching the brief's documented fallback.
//
// Returns the number of rows actually updated (0 is not an error — every
// candidate may already hold the older value, or name nothing local).
func (s *Store) BackfillCreatedAt(entries []domain.CreatedAtEntry) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("BackfillCreatedAt: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	updated := 0
	for _, e := range entries {
		candidate := e.CreatedAt.UTC().Format(sqliteTimeLayout)
		res, err := tx.Exec(
			`UPDATE memories
			   SET created_at = ?
			 WHERE sync_id = ? AND datetime(?) < datetime(created_at)`,
			candidate, e.SyncID, candidate,
		)
		if err != nil {
			return 0, fmt.Errorf("BackfillCreatedAt: sync_id=%s: %w", e.SyncID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("BackfillCreatedAt: sync_id=%s: rows affected: %w", e.SyncID, err)
		}
		updated += int(n)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("BackfillCreatedAt: commit: %w", err)
	}
	return updated, nil
}

// CreatedAtBackfillDone reports whether the one-time created_at backfill has
// already completed for project — a row in created_at_backfill.
func (s *Store) CreatedAtBackfillDone(project string) (bool, error) {
	project = normalizeProject(project)
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM created_at_backfill WHERE project = ?`, project,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("CreatedAtBackfillDone(%q): %w", project, err)
	}
	return n > 0, nil
}

// MarkCreatedAtBackfillDone records that project's created_at backfill has
// completed, so it is never re-attempted. INSERT OR IGNORE makes a repeat
// call (a race between two sync cycles, or an explicit re-run) a harmless
// no-op rather than an error.
func (s *Store) MarkCreatedAtBackfillDone(project string) error {
	project = normalizeProject(project)
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.db.Exec(
		`INSERT OR IGNORE INTO created_at_backfill(project) VALUES (?)`, project,
	); err != nil {
		return fmt.Errorf("MarkCreatedAtBackfillDone(%q): %w", project, err)
	}
	return nil
}
