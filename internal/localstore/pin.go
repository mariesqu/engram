package localstore

import (
	"database/sql"
	"fmt"
	"strings"
)

// pin.go implements the LOCAL-ONLY pinned flag behind mem_pin / mem_unpin.
//
// "Local-only" is the whole design, not a limitation. Pinning answers "what do I
// want in front of me on THIS machine", which is a per-node judgment exactly
// like review_after: my laptop's pins have no business reordering a teammate's
// context. So `pinned` is absent from mutation.CanonicalPayload — it never
// reaches the wire, never reaches central, and no reconciliation path reads it.
// Nothing in internal/domain/reconcile.go changes because this column exists.
//
// The two things pinning DOES affect are both read-side:
//   - FTS ranking: a small multiplicative boost in the ORDER BY (see search.go).
//   - mem_context: pinned rows render in their own section, ahead of recents.

// SetPinned sets or clears the pinned flag on one live memory row and returns
// the resulting state. Returns ErrObservationNotFound when id names no live row
// — a pin that silently succeeded against a deleted or non-existent memory would
// leave the caller believing something is pinned that can never surface.
//
// This does NOT go through LocalWrite: there is no mutation to journal, no
// outbox entry, and nothing to push. It is a direct typed UPDATE, the same shape
// as MarkReviewed.
func (s *Store) SetPinned(id int64, pinned bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	value := 0
	if pinned {
		value = 1
	}

	res, err := s.db.Exec(
		`UPDATE memories SET pinned = ? WHERE id = ? AND deleted_at IS NULL`,
		value, id,
	)
	if err != nil {
		return false, fmt.Errorf("SetPinned(%d): %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("SetPinned(%d): rows affected: %w", id, err)
	}
	if n == 0 {
		return false, ErrObservationNotFound
	}
	return pinned, nil
}

// CountPinned returns how many live rows are pinned for project/scope, using the
// same predicates PinnedObservations selects with so the two can never disagree.
// Empty project or scope disables that filter, matching every other read here.
//
// It exists for FormatContext's overflow line: the pinned section is capped, and
// a cap that silently swallows the rows past it would leave the caller believing
// they are seeing everything they pinned. Counting is a separate query rather
// than "ask for cap+1 rows and look at the length" because the exact number is
// what makes the line worth printing — "and 14 more" is actionable, "and some
// more" is not.
func (s *Store) CountPinned(project, scope string) (int, error) {
	q := `SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND pinned = 1`
	args := []any{}
	if p := normalizeProject(project); p != "" {
		q += ` AND LOWER(project) = ?`
		args = append(args, p)
	}
	if scope = strings.ToLower(strings.TrimSpace(scope)); scope != "" {
		q += ` AND scope = ?`
		args = append(args, scope)
	}

	var n int
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("CountPinned: %w", err)
	}
	return n, nil
}

// IsPinned reports the pinned state of one live row. Returns
// ErrObservationNotFound when id names no live row.
func (s *Store) IsPinned(id int64) (bool, error) {
	var pinned bool
	err := s.db.QueryRow(
		`SELECT pinned FROM memories WHERE id = ? AND deleted_at IS NULL`, id,
	).Scan(&pinned)
	if err == sql.ErrNoRows {
		return false, ErrObservationNotFound
	}
	if err != nil {
		return false, fmt.Errorf("IsPinned(%d): %w", id, err)
	}
	return pinned, nil
}
