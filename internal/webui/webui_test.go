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

// mockConfigStore is a minimal controlapi.ConfigStore for unit tests.
type mockConfigStore struct {
	cfg             controlapi.RedactedConfig
	restartRequired bool
	applyErr        error
	lastPatch       *controlapi.ConfigPatch
}

func (m *mockConfigStore) Load() (controlapi.RedactedConfig, error) {
	return m.cfg, nil
}
func (m *mockConfigStore) Apply(patch controlapi.ConfigPatch) (bool, error) {
	m.lastPatch = &patch
	return m.restartRequired, m.applyErr
}

// recordingStore records SetPolicy calls for assertion in ④b tests.
type recordingStore struct {
	projects       []controlapi.ProjectPolicy
	setPolicyCalls []struct {
		Project string
		Policy  controlapi.Policy
	}
}

func (m *recordingStore) ListProjectsWithPolicy() ([]controlapi.ProjectPolicy, error) {
	return m.projects, nil
}
func (m *recordingStore) SetPolicy(project string, p controlapi.Policy) error {
	m.setPolicyCalls = append(m.setPolicyCalls, struct {
		Project string
		Policy  controlapi.Policy
	}{project, p})
	return nil
}
func (m *recordingStore) GetPolicy(project string) (controlapi.Policy, error) {
	return controlapi.PolicySynced, nil
}

// newTestServer builds a fresh httptest.Server with a real webui.Mount.
// Mount creates a PER-INSTANCE session store, so every test server is fully
// isolated - a cookie minted by one test cannot authenticate against another.
func newTestServer(t *testing.T, secret string, status controlapi.Status, projects []controlapi.ProjectPolicy) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl:    &mockSyncCtrl{status: status},
		Store:       &mockStore{projects: projects},
		ConfigStore: &mockConfigStore{},
		Secret:      secret,
		Port:        7700,
		Version:     "0.1.0-test",
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
// the embedded FS (htmx.min.js, styles.css, layout, status, projects, config templates).
func TestEmbed_AllFilesPresent(t *testing.T) {
	required := []string{
		"static/htmx.min.js",
		"static/styles.css",
		"templates/layout.html",
		"templates/status.html",
		"templates/status_partial.html",
		"templates/projects.html",
		"templates/projects_rows.html",
		"templates/config.html",
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

	for _, path := range []string{"/ui/", "/ui/static/styles.css", "/ui/static/htmx.min.js"} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		resp.Body.Close()
		// Static assets must actually SERVE (200) — headers alone would also
		// appear on a 404 from a broken sub-FS mount.
		if strings.Contains(path, "/static/") && resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 (static sub-FS broken?)", path, resp.StatusCode)
		}
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

// ── PR-④b: mutating route tests ──────────────────────────────────────────────

// authenticatedPost performs a full auth flow (exchange → get both cookies) and
// then POSTs to path with the CSRF token in the X-CSRF-Token header.
// Returns the response for the caller to inspect.
func authenticatedPost(t *testing.T, srv *httptest.Server, secret, path string, extraCookies ...*http.Cookie) *http.Response {
	t.Helper()

	sessionCookie, csrfCookie := exchangeGetBothCookies(t, srv, secret)

	req, err := http.NewRequest(http.MethodPost, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("authenticatedPost: new request: %v", err)
	}
	req.AddCookie(sessionCookie)
	req.AddCookie(csrfCookie)
	req.Header.Set("X-CSRF-Token", csrfCookie.Value)
	for _, c := range extraCookies {
		req.AddCookie(c)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authenticatedPost %s: %v", path, err)
	}
	return resp
}

// authenticatedPostForm POSTs form data with valid session + CSRF cookies.
func authenticatedPostForm(t *testing.T, srv *httptest.Server, secret, path string, form url.Values) *http.Response {
	t.Helper()

	sessionCookie, csrfCookie := exchangeGetBothCookies(t, srv, secret)

	// Add CSRF token to form values.
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf_token", csrfCookie.Value)

	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("authenticatedPostForm: new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionCookie)
	req.AddCookie(csrfCookie)
	req.Header.Set("X-CSRF-Token", csrfCookie.Value)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authenticatedPostForm %s: %v", path, err)
	}
	return resp
}

// TestWebUI_PolicyToggle_POST_Valid_200 verifies that a valid policy toggle POST
// calls SetPolicy with the correct arguments and returns an HTML partial.
func TestWebUI_PolicyToggle_POST_Valid_200(t *testing.T) {
	const secret = "policy-toggle-tok"
	projects := []controlapi.ProjectPolicy{
		{Name: "myproject", Policy: controlapi.PolicySynced},
	}
	store := &recordingStore{projects: projects}
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl:    &mockSyncCtrl{},
		Store:       store,
		ConfigStore: &mockConfigStore{},
		Secret:      secret,
		Port:        7700,
		Version:     "0.1.0-test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp := authenticatedPostForm(t, srv, secret, "/ui/projects/myproject/policy", url.Values{
		"policy": []string{"local-only"},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}

	// Verify SetPolicy was called with the correct args.
	if len(store.setPolicyCalls) != 1 {
		t.Fatalf("want 1 SetPolicy call, got %d", len(store.setPolicyCalls))
	}
	call := store.setPolicyCalls[0]
	if call.Project != "myproject" {
		t.Errorf("SetPolicy project = %q, want %q", call.Project, "myproject")
	}
	if call.Policy != controlapi.PolicyLocalOnly {
		t.Errorf("SetPolicy policy = %q, want %q", call.Policy, controlapi.PolicyLocalOnly)
	}

	// Response must be HTML.
	body, _ := io.ReadAll(resp.Body)
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(string(body), "myproject") {
		t.Error("response body does not contain the project name")
	}
}

// TestWebUI_PolicyToggle_POST_CSRFMissing_403 verifies that a policy toggle POST
// without CSRF returns 403 and SetPolicy is NOT called.
func TestWebUI_PolicyToggle_POST_CSRFMissing_403(t *testing.T) {
	const secret = "policy-csrf-missing-tok"
	store := &recordingStore{}
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl:    &mockSyncCtrl{},
		Store:       store,
		ConfigStore: &mockConfigStore{},
		Secret:      secret,
		Port:        7700,
		Version:     "0.1.0-test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Get session cookie but do NOT include CSRF.
	sessionCookie := exchangeGetSessionCookie(t, srv, secret)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/ui/projects/myproject/policy", nil)
	req.AddCookie(sessionCookie)
	// NO CSRF cookie or header.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("want 403 (CSRF missing), got %d", resp.StatusCode)
	}
	if len(store.setPolicyCalls) != 0 {
		t.Error("SetPolicy must NOT be called when CSRF is missing")
	}
}

// TestWebUI_NoSession_MutatingRoute_401 verifies that mutating routes return
// 401 when no session cookie is present.
func TestWebUI_NoSession_MutatingRoute_401(t *testing.T) {
	srv := newTestServer(t, "tok", controlapi.Status{}, nil)

	for _, path := range []string{
		"/ui/projects/myproject/policy",
		"/ui/config",
		"/ui/sync",
		"/ui/connect",
		"/ui/disconnect",
	} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("POST %s without session: want 401, got %d", path, resp.StatusCode)
		}
	}
}

// TestWebUI_Origin_WrongOrigin_403 verifies that a POST with a mismatched
// Origin header returns 403 even with a valid session and CSRF.
func TestWebUI_Origin_WrongOrigin_403(t *testing.T) {
	const secret = "origin-check-tok"
	centralURL := "http://central.test"
	ctrl := &recordingSyncCtrl{
		status: controlapi.Status{
			CentralConnected: true,
			CentralURL:       &centralURL,
		},
	}
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl:    ctrl,
		Store:       &mockStore{},
		ConfigStore: &mockConfigStore{},
		Secret:      secret,
		Port:        7700,
		Version:     "0.1.0-test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sessionCookie, csrfCookie := exchangeGetBothCookies(t, srv, secret)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/ui/sync", nil)
	req.AddCookie(sessionCookie)
	req.AddCookie(csrfCookie)
	req.Header.Set("X-CSRF-Token", csrfCookie.Value)
	// Set a wrong Origin — this should be rejected.
	req.Header.Set("Origin", "http://evil.example.com")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("want 403 (wrong origin), got %d", resp.StatusCode)
	}
	if ctrl.triggerNowCalled {
		t.Error("TriggerNow must NOT be called when Origin is rejected")
	}
}

// TestWebUI_Sync_POST_Connected_202 verifies that POST /ui/sync with valid auth
// calls TriggerNow when central is connected and returns a 202-equivalent response.
func TestWebUI_Sync_POST_Connected_202(t *testing.T) {
	const secret = "sync-connected-tok"
	centralURL := "http://central.test"
	ctrl := &recordingSyncCtrl{
		status: controlapi.Status{
			CentralConnected: true,
			CentralURL:       &centralURL,
		},
	}
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl:    ctrl,
		Store:       &mockStore{},
		ConfigStore: &mockConfigStore{},
		Secret:      secret,
		Port:        7700,
		Version:     "0.1.0-test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp := authenticatedPost(t, srv, secret, "/ui/sync")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("want 202, got %d: %s", resp.StatusCode, body)
	}
	if !ctrl.triggerNowCalled {
		t.Error("TriggerNow must be called on sync POST when connected")
	}
}

// TestWebUI_Sync_POST_Disconnected_409 verifies that POST /ui/sync returns
// 409 when central is not connected and TriggerNow is NOT called.
func TestWebUI_Sync_POST_Disconnected_409(t *testing.T) {
	const secret = "sync-disconnected-tok"
	ctrl := &recordingSyncCtrl{
		status: controlapi.Status{CentralConnected: false},
	}
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl:    ctrl,
		Store:       &mockStore{},
		ConfigStore: &mockConfigStore{},
		Secret:      secret,
		Port:        7700,
		Version:     "0.1.0-test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp := authenticatedPost(t, srv, secret, "/ui/sync")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("want 409, got %d: %s", resp.StatusCode, body)
	}
	if ctrl.triggerNowCalled {
		t.Error("TriggerNow must NOT be called when central is disconnected")
	}
}

// TestWebUI_Disconnect_POST_Calls_Disconnect verifies POST /ui/disconnect calls
// SyncController.Disconnect and returns HTML.
func TestWebUI_Disconnect_POST_Calls_Disconnect(t *testing.T) {
	const secret = "disconnect-tok"
	centralURL := "http://central.test"
	ctrl := &recordingSyncCtrl{
		status: controlapi.Status{CentralConnected: true, CentralURL: &centralURL},
	}
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl:    ctrl,
		Store:       &mockStore{},
		ConfigStore: &mockConfigStore{},
		Secret:      secret,
		Port:        7700,
		Version:     "0.1.0-test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp := authenticatedPost(t, srv, secret, "/ui/disconnect")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
	if !ctrl.disconnectCalled {
		t.Error("Disconnect must be called on POST /ui/disconnect")
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

// TestWebUI_Connect_POST_Happy verifies POST /ui/connect calls Reconnect with
// the correct CentralConfig fields and writer_key is NOT echoed in the response.
func TestWebUI_Connect_POST_Happy(t *testing.T) {
	const secret = "connect-happy-tok"
	ctrl := &recordingSyncCtrl{
		status: controlapi.Status{CentralConnected: false},
	}
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl:    ctrl,
		Store:       &mockStore{},
		ConfigStore: &mockConfigStore{},
		Secret:      secret,
		Port:        7700,
		Version:     "0.1.0-test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	const theKey = "aaabbbcccdddeee000111222333444555666777888999aaabbbcccdddeee00011"
	resp := authenticatedPostForm(t, srv, secret, "/ui/connect", url.Values{
		"central_url": []string{"http://central.example.com"},
		"writer_id":   []string{"my-laptop"},
		"writer_key":  []string{theKey},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}

	// Reconnect must have been called with the right args.
	if ctrl.reconnectCfg == nil {
		t.Fatal("Reconnect was not called")
	}
	if ctrl.reconnectCfg.URL != "http://central.example.com" {
		t.Errorf("Reconnect URL = %q, want http://central.example.com", ctrl.reconnectCfg.URL)
	}
	if ctrl.reconnectCfg.WriterID != "my-laptop" {
		t.Errorf("Reconnect WriterID = %q, want my-laptop", ctrl.reconnectCfg.WriterID)
	}
	if ctrl.reconnectCfg.WriterKeyPlaintext != theKey {
		t.Errorf("Reconnect WriterKeyPlaintext = %q, want the submitted key", ctrl.reconnectCfg.WriterKeyPlaintext)
	}

	// writer_key must NOT appear anywhere in the response body.
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), theKey) {
		t.Error("writer_key must NOT appear in the response body")
	}
}

// TestWebUI_Connect_POST_CredentialValidationError verifies that
// ErrCredentialValidation from Reconnect returns a friendly error message and
// does NOT echo writer_key anywhere in the response.
func TestWebUI_Connect_POST_CredentialValidationError(t *testing.T) {
	const secret = "connect-cred-err-tok"
	ctrl := &recordingSyncCtrl{
		status:       controlapi.Status{CentralConnected: false},
		reconnectErr: controlapi.ErrCredentialValidation,
	}
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl:    ctrl,
		Store:       &mockStore{},
		ConfigStore: &mockConfigStore{},
		Secret:      secret,
		Port:        7700,
		Version:     "0.1.0-test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	const theKey = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	resp := authenticatedPostForm(t, srv, secret, "/ui/connect", url.Values{
		"central_url": []string{"http://central.example.com"},
		"writer_id":   []string{"my-id"},
		"writer_key":  []string{theKey},
	})
	defer resp.Body.Close()

	// 422 on credential failure.
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("want 422, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// Friendly error message must appear.
	if !strings.Contains(bodyStr, "Credential validation failed") {
		t.Error("response must contain friendly error message for ErrCredentialValidation")
	}

	// writer_key must NOT appear in the response body under ANY circumstances.
	if strings.Contains(bodyStr, theKey) {
		t.Error("writer_key MUST NOT appear in the response body on error")
	}
	// Also check the literal string "writer_key" is not in a value= attribute
	// that could reveal the key (form should not be re-rendered with the key).
	if strings.Contains(bodyStr, `type="password"`) {
		// If a password input is rendered, its value= must be empty.
		if strings.Contains(bodyStr, `value="`+theKey+`"`) {
			t.Error("password input must not have value= containing the writer key")
		}
	}
}

// TestWebUI_Connect_POST_InvalidWriterKeyError verifies ErrInvalidWriterKey
// renders a friendly error and writer_key is not echoed.
func TestWebUI_Connect_POST_InvalidWriterKeyError(t *testing.T) {
	const secret = "connect-inv-key-tok"
	ctrl := &recordingSyncCtrl{
		status:       controlapi.Status{CentralConnected: false},
		reconnectErr: controlapi.ErrInvalidWriterKey,
	}
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl:    ctrl,
		Store:       &mockStore{},
		ConfigStore: &mockConfigStore{},
		Secret:      secret,
		Port:        7700,
		Version:     "0.1.0-test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	const badKey = "tooshort"
	resp := authenticatedPostForm(t, srv, secret, "/ui/connect", url.Values{
		"central_url": []string{"http://central.example.com"},
		"writer_id":   []string{"my-id"},
		"writer_key":  []string{badKey},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("want 422, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	if !strings.Contains(bodyStr, "Invalid writer key") {
		t.Error("response must contain friendly error for ErrInvalidWriterKey")
	}
	if strings.Contains(bodyStr, badKey) {
		t.Error("writer_key must NOT appear in the response body")
	}
}

// TestWebUI_Config_POST_Valid_200 verifies that POST /ui/config with a valid
// sync_interval calls ConfigStore.Apply and returns the updated config page.
func TestWebUI_Config_POST_Valid_200(t *testing.T) {
	const secret = "config-post-tok"
	cfgStore := &mockConfigStore{}
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl:    &mockSyncCtrl{},
		Store:       &mockStore{},
		ConfigStore: cfgStore,
		Secret:      secret,
		Port:        7700,
		Version:     "0.1.0-test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp := authenticatedPostForm(t, srv, secret, "/ui/config", url.Values{
		"sync_interval": []string{"2m"},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
	if cfgStore.lastPatch == nil {
		t.Fatal("ConfigStore.Apply must be called")
	}
	if cfgStore.lastPatch.SyncInterval == nil || *cfgStore.lastPatch.SyncInterval != "2m" {
		t.Errorf("Apply patch SyncInterval = %v, want 2m", cfgStore.lastPatch.SyncInterval)
	}
}

// TestWebUI_Config_POST_InvalidDuration_400 verifies that an unparseable
// sync_interval returns a 200 with an error message (no ConfigStore.Apply call).
func TestWebUI_Config_POST_InvalidDuration_400(t *testing.T) {
	const secret = "config-bad-dur-tok"
	cfgStore := &mockConfigStore{}
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl:    &mockSyncCtrl{},
		Store:       &mockStore{},
		ConfigStore: cfgStore,
		Secret:      secret,
		Port:        7700,
		Version:     "0.1.0-test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp := authenticatedPostForm(t, srv, secret, "/ui/config", url.Values{
		"sync_interval": []string{"not-a-duration"},
	})
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// We return the page with the error message rather than a 4xx HTTP status
	// (the form page re-renders with the error inline — HTMX swap pattern).
	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200 (form re-rendered with error), got %d", resp.StatusCode)
	}
	if !strings.Contains(bodyStr, "invalid sync_interval") {
		t.Errorf("response must contain error text for bad duration, got: %s", bodyStr)
	}
	if cfgStore.lastPatch != nil {
		t.Error("ConfigStore.Apply must NOT be called for an invalid duration")
	}
}

// TestWebUI_Config_GET_DoesNotEchoWriterKey verifies that GET /ui/config
// renders the config form without exposing writer key values.
func TestWebUI_Config_GET_DoesNotEchoWriterKey(t *testing.T) {
	const secret = "config-get-key-tok"
	redactedKey := "***REDACTED***"
	cfgStore := &mockConfigStore{
		cfg: controlapi.RedactedConfig{
			WriterKey:    &redactedKey,
			SyncInterval: "30s",
		},
	}
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl:    &mockSyncCtrl{},
		Store:       &mockStore{},
		ConfigStore: cfgStore,
		Secret:      secret,
		Port:        7700,
		Version:     "0.1.0-test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	body := authenticatedGet(t, srv, secret, "/ui/config")

	// The page must show REDACTED — not the actual key.
	if !strings.Contains(body, "REDACTED") {
		t.Error("config page must show REDACTED for the writer key")
	}
	// The actual "***REDACTED***" sentinel is fine; a real key must never appear.
	// Since our mock returns "***REDACTED***" there is nothing more sensitive here
	// to check — the guarantee is structural: WriterKeyPlaintext never flows to Load().
}
