//go:build acceptance

// Package centralstore_test — end-to-end acceptance coverage for the
// Decide-driven central push-apply (Store.Apply) against a REAL Postgres
// instance (embedded-postgres, started once per package in TestMain).
//
// These tests drive s.Apply(ctx, m) end-to-end and assert the central canonical
// state (central_memories / central_tombstones / central_mutations). Each test
// runs in its own isolated schema via newIsolatedStore (see store_acceptance_test.go).
//
// Invariant coverage matrix:
//   INV1  TestApply_INV1_TopicConvergence              — one live row per topic, newer wins, canonical sync_id
//   INV2  TestApply_INV2_MonotonicSeq                   — central_memories.seq + central_mutations.seq strictly increase
//   INV3  TestApply_INV3_NoLostUpdate                   — older upsert after newer is a no-op
//   INV4  TestApply_INV4_DeleteThenStaleThenReviveUpsert — delete, stale upsert stays deleted, newer upsert revives
//   INV5  TestApply_INV5_IdempotentSameMutationID       — same mutation_id twice → one row, one mutation, no error
//   RACE  TestApply_ConcurrentDuplicate_Idempotent      — N goroutines race the same mutation_id; all must return nil; exactly one row
//   INV6  TestApply_INV6_IndependentTopicsSurvive       — two different topics both survive
//   X-del TestApply_CrossWriterDeleteTombstonesCanonical — delete via topic resolves to another writer's sync_id
package centralstore_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mariesqu/engram/internal/domain"
)

// liveRow holds the subset of a central_memories row the assertions inspect.
type liveRow struct {
	syncID  string
	content string
	version int
	seq     int64
}

// queryLiveTopicRows returns every live (deleted_at IS NULL) central_memories
// row for the given topic identity, ordered by sync_id. Used to assert INV-A
// (≤1 live row per topic) and to read the surviving content/version/seq.
func queryLiveTopicRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tk, project, scope string) []liveRow {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT sync_id, content, version, seq
		FROM central_memories
		WHERE topic_key = $1 AND project = $2 AND scope = $3 AND deleted_at IS NULL
		ORDER BY sync_id`,
		tk, project, scope,
	)
	if err != nil {
		t.Fatalf("queryLiveTopicRows: %v", err)
	}
	defer rows.Close()

	var out []liveRow
	for rows.Next() {
		var r liveRow
		if err := rows.Scan(&r.syncID, &r.content, &r.version, &r.seq); err != nil {
			t.Fatalf("queryLiveTopicRows scan: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("queryLiveTopicRows rows.Err: %v", err)
	}
	return out
}

// ── INV1 — topic convergence ──────────────────────────────────────────────────

// TestApply_INV1_TopicConvergence: two writers Apply upserts for the SAME topic
// (different sync_ids, the second newer). After both applies there must be
// exactly ONE live central_memories row for the topic, holding the newer
// content, under the canonical sync_id (the first writer's row resolved by
// FindByTopic). No duplicate-key failure — Apply routes the update through
// Decision.TargetSyncID, not the incoming sync_id.
func TestApply_INV1_TopicConvergence(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	tk := "sdd/test/inv1-converge"
	base := time.Now().UTC()

	// Writer A — older write, establishes the canonical row under sync-A.
	mA := testMutationWithTopic("mut-inv1-a", "sync-inv1-A", "proj", "scp", tk, domain.OpUpsert)
	mA.WriterID = "writer-A"
	mA.Content = "content from A (older)"
	mA.Version = 1
	mA.UpdatedAt = base.Add(-1 * time.Minute)

	// Writer B — newer write, SAME topic, DIFFERENT sync_id. Must converge onto A.
	mB := testMutationWithTopic("mut-inv1-b", "sync-inv1-B", "proj", "scp", tk, domain.OpUpsert)
	mB.WriterID = "writer-B"
	mB.Content = "content from B (newer winner)"
	mB.Version = 2
	mB.UpdatedAt = base

	if err := store.Apply(ctx, mA); err != nil {
		t.Fatalf("Apply A: %v", err)
	}
	if err := store.Apply(ctx, mB); err != nil {
		t.Fatalf("Apply B (must converge, not duplicate-key): %v", err)
	}

	live := queryLiveTopicRows(t, ctx, store.Pool(), tk, "proj", "scp")
	if len(live) != 1 {
		t.Fatalf("INV1: expected exactly 1 live row for topic, got %d: %+v", len(live), live)
	}
	if live[0].syncID != "sync-inv1-A" {
		t.Errorf("INV1: canonical row sync_id=%q, want %q (the row resolved via FindByTopic)", live[0].syncID, "sync-inv1-A")
	}
	if live[0].content != mB.Content {
		t.Errorf("INV1: content=%q, want %q (writer B's newer content)", live[0].content, mB.Content)
	}
}

// ── INV2 — monotonic seq ──────────────────────────────────────────────────────

// TestApply_INV2_MonotonicSeq: successive Apply calls must assign strictly
// increasing central_mutations.seq AND stamp central_memories.seq with that same
// increasing value. The BIGSERIAL is the authority; Apply threads the returned
// seq into the materialized row.
func TestApply_INV2_MonotonicSeq(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	m1 := testMutation("mut-inv2-1", "sync-inv2-1", "proj", domain.OpUpsert)
	m2 := testMutation("mut-inv2-2", "sync-inv2-2", "proj", domain.OpUpsert)
	m3 := testMutation("mut-inv2-3", "sync-inv2-3", "proj", domain.OpUpsert)

	for _, m := range []domain.Mutation{m1, m2, m3} {
		if err := store.Apply(ctx, m); err != nil {
			t.Fatalf("Apply %s: %v", m.MutationID, err)
		}
	}

	// central_mutations.seq must be strictly increasing in insertion order.
	seqByMut := map[string]int64{}
	rows, err := store.Pool().Query(ctx,
		`SELECT mutation_id, seq FROM central_mutations ORDER BY seq`)
	if err != nil {
		t.Fatalf("query central_mutations: %v", err)
	}
	var ordered []int64
	for rows.Next() {
		var id string
		var seq int64
		if err := rows.Scan(&id, &seq); err != nil {
			rows.Close()
			t.Fatalf("scan central_mutations: %v", err)
		}
		seqByMut[id] = seq
		ordered = append(ordered, seq)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("central_mutations rows.Err: %v", err)
	}

	if len(ordered) != 3 {
		t.Fatalf("INV2: expected 3 mutation rows, got %d", len(ordered))
	}
	for i := 1; i < len(ordered); i++ {
		if ordered[i] <= ordered[i-1] {
			t.Errorf("INV2: central_mutations.seq not strictly increasing: %v", ordered)
		}
	}
	if !(seqByMut["mut-inv2-1"] < seqByMut["mut-inv2-2"] && seqByMut["mut-inv2-2"] < seqByMut["mut-inv2-3"]) {
		t.Errorf("INV2: mutation seqs not in apply order: %+v", seqByMut)
	}

	// central_memories.seq must carry the same monotonic seq the mutation got.
	for _, sid := range []string{"sync-inv2-1", "sync-inv2-2", "sync-inv2-3"} {
		var memSeq int64
		if err := store.Pool().QueryRow(ctx,
			`SELECT seq FROM central_memories WHERE sync_id = $1`, sid,
		).Scan(&memSeq); err != nil {
			t.Fatalf("query central_memories.seq for %s: %v", sid, err)
		}
		if memSeq <= 0 {
			t.Errorf("INV2: central_memories.seq for %s = %d, want > 0", sid, memSeq)
		}
	}

	var mem1, mem2, mem3 int64
	store.Pool().QueryRow(ctx, `SELECT seq FROM central_memories WHERE sync_id='sync-inv2-1'`).Scan(&mem1) //nolint:errcheck
	store.Pool().QueryRow(ctx, `SELECT seq FROM central_memories WHERE sync_id='sync-inv2-2'`).Scan(&mem2) //nolint:errcheck
	store.Pool().QueryRow(ctx, `SELECT seq FROM central_memories WHERE sync_id='sync-inv2-3'`).Scan(&mem3) //nolint:errcheck
	if !(mem1 < mem2 && mem2 < mem3) {
		t.Errorf("INV2: central_memories.seq not strictly increasing across rows: %d,%d,%d", mem1, mem2, mem3)
	}
}

// ── INV3 — no lost update ─────────────────────────────────────────────────────

// TestApply_INV3_NoLostUpdate: after a newer upsert lands, an OLDER upsert
// (lower updated_at / version) for the same row must be a no-op — the stored
// state stays unchanged (content + version of the newer write).
func TestApply_INV3_NoLostUpdate(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	base := time.Now().UTC()

	// Newer write lands first.
	newer := testMutation("mut-inv3-new", "sync-inv3", "proj", domain.OpUpsert)
	newer.Content = "newer content (must survive)"
	newer.Version = 5
	newer.UpdatedAt = base

	if err := store.Apply(ctx, newer); err != nil {
		t.Fatalf("Apply newer: %v", err)
	}

	// Older write for the SAME sync_id — must be discarded by writeWins (INV3).
	older := testMutation("mut-inv3-old", "sync-inv3", "proj", domain.OpUpsert)
	older.Content = "older content (must be dropped)"
	older.Version = 2
	older.UpdatedAt = base.Add(-1 * time.Hour)

	if err := store.Apply(ctx, older); err != nil {
		t.Fatalf("Apply older (must be a clean no-op, not an error): %v", err)
	}

	var content string
	var version int
	if err := store.Pool().QueryRow(ctx,
		`SELECT content, version FROM central_memories WHERE sync_id = $1`, "sync-inv3",
	).Scan(&content, &version); err != nil {
		t.Fatalf("read back row: %v", err)
	}
	if content != newer.Content {
		t.Errorf("INV3: content=%q, want %q (older write must not overwrite)", content, newer.Content)
	}
	if version != newer.Version {
		t.Errorf("INV3: version=%d, want %d (older write must not lower version)", version, newer.Version)
	}

	// Both mutation rows are still journalled (the older one was applied as a
	// NoOp decision but still claimed a seq).
	var mutCount int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM central_mutations WHERE entity_key = $1`, "sync-inv3",
	).Scan(&mutCount); err != nil {
		t.Fatalf("count mutations: %v", err)
	}
	if mutCount != 2 {
		t.Errorf("INV3: expected 2 journalled mutations, got %d", mutCount)
	}
}

// ── INV4 — push-level no resurrection / controlled revive ─────────────────────

// TestApply_INV4_DeleteThenStaleThenReviveUpsert: Apply a delete (writes
// tombstone + sets deleted_at), then a STALE upsert (older than the delete) →
// the row STAYS deleted (tombstone guard blocks it). Then a strictly-NEWER
// upsert → the row REVIVES: deleted_at cleared, tombstone gone, FindByTopic
// returns it again.
func TestApply_INV4_DeleteThenStaleThenReviveUpsert(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	tk := "sdd/test/inv4-revive"
	base := time.Now().UTC()

	// 1. Seed a live row.
	seed := testMutationWithTopic("mut-inv4-seed", "sync-inv4", "proj", "scp", tk, domain.OpUpsert)
	seed.Content = "seed content"
	seed.Version = 1
	seed.UpdatedAt = base.Add(-2 * time.Hour)
	if err := store.Apply(ctx, seed); err != nil {
		t.Fatalf("Apply seed: %v", err)
	}

	// 2. Delete at T = base-1h.
	del := testMutationWithTopic("mut-inv4-del", "sync-inv4", "proj", "scp", tk, domain.OpDelete)
	del.Version = 2
	del.UpdatedAt = base.Add(-1 * time.Hour)
	if err := store.Apply(ctx, del); err != nil {
		t.Fatalf("Apply delete: %v", err)
	}

	// After delete: no live row, tombstone present, memory row soft-deleted.
	if got := queryLiveTopicRows(t, ctx, store.Pool(), tk, "proj", "scp"); len(got) != 0 {
		t.Fatalf("INV4: after delete expected 0 live rows, got %d: %+v", len(got), got)
	}
	if ts, err := store.FindTombstone("sync-inv4", &tk, "proj", "scp"); err != nil || ts == nil {
		t.Fatalf("INV4: after delete expected tombstone, err=%v ts=%v", err, ts)
	}

	// 3. STALE upsert (older than the delete at base-90m) → must stay deleted.
	stale := testMutationWithTopic("mut-inv4-stale", "sync-inv4", "proj", "scp", tk, domain.OpUpsert)
	stale.Content = "stale content (must NOT revive)"
	stale.Version = 1
	stale.UpdatedAt = base.Add(-90 * time.Minute)
	if err := store.Apply(ctx, stale); err != nil {
		t.Fatalf("Apply stale upsert (must be a clean no-op): %v", err)
	}
	if got := queryLiveTopicRows(t, ctx, store.Pool(), tk, "proj", "scp"); len(got) != 0 {
		t.Fatalf("INV4: stale upsert resurrected the row (got %d live rows: %+v)", len(got), got)
	}
	if ts, err := store.FindTombstone("sync-inv4", &tk, "proj", "scp"); err != nil || ts == nil {
		t.Fatalf("INV4: stale upsert must leave the tombstone intact, err=%v ts=%v", err, ts)
	}

	// 4. NEWER upsert (after the delete, at base) → revives.
	revive := testMutationWithTopic("mut-inv4-revive", "sync-inv4", "proj", "scp", tk, domain.OpUpsert)
	revive.Content = "revived content (winner)"
	revive.Version = 3
	revive.UpdatedAt = base
	if err := store.Apply(ctx, revive); err != nil {
		t.Fatalf("Apply revive upsert: %v", err)
	}

	live := queryLiveTopicRows(t, ctx, store.Pool(), tk, "proj", "scp")
	if len(live) != 1 {
		t.Fatalf("INV4: after revive expected exactly 1 live row, got %d: %+v", len(live), live)
	}
	if live[0].content != revive.Content {
		t.Errorf("INV4: revived content=%q, want %q", live[0].content, revive.Content)
	}

	// Tombstone must be gone.
	if ts, err := store.FindTombstone("sync-inv4", &tk, "proj", "scp"); err != nil {
		t.Fatalf("INV4: FindTombstone after revive: %v", err)
	} else if ts != nil {
		t.Errorf("INV4: tombstone must be removed after revive, got %+v", ts)
	}

	// deleted_at must be cleared on the memory row.
	var deletedAt *time.Time
	if err := store.Pool().QueryRow(ctx,
		`SELECT deleted_at FROM central_memories WHERE sync_id = $1`, "sync-inv4",
	).Scan(&deletedAt); err != nil {
		t.Fatalf("read deleted_at after revive: %v", err)
	}
	if deletedAt != nil {
		t.Errorf("INV4: deleted_at must be NULL after revive, got %v", deletedAt)
	}

	// FindByTopic must return the revived record.
	if got, err := store.FindByTopic(tk, "proj", "scp"); err != nil || got == nil {
		t.Fatalf("INV4: FindByTopic must return revived record, err=%v got=%v", err, got)
	}
}

// ── INV5 — idempotent re-apply ────────────────────────────────────────────────

// TestApply_INV5_IdempotentSameMutationID: applying the SAME mutation_id twice
// must be a no-op the second time — exactly one memory row, exactly one mutation
// row, and no error. This exercises BOTH idempotency layers:
//   - the step-1 MutationApplied pool check (the second Apply sees it applied),
//   - and, as defense in depth, the central_mutations.mutation_id UNIQUE.
func TestApply_INV5_IdempotentSameMutationID(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	m := testMutation("mut-inv5-dup", "sync-inv5", "proj", domain.OpUpsert)
	m.Content = "idempotent content"

	if err := store.Apply(ctx, m); err != nil {
		t.Fatalf("Apply first: %v", err)
	}
	if err := store.Apply(ctx, m); err != nil {
		t.Fatalf("Apply second (idempotent, must not error): %v", err)
	}

	var memCount int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM central_memories WHERE sync_id = $1`, "sync-inv5",
	).Scan(&memCount); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if memCount != 1 {
		t.Errorf("INV5: expected exactly 1 memory row, got %d", memCount)
	}

	var mutCount int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM central_mutations WHERE mutation_id = $1`, "mut-inv5-dup",
	).Scan(&mutCount); err != nil {
		t.Fatalf("count mutations: %v", err)
	}
	if mutCount != 1 {
		t.Errorf("INV5: expected exactly 1 mutation row for the duplicate id, got %d", mutCount)
	}
}

// TestApply_ConcurrentDuplicate_Idempotent genuinely races the 23505 branch in
// Apply by launching N goroutines that ALL call store.Apply with the SAME
// mutation_id simultaneously, released together via a start barrier.
//
// Because all goroutines start before any commits, several of them will pass the
// step-1 MutationApplied check (pool read sees nothing yet) and race to INSERT
// into central_mutations. Exactly ONE wins; the rest hit the UNIQUE(mutation_id)
// constraint (SQLSTATE 23505) inside insertMutationQ. Apply must treat that
// 23505 as an idempotent no-op and return nil — NOT surface an error.
//
// Assertions:
//   - every goroutine returns a nil error
//   - exactly ONE central_mutations row for the mutation_id
//   - exactly ONE central_memories row for the sync_id (no double-apply)
//
// This test exercises the isUniqueViolation handler (apply.go:88-92) under
// real concurrent load. Combined with the unit test in apply_internal_test.go
// (no build tag), the handler is verified both deterministically (predicate
// shape) and under real concurrency (end-to-end correctness).
func TestApply_ConcurrentDuplicate_Idempotent(t *testing.T) {
	const goroutines = 24

	store := newIsolatedStore(t)
	ctx := context.Background()

	tk := "sdd/test/concurrent-dup"
	m := testMutationWithTopic("mut-concurrent-dup", "sync-concurrent-dup", "proj", "scp", tk, domain.OpUpsert)
	m.Content = "concurrent duplicate content"

	// Start barrier: all goroutines block until closed, maximising the chance
	// that multiple goroutines pass the step-1 MutationApplied check before any
	// of them commits, which forces the 23505 path.
	start := make(chan struct{})

	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start // wait for the barrier to open
			errs[i] = store.Apply(ctx, m)
		}()
	}

	// Release all goroutines at once.
	close(start)
	wg.Wait()

	// Every goroutine must return nil — 23505 losers are idempotent no-ops.
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: Apply returned error: %v", i, err)
		}
	}

	// Exactly one mutation row must exist.
	var mutCount int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM central_mutations WHERE mutation_id = $1`, m.MutationID,
	).Scan(&mutCount); err != nil {
		t.Fatalf("count mutations: %v", err)
	}
	if mutCount != 1 {
		t.Errorf("expected exactly 1 central_mutations row, got %d", mutCount)
	}

	// Exactly one live memory row must exist for the sync_id.
	var memCount int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM central_memories WHERE sync_id = $1 AND deleted_at IS NULL`, m.SyncID,
	).Scan(&memCount); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if memCount != 1 {
		t.Errorf("expected exactly 1 central_memories row, got %d", memCount)
	}

	// Verify FindByTopic returns the single canonical row.
	got, err := store.FindByTopic(tk, "proj", "scp")
	if err != nil {
		t.Fatalf("FindByTopic: %v", err)
	}
	if got == nil {
		t.Fatal("FindByTopic returned nil — concurrent duplicate must not have erased the record")
	}
	if got.Content != m.Content {
		t.Errorf("FindByTopic content mismatch: got %q want %q", got.Content, m.Content)
	}
}

// ── INV6 — independent writes survive ─────────────────────────────────────────

// TestApply_INV6_IndependentTopicsSurvive: Apply upserts for two DIFFERENT
// topics → both rows survive (distinct topic identities never conflict).
func TestApply_INV6_IndependentTopicsSurvive(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	tk1 := "sdd/test/inv6-topic-1"
	tk2 := "sdd/test/inv6-topic-2"

	m1 := testMutationWithTopic("mut-inv6-1", "sync-inv6-1", "proj", "scp", tk1, domain.OpUpsert)
	m1.Content = "topic 1 content"
	m2 := testMutationWithTopic("mut-inv6-2", "sync-inv6-2", "proj", "scp", tk2, domain.OpUpsert)
	m2.Content = "topic 2 content"

	if err := store.Apply(ctx, m1); err != nil {
		t.Fatalf("Apply topic 1: %v", err)
	}
	if err := store.Apply(ctx, m2); err != nil {
		t.Fatalf("Apply topic 2: %v", err)
	}

	g1, err := store.FindByTopic(tk1, "proj", "scp")
	if err != nil || g1 == nil {
		t.Fatalf("INV6: topic 1 must survive, err=%v got=%v", err, g1)
	}
	if g1.SyncID != "sync-inv6-1" || g1.Content != m1.Content {
		t.Errorf("INV6: topic 1 row wrong: sync_id=%q content=%q", g1.SyncID, g1.Content)
	}

	g2, err := store.FindByTopic(tk2, "proj", "scp")
	if err != nil || g2 == nil {
		t.Fatalf("INV6: topic 2 must survive, err=%v got=%v", err, g2)
	}
	if g2.SyncID != "sync-inv6-2" || g2.Content != m2.Content {
		t.Errorf("INV6: topic 2 row wrong: sync_id=%q content=%q", g2.SyncID, g2.Content)
	}

	// And exactly two live rows total for these two topics.
	var liveCount int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM central_memories WHERE topic_key IN ($1,$2) AND deleted_at IS NULL`,
		tk1, tk2,
	).Scan(&liveCount); err != nil {
		t.Fatalf("count live rows: %v", err)
	}
	if liveCount != 2 {
		t.Errorf("INV6: expected 2 live rows for the two topics, got %d", liveCount)
	}
}

// ── Cross-writer delete ───────────────────────────────────────────────────────

// TestApply_CrossWriterDeleteTombstonesCanonical: Writer A establishes the
// canonical row Y for a topic. Writer B then Applies a DELETE for the SAME topic
// under a DIFFERENT sync_id X. Decide must resolve the canonical identity Y via
// FindByTopic, so the tombstone is written under Y (not X). Result: exactly one
// tombstone for the topic, the canonical row Y is soft-deleted, and there is no
// central_tombstones_topic_uidx violation.
func TestApply_CrossWriterDeleteTombstonesCanonical(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	tk := "sdd/test/xwriter-delete"
	base := time.Now().UTC()

	// Writer A — establishes canonical row Y.
	mA := testMutationWithTopic("mut-xdel-a", "sync-xdel-Y", "proj", "scp", tk, domain.OpUpsert)
	mA.WriterID = "writer-A"
	mA.Content = "A's canonical content"
	mA.Version = 1
	mA.UpdatedAt = base.Add(-1 * time.Minute)
	if err := store.Apply(ctx, mA); err != nil {
		t.Fatalf("Apply A upsert: %v", err)
	}

	// Writer B — DELETE for the same topic under a different sync_id X.
	mB := testMutationWithTopic("mut-xdel-b", "sync-xdel-X", "proj", "scp", tk, domain.OpDelete)
	mB.WriterID = "writer-B"
	mB.Version = 2
	mB.UpdatedAt = base
	if err := store.Apply(ctx, mB); err != nil {
		t.Fatalf("Apply B cross-writer delete (must not violate topic uidx): %v", err)
	}

	// Exactly one tombstone for the topic, under the canonical sync_id Y.
	rows, err := store.Pool().Query(ctx,
		`SELECT sync_id FROM central_tombstones WHERE topic_key=$1 AND project=$2 AND scope=$3`,
		tk, "proj", "scp",
	)
	if err != nil {
		t.Fatalf("query tombstones: %v", err)
	}
	var tsSyncIDs []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			t.Fatalf("scan tombstone: %v", err)
		}
		tsSyncIDs = append(tsSyncIDs, sid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("tombstone rows.Err: %v", err)
	}
	if len(tsSyncIDs) != 1 {
		t.Fatalf("cross-writer delete: expected exactly 1 tombstone for topic, got %d: %v", len(tsSyncIDs), tsSyncIDs)
	}
	if tsSyncIDs[0] != "sync-xdel-Y" {
		t.Errorf("cross-writer delete: tombstone sync_id=%q, want %q (canonical identity)", tsSyncIDs[0], "sync-xdel-Y")
	}

	// The canonical row Y must be soft-deleted (deleted_at set) and no live row remains.
	if got := queryLiveTopicRows(t, ctx, store.Pool(), tk, "proj", "scp"); len(got) != 0 {
		t.Fatalf("cross-writer delete: expected 0 live rows, got %d: %+v", len(got), got)
	}
	var deletedAt *time.Time
	if err := store.Pool().QueryRow(ctx,
		`SELECT deleted_at FROM central_memories WHERE sync_id = $1`, "sync-xdel-Y",
	).Scan(&deletedAt); err != nil {
		t.Fatalf("read deleted_at for canonical row: %v", err)
	}
	if deletedAt == nil {
		t.Error("cross-writer delete: canonical row Y must be soft-deleted (deleted_at set), got NULL")
	}

	// No row should have been created under the incoming sync_id X.
	var xCount int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM central_memories WHERE sync_id = $1`, "sync-xdel-X",
	).Scan(&xCount); err != nil {
		t.Fatalf("count X rows: %v", err)
	}
	if xCount != 0 {
		t.Errorf("cross-writer delete: no row should exist under incoming sync_id X, got %d", xCount)
	}
}
