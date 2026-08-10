//go:build !windows

package main

import (
	"path/filepath"
	"testing"
)

// TestDetachedSysProcAttr_SetsSetsid verifies detachedSysProcAttr sets
// Setsid — an empty &syscall.SysProcAttr{} would satisfy a bare non-nil
// check but would NOT actually start the daemon in a new session, leaving it
// exposed to SIGHUP when the parent's controlling terminal goes away.
func TestDetachedSysProcAttr_SetsSetsid(t *testing.T) {
	attr := detachedSysProcAttr()

	if !attr.Setsid {
		t.Error("Setsid = false, want true (daemon must start in a new session)")
	}
}

// TestBuildSpawnCmd_UnixSetsid verifies buildSpawnCmd's returned *exec.Cmd
// carries Setsid on its SysProcAttr, not just a non-nil placeholder.
func TestBuildSpawnCmd_UnixSetsid(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	cmd := buildSpawnCmd("engram", dbPath)
	closeSpawnLog(t, cmd)

	attr := cmd.SysProcAttr
	if attr == nil {
		t.Fatal("SysProcAttr is nil")
	}

	if !attr.Setsid {
		t.Error("Setsid = false, want true (daemon must start in a new session)")
	}
}
