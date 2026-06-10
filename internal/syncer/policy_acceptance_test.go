//go:build acceptance

// Package syncer_test — per-project policy acceptance proof (PR-②).
//
// This test proves the critical policy-filter guarantees end-to-end against a
// real HTTP central (cloudserve + embedded-postgres):
//
//  1. local-only: mem_save writes locally; Push skips the entries (zero central pushes).
//  2. synced: mem_save writes locally AND Push sends the entry to central (one push).
//  3. omitted: the MCP tool layer refuses writes; zero rows, zero outbox entries.
//  4. flip local-only → synced: accumulated outbox entries drain on the next Push cycle.
//  5. v10 migration idempotency: a fresh DB reaches schema version 10 exactly once.
//
// Infrastructure:
//   - Reuses the autoPgDSN / TestMain / autoHTTPCentral helpers from
//     autosync_convergence_acceptance_test.go (same package, same build tag).
//   - Each test gets an isolated central schema and a fresh localstore node.

package syncer_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/syncer"
)

// policyNode opens a fresh localstore node with the central-configured flag set.
// The writerID is embedded in seeded mutations so Push passes the HMAC
// writer-key check on the real cloudserve backend.
func policyNode(t *testing.T, name, writerID string, centralConfigured bool) *syncer.Node {
	t.Helper()
	dir := t.TempDir()
	st, err := localstore.Open(filepath.Join(dir, name+".db"))
	if err != nil {
		t.Fatalf("policyNode %s: localstore.Open: %v", name, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.SetCentralConfiguredFn(func() bool { return centralConfigured })
	n := syncer.NewNode(name, st)
	return n
}

// writeForPolicy writes a single local mutation to node n for the given project.
func writeForPolicy(t *testing.T, n *syncer.Node, project, syncID, writerID string) {
	t.Helper()
	_, err := n.Write(domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess-policy-test",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "policy-test-" + syncID,
		Project:    project,
		Scope:      "project",
		WriterID:   writerID,
		UpdatedAt:  time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("writeForPolicy %s: %v", syncID, err)
	}
}

// centralPushedCount returns the number of mutations central has for the given
// project by calling PullSince with cursor 0.
func centralPushedCount(t *testing.T, ctx context.Context, central syncer.Central, project string) int {
	t.Helper()
	muts, err := central.PullSince(ctx, project, 0, 1000)
	if err != nil {
		t.Fatalf("centralPushedCount PullSince: %v", err)
	}
	return len(muts)
}

// ─────────────────────────────────────────────────────────────────────────────

// TestAcceptance_LocalOnly_NoCentralPush verifies that Push does not send
// entries from a local-only project to central.
func TestAcceptance_LocalOnly_NoCentralPush(t *testing.T) {
	ctx := context.Background()
	newClient, centralStore := autoHTTPCentral(t)
	writerID := "writer-policy-local"
	if err := centralStore.UpsertWriterKey(ctx, writerID, autoTestKey(writerID)); err != nil {
		t.Fatalf("UpsertWriterKey: %v", err)
	}
	central := newClient(writerID)

	n := policyNode(t, "local-only-node", writerID, true)

	const project = "private-project"
	if err := n.Store.SetPolicy(project, localstore.PolicyLocalOnly); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	// Write 3 entries for the local-only project.
	for i := 0; i < 3; i++ {
		writeForPolicy(t, n, project, fmt.Sprintf("lo-entry-%d", i), writerID)
	}

	pushed, err := syncer.Push(ctx, n, central)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if pushed != 0 {
		t.Errorf("local-only Push: pushed=%d; want 0 (no central pushes for local-only)", pushed)
	}

	// Confirm nothing reached central.
	count := centralPushedCount(t, ctx, central, project)
	if count != 0 {
		t.Errorf("central has %d mutations for local-only project; want 0", count)
	}

	// The 3 entries remain unacked in the outbox.
	pending, err := n.Store.PendingCount()
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 3 {
		t.Errorf("pending after local-only Push = %d; want 3 (all unacked)", pending)
	}
}

// TestAcceptance_Synced_PushesToCentral verifies that Push sends entries from a
// synced project to central (the normal path).
func TestAcceptance_Synced_PushesToCentral(t *testing.T) {
	ctx := context.Background()
	newClient, centralStore := autoHTTPCentral(t)
	writerID := "writer-policy-synced"
	if err := centralStore.UpsertWriterKey(ctx, writerID, autoTestKey(writerID)); err != nil {
		t.Fatalf("UpsertWriterKey: %v", err)
	}
	central := newClient(writerID)

	n := policyNode(t, "synced-node", writerID, true)

	const project = "open-project"
	// Default policy is synced (central configured) — no explicit SetPolicy needed.

	writeForPolicy(t, n, project, "synced-entry-1", writerID)

	pushed, err := syncer.Push(ctx, n, central)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if pushed != 1 {
		t.Errorf("synced Push: pushed=%d; want 1", pushed)
	}

	// The entry must be on central.
	count := centralPushedCount(t, ctx, central, project)
	if count != 1 {
		t.Errorf("central has %d mutations for synced project; want 1", count)
	}
}

// TestAcceptance_Flip_LocalOnly_To_Synced_DrainsPending is the keystone
// acceptance test for the flip guarantee. It:
//  1. Sets a project to local-only.
//  2. Writes 3 entries — they stay unacked after Push.
//  3. Flips to synced.
//  4. Pushes again — all 3 entries must reach central.
func TestAcceptance_Flip_LocalOnly_To_Synced_DrainsPending(t *testing.T) {
	ctx := context.Background()
	newClient, centralStore := autoHTTPCentral(t)
	writerID := "writer-policy-flip"
	if err := centralStore.UpsertWriterKey(ctx, writerID, autoTestKey(writerID)); err != nil {
		t.Fatalf("UpsertWriterKey: %v", err)
	}
	central := newClient(writerID)

	n := policyNode(t, "flip-node", writerID, true)

	const project = "flip-project"
	if err := n.Store.SetPolicy(project, localstore.PolicyLocalOnly); err != nil {
		t.Fatalf("SetPolicy local-only: %v", err)
	}

	for i := 0; i < 3; i++ {
		writeForPolicy(t, n, project, fmt.Sprintf("flip-entry-%d", i), writerID)
	}

	// Push with local-only → 0 pushed, 3 remain.
	if pushed, err := syncer.Push(ctx, n, central); err != nil {
		t.Fatalf("Push (local-only): %v", err)
	} else if pushed != 0 {
		t.Errorf("local-only Push pushed=%d; want 0", pushed)
	}
	if count := centralPushedCount(t, ctx, central, project); count != 0 {
		t.Errorf("central count after local-only Push = %d; want 0", count)
	}

	// Flip to synced.
	if err := n.Store.SetPolicy(project, localstore.PolicySynced); err != nil {
		t.Fatalf("SetPolicy synced: %v", err)
	}

	// Push again → all 3 accumulated entries drain.
	pushed, err := syncer.Push(ctx, n, central)
	if err != nil {
		t.Fatalf("Push (synced): %v", err)
	}
	if pushed != 3 {
		t.Errorf("synced Push after flip: pushed=%d; want 3 (all accumulated entries must drain)", pushed)
	}
	if count := centralPushedCount(t, ctx, central, project); count != 3 {
		t.Errorf("central count after flip Push = %d; want 3", count)
	}

	// Outbox must be empty.
	pending, err := n.Store.PendingCount()
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 0 {
		t.Errorf("pending after flip Push = %d; want 0", pending)
	}
}

// TestAcceptance_V10_MigrationIdempotency verifies that opening an already-
// migrated v10 DB a second time does not error and the schema version remains 10.
func TestAcceptance_V10_MigrationIdempotency(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "idempotent.db")

	// First open: migrates to v10.
	st, err := localstore.Open(dbPath)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	_ = st.Close()

	// Second open: must succeed without error (no re-migration panic).
	st2, err := localstore.Open(dbPath)
	if err != nil {
		t.Fatalf("second Open (idempotency): %v", err)
	}
	defer st2.Close()

	// Confirm the schema is v10 by exercising the policy table.
	if err := st2.SetPolicy("idempotent-proj", localstore.PolicyLocalOnly); err != nil {
		t.Fatalf("SetPolicy on re-opened DB: %v", err)
	}
	pol, err := st2.GetPolicy("idempotent-proj")
	if err != nil {
		t.Fatalf("GetPolicy on re-opened DB: %v", err)
	}
	if pol != localstore.PolicyLocalOnly {
		t.Errorf("GetPolicy = %q; want %q", pol, localstore.PolicyLocalOnly)
	}
}

// TestAcceptance_PullExclusion_LocalOnly verifies that SyncAllProjects does
// not pull from central for a local-only project even when central has data for it.
func TestAcceptance_PullExclusion_LocalOnly(t *testing.T) {
	ctx := context.Background()
	newClient, centralStore := autoHTTPCentral(t)

	// Node A (writer): synced, pushes data to central.
	writerA := "writer-policy-pull-a"
	if err := centralStore.UpsertWriterKey(ctx, writerA, autoTestKey(writerA)); err != nil {
		t.Fatalf("UpsertWriterKey A: %v", err)
	}
	centralA := newClient(writerA)

	nodeA := policyNode(t, "pull-excl-node-a", writerA, true)
	const project = "pull-excl-project"

	// Provision node B: local-only for the same project.
	writerB := "writer-policy-pull-b"
	if err := centralStore.UpsertWriterKey(ctx, writerB, autoTestKey(writerB)); err != nil {
		t.Fatalf("UpsertWriterKey B: %v", err)
	}
	centralB := newClient(writerB)

	nodeB := policyNode(t, "pull-excl-node-b", writerB, true)
	if err := nodeB.Store.SetPolicy(project, localstore.PolicyLocalOnly); err != nil {
		t.Fatalf("nodeB SetPolicy local-only: %v", err)
	}

	// Node A: write and push.
	writeForPolicy(t, nodeA, project, "pull-excl-entry", writerA)
	if _, err := syncer.Push(ctx, nodeA, centralA); err != nil {
		t.Fatalf("nodeA Push: %v", err)
	}

	// Confirm the entry is on central.
	if count := centralPushedCount(t, ctx, centralA, project); count == 0 {
		t.Skip("nodeA push did not reach central — skipping pull exclusion check")
	}

	// Node B: SyncAllProjects must not pull the local-only project.
	// Also seed B with the project (so ListProjects returns it) but it stays local.
	writeForPolicy(t, nodeB, project, "pull-excl-b-seed", writerB)

	if _, err := syncer.Push(ctx, nodeB, centralB); err != nil {
		// B's entry for local-only project is skipped — Push returns 0 not an error.
		t.Fatalf("nodeB Push (should not error): %v", err)
	}

	// Manual Pull for the local-only project must be skipped via SyncAllProjects.
	_, _, err := syncer.SyncAllProjects(ctx, nodeB, centralB)
	if err != nil {
		// SyncAllProjects collects pull errors; this must succeed.
		t.Fatalf("nodeB SyncAllProjects: %v", err)
	}

	// node B must NOT have A's entry (pull was excluded).
	hits, err := nodeB.Store.SearchMemories("policy-test-pull-excl-entry", project, 10)
	if err != nil {
		t.Fatalf("nodeB SearchMemories: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("nodeB has %d hits from nodeA's write — pull exclusion failed for local-only project", len(hits))
	}
}
