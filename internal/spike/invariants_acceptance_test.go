//go:build acceptance

package spike_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/spike"
)

// ─────────────────────────────────────────────────────────────────────────────
// INV1 — topic convergence.
//
// A writes topic T (sync-A, older). B writes topic T (sync-B, STRICTLY newer).
// Both push, both pull (a couple of rounds to settle).
//
// Expected after convergence: exactly ONE live record for T on A.local, B.local,
// AND central — same canonical sync_id (sync-A, the first row pushed, resolved by
// FindByTopic on the central) and B's (newer) content. No duplicate.
//
// Push order is controlled: A pushes first so its row becomes the canonical
// identity on central; B's newer write then converges onto it (TargetSyncID =
// sync-A) carrying B's content. Both nodes pull the converged state back.
// ─────────────────────────────────────────────────────────────────────────────
func TestConvergence_INV1_TopicConvergence(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")

	topic := "sdd/test/inv1-topic"

	// A writes older; B writes newer (same topic, distinct sync_ids).
	if _, err := a.Write(upsert("writer-A", "sync-A", topic, "A content (older)", 1, base.Add(10*time.Second))); err != nil {
		t.Fatalf("A write: %v", err)
	}
	if _, err := b.Write(upsert("writer-B", "sync-B", topic, "B content (newer winner)", 2, base.Add(20*time.Second))); err != nil {
		t.Fatalf("B write: %v", err)
	}

	// A pushes first → sync-A is the canonical row on central.
	if _, err := spikePush(ctx, t, a, central); err != nil {
		t.Fatalf("push A: %v", err)
	}
	// B pushes second → converges onto canonical sync-A with B's newer content.
	if _, err := spikePush(ctx, t, b, central); err != nil {
		t.Fatalf("push B: %v", err)
	}

	// Settle: both nodes pull twice so each sees the converged canonical row.
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)

	// Three-way convergence: one live record, canonical sync-A, B's content.
	assertOneLiveEverywhere(t, a, b, central, topic, "sync-A", "B content (newer winner)")

	// No duplicate live row for the topic on central (belt-and-suspenders).
	assertCentralLiveCount(ctx, t, central, topic, 1)
}

// ─────────────────────────────────────────────────────────────────────────────
// INV2 — monotonic seq.
//
// Central assigns strictly increasing seq regardless of which writer pushes.
// Each writer writes ONE mutation, then pushes it immediately, alternating:
// A1 write+push, B1 write+push, A2 write+push, B2 write+push. This means each
// Push call drains exactly one outbox entry so the seq assignment genuinely
// interleaves across writers — A1 gets seq N, B1 gets N+1, A2 gets N+2, B2
// gets N+3 — rather than A1+A2 landing before B1+B2.
// ─────────────────────────────────────────────────────────────────────────────
func TestConvergence_INV2_MonotonicSeq(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")

	// Interleave writes AND pushes: A1, B1, A2, B2.
	// Each write is followed immediately by a push so central assigns seq in the
	// true interleaved order — A1:seq1, B1:seq2, A2:seq3, B2:seq4.
	if _, err := a.Write(upsert("writer-A", "sync-2a1", "sdd/test/inv2-a1", "a1", 1, base.Add(1*time.Second))); err != nil {
		t.Fatalf("A1 write: %v", err)
	}
	if _, err := spikePush(ctx, t, a, central); err != nil { // pushes a1 only
		t.Fatalf("push A1: %v", err)
	}

	if _, err := b.Write(upsert("writer-B", "sync-2b1", "sdd/test/inv2-b1", "b1", 1, base.Add(2*time.Second))); err != nil {
		t.Fatalf("B1 write: %v", err)
	}
	if _, err := spikePush(ctx, t, b, central); err != nil { // pushes b1 only
		t.Fatalf("push B1: %v", err)
	}

	if _, err := a.Write(upsert("writer-A", "sync-2a2", "sdd/test/inv2-a2", "a2", 1, base.Add(3*time.Second))); err != nil {
		t.Fatalf("A2 write: %v", err)
	}
	if _, err := spikePush(ctx, t, a, central); err != nil { // pushes a2 only
		t.Fatalf("push A2: %v", err)
	}

	if _, err := b.Write(upsert("writer-B", "sync-2b2", "sdd/test/inv2-b2", "b2", 1, base.Add(4*time.Second))); err != nil {
		t.Fatalf("B2 write: %v", err)
	}
	if _, err := spikePush(ctx, t, b, central); err != nil { // pushes b2 only
		t.Fatalf("push B2: %v", err)
	}

	// central_mutations.seq must be strictly increasing in insertion order.
	rows, err := central.Pool().Query(ctx, `SELECT seq FROM central_mutations ORDER BY seq`)
	if err != nil {
		t.Fatalf("query central_mutations: %v", err)
	}
	defer rows.Close()
	var seqs []int64
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan seq: %v", err)
		}
		seqs = append(seqs, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(seqs) != 4 {
		t.Fatalf("INV2: expected 4 central_mutations rows, got %d (%v)", len(seqs), seqs)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Errorf("INV2: central seq not strictly increasing: %v", seqs)
		}
	}

	// After settling, the central seq propagates onto a node's local row ONLY for
	// topics the node PULLED (accepted from central via Insert/Update). For topics
	// the node AUTHORED, its own local row already holds the content and the pulled
	// central-seq'd copy is an INV5 NoOp — so the authored row keeps local seq 0.
	// This is the correct, observed behavior: central seq is the authority; it
	// reaches a replica when that replica ACCEPTS the central row, not when the
	// replica authored the winning content itself.
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)

	// A authored a1,a2 → pulled b1,b2 (those carry central seq on A.local).
	assertPulledTopicHasSeq(t, a, "sdd/test/inv2-b1")
	assertPulledTopicHasSeq(t, a, "sdd/test/inv2-b2")
	// B authored b1,b2 → pulled a1,a2 (those carry central seq on B.local).
	assertPulledTopicHasSeq(t, b, "sdd/test/inv2-a1")
	assertPulledTopicHasSeq(t, b, "sdd/test/inv2-a2")

	// Every topic exists live on both nodes regardless of who authored it.
	for _, topic := range []string{"sdd/test/inv2-a1", "sdd/test/inv2-b1", "sdd/test/inv2-a2", "sdd/test/inv2-b2"} {
		for _, ref := range []*nodeRef{{a}, {b}} {
			if rec := liveTopicOnNode(t, ref.n, topic); rec == nil {
				t.Errorf("INV2: node %s missing live row for %q after sync", ref.n.Name, topic)
			}
		}
	}
}

// assertPulledTopicHasSeq asserts a topic the node PULLED from central carries a
// positive central seq on the node's local row (proves seq propagation on accept).
func assertPulledTopicHasSeq(t *testing.T, n *spike.Node, topic string) {
	t.Helper()
	rec := liveTopicOnNode(t, n, topic)
	if rec == nil {
		t.Errorf("INV2: node %s missing pulled topic %q", n.Name, topic)
		return
	}
	if rec.Seq <= 0 {
		t.Errorf("INV2: node %s pulled topic %q local seq=%d, want >0 (central seq propagated on accept)", n.Name, topic, rec.Seq)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// INV3 — no lost update.
//
// An OLDER write must not clobber a NEWER one after convergence. B writes T newer
// and pushes first (establishes newer state on central). A then writes T older
// and pushes. After full sync, T everywhere holds B's newer content — A's older
// write was discarded (NoOp) and did NOT overwrite.
// ─────────────────────────────────────────────────────────────────────────────
func TestConvergence_INV3_NoLostUpdate(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")

	topic := "sdd/test/inv3-nolost"

	// B writes NEWER and pushes first → newer state is canonical on central.
	if _, err := b.Write(upsert("writer-B", "sync-B", topic, "B content (newer, must survive)", 2, base.Add(50*time.Second))); err != nil {
		t.Fatalf("B write: %v", err)
	}
	if _, err := spikePush(ctx, t, b, central); err != nil {
		t.Fatalf("push B: %v", err)
	}

	// A writes OLDER (lower updated_at and version) and pushes.
	if _, err := a.Write(upsert("writer-A", "sync-A", topic, "A content (older, must lose)", 1, base.Add(10*time.Second))); err != nil {
		t.Fatalf("A write: %v", err)
	}
	if _, err := spikePush(ctx, t, a, central); err != nil {
		t.Fatalf("push A: %v", err)
	}

	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)

	// Everywhere: one live row, canonical sync-B, B's newer content (A lost).
	assertOneLiveEverywhere(t, a, b, central, topic, "sync-B", "B content (newer, must survive)")
	assertCentralLiveCount(ctx, t, central, topic, 1)
}

// ─────────────────────────────────────────────────────────────────────────────
// INV4 — no resurrection (the tombstone boundary).
//
// A writes T, both converge. A DELETES T. B's STALE upsert (older than the
// delete) must NOT revive it after sync. A strictly-NEWER upsert DOES revive it
// (deleted_at cleared on A.local, B.local AND central; FindByTopic returns it).
//
// All scenarios here use DISTINCT UpdatedAt values, so wall-clock (updated_at)
// decides the outcome. The identity tiebreaker (writer_id → winning mutation_id)
// is not exercised — these results are independent of the final tiebreaker field.
// ─────────────────────────────────────────────────────────────────────────────
func TestConvergence_INV4_NoResurrection(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")

	topic := "sdd/test/inv4-resurrect"

	// 1. A writes T; converge so both nodes + central hold it live.
	if _, err := a.Write(upsert("writer-A", "sync-A", topic, "A original", 1, base.Add(10*time.Second))); err != nil {
		t.Fatalf("A write: %v", err)
	}
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)
	assertOneLiveEverywhere(t, a, b, central, topic, "sync-A", "A original")

	// 2. A deletes T (delete time AFTER the original write). Converge.
	if _, err := a.Write(del("writer-A", "sync-A", topic, 2, base.Add(50*time.Second))); err != nil {
		t.Fatalf("A delete: %v", err)
	}
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)
	assertDeletedEverywhere(ctx, t, a, b, central, topic)

	// 3. B's STALE upsert — OLDER than the delete (updated_at < delete time).
	//    Must NOT revive T anywhere. This is the resurrection guard (INV4) and the
	//    updated_at is strictly older than the delete time, so wall-clock alone
	//    blocks resurrection here — the identity tiebreaker is not the deciding factor.
	if _, err := b.Write(upsert("writer-B", "sync-B", topic, "B STALE revive attempt", 1, base.Add(30*time.Second))); err != nil {
		t.Fatalf("B stale write: %v", err)
	}
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 3)
	assertDeletedEverywhere(ctx, t, a, b, central, topic) // STILL deleted — no resurrection.

	// 4. A strictly-NEWER upsert (updated_at AFTER the delete) DOES revive T.
	if _, err := a.Write(upsert("writer-A", "sync-A", topic, "A revived (newer than delete)", 3, base.Add(90*time.Second))); err != nil {
		t.Fatalf("A revive write: %v", err)
	}
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 3)

	// Revived live everywhere with the newer content under the canonical sync-A.
	assertOneLiveEverywhere(t, a, b, central, topic, "sync-A", "A revived (newer than delete)")
	assertCentralLiveCount(ctx, t, central, topic, 1)
}

// ─────────────────────────────────────────────────────────────────────────────
// INV5 — idempotent.
//
// Running sync repeatedly (re-push / re-pull the same mutations) does not
// double-apply: state stays stable and central seq does NOT grow on no-op rounds.
// ─────────────────────────────────────────────────────────────────────────────
func TestConvergence_INV5_Idempotent(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")

	topic := "sdd/test/inv5-idem"

	if _, err := a.Write(upsert("writer-A", "sync-A", topic, "A idem", 1, base.Add(10*time.Second))); err != nil {
		t.Fatalf("A write: %v", err)
	}
	// First settle.
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)

	seqAfterFirst := maxCentralSeq(ctx, t, central)
	mutCountFirst := centralMutationCount(ctx, t, central)
	verAfterFirst := liveTopicOnCentral(t, central, topic).Version

	// Run several more full sync rounds — they re-drain/re-pull the SAME state.
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 3)

	// Central seq must NOT have grown — no new mutations were inserted.
	if got := maxCentralSeq(ctx, t, central); got != seqAfterFirst {
		t.Errorf("INV5: central max seq grew on no-op rounds: %d → %d", seqAfterFirst, got)
	}
	if got := centralMutationCount(ctx, t, central); got != mutCountFirst {
		t.Errorf("INV5: central_mutations row count grew on no-op rounds: %d → %d", mutCountFirst, got)
	}
	// Version unchanged on central and both nodes (no version churn).
	if got := liveTopicOnCentral(t, central, topic).Version; got != verAfterFirst {
		t.Errorf("INV5: central version churned: %d → %d", verAfterFirst, got)
	}
	for _, ref := range []*nodeRef{{a}, {b}} {
		rec := liveTopicOnNode(t, ref.n, topic)
		if rec == nil {
			t.Fatalf("INV5: node %s lost the live row", ref.n.Name)
		}
		if rec.Version != verAfterFirst {
			t.Errorf("INV5: node %s version churned: want %d, got %d", ref.n.Name, verAfterFirst, rec.Version)
		}
	}
	// Exactly one live row everywhere — no duplicates from repeated apply.
	assertOneLiveEverywhere(t, a, b, central, topic, "sync-A", "A idem")
	assertCentralLiveCount(ctx, t, central, topic, 1)
}

// ─────────────────────────────────────────────────────────────────────────────
// INV6 — independent writes.
//
// A writes topic T1, B writes topic T2 (DIFFERENT topics). After sync, BOTH
// survive on both nodes AND central — distinct identities never conflict.
// ─────────────────────────────────────────────────────────────────────────────
func TestConvergence_INV6_IndependentWrites(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")

	t1 := "sdd/test/inv6-t1"
	t2 := "sdd/test/inv6-t2"

	if _, err := a.Write(upsert("writer-A", "sync-A", t1, "A's T1", 1, base.Add(10*time.Second))); err != nil {
		t.Fatalf("A write: %v", err)
	}
	if _, err := b.Write(upsert("writer-B", "sync-B", t2, "B's T2", 1, base.Add(20*time.Second))); err != nil {
		t.Fatalf("B write: %v", err)
	}

	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)

	// Both topics survive everywhere with their own content.
	assertOneLiveEverywhere(t, a, b, central, t1, "sync-A", "A's T1")
	assertOneLiveEverywhere(t, a, b, central, t2, "sync-B", "B's T2")
	assertCentralLiveCount(ctx, t, central, t1, 1)
	assertCentralLiveCount(ctx, t, central, t2, 1)
}

// ─────────────────────────────────────────────────────────────────────────────
// Settling proof — after a full bidirectional sync, A.local == B.local ==
// central for EVERY live record. This is the strongest convergence statement:
// all three stores agree on the canonical CONTENT and VERSION for each topic
// (the user-visible convergence state). sync_id and seq are NOT compared here —
// a node that authored the winning write keeps its own sync_id and local seq 0
// for that row, while the other node's pulled copy carries the central seq.
// See compareSnaps for the exact equality predicate.
// ─────────────────────────────────────────────────────────────────────────────
func TestConvergence_FullBidirectionalSettles(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")

	// Mixed workload across both writers:
	//   T1: contended topic — A older, B newer (B wins).
	//   T2: A-only independent topic.
	//   T3: B-only independent topic.
	//   T4: written by A then DELETED by A (ends tombstoned).
	t1, t2, t3, t4 := "sdd/test/settle-t1", "sdd/test/settle-t2", "sdd/test/settle-t3", "sdd/test/settle-t4"

	mustWrite(t, a, upsert("writer-A", "sync-A1", t1, "A T1 older", 1, base.Add(10*time.Second)))
	mustWrite(t, b, upsert("writer-B", "sync-B1", t1, "B T1 newer", 2, base.Add(20*time.Second)))
	mustWrite(t, a, upsert("writer-A", "sync-A2", t2, "A T2", 1, base.Add(15*time.Second)))
	mustWrite(t, b, upsert("writer-B", "sync-B2", t3, "B T3", 1, base.Add(25*time.Second)))
	mustWrite(t, a, upsert("writer-A", "sync-A4", t4, "A T4 (will delete)", 1, base.Add(30*time.Second)))

	// First convergence round (A pushes T1 first so sync-A1 is canonical for T1).
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)

	// Now A deletes T4 and both settle again.
	mustWrite(t, a, del("writer-A", "sync-A4", t4, 2, base.Add(60*time.Second)))
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)

	// ── Live-row settling: A.local == B.local == central for each live topic ──
	assertOneLiveEverywhere(t, a, b, central, t1, "sync-A1", "B T1 newer")
	assertOneLiveEverywhere(t, a, b, central, t2, "sync-A2", "A T2")
	assertOneLiveEverywhere(t, a, b, central, t3, "sync-B2", "B T3")

	// ── Tombstone settling: T4 deleted everywhere; no live row anywhere ──
	assertDeletedEverywhere(ctx, t, a, b, central, t4)

	// ── Full snapshot equality: A.local live set == B.local live set == central ──
	assertNodesAndCentralAgree(ctx, t, a, b, central)
}

// Helpers used above (spikePush, syncRounds, mustWrite, assertOneLiveEverywhere,
// assertDeletedEverywhere, assertCentralLiveCount, assertNodesAndCentralAgree,
// maxCentralSeq, centralMutationCount, and the nodeRef type) are defined in
// helpers_acceptance_test.go.
var _ = time.Second
