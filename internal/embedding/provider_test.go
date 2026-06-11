package embedding_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
)

// ── Task 1.1: Interface compile-time satisfaction ────────────────────────────

// TestProvider_InterfaceIsSatisfied ensures all three concrete types satisfy
// EmbeddingProvider at compile time. The compiler enforces this via the var
// declarations in provider.go and gated.go; this test makes the intent explicit.
func TestProvider_InterfaceIsSatisfied(t *testing.T) {
	// NoopProvider
	var _ embedding.EmbeddingProvider = embedding.NoopProvider{}
	// RemoteOpenAIProvider
	var _ embedding.EmbeddingProvider = embedding.NewRemoteOpenAI("key")
	// gatedProvider (returned as interface)
	mock := newRecordingMock(256)
	checker := &fakeChecker{policy: localstore.PolicySynced}
	var _ embedding.EmbeddingProvider = embedding.NewGated(mock, checker, true)
}

// TestErrEmbeddingGated_IsDistinct verifies ErrEmbeddingGated is a distinct sentinel.
func TestErrEmbeddingGated_IsDistinct(t *testing.T) {
	err := embedding.ErrEmbeddingGated
	if err == nil {
		t.Fatal("ErrEmbeddingGated must not be nil")
	}
	if !errors.Is(err, embedding.ErrEmbeddingGated) {
		t.Error("errors.Is(ErrEmbeddingGated, ErrEmbeddingGated) must be true")
	}
	other := errors.New("other error")
	if errors.Is(other, embedding.ErrEmbeddingGated) {
		t.Error("errors.Is(other, ErrEmbeddingGated) must be false")
	}
}

// ── Task 1.2: Gate — all six policy × remote combinations ───────────────────

// fakeChecker is a minimal PolicyChecker for gate tests.
type fakeChecker struct {
	policy localstore.Policy
	err    error
}

func (f *fakeChecker) GetPolicy(_ string) (localstore.Policy, error) {
	return f.policy, f.err
}

// TestGate_AllSixCombinations covers all policy × remote cells from the spec.
func TestGate_AllSixCombinations(t *testing.T) {
	type row struct {
		policy localstore.Policy
		remote bool
		want   bool
		label  string
	}
	rows := []row{
		{localstore.PolicyOmitted, true, false, "omitted+remote"},
		{localstore.PolicyOmitted, false, false, "omitted+local"},
		{localstore.PolicyLocalOnly, true, false, "local-only+remote"},
		{localstore.PolicyLocalOnly, false, false, "local-only+local (PR-1 no consent)"},
		{localstore.PolicySynced, true, true, "synced+remote"},
		{localstore.PolicySynced, false, true, "synced+local"},
	}
	for _, r := range rows {
		got := embedding.EligibleForEmbedding(r.policy, r.remote)
		if got != r.want {
			t.Errorf("%s: EligibleForEmbedding(%q, remote=%v) = %v, want %v",
				r.label, r.policy, r.remote, got, r.want)
		}
	}
}

// TestGate_SyncedRemote_Delegates asserts that a synced project delegates to inner.
func TestGate_SyncedRemote_Delegates(t *testing.T) {
	mock := newRecordingMock(256)
	checker := &fakeChecker{policy: localstore.PolicySynced}
	gated := embedding.NewGated(mock, checker, true)

	vecs, err := gated.Embed(context.Background(), "my-project", []string{"hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vecs) != 1 {
		t.Fatalf("expected 1 vector, got %d", len(vecs))
	}
	if mock.callCount() != 1 {
		t.Errorf("inner should have received 1 call, got %d", mock.callCount())
	}
}

// TestGate_OmittedRemote_ReturnsGatedError asserts that omitted project returns ErrEmbeddingGated.
func TestGate_OmittedRemote_ReturnsGatedError(t *testing.T) {
	mock := newRecordingMock(256)
	checker := &fakeChecker{policy: localstore.PolicyOmitted}
	gated := embedding.NewGated(mock, checker, true)

	_, err := gated.Embed(context.Background(), "secret-project", []string{"sensitive text"})
	if !errors.Is(err, embedding.ErrEmbeddingGated) {
		t.Errorf("expected ErrEmbeddingGated, got %v", err)
	}
	if mock.callCount() != 0 {
		t.Errorf("inner must NOT be called for omitted project, got %d calls", mock.callCount())
	}
}

// TestGate_LocalOnlyRemote_ReturnsGatedError asserts local-only + remote provider is denied.
func TestGate_LocalOnlyRemote_ReturnsGatedError(t *testing.T) {
	mock := newRecordingMock(256)
	checker := &fakeChecker{policy: localstore.PolicyLocalOnly}
	gated := embedding.NewGated(mock, checker, true /* remote */)

	_, err := gated.Embed(context.Background(), "private-project", []string{"private text"})
	if !errors.Is(err, embedding.ErrEmbeddingGated) {
		t.Errorf("expected ErrEmbeddingGated, got %v", err)
	}
	if mock.callCount() != 0 {
		t.Errorf("inner must NOT be called for local-only+remote, got %d calls", mock.callCount())
	}
}

// TestGate_LocalOnlyLocal_ReturnsGatedError asserts local-only + local provider
// is also denied in PR-1 (no consent mechanism yet).
func TestGate_LocalOnlyLocal_ReturnsGatedError(t *testing.T) {
	mock := newRecordingMock(256)
	checker := &fakeChecker{policy: localstore.PolicyLocalOnly}
	gated := embedding.NewGated(mock, checker, false /* local */)

	_, err := gated.Embed(context.Background(), "private-local", []string{"local text"})
	if !errors.Is(err, embedding.ErrEmbeddingGated) {
		t.Errorf("expected ErrEmbeddingGated for local-only+local (PR-1), got %v", err)
	}
	if mock.callCount() != 0 {
		t.Errorf("inner must NOT be called for local-only+local (no consent in PR-1), got %d calls", mock.callCount())
	}
}

// TestGate_Dimensions_ModelName_Delegate asserts unconditional delegation.
func TestGate_Dimensions_ModelName_Delegate(t *testing.T) {
	mock := newRecordingMock(256)
	checker := &fakeChecker{policy: localstore.PolicyOmitted}
	gated := embedding.NewGated(mock, checker, true)

	if gated.Dimensions() != 256 {
		t.Errorf("Dimensions: want 256, got %d", gated.Dimensions())
	}
	if gated.ModelName() != "mock" {
		t.Errorf("ModelName: want mock, got %q", gated.ModelName())
	}
}

// ── Task 1.3: OpenAI contract tests ─────────────────────────────────────────

// fakeOpenAIResponseN returns a minimal valid OpenAI embeddings response with
// n vectors of the given dimensionality, all filled with value 0.1.
func fakeOpenAIResponseN(n, dims int) string {
	var sb strings.Builder
	sb.WriteString(`{"data":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"embedding":[`)
		for d := 0; d < dims; d++ {
			if d > 0 {
				sb.WriteString(",")
			}
			sb.WriteString("0.1")
		}
		sb.WriteString(`]}`)
	}
	sb.WriteString(`]}`)
	return sb.String()
}

// TestOpenAI_RequestShape_DefaultConfig asserts the request body when using the
// default configuration (no explicit model or dims). The "dimensions" field MUST
// be absent from the JSON body — omitting it lets the API return the model's
// full-dimensionality output, and some OpenAI-compatible providers reject the
// field entirely if they don't support matryoshka shortening.
func TestOpenAI_RequestShape_DefaultConfig(t *testing.T) {
	const apiKey = "sk-test-request-shape"

	var capturedBody []byte
	var capturedAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fakeOpenAIResponseN(1, 256)))
	}))
	defer srv.Close()

	p := embedding.NewRemoteOpenAI(apiKey, embedding.WithBaseURL(srv.URL))
	vecs, err := p.Embed(context.Background(), "proj", []string{"some text"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != 256 {
		t.Fatalf("expected 1×256 vector, got %d vectors", len(vecs))
	}

	// Assert Authorization: Bearer header.
	if capturedAuth != "Bearer "+apiKey {
		t.Errorf("Authorization header = %q, want %q", capturedAuth, "Bearer "+apiKey)
	}

	// Decode and verify request body fields.
	var reqBody struct {
		Model          string          `json:"model"`
		Input          []string        `json:"input"`
		Dimensions     json.RawMessage `json:"dimensions"` // nil/absent when not configured
		EncodingFormat string          `json:"encoding_format"`
	}
	if err := json.Unmarshal(capturedBody, &reqBody); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if reqBody.Model != "text-embedding-3-small" {
		t.Errorf("model = %q, want text-embedding-3-small", reqBody.Model)
	}
	// Default-model contract: dimensions=256 is ALWAYS sent for
	// text-embedding-3-small (omitting it would silently flip the API to
	// 1536-dim output against a 256-dim store).
	if string(reqBody.Dimensions) != "256" {
		t.Errorf("dimensions field must be 256 for the default config, got: %s", reqBody.Dimensions)
	}
	if reqBody.EncodingFormat != "float" {
		t.Errorf("encoding_format = %q, want float", reqBody.EncodingFormat)
	}
	if len(reqBody.Input) != 1 || reqBody.Input[0] != "some text" {
		t.Errorf("input = %v, want [some text]", reqBody.Input)
	}
}

// TestOpenAI_RequestShape_WithDims asserts that when dims are explicitly configured,
// the "dimensions" field IS present in the request body with the correct value.
func TestOpenAI_RequestShape_WithDims(t *testing.T) {
	const apiKey = "sk-test-dims"
	const explicitDims = 512

	var capturedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fakeOpenAIResponseN(1, explicitDims)))
	}))
	defer srv.Close()

	p := embedding.NewRemoteOpenAI(apiKey,
		embedding.WithBaseURL(srv.URL),
		embedding.WithDims(explicitDims),
	)
	if _, err := p.Embed(context.Background(), "proj", []string{"text"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}

	var reqBody struct {
		Dimensions *int `json:"dimensions"`
	}
	if err := json.Unmarshal(capturedBody, &reqBody); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if reqBody.Dimensions == nil {
		t.Fatal("dimensions field must be present when dims explicitly configured")
	}
	if *reqBody.Dimensions != explicitDims {
		t.Errorf("dimensions = %d, want %d", *reqBody.Dimensions, explicitDims)
	}
}

// TestOpenAI_RequestShape (retained for backward compat label) is now covered by
// TestOpenAI_RequestShape_DefaultConfig. This alias keeps the original test name
// discoverable in test output.
func TestOpenAI_RequestShape(t *testing.T) {
	TestOpenAI_RequestShape_DefaultConfig(t)
}

// TestOpenAI_Non2xx_Error_KeyNotLeaked asserts non-2xx returns an error that
// does not contain the API key string.
func TestOpenAI_Non2xx_Error_KeyNotLeaked(t *testing.T) {
	const apiKey = "sk-test-secret-key"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := embedding.NewRemoteOpenAI(apiKey, embedding.WithBaseURL(srv.URL))
	_, err := p.Embed(context.Background(), "proj", []string{"text"})
	if err == nil {
		t.Fatal("expected error from non-2xx response, got nil")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Errorf("error message leaked API key: %v", err)
	}
}

// TestOpenAI_Timeout_Returns_DeadlineExceeded asserts that a slow server
// causes a timeout error within the configured timeout window.
func TestOpenAI_Timeout_Returns_DeadlineExceeded(t *testing.T) {
	// Use a channel to keep the handler alive long enough but allow the test
	// server to close cleanly after the client disconnects.
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-released:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(released)
		srv.Close()
	}()

	const timeout = 50 * time.Millisecond
	p := embedding.NewRemoteOpenAI("sk-key", embedding.WithBaseURL(srv.URL), embedding.WithTimeout(timeout))

	start := time.Now()
	_, err := p.Embed(context.Background(), "proj", []string{"text"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	// Allow 5× timeout for slow CI machines.
	if elapsed > timeout*5 {
		t.Errorf("timeout took too long: %v (timeout=%v)", elapsed, timeout)
	}
}

// TestOpenAI_Dimensions_DefaultIs256 asserts Dimensions() returns 256 when no
// explicit dims are configured (the default model's native shortening target).
func TestOpenAI_Dimensions_DefaultIs256(t *testing.T) {
	p := embedding.NewRemoteOpenAI("key")
	if p.Dimensions() != 256 {
		t.Errorf("Dimensions() = %d, want 256", p.Dimensions())
	}
}

// TestOpenAI_Dimensions_256 is the original test name; delegates to the renamed test.
func TestOpenAI_Dimensions_256(t *testing.T) {
	TestOpenAI_Dimensions_DefaultIs256(t)
}

// TestOpenAI_Dimensions_WithExplicitDims asserts Dimensions() returns the configured value.
func TestOpenAI_Dimensions_WithExplicitDims(t *testing.T) {
	p := embedding.NewRemoteOpenAI("key", embedding.WithDims(1024))
	if p.Dimensions() != 1024 {
		t.Errorf("Dimensions() = %d, want 1024", p.Dimensions())
	}
}

// ── Task 1.4: NoopProvider ───────────────────────────────────────────────────

// TestNoop_Embed_NilNil asserts Embed returns (nil, nil).
func TestNoop_Embed_NilNil(t *testing.T) {
	n := embedding.NoopProvider{}
	vecs, err := n.Embed(context.Background(), "proj", []string{"text"})
	if err != nil {
		t.Errorf("NoopProvider.Embed: unexpected error: %v", err)
	}
	if vecs != nil {
		t.Errorf("NoopProvider.Embed: expected nil vecs, got %v", vecs)
	}
}

// TestNoop_Dimensions_0 asserts Dimensions returns 0.
func TestNoop_Dimensions_0(t *testing.T) {
	n := embedding.NoopProvider{}
	if n.Dimensions() != 0 {
		t.Errorf("NoopProvider.Dimensions() = %d, want 0", n.Dimensions())
	}
}
