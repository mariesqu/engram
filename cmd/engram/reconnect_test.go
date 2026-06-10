package main

// Tests for runtimeSyncAdapter.Reconnect/Disconnect against a FAKE central
// (httptest). These exist because the round-1 review found the runtime-connect
// loop never started (nil ctx) — a bug no mock-level test could see. The fake
// central observes real HTTP traffic, so "the loop runs" is proven by requests
// arriving, and "the probe validates" by the connect failing against a 403.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"errors"
	"github.com/mariesqu/engram/internal/config"
	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
)

const testWriterKeyHex = "abababababababababababababababababababababababababababababababab" // 32 bytes

// fakeCentral returns an httptest server that accepts /v1/pull with an empty
// page and counts every request by path.
func fakeCentral(t *testing.T, pulls, pushes *atomic.Int64) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/pull"):
			pulls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"mutations":[]}`))
		case strings.HasPrefix(r.URL.Path, "/v1/push"):
			pushes.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func newReconnectFixture(t *testing.T, centralURL string) (*runtimeSyncAdapter, *localstore.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := localstore.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("localstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := daemonCfg{
		db:           filepath.Join(dir, "test.db"),
		syncInterval: 50 * time.Millisecond,
		configDir:    dir,
	}
	cfgAdapter := newConfigStoreAdapter(cfg, 0)
	a := newRuntimeSyncAdapter(t.Context(), cfg, st, nil, cfgAdapter, 0, embedding.NoopProvider{})
	_ = centralURL
	return a, st, dir
}

// TestReconnect_StartsLoop_FlipsPolicy_PersistsConfig is the end-to-end proof
// the round-1 HIGH demanded: after a successful POST-connect-equivalent the
// sync loop is genuinely RUNNING (the fake central receives traffic), the
// policy default flips to synced without restart, and the config is persisted.
// Disconnect reverses all three.
func TestReconnect_StartsLoop_FlipsPolicy_PersistsConfig(t *testing.T) {
	var pulls, pushes atomic.Int64
	central := fakeCentral(t, &pulls, &pushes)
	a, st, dir := newReconnectFixture(t, central.URL)

	// Before connect: no central → local-only default.
	if pol, _ := st.GetPolicy("someproj"); pol != localstore.PolicyLocalOnly {
		t.Fatalf("pre-connect default = %q, want local-only", pol)
	}

	if err := a.Reconnect(controlapi.CentralConfig{
		URL: central.URL, WriterID: "w1", WriterKeyPlaintext: testWriterKeyHex,
	}); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}

	// Probe ran: at least one pull already.
	if pulls.Load() < 1 {
		t.Errorf("probe did not reach central (pulls=0)")
	}

	// Policy default flipped to synced WITHOUT restart (closure re-installed).
	if pol, _ := st.GetPolicy("someproj"); pol != localstore.PolicySynced {
		t.Errorf("post-connect default = %q, want synced", pol)
	}

	// Config persisted to disk.
	fileCfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if fileCfg.CentralURL != central.URL {
		t.Errorf("persisted central_url = %q, want %q", fileCfg.CentralURL, central.URL)
	}

	// THE LOOP RUNS: seed a local write (creates an outbox entry + a project),
	// trigger, and watch the fake central receive traffic from the loop
	// goroutine. With the nil-ctx bug this never happens (zero requests).
	if _, err := st.AddObservation(localstore.AddObservationParams{
		SessionID: "s1", Type: "manual", Title: "loop proof", Content: "x",
		Project: "loopproj", Scope: "project", WriterID: "w1",
	}); err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	base := pulls.Load() + pushes.Load()
	if err := a.TriggerNow(context.Background()); err != nil {
		t.Fatalf("TriggerNow: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for pulls.Load()+pushes.Load() <= base {
		select {
		case <-deadline:
			t.Fatalf("loop produced no central traffic after trigger — loop not running (pulls=%d pushes=%d base=%d)",
				pulls.Load(), pushes.Load(), base)
		case <-time.After(20 * time.Millisecond):
		}
	}

	// Disconnect: loop stops, policy default flips back, disk cleared.
	if err := a.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if pol, _ := st.GetPolicy("someproj"); pol != localstore.PolicyLocalOnly {
		t.Errorf("post-disconnect default = %q, want local-only", pol)
	}
	fileCfg, err = config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load after disconnect: %v", err)
	}
	if fileCfg.CentralURL != "" {
		t.Errorf("persisted central_url after disconnect = %q, want empty", fileCfg.CentralURL)
	}
	if st2 := a.Status(); st2.CentralConnected {
		t.Error("Status.CentralConnected = true after Disconnect")
	}
}

// TestReconnect_ProbeRejects_NothingPersisted proves the probe is REAL: a
// central that 403s the signed pull makes Reconnect fail with
// ErrCredentialValidation and nothing reaches disk or memory.
func TestReconnect_ProbeRejects_NothingPersisted(t *testing.T) {
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(deny.Close)
	a, st, dir := newReconnectFixture(t, deny.URL)

	err := a.Reconnect(controlapi.CentralConfig{
		URL: deny.URL, WriterID: "w1", WriterKeyPlaintext: testWriterKeyHex,
	})
	if !errors.Is(err, controlapi.ErrCredentialValidation) {
		t.Fatalf("Reconnect against 403 central: err = %v, want ErrCredentialValidation", err)
	}
	if fileCfg, loadErr := config.Load(dir); loadErr == nil && fileCfg.CentralURL != "" {
		t.Errorf("config persisted despite probe failure: central_url=%q", fileCfg.CentralURL)
	}
	if pol, _ := st.GetPolicy("p"); pol != localstore.PolicyLocalOnly {
		t.Errorf("policy default flipped despite probe failure: %q", pol)
	}
	if st2 := a.Status(); st2.CentralConnected {
		t.Error("Status.CentralConnected = true after failed connect")
	}
}

// TestReconnect_Disconnect_Churn hammers connect/disconnect from two
// goroutines: no deadlock, no panic, and afterwards a deterministic Disconnect
// leaves disk and memory agreeing.
func TestReconnect_Disconnect_Churn(t *testing.T) {
	var pulls, pushes atomic.Int64
	central := fakeCentral(t, &pulls, &pushes)
	a, _, dir := newReconnectFixture(t, central.URL)

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		for i := 0; i < 10; i++ {
			_ = a.Reconnect(controlapi.CentralConfig{
				URL: central.URL, WriterID: "w1", WriterKeyPlaintext: testWriterKeyHex,
			})
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		for i := 0; i < 10; i++ {
			_ = a.Disconnect()
		}
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("churn deadlocked")
		}
	}

	// Deterministic final state: disconnect once, then disk and memory agree.
	if err := a.Disconnect(); err != nil {
		t.Fatalf("final Disconnect: %v", err)
	}
	fileCfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	st := a.Status()
	if st.CentralConnected || fileCfg.CentralURL != "" {
		t.Errorf("final state inconsistent: memory connected=%v, disk central_url=%q",
			st.CentralConnected, fileCfg.CentralURL)
	}
}
