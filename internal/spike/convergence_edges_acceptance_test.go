//go:build acceptance

package spike_test

// ─────────────────────────────────────────────────────────────────────────────
// CONVERGENCE-EDGE REGRESSION GUARDS — identity tiebreaker + uniform LWW model.
//
// These tests are adversarial convergence probes for the identity-tiebreaker
// model (Path Z / uniform LWW). They guard crown-jewel scenarios that the
// original spike suite missed: cross-writer tombstone resolution, duplicate
// tombstone accumulation, two-live-row invariants, the central topic-uidx crash
// hazard, and pull-order independence at exact ties.
//
// Each probe FORCES its interleaving via explicit Push/Pull sequencing and
// asserts the forced ordering was achieved (t.Fatalf seq-order assertions).
// Tests use descriptive names that identify the invariant under attack.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/mutation"
	"github.com/mariesqu/engram/internal/spike"
)

// ── Helper functions unique to convergence-edge probes ───────────────────────

// localTombstoneCount returns the number of memory_tombstones rows for a topic
// on a node's local SQLite store. Exposes duplicate-tombstone accumulation:
// more than one tombstone per topic makes FindTombstone-by-topic non-deterministic.
func localTombstoneCount(t *testing.T, n *spike.Node, topic string) int {
	t.Helper()
	var got int
	if err := n.Store.DB().QueryRow(
		`SELECT count(*) FROM memory_tombstones
		   WHERE topic_key=? AND project=? AND scope=?`,
		topic, project, scope,
	).Scan(&got); err != nil {
		t.Fatalf("localTombstoneCount %s (%q): %v", n.Name, topic, err)
	}
	return got
}

// localLiveCount returns the number of LIVE rows for a topic on a node's local store.
func localLiveCount(t *testing.T, n *spike.Node, topic string) int {
	t.Helper()
	var got int
	if err := n.Store.DB().QueryRow(
		`SELECT count(*) FROM memories
		   WHERE topic_key=? AND project=? AND scope=? AND deleted_at IS NULL`,
		topic, project, scope,
	).Scan(&got); err != nil {
		t.Fatalf("localLiveCount %s (%q): %v", n.Name, topic, err)
	}
	return got
}

// centralLiveCount returns the number of LIVE rows for a topic on central.
func centralLiveCount(ctx context.Context, t *testing.T, c *centralStore, topic string) int {
	t.Helper()
	var n int
	if err := c.Pool().QueryRow(ctx,
		`SELECT count(*) FROM central_memories
		   WHERE topic_key=$1 AND project=$2 AND scope=$3 AND deleted_at IS NULL`,
		topic, project, scope,
	).Scan(&n); err != nil {
		t.Fatalf("centralLiveCount(%q): %v", topic, err)
	}
	return n
}

// centralTombstoneCount returns the number of central_tombstones rows for a topic.
func centralTombstoneCount(ctx context.Context, t *testing.T, c *centralStore, topic string) int {
	t.Helper()
	var n int
	if err := c.Pool().QueryRow(ctx,
		`SELECT count(*) FROM central_tombstones WHERE topic_key=$1 AND project=$2 AND scope=$3`,
		topic, project, scope,
	).Scan(&n); err != nil {
		t.Fatalf("centralTombstoneCount(%q): %v", topic, err)
	}
	return n
}

func contentOrEmpty(r *domain.Record) string {
	if r == nil {
		return ""
	}
	return r.Content
}

// ── Probe: author keeps own sync_id; foreign delete reaches it via topic ─────

// TestConvergence_CrossWriterForeignDelete_ReachesRowViaTopic is the "strongest"
// probe. When a node AUTHORS the wall-clock-winning content for a contested topic
// it keeps its OWN sync_id locally (central's canonical sync_id is rejected as an
// older NoOp on that node). So the node's live row has a DIFFERENT sync_id than
// central's canonical. If a THIRD writer later deletes the topic the delete is
// materialized on central under the canonical sync_id. When the authoring node
// PULLS that delete it must resolve it to its OWN divergent-sync_id row via
// topic_key (FindByTopic) and tombstone it — otherwise the node keeps a live row
// forever while central is deleted → permanent split-brain.
func TestConvergence_CrossWriterForeignDelete_ReachesRowViaTopic(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")
	c := newNode(t, "C")

	topic := "sdd/test/adv-author-keeps-syncid"
	nodes := []*nodeRef{{a}, {b}, {c}}

	tOlder := base.Add(10 * time.Second) // A authors (older) — pushed FIRST → canonical
	tNewer := base.Add(40 * time.Second) // B authors (newer) — wins content
	tDelete := base.Add(80 * time.Second)

	// A authors T older; pushes FIRST so sync-A is the canonical central identity.
	mustWrite(t, a, upsert("writer-A", "sync-A", topic, "A content (older, canonical id)", 1, tOlder))
	if _, err := spike.Push(ctx, a, central); err != nil {
		t.Fatalf("push A: %v", err)
	}

	// B authors T newer under sync-B; B has its OWN live row (sync-B) locally.
	mustWrite(t, b, upsert("writer-B", "sync-B", topic, "B content (newer winner)", 2, tNewer))

	// Settle: central canonical = sync-A with B's content; B keeps its own sync-B row.
	syncRounds(ctx, t, nodes, central, 3)

	// Confirm the divergent-sync_id precondition: B's local live row is sync-B while
	// central's canonical is sync-A (both holding B's content).
	bRec := liveTopicOnNode(t, b, topic)
	ctrRec := liveTopicOnCentral(t, central, topic)
	if bRec == nil || ctrRec == nil {
		t.Fatalf("precondition: T must be live on B and central")
	}
	t.Logf("precondition: B.local sync_id=%s, central sync_id=%s (content B=%q central=%q)",
		bRec.SyncID, ctrRec.SyncID, bRec.Content, ctrRec.Content)
	if bRec.SyncID == ctrRec.SyncID {
		t.Logf("note: B and central share sync_id %s — divergent-id precondition not hit, "+
			"but convergence assertion below still valid", bRec.SyncID)
	}

	// C (a THIRD writer that never authored T) deletes the topic.
	mustWrite(t, c, del("writer-C", "sync-C", topic, 3, tDelete))
	syncRounds(ctx, t, nodes, central, 4)

	aLive := liveTopicOnNode(t, a, topic) != nil
	bLive := liveTopicOnNode(t, b, topic) != nil
	cLive := liveTopicOnNode(t, c, topic) != nil
	ctrLive := liveTopicOnCentral(t, central, topic) != nil

	t.Logf("FINAL: A=%v B=%v C=%v central=%v | B.local live rows for topic=%d",
		aLive, bLive, cLive, ctrLive, localLiveCount(t, b, topic))

	if !(aLive == bLive && bLive == cLive && cLive == ctrLive) {
		t.Errorf("SPLIT-BRAIN: foreign delete failed to reach a divergent-sync_id row — "+
			"A=%v B=%v C=%v central=%v", aLive, bLive, cLive, ctrLive)
	}
	if aLive || bLive || cLive || ctrLive {
		t.Errorf("WRONG STATE: C's foreign delete must tombstone everywhere; got A=%v B=%v C=%v central=%v",
			aLive, bLive, cLive, ctrLive)
	}
	// B must have ZERO live rows (its divergent sync-B row got tombstoned via topic resolution).
	if got := localLiveCount(t, b, topic); got != 0 {
		t.Errorf("B.local retained %d live rows for topic %q after foreign delete "+
			"(delete did not reach B's divergent-sync_id row via topic resolution)", got, topic)
	}
}

// ── Probe: del→revive→del cycle stays at exactly one tombstone ───────────────

// TestConvergence_DeleteReviveDelete_SingleTombstone constructs a
// delete→revive→delete cycle across three nodes and asserts that no node
// accumulates more than one local tombstone for the topic across the cycle
// (duplicate tombstones make FindTombstone-by-topic non-deterministic).
//
// Cycle:
//  1. A authors T; converge.
//  2. B cross-writer deletes T (never authored it); converge. All DELETED.
//  3. C revives T under a new sync-C (newer than delete); converge. All LIVE.
//  4. A deletes again under sync-A (newest); converge. All DELETED.
func TestConvergence_DeleteReviveDelete_SingleTombstone(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")
	c := newNode(t, "C")

	topic := "sdd/test/adv-del-revive-del"

	tWrite := base.Add(10 * time.Second)
	tDel1 := base.Add(40 * time.Second)
	tRevive := base.Add(70 * time.Second)
	tDel2 := base.Add(100 * time.Second)

	nodes := []*nodeRef{{a}, {b}, {c}}

	// 1. A authors T; converge.
	mustWrite(t, a, upsert("writer-A", "sync-A", topic, "A original", 1, tWrite))
	syncRounds(ctx, t, nodes, central, 2)
	if liveTopicOnCentral(t, central, topic) == nil {
		t.Fatalf("precondition: T live after A authors")
	}

	// 2. B cross-writer deletes T (B never authored it); converge.
	mustWrite(t, b, del("writer-B", "sync-B", topic, 2, tDel1))
	syncRounds(ctx, t, nodes, central, 3)
	assertDeletedEverywhere(ctx, t, a, b, central, topic)
	t.Logf("after cross-writer delete: tombs A=%d B=%d C=%d",
		localTombstoneCount(t, a, topic), localTombstoneCount(t, b, topic), localTombstoneCount(t, c, topic))

	// 3. C revives T under a NEW sync-C (newer than the delete); converge.
	mustWrite(t, c, upsert("writer-C", "sync-C", topic, "C revived", 3, tRevive))
	syncRounds(ctx, t, nodes, central, 4)
	if liveTopicOnCentral(t, central, topic) == nil {
		t.Errorf("revive failed: central not live after C's newer upsert")
	}
	t.Logf("after revive: tombs A=%d B=%d C=%d | central content=%q",
		localTombstoneCount(t, a, topic), localTombstoneCount(t, b, topic), localTombstoneCount(t, c, topic),
		contentOrEmpty(liveTopicOnCentral(t, central, topic)))

	// 4. A deletes again under sync-A (newest); converge.
	mustWrite(t, a, del("writer-A", "sync-A", topic, 4, tDel2))
	syncRounds(ctx, t, nodes, central, 4)

	aLive := liveTopicOnNode(t, a, topic) != nil
	bLive := liveTopicOnNode(t, b, topic) != nil
	cLive := liveTopicOnNode(t, c, topic) != nil
	ctrLive := liveTopicOnCentral(t, central, topic) != nil
	aTombs := localTombstoneCount(t, a, topic)
	bTombs := localTombstoneCount(t, b, topic)
	cTombs := localTombstoneCount(t, c, topic)

	t.Logf("FINAL: live A=%v B=%v C=%v central=%v | tombs A=%d B=%d C=%d",
		aLive, bLive, cLive, ctrLive, aTombs, bTombs, cTombs)

	if !(aLive == bLive && bLive == cLive && cLive == ctrLive) {
		t.Errorf("SPLIT-BRAIN liveness after del→revive→del: A=%v B=%v C=%v central=%v",
			aLive, bLive, cLive, ctrLive)
	}
	if aLive || bLive || cLive || ctrLive {
		t.Errorf("WRONG STATE: final delete must win; got A=%v B=%v C=%v central=%v",
			aLive, bLive, cLive, ctrLive)
	}
	for _, tc := range []struct {
		name string
		n    int
	}{{"A", aTombs}, {"B", bTombs}, {"C", cTombs}} {
		if tc.n > 1 {
			t.Errorf("DUPLICATE TOMBSTONE on %s: %d for topic %q across del→revive→del cycle",
				tc.name, tc.n, topic)
		}
	}
}

// ── Probe: two-live-rows hunt (INV-A) ────────────────────────────────────────

// TestConvergence_HuntTwoLiveRows_ReviveRace hunts for two simultaneously live
// rows for one topic on any local node. Central's UNIQUE partial index forbids
// this at the DB level; local memories has only sync_id UNIQUE (no topic-unique
// index), so a race during revive could leave two live rows and make
// FindByTopic LIMIT 1 non-deterministic.
//
// Construction maximizes the chance of a second live row:
//  1. A authors T under sync-A; converge.
//  2. A deletes T; converge so the tombstone (sync-A) lands everywhere.
//  3. B authors a fresh write under sync-B (newer than the delete). B already
//     holds the tombstone for sync-A so the upsert revives sync-A in place —
//     the convergence path. Each node must hold EXACTLY ONE live row for T.
func TestConvergence_HuntTwoLiveRows_ReviveRace(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")

	topic := "sdd/test/adv-two-live-rows"

	tWrite := base.Add(10 * time.Second)
	tDel := base.Add(40 * time.Second)
	tB := base.Add(70 * time.Second)

	// 1. A authors T; converge.
	mustWrite(t, a, upsert("writer-A", "sync-A", topic, "A original", 1, tWrite))
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)

	// 2. A deletes T; converge so the tombstone (sync-A) lands on A, B, central.
	mustWrite(t, a, del("writer-A", "sync-A", topic, 2, tDel))
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)
	assertDeletedEverywhere(ctx, t, a, b, central, topic)

	// 3. B authors a fresh write under sync-B (newer than the delete).
	mustWrite(t, b, upsert("writer-B", "sync-B", topic, "B revive content", 3, tB))
	t.Logf("B after local revive write: live rows=%d, tombs=%d",
		localLiveCount(t, b, topic), localTombstoneCount(t, b, topic))

	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 5)

	aLiveCount := localLiveCount(t, a, topic)
	bLiveCount := localLiveCount(t, b, topic)
	ctrLiveCount := centralLiveCount(ctx, t, central, topic)

	t.Logf("FINAL live-row counts: A=%d B=%d central=%d", aLiveCount, bLiveCount, ctrLiveCount)

	if aLiveCount > 1 {
		t.Errorf("TWO LIVE ROWS on A.local for topic %q: %d (FindByTopic non-deterministic)", topic, aLiveCount)
	}
	if bLiveCount > 1 {
		t.Errorf("TWO LIVE ROWS on B.local for topic %q: %d (FindByTopic non-deterministic)", topic, bLiveCount)
	}
	if ctrLiveCount > 1 {
		t.Errorf("TWO LIVE ROWS on central for topic %q: %d (UNIQUE index should forbid this)", topic, ctrLiveCount)
	}

	aRec := liveTopicOnNode(t, a, topic)
	bRec := liveTopicOnNode(t, b, topic)
	ctrRec := liveTopicOnCentral(t, central, topic)
	aLive := aRec != nil
	bLive := bRec != nil
	cLive := ctrRec != nil
	if !(aLive == bLive && bLive == cLive) {
		t.Errorf("SPLIT-BRAIN liveness: A=%v B=%v central=%v", aLive, bLive, cLive)
	}
	if aLive && cLive && aRec.Content != ctrRec.Content {
		t.Errorf("CONTENT DIVERGENCE: A=%q central=%q", aRec.Content, ctrRec.Content)
	}
	if bLive && cLive && bRec.Content != ctrRec.Content {
		t.Errorf("CONTENT DIVERGENCE: B=%q central=%q", bRec.Content, ctrRec.Content)
	}
}

// ── Probe: central topic-uidx crash hazard ────────────────────────────────────

// TestConvergence_ConcurrentCrossWriterDeletes_NoCentralCrash stresses the
// central_tombstones_topic_uidx (23505) by forcing two concurrent cross-writer
// deletes to race at the same instant after a revive. The first delete targets
// the live row (canonical sync_id); the second arrives when the row is already
// soft-deleted. Neither push must fail with a unique-index violation, and all
// stores must converge to DELETED with exactly one central tombstone.
func TestConvergence_ConcurrentCrossWriterDeletes_NoCentralCrash(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")
	c := newNode(t, "C")
	d := newNode(t, "D")

	topic := "sdd/test/adv-xwriter-del-after-revive"
	nodes := []*nodeRef{{a}, {b}, {c}, {d}}

	tWrite := base.Add(10 * time.Second)
	tDel1 := base.Add(30 * time.Second)
	tRevive := base.Add(60 * time.Second)
	tDel2 := base.Add(90 * time.Second)

	// 1. A authors T.
	mustWrite(t, a, upsert("writer-A", "sync-A", topic, "A original", 1, tWrite))
	syncRounds(ctx, t, nodes, central, 2)

	// 2. A deletes T.
	mustWrite(t, a, del("writer-A", "sync-A", topic, 2, tDel1))
	syncRounds(ctx, t, nodes, central, 2)
	assertDeletedEverywhere(ctx, t, a, b, central, topic)

	// 3. B revives T under sync-B (newer).
	mustWrite(t, b, upsert("writer-B", "sync-B", topic, "B revived", 3, tRevive))
	syncRounds(ctx, t, nodes, central, 3)
	if liveTopicOnCentral(t, central, topic) == nil {
		t.Fatalf("revive failed on central")
	}

	// 4. TWO concurrent cross-writer deletes at the same newest instant from C and D.
	mustWrite(t, c, del("writer-C", "sync-C", topic, 4, tDel2))
	mustWrite(t, d, del("writer-D", "sync-D", topic, 4, tDel2))

	// Force ordering: C pushes first → central resolves canonical sync-A → tombstone sync-A.
	if _, err := spike.Push(ctx, c, central); err != nil {
		t.Fatalf("push C (delete): central Apply error (possible topic-tombstone uidx crash): %v", err)
	}
	// D pushes second → central: cur==nil (sync-A now soft-deleted), tombstone sync-A
	// exists → re-tombstone sync-A (target=ts.SyncID). Must NOT hit the unique index.
	if _, err := spike.Push(ctx, d, central); err != nil {
		t.Fatalf("push D (delete): central Apply error (possible topic-tombstone uidx crash): %v", err)
	}

	syncRounds(ctx, t, nodes, central, 4)

	aLive := liveTopicOnNode(t, a, topic) != nil
	bLive := liveTopicOnNode(t, b, topic) != nil
	cLive := liveTopicOnNode(t, c, topic) != nil
	dLive := liveTopicOnNode(t, d, topic) != nil
	ctrLive := liveTopicOnCentral(t, central, topic) != nil

	t.Logf("FINAL: A=%v B=%v C=%v D=%v central=%v | central tombstones=%d",
		aLive, bLive, cLive, dLive, ctrLive, centralTombstoneCount(ctx, t, central, topic))

	if !(aLive == bLive && bLive == cLive && cLive == dLive && dLive == ctrLive) {
		t.Errorf("SPLIT-BRAIN: A=%v B=%v C=%v D=%v central=%v", aLive, bLive, cLive, dLive, ctrLive)
	}
	if aLive || bLive || cLive || dLive || ctrLive {
		t.Errorf("WRONG STATE: final deletes must win; got A=%v B=%v C=%v D=%v central=%v",
			aLive, bLive, cLive, dLive, ctrLive)
	}
	// Central must hold exactly ONE tombstone for the topic (uidx enforced).
	if got := centralTombstoneCount(ctx, t, central, topic); got != 1 {
		t.Errorf("central tombstones for topic %q = %d; want exactly 1", topic, got)
	}
}

// ── Probe: hostile-interleaved INV4 (stale upsert, opposite node-local order) ─

// TestConvergence_INV4_HostileInterleaving_NoResurrection tests INV4 under a
// hostile interleaving. The happy-path INV4 test pushes a stale upsert AFTER
// the delete has already converged. This probe instead authors a stale upsert on
// B and a newer delete on A, then forces central to apply the DELETE FIRST and
// the STALE upsert SECOND, while node B applies them in the OPPOSITE local order
// (B has its own stale upsert live, then pulls the delete). The stale upsert
// must NEVER resurrect the record on any store regardless of application order.
func TestConvergence_INV4_HostileInterleaving_NoResurrection(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")

	topic := "sdd/test/adv-inv4-hostile"

	tInit := base.Add(10 * time.Second)
	tStaleUpsert := base.Add(40 * time.Second)
	tDelete := base.Add(70 * time.Second) // A's delete is NEWER → must win

	// 1. Establish a live row at v1 (A authors), converge.
	mustWrite(t, a, upsert("writer-A", "sync-A", topic, "initial", 1, tInit))
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)

	// 2. B authors a STALE upsert (older than the forthcoming delete) under sync-B.
	mustWrite(t, b, upsert("writer-B", "sync-B", topic, "B stale revive attempt", 2, tStaleUpsert))
	// 3. A authors a NEWER delete under sync-A.
	mustWrite(t, a, del("writer-A", "sync-A", topic, 3, tDelete))

	// Force central ordering: A's DELETE pushes FIRST (central tombstones),
	// then B's STALE upsert pushes SECOND (must be blocked — older than tombstone).
	if _, err := spike.Push(ctx, a, central); err != nil {
		t.Fatalf("push A (delete): %v", err)
	}
	sDel := maxCentralSeq(ctx, t, central)
	if rec := liveTopicOnCentral(t, central, topic); rec != nil {
		t.Fatalf("central: expected DELETED after A's delete push; got live %q", rec.SyncID)
	}
	if _, err := spike.Push(ctx, b, central); err != nil {
		t.Fatalf("push B (stale upsert): %v", err)
	}
	sUp := maxCentralSeq(ctx, t, central)
	t.Logf("central seqs: S_del=%d (delete), S_up=%d (stale upsert)", sDel, sUp)

	// Assert the forced ordering.
	if !(sDel < sUp) {
		t.Fatalf("FORCED-ORDERING NOT ACHIEVED: need S_del < S_up, got %d >= %d", sDel, sUp)
	}

	// Central MUST still be DELETED — the stale upsert lost writeWins vs the tombstone.
	if rec := liveTopicOnCentral(t, central, topic); rec != nil {
		t.Errorf("RESURRECTION on central: stale upsert (T=%v) revived the row over a newer tombstone (T=%v); live sync_id=%s",
			tStaleUpsert, tDelete, rec.SyncID)
	}

	// B applies its own stale upsert first (already live locally) then pulls the
	// delete — must end DELETED, no resurrection.
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 4)

	aLive := liveTopicOnNode(t, a, topic) != nil
	bLive := liveTopicOnNode(t, b, topic) != nil
	cLive := liveTopicOnCentral(t, central, topic) != nil

	t.Logf("FINAL: A=%v B=%v central=%v", aLive, bLive, cLive)

	if !(aLive == bLive && bLive == cLive) {
		t.Errorf("SPLIT-BRAIN: A=%v B=%v central=%v", aLive, bLive, cLive)
	}
	if aLive || bLive || cLive {
		t.Errorf("INV4 VIOLATION (resurrection): stale upsert must NOT revive the deleted row; got A=%v B=%v central=%v",
			aLive, bLive, cLive)
	}
}

// ── Probe: 3-node tie with different pull orders → converge ──────────────────

// TestConvergence_ThreeNodeTie_DifferentPullOrders verifies that a 3-way exact
// (updated_at, version) tie converges to the same content regardless of the
// order in which nodes pull from central. All three nodes author the topic at
// the EXACT same (updated_at, version) under distinct writer_ids. Push order:
// A, B, C (canonical = sync-A). The highest writer_id ("writer-C") wins.
// Pull order is rotated deliberately across 3 rounds: (C,A,B), (B,C,A), (A,B,C).
func TestConvergence_ThreeNodeTie_DifferentPullOrders(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")
	c := newNode(t, "C")

	topic := "sdd/test/adv-three-node-tie"
	tie := base.Add(40 * time.Second)

	mustWrite(t, a, upsert("writer-A", "sync-A", topic, "A tie content", 2, tie))
	mustWrite(t, b, upsert("writer-B", "sync-B", topic, "B tie content", 2, tie))
	mustWrite(t, c, upsert("writer-C", "sync-C", topic, "C tie content (highest writer)", 2, tie))

	// Push in order A, B, C → canonical = sync-A; B and C contest the tie.
	for _, n := range []*spike.Node{a, b, c} {
		if _, err := spike.Push(ctx, n, central); err != nil {
			t.Fatalf("push %s: %v", n.Name, err)
		}
	}

	// Drive DIFFERENT pull orders deliberately:
	//   round 1: C, A, B
	//   round 2: B, C, A
	//   round 3: A, B, C
	orders := [][]*spike.Node{
		{c, a, b},
		{b, c, a},
		{a, b, c},
	}
	for ri, order := range orders {
		for _, n := range order {
			if _, err := spike.Pull(ctx, n, central, project); err != nil {
				t.Fatalf("round %d pull %s: %v", ri+1, n.Name, err)
			}
		}
	}

	aRec := liveTopicOnNode(t, a, topic)
	bRec := liveTopicOnNode(t, b, topic)
	cRec := liveTopicOnNode(t, c, topic)
	ctrRec := liveTopicOnCentral(t, central, topic)

	if aRec == nil || bRec == nil || cRec == nil || ctrRec == nil {
		t.Fatalf("missing live row: A=%v B=%v C=%v central=%v",
			aRec != nil, bRec != nil, cRec != nil, ctrRec != nil)
	}

	t.Logf("FINAL content: A=%q B=%q C=%q central=%q",
		aRec.Content, bRec.Content, cRec.Content, ctrRec.Content)

	want := "C tie content (highest writer)"
	for _, tc := range []struct {
		name string
		rec  string
	}{
		{"A", aRec.Content},
		{"B", bRec.Content},
		{"C", cRec.Content},
		{"central", ctrRec.Content},
	} {
		if tc.rec != want {
			t.Errorf("CONTENT DIVERGENCE on %s: %q; want %q (writer-C wins the tie regardless of pull order)",
				tc.name, tc.rec, want)
		}
	}
}

// ── Probe: same-writer delete-vs-upsert at the exact tie (mutation_id decides) ─

// TestConvergence_SameWriterDeleteVsUpsert_MutationIDTiebreaker tests the FINAL
// mutation_id tiebreaker level: the same writer_id issues BOTH a delete (sync-DEL)
// and an upsert (sync-UP) for the same topic at the EXACT same (updated_at,
// version). writer_id ties, so the FINAL tiebreaker is the WINNING mutation's
// content-addressed mutation_id (NOT the canonical PK sync_id). The two
// mutation_ids are computed deterministically and the winner is whichever is
// lexically higher — so the test asserts convergence AND the mutation_id-decided
// outcome, regardless of which way the hashes happen to sort. The central
// ordering is forced: delete pushes FIRST (central tombstones), then the upsert
// arrives and supersedes the tombstone iff its mutation_id is higher.
func TestConvergence_SameWriterDeleteVsUpsert_MutationIDTiebreaker(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")

	topic := "sdd/test/adv-same-writer-tie"

	tInit := base.Add(10 * time.Second)
	tie := base.Add(40 * time.Second)

	const writer = "writer-S"
	const upSync = "sync-UP"
	const delSync = "sync-DEL"
	const upContent = "UP contests the tie"

	// Establish a live row first (so the delete has something to tombstone on central).
	mustWrite(t, a, upsert(writer, delSync, topic, "initial", 1, tInit))
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)
	if liveTopicOnCentral(t, central, topic) == nil {
		t.Fatalf("precondition: central must have live T")
	}

	// Build the two contesting writes (same writer, exact tie, different sync_id).
	upMut := upsert(writer, upSync, topic, upContent, 2, tie)
	delMut := del(writer, delSync, topic, 2, tie)

	// Compute the deciding mutation_ids (content-addressed) so the assertion is
	// exact regardless of how the hashes sort.
	upID := mutation.NewMutationID(mutation.CanonicalPayload(upMut))
	delID := mutation.NewMutationID(mutation.CanonicalPayload(delMut))
	upsertWins := upID > delID
	t.Logf("mutation_ids: upsert=%s delete=%s → upsert %s win",
		upID, delID, map[bool]string{true: "SHOULD", false: "must NOT"}[upsertWins])

	mustWrite(t, b, upMut)
	mustWrite(t, a, delMut)

	// Force ordering: delete pushes FIRST (central tombstones), upsert SECOND.
	if _, err := spike.Push(ctx, a, central); err != nil {
		t.Fatalf("push A (delete): %v", err)
	}
	sDel := maxCentralSeq(ctx, t, central)
	if _, err := spike.Push(ctx, b, central); err != nil {
		t.Fatalf("push B (upsert): %v", err)
	}
	sUp := maxCentralSeq(ctx, t, central)

	t.Logf("central seqs: S_del=%d (delete), S_up=%d (upsert)", sDel, sUp)
	if !(sDel < sUp) {
		t.Fatalf("FORCED-ORDERING NOT ACHIEVED: need S_del < S_up, got %d >= %d", sDel, sUp)
	}

	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 4)

	aRec := liveTopicOnNode(t, a, topic)
	bRec := liveTopicOnNode(t, b, topic)
	ctrRec := liveTopicOnCentral(t, central, topic)
	aLive := aRec != nil
	bLive := bRec != nil
	cLive := ctrRec != nil

	t.Logf("FINAL: A.live=%v B.live=%v central.live=%v", aLive, bLive, cLive)
	if ctrRec != nil {
		t.Logf("central content=%q", ctrRec.Content)
	}

	if !(aLive == bLive && bLive == cLive) {
		t.Errorf("SPLIT-BRAIN: A=%v B=%v central=%v (same-writer tie, mutation_id must decide identically)",
			aLive, bLive, cLive)
	}
	// The winner is decided by the replica-identical mutation_id order.
	if aLive != upsertWins || bLive != upsertWins || cLive != upsertWins {
		t.Errorf("WRONG STATE: higher mutation_id must decide the same-writer tie (upsertWins=%v); "+
			"got live A=%v B=%v central=%v", upsertWins, aLive, bLive, cLive)
	}
	if upsertWins && cLive && ctrRec.Content != upContent {
		t.Errorf("central content=%q, want upsert content %q", ctrRec.Content, upContent)
	}
}

// ── Probe: equal-writer-id, different-sync_id tie, canonical under third sync_id ─

// TestConvergence_EqualWriterMutationIDTiebreak_CanonicalUnderThirdSyncID stacks
// cross-writer topic convergence with equal-writer FINAL-tier tiebreaking. Three
// nodes use the SAME writer_id but DIFFERENT sync_ids, contesting the same topic
// at an exact (updated_at, version) tie. The canonical central row is under a
// THIRD sync_id (the first pushed). With writer_id tied, the FINAL tiebreaker is
// the WINNING mutation's content-addressed mutation_id (NOT the canonical PK
// sync_id): the three mutation_ids are computed deterministically and ALL stores
// must converge to the content of whichever write has the highest mutation_id.
func TestConvergence_EqualWriterMutationIDTiebreak_CanonicalUnderThirdSyncID(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")
	c := newNode(t, "C")

	topic := "sdd/test/adv-equalwriter-syncid-tie"
	tie := base.Add(40 * time.Second)

	const writer = "writer-EQ" // identical writer_id across all three

	// Three contesting writes (same writer, exact tie, distinct sync_id + content).
	m0 := upsert(writer, "sync-M0", topic, "M0 content", 2, tie)
	m1 := upsert(writer, "sync-M1", topic, "M1 content", 2, tie)
	m2 := upsert(writer, "sync-M2", topic, "M2 content", 2, tie)

	// The replica-identical winner is the highest mutation_id, NOT the highest sync_id.
	type cand struct {
		id, content string
	}
	cands := []cand{
		{mutation.NewMutationID(mutation.CanonicalPayload(m0)), m0.Content},
		{mutation.NewMutationID(mutation.CanonicalPayload(m1)), m1.Content},
		{mutation.NewMutationID(mutation.CanonicalPayload(m2)), m2.Content},
	}
	winner := cands[0]
	for _, cd := range cands[1:] {
		if cd.id > winner.id {
			winner = cd
		}
	}
	t.Logf("mutation_ids: M0=%s M1=%s M2=%s → winner content=%q",
		cands[0].id, cands[1].id, cands[2].id, winner.content)

	mustWrite(t, a, m0)
	mustWrite(t, b, m1)
	mustWrite(t, c, m2)

	// A pushes first → sync-M0 canonical. Then B, then C.
	for _, n := range []*spike.Node{a, b, c} {
		if _, err := spike.Push(ctx, n, central); err != nil {
			t.Fatalf("push %s: %v", n.Name, err)
		}
	}

	syncRounds(ctx, t, []*nodeRef{{a}, {b}, {c}}, central, 5)

	aRec := liveTopicOnNode(t, a, topic)
	bRec := liveTopicOnNode(t, b, topic)
	cRec := liveTopicOnNode(t, c, topic)
	ctrRec := liveTopicOnCentral(t, central, topic)

	if aRec == nil || bRec == nil || cRec == nil || ctrRec == nil {
		t.Fatalf("missing live row: A=%v B=%v C=%v central=%v",
			aRec != nil, bRec != nil, cRec != nil, ctrRec != nil)
	}

	t.Logf("FINAL content: A=%q B=%q C=%q central=%q",
		aRec.Content, bRec.Content, cRec.Content, ctrRec.Content)

	for _, tc := range []struct {
		name, rec string
	}{
		{"A", aRec.Content}, {"B", bRec.Content}, {"C", cRec.Content}, {"central", ctrRec.Content},
	} {
		if tc.rec != winner.content {
			t.Errorf("CONTENT DIVERGENCE on %s: %q; want %q (equal-writer → highest mutation_id wins)",
				tc.name, tc.rec, winner.content)
		}
	}
	if ctrRec.SyncID != "sync-M0" {
		t.Logf("note: central canonical sync_id=%q (expected sync-M0 — first pushed)", ctrRec.SyncID)
	}
}

var _ = time.Second
