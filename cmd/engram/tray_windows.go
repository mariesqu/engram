//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/tray"
)

// trayEnsureRetryInitialDelay and trayEnsureRetryMaxDelay bound the backoff
// between ensureDaemon retries on the tray's cold-boot auto-launch path:
// initial 5s, doubling, capped at 60s — same schedule as 'engram serve's
// retryOpen (see serve.go's serveRetryInitialDelay/serveRetryMaxDelay).
// Reused here because ensureDaemon's own internal health-wait budget
// (daemonSpawnMaxAttempts * daemonSpawnInterval, ~10s) can legitimately be
// too short on a cold boot: with central reachable, daemon startup (remote
// purge check plus the first network roundtrips) can exceed 10s even though
// the daemon is not actually stuck — it just needs more time.
const (
	trayEnsureRetryInitialDelay = 5 * time.Second
	trayEnsureRetryMaxDelay     = 60 * time.Second
)

// trayEnsureRetryMaxAttempts bounds ensureDaemonRetrying so a PERMANENT
// failure (missing/corrupt binary, unwritable DB directory, ...) eventually
// surfaces instead of retrying invisibly forever with no tray icon and no
// indication anything is wrong. 15 attempts, each carrying ensureDaemon's
// own ~10s internal health-wait budget (daemonSpawnMaxAttempts *
// daemonSpawnInterval) on top of the 5s-doubling-to-60s backoff between
// attempts, totals roughly 14 minutes before giving up (assuming fast-failing
// probes, i.e. connection refused; slower if probes stall) — generous enough
// to ride out VPN/boot transients (a slow-to-reach central server delaying
// daemon startup) while still bounded.
const trayEnsureRetryMaxAttempts = 15

const trayUsage = `Usage: engram tray [--db <path>]

Start the engram Windows system tray icon.

The tray connects to the resident daemon (engram daemon --db <path> --http
--transport http). If no daemon is running, it automatically starts one in
the background and waits for it to become healthy before displaying the tray
icon. If the daemon does not become healthy in time (e.g. a cold boot with a
slow central server), the tray retries with backoff (5s, doubling, capped at
60s) instead of exiting immediately — up to 15 attempts (~14 minutes,
assuming fast-failing probes (connection refused); slower if probes stall)
before giving up and exiting with an error.

Menu items:
  Connected / Disconnected    — current central server status (non-interactive)
  Open UI                     — opens the web dashboard in the default browser
  Connect to central          — opens the web UI to the connect form
  Disconnect from central     — disconnects from central (stops sync)
  Sync Now                    — triggers an immediate sync cycle
  Quit                        — removes the tray icon (the daemon keeps running)

Flags:
  --db   Path to the local SQLite database (required; or set ENGRAM_DB)

Non-Windows: this subcommand is not available. Use 'engram ui' to open the
web UI in your default browser.
`

// runTrayCmd is the entry point for `engram tray` on Windows.
func runTrayCmd(args []string) error {
	fs := flag.NewFlagSet("tray", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), trayUsage) }

	db := fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("tray takes no positional arguments; unexpected: %v", fs.Args())
	}

	if *db == "" {
		*db = envOr("ENGRAM_DB", "")
	}
	if *db == "" {
		return fmt.Errorf("--db is required (or set ENGRAM_DB)")
	}

	dbDir := daemonDir(*db)

	// Probe the daemon and auto-launch if needed. Retries with backoff, up
	// to trayEnsureRetryMaxAttempts, instead of exiting on the first
	// transient failure — see ensureDaemonRetrying.
	cfg, err := ensureDaemonRetrying(dbDir, *db, ensureDaemon, time.Sleep, log.Printf)
	if err != nil {
		return fmt.Errorf("ensure daemon: %w", err)
	}

	return tray.Run(cfg)
}

// ensureDaemonRetrying calls ensureFn until it succeeds, retrying with
// exponential backoff (trayEnsureRetryInitialDelay doubling up to
// trayEnsureRetryMaxDelay) up to trayEnsureRetryMaxAttempts times, instead
// of giving up on the very first failure.
//
// Before this fix, a single ensureDaemon failure (most commonly
// waitForHealthyDaemon's ~10s budget expiring while a reachable-but-slow
// central server delayed daemon startup past that window) made 'engram tray'
// return an error and exit permanently — no tray icon until the user
// manually relaunched it. Retrying is safe FROM A CORRECTNESS standpoint:
// ensureFn re-reads daemon.json and probes for an existing healthy daemon on
// every call, only spawning when none is found, and this loop threads the
// running tracker returned by one attempt into the next (see ensureDaemon's
// doc comment) so a spawn that is merely slow to become healthy is never
// spawned a second time — at most one daemon child is ever in flight from
// THIS process across the whole retry loop. What retrying does NOT cover:
// the cross-process race between, say, the tray auto-launching at logon and
// 'engram connect' auto-starting at the same moment from an MCP client — two
// separate processes each running their own single-shot-or-retrying spawn
// decision with no shared coordination beyond daemon.json + the port itself
// (runDaemonHTTP's single-SQLite-owner guard lets the loser exit harmlessly)
// — that race is pre-existing and out of scope for this fix.
//
// The loop is bounded (not infinite) so a PERMANENT failure (missing/corrupt
// binary, unwritable DB directory, ...) surfaces as a real error instead of
// retrying invisibly forever with no tray icon and no indication anything is
// wrong — see trayEnsureRetryMaxAttempts.
//
// There is no ctx/signal plumbing around the tray's startup path (unlike
// serve.go's retryOpen or connect.go's ensureConnectDaemon): the tray has no
// signal.NotifyContext installed here — it blocks inside tray.Run until the
// user clicks Quit, and this whole process is otherwise only stopped by OS
// process termination. So this loop is deliberately simple, with no
// cancellation channel to select on. If the tray ever grows its own
// ctx/signal handling, thread a context through here the way retryOpen does.
//
// ensureFn, sleepFn, and logf are seams: production passes ensureDaemon,
// time.Sleep, and log.Printf; tests inject fakes so the retry/backoff
// behavior is exercised without real process spawns or wall-clock delay —
// mirrors the sleepFn/logf seam style established by serve.go's retryOpen.
func ensureDaemonRetrying(
	dbDir, dbPath string,
	ensureFn func(dbDir, dbPath string, running func() bool) (tray.TrayConfig, func() bool, error),
	sleepFn func(time.Duration),
	logf func(format string, args ...any),
) (tray.TrayConfig, error) {
	delay := trayEnsureRetryInitialDelay
	var running func() bool
	var lastErr error

	for attempt := 1; attempt <= trayEnsureRetryMaxAttempts; attempt++ {
		var cfg tray.TrayConfig
		cfg, running, lastErr = ensureFn(dbDir, dbPath, running)
		if lastErr == nil {
			return cfg, nil
		}

		if attempt == trayEnsureRetryMaxAttempts {
			logf("engram tray: ensure daemon failed after %d attempts, giving up: %v", attempt, lastErr)
			break
		}

		logf("engram tray: ensure daemon failed, retrying in %s: %v", delay, lastErr)
		sleepFn(delay)
		delay *= 2
		if delay > trayEnsureRetryMaxDelay {
			delay = trayEnsureRetryMaxDelay
		}
	}
	return tray.TrayConfig{}, lastErr
}

// ensureDaemon probes the daemon and, if not running, spawns it detached.
// Returns the TrayConfig (port + token from daemon.json) once the daemon is
// healthy.
//
// running is the tracker returned by a PREVIOUS retry's spawn (nil on the
// very first attempt, or once a previously-spawned child has exited): when
// running is non-nil and running() still reports true, this call does NOT
// spawn another daemon — it only re-runs the health wait — so a slow-to-
// start daemon (e.g. checkRemotePurgeOnStartup/PurgeForResync stretching
// past the ~10s per-attempt budget while central is reachable-but-slow)
// never accumulates a second, third, ... Nth concurrent daemon process, one
// per retry cycle. The (possibly new, possibly unchanged) tracker is always
// returned so ensureDaemonRetrying can thread it into the next attempt.
//
// Spawn + wait are shared with 'engram connect's auto-start (connect.go's
// ensureConnectDaemon) via spawnDetachedDaemonTracked / waitForHealthyDaemon
// (daemonspawn.go) — the two topologies (tray, connect) must behave
// identically here except for this retry tracking, which is tray-only
// (connect has no retry loop — see spawnDetachedDaemon's doc comment).
func ensureDaemon(dbDir, dbPath string, running func() bool) (tray.TrayConfig, func() bool, error) {
	return ensureDaemonWith(dbDir, dbPath, running, spawnDetachedDaemonTracked, time.Sleep, probeDaemonHTTP)
}

// ensureDaemonWith is the testable core of ensureDaemon; spawnFn, sleepFn,
// and probeHTTPFn are seams so tests can assert on the spawn-skip decision
// (and drive waitForHealthyDaemon's retry/timeout) without starting a real
// process or sleeping in real time — mirrors ensureConnectDaemonWith's seam
// style in connect.go.
func ensureDaemonWith(
	dbDir, dbPath string,
	running func() bool,
	spawnFn func(exe, dbPath string) (func() bool, error),
	sleepFn func(time.Duration),
	probeHTTPFn func(port int, token string) error,
) (tray.TrayConfig, func() bool, error) {
	// Try to connect to an existing daemon.
	if d, err := controlapi.ReadDaemonJSON(dbDir); err == nil {
		if probeErr := probeDaemon(dbDir, d.Port); probeErr == nil {
			return tray.TrayConfig{Port: d.Port, Token: d.Token, DBDir: dbDir, Version: version}, running, nil
		}
	}

	// No healthy daemon. Only spawn a new one if we are not already
	// tracking a still-running child from an earlier retry.
	if running == nil || !running() {
		exe, err := os.Executable()
		if err != nil {
			exe = "engram"
		}

		r, err := spawnFn(exe, dbPath)
		if err != nil {
			return tray.TrayConfig{}, running, fmt.Errorf("auto-launch daemon: %w", err)
		}
		running = r
	}

	d, err := waitForHealthyDaemon(context.Background(), dbDir, daemonSpawnMaxAttempts, daemonSpawnInterval,
		sleepFn, func(d controlapi.DaemonJSON) error { return probeHTTPFn(d.Port, d.Token) })
	if err != nil {
		return tray.TrayConfig{}, running, fmt.Errorf("%w after auto-launch", err)
	}
	return tray.TrayConfig{Port: d.Port, Token: d.Token, DBDir: dbDir, Version: version}, running, nil
}

// daemonJsonPath returns the path to daemon.json in a given DB directory.
// Exported only for tests; production code uses daemonDir(dbPath).
func daemonJsonPath(dbDir string) string {
	return filepath.Join(dbDir, "daemon.json")
}
