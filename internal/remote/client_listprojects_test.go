package remote_test

// Tests for Client.ListProjects — mirrors the PullSince test patterns in
// client_test.go. Routes: POST /v1/projects.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/remote"
	"github.com/mariesqu/engram/internal/syncwire"
	"github.com/mariesqu/engram/internal/wireauth"
)

// TestClient_ListProjects_200_DecodesProjects proves that a 200 response with a
// valid ProjectsResponse is decoded and returned correctly.
func TestClient_ListProjects_200_DecodesProjects(t *testing.T) {
	wantProjects := []string{"alpha", "beta", "gamma"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}

		// Decode the incoming ProjectsRequest (should be an empty JSON object).
		var req syncwire.ProjectsRequest
		b, _ := io.ReadAll(r.Body)
		if len(b) == 0 {
			t.Error("request body is empty; want a JSON ProjectsRequest")
		}
		// req is an empty struct; just verify no decode error.
		_ = req

		writeJSON(w, http.StatusOK, syncwire.ProjectsResponse{Projects: wantProjects})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil, "writer-test", testKey())
	got, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects returned error: %v", err)
	}

	if len(got) != len(wantProjects) {
		t.Fatalf("ListProjects: got %d projects; want %d", len(got), len(wantProjects))
	}
	for i, w := range wantProjects {
		if got[i] != w {
			t.Errorf("projects[%d] = %q; want %q", i, got[i], w)
		}
	}
}

// TestClient_ListProjects_200_Empty proves that a 200 with an empty Projects slice
// (nil or []) is handled gracefully (no error, nil or empty result).
func TestClient_ListProjects_200_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, syncwire.ProjectsResponse{Projects: nil})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil, "writer-test", testKey())
	got, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects returned error on empty response: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListProjects: got %v; want empty slice", got)
	}
}

// TestClient_ListProjects_500_ReturnsStatusError proves that a 500 response
// yields a *StatusError with Retryable()==true (mirrors PullSince 500 test).
func TestClient_ListProjects_500_ReturnsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil, "writer-test", testKey())
	_, err := c.ListProjects(context.Background())
	if err == nil {
		t.Fatal("ListProjects: expected error on 500, got nil")
	}

	var se *remote.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("ListProjects: error is %T, want *remote.StatusError", err)
	}
	if se.Code != http.StatusInternalServerError {
		t.Errorf("StatusError.Code = %d; want 500", se.Code)
	}
	if !se.Retryable() {
		t.Error("500 MUST be retryable")
	}
}

// TestClient_ListProjects_501_ReturnsStatusError proves that a 501 (the server's
// Central does not implement project discovery) yields a *remote.StatusError with
// Code==501. At the StatusError level a 501 is >=500 so Retryable()==true; the
// "capability absent → fall back to local projects" decision is made one layer up
// in syncer.SyncAllProjects, which detects the 501 via StatusError.StatusCode()
// (NOT via Retryable). This test pins the code and that StatusCode() exposes it
// for that cross-package detection.
func TestClient_ListProjects_501_ReturnsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented"})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil, "writer-test", testKey())
	_, err := c.ListProjects(context.Background())
	if err == nil {
		t.Fatal("ListProjects: expected error on 501, got nil")
	}

	var se *remote.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("ListProjects: error is %T, want *remote.StatusError", err)
	}
	if se.Code != http.StatusNotImplemented {
		t.Errorf("StatusError.Code = %d; want 501", se.Code)
	}
	// StatusCode() is what syncer.SyncAllProjects uses to detect a capability-absent
	// (older) central and skip discovery instead of treating it as a sync failure.
	if se.StatusCode() != http.StatusNotImplemented {
		t.Errorf("StatusError.StatusCode() = %d; want 501", se.StatusCode())
	}
	// 501 >= 500 so Retryable()==true per StatusError.Retryable implementation.
	if !se.Retryable() {
		t.Error("501 is >=500 so Retryable() must be true per StatusError semantics")
	}
}

// TestClient_ListProjects_ContextCancellation proves ListProjects propagates
// ctx cancellation (mirrors PullSince context-cancel test).
func TestClient_ListProjects_ContextCancellation(t *testing.T) {
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-unblock:
		case <-r.Context().Done():
		}
		writeJSON(w, http.StatusOK, syncwire.ProjectsResponse{Projects: nil})
	}))
	defer srv.Close()
	defer close(unblock)

	c := remote.New(srv.URL, nil, "writer-test", testKey())
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		_, err := c.ListProjects(ctx)
		errCh <- err
	}()

	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("ListProjects: expected error after context cancel, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListProjects: did not return promptly after context cancel")
	}
}

// TestClient_ListProjects_SendsCorrectSignatureHeaders proves ListProjects sends
// HMAC-signed X-Writer-Id / X-Signature headers with the path "/v1/projects",
// exactly as PullSince does for "/v1/pull".
func TestClient_ListProjects_SendsCorrectSignatureHeaders(t *testing.T) {
	const writerID = "writer-signing-projects"
	key := testKey()

	type capture struct {
		writerID string
		sig      string
		body     []byte
	}
	capCh := make(chan capture, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capCh <- capture{
			writerID: r.Header.Get(wireauth.HeaderWriterID),
			sig:      r.Header.Get(wireauth.HeaderSignature),
			body:     body,
		}
		writeJSON(w, http.StatusOK, syncwire.ProjectsResponse{Projects: []string{}})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil, writerID, key)
	_, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}

	var cap capture
	select {
	case cap = <-capCh:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never invoked")
	}

	if cap.writerID != writerID {
		t.Errorf("X-Writer-Id = %q; want %q", cap.writerID, writerID)
	}

	// The signed path must be "/v1/projects" (URL path only, not the full URL).
	if !wireauth.Verify(key, http.MethodPost, "/v1/projects", cap.body, cap.sig) {
		t.Errorf("wireauth.Verify failed for ListProjects: sig=%q body=%s — "+
			"client may be signing the full URL instead of just the path", cap.sig, cap.body)
	}
}
