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
	"github.com/jackc/pgx/v5/pgconn"
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
		// is only downloaded once across test runs. cacheRoot() already returns a
		// path ending in "embeddedpg" — do NOT append another "embeddedpg" suffix.
		cacheDir := cacheRoot()
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

	// Drop any leftover schema from an interrupted prior run so every run starts
	// from a guaranteed-absent, clean schema (hermetic reruns against external DSN).
	if _, err := adminPool.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE", schemaName)); err != nil {
		t.Fatalf("newIsolatedStore: DROP SCHEMA: %v", err)
	}
	if _, err := adminPool.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %q", schemaName)); err != nil {
		t.Fatalf("newIsolatedStore: CREATE SCHEMA: %v", err)
	}

	// Build a DSN that sets search_path to the isolated schema.  withSearchPath
	// handles both URL-form (postgres://...) and keyword/value DSNs correctly.
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

	// Drop any leftover schema from an interrupted prior run so every run starts
	// from a guaranteed-absent, clean schema (hermetic reruns against external DSN).
	if _, err := adminPool.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE", schemaName)); err != nil {
		t.Fatalf("DROP SCHEMA: %v", err)
	}
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

	// Build a DSN that sets search_path to the isolated schema.  withSearchPath
	// handles both URL-form (postgres://...) and keyword/value DSNs correctly.
	dsn, err := withSearchPath(pgDSN, schemaName)
	if err != nil {
		t.Fatalf("TestCentralSchema_IdempotentApply: withSearchPath: %v", err)
	}

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

// TestInsertMutation_NilPayloadDefaultsToEmptyObject verifies that calling
// InsertMutation with a Mutation whose Payload is nil (zero value) does not
// cause a NOT NULL violation on central_mutations.payload (JSONB NOT NULL).
// The stored payload must be the empty JSON object '{}'.
func TestInsertMutation_NilPayloadDefaultsToEmptyObject(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	m := testMutation("mut-nil-payload-1", "sync-nil-payload-1", "proj", domain.OpUpsert)
	m.Payload = nil // explicitly unset — simulates a caller that left Payload zero

	seq, err := store.InsertMutation(ctx, m)
	if err != nil {
		t.Fatalf("InsertMutation with nil Payload: %v", err)
	}
	if seq <= 0 {
		t.Errorf("InsertMutation with nil Payload: seq=%d, want > 0", seq)
	}

	// Read back the raw payload to confirm it was stored as '{}'.
	var payload []byte
	err = store.Pool().QueryRow(ctx,
		`SELECT payload FROM central_mutations WHERE mutation_id = $1`,
		"mut-nil-payload-1",
	).Scan(&payload)
	if err != nil {
		t.Fatalf("SELECT payload: %v", err)
	}
	if string(payload) != "{}" {
		t.Errorf("stored payload=%q, want %q", string(payload), "{}")
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

	if err := store.UpsertMemory(ctx, m.SyncID, m, 1); err != nil {
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

	if err := store.UpsertMemory(ctx, m.SyncID, m, 1); err != nil {
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

	// WriteTombstone only inserts the tombstone row in central_tombstones; it does
	// NOT touch central_memories.deleted_at.  The full Decide-driven apply (PR3b)
	// does both.  Manually set deleted_at on the memory row to test FindBySyncID
	// with a soft-deleted record.
	md := testMutation("mut-fbs-del", "sync-fbs-1", "proj", domain.OpDelete)
	if err = store.WriteTombstone(ctx, md.SyncID, md); err != nil {
		t.Fatalf("WriteTombstone: %v", err)
	}
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

	if err := store.WriteTombstone(ctx, m.SyncID, m); err != nil {
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

// TestReaderRoundtrip_TombstoneIdentityFields verifies that WriteTombstone
// persists deleted_by (writer_id) and last_write_mutation_id (the winning delete's
// content-addressed mutation_id) and that FindTombstone reads them back — these
// are the identity tiebreaker fields used by writeWins when updated_at and version
// tie. ts.SyncID is the tombstone's PK/identity, NOT a tiebreaker.
func TestReaderRoundtrip_TombstoneIdentityFields(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	const wantWriter = "writer-identity-test"
	const wantSyncID = "sync-identity-test"

	m := testMutation("mut-ident-1", wantSyncID, "proj", domain.OpDelete)
	m.WriterID = wantWriter

	if err := store.WriteTombstone(ctx, m.SyncID, m); err != nil {
		t.Fatalf("WriteTombstone with writer: %v", err)
	}

	ts, err := store.FindTombstone(wantSyncID, nil, "proj", "project")
	if err != nil {
		t.Fatalf("FindTombstone: %v", err)
	}
	if ts == nil {
		t.Fatal("FindTombstone: expected tombstone, got nil")
	}
	if ts.DeletedBy != wantWriter {
		t.Errorf("tombstone deleted_by roundtrip: ts.DeletedBy = %q; want %q", ts.DeletedBy, wantWriter)
	}
	if ts.SyncID != wantSyncID {
		t.Errorf("tombstone sync_id roundtrip: ts.SyncID = %q; want %q", ts.SyncID, wantSyncID)
	}
	if ts.LastWriteMutationID != m.MutationID {
		t.Errorf("tombstone last_write_mutation_id roundtrip: ts.LastWriteMutationID = %q; want %q", ts.LastWriteMutationID, m.MutationID)
	}
}

// TestApply_TombstoneIdentityFieldsWired verifies the full Apply path: after a
// push-apply delete, the central_tombstones row has deleted_by and
// last_write_mutation_id (the winning delete's mutation_id) populated — the
// identity tiebreaker fields used by writeWins. sync_id is the tombstone PK, not
// a tiebreaker.
func TestApply_TombstoneIdentityFieldsWired(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	const wantWriter = "writer-apply-wired"

	// Step 1 — upsert so there is a live row to delete.
	mUpsert := testMutation("mut-taw-up", "sync-taw-1", "proj", domain.OpUpsert)
	mUpsert.UpdatedAt = time.Now().UTC()
	mUpsert.WriterID = wantWriter
	if err := store.Apply(ctx, mUpsert); err != nil {
		t.Fatalf("Apply upsert: %v", err)
	}

	// Step 2 — delete; Apply writes the tombstone with deleted_by = writer_id.
	mDelete := testMutation("mut-taw-del", "sync-taw-1", "proj", domain.OpDelete)
	mDelete.UpdatedAt = mUpsert.UpdatedAt.Add(time.Second)
	mDelete.WriterID = wantWriter
	if err := store.Apply(ctx, mDelete); err != nil {
		t.Fatalf("Apply delete: %v", err)
	}

	ts, err := store.FindTombstone("sync-taw-1", nil, "proj", "project")
	if err != nil {
		t.Fatalf("FindTombstone: %v", err)
	}
	if ts == nil {
		t.Fatal("FindTombstone: expected tombstone after Apply delete, got nil")
	}
	if ts.DeletedBy != wantWriter {
		t.Errorf("tombstone deleted_by after Apply delete: ts.DeletedBy = %q; want %q", ts.DeletedBy, wantWriter)
	}
	if ts.SyncID != "sync-taw-1" {
		t.Errorf("tombstone sync_id after Apply delete: ts.SyncID = %q; want %q", ts.SyncID, "sync-taw-1")
	}
	if ts.LastWriteMutationID != mDelete.MutationID {
		t.Errorf("tombstone last_write_mutation_id after Apply delete: ts.LastWriteMutationID = %q; want %q", ts.LastWriteMutationID, mDelete.MutationID)
	}
}

// TestCentralTombstone_HasNoSeqColumn verifies that central_tombstones does NOT
// have a seq column — it was removed when the tiebreaker changed from central
// seq to (writer_id, then the winning mutation_id via last_write_mutation_id).
// Having no seq column prevents the old tombstone seq roundtrip and proves the
// schema is clean.
func TestCentralTombstone_HasNoSeqColumn(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	var colCount int
	err := store.Pool().QueryRow(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name  = 'central_tombstones'
		  AND column_name = 'seq'`,
	).Scan(&colCount)
	if err != nil {
		t.Fatalf("query information_schema.columns for seq: %v", err)
	}
	if colCount != 0 {
		t.Errorf("central_tombstones must NOT have a seq column after the identity-tiebreaker change; found %d", colCount)
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

	if err := store.UpsertMemory(ctx, m1.SyncID, m1, 1); err != nil {
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
	// Assert the SQLSTATE code deterministically (23505 = unique_violation) rather
	// than pattern-matching error message text, which varies across Postgres versions.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != "23505" {
		t.Errorf("expected SQLSTATE 23505 (unique_violation), got %q: %v", pgErr.Code, err)
	}
}

// TestUpsertMemory_CrossWriterConvergence verifies that UpsertMemory with a
// targetSyncID different from m.SyncID updates the canonical row Y rather than
// creating a second live row — no central_memories_topic_uidx violation and no
// duplicate-key failure. This is the P1-a cross-writer convergence fix.
//
// Setup: seed a live row Y for topic T under writer A.
// Action: call UpsertMemory(ctx, "Y", mX, seq) where mX.SyncID="X" (Writer B,
// newer wall-clock) but targetSyncID="Y" (Decide resolved canonical row Y).
// Assert: exactly one live row for T, its sync_id is still Y, its content is
// mX's content (the winning update was applied to the canonical row).
func TestUpsertMemory_CrossWriterConvergence(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	tk := "sdd/test/crosswriter-upsert"
	tptr := &tk

	// Writer A seeds the canonical row Y.
	mY := domain.Mutation{
		MutationID: "mut-cw-y",
		Op:         domain.OpUpsert,
		SyncID:     "sync-cw-Y",
		SessionID:  "sess-A",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "Row Y (Writer A)",
		Content:    "content from writer A",
		Project:    "proj",
		Scope:      "scp",
		TopicKey:   tptr,
		Version:    1,
		Seq:        0,
		WriterID:   "writer-A",
		UpdatedAt:  time.Now().Add(-10 * time.Second).UTC(),
		OccurredAt: time.Now().Add(-10 * time.Second).UTC(),
		Payload:    []byte(`{}`),
	}
	if err := store.UpsertMemory(ctx, mY.SyncID, mY, 1); err != nil {
		t.Fatalf("seed canonical row Y: %v", err)
	}

	// Writer B has a newer mutation X for the same topic. Decide() resolved
	// canonical row Y; it returns TargetSyncID="sync-cw-Y". We simulate calling
	// UpsertMemory with targetSyncID=Y and m.SyncID=X.
	mX := domain.Mutation{
		MutationID: "mut-cw-x",
		Op:         domain.OpUpsert,
		SyncID:     "sync-cw-X", // incoming writer B sync_id — NOT the canonical row
		SessionID:  "sess-B",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "Row X (Writer B — newer)",
		Content:    "content from writer B (winner)",
		Project:    "proj",
		Scope:      "scp",
		TopicKey:   tptr,
		Version:    2,
		Seq:        0,
		WriterID:   "writer-B",
		UpdatedAt:  time.Now().UTC(), // newer wall-clock → wins
		OccurredAt: time.Now().UTC(),
		Payload:    []byte(`{}`),
	}
	// targetSyncID = "sync-cw-Y" (the canonical row resolved by Decide).
	// This must NOT fail with central_memories_topic_uidx violation.
	if err := store.UpsertMemory(ctx, "sync-cw-Y", mX, 2); err != nil {
		t.Fatalf("UpsertMemory cross-writer (targetSyncID=Y, m.SyncID=X): %v", err)
	}

	// Assert: exactly one live row for topic T.
	rows, err := store.Pool().Query(ctx,
		`SELECT sync_id, title, content FROM central_memories
		 WHERE topic_key=$1 AND project=$2 AND scope=$3 AND deleted_at IS NULL`,
		tk, "proj", "scp",
	)
	if err != nil {
		t.Fatalf("query live rows: %v", err)
	}
	defer rows.Close()

	type row struct{ syncID, title, content string }
	var live []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.syncID, &r.title, &r.content); err != nil {
			t.Fatalf("scan: %v", err)
		}
		live = append(live, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(live) != 1 {
		t.Fatalf("expected exactly 1 live row for topic T, got %d: %+v", len(live), live)
	}
	if live[0].syncID != "sync-cw-Y" {
		t.Errorf("canonical row sync_id=%q, want %q", live[0].syncID, "sync-cw-Y")
	}
	if live[0].content != mX.Content {
		t.Errorf("canonical row content=%q, want %q (writer B's winning content)", live[0].content, mX.Content)
	}
}

// TestWriteTombstone_CrossWriterConvergence verifies that WriteTombstone with a
// targetSyncID different from m.SyncID reuses the canonical tombstone Y rather
// than creating a second tombstone — no central_tombstones_topic_uidx violation.
// This is the P1-b cross-writer convergence fix.
//
// Setup: seed a tombstone under Y for topic T.
// Action: call WriteTombstone(ctx, "Y", mX) where mX.SyncID="X" (different
// writer) but targetSyncID="Y" (Decide resolved canonical tombstone Y).
// Assert: exactly one tombstone for T (ON CONFLICT reused Y, no duplicate).
func TestWriteTombstone_CrossWriterConvergence(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	tk := "sdd/test/crosswriter-tombstone"
	tptr := &tk

	// Seed the canonical tombstone under Y (Writer A's delete).
	mY := domain.Mutation{
		MutationID: "mut-cwts-y",
		Op:         domain.OpDelete,
		SyncID:     "sync-cwts-Y",
		SessionID:  "sess-A",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "",
		Content:    "",
		Project:    "proj",
		Scope:      "scp",
		TopicKey:   tptr,
		Version:    1,
		Seq:        0,
		WriterID:   "writer-A",
		UpdatedAt:  time.Now().Add(-5 * time.Second).UTC(),
		OccurredAt: time.Now().Add(-5 * time.Second).UTC(),
		Payload:    []byte(`{}`),
	}
	if err := store.WriteTombstone(ctx, mY.SyncID, mY); err != nil {
		t.Fatalf("seed canonical tombstone Y: %v", err)
	}

	// Writer B tries to re-delete the same topic (mX.SyncID=X). Decide resolved
	// canonical tombstone Y → TargetSyncID="sync-cwts-Y". We call WriteTombstone
	// with targetSyncID=Y and m.SyncID=X. Must not hit central_tombstones_topic_uidx.
	mX := domain.Mutation{
		MutationID: "mut-cwts-x",
		Op:         domain.OpDelete,
		SyncID:     "sync-cwts-X", // incoming — NOT the canonical tombstone
		SessionID:  "sess-B",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "",
		Content:    "",
		Project:    "proj",
		Scope:      "scp",
		TopicKey:   tptr,
		Version:    2,
		Seq:        0,
		WriterID:   "writer-B",
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
		Payload:    []byte(`{}`),
	}
	// targetSyncID = "sync-cwts-Y" — must reuse the canonical tombstone, not create a new one.
	if err := store.WriteTombstone(ctx, "sync-cwts-Y", mX); err != nil {
		t.Fatalf("WriteTombstone cross-writer (targetSyncID=Y, m.SyncID=X): %v", err)
	}

	// Assert: exactly one tombstone for topic T.
	rows, err := store.Pool().Query(ctx,
		`SELECT sync_id FROM central_tombstones
		 WHERE topic_key=$1 AND project=$2 AND scope=$3`,
		tk, "proj", "scp",
	)
	if err != nil {
		t.Fatalf("query tombstones: %v", err)
	}
	defer rows.Close()

	var syncIDs []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		syncIDs = append(syncIDs, sid)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(syncIDs) != 1 {
		t.Fatalf("expected exactly 1 tombstone for topic T, got %d: %v", len(syncIDs), syncIDs)
	}
	if syncIDs[0] != "sync-cwts-Y" {
		t.Errorf("tombstone sync_id=%q, want %q (canonical row reused)", syncIDs[0], "sync-cwts-Y")
	}
}

// TestUpsertMemory_CreatedAtIsServerTime verifies that UpsertMemory does NOT
// override the server's DEFAULT now() for created_at with the client's
// m.UpdatedAt value. The test inserts with m.UpdatedAt set to a stale past
// time (2020-01-01) and then reads the row back, asserting:
//   - created_at is recent (within a few seconds of server now(), NOT 2020).
//   - updated_at is the stale m.UpdatedAt value (proving updated_at keeps the
//     client logical write time used by writeWins() as the LWW tiebreaker).
func TestUpsertMemory_CreatedAtIsServerTime(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	// m.UpdatedAt is intentionally set to a stale past time to prove that
	// created_at does NOT inherit this value from the INSERT.
	staleTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	m := testMutation("mut-creat-1", "sync-creat-1", "proj", domain.OpUpsert)
	m.UpdatedAt = staleTime

	if err := store.UpsertMemory(ctx, m.SyncID, m, 1); err != nil {
		t.Fatalf("UpsertMemory: %v", err)
	}

	// Read the raw row back to inspect both timestamp columns.
	var createdAt, updatedAt time.Time
	err := store.Pool().QueryRow(ctx,
		`SELECT created_at, updated_at FROM central_memories WHERE sync_id = $1`,
		"sync-creat-1",
	).Scan(&createdAt, &updatedAt)
	if err != nil {
		t.Fatalf("SELECT created_at/updated_at: %v", err)
	}

	// created_at must be server now(), not 2020. Allow 30-second window for slow CI.
	threshold := 30 * time.Second
	sinceCreated := time.Since(createdAt.UTC())
	if sinceCreated < 0 || sinceCreated > threshold {
		t.Errorf("created_at=%v is not server now(): time.Since=%v (want within %v)", createdAt, sinceCreated, threshold)
	}

	// updated_at must be the stale m.UpdatedAt — the client logical write time
	// that writeWins() uses as the LWW tiebreaker.
	if !updatedAt.UTC().Equal(staleTime) {
		t.Errorf("updated_at=%v, want stale client value %v (LWW tiebreaker must be preserved)", updatedAt.UTC(), staleTime)
	}
}

// TestEntityTypeCheck_RejectsBogusValue verifies that the entity_type CHECK
// constraint on central_memories rejects an INSERT with an invalid entity_type.
// A raw INSERT with entity_type='bogus' must fail with SQLSTATE 23514
// (check_violation) — the same pgconn assertion pattern used in
// TestPartialUniqueIndex_RejectsSecondLiveRow.
func TestEntityTypeCheck_RejectsBogusValue(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	_, err := store.Pool().Exec(ctx, `
		INSERT INTO central_memories
		  (sync_id, entity_type, type, title, content, project, scope,
		   version, seq, writer_id, created_by)
		VALUES ($1,$2,'manual','title','content','proj','project',1,1,'w','w')`,
		"sync-check-bogus", "bogus",
	)
	if err == nil {
		t.Fatal("INSERT with entity_type='bogus': expected check_violation error, got nil")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected *pgconn.PgError, got %T: %v", err, err)
	}
	// SQLSTATE 23514 = check_violation
	if pgErr.Code != "23514" {
		t.Errorf("expected SQLSTATE 23514 (check_violation), got %q: %v", pgErr.Code, err)
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
