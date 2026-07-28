package controlapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/controlapi"
)

// mustTime parses an RFC3339 timestamp or fails the test.
func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

// putConfig is a small helper: PUT /api/v1/config with body and return the
// recorder plus the config store the handler talked to.
func putConfig(t *testing.T, body map[string]any) (*httptest.ResponseRecorder, *mockConfigStorePR3) {
	t.Helper()
	cfgStore := &mockConfigStorePR3{restartRequired: true}
	srv := newServerPR3(t, nil, cfgStore)
	req := buildPUT(t, "/api/v1/config", body)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w, cfgStore
}

// TestConfigPutAcceptsObsidianKeys covers REQ-WATCH-09: all four obsidian_*
// keys are patchable through PUT /api/v1/config and reach ConfigStore.Apply.
func TestConfigPutAcceptsObsidianKeys(t *testing.T) {
	w, cfgStore := putConfig(t, map[string]any{
		"obsidian_vault":        `D:\Vault`,
		"obsidian_interval":     "15m",
		"obsidian_project":      "engram",
		"obsidian_graph_config": "force",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body)
	}

	p := cfgStore.lastPatch
	if p.ObsidianVault == nil || *p.ObsidianVault != `D:\Vault` {
		t.Errorf("ObsidianVault patch = %v, want %q", p.ObsidianVault, `D:\Vault`)
	}
	if p.ObsidianInterval == nil || *p.ObsidianInterval != "15m" {
		t.Errorf("ObsidianInterval patch = %v, want %q", p.ObsidianInterval, "15m")
	}
	if p.ObsidianProject == nil || *p.ObsidianProject != "engram" {
		t.Errorf("ObsidianProject patch = %v, want %q", p.ObsidianProject, "engram")
	}
	if p.ObsidianGraphConfig == nil || *p.ObsidianGraphConfig != "force" {
		t.Errorf("ObsidianGraphConfig patch = %v, want %q", p.ObsidianGraphConfig, "force")
	}
}

// TestConfigPutObsidianKeysReportRestartRequired covers REQ-WATCH-09's
// "reported as restart-required": the handler forwards whatever the store
// reports, and the store (config.Patch) reports true for these keys.
func TestConfigPutObsidianKeysReportRestartRequired(t *testing.T) {
	w, _ := putConfig(t, map[string]any{"obsidian_vault": `D:\Vault`})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body)
	}
	var resp map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp["restart_required"] {
		t.Error("restart_required = false, want true for obsidian_vault")
	}
}

// TestConfigPutRejectsUnknownObsidianKey pins that the known-key allowlist was
// extended with EXACTLY the four documented keys and nothing adjacent: a
// plausible-looking fifth key must still 400 rather than 200-no-op.
func TestConfigPutRejectsUnknownObsidianKey(t *testing.T) {
	for _, key := range []string{"obsidian_enabled", "obsidian_vault_path", "obsidian"} {
		w, _ := putConfig(t, map[string]any{key: "x"})
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body: %s", key, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), key) {
			t.Errorf("%s: body %q should name the rejected key", key, w.Body.String())
		}
	}
}

// TestConfigPutRejectsBadObsidianInterval is the write-time half of the
// negative-duration defence (REQ-WATCH-04: "An out-of-range value MUST be
// rejected with 400 at PUT time, so a bad value does not reach disk through the
// API"). It mirrors exactly what config.Load rejects at startup, so the API can
// never persist a value that bricks the next boot.
func TestConfigPutRejectsBadObsidianInterval(t *testing.T) {
	cases := []struct {
		value string
		why   string
	}{
		{"ten minutes", "unparseable"},
		{"-1s", "negative — time.ParseDuration accepts it, a timer would spin"},
		{"-2562047h47m16.854775808s", "math.MinInt64 — negating it overflows back to negative"},
		{"0s", "zero is indistinguishable from unset once stored"},
	}
	for _, tc := range cases {
		w, cfgStore := putConfig(t, map[string]any{"obsidian_interval": tc.value})
		if w.Code != http.StatusBadRequest {
			t.Errorf("obsidian_interval=%q (%s): status = %d, want 400; body: %s", tc.value, tc.why, w.Code, w.Body)
		}
		if cfgStore.lastPatch.ObsidianInterval != nil {
			t.Errorf("obsidian_interval=%q (%s): reached ConfigStore.Apply — the handler must reject BEFORE anything can be persisted", tc.value, tc.why)
		}
	}
}

// TestConfigPutAcceptsGoodObsidianInterval triangulates the rejection above: a
// normal value AND a sub-minute value both pass. Sub-minute is deliberately
// NOT a 400 — REQ-WATCH-04 clamps it at runtime with a warning, and rejecting
// it here would make that clamp unreachable through the API.
func TestConfigPutAcceptsGoodObsidianInterval(t *testing.T) {
	for _, value := range []string{"10m", "1m", "30s", "1h"} {
		w, cfgStore := putConfig(t, map[string]any{"obsidian_interval": value})
		if w.Code != http.StatusOK {
			t.Errorf("obsidian_interval=%q: status = %d, want 200; body: %s", value, w.Code, w.Body)
		}
		if cfgStore.lastPatch.ObsidianInterval == nil || *cfgStore.lastPatch.ObsidianInterval != value {
			t.Errorf("obsidian_interval=%q did not reach Apply", value)
		}
	}
}

// TestConfigPutRejectsBadObsidianGraphConfig covers the enum guard. Note the
// asymmetry with config.Load, which is LENIENT (falls back to preserve with a
// warning): strict at write time, lenient at read time — the brick guard
// satisfied from both directions without ever refusing to boot.
func TestConfigPutRejectsBadObsidianGraphConfig(t *testing.T) {
	for _, value := range []string{"presrve", "PRESERVE", "overwrite", "yes"} {
		w, cfgStore := putConfig(t, map[string]any{"obsidian_graph_config": value})
		if w.Code != http.StatusBadRequest {
			t.Errorf("obsidian_graph_config=%q: status = %d, want 400; body: %s", value, w.Code, w.Body)
		}
		if cfgStore.lastPatch.ObsidianGraphConfig != nil {
			t.Errorf("obsidian_graph_config=%q reached ConfigStore.Apply, want rejection first", value)
		}
	}
}

// TestConfigPutAcceptsGoodObsidianGraphConfig triangulates: every valid mode
// (plus the empty "unset" reset) is accepted.
func TestConfigPutAcceptsGoodObsidianGraphConfig(t *testing.T) {
	for _, value := range []string{"preserve", "force", "skip", ""} {
		w, cfgStore := putConfig(t, map[string]any{"obsidian_graph_config": value})
		if w.Code != http.StatusOK {
			t.Errorf("obsidian_graph_config=%q: status = %d, want 200; body: %s", value, w.Code, w.Body)
		}
		if cfgStore.lastPatch.ObsidianGraphConfig == nil || *cfgStore.lastPatch.ObsidianGraphConfig != value {
			t.Errorf("obsidian_graph_config=%q did not reach Apply", value)
		}
	}
}

// ── Phase 9 review MINOR-2: obsidian_vault has no validation at write time ──

// TestConfigPutRejectsRelativeObsidianVault covers the write-time half of the
// relative-path defence: config.Load rejects the identical relative-path
// case at startup, so a PUT that accepted one would persist a value that
// bricks the next boot.
func TestConfigPutRejectsRelativeObsidianVault(t *testing.T) {
	for _, value := range []string{"relative/vault", "vault", "./vault", "../vault"} {
		w, cfgStore := putConfig(t, map[string]any{"obsidian_vault": value})
		if w.Code != http.StatusBadRequest {
			t.Errorf("obsidian_vault=%q: status = %d, want 400; body: %s", value, w.Code, w.Body)
		}
		if cfgStore.lastPatch.ObsidianVault != nil {
			t.Errorf("obsidian_vault=%q reached ConfigStore.Apply — the handler must reject BEFORE anything can be persisted", value)
		}
	}
}

// TestConfigPutAcceptsAbsoluteObsidianVault triangulates: an absolute path,
// and the empty string (turning the feature off), both pass.
func TestConfigPutAcceptsAbsoluteObsidianVault(t *testing.T) {
	for _, value := range []string{`D:\Vault`, `D:\Vault\Sub`, ""} {
		w, cfgStore := putConfig(t, map[string]any{"obsidian_vault": value})
		if w.Code != http.StatusOK {
			t.Errorf("obsidian_vault=%q: status = %d, want 200; body: %s", value, w.Code, w.Body)
		}
		if cfgStore.lastPatch.ObsidianVault == nil || *cfgStore.lastPatch.ObsidianVault != value {
			t.Errorf("obsidian_vault=%q did not reach Apply", value)
		}
	}
}

// TestStatusOmitsObsidianExportWhenDisabled covers REQ-WATCH-11: the status
// object must be ABSENT (not an all-zero object) when the feature is off,
// following the EmbeddingBackfill precedent — a permanent zero object would
// misreport a disabled feature as a healthy one.
func TestStatusOmitsObsidianExportWhenDisabled(t *testing.T) {
	srv := newServerPR3(t, &mockSyncControllerFull{status: controlapi.Status{}}, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", http.NoBody)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw["obsidian_export"]; present {
		t.Errorf("obsidian_export present in status when the feature is off: %s", w.Body)
	}
}

// TestStatusIncludesObsidianExportWhenConfigured is the triangulation partner:
// when the loop exists, the field carries the last cycle's counters, the vault
// and the cadence.
func TestStatusIncludesObsidianExportWhenConfigured(t *testing.T) {
	at := mustTime(t, "2026-07-27T12:00:00Z")
	sync := &mockSyncControllerFull{status: controlapi.Status{
		ObsidianExport: &controlapi.ObsidianExport{
			LastExportAt: &at,
			Created:      4638,
			Updated:      1,
			Deleted:      2,
			Skipped:      3,
			Hubs:         508,
			Vault:        `D:\Vault`,
			Interval:     "10m",
		},
	}}
	srv := newServerPR3(t, sync, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", http.NoBody)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body)
	}
	var got struct {
		ObsidianExport *controlapi.ObsidianExport `json:"obsidian_export"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ObsidianExport == nil {
		t.Fatalf("obsidian_export absent from status when the feature is on: %s", w.Body)
	}
	if got.ObsidianExport.Created != 4638 || got.ObsidianExport.Hubs != 508 {
		t.Errorf("counters = created %d hubs %d, want 4638 / 508", got.ObsidianExport.Created, got.ObsidianExport.Hubs)
	}
	if got.ObsidianExport.Vault != `D:\Vault` {
		t.Errorf("vault = %q, want %q", got.ObsidianExport.Vault, `D:\Vault`)
	}
	if got.ObsidianExport.Interval != "10m" {
		t.Errorf("interval = %q, want %q", got.ObsidianExport.Interval, "10m")
	}
	if got.ObsidianExport.LastExportAt == nil || !got.ObsidianExport.LastExportAt.Equal(at) {
		t.Errorf("last_export_at = %v, want %v", got.ObsidianExport.LastExportAt, at)
	}
	if got.ObsidianExport.Error != nil {
		t.Errorf("error = %v, want nil on a healthy cycle", *got.ObsidianExport.Error)
	}
}

// TestStatusCarriesLastObsidianError covers the whole reason the status field
// exists: the Obsidian viewer cannot distinguish "daemon dead" from "export
// failing every cycle" — both look like a timestamp that stopped moving. The
// last error string is the only place that answer can live.
func TestStatusCarriesLastObsidianError(t *testing.T) {
	msg := "open vault: permission denied"
	at := mustTime(t, "2026-07-27T12:00:00Z")
	sync := &mockSyncControllerFull{status: controlapi.Status{
		ObsidianExport: &controlapi.ObsidianExport{
			LastExportAt:        &at,
			Error:               &msg,
			ConsecutiveFailures: 7,
		},
	}}
	srv := newServerPR3(t, sync, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", http.NoBody)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	var got struct {
		ObsidianExport *controlapi.ObsidianExport `json:"obsidian_export"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ObsidianExport == nil {
		t.Fatalf("obsidian_export absent: %s", w.Body)
	}
	if got.ObsidianExport.Error == nil || *got.ObsidianExport.Error != msg {
		t.Errorf("error = %v, want %q", got.ObsidianExport.Error, msg)
	}
	if got.ObsidianExport.ConsecutiveFailures != 7 {
		t.Errorf("consecutive_failures = %d, want 7", got.ObsidianExport.ConsecutiveFailures)
	}
}
