package controlapi_test

import (
	"errors"
	"os"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
)

// TestDaemonJSON_RoundTrip verifies that WriteDaemonJSON followed by
// ReadDaemonJSON produces identical values on all platforms.
func TestDaemonJSON_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	const token = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	const port = 7700
	const pid = 12345

	if err := controlapi.WriteDaemonJSON(dir, token, port, pid); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	got, err := controlapi.ReadDaemonJSON(dir)
	if err != nil {
		t.Fatalf("ReadDaemonJSON: %v", err)
	}

	if got.Token != token {
		t.Errorf("token: got %q, want %q", got.Token, token)
	}
	if got.Port != port {
		t.Errorf("port: got %d, want %d", got.Port, port)
	}
	if got.PID != pid {
		t.Errorf("pid: got %d, want %d", got.PID, pid)
	}
	if got.StartedAt == "" {
		t.Error("started_at must not be empty")
	}
}

// TestDaemonJSON_NotFound verifies that ReadDaemonJSON returns an error
// wrapping os.ErrNotExist when the file does not exist.
func TestDaemonJSON_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := controlapi.ReadDaemonJSON(dir)
	if err == nil {
		t.Fatal("ReadDaemonJSON on missing file: want error, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadDaemonJSON missing: want os.ErrNotExist, got %v", err)
	}
}

// TestDaemonJSON_Remove verifies that RemoveDaemonJSON removes the file and
// that a subsequent call on the (now missing) file is a no-op.
func TestDaemonJSON_Remove(t *testing.T) {
	dir := t.TempDir()
	if err := controlapi.WriteDaemonJSON(dir, "tok", 7700, 1); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	if err := controlapi.RemoveDaemonJSON(dir); err != nil {
		t.Fatalf("RemoveDaemonJSON: %v", err)
	}

	// File should no longer exist.
	if _, err := controlapi.ReadDaemonJSON(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after Remove: want os.ErrNotExist, got %v", err)
	}

	// Calling Remove again on missing file must not error.
	if err := controlapi.RemoveDaemonJSON(dir); err != nil {
		t.Errorf("RemoveDaemonJSON on missing file: want nil, got %v", err)
	}
}

// TestDaemonJSON_Atomic verifies that a concurrent read does not observe a
// partial write (the rename makes the write atomic).
func TestDaemonJSON_Atomic(t *testing.T) {
	dir := t.TempDir()

	// Write first version.
	if err := controlapi.WriteDaemonJSON(dir, "token-v1", 7700, 100); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Overwrite with second version — simulates a daemon restart.
	if err := controlapi.WriteDaemonJSON(dir, "token-v2", 7701, 200); err != nil {
		t.Fatalf("second write: %v", err)
	}

	got, err := controlapi.ReadDaemonJSON(dir)
	if err != nil {
		t.Fatalf("ReadDaemonJSON after overwrite: %v", err)
	}
	// Must see the latest version, not an intermediate state.
	if got.Token != "token-v2" {
		t.Errorf("token after overwrite: got %q, want token-v2", got.Token)
	}
	if got.Port != 7701 {
		t.Errorf("port after overwrite: got %d, want 7701", got.Port)
	}
}

// TestDaemonJSON_Permissions verifies that the written file has restrictive
// permissions on POSIX (mode 0600). On Windows the ACL check is in the
// Windows-tagged test file.
func TestDaemonJSON_Permissions(t *testing.T) {
	if isWindows() {
		t.Skip("permission test is Windows-specific in daemon_json_windows_test.go")
	}
	dir := t.TempDir()
	if err := controlapi.WriteDaemonJSON(dir, "tok", 7700, 1); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	fi, err := os.Stat(dir + "/daemon.json")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mode := fi.Mode().Perm()
	if mode != 0o600 {
		t.Errorf("daemon.json permissions: got %04o, want 0600", mode)
	}
}
