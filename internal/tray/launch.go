//go:build windows

package tray

import "errors"

// ProbeResult describes the outcome of a single daemon probe attempt.
type ProbeResult int

const (
	// ProbeHealthy means the daemon responded to GET /api/v1/status with a
	// valid engram status payload — no need to launch.
	ProbeHealthy ProbeResult = iota
	// ProbeMissing means daemon.json does not exist in the DB directory.
	ProbeMissing
	// ProbeUnreachable means daemon.json exists but the daemon did not respond
	// (network error, wrong port, non-engram process on the port).
	ProbeUnreachable
)

// ErrLaunchFailed is returned when the tray tried to auto-launch the daemon
// but did not observe a healthy status within the bounded retry window.
var ErrLaunchFailed = errors.New("auto-launch failed: daemon did not become healthy in time")

// ShouldLaunchDaemon returns true when the tray must start the daemon.
// It is a pure decision function over probe results — no system calls.
//
//   - ProbeHealthy  → already running; attach without launching (false)
//   - ProbeMissing  → no daemon at all; launch (true)
//   - ProbeUnreachable → daemon.json absent/stale or process gone; launch (true)
func ShouldLaunchDaemon(r ProbeResult) bool {
	return r != ProbeHealthy
}
