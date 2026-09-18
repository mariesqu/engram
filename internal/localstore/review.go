package localstore

import (
	"database/sql"
	"fmt"
	"sort"
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

// decayReviewAfterMonths maps an observation type to how many months it stays
// trustworthy before it should be re-checked. A type absent from this map gets
// review_after = NULL and falls back to the store's rolling
// updated_at + reviewWindowDays (see ReviewStatus) — the map is an override for
// the handful of types whose useful life is measured in months, not weeks.
//
// The three entries are deliberate, not a sample:
//   - decision (6mo)   — a decision goes stale when the thing it decided moves.
//   - policy (12mo)    — policy is meant to outlive the work it governs.
//   - preference (3mo) — a stated preference is the most volatile of the three.
//
// "policy" and "preference" are not in the type list mem_save's description
// enumerates; type is a free-form column and callers do pass them. A map miss is
// safe by construction, so listing them costs nothing and catches them when they
// appear.
var decayReviewAfterMonths = map[string]int{
	"decision":   6,
	"policy":     12,
	"preference": 3,
}

// sqliteTimeLayout is the text format SQLite's datetime() produces, and the one
// every timestamp column in this schema is written in. parseTime accepts it.
const sqliteTimeLayout = "2006-01-02 15:04:05"

// reviewAfterForType returns the review_after value for a row of the given type,
// as an `any` ready to bind to a SQL parameter: a formatted timestamp for a type
// in the decay map, or nil (SQL NULL) for one that is not.
//
// Callers pass the reference instant explicitly so the value is testable and so
// the insert path can date the window from the row's own creation moment.
func reviewAfterForType(typ string, now time.Time) any {
	months, ok := decayReviewAfterMonths[typ]
	if !ok {
		return nil
	}
	return now.UTC().AddDate(0, months, 0).Format(sqliteTimeLayout)
}

// ReviewStatus computes the lifecycle status of a record at read time using the
// store's configured staleness window:
//
//   - expired      → expires_at is set and now > expires_at
//   - needs_review → now > COALESCE(review_after, updated_at + window)
//   - active       → otherwise
//
// review_after is set at INSERT time for types in decayReviewAfterMonths and
// reset by MarkReviewed; for every other type it stays NULL and the row is
// active until updated_at + window elapses. The window is the store's
// reviewWindowDays (default 30).
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

// MarkReviewed resets the staleness clock on each live row in ids, recomputing
// review_after FROM THE ROW'S OWN TYPE using the same decayReviewAfterMonths map
// the insert path uses — so re-reviewing a decision buys another 6 months and
// re-reviewing a preference another 3, rather than every type getting the same
// flat window. Returns the number of rows updated. ids that are unknown or
// already deleted are silently skipped (not an error).
//
// DEVIATION from the upstream implementation this is ported from: upstream sets
// review_after = NULL for a type that has no decay entry, because upstream reads
// "needs review" as `review_after IS NOT NULL AND review_after < now` — NULL
// there means "never due". Under THIS store's read-time derivation NULL means
// "fall back to updated_at + window" (see ReviewStatus), and MarkReviewed does
// not touch updated_at, so writing NULL would make marking a bugfix reviewed a
// silent no-op: the row would come straight back as needs_review. Upstream
// compensates by bumping updated_at, which is not an option here — updated_at is
// the LWW ordering field (see writeWins in domain/reconcile.go), and inflating it
// on a local, un-journaled write would let this node wrongly beat a genuinely
// newer remote write on the next pull. So a type with no decay entry gets
// now + reviewWindowDays: the same "clock reset" semantics, expressed in the
// only field this store is allowed to move.
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
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("MarkReviewed: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	// The new due date depends on each row's TYPE, so the types have to be read
	// before anything can be written — but that is ONE read and one write per
	// distinct type, not per id. Marking a 200-id page reviewed used to cost 400
	// round-trips through the SQLite driver; grouped it costs one SELECT plus at
	// most a handful of UPDATEs, since a realistic batch spans two or three types.
	idsByType, err := liveTypesOf(tx, ids)
	if err != nil {
		return 0, err
	}

	// Sort the type keys so the statement order is deterministic — Go map
	// iteration is not, and a write batch that reorders itself between runs is
	// needless nondeterminism in a transaction.
	types := make([]string, 0, len(idsByType))
	for typ := range idsByType {
		types = append(types, typ)
	}
	sort.Strings(types)

	updated := 0
	for _, typ := range types {
		due := reviewAfterForType(typ, now)
		if due == nil {
			// No decay entry — fall back to the store's rolling window. See the
			// DEVIATION note above for why this is not NULL.
			due = now.AddDate(0, 0, window).Format(sqliteTimeLayout)
		}

		group := idsByType[typ]
		args := make([]any, 0, len(group)+1)
		args = append(args, due)
		for _, id := range group {
			args = append(args, id)
		}

		res, err := tx.Exec(
			`UPDATE memories SET review_after = ?
			 WHERE id IN (`+sqlPlaceholders(len(group))+`) AND deleted_at IS NULL`,
			args...,
		)
		if err != nil {
			return 0, fmt.Errorf("MarkReviewed: update %d %s row(s): %w", len(group), typ, err)
		}
		n, _ := res.RowsAffected()
		updated += int(n)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("MarkReviewed: commit: %w", err)
	}
	return updated, nil
}

// sqlPlaceholders returns "?,?,…,?" for an IN clause of n values. n must be > 0;
// callers reach here only after an empty-ids early return.
func sqlPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// liveTypesOf reads the type of every LIVE row named in ids and returns the ids
// grouped by type, in one round-trip. Ids that name nothing live are simply
// absent from the result — MarkReviewed's contract is that unknown and
// soft-deleted ids are skipped, not an error, and "no row came back" says exactly
// that.
//
// A duplicated id collapses to one entry, because the row is what is being
// updated and a row can only be marked reviewed once. The per-id loop this
// replaces counted such an id twice in its return value.
func liveTypesOf(tx *sql.Tx, ids []int64) (map[string][]int64, error) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	rows, err := tx.Query(
		`SELECT id, type FROM memories
		 WHERE id IN (`+sqlPlaceholders(len(ids))+`) AND deleted_at IS NULL`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("MarkReviewed: read types: %w", err)
	}
	defer rows.Close()

	byType := make(map[string][]int64)
	for rows.Next() {
		var id int64
		var typ string
		if err := rows.Scan(&id, &typ); err != nil {
			return nil, fmt.Errorf("MarkReviewed: scan type: %w", err)
		}
		byType[typ] = append(byType[typ], id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("MarkReviewed: read types: %w", err)
	}
	return byType, nil
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
