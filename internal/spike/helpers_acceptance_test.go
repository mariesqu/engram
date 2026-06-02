//go:build acceptance

package spike_test

import (
	"context"
	"sort"
	"testing"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/spike"
)

// nodeRef is a thin wrapper so the invariant tests can build []*nodeRef literals
// and iterate nodes with readable call sites.
type nodeRef struct{ n *spike.Node }

// mustWrite applies a local write on a node, failing the test on error.
func mustWrite(t *testing.T, n *spike.Node, m domain.Mutation) {
	t.Helper()
	if _, err := n.Write(m); err != nil {
		t.Fatalf("node %s write (sync_id=%s): %v", n.Name, m.SyncID, err)
	}
}

// spikePush pushes a node's outbox to central, failing on error. Returns the
// number of mutations pushed.
func spikePush(ctx context.Context, t *testing.T, n *spike.Node, central spike.Central) (int, error) {
	t.Helper()
	return spike.Push(ctx, n, central)
}

// syncRounds runs `rounds` full bidirectional sync rounds across the given nodes,
// failing the test on the first error.
func syncRounds(ctx context.Context, t *testing.T, refs []*nodeRef, central spike.Central, rounds int) {
	t.Helper()
	nodes := make([]*spike.Node, 0, len(refs))
	for _, r := range refs {
		nodes = append(nodes, r.n)
	}
	if err := spike.SyncAll(ctx, nodes, central, project, rounds); err != nil {
		t.Fatalf("syncRounds(%d): %v", rounds, err)
	}
}

// ── central raw-state helpers ───────────────────────────────────────────────────

// assertCentralLiveCount asserts the number of LIVE central_memories rows for a
// topic identity.
func assertCentralLiveCount(ctx context.Context, t *testing.T, c *centralStore, topic string, want int) {
	t.Helper()
	var n int
	if err := c.Pool().QueryRow(ctx, `
		SELECT count(*) FROM central_memories
		WHERE topic_key = $1 AND project = $2 AND scope = $3 AND deleted_at IS NULL`,
		topic, project, scope,
	).Scan(&n); err != nil {
		t.Fatalf("assertCentralLiveCount(%q): %v", topic, err)
	}
	if n != want {
		t.Errorf("central live rows for topic %q = %d, want %d", topic, n, want)
	}
}

// maxCentralSeq returns the maximum seq in central_mutations (0 when empty).
func maxCentralSeq(ctx context.Context, t *testing.T, c *centralStore) int64 {
	t.Helper()
	var seq int64
	if err := c.Pool().QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM central_mutations`,
	).Scan(&seq); err != nil {
		t.Fatalf("maxCentralSeq: %v", err)
	}
	return seq
}

// centralMutationCount returns the number of rows in central_mutations.
func centralMutationCount(ctx context.Context, t *testing.T, c *centralStore) int {
	t.Helper()
	var n int
	if err := c.Pool().QueryRow(ctx,
		`SELECT count(*) FROM central_mutations`,
	).Scan(&n); err != nil {
		t.Fatalf("centralMutationCount: %v", err)
	}
	return n
}

// ── deletion / tombstone assertions across all three stores ─────────────────────

// assertDeletedEverywhere asserts that a topic resolves to NO live record on
// A.local, B.local, and central, AND that a tombstone is present on central for
// the topic identity. This is the convergence-of-deletion assertion.
func assertDeletedEverywhere(ctx context.Context, t *testing.T, a, b *spike.Node, c *centralStore, topic string) {
	t.Helper()

	// No live record on either node.
	if rec := liveTopicOnNode(t, a, topic); rec != nil {
		t.Errorf("A.local: topic %q still live (sync_id=%s) after delete; want deleted", topic, rec.SyncID)
	}
	if rec := liveTopicOnNode(t, b, topic); rec != nil {
		t.Errorf("B.local: topic %q still live (sync_id=%s) after delete; want deleted", topic, rec.SyncID)
	}
	// No live record on central.
	if rec := liveTopicOnCentral(t, c, topic); rec != nil {
		t.Errorf("central: topic %q still live (sync_id=%s) after delete; want deleted", topic, rec.SyncID)
	}

	// Central tombstone present for the topic identity.
	var n int
	if err := c.Pool().QueryRow(ctx,
		`SELECT count(*) FROM central_tombstones WHERE topic_key = $1 AND project = $2 AND scope = $3`,
		topic, project, scope,
	).Scan(&n); err != nil {
		t.Fatalf("assertDeletedEverywhere central tombstone count(%q): %v", topic, err)
	}
	if n < 1 {
		t.Errorf("central: no tombstone for deleted topic %q; want >=1", topic)
	}

	// Local tombstone present on both nodes (proves the delete propagated, not
	// just that the live row vanished).
	for _, n := range []*spike.Node{a, b} {
		ts, err := n.Store.FindTombstone("", strp(topic), project, scope)
		if err != nil {
			t.Fatalf("%s FindTombstone(%q): %v", n.Name, topic, err)
		}
		if ts == nil {
			t.Errorf("%s: no local tombstone for deleted topic %q; want one", n.Name, topic)
		}
	}
}

// strp returns a pointer to s (for the FindTombstone topicKey arg).
func strp(s string) *string { return &s }

// ── full snapshot equality across nodes and central ─────────────────────────────

// liveSnap is the convergence-relevant state of one live record used for
// cross-store equality checks.
//
// seq is intentionally excluded: a node that authored the winning write keeps
// its own local row with seq=0 (the central seq was not applied back to it
// because the pull was an INV5 no-op), while the other node's pulled copy
// carries the positive central seq. sync_id is also intentionally excluded from
// the comparison: when two writers write the same topic_key, the canonical row
// retains the FIRST writer's sync_id regardless of which write wins on content,
// so the two nodes may hold the same topic under different sync_ids.
// topic_key + content + version is the portable convergence state — that is
// what must be identical across A.local, B.local, and central.
type liveSnap struct {
	syncID   string
	topicKey string
	content  string
	version  int
}

// assertNodesAndCentralAgree asserts that the set of LIVE records (keyed by
// topic_key) is identical across A.local, B.local, and central — same CONTENT
// and VERSION for every topic_key. sync_id and seq are NOT compared (see
// liveSnap and compareSnaps for the rationale). This is the end-to-end
// convergence statement: all three stores converge on the same canonical state
// for every topic in the workload.
func assertNodesAndCentralAgree(ctx context.Context, t *testing.T, a, b *spike.Node, c *centralStore) {
	t.Helper()

	aSnap := nodeLiveSnap(t, a)
	bSnap := nodeLiveSnap(t, b)
	cSnap := centralLiveSnap(ctx, t, c)

	compareSnaps(t, "A.local vs central", aSnap, cSnap)
	compareSnaps(t, "B.local vs central", bSnap, cSnap)
	compareSnaps(t, "A.local vs B.local", aSnap, bSnap)
}

// nodeLiveSnap reads every live record from a node's local SQLite store keyed by
// topic_key. Topic-keyed records only (the spike's workload is all topic-keyed).
func nodeLiveSnap(t *testing.T, n *spike.Node) map[string]liveSnap {
	t.Helper()
	rows, err := n.Store.DB().Query(`
		SELECT sync_id, COALESCE(topic_key,''), content, version
		FROM memories
		WHERE deleted_at IS NULL AND topic_key IS NOT NULL
		ORDER BY topic_key`)
	if err != nil {
		t.Fatalf("nodeLiveSnap %s: %v", n.Name, err)
	}
	defer rows.Close()

	out := map[string]liveSnap{}
	for rows.Next() {
		var s liveSnap
		if err := rows.Scan(&s.syncID, &s.topicKey, &s.content, &s.version); err != nil {
			t.Fatalf("nodeLiveSnap %s scan: %v", n.Name, err)
		}
		out[s.topicKey] = s
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("nodeLiveSnap %s rows.Err: %v", n.Name, err)
	}
	return out
}

// centralLiveSnap reads every live record from central keyed by topic_key.
func centralLiveSnap(ctx context.Context, t *testing.T, c *centralStore) map[string]liveSnap {
	t.Helper()
	rows, err := c.Pool().Query(ctx, `
		SELECT sync_id, COALESCE(topic_key,''), content, version
		FROM central_memories
		WHERE deleted_at IS NULL AND topic_key IS NOT NULL
		ORDER BY topic_key`)
	if err != nil {
		t.Fatalf("centralLiveSnap: %v", err)
	}
	defer rows.Close()

	out := map[string]liveSnap{}
	for rows.Next() {
		var s liveSnap
		if err := rows.Scan(&s.syncID, &s.topicKey, &s.content, &s.version); err != nil {
			t.Fatalf("centralLiveSnap scan: %v", err)
		}
		out[s.topicKey] = s
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("centralLiveSnap rows.Err: %v", err)
	}
	return out
}

// compareSnaps asserts two live-snapshot maps AGREE on topic-keyed canonical
// state: same set of topic_keys, each with identical CONTENT and VERSION.
//
// What IS compared: topic_key presence, content, and version — the portable
// convergence-relevant state that must be identical across A.local, B.local,
// and central after a full bidirectional sync.
//
// What is NOT compared:
//   - sync_id: a node that authored the winning write keeps its own sync_id for
//     that topic row, while the other node's pulled copy carries a different
//     sync_id (the canonical one from the first writer). Both are correct.
//   - seq: a node's authored row keeps seq=0 locally (the pulled central-seq'd
//     copy is an INV5 no-op on the author's store), while the other node's
//     pulled copy carries the positive central seq. The seq difference is
//     expected and does not indicate divergence.
func compareSnaps(t *testing.T, label string, x, y map[string]liveSnap) {
	t.Helper()
	if len(x) != len(y) {
		t.Errorf("%s: live-topic count differs: %d vs %d (%v vs %v)", label, len(x), len(y), topicKeys(x), topicKeys(y))
	}
	for tk, sx := range x {
		sy, ok := y[tk]
		if !ok {
			t.Errorf("%s: topic %q present on left but missing on right", label, tk)
			continue
		}
		if sx.content != sy.content || sx.version != sy.version {
			t.Errorf("%s: topic %q canonical state differs:\n  left = %+v\n  right= %+v", label, tk, sx, sy)
		}
	}
	for tk := range y {
		if _, ok := x[tk]; !ok {
			t.Errorf("%s: topic %q present on right but missing on left", label, tk)
		}
	}
}

// topicKeys returns the sorted topic keys of a snapshot map (for messages).
func topicKeys(m map[string]liveSnap) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
