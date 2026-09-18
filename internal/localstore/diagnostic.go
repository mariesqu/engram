package localstore

import (
	"context"
	"database/sql"
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

// OrphanedObservationSessions returns the session ids referenced by live
// observations but absent from the sessions table, newest-heaviest first.
//
// This is not a foreign-key violation — the FK was deliberately removed so an
// out-of-order sync pull can land an observation before its session (see the
// v0→v1 schema note). It IS a signal worth reporting: those observations can
// never be grouped back under the session that produced them.
func (s *Store) OrphanedObservationSessions(project string) ([]OrphanedSessionRef, error) {
	q := `
		SELECT m.session_id, COUNT(*) AS n, GROUP_CONCAT(DISTINCT m.project)
		FROM memories m
		LEFT JOIN sessions s ON s.id = m.session_id
		WHERE m.deleted_at IS NULL
		  AND s.id IS NULL
		  AND TRIM(m.session_id) <> ''`
	args := []any{}
	if project = normalizeProject(project); project != "" {
		q += ` AND m.project = ?`
		args = append(args, project)
	}
	q += ` GROUP BY m.session_id ORDER BY n DESC, m.session_id ASC`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("OrphanedObservationSessions: query: %w", err)
	}
	defer rows.Close()

	var out []OrphanedSessionRef
	for rows.Next() {
		var (
			ref      OrphanedSessionRef
			projects sql.NullString
		)
		if err := rows.Scan(&ref.SessionID, &ref.ObservationCount, &projects); err != nil {
			return nil, fmt.Errorf("OrphanedObservationSessions: scan: %w", err)
		}
		if projects.Valid && projects.String != "" {
			ref.Projects = strings.Split(projects.String, ",")
			sort.Strings(ref.Projects)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("OrphanedObservationSessions: rows: %w", err)
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

// SyncBacklog describes the unacked outbox: how many mutations are waiting to
// be pushed to central, and how old the oldest of them is.
type SyncBacklog struct {
	Pending int       `json:"pending"`
	Oldest  time.Time `json:"oldest_occurred_at"`
}

// SyncBacklog reads the outbound push journal (sync_mutations with no acked_at).
func (s *Store) SyncBacklog() (SyncBacklog, error) {
	var (
		backlog SyncBacklog
		oldest  sql.NullString
	)
	err := s.db.QueryRow(`
		SELECT COUNT(*), MIN(occurred_at)
		FROM sync_mutations
		WHERE acked_at IS NULL`).Scan(&backlog.Pending, &oldest)
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
