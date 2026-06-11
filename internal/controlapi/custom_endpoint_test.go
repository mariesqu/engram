package controlapi_test

// custom_endpoint_test.go — PUT /api/v1/config validation tests for
// embedding_base_url, embedding_model, and embedding_auth_header.
//
// Covers:
//   - Valid base URL → 200 with restart_required=true
//   - Invalid base URL (non-http scheme, relative) → 400
//   - Valid auth header values ("", "authorization", "api-key") → 200
//   - Invalid auth header value → 400
//   - Unknown key still rejected → 400
//   - restart_required=true for each new field on change

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ── embedding_base_url validation ─────────────────────────────────────────────

func TestPUT_Config_EmbeddingBaseURL_Valid_200(t *testing.T) {
	cases := []string{
		"https://api.mistral.ai/v1",
		"https://api.mistral.ai/v1/",
		"http://localhost:8080/v1",
		"https://example.openai.azure.com/openai/v1",
		"", // empty = use default
	}
	for _, u := range cases {
		cfgStore := &mockConfigStorePR3{restartRequired: true}
		srv := newServerPR3(t, nil, cfgStore)

		body := map[string]any{"embedding_base_url": u}
		req := buildPUT(t, "/api/v1/config", body)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("embedding_base_url=%q: got %d, want 200; body: %s", u, w.Code, w.Body)
		}
	}
}

func TestPUT_Config_EmbeddingBaseURL_Invalid_400(t *testing.T) {
	cases := []string{
		"ftp://example.com/v1", // non-http scheme
		"not-a-url",            // no scheme
		"/v1",                  // relative
		"file:///etc/passwd",   // file scheme
	}
	for _, u := range cases {
		srv := newServerPR3(t, nil, nil)

		body := map[string]any{"embedding_base_url": u}
		req := buildPUT(t, "/api/v1/config", body)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("embedding_base_url=%q: got %d, want 400; body: %s", u, w.Code, w.Body)
		}
	}
}

func TestPUT_Config_EmbeddingBaseURL_RestartRequired(t *testing.T) {
	cfgStore := &mockConfigStorePR3{restartRequired: true}
	srv := newServerPR3(t, nil, cfgStore)

	req := buildPUT(t, "/api/v1/config", map[string]any{"embedding_base_url": "https://api.mistral.ai/v1"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var resp map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp["restart_required"] {
		t.Error("embedding_base_url change must report restart_required=true")
	}
}

// ── embedding_auth_header validation ─────────────────────────────────────────

func TestPUT_Config_EmbeddingAuthHeader_Valid_200(t *testing.T) {
	cases := []string{"", "authorization", "api-key"}
	for _, h := range cases {
		cfgStore := &mockConfigStorePR3{restartRequired: true}
		srv := newServerPR3(t, nil, cfgStore)

		req := buildPUT(t, "/api/v1/config", map[string]any{"embedding_auth_header": h})
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("embedding_auth_header=%q: got %d, want 200; body: %s", h, w.Code, w.Body)
		}
	}
}

func TestPUT_Config_EmbeddingAuthHeader_Invalid_400(t *testing.T) {
	cases := []string{"bearer", "x-api-key", "Authorization", "Bearer"}
	for _, h := range cases {
		srv := newServerPR3(t, nil, nil)

		req := buildPUT(t, "/api/v1/config", map[string]any{"embedding_auth_header": h})
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("embedding_auth_header=%q: got %d, want 400; body: %s", h, w.Code, w.Body)
		}
	}
}

func TestPUT_Config_EmbeddingAuthHeader_RestartRequired(t *testing.T) {
	cfgStore := &mockConfigStorePR3{restartRequired: true}
	srv := newServerPR3(t, nil, cfgStore)

	req := buildPUT(t, "/api/v1/config", map[string]any{"embedding_auth_header": "api-key"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var resp map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp["restart_required"] {
		t.Error("embedding_auth_header change must report restart_required=true")
	}
}

// ── embedding_model ───────────────────────────────────────────────────────────

// TestPUT_Config_EmbeddingModel_Valid_200 asserts any non-empty string is accepted
// for embedding_model at PUT time (model×dims pairing is enforced at startup, not
// here — the PUT sees one key at a time and cannot observe combined state).
func TestPUT_Config_EmbeddingModel_Valid_200(t *testing.T) {
	cfgStore := &mockConfigStorePR3{restartRequired: true}
	srv := newServerPR3(t, nil, cfgStore)

	req := buildPUT(t, "/api/v1/config", map[string]any{"embedding_model": "mistral-embed"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("embedding_model: got %d, want 200; body: %s", w.Code, w.Body)
	}
}

func TestPUT_Config_EmbeddingModel_RestartRequired(t *testing.T) {
	cfgStore := &mockConfigStorePR3{restartRequired: true}
	srv := newServerPR3(t, nil, cfgStore)

	req := buildPUT(t, "/api/v1/config", map[string]any{"embedding_model": "text-embedding-ada-002"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var resp map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp["restart_required"] {
		t.Error("embedding_model change must report restart_required=true")
	}
}

// ── unknown key still rejected ────────────────────────────────────────────────

func TestPUT_Config_UnknownKey_StillRejected(t *testing.T) {
	srv := newServerPR3(t, nil, nil)

	req := buildPUT(t, "/api/v1/config", map[string]any{"embedding_base_url_typo": "https://example.com"})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown key: got %d, want 400; body: %s", w.Code, w.Body)
	}
}
