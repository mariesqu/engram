package webui_test

// Tests for the project drill-in modal routes that replaced the standalone
// Memories page:
//   GET /ui/projects/{project}/memories
//   GET /ui/projects/{project}/memories/rows

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/webui"
)

// memoriesStoreWebUI wraps mockStore and serves a fixed, in-memory slice of
// memories through ListMemoriesFiltered — offset/limit are honored (so tests
// can exercise paging/HasMore); query/type/scope/date filters are NOT applied
// by the mock (that filtering logic is exercised at the localstore layer;
// see internal/localstore/search_test.go).
//
// It is also the base type memories_edit_test.go's editableMemoriesStore
// embeds to override UpdateMemory/DeleteMemory.
type memoriesStoreWebUI struct {
	mockStore
	memories []controlapi.MemorySummary
	counts   map[string]int
	// countsCalls records how many times CountsByProject was invoked — used
	// by TestWebUI_ProjectMemoriesRows_DoesNotCallCountsByProject to assert
	// the rows-only route never triggers it (see M3 in the review).
	countsCalls int
}

func (m *memoriesStoreWebUI) ListMemoriesFiltered(opts controlapi.MemoryListOptions) ([]controlapi.MemorySummary, error) {
	start := opts.Offset
	if start < 0 {
		start = 0
	}
	if start > len(m.memories) {
		start = len(m.memories)
	}
	end := len(m.memories)
	if opts.Limit > 0 && start+opts.Limit < end {
		end = start + opts.Limit
	}
	return m.memories[start:end], nil
}
func (m *memoriesStoreWebUI) UpdateMemory(id int64, title, content, typ string) (controlapi.MemorySummary, error) {
	return controlapi.MemorySummary{}, nil
}
func (m *memoriesStoreWebUI) DeleteMemory(id int64) error {
	return nil
}
func (m *memoriesStoreWebUI) CountsByProject() (map[string]int, error) {
	m.countsCalls++
	return m.counts, nil
}

// GetMemory does a full-slice by-id scan — deliberately UNBOUNDED, unlike
// ListMemoriesFiltered's offset/limit window above — so tests can assert that
// the edit route finds a memory regardless of its position (see
// TestWebUI_MemoryEdit_GET_FoundBeyondNewest200 in memories_edit_test.go).
func (m *memoriesStoreWebUI) GetMemory(id int64) (*controlapi.MemorySummary, error) {
	for i := range m.memories {
		if m.memories[i].ID == id {
			found := m.memories[i]
			return &found, nil
		}
	}
	return nil, errors.New("memory not found")
}

// newTestServerWithMemories builds a test server whose Store returns the
// given memories from ListMemoriesFiltered.
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

// ── GET /ui/projects/{project}/memories (modal shell) ───────────────────────

// TestWebUI_ProjectMemoriesModal_GET verifies that the modal shell contains
// the project name, the memory count badge, and the first page of memories.
func TestWebUI_ProjectMemoriesModal_GET(t *testing.T) {
	const secret = "modal-tok"
	mux := http.NewServeMux()
	store := &memoriesStoreWebUI{
		memories: []controlapi.MemorySummary{
			{ID: 1, Project: "proj-a", Type: "bugfix", Title: "Fixed null pointer", Content: "root cause was a missing nil check", CreatedAt: "2024-01-01T00:00:00Z"},
		},
		counts: map[string]int{"proj-a": 1},
	}
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

	body := authenticatedGet(t, srv, secret, "/ui/projects/proj-a/memories")

	assertContains(t, body, "proj-a")
	assertContains(t, body, "1 memory")
	assertContains(t, body, "Fixed null pointer")
	assertContains(t, body, "bugfix")
	assertContains(t, body, "modal-backdrop")
	assertContains(t, body, `href="/ui/memories/1/edit"`)
}

// TestWebUI_ProjectMemoriesModal_HTML verifies the modal response is
// text/html (an HTMX partial, not JSON).
func TestWebUI_ProjectMemoriesModal_HTML(t *testing.T) {
	const secret = "modal-html-tok"
	srv := newTestServerWithMemories(t, secret, nil)

	jar := &simpleCookieJar{}
	client := &http.Client{Jar: jar}
	resp, err := client.Get(srv.URL + "/ui/?token=" + secret)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	resp.Body.Close()

	resp2, err := client.Get(srv.URL + "/ui/projects/proj-a/memories")
	if err != nil {
		t.Fatalf("GET modal: %v", err)
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

// TestWebUI_ProjectMemoriesModal_EmptyState verifies the empty-state message
// when the project has no memories and no filters are active.
func TestWebUI_ProjectMemoriesModal_EmptyState(t *testing.T) {
	const secret = "modal-empty-tok"
	srv := newTestServerWithMemories(t, secret, nil)

	body := authenticatedGet(t, srv, secret, "/ui/projects/proj-a/memories")
	if !strings.Contains(body, "No memories yet in this project") {
		t.Error("empty-state should show 'No memories yet in this project'")
	}
}

// TestWebUI_ProjectMemoriesModal_FilteredEmptyState verifies the DIFFERENT
// empty-state message shown when a filter narrows results to zero.
func TestWebUI_ProjectMemoriesModal_FilteredEmptyState(t *testing.T) {
	const secret = "modal-filtered-empty-tok"
	srv := newTestServerWithMemories(t, secret, nil)

	body := authenticatedGet(t, srv, secret, "/ui/projects/proj-a/memories?type=decision")
	if !strings.Contains(body, "No memories match these filters") {
		t.Error("filtered-empty state should show 'No memories match these filters'")
	}
}

// TestWebUI_ProjectMemoriesModal_EchoesFilters verifies the filter bar
// echoes back the current query params as input/select values.
func TestWebUI_ProjectMemoriesModal_EchoesFilters(t *testing.T) {
	const secret = "modal-echo-tok"
	srv := newTestServerWithMemories(t, secret, nil)

	body := authenticatedGet(t, srv, secret, "/ui/projects/proj-a/memories?q=auth&type=decision&scope=project&from=2024-01-01&to=2024-06-01")

	assertContains(t, body, `value="auth"`)
	assertContains(t, body, `value="decision" selected`)
	assertContains(t, body, `value="project" selected`)
	assertContains(t, body, `value="2024-01-01"`)
	assertContains(t, body, `value="2024-06-01"`)
}

// TestWebUI_ProjectMemoriesModal_MethodNotAllowed verifies POST is rejected.
func TestWebUI_ProjectMemoriesModal_MethodNotAllowed(t *testing.T) {
	const secret = "modal-method-tok"
	srv := newTestServerWithMemories(t, secret, nil)
	client, _ := authenticatedClient(t, srv, secret)

	resp, err := client.Post(srv.URL+"/ui/projects/proj-a/memories", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", resp.StatusCode)
	}
}

// TestWebUI_ProjectMemoriesModal_NoSession_401 verifies the modal route
// requires a session like every other /ui/ route.
func TestWebUI_ProjectMemoriesModal_NoSession_401(t *testing.T) {
	srv := newTestServerWithMemories(t, "tok", nil)

	resp, err := http.Get(srv.URL + "/ui/projects/proj-a/memories")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
}

// ── GET /ui/projects/{project}/memories/rows (lazy load / filter refetch) ───

// TestWebUI_ProjectMemoriesRows_HasMore verifies that when more memories
// exist than the page size, the last row carries the "intersect once"
// lazy-load trigger with the correct next offset — and that the
// (pageSize+1)th over-fetched row is trimmed from the response.
//
// "intersect once" (not htmx's plain "revealed" trigger) is required because
// the modal's scroller is #memory-rows-list / .modal-body (overflow-y:auto),
// NOT the window — htmx's "revealed" trigger only re-evaluates on WINDOW
// scroll/resize, so it would never fire for a row that becomes visible by
// scrolling a nested container. "intersect" uses a real IntersectionObserver,
// which fires correctly for nested scroll containers too.
func TestWebUI_ProjectMemoriesRows_HasMore(t *testing.T) {
	const secret = "rows-hasmore-tok"
	memories := make([]controlapi.MemorySummary, 31) // pageSize (30) + 1
	for i := range memories {
		memories[i] = controlapi.MemorySummary{
			ID:      int64(i + 1),
			Project: "proj-a",
			Type:    "manual",
			Title:   fmt.Sprintf("memory %d", i+1),
			Content: "content",
		}
	}
	srv := newTestServerWithMemories(t, secret, memories)

	body := authenticatedGet(t, srv, secret, "/ui/projects/proj-a/memories/rows")

	assertContains(t, body, "memory 1")
	assertContains(t, body, "memory 30")
	if strings.Contains(body, "memory 31") {
		t.Error("the (pageSize+1)th over-fetched row must be trimmed from the response")
	}
	assertContains(t, body, `hx-trigger="intersect once"`)
	assertContains(t, body, "offset=30")
}

// TestWebUI_ProjectMemoriesRows_NoMore verifies that when all memories fit in
// one page, no lazy-load trigger is rendered.
func TestWebUI_ProjectMemoriesRows_NoMore(t *testing.T) {
	const secret = "rows-nomore-tok"
	memories := []controlapi.MemorySummary{
		{ID: 1, Project: "proj-a", Type: "manual", Title: "only one", Content: "content"},
	}
	srv := newTestServerWithMemories(t, secret, memories)

	body := authenticatedGet(t, srv, secret, "/ui/projects/proj-a/memories/rows")

	assertContains(t, body, "only one")
	if strings.Contains(body, `hx-trigger="intersect once"`) {
		t.Error("no lazy-load trigger should be rendered when everything fits in one page")
	}
}

// TestWebUI_ProjectMemoriesRows_Offset verifies the offset query param is
// forwarded to the store (page 2 excludes page 1's rows).
func TestWebUI_ProjectMemoriesRows_Offset(t *testing.T) {
	const secret = "rows-offset-tok"
	memories := []controlapi.MemorySummary{
		{ID: 1, Project: "proj-a", Type: "manual", Title: "page1 item", Content: "content"},
		{ID: 2, Project: "proj-a", Type: "manual", Title: "page2 item", Content: "content"},
	}
	srv := newTestServerWithMemories(t, secret, memories)

	body := authenticatedGet(t, srv, secret, "/ui/projects/proj-a/memories/rows?offset=1")

	assertContains(t, body, "page2 item")
	if strings.Contains(body, "page1 item") {
		t.Error("offset=1 should skip the first row")
	}
}

// TestWebUI_ProjectMemoriesRows_NoWrapper verifies the rows-only response
// does NOT contain the modal chrome (backdrop/filter form) — only row markup.
func TestWebUI_ProjectMemoriesRows_NoWrapper(t *testing.T) {
	const secret = "rows-nowrapper-tok"
	memories := []controlapi.MemorySummary{
		{ID: 1, Project: "proj-a", Type: "manual", Title: "solo", Content: "content"},
	}
	srv := newTestServerWithMemories(t, secret, memories)

	body := authenticatedGet(t, srv, secret, "/ui/projects/proj-a/memories/rows")

	if strings.Contains(body, "modal-backdrop") {
		t.Error("rows-only response must not include the modal shell")
	}
	assertContains(t, body, "solo")
}

// TestWebUI_ProjectMemoriesRows_MethodNotAllowed verifies POST is rejected.
func TestWebUI_ProjectMemoriesRows_MethodNotAllowed(t *testing.T) {
	const secret = "rows-method-tok"
	srv := newTestServerWithMemories(t, secret, nil)
	client, _ := authenticatedClient(t, srv, secret)

	resp, err := client.Post(srv.URL+"/ui/projects/proj-a/memories/rows", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", resp.StatusCode)
	}
}

// TestWebUI_ProjectMemoriesRows_NoSession_401 verifies the rows route also
// requires a session.
func TestWebUI_ProjectMemoriesRows_NoSession_401(t *testing.T) {
	srv := newTestServerWithMemories(t, "tok", nil)

	resp, err := http.Get(srv.URL + "/ui/projects/proj-a/memories/rows")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
}

// ── Memories page removal ────────────────────────────────────────────────────

// TestWebUI_StandaloneMemoriesPage_Removed verifies that the old standalone
// /ui/memories route no longer exists (it falls through to the 404 catch-all).
func TestWebUI_StandaloneMemoriesPage_Removed(t *testing.T) {
	const secret = "removed-tok"
	srv := newTestServerWithMemories(t, secret, nil)
	client, _ := authenticatedClient(t, srv, secret)

	resp, err := client.Get(srv.URL + "/ui/memories")
	if err != nil {
		t.Fatalf("GET /ui/memories: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/ui/memories should be gone (404), got %d", resp.StatusCode)
	}
}

// TestWebUI_Nav_NoMemoriesLink verifies the layout nav no longer links to a
// standalone Memories page — Projects is the single browse surface.
func TestWebUI_Nav_NoMemoriesLink(t *testing.T) {
	const secret = "nav-tok"
	srv := newTestServerWithMemories(t, secret, nil)

	body := authenticatedGet(t, srv, secret, "/ui/projects")
	if strings.Contains(body, `href="/ui/memories"`) {
		t.Error("nav should no longer contain a link to /ui/memories")
	}
}

// TestWebUI_ProjectMemoriesTemplate_InEmbed verifies project_memories.html is
// present in the embedded filesystem.
func TestWebUI_ProjectMemoriesTemplate_InEmbed(t *testing.T) {
	f, err := webui.TemplatesFS.Open("templates/project_memories.html")
	if err != nil {
		t.Fatalf("templates/project_memories.html not found in embedded FS: %v", err)
	}
	f.Close()
}

// ── CountsByProject call scoping (M3) ────────────────────────────────────────

// TestWebUI_ProjectMemoriesRows_DoesNotCallCountsByProject verifies that the
// rows-only route (hit on every filter keystroke and every lazy-load page)
// never calls Store.CountsByProject — that query's result (the header count
// badge) is only ever rendered by the modal-shell fragment, so computing it
// on every rows request would be pure waste. The modal-shell route, in
// contrast, must still call it exactly once per request.
func TestWebUI_ProjectMemoriesRows_DoesNotCallCountsByProject(t *testing.T) {
	const secret = "rows-nocounts-tok"
	mux := http.NewServeMux()
	store := &memoriesStoreWebUI{
		memories: []controlapi.MemorySummary{
			{ID: 1, Project: "proj-a", Type: "manual", Title: "t", Content: "c"},
		},
		counts: map[string]int{"proj-a": 1},
	}
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

	authenticatedGet(t, srv, secret, "/ui/projects/proj-a/memories/rows")
	if store.countsCalls != 0 {
		t.Errorf("rows-only route must not call CountsByProject; got %d call(s)", store.countsCalls)
	}

	authenticatedGet(t, srv, secret, "/ui/projects/proj-a/memories")
	if store.countsCalls != 1 {
		t.Errorf("modal-shell route must call CountsByProject exactly once; got %d call(s)", store.countsCalls)
	}
}

// TestWebUI_ProjectMemoriesRows_LazyPageEmpty_ReturnsEmptyFragment verifies
// that an offset>0 ("load more") request returning zero rows renders an
// empty fragment rather than the "No memories…" empty-state text — that text
// would otherwise be APPENDED after the already-rendered rows (hx-swap
// "afterend"), which reads as a bug rather than "you've reached the end".
func TestWebUI_ProjectMemoriesRows_LazyPageEmpty_ReturnsEmptyFragment(t *testing.T) {
	const secret = "rows-lazyempty-tok"
	memories := []controlapi.MemorySummary{
		{ID: 1, Project: "proj-a", Type: "manual", Title: "only one", Content: "content"},
	}
	srv := newTestServerWithMemories(t, secret, memories)

	body := authenticatedGet(t, srv, secret, "/ui/projects/proj-a/memories/rows?offset=30")

	if strings.TrimSpace(body) != "" {
		t.Errorf("empty lazy-load page should render an empty fragment, got %q", body)
	}
}
