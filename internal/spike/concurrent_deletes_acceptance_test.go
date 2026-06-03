//go:build acceptance

package spike_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/spike"
)

// ─────────────────────────────────────────────────────────────────────────────
// REGRESSION GUARD — concurrent-delete tombstone metadata convergence.
//
// Bug (before fix): when cur == nil (no live row) and ts != nil (a tombstone
// already exists for the topic identity), the OpDelete gate in domain.Decide
// was skipped entirely. The adapter's INSERT OR REPLACE then OVERWROTE the
// existing tombstone's (deleted_at, version, deleted_by) unconditionally —
// last-applied-wins, not last-write-wins. This caused:
//
//   - Order-dependent tombstone metadata divergence: two nodes applying the
//     same two deletes in opposite orders ended up with different (deleted_at,
//     version, deleted_by) on the tombstone.
//   - Live/deleted split-brain: a subsequent upsert whose updated_at fell
//     BETWEEN the two delete timestamps was blocked on one node (the tombstone
//     showed the newer delete) but revived on the other (the stale delete had
//     overwritten the newer tombstone), leaving the topic alive on one store and
//     deleted on another permanently.
//
// Fix: extend the OpDelete gate to also compete via writeWins against an
// existing tombstone when cur == nil. A delete that loses writeWins vs the
// existing tombstone is a NoOp — it leaves the newer tombstone intact.
// Unconditional tombstoning applies ONLY when there is NEITHER a live row NOR
// a tombstone (first delete of this identity).
// ─────────────────────────────────────────────────────────────────────────────

// localTombstoneMeta reads the (deleted_at, version, deleted_by) fields from
// the local memory_tombstones row for a given topic. Returns zero values when no
// tombstone exists for the topic on this node.
func localTombstoneMeta(t *testing.T, n *spike.Node, topic string) (deletedAt time.Time, version int, deletedBy string) {
	t.Helper()
	row := n.Store.DB().QueryRow(
		`SELECT deleted_at, version, deleted_by
		   FROM memory_tombstones
		  WHERE topic_key=? AND project=? AND scope=?
		  LIMIT 1`,
		topic, project, scope,
	)
	var da string
	if err := row.Scan(&da, &version, &deletedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// No tombstone for this topic on this node → zero values.
			return time.Time{}, 0, ""
		}
		t.Fatalf("localTombstoneMeta: scan tombstone for topic %q: %v", topic, err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, da)
	if err != nil {
		t.Fatalf("localTombstoneMeta: parse deleted_at %q for topic %q: %v", da, topic, err)
	}
	return parsed, version, deletedBy
}

// TestConvergence_ConcurrentDeletes_TombstoneMetadataConverges is the
// regression guard for the "concurrent deletes regress a newer tombstone"
// split-brain.
//
// Two writers delete the SAME topic at DIFFERENT updated_at values (tA < tB).
// The probe applies them in OPPOSITE orders on two distinct local nodes (A
// applies older-then-newer; B applies newer-then-older) by driving Push/Pull
// manually.  Assertions:
//
//  1. Tombstone metadata (deleted_at, deleted_by) is IDENTICAL on A.local,
//     B.local, and central after full convergence (the newer delete's metadata
//     wins everywhere).
//
//  2. A subsequent upsert whose updated_at falls STRICTLY BETWEEN tA and tB
//     resolves to the SAME liveness on all three stores: it must be BLOCKED
//     everywhere (because the winning tombstone at tB is newer than the upsert's
//     timestamp). No live/deleted split-brain.
func TestConvergence_ConcurrentDeletes_TombstoneMetadataConverges(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")

	topic := "sdd/test/concurrent-deletes-tombstone-meta"

	// Timeline:
	//   tInit  — initial live write (writer-A)
	//   tA     — writer-A deletes (OLDER)
	//   tBetween — between the two deletes (for the resurrection probe below)
	//   tB     — writer-B deletes (NEWER) ← should WIN the tombstone everywhere
	tInit := base.Add(10 * time.Second)
	tA := base.Add(40 * time.Second)    // older delete
	tBetween := base.Add(60 * time.Second) // falls between tA and tB
	tB := base.Add(80 * time.Second)    // newer delete → must win

	// ── Step 1: establish a live row on all three stores ───────────────────────
	mustWrite(t, a, upsert("writer-A", "sync-A", topic, "initial content", 1, tInit))
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)
	if liveTopicOnNode(t, a, topic) == nil {
		t.Fatalf("precondition: topic %q not live on A after initial sync", topic)
	}
	if liveTopicOnNode(t, b, topic) == nil {
		t.Fatalf("precondition: topic %q not live on B after initial sync", topic)
	}
	if liveTopicOnCentral(t, central, topic) == nil {
		t.Fatalf("precondition: topic %q not live on central after initial sync", topic)
	}

	// ── Step 2: author the two competing deletes ───────────────────────────────
	// writer-A deletes at tA (OLDER, version 2).
	mustWrite(t, a, del("writer-A", "sync-A", topic, 2, tA))
	// writer-B deletes at tB (NEWER, version 3) — this is the WINNING delete.
	mustWrite(t, b, del("writer-B", "sync-B", topic, 3, tB))

	// ── Step 3: push in order A-first, B-second ────────────────────────────────
	// Central: A's older delete tombstones first (central tombstone = tA/writer-A).
	// Then B's newer delete arrives — with the fix it must WIN the gate vs the
	// existing tombstone and overwrite to tB/writer-B.
	if _, err := spike.Push(ctx, a, central); err != nil {
		t.Fatalf("push A (older delete): %v", err)
	}
	sA := maxCentralSeq(ctx, t, central)

	if _, err := spike.Push(ctx, b, central); err != nil {
		t.Fatalf("push B (newer delete): %v", err)
	}
	sB := maxCentralSeq(ctx, t, central)

	t.Logf("central seqs: S_A=%d (older delete tA), S_B=%d (newer delete tB)", sA, sB)
	if !(sA < sB) {
		t.Fatalf("forced ordering not achieved: need S_A < S_B, got %d >= %d", sA, sB)
	}

	// ── Step 4: A pulls first (gets newer delete in sequence), B pulls ─────────
	// A.local: older delete already applied; now pulls B's newer delete.
	//   With the fix: the gate compares writeWins(newer vs existing tombstone) =
	//   true → overwrites → A.local tombstone = tB/writer-B.
	// B.local: newer delete already applied; pulls A's older delete.
	//   With the fix: writeWins(older vs existing newer tombstone) = false → NoOp
	//   → B.local tombstone stays tB/writer-B.
	if _, err := spike.Pull(ctx, a, central, project); err != nil {
		t.Fatalf("pull A: %v", err)
	}
	if _, err := spike.Pull(ctx, b, central, project); err != nil {
		t.Fatalf("pull B: %v", err)
	}

	// Extra settle rounds to catch any cascading divergence.
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 3)

	// ── Step 5: assert tombstone metadata convergence ──────────────────────────
	aDA, aVer, aBy := localTombstoneMeta(t, a, topic)
	bDA, bVer, bBy := localTombstoneMeta(t, b, topic)

	t.Logf("A.local tombstone: deleted_at=%v version=%d deleted_by=%s", aDA, aVer, aBy)
	t.Logf("B.local tombstone: deleted_at=%v version=%d deleted_by=%s", bDA, bVer, bBy)

	if aDA.IsZero() {
		t.Fatalf("A.local: no tombstone found for topic %q after convergence", topic)
	}
	if bDA.IsZero() {
		t.Fatalf("B.local: no tombstone found for topic %q after convergence", topic)
	}

	// METADATA CONVERGENCE: both nodes must hold the NEWER delete's metadata.
	if !aDA.Equal(bDA) {
		t.Errorf("TOMBSTONE METADATA DIVERGENCE: A.local deleted_at=%v, B.local deleted_at=%v "+
			"(want identical tB=%v on both — newer delete must win)", aDA, bDA, tB)
	}
	if aBy != bBy {
		t.Errorf("TOMBSTONE METADATA DIVERGENCE: A.local deleted_by=%q, B.local deleted_by=%q "+
			"(want 'writer-B' on both — newer delete must win)", aBy, bBy)
	}
	if aVer != bVer {
		t.Errorf("TOMBSTONE METADATA DIVERGENCE: A.local version=%d, B.local version=%d "+
			"(want version=3 on both — newer delete must win)", aVer, bVer)
	}

	// The winning tombstone must carry the NEWER delete's metadata.
	if !aDA.Equal(tB) {
		t.Errorf("WRONG TOMBSTONE WINNER on A: deleted_at=%v, want tB=%v (newer delete)", aDA, tB)
	}
	if aBy != "writer-B" {
		t.Errorf("WRONG TOMBSTONE WINNER on A: deleted_by=%q, want 'writer-B'", aBy)
	}
	if aVer != 3 {
		t.Errorf("WRONG TOMBSTONE WINNER on A: version=%d, want 3", aVer)
	}

	// ── Step 6: resurrection probe ─────────────────────────────────────────────
	// Author a new node (C) that upserts the topic at tBetween (between tA and tB).
	// This upsert is OLDER than the winning tombstone (tB) → must be BLOCKED on ALL
	// stores (no live/deleted split-brain).
	c := newNode(t, "C")
	mustWrite(t, c, upsert("writer-C", "sync-C", topic, "C resurrection attempt (tBetween)", 4, tBetween))
	syncRounds(ctx, t, []*nodeRef{{a}, {b}, {c}}, central, 4)

	aLive := liveTopicOnNode(t, a, topic) != nil
	bLive := liveTopicOnNode(t, b, topic) != nil
	cLive := liveTopicOnNode(t, c, topic) != nil
	ctrLive := liveTopicOnCentral(t, central, topic) != nil

	t.Logf("resurrection probe FINAL: A=%v B=%v C=%v central=%v", aLive, bLive, cLive, ctrLive)

	if !(aLive == bLive && bLive == cLive && cLive == ctrLive) {
		t.Errorf("LIVE/DELETED SPLIT-BRAIN: upsert at tBetween resolved differently across stores — "+
			"A=%v B=%v C=%v central=%v.\n"+
			"Root cause: a stale older delete overwrote the newer tombstone's metadata on one node "+
			"via unconditional INSERT OR REPLACE, making the tBetween upsert appear to WIN the "+
			"tombstone guard on that node (the overwritten tombstone showed tA < tBetween) while "+
			"losing on the other node (tombstone correctly shows tB > tBetween).",
			aLive, bLive, cLive, ctrLive)
	}
	if aLive || bLive || cLive || ctrLive {
		t.Errorf("WRONG STATE: upsert at tBetween (%v) must be BLOCKED by the winning tombstone at "+
			"tB (%v); got A=%v B=%v C=%v central=%v", tBetween, tB, aLive, bLive, cLive, ctrLive)
	}
}
