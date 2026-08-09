//go:build windows

package main

import (
	"testing"

	"golang.org/x/sys/windows"
)

// TestDetachedSysProcAttr_SetsDetachFlags verifies detachedSysProcAttr sets
// exactly the creation flags spawn_windows.go documents (CREATE_NEW_PROCESS_
// GROUP | DETACHED_PROCESS) — an empty &syscall.SysProcAttr{} would satisfy a
// bare non-nil check but would NOT actually detach the daemon from its
// parent's console/process group.
func TestDetachedSysProcAttr_SetsDetachFlags(t *testing.T) {
	attr := detachedSysProcAttr()

	want := uint32(windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS)
	if attr.CreationFlags != want {
		t.Errorf("CreationFlags = %#x, want %#x (CREATE_NEW_PROCESS_GROUP|DETACHED_PROCESS)",
			attr.CreationFlags, want)
	}
}

// TestBuildSpawnCmd_WindowsDetachFlags verifies buildSpawnCmd's returned
// *exec.Cmd carries the same detach flags on its SysProcAttr, not just a
// non-nil placeholder.
func TestBuildSpawnCmd_WindowsDetachFlags(t *testing.T) {
	cmd := buildSpawnCmd("engram", "C:\\tmp\\test.db")

	attr := cmd.SysProcAttr
	if attr == nil {
		t.Fatal("SysProcAttr is nil")
	}

	want := uint32(windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS)
	if attr.CreationFlags != want {
		t.Errorf("CreationFlags = %#x, want %#x (CREATE_NEW_PROCESS_GROUP|DETACHED_PROCESS)",
			attr.CreationFlags, want)
	}
}
