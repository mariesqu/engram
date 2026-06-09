//go:build !windows

package controlapi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DaemonJSON is the discovery file written by the resident daemon at startup.
// It lets the CLI and tray find the daemon's port and bearer token without
// requiring an environment variable.
//
// File location: <db-dir>/daemon.json
// Permissions:   0600 (non-Windows) / user-only ACL (Windows)
// Atomicity:     written to a temp file in the same directory, then renamed.
// Lifecycle:     written on daemon start, removed on clean shutdown.
type DaemonJSON struct {
	Port      int    `json:"port"`
	Token     string `json:"token"`
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

// WriteDaemonJSON atomically writes a DaemonJSON file to dir/daemon.json.
// On non-Windows systems the file is created with 0600 permissions.
// The write is atomic: a temp file is written first, then renamed over
// the target so readers never observe a partial write.
func WriteDaemonJSON(dir, token string, port, pid int) error {
	d := DaemonJSON{
		Port:      port,
		Token:     token,
		PID:       pid,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("controlapi.WriteDaemonJSON: marshal: %w", err)
	}

	target := filepath.Join(dir, "daemon.json")
	tmp := target + ".tmp"

	// Write to temp file, sync, then rename for atomic replacement.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("controlapi.WriteDaemonJSON: create temp: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("controlapi.WriteDaemonJSON: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("controlapi.WriteDaemonJSON: sync: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("controlapi.WriteDaemonJSON: close: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("controlapi.WriteDaemonJSON: rename: %w", err)
	}
	return nil
}

// ReadDaemonJSON reads and parses the daemon.json file from dir.
// Returns an error wrapping os.ErrNotExist when the file does not exist
// (no daemon running) — callers should check errors.Is(err, os.ErrNotExist).
func ReadDaemonJSON(dir string) (DaemonJSON, error) {
	path := filepath.Join(dir, "daemon.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return DaemonJSON{}, fmt.Errorf("controlapi.ReadDaemonJSON: %w", err)
	}
	var d DaemonJSON
	if err := json.Unmarshal(b, &d); err != nil {
		return DaemonJSON{}, fmt.Errorf("controlapi.ReadDaemonJSON: parse: %w", err)
	}
	return d, nil
}

// RemoveDaemonJSON removes the daemon.json file from dir. Called on clean
// daemon shutdown. A non-existent file is silently ignored.
func RemoveDaemonJSON(dir string) error {
	path := filepath.Join(dir, "daemon.json")
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("controlapi.RemoveDaemonJSON: %w", err)
	}
	return nil
}
