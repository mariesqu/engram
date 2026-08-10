//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
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
	dbPath := filepath.Join(t.TempDir(), "test.db")
	cmd := buildSpawnCmd("engram", dbPath)
	closeSpawnLog(t, cmd)

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

// TestRotateSpawnLogIfNeeded_TruncatesWhenRenameBlocked verifies the
// Windows-specific fallback in rotateSpawnLogIfNeeded: a spawn log that is
// still open for writing (simulating a live, resident daemon whose stdout/
// stderr are wired to it — see buildSpawnCmd) cannot be renamed on Windows,
// because os.OpenFile's default handles do not grant FILE_SHARE_DELETE, so
// os.Rename of an open file is rejected outright — this is the normal case,
// not a race. rotateSpawnLogIfNeeded must fall back to truncating the file
// in place instead of leaving it to grow unbounded forever.
func TestRotateSpawnLogIfNeeded_TruncatesWhenRenameBlocked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, daemonSpawnLogName)

	oversize := []byte(strings.Repeat("x", daemonSpawnLogMaxBytes))
	if err := os.WriteFile(path, oversize, 0o600); err != nil {
		t.Fatalf("seed oversize log: %v", err)
	}

	// Hold the file open for writing, exactly as a live daemon's inherited
	// stdout/stderr handle would (see openSpawnLog / buildSpawnCmd).
	held, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open log to simulate a live daemon holding it: %v", err)
	}
	defer held.Close()

	rotateSpawnLogIfNeeded(path)

	if _, err := os.Stat(path + ".old"); !os.IsNotExist(err) {
		t.Error("rename should have failed while the file is held open — .old must not exist")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat original path after rotation attempt: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("original log size = %d, want 0 (truncate fallback should have shrunk it)", info.Size())
	}
}
