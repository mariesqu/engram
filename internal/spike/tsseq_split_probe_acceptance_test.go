//go:build acceptance

package spike_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/spike"
)

// ─────────────────────────────────────────────────────────────────────────────
// CONVERGENCE REGRESSION TESTS — identity tiebreaker (Path Z).
//
// These tests were originally a RED probe (TestTsSeq_SplitBrain_UpsertBeforeDelete)
// that reproduced the CONFIRMED split-brain caused by PR5's central-seq tombstone
// tiebreaker. Root cause: a node's OWN tombstones keep seq=0 permanently
// (AckMutation never back-patches central seq into memory_tombstones.seq; self-
// authored pulls are INV5 NoOps). So the authoring node and central computed
// DIFFERENT winners → permanent divergence.
//
// THE FIX (Path Z): resolve the (updated_at, version) tie by (writer_id, sync_id)
// — both STABLE and REPLICA-IDENTICAL (derived from the mutation payload; no
// central back-channel). Every store computes the SAME winner from the SAME
// inputs. Divergence at the exact tie is STRUCTURALLY IMPOSSIBLE.
//
// KEY TIEBREAKER INVARIANT:
//   writeWins(m, ..., curWriterID, curSyncID) is applied whenever an UPSERT
//   contests a tombstone or a live row. When both stores have identical field
//   values, they compute identical outcomes. The winner is the mutation with the
//   lexicographically HIGHER writer_id (then sync_id as final fallback).
//
// Two probes are provided:
//
//  TestEqualTie_UpsertBeforeDelete_Converges
//    — Forces the pathological interleaving (upsert pushed before delete) with
//      writer names chosen so the DELETE's writer wins the tiebreaker (the
//      deleter has a higher writer_id). Result: all three stores agree = DELETED.
//      This is the regression guard: under the identity tiebreaker every store
//      that applies the upsert against the tombstone computes the SAME result
//      (block, because deleter > upserter). No seq asymmetry, no split-brain.
//
//  TestEqualTie_UpsertVsUpsert_Converges
//    — Two writers upsert the same topic at identical (updated_at, version).
//      After full sync all three stores must agree on the SAME content (the
//      winner's content per writer_id tiebreaker). Proves Z closes the upsert tie.
// ─────────────────────────────────────────────────────────────────────────────

// TestEqualTie_UpsertBeforeDelete_Converges forces the exact interleaving that
// caused the PR5 split-brain (B's upsert pushed to central before A's delete)
// and asserts that all three stores AGREE on the final liveness.
//
// Writer names are chosen so the DELETE's writer_id is lexicographically higher
// ("writer-Z" deletes, "writer-A" upserts). Under the identity tiebreaker:
//   writeWins("writer-A"/"sync-A" vs tombstone "writer-Z"/"sync-Z") = FALSE
//   (upserter < deleter) → upsert BLOCKED everywhere → all three stores = DELETED.
//
// The forced interleaving (B upsert pushes first, A delete pushes second) is
// kept so the probe still exercises the exact PR5 scenario. Unlike the old
// seq tiebreaker (seq=0 asymmetry), the identity tiebreaker is SYMMETRIC:
// every store applies the same comparison from the same payload fields and
// reaches the same conclusion.
func TestEqualTie_UpsertBeforeDelete_Converges(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")

	topic := "sdd/test/tsseq-split"

	// ONE instant for BOTH writes so updated_at ties exactly.
	// Version is also equal (both v2) to force the contest down to the tiebreaker.
	tie := base.Add(50 * time.Second)

	// Writer names: "writer-Z" (deleter, HIGHER) vs "writer-A" (upserter, LOWER).
	// Under the identity tiebreaker: "writer-A" < "writer-Z" → upsert loses → BLOCKED.
	const delWriter = "writer-Z"  // higher writer_id → delete wins tiebreaker
	const upWriter = "writer-A"   // lower writer_id → upsert loses tiebreaker
	const delSync = "sync-Z"
	const upSync = "sync-A"

	// ── Step 1: T live everywhere (deleter authors v1, converge fully) ─────────
	mustWrite(t, a, upsert(delWriter, delSync, topic, "Z original v1", 1, base.Add(10*time.Second)))
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)
	if liveTopicOnNode(t, a, topic) == nil || liveTopicOnNode(t, b, topic) == nil || liveTopicOnCentral(t, central, topic) == nil {
		t.Fatalf("precondition: topic %q not live on all three stores", topic)
	}

	// ── Step 2: author the two contesting writes LOCALLY ──────────────────────
	// B upserts T (v2, TIE) — lower writer_id (upserter).
	mustWrite(t, b, upsert(upWriter, upSync, topic, "A equal-tie upsert", 2, tie))
	// A deletes T (v2, TIE) — higher writer_id (deleter).
	mustWrite(t, a, del(delWriter, delSync, topic, 2, tie))

	// Verify A's local tombstone is present before the pull.
	aTombBefore, err := a.Store.FindTombstone(delSync, strp(topic), project, scope)
	if err != nil {
		t.Fatalf("A FindTombstone (pre-pull): %v", err)
	}
	if aTombBefore == nil {
		t.Fatalf("A: expected a local tombstone for %q after self-delete, got nil", topic)
	}
	t.Logf("A local tombstone before pull: sync_id=%s version=%d deleted_by=%s",
		aTombBefore.SyncID, aTombBefore.Version, aTombBefore.DeletedBy)

	// ── Step 3: FORCE central ordering — B (upserter) PUSHES FIRST ────────────
	if _, err := spike.Push(ctx, b, central); err != nil {
		t.Fatalf("push B (upsert): %v", err)
	}
	sUp := maxCentralSeq(ctx, t, central)

	if _, err := spike.Push(ctx, a, central); err != nil {
		t.Fatalf("push A (delete): %v", err)
	}
	sDel := maxCentralSeq(ctx, t, central)

	t.Logf("central seqs after forced push order: S_up=%d (%s upsert), S_del=%d (%s delete)",
		sUp, upWriter, sDel, delWriter)

	if !(sUp < sDel) {
		t.Fatalf("FORCED-ORDERING NOT ACHIEVED: need S_up < S_del, got %d >= %d", sUp, sDel)
	}

	// Central must be DELETED: the delete WINS the LWW total order against the live
	// upsert (deleter writer-Z > upserter writer-A), so the OpDelete gate tombstones.
	if rec := liveTopicOnCentral(t, central, topic); rec != nil {
		t.Fatalf("central: topic %q unexpectedly LIVE after delete pushed; expected DELETED", topic)
	}

	// ── Step 4+5: A and B PULL ────────────────────────────────────────────────
	if _, err := spike.Pull(ctx, a, central, project); err != nil {
		t.Fatalf("pull A: %v", err)
	}
	if _, err := spike.Pull(ctx, b, central, project); err != nil {
		t.Fatalf("pull B: %v", err)
	}

	// ── Step 6: Additional settle rounds ──────────────────────────────────────
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 3)

	aLive := liveTopicOnNode(t, a, topic) != nil
	bLive := liveTopicOnNode(t, b, topic) != nil
	cLive := liveTopicOnCentral(t, central, topic) != nil

	t.Logf("FINAL LIVENESS: A.local=%v  B.local=%v  central=%v", aLive, bLive, cLive)
	t.Logf("DIAGNOSIS: deleter=%s (higher writer_id), upserter=%s (lower writer_id) "+
		"→ writeWins(%s vs tombstone %s) = %q > %q = FALSE → upsert BLOCKED → all DELETED",
		delWriter, upWriter, upWriter, delWriter, upWriter, delWriter)

	// ALL THREE must agree: DELETED. The identity tiebreaker is symmetric —
	// every store that evaluates writeWins(upserter vs deleter tombstone) computes
	// the same result (upserter < deleter → blocked). No seq=0 asymmetry.
	if !(aLive == bLive && bLive == cLive) {
		t.Errorf("SPLIT-BRAIN: stores disagree on liveness of topic %q — "+
			"A.local=%v  B.local=%v  central=%v.\n"+
			"Identity tiebreaker writeWins(%q/%q vs tombstone %q/%q) must produce "+
			"the SAME outcome on every store. Check writeWins call site at the tombstone "+
			"boundary in domain.Decide.",
			topic, aLive, bLive, cLive,
			upWriter, upSync, delWriter, delSync)
	}

	// Belt-and-suspenders: verify the expected final state (all DELETED).
	if aLive || bLive || cLive {
		t.Errorf("UNEXPECTED REVIVAL: all stores should be DELETED (deleter wins tiebreaker). "+
			"A=%v B=%v central=%v", aLive, bLive, cLive)
	}
}

// TestEqualTie_UpsertVsUpsert_Converges verifies that an UPSERT-vs-UPSERT
// exact tie (two writers upsert the same topic at identical updated_at + version)
// converges to the SAME content on all three stores. This proves the identity
// tiebreaker closes the upsert tie (the old cur.Seq=0 split-brain path).
//
// "writer-B" > "writer-A" → B's content wins on every store that applies the
// tiebreaker. After full sync all three must agree on B's content.
func TestEqualTie_UpsertVsUpsert_Converges(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")

	topic := "sdd/test/tsseq-upsert-tie"

	tie := base.Add(50 * time.Second)

	// Both writers upsert the same topic at identical updated_at + version.
	// "writer-B" > "writer-A" → B's content wins the tiebreaker.
	mustWrite(t, a, upsert("writer-A", "sync-A", topic, "A tie content (should lose)", 2, tie))
	mustWrite(t, b, upsert("writer-B", "sync-B", topic, "B tie content (should win)", 2, tie))

	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 5)

	aRec := liveTopicOnNode(t, a, topic)
	bRec := liveTopicOnNode(t, b, topic)
	cRec := liveTopicOnCentral(t, central, topic)

	if aRec == nil || bRec == nil || cRec == nil {
		t.Fatalf("upsert-vs-upsert tie: record not live everywhere: A=%v B=%v central=%v",
			aRec != nil, bRec != nil, cRec != nil)
	}

	// All three stores must agree on the same content.
	if aRec.Content != bRec.Content || bRec.Content != cRec.Content {
		t.Errorf("upsert-vs-upsert tie: stores disagree on content — "+
			"A=%q  B=%q  central=%q. "+
			"Identity tiebreaker (writer_id→sync_id) must pick the SAME winner everywhere.",
			aRec.Content, bRec.Content, cRec.Content)
	} else {
		t.Logf("CONVERGED: all three stores agree on content=%q (winner: writer-B via writer_id tiebreaker)",
			cRec.Content)
	}
}

var _ = time.Second
