package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultConfigDir_EnvOverride verifies that ENGRAM_CONFIG_DIR overrides the
// platform default config directory (as documented in the README), and that a
// blank/whitespace value is ignored (falls back to <userconfig>/engram).
func TestDefaultConfigDir_EnvOverride(t *testing.T) {
	override := filepath.Join(t.TempDir(), "engram-cfg-override")
	t.Setenv("ENGRAM_CONFIG_DIR", override)
	got, err := DefaultConfigDir()
	if err != nil {
		t.Fatalf("DefaultConfigDir with override: %v", err)
	}
	if got != override {
		t.Errorf("ENGRAM_CONFIG_DIR override: got %q, want %q", got, override)
	}

	// A whitespace-only value must NOT be treated as an override.
	t.Setenv("ENGRAM_CONFIG_DIR", "   ")
	got2, err := DefaultConfigDir()
	if err != nil {
		t.Fatalf("DefaultConfigDir with blank override: %v", err)
	}
	if !strings.HasSuffix(filepath.ToSlash(got2), "engram") {
		t.Errorf("blank override should fall back to <userconfig>/engram, got %q", got2)
	}
}
