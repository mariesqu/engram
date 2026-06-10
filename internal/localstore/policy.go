package localstore

import (
	"database/sql"
	"errors"
	"fmt"
)

// ErrOmittedProject is returned by write paths that refuse a mutation because
// the target project's policy is omitted.  ApplyPulled wraps it so syncer.Pull
// can keep the per-project cursor BEHIND the refused mutation (errors.Is).
var ErrOmittedProject = errors.New("project policy is omitted")

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
// The closure is queried at read time AND the computed default is NEVER cached
// (see GetPolicy), so flipping central connect/disconnect is reflected on the
// very next read — no migration, no cache flush.
//
// Callers must hold policyMu (read or write) — the closure pointer is guarded
// by it so SetCentralConfiguredFn is safe to call at runtime.
func (s *Store) defaultPolicyLocked() Policy {
	if s.isCentralConfigured != nil && s.isCentralConfigured() {
		return PolicySynced
	}
	return PolicyLocalOnly
}

// GetPolicy returns the effective policy for the named project. The project
// name is normalized (normalizeProject) so callers with mixed-case/untrimmed
// names hit the same row and cache entry as the write path.
//
// Lookup order:
//  1. In-memory policy cache — EXPLICIT rows only (cache hit → return).
//  2. project_policy table; absent row → read-time central-aware default.
//
// Only explicit rows are cached. The computed default depends on external
// state (central configured or not) that can change at runtime; caching it
// would freeze the default at its first-read value.
//
// The slow path holds the write lock across the DB read AND the cache fill so
// a concurrent SetPolicy can never be interleaved between them (which would
// re-cache a value SetPolicy just invalidated).
func (s *Store) GetPolicy(project string) (Policy, error) {
	project = normalizeProject(project)

	// Fast path: explicit-row cache hit under a read lock.
	s.policyMu.RLock()
	if p, ok := s.policyCache[project]; ok {
		s.policyMu.RUnlock()
		return p, nil
	}
	s.policyMu.RUnlock()

	// Slow path: full lock across query + cache fill (see doc above).
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	if p, ok := s.policyCache[project]; ok { // re-check after lock upgrade
		return p, nil
	}

	var policy Policy
	err := s.db.QueryRow(
		`SELECT policy FROM project_policy WHERE project = ?`, project,
	).Scan(&policy)

	switch {
	case err == nil:
		// Explicit row: safe to cache (invalidated by SetPolicy on change).
		s.policyCache[project] = policy
		return policy, nil
	case err == sql.ErrNoRows:
		// No row: compute the read-time default. NOT cached.
		return s.defaultPolicyLocked(), nil
	default:
		return "", fmt.Errorf("GetPolicy %q: %w", project, err)
	}
}

// SetPolicy persists the policy for the named project (upsert) and invalidates
// the in-memory policy cache for that project so the next GetPolicy read
// reflects the new value immediately. The project name is normalized the same
// way GetPolicy normalizes it.
func (s *Store) SetPolicy(project string, p Policy) error {
	project = normalizeProject(project)

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
	s.policyMu.RLock()
	def := s.defaultPolicyLocked()
	s.policyMu.RUnlock()

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
