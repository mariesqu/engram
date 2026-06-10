# Tray Specification

## Purpose

Defines the Windows system tray integration that provides a persistent status indicator and quick-action menu. The tray is a thin client of the control API — it owns no state and performs no direct database operations.

---

## Requirements

### Requirement: Windows-only build constraint

The tray package MUST be tagged `//go:build windows` so it is compiled ONLY on Windows. Non-Windows builds MUST compile successfully without any tray code. The `engram tray` subcommand MUST be absent from non-Windows builds (or return an error indicating it is not available on the current platform).

#### Scenario: Non-Windows build excludes tray package

- GIVEN the engram binary is compiled on Linux or macOS
- WHEN the resulting binary is inspected
- THEN no `Shell_NotifyIcon`, tray, or `syscall.HWND` symbols are present

#### Scenario: engram tray on non-Windows returns graceful error

- GIVEN the binary is run on a non-Windows platform
- WHEN the user runs `engram tray`
- THEN the command exits with a clear error message indicating the tray is Windows-only
- AND the exit code is non-zero

---

### Requirement: Shell_NotifyIcon via golang.org/x/sys/windows

The tray implementation SHALL use `Shell_NotifyIcon` from `golang.org/x/sys/windows` (promoted from indirect to direct dependency). No new external Go modules MAY be introduced for the tray. `CGO_ENABLED=0` MUST remain valid for Windows builds.

#### Scenario: Windows build succeeds with CGO_ENABLED=0

- GIVEN `CGO_ENABLED=0` is set in the build environment on Windows
- WHEN `go build ./...` is run
- THEN the build succeeds and the binary includes the tray package

---

### Requirement: Tray menu contents

The tray context menu MUST include these items in order:
1. **Status indicator**: a non-interactive label showing the current central connectivity state (`Connected` / `Disconnected`). The tray icon itself MUST reflect the connectivity state using a distinct visual glyph (e.g., solid vs hollow).
2. **Open UI**: opens the browser at `http://127.0.0.1:<port>/ui/`.
3. **Connect / Disconnect** (context-sensitive label): if disconnected shows `Connect to central`; if connected shows `Disconnect from central`. Invoking it calls `POST /api/v1/central/connect` (showing a prompt for credentials if needed) or `POST /api/v1/central/disconnect`.
4. **Sync Now**: calls `POST /api/v1/sync/trigger`.
5. **Quit**: stops the resident daemon and removes the tray icon.

#### Scenario: Menu reflects connected state

- GIVEN the daemon is connected to central
- WHEN the user right-clicks the tray icon
- THEN the menu shows `Connected` in the status label and `Disconnect from central` as the action item

#### Scenario: Menu reflects disconnected state

- GIVEN the daemon is not connected to central
- WHEN the user right-clicks the tray icon
- THEN the menu shows `Disconnected` in the status label and `Connect to central` as the action item

#### Scenario: Open UI action opens the browser

- GIVEN the daemon is running and the user clicks `Open UI`
- WHEN the action is invoked
- THEN the default browser opens at `http://127.0.0.1:<port>/ui/`

#### Scenario: Sync Now action triggers a sync cycle

- GIVEN the daemon is connected to central
- WHEN the user clicks `Sync Now`
- THEN the tray invokes `POST /api/v1/sync/trigger` and the tray icon briefly indicates sync in progress

#### Scenario: Quit stops the daemon

- GIVEN the daemon is running
- WHEN the user clicks `Quit`
- THEN the daemon shuts down gracefully, the tray icon is removed, and the process exits

---

### Requirement: Daemon auto-launch from tray

When `engram tray` is invoked and the resident daemon is NOT already running, the tray SHALL automatically start the daemon in the background before displaying the tray icon. If the daemon is already running (control API responds on the configured port), the tray MUST attach to the existing daemon without starting a second instance.

#### Scenario: Tray starts daemon when not running

- GIVEN no daemon is running on the configured port
- WHEN the user runs `engram tray`
- THEN the tray starts the daemon, waits for the control API to become responsive, then displays the tray icon

#### Scenario: Tray attaches to existing daemon

- GIVEN a daemon is already running and responsive on port 7700
- WHEN the user runs `engram tray`
- THEN the tray attaches to the existing daemon without spawning a second process

---

### Requirement: Graceful degradation on non-Windows

On non-Windows platforms, the `engram ui` subcommand SHALL be the equivalent of the tray's `Open UI` action: it opens the default browser at the control API's web UI URL. This subcommand MUST be available on all platforms.

#### Scenario: engram ui opens browser on any platform

- GIVEN the daemon is running on any OS
- WHEN the user runs `engram ui`
- THEN the default browser opens at `http://127.0.0.1:<port>/ui/`

---

> **Headless testability**: Build-constraint exclusion (non-Windows compile), daemon auto-launch logic (mocked Shell_NotifyIcon), and control API call dispatch from menu actions are provable via unit tests with mocked syscalls. Visual tray icon rendering, icon glyph changes, and context menu appearance require manual verification on a Windows machine.
