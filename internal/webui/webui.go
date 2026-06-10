package webui

import (
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mariesqu/engram/internal/controlapi"
)

// WebUIDeps are the ports the web UI needs to serve its surfaces.
// These are satisfied by the wired daemon adapters (same instances as the
// controlapi.Server uses) — we do not duplicate the adapters.
type WebUIDeps struct {
	// SyncCtrl supplies the live Status for the status page and polling partial,
	// and handles TriggerNow, Disconnect, and Reconnect for the mutating routes.
	SyncCtrl controlapi.SyncController

	// Store supplies the projects list with effective policies and handles
	// SetPolicy for the policy toggle POST.
	Store controlapi.Store

	// ConfigStore handles config reads and updates for the config form.
	ConfigStore controlapi.ConfigStore

	// Secret is the daemon bearer token used to validate the ?token= exchange.
	Secret string

	// Port is the TCP port the daemon is bound to (used for Origin validation).
	Port int

	// Version is the daemon version string embedded in the status page footer.
	Version string
}

// Mount registers all /ui/ routes on mux.
//
// Route table:
//
//	GET  /ui/                              → full status page (session required)
//	GET  /ui/status                        → HTMX partial status fragment (cookie required, polled every 3s)
//	GET  /ui/projects                      → projects list with policy toggles (cookie required)
//	GET  /ui/config                        → config form page (cookie required)
//	POST /ui/projects/{project}/policy     → policy toggle (session + CSRF + Origin)
//	POST /ui/config                        → update sync_interval (session + CSRF + Origin)
//	POST /ui/sync                          → trigger sync now (session + CSRF + Origin)
//	POST /ui/connect                       → connect to central (session + CSRF + Origin)
//	POST /ui/disconnect                    → disconnect from central (session + CSRF + Origin)
//	GET  /ui/static/…                      → embedded static assets (no auth)
//
// Token exchange (runs before session guard):
//
//	GET /ui/?token=<bearer>                → validate token → set session+CSRF cookies → redirect to /ui/
func Mount(mux *http.ServeMux, deps WebUIDeps) {
	// Static assets — no auth. The browser needs htmx.min.js and styles.css
	// before any session cookie is established, including on the 401 page.
	// fs.Sub roots the FileServer at the static/ directory so the stripped
	// prefix maps 1:1 onto file names (no accidental exposure of siblings).
	staticRoot, err := fs.Sub(StaticFS, "static")
	if err != nil {
		panic("webui: static sub-FS: " + err.Error()) // embed is compile-time; unreachable
	}
	staticServer := http.FileServer(http.FS(staticRoot))
	mux.Handle("/ui/static/", secHeaders(http.StripPrefix("/ui/static/", staticServer)))

	// All other /ui/ routes: token exchange + session-gated handlers.
	// The session store is PER MOUNT — two daemons in one process (tests!)
	// must never share session state.
	sessions := &sessionStore{}
	mux.Handle("/ui/", secHeaders(routeUI(deps, sessions)))
}

// secHeaders sets browser-facing security headers on every /ui response.
// Loopback-only, but the UI renders strings influenced by a remote central
// (sync error text), so defense-in-depth is cheap and worth it:
//   - frame-ancestors/X-Frame-Options: no local page may iframe the dashboard.
//   - CSP self-only sources: no external fetches, no inline/eval'd script
//     (htmx.min.js is served same-origin from /ui/static/).
//   - nosniff: assets are typed correctly; never content-sniffed.
func secHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// routeUI returns a handler that dispatches /ui/ sub-paths, running the token
// exchange before the session guard.
func routeUI(deps WebUIDeps, sessions *sessionStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Token exchange: ONLY at the canonical entry point /ui/ (the URL
		// cmd/engram/ui.go opens). Deep links with a stray ?token= do NOT
		// exchange — keeping the token-accepting surface as small as possible.
		// NOTE: bare "/ui" never reaches here — the ServeMux 301-redirects it to
		// "/ui/" before the handler runs, so only the canonical path is checked.
		if r.URL.Path == "/ui/" && r.URL.Query().Get("token") != "" {
			exchangeToken(deps.Secret, deps.Port, sessions).ServeHTTP(w, r)
			return
		}

		// All other /ui/* routes require a valid session cookie.
		requireSession(sessions, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			dispatchUI(w, r, deps, sessions)
		})).ServeHTTP(w, r)
	})
}

// dispatchUI routes within the cookie-authenticated /ui/* space.
// Mutating routes (POST) additionally require Origin validation and CSRF.
func dispatchUI(w http.ResponseWriter, r *http.Request, deps WebUIDeps, sessions *sessionStore) {
	// Origin check for all mutating methods — mirrors controlapi.withOrigin.
	// Absent Origin → allow (non-browser); present-mismatched → 403.
	if !checkOriginForPost(r, deps.Port) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}

	switch {
	case r.URL.Path == "/ui/":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleStatusPage(w, r, deps, sessions)

	case r.URL.Path == "/ui/status":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleStatusPartial(w, r, deps, sessions)

	case r.URL.Path == "/ui/projects":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleProjectsPage(w, r, deps, sessions)

	case r.URL.Path == "/ui/config":
		switch r.Method {
		case http.MethodGet:
			handleConfigPage(w, r, deps, sessions)
		case http.MethodPost:
			withCSRF(sessions, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handleConfigPost(w, r, deps, sessions)
			})).ServeHTTP(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

	case r.URL.Path == "/ui/sync":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		withCSRF(sessions, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleSyncPost(w, r, deps, sessions)
		})).ServeHTTP(w, r)

	case r.URL.Path == "/ui/connect":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		withCSRF(sessions, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleConnectPost(w, r, deps, sessions)
		})).ServeHTTP(w, r)

	case r.URL.Path == "/ui/disconnect":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		withCSRF(sessions, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleDisconnectPost(w, r, deps, sessions)
		})).ServeHTTP(w, r)

	case isPolicyPath(r.URL.Path):
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		withCSRF(sessions, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlePolicyPost(w, r, deps, sessions)
		})).ServeHTTP(w, r)

	default:
		http.NotFound(w, r)
	}
}

// isPolicyPath reports whether path matches /ui/projects/{project}/policy.
// Go 1.22 ServeMux path variables are not available when using a single
// top-level /ui/ catch-all, so we parse the path manually.
func isPolicyPath(path string) bool {
	// Expected: /ui/projects/<name>/policy
	const prefix = "/ui/projects/"
	const suffix = "/policy"
	if len(path) <= len(prefix)+len(suffix) {
		return false
	}
	if path[:len(prefix)] != prefix {
		return false
	}
	if path[len(path)-len(suffix):] != suffix {
		return false
	}
	// The middle segment (project name) must be exactly ONE non-empty path
	// segment — "/ui/projects/a/b/policy" is NOT a policy path (it would
	// otherwise extract "a/b" as the project name).
	middle := path[len(prefix) : len(path)-len(suffix)]
	return middle != "" && !strings.Contains(middle, "/")
}

// extractProject returns the project name from a /ui/projects/{project}/policy
// path. The segment is percent-DECODED (net/http preserves %2F in paths) so a
// project name round-trips byte-identically; the store normalizes further
// (trim/lowercase). A segment that decodes to contain "/" is rejected by
// returning "" (the handler 404s on empty).
func extractProject(path string) string {
	const prefix = "/ui/projects/"
	const suffix = "/policy"
	raw := path[len(prefix) : len(path)-len(suffix)]
	decoded, err := url.PathUnescape(raw)
	if err != nil || strings.Contains(decoded, "/") {
		return ""
	}
	return decoded
}

// ── View models ──────────────────────────────────────────────────────────────

// statusViewModel is the template data for the status page and status partial.
// It flattens pointer fields from controlapi.Status into plain string values so
// html/template actions can compare them without nil-dereference panics.
//
// PR-④b adds CSRF-related fields and connect-form fields so the partial can
// render the sync-now / disconnect / connect forms in the same fragment.
type statusViewModel struct {
	CentralConnected bool
	CentralURL       string // empty when not configured
	LastSyncAt       string // RFC3339 or empty
	LastSyncError    string // empty when last sync succeeded or never ran
	LastSyncPushed   int
	LastSyncPulled   int
	DaemonVersion    string

	// CSRF fields (④b) — present on the status partial for the action forms.
	CSRFToken string

	// Connect form fields — echoed back on validation error so the user does
	// not lose their typed values. writer_key is NEVER echoed.
	ConnectError      string
	ConnectCentralURL string
	ConnectWriterID   string
}

// projectsViewModel is the template data for the projects page.
type projectsViewModel struct {
	Projects      []controlapi.ProjectPolicy
	DaemonVersion string
	CSRFToken     string // ④b: needed for the policy toggle forms
}

// configViewModel is the template data for the config form page.
type configViewModel struct {
	DaemonVersion   string
	SyncInterval    string
	CentralURL      string
	WriterKeySet    bool // true when a writer key is stored (shown as REDACTED)
	RestartRequired bool
	Error           string
	CSRFToken       string
}

// newStatusVM converts a live controlapi.Status into a statusViewModel.
// csrfToken is injected from the per-session CSRF store.
func newStatusVM(st controlapi.Status, version, csrfToken string) statusViewModel {
	vm := statusViewModel{
		CentralConnected: st.CentralConnected,
		DaemonVersion:    version,
		LastSyncPushed:   st.LastSyncResult.Pushed,
		LastSyncPulled:   st.LastSyncResult.Pulled,
		CSRFToken:        csrfToken,
	}
	if st.CentralURL != nil {
		vm.CentralURL = *st.CentralURL
	}
	if st.LastSyncResult.At != nil {
		vm.LastSyncAt = st.LastSyncResult.At.UTC().Format(time.RFC3339)
	}
	if st.LastSyncResult.Error != nil {
		vm.LastSyncError = *st.LastSyncResult.Error
	}
	return vm
}

// ── Handlers — read ───────────────────────────────────────────────────────────

func handleStatusPage(w http.ResponseWriter, _ *http.Request, deps WebUIDeps, sessions *sessionStore) {
	st := deps.SyncCtrl.Status()
	vm := newStatusVM(st, deps.Version, sessions.csrfToken())
	renderPage(w, statusTmpl, vm)
}

func handleStatusPartial(w http.ResponseWriter, _ *http.Request, deps WebUIDeps, sessions *sessionStore) {
	st := deps.SyncCtrl.Status()
	vm := newStatusVM(st, deps.Version, sessions.csrfToken())
	renderPartial(w, "status-partial", vm)
}

func handleProjectsPage(w http.ResponseWriter, _ *http.Request, deps WebUIDeps, sessions *sessionStore) {
	projects, err := deps.Store.ListProjectsWithPolicy()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if projects == nil {
		projects = []controlapi.ProjectPolicy{}
	}
	vm := projectsViewModel{
		Projects:      projects,
		DaemonVersion: deps.Version,
		CSRFToken:     sessions.csrfToken(),
	}
	renderPage(w, projectsTmpl, vm)
}

func handleConfigPage(w http.ResponseWriter, _ *http.Request, deps WebUIDeps, sessions *sessionStore) {
	vm := buildConfigVM(deps, sessions, "", false)
	renderPage(w, configTmpl, vm)
}

// buildConfigVM loads the current config from ConfigStore and builds the view model.
// errMsg is set when the form POST failed. restartRequired is set on a successful
// POST that required a restart.
func buildConfigVM(deps WebUIDeps, sessions *sessionStore, errMsg string, restartRequired bool) configViewModel {
	vm := configViewModel{
		DaemonVersion:   deps.Version,
		Error:           errMsg,
		RestartRequired: restartRequired,
		CSRFToken:       sessions.csrfToken(),
	}
	if deps.ConfigStore != nil {
		cfg, err := deps.ConfigStore.Load()
		if err == nil {
			vm.SyncInterval = cfg.SyncInterval
			if cfg.WriterKey != nil {
				vm.WriterKeySet = true
			}
			if cfg.Central != nil {
				vm.CentralURL = cfg.Central.URL
			}
		}
	}
	return vm
}

// ── Handlers — mutating ───────────────────────────────────────────────────────

// handlePolicyPost handles POST /ui/projects/{project}/policy.
// It calls Store.SetPolicy with the chosen policy and returns the refreshed
// projects tbody partial for HTMX swap.
func handlePolicyPost(w http.ResponseWriter, r *http.Request, deps WebUIDeps, sessions *sessionStore) {
	project := extractProject(r.URL.Path)
	if project == "" {
		http.Error(w, "project name required", http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	policyStr := r.FormValue("policy")
	p, err := controlapi.ParsePolicy(policyStr)
	if err != nil {
		http.Error(w, "invalid policy value", http.StatusBadRequest)
		return
	}

	if err := deps.Store.SetPolicy(project, p); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Return the refreshed projects tbody rows for the HTMX innerHTML swap.
	projects, err := deps.Store.ListProjectsWithPolicy()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if projects == nil {
		projects = []controlapi.ProjectPolicy{}
	}
	vm := projectsViewModel{
		Projects:      projects,
		DaemonVersion: deps.Version,
		CSRFToken:     sessions.csrfToken(),
	}
	renderPartial(w, "projects-rows", vm)
}

// handleSyncPost handles POST /ui/sync.
// Calls SyncController.TriggerNow and returns the refreshed status partial.
func handleSyncPost(w http.ResponseWriter, r *http.Request, deps WebUIDeps, sessions *sessionStore) {
	st := deps.SyncCtrl.Status()
	if !st.CentralConnected {
		// Return the partial with the disconnect state — the button was somehow
		// submitted while disconnected (race/stale page). Treat as 409.
		w.WriteHeader(http.StatusConflict)
		vm := newStatusVM(st, deps.Version, sessions.csrfToken())
		renderPartial(w, "status-partial", vm)
		return
	}

	if err := deps.SyncCtrl.TriggerNow(r.Context()); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Re-fetch status after triggering.
	st = deps.SyncCtrl.Status()
	vm := newStatusVM(st, deps.Version, sessions.csrfToken())
	w.WriteHeader(http.StatusAccepted)
	renderPartial(w, "status-partial", vm)
}

// handleDisconnectPost handles POST /ui/disconnect.
// Calls SyncController.Disconnect and returns the refreshed status partial.
func handleDisconnectPost(w http.ResponseWriter, r *http.Request, deps WebUIDeps, sessions *sessionStore) {
	if err := deps.SyncCtrl.Disconnect(); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	st := deps.SyncCtrl.Status()
	vm := newStatusVM(st, deps.Version, sessions.csrfToken())
	renderPartial(w, "status-partial", vm)
}

// handleConnectPost handles POST /ui/connect.
// Calls SyncController.Reconnect with the supplied credentials.
// On ErrInvalidWriterKey or ErrCredentialValidation it renders a friendly error
// in the status partial. writer_key is NEVER echoed back in any response.
func handleConnectPost(w http.ResponseWriter, r *http.Request, deps WebUIDeps, sessions *sessionStore) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	centralURL := r.FormValue("central_url")
	writerID := r.FormValue("writer_id")
	writerKey := r.FormValue("writer_key")
	// writer_key is read from the form for use in the call ONLY.
	// It is intentionally NOT stored in any variable that reaches the response.

	cfg := controlapi.CentralConfig{
		URL:                centralURL,
		WriterID:           writerID,
		WriterKeyPlaintext: writerKey,
	}
	// Zero writerKey immediately after use — belt-and-suspenders so it cannot
	// accidentally leak into a closure or stack frame visible to a debugger.
	writerKey = ""

	connectErr := deps.SyncCtrl.Reconnect(cfg)

	// Build the status partial. On error we embed the error text and echo back
	// the non-secret form values so the user does not have to retype them.
	// writer_key is NEVER echoed — on error the user must retype it.
	st := deps.SyncCtrl.Status()
	vm := newStatusVM(st, deps.Version, sessions.csrfToken())

	if connectErr != nil {
		switch {
		case errors.Is(connectErr, controlapi.ErrInvalidWriterKey):
			vm.ConnectError = "Invalid writer key — check that it is the correct 64-char hex HMAC key."
		case errors.Is(connectErr, controlapi.ErrCredentialValidation):
			vm.ConnectError = "Credential validation failed — check the central URL, writer ID, and key."
		default:
			vm.ConnectError = "internal error"
		}
		// Echo back non-secret values so the user can correct them without retyping.
		vm.ConnectCentralURL = centralURL
		vm.ConnectWriterID = writerID
		// writer_key is NOT echoed — the password field must be retyped.
		renderPartialStatus(w, "status-partial", vm, http.StatusUnprocessableEntity)
		return
	}

	renderPartial(w, "status-partial", vm)
}

// handleConfigPost handles POST /ui/config.
// Validates and applies the sync_interval patch via ConfigStore.Apply.
// Returns the refreshed config form partial on success or an error on failure.
func handleConfigPost(w http.ResponseWriter, r *http.Request, deps WebUIDeps, sessions *sessionStore) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	syncInterval := r.FormValue("sync_interval")

	// Validate duration server-side (mirrors controlapi.handleConfigPut logic).
	// Validation failures return 422 — a 200-with-error-text would read as
	// success to HTMX and any non-browser client.
	if syncInterval != "" {
		d, err := time.ParseDuration(syncInterval)
		if err != nil {
			vm := buildConfigVM(deps, sessions, "invalid sync_interval: must be a Go duration (e.g. 30s)", false)
			vm.SyncInterval = syncInterval // echo back the invalid value
			renderPageStatus(w, configTmpl, vm, http.StatusUnprocessableEntity)
			return
		}
		if d <= 0 {
			vm := buildConfigVM(deps, sessions, "invalid sync_interval: must be positive", false)
			vm.SyncInterval = syncInterval
			renderPageStatus(w, configTmpl, vm, http.StatusUnprocessableEntity)
			return
		}
	}

	patch := controlapi.ConfigPatch{}
	if syncInterval != "" {
		patch.SyncInterval = &syncInterval
	}

	restartRequired, err := deps.ConfigStore.Apply(patch)
	if err != nil {
		vm := buildConfigVM(deps, sessions, "internal error", false)
		renderPage(w, configTmpl, vm)
		return
	}

	vm := buildConfigVM(deps, sessions, "", restartRequired)
	renderPage(w, configTmpl, vm)
}
