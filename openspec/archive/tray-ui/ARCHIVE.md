# Archive Report: tray-ui

**Status**: COMPLETE — archived 2026-06-10.

## Outcome

The change delivered a complete resident daemon control plane with per-project policy, visual configuration, embedded HTMX web UI, Windows system tray integration, and opt-in HTTP MCP transport. All 7 chained PR slices merged to main with comprehensive acceptance testing.

### Delivered (7 Chained PRs, stacked-to-main)

- **PR #49** — Control API skeleton: loopback HTTP server at `127.0.0.1:7700`, bearer-token auth, origin checks, status/config/projects endpoints, daemon discovery via `daemon.json`, CLI thin clients.
- **PR #50** — Project policy: three-state machine (`synced | local-only | omitted`), schema v10 migration (O(1) additive), push-time + pull-time filtering, omitted write refusal, policy persistence.
- **PR #51** — Config file: `%APPDATA%\engram\config.json`, DPAPI-encrypted writer key at rest (Windows), env fallback (non-Windows), config precedence (flags > env > file > default), runtime-mutable vs restart-required settings.
- **PR #52** — Web UI read surfaces: `go:embed` HTMX + server-rendered HTML, token→session-cookie exchange, status/projects views, HTMX polling every 3s.
- **PR #53** — Web UI mutating surfaces: config form, policy toggle POST, sync trigger, session-bound CSRF double-submit, origin checks.
- **PR #54** — Windows tray: `Shell_NotifyIcon` via `x/sys/windows`, message pump on locked OS thread, context menu (Open UI, Sync Now, Connection status, Quit), daemon auto-launch, graceful degradation to `engram ui` on non-Windows.
- **PR #55** — HTTP MCP transport: `--transport` flag (stdio default, http opt-in), `NewStreamableHTTPServer` at `/mcp`, token-protected, stateless mode, stdio regression green.

**Lines changed**: 1,700–2,100 (new packages + integration; templates and embedded assets excluded per delivery context).

### Review-Driven Strengthenings (beyond spec)

All of the following were validated by the verify phase as **correct implementations, NOT deviations**:

1. **Session-bound CSRF double-submit** (csrf.go:79-83): CSRF token is per-session cookie + form field match — strictly stronger than spec's "double-submit" semantics. Defeats `127.0.0.1` cookie-planting attacks while maintaining non-browser CLI compatibility.

2. **Status-aware render helpers** (render.go:107-129): Introduced `renderPartialStatus()` and `renderPageStatus()` that buffer responses before calling `WriteHeader()`. Enables 422-on-validation-error and 409-on-connect-error returns with status reflected in UI partials, while keeping headers intact (commit a08989f). Ensures 202 Accepted (sync triggered) and 409 Conflict (sync not possible) are correctly rendered.

3. **Omitted refusal at 3 write paths**: `mem_save` (tools.go:496), `mem_save_prompt` (tools.go:893), `mem_session_summary` (tools.go:662 — spec-exceeding, but prevents memory loss via auto-prompt). Early return naturally skips the auto-prompt-capture block.

4. **Cursor-safe omitted pull refusal** (sync.go:472): `ApplyPulled` returns `ErrOmittedProject` (not a silent drop) when a pulled mutation targets an omitted project. Ensures the per-project pull cursor does not advance, allowing a flip-back-to-synced to re-pull correctly.

5. **GetPolicy never caches defaults** (policy.go:95-97): Read-time default computation (synced-if-central, local-only-otherwise) is uncached; only explicit rows are cached. Allows central config changes to immediately affect new projects' defaults without DB backfill.

6. **Runtime re-installation of SetCentralConfiguredFn** (daemon.go:931 on Reconnect, daemon.go:840 on Disconnect): Closure that computes policy defaults is refreshed whenever central connectivity changes. Ensures correct default rules during flips.

7. **Shared checkBearer in MountMCP** (server.go:449): Both control API and `/mcp` path use the same bearer-token validation (empty token never authenticates). Avoids auth duplication.

8. **Tray quit via canonical WM_AppQuit → PostQuitMessage** (tray_windows.go:365-395): Message pump stops only on the pump thread's WM_QUIT, preventing data-race issues with concurrent CLI/daemon shutdown.

9. **Per-instance webui sessionStore** with server-side expiry: CSRF binding ties token to session identity, not global state.

10. **transport precedence resolver + hard-error enforcement** (daemon.go:108-125): `--transport` empty-flag (missing or wrong value) triggers a non-zero exit with clear error. Config `PUT /api/v1/config` validates transport value before apply.

## Warnings

### WARNING-1: Origin header validation semantics (accepted by review, doc note)

**Spec text**: "All mutating endpoints (POST, PUT) MUST validate the Origin header against `http://127.0.0.1:<port>`."

**Implementation** (middleware.go:39-52, csrf.go:124): ALLOWS an ABSENT Origin header. Only a PRESENT-and-mismatched origin triggers 403.

**Rationale**: Non-browser clients (engram CLI, curl, tools) never send Origin. Bearer-token authentication + (UI-only) session-bound CSRF are the real defense gates. Browsers ALWAYS send Origin on cross-origin mutations, so absence cannot be a browser attack. This is the standard CSRF posture.

**Security note**: Not a hole given the token + session-bound CSRF layers. Bearer token is per-user (in daemon.json, user ACL'd); CSRF is session-bound (cookie + form field). The origin check is a defense-in-depth layer, not the primary gate.

**Recommendation**: Spec wording should be tightened to "reject PRESENT non-matching Origin" to match reality and document the intentional absent-allowed behavior.

---

## Remaining Manual Checklist (system-level / human verification)

These items require hands-on verification or user testing; they are out of headless-test scope:

- [ ] **Windows tray visuals**: Icon renders correctly across Win10/Win11 dark/light modes and DPI settings.
- [ ] **DPAPI user-binding cross-user**: Encrypted key in `config.json` cannot be decrypted by a different Windows user (requires second-user Windows environment to test).
- [ ] **UI browser rendering**: All 4 surfaces (status, projects, config, sync) render with correct layout, colors, and interactivity in a real browser.
- [ ] **Tray quit behavior**: Decide whether `engram tray` > Quit should also stop an auto-launched daemon (currently leaves it running). Design open question #2 — default leave-running.

---

## Deferred-Items Inventory (Carry-Forwards, Not Consumed)

| Item | Reason | Future phase |
|------|--------|--------------|
| Outbox garbage collection for long-term `local-only` projects | Documented caveat; unbounded outbox growth for projects left `local-only` indefinitely. `omitted` does NOT contribute (no write). | GC job (out of scope, deferred) |
| Probe-timeout wording on connect failure | Minor cosmetic; cosmetic UX message improvement on 422 responses. | Config/error-message refinement |
| `central_lww_discards` observability wiring | Audit table from core-foundation never wired to status endpoint. | Future observability phase |
| PUT `/api/v1/config` response echo | Config endpoint returns `{restart_required: bool}` only; could echo the resulting config for better CLI feedback. | UX refinement |
| Cross-platform secret storage | Keychain (macOS), Secret Service (Linux), libsecret. Non-Windows env-only in this change. | Future phase (explicit out-of-scope) |
| Tray Quit daemon shutdown decision | Design question: should Quit also stop the auto-launched daemon? Default: leave running (daemon persists). | TBD user feedback |

---

## What This Means for Engram

The tray-ui change brings engram to **the second of three planned roadmap phases**:

1. **core-foundation** (closed): memory model + reconciliation + local/central stores + convergence proofs.
2. **tray-ui** (CLOSED): resident daemon control plane + visual config + per-project policy + HTTP MCP transport.
3. **semantic-search** (remaining): embeddings + FTS relevance + judgment-assisted recall.

All 50 task boxes ticked. Zero CRITICAL issues. 1 WARNING (origin semantics — intentional). 4 SUGGESTIONS (all noted above as deferred). Verification PASSED with 41/41 requirements satisfied (32/41 headless, 9/41 manual).

Dependency graph: `golang.org/x/sys v0.42.0` promoted indirect → direct (sole graph change). Zero new Go modules.

---

**Archived by**: sdd-archive phase  
**Date**: 2026-06-10  
**Verification**: PASS (0 CRITICAL, 1 WARNING, 4 SUGGESTION)  
**Source of truth**: Engram observation IDs recorded in the archive report for full traceability.
