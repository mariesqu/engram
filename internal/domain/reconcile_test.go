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
	decisionB := Decide(r, mutB)
	if decisionB.Action != ActionInsert {
		t.Fatalf("INV1: first upsert (B) must Insert; got %v", decisionB.Action)
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
	decisionA := Decide(r, mutA)
	if decisionA.Action != ActionUpdate {
		t.Fatalf("INV1: newer upsert (A) must Update; got %v", decisionA.Action)
	}
	// TargetSyncID must be the RESOLVED row's sync_id (syncB, not syncA).
	// This is the P1-a convergence invariant: the adapter must update syncB's row.
	if decisionA.TargetSyncID != syncB {
		t.Fatalf("INV1: TargetSyncID must be resolved row %q; got %q", syncB, decisionA.TargetSyncID)
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
	decision := Decide(r, mutB)
	if decision.Action != NoOp {
		t.Fatalf("INV1: older upsert (B) must be NoOp; got %v", decision.Action)
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
	decision := Decide(r, older)
	if decision.Action != NoOp {
		t.Fatalf("INV3: older write must be NoOp; got %v", decision.Action)
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
	decision := Decide(r, newer)
	if decision.Action != ActionUpdate {
		t.Fatalf("INV3: newer write must Update; got %v", decision.Action)
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
	if first.Action != ActionInsert {
		t.Fatalf("INV5: first apply must Insert; got %v", first.Action)
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
	if second.Action != NoOp {
		t.Fatalf("INV5: re-apply of same mutation_id must be NoOp; got %v", second.Action)
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
	decision := Decide(r, mut2)
	// Empty store → should Insert (not NoOp because mut-second is not in applied set).
	if decision.Action != ActionInsert {
		t.Fatalf("INV5: different mutation_id must be processed; got %v", decision.Action)
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

	decisionA := Decide(r, mutA)
	if decisionA.Action != ActionInsert {
		t.Fatalf("INV6: independent write A must Insert; got %v", decisionA.Action)
	}

	decisionB := Decide(r, mutB)
	if decisionB.Action != ActionInsert {
		t.Fatalf("INV6: independent write B must Insert; got %v", decisionB.Action)
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
		d := Decide(r, m)
		if d.Action != ActionInsert {
			t.Fatalf("INV6: distinct topic_key write must Insert; got %v for %s", d.Action, m.SyncID)
		}
	}
}

// ─────────────────────────────────────────────
// writeWins tiebreaker priority order
// ─────────────────────────────────────────────

// TestWriteWins_Tiebreakers exercises all four priority levels of writeWins:
//  1. Wall-clock (updated_at) is the primary comparator.
//  2. Version breaks ties when timestamps are equal.
//  3. WriterID breaks ties when timestamp AND version are equal.
//  4. last_write_mutation_id (the WINNING mutation's content-addressed id) breaks
//     ties when timestamp, version AND writer_id are equal — replacing the old
//     canonical-PK sync_id tier (which was divergent across replicas).
//
// The final tier compares the incoming mutation's MutationID against the stored
// row/tombstone's LastWriteMutationID (the winner's id, NOT the row PK sync_id).
func TestWriteWins_Tiebreakers(t *testing.T) {
	cases := []struct {
		name                   string
		mUpdatedAt             time.Time
		mVersion               int
		mWriterID              string
		mMutationID            string
		curUpdatedAt           time.Time
		curVersion             int
		curWriterID            string
		curLastWriteMutationID string
		wantWriteWins          bool
	}{
		{
			name:                   "newer timestamp wins regardless of version/identity",
			mUpdatedAt:             t100,
			mVersion:               1,
			mWriterID:              "writer-A",
			mMutationID:            "mut-A",
			curUpdatedAt:           t50,
			curVersion:             9,
			curWriterID:            "writer-Z",
			curLastWriteMutationID: "mut-Z",
			wantWriteWins:          true,
		},
		{
			name:                   "older timestamp loses regardless of version/identity",
			mUpdatedAt:             t50,
			mVersion:               9,
			mWriterID:              "writer-Z",
			mMutationID:            "mut-Z",
			curUpdatedAt:           t100,
			curVersion:             1,
			curWriterID:            "writer-A",
			curLastWriteMutationID: "mut-A",
			wantWriteWins:          false,
		},
		{
			name:                   "equal timestamp: higher version wins",
			mUpdatedAt:             t100,
			mVersion:               3,
			mWriterID:              "writer-A",
			mMutationID:            "mut-A",
			curUpdatedAt:           t100,
			curVersion:             2,
			curWriterID:            "writer-Z",
			curLastWriteMutationID: "mut-Z",
			wantWriteWins:          true,
		},
		{
			name:                   "equal timestamp: lower version loses",
			mUpdatedAt:             t100,
			mVersion:               1,
			mWriterID:              "writer-Z",
			mMutationID:            "mut-Z",
			curUpdatedAt:           t100,
			curVersion:             3,
			curWriterID:            "writer-A",
			curLastWriteMutationID: "mut-A",
			wantWriteWins:          false,
		},
		{
			name:                   "equal timestamp+version: higher writer_id wins",
			mUpdatedAt:             t100,
			mVersion:               2,
			mWriterID:              "writer-Z",
			mMutationID:            "mut-A", // lower mutation_id — irrelevant; writer_id decides first
			curUpdatedAt:           t100,
			curVersion:             2,
			curWriterID:            "writer-A",
			curLastWriteMutationID: "mut-Z",
			wantWriteWins:          true,
		},
		{
			name:                   "equal timestamp+version: lower writer_id loses",
			mUpdatedAt:             t100,
			mVersion:               2,
			mWriterID:              "writer-A",
			mMutationID:            "mut-Z", // higher mutation_id — irrelevant; writer_id decides first
			curUpdatedAt:           t100,
			curVersion:             2,
			curWriterID:            "writer-Z",
			curLastWriteMutationID: "mut-A",
			wantWriteWins:          false,
		},
		{
			name:                   "equal timestamp+version+writer_id: higher mutation_id wins",
			mUpdatedAt:             t100,
			mVersion:               2,
			mWriterID:              "writer-X",
			mMutationID:            "mut-Z",
			curUpdatedAt:           t100,
			curVersion:             2,
			curWriterID:            "writer-X",
			curLastWriteMutationID: "mut-A",
			wantWriteWins:          true,
		},
		{
			name:                   "equal timestamp+version+writer_id: lower mutation_id loses",
			mUpdatedAt:             t100,
			mVersion:               2,
			mWriterID:              "writer-X",
			mMutationID:            "mut-A",
			curUpdatedAt:           t100,
			curVersion:             2,
			curWriterID:            "writer-X",
			curLastWriteMutationID: "mut-Z",
			wantWriteWins:          false,
		},
		{
			name:                   "fully equal: mutation does not win (deterministic NoOp)",
			mUpdatedAt:             t100,
			mVersion:               2,
			mWriterID:              "writer-X",
			mMutationID:            "mut-X",
			curUpdatedAt:           t100,
			curVersion:             2,
			curWriterID:            "writer-X",
			curLastWriteMutationID: "mut-X",
			wantWriteWins:          false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			mut := Mutation{
				UpdatedAt:  tc.mUpdatedAt,
				Version:    tc.mVersion,
				WriterID:   tc.mWriterID,
				MutationID: tc.mMutationID,
			}
			got := writeWins(mut, tc.curUpdatedAt, tc.curVersion, tc.curWriterID, tc.curLastWriteMutationID)
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
	decision := Decide(r, mut)
	if decision.Action != ActionWriteTombstone {
		t.Fatalf("Delete op must return ActionWriteTombstone; got %v", decision.Action)
	}
}

// TestDecide_Delete_NoLiveRowNoTombstone_TombstonesUnconditionally verifies that
// when there is NEITHER a live row (cur == nil) NOR an existing tombstone (ts == nil),
// OpDelete tombstones unconditionally — the first delete of an identity has no prior
// state to compete against. NOTE: when cur == nil but a tombstone EXISTS, OpDelete
// instead competes against it via writeWins and may NoOp (see
// TestDecide_Delete_CurNilWithTombstone_LosingDelete_IsNoOp).
func TestDecide_Delete_NoLiveRowNoTombstone_TombstonesUnconditionally(t *testing.T) {
	syncID := "sync-del-nil"
	project, scope := "engram", "project"
	r := newMockReader()

	// Delete with a LOW updated_at — no live row AND no tombstone, so neither gate
	// arm fires; the delete tombstones unconditionally (pure first-delete path).
	mut := Mutation{
		MutationID: "mut-del-nil",
		Op:         OpDelete,
		SyncID:     syncID,
		Project:    project,
		Scope:      scope,
		Version:    1,
		Seq:        1,
		UpdatedAt:  t0, // oldest possible — but there is nothing to lose against
		WriterID:   "writer-A",
	}
	d := Decide(r, mut)
	if d.Action != ActionWriteTombstone {
		t.Fatalf("OpDelete with no live row and no tombstone must tombstone unconditionally; got %v", d.Action)
	}
}

// TestDecide_Delete_CurNilWithTombstone_LosingDelete_IsNoOp pins the gate added for
// the tombstone-metadata-regression split-brain: when cur == nil but a tombstone
// EXISTS, an incoming delete that LOSES writeWins against the tombstone MUST be a
// NoOp — it must NOT proceed to ActionWriteTombstone, because the adapter's
// INSERT OR REPLACE would regress the newer tombstone's (deleted_at, version,
// deleted_by) and let a later upsert that the newer tombstone should block resurrect
// the topic. (The winning direction — a newer delete re-tombstoning the canonical
// identity — is covered by TestDecide_CrossWriterReDelete_ReusesTombstoneIdentity.)
func TestDecide_Delete_CurNilWithTombstone_LosingDelete_IsNoOp(t *testing.T) {
	project, scope := "engram", "project"

	// Stale delete: older updated_at than the existing tombstone → loses → NoOp.
	t.Run("stale_delete_older_than_tombstone", func(t *testing.T) {
		syncID := "sync-del-vs-tomb-stale"
		r := newMockReader()
		// Existing tombstone is NEWER (t100). No record seeded → cur == nil; ts != nil.
		r.seedTombstone(&Tombstone{
			SyncID:    syncID,
			Project:   project,
			Scope:     scope,
			DeletedAt: t100,
			DeletedBy: "writer-Z",
			Version:   2,
		})
		mut := Mutation{
			MutationID: "mut-del-stale-vs-tomb",
			Op:         OpDelete,
			SyncID:     syncID,
			Project:    project,
			Scope:      scope,
			Version:    1,
			UpdatedAt:  t50, // older than the tombstone → loses writeWins
			WriterID:   "writer-A",
		}
		d := Decide(r, mut)
		if d.Action != NoOp {
			t.Fatalf("cur==nil, stale delete vs newer tombstone must be NoOp (no metadata regression); got %v", d.Action)
		}
	})

	// Tie-losing delete: exact (updated_at, version) tie, lower writer_id → NoOp.
	t.Run("tie_losing_delete_lower_writer_id", func(t *testing.T) {
		syncID := "sync-del-vs-tomb-tie"
		r := newMockReader()
		r.seedTombstone(&Tombstone{
			SyncID:    syncID,
			Project:   project,
			Scope:     scope,
			DeletedAt: t100,
			DeletedBy: "writer-Z", // higher writer_id → tombstone wins the tie
			Version:   2,
		})
		mut := Mutation{
			MutationID: "mut-del-tie-vs-tomb",
			Op:         OpDelete,
			SyncID:     syncID,
			Project:    project,
			Scope:      scope,
			Version:    2,
			UpdatedAt:  t100,       // exact (updated_at, version) tie
			WriterID:   "writer-A", // lower writer_id → loses the identity tiebreaker
		}
		d := Decide(r, mut)
		if d.Action != NoOp {
			t.Fatalf("cur==nil, tie-losing delete (lower writer_id) vs tombstone must be NoOp; got %v", d.Action)
		}
	})
}

// TestDecide_Delete_StaleDelete_IsNoOp verifies that a delete with a strictly
// OLDER updated_at than the live row is a NoOp (the uniform LWW gate).
// This closes the stale-delete split-brain: a delete that loses the total order
// must NOT tombstone a newer live row.
func TestDecide_Delete_StaleDelete_IsNoOp(t *testing.T) {
	syncID := "sync-del-stale"
	project, scope := "engram", "project"
	r := newMockReader()

	// Seed a live row at T+100 v=2.
	r.seedRecord(&Record{
		SyncID:     syncID,
		Project:    project,
		Scope:      scope,
		Version:    2,
		Seq:        5,
		UpdatedAt:  t100,
		Content:    "newer live content",
		EntityType: EntityMemory,
		Type:       "manual",
		Title:      "test",
		WriterID:   "writer-Z",
	})

	// Delete at T+50 v=1 — strictly older in every dimension.
	mut := Mutation{
		MutationID: "mut-del-stale",
		Op:         OpDelete,
		SyncID:     syncID,
		Project:    project,
		Scope:      scope,
		Version:    1,
		Seq:        2,
		UpdatedAt:  t50, // older than the live row
		WriterID:   "writer-A",
	}
	d := Decide(r, mut)
	if d.Action != NoOp {
		t.Fatalf("stale delete must be NoOp against a newer live row; got %v", d.Action)
	}
}

// TestDecide_Delete_NewerDelete_Tombstones verifies that a delete with a strictly
// NEWER updated_at than the live row returns ActionWriteTombstone (wins LWW gate).
func TestDecide_Delete_NewerDelete_Tombstones(t *testing.T) {
	syncID := "sync-del-newer"
	project, scope := "engram", "project"
	r := newMockReader()

	// Seed a live row at T+50 v=1.
	r.seedRecord(&Record{
		SyncID:     syncID,
		Project:    project,
		Scope:      scope,
		Version:    1,
		Seq:        2,
		UpdatedAt:  t50,
		Content:    "older live content",
		EntityType: EntityMemory,
		Type:       "manual",
		Title:      "test",
		WriterID:   "writer-A",
	})

	// Delete at T+100 v=2 — strictly newer.
	mut := Mutation{
		MutationID: "mut-del-newer",
		Op:         OpDelete,
		SyncID:     syncID,
		Project:    project,
		Scope:      scope,
		Version:    2,
		Seq:        5,
		UpdatedAt:  t100, // newer than the live row
		WriterID:   "writer-B",
	}
	d := Decide(r, mut)
	if d.Action != ActionWriteTombstone {
		t.Fatalf("newer delete must tombstone a live row; got %v", d.Action)
	}
}

// TestDecide_Delete_TieLoss_IsNoOp verifies that at the exact (updated_at,
// version) tie where the incoming delete's writer_id is LOWER than the live
// row's writer_id, the delete is a NoOp (it loses the identity tiebreaker).
// This is the "upserter-higher, delete-loses" case that caused the split-brain.
func TestDecide_Delete_TieLoss_IsNoOp(t *testing.T) {
	syncID := "sync-del-tie-loss"
	project, scope := "engram", "project"
	r := newMockReader()

	// Live row at T+100 v=2, written by "writer-Z" (higher writer_id).
	r.seedRecord(&Record{
		SyncID:     syncID,
		Project:    project,
		Scope:      scope,
		Version:    2,
		Seq:        3,
		UpdatedAt:  t100,
		Content:    "live content writer-Z",
		EntityType: EntityMemory,
		Type:       "manual",
		Title:      "test",
		WriterID:   "writer-Z",
	})

	// Delete at the SAME (updated_at, version) but LOWER writer_id — loses the
	// identity tiebreaker.
	mut := Mutation{
		MutationID: "mut-del-tie-loss",
		Op:         OpDelete,
		SyncID:     syncID,
		Project:    project,
		Scope:      scope,
		Version:    2,
		Seq:        4,
		UpdatedAt:  t100,      // same timestamp
		WriterID:   "writer-A", // lower than "writer-Z" → loses
	}
	d := Decide(r, mut)
	if d.Action != NoOp {
		t.Fatalf("tie-losing delete (writer-A < writer-Z) must be NoOp; got %v", d.Action)
	}
}

// TestDecide_Delete_TieWin_Tombstones verifies that at the exact (updated_at,
// version) tie where the incoming delete's writer_id is HIGHER than the live
// row's writer_id, the delete wins and returns ActionWriteTombstone.
func TestDecide_Delete_TieWin_Tombstones(t *testing.T) {
	syncID := "sync-del-tie-win"
	project, scope := "engram", "project"
	r := newMockReader()

	// Live row at T+100 v=2, written by "writer-A" (lower writer_id).
	r.seedRecord(&Record{
		SyncID:     syncID,
		Project:    project,
		Scope:      scope,
		Version:    2,
		Seq:        3,
		UpdatedAt:  t100,
		Content:    "live content writer-A",
		EntityType: EntityMemory,
		Type:       "manual",
		Title:      "test",
		WriterID:   "writer-A",
	})

	// Delete at the SAME (updated_at, version) but HIGHER writer_id — wins.
	mut := Mutation{
		MutationID: "mut-del-tie-win",
		Op:         OpDelete,
		SyncID:     syncID,
		Project:    project,
		Scope:      scope,
		Version:    2,
		Seq:        4,
		UpdatedAt:  t100,      // same timestamp
		WriterID:   "writer-Z", // higher than "writer-A" → wins
	}
	d := Decide(r, mut)
	if d.Action != ActionWriteTombstone {
		t.Fatalf("tie-winning delete (writer-Z > writer-A) must tombstone; got %v", d.Action)
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
	decision := Decide(r, mut)
	if decision.Action != NoOp {
		t.Fatalf("INV4-readiness: stale upsert after tombstone must be NoOp; got %v", decision.Action)
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
	decision := Decide(r, mut)
	// Pinned: must be ActionInsert (record was deleted; no live row exists via FindBySyncID
	// since FindBySyncID returns ANY row including soft-deleted, but there is no live cur).
	if decision.Action != ActionInsert {
		t.Fatalf("INV4: write strictly newer than tombstone must return ActionInsert; got %v", decision.Action)
	}
	// Undelete must be true so the adapter clears the tombstone.
	if !decision.Undelete {
		t.Errorf("INV4: write superseding tombstone must have Undelete=true; got false")
	}
}

// ─────────────────────────────────────────────
// Cross-writer convergence (state-space hardening)
//
// When a topic's canonical row is soft-deleted under sync_id Y and a new mutation
// arrives under a DIFFERENT sync_id X, Decide() must recover the canonical
// identity Y from the tombstone instead of minting X. These pure-decision tests
// model the real store: the soft-deleted row is reachable only via FindBySyncID
// (live-only FindByTopic skips it), and a topic-keyed tombstone exists under Y.
// ─────────────────────────────────────────────

// TestDecide_CrossWriterReDelete_ReusesTombstoneIdentity pins Codex's confirmed
// bug at the pure-decision layer: a second cross-writer delete (sync_id W) of a
// topic whose canonical row was already soft-deleted under Y must RE-tombstone Y,
// not mint a new tombstone under W. cur==nil (Y invisible to live FindByTopic,
// W never stored) but a topic tombstone under Y exists.
func TestDecide_CrossWriterReDelete_ReusesTombstoneIdentity(t *testing.T) {
	project, scope := "engram", "project"
	tk := strPtr("sdd/test/cross-redelete")
	syncY, syncW := "sync-Y", "sync-W"

	r := newMockReader()
	// Canonical row already soft-deleted under Y: reachable via sync_id only.
	r.seedSyncOnlyRecord(&Record{
		SyncID: syncY, TopicKey: tk, Project: project, Scope: scope,
		Version: 2, Seq: 2, UpdatedAt: t50, DeletedAt: &t50,
		Content: "Y content", EntityType: EntityMemory, Type: "manual", Title: "test",
		WriterID: "writer-Y",
	})
	// Tombstone for the topic identity, keyed under Y.
	r.seedTombstone(&Tombstone{
		SyncID: syncY, TopicKey: tk, Project: project, Scope: scope,
		DeletedAt: t50, DeletedBy: "writer-Y", Version: 2,
	})

	// Second delete arrives under a DIFFERENT writer's sync_id W.
	mutDelW := Mutation{
		MutationID: "mut-del-w", Op: OpDelete, SyncID: syncW,
		Project: project, Scope: scope, TopicKey: tk,
		Version: 3, Seq: 3, UpdatedAt: t100, WriterID: "writer-W",
	}
	d := Decide(r, mutDelW)
	if d.Action != ActionWriteTombstone {
		t.Fatalf("re-delete: want ActionWriteTombstone; got %v", d.Action)
	}
	if d.TargetSyncID != syncY {
		t.Errorf("re-delete: TargetSyncID = %q; want %q (reuse existing tombstone identity, not mint W)",
			d.TargetSyncID, syncY)
	}
}

// TestDecide_CrossWriterUpsertAfterDelete_RevivesCanonical pins the sibling at
// the pure-decision layer: a superseding upsert under sync_id X of a topic whose
// canonical row was soft-deleted under Y must REVIVE Y in place (ActionUpdate,
// TargetSyncID=Y, Undelete=true), NOT insert a new row X that orphans the dead
// row Y. cur==nil; a soft-deleted row for Y exists; the tombstone is superseded.
func TestDecide_CrossWriterUpsertAfterDelete_RevivesCanonical(t *testing.T) {
	project, scope := "engram", "project"
	tk := strPtr("sdd/test/cross-upsert-after-delete")
	syncY, syncX := "sync-Y", "sync-X"

	r := newMockReader()
	// Canonical row soft-deleted under Y, reachable via sync_id only.
	r.seedSyncOnlyRecord(&Record{
		SyncID: syncY, TopicKey: tk, Project: project, Scope: scope,
		Version: 2, Seq: 2, UpdatedAt: t50, DeletedAt: &t50,
		Content: "Y content", EntityType: EntityMemory, Type: "manual", Title: "test",
		WriterID: "writer-Y",
	})
	r.seedTombstone(&Tombstone{
		SyncID: syncY, TopicKey: tk, Project: project, Scope: scope,
		DeletedAt: t50, DeletedBy: "writer-Y", Version: 2,
	})

	// Newer upsert under a DIFFERENT writer's sync_id X — supersedes the tombstone.
	mutX := newUpsert(syncX, "mut-upsert-x", tk, project, scope, t100, 3, 5, "X content")
	d := Decide(r, mutX)
	if d.Action != ActionUpdate {
		t.Fatalf("upsert-after-delete: want ActionUpdate (revive canonical); got %v", d.Action)
	}
	if d.TargetSyncID != syncY {
		t.Errorf("upsert-after-delete: TargetSyncID = %q; want %q (revive canonical, not mint X)",
			d.TargetSyncID, syncY)
	}
	if !d.Undelete {
		t.Errorf("upsert-after-delete: want Undelete=true; got false")
	}
}

// TestDecide_PureTombstoneUpsert_InsertsOwnIdentity pins the pure-tombstone
// branch: when cur==nil and NO row exists for the tombstone's sync_id (the
// tombstone covers an identity that was never materialized), a superseding upsert
// must ActionInsert its OWN sync_id with Undelete=true (clear the stale tombstone).
func TestDecide_PureTombstoneUpsert_InsertsOwnIdentity(t *testing.T) {
	project, scope := "engram", "project"
	tk := strPtr("sdd/test/pure-tombstone")
	syncU := "sync-U"

	r := newMockReader()
	// Tombstone exists for the topic identity under U, but NO memories row.
	r.seedTombstone(&Tombstone{
		SyncID: syncU, TopicKey: tk, Project: project, Scope: scope,
		DeletedAt: t50, DeletedBy: "writer-U", Version: 1,
	})

	mutU := newUpsert(syncU, "mut-upsert-u", tk, project, scope, t100, 2, 5, "U content")
	d := Decide(r, mutU)
	if d.Action != ActionInsert {
		t.Fatalf("pure-tombstone upsert: want ActionInsert; got %v", d.Action)
	}
	if d.TargetSyncID != syncU {
		t.Errorf("pure-tombstone upsert: TargetSyncID = %q; want %q", d.TargetSyncID, syncU)
	}
	if !d.Undelete {
		t.Errorf("pure-tombstone upsert: want Undelete=true; got false")
	}
}

// TestDecide_TombstoneTieBreak_IdentityAuthority pins all three cases of the
// tombstone identity tie-break boundary where updated_at and version are EQUAL
// and (writer_id, last_write_mutation_id) are the sole deciding factor.
//
// These are pure-domain unit tests (no Postgres): they directly exercise
// writeWins through Decide with an exact (updated_at, version) tie.
//
// Both the tombstone-vs-upsert and the upsert-vs-row paths are covered.
//
// Scenarios:
//   - different writer_id: incoming.writer_id > tombstone.deleted_by → supersede
//   - equal writer_id, higher mutation_id: incoming.mutation_id >
//     tombstone.last_write_mutation_id → supersede
//   - full equality → blocked (NoOp)
func TestDecide_TombstoneTieBreak_IdentityAuthority(t *testing.T) {
	project, scope := "engram", "project"
	tieAt := t100
	tieVersion := 2

	// ── Direction 1: incoming.writer_id > tombstone.deleted_by → supersede ──
	//
	// writeWins(mut @ tieAt v=2 writerID="writer-Z" mutationID="mut-t1-higher-writer",
	//           curUpdatedAt=tieAt, curVersion=2, curWriterID="writer-A", curLastWriteMutationID="")
	// → timestamps equal → versions equal → "writer-Z" > "writer-A" → true → supersede
	//   (writer_id decides; the mutation_id tier is never reached)
	t.Run("tombstone_higher_writer_id_supersedes", func(t *testing.T) {
		syncID := "sync-tiebreak-t1"
		r := newMockReader()
		r.seedTombstone(&Tombstone{
			SyncID:    syncID,
			Project:   project,
			Scope:     scope,
			DeletedAt: tieAt,
			DeletedBy: "writer-A", // lower writer_id
			Version:   tieVersion,
		})

		// Incoming upsert: same sync_id, same (updated_at, version), but higher writer_id.
		mut := Mutation{
			MutationID: "mut-t1-higher-writer",
			Op:         OpUpsert,
			SyncID:     syncID,
			EntityType: EntityMemory,
			Type:       "manual",
			Title:      "test",
			Content:    "revived content",
			Project:    project,
			Scope:      scope,
			Version:    tieVersion,
			UpdatedAt:  tieAt,
			WriterID:   "writer-Z", // higher writer_id → supersedes
		}
		d := Decide(r, mut)
		if d.Action != ActionInsert {
			t.Fatalf("tombstone tie: higher writer_id must supersede → ActionInsert; got %v", d.Action)
		}
		if !d.Undelete {
			t.Errorf("tombstone tie: supersede must have Undelete=true; got false")
		}
	})

	// ── Direction 2: equal writer_id, incoming.mutation_id > tombstone.last_write_mutation_id → supersede ──
	//
	// writeWins(mut @ tieAt v=2 writerID="writer-X" mutationID="mut-zzz",
	//           curUpdatedAt=tieAt, curVersion=2, curWriterID="writer-X",
	//           curLastWriteMutationID="mut-aaa")
	// → timestamps equal → versions equal → writer_id equal → "mut-zzz" > "mut-aaa" → true
	//
	// Note: since the incoming sync_id ("sync-Z") differs from the tombstone's sync_id
	// ("sync-A"), the tombstone must be found via topic_key — FindTombstone is called with
	// m.SyncID first (no match), then falls back to topic_key (match). The DECIDER, though,
	// is the mutation_id (NOT sync_id): the tombstone carries the winning delete's
	// last_write_mutation_id, and the incoming write's mutation_id is compared against it.
	t.Run("tombstone_equal_writer_higher_mutation_id_supersedes", func(t *testing.T) {
		tk := strPtr("sdd/test/tie-sync-id")
		syncIDTombstone := "sync-A" // tombstone identity (PK) — NOT the tiebreaker
		r := newMockReader()
		r.seedTombstone(&Tombstone{
			SyncID:              syncIDTombstone,
			TopicKey:            tk,
			Project:             project,
			Scope:               scope,
			DeletedAt:           tieAt,
			DeletedBy:           "writer-X",
			Version:             tieVersion,
			LastWriteMutationID: "mut-aaa", // lower → loses the final tier
		})

		// Incoming upsert: same writer_id but higher mutation_id — wins.
		// Uses the same topic_key so FindTombstone finds the tombstone via topic fallback.
		mut := Mutation{
			MutationID: "mut-zzz", // higher mutation_id → supersedes
			Op:         OpUpsert,
			SyncID:     "sync-Z", // PK differs (forces topic fallback) — NOT the decider
			EntityType: EntityMemory,
			Type:       "manual",
			Title:      "test",
			Content:    "revived content",
			Project:    project,
			Scope:      scope,
			TopicKey:   tk,
			Version:    tieVersion,
			UpdatedAt:  tieAt,
			WriterID:   "writer-X", // equal writer_id
		}
		d := Decide(r, mut)
		if d.Action != ActionInsert {
			t.Fatalf("tombstone tie: equal writer + higher mutation_id must supersede → ActionInsert; got %v", d.Action)
		}
		if !d.Undelete {
			t.Errorf("tombstone tie: supersede must have Undelete=true; got false")
		}
	})

	// ── Direction 3: incoming.writer_id < tombstone.deleted_by → blocked ──
	//
	// writeWins(mut @ tieAt v=2 writerID="writer-A" mutationID="mut-t3-lower-writer",
	//           curUpdatedAt=tieAt, curVersion=2, curWriterID="writer-Z", curLastWriteMutationID="")
	// → timestamps equal → versions equal → "writer-A" < "writer-Z" → false → NoOp
	//   (writer_id decides; the mutation_id tier is never reached)
	t.Run("tombstone_lower_writer_id_blocked", func(t *testing.T) {
		syncID := "sync-tiebreak-t3"
		r := newMockReader()
		r.seedTombstone(&Tombstone{
			SyncID:    syncID,
			Project:   project,
			Scope:     scope,
			DeletedAt: tieAt,
			DeletedBy: "writer-Z", // higher writer_id (tombstone wins)
			Version:   tieVersion,
		})

		mut := Mutation{
			MutationID: "mut-t3-lower-writer",
			Op:         OpUpsert,
			SyncID:     syncID,
			EntityType: EntityMemory,
			Type:       "manual",
			Title:      "test",
			Content:    "stale content",
			Project:    project,
			Scope:      scope,
			Version:    tieVersion,
			UpdatedAt:  tieAt,
			WriterID:   "writer-A", // lower writer_id → blocked
		}
		d := Decide(r, mut)
		if d.Action != NoOp {
			t.Fatalf("tombstone tie: lower writer_id must be blocked → NoOp; got %v", d.Action)
		}
	})
}

// TestDecide_UpsertVsRow_IdentityAuthority covers the upsert-vs-row path (cur != nil)
// at the exact (updated_at, version) tie, proving the same identity tiebreaker
// applies symmetrically:
//
//   - different writer_id: higher writer_id wins
//   - equal writer_id, higher mutation_id: higher last_write_mutation_id wins
//   - full equality (same writer_id AND same mutation_id): deterministic no-op
func TestDecide_UpsertVsRow_IdentityAuthority(t *testing.T) {
	project, scope := "engram", "project"
	tieAt := t100
	tieVersion := 2

	// ── Case A: incoming.writer_id > cur.writer_id → wins ──
	t.Run("upsert_row_higher_writer_id_wins", func(t *testing.T) {
		syncID := "sync-uvr-A"
		r := newMockReader()
		r.seedRecord(&Record{
			SyncID:     syncID,
			Project:    project,
			Scope:      scope,
			Version:    tieVersion,
			UpdatedAt:  tieAt,
			WriterID:   "writer-A", // lower writer_id (stored row)
			EntityType: EntityMemory,
			Type:       "manual",
			Title:      "test",
		})

		mut := Mutation{
			MutationID: "mut-uvr-A",
			Op:         OpUpsert,
			SyncID:     syncID,
			EntityType: EntityMemory,
			Type:       "manual",
			Title:      "test",
			Project:    project,
			Scope:      scope,
			Version:    tieVersion,
			UpdatedAt:  tieAt,
			WriterID:   "writer-Z", // higher writer_id → wins
		}
		d := Decide(r, mut)
		if d.Action != ActionUpdate {
			t.Fatalf("upsert-vs-row tie: higher writer_id must win → ActionUpdate; got %v", d.Action)
		}
	})

	// ── Case B: incoming.writer_id < cur.writer_id → blocked ──
	t.Run("upsert_row_lower_writer_id_blocked", func(t *testing.T) {
		syncID := "sync-uvr-B"
		r := newMockReader()
		r.seedRecord(&Record{
			SyncID:     syncID,
			Project:    project,
			Scope:      scope,
			Version:    tieVersion,
			UpdatedAt:  tieAt,
			WriterID:   "writer-Z", // higher writer_id (stored row wins)
			EntityType: EntityMemory,
			Type:       "manual",
			Title:      "test",
		})

		mut := Mutation{
			MutationID: "mut-uvr-B",
			Op:         OpUpsert,
			SyncID:     syncID,
			EntityType: EntityMemory,
			Type:       "manual",
			Title:      "test",
			Project:    project,
			Scope:      scope,
			Version:    tieVersion,
			UpdatedAt:  tieAt,
			WriterID:   "writer-A", // lower writer_id → blocked
		}
		d := Decide(r, mut)
		if d.Action != NoOp {
			t.Fatalf("upsert-vs-row tie: lower writer_id must be blocked → NoOp; got %v", d.Action)
		}
	})

	// ── Case C: equal writer_id, higher mutation_id → wins ──
	// The stored row is found via topic_key so that Decide resolves cur != nil
	// even though the incoming mutation has a different sync_id. This exercises the
	// writeWins(m, cur.UpdatedAt, cur.Version, cur.WriterID, cur.LastWriteMutationID)
	// call site where m.MutationID > cur.LastWriteMutationID with equal writer_id →
	// ActionUpdate. The PK sync_id differs only to force the FindByTopic resolution;
	// it is NOT the decider (that is the winning write's mutation_id).
	t.Run("upsert_row_equal_writer_higher_mutation_id_wins", func(t *testing.T) {
		tk := strPtr("sdd/test/uvr-sync-id")
		curSyncID := "sync-A" // stored row PK — NOT the tiebreaker
		r := newMockReader()
		r.seedRecord(&Record{
			SyncID:              curSyncID,
			TopicKey:            tk,
			Project:             project,
			Scope:               scope,
			Version:             tieVersion,
			UpdatedAt:           tieAt,
			WriterID:            "writer-X",
			LastWriteMutationID: "mut-aaa", // lower → loses the final tier
			EntityType:          EntityMemory,
			Type:                "manual",
			Title:               "test",
		})

		// Incoming mutation: same topic_key, same (updated_at, version, writer_id),
		// but higher mutation_id → wins on the final tiebreaker.
		mut := Mutation{
			MutationID: "mut-zzz", // higher mutation_id → wins
			Op:         OpUpsert,
			SyncID:     "sync-Z", // PK differs (forces topic resolution) — NOT the decider
			EntityType: EntityMemory,
			Type:       "manual",
			Title:      "test",
			Project:    project,
			Scope:      scope,
			TopicKey:   tk, // same topic → FindByTopic resolves cur = stored row
			Version:    tieVersion,
			UpdatedAt:  tieAt,
			WriterID:   "writer-X", // equal writer_id — falls through to mutation_id
		}
		d := Decide(r, mut)
		if d.Action != ActionUpdate {
			t.Fatalf("upsert-vs-row tie: equal writer + higher mutation_id must win → ActionUpdate; got %v", d.Action)
		}
		// TargetSyncID must be the RESOLVED canonical row (curSyncID), not the incoming sync_id.
		if d.TargetSyncID != curSyncID {
			t.Errorf("upsert-vs-row tie: TargetSyncID = %q; want %q (canonical row)", d.TargetSyncID, curSyncID)
		}
	})

	// ── Case D: full equality → no-op ──
	// Same writer_id AND same mutation_id (the stored row's last_write_mutation_id
	// equals the incoming mutation_id — i.e. an idempotent re-materialization of the
	// SAME winning write) → the final tier returns false → deterministic NoOp.
	t.Run("upsert_row_full_equality_noop", func(t *testing.T) {
		syncID := "sync-uvr-D"
		r := newMockReader()
		r.seedRecord(&Record{
			SyncID:              syncID,
			Project:             project,
			Scope:               scope,
			Version:             tieVersion,
			UpdatedAt:           tieAt,
			WriterID:            "writer-X",
			LastWriteMutationID: "mut-uvr-D", // equals incoming → final tier is false
			EntityType:          EntityMemory,
			Type:                "manual",
			Title:               "test",
		})

		mut := Mutation{
			MutationID: "mut-uvr-D", // same mutation_id as the stored winner
			Op:         OpUpsert,
			SyncID:     syncID, // same sync_id
			EntityType: EntityMemory,
			Type:       "manual",
			Title:      "test",
			Project:    project,
			Scope:      scope,
			Version:    tieVersion,
			UpdatedAt:  tieAt,
			WriterID:   "writer-X", // same writer_id
		}
		d := Decide(r, mut)
		if d.Action != NoOp {
			t.Fatalf("upsert-vs-row full equality must be NoOp; got %v", d.Action)
		}
	})
}
