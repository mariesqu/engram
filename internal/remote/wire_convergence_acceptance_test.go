//go:build acceptance

// Package remote_test — convergence-over-the-wire acceptance proof (PR4 keystone).
//
// This file re-runs the spike convergence scenarios with the Central being a
// remote.Client pointing at an httptest.Server backed by a real embedded-postgres
// centralstore.Store, proving:
//
//   - The HTTP client + cloudserve server faithfully relay mutations over the wire.
//   - The JSONB round-trip (Postgres normalizes JSON key order / whitespace) does NOT
//     break the mutation_id content-hash — specifically, the ToWire CARRY fix from PR3
//     (which carries m.MutationID instead of recomputing from the JSONB-normalized
//     payload) survives end-to-end and the last_write_mutation_id tiebreaker remains
//     replica-identical after the round-trip.
//   - All six reconciliation invariants converge over HTTP.
//   - The exact-tie (equal updated_at + version + writer_id → decided by
//     last_write_mutation_id) converges over the wire.
//   - Concurrent-delete tombstone metadata converges over the wire.
//
// Test isolation follows the proven spike_test pattern: one embedded-postgres
// instance per package (TestMain), each test on a fresh schema (withSearchPath),
// each node on a fresh SQLite file (t.TempDir). The httptest.Server is per-test
// so each convergence scenario owns its own store + server.
//
// IMPORTANT: The existing in-process spike acceptance tests are NOT touched.
// This file adds wire coverage alongside them rather than refactoring the spike
// internals, keeping the proven proof hermetic and eliminating split-brain risk
// from a shared-helper extraction.
package remote_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mariesqu/engram/internal/centralstore"
	"github.com/mariesqu/engram/internal/cloudserve"
	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/mutation"
	"github.com/mariesqu/engram/internal/remote"
	"github.com/mariesqu/engram/internal/spike"
)

// ── Package-level embedded-postgres harness ───────────────────────────────────

var (
	wirePgOnce sync.Once
	wirePgDSN  string
	wirePgEP   *embeddedpostgres.EmbeddedPostgres
)

// TestMain starts embedded-postgres once for this package (or uses
// ENGRAM_TEST_PG_DSN if provided) and stops it after all tests finish.
func TestMain(m *testing.M) {
	wirePgOnce.Do(func() {
		if dsn := os.Getenv("ENGRAM_TEST_PG_DSN"); dsn != "" {
			wirePgDSN = dsn
			return
		}
		port, err := wireFreePort()
		if err != nil {
			panic("wire_test: could not find free port: " + err.Error())
		}

		cacheDir := wireCacheRoot()
		runtimeDir := filepath.Join(os.TempDir(), fmt.Sprintf("engram-wire-epg-%d", port))

		cfg := embeddedpostgres.DefaultConfig().
			Port(uint32(port)).
			Database("engram_wire").
			Username("engram").
			Password("engram").
			CachePath(cacheDir).
			RuntimePath(runtimeDir)

		ep := embeddedpostgres.NewDatabase(cfg)
		if err := ep.Start(); err != nil {
			panic("wire_test: embedded-postgres Start: " + err.Error())
		}
		wirePgEP = ep
		wirePgDSN = fmt.Sprintf("host=localhost port=%d user=engram password=engram dbname=engram_wire sslmode=disable", port)
	})

	code := m.Run()

	if wirePgEP != nil {
		if err := wirePgEP.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "wire_test: embedded-postgres Stop: %v\n", err)
		}
	}
	os.Exit(code)
}

// ── Per-test wire setup ───────────────────────────────────────────────────────

// wireStore opens a fresh isolated centralstore.Store on a new Postgres schema,
// returning the store and a cleanup func. Pattern mirrors spike_test.newCentral.
func wireStore(t *testing.T) *centralstore.Store {
	t.Helper()
	schemaName := wireSchemaFor(t)
	ctx := context.Background()

	adminPool, err := pgxpool.New(ctx, wirePgDSN)
	if err != nil {
		t.Fatalf("wireStore: pgxpool admin: %v", err)
	}
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE", schemaName)); err != nil {
		t.Fatalf("wireStore: DROP SCHEMA: %v", err)
	}
	if _, err := adminPool.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %q", schemaName)); err != nil {
		t.Fatalf("wireStore: CREATE SCHEMA: %v", err)
	}

	dsn, err := wireWithSearchPath(wirePgDSN, schemaName)
	if err != nil {
		t.Fatalf("wireStore: withSearchPath: %v", err)
	}
	store, err := centralstore.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("wireStore: centralstore.Open: %v", err)
	}

	t.Cleanup(func() {
		store.Close()
		cleanPool, err2 := pgxpool.New(ctx, wirePgDSN)
		if err2 != nil {
			t.Logf("wireStore cleanup: pgxpool: %v", err2)
			return
		}
		defer cleanPool.Close()
		if _, err2 = cleanPool.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE", schemaName)); err2 != nil {
			t.Logf("wireStore cleanup: DROP SCHEMA %q: %v", schemaName, err2)
		}
	})
	return store
}

// newWireCentral starts a cloudserve httptest.Server backed by a real store
// and returns a *remote.Client implementing transport.Central over HTTP.
// The server is closed (and store connection returned) on test cleanup.
func newWireCentral(t *testing.T) (*remote.Client, *centralstore.Store) {
	t.Helper()
	store := wireStore(t)
	srv := httptest.NewServer(cloudserve.New(store).Handler())
	t.Cleanup(srv.Close)
	return remote.New(srv.URL, nil), store
}

// newWireNode opens a fresh local SQLite store in a temp dir and wraps it as a
// spike node. Pattern mirrors spike_test.newNode.
func newWireNode(t *testing.T, name string) *spike.Node {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name+".db")
	st, err := localstore.Open(path)
	if err != nil {
		t.Fatalf("newWireNode %s: localstore.Open(%q): %v", name, path, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return spike.NewNode(name, st)
}

// ── Shared constants & helpers ────────────────────────────────────────────────

const (
	wireProject = "engram"
	wireScope   = "project"
)

// wireBase is the fixed reference instant for deterministic wall-clock ordering.
var wireBase = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

// wireUpsert builds a topic-keyed upsert domain.Mutation for the wire tests.
// Identity fields (Payload, MutationID) are left empty so LocalWrite derives them.
func wireUpsert(writer, syncID, topic, content string, version int, at time.Time) domain.Mutation {
	tk := topic
	return domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess-" + writer,
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "title-" + syncID,
		Content:    content,
		Project:    wireProject,
		Scope:      wireScope,
		TopicKey:   &tk,
		Version:    version,
		UpdatedAt:  at,
		WriterID:   writer,
	}
}

// wireDel builds a topic-keyed delete domain.Mutation for the wire tests.
func wireDel(writer, syncID, topic string, version int, at time.Time) domain.Mutation {
	tk := topic
	return domain.Mutation{
		Op:         domain.OpDelete,
		SyncID:     syncID,
		SessionID:  "sess-" + writer,
		EntityType: domain.EntityMemory,
		Project:    wireProject,
		Scope:      wireScope,
		TopicKey:   &tk,
		Version:    version,
		UpdatedAt:  at,
		WriterID:   writer,
	}
}

// wireMustWrite applies a local write on a node, failing the test on error.
func wireMustWrite(t *testing.T, n *spike.Node, m domain.Mutation) {
	t.Helper()
	if _, err := n.Write(m); err != nil {
		t.Fatalf("node %s write (sync_id=%s): %v", n.Name, m.SyncID, err)
	}
}

// wireSyncRounds runs `rounds` full bidirectional sync rounds across all given
// nodes against the wire central, failing the test on the first error.
func wireSyncRounds(ctx context.Context, t *testing.T, nodes []*spike.Node, central spike.Central, rounds int) {
	t.Helper()
	if err := spike.SyncAll(ctx, nodes, central, wireProject, rounds); err != nil {
		t.Fatalf("wireSyncRounds(%d): %v", rounds, err)
	}
}

// wirePush pushes a node's outbox to the wire central.
func wirePush(ctx context.Context, t *testing.T, n *spike.Node, central spike.Central) (int, error) {
	t.Helper()
	return spike.Push(ctx, n, central)
}

// wirePull pulls central mutations to a node.
func wirePull(ctx context.Context, t *testing.T, n *spike.Node, central spike.Central) (int, error) {
	t.Helper()
	return spike.Pull(ctx, n, central, wireProject)
}

// ── Local-store assertion helpers (mirror spike_test) ────────────────────────

func wireLiveTopicOnNode(t *testing.T, n *spike.Node, topic string) *domain.Record {
	t.Helper()
	rec, err := n.Store.FindByTopic(topic, wireProject, wireScope)
	if err != nil {
		t.Fatalf("node %s FindByTopic(%q): %v", n.Name, topic, err)
	}
	return rec
}

func wireLiveTopicOnCentral(t *testing.T, c *centralstore.Store, topic string) *domain.Record {
	t.Helper()
	rec, err := c.FindByTopic(topic, wireProject, wireScope)
	if err != nil {
		t.Fatalf("central FindByTopic(%q): %v", topic, err)
	}
	return rec
}

func wireAssertOneLiveEverywhere(t *testing.T, a, b *spike.Node, c *centralstore.Store, topic, wantCentralSyncID, wantContent string) {
	t.Helper()
	aRec := wireLiveTopicOnNode(t, a, topic)
	bRec := wireLiveTopicOnNode(t, b, topic)
	cRec := wireLiveTopicOnCentral(t, c, topic)

	for _, tc := range []struct {
		where string
		rec   *domain.Record
	}{
		{"A.local", aRec},
		{"B.local", bRec},
		{"central", cRec},
	} {
		if tc.rec == nil {
			t.Errorf("%s: FindByTopic(%q) returned nil; want one live record", tc.where, topic)
			continue
		}
		if tc.rec.Content != wantContent {
			t.Errorf("%s: live content=%q, want %q", tc.where, tc.rec.Content, wantContent)
		}
	}
	if cRec != nil && cRec.SyncID != wantCentralSyncID {
		t.Errorf("central: canonical sync_id=%q, want %q", cRec.SyncID, wantCentralSyncID)
	}

	wireAssertNodeLiveCount(t, a, topic, 1)
	wireAssertNodeLiveCount(t, b, topic, 1)
}

func wireAssertNodeLiveCount(t *testing.T, n *spike.Node, topic string, want int) {
	t.Helper()
	var got int
	if err := n.Store.DB().QueryRow(
		`SELECT count(*) FROM memories
		   WHERE topic_key=? AND project=? AND scope=? AND deleted_at IS NULL`,
		topic, wireProject, wireScope,
	).Scan(&got); err != nil {
		t.Fatalf("wireAssertNodeLiveCount %s (%q): %v", n.Name, topic, err)
	}
	if got != want {
		t.Errorf("%s: live rows for topic %q = %d, want %d", n.Name, topic, got, want)
	}
}

func wireAssertCentralLiveCount(ctx context.Context, t *testing.T, c *centralstore.Store, topic string, want int) {
	t.Helper()
	var n int
	if err := c.Pool().QueryRow(ctx, `
		SELECT count(*) FROM central_memories
		WHERE topic_key = $1 AND project = $2 AND scope = $3 AND deleted_at IS NULL`,
		topic, wireProject, wireScope,
	).Scan(&n); err != nil {
		t.Fatalf("wireAssertCentralLiveCount(%q): %v", topic, err)
	}
	if n != want {
		t.Errorf("central live rows for topic %q = %d, want %d", topic, n, want)
	}
}

func wireAssertDeletedEverywhere(ctx context.Context, t *testing.T, a, b *spike.Node, c *centralstore.Store, topic string) {
	t.Helper()

	if rec := wireLiveTopicOnNode(t, a, topic); rec != nil {
		t.Errorf("A.local: topic %q still live (sync_id=%s); want deleted", topic, rec.SyncID)
	}
	if rec := wireLiveTopicOnNode(t, b, topic); rec != nil {
		t.Errorf("B.local: topic %q still live (sync_id=%s); want deleted", topic, rec.SyncID)
	}
	if rec := wireLiveTopicOnCentral(t, c, topic); rec != nil {
		t.Errorf("central: topic %q still live (sync_id=%s); want deleted", topic, rec.SyncID)
	}

	// Central tombstone must exist.
	var n int
	if err := c.Pool().QueryRow(ctx,
		`SELECT count(*) FROM central_tombstones WHERE topic_key = $1 AND project = $2 AND scope = $3`,
		topic, wireProject, wireScope,
	).Scan(&n); err != nil {
		t.Fatalf("wireAssertDeletedEverywhere central tombstone(%q): %v", topic, err)
	}
	if n < 1 {
		t.Errorf("central: no tombstone for deleted topic %q; want >=1", topic)
	}

	// Local tombstones on both nodes.
	for _, nd := range []*spike.Node{a, b} {
		ts, err := nd.Store.FindTombstone("", wireStrp(topic), wireProject, wireScope)
		if err != nil {
			t.Fatalf("%s FindTombstone(%q): %v", nd.Name, topic, err)
		}
		if ts == nil {
			t.Errorf("%s: no local tombstone for deleted topic %q; want one", nd.Name, topic)
		}
	}
}

func wireStrp(s string) *string { return &s }

// wireMaxCentralSeq returns the max seq in central_mutations.
func wireMaxCentralSeq(ctx context.Context, t *testing.T, c *centralstore.Store) int64 {
	t.Helper()
	var seq int64
	if err := c.Pool().QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM central_mutations`,
	).Scan(&seq); err != nil {
		t.Fatalf("wireMaxCentralSeq: %v", err)
	}
	return seq
}

// wireCentralMutationCount returns the count of rows in central_mutations.
func wireCentralMutationCount(ctx context.Context, t *testing.T, c *centralstore.Store) int {
	t.Helper()
	var n int
	if err := c.Pool().QueryRow(ctx,
		`SELECT count(*) FROM central_mutations`,
	).Scan(&n); err != nil {
		t.Fatalf("wireCentralMutationCount: %v", err)
	}
	return n
}

// wireLastWriteMutationIDOnCentral reads last_write_mutation_id from
// central_memories for the given topic (live row). Returns "" if none.
// This is the JSONB round-trip probe: after the JSONB normalization by Postgres
// the mutation_id column must still equal the original content-hash that the
// sender set (because ToWire CARRIES m.MutationID, not recomputes it).
func wireLastWriteMutationIDOnCentral(t *testing.T, c *centralstore.Store, topic string) string {
	t.Helper()
	rec, err := c.FindByTopic(topic, wireProject, wireScope)
	if err != nil {
		t.Fatalf("wireLastWriteMutationIDOnCentral FindByTopic(%q): %v", topic, err)
	}
	if rec == nil {
		return ""
	}
	return rec.LastWriteMutationID
}

// wireLocalTombstoneMeta reads tombstone metadata from a node's local store.
func wireLocalTombstoneMeta(t *testing.T, n *spike.Node, topic string) (deletedAt time.Time, version int, deletedBy string) {
	t.Helper()
	row := n.Store.DB().QueryRow(
		`SELECT deleted_at, version, deleted_by
		   FROM memory_tombstones
		  WHERE topic_key=? AND project=? AND scope=?
		  LIMIT 1`,
		topic, wireProject, wireScope,
	)
	var da string
	if err := row.Scan(&da, &version, &deletedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, 0, ""
		}
		t.Fatalf("wireLocalTombstoneMeta: scan tombstone for topic %q: %v", topic, err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, da)
	if err != nil {
		t.Fatalf("wireLocalTombstoneMeta: parse deleted_at %q for topic %q: %v", da, topic, err)
	}
	return parsed, version, deletedBy
}

// wireLocalCanonicalSyncID returns the stored PK sync_id for a topic on a node.
func wireLocalCanonicalSyncID(t *testing.T, n *spike.Node, topic string) string {
	t.Helper()
	var got string
	if err := n.Store.DB().QueryRow(
		`SELECT sync_id FROM memories
		   WHERE topic_key=? AND project=? AND scope=?
		   ORDER BY sync_id LIMIT 1`,
		topic, wireProject, wireScope,
	).Scan(&got); err != nil {
		t.Fatalf("wireLocalCanonicalSyncID %s (%q): %v", n.Name, topic, err)
	}
	return got
}

// wireLiveSnap captures content+version keyed by topic_key across a store (for
// full snapshot equality assertions).
type wireLiveSnap struct {
	topicKey string
	content  string
	version  int
}

func wireNodeLiveSnap(t *testing.T, n *spike.Node) map[string]wireLiveSnap {
	t.Helper()
	rows, err := n.Store.DB().Query(`
		SELECT COALESCE(topic_key,''), content, version
		FROM memories
		WHERE deleted_at IS NULL AND topic_key IS NOT NULL
		ORDER BY topic_key`)
	if err != nil {
		t.Fatalf("wireNodeLiveSnap %s: %v", n.Name, err)
	}
	defer rows.Close()
	out := map[string]wireLiveSnap{}
	for rows.Next() {
		var s wireLiveSnap
		if err := rows.Scan(&s.topicKey, &s.content, &s.version); err != nil {
			t.Fatalf("wireNodeLiveSnap %s scan: %v", n.Name, err)
		}
		out[s.topicKey] = s
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("wireNodeLiveSnap %s rows.Err: %v", n.Name, err)
	}
	return out
}

func wireCentralLiveSnap(ctx context.Context, t *testing.T, c *centralstore.Store) map[string]wireLiveSnap {
	t.Helper()
	rows, err := c.Pool().Query(ctx, `
		SELECT COALESCE(topic_key,''), content, version
		FROM central_memories
		WHERE deleted_at IS NULL AND topic_key IS NOT NULL
		ORDER BY topic_key`)
	if err != nil {
		t.Fatalf("wireCentralLiveSnap: %v", err)
	}
	defer rows.Close()
	out := map[string]wireLiveSnap{}
	for rows.Next() {
		var s wireLiveSnap
		if err := rows.Scan(&s.topicKey, &s.content, &s.version); err != nil {
			t.Fatalf("wireCentralLiveSnap scan: %v", err)
		}
		out[s.topicKey] = s
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("wireCentralLiveSnap rows.Err: %v", err)
	}
	return out
}

func wireCompareSnaps(t *testing.T, label string, x, y map[string]wireLiveSnap) {
	t.Helper()
	if len(x) != len(y) {
		t.Errorf("%s: live-topic count differs: %d vs %d (%v vs %v)",
			label, len(x), len(y), wireTopicKeys(x), wireTopicKeys(y))
	}
	for tk, sx := range x {
		sy, ok := y[tk]
		if !ok {
			t.Errorf("%s: topic %q present on left but missing on right", label, tk)
			continue
		}
		if sx.content != sy.content || sx.version != sy.version {
			t.Errorf("%s: topic %q state differs:\n  left = %+v\n  right= %+v", label, tk, sx, sy)
		}
	}
	for tk := range y {
		if _, ok := x[tk]; !ok {
			t.Errorf("%s: topic %q present on right but missing on left", label, tk)
		}
	}
}

func wireTopicKeys(m map[string]wireLiveSnap) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ────────────────────────────────────────────────────────────────────────────
// WIRE CONVERGENCE PROOF — 6 invariants + exact-tie + concurrent deletes
// ────────────────────────────────────────────────────────────────────────────

// TestWire_INV1_TopicConvergence: A writes older, B writes newer same topic.
// A pushes first (canonical identity), B pushes second (newer content wins).
// After sync all three stores hold one live row with B's content.
func TestWire_INV1_TopicConvergence(t *testing.T) {
	ctx := context.Background()
	central, store := newWireCentral(t)
	a := newWireNode(t, "A")
	b := newWireNode(t, "B")
	topic := "wire/test/inv1-topic"

	wireMustWrite(t, a, wireUpsert("writer-A", "sync-A", topic, "A content (older)", 1, wireBase.Add(10*time.Second)))
	wireMustWrite(t, b, wireUpsert("writer-B", "sync-B", topic, "B content (newer winner)", 2, wireBase.Add(20*time.Second)))

	if _, err := wirePush(ctx, t, a, central); err != nil {
		t.Fatalf("push A: %v", err)
	}
	if _, err := wirePush(ctx, t, b, central); err != nil {
		t.Fatalf("push B: %v", err)
	}

	wireSyncRounds(ctx, t, []*spike.Node{a, b}, central, 2)

	wireAssertOneLiveEverywhere(t, a, b, store, topic, "sync-A", "B content (newer winner)")
	wireAssertCentralLiveCount(ctx, t, store, topic, 1)

	// JSONB round-trip probe: last_write_mutation_id on central must be non-empty
	// (the winning mutation's hash survived the Postgres JSONB normalization).
	lwmID := wireLastWriteMutationIDOnCentral(t, store, topic)
	if lwmID == "" {
		t.Error("INV1 wire: last_write_mutation_id is empty on central after JSONB round-trip; " +
			"ToWire carry-original fix may not be active")
	}
}

// TestWire_INV2_MonotonicSeq: interleaved pushes produce strictly increasing seqs.
func TestWire_INV2_MonotonicSeq(t *testing.T) {
	ctx := context.Background()
	central, store := newWireCentral(t)
	a := newWireNode(t, "A")
	b := newWireNode(t, "B")

	wireMustWrite(t, a, wireUpsert("writer-A", "sync-2a1", "wire/test/inv2-a1", "a1", 1, wireBase.Add(1*time.Second)))
	if _, err := wirePush(ctx, t, a, central); err != nil {
		t.Fatalf("push A1: %v", err)
	}
	wireMustWrite(t, b, wireUpsert("writer-B", "sync-2b1", "wire/test/inv2-b1", "b1", 1, wireBase.Add(2*time.Second)))
	if _, err := wirePush(ctx, t, b, central); err != nil {
		t.Fatalf("push B1: %v", err)
	}
	wireMustWrite(t, a, wireUpsert("writer-A", "sync-2a2", "wire/test/inv2-a2", "a2", 1, wireBase.Add(3*time.Second)))
	if _, err := wirePush(ctx, t, a, central); err != nil {
		t.Fatalf("push A2: %v", err)
	}
	wireMustWrite(t, b, wireUpsert("writer-B", "sync-2b2", "wire/test/inv2-b2", "b2", 1, wireBase.Add(4*time.Second)))
	if _, err := wirePush(ctx, t, b, central); err != nil {
		t.Fatalf("push B2: %v", err)
	}

	rows, err := store.Pool().Query(ctx, `SELECT seq FROM central_mutations ORDER BY seq`)
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
		t.Fatalf("INV2 wire: expected 4 rows, got %d (%v)", len(seqs), seqs)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Errorf("INV2 wire: central seq not strictly increasing: %v", seqs)
		}
	}

	wireSyncRounds(ctx, t, []*spike.Node{a, b}, central, 2)

	for _, topic := range []string{"wire/test/inv2-a1", "wire/test/inv2-b1", "wire/test/inv2-a2", "wire/test/inv2-b2"} {
		for _, nd := range []*spike.Node{a, b} {
			if wireLiveTopicOnNode(t, nd, topic) == nil {
				t.Errorf("INV2 wire: node %s missing live row for %q", nd.Name, topic)
			}
		}
	}
}

// TestWire_INV3_NoLostUpdate: older write must not clobber newer after convergence.
func TestWire_INV3_NoLostUpdate(t *testing.T) {
	ctx := context.Background()
	central, store := newWireCentral(t)
	a := newWireNode(t, "A")
	b := newWireNode(t, "B")
	topic := "wire/test/inv3-nolost"

	wireMustWrite(t, b, wireUpsert("writer-B", "sync-B", topic, "B content (newer, must survive)", 2, wireBase.Add(50*time.Second)))
	if _, err := wirePush(ctx, t, b, central); err != nil {
		t.Fatalf("push B: %v", err)
	}
	wireMustWrite(t, a, wireUpsert("writer-A", "sync-A", topic, "A content (older, must lose)", 1, wireBase.Add(10*time.Second)))
	if _, err := wirePush(ctx, t, a, central); err != nil {
		t.Fatalf("push A: %v", err)
	}
	wireSyncRounds(ctx, t, []*spike.Node{a, b}, central, 2)

	wireAssertOneLiveEverywhere(t, a, b, store, topic, "sync-B", "B content (newer, must survive)")
	wireAssertCentralLiveCount(ctx, t, store, topic, 1)
}

// TestWire_INV4_NoResurrection: stale upsert must not revive a deleted topic;
// strictly newer upsert DOES revive it.
func TestWire_INV4_NoResurrection(t *testing.T) {
	ctx := context.Background()
	central, store := newWireCentral(t)
	a := newWireNode(t, "A")
	b := newWireNode(t, "B")
	topic := "wire/test/inv4-resurrect"

	wireMustWrite(t, a, wireUpsert("writer-A", "sync-A", topic, "A original", 1, wireBase.Add(10*time.Second)))
	wireSyncRounds(ctx, t, []*spike.Node{a, b}, central, 2)
	wireAssertOneLiveEverywhere(t, a, b, store, topic, "sync-A", "A original")

	wireMustWrite(t, a, wireDel("writer-A", "sync-A", topic, 2, wireBase.Add(50*time.Second)))
	wireSyncRounds(ctx, t, []*spike.Node{a, b}, central, 2)
	wireAssertDeletedEverywhere(ctx, t, a, b, store, topic)

	wireMustWrite(t, b, wireUpsert("writer-B", "sync-B", topic, "B STALE revive attempt", 1, wireBase.Add(30*time.Second)))
	wireSyncRounds(ctx, t, []*spike.Node{a, b}, central, 3)
	wireAssertDeletedEverywhere(ctx, t, a, b, store, topic) // still deleted

	wireMustWrite(t, a, wireUpsert("writer-A", "sync-A", topic, "A revived (newer than delete)", 3, wireBase.Add(90*time.Second)))
	wireSyncRounds(ctx, t, []*spike.Node{a, b}, central, 3)
	wireAssertOneLiveEverywhere(t, a, b, store, topic, "sync-A", "A revived (newer than delete)")
	wireAssertCentralLiveCount(ctx, t, store, topic, 1)
}

// TestWire_INV5_Idempotent: repeated sync rounds do not double-apply or grow seq.
func TestWire_INV5_Idempotent(t *testing.T) {
	ctx := context.Background()
	central, store := newWireCentral(t)
	a := newWireNode(t, "A")
	b := newWireNode(t, "B")
	topic := "wire/test/inv5-idem"

	wireMustWrite(t, a, wireUpsert("writer-A", "sync-A", topic, "A idem", 1, wireBase.Add(10*time.Second)))
	wireSyncRounds(ctx, t, []*spike.Node{a, b}, central, 2)

	seqAfterFirst := wireMaxCentralSeq(ctx, t, store)
	mutCountFirst := wireCentralMutationCount(ctx, t, store)
	verAfterFirst := wireLiveTopicOnCentral(t, store, topic).Version

	wireSyncRounds(ctx, t, []*spike.Node{a, b}, central, 3)

	if got := wireMaxCentralSeq(ctx, t, store); got != seqAfterFirst {
		t.Errorf("INV5 wire: central max seq grew on no-op rounds: %d → %d", seqAfterFirst, got)
	}
	if got := wireCentralMutationCount(ctx, t, store); got != mutCountFirst {
		t.Errorf("INV5 wire: central_mutations count grew: %d → %d", mutCountFirst, got)
	}
	if got := wireLiveTopicOnCentral(t, store, topic).Version; got != verAfterFirst {
		t.Errorf("INV5 wire: central version churned: %d → %d", verAfterFirst, got)
	}
	for _, nd := range []*spike.Node{a, b} {
		rec := wireLiveTopicOnNode(t, nd, topic)
		if rec == nil {
			t.Fatalf("INV5 wire: node %s lost the live row", nd.Name)
		}
		if rec.Version != verAfterFirst {
			t.Errorf("INV5 wire: node %s version churned: want %d, got %d", nd.Name, verAfterFirst, rec.Version)
		}
	}
	wireAssertOneLiveEverywhere(t, a, b, store, topic, "sync-A", "A idem")
	wireAssertCentralLiveCount(ctx, t, store, topic, 1)
}

// TestWire_INV6_IndependentWrites: different topics both survive on all stores.
func TestWire_INV6_IndependentWrites(t *testing.T) {
	ctx := context.Background()
	central, store := newWireCentral(t)
	a := newWireNode(t, "A")
	b := newWireNode(t, "B")
	t1, t2 := "wire/test/inv6-t1", "wire/test/inv6-t2"

	wireMustWrite(t, a, wireUpsert("writer-A", "sync-A", t1, "A's T1", 1, wireBase.Add(10*time.Second)))
	wireMustWrite(t, b, wireUpsert("writer-B", "sync-B", t2, "B's T2", 1, wireBase.Add(20*time.Second)))
	wireSyncRounds(ctx, t, []*spike.Node{a, b}, central, 2)

	wireAssertOneLiveEverywhere(t, a, b, store, t1, "sync-A", "A's T1")
	wireAssertOneLiveEverywhere(t, a, b, store, t2, "sync-B", "B's T2")
	wireAssertCentralLiveCount(ctx, t, store, t1, 1)
	wireAssertCentralLiveCount(ctx, t, store, t2, 1)
}

// TestWire_FullBidirectionalSettles: after a full bidirectional sync, A.local ==
// B.local == central for every live record (full snapshot equality).
func TestWire_FullBidirectionalSettles(t *testing.T) {
	ctx := context.Background()
	central, store := newWireCentral(t)
	a := newWireNode(t, "A")
	b := newWireNode(t, "B")

	t1, t2, t3, t4 := "wire/test/settle-t1", "wire/test/settle-t2", "wire/test/settle-t3", "wire/test/settle-t4"

	wireMustWrite(t, a, wireUpsert("writer-A", "sync-A1", t1, "A T1 older", 1, wireBase.Add(10*time.Second)))
	wireMustWrite(t, b, wireUpsert("writer-B", "sync-B1", t1, "B T1 newer", 2, wireBase.Add(20*time.Second)))
	wireMustWrite(t, a, wireUpsert("writer-A", "sync-A2", t2, "A T2", 1, wireBase.Add(15*time.Second)))
	wireMustWrite(t, b, wireUpsert("writer-B", "sync-B2", t3, "B T3", 1, wireBase.Add(25*time.Second)))
	wireMustWrite(t, a, wireUpsert("writer-A", "sync-A4", t4, "A T4 (will delete)", 1, wireBase.Add(30*time.Second)))

	wireSyncRounds(ctx, t, []*spike.Node{a, b}, central, 2)

	wireMustWrite(t, a, wireDel("writer-A", "sync-A4", t4, 2, wireBase.Add(60*time.Second)))
	wireSyncRounds(ctx, t, []*spike.Node{a, b}, central, 2)

	wireAssertOneLiveEverywhere(t, a, b, store, t1, "sync-A1", "B T1 newer")
	wireAssertOneLiveEverywhere(t, a, b, store, t2, "sync-A2", "A T2")
	wireAssertOneLiveEverywhere(t, a, b, store, t3, "sync-B2", "B T3")
	wireAssertDeletedEverywhere(ctx, t, a, b, store, t4)

	aSnap := wireNodeLiveSnap(t, a)
	bSnap := wireNodeLiveSnap(t, b)
	cSnap := wireCentralLiveSnap(ctx, t, store)
	wireCompareSnaps(t, "wire A.local vs central", aSnap, cSnap)
	wireCompareSnaps(t, "wire B.local vs central", bSnap, cSnap)
	wireCompareSnaps(t, "wire A.local vs B.local", aSnap, bSnap)
}

// ── KEYSTONE: exact-tie over HTTP + JSONB round-trip ─────────────────────────

// TestWire_ExactTie_MutationIDConverges is the bedrock of the PR4 proof.
//
// This test probes the mutation_id tiebreaker (last_write_mutation_id) after the
// JSONB round-trip. It mirrors the spike's crown-jewel test
// TestConvergence_FinalTiebreaker_DivergentStoredPK_MutationIDConverges, but
// drives the Central through the HTTP transport.
//
// The crucial invariant under test: after Postgres JSONB-normalizes the payload
// (key reordering, whitespace), the mutation_id stored in central_mutations and
// propagated back via PullSince must be the ORIGINAL content-hash (carried by
// ToWire from m.MutationID), NOT a recomputation from the normalized bytes.
// If recomputed, the hash would differ and the tiebreaker would be corrupted,
// causing split-brain at the exact-tie boundary — the root cause the PR3
// carry-original fix addressed.
//
// This test DIRECTLY validates that fix survives the full HTTP + JSONB path.
func TestWire_ExactTie_MutationIDConverges(t *testing.T) {
	ctx := context.Background()
	central, store := newWireCentral(t)
	a := newWireNode(t, "A")
	b := newWireNode(t, "B")
	topic := "wire/test/final-tiebreaker-jsonb"

	tOlder := wireBase.Add(10 * time.Second)
	tWinner := wireBase.Add(40 * time.Second)

	const (
		syncA  = "sync-A"
		syncB  = "sync-B"
		syncAM = "sync-AM" // lexically between sync-A and sync-B
	)

	// Phase 1a: A inserts sync-A (older), push so sync-A is central canonical.
	wireMustWrite(t, a, wireUpsert("writer-A", syncA, topic, "A content (older)", 1, tOlder))
	if _, err := wirePush(ctx, t, a, central); err != nil {
		t.Fatalf("push A (sync-A): %v", err)
	}

	// Phase 1b: B inserts sync-B (newer winner).
	wireMustWrite(t, b, wireUpsert("writer-B", syncB, topic, "B content (winner)", 2, tWinner))

	// Settle: writer-B's newer write wins; A stored PK = sync-A, B stored PK = sync-B.
	wireSyncRounds(ctx, t, []*spike.Node{a, b}, central, 3)

	aPK := wireLocalCanonicalSyncID(t, a, topic)
	bPK := wireLocalCanonicalSyncID(t, b, topic)
	aRec := wireLiveTopicOnNode(t, a, topic)
	bRec := wireLiveTopicOnNode(t, b, topic)
	if aRec == nil || bRec == nil {
		t.Fatalf("precondition: topic must be live on A and B")
	}
	if aPK != syncA {
		t.Fatalf("precondition NOT met: A stored PK = %q, want %q", aPK, syncA)
	}
	if bPK != syncB {
		t.Fatalf("precondition NOT met: B stored PK = %q, want %q", bPK, syncB)
	}
	const winnerContent = "B content (winner)"
	if aRec.Content != winnerContent || bRec.Content != winnerContent {
		t.Fatalf("precondition: content not converged A=%q B=%q", aRec.Content, bRec.Content)
	}

	// JSONB round-trip check: last_write_mutation_id on central must equal the
	// mutation_id the push sender computed (not a recomputation from normalized bytes).
	centralLWMID := wireLastWriteMutationIDOnCentral(t, store, topic)
	if centralLWMID == "" {
		t.Fatal("JSONB round-trip FAIL: central last_write_mutation_id is empty after push/pull")
	}

	// Compute what the winner's mutation_id SHOULD be (the original hash).
	winnerMut := wireUpsert("writer-B", syncB, topic, winnerContent, 2, tWinner)
	winnerMut.Payload = mutation.CanonicalPayload(winnerMut)
	winnerMut.MutationID = mutation.NewMutationID(winnerMut.Payload)

	if centralLWMID != winnerMut.MutationID {
		t.Errorf("JSONB round-trip CORRUPTION: central last_write_mutation_id = %q, "+
			"want original mutation_id = %q. "+
			"This means ToWire recomputed from JSONB-normalized bytes instead of carrying m.MutationID. "+
			"PR3 carry-original fix is NOT active on the wire path.",
			centralLWMID, winnerMut.MutationID)
	}
	t.Logf("JSONB round-trip OK: central last_write_mutation_id = %q matches original hash", centralLWMID)

	// Phase 2: the exact-tie probe write.
	probe := wireUpsert("writer-B", syncAM, topic, "PROBE content (tie, sync-AM)", 2, tWinner)
	wireMustWrite(t, b, probe)

	probeMut := wireUpsert("writer-B", syncAM, topic, "PROBE content (tie, sync-AM)", 2, tWinner)
	probeMut.Payload = mutation.CanonicalPayload(probeMut)
	probeID := mutation.NewMutationID(probeMut.Payload)
	probeShouldWin := probeID > winnerMut.MutationID

	t.Logf("mutation_ids: winner=%s probe=%s → probe %s win",
		winnerMut.MutationID, probeID,
		map[bool]string{true: "SHOULD", false: "must NOT"}[probeShouldWin])

	wireSyncRounds(ctx, t, []*spike.Node{a, b}, central, 4)

	aFinal := wireLiveTopicOnNode(t, a, topic)
	bFinal := wireLiveTopicOnNode(t, b, topic)
	cFinal := wireLiveTopicOnCentral(t, store, topic)
	if aFinal == nil || bFinal == nil || cFinal == nil {
		t.Fatalf("post-probe: topic must be live everywhere (A=%v B=%v central=%v)",
			aFinal != nil, bFinal != nil, cFinal != nil)
	}

	t.Logf("FINAL content: A=%q B=%q central=%q", aFinal.Content, bFinal.Content, cFinal.Content)

	// Crown-jewel assertion: all stores converge on the SAME content.
	if aFinal.Content != bFinal.Content || bFinal.Content != cFinal.Content {
		t.Fatalf("SPLIT-BRAIN at exact tie over HTTP: A=%q B=%q central=%q. "+
			"The mutation_id tiebreaker resolved differently across replicas — "+
			"JSONB round-trip may have corrupted last_write_mutation_id.",
			aFinal.Content, bFinal.Content, cFinal.Content)
	}

	wantContent := winnerContent
	if probeShouldWin {
		wantContent = "PROBE content (tie, sync-AM)"
	}
	if cFinal.Content != wantContent {
		t.Errorf("exact-tie wire: converged on %q; want %q (higher mutation_id wins)",
			cFinal.Content, wantContent)
	}
}

// TestWire_ConcurrentDeletes_TombstoneMetadataConverges: two deletes at different
// updated_at values applied in opposite orders on the two nodes must converge
// to the NEWER delete's metadata everywhere.
func TestWire_ConcurrentDeletes_TombstoneMetadataConverges(t *testing.T) {
	ctx := context.Background()
	central, store := newWireCentral(t)
	a := newWireNode(t, "A")
	b := newWireNode(t, "B")
	topic := "wire/test/concurrent-deletes-tombstone"

	tInit := wireBase.Add(10 * time.Second)
	tA := wireBase.Add(40 * time.Second)
	tBetween := wireBase.Add(60 * time.Second)
	tB := wireBase.Add(80 * time.Second)

	// Establish live row on all stores.
	wireMustWrite(t, a, wireUpsert("writer-A", "sync-A", topic, "initial content", 1, tInit))
	wireSyncRounds(ctx, t, []*spike.Node{a, b}, central, 2)
	if wireLiveTopicOnNode(t, a, topic) == nil {
		t.Fatalf("precondition: topic not live on A")
	}
	if wireLiveTopicOnNode(t, b, topic) == nil {
		t.Fatalf("precondition: topic not live on B")
	}

	// Author two competing deletes.
	wireMustWrite(t, a, wireDel("writer-A", "sync-A", topic, 2, tA))
	wireMustWrite(t, b, wireDel("writer-B", "sync-B", topic, 3, tB))

	// Push in order: A's older delete first.
	if _, err := wirePush(ctx, t, a, central); err != nil {
		t.Fatalf("push A (older delete): %v", err)
	}
	sA := wireMaxCentralSeq(ctx, t, store)

	if _, err := wirePush(ctx, t, b, central); err != nil {
		t.Fatalf("push B (newer delete): %v", err)
	}
	sB := wireMaxCentralSeq(ctx, t, store)

	if !(sA < sB) {
		t.Fatalf("forced ordering not achieved: need sA < sB, got %d >= %d", sA, sB)
	}

	// Pull in order: A pulls B's newer delete; B pulls A's older delete.
	if _, err := wirePull(ctx, t, a, central); err != nil {
		t.Fatalf("pull A: %v", err)
	}
	if _, err := wirePull(ctx, t, b, central); err != nil {
		t.Fatalf("pull B: %v", err)
	}
	wireSyncRounds(ctx, t, []*spike.Node{a, b}, central, 3)

	// Tombstone metadata must converge to the NEWER delete's values.
	aDA, aVer, aBy := wireLocalTombstoneMeta(t, a, topic)
	bDA, bVer, bBy := wireLocalTombstoneMeta(t, b, topic)
	t.Logf("A tombstone: deleted_at=%v version=%d deleted_by=%s", aDA, aVer, aBy)
	t.Logf("B tombstone: deleted_at=%v version=%d deleted_by=%s", bDA, bVer, bBy)

	if aDA.IsZero() {
		t.Fatalf("A.local: no tombstone found")
	}
	if bDA.IsZero() {
		t.Fatalf("B.local: no tombstone found")
	}
	if !aDA.Equal(bDA) {
		t.Errorf("TOMBSTONE METADATA DIVERGENCE wire: A.deleted_at=%v B.deleted_at=%v (want tB=%v both)", aDA, bDA, tB)
	}
	if aBy != bBy {
		t.Errorf("TOMBSTONE METADATA DIVERGENCE wire: A.deleted_by=%q B.deleted_by=%q (want writer-B both)", aBy, bBy)
	}
	if aVer != bVer {
		t.Errorf("TOMBSTONE METADATA DIVERGENCE wire: A.version=%d B.version=%d (want 3 both)", aVer, bVer)
	}
	if !aDA.Equal(tB) {
		t.Errorf("WRONG WINNER wire: A tombstone deleted_at=%v; want tB=%v", aDA, tB)
	}
	if aBy != "writer-B" {
		t.Errorf("WRONG WINNER wire: A tombstone deleted_by=%q; want writer-B", aBy)
	}
	if aVer != 3 {
		t.Errorf("WRONG WINNER wire: A tombstone version=%d; want 3", aVer)
	}

	// Resurrection probe: upsert at tBetween (between tA and tB) must be BLOCKED
	// on all stores — the winning tombstone at tB is newer.
	c := newWireNode(t, "C")
	wireMustWrite(t, c, wireUpsert("writer-C", "sync-C", topic, "C resurrection attempt", 4, tBetween))
	wireSyncRounds(ctx, t, []*spike.Node{a, b, c}, central, 4)

	aLive := wireLiveTopicOnNode(t, a, topic) != nil
	bLive := wireLiveTopicOnNode(t, b, topic) != nil
	cLive := wireLiveTopicOnNode(t, c, topic) != nil
	ctrLive := wireLiveTopicOnCentral(t, store, topic) != nil

	t.Logf("resurrection probe: A=%v B=%v C=%v central=%v", aLive, bLive, cLive, ctrLive)

	if !(aLive == bLive && bLive == cLive && cLive == ctrLive) {
		t.Errorf("LIVE/DELETED SPLIT-BRAIN wire: A=%v B=%v C=%v central=%v", aLive, bLive, cLive, ctrLive)
	}
	if aLive || bLive || cLive || ctrLive {
		t.Errorf("WRONG STATE wire: upsert at tBetween must be BLOCKED by winning tombstone at tB")
	}
}

// ── Infrastructure helpers ────────────────────────────────────────────────────

func wireWithSearchPath(dsn, schema string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("wireWithSearchPath: parse DSN: %w", err)
		}
		q := u.Query()
		q.Set("options", fmt.Sprintf("-c search_path=%s,public", schema))
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	return fmt.Sprintf("%s options='-c search_path=%s,public'", dsn, schema), nil
}

func wireSchemaFor(t *testing.T) string {
	t.Helper()
	raw := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, t.Name())
	if len(raw) > 40 {
		raw = raw[:40]
	}
	return strings.ToLower("w_" + raw)
}

func wireFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func wireCacheRoot() string {
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		return filepath.Join(gopath, "pkg", "mod", "cache", "embeddedpg")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		if runtime.GOOS == "windows" {
			return filepath.Join("C:\\", "embeddedpg")
		}
		return "/tmp/embeddedpg"
	}
	return filepath.Join(home, ".cache", "embeddedpg")
}
