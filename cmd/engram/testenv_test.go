package main

// testenv_test.go — BH-001: hermetic test environment.
//
// Untagged (no //go:build line) so isolateUserEnv is available to both the
// default build's TestMain (testmain_test.go, "!acceptance") and the
// acceptance build's TestMain (serve_acceptance_test.go, "acceptance").

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// userEnvVars are the location-bearing environment variables this helper
// points at fresh subdirectories of one isolated temp root. ENGRAM_CONFIG_DIR
// is engram's own override (see internal/config.DefaultConfigDir); the rest
// are what the Go stdlib (os.UserConfigDir, os.UserHomeDir, os.UserCacheDir)
// and the XDG base-directory spec read to find a user's real directories
// across platforms.
var userEnvVars = []string{
	"ENGRAM_CONFIG_DIR",
	"APPDATA",
	"LOCALAPPDATA",
	"HOME",
	"USERPROFILE",
	"XDG_CONFIG_HOME",
	"XDG_CACHE_HOME",
}

// isolateUserEnv points every environment variable engram (or the platform
// resolution its dependencies use) might consult to find a user config,
// cache, or home directory at fresh subdirectories of one temp directory, and
// unsets every OTHER ENGRAM_* variable found in the ambient environment
// (ENGRAM_DB, ENGRAM_WRITER_ID, ENGRAM_WRITER_KEY, ENGRAM_CENTRAL_URL, ...).
//
// Without this, a cmd/engram test that calls run() with no explicit --db (or
// exercises resolveConnectDBPath / spawnWorkingDir / hookSettingsPath without
// its own t.Setenv override) falls through flag > env > config-file
// resolution onto the developer's REAL %APPDATA%\engram\config.json (or
// ~/.config/engram on Unix) — and on a machine with a real install, onto the
// developer's live engram.db (internal/config/config.go's DefaultConfigDir;
// cmd/engram/daemon.go's runDaemonCmd).
//
// It is meant to run ONCE, from TestMain, before any test runs — hence
// os.Setenv rather than t.Setenv, which requires a live *testing.T. Call the
// returned cleanup once, after m.Run() finishes, to restore the environment
// exactly as found (present vars restored to their old value, absent vars
// re-unset) and remove the temp root.
func isolateUserEnv() (cleanup func(), err error) {
	root, err := os.MkdirTemp("", "engram-test-userenv-*")
	if err != nil {
		return nil, fmt.Errorf("isolateUserEnv: MkdirTemp: %w", err)
	}

	// Every var this helper is about to change — the location vars it sets,
	// plus every ENGRAM_* var found in the ambient environment — gets its
	// prior value captured FIRST, so cleanup restores exactly what was there
	// (present → old value, absent → unset) rather than guessing.
	touched := make(map[string]struct{}, len(userEnvVars))
	for _, name := range userEnvVars {
		touched[name] = struct{}{}
	}
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		// ENGRAM_TEST_* are test-harness overrides (e.g. ENGRAM_TEST_PG_DSN),
		// not user state, so they pass through untouched.
		if ok && strings.HasPrefix(name, "ENGRAM_") && !strings.HasPrefix(name, "ENGRAM_TEST_") {
			touched[name] = struct{}{}
		}
	}

	prior := make(map[string]*string, len(touched))
	for name := range touched {
		if v, ok := os.LookupEnv(name); ok {
			vv := v
			prior[name] = &vv
		} else {
			prior[name] = nil
		}
	}

	isLocationVar := func(name string) bool {
		for _, v := range userEnvVars {
			if v == name {
				return true
			}
		}
		return false
	}

	for _, name := range userEnvVars {
		dir := filepath.Join(root, strings.ToLower(name))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("isolateUserEnv: MkdirAll %s: %w", dir, err)
		}
		if err := os.Setenv(name, dir); err != nil {
			return nil, fmt.Errorf("isolateUserEnv: Setenv %s: %w", name, err)
		}
	}

	// Every other ENGRAM_* var (writer keys, central URLs, DSNs, ...) is
	// ambient developer state a test must opt into explicitly via t.Setenv,
	// never inherit from whoever's shell happens to run `go test`.
	for name := range touched {
		if isLocationVar(name) {
			continue
		}
		if err := os.Unsetenv(name); err != nil {
			return nil, fmt.Errorf("isolateUserEnv: Unsetenv %s: %w", name, err)
		}
	}

	cleanup = func() {
		for name, val := range prior {
			if val == nil {
				_ = os.Unsetenv(name)
			} else {
				_ = os.Setenv(name, *val)
			}
		}
		_ = os.RemoveAll(root)
	}
	return cleanup, nil
}

// TestIsolateUserEnv_PointsInsideTempDir is the load-bearing proof that
// isolation actually took effect for THIS test binary. TestMain (either
// build) calls isolateUserEnv before any test runs, so every other test in
// this package relies on it silently; if this one fails, none of the others
// are proof of anything — they would all still be reading the developer's
// real user directories.
func TestIsolateUserEnv_PointsInsideTempDir(t *testing.T) {
	tmp := os.TempDir()
	for _, name := range []string{"APPDATA", "ENGRAM_CONFIG_DIR"} {
		v := os.Getenv(name)
		if v == "" {
			t.Fatalf("%s is empty — isolateUserEnv did not run (or TestMain did not call it)", name)
		}
		rel, err := filepath.Rel(tmp, v)
		if err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("%s = %q, want a path inside os.TempDir() (%q); rel=%q err=%v", name, v, tmp, rel, err)
		}
	}
}
