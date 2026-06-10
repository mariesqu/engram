//go:build acceptance

package webui_test

import (
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

// newFullTestServer builds a test server with BOTH the control API handler and
// the web UI mounted on the same mux — mirroring the production daemon topology.
// This is used in acceptance tests that need to prove isolation between
// /api/* (bearer-only) and /ui/* (cookie-only).
func newFullTestServer(t *testing.T, secret string, status controlapi.Status, projects []controlapi.ProjectPolicy) *httptest.Server {
	t.Helper()

	store := &mockStore{projects: projects}
	syncCtrl := &mockSyncCtrl{status: status}
	cfgStore := &mockCfgStore{}

	ctrlSrv := controlapi.New(secret, 7700, store, syncCtrl, cfgStore, "0.1.0-test")

	mux := http.NewServeMux()
	mux.Handle("/api/", ctrlSrv.Handler())
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl:    syncCtrl,
		Store:       store,
		ConfigStore: cfgStore,
		Secret:      secret,
		Port:        7700,
		Version:     "0.1.0-test",
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// mockCfgStore satisfies controlapi.ConfigStore for acceptance tests.
type mockCfgStore struct{}

func (m *mockCfgStore) Load() (controlapi.RedactedConfig, error) {
	return controlapi.RedactedConfig{}, nil
}
func (m *mockCfgStore) Apply(_ controlapi.ConfigPatch) (bool, error) {
	return false, nil
}

// TestAcceptance_WebUI_TokenExchangeFlow verifies the full exchange flow:
// GET /ui/?token=GOOD → Set-Cookie + 302 to /ui/ without token.
func TestAcceptance_WebUI_TokenExchangeFlow(t *testing.T) {
	const secret = "acc-exchange-tok"
	srv := newFullTestServer(t, secret, controlapi.Status{DaemonVersion: "0.1.0"}, nil)

	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(srv.URL + "/ui/?token=" + secret)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/" {
		t.Errorf("want Location /ui/, got %q", loc)
	}
	var sessFound bool
	for _, c := range resp.Cookies() {
		if c.Name == "engram_session" {
			sessFound = true
			if !c.HttpOnly {
				t.Error("cookie must be HttpOnly")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Error("cookie must be SameSite=Strict")
			}
			if c.Secure {
				t.Error("cookie must NOT be Secure on loopback")
			}
		}
	}
	if !sessFound {
		t.Error("no engram_session cookie in response")
	}
}

// TestAcceptance_WebUI_BadToken_401 verifies a bad token returns 401.
func TestAcceptance_WebUI_BadToken_401(t *testing.T) {
	srv := newFullTestServer(t, "correct", controlapi.Status{}, nil)
	resp, err := http.Get(srv.URL + "/ui/?token=wrong")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
}

// TestAcceptance_WebUI_CookieAuthedStatus_200 verifies cookie-authenticated
// GET /ui/ returns 200 HTML with status markers from the mock SyncController.
func TestAcceptance_WebUI_CookieAuthedStatus_200(t *testing.T) {
	const secret = "acc-status-tok"
	centralURL := "http://central.acc.test"
	now := time.Now().UTC()
	status := controlapi.Status{
		CentralConnected: true,
		CentralURL:       &centralURL,
		LastSyncResult:   controlapi.SyncResult{At: &now, Pushed: 5, Pulled: 2},
		DaemonVersion:    "0.1.0",
	}
	srv := newFullTestServer(t, secret, status, nil)
	body := authenticatedGet(t, srv, secret, "/ui/")

	if !strings.Contains(body, "connected") {
		t.Error("status page must show connected state")
	}
	if !strings.Contains(body, "0.1.0") {
		t.Error("status page must show daemon version")
	}
}

// TestAcceptance_WebUI_ProjectsPageRendersPolicies verifies the projects page
// renders policy badges from the mock store.
func TestAcceptance_WebUI_ProjectsPageRendersPolicies(t *testing.T) {
	const secret = "acc-proj-tok"
	projects := []controlapi.ProjectPolicy{
		{Name: "myproject", Policy: controlapi.PolicySynced},
		{Name: "localproj", Policy: controlapi.PolicyLocalOnly},
	}
	srv := newFullTestServer(t, secret, controlapi.Status{DaemonVersion: "0.1.0"}, projects)
	body := authenticatedGet(t, srv, secret, "/ui/projects")

	if !strings.Contains(body, "myproject") {
		t.Error("projects page must render project names")
	}
	if !strings.Contains(body, "badge-synced") {
		t.Error("projects page must render synced badge")
	}
	if !strings.Contains(body, "badge-local-only") {
		t.Error("projects page must render local-only badge")
	}
}

// TestAcceptance_WebUI_CookieDoesNotAuthControlAPI is the critical isolation
// proof: a request to /api/v1/status carrying a valid session cookie but NO
// bearer token must return 401. The cookie scope is /ui/ only.
func TestAcceptance_WebUI_CookieDoesNotAuthControlAPI(t *testing.T) {
	const secret = "acc-isolation-tok"
	srv := newFullTestServer(t, secret, controlapi.Status{DaemonVersion: "0.1.0"}, nil)

	// Exchange to obtain a real session cookie.
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

	// Request /api/v1/status with the cookie jar but NO bearer header.
	cookieClient := &http.Client{Jar: jar}
	resp2, err := cookieClient.Get(srv.URL + "/api/v1/status")
	if err != nil {
		t.Fatalf("GET /api/v1/status: %v", err)
	}
	defer resp2.Body.Close()
	body, _ := io.ReadAll(resp2.Body)

	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf(
			"cookie MUST NOT auth /api/v1/status — want 401, got %d (body: %s)",
			resp2.StatusCode, body,
		)
	}
}

// TestAcceptance_WebUI_OfflineAssets verifies rendered pages have no external
// asset references (no http:// or https:// in src= or href= attributes).
func TestAcceptance_WebUI_OfflineAssets(t *testing.T) {
	const secret = "acc-offline-tok"
	srv := newFullTestServer(t, secret, controlapi.Status{DaemonVersion: "0.1.0"}, nil)

	paths := []string{"/ui/", "/ui/projects", "/ui/status", "/ui/config"}
	for _, p := range paths {
		body := authenticatedGet(t, srv, secret, p)
		for _, ext := range []string{
			`src="http://`, `src="https://`,
			`href="http://`, `href="https://`,
		} {
			if strings.Contains(body, ext) {
				t.Errorf("page %s has external ref: %q", p, ext)
			}
		}
	}
}

// ── PR-④b acceptance tests ───────────────────────────────────────────────────

// accAuthPost performs a token exchange and POSTs to path with valid session +
// CSRF cookies. Returns the response body as string.
func accAuthPost(t *testing.T, srv *httptest.Server, secret, path string, form map[string]string) (int, string) {
	t.Helper()

	sessionCookie, csrfCookie := exchangeGetBothCookies(t, srv, secret)

	vals := url.Values{}
	vals.Set("csrf_token", csrfCookie.Value)
	for k, v := range form {
		vals.Set(k, v)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(vals.Encode()))
	if err != nil {
		t.Fatalf("accAuthPost: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrfCookie.Value)
	req.AddCookie(sessionCookie)
	req.AddCookie(csrfCookie)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("accAuthPost %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestAcceptance_WebUI_PolicyToggle_MutatesAndReturnsPartial verifies the full
// policy toggle flow: POST with session + CSRF → SetPolicy called → updated
// projects rows returned as HTML partial.
func TestAcceptance_WebUI_PolicyToggle_MutatesAndReturnsPartial(t *testing.T) {
	const secret = "acc-policy-tog-tok"
	projects := []controlapi.ProjectPolicy{
		{Name: "alpha", Policy: controlapi.PolicySynced},
		{Name: "beta", Policy: controlapi.PolicySynced},
	}
	srv := newFullTestServer(t, secret, controlapi.Status{DaemonVersion: "0.1.0"}, projects)

	status, body := accAuthPost(t, srv, secret, "/ui/projects/alpha/policy", map[string]string{
		"policy": "local-only",
	})

	if status != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", status, body)
	}
	if !strings.Contains(body, "alpha") {
		t.Error("response partial must contain the project name")
	}
	if !strings.Contains(body, "beta") {
		t.Error("response partial must contain all projects (refreshed list)")
	}
}

// TestAcceptance_WebUI_CSRFRejection_MutatingRoutes verifies CSRF rejection on
// all mutating routes — 403 with no effect.
func TestAcceptance_WebUI_CSRFRejection_MutatingRoutes(t *testing.T) {
	const secret = "acc-csrf-reject-tok"
	srv := newFullTestServer(t, secret, controlapi.Status{DaemonVersion: "0.1.0"}, nil)

	sessionCookie := exchangeGetSessionCookie(t, srv, secret)

	mutatingPaths := []string{
		"/ui/projects/myproj/policy",
		"/ui/config",
		"/ui/sync",
		"/ui/connect",
		"/ui/disconnect",
	}
	for _, path := range mutatingPaths {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		req.AddCookie(sessionCookie)
		// NO CSRF cookie or header.

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s without CSRF: want 403, got %d", path, resp.StatusCode)
		}
	}
}

// TestAcceptance_WebUI_OriginRejection verifies that a POST with a mismatched
// Origin is rejected 403 even with valid session + CSRF.
func TestAcceptance_WebUI_OriginRejection(t *testing.T) {
	const secret = "acc-origin-rej-tok"
	centralURL := "http://central.test"
	syncCtrl := &mockSyncCtrl{
		status: controlapi.Status{
			CentralConnected: true,
			CentralURL:       &centralURL,
		},
	}
	store := &mockStore{}
	cfgStore := &mockCfgStore{}

	ctrlSrv := controlapi.New(secret, 7700, store, syncCtrl, cfgStore, "0.1.0-test")
	mux := http.NewServeMux()
	mux.Handle("/api/", ctrlSrv.Handler())
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl:    syncCtrl,
		Store:       store,
		ConfigStore: cfgStore,
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
	req.Header.Set("Origin", "http://evil.example.com") // mismatched origin

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /ui/sync: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("mismatched origin: want 403, got %d", resp.StatusCode)
	}
}

// TestAcceptance_WebUI_ConnectForm_WriterKeyNotEchoed verifies that the
// connect form does not echo writer_key in any response — even on error.
func TestAcceptance_WebUI_ConnectForm_WriterKeyNotEchoed(t *testing.T) {
	const secret = "acc-no-echo-tok"
	srv := newFullTestServer(t, secret, controlapi.Status{DaemonVersion: "0.1.0"}, nil)

	const theKey = "aaabbbcccdddeee000111222333444555666777888999aaabbbcccdddeee00011"
	// On the full test server the mockSyncCtrl.Reconnect returns nil (success).
	// The response should be the status partial (200) with no key echoed.
	status, body := accAuthPost(t, srv, secret, "/ui/connect", map[string]string{
		"central_url": "http://central.example.com",
		"writer_id":   "my-laptop",
		"writer_key":  theKey,
	})

	// Whether success or error, the key must never appear in the response.
	if strings.Contains(body, theKey) {
		t.Errorf("writer_key appeared in response body (status %d)", status)
	}
}

// TestAcceptance_WebUI_ExchangeSetsCSRFCookie verifies that the token exchange
// sets BOTH the session and CSRF cookies with correct attributes, and that the
// CSRF cookie is NOT HttpOnly (double-submit pattern requires it accessible).
func TestAcceptance_WebUI_ExchangeSetsCSRFCookie(t *testing.T) {
	const secret = "acc-csrf-cookie-tok"
	srv := newFullTestServer(t, secret, controlapi.Status{DaemonVersion: "0.1.0"}, nil)

	client := noRedirectClient()
	resp, err := client.Get(srv.URL + "/ui/?token=" + secret)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", resp.StatusCode)
	}

	var sessionFound, csrfFound bool
	for _, c := range resp.Cookies() {
		switch c.Name {
		case "engram_session":
			sessionFound = true
			if !c.HttpOnly {
				t.Error("session cookie must be HttpOnly")
			}
		case "engram_csrf":
			csrfFound = true
			if c.HttpOnly {
				t.Error("CSRF cookie must NOT be HttpOnly — double-submit pattern requires it accessible")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Errorf("CSRF cookie SameSite = %v, want Strict", c.SameSite)
			}
			if c.Path != "/ui/" {
				t.Errorf("CSRF cookie Path = %q, want /ui/", c.Path)
			}
		}
	}
	if !sessionFound {
		t.Error("exchange must set engram_session cookie")
	}
	if !csrfFound {
		t.Error("exchange must set engram_csrf cookie")
	}
}
