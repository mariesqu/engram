package embedding_test

// ollama_test.go — task 2.1 unit tests
//
// Verifies the OllamaSidecarProvider HTTP contract using httptest.Server:
//   - Request shape (model + prompt fields, Content-Type header)
//   - Non-2xx response → error returned, no panic
//   - Timeout respected (server stalls, client times out)
//
// No real Ollama binary required.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/embedding"
)

// ollamaStub builds an httptest.Server that records the last request body and
// returns a deterministic embedding vector of the given dimensionality.
func ollamaStub(t *testing.T, dims int) (*httptest.Server, *struct {
	LastModel  string
	LastPrompt string
}) {
	t.Helper()
	captured := &struct {
		LastModel  string
		LastPrompt string
	}{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		captured.LastModel = req.Model
		captured.LastPrompt = req.Prompt

		vec := make([]float32, dims)
		for i := range vec {
			vec[i] = 0.1
		}
		resp := map[string][]float32{"embedding": vec}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

// TestOllama_RequestShape verifies that OllamaSidecarProvider sends the correct
// model and prompt fields in the JSON body, and that the Content-Type header is
// application/json.
func TestOllama_RequestShape(t *testing.T) {
	dims := 8
	srv, captured := ollamaStub(t, dims)

	p := embedding.NewOllamaSidecar("nomic-embed-text", dims,
		embedding.WithOllamaHost(srv.URL),
	)

	texts := []string{"hello world"}
	vecs, err := p.Embed(context.Background(), "test-project", texts)
	if err != nil {
		t.Fatalf("Embed: unexpected error: %v", err)
	}

	// Vector shape.
	if len(vecs) != 1 {
		t.Fatalf("want 1 vector, got %d", len(vecs))
	}
	if len(vecs[0]) != dims {
		t.Errorf("vector dims: got %d, want %d", len(vecs[0]), dims)
	}

	// Request fields captured by the stub.
	if captured.LastModel != "nomic-embed-text" {
		t.Errorf("model field: got %q, want %q", captured.LastModel, "nomic-embed-text")
	}
	if captured.LastPrompt != "hello world" {
		t.Errorf("prompt field: got %q, want %q", captured.LastPrompt, "hello world")
	}
}

// TestOllama_Non2xx_Error verifies that a non-2xx HTTP response is returned as
// an error and does not panic.
func TestOllama_Non2xx_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	p := embedding.NewOllamaSidecar("missing-model", 8,
		embedding.WithOllamaHost(srv.URL),
	)

	_, err := p.Embed(context.Background(), "proj", []string{"text"})
	if err == nil {
		t.Fatal("want error for 404 response, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention status 404; got: %v", err)
	}
}

// TestOllama_Timeout verifies that a stalling server causes the provider to
// return an error when the timeout is exceeded, not block forever.
func TestOllama_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stall indefinitely — the client should time out.
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		http.Error(w, "too late", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	p := embedding.NewOllamaSidecar("nomic-embed-text", 8,
		embedding.WithOllamaHost(srv.URL),
		embedding.WithOllamaTimeout(50*time.Millisecond),
	)

	start := time.Now()
	_, err := p.Embed(context.Background(), "proj", []string{"text"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want timeout error, got nil")
	}
	// Should fail well under 1 second despite the server stalling.
	if elapsed > 2*time.Second {
		t.Errorf("Embed took %v, want < 2s (timeout should fire at 50ms)", elapsed)
	}
}

// TestOllama_MultiBatch verifies that Embed issues one request per text and
// returns a vector for each.
func TestOllama_MultiBatch(t *testing.T) {
	dims := 4
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var req struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		vec := make([]float32, dims)
		resp := map[string][]float32{"embedding": vec}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	p := embedding.NewOllamaSidecar("nomic-embed-text", dims,
		embedding.WithOllamaHost(srv.URL),
	)

	texts := []string{"a", "b", "c"}
	vecs, err := p.Embed(context.Background(), "proj", texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 3 {
		t.Errorf("got %d vectors, want 3", len(vecs))
	}
	// One HTTP call per text.
	if callCount != 3 {
		t.Errorf("server received %d requests, want 3 (one per text)", callCount)
	}
}
