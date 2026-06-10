package webui_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/webui"
)

// ── Task 4b.2: CSRF unit tests ───────────────────────────────────────────────

// TestCSRF_CookieAttributes verifies that the CSRF cookie set during the token
// exchange has the required attributes:
//   - NOT HttpOnly (must be readable for the double-submit pattern)
//   - SameSite=Strict
//   - Path=/ui/
//   - NOT Secure (loopback)
func TestCSRF_CookieAttributes(t *testing.T) {
	const secret = "csrf-attr-tok"
	srv := newTestServer(t, secret, controlapi.Status{DaemonVersion: "0.1.0"}, nil)

	client := noRedirectClient()
	resp, err := client.Get(srv.URL + "/ui/?token=" + secret)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", resp.StatusCode)
	}

	var csrfCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "engram_csrf" {
			csrfCookie = c
		}
	}
	if csrfCookie == nil {
		t.Fatal("no engram_csrf cookie in exchange response")
	}
	if csrfCookie.HttpOnly {
		t.Error("CSRF cookie must NOT be HttpOnly — the double-submit pattern requires it to be readable")
	}
	if csrfCookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("CSRF cookie SameSite = %v, want Strict", csrfCookie.SameSite)
	}
	if csrfCookie.Path != "/ui/" {
		t.Errorf("CSRF cookie Path = %q, want /ui/", csrfCookie.Path)
	}
	if csrfCookie.Secure {
		t.Error("CSRF cookie must NOT be Secure (loopback, no TLS)")
	}
	if csrfCookie.MaxAge <= 0 {
		t.Error("CSRF cookie MaxAge should be positive")
	}
}

// TestCSRF_TokenIsPerSession verifies that the CSRF token is a fresh random
// value set at exchange time (not empty, not the bearer token).
func TestCSRF_TokenIsPerSession(t *testing.T) {
	const secret = "csrf-per-sess-tok"
	srv := newTestServer(t, secret, controlapi.Status{}, nil)
	client := noRedirectClient()

	resp, err := client.Get(srv.URL + "/ui/?token=" + secret)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	defer resp.Body.Close()

	var csrfVal string
	for _, c := range resp.Cookies() {
		if c.Name == "engram_csrf" {
			csrfVal = c.Value
		}
	}
	if csrfVal == "" {
		t.Fatal("no CSRF cookie value after exchange")
	}
	if csrfVal == secret {
		t.Error("CSRF token must not equal the bearer token")
	}
	// 16 random bytes → 32 hex chars.
	if len(csrfVal) != 32 {
		t.Errorf("CSRF token length = %d, want 32 hex chars (16 random bytes)", len(csrfVal))
	}
}

// TestCSRF_MissingCookie_Rejected verifies that a POST without the CSRF cookie
// returns 403 and the handler is NOT called.
func TestCSRF_MissingCookie_Rejected(t *testing.T) {
	const secret = "csrf-missing-tok"
	ctrl := &recordingSyncCtrl{}
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl: ctrl,
		Store:    &mockStore{},
		Secret:   secret,
		Port:     7700,
		Version:  "0.1.0-test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Get a valid session cookie but strip the CSRF cookie.
	sessionCookie := exchangeGetSessionCookie(t, srv, secret)

	// POST /ui/sync with session cookie only — NO CSRF cookie.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/ui/sync", nil)
	req.AddCookie(sessionCookie)
	// Deliberately NO engram_csrf cookie.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /ui/sync: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("want 403 (CSRF missing), got %d", resp.StatusCode)
	}
	if ctrl.triggerNowCalled {
		t.Error("TriggerNow must NOT be called when CSRF is missing")
	}
}

// TestCSRF_WrongValue_Rejected verifies that a POST with the CSRF cookie but
// a mismatched token in the form field returns 403.
func TestCSRF_WrongValue_Rejected(t *testing.T) {
	const secret = "csrf-wrong-tok"
	ctrl := &recordingSyncCtrl{}
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl: ctrl,
		Store:    &mockStore{},
		Secret:   secret,
		Port:     7700,
		Version:  "0.1.0-test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sessionCookie, csrfCookie := exchangeGetBothCookies(t, srv, secret)

	// POST with CSRF cookie but wrong token value in the header.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/ui/sync", nil)
	req.AddCookie(sessionCookie)
	req.AddCookie(csrfCookie)
	req.Header.Set("X-CSRF-Token", "deadbeef-wrong-value")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("want 403 (CSRF mismatch), got %d", resp.StatusCode)
	}
	if ctrl.triggerNowCalled {
		t.Error("TriggerNow must NOT be called when CSRF token mismatches")
	}
}

// TestCSRF_Valid_Passes verifies that a POST with a matching CSRF cookie and
// header token is accepted (200/202).
func TestCSRF_Valid_Passes(t *testing.T) {
	const secret = "csrf-valid-tok"
	centralURL := "http://central.test"
	ctrl := &recordingSyncCtrl{
		status: controlapi.Status{
			CentralConnected: true,
			CentralURL:       &centralURL,
		},
	}
	mux := http.NewServeMux()
	webui.Mount(mux, webui.WebUIDeps{
		SyncCtrl: ctrl,
		Store:    &mockStore{},
		Secret:   secret,
		Port:     7700,
		Version:  "0.1.0-test",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sessionCookie, csrfCookie := exchangeGetBothCookies(t, srv, secret)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/ui/sync", nil)
	req.AddCookie(sessionCookie)
	req.AddCookie(csrfCookie)
	req.Header.Set("X-CSRF-Token", csrfCookie.Value)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /ui/sync: %v", err)
	}
	defer resp.Body.Close()

	// 202 Accepted (sync triggered) or 409 Conflict depending on connected state.
	// We set CentralConnected=true so expect 202.
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("want 202 (sync triggered), got %d", resp.StatusCode)
	}
	if !ctrl.triggerNowCalled {
		t.Error("TriggerNow must be called on valid CSRF POST")
	}
}

// TestCSRF_GetRoutes_NoCSRFRequired verifies that GET routes pass through
// without needing any CSRF token.
func TestCSRF_GetRoutes_NoCSRFRequired(t *testing.T) {
	const secret = "csrf-get-tok"
	srv := newTestServer(t, secret, controlapi.Status{DaemonVersion: "0.1.0"}, nil)

	sessionCookie := exchangeGetSessionCookie(t, srv, secret)

	for _, path := range []string{"/ui/", "/ui/status", "/ui/projects", "/ui/config"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.AddCookie(sessionCookie)
		// No CSRF cookie on GET — must not cause 403.
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden {
			t.Errorf("GET %s returned 403 — CSRF must not be required for GETs", path)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: want 200, got %d", path, resp.StatusCode)
		}
	}
}

// ── Helpers for CSRF tests ────────────────────────────────────────────────────

// exchangeGetSessionCookie performs the token exchange and returns only the
// engram_session cookie.
func exchangeGetSessionCookie(t *testing.T, srv *httptest.Server, secret string) *http.Cookie {
	t.Helper()
	client := noRedirectClient()
	resp, err := client.Get(srv.URL + "/ui/?token=" + secret)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	defer resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "engram_session" {
			return c
		}
	}
	t.Fatal("no engram_session cookie after exchange")
	return nil
}

// exchangeGetBothCookies performs the token exchange and returns both the
// engram_session and engram_csrf cookies.
func exchangeGetBothCookies(t *testing.T, srv *httptest.Server, secret string) (session, csrf *http.Cookie) {
	t.Helper()
	client := noRedirectClient()
	resp, err := client.Get(srv.URL + "/ui/?token=" + secret)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	defer resp.Body.Close()
	for _, c := range resp.Cookies() {
		switch c.Name {
		case "engram_session":
			session = c
		case "engram_csrf":
			csrf = c
		}
	}
	if session == nil {
		t.Fatal("no engram_session cookie after exchange")
	}
	if csrf == nil {
		t.Fatal("no engram_csrf cookie after exchange")
	}
	return session, csrf
}

// recordingSyncCtrl records calls to TriggerNow, Disconnect, and Reconnect
// for assertion in CSRF / mutating-route tests.
type recordingSyncCtrl struct {
	status           controlapi.Status
	triggerNowCalled bool
	disconnectCalled bool
	reconnectCfg     *controlapi.CentralConfig
	reconnectErr     error
}

func (m *recordingSyncCtrl) Status() controlapi.Status {
	return m.status
}
func (m *recordingSyncCtrl) TriggerNow(_ context.Context) error {
	m.triggerNowCalled = true
	return nil
}
func (m *recordingSyncCtrl) Disconnect() error {
	m.disconnectCalled = true
	return nil
}
func (m *recordingSyncCtrl) Reconnect(cfg controlapi.CentralConfig) error {
	m.reconnectCfg = &cfg
	return m.reconnectErr
}
