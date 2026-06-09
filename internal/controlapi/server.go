// Package controlapi provides the loopback HTTP control plane for the engram
// resident daemon. It exposes node-local state (status, config, projects) over
// a versioned JSON API at /api/v1/... bound exclusively to 127.0.0.1.
//
// Threat model: single-user loopback. Auth is a 32-byte-hex bearer token
// written to daemon.json (user-only ACL). This is intentionally separate from
// internal/cloudserve, which is the central (Internet-facing, Postgres, HMAC)
// surface — two different threat models must not share a package.
//
// Patterns borrowed from cloudserve: Server struct, Handler() mux,
// withAuth middleware, writeJSON/writeError helpers.
//
// Port interfaces (load-bearing — all later PRs depend on these shapes):
//
//	Store         — ListProjectsWithPolicy, SetPolicy, GetPolicy
//	SyncController — Status, TriggerNow, Disconnect, Reconnect
//	ConfigStore   — Load, Apply
//
// Routes (PR-①):
//
//	GET /api/v1/status   → handleStatus
//	GET /api/v1/config   → handleConfig   (writer key REDACTED)
//	GET /api/v1/projects → handleProjects (policy per project)
package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// ── Port interfaces ───────────────────────────────────────────────────────────
// These are the ONLY dependencies controlapi takes. They are satisfied by the
// wired daemon (cmd/engram glue) and by mocks in tests. Never import localstore,
// syncer, or cmd packages from here — that would invert the dependency graph.

// Policy is the per-project sync policy. The three values are the canonical
// string representation used in the API and the database CHECK constraint.
type Policy string

const (
	PolicySynced    Policy = "synced"
	PolicyLocalOnly Policy = "local-only"
	PolicyOmitted   Policy = "omitted"
)

// ParsePolicy validates and parses a policy string. Returns an error for
// any value that is not one of the three canonical values.
func ParsePolicy(s string) (Policy, error) {
	switch Policy(s) {
	case PolicySynced, PolicyLocalOnly, PolicyOmitted:
		return Policy(s), nil
	default:
		return "", errors.New("policy must be one of: synced, local-only, omitted")
	}
}

// ProjectPolicy pairs a project name with its effective policy.
type ProjectPolicy struct {
	Name   string `json:"name"`
	Policy Policy `json:"policy"`
}

// SyncResult is the outcome of the most recent sync cycle.
type SyncResult struct {
	// At is the RFC3339 timestamp of the last sync cycle, or nil if none has run.
	At     *time.Time `json:"at"`
	Error  *string    `json:"error"`
	Pushed int        `json:"pushed"`
	Pulled int        `json:"pulled"`
}

// Status holds the runtime state snapshot returned by GET /api/v1/status.
type Status struct {
	CentralConnected bool       `json:"central_connected"`
	CentralURL       *string    `json:"central_url,omitempty"` // omitted when not configured
	LastSyncResult   SyncResult `json:"last_sync_result"`
	DaemonVersion    string     `json:"daemon_version"`
}

// RedactedConfig is the config view returned by GET /api/v1/config.
// The writer key is never present as a raw value — it is either
// "***REDACTED***" (when set) or absent entirely (when not set).
type RedactedConfig struct {
	DB           string            `json:"db,omitempty"`
	Central      *CentralConfig    `json:"central,omitempty"`
	WriterKey    *string           `json:"writer_key,omitempty"` // "***REDACTED***" or absent
	HTTP         *HTTPConfig       `json:"http,omitempty"`
	SyncInterval string            `json:"sync_interval,omitempty"`
	LogLevel     string            `json:"log_level,omitempty"`
	Extra        map[string]string `json:"extra,omitempty"`
}

// CentralConfig holds the central server coordinates visible in config reads.
type CentralConfig struct {
	URL      string `json:"url,omitempty"`
	WriterID string `json:"writer_id,omitempty"`
}

// HTTPConfig holds HTTP listener settings.
type HTTPConfig struct {
	Port int `json:"port,omitempty"`
}

// ConfigPatch is a partial update to the persistent config. Only non-zero fields
// are applied. The caller must never set WriterKey or CentralURL here — those are
// managed by the connect/disconnect endpoints (PR-③).
type ConfigPatch struct {
	SyncInterval *string `json:"sync_interval,omitempty"`
	LogLevel     *string `json:"log_level,omitempty"`
	HTTPPort     *int    `json:"http_port,omitempty"`
	DBPath       *string `json:"db_path,omitempty"`
	Transport    *string `json:"transport,omitempty"`
	// WriterKey and CentralURL must NEVER appear here — rejected at the handler.
}

// Store is the node-local persistence port. PR-① uses ListProjectsWithPolicy
// for the GET /api/v1/projects endpoint. PR-② extends this with SetPolicy and
// GetPolicy for mutation routes.
//
// Implementations: *localstore.Store (production), mock in tests.
type Store interface {
	ListProjectsWithPolicy() ([]ProjectPolicy, error)
	SetPolicy(project string, p Policy) error
	GetPolicy(project string) (Policy, error)
}

// SyncController is the autosync control port. PR-① uses Status for the
// GET /api/v1/status endpoint. PR-③ extends with TriggerNow, Disconnect, Reconnect.
//
// Implementations: syncer.Loop adapter (production), mock in tests.
type SyncController interface {
	Status() Status
	TriggerNow(ctx context.Context) error
	Disconnect() error
	Reconnect(cfg CentralConfig) error
}

// ConfigStore is the config persistence port. PR-① uses Load for the
// GET /api/v1/config endpoint. PR-③ extends with Apply for mutation routes.
//
// Implementations: config.Store adapter (production), mock in tests.
type ConfigStore interface {
	// Load returns the current effective config with the writer key redacted.
	// The raw key MUST NEVER be returned through this interface.
	Load() (RedactedConfig, error)

	// Apply merges a partial patch into the persistent config.
	// Returns true when the change requires a daemon restart.
	Apply(patch ConfigPatch) (restartRequired bool, err error)
}

// ── Server ────────────────────────────────────────────────────────────────────

// Server is the loopback control plane HTTP server.
// Construct it with New; call Handler() to obtain the routed mux.
//
// The server binds exclusively to 127.0.0.1 — Handler() itself does not
// enforce this; the caller (daemon startup) must pass "127.0.0.1:<port>" to
// net.Listen. This is validated in tests by asserting the listen address.
type Server struct {
	token    string
	port     int
	store    Store
	syncCtrl SyncController
	cfgStore ConfigStore
	version  string
}

// New constructs a Server. token is the 32-byte-hex bearer token that clients
// must supply in the Authorization header. port is the TCP port (used for
// Origin validation on mutating requests). version is the binary version string
// embedded in GET /api/v1/status responses.
//
// All three dependency ports (store, syncCtrl, cfgStore) are required.
// Passing a nil port panics at construction time (defensive guard).
func New(token string, port int, store Store, syncCtrl SyncController, cfgStore ConfigStore, version string) *Server {
	if store == nil {
		panic("controlapi.New: store must not be nil")
	}
	if syncCtrl == nil {
		panic("controlapi.New: syncCtrl must not be nil")
	}
	if cfgStore == nil {
		panic("controlapi.New: cfgStore must not be nil")
	}
	return &Server{
		token:    token,
		port:     port,
		store:    store,
		syncCtrl: syncCtrl,
		cfgStore: cfgStore,
		version:  version,
	}
}

// WithAuthAndOrigin is a convenience combinator for tests and PR-② / PR-③
// route registration. It wraps next with bearer-token auth (withAuth) and
// origin validation (withOrigin) — the same chain used by all mutating routes.
func (s *Server) WithAuthAndOrigin(next http.HandlerFunc) http.HandlerFunc {
	return s.withAuth(s.withOrigin(next))
}

// Handler returns a *http.ServeMux pre-wired with all PR-① routes.
//
// Route table:
//
//	GET /api/v1/status   → withAuth → handleStatus
//	GET /api/v1/config   → withAuth → handleConfig
//	GET /api/v1/projects → withAuth → handleProjects
//	/                    → 404 JSON catch-all
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", s.withAuth(s.handleStatus))
	mux.HandleFunc("/api/v1/config", s.withAuth(s.handleConfig))
	mux.HandleFunc("/api/v1/projects", s.withAuth(s.handleProjects))
	mux.HandleFunc("/", s.handleNotFound)
	return mux
}

// ── Route handlers ─────────────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st := s.syncCtrl.Status()
	// Override version from server config so the status is always truthful.
	st.DaemonVersion = s.version
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	cfg, err := s.cfgStore.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	projects, err := s.store.ListProjectsWithPolicy()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Always return a JSON array, never null.
	if projects == nil {
		projects = []ProjectPolicy{}
	}
	writeJSON(w, http.StatusOK, projects)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not found: "+r.URL.Path)
}

// ── HTTP helpers (mirrored from cloudserve — same patterns, separate package) ─

// errorBody is the uniform JSON shape for all error responses.
type errorBody struct {
	Error string `json:"error"`
}

// writeError writes a JSON error response with the given status code.
// Every error response sets Cache-Control: no-store per the spec.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

// writeJSON marshals v as JSON and writes it with the given status,
// Content-Type: application/json, and Cache-Control: no-store.
// If marshaling fails it falls back to a pre-baked error body so every
// response always honors the JSON contract.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal error: response encoding failed"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// decodeBody reads and JSON-decodes the request body into dst.
// Returns false and writes an error response on failure.
const maxRequestBytes = 1 << 20 // 1 MiB — same bound as cloudserve

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
	// Enforce a single JSON document per request.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request body must contain a single JSON document")
		return false
	}
	return true
}
