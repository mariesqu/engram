package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"time"

	"github.com/mariesqu/engram/internal/controlapi"
)

// daemonSpawnMaxAttempts and daemonSpawnInterval bound how long callers wait
// for a freshly-spawned daemon to become healthy: 20 * 500ms = 10s, matching
// the tray's original auto-launch budget (see git history of
// tray_windows.go's ensureDaemon). Shared by both 'engram tray's auto-launch
// and 'engram connect's auto-start (connect.go's ensureConnectDaemon) so the
// two topologies behave identically.
const (
	daemonSpawnMaxAttempts = 20
	daemonSpawnInterval    = 500 * time.Millisecond
)

// buildSpawnCmd constructs (but does not start) the *exec.Cmd for a resident
// daemon (`<exe> daemon --db <dbPath> --http --transport http`), fully
// detached from the current process via platform-specific "detach from
// parent" process attributes (detachedSysProcAttr —
// CREATE_NEW_PROCESS_GROUP|DETACHED_PROCESS on Windows, Setsid on Unix; see
// spawn_windows.go / spawn_other.go) and no inherited stdio handles.
//
// --transport http is required (not just --http) so the spawned daemon
// mounts /mcp — a daemon spawned without it would leave 'engram connect'
// clients failing preflight with the "running WITHOUT the MCP HTTP
// transport" 404 error.
//
// Split out from spawnDetachedDaemon so tests can assert on the constructed
// command's Args/SysProcAttr without actually starting a process.
func buildSpawnCmd(exe, dbPath string) *exec.Cmd {
	cmd := exec.Command(exe, "daemon", "--db", dbPath, "--http", "--transport", "http")
	cmd.SysProcAttr = detachedSysProcAttr()
	// Do not inherit file handles — neither the tray nor 'engram connect'
	// (an MCP stdio bridge) should share stdin/stdout/stderr with a daemon
	// meant to keep running long after the caller exits.
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd
}

// spawnDetachedDaemon starts the daemon built by buildSpawnCmd, fully
// detached from the current process: the daemon must outlive the caller
// (the tray quits, 'engram connect's MCP client disconnects, ...).
//
// After a successful Start(), a reaper goroutine calls cmd.Wait() so the OS
// can reclaim the child's exit status whenever it exits. This matters
// because detachedSysProcAttr's Setsid (spawn_other.go) does NOT reparent
// the child to init — on Unix, a losing racer that exits early (e.g.
// "refusing to start a second SQLite owner") would otherwise remain a
// zombie for the lifetime of this long-lived parent (e.g. 'engram
// connect'). The Wait() runs asynchronously and never blocks the caller.
func spawnDetachedDaemon(exe, dbPath string) error {
	cmd := buildSpawnCmd(exe, dbPath)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// waitForHealthyDaemon polls dir for daemon.json plus a passing probeFn, up
// to attempts*interval, returning the DaemonJSON once healthy. Used after
// spawnDetachedDaemon to block until the newly-spawned daemon (or, in a
// concurrent-start race, whichever daemon WON the port — see
// runDaemonHTTP's "refusing to start a second SQLite owner" in daemon.go)
// has written daemon.json and is answering /api/v1/status.
//
// sleepFn and probeFn are seams: production callers pass time.Sleep and
// probeDaemonHTTP; tests inject fakes so the retry/timeout logic can be
// exercised without real processes or wall-clock delay.
func waitForHealthyDaemon(
	ctx context.Context,
	dir string,
	attempts int,
	interval time.Duration,
	sleepFn func(time.Duration),
	probeFn func(controlapi.DaemonJSON) error,
) (controlapi.DaemonJSON, error) {
	for range attempts {
		select {
		case <-ctx.Done():
			return controlapi.DaemonJSON{}, ctx.Err()
		default:
		}

		sleepFn(interval)

		d, err := controlapi.ReadDaemonJSON(dir)
		if err != nil {
			continue // daemon.json not yet written
		}
		if probeErr := probeFn(d); probeErr == nil {
			return d, nil
		}
	}
	return controlapi.DaemonJSON{}, fmt.Errorf("daemon did not become healthy within %s",
		time.Duration(attempts)*interval)
}

// probeDaemonHTTP sends a GET /api/v1/status request and checks for a valid
// response. It is a lightweight probe that does not read daemon.json — the
// caller provides port+token (typically from a fresh controlapi.ReadDaemonJSON).
// A 200 status alone is not enough: a stale daemon.json can record a port
// that has since been reused by an unrelated loopback service, so the body
// is decoded as controlapi.Status and must carry a non-empty DaemonVersion
// before the probe is trusted (mirrors probeDaemon's check in daemon.go).
//
// Timeout is 5s, not 2s: /api/v1/status does real per-project work and was
// measured at ~3s on an 84-project DB before the CountPendingEmbeddings N+1
// fix — a 2s timeout made this probe falsely report "unhealthy" on a live,
// healthy daemon, causing 'engram connect' to spawn a doomed duplicate. A
// dead daemon still fails near-instantly (connection refused), so the
// ~10s spawn wait budget (daemonSpawnMaxAttempts * daemonSpawnInterval)
// holds for that case; a daemon that accepts the TCP connection but stalls
// on the response can cost up to the full 5s per attempt, stretching the
// worst case to attempts*(interval+timeout) (~110s) — the larger timeout
// only trades that worst case for correctness on the healthy-but-slow path.
func probeDaemonHTTP(port int, token string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/api/v1/status", port), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe: status %d", resp.StatusCode)
	}
	var st controlapi.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil || st.DaemonVersion == "" {
		return fmt.Errorf("probe: not an engram daemon status response")
	}
	return nil
}
