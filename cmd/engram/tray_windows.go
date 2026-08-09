//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/tray"
)

const trayUsage = `Usage: engram tray [--db <path>]

Start the engram Windows system tray icon.

The tray connects to the resident daemon (engram daemon --db <path> --http
--transport http). If no daemon is running, it automatically starts one in
the background and waits for it to become healthy before displaying the tray
icon.

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

	// Probe the daemon and auto-launch if needed.
	cfg, err := ensureDaemon(dbDir, *db)
	if err != nil {
		return fmt.Errorf("tray: ensure daemon: %w", err)
	}

	return tray.Run(cfg)
}

// ensureDaemon probes the daemon and, if not running, spawns it detached.
// Returns the TrayConfig (port + token from daemon.json) once the daemon is healthy.
//
// Spawn + wait are shared with 'engram connect's auto-start (connect.go's
// ensureConnectDaemon) via spawnDetachedDaemon / waitForHealthyDaemon
// (daemonspawn.go) — the two topologies (tray, connect) must behave
// identically here, so the logic lives in one place.
func ensureDaemon(dbDir, dbPath string) (tray.TrayConfig, error) {
	// Try to connect to an existing daemon.
	if d, err := controlapi.ReadDaemonJSON(dbDir); err == nil {
		if probeErr := probeDaemon(dbDir, d.Port); probeErr == nil {
			return tray.TrayConfig{Port: d.Port, Token: d.Token, DBDir: dbDir, Version: version}, nil
		}
	}

	// No healthy daemon — auto-launch detached.
	exe, err := os.Executable()
	if err != nil {
		exe = "engram"
	}

	if err := spawnDetachedDaemon(exe, dbPath); err != nil {
		return tray.TrayConfig{}, fmt.Errorf("auto-launch daemon: %w", err)
	}

	d, err := waitForHealthyDaemon(context.Background(), dbDir, daemonSpawnMaxAttempts, daemonSpawnInterval,
		time.Sleep, func(d controlapi.DaemonJSON) error { return probeDaemonHTTP(d.Port, d.Token) })
	if err != nil {
		return tray.TrayConfig{}, fmt.Errorf("%w after auto-launch", err)
	}
	return tray.TrayConfig{Port: d.Port, Token: d.Token, DBDir: dbDir, Version: version}, nil
}

// daemonJsonPath returns the path to daemon.json in a given DB directory.
// Exported only for tests; production code uses daemonDir(dbPath).
func daemonJsonPath(dbDir string) string {
	return filepath.Join(dbDir, "daemon.json")
}
