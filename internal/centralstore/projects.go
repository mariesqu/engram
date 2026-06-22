package centralstore

import (
	"context"
	"fmt"
)

// ListProjects returns the distinct set of project names central knows, derived
// from the authoritative central_mutations journal. The result is sorted
// ascending and excludes the empty project.
//
// This is the server side of new-project pull discovery: a node can learn about
// projects that originated on OTHER writers and were never written locally, so
// the autosync loop can pull them too (see syncer.SyncAllProjects). The journal
// (not the materialized central_memories) is the source of truth because it
// records every project that has ever synced, including ones whose only live
// state is a tombstone.
func (s *Store) ListProjects(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT project FROM central_mutations WHERE project <> '' ORDER BY project`)
	if err != nil {
		return nil, fmt.Errorf("centralstore.ListProjects: query: %w", err)
	}
	defer rows.Close()

	var projects []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("centralstore.ListProjects: scan: %w", err)
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("centralstore.ListProjects: rows: %w", err)
	}
	return projects, nil
}
