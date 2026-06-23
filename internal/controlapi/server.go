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
//
// PR-⑥ exports:
//
//	MountMCP — registers /mcp on an existing ServeMux with bearer-token auth
package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
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

// MemorySummary is the JSON shape for a single memory returned by
// GET /api/v1/memories. Content is included in full so callers can build
// previews without a second round-trip.
type MemorySummary struct {
	ID        int64  `json:"id"`
	SyncID    string `json:"sync_id"`
	Project   string `json:"project"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Scope     string `json:"scope"`
	CreatedAt string `json:"created_at"` // RFC3339 UTC
	UpdatedAt string `json:"updated_at"` // RFC3339 UTC
}

// SyncResult is the outcome of the most recent sync cycle.
type SyncResult struct {
	// At is the RFC3339 timestamp of the last sync cycle, or nil if none has run.
	At     *time.Time `json:"at"`
	Error  *string    `json:"error"`
	Pushed int        `json:"pushed"`
	Pulled int        `json:"pulled"`
}

// EmbeddingBackfill holds the embedding backfill status included in
// GET /api/v1/status. Pending is the count of observations that have no
// embedding yet (embedding IS NULL). Provider is the active provider name.
type EmbeddingBackfill struct {
	Pending  int    `json:"pending"`
	Provider string `json:"provider,omitempty"` // "", "none", "openai"
}

// Status holds the runtime state snapshot returned by GET /api/v1/status.
type Status struct {
	CentralConnected bool       `json:"central_connected"`
	CentralURL       *string    `json:"central_url,omitempty"` // omitted when not configured
	LastSyncResult   SyncResult `json:"last_sync_result"`
	DaemonVersion    string     `json:"daemon_version"`
	// EmbeddingBackfill is POPULATED BY PR-1b (the backfill loop). Pointer +
	// omitempty so the field is ABSENT from responses until then — a permanent
	// {"pending":0} would misreport thousands of unembedded rows as done.
	EmbeddingBackfill *EmbeddingBackfill `json:"embedding_backfill,omitempty"`
}

// RedactedConfig is the config view returned by GET /api/v1/config.
// The writer key is never present as a raw value — it is either
// "***REDACTED***" (when set) or absent entirely (when not set).
// EmbeddingKeySet indicates whether an embedding API key is stored (true) without
// revealing the key itself.
type RedactedConfig struct {
	DB                  string            `json:"db,omitempty"`
	Central             *CentralConfig    `json:"central,omitempty"`
	WriterKey           *string           `json:"writer_key,omitempty"` // "***REDACTED***" or absent
	HTTP                *HTTPConfig       `json:"http,omitempty"`
	SyncInterval        string            `json:"sync_interval,omitempty"`
	LogLevel            string            `json:"log_level,omitempty"`
	Extra               map[string]string `json:"extra,omitempty"`
	EmbeddingProvider   string            `json:"embedding_provider,omitempty"`
	EmbeddingKeySet     bool              `json:"embedding_key_set,omitempty"`
	EmbeddingBaseURL    string            `json:"embedding_base_url,omitempty"`
	EmbeddingModel      string            `json:"embedding_model,omitempty"`
	EmbeddingAuthHeader string            `json:"embedding_auth_header,omitempty"`
}

// ErrConfigInvalid is wrapped by ConfigStore.Apply implementations when a
// patch would persist a configuration the next daemon startup REJECTS — the
// PUT must fail (400) instead of bricking the restart (config keys are
// restart-required; a persisted-but-fatal value cannot be corrected via the
// API once the daemon refuses to boot).
var ErrConfigInvalid = errors.New("invalid configuration")

// CentralConfig holds the central server coordinates visible in config reads
// and as the argument to SyncController.Reconnect.
//
// WriterKeyPlaintext is an in-memory-only field used when establishing a new
// connection via POST /api/v1/central/connect. It is NEVER serialised to JSON
// (the omitempty + blank tag pair prevents it) and never returned to clients.
type CentralConfig struct {
	URL      string `json:"url,omitempty"`
	WriterID string `json:"writer_id,omitempty"`

	// WriterKeyPlaintext carries the raw writer key from the connect request to
	// the SyncController.Reconnect implementation, which seals it and persists
	// the ciphertext. It is never marshalled — no json tag.
	WriterKeyPlaintext string `json:"-"`
}

// HTTPConfig holds HTTP listener settings.
type HTTPConfig struct {
	Port int `json:"port,omitempty"`
}

// ConfigPatch is a partial update to the persistent config. Only non-zero fields
// are applied. The caller must never set WriterKey, CentralURL, or
// EncryptedEmbeddingKey here — those are managed by dedicated endpoints.
type ConfigPatch struct {
	SyncInterval          *string `json:"sync_interval,omitempty"`
	LogLevel              *string `json:"log_level,omitempty"`
	HTTPPort              *int    `json:"http_port,omitempty"`
	DBPath                *string `json:"db_path,omitempty"`
	Transport             *string `json:"transport,omitempty"`
	EmbeddingProvider     *string `json:"embedding_provider,omitempty"`
	EmbeddingLocalConsent *bool   `json:"embedding_local_consent,omitempty"`
	EmbeddingDims         *int    `json:"embedding_dims,omitempty"`
	OllamaHost            *string `json:"ollama_host,omitempty"`
	OllamaModel           *string `json:"ollama_model,omitempty"`
	EmbeddingBaseURL      *string `json:"embedding_base_url,omitempty"`
	EmbeddingModel        *string `json:"embedding_model,omitempty"`
	EmbeddingAuthHeader   *string `json:"embedding_auth_header,omitempty"`
	// WriterKey, CentralURL, and EncryptedEmbeddingKey must NEVER appear here —
	// rejected at the handler.
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
	// ListMemories returns memories matching the query (FTS when non-empty,
	// recent otherwise), filtered by project, capped at limit.
	ListMemories(query, project string, limit int) ([]MemorySummary, error)
	// UpdateMemory edits an existing memory row in-place and returns the updated summary.
	// Returns an error wrapping ErrObservationNotFound when id is missing or deleted.
	UpdateMemory(id int64, title, content, typ string) (MemorySummary, error)
	// DeleteMemory soft-deletes the memory row with the given id.
	// Returns an error wrapping ErrObservationNotFound when id is missing or deleted.
	DeleteMemory(id int64) error
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

// EmbeddingKeyStore is the port for the embedding key management endpoints
// (POST/DELETE /api/v1/embedding/key). It is separate from ConfigStore to keep
// the sealing/unsealing logic out of the general config mutation path.
//
// Seal must encrypt the plaintext key and persist the ciphertext to the config
// file. The plaintext key MUST NEVER be stored in memory beyond this call.
// Returns ErrNoSecretStore when the platform cannot seal (non-Windows without a
// secret store); callers should return 422 Unprocessable Entity in that case.
//
// ClearKey removes any stored encrypted embedding key from the config file.
//
// Implementations: configStoreAdapter (production), mock in tests.
type EmbeddingKeyStore interface {
	// SealEmbeddingKey encrypts plaintext and persists the ciphertext.
	SealEmbeddingKey(plaintext []byte) error
	// ClearEmbeddingKey removes any stored encrypted embedding key.
	ClearEmbeddingKey() error
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
	keyStore EmbeddingKeyStore // nil when key management is not supported
	version  string
}

// New constructs a Server. token is the 32-byte-hex bearer token that clients
// must supply in the Authorization header. port is the TCP port (used for
// Origin validation on mutating requests). version is the binary version string
// embedded in GET /api/v1/status responses.
//
// All three dependency ports (store, syncCtrl, cfgStore) are required.
// keyStore may be nil — when nil, POST/DELETE /api/v1/embedding/key return 501.
// Passing nil for store, syncCtrl, or cfgStore panics at construction time.
func New(token string, port int, store Store, syncCtrl SyncController, cfgStore ConfigStore, version string, keyStore ...EmbeddingKeyStore) *Server {
	if store == nil {
		panic("controlapi.New: store must not be nil")
	}
	if syncCtrl == nil {
		panic("controlapi.New: syncCtrl must not be nil")
	}
	if cfgStore == nil {
		panic("controlapi.New: cfgStore must not be nil")
	}
	var ks EmbeddingKeyStore
	if len(keyStore) > 0 {
		ks = keyStore[0]
	}
	return &Server{
		token:    token,
		port:     port,
		store:    store,
		syncCtrl: syncCtrl,
		cfgStore: cfgStore,
		keyStore: ks,
		version:  version,
	}
}

// WithAuthAndOrigin is a convenience combinator for tests and PR-② / PR-③
// route registration. It wraps next with bearer-token auth (withAuth) and
// origin validation (withOrigin) — the same chain used by all mutating routes.
func (s *Server) WithAuthAndOrigin(next http.HandlerFunc) http.HandlerFunc {
	return s.withAuth(s.withOrigin(next))
}

// Handler returns a *http.ServeMux pre-wired with all routes.
//
// Route table:
//
//	GET    /api/v1/status                       → withAuth → handleStatus
//	GET    /api/v1/config                       → withAuth → handleConfig
//	PUT    /api/v1/config                       → withAuth+Origin → handleConfigPut
//	GET    /api/v1/projects                     → withAuth → handleProjects (real policies)
//	PUT    /api/v1/projects/{project}/policy    → withAuth+Origin → handleProjectPolicy
//	POST   /api/v1/central/connect              → withAuth+Origin → handleConnect
//	POST   /api/v1/central/disconnect           → withAuth+Origin → handleDisconnect
//	POST   /api/v1/sync/trigger                 → withAuth+Origin → handleSyncTrigger
//	POST   /api/v1/embedding/key                → withAuth+Origin → handleEmbeddingKeyPost
//	DELETE /api/v1/embedding/key                → withAuth+Origin → handleEmbeddingKeyDelete
//	/                                           → withAuth → 404 JSON catch-all
//
// The catch-all is auth-wrapped too: unknown paths return 401 to
// unauthenticated callers (no route enumeration), 404 only with a valid token.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", s.withAuth(s.handleStatus))
	mux.HandleFunc("/api/v1/config", s.withAuth(s.handleConfigDispatch))
	mux.HandleFunc("/api/v1/projects", s.withAuth(s.handleProjects))
	// PR-②: PUT /api/v1/projects/{project}/policy
	// Go 1.22+ ServeMux supports {variable} patterns and method prefixes.
	mux.HandleFunc("PUT /api/v1/projects/{project}/policy", s.WithAuthAndOrigin(s.handleProjectPolicy))
	// PR-③: central connect/disconnect + sync trigger.
	mux.HandleFunc("POST /api/v1/central/connect", s.WithAuthAndOrigin(s.handleConnect))
	mux.HandleFunc("POST /api/v1/central/disconnect", s.WithAuthAndOrigin(s.handleDisconnect))
	mux.HandleFunc("POST /api/v1/sync/trigger", s.WithAuthAndOrigin(s.handleSyncTrigger))
	// PR-2 (semantic-search): embedding key management.
	mux.HandleFunc("POST /api/v1/embedding/key", s.WithAuthAndOrigin(s.handleEmbeddingKeyPost))
	mux.HandleFunc("DELETE /api/v1/embedding/key", s.WithAuthAndOrigin(s.handleEmbeddingKeyDelete))
	mux.HandleFunc("/api/v1/memories", s.withAuth(s.handleMemories))
	// PUT /api/v1/memories/{id} and DELETE /api/v1/memories/{id} — memory mutation routes.
	// Auth + Origin are both required (same chain as config/policy mutation routes).
	mux.HandleFunc("/api/v1/memories/{id}", s.WithAuthAndOrigin(s.handleMemoryMutate))
	mux.HandleFunc("/", s.withAuth(s.handleNotFound))
	return mux
}

// handleConfigDispatch routes /api/v1/config to the appropriate handler based
// on the HTTP method. GET → handleConfig, PUT → handleConfigPut (with Origin).
func (s *Server) handleConfigDispatch(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleConfig(w, r)
	case http.MethodPut:
		// PUT requires Origin validation; apply it inline here since the mux
		// pattern does not include the method (we handle both GET and PUT).
		s.withOrigin(s.handleConfigPut)(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
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

func (s *Server) handleMemories(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	q := r.URL.Query()
	query := q.Get("q")
	project := q.Get("project")
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}

	memories, err := s.store.ListMemories(query, project, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if memories == nil {
		memories = []MemorySummary{}
	}
	writeJSON(w, http.StatusOK, memories)
}

// handleMemoryMutate dispatches PUT and DELETE on /api/v1/memories/{id}.
// Any other method receives 405 Method Not Allowed.
func (s *Server) handleMemoryMutate(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		s.handleMemoryPut(w, r)
	case http.MethodDelete:
		s.handleMemoryDelete(w, r)
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleMemoryPut handles PUT /api/v1/memories/{id}.
// Body: {"title":"...", "content":"...", "type":"..."}.
// type is optional; when absent or empty the existing record's type is preserved.
// Returns 200 with the updated MemorySummary on success, 400 on bad input,
// 404 when the id is missing or deleted, 500 on internal error.
func (s *Server) handleMemoryPut(w http.ResponseWriter, r *http.Request) {
	rawID := r.PathValue("id")
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "id must be a positive integer")
		return
	}

	var body struct {
		Title   string `json:"title"`
		Content string `json:"content"`
		Type    string `json:"type"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Title) == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}

	summary, err := s.store.UpdateMemory(id, body.Title, body.Content, body.Type)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "memory not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, summary)
}

// handleMemoryDelete handles DELETE /api/v1/memories/{id}.
// Returns 200 {"status":"deleted"} on success, 404 when the id is missing or
// deleted, 500 on internal error.
func (s *Server) handleMemoryDelete(w http.ResponseWriter, r *http.Request) {
	rawID := r.PathValue("id")
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "id must be a positive integer")
		return
	}

	if err := s.store.DeleteMemory(id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "memory not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// isNotFound reports whether err wraps or equals a "not found" sentinel from the
// store layer. We check by error message string matching because the localstore
// ErrObservationNotFound is not exported from this package.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "observation not found") ||
		strings.Contains(msg, "memory") && strings.Contains(msg, "not found")
}

func (s *Server) handleNotFound(w http.ResponseWriter, _ *http.Request) {
	// Fixed message: never echo the request path back into the response body.
	writeError(w, http.StatusNotFound, "not found")
}

// handleProjectPolicy handles PUT /api/v1/projects/{project}/policy.
// It decodes a {"policy":"..."} body, validates the value, calls store.SetPolicy,
// and returns 200 on success or 400 on a bad policy value.
//
// The route is registered with the WithAuthAndOrigin chain so both bearer-token
// auth and Origin validation are enforced before this handler runs.
func (s *Server) handleProjectPolicy(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if project == "" {
		writeError(w, http.StatusBadRequest, "project name is required")
		return
	}

	var body struct {
		Policy string `json:"policy"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	p, err := ParsePolicy(body.Policy)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.store.SetPolicy(project, p); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"project": project,
		"policy":  string(p),
	})
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

// ── PR-⑥: MCP HTTP transport helper ──────────────────────────────────────────

// MCPHandler is the interface satisfied by *mcpserver.StreamableHTTPServer.
// Using an interface keeps internal/controlapi free of the mcp-go import so
// the dependency stays in cmd/engram (the wiring layer).
type MCPHandler interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}

// MountMCP registers the MCP Streamable HTTP server at /mcp on mux, wrapped
// with bearer-token authentication that mirrors the /api/v1 contract.
//
// The handler is the *mcpserver.StreamableHTTPServer constructed by buildDaemon
// (it satisfies MCPHandler via its ServeHTTP method). The same token used for
// /api/v1 is reused — a single credential for the whole loopback daemon.
//
// Auth contract (mirrors withAuth):
//   - Missing or wrong Authorization: Bearer <token> → 401, no routing.
//   - Correct token → request forwarded to the streamable handler unchanged.
func MountMCP(mux *http.ServeMux, token string, h MCPHandler) {
	authed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !checkBearer(r.Header.Get("Authorization"), token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h.ServeHTTP(w, r)
	})
	// Mount at both /mcp and /mcp/. NOTE: net/http.ServeMux does NOT strip
	// prefixes (only http.StripPrefix does) — the handler receives the original
	// path. That is fine: StreamableHTTPServer.ServeHTTP dispatches on the
	// HTTP method (POST/GET/DELETE), not the path.
	mux.Handle("/mcp", authed)
	mux.Handle("/mcp/", authed)
}

// checkBearer reports whether the Authorization header carries exactly the
// expected bearer token. An EMPTY configured token NEVER authenticates —
// without this guard a zero-value server would accept "Authorization: Bearer "
// (empty credential vs empty secret). Shared by withAuth and MountMCP so the
// two auth gates cannot drift.
func checkBearer(header, token string) bool {
	if token == "" {
		return false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	return strings.TrimPrefix(header, prefix) == token
}
