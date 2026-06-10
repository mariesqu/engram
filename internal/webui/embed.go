// Package webui provides the embedded HTMX web interface served by the engram
// resident daemon at /ui/. It uses html/template (stdlib) and go:embed — no
// JS build step, no code generation, no new Go module dependencies.
//
// Design decision #8 (design.md): html/template (stdlib) + go:embed + vendored
// htmx.min.js 2.0.4. Does NOT use a-h/templ (old_code precedent), which
// requires a code-gen step and introduces a new module dependency.
//
// HTMX version: 2.0.4
// Source URL:   https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js
// File size:    50917 bytes
// Vendored at:  internal/webui/static/htmx.min.js
package webui

import "embed"

// TemplatesFS embeds the HTML template files and static assets.
// Used by render.go to parse templates at package init.
//
//go:embed templates static
var TemplatesFS embed.FS

// StaticFS embeds only the static asset subtree.
// Served at /ui/static/ via http.FileServer without authentication so that the
// browser can load htmx.min.js and styles.css before a session is established
// (needed to render the 401 page correctly).
//
//go:embed static
var StaticFS embed.FS
