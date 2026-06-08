package localstore

import (
	"fmt"
	"strings"

	"github.com/mariesqu/engram/internal/domain"
)

// SearchFilter carries the optional filter parameters for SearchMemoriesFiltered.
// All fields are optional — a zero SearchFilter is equivalent to calling
// SearchMemories with no filters other than project and limit.
type SearchFilter struct {
	// Type filters by observation type, e.g. "decision", "bugfix", "architecture".
	// Empty means "any type".
	Type string

	// Scope filters by scope: "project" or "personal".
	// Empty means "any scope".
	Scope string

	// TopicKey filters to a specific topic_key value.
	// Empty means "any topic".
	TopicKey string
}

// SearchMemoriesFiltered is an extended version of SearchMemories that accepts
// optional type/scope/topic_key filters in addition to project and limit.
//
// Existing SearchMemories callers are preserved unchanged; this function is the
// canonical entry point for the mem_search handler which needs all filters.
//
// Filter semantics (all ANDed, all optional):
//   - project: LOWER(project) = lower(project) — case-insensitive; empty = all
//   - type:    exact match on the type column
//   - scope:   exact match on the scope column (normalized to lower)
//   - limit:   defaults to 10; max is not enforced here (caller is responsible)
//
// FTS injection prevention: the query string is passed through sanitizeFTS
// (wraps each token in double-quotes) before reaching the FTS5 engine —
// the same defence used by SearchMemories.
func (s *Store) SearchMemoriesFiltered(query, project string, limit int, f SearchFilter) ([]*domain.Record, error) {
	if limit <= 0 {
		limit = 10
	}
	ftsQ := sanitizeFTS(query)
	if ftsQ == "" {
		return nil, nil
	}

	// Build the SQL query dynamically based on which filters are active.
	// Base query joins FTS shadow table to memories for full column access.
	q := `
		SELECT m.id, m.sync_id, m.session_id, m.entity_type, m.type, m.title, m.content,
		       m.project, m.scope, m.version, m.writer_id, m.last_write_mutation_id,
		       m.topic_key, m.status, m.parent_sync_id,
		       m.created_at, m.updated_at, m.deleted_at
		FROM memories_fts fts
		JOIN memories m ON m.id = fts.rowid
		WHERE memories_fts MATCH ?
		  AND m.deleted_at IS NULL`
	args := []any{ftsQ}

	if project != "" {
		q += "\n  AND LOWER(m.project) = ?"
		args = append(args, strings.ToLower(strings.TrimSpace(project)))
	}
	if f.Type != "" {
		q += "\n  AND m.type = ?"
		args = append(args, f.Type)
	}
	if f.Scope != "" {
		q += "\n  AND m.scope = ?"
		args = append(args, strings.ToLower(strings.TrimSpace(f.Scope)))
	}
	if f.TopicKey != "" {
		q += "\n  AND m.topic_key = ?"
		args = append(args, f.TopicKey)
	}

	q += "\nORDER BY fts.rank\nLIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("SearchMemoriesFiltered: %w", err)
	}
	defer rows.Close()

	var results []*domain.Record
	for rows.Next() {
		r, err := scanRecordWithIDFromRows(rows)
		if err != nil {
			return nil, fmt.Errorf("SearchMemoriesFiltered scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// RecentObservations returns the most recent live (non-deleted) memories ordered
// by created_at DESC, id DESC. project and scope are optional filters; an empty
// string disables the filter for that dimension. limit <= 0 defaults to 20.
//
// Mirrors old_code store.RecentObservations (project+scope variant).
func (s *Store) RecentObservations(project, scope string, limit int) ([]*domain.Record, error) {
	if limit <= 0 {
		limit = 20
	}
	project = normalizeProject(project)

	q := `
		SELECT id, sync_id, session_id, entity_type, type, title, content,
		       project, scope, version, writer_id, last_write_mutation_id,
		       topic_key, status, parent_sync_id,
		       created_at, updated_at, deleted_at
		FROM memories
		WHERE deleted_at IS NULL`
	args := []any{}

	if project != "" {
		q += "\n  AND LOWER(project) = ?"
		args = append(args, project)
	}
	if scope != "" {
		q += "\n  AND scope = ?"
		args = append(args, strings.ToLower(strings.TrimSpace(scope)))
	}

	q += "\nORDER BY datetime(created_at) DESC, id DESC\nLIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("RecentObservations: %w", err)
	}
	defer rows.Close()

	var results []*domain.Record
	for rows.Next() {
		r, err := scanRecordWithIDFromRows(rows)
		if err != nil {
			return nil, fmt.Errorf("RecentObservations scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// truncateStr truncates s to at most n runes. Used by FormatContext to produce
// bounded preview text.
func truncateStr(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

// FormatContext assembles the agent-facing memory context blob from recent
// sessions and recent observations, mirroring old_code store.FormatContext.
//
// Format (faithful to old_code):
//
//	## Memory from Previous Sessions
//
//	### Recent Sessions
//	- **project** (started_at)[: summary] [N observations]
//
//	### Recent Observations
//	- [type] **title**: content_preview
//
// Returns an empty string when there are no sessions and no observations.
// project and scope are optional — empty string means "all".
func (s *Store) FormatContext(project, scope string) (string, error) {
	sessions, err := s.RecentSessions(project, 5)
	if err != nil {
		return "", fmt.Errorf("FormatContext: RecentSessions: %w", err)
	}

	observations, err := s.RecentObservations(project, scope, 20)
	if err != nil {
		return "", fmt.Errorf("FormatContext: RecentObservations: %w", err)
	}

	if len(sessions) == 0 && len(observations) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("## Memory from Previous Sessions\n\n")

	if len(sessions) > 0 {
		b.WriteString("### Recent Sessions\n")
		for _, sess := range sessions {
			summary := ""
			if sess.Summary != nil && *sess.Summary != "" {
				summary = fmt.Sprintf(": %s", truncateStr(*sess.Summary, 200))
			}
			// Count observations for this session. We use a best-effort query;
			// errors are silently ignored to avoid failing FormatContext on a
			// non-critical count.
			var obsCount int
			_ = s.db.QueryRow(
				`SELECT count(*) FROM memories WHERE session_id = ? AND deleted_at IS NULL`,
				sess.ID,
			).Scan(&obsCount)

			fmt.Fprintf(&b, "- **%s** (%s)%s [%d observations]\n",
				sess.Project,
				sess.StartedAt.UTC().Format("2006-01-02 15:04:05"),
				summary,
				obsCount,
			)
		}
		b.WriteString("\n")
	}

	if len(observations) > 0 {
		b.WriteString("### Recent Observations\n")
		for _, obs := range observations {
			fmt.Fprintf(&b, "- [%s] **%s**: %s\n",
				obs.Type, obs.Title, truncateStr(obs.Content, 300))
		}
		b.WriteString("\n")
	}

	return b.String(), nil
}
