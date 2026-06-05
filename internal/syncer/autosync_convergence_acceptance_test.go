//go:build acceptance

// Package syncer_test — autosync Loop convergence acceptance proof (PR5b keystone).
//
// This test proves that the autosync Loop drives the proven syncer.Sync
// orchestration end-to-end:
//
//   - Two real localstore nodes (A, B) each with their own Loop running against
//     a shared central (cloudserve HTTP + real embedded-postgres centralstore).
//   - A write is made on node A (then Trigger() is optionally called to speed
//     propagation).
//   - The test POLLS until B's local store reflects A's content, bounded by a
//     ~5-second timeout.
//   - After convergence: A.local == B.local == central for the written topic.
//   - Both loops are stopped cleanly at the end (Stop() blocks until exit).
//
// Infrastructure mirrors the proven wire_convergence_acceptance_test pattern:
// one embedded-postgres instance per package (TestMain), each test on a fresh
// schema (isolated per test), each node on a fresh SQLite file (t.TempDir),
// cloudserve exposed via httptest.NewServer.
package syncer_test

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
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
	"github.com/mariesqu/engram/internal/cloudserve"
	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/remote"
	"github.com/mariesqu/engram/internal/syncer"
)

// ── Package-level embedded-postgres harness ──────────────────────────────────

var (
	autoPgOnce sync.Once
	autoPgDSN  string
	autoPgEP   *embeddedpostgres.EmbeddedPostgres
)

// TestMain starts embedded-postgres once for this package (or uses
// ENGRAM_TEST_PG_DSN) and tears it down after all tests finish.
func TestMain(m *testing.M) {
	autoPgOnce.Do(func() {
		if dsn := os.Getenv("ENGRAM_TEST_PG_DSN"); dsn != "" {
			autoPgDSN = dsn
			return
		}

		port, err := autoFreePort()
		if err != nil {
			panic("autosync_test: could not find free port: " + err.Error())
		}

		cacheDir := autoCacheRoot()
		runtimeDir := filepath.Join(os.TempDir(), fmt.Sprintf("engram-auto-epg-%d", port))

		cfg := embeddedpostgres.DefaultConfig().
			Port(uint32(port)).
			Database("engram_auto").
			Username("engram").
			Password("engram").
			CachePath(cacheDir).
			RuntimePath(runtimeDir)

		ep := embeddedpostgres.NewDatabase(cfg)
		if err := ep.Start(); err != nil {
			panic("autosync_test: embedded-postgres Start: " + err.Error())
		}
		autoPgEP = ep
		autoPgDSN = fmt.Sprintf("host=localhost port=%d user=engram password=engram dbname=engram_auto sslmode=disable", port)
	})

	code := m.Run()

	if autoPgEP != nil {
		if err := autoPgEP.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "autosync_test: embedded-postgres Stop: %v\n", err)
		}
	}
	os.Exit(code)
}

// ── Per-test infrastructure ───────────────────────────────────────────────────

// autoStore opens a fresh isolated centralstore.Store on a unique Postgres schema.
// The schema and the store are cleaned up via t.Cleanup.
func autoStore(t *testing.T) *centralstore.Store {
	t.Helper()
	schema := autoSchemaFor(t)
	ctx := context.Background()

	adminPool, err := pgxpool.New(ctx, autoPgDSN)
	if err != nil {
		t.Fatalf("autoStore: pgxpool admin: %v", err)
	}
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE", schema)); err != nil {
		t.Fatalf("autoStore: DROP SCHEMA: %v", err)
	}
	if _, err := adminPool.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %q", schema)); err != nil {
		t.Fatalf("autoStore: CREATE SCHEMA: %v", err)
	}

	dsn, err := autoWithSearchPath(autoPgDSN, schema)
	if err != nil {
		t.Fatalf("autoStore: withSearchPath: %v", err)
	}
	store, err := centralstore.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("autoStore: centralstore.Open: %v", err)
	}

	t.Cleanup(func() {
		store.Close()
		cleanPool, err2 := pgxpool.New(ctx, autoPgDSN)
		if err2 != nil {
			t.Logf("autoStore cleanup: pgxpool: %v", err2)
			return
		}
		defer cleanPool.Close()
		if _, err2 = cleanPool.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE", schema)); err2 != nil {
			t.Logf("autoStore cleanup: DROP SCHEMA %q: %v", schema, err2)
		}
	})
	return store
}

// autoHTTPCentral starts a cloudserve httptest.Server backed by a real
// centralstore.Store and returns a *remote.Client implementing transport.Central
// over HTTP. The server is closed on test cleanup.
func autoHTTPCentral(t *testing.T) (*remote.Client, *centralstore.Store) {
	t.Helper()
	store := autoStore(t)
	srv := httptest.NewServer(cloudserve.New(store, cloudserve.AllowAllVerifier()).Handler())
	t.Cleanup(srv.Close)
	return remote.New(srv.URL, nil), store
}

// autoNode opens a fresh localstore in t.TempDir and wraps it as a syncer.Node.
func autoNode(t *testing.T, name string) *syncer.Node {
	t.Helper()
	dir := t.TempDir()
	st, err := localstore.Open(filepath.Join(dir, name+".db"))
	if err != nil {
		t.Fatalf("autoNode %s: localstore.Open: %v", name, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return syncer.NewNode(name, st)
}

// ── Acceptance proof ─────────────────────────────────────────────────────────

const (
	autoProject = "engram"
	autoScope   = "project"
)

// TestAutoSync_WriteOnA_ReachesB is the PR5b keystone acceptance proof.
//
// It proves that:
//  1. A Loop running against a real HTTP central (cloudserve + embedded-postgres)
//     automatically syncs local writes to central (push) and distributes them to
//     other nodes (pull).
//  2. A write on node A eventually appears on node B without any manual Sync()
//     call — purely driven by the autosync Loop.
//  3. After the Loops are stopped cleanly (Stop() blocks until goroutine exits),
//     A.local, B.local, and central all converge on the same content.
func TestAutoSync_WriteOnA_ReachesB(t *testing.T) {
	ctx := context.Background()

	central, store := autoHTTPCentral(t)
	nodeA := autoNode(t, "A")
	nodeB := autoNode(t, "B")

	// Short Interval so the test completes quickly without needing Trigger on B.
	loopCfg := syncer.Config{
		Interval: 50 * time.Millisecond,
		Debounce: 10 * time.Millisecond,
	}

	loopA := syncer.NewLoop(nodeA, central, autoProject, loopCfg)
	loopB := syncer.NewLoop(nodeB, central, autoProject, loopCfg)

	loopA.Start(ctx)
	loopB.Start(ctx)

	defer func() {
		// Stop both loops cleanly — Stop() blocks until the goroutine exits,
		// so the defer returns only after both goroutines are done.
		loopA.Stop()
		loopB.Stop()
	}()

	// Write a memory on node A.
	topic := "autosync/test/write-on-a-reaches-b"
	tk := topic
	mut := domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     "sync-A-autosync",
		SessionID:  "sess-A",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "autosync test",
		Content:    "written on A, must reach B via Loop",
		Project:    autoProject,
		Scope:      autoScope,
		TopicKey:   &tk,
		Version:    1,
		UpdatedAt:  time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
		WriterID:   "writer-A",
	}

	if _, err := nodeA.Write(mut); err != nil {
		t.Fatalf("nodeA.Write: %v", err)
	}

	// Trigger A's loop for fast propagation (push ASAP instead of waiting for
	// the periodic Interval). B's loop will pull on its own periodic schedule.
	loopA.Trigger()

	// Poll until B has the topic, bounded by a generous 5-second timeout.
	// Using polling (not time.Sleep) so the test finishes as fast as B converges.
	const pollInterval = 20 * time.Millisecond
	const maxWait = 5 * time.Second
	deadline := time.Now().Add(maxWait)

	var bRec *domain.Record
	for time.Now().Before(deadline) {
		rec, err := nodeB.Store.FindByTopic(topic, autoProject, autoScope)
		if err != nil {
			t.Fatalf("nodeB FindByTopic: %v", err)
		}
		if rec != nil {
			bRec = rec
			break
		}
		time.Sleep(pollInterval)
	}

	if bRec == nil {
		t.Fatalf("autosync: topic %q never reached node B within %v", topic, maxWait)
	}

	wantContent := "written on A, must reach B via Loop"
	if bRec.Content != wantContent {
		t.Errorf("B.local content=%q; want %q", bRec.Content, wantContent)
	}
	t.Logf("autosync: topic %q converged on B with content=%q", topic, bRec.Content)

	// Also assert A.local still has the write (it authored it).
	aRec, err := nodeA.Store.FindByTopic(topic, autoProject, autoScope)
	if err != nil {
		t.Fatalf("nodeA FindByTopic: %v", err)
	}
	if aRec == nil {
		t.Fatal("autosync: topic missing from A.local after write+sync")
	}
	if aRec.Content != wantContent {
		t.Errorf("A.local content=%q; want %q", aRec.Content, wantContent)
	}

	// And central must have it too.
	cRec, err := store.FindByTopic(topic, autoProject, autoScope)
	if err != nil {
		t.Fatalf("central FindByTopic: %v", err)
	}
	if cRec == nil {
		t.Fatal("autosync: topic missing from central after sync")
	}
	if cRec.Content != wantContent {
		t.Errorf("central content=%q; want %q", cRec.Content, wantContent)
	}

	t.Logf("autosync convergence: A.local=%q B.local=%q central=%q — all match",
		aRec.Content, bRec.Content, cRec.Content)
}

// ── Infrastructure helpers ────────────────────────────────────────────────────

func autoWithSearchPath(dsn, schema string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("autoWithSearchPath: parse DSN: %w", err)
		}
		q := u.Query()
		q.Set("options", fmt.Sprintf("-c search_path=%s,public", schema))
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	return fmt.Sprintf("%s options='-c search_path=%s,public'", dsn, schema), nil
}

func autoSchemaFor(t *testing.T) string {
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
	return strings.ToLower("a_" + raw)
}

func autoFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func autoCacheRoot() string {
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
