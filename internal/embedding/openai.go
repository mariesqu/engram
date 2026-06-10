package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	openAIDefaultBaseURL  = "https://api.openai.com"
	openAIModel           = "text-embedding-3-small"
	openAIDefaultDims     = 256
	openAIDefaultTimeout  = 30 * time.Second
	openAIEncodingFormat  = "float"
)

// RemoteOpenAIProvider sends embedding requests to OpenAI's API.
// It uses stdlib net/http + encoding/json — zero new Go module dependencies.
//
// The API key is stored in an unexported field with no json tag so it cannot
// be serialised accidentally. It MUST NOT appear in any error message, log
// line, or HTTP response — see keyGuard below.
type RemoteOpenAIProvider struct {
	apiKey     string // unexported, no json tag — NEVER serialised
	baseURL    string
	timeout    time.Duration
	httpClient *http.Client
}

// Option configures a RemoteOpenAIProvider.
type Option func(*RemoteOpenAIProvider)

// WithBaseURL overrides the default OpenAI base URL. Used by tests to point
// the provider at an httptest.Server without making real network calls.
func WithBaseURL(url string) Option {
	return func(p *RemoteOpenAIProvider) {
		p.baseURL = url
	}
}

// WithTimeout overrides the default 30s per-request timeout.
func WithTimeout(d time.Duration) Option {
	return func(p *RemoteOpenAIProvider) {
		p.timeout = d
	}
}

// NewRemoteOpenAI constructs a RemoteOpenAIProvider.
// key is the OpenAI API key. It must not be empty when real embeddings are
// needed, but the provider is constructible with an empty key (it will fail
// at Embed time with a 401 from the API).
func NewRemoteOpenAI(key string, opts ...Option) *RemoteOpenAIProvider {
	p := &RemoteOpenAIProvider{
		apiKey:  key,
		baseURL: openAIDefaultBaseURL,
		timeout: openAIDefaultTimeout,
	}
	for _, o := range opts {
		o(p)
	}
	p.httpClient = &http.Client{Timeout: p.timeout}
	return p
}

// Dimensions returns 256 (matryoshka shortening fixed for text-embedding-3-small).
func (p *RemoteOpenAIProvider) Dimensions() int { return openAIDefaultDims }

// ModelName returns "text-embedding-3-small".
func (p *RemoteOpenAIProvider) ModelName() string { return openAIModel }

// openAIRequest is the JSON body sent to POST /v1/embeddings.
type openAIRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	Dimensions     int      `json:"dimensions"`
	EncodingFormat string   `json:"encoding_format"`
}

// openAIResponse is the JSON body returned by POST /v1/embeddings.
type openAIResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed sends texts to the OpenAI embeddings API and returns one []float32
// per input in the same order.
//
// Security contract:
//   - The API key is sent only in the Authorization header (never in the URL).
//   - On non-2xx status the response body is discarded (no leakage via body).
//   - Any error returned by Embed MUST NOT contain the API key string.
//
// Retry policy: exactly one attempt. Transient errors (network, 5xx, timeout)
// are returned to the caller; the backfill loop provides the retry strategy.
func (p *RemoteOpenAIProvider) Embed(ctx context.Context, _ string, texts []string) ([][]float32, error) {
	body, err := json.Marshal(openAIRequest{
		Model:          openAIModel,
		Input:          texts,
		Dimensions:     openAIDefaultDims,
		EncodingFormat: openAIEncodingFormat,
	})
	if err != nil {
		return nil, fmt.Errorf("embedding: marshal request: %w", err)
	}

	url := p.baseURL + "/v1/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedding: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Key in header only — NEVER in URL query param.
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.httpClient.Do(req)
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
	for i, d := range result.Data {
		out[i] = d.Embedding
	}
	return out, nil
}

// ensure RemoteOpenAIProvider satisfies EmbeddingProvider at compile time.
var _ EmbeddingProvider = (*RemoteOpenAIProvider)(nil)
