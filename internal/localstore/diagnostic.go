package localstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// diagnostic.go holds the READ-ONLY queries behind `mem_doctor`
// (internal/diagnostic). They live here, not in the diagnostic package, for the
// same reason every other query does: the schema is this package's business,
// and a diagnostic that grew its own copy of the table layout would keep
// reporting on a schema the store had already moved past.
//
// Every function here is a pure read. Doctor reports; it never repairs — a
// "fix" that runs unattended against memories nobody can reconstruct is worth
// less than an accurate description of what is wrong.

// DiagnosticSession is the session projection the doctor checks work from.
// EndedAt is nil for a session that is still open.
type DiagnosticSession struct {
	ID        string     `json:"session_id"`
	Project   string     `json:"project"`
	Directory string     `json:"directory"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// DiagnosticSessions returns sessions ordered by start time, newest first.
// An empty project returns every session.
func (s *Store) DiagnosticSessions(project string) ([]DiagnosticSession, error) {
	q := `SELECT id, project, directory, started_at, ended_at FROM sessions`
	args := []any{}
	if project = normalizeProject(project); project != "" {
		q += ` WHERE project = ?`
		args = append(args, project)
	}
	q += ` ORDER BY started_at DESC, id ASC`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("DiagnosticSessions: query: %w", err)
	}
	defer rows.Close()

	var out []DiagnosticSession
	for rows.Next() {
		var (
			sess               DiagnosticSession
			startedAt          string
			endedAt, directory sql.NullString
		)
		if err := rows.Scan(&sess.ID, &sess.Project, &directory, &startedAt, &endedAt); err != nil {
			return nil, fmt.Errorf("DiagnosticSessions: scan: %w", err)
		}
		sess.Directory = directory.String
		sess.StartedAt = parseTime(startedAt)
		if endedAt.Valid && strings.TrimSpace(endedAt.String) != "" {
			t := parseTime(endedAt.String)
			sess.EndedAt = &t
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("DiagnosticSessions: rows: %w", err)
	}
	return out, nil
}

// OrphanedSessionRef counts live observations that name a session id which has
// no row in sessions.
type OrphanedSessionRef struct {
	SessionID        string   `json:"session_id"`
	ObservationCount int      `json:"observation_count"`
	Projects         []string `json:"projects"`
}

// OrphanedSessions is the split result of ONE scan over the live observations
// whose session id has no row in sessions.
//
// Orphans is the signal: a session id nothing registered, so those observations
// can never be grouped back under the session that produced them. This is not a
// foreign-key violation — the FK was deliberately removed so an out-of-order
// sync pull can land an observation before its session (see the v0→v1 schema
// note) — but it IS worth reporting.
//
// Unregistered is the store's OWN default. A mem_save with no session_id is
// filed under "manual-save-{project}" (DefaultManualSessionID), a session id
// nothing ever registers — so every single manual save produced a permanent
// "orphaned session" warning about behaviour the tool description documents. A
// doctor that warns about its own defaults is a doctor whose warnings get
// skipped, and then the real ones go unread with them. Those rows are reported
// separately and informationally.
type OrphanedSessions struct {
	Orphans      []OrphanedSessionRef
	Unregistered []OrphanedSessionRef
}

// OrphanedSessions returns both classes of orphaned session reference in one
// pass, newest-heaviest first within each.
//
// One query, not two. The predicate that splits them is a CASE over the same
// rows, so running the scan twice with complementary WHERE clauses read the
// whole memories table twice to answer one question — on the store where it
// matters (a large one), doctor is exactly the caller you do not want doing
// that.
//
// The split is an EXACT match against the id the store itself would mint for
// the row's project, not a prefix LIKE. LIKE 'manual-save-%' is
// case-insensitive by default in SQLite and matches any tail, so
// "manual-save-other-project" sitting in project "engram" — a session id no
// code here produces — was silently reclassified as "the store's own default"
// and demoted to an informational note. Whatever wrote that row is precisely
// what an orphan warning is for. COLLATE BINARY keeps "MANUAL-SAVE-x" on the
// warning side too.
//
// The exactness relies on DefaultManualSessionID minting its id from the
// NORMALIZED project name, which is what the memories row stores.
//
// The projects column is json_group_array, not GROUP_CONCAT: GROUP_CONCAT joins
// on a comma and the caller split on one, so a project name containing a comma
// came back as two projects that do not exist.
func (s *Store) OrphanedSessions(project string) (OrphanedSessions, error) {
	const caller = "OrphanedSessions"

	// MIN, not MAX: a group is the store's own default only when EVERY row in it
	// is. One row of a shared session id that does NOT match its project's
	// default is the surprise a doctor exists to surface, and an info note is
	// where surprises go to be ignored.
	q := `
		SELECT m.session_id, COUNT(*) AS n, json_group_array(DISTINCT m.project),
		       MIN(CASE WHEN m.session_id = (? || m.project) COLLATE BINARY THEN 1 ELSE 0 END) AS is_default
		FROM memories m
		LEFT JOIN sessions s ON s.id = m.session_id
		WHERE m.deleted_at IS NULL
		  AND s.id IS NULL
		  AND TRIM(m.session_id) <> ''`
	args := []any{ManualSaveSessionPrefix}
	if project = normalizeProject(project); project != "" {
		q += ` AND m.project = ?`
		args = append(args, project)
	}
	q += ` GROUP BY m.session_id ORDER BY n DESC, m.session_id ASC`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return OrphanedSessions{}, fmt.Errorf("%s: query: %w", caller, err)
	}
	defer rows.Close()

	var out OrphanedSessions
	for rows.Next() {
		var (
			ref       OrphanedSessionRef
			projects  sql.NullString
			isDefault int
		)
		if err := rows.Scan(&ref.SessionID, &ref.ObservationCount, &projects, &isDefault); err != nil {
			return OrphanedSessions{}, fmt.Errorf("%s: scan: %w", caller, err)
		}
		if projects.Valid && strings.TrimSpace(projects.String) != "" {
			if err := json.Unmarshal([]byte(projects.String), &ref.Projects); err != nil {
				return OrphanedSessions{}, fmt.Errorf("%s: decode projects %q: %w", caller, projects.String, err)
			}
			sort.Strings(ref.Projects)
		}
		if isDefault == 1 {
			out.Unregistered = append(out.Unregistered, ref)
			continue
		}
		out.Orphans = append(out.Orphans, ref)
	}
	if err := rows.Err(); err != nil {
		return OrphanedSessions{}, fmt.Errorf("%s: rows: %w", caller, err)
	}
	return out, nil
}

// ProjectsWithoutPolicy returns the projects that own live memories but have no
// row in project_policy, sorted.
//
// A missing row is not a bug by itself: GetPolicy computes the default at read
// time (synced when central is configured, local-only otherwise). It matters
// when central IS configured, because the computed default is "synced" — so
// every one of these projects is being pushed off this machine on the strength
// of a default nobody chose.
func (s *Store) ProjectsWithoutPolicy(project string) ([]string, error) {
	q := `
		SELECT DISTINCT m.project
		FROM memories m
		LEFT JOIN project_policy p ON p.project = m.project
		WHERE m.deleted_at IS NULL
		  AND p.project IS NULL
		  AND TRIM(m.project) <> ''`
	args := []any{}
	if project = normalizeProject(project); project != "" {
		q += ` AND m.project = ?`
		args = append(args, project)
	}
	q += ` ORDER BY m.project ASC`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("ProjectsWithoutPolicy: query: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("ProjectsWithoutPolicy: scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ProjectsWithoutPolicy: rows: %w", err)
	}
	return out, nil
}

// ReviewCounts is the lifecycle breakdown of a project's live memories.
type ReviewCounts struct {
	Total       int `json:"total"`
	Active      int `json:"active"`
	NeedsReview int `json:"needs_review"`
	Expired     int `json:"expired"`
}

// CountByReviewStatus returns how many live memories are active, due for review,
// or expired. The status is computed per row (ReviewStatus), exactly as
// ListForReview computes it — it is not a stored column, so there is nothing to
// aggregate in SQL.
func (s *Store) CountByReviewStatus(project string) (ReviewCounts, error) {
	q := `SELECT updated_at, review_after, expires_at FROM memories WHERE deleted_at IS NULL`
	args := []any{}
	if project = normalizeProject(project); project != "" {
		q += ` AND project = ?`
		args = append(args, project)
	}

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return ReviewCounts{}, fmt.Errorf("CountByReviewStatus: query: %w", err)
	}
	defer rows.Close()

	var counts ReviewCounts
	for rows.Next() {
		var (
			updatedAt            string
			reviewAfter, expires sql.NullString
		)
		if err := rows.Scan(&updatedAt, &reviewAfter, &expires); err != nil {
			return ReviewCounts{}, fmt.Errorf("CountByReviewStatus: scan: %w", err)
		}
		rec := recordForReviewStatus(updatedAt, reviewAfter, expires)
		counts.Total++
		switch s.ReviewStatus(rec) {
		case ReviewStatusExpired:
			counts.Expired++
		case ReviewStatusNeedsReview:
			counts.NeedsReview++
		default:
			counts.Active++
		}
	}
	if err := rows.Err(); err != nil {
		return ReviewCounts{}, fmt.Errorf("CountByReviewStatus: rows: %w", err)
	}
	return counts, nil
}

// recordForReviewStatus rebuilds the minimal domain.Record ReviewStatus reads:
// the three lifecycle timestamps, nothing else.
func recordForReviewStatus(updatedAt string, reviewAfter, expires sql.NullString) *domain.Record {
	rec := &domain.Record{UpdatedAt: parseTime(updatedAt)}
	if reviewAfter.Valid {
		t := parseTime(reviewAfter.String)
		rec.ReviewAfter = &t
	}
	if expires.Valid {
		t := parseTime(expires.String)
		rec.ExpiresAt = &t
	}
	return rec
}

// SyncBacklog describes the unacked, UN-PARKED outbox: how many mutations are
// genuinely still waiting for the next push cycle to pick them up, and how old
// the oldest of them is.
//
// Parked rows (FUP-004b) are deliberately EXCLUDED: a parked entry is not
// "waiting for the next tick" — DrainOutbox has stopped returning it, so no
// amount of waiting drains it. Counting it here would make ParkedMutationsCheck's
// finding look like it is also silently shrinking the backlog every cycle,
// when nothing is actually moving. See ParkedMutationsCheck for parked rows'
// own check.
type SyncBacklog struct {
	Pending int       `json:"pending"`
	Oldest  time.Time `json:"oldest_occurred_at"`
}

// SyncBacklog reads the outbound push journal (sync_mutations with no
// acked_at and no parked_at — see SyncBacklog's own doc comment for why
// parked rows are excluded).
func (s *Store) SyncBacklog() (SyncBacklog, error) {
	var (
		backlog SyncBacklog
		oldest  sql.NullString
	)
	err := s.db.QueryRow(`
		SELECT COUNT(*), MIN(occurred_at)
		FROM sync_mutations
		WHERE acked_at IS NULL AND parked_at IS NULL`).Scan(&backlog.Pending, &oldest)
	if err != nil {
		return SyncBacklog{}, fmt.Errorf("SyncBacklog: query: %w", err)
	}
	if oldest.Valid && strings.TrimSpace(oldest.String) != "" {
		backlog.Oldest = parseTime(oldest.String)
	}
	return backlog, nil
}

// SQLiteLockSnapshot is the evidence behind the lock-contention check.
type SQLiteLockSnapshot struct {
	// BusyTimeoutMS is how long a writer waits for the lock before failing.
	// Zero means a concurrent writer fails IMMEDIATELY with SQLITE_BUSY.
	BusyTimeoutMS int `json:"busy_timeout_ms"`
	// CheckpointBusy is 1 when the WAL checkpoint could not run because another
	// connection held the database — the cheapest true signal of contention
	// available without taking a write lock ourselves.
	CheckpointBusy int `json:"checkpoint_busy"`
	// WALPages / Checkpointed describe the WAL at probe time. A WAL that keeps
	// growing while Checkpointed stays at 0 is a reader that never lets go.
	WALPages     int `json:"wal_pages"`
	Checkpointed int `json:"checkpointed_pages"`
}

// SQLiteLockSnapshot probes the database for lock contention WITHOUT taking a
// write lock: a PASSIVE checkpoint yields immediately when anyone else holds
// the database, which is the answer the doctor needs and the opposite of what a
// probe that blocked would tell you.
func (s *Store) SQLiteLockSnapshot(ctx context.Context) (SQLiteLockSnapshot, error) {
	var snap SQLiteLockSnapshot

	if err := s.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&snap.BusyTimeoutMS); err != nil {
		return SQLiteLockSnapshot{}, fmt.Errorf("SQLiteLockSnapshot: busy_timeout: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).
		Scan(&snap.CheckpointBusy, &snap.WALPages, &snap.Checkpointed); err != nil {
		return SQLiteLockSnapshot{}, fmt.Errorf("SQLiteLockSnapshot: wal_checkpoint: %w", err)
	}
	return snap, nil
}

// CentralConfigured reports whether this node is wired to a central server.
// Several diagnostics are only meaningful on one side of that line: an unpushed
// outbox is a backlog with central configured and simply unused without it.
func (s *Store) CentralConfigured() bool {
	return s.isCentralConfigured != nil && s.isCentralConfigured()
}
