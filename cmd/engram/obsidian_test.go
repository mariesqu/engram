package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/obsidian"
)

// TestObsidianExportMissingVault covers REQ-EXPORT-01: --vault is required.
func TestObsidianExportMissingVault(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	err := runObsidianExportCmd([]string{"--db", dbPath})
	if err == nil {
		t.Fatal("runObsidianExportCmd() error = nil, want an error for missing --vault")
	}
	if !strings.Contains(err.Error(), "--vault") {
		t.Errorf("error = %q, want it to mention --vault", err.Error())
	}
}

// TestObsidianExportMissingDB covers REQ-EXPORT-01 (RECONCILED): --db is
// required (or ENGRAM_DB), matching every other DB-touching subcommand.
func TestObsidianExportMissingDB(t *testing.T) {
	t.Setenv("ENGRAM_DB", "")
	dir := t.TempDir()
	vault := filepath.Join(dir, "vault")

	err := runObsidianExportCmd([]string{"--vault", vault})
	if err == nil {
		t.Fatal("runObsidianExportCmd() error = nil, want an error for missing --db")
	}
	if !strings.Contains(err.Error(), "--db") {
		t.Errorf("error = %q, want it to mention --db", err.Error())
	}
}

// TestObsidianExportCallsInjectedExporter verifies every flag is parsed
// correctly and threaded through to the injectable newObsidianExporter var.
func TestObsidianExportCallsInjectedExporter(t *testing.T) {
	dir := t.TempDir()
	vault := filepath.Join(dir, "vault")
	dbPath := filepath.Join(dir, "test.db")

	var gotCfg obsidian.ExportConfig
	var gotStoreIsNil bool
	called := false

	orig := newObsidianExporter
	newObsidianExporter = func(store obsidian.StoreReader, cfg obsidian.ExportConfig) (*obsidian.Exporter, error) {
		called = true
		gotCfg = cfg
		gotStoreIsNil = store == nil
		return obsidian.NewExporter(store, cfg)
	}
	defer func() { newObsidianExporter = orig }()

	err := runObsidianExportCmd([]string{
		"--vault", vault,
		"--db", dbPath,
		"--project", "engram",
		"--limit", "5",
		"--since", "2026-01-01",
		"--force",
	})
	if err != nil {
		t.Fatalf("runObsidianExportCmd() error = %v", err)
	}
	if !called {
		t.Fatal("newObsidianExporter was not called")
	}
	if gotStoreIsNil {
		t.Fatal("newObsidianExporter received a nil store")
	}
	if gotCfg.VaultPath != vault {
		t.Errorf("cfg.VaultPath = %q, want %q", gotCfg.VaultPath, vault)
	}
	if gotCfg.Project != "engram" {
		t.Errorf("cfg.Project = %q, want %q", gotCfg.Project, "engram")
	}
	if gotCfg.Limit != 5 {
		t.Errorf("cfg.Limit = %d, want 5", gotCfg.Limit)
	}
	if !gotCfg.Force {
		t.Error("cfg.Force = false, want true")
	}
	wantSince, parseErr := time.Parse("2006-01-02", "2026-01-01")
	if parseErr != nil {
		t.Fatalf("test setup: %v", parseErr)
	}
	if !gotCfg.Since.Equal(wantSince) {
		t.Errorf("cfg.Since = %v, want %v", gotCfg.Since, wantSince)
	}
}

// TestObsidianExportMinimalFlags verifies that only --vault and --db are
// required; every optional flag defaults to its zero value.
func TestObsidianExportMinimalFlags(t *testing.T) {
	dir := t.TempDir()
	vault := filepath.Join(dir, "vault")
	dbPath := filepath.Join(dir, "test.db")

	var gotCfg obsidian.ExportConfig
	orig := newObsidianExporter
	newObsidianExporter = func(store obsidian.StoreReader, cfg obsidian.ExportConfig) (*obsidian.Exporter, error) {
		gotCfg = cfg
		return obsidian.NewExporter(store, cfg)
	}
	defer func() { newObsidianExporter = orig }()

	err := runObsidianExportCmd([]string{"--vault", vault, "--db", dbPath})
	if err != nil {
		t.Fatalf("runObsidianExportCmd() error = %v", err)
	}
	if gotCfg.Project != "" {
		t.Errorf("cfg.Project = %q, want empty", gotCfg.Project)
	}
	if gotCfg.Limit != 0 {
		t.Errorf("cfg.Limit = %d, want 0", gotCfg.Limit)
	}
	if gotCfg.Force {
		t.Error("cfg.Force = true, want false")
	}
	if !gotCfg.Since.IsZero() {
		t.Errorf("cfg.Since = %v, want zero value", gotCfg.Since)
	}
}

// TestObsidianExportRejectsPositionalArgs matches the convention established
// by import.go/status.go: this subcommand takes no positional arguments.
func TestObsidianExportRejectsPositionalArgs(t *testing.T) {
	dir := t.TempDir()
	vault := filepath.Join(dir, "vault")
	dbPath := filepath.Join(dir, "test.db")

	err := runObsidianExportCmd([]string{"--vault", vault, "--db", dbPath, "unexpected-arg"})
	if err == nil {
		t.Fatal("runObsidianExportCmd() error = nil, want an error for a positional argument")
	}
}

// TestObsidianExportInUsage verifies the subcommand is discoverable from the
// top-level usage text and that top-level --help still exits 2.
func TestObsidianExportInUsage(t *testing.T) {
	if !strings.Contains(usage, "obsidian-export") {
		t.Error("top-level usage const does not mention \"obsidian-export\"")
	}
	code := run([]string{"--help"})
	if code != 2 {
		t.Errorf("run([--help]) = %d, want 2", code)
	}
}
