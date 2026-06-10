# Tasks: tray-ui — Resident Daemon Control Plane + Per-Project Policy + DPAPI Config + HTMX UI + Windows Tray + HTTP MCP

## Review Workload Forecast

| Field | Value |
|-------|-------|
| Estimated changed lines | 1,700 – 2,100 (new packages dominate; templates/embedded assets excluded) |
| 400-line budget risk | High |
| Chained PRs recommended | Yes |
| Suggested split | PR-① control-api → PR-② project-policy → PR-③ config-file → PR-④a web-ui read → PR-④b web-ui mutate → PR-⑤ tray → PR-⑥ mcp-http-transport |
| Delivery strategy | chained PRs |
| Chain strategy | stacked-to-main |

Decision needed before apply: No
Chained PRs recommended: Yes
Chain strategy: stacked-to-main
400-line budget risk: High

> PR-④ split rationale: the full web-ui package (4 templates + HTMX embed + CSRF + all handlers) would exceed 400 lines. Split into ④a (read-only surfaces: status + projects view, token→cookie exchange, embed scaffold) and ④b (mutating surfaces: config form + policy toggle + sync trigger + CSRF double-submit). Templates and embedded static assets are excluded from the line-count budget per the delivery-context rule.

> `golang.org/x/sys` indirect→direct go.mod promotion: sanctioned in PR-① (first import of `x/sys/windows` in `internal/controlapi` for `daemon.json` ACL). The reviewer MUST accept this go.mod change as intentional.

### Suggested Work Units

| Unit | Goal | Likely PR | Est. lines | Budget risk |
|------|------|-----------|-----------|-------------|
| 1 | Control-API skeleton: daemon --http, daemon.json, /api/v1 read endpoints, engram status/ui CLI | PR-① | ~350 | Low |
| 2 | Project-policy: schema v10, push/pull filters, omitted refusal, policy endpoints, engram projects CLI | PR-② | ~300 | Low |
| 3 | Config-file: internal/config, SecretBox DPAPI/env, PUT config, central connect/disconnect, engram config/sync CLI | PR-③ | ~250 | Low |
| 4a | Web-UI read: go:embed scaffold, layout.html, status partial, projects view, token→cookie exchange, session cookie | PR-④a | ~280 | Low |
| 4b | Web-UI mutate: config form, policy toggle POST, sync trigger POST, CSRF double-submit, origin check | PR-④b | ~250 | Low |
| 5 | Tray: Shell_NotifyIcon windows build-tagged, message pump, menu→API mapping, auto-launch, stub | PR-⑤ | ~250 | Low |
| 6 | MCP HTTP transport: --transport flag, StreamableHTTPServer at /mcp same listener, docs, regression | PR-⑥ | ~200 | Low |

---

## PR-① control-api skeleton

> Spec: control-api. Satisfies: loopback-only bind, bearer-token auth, origin check (GET exemption), status/config/projects read, daemon.json token discovery, CLI status+ui.
> Verify gate: `CGO_ENABLED=0 go build ./...`; `go vet ./... && go vet -tags windows ./...`; `go test ./...`; acceptance tests for handler suite.
> go.mod: promote `golang.org/x/sys` indirect→direct here (sanctioned — first `x/sys/windows` import for daemon.json ACL). Reviewer must accept this change.

- [x] 1.1 `go.mod`: change `golang.org/x/sys v0.42.0 // indirect` to direct (remove `// indirect` comment). Test: `go mod tidy` leaves it direct; `CGO_ENABLED=0 go build ./...` green.
- [x] 1.2 `internal/controlapi/server.go`: define `Server` struct with `Store`, `SyncController`, `ConfigStore` port interfaces (as per design topology); `Handler() http.Handler` returning a `ServeMux` with bearer-token middleware (`withAuth`) and `writeJSON`/`writeError` helpers; bind-address hardcoded `127.0.0.1`. Test: `TestControlAPI_Auth_MissingToken → 401`; `TestControlAPI_Auth_WrongToken → 401`; `TestControlAPI_Auth_ValidToken → passes middleware`.
- [x] 1.3 `internal/controlapi/middleware.go`: `withAuth(token string) func(http.Handler) http.Handler` — validates `Authorization: Bearer <token>`; `withOrigin(port int) func(http.Handler) http.Handler` — checks `Origin` header on POST/PUT, exempts GET; returns 403 on mismatch. Test: `TestOriginCheck_POST_WrongOrigin → 403`; `TestOriginCheck_GET_NoOriginHeader → passes`.
- [x] 1.4 `internal/controlapi/status.go`: `GET /api/v1/status` handler — queries `SyncController.Status()` and returns `{central_connected, central_url?, last_sync_result{at,error,pushed,pulled}, daemon_version}`; omits `central_url` when empty. Test: `TestStatus_Connected`, `TestStatus_NoCentralURL_OmitsField`, `TestStatus_AfterFailedSync`.
- [x] 1.5 `internal/controlapi/config.go`: `GET /api/v1/config` handler — calls `ConfigStore.Load()` and redacts writer key (`"***REDACTED***"` when set, field absent when not set). Test: `TestConfigRead_WriterKeyRedacted`; `TestConfigRead_WriterKeyAbsent`.
- [x] 1.6 `internal/controlapi/projects.go` (read-only stub): `GET /api/v1/projects` handler — calls `Store.ListProjectsWithPolicy()` and serializes `[{name, policy}]` array; returns empty array on empty store. Test: `TestProjectsList_WithPolicy`; `TestProjectsList_Empty`.
- [x] 1.7 `internal/controlapi/daemon_json.go` (`//go:build windows` + non-Windows pair): `WriteDaemonJSON(dir, token string, port, pid int)` — atomic write (temp + rename) with Windows user-only ACL via `x/sys/windows.SetNamedSecurityInfo`; non-Windows uses `os.Chmod(0600)`. `ReadDaemonJSON(dir) (DaemonJSON, error)`. Test: `TestDaemonJSON_RoundTrip` (platform-neutral); Windows-only `TestDaemonJSON_ACL` guarded `//go:build windows`.
- [x] 1.8 `cmd/engram/daemon.go` (modify): add `--http` bool flag and `--http-port` int flag (default 7700); when `--http` is set, generate 32-byte hex token, write `daemon.json`, start `net.Listen("tcp", "127.0.0.1:<port>")`, mount `controlapi.Handler()`, start `http.Server`; detect port-in-use via status probe (healthy engram daemon → "already running", else clear bind error). Test: `TestDaemon_HTTP_AlreadyRunning_Refuses`; `TestDaemon_HTTP_BindsLoopbackOnly`.
- [x] 1.9 `cmd/engram/controlclient.go`: `ControlClient{port, token}` read from `daemon.json`; `Get(path)`, `Post(path, body)`, `Put(path, body)` helpers with `Authorization: Bearer` header; on 401 re-read `daemon.json` once then return "daemon not running / restarting". Test: `TestControlClient_Stale401_Retries`.
- [x] 1.10 `cmd/engram/status.go` + `cmd/engram/ui.go`: `engram status` subcommand — reads `daemon.json`, calls `GET /api/v1/status`, prints `central_connected`, `last_sync_result`; `engram ui` — opens default browser at `http://127.0.0.1:<port>/ui/` (cross-platform `open`/`xdg-open`/`cmd /c start`). Test: `TestCLI_Status_PrintsOutput` (httptest); `TestCLI_UI_NoDaemon_Errors`.
- [x] 1.11 `internal/controlapi/acceptance_test.go` (`//go:build acceptance`): spin real `controlapi.Handler()` over `httptest.NewServer` + real temp SQLite store; assert token-missing → 401, bad origin on POST → 403, `/api/v1/status` shape, `/api/v1/config` redaction, `/api/v1/projects` array. Test: `TestAcceptance_ControlAPI_Suite` (≥6 sub-cases).
- [x] 1.12 `README.md`: add "Resident daemon" section documenting `engram daemon --http`, `--http-port`, `engram status`, `engram ui` flags and behavior. Keep existing stdio section unchanged.

---

## PR-② project-policy

> Spec: project-policy. Satisfies: three-state policy, push filter, pull filter, omitted refusal, flip semantics, default policy, schema v10, policy persistence.
> Verify gate: `CGO_ENABLED=0 go build ./...`; `go vet ./... && go vet -tags windows ./...`; `go test ./...`; acceptance tests.

- [x] 2.1 `internal/localstore/schema.go` (modify): add `CREATE TABLE IF NOT EXISTS project_policy (project TEXT PRIMARY KEY, policy TEXT NOT NULL DEFAULT 'synced' CHECK(policy IN ('synced','local-only','omitted')), updated_at TEXT NOT NULL DEFAULT (datetime('now')))` to `ApplySchema`; bump `currentSchemaVersion` 9→10; add `migrateV9ToV10(db *sql.DB) error` function (creates table only, O(1), no row backfill) + `if ver < 10 { migrateV9ToV10(db); ver = 10 }` block in `runMigrations`; update `_ = ver` trailing comment. Test: `TestMigration_V9ToV10_TableCreated`; `TestMigration_V9ToV10_Idempotent`; `TestSchema_V10_CheckConstraint`.
- [x] 2.2 `internal/localstore/policy.go`: `GetPolicy(project string) (Policy, error)` — reads `project_policy`; absent row returns computed default (synced if central URL set, local-only otherwise) using a `IsCentralConfigured() bool` closure injected at `Store` construction; `SetPolicy(project string, p Policy) error` — upsert; `ListProjectsWithPolicy() ([]ProjectPolicy, error)` — LEFT JOIN `memories` + `project_policy` to enumerate all known projects with their effective policy. Test: `TestGetPolicy_AbsentRow_DefaultSynced`; `TestGetPolicy_AbsentRow_DefaultLocalOnly`; `TestSetPolicy_Upsert`; `TestListProjectsWithPolicy_MixedPolicies`.
- [x] 2.3 `internal/syncer/syncer.go` (modify): in `Push` drain consumer loop, add per-entry policy check `store.GetPolicy(entry.Mutation.Project)` — skip (continue without `AckMutation`) when `!= synced`; in `SyncAllProjects`, filter each project from `ListProjects()` — skip when policy `!= synced`. Policy map cached on `Store` and invalidated on `SetPolicy`. Test: `TestPush_SkipsLocalOnly`; `TestPush_SkipsOmitted`; `TestSyncAllProjects_ExcludesLocalOnly`; `TestFlip_LocalOnly_To_Synced_DrainsPending`.
- [x] 2.4 `cmd/engram/tools.go` (modify): in `handleSave` and `handleSavePrompt`, after `resolveSaveProject(...)` and BEFORE `AddObservation`/`AddPrompt`, call `store.GetPolicy(project)` and if `== Omitted` return `mcp.NewToolResultError("project %q is omitted: capture refused")`. Test: `TestHandleSave_Omitted_ReturnsError_WritesNothing` (assert zero new rows + zero outbox); `TestHandleSavePrompt_Omitted_ReturnsError`.
- [x] 2.5 `internal/controlapi/projects.go` (extend): add `PUT /api/v1/projects/{project}/policy` handler — decode `{"policy":"..."}`, validate value, call `store.SetPolicy`, return 200 or 400; wire `withOrigin` middleware. Test: `TestPolicyUpdate_Valid → 200`; `TestPolicyUpdate_InvalidValue → 400`.
- [x] 2.6 `cmd/engram/projects.go`: `engram projects list` — calls `GET /api/v1/projects`, prints table of name + policy; `engram projects policy <project> <synced|local-only|omitted>` — calls `PUT /api/v1/projects/{project}/policy`. Test: `TestCLI_Projects_List` (httptest); `TestCLI_Projects_Policy_Valid`; `TestCLI_Projects_Policy_Invalid → non-zero exit`.
- [x] 2.7 `internal/localstore/policy_acceptance_test.go` (`//go:build acceptance`): real store + in-process fake central; `mem_save` on local-only project — assert zero central pushes; `mem_save` on synced project — assert one push; omitted project — assert error + zero rows + zero outbox; flip local-only→synced → sync cycle drains 3 accumulated entries; v10 migration idempotency (run twice, table exists, user_version=10). Test: `TestAcceptance_Policy_Suite` (≥7 sub-cases).
- [x] 2.8 `README.md`: add "Per-project policy" section documenting `engram projects list`, `engram projects policy`, and the three policy states with their semantics.

---

## PR-③ config-file

> Spec: visual-config. Satisfies: config file location, precedence order, DPAPI-encrypted key at rest, never-leak guarantees, runtime-mutable vs restart-required, round-trip fidelity.
> Verify gate: `CGO_ENABLED=0 go build ./...`; `go vet ./... && go vet -tags windows ./...`; `go test ./...`; acceptance tests; `TestRun_DaemonHelp_DoesNotLeakWriterKey` stays green.

- [x] 3.1 `internal/config/config.go`: `Config` struct `{DB, Central{URL, WriterID}, EncryptedWriterKey []byte, HTTP{Port}, SyncInterval, LogLevel, Transport}`; `Load(dir string) (Config, error)` — reads `config.json` if present, else returns zero-value; `Save(dir string, c Config) error` — marshal → write temp + rename atomic; `Redact() RedactedConfig` — returns copy with `WriterKey` replaced by `"***REDACTED***"` or omitted; `Patch(base Config, patch ConfigPatch) (Config, bool)` — merge-patch, bool = restart-required. Test: `TestConfig_RoundTrip`; `TestConfig_AbsentFile_ReturnsDefault`; `TestConfig_Redact_KeySet`; `TestConfig_Redact_KeyAbsent`; `TestConfig_Patch_RuntimeMutable_NoRestart`; `TestConfig_Patch_RestartRequired`.
- [x] 3.2 `internal/config/secret.go`: `SecretBox` interface `{ Seal([]byte) ([]byte, error); Open([]byte) ([]byte, error) }`.
- [x] 3.3 `internal/config/secret_windows.go` (`//go:build windows`): `WindowsSecretBox` — `Seal` calls `CryptProtectData` (user scope) via `x/sys/windows`; `Open` calls `CryptUnprotectData`; on decrypt failure returns `ErrNoSecretStore` (caller falls back to env). Test: `TestDPAPI_SealOpen_RoundTrip` (`//go:build windows`); `TestDPAPI_Open_InvalidBlob_ReturnsError` (`//go:build windows`).
- [x] 3.4 `internal/config/secret_other.go` (`//go:build !windows`): `EnvOnlySecretBox` — `Seal` returns `ErrNoSecretStore`; `Open` returns `ErrNoSecretStore`. Test: `TestEnvOnly_Seal_ReturnsError` (`//go:build !windows`).
- [x] 3.5 `cmd/engram/daemon.go` (modify): replace env-only config resolution (lines 115-167) with `config.Load(appDataDir)` + flag/env merge via `config.Patch` precedence; writer key resolution: `ENGRAM_WRITER_KEY` env wins → else `secretBox.Open(c.EncryptedWriterKey)` → else "writer key required" status. Test: `TestDaemon_ConfigPrecedence_FlagWinsOverFile`; `TestDaemon_ConfigPrecedence_EnvWinsOverFile`; `TestRun_DaemonHelp_DoesNotLeakWriterKey` (existing — must stay green).
- [x] 3.6 `internal/controlapi/config.go` (extend): `PUT /api/v1/config` handler — decode partial JSON body, reject `writer_key` or `central_url` fields (400), call `config.Patch`, call `configStore.Apply(patch)`; return `{restart_required: bool}`. Test: `TestPUT_Config_RuntimeMutable_200_RestartFalse`; `TestPUT_Config_RestartRequired_200_RestartTrue`; `TestPUT_Config_WriterKeyField_400`; `TestPUT_Config_CentralURLField_400`.
- [x] 3.7 `internal/controlapi/central.go`: `POST /api/v1/central/connect` handler — decode `{central_url, writer_key}`, probe connection (head request or status check), on success DPAPI-seal the key and persist via `configStore.Apply`; on failure return 422 (config not persisted); `POST /api/v1/central/disconnect` — clear central fields, halt sync, return 200 with local data untouched. Test: `TestConnect_ValidCreds_200_StatusConnected`; `TestConnect_InvalidCreds_422_ConfigNotPersisted`; `TestDisconnect_200_SyncHalted`.
- [x] 3.8 `cmd/engram/config.go` + `cmd/engram/sync.go`: `engram config get` — calls `GET /api/v1/config`, prints redacted JSON; `engram config set <key> <value>` — calls `PUT /api/v1/config` with single-field patch; `engram sync now` — calls `POST /api/v1/sync/trigger`, prints result. Test: `TestCLI_ConfigGet` (httptest); `TestCLI_ConfigSet_RuntimeMutable`; `TestCLI_SyncNow_Connected`; `TestCLI_SyncNow_Disconnected → exit non-zero`.
- [x] 3.9 `internal/controlapi/sync.go`: `POST /api/v1/sync/trigger` handler — if disconnected return 409 `{"error":"central not configured"}`; else call `syncCtrl.TriggerNow(ctx)` return 202. Test: `TestSyncTrigger_Connected_202`; `TestSyncTrigger_Disconnected_409`.
- [x] 3.10 `internal/config/acceptance_test.go` (`//go:build acceptance`): config round-trip identical after restart; DPAPI seal/open on Windows (guarded); `Redact()` never returns key; connect/disconnect flips status. Test: `TestAcceptance_Config_Suite` (≥4 sub-cases).
- [x] 3.11 `README.md`: add "Config file" section documenting `%APPDATA%\engram\config.json`, `engram config get/set`, `engram sync now`, DPAPI behavior, and env-var override.

---

## PR-④a web-ui read-only surfaces

> Spec: web-ui (read surfaces + token exchange + embed scaffold). Satisfies: go:embed, served at /ui/, token→cookie exchange, status surface, projects view (read-only), HTMX polling.
> Verify gate: `CGO_ENABLED=0 go build ./...`; `go vet ./...`; `go test ./...`.

- [x] 4a.1 `internal/webui/static/htmx.min.js`: vendor HTMX (download and commit the minified JS file; single file, no CDN). Test: build succeeds and file present in binary via `go:embed`.
- [x] 4a.2 `internal/webui/static/styles.css`: minimal CSS (loopback dashboard). Committed as a static file.
- [x] 4a.3 `internal/webui/embed.go`: `//go:embed templates static` declarations → two `embed.FS` vars (`Templates`, `Static`). Test: `TestEmbed_AllFilesPresent` (iterate FS, assert `htmx.min.js`, `styles.css`, `layout.html`, `status.html`, `projects.html` present).
- [x] 4a.4 `internal/webui/templates/layout.html`: base page layout with HTMX script tag (from embedded `/static/htmx.min.js`), nav links, `{{block "content"}}` slot.
- [x] 4a.5 `internal/webui/templates/status.html`: status partial template; `hx-trigger="every 3s"` on the status fragment; shows `central_connected`, `last_sync_result.at`, `last_sync_result.error`, `daemon_version`.
- [x] 4a.6 `internal/webui/templates/projects.html`: read-only projects list template; renders `[{name, policy}]` rows with policy badge (no forms yet — mutating forms come in ④b).
- [x] 4a.7 `internal/webui/render.go`: `html/template` parse-on-init (parse once at package init from `embed.FS`); `renderPartial(w, name, data)` and `renderPage(w, name, data)` helpers. Test: `TestRender_StatusPartial_ContainsExpectedField`; `TestRender_ProjectsList_RendersRows`.
- [x] 4a.8 `internal/webui/session.go`: `exchangeToken(secret string) http.Handler` — validates `?token=` query param against bearer secret, sets `HttpOnly; SameSite=Strict; Secure=false; Path=/ui/` session cookie, redirects to `/ui/` (strips token); `requireSession(next http.Handler) http.Handler` — validates session cookie, redirects to `/ui/?token=` (prompts re-entry) on missing/invalid cookie. Test: `TestTokenExchange_ValidToken_SetsCookieAndRedirects`; `TestTokenExchange_InvalidToken_401`; `TestRequireSession_NoCookie_Redirect`; `TestRequireSession_ValidCookie_Passes`.
- [x] 4a.9 `internal/webui/webui.go` (read routes): `Mount(mux *http.ServeMux, deps WebUIDeps)` — registers `GET /ui/` (full page), `GET /ui/status` (HTMX partial, polling), `GET /ui/projects` (projects list read-only); wraps all with `requireSession`; mounts token exchange at `GET /ui/?token=`. Test: `TestWebUI_GetStatus_200_HTML`; `TestWebUI_GetProjects_200_HTML`; `TestWebUI_NoSession_RedirectsToExchange`; `TestWebUI_PollingPartial_Status`.
- [x] 4a.10 `README.md`: add "Web UI" section intro documenting `/ui/` URL, token exchange via tray or `engram ui`, and HTMX polling behavior.

---

## PR-④b web-ui mutating surfaces

> Spec: web-ui (mutating surfaces + CSRF). Satisfies: config form, policy toggle POST, sync trigger POST, CSRF double-submit, origin check on mutations.
> Verify gate: `CGO_ENABLED=0 go build ./...`; `go vet ./...`; `go test ./...`.

- [x] 4b.1 `internal/webui/templates/config.html`: config form template; all fields editable except `central_url`/`writer_key` (those shown read-only, managed via connect/disconnect); `restart_required` notice rendered conditionally; CSRF hidden field `{{.CSRFToken}}`.
- [x] 4b.2 `internal/webui/csrf.go`: `generateCSRF() (cookieVal, formVal string, err error)` — 16-byte random hex; `validateCSRF(r *http.Request) bool` — reads cookie + form field, constant-time compare; `withCSRF(next http.Handler) http.Handler` — middleware for mutating routes. Test: `TestCSRF_Valid_Passes`; `TestCSRF_MissingCookie_Rejected`; `TestCSRF_MismatchFormVsCookie_Rejected`; `TestCSRF_ReplayOnDifferentSession_Rejected`.
- [x] 4b.3 `internal/webui/webui.go` (mutating routes, extend): add `POST /ui/projects/{project}/policy`, `GET /ui/config`, `POST /ui/config`, `POST /ui/sync`; wrap mutating routes with `withCSRF` + `requireSession`; `POST /ui/config` calls `configStore.Apply(patch)`, returns updated config partial or restart notice; `POST /ui/projects/{project}/policy` calls `store.SetPolicy`, returns refreshed projects partial (HTMX swap). Test: `TestWebUI_PolicyToggle_POST_Valid_200`; `TestWebUI_PolicyToggle_POST_CSRFMissing_403`; `TestWebUI_Config_POST_Valid_200`; `TestWebUI_Sync_POST_Connected_202`; `TestWebUI_Sync_POST_Disconnected_409`; `TestWebUI_Origin_WrongOrigin_403`.
- [x] 4b.4 `internal/webui/templates/projects.html` (extend from ④a): add policy toggle `<form>` with HTMX `hx-post` and CSRF hidden field per-row; `hx-target` swaps just the projects partial.
- [x] 4b.5 `internal/webui/acceptance_test.go` (`//go:build acceptance`): real `httptest.Server` with mounted webui + real temp SQLite store; assert: `/ui/` returns HTML; policy toggle POST mutates + returns partial; CSRF rejection on mutating routes; origin rejection; token→cookie exchange full flow. Test: `TestAcceptance_WebUI_Suite` (≥6 sub-cases).
- [x] 4b.6 `README.md`: extend Web UI section with config form and policy toggle usage; note CSRF protection and origin allowlist.

---

## PR-⑤ tray

> Spec: tray. Satisfies: Windows-only build constraint, Shell_NotifyIcon, message pump, menu contents, daemon auto-launch, graceful degradation.
> Verify gate: `CGO_ENABLED=0 go build ./...` (all platforms); `CGO_ENABLED=0 GOOS=windows go build ./...`; `go vet ./... && go vet -tags windows ./...`; unit tests (mock syscalls).

- [x] 5.1 `internal/tray/tray_stub.go` (`//go:build !windows`): `Run(cfg TrayConfig) error { return ErrUnsupported }` — ensures non-Windows compiles. Test: `TestTray_NonWindows_ReturnsUnsupported` (`//go:build !windows`).
- [x] 5.2 `internal/tray/icon.go` (`//go:build windows`): `//go:embed engram.ico`; commit a minimal monochrome `.ico` file alongside. Test: build includes ico (embed compile check).
- [x] 5.3 `internal/tray/pump_windows.go` (`//go:build windows`): `RunMessagePump(hwnd syscall.HWND, quit <-chan struct{})` on `runtime.LockOSThread()`-locked goroutine; `GetMessage`/`TranslateMessage`/`DispatchMessage` loop; exits on `WM_QUIT` or `quit` channel. Test: `TestPump_ExitsOnQuit` (mock Win32 calls via interface).
- [x] 5.4 `internal/tray/tray_windows.go` (`//go:build windows`): `Shell_NotifyIcon` add/modify/delete via `x/sys/windows`; `buildMenu(status StatusSnapshot) []MenuItem`; menu-action handlers dispatch HTTP calls via buffered channel to a separate goroutine (pump never blocks on HTTP); status polling (every 5s) refreshes icon glyph + menu label. Test: `TestMenu_ConnectedState_ShowsDisconnect`; `TestMenu_DisconnectedState_ShowsConnect`; `TestMenuAction_SyncNow_CallsAPI` (mock HTTP client); `TestMenuAction_OpenUI_LaunchesBrowser`; `TestMenuAction_Quit_StopsPump`.
- [x] 5.5 `cmd/engram/tray.go`: `engram tray` subcommand — reads `daemon.json`; if daemon absent/unreachable, spawns `engram daemon --http` detached via `os/exec` + `SysProcAttr{CreationFlags: CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS}`; bounded retry until `daemon.json` exists + `/api/v1/status` healthy (max 10s, 500ms intervals); then calls `tray.Run(cfg)`. Stub exits `ErrUnsupported` on non-Windows. Test: `TestTray_AutoLaunch_NoDaemon` (mock exec + mock status poll); `TestTray_AttachExisting_NoDuplicate`.
- [x] 5.6 `README.md`: add "Windows tray" section documenting `engram tray`, daemon auto-launch behavior, menu items, and non-Windows degradation via `engram ui`.

---

## PR-⑥ mcp-http-transport

> Spec: mcp-http-transport (reconciled: /mcp on SAME listener/port, no separate --mcp-http-port). Satisfies: stdio default, --transport http flag, StreamableHTTPServer at /mcp, identical tool surface, stdio regression.
> Verify gate: `CGO_ENABLED=0 go build ./...`; `go vet ./... && go vet -tags windows ./...`; `go test ./...`; `go test -tags acceptance ./...`; `TestRun_DaemonHelp_DoesNotLeakWriterKey` green.

- [x] 6.1 `cmd/engram/daemon.go` (modify): add `--transport` string flag (`stdio` | `http`, default `stdio`); with `--transport http` and `--http` active, mount `mcp-go`'s `NewStreamableHTTPServer(mcpServer)` at `/mcp` on the existing top-level `ServeMux` (same listener — no new port); with `--transport stdio` (or no flag) behavior is identical to pre-change. Test: `TestDaemon_Transport_Stdio_Default`; `TestDaemon_Transport_HTTP_MountsMCP`; `TestRun_DaemonHelp_DoesNotLeakWriterKey` (existing — must stay green).
- [x] 6.2 `internal/controlapi/server.go` (extend): expose `MountMCP(mux *http.ServeMux, mcpServer MCPHandler)` helper that registers `Handle("/mcp", ...)` with bearer-token middleware; `MCPHandler` is an interface satisfied by `*mcp.StreamableHTTPServer`. Test: `TestMCPMount_TokenMissing_401`; `TestMCPMount_ValidToken_ForwardsToHandler`.
- [x] 6.3 `cmd/engram/daemon_mcp_acceptance_test.go` (`//go:build acceptance`): start daemon with `--transport http` in-process (`httptest.NewServer`); call `mem_save` then `mem_search` via the HTTP MCP transport; assert search returns saved observation; assert policy enforcement (`omitted` project → error). Test: `TestAcceptance_MCPHTTPTransport_RoundTrip`; `TestAcceptance_MCPHTTPTransport_OmittedRefusal`.
- [x] 6.4 `cmd/engram/daemon_test.go` (modify): run full stdio regression suite; add regression assertion that `/mcp` path is absent when `--transport stdio`. Test: existing tests stay green; `TestDaemon_Stdio_MCPPathAbsent`.
- [x] 6.5 `README.md`: add "HTTP MCP transport" section with Claude Code config snippet (`--transport http`, `Authorization` header, `/mcp` path) and explicit note that stdio remains the default; existing configs require no change.
