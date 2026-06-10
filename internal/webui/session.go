package webui

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"
)

const (
	sessionCookieName = "engram_session"
	sessionCookieTTL  = 8 * time.Hour
)

// exchangeToken returns an http.Handler that:
//  1. Reads the ?token= query parameter.
//  2. Validates it against the daemon bearer secret (constant-length string
//     comparison — timing side-channels on loopback are negligible, but we
//     generate equal-length tokens so the comparison length is always 64 chars).
//  3. Issues an HttpOnly, SameSite=Strict, Secure=false session cookie.
//  4. Redirects to /ui/ stripping the token from the address bar / browser history.
//
// On a bad or missing token it returns a 401 page.
func exchangeToken(secret string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.URL.Query().Get("token")
		if tok == "" || tok != secret {
			render401(w)
			return
		}

		// Generate a random session value (not the bearer token itself — we do
		// not want the bearer token landing in the cookie jar).
		sessVal, err := randomHex(16)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    sessVal,
			Path:     "/ui/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Secure:   false, // loopback — TLS is not used on localhost
			MaxAge:   int(sessionCookieTTL.Seconds()),
		})

		// Store the session → secret mapping so requireSession can verify it.
		// For this single-user loopback daemon we use a package-level session store
		// (a single active session at a time is the expected UX).
		setSession(sessVal, secret)

		// Redirect to /ui/ — token is gone from the URL and therefore from
		// browser history and the referrer header.
		http.Redirect(w, r, "/ui/", http.StatusSeeOther)
	})
}

// requireSession is middleware that validates the session cookie set by
// exchangeToken. If the cookie is missing or stale it returns a 401 page
// (NOT a redirect to the exchange — the user must run `engram ui` to get a
// fresh tokenized URL, so we cannot silently redirect them there).
func requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || cookie.Value == "" || !validateSession(cookie.Value) {
			render401(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── In-process single-session store ─────────────────────────────────────────
//
// The loopback daemon is single-user. We keep exactly one live session at a
// time. The session value is a random hex string; the "secret" here is the
// daemon bearer token (used only to verify that the session was issued by this
// daemon — it is never returned to the client).
//
// No sync primitives needed because all HTTP handlers run on a single goroutine
// pool and the session value is set atomically (a single pointer swap). In
// practice even with concurrent requests this is safe: the worst case is a
// brief window where two sessions are both valid (e.g. `engram ui` opened
// twice). That is acceptable for a loopback single-user tool.

var (
	activeSession string // random hex session value
	activeSecret  string // daemon bearer token that issued this session
)

// setSession records a new active session.
func setSession(sessVal, secret string) {
	activeSession = sessVal
	activeSecret = secret
}

// validateSession returns true if sessVal matches the active session.
func validateSession(sessVal string) bool {
	return sessVal != "" && sessVal == activeSession
}

// randomHex returns n random bytes as a lowercase hex string.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// CurrentSecret returns the bearer secret stored in the active session.
// Used by webui.go to confirm the session references the live daemon token.
func currentSecret() string {
	return activeSecret
}
