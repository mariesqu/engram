package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	openAIDefaultBaseURL = "https://api.openai.com"
	openAIDefaultModel   = "text-embedding-3-small"
	openAIDefaultDims    = 256
	openAIDefaultTimeout = 30 * time.Second
	openAIEncodingFormat = "float"
)

// AuthHeader selects which HTTP header carries the API key.
//
// AuthHeaderBearer (default) sends:  Authorization: Bearer <key>
// AuthHeaderAPIKey              sends:  api-key: <key>          (Azure classic header)
type AuthHeader int

const (
	// AuthHeaderBearer is the default. Sends: Authorization: Bearer <key>
	AuthHeaderBearer AuthHeader = iota
	// AuthHeaderAPIKey sends: api-key: <key>. Required by Azure OpenAI Service
	// when using the classic (non-v1) surface. Also accepted by some gateways.
	AuthHeaderAPIKey
)

// RemoteOpenAIProvider sends embedding requests to an OpenAI-compatible API.
// It uses stdlib net/http + encoding/json — zero new Go module dependencies.
//
// Custom base URLs allow pointing the provider at Azure OpenAI, Mistral, vLLM,
// LiteLLM, OpenRouter, or any OpenAI-compatible endpoint. See embeddingsURL for
// the URL-joining rule.
//
// The API key is stored in an unexported field with no json tag so it cannot
// be serialised accidentally. It MUST NOT appear in any error message, log
// line, or HTTP response — see keyGuard below.
type RemoteOpenAIProvider struct {
	apiKey     string // unexported, no json tag — NEVER serialised
	baseURL    string
	model      string
	dims       int // 0 means "use provider default (openAIDefaultDims)"
	authHeader AuthHeader
	timeout    time.Duration
	httpClient *http.Client
}

// Option configures a RemoteOpenAIProvider.
type Option func(*RemoteOpenAIProvider)

// WithBaseURL overrides the default OpenAI base URL. When set, the provider
// joins the base URL with /embeddings using embeddingsURL (see that function's
// doc for the exact joining rule). Used by tests to point the provider at an
// httptest.Server, and in production for custom OpenAI-compatible endpoints.
func WithBaseURL(u string) Option {
	return func(p *RemoteOpenAIProvider) {
		p.baseURL = u
	}
}

// WithModel overrides the default model (text-embedding-3-small). The value is
// sent verbatim in the "model" field of every embeddings request. ModelName()
// returns this configured value, which drives the backfill loop's re-embed
// predicate: changing the model automatically re-embeds all existing observations
// (they carry the old model name; the backfill loop skips only rows whose stored
// embedding_model matches the current ModelName()).
func WithModel(model string) Option {
	return func(p *RemoteOpenAIProvider) {
		p.model = model
	}
}

// WithDims overrides the effective vector dimensionality. When set, the value is
// sent in the "dimensions" field of the request (matryoshka shortening for models
// that support it). When NOT set (0), the "dimensions" field is OMITTED from the
// request — some OpenAI-compatible providers (e.g. Mistral) reject or ignore
// unknown fields, and fixed-output models have no shortening capability.
//
// For non-OpenAI models, set dims to the model's native output size (e.g. 1024
// for mistral-embed) so the store's length guard and cosine math agree with the
// actual vector length in the response.
func WithDims(dims int) Option {
	return func(p *RemoteOpenAIProvider) {
		p.dims = dims
	}
}

// WithAuthHeader selects how the API key is sent. The default (AuthHeaderBearer)
// sends Authorization: Bearer <key>. Pass AuthHeaderAPIKey to send api-key: <key>
// instead (required by Azure OpenAI Service classic surfaces).
func WithAuthHeader(h AuthHeader) Option {
	return func(p *RemoteOpenAIProvider) {
		p.authHeader = h
	}
}

// WithTimeout overrides the default 30s per-request timeout.
func WithTimeout(d time.Duration) Option {
	return func(p *RemoteOpenAIProvider) {
		p.timeout = d
	}
}

// NewRemoteOpenAI constructs a RemoteOpenAIProvider.
// key is the API key. It must not be empty when real embeddings are needed, but
// the provider is constructible with an empty key (it will fail at Embed time
// with a 401 from the API).
func NewRemoteOpenAI(key string, opts ...Option) *RemoteOpenAIProvider {
	p := &RemoteOpenAIProvider{
		apiKey:  key,
		baseURL: openAIDefaultBaseURL,
		model:   openAIDefaultModel,
		timeout: openAIDefaultTimeout,
	}
	for _, o := range opts {
		o(p)
	}
	// Default-model contract: text-embedding-3-small has ALWAYS been requested
	// at 256 dims (matryoshka) — leaving dims at 0 here would silently flip the
	// API to its 1536-dim default while the store still expects 256, breaking
	// semantic search for every default-config install.
	if p.dims == 0 && p.model == openAIDefaultModel {
		p.dims = openAIDefaultDims
	}
	p.httpClient = &http.Client{Timeout: p.timeout}
	return p
}

// Dimensions returns the effective vector dimensionality:
//   - When dims is explicitly configured (non-zero), that value is returned.
//   - When using the default model with no explicit dims, returns openAIDefaultDims (256).
//   - When a custom model is set but dims is zero, the caller must set dims explicitly;
//     the daemon startup validation rejects this combination as FATAL.
func (p *RemoteOpenAIProvider) Dimensions() int {
	if p.dims != 0 {
		return p.dims
	}
	return openAIDefaultDims
}

// ModelName returns the effective model name. This value is stored per row in
// embedding_model, and the backfill loop uses it as the re-embed predicate:
// rows whose stored embedding_model differs from ModelName() are re-embedded
// automatically. Switching embedding_model (and restarting the daemon) therefore
// automatically triggers re-embedding of all existing observations.
func (p *RemoteOpenAIProvider) ModelName() string { return p.model }

// embeddingsURL computes the full embeddings endpoint URL from the configured
// base URL.
//
// Joining rule (tested in three shapes — see TestOpenAI_URLJoining):
//
//	If the base URL has an empty path (e.g. https://api.openai.com):
//	  → append /v1/embeddings  (preserves the existing default behaviour)
//
//	Otherwise (base already carries a path segment, e.g. /v1 or /openai/v1):
//	  → strip any trailing slash from the path, then append /embeddings
//	  → example: https://api.mistral.ai/v1 → https://api.mistral.ai/v1/embeddings
//	  → example: https://<res>.openai.azure.com/openai/v1 →
//	             https://<res>.openai.azure.com/openai/v1/embeddings
//
// This means the base URL IS the version-prefixed root. Callers configure the
// full base including any version prefix — the provider always appends only
// /embeddings (or /v1/embeddings for the bare-host default).
func embeddingsURL(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		// Unreachable after config validation; legacy fallback for direct callers.
		return strings.TrimRight(base, "/") + "/v1/embeddings"
	}
	// Operate on the PARSED path — trimming the raw string mis-handles bases
	// with multiple trailing slashes (".com////" would lose the /v1 segment).
	cleaned := strings.TrimRight(u.Path, "/")
	if cleaned == "" {
		u.Path = "/v1/embeddings" // bare host: the standard OpenAI layout
	} else {
		u.Path = cleaned + "/embeddings" // base already carries its version segment
	}
	u.RawQuery = "" // defensive — the validator rejects query strings anyway
	u.Fragment = ""
	return u.String()
}

// openAIRequest is the JSON body sent to POST /v1/embeddings (or equivalent).
// The Dimensions field is omitted when not set (some providers reject it).
type openAIRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	Dimensions     *int     `json:"dimensions,omitempty"` // nil → field absent in JSON
	EncodingFormat string   `json:"encoding_format"`
}

// openAIResponse is the JSON body returned by POST /v1/embeddings.
type openAIResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed sends texts to the configured embeddings endpoint and returns one
// []float32 per input in the same order.
//
// Security contract:
//   - The API key is sent only in an HTTP header (never in the URL).
//   - On non-2xx status the response body is discarded (no leakage via body).
//   - Any error returned by Embed MUST NOT contain the API key string.
//
// Retry policy: exactly one attempt. Transient errors (network, 5xx, timeout)
// are returned to the caller; the backfill loop provides the retry strategy.
func (p *RemoteOpenAIProvider) Embed(ctx context.Context, _ string, texts []string) ([][]float32, error) {
	req := openAIRequest{
		Model:          p.model,
		Input:          texts,
		EncodingFormat: openAIEncodingFormat,
	}
	// Include "dimensions" only when explicitly configured. Omitting it for
	// fixed-output models (e.g. mistral-embed with native 1024 dims) prevents
	// "unknown field" errors from strict OpenAI-compatible providers.
	// The "dimensions" request field is an OpenAI matryoshka parameter — send
	// it only for the text-embedding-3 family. Other OpenAI-compatible models
	// (mistral-embed etc.) have fixed-size output and may reject the field;
	// for them embedding_dims configures the STORE expectation only, and the
	// response-length validation below enforces agreement.
	if p.dims != 0 && strings.HasPrefix(p.model, "text-embedding-3") {
		d := p.dims
		req.Dimensions = &d
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("embedding: marshal request: %w", err)
	}

	endpoint := embeddingsURL(p.baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedding: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// Key in header only — NEVER in URL query param.
	switch p.authHeader {
	case AuthHeaderAPIKey:
		httpReq.Header.Set("api-key", p.apiKey)
	default: // AuthHeaderBearer
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		// Do NOT include p.apiKey in the error. Wrap the transport error as-is.
		return nil, fmt.Errorf("embedding: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Discard body: may contain sensitive request echo or error details.
		_, _ = io.Copy(io.Discard, resp.Body)
		// Return status only — no body snippet, no key.
		return nil, fmt.Errorf("embedding: API returned status %d", resp.StatusCode)
	}

	var result openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("embedding: decode response: %w", err)
	}

	if len(result.Data) != len(texts) {
		return nil, fmt.Errorf("embedding: expected %d vectors, got %d", len(texts), len(result.Data))
	}

	out := make([][]float32, len(result.Data))
	want := p.Dimensions()
	for i, d := range result.Data {
		// Vector-store integrity: a provider returning a different length than
		// the configured dims would write blobs the length guard silently drops
		// at query time — semantic search degrading to zero results with no
		// error anywhere. Fail LOUDLY here instead; the message tells the user
		// the fix (set embedding_dims to the model native size).
		if want > 0 && len(d.Embedding) != want {
			return nil, fmt.Errorf("embedding: provider returned %d-dim vector, configured %d — set embedding_dims to the model's native output size", len(d.Embedding), want)
		}
		out[i] = d.Embedding
	}
	return out, nil
}

// ensure RemoteOpenAIProvider satisfies EmbeddingProvider at compile time.
var _ EmbeddingProvider = (*RemoteOpenAIProvider)(nil)
