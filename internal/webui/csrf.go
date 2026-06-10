package webui

import (
	"crypto/subtle"
	"fmt"
	"net/http"
)

const (
	csrfCookieName = "engram_csrf"
	csrfHeaderName = "X-CSRF-Token"
	csrfFieldName  = "csrf_token"
)

// setCSRFCookie issues the CSRF double-submit cookie on w.
//
// The cookie is intentionally NOT HttpOnly so that HTMX can read it and
// attach it as an hx-headers value or a form field (the template renders it
// as a hidden input whose value is the token produced by the exchange; no JS
// read of the cookie is required in our approach — the template embeds the
// value directly, but the spec requires the cookie to be non-HttpOnly as
// evidence of the double-submit pattern being browser-accessible).
//
// SameSite=Strict prevents cross-site POSTs from attaching the cookie at all,
// so the CSRF protection degrades gracefully even without JS.
func setCSRFCookie(w http.ResponseWriter, port int, val string) {
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    val,
		Path:     "/ui/",
		HttpOnly: false, // must be readable by the page (double-submit pattern)
		SameSite: http.SameSiteStrictMode,
		Secure:   false, // loopback — no TLS
		MaxAge:   int(sessionCookieTTL.Seconds()),
	})
	// Expose the token in the response header so HTMX hx-headers can read it
	// without needing client-side JS. Templates embed it directly as a hidden
	// input value, so this header is belt-and-suspenders for HTMX swap flows.
	w.Header().Set(csrfHeaderName, val)
}

// validateCSRF checks that the request carries a CSRF token that (a) matches
// the double-submit cookie AND (b) matches the SERVER-SIDE per-session token.
// It accepts the token from the X-CSRF-Token header (HTMX hx-headers) or from
// the csrf_token form field (plain HTML form POST).
//
// The session binding (b) is the load-bearing check on loopback: cookies are
// PORT-AGNOSTIC, so any other local server on 127.0.0.1:<other-port> can plant
// an engram_csrf cookie with a value it knows — pure double-submit would
// accept it. Binding to the token the EXCHANGE minted (stored in the session
// store, rotated with every exchange) defeats cookie planting outright.
//
// Constant-time comparison is used so even on a loopback interface we do not
// introduce a timing oracle.
func validateCSRF(r *http.Request, sessions *sessionStore) bool {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	cookieVal := cookie.Value

	// Prefer the request header (HTMX sets it); fall back to form field.
	tokenVal := r.Header.Get(csrfHeaderName)
	if tokenVal == "" {
		// ParseForm is idempotent; if the body was already parsed this is a no-op.
		_ = r.ParseForm()
		tokenVal = r.FormValue(csrfFieldName)
	}

	if tokenVal == "" {
		return false
	}
	if !constantTimeEq(cookieVal, tokenVal) {
		return false
	}
	// Session binding: the submitted token must be the one THIS session's
	// exchange minted. A planted cookie carries a token the exchange never
	// issued (or a stale one from a rotated-away session) and fails here.
	serverTok := sessions.csrfToken()
	if serverTok == "" {
		return false
	}
	return constantTimeEq(tokenVal, serverTok)
}

// constantTimeEq compares two strings in constant time (length leak aside —
// both sides are fixed-length hex in practice).
func constantTimeEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// withCSRF is middleware that validates the CSRF double-submit cookie on
// mutating requests. Non-POST methods pass through without CSRF validation
// (CSRF is a POST/mutation concern; GET requests are safe).
//
// On failure it returns 403 Forbidden. The handler is not called.
//
// withOrigin is a SEPARATE guard (applied by the routeUI dispatcher for
// mutating routes) that validates the Origin header. The two guards are
// independent layers: withCSRF handles the session-bound double-submit; the
// origin check in dispatchUI handles the browser Origin header.
func withCSRF(sessions *sessionStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut ||
			r.Method == http.MethodPatch || r.Method == http.MethodDelete {
			if !validateCSRF(r, sessions) {
				http.Error(w, "CSRF validation failed", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// checkOriginForPost validates the Origin header for POST requests within the
// webui. It mirrors the controlapi withOrigin contract exactly:
//   - Origin ABSENT  → allow (non-browser clients do not send Origin)
//   - Origin PRESENT → must equal http://127.0.0.1:<port>, else 403
//
// This is applied in addition to CSRF double-submit for defense in depth.
func checkOriginForPost(r *http.Request, port int) bool {
	if r.Method != http.MethodPost && r.Method != http.MethodPut &&
		r.Method != http.MethodPatch && r.Method != http.MethodDelete {
		return true // non-mutating methods exempt
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // absent → allow (non-browser client)
	}
	want := fmt.Sprintf("http://127.0.0.1:%d", port)
	return origin == want
}
