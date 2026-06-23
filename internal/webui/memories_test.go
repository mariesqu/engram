package webui_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/webui"
)

// memoriesStoreWebUI wraps mockStore and overrides ListMemories with a fixed set.
type memoriesStoreWebUI struct {
	mockStore
	memories []controlapi.MemorySummary
}

func (m *memoriesStoreWebUI) ListMemories(query, project string, limit int) ([]controlapi.MemorySummary, error) {
	return m.memories, nil
}
func (m *memoriesStoreWebUI) UpdateMemory(id int64, title, content, typ string) (controlapi.MemorySummary, error) {
	return controlapi.MemorySummary{}, nil
}
func (m *memoriesStoreWebUI) DeleteMemory(id int64) error {
	return nil
}

// newTestServerWithMemories builds a test server that returns the given memories
// from its Store.ListMemories implementation.
func newTestServerWithMemories(t *testing.T, secret string, memories []controlapi.MemorySummary) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	store := &memoriesStoreWebUI{memories: memories}
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

// TestWebUI_MemoriesPage_GET verifies that GET /ui/memories returns 200 HTML
// with the memories table when memories exist.
func TestWebUI_MemoriesPage_GET(t *testing.T) {
	const secret = "mem-tok"
	srv := newTestServerWithMemories(t, secret, []controlapi.MemorySummary{
		{ID: 1, Project: "proj-a", Type: "bugfix", Title: "Fixed null pointer", Content: "the root cause was missing nil check", Scope: "project"},
	})

	body := authenticatedGet(t, srv, secret, "/ui/memories")

	if !strings.Contains(body, "Fixed null pointer") {
		t.Error("memories page should contain the memory title")
	}
	ct := ""
	// Verify the page contains memory type badge.
	if !strings.Contains(body, "bugfix") {
		t.Error("memories page should contain the memory type")
	}
	_ = ct
}

// TestWebUI_MemoriesPage_HTML verifies GET /ui/memories returns text/html.
func TestWebUI_MemoriesPage_HTML(t *testing.T) {
	const secret = "mem-tok2"
	srv := newTestServerWithMemories(t, secret, nil)

	jar := &simpleCookieJar{}
	client := &http.Client{Jar: jar}

	// Exchange token to get session cookie.
	resp, err := client.Get(srv.URL + "/ui/?token=" + secret)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	resp.Body.Close()

	// GET /ui/memories with session cookie.
	resp2, err := client.Get(srv.URL + "/ui/memories")
	if err != nil {
		t.Fatalf("GET /ui/memories: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Errorf("want 200, got %d", resp2.StatusCode)
	}
	ct := resp2.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("want text/html, got %q", ct)
	}
}

// TestWebUI_MemoriesPage_MethodNotAllowed verifies POST /ui/memories returns 405.
func TestWebUI_MemoriesPage_MethodNotAllowed(t *testing.T) {
	const secret = "mem-tok3"
	srv := newTestServerWithMemories(t, secret, nil)

	jar := &simpleCookieJar{}
	client := &http.Client{Jar: jar}

	// Exchange token to get session cookie.
	resp, err := client.Get(srv.URL + "/ui/?token=" + secret)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/ui/memories", nil)
	// Attach session cookie manually.
	if jar.cookie != nil {
		req.AddCookie(jar.cookie)
	}
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /ui/memories: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", resp2.StatusCode)
	}
}

// TestWebUI_MemoriesPage_NoSession_401 verifies that GET /ui/memories without
// a session cookie returns 401.
func TestWebUI_MemoriesPage_NoSession_401(t *testing.T) {
	srv := newTestServerWithMemories(t, "tok", nil)

	resp, err := http.Get(srv.URL + "/ui/memories")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
}

// TestWebUI_MemoriesPage_EmptyState verifies the empty-state message appears
// when there are no memories and no search was issued.
func TestWebUI_MemoriesPage_EmptyState(t *testing.T) {
	const secret = "mem-empty-tok"
	srv := newTestServerWithMemories(t, secret, nil)

	body := authenticatedGet(t, srv, secret, "/ui/memories")
	if !strings.Contains(body, "No memories yet") {
		t.Error("empty-state page should show 'No memories yet'")
	}
}

// TestWebUI_MemoriesPage_NavLink verifies the layout nav contains the Memories link.
func TestWebUI_MemoriesPage_NavLink(t *testing.T) {
	const secret = "mem-nav-tok"
	srv := newTestServerWithMemories(t, secret, nil)

	body := authenticatedGet(t, srv, secret, "/ui/memories")
	if !strings.Contains(body, `href="/ui/memories"`) {
		t.Error("page should contain a nav link to /ui/memories")
	}
}

// TestWebUI_MemoriesPage_InEmbed verifies that memories.html is present in
// the embedded FS.
func TestWebUI_MemoriesPage_InEmbed(t *testing.T) {
	f, err := webui.TemplatesFS.Open("templates/memories.html")
	if err != nil {
		t.Fatalf("templates/memories.html not found in embedded FS: %v", err)
	}
	f.Close()
}
