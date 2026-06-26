package webui_test

// Test for the "exists in central" marker on the projects page (feature #2).
// The marker comes from WebUIDeps.RemoteProjects; when it's nil/returns nil the
// page shows no markers (state unknown).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/webui"
)

func TestWebUI_ProjectsPage_RemoteMarker(t *testing.T) {
	const secret = "proj-remote"
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl: &mockSyncCtrl{},
		Store: &mockStore{projects: []controlapi.ProjectPolicy{
			{Name: "alpha", Policy: controlapi.PolicySynced},
			{Name: "beta", Policy: controlapi.PolicyLocalOnly},
		}},
		ConfigStore: &mockConfigStore{},
		// "Alpha" (mixed case) from central must still match local "alpha".
		RemoteProjects: func(_ context.Context) ([]string, error) {
			return []string{"Alpha"}, nil
		},
		Secret:  secret,
		Port:    7700,
		Version: "test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, _ := authenticatedClient(t, srv, secret)
	resp, err := client.Get(srv.URL + "/ui/projects")
	if err != nil {
		t.Fatalf("get projects: %v", err)
	}
	body := readRespBody(t, resp)

	if !strings.Contains(body, "badge-remote") {
		t.Error("expected a 'central' marker for the remote project")
	}
	// Only alpha exists in central → exactly one marker.
	if got := strings.Count(body, "badge-remote"); got != 1 {
		t.Errorf("central marker count = %d; want 1 (only alpha is remote)", got)
	}
}

// TestWebUI_ProjectsPage_NoRemoteWhenDisconnected verifies that a nil
// RemoteProjects (daemon not connected to central) renders no markers — the
// state is unknown, not "local only".
func TestWebUI_ProjectsPage_NoRemoteWhenDisconnected(t *testing.T) {
	const secret = "proj-noremote"
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl: &mockSyncCtrl{},
		Store: &mockStore{projects: []controlapi.ProjectPolicy{
			{Name: "alpha", Policy: controlapi.PolicySynced},
		}},
		ConfigStore:    &mockConfigStore{},
		RemoteProjects: nil, // disconnected / not wired
		Secret:         secret,
		Port:           7700,
		Version:        "test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, _ := authenticatedClient(t, srv, secret)
	resp, err := client.Get(srv.URL + "/ui/projects")
	if err != nil {
		t.Fatalf("get projects: %v", err)
	}
	body := readRespBody(t, resp)
	if strings.Contains(body, "badge-remote") {
		t.Error("no remote markers should appear when RemoteProjects is nil")
	}
}
