package localstore

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ManualSaveSessionPrefix is the prefix of the session id a write tool invents
// when the caller supplies none: "manual-save-{project}". Nothing ever registers
// such a session — it exists to group saves made outside a tracked session, and
// it is the documented default of mem_save, mem_save_prompt and
// mem_session_summary.
//
// It lives here, in the package that owns session identity, because two very
// different places have to agree on it: the MCP write handlers that MINT the id
// (cmd/engram/tools.go) and the diagnostic query that must not report it as an
// orphan (diagnostic.go). When those two disagreed, every manual save produced
// a permanent doctor warning about the store's own default.
const ManualSaveSessionPrefix = "manual-save-"

// DefaultManualSessionID returns the session id a write defaults to for project.
//
// The project name is NORMALIZED first, exactly as the memories row will store
// it (AddObservation, AddPrompt). Without that, a caller who named their
// project "MyRepo" got a row under project "myrepo" carrying the session id
// "manual-save-MyRepo" — a default that does not match its own project, which
// OrphanedSessions then has to report as an unregistered session id somebody
// invented. Minting the id from the same value the row keeps is what makes that
// query an exact match instead of a fuzzy prefix.
func DefaultManualSessionID(project string) string {
	return ManualSaveSessionPrefix + normalizeProject(project)
}

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

	// LastActivityAt is the DERIVED moment this session was last doing anything:
	// the latest of started_at, ended_at, and the created_at of its newest live
	// memory. It is computed in SQL by RecentSessions, not stored — there is no
	// last_activity column, deliberately (see RecentSessions).
	//
	// It is what RecentSessions orders by and what FormatContext shows, because
	// "most recent session" means the one the user was last working in, not the
	// one that happened to be registered last.
	LastActivityAt time.Time
}

// ErrSessionNotFound is returned by GetSession when the id does not exist.
var ErrSessionNotFound = errors.New("session not found")

// normalizeProject lowercases and trims the project name, collapsing repeated
// hyphens and underscores. Mirrors the legacy predecessor's NormalizeProject
// semantics without the warning return value, which is unused by localstore
// callers.
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

// CreateSession upserts a session row from a DETECTED project. If a session
// with the same id already exists, project and directory are updated only when
// they were previously empty — matching the legacy predecessor's
// createSessionTx semantics (REQ-308: re-detection must never clobber the
// project a session was registered under).
//
// For a project the CALLER named explicitly, use CreateSessionWithProject.
func (s *Store) CreateSession(id, project, directory string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.createSessionLocked(id, project, directory, false)
}

// CreateSessionWithProject is CreateSession for a project the caller supplied
// EXPLICITLY, and it is the one case allowed to overwrite a populated project:
// re-registering a known id under a named project CORRECTS the stored row.
//
// Why the asymmetry with CreateSession: REQ-308 guards against re-DETECTION
// clobbering a good project (the daemon's cwd flip-flopping between calls). An
// explicit argument is not detection — it is the user telling us the stored
// value is wrong, and it is the only lever they have, since `engram connect`
// suppresses the directory injection as soon as a project is present. Without
// this, the documented "just re-run mem_session_start with project=X" fix for a
// misfiled session is a silent no-op.
//
// directory keeps CreateSession's fill-when-empty rule: a corrective call
// typically carries no directory of its own (injection suppressed, so the
// handler falls back to the daemon's cwd), and overwriting a good stored path
// with that would trade one wrong field for another.
//
// Sessions are LOCAL-ONLY — they are not journaled through internal/mutation
// and never reach internal/syncer (see EndSessionAt, which relies on the same
// property) — so a corrected project produces no mutation and needs no sync.
func (s *Store) CreateSessionWithProject(id, project, directory string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.createSessionLocked(id, project, directory, true)
}

// createSessionLocked is the shared upsert. The caller must hold mu.
//
// forceProject switches only the ON CONFLICT project clause. It is ignored when
// the normalized project is empty: a blank explicit project is no correction at
// all, and letting it through would erase a good stored value.
func (s *Store) createSessionLocked(id, project, directory string, forceProject bool) error {
	project = normalizeProject(project)

	projectClause := `CASE WHEN sessions.project = '' THEN excluded.project ELSE sessions.project END`
	if forceProject && project != "" {
		projectClause = `excluded.project`
	}

	_, err := s.db.Exec(
		`INSERT INTO sessions (id, project, directory) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   project   = `+projectClause+`,
		   directory = CASE WHEN sessions.directory = '' THEN excluded.directory ELSE sessions.directory END`,
		id, project, directory,
	)
	return err
}

// EndSession records ended_at = now and stores the summary for the given
// session id. If the session does not exist the call is a no-op (returns nil),
// mirroring the legacy predecessor's EndSession behaviour: rows==0 is not an
// error.
func (s *Store) EndSession(id, summary string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// An UPDATE affecting zero rows (unknown id) is intentionally NOT an error —
	// it is a no-op, mirroring the legacy predecessor's EndSession.
	_, err := s.db.Exec(
		`UPDATE sessions SET ended_at = datetime('now'), summary = ? WHERE id = ?`,
		nullableStr(summary), id,
	)
	return err
}

// EndSessionAt is EndSession with a caller-supplied end timestamp — used by
// the legacy importer to PRESERVE the original ended_at instead of stamping
// import wall-clock time (sessions are local-only, not journaled, so a typed
// direct update is the correct path). Zero-row updates are a no-op like
// EndSession.
func (s *Store) EndSessionAt(id, summary string, endedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`UPDATE sessions SET ended_at = ?, summary = ? WHERE id = ?`,
		endedAt.UTC().Format("2006-01-02 15:04:05"), nullableStr(summary), id,
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
// ordered by LAST ACTIVITY DESC with id DESC as a deterministic tie-breaker.
// If project is empty all projects are included. limit <= 0 defaults to 5.
//
// Last activity is derived, not stored: the latest of started_at, ended_at (set
// together with the summary by EndSession, so it covers "the session was closed
// out at T"), and the created_at of the session's newest live memory. Ordering
// by started_at — what this did before — ranked a session registered an hour ago
// and never used above a session opened yesterday that has been saving memories
// all morning. The second one is the context the agent actually needs back.
//
// Why derived rather than a last_activity column: a stored column would need a
// write on every memory insert, and it would be one more value that can drift out
// of sync with the rows it summarizes. The MAX() below is computed from the
// memories themselves, so it cannot be wrong.
//
// The derivation is a DERIVED TABLE, not a correlated subquery, and that is the
// whole cost story. A correlated `(SELECT MAX(created_at) ... WHERE session_id =
// s.id)` in the ORDER BY is evaluated once per session ROW — before LIMIT, which
// cannot bound a sort key — so it is O(sessions × memories), not "bounded by
// limit sessions" as this comment used to claim: 300 sessions over 20k memories
// measured 1.39s, and FormatContext (which calls this first) 1.58s. The grouped
// join below computes every session's newest memory in ONE pass over memories
// and measures ~20ms on the same fixture (see
// TestRecentSessions_ScalesToThreeHundredSessions).
//
// The SHAPE is the win here, not the index. The grouped pass still visits every
// live memory; idx_mem_session (schema v14) only lets SQLite walk them already
// in session order — "SCAN memories USING INDEX idx_mem_session" in the plan —
// instead of sorting them for the GROUP BY. That saves a sort, not the
// O(sessions × memories) evaluation: the correlated subquery had to go for that.
// Where the index measurably pays for itself is the OTHER half of the mem_context
// path, FormatContext's per-session observation COUNT — one indexed lookup per
// returned session instead of a scan each, ~26% off FormatContext at this
// fixture size.
func (s *Store) RecentSessions(project string, limit int) ([]SessionSummary, error) {
	project = normalizeProject(project)
	if limit <= 0 {
		limit = 5
	}

	query, args := recentSessionsQuery(project, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SessionSummary
	for rows.Next() {
		var ss SessionSummary
		var startedRaw, endedRaw, summaryRaw, lastActivityRaw sql.NullString
		if err := rows.Scan(&ss.ID, &ss.Project, &startedRaw, &endedRaw, &summaryRaw, &lastActivityRaw); err != nil {
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
		// Fall back to StartedAt when the derived value is unparseable, so a data
		// anomaly degrades to the OLD behaviour instead of a zero timestamp.
		ss.LastActivityAt = ss.StartedAt
		if lastActivityRaw.Valid {
			if t, err := parseSessionTime(lastActivityRaw.String); err == nil {
				ss.LastActivityAt = t
			}
		}
		results = append(results, ss)
	}
	return results, rows.Err()
}

// recentSessionsQuery builds RecentSessions' statement and its args. project is
// expected ALREADY normalized ("" disables the filter) and limit already
// defaulted — this is the SQL, not the argument handling.
//
// It is a separate function so the scale test can run EXPLAIN QUERY PLAN on the
// EXACT statement that ships, rather than on a copy that can drift away from it.
// A plan assertion over a re-typed query proves nothing about production.
func recentSessionsQuery(project string, limit int) (string, []any) {
	// MAX() here is SQLite's SCALAR max (3 arguments) over three datetime()
	// strings; the single-argument MAX inside the derived table is the AGGREGATE.
	// The two forms stay unambiguous because the aggregate lives in its own
	// subquery — now a GROUPED one that runs once, instead of a correlated one
	// that runs per session row (see the doc comment for the numbers).
	//
	// datetime() wraps the aggregate's argument, not just its result, and it is
	// defensive: MAX() over raw text is a LEXICAL max, so a column holding a mix
	// of SQLite's 'YYYY-MM-DD HH:MM:SS' and RFC3339 with a 'T' and a 'Z' compares
	// wrong — '2024-01-02T…' sorts ABOVE '2024-06-10 …' because 'T' > ' ', and a
	// January row would win over a June one. datetime() normalizes both storage
	// formats to the same comparable text first.
	const lastActivityExpr = `MAX(
	            datetime(s.started_at),
	            datetime(COALESCE(s.ended_at, s.started_at)),
	            datetime(COALESCE(lm.last_created, s.started_at))
	          )`

	query := `SELECT s.id, s.project, s.started_at, s.ended_at, s.summary,
	                 ` + lastActivityExpr + ` AS last_activity_at
	          FROM sessions s
	          LEFT JOIN (
	            SELECT session_id, MAX(datetime(created_at)) AS last_created
	            FROM memories
	            WHERE deleted_at IS NULL
	            GROUP BY session_id
	          ) lm ON lm.session_id = s.id
	          WHERE 1=1`
	args := []any{}
	if project != "" {
		query += " AND LOWER(s.project) = ?"
		args = append(args, project)
	}
	query += " ORDER BY datetime(last_activity_at) DESC, s.id DESC LIMIT ?"
	args = append(args, limit)

	return query, args
}

// nullableStr converts an empty string to a SQL NULL so that summary="" is
// stored as NULL rather than an empty string, matching the legacy
// predecessor's behaviour.
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
