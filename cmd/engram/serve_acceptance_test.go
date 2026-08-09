//go:build acceptance

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mariesqu/engram/internal/centralstore"
	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/mutation"
	"github.com/mariesqu/engram/internal/remote"
	"github.com/mariesqu/engram/internal/wireauth"
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
//  1. Provisions a writer key via provisionKey.
//  2. Picks a free TCP port.
//  3. Starts runServe in a goroutine with a cancellable context.
//  4. Polls until the port accepts connections.
//  5. Builds a signed remote.Client and calls Apply → asserts nil (200).
//  6. Asserts a WRONG-key client gets 401 (proves serve enforces real auth).
//  7. Cancels the context and asserts runServe returns within a bounded time
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
		// Fail fast if runServe exited early (e.g. centralstore.Open or
		// ListenAndServe failed) instead of waiting out the full deadline. The read
		// is non-blocking: during normal startup serveErrCh is empty, so it is not
		// consumed and the Step 7 graceful-shutdown read still observes the value.
		select {
		case err := <-serveErrCh:
			t.Fatalf("runServe exited before accepting connections: %v", err)
		default:
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

	// Step 5b — NEGATIVE: a client signing with the WRONG key must be rejected (401).
	// This proves the binary's serve genuinely ENFORCES auth (NewKeyVerifier): a
	// signed-request-succeeds check alone would also pass under an open AllowAll
	// server, so this guards against a serve.go → AllowAll regression.
	wrongKey, err := wireauth.NewKey()
	if err != nil {
		cancelServe()
		t.Fatalf("wireauth.NewKey: %v", err)
	}
	badClient := remote.New("http://"+addr, nil, "writer-A", wrongKey)
	badM := testMutation("sync-serve-e2e-bad", "engram-test", "writer-A")
	if applyErr := badClient.Apply(ctx, badM); applyErr == nil {
		cancelServe()
		t.Fatal("wrong-key Apply: got nil; want 401 (serve must ENFORCE auth, not run open)")
	} else {
		var se *remote.StatusError
		if !errors.As(applyErr, &se) || se.Code != http.StatusUnauthorized {
			cancelServe()
			t.Fatalf("wrong-key Apply: got %v; want *remote.StatusError with Code 401", applyErr)
		}
	}

	// Step 6 — cancel the context and assert graceful shutdown within 5 seconds.
	cancelServe()
	select {
	case err := <-serveErrCh:
		// runServe → cloudserve.Run returns Shutdown's result on ctx cancel: nil on a
		// clean graceful shutdown. (Run filters http.ErrServerClosed from
		// ListenAndServe internally, so it never surfaces here.)
		if err != nil {
			t.Errorf("runServe after cancel: got error %v, want nil (graceful shutdown)", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("runServe did not shut down within 5 seconds after context cancellation")
	}
}

// ── Real-Postgres proof for the retry/fatal-error classification ──────────────
//
// serve_test.go's unit tests exercise shouldRetryOpenErr/retryOpen against
// HAND-CONSTRUCTED *pgconn.PgError values and fake sleep/openFn seams. That
// proves the classification logic in isolation, but not that errors.As
// actually reaches a *pgconn.PgError through the REAL wrap chain pgx produces
// when a genuine connection attempt fails or later succeeds. These two tests
// close that gap against the package's real embedded Postgres.

// TestAcceptance_RunServe_WrongPassword_FailsFast proves that a wrong-password
// DSN against the REAL running embedded Postgres surfaces a genuine
// *pgconn.PgError (SQLState class 28, invalid_password/invalid_authorization)
// through pgx's actual wrap chain, and that shouldRetryOpenErr classifies it
// fatal: runServe must return quickly with a non-nil auth error instead of
// looping the 5s/60s backoff schedule.
func TestAcceptance_RunServe_WrongPassword_FailsFast(t *testing.T) {
	badDSN, err := withWrongPassword(pgDSN)
	if err != nil {
		t.Fatalf("withWrongPassword: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	runErr := runServe(ctx, "127.0.0.1:0", badDSN)
	elapsed := time.Since(start)

	if runErr == nil {
		t.Fatal("runServe with wrong password: got nil error, want an auth failure")
	}
	if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled) {
		t.Fatalf("runServe with wrong password: got %v — looks like it retried until the test's ctx timeout instead of failing fast", runErr)
	}
	if elapsed > 5*time.Second {
		t.Errorf("runServe with wrong password took %s, want a fast (no-retry) failure well under 5s", elapsed)
	}

	var pgErr *pgconn.PgError
	if !errors.As(runErr, &pgErr) {
		t.Fatalf("runServe with wrong password: error %v does not unwrap to *pgconn.PgError via the real pgx wrap chain", runErr)
	}
	if len(pgErr.Code) < 2 || pgErr.Code[:2] != "28" {
		t.Errorf("runServe with wrong password: PgError.Code = %q, want class 28 (login/authentication failure)", pgErr.Code)
	}
}

// tcpProxy is a minimal raw-byte TCP forwarder. It lets
// TestAcceptance_RetryOpen_RecoversWhenPostgresBecomesReachable simulate
// "Postgres becomes reachable a moment after the caller starts trying"
// without paying the cost of starting a second embedded-postgres instance:
// retryOpen dials a port with NOTHING listening (connection refused —
// retryable) until the proxy starts and forwards to the REAL embedded
// Postgres already running for this package.
type tcpProxy struct {
	backend string // host:port of the real destination
}

func (p *tcpProxy) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed — test cleanup
		}
		go p.handle(conn)
	}
}

func (p *tcpProxy) handle(conn net.Conn) {
	defer conn.Close()
	backendConn, err := net.Dial("tcp", p.backend)
	if err != nil {
		return
	}
	defer backendConn.Close()

	done := make(chan struct{}, 2)
	go func() { io.Copy(backendConn, conn); done <- struct{}{} }()
	go func() { io.Copy(conn, backendConn); done <- struct{}{} }()
	<-done
}

// TestAcceptance_RetryOpen_RecoversWhenPostgresBecomesReachable proves the
// end-to-end recovery path this whole retry mechanism exists for: retryOpen
// starts against an address with NOTHING listening (the exact "laptop
// booting off-network" shape from the incident that motivated this feature),
// receives connection-refused errors classified retryable by
// shouldRetryOpenErr, and recovers into a genuinely working *centralstore.Store
// once the address becomes reachable a short time later — all within a
// bounded test runtime via an injected fast sleepFn (NOT the production
// 5s/60s schedule; that seam is exactly what retryOpen exposes sleepFn for).
func TestAcceptance_RetryOpen_RecoversWhenPostgresBecomesReachable(t *testing.T) {
	backendCfg, err := pgconn.ParseConfig(pgDSN)
	if err != nil {
		t.Fatalf("parse pgDSN: %v", err)
	}
	backend := fmt.Sprintf("%s:%d", backendCfg.Host, backendCfg.Port)

	proxyPort, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}

	schemaName := schemaFor(t)
	adminCtx := context.Background()
	adminPool, err := pgxpool.New(adminCtx, pgDSN)
	if err != nil {
		t.Fatalf("pgxpool.New admin: %v", err)
	}
	if _, err := adminPool.Exec(adminCtx, fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE", schemaName)); err != nil {
		adminPool.Close()
		t.Fatalf("DROP SCHEMA: %v", err)
	}
	if _, err := adminPool.Exec(adminCtx, fmt.Sprintf("CREATE SCHEMA %q", schemaName)); err != nil {
		adminPool.Close()
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	adminPool.Close()
	t.Cleanup(func() {
		cleanCtx := context.Background()
		cleanPool, err := pgxpool.New(cleanCtx, pgDSN)
		if err != nil {
			return
		}
		defer cleanPool.Close()
		_, _ = cleanPool.Exec(cleanCtx, fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE", schemaName))
	})

	// dsn points at the proxy port (nothing listening there yet), reusing the
	// real credentials/database against the isolated schema above.
	dsn := fmt.Sprintf(
		"host=127.0.0.1 port=%d user=%s password=%s dbname=%s sslmode=disable options='-c search_path=%s,public'",
		proxyPort, backendCfg.User, backendCfg.Password, backendCfg.Database, schemaName,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var store *centralstore.Store
	openFn := func() error {
		s, err := centralstore.Open(ctx, dsn)
		if err != nil {
			return err
		}
		store = s
		return nil
	}

	// Short, test-only backoff — the production 5s/60s schedule would blow the
	// test's runtime budget several times over.
	fastSleep := func(ctx context.Context, _ time.Duration) error {
		return sleepCtx(ctx, 50*time.Millisecond)
	}

	// Start the proxy AFTER a short delay so retryOpen's first several
	// attempts observe "connection refused", proving the recovery path (not
	// just first-try success).
	go func() {
		time.Sleep(300 * time.Millisecond)
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort))
		if err != nil {
			t.Errorf("proxy listen: %v", err)
			return
		}
		t.Cleanup(func() { ln.Close() })
		(&tcpProxy{backend: backend}).serve(ln)
	}()

	start := time.Now()
	err = retryOpen(ctx, openFn, fastSleep, shouldRetryOpenErr, noopLogf)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("retryOpen did not recover: %v (elapsed %s)", err, elapsed)
	}
	if store == nil {
		t.Fatal("retryOpen succeeded but produced a nil store")
	}
	defer store.Close()

	if elapsed > 8*time.Second {
		t.Errorf("retryOpen took %s to recover, want well under the 10s test budget", elapsed)
	}

	// Prove the recovered store is genuinely usable, not just "no error".
	if _, err := store.WriterKey(ctx, "nonexistent-writer-recovery-check"); !errors.Is(err, centralstore.ErrWriterKeyNotFound) {
		t.Errorf("WriterKey sanity check on recovered store: got %v, want ErrWriterKeyNotFound (store not genuinely connected)", err)
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

// withSearchPath returns a DSN with its PostgreSQL search_path set to
// "<schema>,public" (mirrors internal/centralstore/dsn_acceptance_test.go).
//
// Two DSN forms are handled:
//   - URL-form (scheme "postgres://" / "postgresql://"): the "options" query
//     parameter is set to "-c search_path=<schema>,public" (URL-encoded).
//   - Keyword/value form (everything else): the options string is appended as a
//     space-separated key=value pair — the format embedded-postgres produces.
//
// An error is returned only when a URL-form DSN cannot be parsed by net/url.
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
	return fmt.Sprintf("%s options='-c search_path=%s,public'", dsn, schema), nil
}

// dsnPasswordRE matches a keyword/value-form "password=<value>" token so
// withWrongPassword can swap it without disturbing the rest of the DSN.
var dsnPasswordRE = regexp.MustCompile(`password=\S+`)

// withWrongPassword returns dsn with its password replaced by an incorrect
// value, leaving host/port/dbname/sslmode untouched. Used by
// TestAcceptance_RunServe_WrongPassword_FailsFast to reach a REAL
// *pgconn.PgError auth failure through pgx's actual wrap chain — a
// hand-constructed *pgconn.PgError (as used in serve_test.go's unit tests)
// cannot prove errors.As unwraps what pgx genuinely produces in production.
// Mirrors withSearchPath's URL-form vs keyword/value-form handling.
func withWrongPassword(dsn string) (string, error) {
	const wrongPassword = "definitely-wrong-password"
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("withWrongPassword: parse DSN: %w", err)
		}
		u.User = url.UserPassword(u.User.Username(), wrongPassword)
		return u.String(), nil
	}
	if dsnPasswordRE.MatchString(dsn) {
		return dsnPasswordRE.ReplaceAllString(dsn, "password="+wrongPassword), nil
	}
	return fmt.Sprintf("%s password=%s", dsn, wrongPassword), nil
}
