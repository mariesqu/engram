package localstore

// Tests for PurgeForResync and the honored_purge_epoch accessors.
//
// Coverage:
//   - HonoredPurgeEpoch defaults to 0 on a fresh store; SetHonoredPurgeEpoch
//     persists and round-trips.
//   - PurgeForResync hard-deletes rows for SYNCED projects across all five
//     tables (memories, user_prompts, memory_tombstones, prompt_tombstones,
//     sync_mutations), clears applied_mutations, resets pull cursors, and
//     records the new honored epoch — all atomically.
//   - A local-only project's data AND policy are left completely untouched.
//   - The synced project's own policy row is untouched (unlike PurgeProjectLocal).
//   - A failed transaction (simulated via a closed DB) leaves the honored
//     epoch unrecorded (crash-safety).

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// TestHonoredPurgeEpoch_DefaultsToZero verifies a fresh store has never
// honored any purge.
func TestHonoredPurgeEpoch_DefaultsToZero(t *testing.T) {
	st := openTempStore(t)

	epoch, err := st.HonoredPurgeEpoch()
	if err != nil {
		t.Fatalf("HonoredPurgeEpoch: %v", err)
	}
	if epoch != 0 {
		t.Errorf("HonoredPurgeEpoch = %d; want 0 on a fresh store", epoch)
	}
}

// TestSetHonoredPurgeEpoch_RoundTrips verifies SetHonoredPurgeEpoch persists a
// value that HonoredPurgeEpoch then returns, including on a second call
// (upsert, not insert-only).
func TestSetHonoredPurgeEpoch_RoundTrips(t *testing.T) {
	st := openTempStore(t)

	if err := st.SetHonoredPurgeEpoch(5); err != nil {
		t.Fatalf("SetHonoredPurgeEpoch(5): %v", err)
	}
	got, err := st.HonoredPurgeEpoch()
	if err != nil {
		t.Fatalf("HonoredPurgeEpoch: %v", err)
	}
	if got != 5 {
		t.Errorf("HonoredPurgeEpoch = %d; want 5", got)
	}

	// Upsert to a new value.
	if err := st.SetHonoredPurgeEpoch(9); err != nil {
		t.Fatalf("SetHonoredPurgeEpoch(9): %v", err)
	}
	got, err = st.HonoredPurgeEpoch()
	if err != nil {
		t.Fatalf("HonoredPurgeEpoch (2nd): %v", err)
	}
	if got != 9 {
		t.Errorf("HonoredPurgeEpoch = %d; want 9 after re-set", got)
	}
}

// seedFullPurgeScenario builds a store with:
//   - "synced-proj" (explicit policy=synced): one live memory (via LocalWrite,
//     populating sync_mutations + memories), one pulled mutation (via
//     ApplyPulled, populating applied_mutations without touching the outbox),
//     one tombstone, one prompt, one prompt tombstone, and a non-zero pull
//     cursor.
//   - "local-proj" (explicit policy=local-only): one live memory + one pulled
//     mutation (applied_mutations row), used to prove PurgeForResync never
//     touches local-only data.
//
// Returns the store for the caller to purge and assert against.
func seedFullPurgeScenario(t *testing.T) *Store {
	t.Helper()
	st := openTempStore(t)

	// ── synced-proj: explicit policy ────────────────────────────────────────
	if err := st.SetPolicy("synced-proj", PolicySynced); err != nil {
		t.Fatalf("SetPolicy(synced-proj): %v", err)
	}

	// Live memory via LocalWrite — populates memories AND sync_mutations (outbox).
	if _, err := st.LocalWrite(domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     "sync-purge-local-1",
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "synced local write",
		Content:    "content",
		Project:    "synced-proj",
		Scope:      "project",
		Version:    1,
		WriterID:   "w",
		UpdatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("LocalWrite(synced-proj): %v", err)
	}

	// Pulled memory via ApplyPulled — populates memories AND applied_mutations,
	// but NOT sync_mutations (pulled mutations are never re-enqueued).
	if err := st.ApplyPulled(domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     "sync-purge-pulled-1",
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "synced pulled write",
		Content:    "content",
		Project:    "synced-proj",
		Scope:      "project",
		Version:    1,
		WriterID:   "other-writer",
		UpdatedAt:  time.Now().UTC(),
		MutationID: "mut-purge-pulled-1",
		Seq:        1,
	}); err != nil {
		t.Fatalf("ApplyPulled(synced-proj): %v", err)
	}

	// A tombstone: pull a delete for a third sync_id.
	if err := st.ApplyPulled(domain.Mutation{
		Op:         domain.OpDelete,
		SyncID:     "sync-purge-tomb-1",
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Project:    "synced-proj",
		Scope:      "project",
		Version:    1,
		WriterID:   "other-writer",
		UpdatedAt:  time.Now().UTC(),
		MutationID: "mut-purge-tomb-1",
		Seq:        2,
	}); err != nil {
		t.Fatalf("ApplyPulled delete (synced-proj): %v", err)
	}

	// A prompt + prompt tombstone, raw-inserted (prompts have their own apply
	// path but direct seeding keeps this test focused on PurgeForResync's SQL).
	if _, err := st.DB().Exec(
		`INSERT INTO user_prompts (sync_id, session_id, content, project, writer_id) VALUES (?, 'sess', 'prompt content', ?, 'w')`,
		"sync-purge-prompt-1", "synced-proj",
	); err != nil {
		t.Fatalf("seed user_prompts: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO prompt_tombstones (sync_id, session_id, project, deleted_at, deleted_by) VALUES (?, 'sess', ?, datetime('now'), 'w')`,
		"sync-purge-prompt-tomb-1", "synced-proj",
	); err != nil {
		t.Fatalf("seed prompt_tombstones: %v", err)
	}

	// Advance the pull cursor for synced-proj so we can assert it gets reset.
	if err := st.SetPullCursorFor("synced-proj", 42); err != nil {
		t.Fatalf("SetPullCursorFor(synced-proj): %v", err)
	}

	// ── local-proj: explicit local-only policy ──────────────────────────────
	if err := st.SetPolicy("local-proj", PolicyLocalOnly); err != nil {
		t.Fatalf("SetPolicy(local-proj): %v", err)
	}
	if _, err := st.LocalWrite(domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     "sync-purge-localonly-1",
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "local-only write",
		Content:    "content",
		Project:    "local-proj",
		Scope:      "project",
		Version:    1,
		WriterID:   "w",
		UpdatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("LocalWrite(local-proj): %v", err)
	}
	if err := st.ApplyPulled(domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     "sync-purge-localonly-pulled-1",
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "local-only pulled write",
		Content:    "content",
		Project:    "local-proj",
		Scope:      "project",
		Version:    1,
		WriterID:   "other-writer",
		UpdatedAt:  time.Now().UTC(),
		MutationID: "mut-purge-localonly-pulled-1",
		Seq:        1,
	}); err != nil {
		t.Fatalf("ApplyPulled(local-proj): %v", err)
	}
	if err := st.SetPullCursorFor("local-proj", 7); err != nil {
		t.Fatalf("SetPullCursorFor(local-proj): %v", err)
	}

	return st
}

func countRows(t *testing.T, st *Store, table, whereCol, project string) int {
	t.Helper()
	var n int
	q := "SELECT COUNT(*) FROM " + table + " WHERE " + whereCol + " = ? COLLATE NOCASE"
	if err := st.DB().QueryRow(q, project).Scan(&n); err != nil {
		t.Fatalf("count %s for %q: %v", table, project, err)
	}
	return n
}

// TestPurgeForResync_PurgesSyncedProjectOnly is the core scenario: verify that
// PurgeForResync hard-deletes ALL five tables' rows for the synced project,
// clears applied_mutations, resets its pull cursor, records the new epoch —
// while leaving the local-only project's data, policy, and cursor COMPLETELY
// untouched, and leaving the synced project's OWN policy row untouched too.
func TestPurgeForResync_PurgesSyncedProjectOnly(t *testing.T) {
	st := seedFullPurgeScenario(t)

	// Preconditions.
	if countRows(t, st, "memories", "project", "synced-proj") != 2 {
		t.Fatal("precondition: expected 2 memories for synced-proj")
	}
	if countRows(t, st, "memory_tombstones", "project", "synced-proj") != 1 {
		t.Fatal("precondition: expected 1 tombstone for synced-proj")
	}
	if countRows(t, st, "user_prompts", "project", "synced-proj") != 1 {
		t.Fatal("precondition: expected 1 prompt for synced-proj")
	}
	if countRows(t, st, "prompt_tombstones", "project", "synced-proj") != 1 {
		t.Fatal("precondition: expected 1 prompt tombstone for synced-proj")
	}
	var syncMutBefore int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM sync_mutations WHERE json_extract(payload, '$.project') = ? COLLATE NOCASE`,
		"synced-proj",
	).Scan(&syncMutBefore); err != nil {
		t.Fatalf("count sync_mutations before: %v", err)
	}
	if syncMutBefore == 0 {
		t.Fatal("precondition: expected at least 1 outbox row for synced-proj")
	}
	var appliedBefore int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM applied_mutations`).Scan(&appliedBefore); err != nil {
		t.Fatalf("count applied_mutations before: %v", err)
	}
	if appliedBefore < 3 { // 2 pulled writes on synced-proj + 1 on local-proj
		t.Fatalf("precondition: expected >= 3 applied_mutations rows, got %d", appliedBefore)
	}
	cursorBefore, err := st.PullCursorFor("synced-proj")
	if err != nil {
		t.Fatalf("PullCursorFor(synced-proj) before: %v", err)
	}
	if cursorBefore != 42 {
		t.Fatalf("precondition: expected pull cursor 42, got %d", cursorBefore)
	}

	// ── Run the purge ────────────────────────────────────────────────────────
	result, err := st.PurgeForResync(3)
	if err != nil {
		t.Fatalf("PurgeForResync: %v", err)
	}

	if len(result.Projects) != 1 || result.Projects[0] != "synced-proj" {
		t.Errorf("result.Projects = %v; want [\"synced-proj\"]", result.Projects)
	}
	if result.MemoriesDeleted != 2 {
		t.Errorf("MemoriesDeleted = %d; want 2", result.MemoriesDeleted)
	}
	if result.MemoryTombstones != 1 {
		t.Errorf("MemoryTombstones = %d; want 1", result.MemoryTombstones)
	}
	if result.UserPromptsDeleted != 1 {
		t.Errorf("UserPromptsDeleted = %d; want 1", result.UserPromptsDeleted)
	}
	if result.PromptTombstones != 1 {
		t.Errorf("PromptTombstones = %d; want 1", result.PromptTombstones)
	}
	if result.SyncMutations != int64(syncMutBefore) {
		t.Errorf("SyncMutations = %d; want %d", result.SyncMutations, syncMutBefore)
	}
	if result.PullCursorsDeleted != 1 {
		t.Errorf("PullCursorsDeleted = %d; want 1", result.PullCursorsDeleted)
	}
	if result.NewHonoredEpoch != 3 {
		t.Errorf("NewHonoredEpoch = %d; want 3", result.NewHonoredEpoch)
	}

	// ── Post-conditions: synced-proj is fully gone ──────────────────────────
	if n := countRows(t, st, "memories", "project", "synced-proj"); n != 0 {
		t.Errorf("memories remaining for synced-proj = %d; want 0", n)
	}
	if n := countRows(t, st, "memory_tombstones", "project", "synced-proj"); n != 0 {
		t.Errorf("memory_tombstones remaining for synced-proj = %d; want 0", n)
	}
	if n := countRows(t, st, "user_prompts", "project", "synced-proj"); n != 0 {
		t.Errorf("user_prompts remaining for synced-proj = %d; want 0", n)
	}
	if n := countRows(t, st, "prompt_tombstones", "project", "synced-proj"); n != 0 {
		t.Errorf("prompt_tombstones remaining for synced-proj = %d; want 0", n)
	}
	var syncMutAfter int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM sync_mutations WHERE json_extract(payload, '$.project') = ? COLLATE NOCASE`,
		"synced-proj",
	).Scan(&syncMutAfter); err != nil {
		t.Fatalf("count sync_mutations after: %v", err)
	}
	if syncMutAfter != 0 {
		t.Errorf("sync_mutations remaining for synced-proj = %d; want 0", syncMutAfter)
	}
	var appliedAfter int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM applied_mutations`).Scan(&appliedAfter); err != nil {
		t.Fatalf("count applied_mutations after: %v", err)
	}
	if appliedAfter != 0 {
		t.Errorf("applied_mutations remaining = %d; want 0 (whole table cleared — see PurgeForResync doc)", appliedAfter)
	}
	cursorAfter, err := st.PullCursorFor("synced-proj")
	if err != nil {
		t.Fatalf("PullCursorFor(synced-proj) after: %v", err)
	}
	if cursorAfter != 0 {
		t.Errorf("pull cursor for synced-proj = %d; want 0 (reset)", cursorAfter)
	}

	// synced-proj's OWN policy row must be untouched (still 'synced' — NOT
	// flipped to 'omitted' the way PurgeProjectLocal would).
	pol, err := st.GetPolicy("synced-proj")
	if err != nil {
		t.Fatalf("GetPolicy(synced-proj) after purge: %v", err)
	}
	if pol != PolicySynced {
		t.Errorf("GetPolicy(synced-proj) after purge = %q; want %q (policy must be untouched)", pol, PolicySynced)
	}

	// ── Post-conditions: local-proj is COMPLETELY untouched ─────────────────
	// (applied_mutations was cleared entirely for both projects — see the
	// PurgeForResync doc comment's justification — so the strongest available
	// "untouched" signal for local-proj is that its DATA and POLICY survive.)
	if n := countRows(t, st, "memories", "project", "local-proj"); n != 2 {
		t.Errorf("memories remaining for local-proj = %d; want 2 (untouched)", n)
	}
	pol, err = st.GetPolicy("local-proj")
	if err != nil {
		t.Fatalf("GetPolicy(local-proj) after purge: %v", err)
	}
	if pol != PolicyLocalOnly {
		t.Errorf("GetPolicy(local-proj) after purge = %q; want %q (must never change)", pol, PolicyLocalOnly)
	}
	cursorLocal, err := st.PullCursorFor("local-proj")
	if err != nil {
		t.Fatalf("PullCursorFor(local-proj) after: %v", err)
	}
	if cursorLocal != 7 {
		t.Errorf("pull cursor for local-proj = %d; want 7 (untouched — only synced-proj's cursor resets)", cursorLocal)
	}

	// Honored epoch recorded.
	honored, err := st.HonoredPurgeEpoch()
	if err != nil {
		t.Fatalf("HonoredPurgeEpoch after purge: %v", err)
	}
	if honored != 3 {
		t.Errorf("HonoredPurgeEpoch after purge = %d; want 3", honored)
	}
}

// TestPurgeForResync_OmittedProjectNeverTouched proves an omitted project's
// data survives PurgeForResync exactly like a local-only project's.
func TestPurgeForResync_OmittedProjectNeverTouched(t *testing.T) {
	st := openTempStore(t)

	seedMemory(t, st, "omitted-proj", "sync-omitted-1")
	if err := st.SetPolicy("omitted-proj", PolicyOmitted); err != nil {
		t.Fatalf("SetPolicy(omitted-proj): %v", err)
	}

	result, err := st.PurgeForResync(1)
	if err != nil {
		t.Fatalf("PurgeForResync: %v", err)
	}
	for _, p := range result.Projects {
		if p == "omitted-proj" {
			t.Fatalf("omitted-proj must never appear in PurgeForResync's Projects list; got %v", result.Projects)
		}
	}

	if n := countRows(t, st, "memories", "project", "omitted-proj"); n != 1 {
		t.Errorf("memories remaining for omitted-proj = %d; want 1 (untouched)", n)
	}
	pol, err := st.GetPolicy("omitted-proj")
	if err != nil {
		t.Fatalf("GetPolicy(omitted-proj): %v", err)
	}
	if pol != PolicyOmitted {
		t.Errorf("GetPolicy(omitted-proj) = %q; want %q", pol, PolicyOmitted)
	}
}

// TestPurgeForResync_NoSyncedProjects_StillRecordsEpoch verifies that even
// with zero synced projects (e.g. a purely local-only store), PurgeForResync
// still records the new honored epoch — the epoch bookkeeping is independent
// of whether there was any data to purge.
func TestPurgeForResync_NoSyncedProjects_StillRecordsEpoch(t *testing.T) {
	st := openTempStore(t)

	seedMemory(t, st, "only-local", "sync-only-local-1")
	if err := st.SetPolicy("only-local", PolicyLocalOnly); err != nil {
		t.Fatalf("SetPolicy(only-local): %v", err)
	}

	result, err := st.PurgeForResync(2)
	if err != nil {
		t.Fatalf("PurgeForResync: %v", err)
	}
	if len(result.Projects) != 0 {
		t.Errorf("Projects = %v; want empty (no synced projects)", result.Projects)
	}
	if result.NewHonoredEpoch != 2 {
		t.Errorf("NewHonoredEpoch = %d; want 2", result.NewHonoredEpoch)
	}

	honored, err := st.HonoredPurgeEpoch()
	if err != nil {
		t.Fatalf("HonoredPurgeEpoch: %v", err)
	}
	if honored != 2 {
		t.Errorf("HonoredPurgeEpoch = %d; want 2 (epoch recorded even with nothing to purge)", honored)
	}
}

// TestPurgeForResync_FailedTx_LeavesEpochUnrecorded is the crash-safety proof:
// if the transaction fails partway through, NOTHING commits — including the
// epoch write. We force a failure by closing the underlying *sql.DB before
// calling PurgeForResync, which makes db.Begin() fail immediately (the
// simplest reliable way to force a PurgeForResync failure without reaching
// into unexported transaction internals). We open the store directly (not via
// openTempStore) so the test keeps its own handle on the file path and can
// reopen a fresh *Store against the SAME file after the crash to verify durable
// state without relying on the now-closed original handle.
func TestPurgeForResync_FailedTx_LeavesEpochUnrecorded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crash.db")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}

	seedMemory(t, st, "synced-crash", "sync-crash-1")
	if err := st.SetPolicy("synced-crash", PolicySynced); err != nil {
		t.Fatalf("SetPolicy(synced-crash): %v", err)
	}

	// Baseline: epoch is 0 before the attempted purge.
	before, err := st.HonoredPurgeEpoch()
	if err != nil {
		t.Fatalf("HonoredPurgeEpoch before: %v", err)
	}
	if before != 0 {
		t.Fatalf("precondition: expected epoch 0 before crash test, got %d", before)
	}

	// Force every subsequent DB operation on st to fail by closing the connection.
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	_, err = st.PurgeForResync(99)
	if err == nil {
		t.Fatal("PurgeForResync on a closed store: expected an error, got nil")
	}

	// Reopen a FRESH *Store against the SAME file to verify durable state on
	// disk — independent of the now-closed original handle.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen %q: %v", path, err)
	}
	t.Cleanup(func() { _ = st2.Close() })

	honored, err := st2.HonoredPurgeEpoch()
	if err != nil {
		t.Fatalf("HonoredPurgeEpoch after failed purge (reopened): %v", err)
	}
	if honored != 0 {
		t.Errorf("HonoredPurgeEpoch after a FAILED purge = %d; want 0 (unrecorded — no partial commit)", honored)
	}

	// The synced-crash project's memory must ALSO have survived (no partial delete).
	if n := countRows(t, st2, "memories", "project", "synced-crash"); n != 1 {
		t.Errorf("memories remaining for synced-crash (reopened) = %d; want 1 (no partial commit)", n)
	}
}

// TestPurgeForResync_PromptOnlyProject_NoPolicyRow_NoLiveMemories proves the
// enumeration bug fix: a project that is synced-by-default (central
// configured, no explicit project_policy row) and has NO live memories row —
// only a user_prompts row and a memory_tombstones row — must still be
// discovered and fully purged. Before the fix, PurgeForResync enumerated
// candidates via ListProjectsWithPolicy(), whose name discovery sources ONLY
// from live (deleted_at IS NULL) memories ∪ project_policy — neither of which
// this project has — so it was silently skipped, and its prompts,
// tombstones, outbox rows, and pull cursor survived the purge. The fix
// enumerates via ListProjects() (memories ∪ memory_tombstones ∪ user_prompts
// ∪ prompt_tombstones), the SAME union the autosync pull loop uses, then
// filters with GetPolicy — so this project is now a candidate.
func TestPurgeForResync_PromptOnlyProject_NoPolicyRow_NoLiveMemories(t *testing.T) {
	st := openTempStore(t)
	st.SetCentralConfiguredFn(func() bool { return true }) // central configured → default policy is synced

	const proj = "prompt-only-proj"

	// No SetPolicy call: this project has NO project_policy row. Its effective
	// policy is the read-time default (synced, since central is configured).

	// A prompt — the ONLY presence in user_prompts, no memories row at all.
	if _, err := st.DB().Exec(
		`INSERT INTO user_prompts (sync_id, session_id, content, project, writer_id) VALUES (?, 'sess', 'prompt content', ?, 'w')`,
		"sync-promptonly-prompt-1", proj,
	); err != nil {
		t.Fatalf("seed user_prompts: %v", err)
	}

	// A memory tombstone — a pulled delete for a sync_id that never had (or no
	// longer has) a live memories row.
	if err := st.ApplyPulled(domain.Mutation{
		Op:         domain.OpDelete,
		SyncID:     "sync-promptonly-tomb-1",
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Project:    proj,
		Scope:      "project",
		Version:    1,
		WriterID:   "other-writer",
		UpdatedAt:  time.Now().UTC(),
		MutationID: "mut-promptonly-tomb-1",
		Seq:        1,
	}); err != nil {
		t.Fatalf("ApplyPulled delete (%s): %v", proj, err)
	}

	// Advance the pull cursor so we can assert it gets reset.
	if err := st.SetPullCursorFor(proj, 17); err != nil {
		t.Fatalf("SetPullCursorFor(%s): %v", proj, err)
	}

	// Preconditions.
	if countRows(t, st, "memories", "project", proj) != 0 {
		t.Fatal("precondition: expected 0 memories (no live memories row)")
	}
	if countRows(t, st, "user_prompts", "project", proj) != 1 {
		t.Fatal("precondition: expected 1 user_prompts row")
	}
	if countRows(t, st, "memory_tombstones", "project", proj) != 1 {
		t.Fatal("precondition: expected 1 memory_tombstones row")
	}
	var appliedBefore int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM applied_mutations`).Scan(&appliedBefore); err != nil {
		t.Fatalf("count applied_mutations before: %v", err)
	}
	if appliedBefore == 0 {
		t.Fatal("precondition: expected at least 1 applied_mutations row")
	}
	cursorBefore, err := st.PullCursorFor(proj)
	if err != nil {
		t.Fatalf("PullCursorFor(%s) before: %v", proj, err)
	}
	if cursorBefore != 17 {
		t.Fatalf("precondition: expected pull cursor 17, got %d", cursorBefore)
	}

	// ── Run the purge ────────────────────────────────────────────────────────
	result, err := st.PurgeForResync(1)
	if err != nil {
		t.Fatalf("PurgeForResync: %v", err)
	}

	found := false
	for _, p := range result.Projects {
		if p == proj {
			found = true
		}
	}
	if !found {
		t.Fatalf("result.Projects = %v; want it to include %q (prompt/tombstone-only synced-by-default project must be discovered)", result.Projects, proj)
	}

	// ── Post-conditions: fully purged ───────────────────────────────────────
	if n := countRows(t, st, "user_prompts", "project", proj); n != 0 {
		t.Errorf("user_prompts remaining for %s = %d; want 0", proj, n)
	}
	if n := countRows(t, st, "memory_tombstones", "project", proj); n != 0 {
		t.Errorf("memory_tombstones remaining for %s = %d; want 0", proj, n)
	}
	var appliedAfter int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM applied_mutations`).Scan(&appliedAfter); err != nil {
		t.Fatalf("count applied_mutations after: %v", err)
	}
	if appliedAfter != 0 {
		t.Errorf("applied_mutations remaining = %d; want 0", appliedAfter)
	}
	cursorAfter, err := st.PullCursorFor(proj)
	if err != nil {
		t.Fatalf("PullCursorFor(%s) after: %v", proj, err)
	}
	if cursorAfter != 0 {
		t.Errorf("pull cursor for %s = %d; want 0 (reset)", proj, cursorAfter)
	}
}

// TestPurgeForResync_MixedCaseProject_NocaseEndToEnd locks the NOCASE contract
// end-to-end: a project whose project_policy row is seeded with MiXeD-CaSe,
// while its data rows (memories, prompts, tombstones) use different casing
// variants, must still be fully discovered and purged. This exercises
// GetPolicy's normalization (lowercases for lookup) together with the NOCASE
// deletes in purgeProjectRowsTx and the NOCASE pull-cursor reset.
func TestPurgeForResync_MixedCaseProject_NocaseEndToEnd(t *testing.T) {
	st := openTempStore(t)

	const policyName = "MiXeD-CaSe-Proj"
	const dataName = "mixed-case-proj"  // lowercase variant used by data rows
	const dataName2 = "MIXED-CASE-PROJ" // uppercase variant used by another data row

	if err := st.SetPolicy(policyName, PolicySynced); err != nil {
		t.Fatalf("SetPolicy(%s): %v", policyName, err)
	}

	// Live memory under the lowercase variant.
	if _, err := st.LocalWrite(domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     "sync-mixedcase-1",
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "mixed case memory",
		Content:    "content",
		Project:    dataName,
		Scope:      "project",
		Version:    1,
		WriterID:   "w",
		UpdatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("LocalWrite(%s): %v", dataName, err)
	}

	// Prompt under the uppercase variant.
	if _, err := st.DB().Exec(
		`INSERT INTO user_prompts (sync_id, session_id, content, project, writer_id) VALUES (?, 'sess', 'prompt content', ?, 'w')`,
		"sync-mixedcase-prompt-1", dataName2,
	); err != nil {
		t.Fatalf("seed user_prompts: %v", err)
	}

	// Tombstone under the original policy-row casing.
	if err := st.ApplyPulled(domain.Mutation{
		Op:         domain.OpDelete,
		SyncID:     "sync-mixedcase-tomb-1",
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Project:    policyName,
		Scope:      "project",
		Version:    1,
		WriterID:   "other-writer",
		UpdatedAt:  time.Now().UTC(),
		MutationID: "mut-mixedcase-tomb-1",
		Seq:        1,
	}); err != nil {
		t.Fatalf("ApplyPulled delete (%s): %v", policyName, err)
	}

	// Advance the pull cursor under the lowercase variant (SetPullCursorFor
	// does not normalize the project key it stores, mirroring how central-
	// pulled cursors key off the incoming payload's original-case project).
	if err := st.SetPullCursorFor(dataName, 5); err != nil {
		t.Fatalf("SetPullCursorFor(%s): %v", dataName, err)
	}

	// ── Run the purge ────────────────────────────────────────────────────────
	// ListProjects() does a plain (case-sensitive) SELECT DISTINCT, so the
	// three differently-cased names surface as three distinct candidate rows.
	// GetPolicy normalizes (lowercases) each one for its lookup and resolves
	// all three to the SAME 'synced' policy row, so all three are purge
	// candidates — proving the NOCASE contract holds end-to-end regardless of
	// which casing variant a table happened to store.
	result, err := st.PurgeForResync(1)
	if err != nil {
		t.Fatalf("PurgeForResync: %v", err)
	}
	if len(result.Projects) != 3 {
		t.Fatalf("result.Projects = %v; want exactly 3 casing variants, all resolved to 'synced'", result.Projects)
	}

	// ── Post-conditions: fully purged regardless of casing ──────────────────
	if n := countRows(t, st, "memories", "project", dataName); n != 0 {
		t.Errorf("memories remaining for %s = %d; want 0", dataName, n)
	}
	if n := countRows(t, st, "user_prompts", "project", dataName2); n != 0 {
		t.Errorf("user_prompts remaining for %s = %d; want 0", dataName2, n)
	}
	if n := countRows(t, st, "memory_tombstones", "project", policyName); n != 0 {
		t.Errorf("memory_tombstones remaining for %s = %d; want 0", policyName, n)
	}
	cursorAfter, err := st.PullCursorFor(dataName)
	if err != nil {
		t.Fatalf("PullCursorFor(%s) after: %v", dataName, err)
	}
	if cursorAfter != 0 {
		t.Errorf("pull cursor for %s = %d; want 0 (reset)", dataName, cursorAfter)
	}

	// The policy row itself must survive (untouched), resolvable regardless of
	// which casing variant is queried.
	pol, err := st.GetPolicy(dataName)
	if err != nil {
		t.Fatalf("GetPolicy(%s) after purge: %v", dataName, err)
	}
	if pol != PolicySynced {
		t.Errorf("GetPolicy(%s) after purge = %q; want %q", dataName, pol, PolicySynced)
	}
}
