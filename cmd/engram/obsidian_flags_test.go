package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/obsidian"
)

// captureExportConfig swaps the injectable exporter constructor for one that
// records the ExportConfig it was handed, runs the command, and restores it.
func captureExportConfig(t *testing.T, args []string) (obsidian.ExportConfig, bool, error) {
	t.Helper()
	var got obsidian.ExportConfig
	called := false
	orig := newObsidianExporter
	newObsidianExporter = func(store obsidian.StoreReader, cfg obsidian.ExportConfig) (*obsidian.Exporter, error) {
		called = true
		got = cfg
		return obsidian.NewExporter(store, cfg)
	}
	defer func() { newObsidianExporter = orig }()
	err := runObsidianExportCmd(args)
	return got, called, err
}

// baseArgs returns the two required flags plus whatever extra is passed.
func baseArgs(t *testing.T, extra ...string) []string {
	t.Helper()
	dir := t.TempDir()
	return append([]string{
		"--vault", filepath.Join(dir, "vault"),
		"--db", filepath.Join(dir, "test.db"),
	}, extra...)
}

// ── 9.14: --graph-config (REQ-GRAPH-01) ──────────────────────────────────────

// TestObsidianExportGraphConfigDefaultsToPreserve covers REQ-GRAPH-01's default.
// It matters that the CLI defaults to "preserve" and not to the ExportConfig
// zero value: obsidian.ExportConfig treats "" as SKIP on purpose (an unset
// field must be inert and never silently write a file), so the CLI is
// responsible for defaulting its OWN knob before the value reaches the
// exporter. Without this, `engram obsidian-export --vault X --db Y` would
// silently never bootstrap the curated graph.
func TestObsidianExportGraphConfigDefaultsToPreserve(t *testing.T) {
	cfg, called, err := captureExportConfig(t, baseArgs(t))
	if err != nil {
		t.Fatalf("runObsidianExportCmd: %v", err)
	}
	if !called {
		t.Fatal("exporter constructor was not called")
	}
	if cfg.GraphConfig != obsidian.GraphConfigPreserve {
		t.Errorf("cfg.GraphConfig = %q, want %q by default", cfg.GraphConfig, obsidian.GraphConfigPreserve)
	}
}

// TestObsidianExportGraphConfigParsesEveryMode triangulates the default.
func TestObsidianExportGraphConfigParsesEveryMode(t *testing.T) {
	cases := map[string]obsidian.GraphConfigMode{
		"preserve": obsidian.GraphConfigPreserve,
		"force":    obsidian.GraphConfigForce,
		"skip":     obsidian.GraphConfigSkip,
	}
	for flagValue, want := range cases {
		cfg, called, err := captureExportConfig(t, baseArgs(t, "--graph-config", flagValue))
		if err != nil {
			t.Fatalf("--graph-config %s: %v", flagValue, err)
		}
		if !called {
			t.Fatalf("--graph-config %s: exporter constructor was not called", flagValue)
		}
		if cfg.GraphConfig != want {
			t.Errorf("--graph-config %s: cfg.GraphConfig = %q, want %q", flagValue, cfg.GraphConfig, want)
		}
	}
}

// TestObsidianExportRejectsInvalidGraphConfig covers REQ-GRAPH-01's "an invalid
// mode MUST be rejected with a clear error". Note the deliberate asymmetry with
// the DAEMON's obsidian_graph_config key, which falls back to preserve with a
// warning: an attended CLI invocation is a typo the user can fix immediately,
// whereas refusing to boot the daemon over the same typo would take MCP,
// autosync and the store down with it.
func TestObsidianExportRejectsInvalidGraphConfig(t *testing.T) {
	for _, bad := range []string{"presrve", "PRESERVE", "overwrite", "yes"} {
		_, called, err := captureExportConfig(t, baseArgs(t, "--graph-config", bad))
		if err == nil {
			t.Errorf("--graph-config %s: error = nil, want a rejection", bad)
			continue
		}
		if !strings.Contains(err.Error(), "graph-config") {
			t.Errorf("--graph-config %s: error = %q, want it to name the flag", bad, err.Error())
		}
		if called {
			t.Errorf("--graph-config %s: the exporter was constructed despite an invalid mode", bad)
		}
	}
}

// TestObsidianExportRejectsEmptyGraphConfig pins the one value the underlying
// parser accepts but the FLAG must not: obsidian.ParseGraphConfigMode("")
// returns GraphConfigSkip (the inert zero value for an unset ExportConfig
// field), so passing --graph-config= explicitly would silently mean "skip"
// while looking like "unset". An explicit flag with an empty value is a user
// error, not a request to skip.
func TestObsidianExportRejectsEmptyGraphConfig(t *testing.T) {
	_, called, err := captureExportConfig(t, baseArgs(t, "--graph-config", ""))
	if err == nil {
		t.Fatal("--graph-config \"\": error = nil, want a rejection")
	}
	if called {
		t.Error("--graph-config \"\": the exporter was constructed despite an empty mode")
	}
}

// TestObsidianExportGraphConfigInUsage keeps the usage text honest.
func TestObsidianExportGraphConfigInUsage(t *testing.T) {
	if !strings.Contains(obsidianExportUsage, "--graph-config") {
		t.Error("obsidianExportUsage does not document --graph-config")
	}
	for _, mode := range []string{"preserve", "force", "skip"} {
		if !strings.Contains(obsidianExportUsage, mode) {
			t.Errorf("obsidianExportUsage does not mention the %q mode", mode)
		}
	}
}

// ── 9.13: closing MAJOR-9 — --since and --force had no tests ─────────────────

// TestObsidianExportSinceAcceptsRFC3339 closes half of verify-report #4710's
// MAJOR-9: parseObsidianSince was exercised only through its "2006-01-02"
// branch, so the RFC3339 branch — the one REQ-EXPORT-01 names FIRST — had zero
// coverage.
func TestObsidianExportSinceAcceptsRFC3339(t *testing.T) {
	const raw = "2026-03-04T05:06:07Z"
	want, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("test setup: %v", err)
	}

	cfg, called, err := captureExportConfig(t, baseArgs(t, "--since", raw))
	if err != nil {
		t.Fatalf("--since %s: %v", raw, err)
	}
	if !called {
		t.Fatal("exporter constructor was not called")
	}
	if !cfg.Since.Equal(want) {
		t.Errorf("cfg.Since = %v, want %v", cfg.Since, want)
	}
}

// TestObsidianExportSinceAcceptsRFC3339WithOffset pins that a non-UTC offset is
// honoured rather than silently reinterpreted — the incremental cutoff is
// compared against UTC timestamps from the store, so an hour of drift here
// silently skips or re-exports an hour of observations.
func TestObsidianExportSinceAcceptsRFC3339WithOffset(t *testing.T) {
	const raw = "2026-03-04T05:06:07+02:00"
	want, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("test setup: %v", err)
	}
	cfg, _, err := captureExportConfig(t, baseArgs(t, "--since", raw))
	if err != nil {
		t.Fatalf("--since %s: %v", raw, err)
	}
	if !cfg.Since.Equal(want) {
		t.Errorf("cfg.Since = %v, want %v (the +02:00 offset must be honoured)", cfg.Since, want)
	}
	if cfg.Since.UTC().Hour() != 3 {
		t.Errorf("cfg.Since in UTC = %v, want 03:06:07Z", cfg.Since.UTC())
	}
}

// TestObsidianExportSinceAcceptsBareDate is the other accepted layout.
func TestObsidianExportSinceAcceptsBareDate(t *testing.T) {
	want, err := time.Parse("2006-01-02", "2026-03-04")
	if err != nil {
		t.Fatalf("test setup: %v", err)
	}
	cfg, _, err := captureExportConfig(t, baseArgs(t, "--since", "2026-03-04"))
	if err != nil {
		t.Fatalf("--since: %v", err)
	}
	if !cfg.Since.Equal(want) {
		t.Errorf("cfg.Since = %v, want %v", cfg.Since, want)
	}
}

// TestObsidianExportRejectsInvalidSince is the other half of MAJOR-9: invalid
// --since input had no test at all. A silently-ignored bad cutoff would export
// from the beginning of time (or from the state file) while the user believed
// they had scoped the run.
func TestObsidianExportRejectsInvalidSince(t *testing.T) {
	cases := []string{
		"yesterday",
		"04-03-2026",          // DD-MM-YYYY is not one of the two accepted layouts
		"2026-13-01",          // month 13
		"2026-03-04 05:06:07", // space instead of T — not RFC3339
		"",                    // present but empty is silently "unset", see below
	}
	for _, raw := range cases {
		_, called, err := captureExportConfig(t, baseArgs(t, "--since", raw))
		if raw == "" {
			// An empty --since is indistinguishable from omitting the flag; the
			// shipped behaviour is to treat it as unset. Pinned here so the
			// distinction is deliberate rather than accidental.
			if err != nil {
				t.Errorf("--since \"\": error = %v, want it treated as unset", err)
			}
			continue
		}
		if err == nil {
			t.Errorf("--since %q: error = nil, want a rejection", raw)
			continue
		}
		if !strings.Contains(err.Error(), "since") {
			t.Errorf("--since %q: error = %q, want it to name the flag", raw, err.Error())
		}
		if called {
			t.Errorf("--since %q: the exporter was constructed despite an invalid cutoff", raw)
		}
	}
}

// TestObsidianExportForceReachesExportConfig closes the last part of MAJOR-9:
// no test ever set Force: true. --force is the only state-repair lever the CLI
// has (it also rebuilds an unparseable state file), so a wiring break here is
// silent until a user needs it most.
func TestObsidianExportForceReachesExportConfig(t *testing.T) {
	cfg, _, err := captureExportConfig(t, baseArgs(t, "--force"))
	if err != nil {
		t.Fatalf("--force: %v", err)
	}
	if !cfg.Force {
		t.Error("cfg.Force = false with --force passed")
	}

	cfg, _, err = captureExportConfig(t, baseArgs(t))
	if err != nil {
		t.Fatalf("no --force: %v", err)
	}
	if cfg.Force {
		t.Error("cfg.Force = true without --force")
	}
}

// ── REQ-WATCH-01/-02 retirement: --watch and --interval are NEVER added ───────

// TestObsidianExportHasNoWatchFlag pins the retirement of REQ-WATCH-01/-02. The
// scheduled export runs INSIDE the daemon, on the store the daemon already has
// open — a standalone `--watch` CLI daemon is the unattended, repeating
// localstore.Open that verify-report #4710's MAJOR-10 names (localstore.Open
// runs ApplySchema + runMigrations on EVERY invocation, so a version-mismatched
// binary on a timer would migrate the schema out from under a running daemon).
//
// Neither flag was ever shipped, so there is no migration population and no
// compatibility obligation: passing one fails with Go's own "flag provided but
// not defined" plus usage, which is a clearer message than a hand-rolled
// rejection — and it is obtained for free.
func TestObsidianExportHasNoWatchFlag(t *testing.T) {
	for _, flagArgs := range [][]string{
		{"--watch"},
		{"--interval", "10m"},
		{"--watch", "--interval", "10m"},
	} {
		_, called, err := captureExportConfig(t, baseArgs(t, flagArgs...))
		if err == nil {
			t.Errorf("%v: error = nil, want Go's \"flag provided but not defined\"", flagArgs)
			continue
		}
		if !strings.Contains(err.Error(), "not defined") {
			t.Errorf("%v: error = %q, want Go's own \"flag provided but not defined\"", flagArgs, err.Error())
		}
		if called {
			t.Errorf("%v: the exporter was constructed despite an undefined flag", flagArgs)
		}
	}

	// And the usage text must not advertise them either.
	for _, word := range []string{"--watch", "--interval"} {
		if strings.Contains(obsidianExportUsage, word) {
			t.Errorf("obsidianExportUsage mentions %s; the scheduled export is a daemon config key, not a CLI flag", word)
		}
	}
}

// TestObsidianExportUsageWarnsAgainstScheduling covers REQ-EXPORT-01's binding
// documentation obligation and the accepted residual of MAJOR-10: this command
// opens the SQLite database directly (read-write, running schema migrations),
// so it MUST NOT be put on cron / Task Scheduler / launchd. Stating it converts
// an implicit trap into an explicit instruction — and the usage text is where a
// user looking for "how do I automate this" will actually look.
func TestObsidianExportUsageWarnsAgainstScheduling(t *testing.T) {
	lower := strings.ToLower(obsidianExportUsage)
	for _, want := range []string{"cron", "task scheduler", "obsidian_vault"} {
		if !strings.Contains(lower, want) {
			t.Errorf("obsidianExportUsage does not mention %q — the do-not-schedule warning and the daemon alternative are a binding documentation obligation", want)
		}
	}
}
