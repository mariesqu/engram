//go:build acceptance

package webui_test

import (
	"io"
	"net/http"
	"net/http/httptest"
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
		SyncCtrl: syncCtrl,
		Store:    store,
		Secret:   secret,
		Port:     7700,
		Version:  "0.1.0-test",
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

	paths := []string{"/ui/", "/ui/projects", "/ui/status"}
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
