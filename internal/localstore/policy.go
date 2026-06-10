package localstore

import (
	"database/sql"
	"fmt"
)

// Policy is the per-project sync policy. Three canonical values match the
// CHECK constraint in the project_policy table (schema v10).
// They are redeclared here (not imported from controlapi) to keep localstore
// free of upward dependencies — controlapi imports localstore, not the reverse.
type Policy string

const (
	PolicySynced    Policy = "synced"
	PolicyLocalOnly Policy = "local-only"
	PolicyOmitted   Policy = "omitted"
)

// ProjectPolicy pairs a project name with its effective policy.
type ProjectPolicy struct {
	Name   string
	Policy Policy
}

// defaultPolicy returns the read-time default policy for a project that has no
// explicit row in project_policy.
//
// Central-aware rule (design decision #4):
//   - Central configured → default is synced  (pushes + pulls as normal).
//   - Central NOT configured → default is local-only  (no outbound traffic).
//
// The closure is queried at read time so flipping central connect/disconnect
// is reflected immediately without any migration or cache invalidation.
func (s *Store) defaultPolicy() Policy {
	if s.isCentralConfigured != nil && s.isCentralConfigured() {
		return PolicySynced
	}
	return PolicyLocalOnly
}

// GetPolicy returns the effective policy for the named project.
//
// Lookup order:
//  1. In-memory policy cache (cache hit → return immediately).
//  2. project_policy table (absent row → read-time default from defaultPolicy).
//
// The returned policy is cached so subsequent calls (e.g. the outbox drain
// hot path) avoid repeated DB round-trips.
func (s *Store) GetPolicy(project string) (Policy, error) {
	// Fast path: check the cache under a read lock.
	s.policyMu.RLock()
	if p, ok := s.policyCache[project]; ok {
		s.policyMu.RUnlock()
		return p, nil
	}
	s.policyMu.RUnlock()

	// Slow path: query the DB.
	var policy Policy
	err := s.db.QueryRow(
		`SELECT policy FROM project_policy WHERE project = ?`, project,
	).Scan(&policy)

	switch {
	case err == nil:
		// Row found: explicit policy stored.
	case err == sql.ErrNoRows:
		// No row: apply the read-time default.
		policy = s.defaultPolicy()
	default:
		return "", fmt.Errorf("GetPolicy %q: %w", project, err)
	}

	// Cache the result under a write lock.
	s.policyMu.Lock()
	s.policyCache[project] = policy
	s.policyMu.Unlock()

	return policy, nil
}

// SetPolicy persists the policy for the named project (upsert) and invalidates
// the in-memory policy cache for that project so the next GetPolicy read reflects
// the new value immediately.
func (s *Store) SetPolicy(project string, p Policy) error {
	_, err := s.db.Exec(
		`INSERT INTO project_policy (project, policy, updated_at)
		 VALUES (?, ?, datetime('now'))
		 ON CONFLICT(project) DO UPDATE SET
		     policy     = excluded.policy,
		     updated_at = excluded.updated_at`,
		project, string(p),
	)
	if err != nil {
		return fmt.Errorf("SetPolicy %q=%q: %w", project, p, err)
	}

	// Invalidate cache entry so the next read fetches the updated value.
	s.policyMu.Lock()
	delete(s.policyCache, project)
	s.policyMu.Unlock()

	return nil
}

// ListProjectsWithPolicy returns all projects known to the local store, each
// paired with its effective policy.
//
// Projects are discovered via a LEFT JOIN of the distinct project values in
// the memories table against the project_policy table.  Projects that have no
// explicit policy row receive the read-time default from defaultPolicy.
//
// The query also includes projects that exist only in project_policy (no
// memories row) via a UNION with project_policy rows, so explicitly-set
// policies are always visible even for projects not yet written to memories.
func (s *Store) ListProjectsWithPolicy() ([]ProjectPolicy, error) {
	def := s.defaultPolicy()

	// UNION of:
	//   (a) all projects in memories with their policy (or default when absent)
	//   (b) all projects in project_policy that have no memories row
	// This covers:
	//   - Projects that have memories but no policy row → default
	//   - Projects that have memories and a policy row → explicit policy
	//   - Projects that have a policy row but no memories → explicit policy
	// The outer COALESCE maps NULL (no policy row) to the default string.
	const q = `
		SELECT project, COALESCE(pp.policy, ?) AS effective_policy
		FROM (
			SELECT DISTINCT project FROM memories WHERE deleted_at IS NULL
			UNION
			SELECT project FROM project_policy
		) AS all_projects
		LEFT JOIN project_policy pp USING (project)
		ORDER BY project`

	rows, err := s.db.Query(q, string(def))
	if err != nil {
		return nil, fmt.Errorf("ListProjectsWithPolicy: query: %w", err)
	}
	defer rows.Close()

	var results []ProjectPolicy
	for rows.Next() {
		var proj string
		var pol string
		if err := rows.Scan(&proj, &pol); err != nil {
			return nil, fmt.Errorf("ListProjectsWithPolicy: scan: %w", err)
		}
		results = append(results, ProjectPolicy{
			Name:   proj,
			Policy: Policy(pol),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListProjectsWithPolicy: rows: %w", err)
	}
	return results, nil
}
