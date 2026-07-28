package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/obsidian"
)

// ── 9.9/9.10: the store-ownership guarantees ─────────────────────────────────

// TestLocalstoreSatisfiesObsidianStoreReader documents that
// obsidianStoreAdapter is currently behaviour-free: *localstore.Store already
// satisfies obsidian.StoreReader STRUCTURALLY, with no translation at all. The
// adapter exists so there is exactly ONE boundary for both callers (the manual
// CLI and the daemon loop) and one place to fix if localstore ever changes a
// signature — not because a conversion is needed.
//
// If this assertion ever fails, the adapter has silently become load-bearing
// and its "pure 1:1 pass-through" comment has become a lie.
var _ obsidian.StoreReader = (*localstore.Store)(nil)

func TestLocalstoreSatisfiesObsidianStoreReader(t *testing.T) {
	// The compile-time assertion above is the real check; this test exists so
	// the guarantee has a name in the test output and a place for the reason.
	var r obsidian.StoreReader = (*localstore.Store)(nil)
	if r == nil {
		t.Fatal("unreachable: typed nil in an interface is never == nil")
	}
}

// ── 9.7/9.8: interval and graph-mode resolution ──────────────────────────────

// TestObsidianIntervalDefaultsToTenMinutes covers REQ-WATCH-04's default.
func TestObsidianIntervalDefaultsToTenMinutes(t *testing.T) {
	got, warn := resolveObsidianInterval(0)
	if got != 10*time.Minute {
		t.Errorf("resolveObsidianInterval(0) = %v, want 10m", got)
	}
	if warn != "" {
		t.Errorf("unset interval produced warning %q, want silence", warn)
	}
}

// TestObsidianIntervalBelowOneMinuteIsClampedWithWarning covers REQ-WATCH-04's
// floor: a positive sub-minute cadence is clamped to 1m and WARNED about — it
// is deliberately not a startup abort, because refusing to boot the process
// that owns MCP, autosync and the store over a too-small export cadence is a
// strictly worse failure than exporting less often than asked.
func TestObsidianIntervalBelowOneMinuteIsClampedWithWarning(t *testing.T) {
	for _, in := range []time.Duration{time.Nanosecond, time.Second, 30 * time.Second, 59 * time.Second} {
		got, warn := resolveObsidianInterval(in)
		if got != time.Minute {
			t.Errorf("resolveObsidianInterval(%v) = %v, want the 1m floor", in, got)
		}
		if !strings.Contains(warn, "obsidian_interval") {
			t.Errorf("resolveObsidianInterval(%v) warning = %q, want it to name obsidian_interval", in, warn)
		}
		if !strings.Contains(warn, in.String()) {
			t.Errorf("resolveObsidianInterval(%v) warning = %q, want it to quote the requested value", in, warn)
		}
	}
}

// TestObsidianIntervalNonPositiveFallsBackToDefault is the daemon's own layer
// of the negative-duration defence. config.Load already rejects a non-positive
// obsidian_interval outright, so this branch should be unreachable from a
// config file — but daemonCfg is a plain struct that any caller can build, and
// a negative Interval reaching a time.Timer is the failure that produced Phase
// 8 CRITICAL-1 (measured: 1,024,805 cycles/sec from math.MinInt64). Falling
// back to the default matches how BOTH sibling loops read Interval <= 0.
func TestObsidianIntervalNonPositiveFallsBackToDefault(t *testing.T) {
	for _, in := range []time.Duration{-time.Nanosecond, -time.Second, -10 * time.Minute, time.Duration(math.MinInt64)} {
		got, warn := resolveObsidianInterval(in)
		if got != 10*time.Minute {
			t.Errorf("resolveObsidianInterval(%v) = %v, want the 10m default", in, got)
		}
		if got <= 0 {
			t.Errorf("resolveObsidianInterval(%v) returned a NON-POSITIVE %v — a timer would spin", in, got)
		}
		if !strings.Contains(warn, "obsidian_interval") {
			t.Errorf("resolveObsidianInterval(%v) warning = %q, want a warning naming obsidian_interval", in, warn)
		}
	}
}

// TestObsidianIntervalNormalValuePassesThrough is the control case.
func TestObsidianIntervalNormalValuePassesThrough(t *testing.T) {
	for _, in := range []time.Duration{time.Minute, 10 * time.Minute, 2 * time.Hour} {
		got, warn := resolveObsidianInterval(in)
		if got != in {
			t.Errorf("resolveObsidianInterval(%v) = %v, want it unchanged", in, got)
		}
		if warn != "" {
			t.Errorf("resolveObsidianInterval(%v) warning = %q, want silence", in, warn)
		}
	}
}

// TestObsidianGraphConfigDefaultsToPreserve covers REQ-GRAPH-01: the daemon's
// default matches the CLI's. Defaulting the daemon to "skip" was considered and
// rejected — a user who only ever runs the daemon would then never get the
// curated graph, which is the entire visual point of the feature.
func TestObsidianGraphConfigDefaultsToPreserve(t *testing.T) {
	got, warn := resolveObsidianGraphConfig("")
	if got != obsidian.GraphConfigPreserve {
		t.Errorf("resolveObsidianGraphConfig(\"\") = %q, want %q", got, obsidian.GraphConfigPreserve)
	}
	if warn != "" {
		t.Errorf("unset graph config produced warning %q, want silence", warn)
	}
}

// TestObsidianGraphConfigParsesValidModes triangulates the default above.
func TestObsidianGraphConfigParsesValidModes(t *testing.T) {
	cases := map[string]obsidian.GraphConfigMode{
		"preserve": obsidian.GraphConfigPreserve,
		"force":    obsidian.GraphConfigForce,
		"skip":     obsidian.GraphConfigSkip,
	}
	for in, want := range cases {
		got, warn := resolveObsidianGraphConfig(in)
		if got != want {
			t.Errorf("resolveObsidianGraphConfig(%q) = %q, want %q", in, got, want)
		}
		if warn != "" {
			t.Errorf("resolveObsidianGraphConfig(%q) warning = %q, want silence", in, warn)
		}
	}
}

// TestObsidianGraphConfigUnknownFallsBackToPreserve pins the lenient posture at
// the daemon layer too (config.Load already normalises, but daemonCfg is a
// plain struct).
func TestObsidianGraphConfigUnknownFallsBackToPreserve(t *testing.T) {
	got, warn := resolveObsidianGraphConfig("presrve")
	if got != obsidian.GraphConfigPreserve {
		t.Errorf("resolveObsidianGraphConfig(%q) = %q, want the preserve fallback", "presrve", got)
	}
	if !strings.Contains(warn, "presrve") {
		t.Errorf("warning = %q, want it to quote the rejected value", warn)
	}
}

// ── 9.5/9.6: buildDaemon construction and inertness ──────────────────────────

// TestBuildDaemonSkipsObsidianLoopWhenVaultUnset covers REQ-WATCH-09's "OFF
// MUST mean inert, in the strongest sense": no loop, no goroutine, no directory
// created, no file probed, nothing written anywhere, and no log line about it.
//
// The empty-directory assertion is the load-bearing half. A test that only
// checked `obsidianLoop == nil` would still pass if buildDaemon had, say,
// mkdir'd the vault before discovering the path was empty.
func TestBuildDaemonSkipsObsidianLoopWhenVaultUnset(t *testing.T) {
	dbDir := t.TempDir()
	vaultDir := t.TempDir() // stays empty: nothing may touch it

	var logBuf bytes.Buffer
	restore := captureStdLog(&logBuf)

	components, err := buildDaemon(daemonCfg{
		db:           filepath.Join(dbDir, "engram.db"),
		syncInterval: 30 * time.Second,
		// obsidianVault deliberately unset — the master OFF switch.
	})
	restore()
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	defer components.Close()

	if components.obsidianLoop != nil {
		t.Error("obsidianLoop is non-nil with obsidian_vault unset; the feature must be inert")
	}

	entries, err := os.ReadDir(vaultDir)
	if err != nil {
		t.Fatalf("read vault dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the would-be vault directory is not empty: %v — OFF must write nothing", names)
	}

	if strings.Contains(strings.ToLower(logBuf.String()), "obsidian") {
		t.Errorf("buildDaemon logged about obsidian while the feature is off: %q", logBuf.String())
	}
}

// TestBuildDaemonConstructsObsidianLoopWhenVaultSet is the triangulation
// partner: presence of the coordinate IS the switch.
func TestBuildDaemonConstructsObsidianLoopWhenVaultSet(t *testing.T) {
	dbDir := t.TempDir()
	vaultDir := t.TempDir()

	components, err := buildDaemon(daemonCfg{
		db:                  filepath.Join(dbDir, "engram.db"),
		syncInterval:        30 * time.Second,
		obsidianVault:       vaultDir,
		obsidianInterval:    time.Hour,
		obsidianGraphConfig: "skip", // keep the test off .obsidian/ entirely
	})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	defer components.Close()

	if components.obsidianLoop == nil {
		t.Fatal("obsidianLoop is nil with obsidian_vault set; the loop must be constructed")
	}
	// Constructing must not have written anything yet — the first cycle only
	// runs after Start.
	entries, err := os.ReadDir(vaultDir)
	if err != nil {
		t.Fatalf("read vault dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("buildDaemon wrote into the vault before Start: %d entries", len(entries))
	}
	if out := components.obsidianLoop.LastResult(); !out.At.IsZero() {
		t.Errorf("LastResult().At = %v before Start, want the zero value", out.At)
	}
}

// TestBuildDaemonWarnsWhenObsidianProjectSet covers REQ-WATCH-11's REQUIRED
// startup warning. This is the agreed mitigation for verify-report #4710's
// deferred MAJOR-3, which remains otherwise unfixed: a project-filtered run
// renders cross-project session/topic hubs from that scope only (truncating
// them) and disables the stale-hub sweep. Under a daemon that now happens every
// cycle, forever — so it has to be loud.
func TestBuildDaemonWarnsWhenObsidianProjectSet(t *testing.T) {
	dbDir := t.TempDir()

	var logBuf bytes.Buffer
	restore := captureStdLog(&logBuf)
	components, err := buildDaemon(daemonCfg{
		db:                  filepath.Join(dbDir, "engram.db"),
		syncInterval:        30 * time.Second,
		obsidianVault:       t.TempDir(),
		obsidianProject:     "engram",
		obsidianGraphConfig: "skip",
	})
	restore()
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	defer components.Close()

	got := logBuf.String()
	if !strings.Contains(got, "obsidian_project") {
		t.Errorf("startup log = %q, want a warning naming obsidian_project", got)
	}
	if !strings.Contains(got, "engram") {
		t.Errorf("startup log = %q, want the warning to quote the configured project", got)
	}
	if !strings.Contains(got, "hub") {
		t.Errorf("startup log = %q, want the warning to name the hub consequence", got)
	}
}

// TestBuildDaemonNoProjectFilterNoWarning triangulates the warning above: the
// default deployment (no filter) must not emit it.
func TestBuildDaemonNoProjectFilterNoWarning(t *testing.T) {
	dbDir := t.TempDir()

	var logBuf bytes.Buffer
	restore := captureStdLog(&logBuf)
	components, err := buildDaemon(daemonCfg{
		db:                  filepath.Join(dbDir, "engram.db"),
		syncInterval:        30 * time.Second,
		obsidianVault:       t.TempDir(),
		obsidianGraphConfig: "skip",
	})
	restore()
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	defer components.Close()

	if strings.Contains(logBuf.String(), "obsidian_project") {
		t.Errorf("startup log = %q, want no project-filter warning when no filter is set", logBuf.String())
	}
}

// ── 9.5: shutdown ordering ───────────────────────────────────────────────────

// blockingProbeExportable is an Exportable that reports whether the daemon's
// store was still usable at the moment the cycle ran, and blocks until the test
// releases it so the shutdown race is real rather than theoretical.
type blockingProbeExportable struct {
	entered  chan struct{}
	release  chan struct{}
	mu       sync.Mutex
	storeErr error
	ran      bool
	probe    func() error
}

func (e *blockingProbeExportable) Export() (*obsidian.ExportResult, error) {
	close(e.entered)
	<-e.release
	err := e.probe()
	e.mu.Lock()
	e.ran = true
	e.storeErr = err
	e.mu.Unlock()
	return &obsidian.ExportResult{}, nil
}

func (e *blockingProbeExportable) SetGraphConfig(obsidian.GraphConfigMode) {}

func (e *blockingProbeExportable) result() (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ran, e.storeErr
}

// TestObsidianLoopStoppedBeforeStoreClose is the shutdown-ordering guarantee.
// daemonComponents.Close() must stop the export loop — which BLOCKS until the
// in-flight cycle finishes — before closing the store. A cycle mid
// RecentObservations against a closed DB is the exact failure being prevented,
// and Loop.Stop()'s drain is only useful if the store is still alive for it.
//
// The test makes the ordering observable: the fake reads through the daemon's
// real store INSIDE the cycle, while Close() is already running.
func TestObsidianLoopStoppedBeforeStoreClose(t *testing.T) {
	dbDir := t.TempDir()
	store, err := localstore.Open(filepath.Join(dbDir, "engram.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	fake := &blockingProbeExportable{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		probe: func() error {
			_, err := store.ListProjects()
			return err
		},
	}
	components := &daemonComponents{
		store: store,
		obsidianLoop: obsidian.NewLoop(fake, obsidian.LoopConfig{
			Interval: time.Hour,
			Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		}),
	}

	components.obsidianLoop.Start(t.Context())
	components.obsidianStarted = true // mirrors what both run modes do immediately after Start (MINOR-3)
	<-fake.entered                    // a cycle is now in flight

	closed := make(chan struct{})
	go func() {
		components.Close()
		close(closed)
	}()

	// Close() must still be blocked on the drain.
	select {
	case <-closed:
		t.Fatal("Close() returned while an export cycle was still in flight — the store could be closed under a live read")
	case <-time.After(150 * time.Millisecond):
	}

	close(fake.release)

	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close() did not return after the cycle was released")
	}

	ran, storeErr := fake.result()
	if !ran {
		t.Fatal("the export cycle never completed")
	}
	if storeErr != nil {
		t.Errorf("the in-flight cycle saw a store error during shutdown: %v — the store was closed before the loop drained", storeErr)
	}
	// And the store IS closed afterwards.
	if _, err := store.ListProjects(); err == nil {
		t.Error("store is still usable after Close(); it must be closed once the loops have stopped")
	}
}

// TestCloseLogsBeforeBlockingOnObsidianDrain covers REQ-WATCH-05's "SHOULD log
// that it is waiting" clause, which the Phase 8 review (MINOR-2) explicitly
// handed to the daemon. A measured COLD first cycle over ~4,650 observations
// takes ~18 seconds; an 18-second silent hang at shutdown reads as a deadlock
// and gets SIGKILLed by an impatient operator mid-write.
//
// The log must be emitted BEFORE Stop() blocks, not after it returns.
func TestCloseLogsBeforeBlockingOnObsidianDrain(t *testing.T) {
	dbDir := t.TempDir()
	store, err := localstore.Open(filepath.Join(dbDir, "engram.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	fake := &blockingProbeExportable{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		probe:   func() error { return nil },
	}
	components := &daemonComponents{
		store: store,
		obsidianLoop: obsidian.NewLoop(fake, obsidian.LoopConfig{
			Interval: time.Hour,
			Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		}),
	}

	var logBuf lockedBuffer
	restore := captureStdLog(&logBuf)
	defer restore()

	components.obsidianLoop.Start(t.Context())
	components.obsidianStarted = true // mirrors what both run modes do immediately after Start (MINOR-3)
	<-fake.entered

	closed := make(chan struct{})
	go func() {
		components.Close()
		close(closed)
	}()

	// While Close() is still blocked on the drain, the explanation must ALREADY
	// be in the log.
	time.Sleep(150 * time.Millisecond)
	during := logBuf.String()
	if !strings.Contains(strings.ToLower(during), "obsidian") {
		t.Errorf("nothing logged about obsidian while Close() was blocked; log = %q", during)
	}
	if !strings.Contains(strings.ToLower(during), "wait") {
		t.Errorf("log = %q, want it to say the daemon is WAITING (an unexplained multi-second hang reads as a deadlock)", during)
	}

	close(fake.release)
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close() did not return")
	}
	restore()
}

// TestCloseSilentWhenObsidianDisabled triangulates the log above: with the
// feature off, shutdown says nothing about it.
func TestCloseSilentWhenObsidianDisabled(t *testing.T) {
	dbDir := t.TempDir()
	store, err := localstore.Open(filepath.Join(dbDir, "engram.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	components := &daemonComponents{store: store}

	var logBuf bytes.Buffer
	restore := captureStdLog(&logBuf)
	components.Close()
	restore()

	if strings.Contains(strings.ToLower(logBuf.String()), "obsidian") {
		t.Errorf("Close() logged about obsidian with the feature off: %q", logBuf.String())
	}
}

// ── constraint 16: nothing on the daemon export path may write to stdout ─────

// TestDaemonObsidianPathWritesNothingToStdout is the daemon-level half of
// design constraint 16. In its DEFAULT mode the daemon serves MCP JSON-RPC over
// stdout (runDaemonWithIO -> mcpserver.NewStdioServer(...).Listen(ctx, stdin,
// stdout)), so a single stray print from the export path injects non-JSON into
// the framing and breaks EVERY connected MCP client.
//
// internal/obsidian already has TestLoopWritesNothingToStdout over the Loop in
// isolation. This one covers the wiring: the exporter the DAEMON constructs,
// driven by the loop the DAEMON constructs, against a real store — including
// the graph-config bootstrap (preserve) and the state-file write, which the
// isolated Loop test fakes away.
func TestDaemonObsidianPathWritesNothingToStdout(t *testing.T) {
	dbDir := t.TempDir()
	vaultDir := t.TempDir()

	// Seed the store so the cycle has real work to do — a cycle over an empty
	// store would exercise almost none of the write paths.
	seed, err := localstore.Open(filepath.Join(dbDir, "engram.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for i := 0; i < 3; i++ {
		_, err := seed.AddObservation(localstore.AddObservationParams{
			SessionID: "sess-1",
			Type:      "decision",
			Title:     fmt.Sprintf("Probe %d", i),
			Content:   fmt.Sprintf("Body content %d", i),
			Project:   "stdoutprobe",
		})
		if err != nil {
			t.Fatalf("seed observation: %v", err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	components, err := buildDaemon(daemonCfg{
		db:                  filepath.Join(dbDir, "engram.db"),
		syncInterval:        30 * time.Second,
		obsidianVault:       vaultDir,
		obsidianInterval:    time.Hour,
		obsidianGraphConfig: "preserve", // exercise the .obsidian/graph.json write too
	})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}

	captured := captureStdoutBytes(t, func() {
		components.obsidianLoop.Start(t.Context())
		// Wait for the first cycle to complete.
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if !components.obsidianLoop.LastResult().At.IsZero() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("first export cycle did not complete within 15s")
	})
	components.Close()

	if len(captured) != 0 {
		t.Errorf("the daemon's Obsidian export path wrote %d bytes to stdout: %q — stdout is the MCP JSON-RPC channel", len(captured), captured)
	}

	// Prove the cycle actually did the work whose silence we just asserted;
	// otherwise this would be a vacuous pass.
	out := components.obsidianLoop.LastResult()
	if out.Created == 0 {
		t.Errorf("LastResult().Created = 0, want the seeded observations to have been exported (counters: %+v)", out)
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "engram")); err != nil {
		t.Errorf("vault namespace not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(vaultDir, ".obsidian", "graph.json")); err != nil {
		t.Errorf("graph config not bootstrapped under preserve: %v", err)
	}
}

// ── 9.6: the loop is started in BOTH run modes ───────────────────────────────

// seedObsidianStore creates a db with a few observations and returns its path.
func seedObsidianStore(t *testing.T, dir string, project string) string {
	t.Helper()
	dbPath := filepath.Join(dir, "engram.db")
	seed, err := localstore.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for i := 0; i < 3; i++ {
		_, err := seed.AddObservation(localstore.AddObservationParams{
			SessionID: "sess-1",
			Type:      "decision",
			Title:     fmt.Sprintf("Seed %d", i),
			Content:   fmt.Sprintf("Seed body %d", i),
			Project:   project,
		})
		if err != nil {
			t.Fatalf("seed observation: %v", err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	return dbPath
}

// waitForVaultNotes polls until the vault contains at least one exported note.
func waitForVaultNotes(t *testing.T, vaultDir string, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	stateFile := filepath.Join(vaultDir, "engram", ".engram-sync-state.json")
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stateFile); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TestRunDaemonWithIOStartsObsidianLoop covers REQ-WATCH-09's "the loop MUST be
// started in BOTH daemon run modes". This is the stdio-MCP mode — the DEFAULT
// transport. Starting the loop in only one mode is the asymmetry that ships as
// "works on my machine": a user on the default stdio transport is entitled to a
// fresh vault too.
//
// The assertion is end-to-end on purpose: it does not inspect a struct field,
// it waits for the export to actually land on disk.
func TestRunDaemonWithIOStartsObsidianLoop(t *testing.T) {
	dir := t.TempDir()
	vaultDir := t.TempDir()
	dbPath := seedObsidianStore(t, dir, "stdiomode")

	cfg := daemonCfg{
		db:                  dbPath,
		syncInterval:        30 * time.Second,
		obsidianVault:       vaultDir,
		obsidianInterval:    time.Hour, // one cycle is enough
		obsidianGraphConfig: "skip",
	}

	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errc := make(chan error, 1)
	go func() { errc <- runDaemonWithIO(ctx, cfg, pr, io.Discard) }()

	if !waitForVaultNotes(t, vaultDir, 20*time.Second) {
		cancel()
		t.Fatal("stdio-mode daemon did not run an export cycle within 20s — the loop was not started in runDaemonWithIO")
	}

	// MAJOR-2 (Phase 9 review): obsidian_graph_config="skip" MUST mean the
	// daemon never touches .obsidian/ at all. A hardcoded GraphConfigForce at
	// the daemon's construction site would satisfy every OTHER assertion in
	// this test (an export cycle still runs and the state file still appears)
	// while silently overwriting a user's hand-tuned graph.json — this is the
	// one assertion that would catch it. Checked AFTER a completed cycle
	// (waitForVaultNotes above already proved one ran), so a false pass from
	// "the cycle just hasn't happened yet" is not possible.
	if _, err := os.Stat(filepath.Join(vaultDir, ".obsidian")); !os.IsNotExist(err) {
		cancel()
		t.Fatalf(".obsidian directory exists under obsidian_graph_config=skip (stat err=%v) — skip must never write anything under .obsidian/", err)
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("runDaemonWithIO: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runDaemonWithIO did not return after ctx cancel")
	}
}

// TestRunDaemonHTTPStartsObsidianLoop is the other half of REQ-WATCH-09's
// both-modes requirement.
func TestRunDaemonHTTPStartsObsidianLoop(t *testing.T) {
	dir := t.TempDir()
	vaultDir := t.TempDir()
	dbPath := seedObsidianStore(t, dir, "httpmode")

	cfg := daemonCfg{
		db:                  dbPath,
		syncInterval:        30 * time.Second,
		httpMode:            true,
		httpPort:            0, // let the kernel pick — no collision with a real daemon
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
		t.Fatal("HTTP-mode daemon did not run an export cycle within 20s — the loop was not started in runDaemonHTTP")
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

// ── 9.11/9.12: status surface ────────────────────────────────────────────────

// TestStatusOmitsObsidianExportWhenLoopNil covers REQ-WATCH-11 at the adapter
// layer: the field is ABSENT (nil), never an all-zero object, when the feature
// is off — a permanent zero object would misreport a disabled feature as a
// healthy one.
func TestStatusOmitsObsidianExportWhenLoopNil(t *testing.T) {
	a := &runtimeSyncAdapter{}
	if got := a.obsidianStatus(); got != nil {
		t.Errorf("obsidianStatus() = %+v, want nil when no loop is configured", got)
	}
}

// TestStatusIncludesObsidianExportFromLoop triangulates: with a loop present,
// the counters come from Loop.LastResult() and the coordinates from config.
func TestStatusIncludesObsidianExportFromLoop(t *testing.T) {
	fake := &countingFakeExportable{result: &obsidian.ExportResult{Created: 7, Hubs: 2}}
	loop := obsidian.NewLoop(fake, obsidian.LoopConfig{
		Interval: time.Hour,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	a := &runtimeSyncAdapter{
		obsidianLoop:     loop,
		obsidianVault:    `D:\Vault`,
		obsidianInterval: 10 * time.Minute,
	}

	// Before any cycle: present (the feature IS on) but with a nil timestamp.
	got := a.obsidianStatus()
	if got == nil {
		t.Fatal("obsidianStatus() = nil with a loop configured")
	}
	if got.LastExportAt != nil {
		t.Errorf("LastExportAt = %v before any cycle, want nil", got.LastExportAt)
	}
	if got.Vault != `D:\Vault` {
		t.Errorf("Vault = %q, want %q", got.Vault, `D:\Vault`)
	}
	if got.Interval != "10m0s" {
		t.Errorf("Interval = %q, want %q", got.Interval, "10m0s")
	}

	// After a cycle: real counters and a real timestamp.
	loop.Start(t.Context())
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && loop.LastResult().At.IsZero() {
		time.Sleep(5 * time.Millisecond)
	}
	loop.Stop()

	got = a.obsidianStatus()
	if got.LastExportAt == nil || got.LastExportAt.IsZero() {
		t.Fatalf("LastExportAt = %v after a cycle, want a stamped time", got.LastExportAt)
	}
	if got.Created != 7 || got.Hubs != 2 {
		t.Errorf("counters = created %d hubs %d, want 7 / 2", got.Created, got.Hubs)
	}
	if got.Error != nil {
		t.Errorf("Error = %v after a successful cycle, want nil", *got.Error)
	}
}

// countingFakeExportable returns a fixed result and counts cycles.
type countingFakeExportable struct {
	mu     sync.Mutex
	n      int
	result *obsidian.ExportResult
}

func (f *countingFakeExportable) Export() (*obsidian.ExportResult, error) {
	f.mu.Lock()
	f.n++
	f.mu.Unlock()
	return f.result, nil
}

func (f *countingFakeExportable) SetGraphConfig(obsidian.GraphConfigMode) {}

// ── test helpers ─────────────────────────────────────────────────────────────

// captureStdLog redirects the standard logger (stderr by default) into w and
// returns a restore func. Idempotent: calling restore twice is safe.
func captureStdLog(w io.Writer) func() {
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(w)
	log.SetFlags(0)
	var once sync.Once
	return func() {
		once.Do(func() {
			log.SetOutput(origOut)
			log.SetFlags(origFlags)
		})
	}
}

// lockedBuffer is a bytes.Buffer safe for the concurrent read this file needs
// (the log is written from the goroutine running Close while the test body
// reads it).
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureStdoutBytes swaps os.Stdout for a pipe around fn and returns every
// byte written to it. Nothing on the daemon export path may produce any.
//
// Deliberately NOT main_test.go's captureStdout: that one closes the writer and
// only THEN reads, which deadlocks once the OS pipe buffer fills. The failure
// this test exists to catch is a chatty export path, so the capture must not
// hang precisely when it finds the bug.
func captureStdoutBytes(t *testing.T, fn func()) []byte {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w

	done := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(r)
		done <- data
	}()

	fn()

	os.Stdout = orig
	_ = w.Close()
	data := <-done
	_ = r.Close()
	return data
}
