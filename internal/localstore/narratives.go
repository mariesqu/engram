package localstore

import (
	"fmt"
	"strings"
	"time"
)

// NarrativeRow is the local-store representation of one row in the
// narratives table (schema v13) — a LOCAL-ONLY cache of LLM-generated
// narrative prose for one (project, topic_prefix) unit. It never syncs
// (obsidian-narrative decision #4751 decision 2): each machine regenerates
// its own narratives, and this type carries no sync_id, writer_id, or
// domain.EntityType.
type NarrativeRow struct {
	Project         string
	TopicPrefix     string
	Body            string
	SourceHash      string
	Model           string
	TemplateVersion string
	RendererVersion string
	SourceCount     int
	SourceWriters   string // comma-joined, sorted — see narrativesTableDDL
	UnverifiedPaths string // newline-joined — see narrativesTableDDL
	Truncated       bool
	Stale           bool
	GeneratedAt     time.Time
}

// narrativeFoldKey folds project and topic_prefix the same way the table's
// stored primary key is folded: strings.ToLower, NOT COLLATE NOCASE (which
// folds ASCII only) — see narrativesTableDDL's doc comment in schema.go.
// The NUL separator mirrors the "lower(project)\x00lower(topic_prefix)"
// convention the design specifies for both narrative readers.
func narrativeFoldKey(project, topicPrefix string) string {
	return strings.ToLower(project) + "\x00" + strings.ToLower(topicPrefix)
}

// UpsertNarrative inserts or overwrites the row for (row.Project,
// row.TopicPrefix), folded via strings.ToLower. A second upsert on the same
// folded key OVERWRITES the existing row rather than duplicating it — the
// table carries exactly one narrative per unit.
func (s *Store) UpsertNarrative(row NarrativeRow) error {
	project := strings.ToLower(row.Project)
	topicPrefix := strings.ToLower(row.TopicPrefix)

	_, err := s.db.Exec(`
		INSERT INTO narratives (
			project, topic_prefix, body, source_hash, model, template_version,
			renderer_version, source_count, source_writers, unverified_paths,
			truncated, stale, generated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project, topic_prefix) DO UPDATE SET
			body             = excluded.body,
			source_hash      = excluded.source_hash,
			model            = excluded.model,
			template_version = excluded.template_version,
			renderer_version = excluded.renderer_version,
			source_count     = excluded.source_count,
			source_writers   = excluded.source_writers,
			unverified_paths = excluded.unverified_paths,
			truncated        = excluded.truncated,
			stale            = excluded.stale,
			generated_at     = excluded.generated_at`,
		project, topicPrefix, row.Body, row.SourceHash, row.Model, row.TemplateVersion,
		row.RendererVersion, row.SourceCount, row.SourceWriters, row.UnverifiedPaths,
		boolToNarrativeInt(row.Truncated), boolToNarrativeInt(row.Stale),
		row.GeneratedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("UpsertNarrative %q/%q: %w", project, topicPrefix, err)
	}
	return nil
}

// NarrativeCacheKeys returns every stored row's folded key
// ("lower(project)\x00lower(topic_prefix)") mapped to its source_hash. This
// backs the narrative loop's cache-miss test: ONE full-table scan per cycle,
// deliberately not a per-unit GetNarrative, which would add roughly one
// query per topic prefix behind SetMaxOpenConns(1).
func (s *Store) NarrativeCacheKeys() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT project, topic_prefix, source_hash FROM narratives`)
	if err != nil {
		return nil, fmt.Errorf("NarrativeCacheKeys: query: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var project, topicPrefix, hash string
		if err := rows.Scan(&project, &topicPrefix, &hash); err != nil {
			return nil, fmt.Errorf("NarrativeCacheKeys: scan: %w", err)
		}
		out[narrativeFoldKey(project, topicPrefix)] = hash
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("NarrativeCacheKeys: rows: %w", err)
	}
	return out, nil
}

// NarrativesForExport returns every stored row keyed by its folded key,
// carrying every field the exporter's render path needs. ONE full-table
// scan per cycle — see NarrativeCacheKeys.
func (s *Store) NarrativesForExport() (map[string]NarrativeRow, error) {
	rows, err := s.db.Query(`
		SELECT project, topic_prefix, body, source_hash, model, template_version,
			renderer_version, source_count, source_writers, unverified_paths,
			truncated, stale, generated_at
		FROM narratives`)
	if err != nil {
		return nil, fmt.Errorf("NarrativesForExport: query: %w", err)
	}
	defer rows.Close()

	out := make(map[string]NarrativeRow)
	for rows.Next() {
		var row NarrativeRow
		var truncated, stale int
		var generatedAt string
		if err := rows.Scan(
			&row.Project, &row.TopicPrefix, &row.Body, &row.SourceHash, &row.Model,
			&row.TemplateVersion, &row.RendererVersion, &row.SourceCount,
			&row.SourceWriters, &row.UnverifiedPaths, &truncated, &stale, &generatedAt,
		); err != nil {
			return nil, fmt.Errorf("NarrativesForExport: scan: %w", err)
		}
		row.Truncated = truncated != 0
		row.Stale = stale != 0
		row.GeneratedAt = parseTime(generatedAt)
		out[narrativeFoldKey(row.Project, row.TopicPrefix)] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("NarrativesForExport: rows: %w", err)
	}
	return out, nil
}

// MarkNarrativesStale sets stale=1 on every row whose folded key
// ("lower(project)\x00lower(topic_prefix)") appears in keys, leaving every
// other row untouched (stale stays whatever it already was). keys are split
// back into (project, topic_prefix) pairs for the WHERE clause.
//
// An empty keys slice issues NO statement at all — a cold or fully-cached
// cycle that defers nothing must not touch the database.
func (s *Store) MarkNarrativesStale(keys []string) error {
	if len(keys) == 0 {
		return nil
	}

	placeholders := make([]string, 0, len(keys))
	args := make([]any, 0, len(keys)*2)
	for _, k := range keys {
		project, topicPrefix, ok := strings.Cut(k, "\x00")
		if !ok {
			return fmt.Errorf("MarkNarrativesStale: malformed key %q (want \"project\\x00topic_prefix\")", k)
		}
		placeholders = append(placeholders, "(?, ?)")
		args = append(args, project, topicPrefix)
	}

	q := fmt.Sprintf(
		`UPDATE narratives SET stale = 1 WHERE (project, topic_prefix) IN (%s)`,
		strings.Join(placeholders, ", "),
	)
	if _, err := s.db.Exec(q, args...); err != nil {
		return fmt.Errorf("MarkNarrativesStale: %w", err)
	}
	return nil
}

// boolToNarrativeInt converts a Go bool to the 0/1 representation stored in
// the narratives table's INTEGER columns (truncated, stale).
func boolToNarrativeInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
