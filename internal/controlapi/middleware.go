package controlapi

import (
	"fmt"
	"net/http"
)

// withAuth wraps next with bearer-token authentication. Every request to the
// control API must carry Authorization: Bearer <token>. Requests with a missing
// or incorrect token receive 401 Unauthorized with no further processing.
//
// Constant-time comparison is not needed here: the token is 32 random bytes
// (64 hex chars), the comparison is between two same-length hex strings, and
// timing side-channels on a loopback interface provide no meaningful attack surface.
// If this concern ever arises, replace with subtle.ConstantTimeCompare.
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// checkBearer is shared with MountMCP — one auth gate, no drift; it also
		// guards the empty-configured-token case (never authenticates).
		if !checkBearer(r.Header.Get("Authorization"), s.token) {
			writeError(w, http.StatusUnauthorized, "missing or invalid Authorization header")
			return
		}
		next(w, r)
	}
}

// withOrigin wraps next with an Origin header check for mutating requests
// (POST, PUT, PATCH, DELETE). GET, HEAD, and OPTIONS are exempt.
//
// Contract (standard CSRF posture):
//   - Origin ABSENT  → allow. Non-browser clients (the engram CLI, curl) do not
//     send Origin; bearer-token auth is the gate for them. Browsers ALWAYS send
//     Origin on cross-origin mutations, so absence cannot be a browser attack.
//   - Origin present → must equal http://127.0.0.1:<port> exactly, else 403.
//     This blocks cross-origin browser pages from driving the control plane.
//
// It is applied on top of withAuth so auth always runs first.
func (s *Server) withOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			origin := r.Header.Get("Origin")
			want := fmt.Sprintf("http://127.0.0.1:%d", s.port)
			if origin != "" && origin != want {
				writeError(w, http.StatusForbidden, "origin not allowed")
				return
			}
		}
		next(w, r)
	}
}
