package localstore

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// Review status values surfaced by ReviewStatus / ListForReview. These are
// computed at READ time — there is no stored status enum (see the proposal:
// review_after/expires_at are the only persisted lifecycle columns).
const (
	ReviewStatusActive      = "active"
	ReviewStatusNeedsReview = "needs_review"
	ReviewStatusExpired     = "expired"
)

// ReviewRow is the projection returned by ListForReview — the minimal set of
// fields a human or agent needs to triage stale memories.
type ReviewRow struct {
	ID          int64
	Title       string
	Type        string
	Project     string
	Status      string
	ReviewAfter *time.Time
}

// ReviewStatus computes the lifecycle status of a record at read time using the
// store's configured staleness window:
//
//   - expired      → expires_at is set and now > expires_at
//   - needs_review → now > COALESCE(review_after, updated_at + window)
//   - active       → otherwise
//
// review_after is set ONLY by MarkReviewed; on a normal save it is NULL, so a
// fresh memory is active until updated_at + window elapses. The window is the
// store's reviewWindowDays (default 30).
func (s *Store) ReviewStatus(rec *domain.Record) string {
	if rec == nil {
		return ReviewStatusActive
	}
	now := time.Now().UTC()

	if rec.ExpiresAt != nil && now.After(rec.ExpiresAt.UTC()) {
		return ReviewStatusExpired
	}

	// Due date: explicit review_after when present, else updated_at + window.
	var due time.Time
	if rec.ReviewAfter != nil {
		due = rec.ReviewAfter.UTC()
	} else {
		due = rec.UpdatedAt.UTC().AddDate(0, 0, s.reviewWindow())
	}
	if now.After(due) {
		return ReviewStatusNeedsReview
	}
	return ReviewStatusActive
}

// ReviewStatusForID reads the three lifecycle columns for one live row and
// computes its status. Used by handleGetObservation to surface a Status line
// without widening the shared record-scan column list. Returns ("", nil) when
// the row is missing or deleted (caller omits the Status line).
func (s *Store) ReviewStatusForID(id int64) (string, error) {
	var reviewAfter, expiresAt sql.NullString
	var updatedAt string
	err := s.db.QueryRow(
		`SELECT updated_at, review_after, expires_at
		 FROM memories WHERE id = ? AND deleted_at IS NULL`,
		id,
	).Scan(&updatedAt, &reviewAfter, &expiresAt)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("ReviewStatusForID(%d): %w", id, err)
	}

	rec := &domain.Record{UpdatedAt: parseTime(updatedAt)}
	if reviewAfter.Valid {
		t := parseTime(reviewAfter.String)
		rec.ReviewAfter = &t
	}
	if expiresAt.Valid {
		t := parseTime(expiresAt.String)
		rec.ExpiresAt = &t
	}
	return s.ReviewStatus(rec), nil
}

// ListForReview returns live rows matching the requested status — oldest-updated
// first for needs_review/expired (staleness triage), newest-first for active/all.
// status filter: "needs_review" (default) | "active" | "expired" | "all".
// project filters to one project when non-empty (normalized). limit caps the
// result count (default 50, max 200).
//
// Status is computed per row in Go from the persisted columns — there is no
// stored enum — so the filter is applied after the scan. The row scan is cheap
// (id/title/type/project + the three lifecycle columns).
func (s *Store) ListForReview(status, project string, limit int) ([]ReviewRow, error) {
	status = strings.TrimSpace(strings.ToLower(status))
	if status == "" {
		status = ReviewStatusNeedsReview
	}
	switch status {
	case ReviewStatusActive, ReviewStatusNeedsReview, ReviewStatusExpired, "all":
	default:
		return nil, fmt.Errorf("ListForReview: invalid status %q (want needs_review|active|expired|all)", status)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	project = normalizeProject(project)

	// We scan in order, compute status per row, and stop once `limit` matches are
	// collected. For staleness triage (needs_review/expired) order OLDEST-updated
	// first so the early-exit keeps the MOST-stale rows, not the least; for
	// active/all order newest-first where recency is what the caller wants.
	q := `SELECT id, title, type, project, updated_at, review_after, expires_at
	      FROM memories
	      WHERE deleted_at IS NULL`
	args := []any{}
	if project != "" {
		q += ` AND project = ?`
		args = append(args, project)
	}
	if status == ReviewStatusNeedsReview || status == ReviewStatusExpired {
		q += ` ORDER BY updated_at ASC, id ASC`
	} else {
		q += ` ORDER BY updated_at DESC, id DESC`
	}

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListForReview: query: %w", err)
	}
	defer rows.Close()

	var out []ReviewRow
	for rows.Next() {
		var (
			id                   int64
			title, typ, proj     string
			updatedAt            string
			reviewAfter, expires sql.NullString
		)
		if err := rows.Scan(&id, &title, &typ, &proj, &updatedAt, &reviewAfter, &expires); err != nil {
			return nil, fmt.Errorf("ListForReview: scan: %w", err)
		}
		rec := &domain.Record{UpdatedAt: parseTime(updatedAt)}
		var reviewAfterT *time.Time
		if reviewAfter.Valid {
			t := parseTime(reviewAfter.String)
			rec.ReviewAfter = &t
			reviewAfterT = &t
		}
		if expires.Valid {
			t := parseTime(expires.String)
			rec.ExpiresAt = &t
		}
		st := s.ReviewStatus(rec)
		if status != "all" && st != status {
			continue
		}
		out = append(out, ReviewRow{
			ID:          id,
			Title:       title,
			Type:        typ,
			Project:     proj,
			Status:      st,
			ReviewAfter: reviewAfterT,
		})
		if len(out) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListForReview: rows: %w", err)
	}
	return out, nil
}

// MarkReviewed stamps review_after = now + window on each live row in ids,
// resetting its staleness clock. Returns the number of rows updated. ids that
// are unknown or already deleted are silently skipped (not an error).
//
// This is a LOCAL-ONLY write: it sets per-node lifecycle metadata and does NOT
// go through LocalWrite, so it enqueues no outbox entry and never syncs (review
// is a per-node judgment, not shared truth — see the proposal's Sync semantics).
// It holds s.mu for parity with other write paths.
func (s *Store) MarkReviewed(ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	window := s.reviewWindow()

	s.mu.Lock()
	defer s.mu.Unlock()

	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}

	// datetime('now','+<N> days') computes the new due date in SQLite UTC, the
	// same clock used by the created_at/updated_at defaults.
	q := fmt.Sprintf(
		`UPDATE memories
		 SET review_after = datetime('now', ?)
		 WHERE id IN (%s) AND deleted_at IS NULL`,
		strings.Join(placeholders, ","),
	)
	// Prepend the interval modifier as the first bound arg.
	allArgs := make([]any, 0, len(args)+1)
	allArgs = append(allArgs, fmt.Sprintf("+%d days", window))
	allArgs = append(allArgs, args...)

	res, err := s.db.Exec(q, allArgs...)
	if err != nil {
		return 0, fmt.Errorf("MarkReviewed: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// IDByTopicKey resolves the integer primary key of the live memory row for the
// given (topic_key, project, scope), used by mem_review's topic_key →
// mark_reviewed path. scope defaults to "project" when empty; project is
// normalized. Returns ErrObservationNotFound when no live row matches.
func (s *Store) IDByTopicKey(topicKey, project, scope string) (int64, error) {
	if strings.TrimSpace(topicKey) == "" {
		return 0, fmt.Errorf("IDByTopicKey: topic_key must not be empty")
	}
	if scope == "" {
		scope = "project"
	}
	project = normalizeProject(project)

	var id int64
	err := s.db.QueryRow(
		`SELECT id FROM memories
		 WHERE topic_key = ? AND project = ? AND scope = ? AND deleted_at IS NULL
		 LIMIT 1`,
		topicKey, project, scope,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, ErrObservationNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("IDByTopicKey(%q): %w", topicKey, err)
	}
	return id, nil
}

// DistinctProjects returns the distinct non-empty project names across all live
// memory rows, used by the save-time name-drift warning (Feature 2). Order is
// unspecified; callers treat it as a set.
func (s *Store) DistinctProjects() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT project FROM memories
		 WHERE project <> '' AND deleted_at IS NULL`,
	)
	if err != nil {
		return nil, fmt.Errorf("DistinctProjects: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("DistinctProjects: scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
