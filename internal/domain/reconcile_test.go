package domain

import (
	"testing"
	"time"
)

// helpers

// strPtr is a convenience helper for *string literals in tests.
func strPtr(s string) *string { return &s }

// baseTime is a fixed reference point; +N seconds gives ordered timestamps.
var baseTime = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// t0, t50, t100 are shorthand wall-clock instants used across tests.
var (
	t0   = baseTime
	t50  = baseTime.Add(50 * time.Second)
	t100 = baseTime.Add(100 * time.Second)
	t150 = baseTime.Add(150 * time.Second)
	t200 = baseTime.Add(200 * time.Second)
)

// newUpsert builds a minimal Mutation with OpUpsert.
func newUpsert(syncID, mutID string, tk *string, project, scope string,
	updatedAt time.Time, version int, seq int64, content string) Mutation {
	return Mutation{
		MutationID: mutID,
		Op:         OpUpsert,
		SyncID:     syncID,
		EntityType: EntityMemory,
		Type:       "manual",
		Title:      "test",
		Content:    content,
		Project:    project,
		Scope:      scope,
		TopicKey:   tk,
		Version:    version,
		Seq:        seq,
		UpdatedAt:  updatedAt,
		WriterID:   "writer-A",
	}
}

// ─────────────────────────────────────────────
// INV1 — Identity convergence
// ─────────────────────────────────────────────

// TestDecide_INV1_TopicIdentityConvergence verifies that concurrent upserts
// to the SAME topic_key converge to a single row holding the newer write.
//
// Scenario:
//   - Writer A upserts topic="sdd/test/explore" at T+100, content="A's content"
//   - Writer B upserts same topic at T+50, content="B's content"
//   - Apply B first (newer A is not stored yet) → Insert (B is first arrival)
//   - Apply A next (A is newer than B) → Update
//   - Final state: one record, A's content, version=A's
func TestDecide_INV1_TopicIdentityConvergence(t *testing.T) {
	tk := strPtr("sdd/test/explore")
	project, scope := "engram", "project"
	syncA, syncB := "sync-A", "sync-B"

	mutA := newUpsert(syncA, "mut-A", tk, project, scope, t100, 2, 2, "A's content")
	mutB := newUpsert(syncB, "mut-B", tk, project, scope, t50, 1, 1, "B's content")

	r := newMockReader()

	// ── Apply B first (empty store → must Insert) ──
	actionB := Decide(r, mutB)
	if actionB != ActionInsert {
		t.Fatalf("INV1: first upsert (B) must Insert; got %v", actionB)
	}

	// Simulate the adapter having executed Insert(mutB) — seed B's record.
	storedB := &Record{
		SyncID:    syncB,
		TopicKey:  tk,
		Project:   project,
		Scope:     scope,
		Version:   mutB.Version,
		Seq:       mutB.Seq,
		UpdatedAt: mutB.UpdatedAt,
		Content:   mutB.Content,
		EntityType: EntityMemory,
		Type:      "manual",
		Title:     "test",
		WriterID:  "writer-B",
	}
	r.seedRecord(storedB)
	r.markApplied(mutB.MutationID)

	// ── Apply A (A is newer → must Update the existing record) ──
	actionA := Decide(r, mutA)
	if actionA != ActionUpdate {
		t.Fatalf("INV1: newer upsert (A) must Update; got %v", actionA)
	}

	// INV1 guarantee: only ONE record should exist for this topic_key after
	// a correct adapter executes Update (replaces B's record in place).
	// We assert Decide returns exactly Update — the single-row invariant is
	// maintained because Update overwrites the existing row (not a second Insert).
}

// TestDecide_INV1_OlderUpsertIsNoOp verifies the complementary direction:
// once A's record is stored, arriving B (older) must be discarded.
func TestDecide_INV1_OlderUpsertIsNoOp(t *testing.T) {
	tk := strPtr("sdd/test/explore")
	project, scope := "engram", "project"
	syncA := "sync-A"

	r := newMockReader()

	// Seed A's record (the newer, already stored).
	r.seedRecord(&Record{
		SyncID:    syncA,
		TopicKey:  tk,
		Project:   project,
		Scope:     scope,
		Version:   2,
		Seq:       2,
		UpdatedAt: t100,
		Content:   "A's content",
		EntityType: EntityMemory,
		Type:      "manual",
		Title:     "test",
		WriterID:  "writer-A",
	})
	r.markApplied("mut-A")

	// Apply B (older) — must be NoOp.
	mutB := newUpsert("sync-B", "mut-B", tk, project, scope, t50, 1, 1, "B's content")
	action := Decide(r, mutB)
	if action != NoOp {
		t.Fatalf("INV1: older upsert (B) must be NoOp; got %v", action)
	}
}

// ─────────────────────────────────────────────
// INV3 — No lost updates (version-guarded LWW)
// ─────────────────────────────────────────────

// TestDecide_INV3_NoLostUpdate verifies that a stale write (older timestamp +
// lower version) does NOT overwrite a newer stored record.
func TestDecide_INV3_NoLostUpdate(t *testing.T) {
	syncID := "sync-X"
	project, scope := "engram", "project"

	r := newMockReader()
	// Seed the newer record (v=2, T+100).
	r.seedRecord(&Record{
		SyncID:    syncID,
		Project:   project,
		Scope:     scope,
		Version:   2,
		Seq:       5,
		UpdatedAt: t100,
		Content:   "newer content",
		EntityType: EntityMemory,
		Type:      "manual",
		Title:     "test",
		WriterID:  "writer-A",
	})

	// Incoming mutation: v=1, T+50 — older in every dimension.
	older := newUpsert(syncID, "mut-old", nil, project, scope, t50, 1, 3, "older content")
	action := Decide(r, older)
	if action != NoOp {
		t.Fatalf("INV3: older write must be NoOp; got %v", action)
	}
}

// TestDecide_INV3_NewerWriteWins verifies the counterpart: a newer incoming
// write DOES overwrite the stored record.
func TestDecide_INV3_NewerWriteWins(t *testing.T) {
	syncID := "sync-X"
	project, scope := "engram", "project"

	r := newMockReader()
	// Seed an older record (v=1, T+50).
	r.seedRecord(&Record{
		SyncID:    syncID,
		Project:   project,
		Scope:     scope,
		Version:   1,
		Seq:       1,
		UpdatedAt: t50,
		Content:   "old content",
		EntityType: EntityMemory,
		Type:      "manual",
		Title:     "test",
		WriterID:  "writer-A",
	})

	// Incoming mutation: v=2, T+100 — newer.
	newer := newUpsert(syncID, "mut-new", nil, project, scope, t100, 2, 2, "new content")
	action := Decide(r, newer)
	if action != ActionUpdate {
		t.Fatalf("INV3: newer write must Update; got %v", action)
	}
}

// ─────────────────────────────────────────────
// INV5 — Idempotent re-apply
// ─────────────────────────────────────────────

// TestDecide_INV5_IdempotentReApply verifies that applying the same mutation
// twice returns NoOp on the second call, leaving state unchanged.
func TestDecide_INV5_IdempotentReApply(t *testing.T) {
	syncID := "sync-idem"
	mutID := "mut-idem"
	project, scope := "engram", "project"

	r := newMockReader()

	mut := newUpsert(syncID, mutID, nil, project, scope, t100, 1, 1, "content")

	// First apply — store is empty, must Insert.
	first := Decide(r, mut)
	if first != ActionInsert {
		t.Fatalf("INV5: first apply must Insert; got %v", first)
	}

	// Simulate adapter executed the Insert and recorded the mutation.
	r.seedRecord(&Record{
		SyncID:    syncID,
		Project:   project,
		Scope:     scope,
		Version:   1,
		Seq:       1,
		UpdatedAt: t100,
		Content:   "content",
		EntityType: EntityMemory,
		Type:      "manual",
		Title:     "test",
		WriterID:  "writer-A",
	})
	r.markApplied(mutID) // <-- this is what blocks re-apply

	// Second apply — same mutation_id → must be NoOp regardless of state.
	second := Decide(r, mut)
	if second != NoOp {
		t.Fatalf("INV5: re-apply of same mutation_id must be NoOp; got %v", second)
	}
}

// TestDecide_INV5_DifferentMutationIDNotIdempotent ensures that a different
// mutation_id for the same record IS processed (not mistakenly skipped).
func TestDecide_INV5_DifferentMutationIDNotIdempotent(t *testing.T) {
	syncID := "sync-idem2"
	project, scope := "engram", "project"

	r := newMockReader()
	r.markApplied("mut-first") // mark first as applied

	// Second mutation has a distinct ID — must not be blocked.
	mut2 := newUpsert(syncID, "mut-second", nil, project, scope, t100, 1, 1, "content")
	action := Decide(r, mut2)
	// Empty store → should Insert (not NoOp because mut-second is not in applied set).
	if action != ActionInsert {
		t.Fatalf("INV5: different mutation_id must be processed; got %v", action)
	}
}

// ─────────────────────────────────────────────
// INV6 — Independent new writes preserved
// ─────────────────────────────────────────────

// TestDecide_INV6_IndependentWritesPreserved verifies that concurrent upserts
// to DIFFERENT records (distinct sync_ids, no shared topic_key) both survive.
func TestDecide_INV6_IndependentWritesPreserved(t *testing.T) {
	project, scope := "engram", "project"

	r := newMockReader()

	mutA := newUpsert("sync-indep-A", "mut-indep-A", nil, project, scope, t100, 1, 1, "A content")
	mutB := newUpsert("sync-indep-B", "mut-indep-B", nil, project, scope, t100, 1, 2, "B content")

	actionA := Decide(r, mutA)
	if actionA != ActionInsert {
		t.Fatalf("INV6: independent write A must Insert; got %v", actionA)
	}

	actionB := Decide(r, mutB)
	if actionB != ActionInsert {
		t.Fatalf("INV6: independent write B must Insert; got %v", actionB)
	}
}

// TestDecide_INV6_IndependentTopicKeysNeverConflict verifies that two upserts
// to DIFFERENT topic_keys in the same project are both inserted without conflict.
func TestDecide_INV6_IndependentTopicKeysNeverConflict(t *testing.T) {
	project, scope := "engram", "project"
	tkA := strPtr("sdd/test/a")
	tkB := strPtr("sdd/test/b")

	r := newMockReader()

	mutA := newUpsert("sync-tk-A", "mut-tk-A", tkA, project, scope, t100, 1, 1, "A")
	mutB := newUpsert("sync-tk-B", "mut-tk-B", tkB, project, scope, t100, 1, 2, "B")

	for _, m := range []Mutation{mutA, mutB} {
		act := Decide(r, m)
		if act != ActionInsert {
			t.Fatalf("INV6: distinct topic_key write must Insert; got %v for %s", act, m.SyncID)
		}
	}
}

// ─────────────────────────────────────────────
// writeWins tiebreaker priority order
// ─────────────────────────────────────────────

// TestWriteWins_Tiebreakers exercises all three priority levels of writeWins:
//  1. Wall-clock (updated_at) is the primary comparator.
//  2. Version breaks ties when timestamps are equal.
//  3. Seq breaks ties when timestamp AND version are equal.
func TestWriteWins_Tiebreakers(t *testing.T) {
	cases := []struct {
		name          string
		mUpdatedAt    time.Time
		mVersion      int
		mSeq          int64
		curUpdatedAt  time.Time
		curVersion    int
		curSeq        int64
		wantWriteWins bool
	}{
		{
			name:          "newer timestamp wins regardless of version/seq",
			mUpdatedAt:    t100,
			mVersion:      1,
			mSeq:          1,
			curUpdatedAt:  t50,
			curVersion:    9,
			curSeq:        99,
			wantWriteWins: true,
		},
		{
			name:          "older timestamp loses regardless of version/seq",
			mUpdatedAt:    t50,
			mVersion:      9,
			mSeq:          99,
			curUpdatedAt:  t100,
			curVersion:    1,
			curSeq:        1,
			wantWriteWins: false,
		},
		{
			name:          "equal timestamp: higher version wins",
			mUpdatedAt:    t100,
			mVersion:      3,
			mSeq:          1,
			curUpdatedAt:  t100,
			curVersion:    2,
			curSeq:        99,
			wantWriteWins: true,
		},
		{
			name:          "equal timestamp: lower version loses",
			mUpdatedAt:    t100,
			mVersion:      1,
			mSeq:          99,
			curUpdatedAt:  t100,
			curVersion:    3,
			curSeq:        1,
			wantWriteWins: false,
		},
		{
			name:          "equal timestamp+version: higher seq wins",
			mUpdatedAt:    t100,
			mVersion:      2,
			mSeq:          5,
			curUpdatedAt:  t100,
			curVersion:    2,
			curSeq:        3,
			wantWriteWins: true,
		},
		{
			name:          "equal timestamp+version: lower seq loses",
			mUpdatedAt:    t100,
			mVersion:      2,
			mSeq:          2,
			curUpdatedAt:  t100,
			curVersion:    2,
			curSeq:        5,
			wantWriteWins: false,
		},
		{
			name:          "fully equal: mutation does not win (deterministic NoOp)",
			mUpdatedAt:    t100,
			mVersion:      2,
			mSeq:          5,
			curUpdatedAt:  t100,
			curVersion:    2,
			curSeq:        5,
			wantWriteWins: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			mut := Mutation{
				UpdatedAt: tc.mUpdatedAt,
				Version:   tc.mVersion,
				Seq:       tc.mSeq,
			}
			got := writeWins(mut, tc.curUpdatedAt, tc.curVersion, tc.curSeq)
			if got != tc.wantWriteWins {
				t.Errorf("writeWins() = %v; want %v", got, tc.wantWriteWins)
			}
		})
	}
}

// TestDecide_DeleteWritesTombstone verifies that a Delete op returns
// ActionWriteTombstone (tombstone-respecting logic path exercised).
func TestDecide_DeleteWritesTombstone(t *testing.T) {
	syncID := "sync-del"
	project, scope := "engram", "project"
	r := newMockReader()

	mut := Mutation{
		MutationID: "mut-del",
		Op:         OpDelete,
		SyncID:     syncID,
		Project:    project,
		Scope:      scope,
		Version:    1,
		Seq:        1,
		UpdatedAt:  t100,
		WriterID:   "writer-A",
	}
	action := Decide(r, mut)
	if action != ActionWriteTombstone {
		t.Fatalf("Delete op must return ActionWriteTombstone; got %v", action)
	}
}

// TestDecide_TombstonedSyncIDBlocksStaleUpsert verifies INV4-readiness:
// a tombstone with deleted_at >= mutation.updated_at blocks the upsert (NoOp).
// The full INV4 proof is PR4 acceptance, but Decide() must already handle it.
func TestDecide_TombstonedSyncIDBlocksStaleUpsert(t *testing.T) {
	syncID := "sync-tombed"
	project, scope := "engram", "project"
	r := newMockReader()

	// Tombstone was written at T+200 (newer than the incoming upsert at T+150).
	r.seedTombstone(&Tombstone{
		SyncID:    syncID,
		Project:   project,
		Scope:     scope,
		DeletedAt: t200,
		DeletedBy: "writer-A",
		Version:   2,
	})

	// Incoming upsert at T+150 — older than the tombstone → must be blocked.
	mut := newUpsert(syncID, "mut-stale", nil, project, scope, t150, 1, 1, "content")
	action := Decide(r, mut)
	if action != NoOp {
		t.Fatalf("INV4-readiness: stale upsert after tombstone must be NoOp; got %v", action)
	}
}

// TestDecide_TombstoneNewerWriteMaySupersede verifies that a write strictly
// NEWER than the tombstone (higher updated_at) is NOT blocked and results in
// ActionInsert (the record was deleted, so there is no live row to update).
func TestDecide_TombstoneNewerWriteMaySupersede(t *testing.T) {
	syncID := "sync-supersede"
	project, scope := "engram", "project"
	r := newMockReader()

	// Tombstone at T+100, version=1.
	r.seedTombstone(&Tombstone{
		SyncID:    syncID,
		Project:   project,
		Scope:     scope,
		DeletedAt: t100,
		DeletedBy: "writer-A",
		Version:   1,
	})

	// Incoming upsert at T+150 — strictly newer timestamp → must supersede.
	mut := newUpsert(syncID, "mut-supersede", nil, project, scope, t150, 2, 5, "new content")
	action := Decide(r, mut)
	// Pinned: must be ActionInsert (record was deleted; no live row exists).
	if action != ActionInsert {
		t.Fatalf("INV4: write strictly newer than tombstone must return ActionInsert; got %v", action)
	}
}

// TestDecide_TombstoneTieBreak_SeqAuthority pins both directions of the
// tombstone tie-break boundary where updated_at and version are EQUAL and seq
// is the sole deciding factor (the final authoritative tiebreaker per INV4 spec).
//
// Current implementation: Decide() passes curSeq=0 when checking against a
// tombstone (reconcile.go line ~42). This is intentional for PR2: the local
// SQLite store does not assign Postgres BIGSERIAL seqs. The full wiring of
// ts.Seq will be done in PR4 when the Postgres central store is live and
// Tombstone.Seq is populated. These tests pin the boundary at the effective
// current value (0) and document the required semantics for PR4.
func TestDecide_TombstoneTieBreak_SeqAuthority(t *testing.T) {
	project, scope := "engram", "project"
	tombstoneTs := t100
	tombstoneVersion := 1
	// Effective tombstone seq seen by writeWins in this PR slice = 0
	// (Decide passes 0 as curSeq for tombstone checks until PR4 wires ts.Seq).
	const effectiveTombstoneSeq int64 = 0

	// ── Direction 1: incoming seq > effective tombstone seq (0) → superseded ──
	//
	// writeWins(mut @ T+100 v=1 seq=1, curUpdatedAt=T+100, curVersion=1, curSeq=0)
	// → timestamps equal → versions equal → 1 > 0 → true → ActionInsert
	t.Run("higher_seq_supersedes_tombstone", func(t *testing.T) {
		syncID := "sync-tiebreak-higher"
		r := newMockReader()
		r.seedTombstone(&Tombstone{
			SyncID:    syncID,
			Project:   project,
			Scope:     scope,
			DeletedAt: tombstoneTs,
			DeletedBy: "writer-A",
			Version:   tombstoneVersion,
		})

		// seq=1 > effectiveTombstoneSeq(0) → writeWins returns true → ActionInsert
		mut := newUpsert(syncID, "mut-tiebreak-higher", nil, project, scope,
			tombstoneTs, tombstoneVersion, effectiveTombstoneSeq+1, "revived content")
		action := Decide(r, mut)
		if action != ActionInsert {
			t.Fatalf("INV4 tie-break: seq(%d) > effective tombstone seq(%d) must supersede → ActionInsert; got %v",
				effectiveTombstoneSeq+1, effectiveTombstoneSeq, action)
		}
	})

	// ── Direction 2: incoming seq == effective tombstone seq (0) → blocked ──
	//
	// writeWins(mut @ T+100 v=1 seq=0, curUpdatedAt=T+100, curVersion=1, curSeq=0)
	// → timestamps equal → versions equal → 0 > 0 = false → NoOp
	t.Run("equal_seq_blocked_by_tombstone", func(t *testing.T) {
		syncID := "sync-tiebreak-equal"
		r := newMockReader()
		r.seedTombstone(&Tombstone{
			SyncID:    syncID,
			Project:   project,
			Scope:     scope,
			DeletedAt: tombstoneTs,
			DeletedBy: "writer-A",
			Version:   tombstoneVersion,
		})

		// seq=0 == effectiveTombstoneSeq(0) → writeWins returns false → NoOp
		mut := newUpsert(syncID, "mut-tiebreak-equal", nil, project, scope,
			tombstoneTs, tombstoneVersion, effectiveTombstoneSeq, "equal-seq content")
		action := Decide(r, mut)
		if action != NoOp {
			t.Fatalf("INV4 tie-break: seq(%d) == effective tombstone seq(%d) must be blocked → NoOp; got %v",
				effectiveTombstoneSeq, effectiveTombstoneSeq, action)
		}
	})
}
