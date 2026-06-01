//go:build acceptance

// Package centralstore_test exercises the central Postgres data layer using a
// real Postgres instance.  When ENGRAM_TEST_PG_DSN is set the tests use that
// DSN (CI/CD with an external Postgres); otherwise embedded-postgres is started
// once per package run via TestMain and stopped when all tests complete.
//
// Per-test isolation: each test creates a fresh schema with a unique name,
// sets the search_path, applies the central schema there, and drops the schema
// on t.Cleanup.  This keeps every test hermetic without the overhead of a full
// database create/drop cycle.
package centralstore_test

import (
	"context"
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
)

// ── Package-level embedded-postgres harness ───────────────────────────────────

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
		// Find a free port for embedded-postgres.
		port, err := freePort()
		if err != nil {
			panic("centralstore_test: could not find free port: " + err.Error())
		}

		// Cache/runtime directories under the module cache so the Postgres binary
		// is only downloaded once across test runs.
		cacheDir := filepath.Join(cacheRoot(), "embeddedpg")
		runtimeDir := filepath.Join(os.TempDir(), fmt.Sprintf("engram-epg-%d", port))

		cfg := embeddedpostgres.DefaultConfig().
			Port(uint32(port)).
			Database("engram_test").
			Username("engram").
			Password("engram").
			CachePath(cacheDir).
			RuntimePath(runtimeDir)

		ep := embeddedpostgres.NewDatabase(cfg)
		if err := ep.Start(); err != nil {
			panic("centralstore_test: embedded-postgres Start: " + err.Error())
		}
		pgEP = ep
		pgDSN = fmt.Sprintf("host=localhost port=%d user=engram password=engram dbname=engram_test sslmode=disable", port)
	})

	code := m.Run()

	if pgEP != nil {
		if err := pgEP.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "centralstore_test: embedded-postgres Stop: %v\n", err)
		}
	}

	os.Exit(code)
}

// ── Per-test helpers ─────────────────────────────────────────────────────────

// newIsolatedStore returns a *centralstore.Store whose search_path is set to a
// fresh schema unique to this test. The schema (and all objects in it) is
// dropped on t.Cleanup. Each test thus runs in full isolation.
func newIsolatedStore(t *testing.T) *centralstore.Store {
	t.Helper()

	schemaName := schemaFor(t)
	ctx := context.Background()

	// Open a raw pool to create the schema before calling centralstore.Open,
	// which calls ApplySchema against the pool.
	adminPool, err := pgxpool.New(ctx, pgDSN)
	if err != nil {
		t.Fatalf("newIsolatedStore: pgxpool.New admin: %v", err)
	}
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %q", schemaName)); err != nil {
		t.Fatalf("newIsolatedStore: CREATE SCHEMA: %v", err)
	}

	// Build a DSN that sets search_path to the isolated schema.
	dsn := fmt.Sprintf("%s options='-c search_path=%s,public'", pgDSN, schemaName)

	store, err := centralstore.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("newIsolatedStore: centralstore.Open: %v", err)
	}

	t.Cleanup(func() {
		store.Close()
		// Drop the schema using a new connection (store is already closed).
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

// schemaFor derives a valid Postgres identifier from the test name.
func schemaFor(t *testing.T) string {
	t.Helper()
	// Replace non-alphanumeric characters with underscores; limit to 40 chars.
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

// ── Tests ────────────────────────────────────────────────────────────────────

// TestCentralSchema_IdempotentApply verifies that ApplySchema can be called
// twice on the same pool/schema without returning an error.
func TestCentralSchema_IdempotentApply(t *testing.T) {
	ctx := context.Background()
	schemaName := schemaFor(t)

	adminPool, err := pgxpool.New(ctx, pgDSN)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %q", schemaName)); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	t.Cleanup(func() {
		cleanPool, err2 := pgxpool.New(ctx, pgDSN)
		if err2 != nil {
			return
		}
		defer cleanPool.Close()
		cleanPool.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE", schemaName)) //nolint:errcheck
	})

	dsn := fmt.Sprintf("%s options='-c search_path=%s,public'", pgDSN, schemaName)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New isolated: %v", err)
	}
	defer pool.Close()

	// First apply.
	if err := centralstore.ApplySchema(ctx, pool); err != nil {
		t.Fatalf("first ApplySchema: %v", err)
	}

	// Second apply — must be a no-op, no error.
	if err := centralstore.ApplySchema(ctx, pool); err != nil {
		t.Fatalf("second ApplySchema (idempotency): %v", err)
	}
}

// TestInsertMutation_MonotonicSeq verifies that two successive InsertMutation
// calls return strictly increasing BIGSERIAL seq values (INV2).
func TestInsertMutation_MonotonicSeq(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	m1 := testMutation("mut-seq-1", "sync-a", "proj", domain.OpUpsert)
	m2 := testMutation("mut-seq-2", "sync-b", "proj", domain.OpUpsert)

	seq1, err := store.InsertMutation(ctx, m1)
	if err != nil {
		t.Fatalf("InsertMutation m1: %v", err)
	}
	seq2, err := store.InsertMutation(ctx, m2)
	if err != nil {
		t.Fatalf("InsertMutation m2: %v", err)
	}

	if seq2 <= seq1 {
		t.Errorf("seq not monotonic: seq1=%d seq2=%d (want seq2 > seq1)", seq1, seq2)
	}
}

// TestMutationApplied_AfterInsert verifies that MutationApplied returns true
// after a mutation_id has been inserted into central_mutations.
func TestMutationApplied_AfterInsert(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	m := testMutation("mut-applied-1", "sync-applied-a", "proj", domain.OpUpsert)

	applied, err := store.MutationApplied("mut-applied-1")
	if err != nil {
		t.Fatalf("MutationApplied before insert: %v", err)
	}
	if applied {
		t.Fatal("MutationApplied: want false before insert, got true")
	}

	if _, err = store.InsertMutation(ctx, m); err != nil {
		t.Fatalf("InsertMutation: %v", err)
	}

	applied, err = store.MutationApplied("mut-applied-1")
	if err != nil {
		t.Fatalf("MutationApplied after insert: %v", err)
	}
	if !applied {
		t.Fatal("MutationApplied: want true after insert, got false")
	}
}

// TestReaderRoundtrip_FindByTopic verifies that UpsertMemory followed by
// FindByTopic returns the correct live record.
func TestReaderRoundtrip_FindByTopic(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	tk := "sdd/test/topic"
	m := testMutationWithTopic("mut-fbt-1", "sync-fbt-1", "proj", "scp", tk, domain.OpUpsert)

	if err := store.UpsertMemory(ctx, m, 1); err != nil {
		t.Fatalf("UpsertMemory: %v", err)
	}

	got, err := store.FindByTopic(tk, "proj", "scp")
	if err != nil {
		t.Fatalf("FindByTopic: %v", err)
	}
	if got == nil {
		t.Fatal("FindByTopic: expected record, got nil")
	}
	if got.SyncID != "sync-fbt-1" {
		t.Errorf("FindByTopic: SyncID=%q want %q", got.SyncID, "sync-fbt-1")
	}
	if got.Title != m.Title {
		t.Errorf("FindByTopic: Title=%q want %q", got.Title, m.Title)
	}
}

// TestReaderRoundtrip_FindBySyncID verifies that FindBySyncID returns a live
// record, and also returns a soft-deleted record (deleted_at set).
func TestReaderRoundtrip_FindBySyncID(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	m := testMutation("mut-fbs-1", "sync-fbs-1", "proj", domain.OpUpsert)

	if err := store.UpsertMemory(ctx, m, 1); err != nil {
		t.Fatalf("UpsertMemory: %v", err)
	}

	// Live row — FindBySyncID must return it.
	got, err := store.FindBySyncID("sync-fbs-1")
	if err != nil {
		t.Fatalf("FindBySyncID live: %v", err)
	}
	if got == nil {
		t.Fatal("FindBySyncID live: expected record, got nil")
	}
	if got.SyncID != "sync-fbs-1" {
		t.Errorf("FindBySyncID live: SyncID=%q", got.SyncID)
	}

	// Soft-delete via WriteTombstone (sets deleted_at on central_memories).
	md := testMutation("mut-fbs-del", "sync-fbs-1", "proj", domain.OpDelete)
	if err = store.WriteTombstone(ctx, md); err != nil {
		t.Fatalf("WriteTombstone: %v", err)
	}
	// Manually set deleted_at on the memory row (WriteTombstone only creates the
	// tombstone row; the full Decide-driven apply in PR3b does both).
	if _, err = store.Pool().Exec(ctx,
		"UPDATE central_memories SET deleted_at = now() WHERE sync_id = $1",
		"sync-fbs-1",
	); err != nil {
		t.Fatalf("SET deleted_at: %v", err)
	}

	// FindBySyncID MUST return soft-deleted rows too (PR3b pull-apply needs them).
	got, err = store.FindBySyncID("sync-fbs-1")
	if err != nil {
		t.Fatalf("FindBySyncID soft-deleted: %v", err)
	}
	if got == nil {
		t.Fatal("FindBySyncID soft-deleted: expected record (including soft-deleted), got nil")
	}
	if got.DeletedAt == nil {
		t.Error("FindBySyncID soft-deleted: expected DeletedAt set, got nil")
	}
}

// TestReaderRoundtrip_FindTombstone verifies that WriteTombstone followed by
// FindTombstone (by sync_id and by topic_key) returns the tombstone.
func TestReaderRoundtrip_FindTombstone(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	tk := "sdd/test/tombstone"
	m := testMutationWithTopic("mut-ts-1", "sync-ts-1", "proj", "scp", tk, domain.OpDelete)

	if err := store.WriteTombstone(ctx, m); err != nil {
		t.Fatalf("WriteTombstone: %v", err)
	}

	// Lookup by sync_id.
	ts, err := store.FindTombstone("sync-ts-1", nil, "proj", "scp")
	if err != nil {
		t.Fatalf("FindTombstone by sync_id: %v", err)
	}
	if ts == nil {
		t.Fatal("FindTombstone by sync_id: expected tombstone, got nil")
	}
	if ts.SyncID != "sync-ts-1" {
		t.Errorf("FindTombstone by sync_id: SyncID=%q", ts.SyncID)
	}

	// Lookup by topic_key.
	ts2, err := store.FindTombstone("nonexistent", &tk, "proj", "scp")
	if err != nil {
		t.Fatalf("FindTombstone by topic_key: %v", err)
	}
	if ts2 == nil {
		t.Fatal("FindTombstone by topic_key: expected tombstone, got nil")
	}
	if ts2.SyncID != "sync-ts-1" {
		t.Errorf("FindTombstone by topic_key: SyncID=%q", ts2.SyncID)
	}
}

// TestPartialUniqueIndex_RejectsSecondLiveRow verifies the partial UNIQUE INDEX
// central_memories_topic_uidx enforces INV-A at the DB level: inserting a
// second LIVE row for the same (topic_key, project, scope) must fail with a
// unique-violation error.
func TestPartialUniqueIndex_RejectsSecondLiveRow(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	tk := "sdd/test/unique"
	m1 := testMutationWithTopic("mut-uniq-1", "sync-uniq-1", "proj", "scp", tk, domain.OpUpsert)
	m2 := testMutationWithTopic("mut-uniq-2", "sync-uniq-2", "proj", "scp", tk, domain.OpUpsert)

	if err := store.UpsertMemory(ctx, m1, 1); err != nil {
		t.Fatalf("UpsertMemory first live row: %v", err)
	}

	// Inserting a second live row with the SAME topic_key/project/scope must fail.
	// UpsertMemory uses ON CONFLICT (sync_id) — it won't trigger the topic partial
	// unique on a different sync_id. We use a raw INSERT to directly test the index.
	_, err := store.Pool().Exec(ctx, `
		INSERT INTO central_memories
		  (sync_id, entity_type, type, title, content, project, scope, topic_key,
		   version, seq, writer_id, created_by)
		VALUES ($1,'memory','manual','title2','content2',$2,$3,$4,1,2,'w','w')`,
		m2.SyncID, m2.Project, m2.Scope, m2.TopicKey,
	)
	if err == nil {
		t.Fatal("second live row with same topic_key/project/scope: expected unique-violation error, got nil")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "unique") && !strings.Contains(errMsg, "duplicate") &&
		!strings.Contains(errMsg, "central_memories_topic_uidx") {
		t.Errorf("expected unique-violation error; got: %v", err)
	}
}

// ── Utility helpers ───────────────────────────────────────────────────────────

func testMutation(mutID, syncID, project string, op domain.Op) domain.Mutation {
	return domain.Mutation{
		MutationID: mutID,
		Op:         op,
		SyncID:     syncID,
		SessionID:  "sess-test",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "Test Memory " + syncID,
		Content:    "content for " + syncID,
		Project:    project,
		Scope:      "project",
		Version:    1,
		Seq:        0,
		WriterID:   "writer-test",
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
		Payload:    []byte(`{}`),
	}
}

func testMutationWithTopic(mutID, syncID, project, scope, topicKey string, op domain.Op) domain.Mutation {
	m := testMutation(mutID, syncID, project, op)
	m.Scope = scope
	m.TopicKey = &topicKey
	return m
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

// cacheRoot returns the directory where embedded-postgres binaries are cached.
// Uses GOPATH/pkg/mod/cache or a sensible fallback.
func cacheRoot() string {
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		return filepath.Join(gopath, "pkg", "mod", "cache", "embeddedpg")
	}
	// Fallback to user home dir.
	home, err := os.UserHomeDir()
	if err != nil {
		if runtime.GOOS == "windows" {
			return filepath.Join("C:\\", "embeddedpg")
		}
		return "/tmp/embeddedpg"
	}
	return filepath.Join(home, ".cache", "embeddedpg")
}
