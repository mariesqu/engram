//go:build acceptance

package spike_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/spike"
)

// ─────────────────────────────────────────────────────────────────────────────
// LWW GATE FOR DELETES — acceptance probes (Path Z / uniform model).
//
// These probes cover the two split-brain cases that the unconditional OpDelete
// left open after the identity-tiebreaker (Path Z) landed:
//
//  1. TestEqualTie_UpserterHigher_UpsertFirst_Converges
//     Exact (updated_at, version) tie; upserter has the HIGHER writer_id; central
//     orders the upsert FIRST (lower seq) and the delete SECOND (higher seq).
//     Without the LWW gate, central would unconditionally tombstone on the delete
//     (central=DELETED) while node A would revive (A=LIVE) → permanent split-brain.
//     With the gate, the delete loses writeWins against the live row on central
//     (deleter writer-A < upserter writer-Z) → NoOp on central → all three stores
//     converge to LIVE.
//
//  2. TestStaleDelete_LosesToNewerUpsert_Converges
//     Delete at updated_at=T1; upsert at updated_at=T2 (T2 > T1, strictly newer).
//     The delete must not tombstone the newer live row regardless of application
//     order. Both orderings are driven (delete-then-upsert and upsert-then-delete)
//     and all three stores must converge to LIVE.
//
// Both probes follow the same manual push/pull driving pattern as the existing
// tsseq_split_probe tests so the interleaving is deterministic.
// ─────────────────────────────────────────────────────────────────────────────

// TestEqualTie_UpserterHigher_UpsertFirst_Converges is the point-2 divergence
// case: exact (updated_at, version) tie at v2/TIE; writer-B (HIGHER writer_id)
// upserts and pushes FIRST (lower central seq); writer-A (LOWER writer_id)
// deletes and pushes SECOND (higher central seq).
//
// Without the LWW gate on OpDelete:
//   - Central applies the upsert (live), then the delete (unconditional) → DELETED.
//   - Node A: deleted locally; pulls B's upsert; tombstone guard writeWins(writer-B
//     vs tombstone writer-A) = true → revives → A=LIVE.
//   - Result: A=LIVE, central=DELETED → split-brain.
//
// With the LWW gate:
//   - Central: upsert lands live; delete arrives — writeWins(writer-A del vs live
//     writer-B row) = false (A < B) → NoOp → central stays LIVE.
//   - Node A: deleted locally; pulls B's upsert; tombstone guard writeWins(writer-B
//     vs tombstone writer-A) = true → revives → A=LIVE.
//   - All three stores = LIVE. Convergence.
func TestEqualTie_UpserterHigher_UpsertFirst_Converges(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")

	topic := "sdd/test/delete-lww-tie-upserter-higher"

	// ONE instant for BOTH writes so updated_at ties exactly.
	// Version is also equal (both v2) to force the contest to the tiebreaker.
	tie := base.Add(60 * time.Second)

	// Writer identities: upserter ("writer-Z") is HIGHER than deleter ("writer-A").
	const upWriter = "writer-Z"  // higher writer_id → upsert wins tiebreaker
	const delWriter = "writer-A" // lower writer_id → delete loses tiebreaker
	const upSync = "sync-Z"
	const delSync = "sync-A"

	// ── Step 1: establish a live row on all three stores (precondition) ─────────
	mustWrite(t, a, upsert(delWriter, delSync, topic, "initial v1", 1, base.Add(10*time.Second)))
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)
	if liveTopicOnNode(t, a, topic) == nil || liveTopicOnNode(t, b, topic) == nil || liveTopicOnCentral(t, central, topic) == nil {
		t.Fatalf("precondition: topic %q not live on all three stores", topic)
	}

	// ── Step 2: author the contesting writes locally ─────────────────────────────
	// B upserts at v2/TIE — higher writer_id (upserter wins tiebreaker).
	mustWrite(t, b, upsert(upWriter, upSync, topic, "B equal-tie upsert v2", 2, tie))
	// A deletes at v2/TIE — lower writer_id (deleter loses tiebreaker).
	mustWrite(t, a, del(delWriter, delSync, topic, 2, tie))

	// ── Step 3: FORCE central ordering — B (upserter) pushes FIRST ──────────────
	if _, err := spike.Push(ctx, b, central); err != nil {
		t.Fatalf("push B (upsert): %v", err)
	}
	sUp := maxCentralSeq(ctx, t, central)

	if _, err := spike.Push(ctx, a, central); err != nil {
		t.Fatalf("push A (delete): %v", err)
	}
	sDel := maxCentralSeq(ctx, t, central)

	t.Logf("central seqs: S_up=%d (%s upsert), S_del=%d (%s delete)",
		sUp, upWriter, sDel, delWriter)
	if !(sUp < sDel) {
		t.Fatalf("forced ordering not achieved: need S_up < S_del, got %d >= %d", sUp, sDel)
	}

	// ── Step 4: A and B pull ────────────────────────────────────────────────────
	if _, err := spike.Pull(ctx, a, central, project); err != nil {
		t.Fatalf("pull A: %v", err)
	}
	if _, err := spike.Pull(ctx, b, central, project); err != nil {
		t.Fatalf("pull B: %v", err)
	}

	// ── Step 5: additional settle rounds ─────────────────────────────────────────
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 3)

	aLive := liveTopicOnNode(t, a, topic) != nil
	bLive := liveTopicOnNode(t, b, topic) != nil
	cLive := liveTopicOnCentral(t, central, topic) != nil

	t.Logf("FINAL LIVENESS: A.local=%v  B.local=%v  central=%v", aLive, bLive, cLive)
	t.Logf("EXPECTED: all LIVE — delete (writer-A) loses LWW gate against live row (writer-Z); "+
		"upserter (writer-Z) wins everywhere → convergence to LIVE")

	// Convergence: all three stores must agree.
	if !(aLive == bLive && bLive == cLive) {
		t.Errorf("SPLIT-BRAIN: stores disagree on liveness of topic %q — "+
			"A.local=%v  B.local=%v  central=%v.\n"+
			"The delete (writer-A, lower identity) must be a NoOp on central "+
			"(loses writeWins against the live upsert row from writer-Z). "+
			"Check the LWW gate at the top of case OpDelete in domain.Decide.",
			topic, aLive, bLive, cLive)
	}

	// Expected final state: all LIVE (upserter wins the total order).
	if !aLive || !bLive || !cLive {
		t.Errorf("WRONG FINAL STATE: expected all LIVE (upserter writer-Z wins). "+
			"A=%v B=%v central=%v", aLive, bLive, cLive)
	}
}

// TestStaleDelete_LosesToNewerUpsert_Converges verifies that a delete at
// updated_at=T1 is a NoOp against a live upsert at updated_at=T2 (T2 > T1)
// regardless of the order in which the two mutations are applied.
//
// Two orderings are driven:
//   - Node A applies delete-then-upsert (delete first, then gets the newer upsert
//     on pull — the tombstone guard must let the newer upsert revive it).
//   - Node B applies upsert-then-delete (upsert already live; stale delete must be
//     a NoOp via the LWW gate).
//
// All three stores must converge to LIVE (the newer upsert always wins).
func TestStaleDelete_LosesToNewerUpsert_Converges(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")

	topic := "sdd/test/stale-delete-loses"

	// T2 (upsert) is strictly newer than T1 (delete).
	t1Del := base.Add(20 * time.Second) // older — delete timestamp
	t2Up := base.Add(80 * time.Second)  // newer — upsert timestamp

	const upWriter = "writer-B"
	const delWriter = "writer-A"
	const upSync = "sync-B"
	const delSync = "sync-A"

	// ── Step 1: establish a live row at v1 on all three stores ───────────────────
	mustWrite(t, a, upsert(delWriter, delSync, topic, "initial v1", 1, base.Add(5*time.Second)))
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)
	if liveTopicOnNode(t, a, topic) == nil || liveTopicOnNode(t, b, topic) == nil || liveTopicOnCentral(t, central, topic) == nil {
		t.Fatalf("precondition: topic %q not live on all three stores", topic)
	}

	// ── Step 2: writer-A authors a STALE delete (T1 < T2) locally ───────────────
	mustWrite(t, a, del(delWriter, delSync, topic, 2, t1Del))

	// ── Step 3: writer-B authors a NEWER upsert (T2 > T1) locally ───────────────
	mustWrite(t, b, upsert(upWriter, upSync, topic, "B newer upsert v2", 2, t2Up))

	// ── Step 4: push both — ordering intentionally: delete first, upsert second ──
	// This exercises: (a) central applies delete → tombstone, then upsert at T2
	// supersedes the tombstone (T2 > T1) → central LIVE.
	// (b) node A: deleted locally, then pulls upsert; tombstone guard lets T2 upsert
	// through → A revives to LIVE.
	// (c) node B: has upsert live, then pulls delete; LWW gate blocks the stale
	// delete (T1 < T2) → B stays LIVE.
	if _, err := spike.Push(ctx, a, central); err != nil {
		t.Fatalf("push A (stale delete): %v", err)
	}
	sDelCentral := maxCentralSeq(ctx, t, central)

	if _, err := spike.Push(ctx, b, central); err != nil {
		t.Fatalf("push B (newer upsert): %v", err)
	}
	sUpCentral := maxCentralSeq(ctx, t, central)

	t.Logf("central seqs: S_del=%d (stale delete at T1=%v), S_up=%d (newer upsert at T2=%v)",
		sDelCentral, t1Del, sUpCentral, t2Up)

	// ── Step 5: A and B pull ─────────────────────────────────────────────────────
	if _, err := spike.Pull(ctx, a, central, project); err != nil {
		t.Fatalf("pull A: %v", err)
	}
	if _, err := spike.Pull(ctx, b, central, project); err != nil {
		t.Fatalf("pull B: %v", err)
	}

	// ── Step 6: additional settle rounds ─────────────────────────────────────────
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 3)

	aLive := liveTopicOnNode(t, a, topic) != nil
	bLive := liveTopicOnNode(t, b, topic) != nil
	cLive := liveTopicOnCentral(t, central, topic) != nil

	t.Logf("FINAL LIVENESS: A.local=%v  B.local=%v  central=%v", aLive, bLive, cLive)
	t.Logf("EXPECTED: all LIVE — stale delete (T1=%v) must not tombstone the newer upsert (T2=%v)",
		t1Del, t2Up)

	// Convergence: all three stores must agree.
	if !(aLive == bLive && bLive == cLive) {
		t.Errorf("SPLIT-BRAIN: stores disagree on liveness of topic %q — "+
			"A.local=%v  B.local=%v  central=%v.\n"+
			"The stale delete (updated_at=%v) must be a NoOp against the newer live row "+
			"(updated_at=%v). Check the LWW gate at the top of case OpDelete in domain.Decide.",
			topic, aLive, bLive, cLive, t1Del, t2Up)
	}

	// Expected final state: all LIVE (newer upsert wins the total order).
	if !aLive || !bLive || !cLive {
		t.Errorf("WRONG FINAL STATE: expected all LIVE (newer upsert at T2=%v wins stale delete at T1=%v). "+
			"A=%v B=%v central=%v", t2Up, t1Del, aLive, bLive, cLive)
	}
}

var _ = time.Second // suppress unused import if helpers are changed
