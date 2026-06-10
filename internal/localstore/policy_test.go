package localstore

// Unit tests for GetPolicy, SetPolicy, ListProjectsWithPolicy, and the
// read-time central-aware default (design decision #4).

import (
	"testing"
)

// TestGetPolicy_AbsentRow_DefaultSynced verifies that GetPolicy returns
// PolicySynced when no row exists in project_policy and central is configured.
func TestGetPolicy_AbsentRow_DefaultSynced(t *testing.T) {
	st := openTempStore(t)
	st.SetCentralConfiguredFn(func() bool { return true })

	pol, err := st.GetPolicy("new-project")
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if pol != PolicySynced {
		t.Errorf("GetPolicy (absent row, central configured) = %q; want %q", pol, PolicySynced)
	}
}

// TestGetPolicy_AbsentRow_DefaultLocalOnly verifies that GetPolicy returns
// PolicyLocalOnly when no row exists in project_policy and central is NOT
// configured.  This is the carry-forward test mandated by the PR-② spec —
// the PR-① stub was intentionally wrong about this case.
func TestGetPolicy_AbsentRow_DefaultLocalOnly(t *testing.T) {
	st := openTempStore(t)
	// Do NOT call SetCentralConfiguredFn — nil fn means "not configured".
	pol, err := st.GetPolicy("new-project")
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if pol != PolicyLocalOnly {
		t.Errorf("GetPolicy (absent row, no central) = %q; want %q", pol, PolicyLocalOnly)
	}
}

// TestGetPolicy_AbsentRow_DefaultLocalOnly_ExplicitFalse verifies the
// local-only default when the fn explicitly returns false.
func TestGetPolicy_AbsentRow_DefaultLocalOnly_ExplicitFalse(t *testing.T) {
	st := openTempStore(t)
	st.SetCentralConfiguredFn(func() bool { return false })

	pol, err := st.GetPolicy("no-central-proj")
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if pol != PolicyLocalOnly {
		t.Errorf("GetPolicy (absent row, central fn=false) = %q; want %q", pol, PolicyLocalOnly)
	}
}

// TestSetPolicy_Upsert verifies that SetPolicy inserts a new row and that a
// second call updates (upserts) it.
func TestSetPolicy_Upsert(t *testing.T) {
	st := openTempStore(t)
	st.SetCentralConfiguredFn(func() bool { return true })

	// Insert.
	if err := st.SetPolicy("proj-a", PolicyLocalOnly); err != nil {
		t.Fatalf("SetPolicy insert: %v", err)
	}
	pol, err := st.GetPolicy("proj-a")
	if err != nil {
		t.Fatalf("GetPolicy after insert: %v", err)
	}
	if pol != PolicyLocalOnly {
		t.Errorf("after insert: policy = %q; want %q", pol, PolicyLocalOnly)
	}

	// Upsert (update).
	if err := st.SetPolicy("proj-a", PolicyOmitted); err != nil {
		t.Fatalf("SetPolicy update: %v", err)
	}
	pol, err = st.GetPolicy("proj-a")
	if err != nil {
		t.Fatalf("GetPolicy after update: %v", err)
	}
	if pol != PolicyOmitted {
		t.Errorf("after update: policy = %q; want %q", pol, PolicyOmitted)
	}
}

// TestSetPolicy_CacheInvalidation verifies that SetPolicy invalidates the
// policy cache so the next GetPolicy call sees the updated value rather than
// the cached stale one.
func TestSetPolicy_CacheInvalidation(t *testing.T) {
	st := openTempStore(t)
	st.SetCentralConfiguredFn(func() bool { return true })

	// Prime the cache with synced.
	if err := st.SetPolicy("cache-proj", PolicySynced); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	if _, err := st.GetPolicy("cache-proj"); err != nil {
		t.Fatalf("GetPolicy (prime cache): %v", err)
	}

	// Change to local-only; cache must be invalidated.
	if err := st.SetPolicy("cache-proj", PolicyLocalOnly); err != nil {
		t.Fatalf("SetPolicy update: %v", err)
	}
	pol, err := st.GetPolicy("cache-proj")
	if err != nil {
		t.Fatalf("GetPolicy after update: %v", err)
	}
	if pol != PolicyLocalOnly {
		t.Errorf("cached stale value not invalidated: got %q, want %q", pol, PolicyLocalOnly)
	}
}

// TestListProjectsWithPolicy_MixedPolicies verifies that ListProjectsWithPolicy
// returns the correct effective policy for each project:
//   - explicit row → explicit policy
//   - absent row + central configured → synced default
func TestListProjectsWithPolicy_MixedPolicies(t *testing.T) {
	st := openTempStore(t)
	st.SetCentralConfiguredFn(func() bool { return true })

	// Seed two projects in memories.
	seedMemory(t, st, "proj-explicit", "mem-explicit")
	seedMemory(t, st, "proj-default", "mem-default")

	// Set an explicit policy for proj-explicit.
	if err := st.SetPolicy("proj-explicit", PolicyLocalOnly); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	projects, err := st.ListProjectsWithPolicy()
	if err != nil {
		t.Fatalf("ListProjectsWithPolicy: %v", err)
	}

	byName := make(map[string]Policy)
	for _, p := range projects {
		byName[p.Name] = p.Policy
	}

	if byName["proj-explicit"] != PolicyLocalOnly {
		t.Errorf("proj-explicit policy = %q; want %q", byName["proj-explicit"], PolicyLocalOnly)
	}
	if byName["proj-default"] != PolicySynced {
		t.Errorf("proj-default policy = %q; want %q (central configured → synced default)", byName["proj-default"], PolicySynced)
	}
}

// TestListProjectsWithPolicy_LocalOnlyDefault verifies that without central
// configured all projects default to local-only in list output.
func TestListProjectsWithPolicy_LocalOnlyDefault(t *testing.T) {
	st := openTempStore(t)
	// No central configured: all absent-row projects default to local-only.

	seedMemory(t, st, "no-central-proj", "mem-nocentral")

	projects, err := st.ListProjectsWithPolicy()
	if err != nil {
		t.Fatalf("ListProjectsWithPolicy: %v", err)
	}

	for _, p := range projects {
		if p.Name == "no-central-proj" && p.Policy != PolicyLocalOnly {
			t.Errorf("no-central-proj policy = %q; want %q", p.Policy, PolicyLocalOnly)
		}
	}
}

// TestListProjectsWithPolicy_PolicyOnlyProject verifies that a project with an
// explicit policy row but no memories row still appears in the list.
func TestListProjectsWithPolicy_PolicyOnlyProject(t *testing.T) {
	st := openTempStore(t)
	st.SetCentralConfiguredFn(func() bool { return true })

	if err := st.SetPolicy("policy-only-proj", PolicyOmitted); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	projects, err := st.ListProjectsWithPolicy()
	if err != nil {
		t.Fatalf("ListProjectsWithPolicy: %v", err)
	}

	found := false
	for _, p := range projects {
		if p.Name == "policy-only-proj" {
			found = true
			if p.Policy != PolicyOmitted {
				t.Errorf("policy-only-proj policy = %q; want %q", p.Policy, PolicyOmitted)
			}
		}
	}
	if !found {
		t.Error("policy-only-proj not found in ListProjectsWithPolicy")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// seedMemory inserts a minimal memory row for the given project so
// ListProjects / ListProjectsWithPolicy returns it.
func seedMemory(t *testing.T, st *Store, project, syncID string) {
	t.Helper()
	_, err := st.DB().Exec(`
		INSERT INTO memories (sync_id, session_id, entity_type, type, title, content, project, scope, writer_id)
		VALUES (?, 'sess', 'memory', 'manual', 'seed', 'seed', ?, 'project', 'w')
	`, syncID, project)
	if err != nil {
		t.Fatalf("seedMemory(%q): %v", project, err)
	}
}
