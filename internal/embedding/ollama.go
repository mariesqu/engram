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
	ollamaDefaultHost    = "http://localhost:11434"
	ollamaDefaultTimeout = 30 * time.Second
	// NOTE: /api/embeddings is ollama's LEGACY single-prompt endpoint (request
	// {model, prompt} → {embedding}). The newer /api/embed takes batched input
	// and returns {embeddings}. Both work today; migrate to /api/embed before
	// ollama removes the legacy route (TODO, tracked in the change's deferred
	// inventory).
	ollamaEmbedPath = "/api/embeddings"
)

// OllamaSidecarProvider sends embedding requests to a local Ollama sidecar.
// It uses stdlib net/http + encoding/json — zero new Go module dependencies.
//
// IsRemote = false: Ollama runs on the same node. The privacy gate treats it as
// a LOCAL provider — local-only projects are embeddable when consent=true.
//
// Timeout and host are configurable via constructor options.
type OllamaSidecarProvider struct {
	host       string
	model      string
	dims       int
	timeout    time.Duration
	httpClient *http.Client
}

// OllamaOption configures an OllamaSidecarProvider.
type OllamaOption func(*OllamaSidecarProvider)

// WithOllamaHost overrides the default Ollama host URL.
// Used by tests to point the provider at an httptest.Server.
func WithOllamaHost(host string) OllamaOption {
	return func(p *OllamaSidecarProvider) {
		p.host = host
	}
}

// WithOllamaTimeout overrides the default 30s per-request timeout.
func WithOllamaTimeout(d time.Duration) OllamaOption {
	return func(p *OllamaSidecarProvider) {
		p.timeout = d
	}
}

// NewOllamaSidecar constructs an OllamaSidecarProvider.
//
// model is the Ollama model name (e.g. "nomic-embed-text").
// dims is the expected embedding dimensionality (matches embedding_dims config).
// Default dims is 256 when <= 0 (matches the OpenAI provider default).
func NewOllamaSidecar(model string, dims int, opts ...OllamaOption) *OllamaSidecarProvider {
	if dims <= 0 {
		dims = 256
	}
	p := &OllamaSidecarProvider{
		host:    ollamaDefaultHost,
		model:   model,
		dims:    dims,
		timeout: ollamaDefaultTimeout,
	}
	for _, o := range opts {
		o(p)
	}
	p.httpClient = &http.Client{Timeout: p.timeout}
	return p
}

// Dimensions returns the configured embedding dimensionality.
func (p *OllamaSidecarProvider) Dimensions() int { return p.dims }

// ModelName returns the configured Ollama model name.
func (p *OllamaSidecarProvider) ModelName() string { return p.model }

// ollamaRequest is the JSON body sent to POST /api/embeddings.
// Ollama processes one text at a time — no batch parameter.
type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

// ollamaResponse is the JSON body returned by POST /api/embeddings.
type ollamaResponse struct {
	Embedding []float32 `json:"embedding"`
}

// Embed embeds each text by issuing one POST /api/embeddings per text.
// Ollama's /api/embeddings endpoint processes a single prompt per request.
//
// On connection refused (sidecar absent) the error is returned to the caller.
// The backfill loop's existing skip/backoff path handles provider errors.
//
// The project parameter is used by the gatedProvider wrapping this provider;
// OllamaSidecarProvider itself does not inspect it.
func (p *OllamaSidecarProvider) Embed(ctx context.Context, _ string, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		body, err := json.Marshal(ollamaRequest{Model: p.model, Prompt: text})
		if err != nil {
			return nil, fmt.Errorf("embedding/ollama: marshal request: %w", err)
		}

		url := p.host + ollamaEmbedPath
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("embedding/ollama: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := p.httpClient.Do(req)
		if err != nil {
			// Connection refused or timeout — sidecar absent or slow.
			// Return as a provider error so the loop backs off.
			return nil, fmt.Errorf("embedding/ollama: request failed: %w", err)
		}
		// Close the body BEFORE the next iteration — a deferred close inside a
		// loop holds every connection open until the function returns (the
		// interface contract allows large batches even though current callers
		// pass single texts).
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("embedding/ollama: API returned status %d", resp.StatusCode)
		}

		var result ollamaResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&result)
		_ = resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("embedding/ollama: decode response: %w", decodeErr)
		}
		out[i] = result.Embedding
	}
	return out, nil
}

// ensure OllamaSidecarProvider satisfies EmbeddingProvider at compile time.
var _ EmbeddingProvider = (*OllamaSidecarProvider)(nil)
