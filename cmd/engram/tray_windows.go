//go:build windows

package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/tray"
)

const trayUsage = `Usage: engram tray [--db <path>]

Start the engram Windows system tray icon.

The tray connects to the resident daemon (engram daemon --http). If no daemon
is running, it automatically starts one in the background and waits for it to
become healthy before displaying the tray icon.

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

	cmd := exec.Command(exe, "daemon", "--db", dbPath, "--http")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
	// Do not inherit file handles (no console handle for the tray process).
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return tray.TrayConfig{}, fmt.Errorf("auto-launch daemon: %w", err)
	}

	// Wait for the daemon to write daemon.json and become healthy.
	// Bounded retry: 10s, 500ms intervals = 20 attempts.
	const (
		maxAttempts = 20
		interval    = 500 * time.Millisecond
	)

	for i := range maxAttempts {
		time.Sleep(interval)

		d, err := controlapi.ReadDaemonJSON(dbDir)
		if err != nil {
			continue // daemon.json not yet written
		}

		if probeErr := probeDaemonHTTP(d.Port, d.Token); probeErr == nil {
			return tray.TrayConfig{Port: d.Port, Token: d.Token, DBDir: dbDir, Version: version}, nil
		}
		_ = i
	}

	return tray.TrayConfig{}, fmt.Errorf("daemon did not become healthy within %s after auto-launch",
		time.Duration(maxAttempts)*interval)
}

// probeDaemonHTTP sends a GET /api/v1/status request and checks for a valid response.
// It is a lightweight probe that does not read daemon.json — the caller provides port+token.
func probeDaemonHTTP(port int, token string) error {
	client := &http.Client{Timeout: 2 * time.Second}
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
	return nil
}

// daemonJsonPath returns the path to daemon.json in a given DB directory.
// Exported only for tests; production code uses daemonDir(dbPath).
func daemonJsonPath(dbDir string) string {
	return filepath.Join(dbDir, "daemon.json")
}
