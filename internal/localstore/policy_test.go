package localstore

// Unit tests for GetPolicy, SetPolicy, ListProjectsWithPolicy, and the
// read-time central-aware default (design decision #4).

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
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

// TestListProjectsWithPolicy_MixedCasePolicy is the regression test for the
// policy-toggle bug: central-pulled projects keep their ORIGINAL case in memories,
// but SetPolicy stores the policy under the normalized lowercase name. The list
// used to (a) show the DEFAULT badge for the mixed-case project (the toggle looked
// like a no-op) and (b) emit a phantom duplicate lowercase row. The list now
// resolves the policy case-insensitively and collapses the duplicate.
func TestListProjectsWithPolicy_MixedCasePolicy(t *testing.T) {
	st := openTempStore(t)
	st.SetCentralConfiguredFn(func() bool { return true }) // default = synced

	// Mimic a central-pulled project: stored with original case (not normalized).
	seedMemory(t, st, "Gentleman.Dots", "mem-mixed")

	// The UI sets the policy on the displayed (mixed-case) name; SetPolicy
	// normalizes it to "gentleman.dots" internally.
	if err := st.SetPolicy("Gentleman.Dots", PolicyLocalOnly); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	projects, err := st.ListProjectsWithPolicy()
	if err != nil {
		t.Fatalf("ListProjectsWithPolicy: %v", err)
	}

	// Exactly ONE row for the logical project (no phantom lowercase duplicate).
	var rows []ProjectPolicy
	for _, p := range projects {
		if strings.EqualFold(p.Name, "gentleman.dots") {
			rows = append(rows, p)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows for the project; want exactly 1 (no case-variant duplicate): %+v", len(rows), rows)
	}
	// Display name keeps the original case, and the explicit policy is shown.
	if rows[0].Name != "Gentleman.Dots" {
		t.Errorf("display name = %q; want original-case Gentleman.Dots", rows[0].Name)
	}
	if rows[0].Policy != PolicyLocalOnly {
		t.Errorf("policy = %q; want %q (explicit toggle, not the default)", rows[0].Policy, PolicyLocalOnly)
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

// TestGetPolicy_DefaultNotCached_RuntimeFlip proves two things at once:
//  1. The computed default is NOT cached — only explicit rows are.
//  2. SetCentralConfiguredFn can be re-installed at runtime (PR-③ relies on
//     this for connect/disconnect) and the very next read reflects it.
func TestGetPolicy_DefaultNotCached_RuntimeFlip(t *testing.T) {
	st := openTempStore(t)

	// Not configured → local-only default.
	pol, err := st.GetPolicy("flip-proj")
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if pol != PolicyLocalOnly {
		t.Fatalf("before flip: got %q, want %q", pol, PolicyLocalOnly)
	}

	// Runtime central connect (no SetPolicy in between). If the default had
	// been cached above, this would still return local-only.
	st.SetCentralConfiguredFn(func() bool { return true })
	pol, err = st.GetPolicy("flip-proj")
	if err != nil {
		t.Fatalf("GetPolicy after flip: %v", err)
	}
	if pol != PolicySynced {
		t.Errorf("after central connect: got %q, want %q (stale cached default?)", pol, PolicySynced)
	}

	// And back: runtime disconnect must surface immediately too.
	st.SetCentralConfiguredFn(func() bool { return false })
	pol, err = st.GetPolicy("flip-proj")
	if err != nil {
		t.Fatalf("GetPolicy after disconnect: %v", err)
	}
	if pol != PolicyLocalOnly {
		t.Errorf("after central disconnect: got %q, want %q", pol, PolicyLocalOnly)
	}
}

// TestPolicy_ProjectNormalized verifies GetPolicy/SetPolicy normalize the
// project name (trim + lowercase) so mixed-case callers — the control API PUT
// endpoint, the CLI — hit the same row and cache entry as the write path.
func TestPolicy_ProjectNormalized(t *testing.T) {
	st := openTempStore(t)

	if err := st.SetPolicy("  MixedCase ", PolicyOmitted); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	pol, err := st.GetPolicy("mixedcase")
	if err != nil {
		t.Fatalf("GetPolicy lowercase: %v", err)
	}
	if pol != PolicyOmitted {
		t.Errorf("GetPolicy(\"mixedcase\") = %q, want %q (project not normalized on write?)", pol, PolicyOmitted)
	}
	pol, err = st.GetPolicy("MIXEDCASE")
	if err != nil {
		t.Fatalf("GetPolicy uppercase: %v", err)
	}
	if pol != PolicyOmitted {
		t.Errorf("GetPolicy(\"MIXEDCASE\") = %q, want %q (project not normalized on read?)", pol, PolicyOmitted)
	}
}

// TestApplyPulled_Omitted_RefusedWithError verifies the defensive guard: a
// pulled mutation for an omitted project is refused with ErrOmittedProject (an
// error, NOT a silent nil) so syncer.Pull keeps the per-project cursor BEHIND
// the mutation — a later flip back to synced re-pulls it instead of silently
// losing it forever.
func TestApplyPulled_Omitted_RefusedWithError(t *testing.T) {
	st := openTempStore(t)
	if err := st.SetPolicy("omit-pull", PolicyOmitted); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	m := domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     "sync-omit-pull-1",
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "pulled into omitted",
		Content:    "must be refused",
		Project:    "omit-pull",
		Scope:      "project",
		Version:    1,
		UpdatedAt:  time.Now().UTC(),
		WriterID:   "writer-remote",
		Seq:        42,
	}

	err := st.ApplyPulled(m)
	if !errors.Is(err, ErrOmittedProject) {
		t.Fatalf("ApplyPulled for omitted project: err = %v, want errors.Is(_, ErrOmittedProject)", err)
	}

	// Nothing materialized.
	rows, searchErr := st.SearchMemories("pulled into omitted", "omit-pull", 10)
	if searchErr != nil {
		t.Fatalf("SearchMemories: %v", searchErr)
	}
	if len(rows) != 0 {
		t.Errorf("want 0 rows, got %d (pulled mutation must be refused)", len(rows))
	}
}
