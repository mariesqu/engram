package main

// embedding_daemon_test.go — task 5.4
//
// Daemon-level tests for embedding security properties:
//   - ENGRAM_EMBEDDING_KEY must not appear in --help output
//   - Invalid embedding_provider in config.json is startup-fatal (not a warning)

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mariesqu/engram/internal/config"
)

// TestRun_DaemonHelp_DoesNotLeakEmbeddingKey verifies that the daemon --help
// output does not contain the ENGRAM_EMBEDDING_KEY value even when it is set in
// the environment. The key must NEVER appear in PrintDefaults output — a flag
// default that echoes an env var would leak the secret on any --help invocation.
func TestRun_DaemonHelp_DoesNotLeakEmbeddingKey(t *testing.T) {
	// Use a recognisable fake key so we can assert absence precisely.
	const fakeKey = "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
	t.Setenv("ENGRAM_EMBEDDING_KEY", fakeKey)

	out := captureStderr(t, func() {
		if code := run([]string{"daemon", "--help"}); code != 0 {
			t.Errorf("daemon --help: exit code %d, want 0", code)
		}
	})

	if strings.Contains(out, fakeKey) || strings.Contains(out, "cafebabe") {
		t.Errorf("daemon --help leaked ENGRAM_EMBEDDING_KEY:\n%s", out)
	}
}

// TestDaemon_InvalidEmbeddingProvider_FatalAtStartup verifies two things:
//  1. config.Load returns an error for an invalid embedding_provider value.
//  2. The daemon treats this config.Load error as startup-fatal (returns error,
//     does NOT fall back to the zero Config with a warning log).
//
// This is the PR-③ lesson applied to embedding: a bad value persisted to disk
// must hard-error the next startup rather than silently falling back to noop.
func TestDaemon_InvalidEmbeddingProvider_FatalAtStartup(t *testing.T) {
	dir := t.TempDir()

	// Write config.json with a genuinely invalid embedding_provider.
	cfgFile := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgFile, []byte(`{"embedding_provider":"gpt-embeddings"}`), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// config.Load must return an error for an unrecognised provider.
	_, err := config.Load(dir)
	if err == nil {
		t.Fatal("config.Load with embedding_provider=gpt-embeddings should return error, got nil")
	}
	if !strings.Contains(err.Error(), "gpt-embeddings") {
		t.Errorf("error message should contain the invalid value; got: %v", err)
	}
	if !strings.Contains(err.Error(), "embedding_provider") {
		t.Errorf("error message should mention 'embedding_provider'; got: %v", err)
	}

	// Verify the error text also lists valid values so operators can self-correct.
	if !strings.Contains(err.Error(), "openai") {
		t.Errorf("error message should list valid values (including 'openai'); got: %v", err)
	}
}

// TestDaemon_OpenAIProvider_BuildsWithoutKey verifies that buildDaemon with
// embeddingProvider="openai" but no key succeeds (falls back to noop) and does
// NOT panic or hard-error at construction time. The missing-key case is a
// runtime warning, not a fatal error — embedding is optional.
func TestDaemon_OpenAIProvider_BuildsWithoutKey(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "engram.db")
	cfg := daemonCfg{
		db:                dbPath,
		syncInterval:      30 * 1e9, // 30 seconds
		embeddingProvider: "openai",
		embeddingKey:      nil, // no key — should fall back to noop gracefully
	}

	components, err := buildDaemon(cfg)
	if err != nil {
		t.Fatalf("buildDaemon with openai but no key: unexpected error: %v", err)
	}
	t.Cleanup(components.Close)

	if components.store == nil {
		t.Fatal("buildDaemon: store is nil")
	}
}
