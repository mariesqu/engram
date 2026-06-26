package remote_test

// Tests for Client.Unshare — mirrors the ListProjects test patterns.
// Route: POST /v1/unshare.

import (
	"context"
	"encoding/json"
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

func TestClient_Unshare_SignsAndDecodes(t *testing.T) {
	const writerID = "writer-unshare"
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
		writeJSON(w, http.StatusOK, syncwire.UnshareResponse{Deleted: 5})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil, writerID, key)
	n, err := c.Unshare(context.Background(), "Gentleman.Dots")
	if err != nil {
		t.Fatalf("Unshare: %v", err)
	}
	if n != 5 {
		t.Errorf("deleted = %d; want 5", n)
	}

	var cap capture
	select {
	case cap = <-capCh:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never invoked")
	}
	if cap.path != "/v1/unshare" {
		t.Errorf("path = %q; want /v1/unshare", cap.path)
	}
	if cap.writerID != writerID {
		t.Errorf("X-Writer-Id = %q; want %q", cap.writerID, writerID)
	}
	if !wireauth.Verify(key, http.MethodPost, "/v1/unshare", cap.body, cap.sig) {
		t.Errorf("wireauth.Verify failed for Unshare (client may be signing the full URL)")
	}
	var req syncwire.UnshareRequest
	if err := json.Unmarshal(cap.body, &req); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if req.Project != "Gentleman.Dots" {
		t.Errorf("project = %q; want Gentleman.Dots (case preserved)", req.Project)
	}
}

func TestClient_Unshare_500_ReturnsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "boom"})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil, "w", testKey())
	_, err := c.Unshare(context.Background(), "p")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	var se *remote.StatusError
	if !errors.As(err, &se) || se.Code != http.StatusInternalServerError {
		t.Fatalf("want *remote.StatusError 500, got %v", err)
	}
}
