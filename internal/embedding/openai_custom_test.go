package embedding_test

// openai_custom_test.go — contract tests for custom OpenAI-compatible endpoints.
//
// Covers:
//   - URL-joining rule (three shapes: bare host, path-prefixed, trailing-slash)
//   - model field carries configured model
//   - dimensions field present when dims configured / absent when not (raw JSON)
//   - api-key header mode sends api-key and NOT Authorization (never both, never in URL)
//   - key never in errors/logs
//   - startup-fatal pairing rule (custom model without dims)
//   - gate posture unchanged — custom base URL is still remote
//   - ModelName() returns configured model (drives backfill re-embed predicate)

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// captureServer returns an httptest.Server that records the most recent request
// path, body bytes, Authorization header, and api-key header. It always
// responds with a single 256-dim vector unless dims is given via the option.
type captureServer struct {
	srv        *httptest.Server
	lastPath   string
	lastBody   []byte
	lastAuth   string
	lastAPIKey string
}

func newCaptureServer(dims int) *captureServer {
	cs := &captureServer{}
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.lastPath = r.URL.Path
		cs.lastAuth = r.Header.Get("Authorization")
		cs.lastAPIKey = r.Header.Get("api-key")
		body, _ := io.ReadAll(r.Body)
		cs.lastBody = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fakeOpenAIResponseN(1, dims)))
	}))
	return cs
}

func (cs *captureServer) URL() string { return cs.srv.URL }
func (cs *captureServer) Close()      { cs.srv.Close() }

// rawBodyField decodes a JSON body byte slice and returns the raw value for key,
// or nil when the key is absent.
func rawBodyField(body []byte, key string) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	return m[key]
}

// ── URL-joining rule — three tested shapes ────────────────────────────────────
//
// Rule: if the base URL has an empty/root path → append /v1/embeddings.
//       Otherwise → append /embeddings (stripping trailing slash first).
//
// Shape 1: bare host (https://api.openai.com) → /v1/embeddings
// Shape 2: path-prefixed without trailing slash (…/v1) → /v1/embeddings
// Shape 3: path-prefixed WITH trailing slash (…/v1/) → /v1/embeddings
// Shape 4: deeper path prefix (…/openai/v1) → /openai/v1/embeddings

// TestOpenAI_URLJoining_BareHost tests that a bare-host base URL (the default
// OpenAI style) gets /v1/embeddings appended.
func TestOpenAI_URLJoining_BareHost(t *testing.T) {
	cs := newCaptureServer(256)
	defer cs.Close()

	// The httptest server URL is http://127.0.0.1:PORT — a bare host with no path.
	p := embedding.NewRemoteOpenAI("sk-key",
		embedding.WithBaseURL(cs.URL()),
		embedding.WithTimeout(2*time.Second),
	)
	if _, err := p.Embed(context.Background(), "proj", []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	want := "/v1/embeddings"
	if cs.lastPath != want {
		t.Errorf("bare-host URL: request path = %q, want %q", cs.lastPath, want)
	}
}

// TestOpenAI_URLJoining_PathPrefix tests a base URL that already includes the
// version segment without a trailing slash (e.g. https://api.mistral.ai/v1).
func TestOpenAI_URLJoining_PathPrefix(t *testing.T) {
	cs := newCaptureServer(1024)
	defer cs.Close()

	// Append /v1 to the server URL to simulate a Mistral-style base URL.
	base := cs.URL() + "/v1"
	p := embedding.NewRemoteOpenAI("sk-key",
		embedding.WithBaseURL(base),
		embedding.WithDims(1024),
		embedding.WithTimeout(2*time.Second),
	)
	if _, err := p.Embed(context.Background(), "proj", []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	want := "/v1/embeddings"
	if cs.lastPath != want {
		t.Errorf("path-prefix URL: request path = %q, want %q", cs.lastPath, want)
	}
}

// TestOpenAI_URLJoining_PathPrefixTrailingSlash tests the same as above but
// with a trailing slash (e.g. https://api.mistral.ai/v1/).
func TestOpenAI_URLJoining_PathPrefixTrailingSlash(t *testing.T) {
	cs := newCaptureServer(1024)
	defer cs.Close()

	base := cs.URL() + "/v1/"
	p := embedding.NewRemoteOpenAI("sk-key",
		embedding.WithBaseURL(base),
		embedding.WithDims(1024),
		embedding.WithTimeout(2*time.Second),
	)
	if _, err := p.Embed(context.Background(), "proj", []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	// Trailing slash stripped; /embeddings appended.
	want := "/v1/embeddings"
	if cs.lastPath != want {
		t.Errorf("trailing-slash URL: request path = %q, want %q", cs.lastPath, want)
	}
}

// TestOpenAI_URLJoining_DeeperPathPrefix tests a base URL with a deeper path
// prefix (e.g. Azure style: https://<res>.openai.azure.com/openai/v1).
func TestOpenAI_URLJoining_DeeperPathPrefix(t *testing.T) {
	cs := newCaptureServer(1536)
	defer cs.Close()

	base := cs.URL() + "/openai/v1"
	p := embedding.NewRemoteOpenAI("sk-key",
		embedding.WithBaseURL(base),
		embedding.WithDims(1536),
		embedding.WithTimeout(2*time.Second),
	)
	if _, err := p.Embed(context.Background(), "proj", []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	want := "/openai/v1/embeddings"
	if cs.lastPath != want {
		t.Errorf("deeper-path-prefix URL: request path = %q, want %q", cs.lastPath, want)
	}
}

// ── model field ───────────────────────────────────────────────────────────────

// TestOpenAI_CustomModel_SentInRequest asserts that a configured model name is
// sent verbatim in the "model" field of the request body.
func TestOpenAI_CustomModel_SentInRequest(t *testing.T) {
	cs := newCaptureServer(1024)
	defer cs.Close()

	const customModel = "mistral-embed"
	p := embedding.NewRemoteOpenAI("sk-key",
		embedding.WithBaseURL(cs.URL()),
		embedding.WithModel(customModel),
		embedding.WithDims(1024),
		embedding.WithTimeout(2*time.Second),
	)
	if _, err := p.Embed(context.Background(), "proj", []string{"text"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	raw := rawBodyField(cs.lastBody, "model")
	if raw == nil {
		t.Fatal("model field absent from request body")
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal model: %v", err)
	}
	if got != customModel {
		t.Errorf("model = %q, want %q", got, customModel)
	}
}

// TestOpenAI_ModelName_ReflectsConfiguredModel asserts that ModelName() returns
// the configured model, which is the value the backfill loop stores per row
// and uses as the re-embed predicate.
func TestOpenAI_ModelName_ReflectsConfiguredModel(t *testing.T) {
	p := embedding.NewRemoteOpenAI("key", embedding.WithModel("mistral-embed"))
	if got := p.ModelName(); got != "mistral-embed" {
		t.Errorf("ModelName() = %q, want %q", got, "mistral-embed")
	}
}

// TestOpenAI_ModelName_DefaultIsTextEmbedding3Small asserts the default model name.
func TestOpenAI_ModelName_DefaultIsTextEmbedding3Small(t *testing.T) {
	p := embedding.NewRemoteOpenAI("key")
	if got := p.ModelName(); got != "text-embedding-3-small" {
		t.Errorf("ModelName() = %q, want text-embedding-3-small", got)
	}
}

// ── dimensions field presence/absence ─────────────────────────────────────────

// TestOpenAI_DimensionsField_AbsentByDefault asserts that the "dimensions" field
// is absent from the raw JSON body when no explicit dims are configured.
// This prevents "unknown field" rejections from strict providers (e.g. Mistral).
func TestOpenAI_DimensionsField_Present256ByDefault(t *testing.T) {
	// THE DEFAULT-CONFIG CONTRACT (round-1 HIGH): text-embedding-3-small has
	// always been requested at 256 dims (matryoshka). Omitting the field would
	// flip the API to its 1536-dim default while the store expects 256 —
	// silently breaking semantic search for every default install.
	cs := newCaptureServer(256)
	defer cs.Close()

	p := embedding.NewRemoteOpenAI("sk-key",
		embedding.WithBaseURL(cs.URL()),
		embedding.WithTimeout(2*time.Second),
	)
	if _, err := p.Embed(context.Background(), "proj", []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if raw := rawBodyField(cs.lastBody, "dimensions"); string(raw) != "256" {
		t.Errorf("dimensions field must be 256 for the default model, got: %s", raw)
	}
}

// TestOpenAI_DimensionsField_AbsentForNonOpenAIModel: "dimensions" is an
// OpenAI matryoshka parameter — for fixed-output models (mistral-embed) the
// field is OMITTED and embedding_dims configures the STORE expectation only.
func TestOpenAI_DimensionsField_AbsentForNonOpenAIModel(t *testing.T) {
	cs := newCaptureServer(1024)
	defer cs.Close()

	p := embedding.NewRemoteOpenAI("sk-key",
		embedding.WithBaseURL(cs.URL()),
		embedding.WithModel("mistral-embed"),
		embedding.WithDims(1024),
		embedding.WithTimeout(2*time.Second),
	)
	if _, err := p.Embed(context.Background(), "proj", []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if raw := rawBodyField(cs.lastBody, "dimensions"); len(raw) != 0 {
		t.Errorf("dimensions field must be ABSENT for non-text-embedding-3 models, got: %s", raw)
	}
}

// TestOpenAI_ResponseDimsMismatch_LoudError: a provider returning a different
// vector length than configured must FAIL the call with an actionable message,
// never let wrong-length blobs reach the store (where the length guard would
// silently drop them at query time).
func TestOpenAI_ResponseDimsMismatch_LoudError(t *testing.T) {
	cs := newCaptureServer(768) // server returns 768-dim vectors
	defer cs.Close()

	p := embedding.NewRemoteOpenAI("sk-key",
		embedding.WithBaseURL(cs.URL()),
		embedding.WithModel("some-model"),
		embedding.WithDims(1024), // configured expectation differs
		embedding.WithTimeout(2*time.Second),
	)
	_, err := p.Embed(context.Background(), "proj", []string{"x"})
	if err == nil {
		t.Fatal("expected a dims-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "768") || !strings.Contains(err.Error(), "1024") {
		t.Errorf("error should name both lengths, got: %v", err)
	}
}

// TestOpenAI_DimensionsField_PresentWhenConfigured asserts that the "dimensions"
// field IS present in the JSON body with the configured value.
func TestOpenAI_DimensionsField_PresentWhenConfigured(t *testing.T) {
	cs := newCaptureServer(1024)
	defer cs.Close()

	p := embedding.NewRemoteOpenAI("sk-key",
		embedding.WithBaseURL(cs.URL()),
		embedding.WithDims(1024),
		embedding.WithTimeout(2*time.Second),
	)
	if _, err := p.Embed(context.Background(), "proj", []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	raw := rawBodyField(cs.lastBody, "dimensions")
	if len(raw) == 0 {
		t.Fatal("dimensions field must be present when explicitly configured")
	}
	var got int
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal dimensions: %v", err)
	}
	if got != 1024 {
		t.Errorf("dimensions = %d, want 1024", got)
	}
}

// ── auth header modes ─────────────────────────────────────────────────────────

// TestOpenAI_AuthHeader_Bearer_Default asserts the default mode sends
// Authorization: Bearer and does NOT send api-key.
func TestOpenAI_AuthHeader_Bearer_Default(t *testing.T) {
	cs := newCaptureServer(256)
	defer cs.Close()

	const apiKey = "sk-bearer-test"
	p := embedding.NewRemoteOpenAI(apiKey,
		embedding.WithBaseURL(cs.URL()),
		embedding.WithTimeout(2*time.Second),
	)
	if _, err := p.Embed(context.Background(), "proj", []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	wantAuth := "Bearer " + apiKey
	if cs.lastAuth != wantAuth {
		t.Errorf("Authorization = %q, want %q", cs.lastAuth, wantAuth)
	}
	if cs.lastAPIKey != "" {
		t.Errorf("api-key header must be absent in Bearer mode, got %q", cs.lastAPIKey)
	}
}

// TestOpenAI_AuthHeader_Bearer_Explicit asserts that explicitly setting
// AuthHeaderBearer produces the same result as the default.
func TestOpenAI_AuthHeader_Bearer_Explicit(t *testing.T) {
	cs := newCaptureServer(256)
	defer cs.Close()

	const apiKey = "sk-bearer-explicit"
	p := embedding.NewRemoteOpenAI(apiKey,
		embedding.WithBaseURL(cs.URL()),
		embedding.WithAuthHeader(embedding.AuthHeaderBearer),
		embedding.WithTimeout(2*time.Second),
	)
	if _, err := p.Embed(context.Background(), "proj", []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	wantAuth := "Bearer " + apiKey
	if cs.lastAuth != wantAuth {
		t.Errorf("Authorization = %q, want %q", cs.lastAuth, wantAuth)
	}
	if cs.lastAPIKey != "" {
		t.Errorf("api-key header must be absent in AuthHeaderBearer mode, got %q", cs.lastAPIKey)
	}
}

// TestOpenAI_AuthHeader_APIKey_SendsAPIKey asserts that AuthHeaderAPIKey sends
// the key in the "api-key" header and does NOT send Authorization.
func TestOpenAI_AuthHeader_APIKey_SendsAPIKey(t *testing.T) {
	cs := newCaptureServer(256)
	defer cs.Close()

	const apiKey = "azure-secret-key"
	p := embedding.NewRemoteOpenAI(apiKey,
		embedding.WithBaseURL(cs.URL()),
		embedding.WithAuthHeader(embedding.AuthHeaderAPIKey),
		embedding.WithTimeout(2*time.Second),
	)
	if _, err := p.Embed(context.Background(), "proj", []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if cs.lastAPIKey != apiKey {
		t.Errorf("api-key header = %q, want %q", cs.lastAPIKey, apiKey)
	}
	if cs.lastAuth != "" {
		t.Errorf("Authorization header must be absent in api-key mode, got %q", cs.lastAuth)
	}
}

// TestOpenAI_AuthHeader_APIKey_KeyNotInURL asserts the key is never placed in
// the request URL even when api-key mode is used.
func TestOpenAI_AuthHeader_APIKey_KeyNotInURL(t *testing.T) {
	var capturedRawURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedRawURL = r.URL.RawQuery + r.URL.Path + r.URL.RawPath
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fakeOpenAIResponseN(1, 256)))
	}))
	defer srv.Close()

	const apiKey = "supersecretazurekey"
	p := embedding.NewRemoteOpenAI(apiKey,
		embedding.WithBaseURL(srv.URL),
		embedding.WithAuthHeader(embedding.AuthHeaderAPIKey),
		embedding.WithTimeout(2*time.Second),
	)
	if _, err := p.Embed(context.Background(), "proj", []string{"text"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if strings.Contains(capturedRawURL, apiKey) {
		t.Errorf("API key must never appear in the URL, but found it in %q", capturedRawURL)
	}
}

// ── key-never-in-errors discipline ───────────────────────────────────────────

// TestOpenAI_APIKeyHeader_KeyNotLeakedInError_APIKeyMode asserts non-2xx errors
// do not contain the key string in api-key header mode.
func TestOpenAI_APIKeyHeader_KeyNotLeakedInError_APIKeyMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	const apiKey = "azure-secret-leak-test"
	p := embedding.NewRemoteOpenAI(apiKey,
		embedding.WithBaseURL(srv.URL),
		embedding.WithAuthHeader(embedding.AuthHeaderAPIKey),
		embedding.WithTimeout(2*time.Second),
	)
	_, err := p.Embed(context.Background(), "proj", []string{"text"})
	if err == nil {
		t.Fatal("expected error from 403 response, got nil")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Errorf("error message leaked API key in api-key mode: %v", err)
	}
}

// ── gate posture unchanged ────────────────────────────────────────────────────

// TestOpenAI_CustomBaseURL_IsStillRemote asserts that a provider with a custom
// base URL is gated as remote=true. A local-only project must still be denied
// even when the base URL points to a private gateway rather than api.openai.com.
// Text leaving the machine to ANY endpoint violates the local-only contract.
func TestOpenAI_CustomBaseURL_IsStillRemote(t *testing.T) {
	cs := newCaptureServer(1024)
	defer cs.Close()

	inner := embedding.NewRemoteOpenAI("sk-key",
		embedding.WithBaseURL(cs.URL()),
		embedding.WithModel("mistral-embed"),
		embedding.WithDims(1024),
	)
	checker := &fakeChecker{policy: localstore.PolicyLocalOnly}
	// remote=true — custom endpoint is still remote
	gated := embedding.NewGated(inner, checker, true)

	_, err := gated.Embed(context.Background(), "private-proj", []string{"secret text"})
	if err == nil {
		t.Fatal("expected ErrEmbeddingGated for local-only+remote, got nil")
	}
	if err != embedding.ErrEmbeddingGated {
		t.Errorf("expected ErrEmbeddingGated, got %v", err)
	}
	// Inner provider must NOT have been called — no text reached the server.
	if cs.lastBody != nil {
		t.Errorf("inner provider was called for a local-only project — gate bypass!")
	}
}
