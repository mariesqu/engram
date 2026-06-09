package controlapi

import (
	"fmt"
	"net/http"
	"strings"
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
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			writeError(w, http.StatusUnauthorized, "missing or malformed Authorization header")
			return
		}
		token := strings.TrimPrefix(auth, prefix)
		if token != s.token {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next(w, r)
	}
}

// withOrigin wraps next with an Origin header check for mutating requests
// (POST, PUT, PATCH, DELETE). The allowed origin is http://127.0.0.1:<port>.
// GET, HEAD, and OPTIONS requests are exempt — they carry no Origin requirement.
//
// This defends against cross-origin browser requests driving the control plane.
// It is applied on top of withAuth so auth always runs first.
func (s *Server) withOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			origin := r.Header.Get("Origin")
			want := fmt.Sprintf("http://127.0.0.1:%d", s.port)
			if origin != want {
				writeError(w, http.StatusForbidden, "origin not allowed")
				return
			}
		}
		next(w, r)
	}
}
