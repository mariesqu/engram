//go:build acceptance

package embedding_test

// backfill_acceptance_test.go — task 1b.5
//
// Backfill loop acceptance tests.
//
// Uses a real temp SQLite store (no mocking of storage). The inner provider is
// the recordingMockProvider wired as 'inner' of NewGated (real-gate pattern —
// binding constraint from PR-1).
//
// Build tag: acceptance — run with:
//
//	go test -tags acceptance ./internal/embedding/...
//
// No real API key required. No external services required.
//
// Scenarios:
//  1. 5 synced rows → loop runs → all 5 embedded (happy path).
//  2. 3 omitted rows → loop runs → 0 rows embedded, 0 provider calls.
//  3. GET /api/v1/status path: after backfill, CountNullEmbeddings() returns 0
//     for a store that had all-synced rows.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
)

// ── Helpers ──────────────────────────────────────────────────────────────────

// openAcceptanceStore opens a temp SQLite store and registers cleanup.
func openAcceptanceStore(t *testing.T) *localstore.Store {
	t.Helper()
	st, err := localstore.Open(filepath.Join(t.TempDir(), "acceptance.db"))
	if err != nil {
		t.Fatalf("localstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// insertAcceptanceRow inserts a bare row with embedding IS NULL.
func insertAcceptanceRow(t *testing.T, st *localstore.Store, syncID, project, title string) {
	t.Helper()
	_, err := st.DB().Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content, project, scope,
		   version, writer_id, last_write_mutation_id, created_at, updated_at)
		VALUES (?, 'sess1', 'memory', 'manual', ?, '', ?, 'project',
		        1, 'w1', 'mut-'||?, datetime('now'), datetime('now'))`,
		syncID, title, project, syncID)
	if err != nil {
		t.Fatalf("insertAcceptanceRow %q: %v", syncID, err)
	}
}

// runBackfill creates a Loop, triggers one pass, waits, and stops.
func runBackfill(t *testing.T, st *localstore.Store, gated embedding.EmbeddingProvider, mock *recordingMockProvider) {
	t.Helper()
	loop := embedding.NewLoop(st, gated, embedding.LoopConfig{
		Interval:   10 * time.Second,
		Debounce:   1 * time.Millisecond,
		BatchPause: -1,
		BatchSize:  100,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	loop.Start(ctx)
	loop.Trigger()
	waitQuiescent(mock, 600*time.Millisecond, 8*time.Second)
	loop.Stop()
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestBackfill_SyncedRows_AllEmbedded is the primary acceptance test:
// 5 synced rows → loop runs → all 5 embedded, provider called ≥ 1 times.
func TestBackfill_SyncedRows_AllEmbedded(t *testing.T) {
	st := openAcceptanceStore(t)

	const project = "shared-project"
	if err := st.SetPolicy(project, localstore.PolicySynced); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	for i := 0; i < 5; i++ {
		syncID := "acc-synced-" + string(rune('A'+i))
		insertAcceptanceRow(t, st, syncID, project, "acceptance note "+string(rune('A'+i)))
	}

	mock := newRecordingMock(4)
	gated := embedding.NewGated(mock, st, true /* remote */)
	runBackfill(t, st, gated, mock)

	// All 5 rows must be embedded.
	for i := 0; i < 5; i++ {
		syncID := "acc-synced-" + string(rune('A'+i))
		if !hasEmbedding(t, st, syncID) {
			t.Errorf("row %q should be embedded after backfill", syncID)
		}
	}

	// Provider must have been called.
	if mock.callCount() == 0 {
		t.Error("provider was never called; expected at least 1 Embed call")
	}
	if mock.totalTexts() != 5 {
		t.Errorf("provider received %d texts, want 5", mock.totalTexts())
	}
}

// TestBackfill_OmittedRows_ZeroCalls verifies that 3 omitted rows stay NULL
// and the provider receives zero calls.
func TestBackfill_OmittedRows_ZeroCalls(t *testing.T) {
	st := openAcceptanceStore(t)

	const project = "secret-project"
	if err := st.SetPolicy(project, localstore.PolicyOmitted); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	for i := 0; i < 3; i++ {
		syncID := "acc-omitted-" + string(rune('A'+i))
		insertAcceptanceRow(t, st, syncID, project, "sensitive note "+string(rune('A'+i)))
	}

	mock := newRecordingMock(4)
	gated := embedding.NewGated(mock, st, true /* remote */)
	runBackfill(t, st, gated, mock)

	// All 3 rows must remain NULL.
	for i := 0; i < 3; i++ {
		syncID := "acc-omitted-" + string(rune('A'+i))
		if hasEmbedding(t, st, syncID) {
			t.Errorf("row %q must NOT be embedded (policy: omitted)", syncID)
		}
	}

	// Provider must never have been called.
	if mock.callCount() != 0 {
		t.Errorf("provider called %d times for omitted project, want 0", mock.callCount())
	}
}

// TestBackfill_Status_Pending_ReachesZero verifies the GET /api/v1/status
// integration: after backfill, CountNullEmbeddings returns 0 for all-synced store.
// This test validates the observability path (runtimeSyncAdapter.Status →
// store.CountNullEmbeddings → EmbeddingBackfill.Pending).
func TestBackfill_Status_Pending_ReachesZero(t *testing.T) {
	st := openAcceptanceStore(t)

	const project = "status-project"
	if err := st.SetPolicy(project, localstore.PolicySynced); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}

	// Insert 4 rows.
	for i := 0; i < 4; i++ {
		syncID := "status-row-" + string(rune('A'+i))
		insertAcceptanceRow(t, st, syncID, project, "note "+string(rune('A'+i)))
	}

	// Verify pending count is 4 before backfill.
	before, err := st.CountNullEmbeddings()
	if err != nil {
		t.Fatalf("CountNullEmbeddings before: %v", err)
	}
	if before != 4 {
		t.Fatalf("expected 4 pending before backfill, got %d", before)
	}

	// Run backfill.
	mock := newRecordingMock(4)
	gated := embedding.NewGated(mock, st, true)
	runBackfill(t, st, gated, mock)

	// Verify pending count is 0 after backfill.
	after, err := st.CountNullEmbeddings()
	if err != nil {
		t.Fatalf("CountNullEmbeddings after: %v", err)
	}
	if after != 0 {
		t.Errorf("CountNullEmbeddings = %d after backfill, want 0", after)
	}

	// Verify the GET /api/v1/status endpoint returns pending=0 via a real
	// HTTP handler. Wire a minimal controlapi.Server with a stub SyncController
	// that populates EmbeddingBackfill from the store.
	syncCtrl := &backfillStatusCtrl{store: st, provider: "mock"}
	srv := newStatusTestServer(t, st, syncCtrl)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rw := httptest.NewRecorder()
	srv.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/status: status %d, want 200. Body: %s", rw.Code, rw.Body.String())
	}

	var resp controlapi.Status
	if err := json.NewDecoder(rw.Body).Decode(&resp); err != nil {
		t.Fatalf("decode status response: %v", err)
	}

	if resp.EmbeddingBackfill == nil {
		t.Fatal("EmbeddingBackfill is nil in status response, want populated")
	}
	if resp.EmbeddingBackfill.Pending != 0 {
		t.Errorf("EmbeddingBackfill.Pending = %d, want 0", resp.EmbeddingBackfill.Pending)
	}
	if resp.EmbeddingBackfill.Provider != "mock" {
		t.Errorf("EmbeddingBackfill.Provider = %q, want mock", resp.EmbeddingBackfill.Provider)
	}
}

// ── Stubs for status endpoint test ───────────────────────────────────────────

// backfillStatusCtrl is a minimal SyncController that reads CountNullEmbeddings
// from a real store to populate EmbeddingBackfill in the status response.
type backfillStatusCtrl struct {
	store    *localstore.Store
	provider string
}

func (c *backfillStatusCtrl) Status() controlapi.Status {
	pending, _ := c.store.CountNullEmbeddings()
	return controlapi.Status{
		CentralConnected: false,
		LastSyncResult:   controlapi.SyncResult{},
		DaemonVersion:    "acceptance-test",
		EmbeddingBackfill: &controlapi.EmbeddingBackfill{
			Pending:  pending,
			Provider: c.provider,
		},
	}
}

func (c *backfillStatusCtrl) TriggerNow(_ context.Context) error         { return nil }
func (c *backfillStatusCtrl) Disconnect() error                          { return nil }
func (c *backfillStatusCtrl) Reconnect(_ controlapi.CentralConfig) error { return nil }

// acceptanceStoreAdapter wraps *localstore.Store to satisfy controlapi.Store.
type acceptanceStoreAdapter struct{ s *localstore.Store }

func (a *acceptanceStoreAdapter) ListProjectsWithPolicy() ([]controlapi.ProjectPolicy, error) {
	lpp, err := a.s.ListProjectsWithPolicy()
	if err != nil {
		return nil, err
	}
	out := make([]controlapi.ProjectPolicy, len(lpp))
	for i, p := range lpp {
		out[i] = controlapi.ProjectPolicy{
			Name:   p.Name,
			Policy: controlapi.Policy(p.Policy),
		}
	}
	return out, nil
}
func (a *acceptanceStoreAdapter) SetPolicy(project string, p controlapi.Policy) error {
	return a.s.SetPolicy(project, localstore.Policy(p))
}
func (a *acceptanceStoreAdapter) GetPolicy(project string) (controlapi.Policy, error) {
	p, err := a.s.GetPolicy(project)
	return controlapi.Policy(p), err
}

// Memory/project-purge methods are unused by the backfill acceptance test;
// stubbed to satisfy the grown controlapi.Store interface.
func (a *acceptanceStoreAdapter) ListMemories(query, project string, limit int) ([]controlapi.MemorySummary, error) {
	return nil, nil
}
func (a *acceptanceStoreAdapter) UpdateMemory(id int64, title, content, typ string) (controlapi.MemorySummary, error) {
	return controlapi.MemorySummary{}, nil
}
func (a *acceptanceStoreAdapter) DeleteMemory(id int64) error             { return nil }
func (a *acceptanceStoreAdapter) PurgeProjectLocal(p string) (int, error) { return 0, nil }
func (a *acceptanceStoreAdapter) TombstoneProject(p string) (int, error)  { return 0, nil }

type noopCfgStore struct{}

func (n *noopCfgStore) Load() (controlapi.RedactedConfig, error) {
	return controlapi.RedactedConfig{}, nil
}
func (n *noopCfgStore) Apply(_ controlapi.ConfigPatch) (bool, error) { return false, nil }

// newStatusTestServer builds a minimal controlapi.Server for the status probe.
func newStatusTestServer(t *testing.T, st *localstore.Store, syncCtrl controlapi.SyncController) http.Handler {
	t.Helper()
	srv := controlapi.New(
		"test-key", // bearer token
		0,          // port (unused for httptest)
		&acceptanceStoreAdapter{s: st},
		syncCtrl,
		&noopCfgStore{},
		"acceptance-test", // version
	)
	return srv.Handler()
}
