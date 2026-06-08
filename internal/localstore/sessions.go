package localstore

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Session represents a tracked MCP coding session.
type Session struct {
	ID        string
	Project   string
	Directory string
	StartedAt time.Time
	EndedAt   *time.Time
	Summary   *string
}

// SessionSummary is a lightweight view of a session returned by RecentSessions.
// It omits the directory field to keep result sets small.
type SessionSummary struct {
	ID        string
	Project   string
	StartedAt time.Time
	EndedAt   *time.Time
	Summary   *string
}

// ErrSessionNotFound is returned by GetSession when the id does not exist.
var ErrSessionNotFound = errors.New("session not found")

// normalizeProject lowercases and trims the project name, collapsing repeated
// hyphens and underscores. Mirrors old_code NormalizeProject semantics without
// the warning return value, which is unused by localstore callers.
func normalizeProject(project string) string {
	n := strings.TrimSpace(strings.ToLower(project))
	for strings.Contains(n, "--") {
		n = strings.ReplaceAll(n, "--", "-")
	}
	for strings.Contains(n, "__") {
		n = strings.ReplaceAll(n, "__", "_")
	}
	return n
}

// CreateSession upserts a session row. If a session with the same id already
// exists, project and directory are updated only when they were previously
// empty — matching old_code createSessionTx semantics (REQ-308: no overwrite
// of a populated project with a new one).
func (s *Store) CreateSession(id, project, directory string) error {
	project = normalizeProject(project)
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, project, directory) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   project   = CASE WHEN sessions.project = '' THEN excluded.project ELSE sessions.project END,
		   directory = CASE WHEN sessions.directory = '' THEN excluded.directory ELSE sessions.directory END`,
		id, project, directory,
	)
	return err
}

// EndSession records ended_at = now and stores the summary for the given
// session id. If the session does not exist the call is a no-op (returns nil),
// mirroring old_code EndSession behaviour: rows==0 is not an error.
func (s *Store) EndSession(id, summary string) error {
	// An UPDATE affecting zero rows (unknown id) is intentionally NOT an error —
	// it is a no-op, mirroring old_code EndSession.
	_, err := s.db.Exec(
		`UPDATE sessions SET ended_at = datetime('now'), summary = ? WHERE id = ?`,
		nullableStr(summary), id,
	)
	return err
}

// GetSession fetches the full session row for id. Returns ErrSessionNotFound
// when no row exists.
func (s *Store) GetSession(id string) (*Session, error) {
	row := s.db.QueryRow(
		`SELECT id, project, directory, started_at, ended_at, summary
		 FROM sessions WHERE id = ?`,
		id,
	)
	var sess Session
	var startedRaw, endedRaw, summaryRaw sql.NullString
	if err := row.Scan(
		&sess.ID, &sess.Project, &sess.Directory,
		&startedRaw, &endedRaw, &summaryRaw,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSessionNotFound
		}
		return nil, err
	}
	if t, err := parseSessionTime(startedRaw.String); err == nil {
		sess.StartedAt = t
	}
	if endedRaw.Valid && endedRaw.String != "" {
		if t, err := parseSessionTime(endedRaw.String); err == nil {
			sess.EndedAt = &t
		}
	}
	if summaryRaw.Valid {
		sess.Summary = &summaryRaw.String
	}
	return &sess, nil
}

// RecentSessions returns the most recent sessions for the given project,
// ordered by started_at DESC with id DESC as a deterministic tie-breaker.
// If project is empty all projects are included. limit <= 0 defaults to 5.
//
// TODO(PR4+): old_code orders by MAX(COALESCE(obs.created_at, started_at)) DESC
// (latest ACTIVITY). This uses started_at DESC as a simplification until the
// observations table exists — revisit the ORDER BY when it lands.
func (s *Store) RecentSessions(project string, limit int) ([]SessionSummary, error) {
	project = normalizeProject(project)
	if limit <= 0 {
		limit = 5
	}

	query := `SELECT id, project, started_at, ended_at, summary
	          FROM sessions WHERE 1=1`
	args := []any{}
	if project != "" {
		query += " AND LOWER(project) = ?"
		args = append(args, project)
	}
	query += " ORDER BY datetime(started_at) DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SessionSummary
	for rows.Next() {
		var ss SessionSummary
		var startedRaw, endedRaw, summaryRaw sql.NullString
		if err := rows.Scan(&ss.ID, &ss.Project, &startedRaw, &endedRaw, &summaryRaw); err != nil {
			return nil, err
		}
		if summaryRaw.Valid {
			ss.Summary = &summaryRaw.String
		}
		if t, err := parseSessionTime(startedRaw.String); err == nil {
			ss.StartedAt = t
		}
		if endedRaw.Valid && endedRaw.String != "" {
			if t, err := parseSessionTime(endedRaw.String); err == nil {
				ss.EndedAt = &t
			}
		}
		results = append(results, ss)
	}
	return results, rows.Err()
}

// nullableStr converts an empty string to a SQL NULL so that summary="" is
// stored as NULL rather than an empty string, matching old_code behaviour.
func nullableStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// parseSessionTime parses a SQLite datetime string ("2006-01-02 15:04:05" or
// "2006-01-02T15:04:05Z"). Returns the zero time on parse failure — callers
// treat an unpopulated StartedAt as a data anomaly and continue.
func parseSessionTime(s string) (time.Time, error) {
	for _, layout := range []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("unparseable session time: " + s)
}
