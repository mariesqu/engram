package webui

import (
	"bytes"
	"html/template"
	"net/http"
)

// sharedFuncs is the template.FuncMap applied to all template sets.
var sharedFuncs = template.FuncMap{
	// policyBadge returns safe HTML for a policy badge span.
	// The policy value is already validated by the store layer, but we
	// HTMLEscapeString it as a belt-and-suspenders measure against unknown values.
	"policyBadge": func(p string) template.HTML {
		switch p {
		case "synced":
			return `<span class="badge badge-synced">synced</span>`
		case "local-only":
			return `<span class="badge badge-local-only">local-only</span>`
		case "omitted":
			return `<span class="badge badge-omitted">omitted</span>`
		default:
			return template.HTML(`<span class="badge">` + template.HTMLEscapeString(p) + `</span>`)
		}
	},
	// truncateContent truncates s to at most n runes, appending "…" when truncated.
	"truncateContent": func(s string, n int) string {
		runes := []rune(s)
		if len(runes) <= n {
			return s
		}
		return string(runes[:n]) + "…"
	},
}

// pageTmpl is a parsed (layout + page) template pair for a full HTML page.
// Each page is parsed as an independent set so that multiple pages can each
// define a {{define "content"}} block without conflicting in the same set.
type pageTmpl struct {
	t *template.Template
}

// Page template sets — one per page, each parsed independently.
var (
	statusTmpl     *pageTmpl
	projectsTmpl   *pageTmpl
	configTmpl     *pageTmpl
	memoriesTmpl   *pageTmpl
	memoryEditTmpl *pageTmpl
)

// partialTmpl is the template set for HTMX polling partials (no layout wrapper).
// It includes all named partial defines used for HTMX swaps.
var partialTmpl *template.Template

func init() {
	// Status page: layout + status_partial (shared fragment) + status page wrapper.
	statusTmpl = mustParsePage(
		"templates/layout.html",
		"templates/status_partial.html",
		"templates/status.html",
	)

	// Projects page: layout + projects page (includes the projects-partial define).
	projectsTmpl = mustParsePage(
		"templates/layout.html",
		"templates/projects.html",
	)

	// Config page: layout + config form.
	configTmpl = mustParsePage(
		"templates/layout.html",
		"templates/config.html",
	)

	// Memories page: layout + memories page.
	memoriesTmpl = mustParsePage(
		"templates/layout.html",
		"templates/memories.html",
	)

	// Memory edit form page: layout + edit form.
	memoryEditTmpl = mustParsePage(
		"templates/layout.html",
		"templates/memory_edit.html",
	)

	// Partial template set: all named {{define}} blocks used for HTMX swaps.
	// This includes status-partial (polled every 3s) and projects-rows
	// (returned after a policy toggle POST).
	var err error
	partialTmpl, err = template.New("").Funcs(sharedFuncs).ParseFS(
		TemplatesFS,
		"templates/status_partial.html",
		"templates/projects_rows.html",
	)
	if err != nil {
		panic("webui: parse partial templates: " + err.Error())
	}
}

// mustParsePage parses a set of template files into a single template set.
// Panics if parsing fails — a broken embedded template is a programmer error
// that must be caught at startup, not silently swallowed at request time.
func mustParsePage(files ...string) *pageTmpl {
	t, err := template.New("").Funcs(sharedFuncs).ParseFS(TemplatesFS, files...)
	if err != nil {
		panic("webui: parse templates " + files[len(files)-1] + ": " + err.Error())
	}
	return &pageTmpl{t: t}
}

// renderPage executes the "layout" template (which includes the page-specific
// "content" block) and writes a full HTML page to w.
//
// The template is executed into a BUFFER first: executing straight into w
// would commit a 200 status on the first byte, so a mid-stream template error
// could only append "template error" to half-written HTML — a corrupted 200
// instead of a clean 500. Buffer-then-write makes the error path atomic.
// Cache-Control: no-store prevents any proxy or browser cache from serving a
// stale version of the dashboard.
func renderPage(w http.ResponseWriter, tmpl *pageTmpl, data any) {
	renderPageStatus(w, tmpl, data, http.StatusOK)
}

// renderPageStatus renders a full page with an explicit HTTP status code.
// Headers MUST be set before WriteHeader — the old WriteHeader-then-render
// pattern silently dropped Content-Type/Cache-Control on error responses.
func renderPageStatus(w http.ResponseWriter, tmpl *pageTmpl, data any, status int) {
	var buf bytes.Buffer
	if err := tmpl.t.ExecuteTemplate(&buf, "layout", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// renderPartial executes a named {{define}} block as an HTMX partial fragment.
// Buffered for the same atomic-error reason as renderPage.
// Cache-Control: no-store is mandatory — polled HTMX fragments must never be
// served from an HTTP or browser cache or the status display stops updating.
func renderPartial(w http.ResponseWriter, name string, data any) {
	renderPartialStatus(w, name, data, http.StatusOK)
}

// renderPartialStatus renders a partial with an explicit HTTP status code.
// See renderPageStatus for the header-ordering rationale.
func renderPartialStatus(w http.ResponseWriter, name string, data any, status int) {
	var buf bytes.Buffer
	if err := partialTmpl.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// render401 writes a plain 401 HTML page that hints the user to run `engram ui`.
// Intentionally avoids the layout template so it renders even when the session
// machinery has a problem.
func render401(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	const body = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>engram — not authenticated</title>
<link rel="stylesheet" href="/ui/static/styles.css">
</head>
<body>
<div class="page">
<div class="error-page">
<h1>Not authenticated</h1>
<p>Your session has expired or is missing.</p>
<p>Run <code>engram ui</code> to open a fresh authenticated session.</p>
</div>
</div>
</body>
</html>`
	_, _ = w.Write([]byte(body))
}
