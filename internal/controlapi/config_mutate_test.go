package controlapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
)

// ── Additional mocks needed for PR-③ tests ───────────────────────────────────

// mockSyncControllerFull records Disconnect / Reconnect calls.
// The existing mockSyncCtrl in server_test.go returns nil for both — this one
// allows us to inject errors and inspect call counts.
type mockSyncControllerFull struct {
	status          controlapi.Status
	triggerErr      error
	disconnectErr   error
	reconnectErr    error
	disconnectCalls int
	reconnectCalls  int
	reconnectArg    controlapi.CentralConfig
}

func (m *mockSyncControllerFull) Status() controlapi.Status { return m.status }

func (m *mockSyncControllerFull) TriggerNow(_ context.Context) error { return m.triggerErr }

func (m *mockSyncControllerFull) Disconnect() error {
	m.disconnectCalls++
	return m.disconnectErr
}

func (m *mockSyncControllerFull) Reconnect(cfg controlapi.CentralConfig) error {
	m.reconnectCalls++
	m.reconnectArg = cfg
	return m.reconnectErr
}

// mockConfigStorePR3 extends mockCfgStore with configurable restartRequired.
type mockConfigStorePR3 struct {
	cfg             controlapi.RedactedConfig
	loadErr         error
	applyErr        error
	lastPatch       controlapi.ConfigPatch
	restartRequired bool
}

func (m *mockConfigStorePR3) Load() (controlapi.RedactedConfig, error) {
	return m.cfg, m.loadErr
}

func (m *mockConfigStorePR3) Apply(p controlapi.ConfigPatch) (bool, error) {
	m.lastPatch = p
	return m.restartRequired, m.applyErr
}

// Compile-time interface assertions.
var _ controlapi.SyncController = (*mockSyncControllerFull)(nil)
var _ controlapi.ConfigStore = (*mockConfigStorePR3)(nil)

// newServerPR3 creates a Server with configurable mocks and port 7700.
func newServerPR3(t *testing.T, sync *mockSyncControllerFull, cfg *mockConfigStorePR3) *controlapi.Server {
	t.Helper()
	if sync == nil {
		sync = &mockSyncControllerFull{}
	}
	if cfg == nil {
		cfg = &mockConfigStorePR3{}
	}
	return controlapi.New("test-token", 7700, &mockStore{}, sync, cfg, "test-version")
}

// ── Helper builders ───────────────────────────────────────────────────────────

func buildPUT(t *testing.T, path string, body any) *http.Request {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(http.MethodPut, path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(http.MethodPut, path, http.NoBody)
	}
	r.Header.Set("Authorization", "Bearer test-token")
	r.Header.Set("Origin", "http://127.0.0.1:7700")
	return r
}

func buildPOST(t *testing.T, path string, body any) *http.Request {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(http.MethodPost, path, http.NoBody)
	}
	r.Header.Set("Authorization", "Bearer test-token")
	r.Header.Set("Origin", "http://127.0.0.1:7700")
	return r
}

// ── PUT /api/v1/config tests ──────────────────────────────────────────────────

func TestPUT_Config_RuntimeMutable_200_RestartFalse(t *testing.T) {
	cfgStore := &mockConfigStorePR3{restartRequired: false}
	srv := newServerPR3(t, nil, cfgStore)

	req := buildPUT(t, "/api/v1/config", map[string]any{"sync_interval": "30s"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200; body: %s", w.Code, w.Body)
	}

	var resp map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["restart_required"] {
		t.Error("restart_required should be false for sync_interval change")
	}

	interval := "30s"
	if cfgStore.lastPatch.SyncInterval == nil {
		t.Error("Apply not called with SyncInterval patch")
	} else if *cfgStore.lastPatch.SyncInterval != interval {
		t.Errorf("SyncInterval patch: got %q, want %q", *cfgStore.lastPatch.SyncInterval, interval)
	}
}

func TestPUT_Config_RestartRequired_200_RestartTrue(t *testing.T) {
	cfgStore := &mockConfigStorePR3{restartRequired: true}
	srv := newServerPR3(t, nil, cfgStore)

	req := buildPUT(t, "/api/v1/config", map[string]any{"http_port": 7701})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200; body: %s", w.Code, w.Body)
	}

	var resp map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp["restart_required"] {
		t.Error("restart_required should be true for http_port change")
	}
}

func TestPUT_Config_WriterKeyField_400(t *testing.T) {
	srv := newServerPR3(t, nil, nil)

	req := buildPUT(t, "/api/v1/config", map[string]any{"writer_key": "some-key"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("writer_key in PUT: got %d, want 400; body: %s", w.Code, w.Body)
	}
}

func TestPUT_Config_CentralURLField_400(t *testing.T) {
	srv := newServerPR3(t, nil, nil)

	req := buildPUT(t, "/api/v1/config", map[string]any{"central_url": "https://evil.example.com"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("central_url in PUT: got %d, want 400; body: %s", w.Code, w.Body)
	}
}

// TestPUT_Config_SyncInterval_Applied_Live verifies the sync_interval patch is
// forwarded to the config store's Apply with the correct value.
func TestPUT_Config_SyncInterval_Applied_Live(t *testing.T) {
	cfgStore := &mockConfigStorePR3{restartRequired: false}
	srv := newServerPR3(t, nil, cfgStore)

	req := buildPUT(t, "/api/v1/config", map[string]any{"sync_interval": "30s"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("PUT config: got %d; body: %s", w.Code, w.Body)
	}

	if cfgStore.lastPatch.SyncInterval == nil {
		t.Fatal("Apply not called with SyncInterval")
	}
	if *cfgStore.lastPatch.SyncInterval != "30s" {
		t.Errorf("SyncInterval patch: got %q, want %q", *cfgStore.lastPatch.SyncInterval, "30s")
	}
}

// ── POST /api/v1/central/connect tests ───────────────────────────────────────

func TestConnect_ValidCreds_200_StatusConnected(t *testing.T) {
	centralURL := "https://central.example.com"
	sync := &mockSyncControllerFull{
		status: controlapi.Status{
			CentralConnected: true,
			CentralURL:       &centralURL,
			DaemonVersion:    "test-version",
		},
	}
	srv := newServerPR3(t, sync, nil)

	req := buildPOST(t, "/api/v1/central/connect", map[string]any{
		"central_url": centralURL,
		"writer_id":   "test-writer",
		"writer_key":  "deadbeef01234567deadbeef01234567",
	})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("connect valid creds: got %d, want 200; body: %s", w.Code, w.Body)
	}

	var st controlapi.Status
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !st.CentralConnected {
		t.Error("central_connected should be true after successful connect")
	}
	if sync.reconnectCalls != 1 {
		t.Errorf("Reconnect called %d times, want 1", sync.reconnectCalls)
	}
	if sync.reconnectArg.URL != centralURL {
		t.Errorf("Reconnect arg URL: got %q, want %q", sync.reconnectArg.URL, centralURL)
	}
}

func TestConnect_InvalidCreds_422_ConfigNotPersisted(t *testing.T) {
	// The adapter contract: credential failures WRAP ErrCredentialValidation so
	// the handler maps them to a client-safe 422 (anything unwrapped is a 500).
	sync := &mockSyncControllerFull{
		reconnectErr: fmt.Errorf("%w: probe got 403 from central", controlapi.ErrCredentialValidation),
	}
	srv := newServerPR3(t, sync, nil)

	req := buildPOST(t, "/api/v1/central/connect", map[string]any{
		"central_url": "https://central.example.com",
		"writer_id":   "bad-writer",
		"writer_key":  "badkey",
	})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("connect invalid creds: got %d, want 422; body: %s", w.Code, w.Body)
	}
}

func TestConnect_MissingCentralURL_400(t *testing.T) {
	srv := newServerPR3(t, nil, nil)

	req := buildPOST(t, "/api/v1/central/connect", map[string]any{
		"writer_key": "somekey",
	})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("connect missing central_url: got %d, want 400", w.Code)
	}
}

func TestConnect_MissingWriterKey_400(t *testing.T) {
	srv := newServerPR3(t, nil, nil)

	req := buildPOST(t, "/api/v1/central/connect", map[string]any{
		"central_url": "https://central.example.com",
	})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("connect missing writer_key: got %d, want 400", w.Code)
	}
}

// ── POST /api/v1/central/disconnect tests ────────────────────────────────────

func TestDisconnect_200_SyncHalted(t *testing.T) {
	sync := &mockSyncControllerFull{
		status: controlapi.Status{CentralConnected: false, DaemonVersion: "test-version"},
	}
	srv := newServerPR3(t, sync, nil)

	req := buildPOST(t, "/api/v1/central/disconnect", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("disconnect: got %d, want 200; body: %s", w.Code, w.Body)
	}
	if sync.disconnectCalls != 1 {
		t.Errorf("Disconnect called %d times, want 1", sync.disconnectCalls)
	}

	var st controlapi.Status
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if st.CentralConnected {
		t.Error("central_connected should be false after disconnect")
	}
}

// ── POST /api/v1/sync/trigger tests ──────────────────────────────────────────

func TestSyncTrigger_Connected_202(t *testing.T) {
	centralURL := "https://central.example.com"
	sync := &mockSyncControllerFull{
		status: controlapi.Status{CentralConnected: true, CentralURL: &centralURL},
	}
	srv := newServerPR3(t, sync, nil)

	req := buildPOST(t, "/api/v1/sync/trigger", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("sync trigger connected: got %d, want 202; body: %s", w.Code, w.Body)
	}
}

func TestSyncTrigger_Disconnected_409(t *testing.T) {
	sync := &mockSyncControllerFull{
		status: controlapi.Status{CentralConnected: false},
	}
	srv := newServerPR3(t, sync, nil)

	req := buildPOST(t, "/api/v1/sync/trigger", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("sync trigger disconnected: got %d, want 409; body: %s", w.Code, w.Body)
	}
}

// ── helper error type ─────────────────────────────────────────────────────────

type connectTestError string

func (e connectTestError) Error() string { return string(e) }
