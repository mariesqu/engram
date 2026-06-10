# Web UI Specification

## Purpose

Defines the embedded browser-based UI served by the resident daemon at `/ui/`. The UI provides visual surfaces for status, project policy, configuration, and sync — accessible from the tray menu or `engram ui` on any platform.

---

## Requirements

### Requirement: Embedded static assets (go:embed)

All UI assets (HTML templates, HTMX script, CSS, and any other static files) MUST be embedded in the daemon binary using `go:embed`. No external CDN fetches or runtime asset downloads are permitted. The UI MUST be fully functional in an environment with no internet access.

#### Scenario: UI loads with no external network

- GIVEN the daemon is running with no internet access
- WHEN a client browser navigates to `http://127.0.0.1:7700/ui/`
- THEN all UI assets load from the binary and the page renders without any failed network requests

#### Scenario: Binary embeds all required UI files

- GIVEN the engram binary is built
- WHEN the binary is inspected (e.g., via `go:embed` test or strings check)
- THEN HTMX script, base CSS, and all HTML template files are present inside the binary

---

### Requirement: Served at /ui/ on the loopback server

The web UI SHALL be served under the `/ui/` path on the same loopback HTTP server as the control API (`127.0.0.1:<port>`). Navigation to `http://127.0.0.1:<port>/ui/` MUST return a valid HTML page. The UI shares the loopback-only bind and bearer-token constraints of the control API server.

#### Scenario: /ui/ returns HTML

- GIVEN the daemon is running
- WHEN a client sends `GET http://127.0.0.1:7700/ui/`
- THEN the server responds `200 OK` with `Content-Type: text/html`

---

### Requirement: Token-to-cookie exchange for browser sessions

Browser clients cannot set `Authorization: Bearer` headers on navigation. The UI SHALL implement a token-to-cookie exchange: on first visit the browser navigates to `GET /ui/?token=<bearer-token>`, the server validates the token, issues a session cookie, and redirects to `/ui/` stripping the token from the URL. The cookie MUST be `HttpOnly`, `SameSite=Strict`, `Secure=false` (loopback — TLS is not used on localhost), and scoped to `127.0.0.1`. Mutating UI actions (POST/PUT via HTMX) MUST additionally carry a CSRF token using the double-submit cookie pattern: the CSRF token is embedded as a hidden form field and also stored in a cookie; the server validates that both match. The `Origin` header MUST also be checked against `http://127.0.0.1:<port>` on all mutating routes.

#### Scenario: Token-to-cookie exchange on first visit

- GIVEN a user navigates to `http://127.0.0.1:7700/ui/?token=<bearer-token>`
- WHEN the server validates the token
- THEN the server issues an `HttpOnly; SameSite=Strict; Secure=false` session cookie, also sets a CSRF double-submit cookie, and redirects to `/ui/` (token stripped from URL)
- AND subsequent UI requests use the session cookie and do not need the token in the URL

#### Scenario: Cookie-authenticated UI request is accepted

- GIVEN a browser holds a valid session cookie from the token exchange
- WHEN the browser requests any `/ui/` page
- THEN the server processes the request without requiring the Authorization header

---

### Requirement: Four required UI surfaces

The web UI MUST provide the following surfaces:
1. **Status page**: displays `central_connected`, `last_sync_result`, and `daemon_version` (sourced from `GET /api/v1/status`).
2. **Projects + policy surface**: lists all projects with their current policy; allows toggling a project's policy between `synced`, `local-only`, and `omitted` (invokes `PUT /api/v1/projects/{project}/policy`).
3. **Config form**: displays current config (writer key redacted); allows editing runtime-mutable fields and displays a "restart required" notice for restart-required changes (invokes `PUT /api/v1/config`).
4. **Sync trigger**: a button that invokes `POST /api/v1/sync/trigger` and displays the outcome.

#### Scenario: Status surface reflects actual daemon state

- GIVEN the daemon is connected to central and the last sync succeeded
- WHEN a user views the status page
- THEN the page shows `central_connected: true` and the last sync timestamp

#### Scenario: Policy toggle invokes the control API

- GIVEN the projects surface lists project "my-proj" with policy `synced`
- WHEN the user selects `local-only` for "my-proj" and confirms
- THEN the UI invokes `PUT /api/v1/projects/my-proj/policy` with `{"policy": "local-only"}`
- AND the projects list refreshes to show `local-only` for "my-proj"

#### Scenario: Config form redacts writer key

- GIVEN the config form is displayed
- WHEN the form renders
- THEN the writer key field shows `***REDACTED***` and does NOT render the actual key value in the HTML source

#### Scenario: Sync trigger button invokes POST /api/v1/sync/trigger

- GIVEN the user is on the sync trigger surface and central is connected
- WHEN the user clicks the sync trigger button
- THEN the UI sends `POST /api/v1/sync/trigger` and displays the result (accepted or error reason)

---

### Requirement: Mutating UI actions require origin validation

All mutating actions from the UI (POST, PUT via HTMX) MUST include the `Origin: http://127.0.0.1:<port>` header. The server MUST validate this origin as required by the control API origin check requirement. UI-triggered mutations without a valid origin MUST be rejected with `403`.

#### Scenario: HTMX mutating request includes correct origin

- GIVEN the user triggers a policy toggle via the UI
- WHEN the HTMX request is sent
- THEN the request includes `Origin: http://127.0.0.1:7700` and the server accepts it

---

> **Headless testability**: HTTP handler unit tests for all `/ui/` routes, token-to-cookie exchange, and origin validation are provable headlessly. Visual rendering of the four surfaces requires a browser and is manual verification.
