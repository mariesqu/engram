//go:build acceptance

package spike_test

import (
	"context"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// SPEC-CORRECT VERIFICATION — identity tiebreaker at the exact-tie boundary.
//
// Path Z rule (writeWins final tiebreaker):
//   When updated_at and version are EQUAL, the winner is determined by
//   (writer_id, then the WINNING mutation's content-addressed mutation_id, carried
//   by last_write_mutation_id) — higher string wins. Both are payload-derived and
//   REPLICA-IDENTICAL: every store derives them from the same mutation, with no
//   central back-channel. This makes the exact tie STRUCTURALLY CONVERGENT: every
//   store computes the same winner. (The final tier is NOT the canonical PK
//   sync_id — that is fixed at first-insert and diverges across replicas for the
//   same topic; this probe uses distinct writer_ids so writer_id decides here.)
//
// Why payload-derived fields and not central seq:
//   Central seq is ASYMMETRIC — a node's own tombstones keep seq=0 (AckMutation
//   never back-patches it; self-authored mutations pulled back are INV5 NoOps),
//   so seq cannot safely break a tie between the authoring node and central.
//   writer_id and the winning mutation_id are derived from the mutation payload,
//   are identical on every replica that has the same mutation, and require no
//   central coordination.
//
// This probe manufactures an exact (updated_at, version) tie between a delete
// and a competing upsert, choosing writer names so the UPSERT's writer wins
// ("writer-B" > "writer-A"). After full sync, the row MUST resurrect everywhere.
//
// The lower-writer-blocked branch is covered by
//   TestEqualTie_UpsertBeforeDelete_Converges (forced-interleaving probe).
// The upsert-vs-upsert branch is covered by
//   TestEqualTie_UpsertVsUpsert_Converges.
//
// NOTE on realism: an exact (updated_at, version) tie across two DISTINCT writers
// is astronomically unlikely in production. This probe manufactures it on purpose
// to verify the tiebreaker boundary precisely.
// ─────────────────────────────────────────────────────────────────────────────

// TestEqualTie_IdentityTiebreak_Converges verifies that on an exact
// (updated_at, version) tie at the tombstone boundary, when the incoming
// upsert's writer_id is HIGHER than the tombstone's writer_id (deleted_by),
// the upsert SUPERSEDES the tombstone and the row RESURRECTS on all three stores.
//
// Writer names: "writer-B" upserts (higher), "writer-A" deletes (lower).
// writeWins("writer-B"/"sync-B" vs tombstone @ "writer-A"/"sync-A") =
//   "writer-B" > "writer-A" → TRUE → resurrection is the spec-correct outcome.
//
// Convergence is asserted across all three stores: all must agree the row is LIVE.
func TestEqualTie_IdentityTiebreak_Converges(t *testing.T) {
	ctx := context.Background()
	central := newCentral(t)
	a := newNode(t, "A")
	b := newNode(t, "B")

	topic := "sdd/test/tsseq-tie"

	// Fix ONE instant used for BOTH the delete and the competing upsert so
	// updated_at ties exactly. Version is also held equal (both v2).
	tie := base.Add(50 * time.Second)

	// 1. A writes T (older than the tie), converge.
	mustWrite(t, a, upsert("writer-A", "sync-A", topic, "A original", 1, base.Add(10*time.Second)))
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)

	// 2. A DELETES T at the tie instant with version 2.
	//    Converge so the tombstone lands on A.local, B.local AND central.
	//    Tombstone identity: deleted_by="writer-A", sync_id="sync-A".
	mustWrite(t, a, del("writer-A", "sync-A", topic, 2, tie))
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 2)
	assertDeletedEverywhere(ctx, t, a, b, central, topic)

	// 3. B upserts T at the EXACT SAME instant (tie) with the SAME version (2).
	//    updated_at == tombstone.deleted_at AND version == tombstone.version, so
	//    only the identity tiebreaker can separate them.
	//    writeWins("writer-B"/"sync-B" vs "writer-A"/"sync-A"):
	//      "writer-B" > "writer-A" → TRUE → resurrection is correct.
	mustWrite(t, b, upsert("writer-B", "sync-B", topic, "B equal-tie upsert", 2, tie))
	syncRounds(ctx, t, []*nodeRef{{a}, {b}}, central, 3)

	// IDENTITY-TIEBREAKER OUTCOME: the exact-tie upsert MUST revive the row
	// everywhere because its writer_id is lexicographically higher.
	// Failing to revive means the tiebreaker is not being applied correctly.
	t.Logf("identity tiebreaker VERIFIED: on an exact (updated_at,version) tie at the "+
		"tombstone boundary, topic %q resurrects on all three stores because 'writer-B' > "+
		"'writer-A' → writeWins returns true → tombstone superseded.",
		topic)

	for _, where := range []struct {
		name string
		rec  bool
	}{
		{"A.local", liveTopicOnNode(t, a, topic) != nil},
		{"B.local", liveTopicOnNode(t, b, topic) != nil},
		{"central", liveTopicOnCentral(t, central, topic) != nil},
	} {
		if !where.rec {
			t.Errorf("identity tiebreaker VIOLATION on %s: higher-writer_id upsert did NOT "+
				"resurrect the row after exact (updated_at,version) tie. "+
				"writeWins('writer-B'/'sync-B' vs tombstone 'writer-A'/'sync-A') must return true. "+
				"Investigate writeWins call site at the tombstone boundary in domain.Decide.",
				where.name)
		}
	}

	// Confirm convergence is CONSISTENT across all three stores (no split-brain
	// where one store revives and another stays deleted).
	aLive := liveTopicOnNode(t, a, topic) != nil
	bLive := liveTopicOnNode(t, b, topic) != nil
	cLive := liveTopicOnCentral(t, central, topic) != nil
	if !(aLive == bLive && bLive == cLive) {
		t.Errorf("identity tiebreaker SPLIT-BRAIN: stores disagree on liveness under the tie "+
			"(A=%v B=%v central=%v) — convergence broke, not just the tie policy",
			aLive, bLive, cLive)
	}
}

var _ = time.Second
