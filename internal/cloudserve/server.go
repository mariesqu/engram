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
// # Auth
//
// Every request to /v1/push and /v1/pull passes through the auth middleware
// before reaching the handler. The middleware:
//
//  1. Buffers the full body (bounded by maxRequestBytes; 413 on overflow).
//  2. Calls [Verifier.Verify] with the writer_id header, method, path, body,
//     and signature header.
//  3. On error → 401 "authentication failed" (no detail leaked).
//  4. On success → resets r.Body from the buffer and stashes the authenticated
//     writer_id in the request context.
//
// The push handler additionally enforces a forgery check: the writer_id in the
// mutation body must match the writer_id authenticated by the middleware. A
// mismatch → 403.
//
// Pass [AllowAllVerifier] to disable auth (functional tests, local dev). Passing
// nil panics at construction time.
package cloudserve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/mariesqu/engram/internal/syncwire"
	"github.com/mariesqu/engram/internal/transport"
	"github.com/mariesqu/engram/internal/wireauth"
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

// ── Verifier interface ────────────────────────────────────────────────────────

// Verifier authenticates an incoming request by verifying that the HMAC
// signature in the X-Signature header was produced with the key registered for
// the writer identified by the X-Writer-Id header.
//
// Verify must return a non-nil error if authentication fails for any reason —
// missing writer_id, unknown writer_id, revoked key, or bad signature. It must
// return nil if and only if the request is authentically signed.
//
// The error value is only logged (WARN level); it is never sent to the client.
// Use opaque sentinel errors or simple string messages — do NOT include key
// material or DB details.
type Verifier interface {
	Verify(ctx context.Context, writerID, method, path string, body []byte, sig string) error
}

// errAuthFailed is the sentinel error for HMAC verification failures. It is
// intentionally unexported and generic — it must never be sent to the client.
var errAuthFailed = errors.New("authentication failed")

// NewKeyVerifier returns a [Verifier] that looks up the HMAC key for each
// writer via lookup and then calls [wireauth.Verify]. The lookup function
// signature matches [centralstore.Store.WriterKey] so the production caller can
// pass store.WriterKey directly without an adapter.
//
// A nil lookup panics.
//
// Verify returns an error (never leaking details) when:
//   - lookup returns any error (including [centralstore.ErrWriterKeyNotFound])
//   - [wireauth.Verify] returns false (wrong signature, bad hex, etc.)
func NewKeyVerifier(lookup func(ctx context.Context, writerID string) ([]byte, error)) Verifier {
	if lookup == nil {
		panic("cloudserve.NewKeyVerifier: lookup must not be nil")
	}
	return &keyVerifier{lookup: lookup}
}

type keyVerifier struct {
	lookup func(ctx context.Context, writerID string) ([]byte, error)
}

func (v *keyVerifier) Verify(ctx context.Context, writerID, method, path string, body []byte, sig string) error {
	key, err := v.lookup(ctx, writerID)
	if err != nil {
		// Includes ErrWriterKeyNotFound and any DB error — both are auth rejections.
		return errAuthFailed
	}
	if !wireauth.Verify(key, method, path, body, sig) {
		return errAuthFailed
	}
	return nil
}

// AllowAllVerifier returns a [Verifier] that always returns nil — every request
// is accepted regardless of headers. Use this as an explicit opt-out from auth
// in functional tests, acceptance harnesses, and local development. Passing
// AllowAllVerifier() is required; passing nil panics.
//
// When AllowAllVerifier is in use:
//   - Auth headers are read but ignored (Verify is never called for the HMAC).
//   - The authenticated writer_id stashed in context is empty.
//   - The push forgery check is skipped (see handlePush).
func AllowAllVerifier() Verifier { return allowAllVerifier{} }

type allowAllVerifier struct{}

func (allowAllVerifier) Verify(_ context.Context, _, _, _ string, _ []byte, _ string) error {
	return nil
}

// ── context key for authenticated writer_id ───────────────────────────────────

// writerIDKey is an unexported type used as a context key to avoid collisions
// with other packages that use context.WithValue.
type writerIDKey struct{}

// writerIDFromContext returns the writer_id stashed by the auth middleware, or
// "" when no auth was performed (AllowAllVerifier path) or the middleware was
// bypassed.
func writerIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(writerIDKey{}).(string)
	return v
}

// ── Server ────────────────────────────────────────────────────────────────────

// Server is a net/http handler that exposes a [transport.Central] over HTTP.
// Construct it with [New]; call [Server.Handler] to obtain the routed mux.
type Server struct {
	central  transport.Central
	verifier Verifier
	logger   *slog.Logger
}

// New constructs a Server wrapping c. verifier is REQUIRED — pass
// [AllowAllVerifier] to disable auth instead of nil.
//
// Panics if verifier is nil (defensive guard against accidents).
func New(c transport.Central, verifier Verifier) *Server {
	if verifier == nil {
		panic("cloudserve.New: verifier is required; pass AllowAllVerifier() to disable auth")
	}
	return &Server{
		central:  c,
		verifier: verifier,
		logger:   slog.Default(),
	}
}

// Handler returns a *http.ServeMux pre-wired with all routes. Tests drive the
// server via httptest.NewServer(s.Handler()); production wires it into an
// http.Server in the serve subcommand (PR7).
//
// Route table:
//
//	POST /v1/push  → auth middleware → [Server.handlePush]
//	POST /v1/pull  → auth middleware → [Server.handlePull]
//
// Wrong method on a known path → 405. Unknown path → 404 with the JSON
// {"error":...} shape (via the "/" catch-all, not the text/plain ServeMux default).
//
// The "/" catch-all and the 405 path do NOT go through the auth middleware —
// those paths are rejected before reaching any handler logic.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/push", s.methodGuard(http.MethodPost, s.withAuth(s.handlePush)))
	mux.HandleFunc("/v1/pull", s.methodGuard(http.MethodPost, s.withAuth(s.handlePull)))
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

// ── Auth middleware ───────────────────────────────────────────────────────────

// withAuth wraps next with the HMAC auth middleware. It:
//
//  1. Reads the X-Writer-Id and X-Signature headers (does NOT pre-reject empty
//     values — the verifier decides, so AllowAllVerifier passes header-less requests).
//  2. Buffers the full body, bounded by maxRequestBytes.
//     On *http.MaxBytesError → 413. Other read error → 400.
//  3. Calls s.verifier.Verify; on error → WARN log + 401 "authentication failed".
//  4. On success: resets r.Body from the buffer, stashes the writerID in the
//     request context, calls next.
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writerID := r.Header.Get(wireauth.HeaderWriterID)
		sig := r.Header.Get(wireauth.HeaderSignature)

		// Buffer the body bounded by maxRequestBytes. This is the ONLY read of
		// r.Body in the auth layer — the buffered bytes are handed to the verifier
		// and then the body is reset for the downstream handler.
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			} else {
				writeError(w, http.StatusBadRequest, "could not read request body")
			}
			return
		}

		if err := s.verifier.Verify(r.Context(), writerID, r.Method, r.URL.Path, body, sig); err != nil {
			s.logger.WarnContext(r.Context(), "cloudserve: auth failed",
				"writer_id", writerID,
				"method", r.Method,
				"path", r.URL.Path,
				"err", err,
			)
			writeError(w, http.StatusUnauthorized, "authentication failed")
			return
		}

		// Reset the body so downstream handlers can read the buffered bytes normally.
		r.Body = io.NopCloser(bytes.NewReader(body))

		// Stash the authenticated writer_id in context for the push forgery check.
		ctx := context.WithValue(r.Context(), writerIDKey{}, writerID)
		next(w, r.WithContext(ctx))
	}
}

// ── Route handlers ────────────────────────────────────────────────────────────

// handlePush processes a POST /v1/push request.
//
// Pipeline:
//  1. Auth middleware (runs before this handler via withAuth): body buffered,
//     HMAC verified, writerID stashed in context.
//  2. Decode JSON body into [syncwire.PushRequest]. Decode error → 400 or 413.
//  3. [syncwire.VerifyMutationID] — tampered payload → 400.
//  4. [syncwire.FromWire] — malformed payload / bad occurred_at → 400.
//  5. Forgery check: if authWriterID != "" && m.WriterID != authWriterID → 403.
//     (The authWriterID == "" guard means AllowAllVerifier skips this check.)
//  6. central.Apply — DB/internal error → 500. nil → 200.
//
// On 200 the response body is a [syncwire.PushResponse] with status "ok" and
// applied=true.
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

	// Forgery check: the mutation's writer_id must match the authenticated writer.
	// The guard (authWriterID != "") means AllowAllVerifier (which stashes "") skips
	// this check, preserving existing no-auth functional test behavior.
	if authWriterID := writerIDFromContext(r.Context()); authWriterID != "" && m.WriterID != authWriterID {
		s.logger.WarnContext(r.Context(), "cloudserve: writer_id mismatch (forgery attempt)",
			"auth_writer_id", authWriterID,
			"mutation_writer_id", m.WriterID,
		)
		writeError(w, http.StatusForbidden, "writer_id does not match authenticated writer")
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
//  1. Auth middleware (runs before this handler via withAuth).
//  2. Decode JSON body into [syncwire.PullRequest]. Decode error → 400 or 413.
//  3. Validate: Project non-empty → else 400.
//  4. Clamp Limit: ≤0 → pullDefaultLimit; >pullMaxLimit → pullMaxLimit.
//  5. central.PullSince → error → 500. Success → 200 with [syncwire.PullResponse].
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

// decodeBody reads and JSON-decodes the request body into dst. By the time this
// is called from handlePush/handlePull, the auth middleware has already buffered
// the body (bounded by maxRequestBytes) and reset r.Body to an in-memory reader.
// The MaxBytesReader call here is therefore a harmless no-op on the in-memory
// buffer; the 413 path is already covered by the middleware. It is retained so
// the 400/413 semantics remain correct if decodeBody is ever called from a path
// that bypasses the middleware.
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
