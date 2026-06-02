//go:build acceptance

package spike_test

import (
	"context"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// SPEC-CORRECT VERIFICATION — tombstone seq tiebreaker at the exact-tie boundary.
//
// Spec rule (reconciliation spec.md:117-122):
//
//   When updated_at and version are EQUAL, the seq tiebreaker decides:
//   - incoming.seq > tombstone.seq → upsert SUPERSEDES (delete revived; NOT NoOp).
//   - incoming.seq <= tombstone.seq → upsert BLOCKED (NoOp).
//
// This test verifies that scenario from spec.md:117-122 holds end-to-end across
// all three stores: when a competing upsert's central seq is genuinely HIGHER
// than the tombstone's seq, the row MUST resurrect (the tombstone is superseded).
//
// Why resurrection is SPEC-CORRECT here (not a bug):
//
//   The upsert (pushed by B at a positive central seq) arrives with a seq that is
//   strictly greater than the delete's seq (the tombstone's seq). Under the spec
//   tiebreaker chain, m.Seq > ts.Seq → writeWins returns true → the tombstone is
//   superseded and the row is revived. This is the intended outcome.
//
// ts.Seq is now WIRED: Tombstone.Seq is populated from the persistent stores
// (local memory_tombstones.seq, central central_tombstones.seq) and domain.Decide
// passes ts.Seq to writeWins instead of the former hardcoded 0. The resurrection
// outcome is UNCHANGED because the competing upsert's seq is still the higher of
// the two — wiring ts.Seq confirms the spec is implemented, not merely tolerated.
//
// The lower-seq-blocked branch (spec.md:124-129) cannot be produced via the
// push/pull cycle used here: push always assigns a fresh, higher BIGSERIAL seq
// for the upsert. That branch is pinned deterministically by the pure domain unit
// test TestDecide_TombstoneTieBreak_SeqAuthority/lower_seq_blocked_by_tombstone.
//
// NOTE on realism: an exact (updated_at, version) tie across two DISTINCT writers
// is astronomically unlikely in production (updated_at is nanosecond wall-clock).
// This probe manufactures it on purpose to verify the tiebreaker boundary precisely.
// ─────────────────────────────────────────────────────────────────────────────
func TestTsSeq_EqualTie_HigherSeqRevives(t *testing.T) {
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
	//    lands on A.local, B.local AND central — each tombstone now carries the
	//    delete's central seq (the seq column is wired).
	mustWrite(t, a, del("writer-A", "sync-A", topic, 2, tie))
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)
	assertDeletedEverywhere(ctx, t, a, b, central, topic)

	// 3. B upserts T at the EXACT SAME instant (tie) with the SAME version (2).
	//    updated_at == tombstone.deleted_at AND version == tombstone.version, so
	//    only the seq tiebreaker can separate them. B's upsert gets a HIGHER
	//    central seq than the tombstone (push assigns a fresh BIGSERIAL), so
	//    writeWins(m.Seq > ts.Seq) returns true → resurrection is spec-correct.
	mustWrite(t, b, upsert("writer-B", "sync-B", topic, "B equal-tie upsert", 2, tie))
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 3)

	// SPEC-CORRECT OUTCOME (spec.md:117-122): the exact-tie upsert MUST revive the
	// row everywhere because its seq is strictly higher than the tombstone's seq.
	// Failing to revive is the REAL bug — a higher-seq upsert that cannot supersede
	// a lower-seq tombstone violates the spec tiebreaker.
	t.Logf("ts.Seq VERIFIED: on an exact (updated_at,version) tie at the tombstone "+
		"boundary, topic %q resurrects on all three stores because B's upsert seq "+
		"is strictly higher than the delete's tombstone seq (spec.md:117-122). "+
		"ts.Seq is now wired; this is the spec-correct outcome, not a pinned bug.", topic)

	for _, where := range []struct {
		name string
		rec  bool // true == a live row exists
	}{
		{"A.local", liveTopicOnNode(t, a, topic) != nil},
		{"B.local", liveTopicOnNode(t, b, topic) != nil},
		{"central", liveTopicOnCentral(t, central, topic) != nil},
	} {
		if !where.rec {
			t.Errorf("spec.md:117-122 VIOLATION on %s: higher-seq upsert did NOT "+
				"resurrect the row after exact (updated_at,version) tie. "+
				"A higher-seq upsert MUST supersede a lower-seq tombstone per spec. "+
				"Investigate ts.Seq wiring in domain.Decide / tombstone persistence.", where.name)
		}
	}

	// Belt-and-suspenders: confirm convergence is CONSISTENT across all three
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
