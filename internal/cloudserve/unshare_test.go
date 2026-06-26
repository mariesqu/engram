package cloudserve_test

// Tests for the POST /v1/unshare endpoint (feature #3): the authenticated-wire
// equivalent of the --remote=unshare admin op. Uses AllowAllVerifier so the auth
// signature is not required here (auth itself is covered by the push/pull tests).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mariesqu/engram/internal/syncwire"
)

func TestHandleUnshare_Success(t *testing.T) {
	central := &mockCentral{deleteResult: 7}
	ts := newTestServer(t, central)

	body, _ := json.Marshal(syncwire.UnshareRequest{Project: "Gentleman.Dots"})
	resp, err := http.Post(ts.URL+"/v1/unshare", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post unshare: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var ur syncwire.UnshareResponse
	if err := json.NewDecoder(resp.Body).Decode(&ur); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if ur.Deleted != 7 {
		t.Errorf("deleted = %d; want 7", ur.Deleted)
	}
	if central.gotDeleteProject != "Gentleman.Dots" {
		t.Errorf("DeleteProject called with %q; want %q", central.gotDeleteProject, "Gentleman.Dots")
	}
}

func TestHandleUnshare_EmptyProject_400(t *testing.T) {
	central := &mockCentral{}
	ts := newTestServer(t, central)

	body, _ := json.Marshal(syncwire.UnshareRequest{Project: "   "})
	resp, err := http.Post(ts.URL+"/v1/unshare", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post unshare: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 for empty project", resp.StatusCode)
	}
	if central.gotDeleteProject != "" {
		t.Errorf("DeleteProject must not be called for empty project; got %q", central.gotDeleteProject)
	}
}

func TestHandleUnshare_MethodNotAllowed(t *testing.T) {
	central := &mockCentral{}
	ts := newTestServer(t, central)

	resp, err := http.Get(ts.URL + "/v1/unshare")
	if err != nil {
		t.Fatalf("get unshare: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405 for GET", resp.StatusCode)
	}
}
