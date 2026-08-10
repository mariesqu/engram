package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// daemonSpawnLogName is the file (written next to daemon.json, i.e. in
// daemonDir(dbPath)) that captures a spawned daemon's combined stdout+stderr
// — see openSpawnLog. Before this fix, buildSpawnCmd discarded the child's
// stdio entirely, so a daemon that failed during startup left no trace
// anywhere: the operator would see "did not become healthy" from
// waitForHealthyDaemon with no way to find out why.
const daemonSpawnLogName = "daemon-spawn.log"

// daemonSpawnLogMaxBytes bounds daemon-spawn.log's size before it is rotated
// (or, when rotation is blocked, truncated — see rotateSpawnLogIfNeeded) on
// the next spawn attempt. This file is wired as the CHILD daemon's stdout
// AND stderr for its entire resident lifetime (see buildSpawnCmd) — it
// receives not just startup lines but everything the daemon process ever
// writes to those streams while it runs, including net/http's default error
// logger (runDaemonHTTP installs no custom ErrorLog). 5MB is generous
// headroom for that, while still bounding worst-case disk usage on a
// long-lived machine. Rotation is attempted at the START of the NEXT spawn
// (tray auto-launch or connect auto-start), by which point the previous
// daemon is normally either gone or about to be replaced — but if a prior
// daemon is still resident and holding the file open, os.Rename fails on
// Windows (no FILE_SHARE_DELETE) and rotateSpawnLogIfNeeded falls back to
// truncating the file in place instead.
const daemonSpawnLogMaxBytes = 5 * 1024 * 1024

// buildSpawnCmd constructs (but does not start) the *exec.Cmd for a resident
// daemon (`<exe> daemon --db <dbPath> --http --transport http`), fully
// detached from the current process via platform-specific "detach from
// parent" process attributes (detachedSysProcAttr —
// CREATE_NEW_PROCESS_GROUP|DETACHED_PROCESS on Windows, Setsid on Unix; see
// spawn_windows.go / spawn_other.go).
//
// --transport http is required (not just --http) so the spawned daemon
// mounts /mcp — a daemon spawned without it would leave 'engram connect'
// clients failing preflight with the "running WITHOUT the MCP HTTP
// transport" 404 error.
//
// The child's stdin is never inherited (nil) — nothing should write to it.
// Its stdout and stderr are wired to the SAME daemon-spawn.log file (see
// openSpawnLog) so a daemon that fails during startup leaves a diagnosable
// trace instead of vanishing into the null device. An *os.File works as
// Cmd.Stdout/Stderr under both of this package's detach mechanisms: Go's
// os/exec marks file handles it is given as inheritable and passes them
// directly to CreateProcess on Windows (independent of the
// DETACHED_PROCESS/CREATE_NEW_PROCESS_GROUP flags, which only control
// console attachment, not handle inheritance) and dup's them into the
// child's fd table on Unix (independent of Setsid, which only controls
// session/controlling-terminal detachment) — the same mechanism ordinary Go
// programs rely on to redirect a subprocess's output to a log file. If the
// log file cannot be opened for any reason, buildSpawnCmd falls back
// silently to nil (discard) — a missing log must NEVER block a spawn.
//
// Split out from spawnDetachedDaemon so tests can assert on the constructed
// command's Args/SysProcAttr/Stdout/Stderr without actually starting a
// process.
func buildSpawnCmd(exe, dbPath string) *exec.Cmd {
	cmd := exec.Command(exe, "daemon", "--db", dbPath, "--http", "--transport", "http")
	cmd.SysProcAttr = detachedSysProcAttr()
	cmd.Stdin = nil

	if logFile, err := openSpawnLog(dbPath, cmd.Args); err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	return cmd
}

// spawnLogNeedsRotation reports whether a spawn log of the given size should
// be rotated before being reopened for a new spawn attempt. Split out as a
// pure function (no filesystem access) so the daemonSpawnLogMaxBytes
// threshold decision is unit-testable with a fake size, without needing to
// actually write 5MB to disk in a test.
func spawnLogNeedsRotation(size int64) bool {
	return size >= daemonSpawnLogMaxBytes
}

// rotateSpawnLogIfNeeded renames the spawn log at path to path+".old"
// (overwriting any previous .old — os.Rename replaces an existing
// destination on both Windows and Unix) when it has grown past
// daemonSpawnLogMaxBytes. Best-effort: a Stat or Rename failure here is not
// itself fatal — per this fix's contract, a log-side problem must never
// block a spawn.
//
// The rename ALWAYS fails while the previous daemon is still resident and
// holding the file open for writing (its stdout/stderr — see
// buildSpawnCmd): Windows opens file handles without FILE_SHARE_DELETE by
// default, so a rename (an implicit delete-and-recreate of the directory
// entry) of an open file is rejected outright — this is not a race, it is
// the normal case whenever daemon-spawn.log grows past the cap while a
// daemon is alive and still logging. When the rename fails, fall back to
// best-effort truncation in place: os.Truncate is allowed under
// FILE_SHARE_WRITE, and any O_APPEND writer (including the live daemon's
// inherited handle — see openSpawnLog) simply continues writing at the new
// (zero) EOF afterward. This loses the .old backup for that rotation, but
// keeps the live file bounded instead of growing unbounded forever.
func rotateSpawnLogIfNeeded(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return // no existing log (first-ever spawn) or unreadable — nothing to rotate
	}
	if !spawnLogNeedsRotation(info.Size()) {
		return
	}
	if err := os.Rename(path, path+".old"); err != nil {
		// Most likely cause: a still-resident daemon holds path open (no
		// FILE_SHARE_DELETE on Windows). Truncate in place instead so the
		// file does not grow without bound; best-effort, never fatal.
		_ = os.Truncate(path, 0)
	}
}

// openSpawnLog opens (creating if absent, appending if present)
// daemon-spawn.log next to dbPath's daemon.json (daemonDir(dbPath)),
// rotating it first via rotateSpawnLogIfNeeded, and writes a one-line header
// naming the spawn args before returning. The header is written here, from
// the PARENT, synchronously before buildSpawnCmd's caller ever calls
// cmd.Start() — so even a child that dies immediately after starting still
// leaves a trace of the attempt that was made.
//
// Returns a non-nil error on any failure (dbPath's directory missing,
// permission denied, disk full, ...); callers MUST treat that as "no log
// available" and fall back to discarding the child's stdio (see
// buildSpawnCmd) rather than failing the spawn itself.
func openSpawnLog(dbPath string, args []string) (*os.File, error) {
	path := filepath.Join(daemonDir(dbPath), daemonSpawnLogName)

	rotateSpawnLogIfNeeded(path)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(f, "=== %s spawning: %s ===\n", time.Now().UTC().Format(time.RFC3339), strings.Join(args, " "))
	return f, nil
}

// spawnDetachedDaemon starts the daemon built by buildSpawnCmd, fully
// detached from the current process: the daemon must outlive the caller
// (the tray quits, 'engram connect's MCP client disconnects, ...).
//
// Thin wrapper around spawnDetachedDaemonTracked that discards the "is it
// still running" handle — 'engram connect's single-shot auto-start
// (ensureConnectDaemon) has no retry loop to feed it to, so it has no use
// for the tracker. Kept as a separate, stable entry point so connect.go's
// call site and its test seam (ensureConnectDaemonWith's spawnFn parameter)
// are untouched by the tray's at-most-one-in-flight retry tracking (see
// spawnDetachedDaemonTracked).
func spawnDetachedDaemon(exe, dbPath string) error {
	_, err := spawnDetachedDaemonTracked(exe, dbPath)
	return err
}

// spawnDetachedDaemonTracked starts the daemon built by buildSpawnCmd,
// fully detached from the current process (see spawnDetachedDaemon), and
// additionally returns a running func that reports whether the spawned
// child has not yet exited.
//
// This exists so a caller that RETRIES (tray_windows.go's
// ensureDaemonRetrying / ensureDaemon) can tell "my previous spawn is still
// starting up" apart from "my previous spawn already died" — without it, a
// retry loop has no way to avoid spawning a second concurrent daemon while
// the first one is merely slow (e.g. a reachable-but-slow central server
// stretching out checkRemotePurgeOnStartup/PurgeForResync past the ~10s
// per-attempt health-wait budget): both daemons would open the same SQLite
// database (WAL mode holds no exclusive OS lock) and race for the port,
// and a HUNG child would leak one more detached orphan process per retry
// cycle, unbounded. See ensureDaemon's doc comment for how the tracker is
// threaded across retries.
//
// After a successful Start(), a reaper goroutine calls cmd.Wait() so the OS
// can reclaim the child's exit status whenever it exits, and closes the
// done channel that running() checks. This also matters because
// detachedSysProcAttr's Setsid (spawn_other.go) does NOT reparent the
// child to init — on Unix, a losing racer that exits early (e.g. "refusing
// to start a second SQLite owner") would otherwise remain a zombie for the
// lifetime of this long-lived parent (e.g. 'engram connect'). The Wait()
// runs asynchronously and never blocks the caller.
//
// If buildSpawnCmd opened a spawn log, its *os.File is closed here (both on
// a failed and a successful Start()) once the child has had a chance to
// inherit the handle — the parent has no further use for its own copy, and
// holding it open would needlessly keep the file locked against a later
// spawn attempt's rotation (see rotateSpawnLogIfNeeded's truncate fallback
// for when that lock is instead held by the CHILD for its own lifetime).
func spawnDetachedDaemonTracked(exe, dbPath string) (running func() bool, err error) {
	cmd := buildSpawnCmd(exe, dbPath)
	logFile, _ := cmd.Stdout.(*os.File) // nil if openSpawnLog failed (discard fallback)

	startErr := cmd.Start()
	if logFile != nil {
		_ = logFile.Close()
	}
	if startErr != nil {
		return nil, startErr
	}

	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	running = func() bool {
		select {
		case <-done:
			return false
		default:
			return true
		}
	}
	return running, nil
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
