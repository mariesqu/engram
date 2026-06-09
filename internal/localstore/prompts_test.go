package localstore

// Tests for the EntityPrompt local materialization path:
//   - AddPrompt: row in user_prompts + outbox entry with entity=prompt
//   - Cross-store dispatch: A → drain → ApplyPulled on B → user_prompts on B;
//     nothing lands in memories on B (dispatch correctness).
//   - AddPromptIfMissing dedup: same session+project+content → one row;
//     different content → two rows.
//   - Tombstone + resurrection guard:
//       delete → row gone + tombstone present
//       stale upsert (UpdatedAt ≤ tombstone.deleted_at) → no-op
//       fresh upsert (UpdatedAt > tombstone.deleted_at) → revives
//   - Idempotency: ApplyPulled twice → one row, no error.
//   - REGRESSION: AddObservation still flows through Decide → memories
//     (the EntityPrompt dispatch must not perturb the memory path).
//   - REGRESSION (concurrent mix): prompt + memory write don't deadlock.
//   - ListProjects includes a prompt-only project.

import (
	"sync"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/mutation"
)

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// countUserPrompts returns the number of rows in user_prompts for the given syncID.
func countUserPromptsBySyncID(t *testing.T, s *Store, syncID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_prompts WHERE sync_id = ?`, syncID).Scan(&n); err != nil {
		t.Fatalf("countUserPromptsBySyncID: %v", err)
	}
	return n
}

// countPromptTombstones returns the number of tombstone rows for the given syncID.
func countPromptTombstones(t *testing.T, s *Store, syncID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM prompt_tombstones WHERE sync_id = ?`, syncID).Scan(&n); err != nil {
		t.Fatalf("countPromptTombstones: %v", err)
	}
	return n
}

// countMemoriesBySyncID returns the number of rows in memories for the given syncID.
func countMemoriesBySyncID(t *testing.T, s *Store, syncID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE sync_id = ?`, syncID).Scan(&n); err != nil {
		t.Fatalf("countMemoriesBySyncID: %v", err)
	}
	return n
}

// promptMut builds a minimal OpUpsert EntityPrompt mutation.
func promptMut(syncID, project, content, writerID string, at time.Time) domain.Mutation {
	return domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess-" + syncID,
		EntityType: domain.EntityPrompt,
		Content:    content,
		Project:    project,
		Scope:      "project",
		Version:    1,
		UpdatedAt:  at,
		WriterID:   writerID,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestAddPrompt_RowAndOutbox
// ─────────────────────────────────────────────────────────────────────────────

// TestAddPrompt_RowAndOutbox verifies that AddPrompt:
//  1. Creates a row in user_prompts.
//  2. Enqueues exactly one pending outbox entry with the correct entity type,
//     writer_id, and a valid content-addressed mutation_id.
func TestAddPrompt_RowAndOutbox(t *testing.T) {
	s := openTempStore(t)

	got, err := s.AddPrompt(AddPromptParams{
		SessionID: "sess-add",
		Content:   "hello world",
		Project:   "engram",
		WriterID:  "writer-a",
	})
	if err != nil {
		t.Fatalf("AddPrompt: %v", err)
	}

	if got.ID == 0 {
		t.Error("AddPrompt: ID must be non-zero")
	}
	if got.SyncID == "" {
		t.Error("AddPrompt: SyncID must be non-empty")
	}

	// Row in user_prompts.
	if n := countUserPromptsBySyncID(t, s, got.SyncID); n != 1 {
		t.Errorf("user_prompts rows for sync_id=%q: got %d, want 1", got.SyncID, n)
	}

	// Outbox entry.
	entries, err := s.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("outbox entries: got %d, want 1", len(entries))
	}

	e := entries[0].Mutation
	if e.EntityType != domain.EntityPrompt {
		t.Errorf("outbox entity_type=%q, want %q", e.EntityType, domain.EntityPrompt)
	}
	if e.WriterID != "writer-a" {
		t.Errorf("outbox writer_id=%q, want %q", e.WriterID, "writer-a")
	}
	// MutationID must be the content-addressed hash of the canonical payload.
	wantID := mutation.NewMutationID(mutation.CanonicalPayload(e))
	if e.MutationID != wantID {
		t.Errorf("outbox mutation_id=%q, want content-addressed %q", e.MutationID, wantID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCrossStoreDispatch_PromptToB_NotInMemories
// ─────────────────────────────────────────────────────────────────────────────

// TestCrossStoreDispatch_PromptToB_NotInMemories verifies the cross-store
// dispatch path:
//  1. AddPrompt on store A produces an outbox entry.
//  2. Drain the outbox and ApplyPulled the mutation on store B.
//  3. Store B has the prompt row in user_prompts.
//  4. Store B's memories table has ZERO rows for that sync_id (the dispatch
//     correctly routed the prompt away from the memories path).
func TestCrossStoreDispatch_PromptToB_NotInMemories(t *testing.T) {
	storeA := openTempStore(t)
	storeB := openTempStore(t)

	_, err := storeA.AddPrompt(AddPromptParams{
		SessionID: "sess-cross",
		Content:   "cross-store content",
		Project:   "myproject",
		WriterID:  "writer-x",
	})
	if err != nil {
		t.Fatalf("AddPrompt on A: %v", err)
	}

	entries, err := storeA.DrainOutbox(0)
	if err != nil {
		t.Fatalf("DrainOutbox on A: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 outbox entry, got %d", len(entries))
	}

	mut := entries[0].Mutation

	if err := storeB.ApplyPulled(mut); err != nil {
		t.Fatalf("ApplyPulled on B: %v", err)
	}

	// user_prompts on B has the row.
	if n := countUserPromptsBySyncID(t, storeB, mut.SyncID); n != 1 {
		t.Errorf("storeB user_prompts: got %d rows for sync_id=%q, want 1", n, mut.SyncID)
	}

	// memories on B has ZERO rows (dispatch must not route prompts to memories).
	if n := countMemoriesBySyncID(t, storeB, mut.SyncID); n != 0 {
		t.Errorf("storeB memories: got %d rows for sync_id=%q, want 0 (prompt must not reach memories)", n, mut.SyncID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestAddPromptIfMissing_Dedup
// ─────────────────────────────────────────────────────────────────────────────

// TestAddPromptIfMissing_Dedup verifies the dedup semantics:
//   - Same (session, project, content) twice → exactly one row.
//   - Different content → two rows.
func TestAddPromptIfMissing_Dedup(t *testing.T) {
	s := openTempStore(t)

	p := AddPromptParams{
		SessionID: "sess-dedup",
		Content:   "deduplicated content",
		Project:   "engram",
		WriterID:  "writer-b",
	}

	first, err := s.AddPromptIfMissing(p)
	if err != nil {
		t.Fatalf("first AddPromptIfMissing: %v", err)
	}

	second, err := s.AddPromptIfMissing(p)
	if err != nil {
		t.Fatalf("second AddPromptIfMissing: %v", err)
	}

	// Same identity: same row must be returned.
	if first.ID != second.ID {
		t.Errorf("dedup: first.ID=%d, second.ID=%d — expected same row", first.ID, second.ID)
	}
	if first.SyncID != second.SyncID {
		t.Errorf("dedup: first.SyncID=%q, second.SyncID=%q — expected same row", first.SyncID, second.SyncID)
	}

	// Different content → a NEW row.
	other, err := s.AddPromptIfMissing(AddPromptParams{
		SessionID: "sess-dedup",
		Content:   "different content",
		Project:   "engram",
		WriterID:  "writer-b",
	})
	if err != nil {
		t.Fatalf("AddPromptIfMissing (other content): %v", err)
	}
	if other.ID == first.ID {
		t.Error("different content must produce a new row, but got the same ID")
	}

	// Total user_prompts rows in the store should be 2.
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_prompts`).Scan(&total); err != nil {
		t.Fatalf("count user_prompts: %v", err)
	}
	if total != 2 {
		t.Errorf("user_prompts total rows: got %d, want 2", total)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestTombstone_DeleteRevive
// ─────────────────────────────────────────────────────────────────────────────

// TestTombstone_DeleteRevive exercises the full tombstone / resurrection sequence:
//  1. Apply upsert → row present.
//  2. Apply delete → row gone + tombstone present.
//  3. Apply STALE upsert (UpdatedAt <= tombstone.deleted_at) → no-op (row stays gone).
//  4. Apply FRESH upsert (UpdatedAt > tombstone.deleted_at) → row revived + tombstone gone.
func TestTombstone_DeleteRevive(t *testing.T) {
	s := openTempStore(t)

	syncID := "prompt-tomb-test"
	t0 := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(10 * time.Second)
	t3 := t0.Add(30 * time.Second) // fresh

	// 1. Insert via ApplyPulled.
	upsert := promptMut(syncID, "proj", "content", "w1", t0)
	if err := s.ApplyPulled(upsert); err != nil {
		t.Fatalf("ApplyPulled upsert: %v", err)
	}
	if n := countUserPromptsBySyncID(t, s, syncID); n != 1 {
		t.Fatalf("after upsert: user_prompts rows=%d, want 1", n)
	}

	// 2. Delete.
	del := domain.Mutation{
		Op:         domain.OpDelete,
		SyncID:     syncID,
		SessionID:  "sess-del",
		EntityType: domain.EntityPrompt,
		Project:    "proj",
		Scope:      "project",
		UpdatedAt:  t1,
		WriterID:   "w1",
	}
	if err := s.ApplyPulled(del); err != nil {
		t.Fatalf("ApplyPulled delete: %v", err)
	}
	if n := countUserPromptsBySyncID(t, s, syncID); n != 0 {
		t.Errorf("after delete: user_prompts rows=%d, want 0", n)
	}
	if n := countPromptTombstones(t, s, syncID); n != 1 {
		t.Errorf("after delete: tombstone rows=%d, want 1", n)
	}

	// 3. Stale upsert (UpdatedAt == tombstone.deleted_at — equal is stale).
	staleUpsert := promptMut(syncID, "proj", "content-stale", "w1", t1) // t1 == deleted_at
	if err := s.ApplyPulled(staleUpsert); err != nil {
		t.Fatalf("ApplyPulled stale upsert: %v", err)
	}
	// Row must still be gone.
	if n := countUserPromptsBySyncID(t, s, syncID); n != 0 {
		t.Errorf("after stale upsert: user_prompts rows=%d, want 0 (resurrection denied)", n)
	}

	// Re-confirm with UpdatedAt strictly less than tombstone (t0 < t1).
	staleUpsert2 := promptMut(syncID, "proj", "content-stale2", "w1", t0) // t0 < t1
	if err := s.ApplyPulled(staleUpsert2); err != nil {
		t.Fatalf("ApplyPulled stale upsert2: %v", err)
	}
	if n := countUserPromptsBySyncID(t, s, syncID); n != 0 {
		t.Errorf("after stale upsert2: user_prompts rows=%d, want 0", n)
	}
	if n := countPromptTombstones(t, s, syncID); n != 1 {
		t.Errorf("after stale upsert2: tombstone rows=%d, want 1", n)
	}

	// 4. Fresh upsert (UpdatedAt strictly after tombstone.deleted_at).
	freshUpsert := promptMut(syncID, "proj", "content-fresh", "w1", t3)
	if err := s.ApplyPulled(freshUpsert); err != nil {
		t.Fatalf("ApplyPulled fresh upsert: %v", err)
	}
	if n := countUserPromptsBySyncID(t, s, syncID); n != 1 {
		t.Errorf("after fresh upsert: user_prompts rows=%d, want 1 (revived)", n)
	}
	if n := countPromptTombstones(t, s, syncID); n != 0 {
		t.Errorf("after fresh upsert: tombstone rows=%d, want 0 (removed)", n)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestApplyPulled_PromptIdempotent
// ─────────────────────────────────────────────────────────────────────────────

// TestApplyPulled_PromptIdempotent verifies that applying the same prompt
// mutation twice via ApplyPulled produces exactly one user_prompts row and no
// error (INV5 idempotency).
func TestApplyPulled_PromptIdempotent(t *testing.T) {
	s := openTempStore(t)

	m := promptMut("prompt-idem", "proj", "idempotent content", "w1", baseT)
	// Normalize so MutationID is set (mirrors what central would send).
	m = normalizeMutation(m)

	if err := s.ApplyPulled(m); err != nil {
		t.Fatalf("first ApplyPulled: %v", err)
	}
	if err := s.ApplyPulled(m); err != nil {
		t.Fatalf("second ApplyPulled: %v", err)
	}

	if n := countUserPromptsBySyncID(t, s, m.SyncID); n != 1 {
		t.Errorf("idempotency: user_prompts rows=%d, want 1", n)
	}

	// applied_mutations must have exactly one row for this mutation_id.
	var cnt int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM applied_mutations WHERE mutation_id = ?`, m.MutationID,
	).Scan(&cnt); err != nil {
		t.Fatalf("applied_mutations count: %v", err)
	}
	if cnt != 1 {
		t.Errorf("applied_mutations rows=%d, want 1 (INSERT OR IGNORE)", cnt)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestRegression_MemoryPathUnperturbed
// ─────────────────────────────────────────────────────────────────────────────

// TestRegression_MemoryPathUnperturbed ensures the EntityPrompt dispatch does
// NOT break the existing memory write path.  A normal AddObservation must
// still flow through domain.Decide and materialize in memories, not in
// user_prompts.
func TestRegression_MemoryPathUnperturbed(t *testing.T) {
	s := openTempStore(t)

	res, err := s.AddObservation(AddObservationParams{
		SessionID: "sess-reg",
		Type:      "decision",
		Title:     "regression check",
		Content:   "memory path must still work",
		Project:   "engram",
		Scope:     "project",
		WriterID:  "writer-r",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	if res.ID == 0 {
		t.Error("AddObservation: ID must be non-zero")
	}

	// Row must be in memories.
	if n := countMemoriesBySyncID(t, s, res.SyncID); n != 1 {
		t.Errorf("memories rows for sync_id=%q: got %d, want 1", res.SyncID, n)
	}

	// Row must NOT appear in user_prompts.
	if n := countUserPromptsBySyncID(t, s, res.SyncID); n != 0 {
		t.Errorf("user_prompts must be empty for a memory sync_id, got %d", n)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestRegression_ConcurrentMemoryAndPrompt_NoDeadlock
// ─────────────────────────────────────────────────────────────────────────────

// TestRegression_ConcurrentMemoryAndPrompt_NoDeadlock fires both AddObservation
// and AddPrompt concurrently to verify that the write mutex is correctly
// serializing them without deadlock.
func TestRegression_ConcurrentMemoryAndPrompt_NoDeadlock(t *testing.T) {
	s := openTempStore(t)

	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n*2)

	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_, err := s.AddObservation(AddObservationParams{
				SessionID: "sess",
				Type:      "decision",
				Title:     "concurrent memory",
				Content:   "content",
				Project:   "engram",
				WriterID:  "w1",
			})
			if err != nil {
				errs <- err
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			_, err := s.AddPrompt(AddPromptParams{
				SessionID: "sess",
				Content:   "concurrent prompt",
				Project:   "engram",
				WriterID:  "w1",
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent write error: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestListProjects_IncludesPromptOnlyProject
// ─────────────────────────────────────────────────────────────────────────────

// TestListProjects_IncludesPromptOnlyProject verifies that ListProjects returns
// a project that exists ONLY in user_prompts (no memories for that project).
func TestListProjects_IncludesPromptOnlyProject(t *testing.T) {
	s := openTempStore(t)

	// Add a memory to project "mem-project".
	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "sess",
		Type:      "decision",
		Title:     "title",
		Content:   "content",
		Project:   "mem-project",
		WriterID:  "w1",
	}); err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	// Add a prompt to project "prompt-only-project" (no memories there).
	if _, err := s.AddPrompt(AddPromptParams{
		SessionID: "sess",
		Content:   "prompt content",
		Project:   "prompt-only-project",
		WriterID:  "w1",
	}); err != nil {
		t.Fatalf("AddPrompt: %v", err)
	}

	projects, err := s.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}

	found := make(map[string]bool)
	for _, p := range projects {
		found[p] = true
	}

	if !found["mem-project"] {
		t.Error("ListProjects must include 'mem-project'")
	}
	if !found["prompt-only-project"] {
		t.Error("ListProjects must include 'prompt-only-project' (prompt-only, no memories)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestListProjects_IncludesPromptTombstoneProject
// ─────────────────────────────────────────────────────────────────────────────

// TestListProjects_IncludesPromptTombstoneProject verifies that ListProjects
// returns a project that exists ONLY in prompt_tombstones (the prompt was
// deleted, leaving only a tombstone).
func TestListProjects_IncludesPromptTombstoneProject(t *testing.T) {
	s := openTempStore(t)

	syncID := "prompt-tombproj"
	t0 := baseT

	// Insert then delete the prompt so only prompt_tombstones has the project.
	upsert := promptMut(syncID, "tombstone-only-project", "deleted content", "w1", t0)
	if err := s.ApplyPulled(upsert); err != nil {
		t.Fatalf("ApplyPulled upsert: %v", err)
	}
	del := domain.Mutation{
		Op:         domain.OpDelete,
		SyncID:     syncID,
		SessionID:  "sess",
		EntityType: domain.EntityPrompt,
		Project:    "tombstone-only-project",
		Scope:      "project",
		UpdatedAt:  t0.Add(5 * time.Second),
		WriterID:   "w1",
	}
	if err := s.ApplyPulled(del); err != nil {
		t.Fatalf("ApplyPulled delete: %v", err)
	}
	// Confirm the row is gone from user_prompts.
	if n := countUserPromptsBySyncID(t, s, syncID); n != 0 {
		t.Fatalf("user_prompts rows=%d after delete, want 0", n)
	}

	projects, err := s.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	found := false
	for _, p := range projects {
		if p == "tombstone-only-project" {
			found = true
			break
		}
	}
	if !found {
		t.Error("ListProjects must include 'tombstone-only-project' (only in prompt_tombstones)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestApplyPromptTx_GuardRejectsEmptySyncIDOnDelete
// ─────────────────────────────────────────────────────────────────────────────

// TestApplyPromptTx_GuardRejectsEmptySyncIDOnDelete verifies the delete guard:
// a delete mutation with an empty sync_id returns an error rather than silently
// deleting all rows.
func TestApplyPromptTx_GuardRejectsEmptySyncIDOnDelete(t *testing.T) {
	s := openTempStore(t)

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	m := domain.Mutation{
		Op:         domain.OpDelete,
		SyncID:     "", // empty — must be rejected
		EntityType: domain.EntityPrompt,
		UpdatedAt:  baseT,
	}
	if err := applyPromptTx(tx, m); err == nil {
		t.Error("applyPromptTx must reject an OpDelete mutation with empty sync_id")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestAppliedMutations_PromptRecorded
// ─────────────────────────────────────────────────────────────────────────────

// TestAppliedMutations_PromptRecorded verifies that both upsert and delete
// prompt mutations write to applied_mutations so they satisfy the INV5
// idempotency contract shared with memories.
func TestAppliedMutations_PromptRecorded(t *testing.T) {
	s := openTempStore(t)

	t0 := baseT

	// Upsert.
	upsertM := normalizeMutation(promptMut("prompt-am-test", "proj", "content", "w1", t0))
	if err := s.ApplyPulled(upsertM); err != nil {
		t.Fatalf("ApplyPulled upsert: %v", err)
	}

	var cnt int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM applied_mutations WHERE mutation_id = ?`, upsertM.MutationID,
	).Scan(&cnt); err != nil {
		t.Fatalf("applied_mutations upsert: %v", err)
	}
	if cnt != 1 {
		t.Errorf("applied_mutations upsert rows=%d, want 1", cnt)
	}

	// Delete.
	delM := normalizeMutation(domain.Mutation{
		Op:         domain.OpDelete,
		SyncID:     "prompt-am-test",
		SessionID:  "sess",
		EntityType: domain.EntityPrompt,
		Project:    "proj",
		Scope:      "project",
		UpdatedAt:  t0.Add(5 * time.Second),
		WriterID:   "w1",
	})
	if err := s.ApplyPulled(delM); err != nil {
		t.Fatalf("ApplyPulled delete: %v", err)
	}
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM applied_mutations WHERE mutation_id = ?`, delM.MutationID,
	).Scan(&cnt); err != nil {
		t.Fatalf("applied_mutations delete: %v", err)
	}
	if cnt != 1 {
		t.Errorf("applied_mutations delete rows=%d, want 1", cnt)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestIsStalePromptUpsert
// ─────────────────────────────────────────────────────────────────────────────

// TestIsStalePromptUpsert is a pure-unit test for the staleness guard helper.
func TestIsStalePromptUpsert(t *testing.T) {
	t0 := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(10 * time.Second)

	cases := []struct {
		name          string
		mutationAt    time.Time
		tombDeletedAt string
		wantStale     bool
	}{
		{
			name:          "mutation before tombstone is stale",
			mutationAt:    t0,
			tombDeletedAt: t1.UTC().Format(time.RFC3339Nano),
			wantStale:     true,
		},
		{
			name:          "mutation equal to tombstone is stale (not strictly after)",
			mutationAt:    t1,
			tombDeletedAt: t1.UTC().Format(time.RFC3339Nano),
			wantStale:     true,
		},
		{
			name:          "mutation after tombstone is fresh",
			mutationAt:    t1.Add(1 * time.Second),
			tombDeletedAt: t1.UTC().Format(time.RFC3339Nano),
			wantStale:     false,
		},
		{
			name:          "empty tombstone timestamp is treated as fresh (guard disabled)",
			mutationAt:    t0,
			tombDeletedAt: "",
			wantStale:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := domain.Mutation{
				Op:         domain.OpUpsert,
				SyncID:     "x",
				EntityType: domain.EntityPrompt,
				UpdatedAt:  tc.mutationAt,
			}
			got := isStalePromptUpsert(m, tc.tombDeletedAt)
			if got != tc.wantStale {
				t.Errorf("isStalePromptUpsert = %v, want %v", got, tc.wantStale)
			}
		})
	}
}
