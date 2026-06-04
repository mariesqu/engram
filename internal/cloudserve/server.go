// Package cloudserve provides a net/http server that exposes the central sync
// store's push/pull operations over HTTP.
//
// The server wraps [transport.Central] (not *centralstore.Store directly) so it
// is unit-testable with a mock and production-deployable with the real store.
//
// Routes:
//
//	POST /v1/push  — apply one mutation to central (see [handlePush])
//	POST /v1/pull  — fetch mutations since a given seq (see [handlePull])
//
// Auth is deferred to PR6 (HMAC + shared secret). All endpoints are currently
// unauthenticated.
package cloudserve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/mariesqu/engram/internal/syncwire"
	"github.com/mariesqu/engram/internal/transport"
)

// pullDefaultLimit is the number of mutations returned when the client sends
// limit <= 0. pullMaxLimit caps the value to prevent oversized responses.
const (
	pullDefaultLimit = 100
	pullMaxLimit     = 1000
)

// maxRequestBytes bounds the request body size both handlers will parse, capping
// CPU/memory spent on JSON decoding (DoS guard). One mutation's canonical payload
// is far smaller than this.
const maxRequestBytes = 1 << 20 // 1 MiB

// HTTP server timeouts used by Run. They bound how long a single connection can
// hold server resources, defending an Internet-facing deployment against
// Slowloris-style slow-client attacks (clients that trickle headers/body or never
// read the response).
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second
)

// Server is a net/http handler that exposes a [transport.Central] over HTTP.
// Construct it with [New]; call [Server.Handler] to obtain the routed mux.
type Server struct {
	central transport.Central
	logger  *slog.Logger
}

// New constructs a Server wrapping c. Internal (5xx) errors are logged via
// slog.Default(); configure process-wide logging with slog.SetDefault.
func New(c transport.Central) *Server {
	return &Server{
		central: c,
		logger:  slog.Default(),
	}
}

// Handler returns a *http.ServeMux pre-wired with all routes. Tests drive the
// server via httptest.NewServer(s.Handler()); production wires it into an
// http.Server in the serve subcommand (PR7).
//
// Route table:
//
//	POST /v1/push  → [Server.handlePush]
//	POST /v1/pull  → [Server.handlePull]
//
// Wrong method on a known path → 405. Unknown path → 404 with the JSON
// {"error":...} shape (via the "/" catch-all, not the text/plain ServeMux default).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/push", s.methodGuard(http.MethodPost, s.handlePush))
	mux.HandleFunc("/v1/pull", s.methodGuard(http.MethodPost, s.handlePull))
	// Catch-all: any path not matched by the exact /v1/* routes above gets a JSON
	// 404 instead of net/http's default text/plain "404 page not found".
	mux.HandleFunc("/", s.handleNotFound)
	return mux
}

// Run starts an http.Server on addr using s.Handler() and blocks until ctx is
// cancelled. On cancellation it calls Shutdown with a 10-second drain timeout.
// The server carries read/write/idle timeouts (see httpServer) to bound slow-client
// resource use. Run is a convenience helper for PR7 — tests use Handler() directly
// with httptest.NewServer.
func (s *Server) Run(ctx context.Context, addr string) error {
	srv := s.httpServer(addr)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

// httpServer builds the *http.Server for Run with hardened read/write/idle
// timeouts. Separated from Run so the timeout configuration is unit-testable.
func (s *Server) httpServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// ── Route handlers ────────────────────────────────────────────────────────────

// handlePush processes a POST /v1/push request.
//
// Pipeline:
//  1. Decode JSON body into [syncwire.PushRequest]. Decode error → 400.
//  2. [syncwire.VerifyMutationID] — tampered payload → 400.
//  3. [syncwire.FromWire] — malformed payload / bad occurred_at → 400.
//  4. central.Apply — DB/internal error → 500. nil → 200.
//
// On 200 the response body is a [syncwire.PushResponse] with status "ok" and
// applied=true. Apply does NOT distinguish applied vs idempotent NoOp, so
// applied=true means "the server accepted the mutation" in all success cases.
// A 409 for version-guard NoOp is intentionally deferred (see package doc).
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	var req syncwire.PushRequest
	if !decodeBody(w, r, &req) {
		return
	}

	if err := syncwire.VerifyMutationID(req.Mutation); err != nil {
		writeError(w, http.StatusBadRequest, "mutation_id verification failed: "+err.Error())
		return
	}

	m, err := syncwire.FromWire(req.Mutation)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid mutation: "+err.Error())
		return
	}

	if err := s.central.Apply(r.Context(), m); err != nil {
		s.logger.ErrorContext(r.Context(), "cloudserve: Apply failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, syncwire.PushResponse{
		Status:     "ok",
		MutationID: m.MutationID,
		Applied:    true,
	})
}

// handlePull processes a POST /v1/pull request.
//
// Pipeline:
//  1. Decode JSON body into [syncwire.PullRequest]. Decode error → 400.
//  2. Validate: Project non-empty → else 400.
//  3. Clamp Limit: ≤0 → pullDefaultLimit; >pullMaxLimit → pullMaxLimit.
//  4. central.PullSince → error → 500. Success → 200 with [syncwire.PullResponse].
func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	var req syncwire.PullRequest
	if !decodeBody(w, r, &req) {
		return
	}

	if req.Project == "" {
		writeError(w, http.StatusBadRequest, "project is required")
		return
	}

	limit := req.Limit
	switch {
	case limit <= 0:
		limit = pullDefaultLimit
	case limit > pullMaxLimit:
		limit = pullMaxLimit
	}

	mutations, err := s.central.PullSince(r.Context(), req.Project, req.SinceSeq, limit)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "cloudserve: PullSince failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	wires := make([]syncwire.WireMutation, 0, len(mutations))
	for _, m := range mutations {
		wires = append(wires, syncwire.ToWire(m))
	}

	writeJSON(w, http.StatusOK, syncwire.PullResponse{Mutations: wires})
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

// errorBody is the consistent JSON shape for all error responses.
type errorBody struct {
	Error string `json:"error"`
}

// writeError writes a JSON error response with the given status code.
// Body shape: {"error":"<msg>"}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

// writeJSON marshals v as JSON and writes it with the given status and
// Content-Type: application/json. If marshaling v fails (should never happen with
// our controlled response types), it still emits a JSON {"error":...} body with a
// 500 status — never a text/plain response — so every response honors the wire
// contract.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		// The value failed to marshal — write a pre-baked constant body (NOT
		// json.Marshal, which just failed) so this path can never itself fail to
		// encode, while keeping the JSON shape + Content-Type the contract promises.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal error: response encoding failed"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// decodeBody reads and JSON-decodes the request body into dst, enforcing
// maxRequestBytes via http.MaxBytesReader (DoS guard). On failure it writes the
// appropriate error — 413 when the body exceeds the cap, 400 when it is malformed
// — and returns false so the caller can return immediately.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		}
		return false
	}
	// Enforce a single JSON document per request: a second value or any trailing
	// non-whitespace (anything other than EOF) violates the wire contract.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request body must contain a single JSON document")
		return false
	}
	return true
}

// methodGuard returns a handler that calls next only when the request method
// matches allowed. Any other method returns 405 with an Allow header.
func (s *Server) methodGuard(allowed string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != allowed {
			w.Header().Set("Allow", allowed)
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		next(w, r)
	}
}

// handleNotFound returns a JSON 404 for any path not matched by a specific route,
// keeping every error response in the {"error":...} shape. Registered on "/" so it
// catches all unmatched paths (the more specific /v1/* routes take precedence).
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not found: "+r.URL.Path)
}
