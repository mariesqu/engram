package config

import (
	"os"
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

// TestDefaultConfigDir_RelativeOverrideIsMadeAbsolute pins the other half of
// the override contract. Callers CREATE this directory — config.Save writes
// into it, and a daemon spawn hands it to a detached process as its working
// directory — so a relative value would be created inside whatever repo the
// current process happens to be standing in, and a daemon autostarted from a
// different repo would read a different config under the same setting. A path
// the user cannot predict is not an override, it is a surprise.
func TestDefaultConfigDir_RelativeOverrideIsMadeAbsolute(t *testing.T) {
	workspace := t.TempDir()
	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatalf("chdir %s: %v", workspace, err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}

	for _, override := range []string{".engram-cfg", ".", filepath.Join("nested", "cfg")} {
		t.Run(override, func(t *testing.T) {
			t.Setenv("ENGRAM_CONFIG_DIR", override)

			got, err := DefaultConfigDir()
			if err != nil {
				t.Fatalf("DefaultConfigDir: %v", err)
			}
			if !filepath.IsAbs(got) {
				t.Fatalf("DefaultConfigDir() = %q, which a caller would create relative to its own cwd", got)
			}
			if want := filepath.Clean(filepath.Join(cwd, override)); got != want {
				t.Errorf("DefaultConfigDir() = %q, want %q", got, want)
			}
			// Nothing is created by resolving a path: only callers create it.
			if entries, err := os.ReadDir(workspace); err == nil && len(entries) != 0 {
				t.Errorf("resolving the override created %d entry/entries in the working directory", len(entries))
			}
		})
	}
}
