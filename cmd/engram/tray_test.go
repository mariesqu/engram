//go:build !windows

package main

import (
	"strings"
	"testing"
)

// TestTrayCmd_NonWindows_ReturnsError verifies that `engram tray` returns a
// clear error on non-Windows platforms. This test runs on Linux/macOS in CI.
func TestTrayCmd_NonWindows_ReturnsError(t *testing.T) {
	err := runTrayCmd([]string{})
	if err == nil {
		t.Fatal("expected error from runTrayCmd on non-Windows, got nil")
	}
	if !strings.Contains(err.Error(), "Windows") {
		t.Errorf("error %q does not mention Windows", err.Error())
	}
	if !strings.Contains(err.Error(), "engram ui") {
		t.Errorf("error %q does not mention 'engram ui' as the alternative", err.Error())
	}
}

// TestRun_Tray_NonWindows_ExitNonZero verifies that the top-level run()
// dispatcher returns exit code 1 for `engram tray` on non-Windows.
func TestRun_Tray_NonWindows_ExitNonZero(t *testing.T) {
	code := run([]string{"tray"})
	if code == 0 {
		t.Error("expected non-zero exit code for tray on non-Windows, got 0")
	}
}
