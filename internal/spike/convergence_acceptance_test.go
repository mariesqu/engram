//go:build acceptance

// Package spike_test — the TWO-WRITER CONVERGENCE ACCEPTANCE PROOF.
//
// This is the core acceptance proof of the entire core-foundation change. It
// runs TWO independent local SQLite stores (writer A, writer B — each its own
// temp file) against ONE real central Postgres (embedded-postgres, started once
// per package in TestMain) and proves all six reconciliation invariants converge
// end-to-end: after the push/pull rounds, writer A's local store, writer B's
// local store, AND the central store all agree on the canonical state.
//
// Central isolation mirrors the proven centralstore_test pattern: each test gets
// a fresh Postgres schema (unique name, dropped on cleanup) so tests are
// hermetic. Each writer gets a fresh SQLite temp file via t.TempDir().
//
// Invariant coverage:
//
//	INV1 TestConvergence_INV1_TopicConvergence        — one live row per topic on A, B, central
//	INV2 TestConvergence_INV2_MonotonicSeq            — central seq strictly increases across interleaved pushes
//	INV3 TestConvergence_INV3_NoLostUpdate            — older write never clobbers newer after convergence
//	INV4 TestConvergence_INV4_NoResurrection          — stale upsert can't revive a delete; newer upsert does
//	INV5 TestConvergence_INV5_Idempotent              — repeated sync does not double-apply; seq stable on no-op rounds
//	INV6 TestConvergence_INV6_IndependentWrites       — different topics both survive on A, B, central
//	      TestConvergence_FullBidirectionalSettles     — A.local == B.local == central for every record
package spike_test

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mariesqu/engram/internal/centralstore"
	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/spike"
)

// ── Package-level embedded-postgres harness (mirrors centralstore_test) ────────

var (
	pgStartOnce sync.Once
	pgDSN       string
	pgEP        *embeddedpostgres.EmbeddedPostgres // nil when using external DSN
)

// TestMain starts embedded-postgres once for the package (unless
// ENGRAM_TEST_PG_DSN is provided) and stops it after all tests finish.
func TestMain(m *testing.M) {
	pgStartOnce.Do(func() {
		if dsn := os.Getenv("ENGRAM_TEST_PG_DSN"); dsn != "" {
			pgDSN = dsn
			return
		}
		port, err := freePort()
		if err != nil {
			panic("spike_test: could not find free port: " + err.Error())
		}

		// Reuse the shared embedded-postgres binary cache so it is downloaded once
		// across packages. cacheRoot() already ends in "embeddedpg".
		cacheDir := cacheRoot()
		runtimeDir := filepath.Join(os.TempDir(), fmt.Sprintf("engram-spike-epg-%d", port))

		cfg := embeddedpostgres.DefaultConfig().
			Port(uint32(port)).
			Database("engram_spike").
			Username("engram").
			Password("engram").
			CachePath(cacheDir).
			RuntimePath(runtimeDir)

		ep := embeddedpostgres.NewDatabase(cfg)
		if err := ep.Start(); err != nil {
			panic("spike_test: embedded-postgres Start: " + err.Error())
		}
		pgEP = ep
		pgDSN = fmt.Sprintf("host=localhost port=%d user=engram password=engram dbname=engram_spike sslmode=disable", port)
	})

	code := m.Run()

	if pgEP != nil {
		if err := pgEP.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "spike_test: embedded-postgres Stop: %v\n", err)
		}
	}
	os.Exit(code)
}

// newCentral returns a *centralstore.Store on a fresh isolated schema dropped on
// cleanup. Mirrors centralstore_test.newIsolatedStore.
func newCentral(t *testing.T) *centralstore.Store {
	t.Helper()
	schemaName := schemaFor(t)
	ctx := context.Background()

	adminPool, err := pgxpool.New(ctx, pgDSN)
	if err != nil {
		t.Fatalf("newCentral: pgxpool.New admin: %v", err)
	}
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE", schemaName)); err != nil {
		t.Fatalf("newCentral: DROP SCHEMA: %v", err)
	}
	if _, err := adminPool.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %q", schemaName)); err != nil {
		t.Fatalf("newCentral: CREATE SCHEMA: %v", err)
	}

	dsn, err := withSearchPath(pgDSN, schemaName)
	if err != nil {
		t.Fatalf("newCentral: withSearchPath: %v", err)
	}
	store, err := centralstore.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("newCentral: centralstore.Open: %v", err)
	}

	t.Cleanup(func() {
		store.Close()
		cleanPool, err2 := pgxpool.New(ctx, pgDSN)
		if err2 != nil {
			t.Logf("newCentral cleanup: pgxpool.New: %v", err2)
			return
		}
		defer cleanPool.Close()
		if _, err2 = cleanPool.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE", schemaName)); err2 != nil {
			t.Logf("newCentral cleanup: DROP SCHEMA %q: %v", schemaName, err2)
		}
	})
	return store
}

// newNode opens a fresh local SQLite store in a temp dir and wraps it as a spike
// node. Each node owns its own DB file → two nodes are fully independent.
func newNode(t *testing.T, name string) *spike.Node {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name+".db")
	st, err := localstore.Open(path)
	if err != nil {
		t.Fatalf("newNode %s: localstore.Open(%q): %v", name, path, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return spike.NewNode(name, st)
}

// centralStore is a short alias for the central adapter type used throughout the
// acceptance helpers (keeps assertion helper signatures terse).
type centralStore = centralstore.Store

// ── shared constants ───────────────────────────────────────────────────────────

const (
	project = "engram"
	scope   = "project"
)

// base is a fixed reference instant so wall-clock ordering in tests is explicit
// and deterministic (no reliance on time.Now ordering between writes).
var base = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

// upsert builds a topic-keyed upsert mutation for the given writer. Identity
// (Payload, MutationID) is left unset so LocalWrite derives the content-addressed
// ID. seq/occurredAt are assigned by central on push.
func upsert(writer, syncID, topic, content string, version int, at time.Time) domain.Mutation {
	tk := topic
	return domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess-" + writer,
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "title-" + syncID,
		Content:    content,
		Project:    project,
		Scope:      scope,
		TopicKey:   &tk,
		Version:    version,
		UpdatedAt:  at,
		WriterID:   writer,
	}
}

// del builds a topic-keyed delete mutation for the given writer.
func del(writer, syncID, topic string, version int, at time.Time) domain.Mutation {
	tk := topic
	return domain.Mutation{
		Op:         domain.OpDelete,
		SyncID:     syncID,
		SessionID:  "sess-" + writer,
		EntityType: domain.EntityMemory,
		Project:    project,
		Scope:      scope,
		TopicKey:   &tk,
		Version:    version,
		UpdatedAt:  at,
		WriterID:   writer,
	}
}

// ── local-store assertion helpers ──────────────────────────────────────────────

// liveTopicOnNode returns the single live record for a topic on a node's local
// store via the public FindByTopic Reader method, failing if more than one
// exists is implied by content checks elsewhere. Returns nil when none is live.
func liveTopicOnNode(t *testing.T, n *spike.Node, topic string) *domain.Record {
	t.Helper()
	rec, err := n.Store.FindByTopic(topic, project, scope)
	if err != nil {
		t.Fatalf("node %s FindByTopic(%q): %v", n.Name, topic, err)
	}
	return rec
}

// liveTopicOnCentral returns the single live record for a topic on central.
func liveTopicOnCentral(t *testing.T, c *centralstore.Store, topic string) *domain.Record {
	t.Helper()
	rec, err := c.FindByTopic(topic, project, scope)
	if err != nil {
		t.Fatalf("central FindByTopic(%q): %v", topic, err)
	}
	return rec
}

// assertOneLiveEverywhere asserts the spec-correct INV1 convergence: topic
// resolves to EXACTLY ONE live record holding wantContent on writer A's local
// store, writer B's local store, AND central, and the CENTRAL canonical sync_id
// is wantCentralSyncID.
//
// IMPORTANT — what converges vs what does not:
//
//   - Convergence guarantee = topic_key identity + winning CONTENT + single live
//     row per topic per store. topic_key is the portable identity; reads are by
//     topic_key (FindByTopic). This is exactly what the spec INV1 requires:
//     "after full sync both local stores MUST hold exactly ONE row for a given
//     topic_key" with the winning content.
//
//   - The per-replica sync_id surrogate is NOT globally identical. When a node
//     authored the wall-clock-WINNING write under its own sync_id, that row
//     survives locally under that sync_id; the central canonical sync_id (the
//     first row pushed) is rejected as an older NoOp on that node. So the node
//     keeps its own sync_id while still holding the winning content and exactly
//     one row. central holds the canonical sync_id. This is benign: sync_id is a
//     local surrogate, topic_key is the convergence key. (Empirical finding from
//     the spike — see apply-progress.)
//
// We therefore assert: content everywhere, single live row per topic per store,
// and the central canonical sync_id. Node sync_ids are validated for liveness +
// content, not for global equality.
func assertOneLiveEverywhere(t *testing.T, a, b *spike.Node, c *centralstore.Store, topic, wantCentralSyncID, wantContent string) {
	t.Helper()

	aRec := liveTopicOnNode(t, a, topic)
	bRec := liveTopicOnNode(t, b, topic)
	cRec := liveTopicOnCentral(t, c, topic)

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

	// Central canonical sync_id is deterministic (the first row pushed).
	if cRec != nil && cRec.SyncID != wantCentralSyncID {
		t.Errorf("central: canonical sync_id=%q, want %q", cRec.SyncID, wantCentralSyncID)
	}

	// EXACTLY ONE live row per topic on each node's local store (the core INV1
	// single-row guarantee — no duplicate/forked topic identity locally).
	assertNodeLiveCount(t, a, topic, 1)
	assertNodeLiveCount(t, b, topic, 1)
}

// assertNodeLiveCount asserts the number of LIVE (deleted_at IS NULL) rows for a
// topic identity on a node's local SQLite store.
func assertNodeLiveCount(t *testing.T, n *spike.Node, topic string, want int) {
	t.Helper()
	var got int
	if err := n.Store.DB().QueryRow(
		`SELECT count(*) FROM memories
		   WHERE topic_key=? AND project=? AND scope=? AND deleted_at IS NULL`,
		topic, project, scope,
	).Scan(&got); err != nil {
		t.Fatalf("assertNodeLiveCount %s (%q): %v", n.Name, topic, err)
	}
	if got != want {
		t.Errorf("%s: live rows for topic %q = %d, want %d", n.Name, topic, got, want)
	}
}

// ── DSN / port / cache helpers (mirror centralstore_test) ──────────────────────

// withSearchPath returns a DSN that sets search_path to "<schema>,public".
//
// Two DSN forms are handled:
//   - URL-form (scheme "postgres://" or "postgresql://"): the "options" query
//     parameter is set to "-c search_path=<schema>,public" (URL-encoded) and
//     the URL is re-encoded and returned.
//   - Keyword/value form (everything else): the options string is appended as a
//     space-separated key=value pair — the format produced by embedded-postgres
//     and accepted by pgx.
//
// An error is returned only when the URL-form DSN cannot be parsed by net/url.
// NOTE: this duplicates centralstore_test.withSearchPath intentionally — a
// shared testutil extraction is a future cleanup; for now behaviour must match.
func withSearchPath(dsn, schema string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("withSearchPath: parse DSN: %w", err)
		}
		q := u.Query()
		q.Set("options", fmt.Sprintf("-c search_path=%s,public", schema))
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	// Keyword/value form — append options key=value pair.
	return fmt.Sprintf("%s options='-c search_path=%s,public'", dsn, schema), nil
}

// schemaFor derives a valid Postgres identifier from the test name.
func schemaFor(t *testing.T) string {
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
	return strings.ToLower("s_" + raw)
}

// freePort returns a random free TCP port on localhost.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// cacheRoot returns the embedded-postgres binary cache dir (shared with
// centralstore_test so the binary downloads once).
func cacheRoot() string {
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
