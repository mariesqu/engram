package config

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfigJSON drops a raw config.json into dir so Load can be exercised
// against hand-authored values (the only way a user can produce the hostile
// inputs below — PUT /api/v1/config rejects them at write time).
func writeConfigJSON(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0600); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
}

// captureLog redirects the standard logger for the duration of fn and returns
// everything written. Used to prove a warning was emitted rather than the
// value being silently swallowed.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	}()
	fn()
	return buf.String()
}

// TestLoadObsidianKeys covers REQ-WATCH-09: all four obsidian_* keys are read
// from the config file into the resolved Config.
func TestLoadObsidianKeys(t *testing.T) {
	dir := t.TempDir()
	writeConfigJSON(t, dir, `{
  "obsidian_vault": "D:\\Vault",
  "obsidian_interval": "15m",
  "obsidian_project": "engram",
  "obsidian_graph_config": "force"
}`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ObsidianVault != `D:\Vault` {
		t.Errorf("ObsidianVault = %q, want %q", cfg.ObsidianVault, `D:\Vault`)
	}
	if cfg.ObsidianInterval != 15*time.Minute {
		t.Errorf("ObsidianInterval = %v, want 15m", cfg.ObsidianInterval)
	}
	if cfg.ObsidianProject != "engram" {
		t.Errorf("ObsidianProject = %q, want %q", cfg.ObsidianProject, "engram")
	}
	if cfg.ObsidianGraphConfig != "force" {
		t.Errorf("ObsidianGraphConfig = %q, want %q", cfg.ObsidianGraphConfig, "force")
	}
}

// TestLoadObsidianKeysAbsentAreInert covers REQ-WATCH-09's "OFF means inert":
// a config file with no obsidian_* keys resolves to the zero values, and
// obsidian_vault == "" is the master OFF switch.
func TestLoadObsidianKeysAbsentAreInert(t *testing.T) {
	dir := t.TempDir()
	writeConfigJSON(t, dir, `{"db_path":"x.db"}`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ObsidianVault != "" {
		t.Errorf("ObsidianVault = %q, want empty (feature OFF)", cfg.ObsidianVault)
	}
	if cfg.ObsidianInterval != 0 {
		t.Errorf("ObsidianInterval = %v, want 0 (unset)", cfg.ObsidianInterval)
	}
	if cfg.ObsidianProject != "" {
		t.Errorf("ObsidianProject = %q, want empty", cfg.ObsidianProject)
	}
	if cfg.ObsidianGraphConfig != "" {
		t.Errorf("ObsidianGraphConfig = %q, want empty (daemon defaults it to preserve)", cfg.ObsidianGraphConfig)
	}
}

// TestSaveObsidianKeys covers the Save half of the round trip: the four keys
// survive Save+Load unchanged.
func TestSaveObsidianKeys(t *testing.T) {
	dir := t.TempDir()
	original := Config{
		ObsidianVault:       filepath.Join(dir, "vault"),
		ObsidianInterval:    12 * time.Minute,
		ObsidianProject:     "engram",
		ObsidianGraphConfig: "skip",
	}
	if err := Save(dir, original); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ObsidianVault != original.ObsidianVault {
		t.Errorf("ObsidianVault: got %q, want %q", loaded.ObsidianVault, original.ObsidianVault)
	}
	if loaded.ObsidianInterval != original.ObsidianInterval {
		t.Errorf("ObsidianInterval: got %v, want %v", loaded.ObsidianInterval, original.ObsidianInterval)
	}
	if loaded.ObsidianProject != original.ObsidianProject {
		t.Errorf("ObsidianProject: got %q, want %q", loaded.ObsidianProject, original.ObsidianProject)
	}
	if loaded.ObsidianGraphConfig != original.ObsidianGraphConfig {
		t.Errorf("ObsidianGraphConfig: got %q, want %q", loaded.ObsidianGraphConfig, original.ObsidianGraphConfig)
	}
}

// TestSaveNeverPersistsNonPositiveObsidianInterval pins that Save cannot write
// a negative or zero cadence to disk even if one somehow reaches an in-memory
// Config — the on-disk value is the one the NEXT boot reads.
func TestSaveNeverPersistsNonPositiveObsidianInterval(t *testing.T) {
	for _, d := range []time.Duration{-time.Second, 0} {
		dir := t.TempDir()
		if err := Save(dir, Config{ObsidianInterval: d}); err != nil {
			t.Fatalf("Save(%v): %v", d, err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if strings.Contains(string(raw), "obsidian_interval") {
			t.Errorf("Save(%v) persisted obsidian_interval; file:\n%s", d, raw)
		}
	}
}

// TestLoadRejectsUnparseableObsidianInterval covers REQ-WATCH-04: a value that
// is not a Go duration is startup-fatal, mirroring sync_interval.
func TestLoadRejectsUnparseableObsidianInterval(t *testing.T) {
	dir := t.TempDir()
	writeConfigJSON(t, dir, `{"obsidian_interval":"ten minutes"}`)

	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load() error = nil, want a startup-fatal error for an unparseable obsidian_interval")
	}
	if !strings.Contains(err.Error(), "obsidian_interval") {
		t.Errorf("error = %q, want it to name obsidian_interval", err.Error())
	}
}

// TestLoadRejectsNegativeObsidianInterval is THE defence this phase exists to
// add. time.ParseDuration("-1s") SUCCEEDS and performs no sign check — the
// exact hole that produced Phase 8 CRITICAL-1 (a negative interval fed to
// time.NewTimer fires instantly, forever, saturating the single SQLite
// connection). obsidian.Loop now clamps a negative Interval defensively, but a
// negative value must never be stored or propagated in the first place.
func TestLoadRejectsNegativeObsidianInterval(t *testing.T) {
	for _, raw := range []string{"-1s", "-10m", "-2562047h47m16.854775808s"} {
		dir := t.TempDir()
		writeConfigJSON(t, dir, `{"obsidian_interval":"`+raw+`"}`)

		cfg, err := Load(dir)
		if err == nil {
			t.Fatalf("Load() with obsidian_interval=%q error = nil, want rejection; stored %v", raw, cfg.ObsidianInterval)
		}
		if !strings.Contains(err.Error(), "obsidian_interval") {
			t.Errorf("obsidian_interval=%q: error = %q, want it to name obsidian_interval", raw, err.Error())
		}
		if !strings.Contains(err.Error(), "positive") {
			t.Errorf("obsidian_interval=%q: error = %q, want it to say the value must be positive", raw, err.Error())
		}
		if cfg.ObsidianInterval < 0 {
			t.Errorf("obsidian_interval=%q: Load returned a NEGATIVE ObsidianInterval %v alongside the error", raw, cfg.ObsidianInterval)
		}
	}
}

// TestLoadRejectsZeroObsidianInterval covers the other non-positive case: an
// explicit "0s" parses cleanly but is indistinguishable from "unset" once
// stored, so it is rejected rather than silently meaning "use the default".
func TestLoadRejectsZeroObsidianInterval(t *testing.T) {
	dir := t.TempDir()
	writeConfigJSON(t, dir, `{"obsidian_interval":"0s"}`)

	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load() with obsidian_interval=\"0s\" error = nil, want rejection")
	}
	if !strings.Contains(err.Error(), "positive") {
		t.Errorf("error = %q, want it to say the value must be positive", err.Error())
	}
}

// TestLoadAcceptsSubMinuteObsidianInterval covers REQ-WATCH-04's deliberate
// asymmetry: a value that is positive but below the 1-minute floor is NOT
// startup-fatal. It is accepted here and clamped (with a warning) by
// obsidian.Loop — refusing to boot the process that owns MCP, autosync and the
// store over a too-small export cadence would be a strictly worse failure.
func TestLoadAcceptsSubMinuteObsidianInterval(t *testing.T) {
	dir := t.TempDir()
	writeConfigJSON(t, dir, `{"obsidian_interval":"30s"}`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v, want a sub-minute interval to be accepted (the Loop clamps it)", err)
	}
	if cfg.ObsidianInterval != 30*time.Second {
		t.Errorf("ObsidianInterval = %v, want 30s passed through to the Loop's clamp", cfg.ObsidianInterval)
	}
}

// TestLoadAcceptsNormalObsidianInterval is the control case: a normal value is
// stored verbatim with no warning.
func TestLoadAcceptsNormalObsidianInterval(t *testing.T) {
	dir := t.TempDir()
	writeConfigJSON(t, dir, `{"obsidian_interval":"10m"}`)

	var cfg Config
	var err error
	out := captureLog(t, func() { cfg, err = Load(dir) })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ObsidianInterval != 10*time.Minute {
		t.Errorf("ObsidianInterval = %v, want 10m", cfg.ObsidianInterval)
	}
	if out != "" {
		t.Errorf("Load logged %q for a normal interval, want silence", out)
	}
}

// TestLoadFallsBackOnUnknownGraphConfigMode covers the design's deliberate
// leniency: an unknown obsidian_graph_config falls back to "preserve" with a
// warning rather than aborting startup. A cosmetic export setting must not be
// able to prevent the daemon from booting.
func TestLoadFallsBackOnUnknownGraphConfigMode(t *testing.T) {
	dir := t.TempDir()
	writeConfigJSON(t, dir, `{"obsidian_graph_config":"presrve"}`)

	var cfg Config
	var err error
	out := captureLog(t, func() { cfg, err = Load(dir) })
	if err != nil {
		t.Fatalf("Load() error = %v, want a lenient fallback rather than a startup abort", err)
	}
	if cfg.ObsidianGraphConfig != "preserve" {
		t.Errorf("ObsidianGraphConfig = %q, want the %q fallback", cfg.ObsidianGraphConfig, "preserve")
	}
	if !strings.Contains(out, "obsidian_graph_config") {
		t.Errorf("Load logged %q, want a warning naming obsidian_graph_config", out)
	}
	if !strings.Contains(out, "presrve") {
		t.Errorf("Load logged %q, want the warning to quote the rejected value", out)
	}
}

// TestLoadKeepsValidGraphConfigModes is the triangulation partner: each valid
// mode survives untouched and logs nothing.
func TestLoadKeepsValidGraphConfigModes(t *testing.T) {
	for _, mode := range []string{"preserve", "force", "skip"} {
		dir := t.TempDir()
		writeConfigJSON(t, dir, `{"obsidian_graph_config":"`+mode+`"}`)

		var cfg Config
		var err error
		out := captureLog(t, func() { cfg, err = Load(dir) })
		if err != nil {
			t.Fatalf("Load(%s): %v", mode, err)
		}
		if cfg.ObsidianGraphConfig != mode {
			t.Errorf("ObsidianGraphConfig = %q, want %q", cfg.ObsidianGraphConfig, mode)
		}
		if out != "" {
			t.Errorf("Load(%s) logged %q, want silence", mode, out)
		}
	}
}

// TestPatchObsidianKeysAreRestartRequired covers REQ-WATCH-09: all four keys
// are restart-required, because the Exporter and the Loop are constructed once
// in buildDaemon exactly like the embedding provider.
func TestPatchObsidianKeysAreRestartRequired(t *testing.T) {
	base := Config{
		ObsidianVault:       `C:\old`,
		ObsidianInterval:    10 * time.Minute,
		ObsidianProject:     "",
		ObsidianGraphConfig: "preserve",
	}

	vault := `C:\new`
	interval := "20m"
	project := "engram"
	graph := "force"

	cases := []struct {
		name  string
		patch ConfigPatch
		check func(Config) error
	}{
		{"obsidian_vault", ConfigPatch{ObsidianVault: &vault}, func(c Config) error {
			if c.ObsidianVault != vault {
				return errf("ObsidianVault = %q, want %q", c.ObsidianVault, vault)
			}
			return nil
		}},
		{"obsidian_interval", ConfigPatch{ObsidianInterval: &interval}, func(c Config) error {
			if c.ObsidianInterval != 20*time.Minute {
				return errf("ObsidianInterval = %v, want 20m", c.ObsidianInterval)
			}
			return nil
		}},
		{"obsidian_project", ConfigPatch{ObsidianProject: &project}, func(c Config) error {
			if c.ObsidianProject != project {
				return errf("ObsidianProject = %q, want %q", c.ObsidianProject, project)
			}
			return nil
		}},
		{"obsidian_graph_config", ConfigPatch{ObsidianGraphConfig: &graph}, func(c Config) error {
			if c.ObsidianGraphConfig != graph {
				return errf("ObsidianGraphConfig = %q, want %q", c.ObsidianGraphConfig, graph)
			}
			return nil
		}},
	}

	for _, tc := range cases {
		out, restart := Patch(base, tc.patch)
		if !restart {
			t.Errorf("%s: restartRequired = false, want true", tc.name)
		}
		if err := tc.check(out); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// TestPatchObsidianKeysSameValueNoRestart triangulates the branch above: an
// unchanged value must NOT report restart_required (matching every other key).
func TestPatchObsidianKeysSameValueNoRestart(t *testing.T) {
	base := Config{
		ObsidianVault:       `C:\vault`,
		ObsidianInterval:    10 * time.Minute,
		ObsidianProject:     "engram",
		ObsidianGraphConfig: "preserve",
	}
	vault := `C:\vault`
	interval := "10m"
	project := "engram"
	graph := "preserve"

	_, restart := Patch(base, ConfigPatch{
		ObsidianVault:       &vault,
		ObsidianInterval:    &interval,
		ObsidianProject:     &project,
		ObsidianGraphConfig: &graph,
	})
	if restart {
		t.Error("restartRequired = true for an all-same-value patch, want false")
	}
}

// TestPatchRejectsNonPositiveObsidianInterval is the second half of the
// negative-duration defence: even if a non-positive value reached Patch (the
// handler validates first, but Patch is exported and must not depend on that),
// it must never overwrite the live value — a negative would be Saved to disk
// and read back by the next boot.
func TestPatchRejectsNonPositiveObsidianInterval(t *testing.T) {
	base := Config{ObsidianInterval: 10 * time.Minute}
	for _, raw := range []string{"-1s", "0s", "not-a-duration"} {
		v := raw
		out, restart := Patch(base, ConfigPatch{ObsidianInterval: &v})
		if out.ObsidianInterval != 10*time.Minute {
			t.Errorf("Patch(%q): ObsidianInterval = %v, want the prior 10m to survive", raw, out.ObsidianInterval)
		}
		if restart {
			t.Errorf("Patch(%q): restartRequired = true, want false (nothing changed)", raw)
		}
	}
}

// TestPatchClearsObsidianIntervalOnEmptyString mirrors sync_interval: an
// explicit empty string resets the key to "unset" (the daemon then uses 10m).
func TestPatchClearsObsidianIntervalOnEmptyString(t *testing.T) {
	base := Config{ObsidianInterval: 10 * time.Minute}
	empty := ""
	out, restart := Patch(base, ConfigPatch{ObsidianInterval: &empty})
	if out.ObsidianInterval != 0 {
		t.Errorf("ObsidianInterval = %v, want 0 (unset)", out.ObsidianInterval)
	}
	if !restart {
		t.Error("restartRequired = false, want true (the cadence changed)")
	}
}

// TestRedactExposesObsidianVault covers REQ-WATCH-09's "visible in the redacted
// config read": obsidian_vault is a filesystem path, not a secret, and is
// exposed exactly like db_path.
func TestRedactExposesObsidianVault(t *testing.T) {
	cfg := Config{
		ObsidianVault:       `D:\Vault`,
		ObsidianInterval:    15 * time.Minute,
		ObsidianProject:     "engram",
		ObsidianGraphConfig: "preserve",
	}
	rc := cfg.Redact()
	if rc.ObsidianVault != `D:\Vault` {
		t.Errorf("ObsidianVault = %q, want %q", rc.ObsidianVault, `D:\Vault`)
	}
	if rc.ObsidianInterval != "15m0s" {
		t.Errorf("ObsidianInterval = %q, want %q", rc.ObsidianInterval, "15m0s")
	}
	if rc.ObsidianProject != "engram" {
		t.Errorf("ObsidianProject = %q, want %q", rc.ObsidianProject, "engram")
	}
	if rc.ObsidianGraphConfig != "preserve" {
		t.Errorf("ObsidianGraphConfig = %q, want %q", rc.ObsidianGraphConfig, "preserve")
	}
}

// TestRedactOmitsObsidianWhenOff triangulates: with the feature off, nothing
// obsidian-shaped appears in the redacted read.
func TestRedactOmitsObsidianWhenOff(t *testing.T) {
	rc := Config{}.Redact()
	if rc.ObsidianVault != "" || rc.ObsidianInterval != "" || rc.ObsidianProject != "" || rc.ObsidianGraphConfig != "" {
		t.Errorf("Redact() of a zero Config exposed obsidian values: %+v", rc)
	}
}

// errf keeps the table above readable: each check closure returns a formatted
// error instead of taking *testing.T.
func errf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

// ── Phase 9 review MINOR-2: obsidian_vault had no validation anywhere ───────
//
// A relative obsidian_vault resolves against the daemon process's CWD, which
// under a service manager is arbitrary and surprising. These tests cover
// ValidateObsidianVaultPath directly plus its two call sites: config.Load
// (startup-fatal) and config.Patch (silently discarded, mirroring
// ObsidianInterval's non-positive discard).

// TestValidateObsidianVaultPathRejectsRelative is the direct unit test for
// the new validator.
func TestValidateObsidianVaultPathRejectsRelative(t *testing.T) {
	for _, raw := range []string{"relative/path", "vault", "./vault", "../vault"} {
		err := ValidateObsidianVaultPath(raw)
		if err == nil {
			t.Errorf("ValidateObsidianVaultPath(%q) = nil, want a rejection", raw)
			continue
		}
		if !strings.Contains(err.Error(), "obsidian_vault") {
			t.Errorf("ValidateObsidianVaultPath(%q) error = %q, want it to name obsidian_vault", raw, err.Error())
		}
		if !strings.Contains(err.Error(), "absolute") {
			t.Errorf("ValidateObsidianVaultPath(%q) error = %q, want it to say the path must be absolute", raw, err.Error())
		}
	}
}

// TestValidateObsidianVaultPathAcceptsAbsoluteOrEmpty triangulates: an
// absolute path is accepted, and empty (the feature's OFF switch) is exempt
// from the check entirely.
func TestValidateObsidianVaultPathAcceptsAbsoluteOrEmpty(t *testing.T) {
	dir := t.TempDir() // always absolute
	for _, raw := range []string{dir, filepath.Join(dir, "vault"), ""} {
		if err := ValidateObsidianVaultPath(raw); err != nil {
			t.Errorf("ValidateObsidianVaultPath(%q) = %v, want nil", raw, err)
		}
	}
}

// TestLoadRejectsRelativeObsidianVault covers the startup-fatal half: a
// hand-edited config.json with a relative obsidian_vault must not boot the
// daemon silently against a path resolved from an arbitrary CWD.
func TestLoadRejectsRelativeObsidianVault(t *testing.T) {
	for _, raw := range []string{"relative/vault", "vault", "./vault"} {
		dir := t.TempDir()
		writeConfigJSON(t, dir, `{"obsidian_vault":"`+strings.ReplaceAll(raw, `\`, `\\`)+`"}`)

		cfg, err := Load(dir)
		if err == nil {
			t.Fatalf("Load() with obsidian_vault=%q error = nil, want rejection; stored %q", raw, cfg.ObsidianVault)
		}
		if !strings.Contains(err.Error(), "obsidian_vault") {
			t.Errorf("obsidian_vault=%q: error = %q, want it to name obsidian_vault", raw, err.Error())
		}
		if !strings.Contains(err.Error(), "absolute") {
			t.Errorf("obsidian_vault=%q: error = %q, want it to say the path must be absolute", raw, err.Error())
		}
	}
}

// TestLoadAcceptsAbsoluteObsidianVault triangulates the rejection above.
func TestLoadAcceptsAbsoluteObsidianVault(t *testing.T) {
	dir := t.TempDir()
	vault := filepath.Join(dir, "vault")
	escaped := strings.ReplaceAll(vault, `\`, `\\`)
	writeConfigJSON(t, dir, `{"obsidian_vault":"`+escaped+`"}`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v, want an absolute obsidian_vault to be accepted", err)
	}
	if cfg.ObsidianVault != vault {
		t.Errorf("ObsidianVault = %q, want %q", cfg.ObsidianVault, vault)
	}
}

// TestPatchDiscardsRelativeObsidianVault mirrors
// TestPatchRejectsNonPositiveObsidianInterval: even if a relative path
// reached Patch directly (the handler validates first, but Patch is exported
// and must not depend on that), it must never overwrite the live value — a
// relative path would be Saved to disk and rejected by the NEXT boot's Load,
// bricking it.
func TestPatchDiscardsRelativeObsidianVault(t *testing.T) {
	base := Config{ObsidianVault: `C:\old`}
	for _, raw := range []string{"relative/vault", "vault"} {
		v := raw
		out, restart := Patch(base, ConfigPatch{ObsidianVault: &v})
		if out.ObsidianVault != `C:\old` {
			t.Errorf("Patch(%q): ObsidianVault = %q, want the prior value to survive", raw, out.ObsidianVault)
		}
		if restart {
			t.Errorf("Patch(%q): restartRequired = true, want false (nothing changed)", raw)
		}
	}
}

// TestPatchAcceptsAbsoluteObsidianVault triangulates: a normal absolute patch
// still applies and reports restart-required, unaffected by the new guard.
func TestPatchAcceptsAbsoluteObsidianVault(t *testing.T) {
	base := Config{ObsidianVault: `C:\old`}
	v := `C:\new`
	out, restart := Patch(base, ConfigPatch{ObsidianVault: &v})
	if out.ObsidianVault != `C:\new` {
		t.Errorf("ObsidianVault = %q, want %q", out.ObsidianVault, `C:\new`)
	}
	if !restart {
		t.Error("restartRequired = false, want true")
	}
}

// TestPatchAcceptsEmptyObsidianVault covers the OFF-switch reset: an
// explicit empty string must still be accepted (it is not "relative", it is
// the sentinel for "feature off"), matching config.Load's own exemption.
func TestPatchAcceptsEmptyObsidianVault(t *testing.T) {
	base := Config{ObsidianVault: `C:\old`}
	empty := ""
	out, restart := Patch(base, ConfigPatch{ObsidianVault: &empty})
	if out.ObsidianVault != "" {
		t.Errorf("ObsidianVault = %q, want empty (feature off)", out.ObsidianVault)
	}
	if !restart {
		t.Error("restartRequired = false, want true (the vault changed from set to unset)")
	}
}
