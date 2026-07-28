package main

// This file holds the tests added in response to the Phase 9 fresh-context
// adversarial review (verify-report #4737, "SHIP WITH FIXES"): MAJOR-1 (the
// cmd/engram <-> controlapi adapter had zero coverage), MAJOR-2 (the daemon
// ignoring obsidian_graph_config was undetectable), MINOR-1 (nothing pinned
// that the RESOLVED interval — not the raw config value — reaches the Loop),
// MINOR-3 (a spurious drain warning on early-return shutdown paths) and
// MINOR-5 (config.Load's graph-mode fallback warning firing twice per HTTP
// boot). New file, not appended to daemon_obsidian_test.go, so the review's
// findings stay reviewable as one unit — same convention batch 8 used.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/config"
	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/obsidian"
)

// strp is a tiny helper for building *string patch fields inline.
func strp(s string) *string { return &s }

// ── MAJOR-1: the cmd/engram <-> controlapi adapter had ZERO coverage ────────
//
// internal/controlapi/obsidian_config_test.go drives a MOCK ConfigStore.
// internal/config/obsidian_test.go calls config.Patch/Redact directly.
// Nothing in either package — or anywhere else — built a REAL
// configStoreAdapter and pushed a patch through Apply -> config.Patch ->
// config.Save -> disk -> config.Load -> Redact -> Load. Three deletions at
// the wiring lines proved it: daemon.go:1314-1317 (the ConfigPatch mapping in
// Apply) and daemon.go:1287-1290 (the RedactedConfig mapping in Load) both
// left the full suite green.

// TestConfigStoreAdapterAppliesAndLoadsObsidianKeys drives the REAL adapter —
// not a mock — through exactly the path PUT /api/v1/config uses in
// production: Apply(patch) -> config.Patch -> config.Save -> disk, then
// Load() -> config.Redact -> controlapi.RedactedConfig. This is what
// `engram config set obsidian_vault ...` (cmd/engram/config.go:145) actually
// depends on: it is the ONLY documented way to enable the feature.
func TestConfigStoreAdapterAppliesAndLoadsObsidianKeys(t *testing.T) {
	configDir := t.TempDir()
	vault := filepath.Join(t.TempDir(), "vault")

	adapter := newConfigStoreAdapter(daemonCfg{configDir: configDir}, 0)

	patch := controlapi.ConfigPatch{
		ObsidianVault:       strp(vault),
		ObsidianInterval:    strp("15m"),
		ObsidianProject:     strp("engram"),
		ObsidianGraphConfig: strp("force"),
	}

	restartRequired, err := adapter.Apply(patch)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !restartRequired {
		t.Error("restartRequired = false, want true — all four obsidian_* keys are restart-required")
	}

	// Half 1: the four keys must come back from THIS adapter's own Load() —
	// this is exactly what GET /api/v1/config returns. If the RedactedConfig
	// mapping at daemon.go:1287-1290 were deleted, every field below would be
	// its zero value even though Apply reported success.
	got, err := adapter.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ObsidianVault != vault {
		t.Errorf("Load().ObsidianVault = %q, want %q", got.ObsidianVault, vault)
	}
	if got.ObsidianInterval != "15m0s" {
		t.Errorf("Load().ObsidianInterval = %q, want %q", got.ObsidianInterval, "15m0s")
	}
	if got.ObsidianProject != "engram" {
		t.Errorf("Load().ObsidianProject = %q, want %q", got.ObsidianProject, "engram")
	}
	if got.ObsidianGraphConfig != "force" {
		t.Errorf("Load().ObsidianGraphConfig = %q, want %q", got.ObsidianGraphConfig, "force")
	}

	// Half 2: the values must have reached DISK, independent of this
	// adapter's own in-memory cfg — the next daemon boot reads config.json
	// directly via config.Load, not through this adapter instance. If the
	// ConfigPatch mapping in Apply (daemon.go:1314-1317) were deleted, Apply
	// would still return (true, nil) — config.Patch just wouldn't have
	// changed anything — and config.Save would persist the OLD (empty)
	// values, so this independent read would come back empty.
	onDisk, err := config.Load(configDir)
	if err != nil {
		t.Fatalf("config.Load(configDir): %v", err)
	}
	if onDisk.ObsidianVault != vault {
		t.Errorf("on-disk ObsidianVault = %q, want %q — PUT /api/v1/config wrote nothing", onDisk.ObsidianVault, vault)
	}
	if onDisk.ObsidianInterval != 15*time.Minute {
		t.Errorf("on-disk ObsidianInterval = %v, want 15m", onDisk.ObsidianInterval)
	}
	if onDisk.ObsidianProject != "engram" {
		t.Errorf("on-disk ObsidianProject = %q, want %q", onDisk.ObsidianProject, "engram")
	}
	if onDisk.ObsidianGraphConfig != "force" {
		t.Errorf("on-disk ObsidianGraphConfig = %q, want %q", onDisk.ObsidianGraphConfig, "force")
	}
}

// TestConfigStoreAdapterLoadOmitsObsidianKeysWhenUnset triangulates the test
// above: an adapter that was never patched must not fabricate values.
func TestConfigStoreAdapterLoadOmitsObsidianKeysWhenUnset(t *testing.T) {
	adapter := newConfigStoreAdapter(daemonCfg{configDir: t.TempDir()}, 0)
	got, err := adapter.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ObsidianVault != "" || got.ObsidianInterval != "" || got.ObsidianProject != "" || got.ObsidianGraphConfig != "" {
		t.Errorf("Load() on an unconfigured adapter exposed obsidian values: %+v", got)
	}
}

// TestRunDaemonHTTPExposesObsidianStatusOverAPI covers the other half of
// MAJOR-1: that GET /api/v1/status, served by a REAL running HTTP daemon,
// actually contains obsidian_export. This is the wiring block at
// daemon.go:868-872 (syncAdapter.obsidianLoop/.obsidianVault/.obsidianInterval)
// — deleting it left the full suite green because nothing exercised
// runDaemonHTTP's real status endpoint with the feature turned on.
func TestRunDaemonHTTPExposesObsidianStatusOverAPI(t *testing.T) {
	dir := t.TempDir()
	vaultDir := t.TempDir()
	dbPath := seedObsidianStore(t, dir, "statusprobe")

	cfg := daemonCfg{
		db:                  dbPath,
		syncInterval:        30 * time.Second,
		httpMode:            true,
		httpPort:            0,
		obsidianVault:       vaultDir,
		obsidianInterval:    time.Hour,
		obsidianGraphConfig: "skip",
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errc := make(chan error, 1)
	go func() { errc <- runDaemonHTTP(ctx, cfg) }()

	if !waitForVaultNotes(t, vaultDir, 20*time.Second) {
		cancel()
		t.Fatal("HTTP-mode daemon did not run an export cycle within 20s")
	}

	dj, err := controlapi.ReadDaemonJSON(dir)
	if err != nil {
		cancel()
		t.Fatalf("read daemon.json: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/api/v1/status", dj.Port), nil)
	if err != nil {
		cancel()
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+dj.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET /api/v1/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("GET /api/v1/status: status = %d, want 200", resp.StatusCode)
	}

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		cancel()
		t.Fatalf("decode status: %v", err)
	}
	oe, present := raw["obsidian_export"]
	if !present {
		cancel()
		t.Fatal("obsidian_export absent from a REAL GET /api/v1/status with obsidian_vault set — REQ-WATCH-11 is dead")
	}

	var st controlapi.ObsidianExport
	if err := json.Unmarshal(oe, &st); err != nil {
		cancel()
		t.Fatalf("decode obsidian_export: %v", err)
	}
	if st.Created == 0 {
		cancel()
		t.Errorf("obsidian_export.created = 0, want the seeded observations to have been counted (not a vacuous pass): %+v", st)
	}
	if st.Vault != vaultDir {
		t.Errorf("obsidian_export.vault = %q, want %q", st.Vault, vaultDir)
	}
	if st.Interval != time.Hour.String() {
		t.Errorf("obsidian_export.interval = %q, want %q", st.Interval, time.Hour.String())
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("runDaemonHTTP: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runDaemonHTTP did not return after ctx cancel")
	}
}

// ── MAJOR-2: the configured graph mode must reach the Exporter, positively ──
//
// daemon_obsidian_test.go's TestRunDaemonWithIOStartsObsidianLoop now asserts
// that .obsidian/ is ABSENT under obsidian_graph_config="skip" — that alone
// kills a hardcoded obsidian.GraphConfigForce at daemon.go:716. The test
// below is the positive triangulation: "force" and "preserve" must ALSO
// behave differently from each other through the daemon, not just
// differently from "skip".

// TestBuildDaemonAppliesConfiguredGraphConfigModeForce seeds
// .obsidian/graph.json with sentinel content, runs a real cycle with
// obsidian_graph_config="force", and asserts the sentinel was overwritten —
// force's whole contract.
func TestBuildDaemonAppliesConfiguredGraphConfigModeForce(t *testing.T) {
	sentinel := []byte(`{"sentinel":"pre-existing user content, must be overwritten by force"}`)
	got := runOneCycleAndReadGraphJSON(t, "force", sentinel)
	if bytes.Equal(got, sentinel) {
		t.Error("graph.json unchanged after a cycle with obsidian_graph_config=\"force\" — the configured mode did not reach the Exporter")
	}
}

// TestBuildDaemonAppliesConfiguredGraphConfigModePreserve is the mirror:
// "preserve" must leave existing sentinel content byte-identical.
func TestBuildDaemonAppliesConfiguredGraphConfigModePreserve(t *testing.T) {
	sentinel := []byte(`{"sentinel":"pre-existing user content, must survive preserve"}`)
	got := runOneCycleAndReadGraphJSON(t, "preserve", sentinel)
	if !bytes.Equal(got, sentinel) {
		t.Errorf("graph.json changed after a cycle with obsidian_graph_config=\"preserve\": got %q, want the sentinel untouched", got)
	}
}

// runOneCycleAndReadGraphJSON seeds {vault}/.obsidian/graph.json with
// sentinel, builds and starts a daemon with the given graph mode against a
// store containing at least one observation, waits for the first cycle to
// complete, and returns the post-cycle file content.
func runOneCycleAndReadGraphJSON(t *testing.T, graphMode string, sentinel []byte) []byte {
	t.Helper()
	dbDir := t.TempDir()
	vaultDir := t.TempDir()

	obsidianDir := filepath.Join(vaultDir, ".obsidian")
	if err := os.MkdirAll(obsidianDir, 0755); err != nil {
		t.Fatalf("mkdir .obsidian: %v", err)
	}
	if err := os.WriteFile(filepath.Join(obsidianDir, "graph.json"), sentinel, 0644); err != nil {
		t.Fatalf("seed graph.json: %v", err)
	}

	seed, err := localstore.Open(filepath.Join(dbDir, "engram.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := seed.AddObservation(localstore.AddObservationParams{
		SessionID: "sess-1",
		Type:      "decision",
		Title:     "Probe",
		Content:   "Body",
		Project:   "graphmodeprobe",
	}); err != nil {
		t.Fatalf("seed observation: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	components, err := buildDaemon(daemonCfg{
		db:                  filepath.Join(dbDir, "engram.db"),
		syncInterval:        30 * time.Second,
		obsidianVault:       vaultDir,
		obsidianInterval:    time.Hour,
		obsidianGraphConfig: graphMode,
	})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	defer components.Close()

	components.obsidianLoop.Start(t.Context())
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !components.obsidianLoop.LastResult().At.IsZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if components.obsidianLoop.LastResult().At.IsZero() {
		t.Fatal("first export cycle did not complete within 15s")
	}

	got, err := os.ReadFile(filepath.Join(obsidianDir, "graph.json"))
	if err != nil {
		t.Fatalf("read graph.json after cycle: %v", err)
	}
	return got
}

// ── MINOR-1: the RESOLVED interval, not the raw config value, must reach the
// Loop ────────────────────────────────────────────────────────────────────

// TestBuildDaemonResolvedIntervalReachesLoop pins that obsidian.NewLoop is
// constructed with resolveObsidianInterval's OUTPUT, not the raw
// cfg.obsidianInterval. A negative interval is the input that makes the two
// layers disagree (see the comments on resolveObsidianInterval and
// applyLoopDefaults): the daemon resolves a negative value to its 10m
// DEFAULT, while obsidian.Loop's own floor would clamp the identical raw
// value to its 1m FLOOR. If daemon.go:723 ever passed cfg.obsidianInterval
// straight through instead of the resolved value, this test observes 1m
// where it expects 10m.
func TestBuildDaemonResolvedIntervalReachesLoop(t *testing.T) {
	dbDir := t.TempDir()
	vaultDir := t.TempDir()

	raw := -5 * time.Second
	components, err := buildDaemon(daemonCfg{
		db:                  filepath.Join(dbDir, "engram.db"),
		syncInterval:        30 * time.Second,
		obsidianVault:       vaultDir,
		obsidianInterval:    raw,
		obsidianGraphConfig: "skip",
	})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	defer components.Close()

	want, _ := resolveObsidianInterval(raw)
	got := components.obsidianLoop.ConfiguredInterval()
	if got != want {
		t.Errorf("obsidianLoop.ConfiguredInterval() = %v, want the daemon-resolved %v (raw cfg.obsidianInterval=%v must not reach the Loop unresolved)", got, want, raw)
	}
	// Triangulate: prove this isn't vacuously passing because the floor
	// happens to coincide — 10m (the daemon's default) and 1m (the Loop's own
	// floor for a negative value) are different, so the assertion above is
	// only satisfiable by the correct wiring.
	if want != 10*time.Minute {
		t.Fatalf("test setup error: resolveObsidianInterval(%v) = %v, want 10m (test assumption broken)", raw, want)
	}
}

// TestBuildDaemonNormalIntervalReachesLoopUnchanged triangulates the
// pinning test above with a normal, already-valid value: no resolution
// drama, straight pass-through.
func TestBuildDaemonNormalIntervalReachesLoopUnchanged(t *testing.T) {
	dbDir := t.TempDir()
	vaultDir := t.TempDir()

	components, err := buildDaemon(daemonCfg{
		db:                  filepath.Join(dbDir, "engram.db"),
		syncInterval:        30 * time.Second,
		obsidianVault:       vaultDir,
		obsidianInterval:    5 * time.Minute,
		obsidianGraphConfig: "skip",
	})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	defer components.Close()

	if got := components.obsidianLoop.ConfiguredInterval(); got != 5*time.Minute {
		t.Errorf("obsidianLoop.ConfiguredInterval() = %v, want 5m unchanged", got)
	}
}

// ── MINOR-3: Close() must not warn about a drain that could not be in flight
// ─────────────────────────────────────────────────────────────────────────

// TestCloseSilentWhenObsidianLoopNeverStarted covers every early return in
// runDaemonHTTP before its obsidianLoop.Start(ctx) call (port already in
// use, token generation failure, WriteDaemonJSON failure): the deferred
// components.Close() must not print the "waiting for any in-flight export
// cycle" line when no cycle could ever have been in flight.
// obsidian.Loop.Stop() was ALREADY a safe, fast no-op on a never-started
// loop (see Stop's own !l.started guard) — only the log line was wrong,
// gated on obsidianLoop's nil-ness alone rather than on whether Start ever
// ran.
func TestCloseSilentWhenObsidianLoopNeverStarted(t *testing.T) {
	dbDir := t.TempDir()
	store, err := localstore.Open(filepath.Join(dbDir, "engram.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	fake := &countingFakeExportable{result: &obsidian.ExportResult{}}
	components := &daemonComponents{
		store: store,
		obsidianLoop: obsidian.NewLoop(fake, obsidian.LoopConfig{
			Interval: time.Hour,
			Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		}),
		// obsidianStarted deliberately left false: Start() was never called,
		// mirroring an early return in runDaemonHTTP before its Start() line.
	}

	var logBuf bytes.Buffer
	restore := captureStdLog(&logBuf)

	done := make(chan struct{})
	go func() {
		components.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		restore()
		t.Fatal("Close() did not return promptly for a never-started loop — Stop() should be an immediate no-op")
	}
	restore()

	if strings.Contains(strings.ToLower(logBuf.String()), "obsidian") {
		t.Errorf("Close() logged about obsidian for a loop that was never started: %q", logBuf.String())
	}
}

// ── MINOR-5: config.Load's graph-mode fallback warning must not double-fire
// ─────────────────────────────────────────────────────────────────────────

// TestNewConfigStoreAdapterSkipsReloadWhenFileConfigCached covers the
// production HTTP-boot path directly: when daemonCfg carries an
// already-loaded config.Config (as runDaemonCmd now populates it),
// newConfigStoreAdapter must NOT re-parse config.json — proven here by
// pointing configDir at a file whose obsidian_graph_config would trigger
// config.Load's fallback warning, and observing that constructing the
// adapter logs nothing at all.
func TestNewConfigStoreAdapterSkipsReloadWhenFileConfigCached(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"obsidian_graph_config":"presrve"}`), 0600); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	cfg := daemonCfg{
		configDir: dir,
		loadedFileConfig: config.Config{
			// A cached Config that has ALREADY been through the fallback
			// (ObsidianGraphConfig is the normalised "preserve", not the raw
			// "presrve" on disk) — exactly what runDaemonCmd's earlier
			// config.Load call would have produced and warned about once.
			ObsidianGraphConfig: "preserve",
		},
		loadedFileConfigCached: true,
	}

	var logBuf bytes.Buffer
	restore := captureStdLog(&logBuf)
	adapter := newConfigStoreAdapter(cfg, 0)
	restore()

	if got := logBuf.String(); got != "" {
		t.Errorf("newConfigStoreAdapter logged %q with a cached file config — it must reuse the cache, not re-parse config.json", got)
	}
	loaded, err := adapter.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ObsidianGraphConfig != "preserve" {
		t.Errorf("ObsidianGraphConfig = %q, want the cached %q to have been used", loaded.ObsidianGraphConfig, "preserve")
	}
}

// TestNewConfigStoreAdapterFallsBackToLoadWhenNotCached triangulates the test
// above: WITHOUT a cached file config (every pre-existing test in this
// package constructs daemonCfg{} literals directly, bypassing runDaemonCmd),
// newConfigStoreAdapter must still fall back to its own config.Load — proving
// the caching path is a real branch and not a vacuous no-op.
func TestNewConfigStoreAdapterFallsBackToLoadWhenNotCached(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"obsidian_graph_config":"presrve"}`), 0600); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	cfg := daemonCfg{configDir: dir} // loadedFileConfigCached left false

	var logBuf bytes.Buffer
	restore := captureStdLog(&logBuf)
	adapter := newConfigStoreAdapter(cfg, 0)
	restore()

	if got := logBuf.String(); !strings.Contains(got, "obsidian_graph_config") {
		t.Errorf("newConfigStoreAdapter without a cached config logged %q, want the fallback warning (proves the fallback Load still runs)", got)
	}
	loaded, err := adapter.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ObsidianGraphConfig != "preserve" {
		t.Errorf("ObsidianGraphConfig = %q, want the fallback %q", loaded.ObsidianGraphConfig, "preserve")
	}
}

// TestGraphModeFallbackWarnsExactlyOnceAcrossHTTPBoot reproduces the exact
// sequence the review flagged: runDaemonCmd's own config.Load (daemon.go:391)
// followed by newConfigStoreAdapter's (daemon.go:1222, guarded by the cache
// added for MINOR-5) for the SAME misconfigured file, and counts the total
// number of times the warning fires across both. Before the fix this counted
// 2; the fix caches the first Load's result so the second call is a no-op.
func TestGraphModeFallbackWarnsExactlyOnceAcrossHTTPBoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"obsidian_graph_config":"presrve"}`), 0600); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	var logBuf bytes.Buffer
	restore := captureStdLog(&logBuf)

	// Step 1: mirrors runDaemonCmd's ONE config.Load call.
	fileCfg, err := config.Load(dir)
	if err != nil {
		restore()
		t.Fatalf("config.Load: %v", err)
	}
	cfg := daemonCfg{
		configDir:              dir,
		loadedFileConfig:       fileCfg,
		loadedFileConfigCached: true,
	}

	// Step 2: mirrors runDaemonHTTP's newConfigStoreAdapter call.
	_ = newConfigStoreAdapter(cfg, 0)

	restore()
	got := logBuf.String()
	count := strings.Count(got, "obsidian_graph_config")
	if count != 1 {
		t.Errorf("graph-mode fallback warning fired %d times across one simulated HTTP boot, want exactly 1; log = %q", count, got)
	}
}
