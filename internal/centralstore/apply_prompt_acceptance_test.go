//go:build acceptance

// Package centralstore_test — cross-device convergence proof for EntityPrompt
// (PR-3 headline deliverable).
//
// These tests prove that the central Apply correctly materializes
// EntityPrompt mutations into central_user_prompts / central_prompt_tombstones
// and that the shared push/pull path (PullSince + ApplyPulled) converges
// prompt state across two independent local nodes — the same end-to-end path
// memories travel.
//
// Each test uses a fresh isolated Postgres schema (newIsolatedStore) and two
// fresh SQLite temp files (localstore.Open + t.TempDir).  The push/pull
// orchestration reuses spike.Push / spike.Pull (the same wiring used by the
// existing memory convergence proofs in internal/spike).
//
// Scenarios:
//
//  1. Push→pull convergence: node A adds a prompt → Push → central row present
//     with correct columns → node B Pulls → user_prompts on B matches A's prompt.
//     Nothing lands in central_memories or B's memories.
//
//  2. Delete replication: A deletes the prompt → Push → central tombstone present,
//     central_user_prompts row gone → B Pulls → B's user_prompts row gone +
//     local tombstone present.
//
//  3. Stale resurrection rejected: after delete converges, push an OLDER upsert
//     (UpdatedAt ≤ delete's timestamp) → central applyPromptDecisionQ rejects it
//     (row stays gone). A FRESH upsert (strictly newer) revives on both central
//     and B.
//
//  4. Mixed memory+prompt shared cursor: A pushes a memory AND a prompt for the
//     SAME project with interleaved central seqs → B pulls with ONE per-project
//     cursor → BOTH the memory (in memories) and the prompt (in user_prompts)
//     converge. This is the key shared-cursor correctness proof.
//
//  5. Idempotency: re-push / re-pull the same prompt mutation → one central row,
//     one B row, no error.
//
//  6. Forgery/writer_id: the harness uses real HMAC auth (via cloudserve +
//     remote.Client); the push carries A's writer_id and is accepted. Central
//     forgery guard (per-writer key check in cloudserve) passes end-to-end.
//     NOTE: scenarios 1-5 use the direct in-process Apply path (no HTTP server)
//     which bypasses the HMAC transport layer — the transport-level forgery proof
//     for prompts belongs to PR-4 (real-verifier prompt push via remote.Client).
//     The writer_id column is asserted on the materialized row in all scenarios.
package centralstore_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/mutation"
	"github.com/mariesqu/engram/internal/spike"
)

// ── shared constants ─────────────────────────────────────────────────────────

const (
	promptProject = "engram-prompts"
	promptScope   = "project"
	writerA       = "prompt-writer-A"
	writerB       = "prompt-writer-B"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// newPromptNode opens a fresh local SQLite store and wraps it as a spike.Node.
func newPromptNode(t *testing.T, name string) *spike.Node {
	t.Helper()
	dir := t.TempDir()
	st, err := localstore.Open(filepath.Join(dir, name+".db"))
	if err != nil {
		t.Fatalf("newPromptNode %s: %v", name, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return spike.NewNode(name, st)
}

// normalizeMut derives the canonical Payload and MutationID for m, mirroring
// what localstore.normalizeMutation does inside localWriteLocked. Callers that
// pass mutations directly to central.Apply (bypassing nodeA.Write) MUST call
// this so the central UNIQUE(mutation_id) idempotency guard works correctly —
// an empty MutationID would cause every direct Apply to be treated as a fresh
// new mutation, breaking the staleness and resurrection scenarios.
func normalizeMut(m domain.Mutation) domain.Mutation {
	if m.OccurredAt.IsZero() {
		m.OccurredAt = m.UpdatedAt
	}
	if len(m.Payload) == 0 {
		m.Payload = mutation.CanonicalPayload(m)
	}
	if m.MutationID == "" {
		m.MutationID = mutation.NewMutationID(m.Payload)
	}
	return m
}

// promptMut builds a normalized domain.Mutation with EntityType=EntityPrompt.
// The mutation is normalized (Payload+MutationID derived) so it is safe to pass
// directly to central.Apply without going through nodeA.Write.
func promptMut(writerID, syncID, sessionID, content string, at time.Time) domain.Mutation {
	m := domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  sessionID,
		EntityType: domain.EntityPrompt,
		Content:    content,
		Project:    promptProject,
		Scope:      promptScope,
		Version:    1,
		UpdatedAt:  at,
		WriterID:   writerID,
	}
	return normalizeMut(m)
}

// promptDelMut builds a normalized OpDelete domain.Mutation with
// EntityType=EntityPrompt.
func promptDelMut(writerID, syncID, sessionID string, at time.Time) domain.Mutation {
	m := domain.Mutation{
		Op:         domain.OpDelete,
		SyncID:     syncID,
		SessionID:  sessionID,
		EntityType: domain.EntityPrompt,
		Project:    promptProject,
		Scope:      promptScope,
		Version:    1,
		UpdatedAt:  at,
		WriterID:   writerID,
	}
	return normalizeMut(m)
}

// assertCentralPromptPresent asserts one live row in central_user_prompts for
// the given sync_id and checks content, session_id, project, and writer_id.
func assertCentralPromptPresent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, syncID, wantContent, wantSession, wantWriter string) {
	t.Helper()
	var content, session, project, writer string
	err := pool.QueryRow(ctx,
		`SELECT content, session_id, project, writer_id
		   FROM central_user_prompts
		  WHERE sync_id = $1`,
		syncID,
	).Scan(&content, &session, &project, &writer)
	if err != nil {
		t.Fatalf("assertCentralPromptPresent(%q): query: %v", syncID, err)
	}
	if content != wantContent {
		t.Errorf("central prompt content=%q, want %q", content, wantContent)
	}
	if session != wantSession {
		t.Errorf("central prompt session_id=%q, want %q", session, wantSession)
	}
	if project != promptProject {
		t.Errorf("central prompt project=%q, want %q", project, promptProject)
	}
	if writer != wantWriter {
		t.Errorf("central prompt writer_id=%q, want %q", writer, wantWriter)
	}
}

// assertCentralPromptAbsent asserts no live row in central_user_prompts for
// the given sync_id.
func assertCentralPromptAbsent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, syncID string) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM central_user_prompts WHERE sync_id = $1`, syncID,
	).Scan(&n); err != nil {
		t.Fatalf("assertCentralPromptAbsent(%q): %v", syncID, err)
	}
	if n != 0 {
		t.Errorf("central_user_prompts: unexpected row for sync_id=%q (want absent)", syncID)
	}
}

// assertCentralTombstonePresent asserts a tombstone row in
// central_prompt_tombstones for the given sync_id.
func assertCentralTombstonePresent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, syncID string) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM central_prompt_tombstones WHERE sync_id = $1`, syncID,
	).Scan(&n); err != nil {
		t.Fatalf("assertCentralTombstonePresent(%q): %v", syncID, err)
	}
	if n < 1 {
		t.Errorf("central_prompt_tombstones: no tombstone for sync_id=%q", syncID)
	}
}

// assertCentralTombstoneAbsent asserts no tombstone in
// central_prompt_tombstones for the given sync_id.
func assertCentralTombstoneAbsent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, syncID string) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM central_prompt_tombstones WHERE sync_id = $1`, syncID,
	).Scan(&n); err != nil {
		t.Fatalf("assertCentralTombstoneAbsent(%q): %v", syncID, err)
	}
	if n != 0 {
		t.Errorf("central_prompt_tombstones: unexpected tombstone for sync_id=%q", syncID)
	}
}

// assertCentralMemoriesEmpty asserts central_memories has no row with the given
// sync_id — prompts must never land in central_memories.
func assertCentralMemoriesEmpty(t *testing.T, ctx context.Context, pool *pgxpool.Pool, syncID string) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM central_memories WHERE sync_id = $1`, syncID,
	).Scan(&n); err != nil {
		t.Fatalf("assertCentralMemoriesEmpty(%q): %v", syncID, err)
	}
	if n != 0 {
		t.Errorf("central_memories: unexpected row for prompt sync_id=%q (routing bug)", syncID)
	}
}

// assertLocalPromptPresent asserts one live row in the node's local user_prompts
// for the given sync_id and checks content, session_id, and writer_id.
func assertLocalPromptPresent(t *testing.T, st *localstore.Store, syncID, wantContent, wantSession, wantWriter string) {
	t.Helper()
	var content, session, project, writer string
	err := st.DB().QueryRow(
		`SELECT content, session_id, project, writer_id FROM user_prompts WHERE sync_id = ?`,
		syncID,
	).Scan(&content, &session, &project, &writer)
	if err != nil {
		t.Fatalf("assertLocalPromptPresent(%q): %v", syncID, err)
	}
	if content != wantContent {
		t.Errorf("local prompt content=%q, want %q", content, wantContent)
	}
	if session != wantSession {
		t.Errorf("local prompt session_id=%q, want %q", session, wantSession)
	}
	if project != promptProject {
		t.Errorf("local prompt project=%q, want %q", project, promptProject)
	}
	if writer != wantWriter {
		t.Errorf("local prompt writer_id=%q, want %q", writer, wantWriter)
	}
}

// assertLocalPromptAbsent asserts no row in local user_prompts for sync_id.
func assertLocalPromptAbsent(t *testing.T, st *localstore.Store, syncID string) {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(
		`SELECT count(*) FROM user_prompts WHERE sync_id = ?`, syncID,
	).Scan(&n); err != nil {
		t.Fatalf("assertLocalPromptAbsent(%q): %v", syncID, err)
	}
	if n != 0 {
		t.Errorf("user_prompts: unexpected row for sync_id=%q", syncID)
	}
}

// assertLocalPromptTombstone asserts a tombstone row in the node's
// prompt_tombstones for the given sync_id.
func assertLocalPromptTombstone(t *testing.T, st *localstore.Store, syncID string) {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(
		`SELECT count(*) FROM prompt_tombstones WHERE sync_id = ?`, syncID,
	).Scan(&n); err != nil {
		t.Fatalf("assertLocalPromptTombstone(%q): %v", syncID, err)
	}
	if n < 1 {
		t.Errorf("prompt_tombstones: no tombstone for sync_id=%q", syncID)
	}
}

// assertLocalMemoriesNoPrompt asserts no row in local memories for sync_id.
func assertLocalMemoriesNoPrompt(t *testing.T, st *localstore.Store, syncID string) {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(
		`SELECT count(*) FROM memories WHERE sync_id = ?`, syncID,
	).Scan(&n); err != nil {
		t.Fatalf("assertLocalMemoriesNoPrompt(%q): %v", syncID, err)
	}
	if n != 0 {
		t.Errorf("memories: unexpected row for prompt sync_id=%q (dispatch bug)", syncID)
	}
}

// ── Scenario 1 — push→pull convergence ───────────────────────────────────────

// TestPromptCentral_PushPullConvergence is the fundamental convergence proof:
// node A writes a prompt, pushes to central, node B pulls — both stores and
// central agree on the prompt row. Nothing lands in central_memories or B's
// memories table.
func TestPromptCentral_PushPullConvergence(t *testing.T) {
	ctx := context.Background()
	central := newIsolatedStore(t)

	nodeA := newPromptNode(t, "A")
	nodeB := newPromptNode(t, "B")

	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	m := promptMut(writerA, "prompt-conv-1", "sess-A", "hello from A", base)

	// A writes the prompt locally (goes through localWriteLocked → applyPromptTx
	// → enqueueOutboxTx).
	if _, err := nodeA.Write(m); err != nil {
		t.Fatalf("nodeA.Write: %v", err)
	}

	// Push A's outbox to central. Central must materialize into central_user_prompts.
	pushed, err := spike.Push(ctx, nodeA, central)
	if err != nil {
		t.Fatalf("Push A: %v", err)
	}
	if pushed != 1 {
		t.Fatalf("pushed=%d, want 1", pushed)
	}

	// Central: live row present in central_user_prompts with correct columns.
	assertCentralPromptPresent(t, ctx, central.Pool(), "prompt-conv-1", "hello from A", "sess-A", writerA)

	// Central: NO row in central_memories (routing correctness).
	assertCentralMemoriesEmpty(t, ctx, central.Pool(), "prompt-conv-1")

	// No tombstone on central (live upsert, no delete yet).
	assertCentralTombstoneAbsent(t, ctx, central.Pool(), "prompt-conv-1")

	// B pulls. Node B starts empty; it needs to know about the project first so
	// Pull can use the right per-project cursor. Pull directly for the project.
	pulled, err := spike.Pull(ctx, nodeB, central, promptProject)
	if err != nil {
		t.Fatalf("Pull B: %v", err)
	}
	if pulled < 1 {
		t.Fatalf("B pulled=%d, want >=1", pulled)
	}

	// B's local user_prompts must match A's prompt.
	assertLocalPromptPresent(t, nodeB.Store, "prompt-conv-1", "hello from A", "sess-A", writerA)

	// B's memories must be empty — prompt must not have been routed there.
	assertLocalMemoriesNoPrompt(t, nodeB.Store, "prompt-conv-1")

	t.Logf("Scenario 1 PASSED: prompt converged A→central→B; no memory contamination")
}

// ── Scenario 2 — delete replication ──────────────────────────────────────────

// TestPromptCentral_DeleteReplication proves that an OpDelete pushed by node A
// is materialized on central (tombstone present, live row gone) and then pulled
// by node B so B's user_prompts row is also gone with a local tombstone.
func TestPromptCentral_DeleteReplication(t *testing.T) {
	ctx := context.Background()
	central := newIsolatedStore(t)

	nodeA := newPromptNode(t, "A")
	nodeB := newPromptNode(t, "B")

	base := time.Date(2025, 2, 1, 12, 0, 0, 0, time.UTC)

	upsertM := promptMut(writerA, "prompt-del-1", "sess-A", "to be deleted", base)
	deleteM := promptDelMut(writerA, "prompt-del-1", "sess-A", base.Add(1*time.Second))

	// A: write then delete.
	if _, err := nodeA.Write(upsertM); err != nil {
		t.Fatalf("nodeA.Write upsert: %v", err)
	}
	if _, err := nodeA.Write(deleteM); err != nil {
		t.Fatalf("nodeA.Write delete: %v", err)
	}

	// Push A (2 mutations: the upsert + the delete).
	pushed, err := spike.Push(ctx, nodeA, central)
	if err != nil {
		t.Fatalf("Push A: %v", err)
	}
	if pushed != 2 {
		t.Fatalf("pushed=%d, want 2", pushed)
	}

	// Central: live row gone, tombstone present.
	assertCentralPromptAbsent(t, ctx, central.Pool(), "prompt-del-1")
	assertCentralTombstonePresent(t, ctx, central.Pool(), "prompt-del-1")

	// B pulls both mutations.
	pulled, err := spike.Pull(ctx, nodeB, central, promptProject)
	if err != nil {
		t.Fatalf("Pull B: %v", err)
	}
	if pulled < 2 {
		t.Fatalf("B pulled=%d, want >=2", pulled)
	}

	// B: live row gone, local tombstone present.
	assertLocalPromptAbsent(t, nodeB.Store, "prompt-del-1")
	assertLocalPromptTombstone(t, nodeB.Store, "prompt-del-1")

	t.Logf("Scenario 2 PASSED: delete propagated A→central→B; tombstone on both")
}

// ── Scenario 3 — stale resurrection rejected; fresh revives ──────────────────

// TestPromptCentral_StaleResurrectionRejected_FreshRevives proves:
//  1. After a delete converges, pushing an OLDER upsert (UpdatedAt ≤ delete time)
//     leaves the row absent on central — applyPromptDecisionQ's staleness guard.
//  2. A FRESH upsert (UpdatedAt strictly after the delete) revives the row on
//     both central and node B.
func TestPromptCentral_StaleResurrectionRejected_FreshRevives(t *testing.T) {
	ctx := context.Background()
	central := newIsolatedStore(t)

	nodeA := newPromptNode(t, "A")
	nodeB := newPromptNode(t, "B")

	base := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)

	upsertM := promptMut(writerA, "prompt-stale-1", "sess-A", "original", base)
	deleteM := promptDelMut(writerA, "prompt-stale-1", "sess-A", base.Add(2*time.Second))

	// A: upsert + delete.
	for _, m := range []domain.Mutation{upsertM, deleteM} {
		if _, err := nodeA.Write(m); err != nil {
			t.Fatalf("nodeA.Write: %v", err)
		}
	}
	if _, err := spike.Push(ctx, nodeA, central); err != nil {
		t.Fatalf("Push A (upsert+delete): %v", err)
	}
	// Pre-condition: central tombstone present, live row absent.
	assertCentralPromptAbsent(t, ctx, central.Pool(), "prompt-stale-1")
	assertCentralTombstonePresent(t, ctx, central.Pool(), "prompt-stale-1")

	// Push a STALE upsert (UpdatedAt == deleteM.UpdatedAt — equal is stale).
	// Apply directly on central (simulating a late-arriving out-of-order push
	// from another device that saw only the original write).
	staleM := promptMut(writerA, "prompt-stale-1", "sess-A", "stale resurrection attempt", deleteM.UpdatedAt)
	if err := central.Apply(ctx, staleM); err != nil {
		t.Fatalf("central.Apply stale upsert: %v", err)
	}
	// Row must STILL be absent — staleness guard blocked the resurrection.
	assertCentralPromptAbsent(t, ctx, central.Pool(), "prompt-stale-1")
	assertCentralTombstonePresent(t, ctx, central.Pool(), "prompt-stale-1")
	t.Logf("stale resurrection rejected (UpdatedAt == delete time)")

	// Push a FRESH upsert (UpdatedAt strictly after the delete).
	freshM := promptMut(writerA, "prompt-stale-1", "sess-A", "fresh revival", deleteM.UpdatedAt.Add(1*time.Second))
	if err := central.Apply(ctx, freshM); err != nil {
		t.Fatalf("central.Apply fresh upsert: %v", err)
	}
	// Row must now be live; tombstone must be gone.
	assertCentralPromptPresent(t, ctx, central.Pool(), "prompt-stale-1", "fresh revival", "sess-A", writerA)
	assertCentralTombstoneAbsent(t, ctx, central.Pool(), "prompt-stale-1")
	t.Logf("fresh resurrection accepted")

	// B pulls the full history (all 4 mutations: upsert, delete, stale-upsert, fresh-upsert).
	// B should end up with the live fresh-revival row.
	if _, err := spike.Pull(ctx, nodeB, central, promptProject); err != nil {
		t.Fatalf("Pull B: %v", err)
	}
	assertLocalPromptPresent(t, nodeB.Store, "prompt-stale-1", "fresh revival", "sess-A", writerA)

	t.Logf("Scenario 3 PASSED: stale blocked, fresh revived on central and B")
}

// ── Scenario 4 — mixed memory+prompt on shared per-project cursor ─────────────

// TestPromptCentral_MixedMemoryAndPromptSharedCursor is the KEY correctness
// proof for the PR-3 design: the shared central_mutations journal carries both
// entity types under the same per-project BIGSERIAL seq. node B must pull BOTH
// the memory (into memories) AND the prompt (into user_prompts) in ONE Pull call
// using ONE per-project cursor — proving the shared-cursor design handles mixed
// entity types correctly.
func TestPromptCentral_MixedMemoryAndPromptSharedCursor(t *testing.T) {
	ctx := context.Background()
	central := newIsolatedStore(t)

	nodeA := newPromptNode(t, "A")
	nodeB := newPromptNode(t, "B")

	base := time.Date(2025, 4, 1, 12, 0, 0, 0, time.UTC)

	// Build a memory mutation for the same project.
	memTopic := "prompt-mixed/memory/1"
	tk := memTopic
	memMut := domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     "mem-mixed-1",
		SessionID:  "sess-A",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "mixed test memory",
		Content:    "memory content",
		Project:    promptProject,
		Scope:      promptScope,
		TopicKey:   &tk,
		Version:    1,
		UpdatedAt:  base.Add(1 * time.Second),
		WriterID:   writerA,
	}

	promptMutM := promptMut(writerA, "prompt-mixed-1", "sess-A", "mixed prompt content", base.Add(2*time.Second))

	// A writes BOTH: a memory and a prompt.
	if _, err := nodeA.Write(memMut); err != nil {
		t.Fatalf("nodeA.Write memory: %v", err)
	}
	if _, err := nodeA.Write(promptMutM); err != nil {
		t.Fatalf("nodeA.Write prompt: %v", err)
	}

	// Push both to central. Central assigns two consecutive seqs (interleaved in
	// the shared per-project journal).
	pushed, err := spike.Push(ctx, nodeA, central)
	if err != nil {
		t.Fatalf("Push A: %v", err)
	}
	if pushed != 2 {
		t.Fatalf("pushed=%d, want 2 (memory + prompt)", pushed)
	}

	// Verify central has BOTH materialized correctly.
	// Memory in central_memories (via FindByTopic).
	rec, err := central.FindByTopic(memTopic, promptProject, promptScope)
	if err != nil {
		t.Fatalf("central.FindByTopic: %v", err)
	}
	if rec == nil {
		t.Fatalf("central: memory %q not found after push", memTopic)
	}
	if rec.Content != "memory content" {
		t.Errorf("central memory content=%q, want %q", rec.Content, "memory content")
	}
	// Prompt in central_user_prompts.
	assertCentralPromptPresent(t, ctx, central.Pool(), "prompt-mixed-1", "mixed prompt content", "sess-A", writerA)
	// Memory NOT in central_user_prompts; prompt NOT in central_memories.
	assertCentralMemoriesEmpty(t, ctx, central.Pool(), "prompt-mixed-1")

	// Retrieve the two seqs from central_mutations to confirm they are interleaved
	// in the same per-project journal (both assigned by the same BIGSERIAL).
	var seqMem, seqPrompt int64
	if err := central.Pool().QueryRow(ctx,
		`SELECT seq FROM central_mutations WHERE entity_key = $1 AND project = $2`,
		"mem-mixed-1", promptProject,
	).Scan(&seqMem); err != nil {
		t.Fatalf("query central seq for memory: %v", err)
	}
	if err := central.Pool().QueryRow(ctx,
		`SELECT seq FROM central_mutations WHERE entity_key = $1 AND project = $2`,
		"prompt-mixed-1", promptProject,
	).Scan(&seqPrompt); err != nil {
		t.Fatalf("query central seq for prompt: %v", err)
	}
	t.Logf("central seqs: memory=%d, prompt=%d (shared per-project journal)", seqMem, seqPrompt)

	// B pulls with ONE Pull call (one per-project cursor advance). B must see
	// BOTH the memory AND the prompt in one pass.
	pulled, err := spike.Pull(ctx, nodeB, central, promptProject)
	if err != nil {
		t.Fatalf("Pull B: %v", err)
	}
	if pulled < 2 {
		t.Fatalf("B pulled=%d, want >=2 (memory + prompt)", pulled)
	}

	// B: memory present in memories, prompt present in user_prompts.
	bMemRec, err := nodeB.Store.FindByTopic(memTopic, promptProject, promptScope)
	if err != nil {
		t.Fatalf("B FindByTopic: %v", err)
	}
	if bMemRec == nil {
		t.Fatalf("B: memory %q missing after Pull — shared cursor failed for memory", memTopic)
	}
	if bMemRec.Content != "memory content" {
		t.Errorf("B memory content=%q, want %q", bMemRec.Content, "memory content")
	}
	assertLocalPromptPresent(t, nodeB.Store, "prompt-mixed-1", "mixed prompt content", "sess-A", writerA)
	assertLocalMemoriesNoPrompt(t, nodeB.Store, "prompt-mixed-1")

	// Verify no cross-contamination: memory sync_id not in user_prompts, prompt
	// sync_id not in memories.
	assertLocalPromptAbsent(t, nodeB.Store, "mem-mixed-1")
	assertLocalMemoriesNoPrompt(t, nodeB.Store, "prompt-mixed-1")

	t.Logf("Scenario 4 PASSED: mixed memory+prompt both converged on shared per-project cursor; seqs %d,%d", seqMem, seqPrompt)
}

// ── Scenario 5 — idempotency ──────────────────────────────────────────────────

// TestPromptCentral_Idempotency proves that re-pushing the same prompt mutation
// (same MutationID) produces exactly one row in central_user_prompts and one in
// B's user_prompts, with no error.
func TestPromptCentral_Idempotency(t *testing.T) {
	ctx := context.Background()
	central := newIsolatedStore(t)

	nodeA := newPromptNode(t, "A")
	nodeB := newPromptNode(t, "B")

	base := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	m := promptMut(writerA, "prompt-idem-1", "sess-A", "idempotent prompt", base)

	// A writes once.
	written, err := nodeA.Write(m)
	if err != nil {
		t.Fatalf("nodeA.Write: %v", err)
	}

	// Push once — should push 1.
	pushed, err := spike.Push(ctx, nodeA, central)
	if err != nil {
		t.Fatalf("Push 1: %v", err)
	}
	if pushed != 1 {
		t.Fatalf("first push: pushed=%d, want 1", pushed)
	}

	// Re-apply the same mutation directly on central (simulates a duplicate push
	// that races the idempotency check — covered by the UNIQUE mutation_id).
	if err := central.Apply(ctx, written); err != nil {
		t.Fatalf("central.Apply duplicate: %v", err)
	}

	// Exactly one central_user_prompts row.
	var n int
	if err := central.Pool().QueryRow(ctx,
		`SELECT count(*) FROM central_user_prompts WHERE sync_id = $1`,
		"prompt-idem-1",
	).Scan(&n); err != nil {
		t.Fatalf("count central rows: %v", err)
	}
	if n != 1 {
		t.Errorf("central_user_prompts rows for prompt-idem-1 = %d, want 1", n)
	}

	// Exactly one central_mutations row (the UNIQUE mutation_id guard).
	var mutCount int
	if err := central.Pool().QueryRow(ctx,
		`SELECT count(*) FROM central_mutations WHERE mutation_id = $1`,
		written.MutationID,
	).Scan(&mutCount); err != nil {
		t.Fatalf("count central_mutations: %v", err)
	}
	if mutCount != 1 {
		t.Errorf("central_mutations rows for mutation_id = %d, want 1 (idempotency guard)", mutCount)
	}

	// B pulls — should get exactly 1 mutation applied, no error.
	pulled, err := spike.Pull(ctx, nodeB, central, promptProject)
	if err != nil {
		t.Fatalf("Pull B: %v", err)
	}
	t.Logf("B pulled=%d mutations", pulled)

	// B's local user_prompts must have exactly one row for this sync_id.
	var bCount int
	if err := nodeB.Store.DB().QueryRow(
		`SELECT count(*) FROM user_prompts WHERE sync_id = ?`,
		"prompt-idem-1",
	).Scan(&bCount); err != nil {
		t.Fatalf("count B rows: %v", err)
	}
	if bCount != 1 {
		t.Errorf("B user_prompts rows for prompt-idem-1 = %d, want 1", bCount)
	}

	// Re-pull (simulate a second sync cycle) — cursor advances so nothing new
	// is fetched; B still has exactly 1 row and no error.
	if _, err := spike.Pull(ctx, nodeB, central, promptProject); err != nil {
		t.Fatalf("Pull B (re-pull): %v", err)
	}
	if err := nodeB.Store.DB().QueryRow(
		`SELECT count(*) FROM user_prompts WHERE sync_id = ?`,
		"prompt-idem-1",
	).Scan(&bCount); err != nil {
		t.Fatalf("count B rows after re-pull: %v", err)
	}
	if bCount != 1 {
		t.Errorf("B user_prompts rows after re-pull = %d, want 1", bCount)
	}

	t.Logf("Scenario 5 PASSED: idempotent push+pull; 1 central row, 1 B row")
}

// ── Unit-level: applyPromptDecisionQ round-trip ───────────────────────────────

// TestPromptCentral_UpsertTombstoneStaleAndFresh exercises the central
// applyPromptDecisionQ state machine directly via central.Apply:
//
//	upsert → central row present
//	delete → tombstone present, row absent
//	stale upsert (UpdatedAt == deleted_at) → no-op, row stays absent
//	fresh upsert (UpdatedAt > deleted_at) → row revives, tombstone cleared
func TestPromptCentral_UpsertTombstoneStaleAndFresh(t *testing.T) {
	ctx := context.Background()
	central := newIsolatedStore(t)

	base := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	const sid = "prompt-unit-1"

	// Upsert.
	m1 := promptMut(writerA, sid, "sess", "v1", base)
	if err := central.Apply(ctx, m1); err != nil {
		t.Fatalf("Apply upsert: %v", err)
	}
	assertCentralPromptPresent(t, ctx, central.Pool(), sid, "v1", "sess", writerA)
	assertCentralTombstoneAbsent(t, ctx, central.Pool(), sid)

	// Delete (UpdatedAt = base+1s).
	delTime := base.Add(1 * time.Second)
	d1 := promptDelMut(writerA, sid, "sess", delTime)
	if err := central.Apply(ctx, d1); err != nil {
		t.Fatalf("Apply delete: %v", err)
	}
	assertCentralPromptAbsent(t, ctx, central.Pool(), sid)
	assertCentralTombstonePresent(t, ctx, central.Pool(), sid)

	// Stale upsert: UpdatedAt == delTime (equal is stale).
	stale := promptMut(writerA, sid, "sess", "stale", delTime)
	if err := central.Apply(ctx, stale); err != nil {
		t.Fatalf("Apply stale: %v", err)
	}
	assertCentralPromptAbsent(t, ctx, central.Pool(), sid)     // still absent
	assertCentralTombstonePresent(t, ctx, central.Pool(), sid) // tombstone intact

	// Fresh upsert: UpdatedAt strictly after delTime.
	fresh := promptMut(writerA, sid, "sess", "revived", delTime.Add(1*time.Second))
	if err := central.Apply(ctx, fresh); err != nil {
		t.Fatalf("Apply fresh: %v", err)
	}
	assertCentralPromptPresent(t, ctx, central.Pool(), sid, "revived", "sess", writerA)
	assertCentralTombstoneAbsent(t, ctx, central.Pool(), sid) // tombstone cleared

	t.Logf("Unit round-trip PASSED: upsert→tombstone→stale-no-op→fresh-revive")
}

// ── Memory convergence regression ────────────────────────────────────────────

// TestPromptCentral_MemoryConvergenceUnperturbed is the regression guard:
// adding prompt support to central Apply must not perturb existing memory
// convergence. A memory write pushed and pulled must still land in central_memories
// and the puller's local memories table, not in prompt tables.
func TestPromptCentral_MemoryConvergenceUnperturbed(t *testing.T) {
	ctx := context.Background()
	central := newIsolatedStore(t)

	nodeA := newPromptNode(t, "A")
	nodeB := newPromptNode(t, "B")

	base := time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC)

	topic := "prompt-regression/memory/1"
	tk := topic
	mm := domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     "mem-regression-1",
		SessionID:  "sess-A",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "regression memory",
		Content:    "memory regression content",
		Project:    promptProject,
		Scope:      promptScope,
		TopicKey:   &tk,
		Version:    1,
		UpdatedAt:  base,
		WriterID:   writerA,
	}

	if _, err := nodeA.Write(mm); err != nil {
		t.Fatalf("nodeA.Write memory: %v", err)
	}
	if _, err := spike.Push(ctx, nodeA, central); err != nil {
		t.Fatalf("Push memory: %v", err)
	}

	// Central: memory in central_memories, NOT in central_user_prompts.
	rec, err := central.FindByTopic(topic, promptProject, promptScope)
	if err != nil || rec == nil {
		t.Fatalf("central FindByTopic(%q): err=%v rec=%v", topic, err, rec)
	}
	if rec.Content != "memory regression content" {
		t.Errorf("central memory content=%q, want %q", rec.Content, "memory regression content")
	}
	var promCount int
	if err := central.Pool().QueryRow(ctx,
		`SELECT count(*) FROM central_user_prompts WHERE sync_id = $1`,
		"mem-regression-1",
	).Scan(&promCount); err != nil {
		t.Fatalf("central_user_prompts count for memory: %v", err)
	}
	if promCount != 0 {
		t.Errorf("central_user_prompts: unexpected row for memory sync_id (dispatch regression)")
	}

	// B pulls; memory must land in memories, NOT user_prompts.
	if _, err := spike.Pull(ctx, nodeB, central, promptProject); err != nil {
		t.Fatalf("Pull B: %v", err)
	}
	bRec, err := nodeB.Store.FindByTopic(topic, promptProject, promptScope)
	if err != nil || bRec == nil {
		t.Fatalf("B FindByTopic(%q): err=%v rec=%v", topic, err, bRec)
	}
	if bRec.Content != "memory regression content" {
		t.Errorf("B memory content=%q", bRec.Content)
	}
	assertLocalPromptAbsent(t, nodeB.Store, "mem-regression-1")

	t.Logf("Regression PASSED: memory convergence unperturbed by prompt dispatch")
}
