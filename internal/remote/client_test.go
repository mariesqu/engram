package remote_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/mutation"
	"github.com/mariesqu/engram/internal/remote"
	"github.com/mariesqu/engram/internal/syncwire"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// testMutation builds a minimal domain.Mutation with a valid canonical payload
// and mutation_id so it passes VerifyMutationID on the server side.
func testMutation(t *testing.T, topic, content string) domain.Mutation {
	t.Helper()
	tk := topic
	m := domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     "sync-test",
		SessionID:  "sess-test",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "test title",
		Content:    content,
		Project:    "engram",
		Scope:      "project",
		TopicKey:   &tk,
		Version:    1,
		UpdatedAt:  time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
		WriterID:   "writer-test",
		OccurredAt: time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
	}
	// Derive canonical payload and mutation_id (mirrors what normalizeMutation does).
	m.Payload = mutation.CanonicalPayload(m)
	m.MutationID = mutation.NewMutationID(m.Payload)
	return m
}

// writeJSON writes a JSON-encoded body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// ── Apply tests ──────────────────────────────────────────────────────────────

func TestClient_Apply_200_ReturnsNil(t *testing.T) {
	gotReqCh := make(chan syncwire.PushRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/push" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		b, _ := io.ReadAll(r.Body)
		var req syncwire.PushRequest
		if err := json.Unmarshal(b, &req); err != nil {
			t.Errorf("decode PushRequest: %v", err)
		}
		writeJSON(w, http.StatusOK, syncwire.PushResponse{
			Status:     "ok",
			MutationID: req.Mutation.MutationID,
			Applied:    true,
		})
		gotReqCh <- req
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil)
	m := testMutation(t, "sdd/test/apply-200", "hello")

	if err := c.Apply(context.Background(), m); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	// Assert the request body was a valid PushRequest with correct mutation_id.
	gotReq := <-gotReqCh
	if gotReq.Mutation.MutationID != m.MutationID {
		t.Errorf("PushRequest mutation_id = %q; want %q", gotReq.Mutation.MutationID, m.MutationID)
	}
	// The payload round-trips (can be decoded back to a mutation).
	decoded, err := syncwire.FromWire(gotReq.Mutation)
	if err != nil {
		t.Fatalf("FromWire on pushed mutation: %v", err)
	}
	if decoded.Content != m.Content {
		t.Errorf("round-trip content = %q; want %q", decoded.Content, m.Content)
	}
}

func TestClient_Apply_400_ReturnsStatusError_NonRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil)
	m := testMutation(t, "sdd/test/apply-400", "content")

	err := c.Apply(context.Background(), m)
	if err == nil {
		t.Fatal("Apply: expected error on 400, got nil")
	}

	var se *remote.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("Apply: error is %T, want *remote.StatusError", err)
	}
	if se.Code != http.StatusBadRequest {
		t.Errorf("StatusError.Code = %d; want 400", se.Code)
	}
	if se.Retryable() {
		t.Error("400 must NOT be retryable")
	}
}

func TestClient_Apply_500_ReturnsStatusError_Retryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil)
	m := testMutation(t, "sdd/test/apply-500", "content")

	err := c.Apply(context.Background(), m)
	if err == nil {
		t.Fatal("Apply: expected error on 500, got nil")
	}

	var se *remote.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("Apply: error is %T, want *remote.StatusError", err)
	}
	if se.Code != http.StatusInternalServerError {
		t.Errorf("StatusError.Code = %d; want 500", se.Code)
	}
	if !se.Retryable() {
		t.Error("500 MUST be retryable")
	}
}

func TestClient_Apply_ContextCancellation(t *testing.T) {
	// Server that blocks until the client cancels.
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-unblock:
		case <-r.Context().Done():
		}
		writeJSON(w, http.StatusOK, syncwire.PushResponse{Status: "ok", Applied: true})
	}))
	defer srv.Close()
	defer close(unblock)

	c := remote.New(srv.URL, nil)
	m := testMutation(t, "sdd/test/apply-cancel", "content")

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- c.Apply(ctx, m) }()

	cancel() // cancel immediately

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Apply: expected error after context cancel, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Apply: did not return promptly after context cancel")
	}
}

// ── PullSince tests ──────────────────────────────────────────────────────────

func TestClient_PullSince_200_DecodesMutations(t *testing.T) {
	m1 := testMutation(t, "sdd/test/pull-a", "content A")
	m1.Seq = 10
	m2 := testMutation(t, "sdd/test/pull-b", "content B")
	m2.Seq = 11

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pull" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var req syncwire.PullRequest
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil {
			t.Errorf("decode PullRequest: %v", err)
		}
		writeJSON(w, http.StatusOK, syncwire.PullResponse{
			Mutations: []syncwire.WireMutation{
				syncwire.ToWire(m1),
				syncwire.ToWire(m2),
			},
		})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil)
	got, err := c.PullSince(context.Background(), "engram", 0, 100)
	if err != nil {
		t.Fatalf("PullSince returned error: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("PullSince: got %d mutations; want 2", len(got))
	}
	if got[0].Content != m1.Content {
		t.Errorf("got[0].Content = %q; want %q", got[0].Content, m1.Content)
	}
	if got[1].Content != m2.Content {
		t.Errorf("got[1].Content = %q; want %q", got[1].Content, m2.Content)
	}
	// Seq propagated through the wire.
	if got[0].Seq != m1.Seq {
		t.Errorf("got[0].Seq = %d; want %d", got[0].Seq, m1.Seq)
	}
	if got[1].Seq != m2.Seq {
		t.Errorf("got[1].Seq = %d; want %d", got[1].Seq, m2.Seq)
	}
	// MutationID round-trips correctly (critical for the JSONB round-trip proof).
	if got[0].MutationID != m1.MutationID {
		t.Errorf("got[0].MutationID = %q; want %q", got[0].MutationID, m1.MutationID)
	}
	if got[1].MutationID != m2.MutationID {
		t.Errorf("got[1].MutationID = %q; want %q", got[1].MutationID, m2.MutationID)
	}
}

func TestClient_PullSince_500_ReturnsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil)
	_, err := c.PullSince(context.Background(), "engram", 0, 100)
	if err == nil {
		t.Fatal("PullSince: expected error on 500, got nil")
	}

	var se *remote.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("PullSince: error is %T, want *remote.StatusError", err)
	}
	if se.Code != http.StatusInternalServerError {
		t.Errorf("StatusError.Code = %d; want 500", se.Code)
	}
	if !se.Retryable() {
		t.Error("500 MUST be retryable")
	}
}

func TestClient_PullSince_ContextCancellation(t *testing.T) {
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-unblock:
		case <-r.Context().Done():
		}
		writeJSON(w, http.StatusOK, syncwire.PullResponse{Mutations: nil})
	}))
	defer srv.Close()
	defer close(unblock)

	c := remote.New(srv.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		_, err := c.PullSince(ctx, "engram", 0, 100)
		errCh <- err
	}()

	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("PullSince: expected error after context cancel, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PullSince: did not return promptly after context cancel")
	}
}

// TestStatusError_ErrorMessage ensures the error message is human-readable and
// includes both the status code and the server's body text.
func TestStatusError_ErrorMessage(t *testing.T) {
	se := &remote.StatusError{Code: 503, Body: "service unavailable"}
	got := se.Error()
	if !strings.Contains(got, "503") {
		t.Errorf("StatusError.Error() = %q; want it to contain the status code 503", got)
	}
	if !strings.Contains(got, "service unavailable") {
		t.Errorf("StatusError.Error() = %q; want it to contain the body text", got)
	}
}

// TestNew_NilHttpClient verifies that passing nil results in a working client
// (the default timeout client is used).
func TestNew_NilHttpClient(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		writeJSON(w, http.StatusOK, syncwire.PullResponse{Mutations: nil})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil)
	_, err := c.PullSince(context.Background(), "engram", 0, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called.Load() {
		t.Error("server was never called with nil httpClient")
	}
}

// TestNew_TrimsTrailingSlash proves New trims trailing slashes so route paths are
// appended cleanly (no "//v1/push", which could trigger a POST→GET redirect).
func TestNew_TrimsTrailingSlash(t *testing.T) {
	gotPathCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPathCh <- r.URL.Path
		writeJSON(w, http.StatusOK, syncwire.PushResponse{Status: "ok", Applied: true})
	}))
	defer srv.Close()

	// Pass a baseURL WITH a trailing slash; New must trim it.
	c := remote.New(srv.URL+"/", nil)
	m := testMutation(t, "sdd/test/trailing-slash", "hello")
	if err := c.Apply(context.Background(), m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if path := <-gotPathCh; path != "/v1/push" {
		t.Errorf("request path = %q; want %q (trailing slash not trimmed → double slash)", path, "/v1/push")
	}
}

// TestClient_Apply_TruncatedBody_ReturnsError proves Apply returns the body-read
// error instead of silently treating a truncated response as success: the server
// promises Content-Length 1000 but writes a few bytes then closes the connection,
// so the client's io.ReadAll fails with an unexpected EOF.
func TestClient_Apply_TruncatedBody_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("ResponseWriter is not a Hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		// Promise 1000 bytes, send 5, then close → truncated body.
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\nshort"))
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil)
	m := testMutation(t, "sdd/test/truncated", "hello")
	if err := c.Apply(context.Background(), m); err == nil {
		t.Error("Apply returned nil on a truncated response body; want a read error")
	}
}

// TestClient_Apply_OversizedBody_ReturnsError proves the client rejects a response
// body larger than maxResponseBytes with an explicit error (fail-fast) instead of
// silently truncating it. The server returns 200 with a body one byte over the cap.
func TestClient_Apply_OversizedBody_ReturnsError(t *testing.T) {
	const oversized = (4 << 20) + 1 // maxResponseBytes (4 MiB) + 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, oversized))
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil)
	m := testMutation(t, "sdd/test/oversized", "hello")
	if err := c.Apply(context.Background(), m); err == nil {
		t.Error("Apply returned nil on an oversized response body; want an explicit overflow error")
	}
}
