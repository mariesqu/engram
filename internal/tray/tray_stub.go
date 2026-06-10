//go:build !windows

// Package tray provides the Windows system-tray integration for the engram
// resident daemon.  On non-Windows platforms this stub is the sole compiled
// file; it exports only ErrUnsupported so that cmd/engram/tray.go can print
// a clear "tray is Windows-only; use `engram ui`" error on every non-Windows
// platform.
package tray

import "errors"

// ErrUnsupported is returned by Run on every non-Windows platform.
var ErrUnsupported = errors.New("engram tray is only supported on Windows; use `engram ui` instead")

// TrayConfig is defined in both build variants so that cmd/engram/tray.go
// can reference it without a build-tag split.
type TrayConfig struct {
	// Port is the TCP port of the running resident daemon.
	Port int
	// Token is the bearer token read from daemon.json.
	Token string
	// DBDir is the directory that contains daemon.json (same as the DB dir).
	DBDir string
}

// Run always returns ErrUnsupported on non-Windows builds.
func Run(_ TrayConfig) error {
	return ErrUnsupported
}
