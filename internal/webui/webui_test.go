package webui_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/webui"
)

// ── Mock dependencies ────────────────────────────────────────────────────────

type mockSyncCtrl struct {
	status controlapi.Status
}

func (m *mockSyncCtrl) Status() controlapi.Status                  { return m.status }
func (m *mockSyncCtrl) TriggerNow(_ context.Context) error         { return nil }
func (m *mockSyncCtrl) Disconnect() error                          { return nil }
func (m *mockSyncCtrl) Reconnect(_ controlapi.CentralConfig) error { return nil }

type mockStore struct {
	projects []controlapi.ProjectPolicy
}

func (m *mockStore) ListProjectsWithPolicy() ([]controlapi.ProjectPolicy, error) {
	return m.projects, nil
}
func (m *mockStore) SetPolicy(project string, p controlapi.Policy) error {
	return nil
}
func (m *mockStore) GetPolicy(project string) (controlapi.Policy, error) {
	return controlapi.PolicySynced, nil
}

// newTestServer builds a fresh httptest.Server with a real webui.Mount.
// Mount creates a PER-INSTANCE session store, so every test server is fully
// isolated - a cookie minted by one test cannot authenticate against another.
func newTestServer(t *testing.T, secret string, status controlapi.Status, projects []controlapi.ProjectPolicy) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl: &mockSyncCtrl{status: status},
		Store:    &mockStore{projects: projects},
		Secret:   secret,
		Port:     7700,
		Version:  "0.1.0-test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// ── Task 4a.8: token→cookie exchange ────────────────────────────────────────

// TestTokenExchange_ValidToken_SetsCookieAndRedirects verifies the happy path:
// GET /ui/?token=GOOD → 303 → /ui/, Set-Cookie with HttpOnly+SameSite=Strict.
func TestTokenExchange_ValidToken_SetsCookieAndRedirects(t *testing.T) {
	const secret = "correct-token-abc"
	srv := newTestServer(t, secret, controlapi.Status{DaemonVersion: "0.1.0"}, nil)

	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse // don't follow redirect
		},
	}

	resp, err := client.Get(srv.URL + "/ui/?token=" + secret)
	if err != nil {
		t.Fatalf("GET /ui/?token=...: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("want 303 SeeOther, got %d", resp.StatusCode)
	}

	loc := resp.Header.Get("Location")
	if loc != "/ui/" {
		t.Errorf("want Location: /ui/, got %q", loc)
	}

	// Verify session cookie is present with correct attributes.
	var found bool
	for _, c := range resp.Cookies() {
		if c.Name != "engram_session" {
			continue
		}
		found = true
		if !c.HttpOnly {
			t.Error("session cookie must be HttpOnly")
		}
		if c.SameSite != http.SameSiteStrictMode {
			t.Errorf("want SameSite=Strict, got %v", c.SameSite)
		}
		if c.Path != "/ui/" {
			t.Errorf("want Path=/ui/, got %q", c.Path)
		}
		if c.Secure {
			t.Error("session cookie must NOT be Secure on loopback (Secure=false)")
		}
		if c.MaxAge <= 0 {
			t.Error("session cookie MaxAge should be positive")
		}
	}
	if !found {
		t.Error("response has no engram_session cookie")
	}
}

// TestTokenExchange_InvalidToken_401 verifies that a wrong token → 401.
func TestTokenExchange_InvalidToken_401(t *testing.T) {
	srv := newTestServer(t, "right-token", controlapi.Status{}, nil)

	resp, err := http.Get(srv.URL + "/ui/?token=wrong-token")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
}

// TestTokenExchange_AbsentToken_NoCookieRequireSession_401 verifies that
// GET /ui/ without any token and without a session cookie → 401.
func TestTokenExchange_AbsentToken_NoCookieRequireSession_401(t *testing.T) {
	srv := newTestServer(t, "mytoken", controlapi.Status{}, nil)

	resp, err := http.Get(srv.URL + "/ui/")
	if err != nil {
		t.Fatalf("GET /ui/: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
}

// TestRequireSession_ValidCookie_Passes verifies that after the exchange, a
// session cookie allows subsequent requests.
func TestRequireSession_ValidCookie_Passes(t *testing.T) {
	const secret = "valid-session-token"
	srv := newTestServer(t, secret, controlapi.Status{DaemonVersion: "0.1.0"}, nil)

	// Step 1: exchange the token to get a session cookie.
	jar := &simpleCookieJar{}
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Perform exchange.
	resp, err := client.Get(srv.URL + "/ui/?token=" + secret)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("exchange: want 303, got %d", resp.StatusCode)
	}

	// Collect the session cookie.
	for _, c := range resp.Cookies() {
		if c.Name == "engram_session" {
			jar.cookie = c
		}
	}
	if jar.cookie == nil {
		t.Fatal("no session cookie after exchange")
	}

	// Step 2: GET /ui/ with the session cookie — should return 200 HTML.
	client2 := &http.Client{Jar: jar}
	resp2, err := client2.Get(srv.URL + "/ui/")
	if err != nil {
		t.Fatalf("GET /ui/ with cookie: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Errorf("want 200, got %d", resp2.StatusCode)
	}
	ct := resp2.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("want Content-Type text/html, got %q", ct)
	}
}

// ── Task 4a.9: route tests ───────────────────────────────────────────────────

// TestWebUI_GetStatus_200_HTML verifies GET /ui/ returns 200 HTML with status markers.
func TestWebUI_GetStatus_200_HTML(t *testing.T) {
	const secret = "status-tok"
	now := time.Now().UTC()
	centralURL := "http://central.example.com"
	status := controlapi.Status{
		CentralConnected: true,
		CentralURL:       &centralURL,
		LastSyncResult: controlapi.SyncResult{
			At:     &now,
			Pushed: 3,
			Pulled: 1,
		},
		DaemonVersion: "0.1.0",
	}
	srv := newTestServer(t, secret, status, nil)
	body := authenticatedGet(t, srv, secret, "/ui/")

	assertContains(t, body, "connected")
	assertContains(t, body, "Daemon Status")
	assertContains(t, body, "0.1.0")
}

// TestWebUI_GetProjects_200_HTML verifies GET /ui/projects returns 200 HTML with project rows.
func TestWebUI_GetProjects_200_HTML(t *testing.T) {
	const secret = "proj-tok"
	projects := []controlapi.ProjectPolicy{
		{Name: "alpha", Policy: controlapi.PolicySynced},
		{Name: "beta", Policy: controlapi.PolicyLocalOnly},
		{Name: "gamma", Policy: controlapi.PolicyOmitted},
	}
	srv := newTestServer(t, secret, controlapi.Status{DaemonVersion: "0.1.0"}, projects)
	body := authenticatedGet(t, srv, secret, "/ui/projects")

	assertContains(t, body, "alpha")
	assertContains(t, body, "beta")
	assertContains(t, body, "gamma")
	assertContains(t, body, "badge-synced")
	assertContains(t, body, "badge-local-only")
	assertContains(t, body, "badge-omitted")
}

// TestWebUI_PollingPartial_Status verifies GET /ui/status returns the HTMX
// partial fragment with live status values and no full HTML wrapper.
func TestWebUI_PollingPartial_Status(t *testing.T) {
	const secret = "partial-tok"
	centralURL := "http://central.test"
	status := controlapi.Status{
		CentralConnected: true,
		CentralURL:       &centralURL,
		DaemonVersion:    "0.1.0",
	}
	srv := newTestServer(t, secret, status, nil)
	body := authenticatedGet(t, srv, secret, "/ui/status")

	assertContains(t, body, "status-fragment")
	assertContains(t, body, "connected")
	assertContains(t, body, "hx-trigger")
	// A partial must NOT contain the full HTML wrapper.
	if strings.Contains(body, "<html") {
		t.Error("status partial must not contain <html> wrapper")
	}
}

// TestWebUI_NoSession_Returns401 verifies that /ui/ without a session
// cookie returns 401 (not a redirect, since we can't auto-redirect to the
// exchange — the user must run `engram ui` for a fresh token).
func TestWebUI_NoSession_Returns401(t *testing.T) {
	srv := newTestServer(t, "tok", controlapi.Status{}, nil)

	resp, err := http.Get(srv.URL + "/ui/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "engram ui") {
		t.Error("401 page should hint at running `engram ui`")
	}
}

// ── Task 4a.3: embed test ────────────────────────────────────────────────────

// TestEmbed_AllFilesPresent verifies that all required files are present in
// the embedded FS (htmx.min.js, styles.css, layout, status, projects templates).
func TestEmbed_AllFilesPresent(t *testing.T) {
	required := []string{
		"static/htmx.min.js",
		"static/styles.css",
		"templates/layout.html",
		"templates/status.html",
		"templates/status_partial.html",
		"templates/projects.html",
	}
	for _, path := range required {
		f, err := webui.TemplatesFS.Open(path)
		if err != nil {
			t.Errorf("embedded file missing: %s: %v", path, err)
			continue
		}
		f.Close()
	}
}

// ── Offline assets test ──────────────────────────────────────────────────────

// TestNoExternalAssetReferences verifies that rendered HTML pages do not
// reference any external host (http:// or https:// absolute URLs in asset
// references). The htmx.min.js file itself may contain spec/license comments
// with URLs — those are in JS not HTML src/href attributes, and are excluded
// from this check by only scanning rendered page output, not the JS file itself.
func TestNoExternalAssetReferences(t *testing.T) {
	const secret = "offline-tok"
	centralURL := "http://central.test"
	now := time.Now().UTC()
	projects := []controlapi.ProjectPolicy{
		{Name: "proj-a", Policy: controlapi.PolicySynced},
	}
	status := controlapi.Status{
		CentralConnected: true,
		CentralURL:       &centralURL,
		LastSyncResult:   controlapi.SyncResult{At: &now},
		DaemonVersion:    "0.1.0",
	}
	srv := newTestServer(t, secret, status, projects)

	pages := []string{"/ui/", "/ui/projects", "/ui/status"}
	for _, page := range pages {
		body := authenticatedGet(t, srv, secret, page)

		// Check for external src/href references — patterns that would cause a
		// browser to fetch from an external host.
		for _, pattern := range []string{
			`src="http://`, `src="https://`,
			`href="http://`, `href="https://`,
			`url(http://`, `url(https://`,
		} {
			if strings.Contains(body, pattern) {
				t.Errorf("page %s contains external asset reference: %q", page, pattern)
			}
		}
	}

	// Additionally verify htmx.min.js is served from a relative path.
	assertContains(t, authenticatedGet(t, srv, secret, "/ui/"), `/ui/static/htmx.min.js`)
}

// TestCookieDoesNotAuthenticateControlAPI verifies that the /api/* routes
// return 401 when only a session cookie is presented (no bearer token).
// This test wires a minimal bearer-gated handler at /api/ to prove isolation.
// The full daemon-topology proof lives in acceptance_test.go.
func TestCookieDoesNotAuthenticateControlAPI(t *testing.T) {
	const secret = "api-isolation-tok"

	// Build a mux with BOTH the webui AND a minimal bearer-gated /api/ handler.
	mux := http.NewServeMux()

	// A simple bearer-gated mock for /api/v1/status.
	mux.HandleFunc("/api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+secret {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"daemon_version":"0.1.0-test"}`))
	})

	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl: &mockSyncCtrl{},
		Store:    &mockStore{},
		Secret:   secret,
		Port:     7700,
		Version:  "0.1.0-test",
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Obtain a valid session cookie via the exchange.
	jar := &simpleCookieJar{}
	exchClient := &http.Client{
		Jar: jar,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := exchClient.Get(srv.URL + "/ui/?token=" + secret)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "engram_session" {
			jar.cookie = c
		}
	}
	if jar.cookie == nil {
		t.Fatal("no session cookie after exchange")
	}

	// Now hit /api/v1/status with the cookie jar but WITHOUT a bearer token.
	// The bearer gate must return 401.
	cookieClient := &http.Client{Jar: jar}
	resp2, err := cookieClient.Get(srv.URL + "/api/v1/status")
	if err != nil {
		t.Fatalf("GET /api/v1/status: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf(
			"cookie MUST NOT authenticate /api/v1/status — want 401, got %d",
			resp2.StatusCode,
		)
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// authenticatedGet performs the token exchange and then GETs the target path,
// returning the response body as a string. The test fails if either step fails.
func authenticatedGet(t *testing.T, srv *httptest.Server, secret, path string) string {
	t.Helper()

	jar := &simpleCookieJar{}
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Follow redirects (default) but capture cookies from intermediate responses.
			return nil
		},
	}

	// Exchange to set the session cookie.
	resp, err := client.Get(srv.URL + "/ui/?token=" + secret)
	if err != nil {
		t.Fatalf("authenticatedGet: exchange: %v", err)
	}
	resp.Body.Close()
	// After the redirect chain the client should hold the cookie in jar.

	// GET the target path.
	resp2, err := client.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("authenticatedGet: GET %s: %v", path, err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("authenticatedGet: want 200 for %s, got %d", path, resp2.StatusCode)
	}
	body, _ := io.ReadAll(resp2.Body)
	return string(body)
}

func assertContains(t *testing.T, body, substr string) {
	t.Helper()
	if !strings.Contains(body, substr) {
		t.Errorf("response body does not contain %q", substr)
	}
}

// simpleCookieJar is a minimal http.CookieJar that stores one cookie for use
// in test sequences where we only need to forward the session cookie.
type simpleCookieJar struct {
	cookie *http.Cookie
}

func (j *simpleCookieJar) SetCookies(_ *url.URL, cookies []*http.Cookie) {
	for _, c := range cookies {
		if c.Name == "engram_session" {
			j.cookie = c
		}
	}
}

func (j *simpleCookieJar) Cookies(_ *url.URL) []*http.Cookie {
	if j.cookie == nil {
		return nil
	}
	return []*http.Cookie{j.cookie}
}

// ─── Round-1 review additions ───────────────────────────────────────────────

// TestExchange_DeepLinkWithToken_DoesNotExchange: the token-accepting surface
// is ONLY the canonical /ui/ entry point - a stray ?token= on a sub-path must
// not mint a session (it 401s like any unauthenticated request).
func TestExchange_DeepLinkWithToken_DoesNotExchange(t *testing.T) {
	ts := newTestServer(t, "sekrit-token", controlapi.Status{}, nil)
	client := noRedirectClient()

	resp, err := client.Get(ts.URL + "/ui/projects?token=sekrit-token")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("deep link with token: got %d, want 401 (no exchange outside /ui/)", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "engram_session" {
			t.Error("deep link with token minted a session cookie")
		}
	}
}

// TestExchange_EmptySecret_NeverExchanges: a daemon misconfigured with an
// empty secret must not let ?token= (any value, including empty) mint a session.
func TestExchange_EmptySecret_NeverExchanges(t *testing.T) {
	ts := newTestServer(t, "", controlapi.Status{}, nil)
	client := noRedirectClient()

	for _, q := range []string{"?token=", "?token=anything"} {
		resp, err := client.Get(ts.URL + "/ui/" + q)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("empty-secret exchange %q: got %d, want 401", q, resp.StatusCode)
		}
	}
}

// TestSessionIsolation_TwoServers: a cookie minted by server A must NOT
// authenticate against server B (per-instance session stores - this was a
// package-level global before round 1).
func TestSessionIsolation_TwoServers(t *testing.T) {
	a := newTestServer(t, "secret-a", controlapi.Status{}, nil)
	b := newTestServer(t, "secret-b", controlapi.Status{}, nil)
	client := noRedirectClient()

	resp, err := client.Get(a.URL + "/ui/?token=secret-a")
	if err != nil {
		t.Fatalf("exchange on A: %v", err)
	}
	resp.Body.Close()
	var sess *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "engram_session" {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("no session cookie from A")
	}

	req, _ := http.NewRequest(http.MethodGet, b.URL+"/ui/", nil)
	req.AddCookie(sess)
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatalf("replay on B: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("cookie from server A accepted by server B: got %d, want 401 (session state leaked)", resp2.StatusCode)
	}
}

// TestSecurityHeaders_OnUIResponses: every /ui response (page and static
// asset alike) carries the browser security headers.
func TestSecurityHeaders_OnUIResponses(t *testing.T) {
	ts := newTestServer(t, "tok-sec", controlapi.Status{}, nil)
	client := noRedirectClient()

	for _, path := range []string{"/ui/", "/ui/static/styles.css"} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q, want DENY", path, got)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", path, got)
		}
		if got := resp.Header.Get("Content-Security-Policy"); !strings.Contains(got, "default-src "+"'self'") {
			t.Errorf("%s: CSP = %q, want self-only policy", path, got)
		}
	}
}

// noRedirectClient returns an http.Client that does not follow redirects, so
// tests can assert on the 303 exchange response itself.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
