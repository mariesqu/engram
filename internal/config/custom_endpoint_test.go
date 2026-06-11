package config_test

// custom_endpoint_test.go — tests for the three new custom-endpoint config keys:
// embedding_base_url, embedding_model, embedding_auth_header.
//
// Covers:
//   - Round-trip: Save → Load for all three new fields
//   - ValidateEmbeddingBaseURL: empty OK, valid URL OK, non-http scheme error, no host error
//   - Load: invalid base URL returns error
//   - Load: invalid auth header enum returns error
//   - Load: custom model without dims returns startup-fatal error
//   - Patch: new fields trigger restart_required=true on change
//   - Redact: new fields reflected in RedactedConfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mariesqu/engram/internal/config"
)

// ── ValidateEmbeddingBaseURL ──────────────────────────────────────────────────

func TestValidateEmbeddingBaseURL_Empty(t *testing.T) {
	if err := config.ValidateEmbeddingBaseURL(""); err != nil {
		t.Errorf("empty string should be valid (use default), got: %v", err)
	}
}

func TestValidateEmbeddingBaseURL_ValidHTTPS(t *testing.T) {
	cases := []string{
		"https://api.mistral.ai/v1",
		"https://api.mistral.ai/v1/",
		"https://example.openai.azure.com/openai/v1",
		"http://localhost:8080/v1",
	}
	for _, u := range cases {
		if err := config.ValidateEmbeddingBaseURL(u); err != nil {
			t.Errorf("ValidateEmbeddingBaseURL(%q) = %v, want nil", u, err)
		}
	}
}

func TestValidateEmbeddingBaseURL_NonHTTPScheme(t *testing.T) {
	cases := []string{
		"ftp://example.com/v1",
		"file:///etc/passwd",
		"ws://example.com",
	}
	for _, u := range cases {
		if err := config.ValidateEmbeddingBaseURL(u); err == nil {
			t.Errorf("ValidateEmbeddingBaseURL(%q) should return error for non-http(s) scheme", u)
		}
	}
}

func TestValidateEmbeddingBaseURL_RelativeURL(t *testing.T) {
	if err := config.ValidateEmbeddingBaseURL("/v1"); err == nil {
		t.Error("relative URL /v1 should return error (no scheme)")
	}
}

func TestValidateEmbeddingBaseURL_NoHost(t *testing.T) {
	if err := config.ValidateEmbeddingBaseURL("https://"); err == nil {
		t.Error("https:// with no host should return error")
	}
}

// ── Load validation ───────────────────────────────────────────────────────────

func writeConfigJSON(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(content), 0600); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
}

func TestLoad_InvalidBaseURL(t *testing.T) {
	dir := t.TempDir()
	writeConfigJSON(t, dir, `{"embedding_base_url":"ftp://bad.example.com/v1"}`)
	_, err := config.Load(dir)
	if err == nil {
		t.Error("expected error for invalid embedding_base_url, got nil")
	}
}

func TestLoad_InvalidAuthHeader(t *testing.T) {
	dir := t.TempDir()
	writeConfigJSON(t, dir, `{"embedding_auth_header":"x-custom-auth"}`)
	_, err := config.Load(dir)
	if err == nil {
		t.Error("expected error for unsupported embedding_auth_header, got nil")
	}
}

// TestLoad_CustomModelWithoutDims_IsFatal asserts that configuring a custom model
// without explicit embedding_dims is a startup-fatal error.
// The store's length guard and cosine math require knowing the exact vector size.
func TestLoad_CustomModelWithoutDims_IsFatal(t *testing.T) {
	dir := t.TempDir()
	writeConfigJSON(t, dir, `{"embedding_model":"mistral-embed"}`)
	_, err := config.Load(dir)
	if err == nil {
		t.Error("expected fatal error for custom model without dims, got nil")
	}
	// Error message should mention the model name and dims.
	if err != nil && !containsAll(err.Error(), "mistral-embed", "embedding_dims") {
		t.Errorf("error message %q should mention model name and embedding_dims", err.Error())
	}
}

// TestLoad_CustomModelWithDims_OK asserts that the model×dims pairing is valid.
func TestLoad_CustomModelWithDims_OK(t *testing.T) {
	dir := t.TempDir()
	writeConfigJSON(t, dir, `{"embedding_model":"mistral-embed","embedding_dims":1024}`)
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.EmbeddingModel != "mistral-embed" {
		t.Errorf("EmbeddingModel = %q, want mistral-embed", cfg.EmbeddingModel)
	}
	if cfg.EmbeddingDims != 1024 {
		t.Errorf("EmbeddingDims = %d, want 1024", cfg.EmbeddingDims)
	}
}

// TestLoad_DefaultModelWithoutDims_OK asserts that the default model with no
// explicit dims (the keyless-simple default pair) is valid.
func TestLoad_DefaultModelWithoutDims_OK(t *testing.T) {
	dir := t.TempDir()
	writeConfigJSON(t, dir, `{"embedding_provider":"openai"}`)
	_, err := config.Load(dir)
	if err != nil {
		t.Errorf("default model + no dims should be valid, got: %v", err)
	}
}

// TestLoad_ValidAuthHeaders asserts accepted embedding_auth_header values.
func TestLoad_ValidAuthHeaders(t *testing.T) {
	cases := []string{"", "authorization", "api-key"}
	for _, h := range cases {
		dir := t.TempDir()
		var content string
		if h == "" {
			content = `{}`
		} else {
			content = `{"embedding_auth_header":"` + h + `"}`
		}
		writeConfigJSON(t, dir, content)
		if _, err := config.Load(dir); err != nil {
			t.Errorf("embedding_auth_header=%q should be valid, got: %v", h, err)
		}
	}
}

// ── Round-trip: Save → Load ───────────────────────────────────────────────────

func TestConfig_RoundTrip_NewFields(t *testing.T) {
	dir := t.TempDir()
	original := config.Config{
		EmbeddingProvider:   "openai",
		EmbeddingBaseURL:    "https://api.mistral.ai/v1",
		EmbeddingModel:      "mistral-embed",
		EmbeddingDims:       1024,
		EmbeddingAuthHeader: "api-key",
	}
	if err := config.Save(dir, original); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.EmbeddingBaseURL != original.EmbeddingBaseURL {
		t.Errorf("EmbeddingBaseURL = %q, want %q", loaded.EmbeddingBaseURL, original.EmbeddingBaseURL)
	}
	if loaded.EmbeddingModel != original.EmbeddingModel {
		t.Errorf("EmbeddingModel = %q, want %q", loaded.EmbeddingModel, original.EmbeddingModel)
	}
	if loaded.EmbeddingDims != original.EmbeddingDims {
		t.Errorf("EmbeddingDims = %d, want %d", loaded.EmbeddingDims, original.EmbeddingDims)
	}
	if loaded.EmbeddingAuthHeader != original.EmbeddingAuthHeader {
		t.Errorf("EmbeddingAuthHeader = %q, want %q", loaded.EmbeddingAuthHeader, original.EmbeddingAuthHeader)
	}
}

// ── Patch: restart_required on change ────────────────────────────────────────

func str(s string) *string { return &s }

func TestPatch_EmbeddingBaseURL_RestartRequired(t *testing.T) {
	base := config.Config{}
	_, restart := config.Patch(base, config.ConfigPatch{EmbeddingBaseURL: str("https://api.mistral.ai/v1")})
	if !restart {
		t.Error("changing embedding_base_url must require restart")
	}
}

func TestPatch_EmbeddingModel_RestartRequired(t *testing.T) {
	base := config.Config{}
	_, restart := config.Patch(base, config.ConfigPatch{EmbeddingModel: str("mistral-embed")})
	if !restart {
		t.Error("changing embedding_model must require restart")
	}
}

func TestPatch_EmbeddingAuthHeader_RestartRequired(t *testing.T) {
	base := config.Config{}
	_, restart := config.Patch(base, config.ConfigPatch{EmbeddingAuthHeader: str("api-key")})
	if !restart {
		t.Error("changing embedding_auth_header must require restart")
	}
}

func TestPatch_NoChange_NoRestart(t *testing.T) {
	base := config.Config{
		EmbeddingBaseURL:    "https://api.mistral.ai/v1",
		EmbeddingModel:      "mistral-embed",
		EmbeddingAuthHeader: "api-key",
	}
	// Patching with the SAME values must not trigger restart.
	_, restart := config.Patch(base, config.ConfigPatch{
		EmbeddingBaseURL:    str("https://api.mistral.ai/v1"),
		EmbeddingModel:      str("mistral-embed"),
		EmbeddingAuthHeader: str("api-key"),
	})
	if restart {
		t.Error("patching with identical values must not require restart")
	}
}

// ── Redact: new fields surfaced ───────────────────────────────────────────────

func TestRedact_NewFieldsSurfaced(t *testing.T) {
	cfg := config.Config{
		EmbeddingBaseURL:    "https://api.mistral.ai/v1",
		EmbeddingModel:      "mistral-embed",
		EmbeddingAuthHeader: "api-key",
	}
	rc := cfg.Redact()
	if rc.EmbeddingBaseURL != "https://api.mistral.ai/v1" {
		t.Errorf("Redact.EmbeddingBaseURL = %q", rc.EmbeddingBaseURL)
	}
	if rc.EmbeddingModel != "mistral-embed" {
		t.Errorf("Redact.EmbeddingModel = %q", rc.EmbeddingModel)
	}
	if rc.EmbeddingAuthHeader != "api-key" {
		t.Errorf("Redact.EmbeddingAuthHeader = %q", rc.EmbeddingAuthHeader)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
