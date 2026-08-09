package main

// Tests for runtimeSyncAdapter.Status's CentralConnected/CentralConfigured
// semantics (Fix 2: central_connected must mean "configured AND the most
// recent sync attempt succeeded", not merely "configured"). Uses the same
// fakeCentral/testWriterKeyHex fixtures as reconnect_test.go, against a real
// httptest central so the sync loop's actual push/pull/discovery calls drive
// the outcome — no mocking of syncer.Loop itself.

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
)

// newStatusFixture is newReconnectFixture (reconnect_test.go) with an
// explicit, caller-chosen sync interval. These tests need control over the
// interval: a long one so the loop's own periodic cycle cannot race the
// "not yet synced" assertion, a short one where a real cycle must be observed
// within the test's deadline.
func newStatusFixture(t *testing.T, syncInterval time.Duration) *runtimeSyncAdapter {
	t.Helper()
	dir := t.TempDir()
	st, err := localstore.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("localstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := daemonCfg{
		db:           filepath.Join(dir, "test.db"),
		syncInterval: syncInterval,
		configDir:    dir,
	}
	cfgAdapter := newConfigStoreAdapter(cfg, 0)
	return newRuntimeSyncAdapter(t.Context(), cfg, st, nil, cfgAdapter, 0, embedding.NoopProvider{})
}

// flakyCentral returns an httptest server whose FIRST request (Reconnect's
// probeRemote pull) succeeds, so Reconnect persists and starts the loop, but
// every request after that fails with 500 — simulating a central that was
// reachable at connect time but has since gone down (the exact "sync started
// succeeding, then started failing" scenario Fix 2's status semantics target).
func flakyCentral(t *testing.T) *httptest.Server {
	t.Helper()
	var calls atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"mutations":[]}`))
			return
		}
		http.Error(w, "central down", http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestStatus_ConfiguredButNotYetSynced verifies that immediately after a
// successful Reconnect — before the sync loop's first cycle completes —
// Status reports CentralConfigured=true but CentralConnected=false. Before
// this fix, Status.CentralConnected meant "configured" and was already true
// at this point: exactly the "central_connected: true while sync has never
// actually run" gap this fix closes.
func TestStatus_ConfiguredButNotYetSynced(t *testing.T) {
	var pulls, pushes atomic.Int64
	central := fakeCentral(t, &pulls, &pushes)
	// A long interval so the loop's own periodic cycle cannot race this assertion.
	a := newStatusFixture(t, time.Hour)

	if err := a.Reconnect(controlapi.CentralConfig{
		URL: central.URL, WriterID: "w1", WriterKeyPlaintext: testWriterKeyHex,
	}); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}

	st := a.Status()
	if !st.CentralConfigured {
		t.Error("Status.CentralConfigured = false immediately after Reconnect, want true")
	}
	if st.CentralConnected {
		t.Error("Status.CentralConnected = true before any sync cycle has completed, want false")
	}
}

// TestStatus_ConnectedAfterSuccessfulSync verifies that once the sync loop
// completes a cycle successfully, Status reports CentralConnected=true.
func TestStatus_ConnectedAfterSuccessfulSync(t *testing.T) {
	var pulls, pushes atomic.Int64
	central := fakeCentral(t, &pulls, &pushes)
	a := newStatusFixture(t, 50*time.Millisecond)

	if err := a.Reconnect(controlapi.CentralConfig{
		URL: central.URL, WriterID: "w1", WriterKeyPlaintext: testWriterKeyHex,
	}); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		st := a.Status()
		if st.CentralConnected {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("Status.CentralConnected never became true after a successful sync cycle (last_sync_result=%+v)", st.LastSyncResult)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestStatus_DisconnectedAfterFailedSync verifies that once the sync loop's
// most recent cycle FAILS, Status reports CentralConnected=false while
// CentralConfigured stays true — the daemon remains configured and keeps
// retrying on its own interval, but the status endpoint must not claim
// reachability it doesn't have.
func TestStatus_DisconnectedAfterFailedSync(t *testing.T) {
	central := flakyCentral(t)
	a := newStatusFixture(t, 50*time.Millisecond)

	if err := a.Reconnect(controlapi.CentralConfig{
		URL: central.URL, WriterID: "w1", WriterKeyPlaintext: testWriterKeyHex,
	}); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		st := a.Status()
		if st.LastSyncResult.Error != nil {
			if st.CentralConnected {
				t.Error("Status.CentralConnected = true after a failed sync cycle, want false")
			}
			if !st.CentralConfigured {
				t.Error("Status.CentralConfigured = false after a failed sync cycle, want true (still configured, still retrying)")
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("no sync cycle recorded a failure within the deadline")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
