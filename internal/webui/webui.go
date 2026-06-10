package webui

import (
	"net/http"
	"time"

	"github.com/mariesqu/engram/internal/controlapi"
)

// WebUIDeps are the read-only ports the web UI needs to serve its surfaces.
// These are satisfied by the wired daemon adapters (same instances as the
// controlapi.Server uses) — we do not duplicate the adapters.
type WebUIDeps struct {
	// SyncCtrl supplies the live Status for the status page and polling partial.
	SyncCtrl controlapi.SyncController

	// Store supplies the projects list with effective policies.
	Store controlapi.Store

	// Secret is the daemon bearer token used to validate the ?token= exchange.
	Secret string

	// Port is the TCP port the daemon is bound to.
	Port int

	// Version is the daemon version string embedded in the status page footer.
	Version string
}

// Mount registers all /ui/ routes on mux.
//
// Route table (PR-④a — read-only surfaces):
//
//	GET /ui/                    → full status page (session cookie required)
//	GET /ui/status              → HTMX partial status fragment (cookie required, polled every 3s)
//	GET /ui/projects            → read-only projects list (cookie required)
//	GET /ui/static/…            → embedded static assets (no auth — needed before session)
//
// Token exchange (runs before session guard):
//
//	GET /ui/?token=<bearer>     → validate token → set HttpOnly cookie → redirect to /ui/
func Mount(mux *http.ServeMux, deps WebUIDeps) {
	// Static assets — no auth. The browser needs htmx.min.js and styles.css
	// before any session cookie is established, including on the 401 page.
	staticServer := http.FileServer(http.FS(StaticFS))
	mux.Handle("/ui/static/", http.StripPrefix("/ui", staticServer))

	// All other /ui/ routes: token exchange + session-gated handlers.
	mux.Handle("/ui/", routeUI(deps))
}

// routeUI returns a handler that dispatches /ui/ sub-paths, running the token
// exchange before the session guard.
func routeUI(deps WebUIDeps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Token exchange: any /ui/* path with ?token= triggers the exchange.
		// This handles both /ui/?token=... (the canonical entry point from
		// cmd/engram/ui.go) and accidental deep links with a token param.
		if r.URL.Query().Get("token") != "" {
			exchangeToken(deps.Secret).ServeHTTP(w, r)
			return
		}

		// All other /ui/* routes require a valid session cookie.
		requireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			dispatchUI(w, r, deps)
		})).ServeHTTP(w, r)
	})
}

// dispatchUI routes within the cookie-authenticated /ui/* space.
func dispatchUI(w http.ResponseWriter, r *http.Request, deps WebUIDeps) {
	switch {
	case r.URL.Path == "/ui/" || r.URL.Path == "/ui":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleStatusPage(w, r, deps)

	case r.URL.Path == "/ui/status":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleStatusPartial(w, r, deps)

	case r.URL.Path == "/ui/projects":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleProjectsPage(w, r, deps)

	default:
		http.NotFound(w, r)
	}
}

// ── View models ──────────────────────────────────────────────────────────────

// statusViewModel is the template data for the status page and status partial.
// It flattens pointer fields from controlapi.Status into plain string values so
// html/template actions can compare them without nil-dereference panics.
type statusViewModel struct {
	CentralConnected bool
	CentralURL       string // empty when not configured
	LastSyncAt       string // RFC3339 or empty
	LastSyncError    string // empty when last sync succeeded or never ran
	LastSyncPushed   int
	LastSyncPulled   int
	DaemonVersion    string
}

// projectsViewModel is the template data for the projects page.
type projectsViewModel struct {
	Projects      []controlapi.ProjectPolicy
	DaemonVersion string
}

// newStatusVM converts a live controlapi.Status into a statusViewModel.
func newStatusVM(st controlapi.Status, version string) statusViewModel {
	vm := statusViewModel{
		CentralConnected: st.CentralConnected,
		DaemonVersion:    version,
		LastSyncPushed:   st.LastSyncResult.Pushed,
		LastSyncPulled:   st.LastSyncResult.Pulled,
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

// ── Handlers ─────────────────────────────────────────────────────────────────

func handleStatusPage(w http.ResponseWriter, _ *http.Request, deps WebUIDeps) {
	st := deps.SyncCtrl.Status()
	vm := newStatusVM(st, deps.Version)
	renderPage(w, statusTmpl, vm)
}

func handleStatusPartial(w http.ResponseWriter, _ *http.Request, deps WebUIDeps) {
	st := deps.SyncCtrl.Status()
	vm := newStatusVM(st, deps.Version)
	renderPartial(w, "status-partial", vm)
}

func handleProjectsPage(w http.ResponseWriter, _ *http.Request, deps WebUIDeps) {
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
	}
	renderPage(w, projectsTmpl, vm)
}
