//go:build acceptance

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
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
	"github.com/mariesqu/engram/internal/mutation"
	"github.com/mariesqu/engram/internal/remote"
)

// ── Package-level embedded-postgres harness (mirrors centralstore pattern) ───

var (
	pgStartOnce sync.Once
	pgDSN       string
	pgEP        *embeddedpostgres.EmbeddedPostgres
)

// TestMain starts embedded-postgres once per package run and stops it when all
// tests finish. Mirrors the pattern from centralstore/store_acceptance_test.go.
func TestMain(m *testing.M) {
	pgStartOnce.Do(func() {
		if dsn := os.Getenv("ENGRAM_TEST_PG_DSN"); dsn != "" {
			pgDSN = dsn
			return
		}

		port, err := freePort()
		if err != nil {
			panic("cmd/engram acceptance: could not find free port for postgres: " + err.Error())
		}

		cacheDir := cacheRoot()
		runtimeDir := filepath.Join(os.TempDir(), fmt.Sprintf("engram-cmd-epg-%d", port))

		cfg := embeddedpostgres.DefaultConfig().
			Port(uint32(port)).
			Database("engram_test").
			Username("engram").
			Password("engram").
			CachePath(cacheDir).
			RuntimePath(runtimeDir)

		ep := embeddedpostgres.NewDatabase(cfg)
		if err := ep.Start(); err != nil {
			panic("cmd/engram acceptance: embedded-postgres Start: " + err.Error())
		}
		pgEP = ep
		pgDSN = fmt.Sprintf("host=localhost port=%d user=engram password=engram dbname=engram_test sslmode=disable", port)
	})

	code := m.Run()

	if pgEP != nil {
		if err := pgEP.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "cmd/engram acceptance: embedded-postgres Stop: %v\n", err)
		}
	}

	os.Exit(code)
}

// ── Per-test isolation ────────────────────────────────────────────────────────

// newIsolatedStore returns a *centralstore.Store scoped to a fresh schema
// unique to this test. The schema is dropped on t.Cleanup.
func newIsolatedStore(t *testing.T) *centralstore.Store {
	t.Helper()

	schemaName := schemaFor(t)
	ctx := context.Background()

	adminPool, err := pgxpool.New(ctx, pgDSN)
	if err != nil {
		t.Fatalf("newIsolatedStore: pgxpool.New admin: %v", err)
	}
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE", schemaName)); err != nil {
		t.Fatalf("newIsolatedStore: DROP SCHEMA: %v", err)
	}
	if _, err := adminPool.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %q", schemaName)); err != nil {
		t.Fatalf("newIsolatedStore: CREATE SCHEMA: %v", err)
	}

	dsn, err := withSearchPath(pgDSN, schemaName)
	if err != nil {
		t.Fatalf("newIsolatedStore: withSearchPath: %v", err)
	}

	store, err := centralstore.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("newIsolatedStore: centralstore.Open: %v", err)
	}

	t.Cleanup(func() {
		store.Close()
		cleanPool, err2 := pgxpool.New(ctx, pgDSN)
		if err2 != nil {
			t.Logf("newIsolatedStore cleanup: pgxpool.New: %v (schema %q not dropped)", err2, schemaName)
			return
		}
		defer cleanPool.Close()
		if _, err2 = cleanPool.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE", schemaName)); err2 != nil {
			t.Logf("newIsolatedStore cleanup: DROP SCHEMA %q: %v", schemaName, err2)
		}
	})

	return store
}

// newIsolatedDSN returns a schema-scoped DSN for use in tests that call
// runServe or provisionKey/revokeKey directly (they open their own store).
func newIsolatedDSN(t *testing.T) string {
	t.Helper()

	schemaName := schemaFor(t)
	ctx := context.Background()

	adminPool, err := pgxpool.New(ctx, pgDSN)
	if err != nil {
		t.Fatalf("newIsolatedDSN: pgxpool.New admin: %v", err)
	}
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE", schemaName)); err != nil {
		t.Fatalf("newIsolatedDSN: DROP SCHEMA: %v", err)
	}
	if _, err := adminPool.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %q", schemaName)); err != nil {
		t.Fatalf("newIsolatedDSN: CREATE SCHEMA: %v", err)
	}

	dsn, err := withSearchPath(pgDSN, schemaName)
	if err != nil {
		t.Fatalf("newIsolatedDSN: withSearchPath: %v", err)
	}

	t.Cleanup(func() {
		cleanPool, err2 := pgxpool.New(ctx, pgDSN)
		if err2 != nil {
			t.Logf("newIsolatedDSN cleanup: pgxpool.New: %v (schema %q not dropped)", err2, schemaName)
			return
		}
		defer cleanPool.Close()
		if _, err2 = cleanPool.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE", schemaName)); err2 != nil {
			t.Logf("newIsolatedDSN cleanup: DROP SCHEMA %q: %v", schemaName, err2)
		}
	})

	return dsn
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
	return strings.ToLower("t_" + raw)
}

// ── Keys acceptance tests ─────────────────────────────────────────────────────

// TestAcceptance_ProvisionKey verifies that provisionKey stores a key that can
// be retrieved from the store with WriterKey.
func TestAcceptance_ProvisionKey(t *testing.T) {
	dsn := newIsolatedDSN(t)
	ctx := context.Background()

	key, err := provisionKey(ctx, dsn, "writer-provision-test")
	if err != nil {
		t.Fatalf("provisionKey: %v", err)
	}
	if len(key) == 0 {
		t.Fatal("provisionKey returned empty key")
	}

	// Verify the stored key matches what was returned.
	store, err := centralstore.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open store for verify: %v", err)
	}
	defer store.Close()

	got, err := store.WriterKey(ctx, "writer-provision-test")
	if err != nil {
		t.Fatalf("WriterKey after provision: %v", err)
	}
	if string(got) != string(key) {
		t.Errorf("WriterKey returned different key: got %x, want %x", got, key)
	}
}

// TestAcceptance_RevokeKey verifies that revokeKey deactivates the key so
// WriterKey returns ErrWriterKeyNotFound.
func TestAcceptance_RevokeKey(t *testing.T) {
	dsn := newIsolatedDSN(t)
	ctx := context.Background()

	if _, err := provisionKey(ctx, dsn, "writer-revoke-test"); err != nil {
		t.Fatalf("provisionKey: %v", err)
	}

	if err := revokeKey(ctx, dsn, "writer-revoke-test"); err != nil {
		t.Fatalf("revokeKey: %v", err)
	}

	// After revocation, WriterKey must return ErrWriterKeyNotFound.
	store, err := centralstore.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open store for verify: %v", err)
	}
	defer store.Close()

	_, err = store.WriterKey(ctx, "writer-revoke-test")
	if !errors.Is(err, centralstore.ErrWriterKeyNotFound) {
		t.Errorf("WriterKey after revoke: got %v, want ErrWriterKeyNotFound", err)
	}
}

// TestAcceptance_RevokeUnprovisioned verifies that revoking a writer that was
// never provisioned returns an error (wraps ErrWriterKeyNotFound).
func TestAcceptance_RevokeUnprovisioned(t *testing.T) {
	dsn := newIsolatedDSN(t)
	ctx := context.Background()

	err := revokeKey(ctx, dsn, "never-provisioned-writer")
	if err == nil {
		t.Fatal("revokeKey for unprovisioned writer: expected error, got nil")
	}
	// The error wraps ErrWriterKeyNotFound with a human-readable message.
	if !errors.Is(err, centralstore.ErrWriterKeyNotFound) {
		t.Errorf("revokeKey for unprovisioned writer: error %v does not wrap ErrWriterKeyNotFound", err)
	}
}

// TestAcceptance_ServeE2E is the end-to-end acceptance test for the serve
// subcommand. It:
//  1. Picks a free TCP port.
//  2. Provisions a writer key via provisionKey.
//  3. Starts runServe in a goroutine with a cancellable context.
//  4. Polls until the port accepts connections.
//  5. Builds a signed remote.Client and calls Apply → asserts nil (200).
//  6. Cancels the context and asserts runServe returns within a bounded time
//     (graceful shutdown).
func TestAcceptance_ServeE2E(t *testing.T) {
	dsn := newIsolatedDSN(t)
	ctx := context.Background()

	// Step 1 — provision "writer-A" before starting the server (the server
	// must be able to look up the key on the first request).
	key, err := provisionKey(ctx, dsn, "writer-A")
	if err != nil {
		t.Fatalf("provisionKey writer-A: %v", err)
	}

	// Step 2 — pick a free port for the server.
	port, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// Step 3 — start the server with a cancellable context (NOT a signal context).
	serveCtx, cancelServe := context.WithCancel(context.Background())
	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- runServe(serveCtx, addr, dsn)
	}()

	// Step 4 — poll until the port accepts TCP connections (the server is up).
	deadline := time.Now().Add(15 * time.Second)
	for {
		if time.Now().After(deadline) {
			cancelServe()
			t.Fatal("timed out waiting for server to accept connections")
		}
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Step 5 — build a signed remote.Client and Apply a mutation.
	client := remote.New("http://"+addr, nil, "writer-A", key)

	m := testMutation("sync-serve-e2e", "engram-test", "writer-A")
	if err := client.Apply(ctx, m); err != nil {
		cancelServe()
		t.Fatalf("client.Apply: %v (server must accept a signed request from writer-A)", err)
	}

	// Step 6 — cancel the context and assert graceful shutdown within 5 seconds.
	cancelServe()
	select {
	case err := <-serveErrCh:
		// nil means clean shutdown (Shutdown returned nil); http.ErrServerClosed
		// is also acceptable (it can surface on some platforms).
		if err != nil {
			t.Errorf("runServe after cancel: got error %v, want nil (graceful shutdown)", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("runServe did not shut down within 5 seconds after context cancellation")
	}
}

// ── Utility helpers ───────────────────────────────────────────────────────────

// freePort returns a random free TCP port on localhost.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// testMutation returns a minimal valid domain.Mutation with a correct canonical
// payload and mutation_id (required by the server's VerifyMutationID check).
func testMutation(syncID, project, writerID string) domain.Mutation {
	tk := "cmd/engram/e2e/" + syncID
	m := domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess-e2e",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "E2E test — " + syncID,
		Content:    "acceptance test content",
		Project:    project,
		Scope:      "project",
		TopicKey:   &tk,
		Version:    1,
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
		WriterID:   writerID,
	}
	m.Payload = mutation.CanonicalPayload(m)
	m.MutationID = mutation.NewMutationID(m.Payload)
	return m
}

// cacheRoot returns the directory where embedded-postgres binaries are cached.
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

// withSearchPath appends search_path=<schema> to a Postgres DSN.
// Handles both keyword=value and postgres://... URL forms.
func withSearchPath(dsn, schema string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		return dsn + sep + "search_path=" + schema, nil
	}
	return dsn + " search_path=" + schema, nil
}
