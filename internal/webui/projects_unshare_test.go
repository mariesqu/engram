package webui_test

// Tests for the web UI "unshare" delete scope (feature #3): it calls the wire
// Unshare dep (remove from central, no DSN) and then sets the local policy to
// local-only (keep the local copy, stop re-pushing).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/webui"
)

// unshareRecordingStore records SetPolicy so the test can assert the local-only
// flip after a successful unshare.
type unshareRecordingStore struct {
	mockStore
	lastPolicyProject string
	lastPolicy        controlapi.Policy
}

func (s *unshareRecordingStore) SetPolicy(project string, p controlapi.Policy) error {
	s.lastPolicyProject = project
	s.lastPolicy = p
	return nil
}

func TestWebUI_ProjectDelete_Unshare_CallsWireAndSetsLocalOnly(t *testing.T) {
	const secret = "proj-del-unshare"
	store := &unshareRecordingStore{}
	var unshared []string

	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl:    &mockSyncCtrl{},
		Store:       store,
		ConfigStore: &mockConfigStore{},
		Unshare: func(_ context.Context, project string) (int, error) {
			unshared = append(unshared, project)
			return 3, nil
		},
		Secret:  secret,
		Port:    7700,
		Version: "test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, tok := authenticatedClient(t, srv, secret)
	resp := postForm(t, client, fmt.Sprintf("%s/ui/projects/Gentleman.Dots/delete", srv.URL), url.Values{
		"csrf_token": {tok},
		"scope":      {"unshare"},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if len(unshared) != 1 || unshared[0] != "Gentleman.Dots" {
		t.Errorf("Unshare calls = %v; want [Gentleman.Dots]", unshared)
	}
	if store.lastPolicyProject != "Gentleman.Dots" || store.lastPolicy != controlapi.PolicyLocalOnly {
		t.Errorf("SetPolicy = (%q, %q); want (Gentleman.Dots, local-only)",
			store.lastPolicyProject, store.lastPolicy)
	}
}

// TestWebUI_ProjectDelete_Unshare_Unavailable verifies that when the daemon is not
// connected to central (Unshare dep nil) the unshare scope returns 409 and never
// touches the local policy.
func TestWebUI_ProjectDelete_Unshare_Unavailable(t *testing.T) {
	const secret = "proj-del-unshare-nil"
	store := &unshareRecordingStore{}

	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl:    &mockSyncCtrl{},
		Store:       store,
		ConfigStore: &mockConfigStore{},
		Unshare:     nil, // disconnected / not wired
		Secret:      secret,
		Port:        7700,
		Version:     "test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, tok := authenticatedClient(t, srv, secret)
	resp := postForm(t, client, fmt.Sprintf("%s/ui/projects/x/delete", srv.URL), url.Values{
		"csrf_token": {tok},
		"scope":      {"unshare"},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("want 409 when Unshare is unavailable, got %d", resp.StatusCode)
	}
	if store.lastPolicyProject != "" {
		t.Errorf("SetPolicy must not be called when unshare is unavailable; got %q", store.lastPolicyProject)
	}
}
