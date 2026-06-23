package webui_test

// Tests for the WebUI edit and delete memory routes:
//   POST /ui/memories/{id}/delete
//   GET  /ui/memories/{id}/edit
//   POST /ui/memories/{id}/edit

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/webui"
)

// ── Configurable store for edit/delete scenarios ─────────────────────────────

// editableMemoriesStore extends memoriesStoreWebUI with configurable
// UpdateMemory and DeleteMemory behavior.
type editableMemoriesStore struct {
	memoriesStoreWebUI
	updateResult controlapi.MemorySummary
	updateErr    error
	deleteErr    error
}

func (m *editableMemoriesStore) UpdateMemory(id int64, title, content, typ string) (controlapi.MemorySummary, error) {
	return m.updateResult, m.updateErr
}
func (m *editableMemoriesStore) DeleteMemory(id int64) error {
	return m.deleteErr
}

// newEditTestServer builds a test server with the given memories list and
// configurable update/delete behavior.
func newEditTestServer(t *testing.T, secret string, memories []controlapi.MemorySummary, updateErr, deleteErr error) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	store := &editableMemoriesStore{
		memoriesStoreWebUI: memoriesStoreWebUI{memories: memories},
		deleteErr:          deleteErr,
		updateErr:          updateErr,
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
	return srv
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// exchangeResult holds the cookies extracted from a successful token exchange.
type exchangeResult struct {
	sessionVal string
	csrfVal    string
}

// doExchange performs the token exchange and returns the session and CSRF cookie
// values from the response. It does not follow the redirect.
func doExchange(t *testing.T, srvURL, secret string) exchangeResult {
	t.Helper()
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(srvURL + "/ui/?token=" + secret)
	if err != nil {
		t.Fatalf("doExchange: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("doExchange: want 303, got %d", resp.StatusCode)
	}
	var res exchangeResult
	for _, c := range resp.Cookies() {
		switch c.Name {
		case "engram_session":
			res.sessionVal = c.Value
		case "engram_csrf":
			res.csrfVal = c.Value
		}
	}
	if res.sessionVal == "" {
		t.Fatal("doExchange: no engram_session cookie")
	}
	if res.csrfVal == "" {
		t.Fatal("doExchange: no engram_csrf cookie")
	}
	return res
}

// authenticatedClient returns an http.Client that sends both the session and
// CSRF cookies on every request. It does NOT follow redirects so tests can
// inspect 303 responses. The CSRF cookie value is also returned so callers can
// embed it as a form field.
func authenticatedClient(t *testing.T, srv *httptest.Server, secret string) (*http.Client, string) {
	t.Helper()
	ex := doExchange(t, srv.URL, secret)

	sessionCookie := &http.Cookie{Name: "engram_session", Value: ex.sessionVal, Path: "/ui/"}
	csrfCookie := &http.Cookie{Name: "engram_csrf", Value: ex.csrfVal, Path: "/ui/"}

	client := &http.Client{
		Transport: &cookieInjector{
			wrapped:  http.DefaultTransport,
			cookies:  []*http.Cookie{sessionCookie, csrfCookie},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return client, ex.csrfVal
}

// cookieInjector is a RoundTripper that injects fixed cookies on every request.
type cookieInjector struct {
	wrapped http.RoundTripper
	cookies []*http.Cookie
}

func (c *cookieInjector) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone the request so we don't mutate the caller's copy.
	r2 := req.Clone(req.Context())
	for _, ck := range c.cookies {
		r2.AddCookie(ck)
	}
	return c.wrapped.RoundTrip(r2)
}

// postForm sends a POST with url.Values form data using the given client.
func postForm(t *testing.T, client *http.Client, target string, vals url.Values) *http.Response {
	t.Helper()
	resp, err := client.PostForm(target, vals)
	if err != nil {
		t.Fatalf("postForm %s: %v", target, err)
	}
	return resp
}

// readRespBody reads and closes the response body.
func readRespBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("readRespBody: %v", err)
	}
	return string(b)
}

// ── DELETE tests ─────────────────────────────────────────────────────────────

// TestWebUI_MemoryDelete_POST_WithCSRF verifies that a POST with a valid CSRF
// token returns 303 and redirects to /ui/memories.
func TestWebUI_MemoryDelete_POST_WithCSRF(t *testing.T) {
	const secret = "del-tok"
	memories := []controlapi.MemorySummary{
		{ID: 1, Title: "to delete", Type: "bugfix", Content: "x"},
	}
	srv := newEditTestServer(t, secret, memories, nil, nil)

	client, tok := authenticatedClient(t, srv, secret)

	resp := postForm(t, client, fmt.Sprintf("%s/ui/memories/1/delete", srv.URL), url.Values{
		"csrf_token": {tok},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("want 303 on successful delete, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/memories" {
		t.Errorf("Location = %q, want %q", loc, "/ui/memories")
	}
}

// TestWebUI_MemoryDelete_POST_MissingCSRF verifies that a POST without the CSRF
// token is rejected with 403.
func TestWebUI_MemoryDelete_POST_MissingCSRF(t *testing.T) {
	const secret = "del-csrf-tok"
	memories := []controlapi.MemorySummary{
		{ID: 2, Title: "another", Type: "decision", Content: "y"},
	}
	srv := newEditTestServer(t, secret, memories, nil, nil)
	client, _ := authenticatedClient(t, srv, secret)

	resp := postForm(t, client, fmt.Sprintf("%s/ui/memories/2/delete", srv.URL), url.Values{
		// No csrf_token field.
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("want 403 without CSRF token, got %d", resp.StatusCode)
	}
}

// TestWebUI_MemoryDelete_POST_NoSession verifies that a POST without a session
// cookie returns 401.
func TestWebUI_MemoryDelete_POST_NoSession(t *testing.T) {
	const secret = "del-nosession-tok"
	srv := newEditTestServer(t, secret, nil, nil, nil)

	// Unauthenticated client (no cookie).
	resp, err := http.PostForm(fmt.Sprintf("%s/ui/memories/1/delete", srv.URL), url.Values{
		"csrf_token": {"some-token"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401 without session, got %d", resp.StatusCode)
	}
}

// TestWebUI_MemoryDelete_GET_MethodNotAllowed verifies that GET on the delete
// path returns 405.
func TestWebUI_MemoryDelete_GET_MethodNotAllowed(t *testing.T) {
	const secret = "del-method-tok"
	srv := newEditTestServer(t, secret, nil, nil, nil)
	client, _ := authenticatedClient(t, srv, secret)

	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/ui/memories/1/delete", srv.URL), nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET delete path: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("want 405 for GET on delete path, got %d", resp.StatusCode)
	}
}

// ── EDIT GET tests ────────────────────────────────────────────────────────────

// TestWebUI_MemoryEdit_GET_ShowsForm verifies that GET /ui/memories/{id}/edit
// returns 200 with an HTML form containing the memory's current values.
func TestWebUI_MemoryEdit_GET_ShowsForm(t *testing.T) {
	const secret = "edit-get-tok"
	memories := []controlapi.MemorySummary{
		{ID: 5, Title: "original title", Type: "decision", Content: "original content"},
	}
	srv := newEditTestServer(t, secret, memories, nil, nil)

	body := authenticatedGet(t, srv, secret, "/ui/memories/5/edit")

	if !strings.Contains(body, "original title") {
		t.Error("edit form should contain the current title")
	}
	if !strings.Contains(body, "original content") {
		t.Error("edit form should contain the current content")
	}
	if !strings.Contains(body, `action="/ui/memories/5/edit"`) {
		t.Error("edit form action attribute should post to the same path")
	}
}

// TestWebUI_MemoryEdit_GET_NotFound verifies that GET for an unknown ID returns 404.
func TestWebUI_MemoryEdit_GET_NotFound(t *testing.T) {
	const secret = "edit-404-tok"
	srv := newEditTestServer(t, secret, nil, nil, nil) // empty memories list

	client, _ := authenticatedClient(t, srv, secret)

	resp, err := client.Get(srv.URL + "/ui/memories/999/edit")
	if err != nil {
		t.Fatalf("GET edit 999: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404 for unknown memory id, got %d", resp.StatusCode)
	}
}

// TestWebUI_MemoryEdit_GET_NoSession verifies that GET /ui/memories/{id}/edit
// without a session returns 401.
func TestWebUI_MemoryEdit_GET_NoSession(t *testing.T) {
	const secret = "edit-nosess-tok"
	srv := newEditTestServer(t, secret, nil, nil, nil)

	resp, err := http.Get(fmt.Sprintf("%s/ui/memories/1/edit", srv.URL))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401 without session, got %d", resp.StatusCode)
	}
}

// ── EDIT POST tests ───────────────────────────────────────────────────────────

// TestWebUI_MemoryEdit_POST_WithCSRF verifies that a valid POST to the edit
// route returns 303 redirect to /ui/memories.
func TestWebUI_MemoryEdit_POST_WithCSRF(t *testing.T) {
	const secret = "edit-post-tok"
	memories := []controlapi.MemorySummary{
		{ID: 10, Title: "old title", Type: "bugfix", Content: "old content"},
	}
	srv := newEditTestServer(t, secret, memories, nil, nil)

	client, tok := authenticatedClient(t, srv, secret)

	resp := postForm(t, client, fmt.Sprintf("%s/ui/memories/10/edit", srv.URL), url.Values{
		"csrf_token": {tok},
		"title":      {"new title"},
		"content":    {"new content"},
		"type":       {"decision"},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		body := readRespBody(t, resp)
		t.Errorf("want 303 on edit, got %d — body: %s", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/memories" {
		t.Errorf("Location = %q, want /ui/memories", loc)
	}
}

// TestWebUI_MemoryEdit_POST_MissingCSRF verifies that a POST without the CSRF
// token is rejected with 403.
func TestWebUI_MemoryEdit_POST_MissingCSRF(t *testing.T) {
	const secret = "edit-csrf-tok"
	memories := []controlapi.MemorySummary{
		{ID: 11, Title: "existing", Type: "bugfix", Content: "content"},
	}
	srv := newEditTestServer(t, secret, memories, nil, nil)
	client, _ := authenticatedClient(t, srv, secret)

	resp := postForm(t, client, fmt.Sprintf("%s/ui/memories/11/edit", srv.URL), url.Values{
		"title":   {"new title"},
		"content": {"new content"},
		// Missing csrf_token.
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("want 403 without CSRF token, got %d", resp.StatusCode)
	}
}

// TestWebUI_MemoryEdit_POST_MissingTitle verifies that a POST with an empty
// title returns 422 (Unprocessable Entity) and re-renders the form with an
// error message.
func TestWebUI_MemoryEdit_POST_MissingTitle(t *testing.T) {
	const secret = "edit-notitle-tok"
	memories := []controlapi.MemorySummary{
		{ID: 12, Title: "existing", Type: "bugfix", Content: "content"},
	}
	srv := newEditTestServer(t, secret, memories, nil, nil)
	client, tok := authenticatedClient(t, srv, secret)

	resp := postForm(t, client, fmt.Sprintf("%s/ui/memories/12/edit", srv.URL), url.Values{
		"csrf_token": {tok},
		"title":      {""},
		"content":    {"some content"},
	})
	defer resp.Body.Close()

	// The webui re-renders the edit form with a validation error (422).
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for empty title, got %d", resp.StatusCode)
	}
}

// TestWebUI_MemoryEdit_POST_NoSession verifies that POST /ui/memories/{id}/edit
// without a session returns 401.
func TestWebUI_MemoryEdit_POST_NoSession(t *testing.T) {
	const secret = "edit-noauth-tok"
	srv := newEditTestServer(t, secret, nil, nil, nil)

	resp, err := http.PostForm(fmt.Sprintf("%s/ui/memories/1/edit", srv.URL), url.Values{
		"csrf_token": {"any"},
		"title":      {"t"},
		"content":    {"c"},
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401 without session, got %d", resp.StatusCode)
	}
}

// TestWebUI_MemoryEditTemplate_InEmbed verifies that memory_edit.html is present
// in the embedded filesystem.
func TestWebUI_MemoryEditTemplate_InEmbed(t *testing.T) {
	f, err := webui.TemplatesFS.Open("templates/memory_edit.html")
	if err != nil {
		t.Fatalf("templates/memory_edit.html not in embedded FS: %v", err)
	}
	f.Close()
}
