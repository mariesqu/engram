//go:build acceptance

// Acceptance tests for the cloudserve HTTP server against a real Postgres
// instance via embedded-postgres (or ENGRAM_TEST_PG_DSN if set).
//
// Each test creates an isolated schema using the same withSearchPath + schema-
// per-test pattern as internal/centralstore. The HTTP server is driven via
// httptest.NewServer(cloudserve.New(store).Handler()) — no network ports, fully
// hermetic.
package cloudserve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
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
	"github.com/mariesqu/engram/internal/mutation"
	"github.com/mariesqu/engram/internal/syncwire"
)

// ── Package-level embedded-postgres harness ───────────────────────────────────
//
// Mirrors internal/centralstore/store_acceptance_test.go: embedded-postgres is
// started once per package run and shared across all tests via pgDSN.

var (
	pgStartOnce sync.Once
	pgDSN       string
	pgEP        *embeddedpostgres.EmbeddedPostgres
)

func TestMain(m *testing.M) {
	pgStartOnce.Do(func() {
		if dsn := os.Getenv("ENGRAM_TEST_PG_DSN"); dsn != "" {
			pgDSN = dsn
			return
		}
		port, err := freePort()
		if err != nil {
			panic("cloudserve_test: could not find free port: " + err.Error())
		}

		cacheDir := cacheRoot()
		runtimeDir := filepath.Join(os.TempDir(), fmt.Sprintf("engram-cloudserve-epg-%d", port))

		cfg := embeddedpostgres.DefaultConfig().
			Port(uint32(port)).
			Database("engram_test").
			Username("engram").
			Password("engram").
			CachePath(cacheDir).
			RuntimePath(runtimeDir)

		ep := embeddedpostgres.NewDatabase(cfg)
		if err := ep.Start(); err != nil {
			panic("cloudserve_test: embedded-postgres Start: " + err.Error())
		}
		pgEP = ep
		pgDSN = fmt.Sprintf("host=localhost port=%d user=engram password=engram dbname=engram_test sslmode=disable", port)
	})

	code := m.Run()

	if pgEP != nil {
		if err := pgEP.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "cloudserve_test: embedded-postgres Stop: %v\n", err)
		}
	}

	os.Exit(code)
}

// ── Per-test helpers ──────────────────────────────────────────────────────────

// newIsolatedStore opens a *centralstore.Store in a fresh per-test schema.
// The schema (and all objects) is dropped on t.Cleanup.
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

// newTestHTTPServer returns an httptest.Server backed by a real centralstore.
func newTestHTTPServer(t *testing.T, store *centralstore.Store) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(cloudserve.New(store).Handler())
	t.Cleanup(ts.Close)
	return ts
}

// buildPushBody constructs a valid PushRequest body from a domain.Mutation.
// The mutation's Payload and MutationID are derived deterministically.
func buildPushBody(t *testing.T, m domain.Mutation) ([]byte, string) {
	t.Helper()
	payload := mutation.CanonicalPayload(m)
	m.Payload = payload
	m.MutationID = mutation.NewMutationID(payload)
	wire := syncwire.ToWire(m)
	req := syncwire.PushRequest{Mutation: wire}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("buildPushBody: marshal: %v", err)
	}
	return b, m.MutationID
}

// ── Acceptance tests ──────────────────────────────────────────────────────────

// TestAcceptance_Push_LandsInCentral pushes a mutation via POST /v1/push,
// asserts 200, then verifies the mutation is present in the central store by
// querying FindByTopic.
func TestAcceptance_Push_LandsInCentral(t *testing.T) {
	store := newIsolatedStore(t)
	ts := newTestHTTPServer(t, store)

	topicKey := "acceptance/push/topic-1"
	m := domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     "sync-acc-push-1",
		SessionID:  "sess-acc-1",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "Acceptance push test",
		Content:    "push content acceptance",
		Project:    "acc-project",
		Scope:      "project",
		TopicKey:   &topicKey,
		Version:    1,
		WriterID:   "writer-acc",
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
	}

	body, mutID := buildPushBody(t, m)
	resp, err := http.Post(ts.URL+"/v1/push", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/push: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push status = %d, want 200", resp.StatusCode)
	}

	var pushResp syncwire.PushResponse
	if err := json.NewDecoder(resp.Body).Decode(&pushResp); err != nil {
		t.Fatalf("decode PushResponse: %v", err)
	}
	if pushResp.Status != "ok" {
		t.Errorf("PushResponse.Status = %q, want %q", pushResp.Status, "ok")
	}
	if pushResp.MutationID != mutID {
		t.Errorf("PushResponse.MutationID = %q, want %q", pushResp.MutationID, mutID)
	}

	// Verify the mutation landed in central: FindByTopic must return the record.
	rec, err := store.FindByTopic(topicKey, "acc-project", "project")
	if err != nil {
		t.Fatalf("FindByTopic: %v", err)
	}
	if rec == nil {
		t.Fatal("FindByTopic: expected record to land in central, got nil")
	}
	if rec.SyncID != "sync-acc-push-1" {
		t.Errorf("record SyncID = %q, want %q", rec.SyncID, "sync-acc-push-1")
	}
	if rec.Title != m.Title {
		t.Errorf("record Title = %q, want %q", rec.Title, m.Title)
	}
}

// TestAcceptance_PushThenPull_RoundTrip pushes a mutation, then pulls it back
// and verifies content and mutation_id survive the full wire round-trip.
func TestAcceptance_PushThenPull_RoundTrip(t *testing.T) {
	store := newIsolatedStore(t)
	ts := newTestHTTPServer(t, store)

	topicKey := "acceptance/round-trip/topic-1"
	m := domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     "sync-acc-rt-1",
		SessionID:  "sess-acc-rt-1",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "Round-trip title",
		Content:    "round-trip content",
		Project:    "rt-project",
		Scope:      "project",
		TopicKey:   &topicKey,
		Version:    1,
		WriterID:   "writer-rt",
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
	}

	// Push.
	pushBody, mutID := buildPushBody(t, m)
	pushResp, err := http.Post(ts.URL+"/v1/push", "application/json", bytes.NewReader(pushBody))
	if err != nil {
		t.Fatalf("POST /v1/push: %v", err)
	}
	pushResp.Body.Close()
	if pushResp.StatusCode != http.StatusOK {
		t.Fatalf("push status = %d, want 200", pushResp.StatusCode)
	}

	// Pull since seq=0.
	pullReq := syncwire.PullRequest{Project: "rt-project", SinceSeq: 0}
	pullBody, _ := json.Marshal(pullReq)
	pullHTTP, err := http.Post(ts.URL+"/v1/pull", "application/json", bytes.NewReader(pullBody))
	if err != nil {
		t.Fatalf("POST /v1/pull: %v", err)
	}
	defer pullHTTP.Body.Close()

	if pullHTTP.StatusCode != http.StatusOK {
		t.Fatalf("pull status = %d, want 200", pullHTTP.StatusCode)
	}

	var pr syncwire.PullResponse
	if err := json.NewDecoder(pullHTTP.Body).Decode(&pr); err != nil {
		t.Fatalf("decode PullResponse: %v", err)
	}
	if len(pr.Mutations) == 0 {
		t.Fatal("pull returned no mutations — push must have landed before pull")
	}

	// Round-trip via FromWire.
	got, err := syncwire.FromWire(pr.Mutations[0])
	if err != nil {
		t.Fatalf("FromWire: %v", err)
	}

	// ToWire now CARRIES m.MutationID (the authoritative central_mutations.mutation_id
	// column value) rather than recomputing from the JSONB-normalized payload bytes.
	// The pulled mutation_id must therefore equal the one we originally pushed — this
	// is the red→green proof: under always-recompute the pulled id was the JSONB-
	// normalized payload hash (differing from mutID); under carry-original it matches.
	if got.MutationID != mutID {
		t.Errorf("pulled mutation_id = %q, want pushed id %q (carry-original regression)",
			got.MutationID, mutID)
	}

	// Verify the STORED mutation_id (the original hash, from the DB column)
	// is the one we pushed — MutationApplied checks central_mutations by this ID.
	applied, err := store.MutationApplied(mutID)
	if err != nil {
		t.Fatalf("MutationApplied: %v", err)
	}
	if !applied {
		t.Errorf("MutationApplied(%q) = false after push; want true", mutID)
	}

	// Content fields must survive the round-trip exactly.
	if got.Title != m.Title {
		t.Errorf("title round-trip: got %q, want %q", got.Title, m.Title)
	}
	if got.Content != m.Content {
		t.Errorf("content round-trip: got %q, want %q", got.Content, m.Content)
	}
	if got.Project != m.Project {
		t.Errorf("project round-trip: got %q, want %q", got.Project, m.Project)
	}
	if got.Seq <= 0 {
		t.Errorf("pulled mutation has seq=%d, want > 0", got.Seq)
	}
}

// TestAcceptance_IdempotentPush pushes the same mutation twice and asserts:
//   - both pushes return 200
//   - central has exactly one row for the topic
func TestAcceptance_IdempotentPush(t *testing.T) {
	store := newIsolatedStore(t)
	ts := newTestHTTPServer(t, store)

	topicKey := "acceptance/idempotent/topic-1"
	m := domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     "sync-acc-idem-1",
		SessionID:  "sess-acc-idem-1",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "Idempotent push",
		Content:    "idem content",
		Project:    "idem-project",
		Scope:      "project",
		TopicKey:   &topicKey,
		Version:    1,
		WriterID:   "writer-idem",
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
	}

	body, _ := buildPushBody(t, m)

	for i := range 2 {
		resp, err := http.Post(ts.URL+"/v1/push", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("push #%d: %v", i+1, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("push #%d: status = %d, want 200", i+1, resp.StatusCode)
		}
	}

	// Exactly one live row in central.
	ctx := context.Background()
	var count int
	if err := store.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM central_memories
		 WHERE topic_key=$1 AND project=$2 AND scope=$3 AND deleted_at IS NULL`,
		topicKey, "idem-project", "project",
	).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 live row after idempotent push, got %d", count)
	}
}

// ── DSN helpers (mirroring centralstore/dsn_acceptance_test.go) ───────────────

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
	return strings.ToLower("cs_" + raw)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

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
