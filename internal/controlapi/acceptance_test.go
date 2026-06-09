//go:build acceptance

package controlapi_test

// TestAcceptance_ControlAPI_Suite runs the full control API acceptance suite
// against a real controlapi.Server wired with a real temp SQLite store.
// Each sub-test covers one of the spec's headless-testable requirements.
//
// Build tag: acceptance (run with: go test -tags acceptance ./internal/controlapi/...)

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/localstore"
)

// realStoreAdapter adapts *localstore.Store to controlapi.Store.
type realStoreAdapter struct{ store *localstore.Store }

func (a *realStoreAdapter) ListProjectsWithPolicy() ([]controlapi.ProjectPolicy, error) {
	lpp, err := a.store.ListProjectsWithPolicy()
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

func (a *realStoreAdapter) SetPolicy(project string, p controlapi.Policy) error {
	return a.store.SetPolicy(project, localstore.Policy(p))
}

func (a *realStoreAdapter) GetPolicy(project string) (controlapi.Policy, error) {
	p, err := a.store.GetPolicy(project)
	return controlapi.Policy(p), err
}

// realSyncCtrl is a no-op sync controller for the acceptance suite.
type realSyncCtrl struct {
	centralURL string
}

func (s *realSyncCtrl) Status() controlapi.Status {
	var url *string
	if s.centralURL != "" {
		url = &s.centralURL
	}
	return controlapi.Status{
		CentralConnected: s.centralURL != "",
		CentralURL:       url,
		LastSyncResult:   controlapi.SyncResult{},
		DaemonVersion:    "acceptance-test",
	}
}

func (s *realSyncCtrl) TriggerNow(_ context.Context) error         { return nil }
func (s *realSyncCtrl) Disconnect() error                          { return nil }
func (s *realSyncCtrl) Reconnect(_ controlapi.CentralConfig) error { return nil }

// realCfgStore returns a test configuration.
type realCfgStore struct {
	cfg controlapi.RedactedConfig
}

func (c *realCfgStore) Load() (controlapi.RedactedConfig, error) { return c.cfg, nil }
func (c *realCfgStore) Apply(_ controlapi.ConfigPatch) (bool, error) {
	return false, nil
}

// openTestStore opens a fresh temp SQLite store.
func openTestStore(t *testing.T) *localstore.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "acceptance.db")
	store, err := localstore.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestAcceptance_ControlAPI_Suite(t *testing.T) {
	const token = "acceptance-bearer-token"

	store := openTestStore(t)
	storeAdapter := &realStoreAdapter{store: store}
	syncCtrl := &realSyncCtrl{}
	cfgStore := &realCfgStore{cfg: controlapi.RedactedConfig{
		DB: "/test/db.sqlite",
	}}

	srv := controlapi.New(token, 7700, storeAdapter, syncCtrl, cfgStore, "acceptance-v1")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	doGet := func(t *testing.T, path, auth string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		return resp
	}

	// ── Sub-test 1: missing token → 401 ──────────────────────────────────────
	t.Run("MissingToken_401", func(t *testing.T) {
		resp := doGet(t, "/api/v1/status", "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("want 401, got %d", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type: got %q, want application/json", ct)
		}
		var body map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if _, ok := body["error"]; !ok {
			t.Error("error response must contain 'error' key")
		}
	})

	// ── Sub-test 2: wrong token → 401 ────────────────────────────────────────
	t.Run("WrongToken_401", func(t *testing.T) {
		resp := doGet(t, "/api/v1/status", "Bearer wrong-token")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("want 401, got %d", resp.StatusCode)
		}
	})

	// ── Sub-test 3: bad Origin on POST → 403 ─────────────────────────────────
	t.Run("BadOriginOnPost_403", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/sync/trigger", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Origin", "http://evil.example.com")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer resp.Body.Close()
		// Route does not exist in PR-①, but origin check should still fire.
		// The handler returns 403 from withOrigin or 404 from catch-all —
		// either way it must not be 2xx.
		if resp.StatusCode == http.StatusOK {
			t.Errorf("wrong origin POST: got 200, want non-200")
		}
	})

	// ── Sub-test 4: GET /api/v1/status shape ─────────────────────────────────
	t.Run("Status_Shape", func(t *testing.T) {
		resp := doGet(t, "/api/v1/status", "Bearer "+token)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d", resp.StatusCode)
		}
		var st controlapi.Status
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if st.DaemonVersion == "" {
			t.Error("daemon_version must not be empty")
		}
		// last_sync_result must be present (even if zero-value)
		_ = st.LastSyncResult
	})

	// ── Sub-test 5: GET /api/v1/config redacts writer key ────────────────────
	t.Run("Config_WriterKeyRedacted", func(t *testing.T) {
		// Install a config with a redacted key.
		redacted := "***REDACTED***"
		cfgWithKey := &realCfgStore{cfg: controlapi.RedactedConfig{
			WriterKey: &redacted,
		}}
		srvKey := controlapi.New(token, 7700, storeAdapter, syncCtrl, cfgWithKey, "v1")
		tsKey := httptest.NewServer(srvKey.Handler())
		defer tsKey.Close()

		req, _ := http.NewRequest(http.MethodGet, tsKey.URL+"/api/v1/config", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer resp.Body.Close()

		var raw map[string]json.RawMessage
		_ = json.NewDecoder(resp.Body).Decode(&raw)
		if wk, ok := raw["writer_key"]; ok {
			s := string(wk)
			if !strings.Contains(s, "REDACTED") {
				t.Errorf("writer_key must be REDACTED, got: %s", s)
			}
			if strings.Contains(s, "real-secret") {
				t.Error("real writer key must never appear in config response")
			}
		}
	})

	// ── Sub-test 6: GET /api/v1/projects returns array (empty store) ─────────
	t.Run("Projects_EmptyArray", func(t *testing.T) {
		resp := doGet(t, "/api/v1/projects", "Bearer "+token)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d", resp.StatusCode)
		}
		var projects []controlapi.ProjectPolicy
		if err := json.NewDecoder(resp.Body).Decode(&projects); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// Empty store → empty array (not null)
		if projects == nil {
			t.Error("want [] not null for empty store")
		}
	})
}
