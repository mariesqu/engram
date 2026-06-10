package syncer_test

// Tests for the policy-aware push and pull filters introduced in PR-②.
// These tests assert the three key policy behaviors:
//   - local-only entries stay unacked after Push
//   - omitted entries stay unacked after Push
//   - SyncAllProjects excludes local-only and omitted projects from Pull
//   - flipping local-only → synced drains previously accumulated outbox entries

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/syncer"
)

// openSyncNode opens a test node with central configured, so all projects
// default to synced.  Callers that want to test non-synced behavior must call
// store.SetPolicy explicitly.
func openSyncNode(t *testing.T, name string, centralConfigured bool) *syncer.Node {
	t.Helper()
	dir := t.TempDir()
	st, err := localstore.Open(filepath.Join(dir, name+".db"))
	if err != nil {
		t.Fatalf("openSyncNode %s: %v", name, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.SetCentralConfiguredFn(func() bool { return centralConfigured })
	n := syncer.NewNode(name, st)
	return n
}

// simpleCentral records Apply calls and returns configurable mutations from PullSince.
type simpleCentral struct {
	applied []domain.Mutation
	pulls   map[string][]domain.Mutation // project → mutations
}

func (c *simpleCentral) Apply(_ context.Context, m domain.Mutation) error {
	c.applied = append(c.applied, m)
	return nil
}

func (c *simpleCentral) PullSince(_ context.Context, project string, since int64, _ int) ([]domain.Mutation, error) {
	var out []domain.Mutation
	for _, m := range c.pulls[project] {
		if m.Seq > since {
			out = append(out, m)
		}
	}
	return out, nil
}

// writeToNode writes a local mutation to node n's store and returns the outbox entry count.
func writeToNode(t *testing.T, n *syncer.Node, project, syncID string) {
	t.Helper()
	if _, err := n.Write(domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "title-" + syncID,
		Project:    project,
		Scope:      "project",
		WriterID:   "writer",
		UpdatedAt:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("writeToNode %s: %v", syncID, err)
	}
}

// pendingCount returns the number of unacked outbox entries for the node.
func pendingCount(t *testing.T, n *syncer.Node) int {
	t.Helper()
	cnt, err := n.Store.PendingCount()
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	return cnt
}

// ─────────────────────────────────────────────────────────────────────────────

// TestPush_SkipsLocalOnly verifies that Push does not push or ack outbox
// entries for a local-only project, leaving them unacked.
func TestPush_SkipsLocalOnly(t *testing.T) {
	ctx := context.Background()
	n := openSyncNode(t, "push-local-only", true)
	central := &simpleCentral{}

	// Write one synced entry and one local-only entry.
	writeToNode(t, n, "open", "open-1")
	writeToNode(t, n, "private", "private-1")

	if err := n.Store.SetPolicy("private", localstore.PolicyLocalOnly); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	pushed, err := syncer.Push(ctx, n, central)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if pushed != 1 {
		t.Errorf("pushed = %d; want 1 (only 'open' entry)", pushed)
	}

	// Central must have received only the "open" mutation.
	if len(central.applied) != 1 {
		t.Errorf("central.applied = %d; want 1", len(central.applied))
	}
	if len(central.applied) == 1 && central.applied[0].Project != "open" {
		t.Errorf("pushed project = %q; want %q", central.applied[0].Project, "open")
	}

	// The "private" entry must remain unacked.
	if cnt := pendingCount(t, n); cnt != 1 {
		t.Errorf("pending after Push = %d; want 1 (private entry unacked)", cnt)
	}
}

// TestPush_SkipsOmitted verifies that Push does not push or ack outbox entries
// for an omitted project.  (Omitted projects should never have outbox entries
// in production — the tool-level refusal prevents writes.  This test guards
// against the edge case of a flip from local-only to omitted.)
func TestPush_SkipsOmitted(t *testing.T) {
	ctx := context.Background()
	n := openSyncNode(t, "push-omitted", true)
	central := &simpleCentral{}

	// Write a local-only entry first (allowed), then flip to omitted.
	writeToNode(t, n, "secret", "secret-1")
	if err := n.Store.SetPolicy("secret", localstore.PolicyLocalOnly); err != nil {
		t.Fatalf("SetPolicy local-only: %v", err)
	}
	// Flip to omitted — the outbox entry is still there from the local-only write.
	if err := n.Store.SetPolicy("secret", localstore.PolicyOmitted); err != nil {
		t.Fatalf("SetPolicy omitted: %v", err)
	}

	pushed, err := syncer.Push(ctx, n, central)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if pushed != 0 {
		t.Errorf("pushed = %d; want 0 (omitted entry must be skipped)", pushed)
	}
	if len(central.applied) != 0 {
		t.Errorf("central.applied = %d; want 0", len(central.applied))
	}
	if cnt := pendingCount(t, n); cnt != 1 {
		t.Errorf("pending after Push = %d; want 1 (omitted entry unacked)", cnt)
	}
}

// TestSyncAllProjects_ExcludesLocalOnly verifies that SyncAllProjects does not
// issue a PullSince for a local-only project.
func TestSyncAllProjects_ExcludesLocalOnly(t *testing.T) {
	ctx := context.Background()
	n := openSyncNode(t, "sap-excludes-local-only", true)

	writeToNode(t, n, "synced-proj", "synced-1")
	writeToNode(t, n, "local-only-proj", "local-only-1")

	if err := n.Store.SetPolicy("local-only-proj", localstore.PolicyLocalOnly); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	central := newMultiProjectCentral(map[string][]domain.Mutation{
		"synced-proj":     {},
		"local-only-proj": {makeMutation("local-only-proj", 1)},
	})

	if _, _, err := syncer.SyncAllProjects(ctx, n, central); err != nil {
		t.Fatalf("SyncAllProjects: %v", err)
	}

	// PullSince must NOT have been called for local-only-proj.
	calls := central.getCalls()
	for _, c := range calls {
		if c.project == "local-only-proj" {
			t.Errorf("PullSince called for local-only project %q; must be excluded", c.project)
		}
	}

	// Confirm the local-only project's store was not updated.
	cnt, err := n.Store.PendingCount()
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	// Both writes are unacked: synced push was acked, local-only was not.
	// synced-1 gets pushed (acked), local-only-1 stays pending.
	if cnt != 1 {
		t.Errorf("pending = %d; want 1 (local-only-1 unacked)", cnt)
	}
}

// TestFlip_LocalOnly_To_Synced_DrainsPending verifies that accumulated outbox
// entries for a local-only project become eligible (and are drained) on the
// next Push cycle after the policy is flipped to synced.
func TestFlip_LocalOnly_To_Synced_DrainsPending(t *testing.T) {
	ctx := context.Background()
	n := openSyncNode(t, "flip-local-to-synced", true)
	central := &simpleCentral{}

	// Set local-only and write several entries.
	if err := n.Store.SetPolicy("private", localstore.PolicyLocalOnly); err != nil {
		t.Fatalf("SetPolicy local-only: %v", err)
	}
	for i := 0; i < 3; i++ {
		writeToNode(t, n, "private", "private-"+string(rune('1'+i)))
	}

	// Confirm entries are unacked.
	if cnt := pendingCount(t, n); cnt != 3 {
		t.Fatalf("pre-flip pending = %d; want 3", cnt)
	}

	// Push with local-only → nothing pushed, all entries remain.
	pushed, err := syncer.Push(ctx, n, central)
	if err != nil {
		t.Fatalf("Push (local-only): %v", err)
	}
	if pushed != 0 {
		t.Errorf("pushed while local-only = %d; want 0", pushed)
	}
	if cnt := pendingCount(t, n); cnt != 3 {
		t.Errorf("pending after local-only Push = %d; want 3 (still unacked)", cnt)
	}

	// Flip to synced.
	if err := n.Store.SetPolicy("private", localstore.PolicySynced); err != nil {
		t.Fatalf("SetPolicy synced: %v", err)
	}

	// Push again → all 3 entries are now eligible and must be pushed.
	pushed, err = syncer.Push(ctx, n, central)
	if err != nil {
		t.Fatalf("Push (synced): %v", err)
	}
	if pushed != 3 {
		t.Errorf("pushed after flip = %d; want 3", pushed)
	}
	if cnt := pendingCount(t, n); cnt != 0 {
		t.Errorf("pending after synced Push = %d; want 0", cnt)
	}
}
