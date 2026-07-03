package remote_test

// Tests for Client.State — mirrors the Unshare/ListProjects test patterns.
// Route: POST /v1/state.

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

func TestClient_State_SignsAndDecodes(t *testing.T) {
	const writerID = "writer-state"
	key := testKey()

	type capture struct {
		path     string
		writerID string
		sig      string
		body     []byte
	}
	capCh := make(chan capture, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capCh <- capture{
			path:     r.URL.Path,
			writerID: r.Header.Get(wireauth.HeaderWriterID),
			sig:      r.Header.Get(wireauth.HeaderSignature),
			body:     body,
		}
		writeJSON(w, http.StatusOK, syncwire.StateResponse{PurgeEpoch: 4})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil, writerID, key)
	state, err := c.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state.PurgeEpoch != 4 {
		t.Errorf("PurgeEpoch = %d; want 4", state.PurgeEpoch)
	}

	var cap capture
	select {
	case cap = <-capCh:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never invoked")
	}
	if cap.path != "/v1/state" {
		t.Errorf("path = %q; want /v1/state", cap.path)
	}
	if cap.writerID != writerID {
		t.Errorf("X-Writer-Id = %q; want %q", cap.writerID, writerID)
	}
	if !wireauth.Verify(key, http.MethodPost, "/v1/state", cap.body, cap.sig) {
		t.Errorf("wireauth.Verify failed for State (client may be signing the full URL)")
	}
}

func TestClient_State_500_ReturnsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "boom"})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil, "w", testKey())
	_, err := c.State(context.Background())
	if err == nil {
		t.Fatal("expected error on 500")
	}
	var se *remote.StatusError
	if !errors.As(err, &se) || se.Code != http.StatusInternalServerError {
		t.Fatalf("want *remote.StatusError 500, got %v", err)
	}
}

// TestClient_State_501_ReturnsStatusError proves a 501 (older central, or a
// Central lacking the writerPurgeEpoch capability) yields a *remote.StatusError
// whose StatusCode() exposes 501 for the daemon's isRemoteStateUnsupported
// classification (mirrors the ListProjects 501 contract).
func TestClient_State_501_ReturnsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented"})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil, "w", testKey())
	_, err := c.State(context.Background())
	if err == nil {
		t.Fatal("expected error on 501")
	}
	var se *remote.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error is %T, want *remote.StatusError", err)
	}
	if se.StatusCode() != http.StatusNotImplemented {
		t.Errorf("StatusError.StatusCode() = %d; want 501", se.StatusCode())
	}
}

func TestClient_State_ContextCancellation(t *testing.T) {
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-unblock:
		case <-r.Context().Done():
		}
		writeJSON(w, http.StatusOK, syncwire.StateResponse{})
	}))
	defer srv.Close()
	defer close(unblock)

	c := remote.New(srv.URL, nil, "writer-test", testKey())
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		_, err := c.State(ctx)
		errCh <- err
	}()

	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("State: expected error after context cancel, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("State: did not return promptly after context cancel")
	}
}
