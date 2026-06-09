package localstore

import "fmt"

// Policy is the per-project sync policy. Three canonical values match the
// CHECK constraint in the project_policy table (added in schema v10 / PR-②).
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

// ListProjectsWithPolicy returns all projects known to the local store, each
// paired with its effective policy. Projects are discovered by querying the
// distinct project values in the memories table (same source as ListProjects).
//
// PR-① stub: the project_policy table (schema v10) does not exist yet.
// All projects are returned with the default policy "synced". PR-② will add
// the table, schema migration, and a real LEFT JOIN implementation that reads
// persisted policy overrides.
func (s *Store) ListProjectsWithPolicy() ([]ProjectPolicy, error) {
	// Enumerate distinct projects from the memories table (non-deleted rows only,
	// same as ListProjects). Includes the empty string project for default-project
	// writes; callers may filter it if needed.
	const q = `
		SELECT DISTINCT project
		FROM memories
		WHERE deleted_at IS NULL
		ORDER BY project`

	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("ListProjectsWithPolicy: query: %w", err)
	}
	defer rows.Close()

	var results []ProjectPolicy
	for rows.Next() {
		var proj string
		if err := rows.Scan(&proj); err != nil {
			return nil, fmt.Errorf("ListProjectsWithPolicy: scan: %w", err)
		}
		results = append(results, ProjectPolicy{
			Name: proj,
			// PR-① stub: hardcoded default. PR-② replaces this with the actual
			// persisted policy from the project_policy table (v10 schema).
			Policy: PolicySynced,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListProjectsWithPolicy: rows: %w", err)
	}
	return results, nil
}

// SetPolicy persists the policy for a named project.
//
// PR-① stub: the project_policy table does not exist yet. This method is
// declared here to satisfy the controlapi.Store interface so the daemon can be
// wired up. It returns an error indicating the table is not yet available.
// PR-② will replace this with a real upsert against the v10 schema table.
func (s *Store) SetPolicy(project string, p Policy) error {
	return fmt.Errorf("SetPolicy: project_policy table not available before schema v10 (PR-②)")
}

// GetPolicy returns the effective policy for a named project.
//
// PR-① stub: the project_policy table does not exist yet. Returns the default
// policy (PolicySynced) for all projects. PR-② will replace this with a real
// lookup against the v10 schema table, computing the correct default from the
// central configuration state when no explicit row exists.
func (s *Store) GetPolicy(_ string) (Policy, error) {
	// Default before the project_policy table exists (PR-①). PR-② overrides.
	return PolicySynced, nil
}
