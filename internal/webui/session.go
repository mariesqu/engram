package webui

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

const (
	sessionCookieName = "engram_session"
	sessionCookieTTL  = 8 * time.Hour
)

// sessionStore holds the single active UI session for ONE mounted web UI.
//
// It is created per Mount call (per daemon instance), NOT package-level:
//   - net/http serves each connection on its own goroutine, so reads and
//     writes must be synchronized (RWMutex) — package-level bare strings were
//     a data race.
//   - Two daemons in one process (the test suite does this) must not share
//     session state: a cookie minted by daemon A must never authenticate
//     against daemon B.
//
// Single-user loopback semantics: exactly one live session; a second
// `engram ui` exchange replaces the first. Expiry is enforced SERVER-side
// (issuedAt + TTL) — the cookie MaxAge alone is client-side advice a stolen
// cookie value would ignore.
//
// PR-④b addition: the session also carries a per-session CSRF token that is
// set at exchange time and rotated on every fresh exchange.
type sessionStore struct {
	mu       sync.RWMutex
	value    string // random hex session value; "" → no active session
	csrf     string // random hex CSRF token; "" → no active session
	issuedAt time.Time
}

// set records a new active session, replacing any previous one.
// It accepts the session value AND a pre-generated CSRF token so both are
// set atomically under the same lock.
func (s *sessionStore) set(sessVal, csrfVal string) {
	s.mu.Lock()
	s.value = sessVal
	s.csrf = csrfVal
	s.issuedAt = time.Now()
	s.mu.Unlock()
}

// valid reports whether val matches the live, unexpired session.
func (s *sessionStore) valid(val string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if val == "" || s.value == "" || val != s.value {
		return false
	}
	return time.Since(s.issuedAt) <= sessionCookieTTL
}

// csrfToken returns the current per-session CSRF token.
// Returns "" when no session is active — callers must only invoke this
// after requireSession has passed.
func (s *sessionStore) csrfToken() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.csrf
}

// exchangeToken returns an http.Handler that:
//  1. Reads the ?token= query parameter.
//  2. Validates it against the daemon bearer secret (equal-length hex strings;
//     timing side-channels on loopback are negligible).
//  3. Issues an HttpOnly, SameSite=Strict, Secure=false session cookie whose
//     value is a FRESH random string — the bearer token never lands in the
//     cookie jar.
//  4. Issues a per-session CSRF double-submit cookie (NOT HttpOnly) for use
//     in HTMX mutating forms.
//  5. Redirects to /ui/ stripping the token from the address bar / history.
//
// On a bad or missing token it returns a 401 page. An EMPTY configured secret
// always 401s — the exchange must never be satisfiable by an empty ?token=.
func exchangeToken(secret string, port int, sessions *sessionStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.URL.Query().Get("token")
		if secret == "" || tok == "" || tok != secret {
			render401(w)
			return
		}

		sessVal, err := randomHex(16)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		csrfVal, err := randomHex(16)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Session cookie — HttpOnly so JS cannot read it.
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    sessVal,
			Path:     "/ui/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Secure:   false, // loopback — TLS is not used on localhost
			MaxAge:   int(sessionCookieTTL.Seconds()),
		})

		// CSRF double-submit cookie — NOT HttpOnly so the template-rendered
		// hidden input value (set from the server-side csrfToken()) and the
		// cookie are both the same value. Browsers send SameSite=Strict cookies
		// only on same-site navigations, so cross-site POSTs cannot attach it.
		setCSRFCookie(w, port, csrfVal)

		sessions.set(sessVal, csrfVal)

		// Redirect to /ui/ — token is gone from the URL and therefore from
		// browser history and the referrer header.
		http.Redirect(w, r, "/ui/", http.StatusSeeOther)
	})
}

// requireSession is middleware that validates the session cookie set by
// exchangeToken. If the cookie is missing, stale, or expired it returns a 401
// page (NOT a redirect to the exchange — the user must run `engram ui` to get
// a fresh tokenized URL, so we cannot silently redirect them there).
func requireSession(sessions *sessionStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || !sessions.valid(cookie.Value) {
			render401(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// randomHex returns n random bytes as a lowercase hex string.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
