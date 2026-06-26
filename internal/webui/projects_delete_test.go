package webui_test

// HTTP-level tests for the project delete route:
//   POST /ui/projects/{project}/delete   (scope=local | purge-all)
//
// Before these existed the project delete route had no HTTP coverage — only the
// memory delete route did. The dotted-name case is a regression guard for projects
// like "gentleman.dots" whose name contains a '.'.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/webui"
)

// projectDeleteStore records which destructive method was invoked and with what
// project, while serving a fixed project list for the re-render.
type projectDeleteStore struct {
	mockStore
	purgeCalls     []string
	tombstoneCalls []string
}

func (m *projectDeleteStore) PurgeProjectLocal(project string) (int, error) {
	m.purgeCalls = append(m.purgeCalls, project)
	return 1, nil
}

func (m *projectDeleteStore) TombstoneProject(project string) (int, error) {
	m.tombstoneCalls = append(m.tombstoneCalls, project)
	return 1, nil
}

func newProjectDeleteServer(t *testing.T, secret string, store controlapi.Store) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl:    &mockSyncCtrl{},
		Store:       store,
		ConfigStore: &mockConfigStore{},
		Secret:      secret,
		Port:        7700,
		Version:     "test-version",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestWebUI_ProjectDelete_PurgeAll_CallsTombstone verifies scope=purge-all routes
// to TombstoneProject and returns the re-rendered rows partial (200).
func TestWebUI_ProjectDelete_PurgeAll_CallsTombstone(t *testing.T) {
	const secret = "proj-del-purge"
	store := &projectDeleteStore{mockStore: mockStore{projects: []controlapi.ProjectPolicy{}}}
	srv := newProjectDeleteServer(t, secret, store)
	client, tok := authenticatedClient(t, srv, secret)

	resp := postForm(t, client, fmt.Sprintf("%s/ui/projects/gentleman.dots/delete", srv.URL), url.Values{
		"csrf_token": {tok},
		"scope":      {"purge-all"},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if len(store.tombstoneCalls) != 1 || store.tombstoneCalls[0] != "gentleman.dots" {
		t.Errorf("TombstoneProject calls = %v; want [gentleman.dots]", store.tombstoneCalls)
	}
	if len(store.purgeCalls) != 0 {
		t.Errorf("PurgeProjectLocal should not be called for purge-all; got %v", store.purgeCalls)
	}
}

// TestWebUI_ProjectDelete_Local_CallsPurge verifies scope=local routes to
// PurgeProjectLocal.
func TestWebUI_ProjectDelete_Local_CallsPurge(t *testing.T) {
	const secret = "proj-del-local"
	store := &projectDeleteStore{mockStore: mockStore{projects: []controlapi.ProjectPolicy{}}}
	srv := newProjectDeleteServer(t, secret, store)
	client, tok := authenticatedClient(t, srv, secret)

	resp := postForm(t, client, fmt.Sprintf("%s/ui/projects/my-proj/delete", srv.URL), url.Values{
		"csrf_token": {tok},
		"scope":      {"local"},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if len(store.purgeCalls) != 1 || store.purgeCalls[0] != "my-proj" {
		t.Errorf("PurgeProjectLocal calls = %v; want [my-proj]", store.purgeCalls)
	}
	if len(store.tombstoneCalls) != 0 {
		t.Errorf("TombstoneProject should not be called for local; got %v", store.tombstoneCalls)
	}
}

// TestWebUI_ProjectDelete_BadScope verifies an unknown scope is rejected with 400
// and touches no store method.
func TestWebUI_ProjectDelete_BadScope(t *testing.T) {
	const secret = "proj-del-badscope"
	store := &projectDeleteStore{mockStore: mockStore{projects: []controlapi.ProjectPolicy{}}}
	srv := newProjectDeleteServer(t, secret, store)
	client, tok := authenticatedClient(t, srv, secret)

	resp := postForm(t, client, fmt.Sprintf("%s/ui/projects/my-proj/delete", srv.URL), url.Values{
		"csrf_token": {tok},
		"scope":      {"nonsense"},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400 for bad scope, got %d", resp.StatusCode)
	}
	if len(store.purgeCalls) != 0 || len(store.tombstoneCalls) != 0 {
		t.Errorf("no store method should be called on bad scope; purge=%v tombstone=%v",
			store.purgeCalls, store.tombstoneCalls)
	}
}

// TestWebUI_ProjectDelete_MissingCSRF verifies the route is CSRF-guarded (403) and
// no destructive method runs.
func TestWebUI_ProjectDelete_MissingCSRF(t *testing.T) {
	const secret = "proj-del-nocsrf"
	store := &projectDeleteStore{mockStore: mockStore{projects: []controlapi.ProjectPolicy{}}}
	srv := newProjectDeleteServer(t, secret, store)
	client, _ := authenticatedClient(t, srv, secret)

	resp := postForm(t, client, fmt.Sprintf("%s/ui/projects/my-proj/delete", srv.URL), url.Values{
		"scope": {"purge-all"}, // no csrf_token
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("want 403 without CSRF, got %d", resp.StatusCode)
	}
	if len(store.tombstoneCalls) != 0 {
		t.Errorf("TombstoneProject must not run without CSRF; got %v", store.tombstoneCalls)
	}
}

// TestWebUI_ProjectDelete_GET_MethodNotAllowed verifies GET on the delete path is 405.
func TestWebUI_ProjectDelete_GET_MethodNotAllowed(t *testing.T) {
	const secret = "proj-del-get"
	store := &projectDeleteStore{mockStore: mockStore{projects: []controlapi.ProjectPolicy{}}}
	srv := newProjectDeleteServer(t, secret, store)
	client, _ := authenticatedClient(t, srv, secret)

	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/ui/projects/my-proj/delete", srv.URL), nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET delete path: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("want 405 for GET, got %d", resp.StatusCode)
	}
}
