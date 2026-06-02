//go:build acceptance

package spike_test

import (
	"context"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// EMPIRICAL ts.Seq PROBE — the tombstone-boundary tie case.
//
// The six convergence invariants (INV1–INV6) all use STRICTLY DISTINCT
// UpdatedAt values, so writeWins() decides every contest on wall-clock alone and
// the seq tiebreaker is never reached. This probe deliberately drives the ONE
// case where the seq tiebreaker COULD matter at the tombstone boundary: a pulled
// upsert whose UpdatedAt and Version EXACTLY equal the tombstone's.
//
// Mechanics under test (the reason ts.Seq is "not wired" locally):
//   - The local memory_tombstones table has NO seq column.
//   - domain.Decide calls writeWins(m, ts.DeletedAt, ts.Version, /*curSeq=*/0)
//     at the tombstone guard — the tombstone seq is hardcoded 0.
//   - A pulled mutation carries a POSITIVE central seq.
//   - So when UpdatedAt and Version tie, writeWins falls through to
//     (m.Seq > 0) == true → the tombstone is treated as superseded.
//
// This probe RECORDS and PINS the actual end-state under that tie. The empirical
// result (verified by running it): under an EXACT (updated_at, version) tie the
// row RESURRECTS on A.local, B.local AND central. Root cause:
//
//	the local memory_tombstones row has no seq; Decide passes curSeq=0 to
//	writeWins; the pulled upsert carries a POSITIVE central seq; with
//	updated_at and version tied, writeWins returns (m.Seq > 0) == true → the
//	tombstone is treated as superseded and the row revives.
//
// SCOPE VERDICT (the reason this test ASSERTS the observed resurrection rather
// than failing):
//
//   - The SIX core convergence invariants (INV1–INV6) all use STRICTLY DISTINCT
//     updated_at values and ALL PASS green. For them, the seq tiebreaker is never
//     reached, so ts.Seq is empirically NOT required.
//   - ts.Seq wiring is ONLY needed to close THIS exact-(updated_at,version)-tie
//     tombstone boundary — an edge case OUTSIDE the six invariants and outside
//     this spike's scope. Wiring it (adding a seq column to memory_tombstones and
//     threading it into Decide) is the documented post-spike follow-up.
//
// This test therefore PINS the current observed behavior as a regression sentinel
// for that follow-up: it stays green today, and will FAIL LOUDLY the moment a
// future change wires ts.Seq and flips the tie outcome — at which point the
// assertion below should be inverted to require the row to STAY deleted.
//
// NOTE on realism: an exact (updated_at, version) tie across two DISTINCT writers
// is astronomically unlikely in production (updated_at is nanosecond wall-clock).
// This probe manufactures it on purpose to map the boundary precisely.
// ─────────────────────────────────────────────────────────────────────────────
func TestTsSeqProbe_EqualTimestampTombstoneTie(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")

	topic := "sdd/test/tsseq-tie"

	// Fix ONE instant used for BOTH the delete and the competing stale upsert so
	// updated_at ties exactly. Version is also held equal (both v2) to force the
	// contest all the way down to the seq tiebreaker.
	tie := base.Add(50 * time.Second)

	// 1. A writes T (older than the tie), converge.
	mustWrite(t, a, upsert("writer-A", "sync-A", topic, "A original", 1, base.Add(10*time.Second)))
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)

	// 2. A deletes T at the tie instant with version 2. Converge so the tombstone
	//    lands on A.local, B.local AND central.
	mustWrite(t, a, del("writer-A", "sync-A", topic, 2, tie))
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)
	assertDeletedEverywhere(ctx, t, a, b, central, topic)

	// 3. B upserts T at the EXACT SAME instant (tie) with the SAME version (2).
	//    updated_at == tombstone.deleted_at AND version == tombstone.version, so
	//    only the seq tiebreaker can separate them. The local tombstone seq is 0;
	//    the pulled mutation carries a positive central seq.
	mustWrite(t, b, upsert("writer-B", "sync-B", topic, "B equal-tie upsert", 2, tie))
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 3)

	// PINNED OBSERVED BEHAVIOR (ts.Seq NOT wired): the exact-tie upsert revives the
	// row everywhere. We assert that observed outcome so the test is a green
	// regression sentinel; the log states the precise finding for the reader.
	t.Logf("ts.Seq EMPIRICAL FINDING: on an exact (updated_at,version) tie at the "+
		"tombstone boundary, topic %q RESURRECTS on all three stores because the "+
		"local tombstone seq is 0 and the pulled upsert carries a positive central "+
		"seq (writeWins falls through to m.Seq>0). The six core invariants avoid "+
		"this by using distinct updated_at, so ts.Seq wiring is NOT required for "+
		"them; closing THIS tie is the documented post-spike follow-up.", topic)

	for _, where := range []struct {
		name string
		rec  bool // true == a live row exists
	}{
		{"A.local", liveTopicOnNode(t, a, topic) != nil},
		{"B.local", liveTopicOnNode(t, b, topic) != nil},
		{"central", liveTopicOnCentral(t, central, topic) != nil},
	} {
		if !where.rec {
			t.Errorf("ts.Seq SENTINEL CHANGED: %s no longer resurrects on an exact "+
				"(updated_at,version) tie. The boundary behavior flipped — ts.Seq may "+
				"now be wired. UPDATE this probe to require the row to STAY deleted "+
				"(the spec-correct outcome) and remove this sentinel.", where.name)
		}
	}

	// Belt-and-suspenders: confirm convergence is still CONSISTENT across all three
	// stores even in the tie case (all agree the row is live — no split-brain where
	// one store revives and another stays deleted).
	aLive := liveTopicOnNode(t, a, topic) != nil
	bLive := liveTopicOnNode(t, b, topic) != nil
	cLive := liveTopicOnCentral(t, central, topic) != nil
	if !(aLive == bLive && bLive == cLive) {
		t.Errorf("ts.Seq SPLIT-BRAIN: stores disagree on liveness under the tie "+
			"(A=%v B=%v central=%v) — convergence itself broke, not just the tie policy",
			aLive, bLive, cLive)
	}
}

var _ = time.Second
