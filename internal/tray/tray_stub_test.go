//go:build !windows

package tray_test

import (
	"errors"
	"testing"

	"github.com/mariesqu/engram/internal/tray"
)

// TestTray_NonWindows_ReturnsUnsupported verifies that Run returns ErrUnsupported
// on non-Windows platforms. This is headless-testable everywhere.
func TestTray_NonWindows_ReturnsUnsupported(t *testing.T) {
	err := tray.Run(tray.TrayConfig{Port: 7700, Token: "abc", DBDir: t.TempDir()})
	if !errors.Is(err, tray.ErrUnsupported) {
		t.Fatalf("Run() = %v, want ErrUnsupported", err)
	}
}
