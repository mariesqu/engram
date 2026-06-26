package webui

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
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

	// RemoteProjects returns the project names central knows, so the projects page
	// can mark which local projects also exist in the remote DB. Optional: when nil
	// (or when it returns nil because the daemon is disconnected) no remote markers
	// are shown — the state is simply "unknown".
	RemoteProjects func(ctx context.Context) ([]string, error)

	// Unshare removes a project from central over the authenticated wire (no DSN).
	// Optional: when nil the "unshare" delete scope is unavailable. Returns an error
	// when the daemon is not connected to central.
	Unshare func(ctx context.Context, project string) (int, error)

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

	case r.URL.Path == "/ui/memories":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleMemoriesPage(w, r, deps, sessions)

	case isMemoryDeletePath(r.URL.Path):
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id, _ := extractMemoryID(r.URL.Path, "/delete")
		withCSRF(sessions, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleMemoryDeletePost(w, r, deps, id)
		})).ServeHTTP(w, r)

	case isMemoryEditPath(r.URL.Path):
		id, _ := extractMemoryID(r.URL.Path, "/edit")
		switch r.Method {
		case http.MethodGet:
			handleMemoryEditGet(w, r, deps, sessions, id)
		case http.MethodPost:
			withCSRF(sessions, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handleMemoryEditPost(w, r, deps, sessions, id)
			})).ServeHTTP(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

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

	case isProjectDeletePath(r.URL.Path):
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		withCSRF(sessions, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleProjectDeletePost(w, r, deps, sessions)
		})).ServeHTTP(w, r)

	default:
		http.NotFound(w, r)
	}
}

// isMemoryDeletePath reports whether path matches /ui/memories/{id}/delete.
func isMemoryDeletePath(path string) bool {
	const prefix = "/ui/memories/"
	const suffix = "/delete"
	return isMemoryActionPath(path, prefix, suffix)
}

// isMemoryEditPath reports whether path matches /ui/memories/{id}/edit.
func isMemoryEditPath(path string) bool {
	const prefix = "/ui/memories/"
	const suffix = "/edit"
	return isMemoryActionPath(path, prefix, suffix)
}

// isMemoryActionPath is the shared structural check for memory sub-paths.
// Path must be /ui/memories/<id>/<action> where <id> is a non-empty numeric segment.
func isMemoryActionPath(path, prefix, suffix string) bool {
	if len(path) <= len(prefix)+len(suffix) {
		return false
	}
	if path[:len(prefix)] != prefix {
		return false
	}
	if path[len(path)-len(suffix):] != suffix {
		return false
	}
	middle := path[len(prefix) : len(path)-len(suffix)]
	if middle == "" || strings.Contains(middle, "/") {
		return false
	}
	_, err := strconv.ParseInt(middle, 10, 64)
	return err == nil
}

// extractMemoryID parses the integer id from a /ui/memories/{id}/{action} path.
// suffix is "/delete" or "/edit". Returns (0, false) on any parse error.
func extractMemoryID(path, suffix string) (int64, bool) {
	const prefix = "/ui/memories/"
	if len(path) <= len(prefix)+len(suffix) {
		return 0, false
	}
	middle := path[len(prefix) : len(path)-len(suffix)]
	id, err := strconv.ParseInt(middle, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// handleMemoryDeletePost handles POST /ui/memories/{id}/delete.
// On success it redirects to /ui/memories. On 404 it returns 404.
func handleMemoryDeletePost(w http.ResponseWriter, r *http.Request, deps WebUIDeps, id int64) {
	if err := deps.Store.DeleteMemory(id); err != nil {
		if isStoreNotFound(err) {
			http.Error(w, "memory not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/ui/memories", http.StatusSeeOther)
}

// handleMemoryEditGet handles GET /ui/memories/{id}/edit.
// Fetches the existing memory and renders the edit form.
func handleMemoryEditGet(w http.ResponseWriter, _ *http.Request, deps WebUIDeps, sessions *sessionStore, id int64) {
	memories, err := deps.Store.ListMemories("", "", 200)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Find the specific memory by id.
	var found *controlapi.MemorySummary
	for i := range memories {
		if memories[i].ID == id {
			found = &memories[i]
			break
		}
	}
	if found == nil {
		http.Error(w, "memory not found", http.StatusNotFound)
		return
	}
	vm := memoryEditViewModel{
		DaemonVersion: deps.Version,
		Memory:        *found,
		CSRFToken:     sessions.csrfToken(),
	}
	renderPage(w, memoryEditTmpl, vm)
}

// handleMemoryEditPost handles POST /ui/memories/{id}/edit.
// Applies the edit and redirects to /ui/memories on success.
func handleMemoryEditPost(w http.ResponseWriter, r *http.Request, deps WebUIDeps, sessions *sessionStore, id int64) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	content := strings.TrimSpace(r.FormValue("content"))
	typ := strings.TrimSpace(r.FormValue("type"))

	if title == "" || content == "" {
		// Re-render the form with an error.
		memories, _ := deps.Store.ListMemories("", "", 200)
		var mem controlapi.MemorySummary
		for _, m := range memories {
			if m.ID == id {
				mem = m
				break
			}
		}
		vm := memoryEditViewModel{
			DaemonVersion: deps.Version,
			Memory: controlapi.MemorySummary{
				ID:      id,
				Title:   title,
				Content: content,
				Type:    typ,
				Project: mem.Project,
				Scope:   mem.Scope,
			},
			CSRFToken: sessions.csrfToken(),
			Error:     "title and content are required",
		}
		renderPageStatus(w, memoryEditTmpl, vm, http.StatusUnprocessableEntity)
		return
	}

	_, err := deps.Store.UpdateMemory(id, title, content, typ)
	if err != nil {
		if isStoreNotFound(err) {
			http.Error(w, "memory not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/ui/memories", http.StatusSeeOther)
}

// isStoreNotFound reports whether an error from the store layer indicates a
// missing or deleted record. The localstore.ErrObservationNotFound is wrapped
// with context text; we detect it by message content.
func isStoreNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "observation not found") ||
		(strings.Contains(msg, "memory") && strings.Contains(msg, "not found"))
}

// isProjectDeletePath reports whether path matches /ui/projects/{project}/delete.
func isProjectDeletePath(path string) bool {
	const prefix = "/ui/projects/"
	const suffix = "/delete"
	if len(path) <= len(prefix)+len(suffix) {
		return false
	}
	if path[:len(prefix)] != prefix {
		return false
	}
	if path[len(path)-len(suffix):] != suffix {
		return false
	}
	middle := path[len(prefix) : len(path)-len(suffix)]
	return middle != "" && !strings.Contains(middle, "/")
}

// extractProjectFromDeletePath returns the project name from a
// /ui/projects/{project}/delete path. Returns "" on any parse error.
func extractProjectFromDeletePath(path string) string {
	const prefix = "/ui/projects/"
	const suffix = "/delete"
	raw := path[len(prefix) : len(path)-len(suffix)]
	decoded, err := url.PathUnescape(raw)
	if err != nil || strings.Contains(decoded, "/") {
		return ""
	}
	return decoded
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

// projectRow is one project plus whether it also exists in central. RemoteKnown
// is false when the daemon is disconnected (we can't tell), so the template shows
// nothing rather than a misleading "local only".
type projectRow struct {
	controlapi.ProjectPolicy
	InRemote    bool
	RemoteKnown bool
}

// projectsViewModel is the template data for the projects page.
type projectsViewModel struct {
	Projects         []projectRow
	CentralConnected bool // true → offer the "unshare" delete scope (needs central)
	DaemonVersion    string
	CSRFToken        string // ④b: needed for the policy toggle forms
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

// memoriesViewModel is the template data for the memories browse page.
type memoriesViewModel struct {
	DaemonVersion string
	Memories      []controlapi.MemorySummary
	Query         string // current search query (echoed for form)
	Project       string // current project filter (echoed for form)
	Searched      bool   // true when a query was submitted (vs first load)
	CSRFToken     string // needed for delete/edit forms
}

// memoryEditViewModel is the template data for the memory edit form page.
type memoryEditViewModel struct {
	DaemonVersion string
	Memory        controlapi.MemorySummary
	CSRFToken     string
	Error         string
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

func handleProjectsPage(w http.ResponseWriter, r *http.Request, deps WebUIDeps, sessions *sessionStore) {
	rows, connected, err := buildProjectRows(deps, r)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	vm := projectsViewModel{
		Projects:         rows,
		CentralConnected: connected,
		DaemonVersion:    deps.Version,
		CSRFToken:        sessions.csrfToken(),
	}
	renderPage(w, projectsTmpl, vm)
}

// buildProjectRows lists local projects with their effective policy and annotates
// each with whether central also has it. Best-effort: a nil or erroring
// RemoteProjects (daemon disconnected from central) leaves RemoteKnown=false, so
// the template shows no remote marker rather than a misleading one. The project
// name is matched case-insensitively (central-pulled names keep their case).
// Returns the rows plus whether central was reachable (remoteKnown) — the latter
// gates the "unshare" delete scope, which needs a live central connection.
func buildProjectRows(deps WebUIDeps, r *http.Request) ([]projectRow, bool, error) {
	projects, err := deps.Store.ListProjectsWithPolicy()
	if err != nil {
		return nil, false, err
	}

	var remoteSet map[string]bool
	remoteKnown := false
	if deps.RemoteProjects != nil {
		if names, rerr := deps.RemoteProjects(r.Context()); rerr == nil && names != nil {
			remoteKnown = true
			remoteSet = make(map[string]bool, len(names))
			for _, n := range names {
				remoteSet[strings.ToLower(strings.TrimSpace(n))] = true
			}
		}
	}

	rows := make([]projectRow, 0, len(projects))
	for _, p := range projects {
		rows = append(rows, projectRow{
			ProjectPolicy: p,
			RemoteKnown:   remoteKnown,
			InRemote:      remoteKnown && remoteSet[strings.ToLower(strings.TrimSpace(p.Name))],
		})
	}
	return rows, remoteKnown, nil
}

func handleConfigPage(w http.ResponseWriter, _ *http.Request, deps WebUIDeps, sessions *sessionStore) {
	vm := buildConfigVM(deps, sessions, "", false)
	renderPage(w, configTmpl, vm)
}

func handleMemoriesPage(w http.ResponseWriter, r *http.Request, deps WebUIDeps, sessions *sessionStore) {
	q := r.URL.Query()
	query := q.Get("q")
	project := q.Get("project")
	// "searched" is true any time the form was submitted (q or project param present).
	searched := query != "" || project != ""

	memories, err := deps.Store.ListMemories(query, project, 50)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	vm := memoriesViewModel{
		DaemonVersion: deps.Version,
		Memories:      memories,
		Query:         query,
		Project:       project,
		Searched:      searched,
		CSRFToken:     sessions.csrfToken(),
	}
	renderPage(w, memoriesTmpl, vm)
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
	rows, connected, err := buildProjectRows(deps, r)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	vm := projectsViewModel{
		Projects:         rows,
		CentralConnected: connected,
		DaemonVersion:    deps.Version,
		CSRFToken:        sessions.csrfToken(),
	}
	renderPartial(w, "projects-rows", vm)
}

// handleProjectDeletePost handles POST /ui/projects/{project}/delete.
//
// The form must include a "scope" field with value "local", "purge-all", or
// "unshare". Unshare removes the project from central over the AUTHENTICATED WIRE
// (deps.Unshare → POST /v1/unshare, signed with the writer key — no Postgres DSN
// in the daemon) and then sets the local policy to local-only so the node keeps
// its copy but stops re-pushing. It is offered only when the daemon is connected
// to central (CentralConnected gates the option). After a successful delete the
// projects list partial is re-rendered so the HTMX swap shows the updated state.
func handleProjectDeletePost(w http.ResponseWriter, r *http.Request, deps WebUIDeps, sessions *sessionStore) {
	project := extractProjectFromDeletePath(r.URL.Path)
	if project == "" {
		http.Error(w, "project name required", http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	scope := r.FormValue("scope")
	switch scope {
	case "local":
		if _, err := deps.Store.PurgeProjectLocal(project); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	case "purge-all":
		if _, err := deps.Store.TombstoneProject(project); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	case "unshare":
		// Remove from central over the authenticated wire (no DSN), then keep the
		// local copy but stop re-pushing by setting the policy to local-only.
		if deps.Unshare == nil {
			http.Error(w, "unshare unavailable: not connected to central", http.StatusConflict)
			return
		}
		if _, err := deps.Unshare(r.Context(), project); err != nil {
			http.Error(w, "unshare failed (central unreachable?)", http.StatusBadGateway)
			return
		}
		if err := deps.Store.SetPolicy(project, controlapi.PolicyLocalOnly); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	default:
		http.Error(w, "scope must be local, purge-all, or unshare", http.StatusBadRequest)
		return
	}

	// Re-render the projects tbody rows for the HTMX innerHTML swap.
	rows, connected, err := buildProjectRows(deps, r)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	vm := projectsViewModel{
		Projects:         rows,
		CentralConnected: connected,
		DaemonVersion:    deps.Version,
		CSRFToken:        sessions.csrfToken(),
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
		vm := newStatusVM(st, deps.Version, sessions.csrfToken())
		renderPartialStatus(w, "status-partial", vm, http.StatusConflict)
		return
	}

	if err := deps.SyncCtrl.TriggerNow(r.Context()); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Re-fetch status after triggering.
	st = deps.SyncCtrl.Status()
	vm := newStatusVM(st, deps.Version, sessions.csrfToken())
	renderPartialStatus(w, "status-partial", vm, http.StatusAccepted)
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
