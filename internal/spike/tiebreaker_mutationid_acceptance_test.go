//go:build acceptance

package spike_test

// ─────────────────────────────────────────────────────────────────────────────
// CROWN-JEWEL REGRESSION GUARD — final LWW tiebreaker must be replica-identical.
//
// Codex HIGH finding: the final LWW tier compared the canonical PK sync_id
// (m.SyncID vs cur/ts.SyncID). The cur/ts side sync_id is the row's PRIMARY KEY,
// set at first-insert and NEVER changed on in-place updates/tombstones — only
// writer_id/updated_at/version are overwritten with the winning write's values.
// Because topic_key (not sync_id) is the convergence key, two replicas can hold
// the SAME topic with the SAME converged content under DIFFERENT stored PK
// sync_ids. At an exact (updated_at, version, writer_id) tie, comparing the
// incoming write's sync_id against each replica's divergent stored PK sync_id
// resolves DIFFERENTLY per replica → split-brain.
//
// This probe constructs exactly that divergent-PK precondition and then issues a
// third write that ties on (updated_at, version, writer_id) and whose sync_id
// sorts BETWEEN the two replicas' stored PK sync_ids. Under the OLD sync_id
// tiebreaker the write wins on one replica and loses on the other → divergence.
// Under the FIX (final tier = the winning mutation's content-addressed
// mutation_id, carried by last_write_mutation_id) both replicas compare the SAME
// pair of mutation_ids and converge.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/mutation"
	"github.com/mariesqu/engram/internal/spike"
)

// localCanonicalSyncID returns the stored PRIMARY-KEY sync_id of the row backing
// a topic on a node's local store (regardless of liveness). This exposes the
// divergent-PK precondition: the value is the FIRST-inserted sync_id, not the
// winning write's sync_id.
func localCanonicalSyncID(t *testing.T, n *spike.Node, topic string) string {
	t.Helper()
	var got string
	if err := n.Store.DB().QueryRow(
		`SELECT sync_id FROM memories
		   WHERE topic_key=? AND project=? AND scope=?
		   ORDER BY sync_id LIMIT 1`,
		topic, project, scope,
	).Scan(&got); err != nil {
		t.Fatalf("localCanonicalSyncID %s (%q): %v", n.Name, topic, err)
	}
	return got
}

// TestConvergence_FinalTiebreaker_DivergentStoredPK_MutationIDConverges is the
// crown-jewel guard for the sync_id→mutation_id tiebreaker fix.
//
// Phase 1 — build the divergent stored-PK precondition:
//
//	Node A inserts sync-A (writer-A) FIRST, then writer-B's NEWER write (sync-B)
//	WINS and updates A's row IN PLACE → A's stored PK = sync-A, content = B's.
//	Node B inserts sync-B (writer-B) FIRST, then writer-A's OLDER write LOSES
//	(NoOp) → B's stored PK = sync-B, content = B's.
//	Both replicas now hold the SAME topic + SAME content under DIFFERENT PKs.
//
// Phase 2 — the exact-tie probe write:
//
//	A THIRD write from writer-B (SAME writer_id as the current winner) at the
//	EXACT same (updated_at, version) as B's winning write, sync_id "sync-AM"
//	(sorts strictly BETWEEN sync-A and sync-B) and DIFFERENT content.
//
// Under the OLD code this DIVERGES: on A, sync-AM > sync-A → third write wins
// (A flips to the new content); on B, sync-AM < sync-B → third write loses
// (B keeps B's content). Under the FIX both replicas compare mutation_ids and
// converge identically.
func TestConvergence_FinalTiebreaker_DivergentStoredPK_MutationIDConverges(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")
	nodes := []*nodeRef{{a}, {b}}

	topic := "sdd/test/final-tiebreaker-divergent-pk"

	tOlder := base.Add(10 * time.Second) // writer-A authors (older)
	tWinner := base.Add(40 * time.Second) // writer-B authors (newer) — wins content

	// sync_id lexical order: "sync-A" < "sync-AM" < "sync-B".
	const (
		syncA  = "sync-A"  // writer-A's identity (canonical PK on A)
		syncB  = "sync-B"  // writer-B's identity (canonical PK on B)
		syncAM = "sync-AM" // probe write — sorts BETWEEN sync-A and sync-B
	)

	// ── Phase 1a: A inserts sync-A first (older), pushes so sync-A is central canonical.
	mustWrite(t, a, upsert("writer-A", syncA, topic, "A content (older)", 1, tOlder))
	if _, err := spike.Push(ctx, a, central); err != nil {
		t.Fatalf("push A (sync-A): %v", err)
	}

	// ── Phase 1b: B inserts sync-B first (newer) — B's OWN row is sync-B locally.
	mustWrite(t, b, upsert("writer-B", syncB, topic, "B content (winner)", 2, tWinner))

	// Settle: writer-B's newer write wins content everywhere; A's stored PK stays
	// sync-A (updated in place), B's stored PK stays sync-B (its own first insert).
	syncRounds(ctx, t, nodes, central, 3)

	aPK := localCanonicalSyncID(t, a, topic)
	bPK := localCanonicalSyncID(t, b, topic)
	aRec := liveTopicOnNode(t, a, topic)
	bRec := liveTopicOnNode(t, b, topic)
	if aRec == nil || bRec == nil {
		t.Fatalf("precondition: topic must be live on A and B (A=%v B=%v)", aRec != nil, bRec != nil)
	}
	t.Logf("PRECONDITION: A.pk=%s B.pk=%s | content A=%q B=%q",
		aPK, bPK, aRec.Content, bRec.Content)

	// Assert the divergent stored-PK precondition the bug depends on.
	if aPK != syncA {
		t.Fatalf("precondition NOT met: A stored PK = %q, want %q (in-place update keeps first-insert PK)", aPK, syncA)
	}
	if bPK != syncB {
		t.Fatalf("precondition NOT met: B stored PK = %q, want %q (B keeps its own first-insert PK)", bPK, syncB)
	}
	// Both replicas converged on writer-B's content.
	const winnerContent = "B content (winner)"
	if aRec.Content != winnerContent || bRec.Content != winnerContent {
		t.Fatalf("precondition NOT met: A=%q B=%q, want both %q", aRec.Content, bRec.Content, winnerContent)
	}

	// ── Phase 2: the exact-tie probe write.
	// writer-B again (ties writer_id with the current winner), EXACT same
	// (updated_at, version) as B's winning write (tWinner, v=2), sync-AM (between
	// sync-A and sync-B), DIFFERENT content. Author it on B and propagate.
	probe := upsert("writer-B", syncAM, topic, "PROBE content (tie, sync-AM)", 2, tWinner)
	mustWrite(t, b, probe)

	// Compute the deciding mutation_ids so the assertion is precise regardless of
	// which way the content-addressed hashes happen to sort.
	winnerMut := upsert("writer-B", syncB, topic, winnerContent, 2, tWinner)
	winnerID := mutation.NewMutationID(mutation.CanonicalPayload(winnerMut))
	probeID := mutation.NewMutationID(mutation.CanonicalPayload(probe))
	probeShouldWin := probeID > winnerID
	t.Logf("mutation_ids: winner=%s probe=%s → probe %s win",
		winnerID, probeID, map[bool]string{true: "SHOULD", false: "must NOT"}[probeShouldWin])

	syncRounds(ctx, t, nodes, central, 4)

	aFinal := liveTopicOnNode(t, a, topic)
	bFinal := liveTopicOnNode(t, b, topic)
	cFinal := liveTopicOnCentral(t, central, topic)
	if aFinal == nil || bFinal == nil || cFinal == nil {
		t.Fatalf("post-probe: topic must be live everywhere (A=%v B=%v central=%v)",
			aFinal != nil, bFinal != nil, cFinal != nil)
	}

	t.Logf("FINAL content: A=%q B=%q central=%q (A.pk=%s B.pk=%s)",
		aFinal.Content, bFinal.Content, cFinal.Content,
		localCanonicalSyncID(t, a, topic), localCanonicalSyncID(t, b, topic))

	// The crown-jewel assertion: all stores converge on the SAME content. Under the
	// old sync_id tiebreaker A and B split (A flips to PROBE because sync-AM>sync-A;
	// B keeps winner because sync-AM<sync-B). Under the fix they agree via mutation_id.
	if aFinal.Content != bFinal.Content || bFinal.Content != cFinal.Content {
		t.Fatalf("SPLIT-BRAIN at exact tie: content diverged — A=%q B=%q central=%q "+
			"(canonical-PK sync_id tiebreaker decided differently across replicas)",
			aFinal.Content, bFinal.Content, cFinal.Content)
	}

	// And the winner is the one the replica-identical mutation_id order picks.
	wantContent := winnerContent
	if probeShouldWin {
		wantContent = "PROBE content (tie, sync-AM)"
	}
	if cFinal.Content != wantContent {
		t.Errorf("converged on %q; want %q (higher mutation_id wins the exact tie)",
			cFinal.Content, wantContent)
	}
}
