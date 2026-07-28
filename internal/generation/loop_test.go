package generation

// loop_test.go -- Phase 6 (internal/generation.Loop, tasks 6.1-6.24). No
// mocking framework exists in this repo (design constraint 6) -- every fake
// below is hand-rolled, mirroring obsidian/loop_test.go's countingExportable
// and embedding/loop_test.go's fakes.
//
// This entire phase makes ZERO network calls: every test here drives the
// Loop against fakeLoopProvider (a stdlib-only GenerationProvider double)
// and fakeLoopStore (a stdlib-only NarrativeStore double). Some tests wrap
// fakeLoopProvider in the REAL gate (NewGated) with fakeProjectPolicyChecker
// to exercise the loop's gated-unit handling against production gating
// logic, not a shortcut.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/localstore"
)

// --- shared test helpers ---------------------------------------------------

// waitFor polls cond every 5ms until it holds or the deadline passes. It
// does not fail the test itself -- callers assert the real condition after
// the wait returns (mirrors obsidian/loop_test.go's identical helper).
func waitFor(d time.Duration, cond func() bool) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// syncBuffer is a mutex-guarded io.Writer so a slog handler can be written
// to concurrently by the loop goroutine while the test goroutine reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// findLoopLogLine returns the first JSON log line whose "msg" field equals
// msg, failing the test if none is found.
func findLoopLogLine(t *testing.T, logOutput, msg string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logOutput), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m["msg"] == msg {
			return m
		}
	}
	t.Fatalf("no log line with msg %q found in: %s", msg, logOutput)
	return nil
}

// assertLoopLogField asserts line[key] == want.
func assertLoopLogField(t *testing.T, line map[string]any, key string, want any) {
	t.Helper()
	got, ok := line[key]
	if !ok {
		t.Errorf("log line missing field %q: %v", key, line)
		return
	}
	if got != want {
		t.Errorf("log field %q = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}

// eligibleUnitRecords returns n records under project, whose derived
// TOPIC PREFIX (AssembleUnits splits a record's TopicKey on the LAST "/")
// equals wantPrefix exactly, and whose combined Title+Content characters
// comfortably exceed defaultMinCombinedChars for any n >= 3 -- callers do
// not need to reason about the eligibility threshold. To make the derived
// prefix equal wantPrefix verbatim (rather than a truncated version of it),
// every record's TopicKey is wantPrefix PLUS a synthetic "/leaf" segment:
// topicPrefix("topic/only/leaf") == "topic/only", matching wantPrefix
// "topic/only" exactly, so callers can pass rowFor/seedRow the SAME string
// they passed here without re-deriving the split themselves. Reuses
// topicKeyPtr from unit_test.go (same package).
func eligibleUnitRecords(project, wantPrefix string, n int) []*domain.Record {
	fullTopicKey := wantPrefix + "/leaf"
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recs := make([]*domain.Record, 0, n)
	for i := 0; i < n; i++ {
		syncID := fmt.Sprintf("%s|%s|%03d", project, wantPrefix, i)
		recs = append(recs, &domain.Record{
			SyncID:    syncID,
			Project:   project,
			TopicKey:  topicKeyPtr(fullTopicKey),
			Title:     "Narrative source " + syncID,
			Content:   strings.Repeat("word ", 150) + syncID, // ~750+ chars/record
			WriterID:  "writer-1",
			UpdatedAt: base.Add(time.Duration(i) * time.Hour),
		})
	}
	return recs
}

// --- fakeLoopStore -----------------------------------------------------

// fakeLoopStore is a stdlib-only, hand-rolled NarrativeStore double. All
// fields are guarded by mu so it is safe for the Loop's background
// goroutine to call it while a test goroutine inspects it concurrently.
type fakeLoopStore struct {
	mu sync.Mutex

	projects         []string
	recordsByProject map[string][]*domain.Record
	listProjectsErr  error
	recentObsErr     map[string]error

	rows         map[string]localstore.NarrativeRow // keyed by UnitKey(project, prefix)
	cacheKeysErr error
	upsertErr    error
	markStaleErr error

	upsertCalls    []localstore.NarrativeRow
	markStaleCalls [][]string
}

func newFakeLoopStore() *fakeLoopStore {
	return &fakeLoopStore{
		recordsByProject: map[string][]*domain.Record{},
		rows:             map[string]localstore.NarrativeRow{},
		recentObsErr:     map[string]error{},
	}
}

// addRecords registers project (once) and appends recs to its record set.
func (f *fakeLoopStore) addRecords(project string, recs []*domain.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	for _, p := range f.projects {
		if p == project {
			found = true
			break
		}
	}
	if !found {
		f.projects = append(f.projects, project)
	}
	f.recordsByProject[project] = append(f.recordsByProject[project], recs...)
}

// seedRow pre-populates a stored narrative row, as if a prior cycle had
// already generated it.
func (f *fakeLoopStore) seedRow(row localstore.NarrativeRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[UnitKey(row.Project, row.TopicPrefix)] = row
}

func (f *fakeLoopStore) ListProjects() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listProjectsErr != nil {
		return nil, f.listProjectsErr
	}
	out := make([]string, len(f.projects))
	copy(out, f.projects)
	return out, nil
}

func (f *fakeLoopStore) RecentObservations(project, _ string, _ int) ([]*domain.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.recentObsErr[project]; err != nil {
		return nil, err
	}
	recs := f.recordsByProject[project]
	out := make([]*domain.Record, len(recs))
	copy(out, recs)
	return out, nil
}

func (f *fakeLoopStore) NarrativeCacheKeys() (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cacheKeysErr != nil {
		return nil, f.cacheKeysErr
	}
	out := make(map[string]string, len(f.rows))
	for k, row := range f.rows {
		out[k] = row.SourceHash
	}
	return out, nil
}

func (f *fakeLoopStore) UpsertNarrative(row localstore.NarrativeRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.rows[UnitKey(row.Project, row.TopicPrefix)] = row
	f.upsertCalls = append(f.upsertCalls, row)
	return nil
}

func (f *fakeLoopStore) MarkNarrativesStale(keys []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markStaleErr != nil {
		return f.markStaleErr
	}
	cp := make([]string, len(keys))
	copy(cp, keys)
	f.markStaleCalls = append(f.markStaleCalls, cp)
	for _, k := range keys {
		if row, ok := f.rows[k]; ok {
			row.Stale = true
			f.rows[k] = row
		}
	}
	return nil
}

func (f *fakeLoopStore) upsertCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.upsertCalls)
}

func (f *fakeLoopStore) rowFor(project, prefix string) (localstore.NarrativeRow, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[UnitKey(project, prefix)]
	return row, ok
}

func (f *fakeLoopStore) markStaleCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.markStaleCalls)
}

// ensure fakeLoopStore satisfies NarrativeStore at compile time.
var _ NarrativeStore = (*fakeLoopStore)(nil)

// --- fakeLoopProvider --------------------------------------------------

type loopProviderCall struct {
	project string
	prompt  string
}

// fakeLoopProvider is a stdlib-only GenerationProvider double, richer than
// gated_test.go's fakeGenerationProvider: it records every call (project +
// prompt), can script a per-call error via errFn, and can BLOCK every call
// until released (blockUntil) to exercise Stop()'s drain guarantee.
type fakeLoopProvider struct {
	mu    sync.Mutex
	model string
	body  string
	calls []loopProviderCall

	// errFn, when non-nil, is consulted for every call (1-based call
	// index across this fake's whole lifetime, the project name), letting
	// a test script per-call outcomes.
	errFn func(callNum int, project string) error

	// blockUntil, when non-nil, makes every call BLOCK until it is
	// closed -- simulating an HTTP call that ignores ctx cancellation
	// entirely (a real network call only unblocks via its own timeout,
	// never immediate ctx cancellation), so Loop.Stop()'s drain guarantee
	// can be observed directly.
	blockUntil chan struct{}
	entered    chan struct{}
	enterOnce  sync.Once
}

func (f *fakeLoopProvider) Generate(_ context.Context, project, prompt string) (string, error) {
	if f.blockUntil != nil {
		f.enterOnce.Do(func() {
			if f.entered != nil {
				close(f.entered)
			}
		})
		<-f.blockUntil
	}

	f.mu.Lock()
	f.calls = append(f.calls, loopProviderCall{project: project, prompt: prompt})
	callNum := len(f.calls)
	f.mu.Unlock()

	if f.errFn != nil {
		if err := f.errFn(callNum, project); err != nil {
			return "", err
		}
	}

	body := f.body
	if body == "" {
		body = "generated narrative body"
	}
	return body, nil
}

func (f *fakeLoopProvider) ModelName() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.model == "" {
		return "fake-loop-model"
	}
	return f.model
}

func (f *fakeLoopProvider) setModel(m string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.model = m
}

func (f *fakeLoopProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeLoopProvider) callList() []loopProviderCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]loopProviderCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// ensure fakeLoopProvider satisfies GenerationProvider at compile time.
var _ GenerationProvider = (*fakeLoopProvider)(nil)

// --- fakeProjectPolicyChecker --------------------------------------------

// fakeProjectPolicyChecker is a PolicyChecker double whose policy varies
// PER PROJECT (unlike gated_test.go's fakePolicyChecker, which is fixed for
// every project) -- needed to test a cycle that mixes a gated project with
// an eligible one. Projects absent from policies default to PolicySynced.
type fakeProjectPolicyChecker struct {
	mu       sync.Mutex
	policies map[string]localstore.Policy
}

func (f *fakeProjectPolicyChecker) GetPolicy(project string) (localstore.Policy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pol, ok := f.policies[project]; ok {
		return pol, nil
	}
	return localstore.PolicySynced, nil
}

// ensure fakeProjectPolicyChecker satisfies PolicyChecker at compile time.
var _ PolicyChecker = (*fakeProjectPolicyChecker)(nil)

// ===========================================================================
// 6.1 -- first cycle runs immediately; zero-value defaults
// ===========================================================================

// TestLoopRunsFirstCycleImmediately mirrors obsidian's identical test: the
// interval (1 hour) makes "immediately" unambiguous -- if the loop waited
// for the interval, the provider would still show zero calls well within
// this test's budget. TIMING (necessarily -- proving "no interval elapsed"
// requires observing real wall-clock scheduling), but the multi-order-of-
// magnitude gap between the budget and the interval keeps flake risk near
// zero.
func TestLoopRunsFirstCycleImmediately(t *testing.T) {
	store := newFakeLoopStore()
	store.addRecords("proj-a", eligibleUnitRecords("proj-a", "topic/only", 3))
	provider := &fakeLoopProvider{}

	loop := NewLoop(store, provider, LoopConfig{Interval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop.Start(ctx)
	defer loop.Stop()

	waitFor(2*time.Second, func() bool { return provider.callCount() >= 1 })

	if got := provider.callCount(); got < 1 {
		t.Fatalf("provider.callCount() = %d after 2s with a 1h interval, want >= 1 (first cycle must run immediately)", got)
	}
}

// TestNewLoopAppliesZeroValueDefaults pins every documented zero-value
// default in one place.
func TestNewLoopAppliesZeroValueDefaults(t *testing.T) {
	got := applyLoopDefaults(LoopConfig{})

	if got.Interval != defaultLoopInterval {
		t.Errorf("Interval = %v, want %v", got.Interval, defaultLoopInterval)
	}
	if got.MaxUnitsPerCycle != defaultMaxUnitsPerCycle {
		t.Errorf("MaxUnitsPerCycle = %d, want %d", got.MaxUnitsPerCycle, defaultMaxUnitsPerCycle)
	}
	if got.MaxInputCharsPerCycle != defaultMaxInputCharsPerCycle {
		t.Errorf("MaxInputCharsPerCycle = %d, want %d", got.MaxInputCharsPerCycle, defaultMaxInputCharsPerCycle)
	}
	if got.MinObservations != defaultMinObservations {
		t.Errorf("MinObservations = %d, want %d", got.MinObservations, defaultMinObservations)
	}
	if got.MinCombinedChars != defaultMinCombinedChars {
		t.Errorf("MinCombinedChars = %d, want %d", got.MinCombinedChars, defaultMinCombinedChars)
	}
	if got.Logger == nil {
		t.Error("Logger is nil, want slog.Default()")
	}
}

// ===========================================================================
// 6.3 -- sub-floor interval clamping
// ===========================================================================

// TestLoopSubFloorIntervalIsClampedWithWarning tables three sub-floor
// inputs of different signs -- all clamp to the 5-minute floor with exactly
// one warning. DEVIATION FROM THE TASKS DOCUMENT'S LITERAL TABLE, disclosed
// here: the tasks artifact's table names "1s, 0, -1s, math.MinInt64" as ALL
// clamping with a warning. That directly contradicts task 6.1 ("Interval 0
// -> 30m") and design #4754 §5's own LoopConfig comment, which states TWO
// DISTINCT branches: "0 -> 30m; < floor -> clamped to 5m + warn" -- i.e.
// zero is the silent "unset, use the default cadence" case, and only a
// NON-ZERO sub-floor value (of either sign) is a clamp-with-warning
// misconfiguration. This table therefore covers 1s, -1s and
// math.MinInt64 only; TestNewLoopAppliesZeroValueDefaults already pins 0's
// behaviour. This exactly mirrors obsidian.Loop's own applyLoopDefaults
// switch (case Interval == 0 first, case Interval < floor second) --
// Phase A's own precedent, not a deviation from it.
func TestLoopSubFloorIntervalIsClampedWithWarning(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
	}{
		{"positive sub-floor (1s)", 1 * time.Second},
		{"negative (-1s)", -1 * time.Second},
		{"math.MinInt64", time.Duration(math.MinInt64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := &syncBuffer{}
			logger := slog.New(slog.NewJSONHandler(sb, nil))

			got := applyLoopDefaults(LoopConfig{Interval: tt.in, Logger: logger})

			if got.Interval != loopIntervalFloor {
				t.Errorf("Interval = %v, want %v (the 5-minute floor) for input %v", got.Interval, loopIntervalFloor, tt.in)
			}
			if got.Interval <= 0 {
				t.Fatalf("Interval = %v is still non-positive: a timer built from this would still spin", got.Interval)
			}
			if !strings.Contains(sb.String(), "interval below the 5-minute floor") {
				t.Errorf("expected a clamp warning to be logged for %v, got: %s", tt.in, sb.String())
			}
		})
	}

	t.Run("floorOverride honours the raw value with no warning", func(t *testing.T) {
		sb := &syncBuffer{}
		logger := slog.New(slog.NewJSONHandler(sb, nil))

		got := applyLoopDefaults(LoopConfig{Interval: 10 * time.Millisecond, floorOverride: time.Millisecond, Logger: logger})

		if got.Interval != 10*time.Millisecond {
			t.Errorf("Interval = %v, want the raw 10ms honoured via floorOverride", got.Interval)
		}
		if strings.Contains(sb.String(), "clamping") {
			t.Errorf("floorOverride must suppress the clamp warning entirely, got: %s", sb.String())
		}
	})
}

// ===========================================================================
// 6.5 -- unchanged corpus issues zero provider calls; model change invalidates
// ===========================================================================

func TestUnchangedCorpusIssuesZeroProviderCalls(t *testing.T) {
	store := newFakeLoopStore()
	// Two genuinely distinct topic-prefix units under the SAME project.
	store.addRecords("proj-a", eligibleUnitRecords("proj-a", "topic/one", 3))
	store.addRecords("proj-a", eligibleUnitRecords("proj-a", "topic/two", 3))
	provider := &fakeLoopProvider{}

	loop := NewLoop(store, provider, LoopConfig{})

	if err := loop.runCycle(context.Background()); err != nil {
		t.Fatalf("cycle 1: runCycle() = %v, want nil", err)
	}
	if got := provider.callCount(); got != 2 {
		t.Fatalf("cycle 1: provider.callCount() = %d, want 2 (two distinct topic-prefix units)", got)
	}
	if got := store.upsertCount(); got != 2 {
		t.Fatalf("cycle 1: store.upsertCount() = %d, want 2", got)
	}

	if err := loop.runCycle(context.Background()); err != nil {
		t.Fatalf("cycle 2: runCycle() = %v, want nil", err)
	}
	if got := provider.callCount(); got != 2 {
		t.Errorf("cycle 2: provider.callCount() = %d, want STILL 2 (zero new calls over an unchanged corpus)", got)
	}
	if got := store.upsertCount(); got != 2 {
		t.Errorf("cycle 2: store.upsertCount() = %d, want STILL 2 (zero new writes)", got)
	}
}

func TestModelNameChangeInvalidatesEveryCacheEntry(t *testing.T) {
	store := newFakeLoopStore()
	store.addRecords("proj-a", eligibleUnitRecords("proj-a", "topic/one", 3))
	provider := &fakeLoopProvider{model: "model-a"}

	loop := NewLoop(store, provider, LoopConfig{})
	if err := loop.runCycle(context.Background()); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if got := provider.callCount(); got != 1 {
		t.Fatalf("cycle 1: provider.callCount() = %d, want 1", got)
	}

	provider.setModel("model-b")
	if err := loop.runCycle(context.Background()); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if got := provider.callCount(); got != 2 {
		t.Errorf("cycle 2 (after a model change): provider.callCount() = %d, want 2 (the unit must miss again)", got)
	}
}

// ===========================================================================
// 6.7 -- budget deferral never errors; char budget can bind first; budget
// checked before prompt build/HTTP
// ===========================================================================

func TestBudgetDefersNeverErrors(t *testing.T) {
	store := newFakeLoopStore()
	const totalUnits = 100
	for i := 0; i < totalUnits; i++ {
		project := fmt.Sprintf("proj-%03d", i)
		store.addRecords(project, eligibleUnitRecords(project, "topic/only", 3))
	}
	provider := &fakeLoopProvider{}
	sb := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(sb, nil))

	loop := NewLoop(store, provider, LoopConfig{MaxUnitsPerCycle: 3, Logger: logger})

	if err := loop.runCycle(context.Background()); err != nil {
		t.Fatalf("runCycle() = %v, want nil (a budget ceiling must never be an error)", err)
	}
	if got := provider.callCount(); got != 3 {
		t.Fatalf("provider.callCount() = %d, want exactly 3", got)
	}

	line := findLoopLogLine(t, sb.String(), "narrative loop cycle")
	assertLoopLogField(t, line, "generated", float64(3))
	assertLoopLogField(t, line, "deferred", float64(97))

	if strings.Contains(sb.String(), `"level":"WARN"`) || strings.Contains(sb.String(), `"level":"ERROR"`) {
		t.Errorf("a budget deferral must log ZERO warning/error records about the deferral itself, got: %s", sb.String())
	}
}

func TestCharBudgetBindsBeforeUnitBudget(t *testing.T) {
	store := newFakeLoopStore()
	recsPerUnit := eligibleUnitRecords("proj-a", "topic/a", 3)
	unitChars := 0
	for _, r := range recsPerUnit {
		unitChars += len(r.Title) + len(r.Content)
	}
	store.addRecords("proj-a", recsPerUnit)
	store.addRecords("proj-b", eligibleUnitRecords("proj-b", "topic/a", 3))
	store.addRecords("proj-c", eligibleUnitRecords("proj-c", "topic/a", 3))
	provider := &fakeLoopProvider{}

	// Unit budget is generous (25); the char budget is sized for exactly
	// 2 units, so it must be the one that binds.
	loop := NewLoop(store, provider, LoopConfig{MaxUnitsPerCycle: 25, MaxInputCharsPerCycle: unitChars*2 + 10})

	if err := loop.runCycle(context.Background()); err != nil {
		t.Fatalf("runCycle() = %v, want nil", err)
	}
	if got := provider.callCount(); got != 2 {
		t.Errorf("provider.callCount() = %d, want 2 (the char budget must bind before the 25-unit budget)", got)
	}
}

func TestBudgetCheckedBeforePromptBuildAndBeforeHTTP(t *testing.T) {
	store := newFakeLoopStore()
	for i := 0; i < 5; i++ {
		project := fmt.Sprintf("proj-%d", i)
		store.addRecords(project, eligibleUnitRecords(project, "topic/only", 3))
	}
	provider := &fakeLoopProvider{}
	loop := NewLoop(store, provider, LoopConfig{MaxUnitsPerCycle: 2})

	if err := loop.runCycle(context.Background()); err != nil {
		t.Fatalf("runCycle() = %v, want nil", err)
	}

	// promptsBuilt: the fake only ever receives a call AFTER BuildPrompt
	// ran (runCycle builds the prompt immediately before calling
	// Generate) -- so the fake's call count IS the prompt-build count.
	promptsBuilt := provider.callCount()
	if promptsBuilt != 2 {
		t.Fatalf("promptsBuilt (== provider.callCount()) = %d, want exactly 2 (== generated), never 5 (== len(units))", promptsBuilt)
	}
	for _, c := range provider.callList() {
		if c.prompt == "" {
			t.Errorf("a call reached the provider with an empty prompt for project %q -- BuildPrompt must run before every call actually made", c.project)
		}
	}
}

// ===========================================================================
// 6.9 -- no starvation across cycles (drives runCycle DIRECTLY, deterministic)
// ===========================================================================

func TestNoStarvationAcrossCycles(t *testing.T) {
	store := newFakeLoopStore()
	const totalUnits = 12
	for i := 0; i < totalUnits; i++ {
		project := fmt.Sprintf("proj-%02d", i)
		store.addRecords(project, eligibleUnitRecords(project, "topic/only", 3))
	}
	provider := &fakeLoopProvider{}
	loop := NewLoop(store, provider, LoopConfig{MaxUnitsPerCycle: 3})

	for cycle := 1; cycle <= 5; cycle++ {
		if err := loop.runCycle(context.Background()); err != nil {
			t.Fatalf("cycle %d: runCycle() = %v, want nil", cycle, err)
		}
		if cycle == 4 {
			if got := store.upsertCount(); got != totalUnits {
				t.Errorf("after cycle 4: store.upsertCount() = %d, want %d (all units generated exactly once)", got, totalUnits)
			}
		}
	}

	if got := store.upsertCount(); got != totalUnits {
		t.Fatalf("after 5 cycles: store.upsertCount() = %d, want exactly %d (no duplicates, no starvation)", got, totalUnits)
	}
	if got := provider.callCount(); got != totalUnits {
		t.Errorf("provider.callCount() = %d, want exactly %d (every unit generated exactly once)", got, totalUnits)
	}
}

// ===========================================================================
// 6.11 -- a gated unit consumes no budget; its assembled text is discarded
// ===========================================================================

func TestGatedUnitConsumesNoBudget(t *testing.T) {
	store := newFakeLoopStore()
	store.addRecords("gated-proj", eligibleUnitRecords("gated-proj", "topic/only", 3))
	store.addRecords("synced-proj", eligibleUnitRecords("synced-proj", "topic/only", 3))

	raw := &fakeLoopProvider{}
	checker := &fakeProjectPolicyChecker{policies: map[string]localstore.Policy{"gated-proj": localstore.PolicyLocalOnly}}
	gated := NewGated(raw, checker)

	// "gated-proj" sorts before "synced-proj" byte-wise, so it is
	// attempted FIRST. A 1-unit budget proves it consumed none: the
	// synced unit must still generate in the SAME cycle.
	loop := NewLoop(store, gated, LoopConfig{MaxUnitsPerCycle: 1})

	if err := loop.runCycle(context.Background()); err != nil {
		t.Fatalf("runCycle() = %v, want nil", err)
	}

	if _, ok := store.rowFor("synced-proj", "topic/only"); !ok {
		t.Errorf("synced-proj/topic/only was not generated -- a gated unit ahead of it must not have consumed the 1-unit budget")
	}
	if _, ok := store.rowFor("gated-proj", "topic/only"); ok {
		t.Errorf("gated-proj/topic/only has a stored row -- a gated project must never be cached")
	}
	// raw.callCount() is legitimately 1 here: synced-proj IS eligible and
	// DOES reach the raw provider (that is the whole point of the "still
	// runs in the same cycle" assertion above). What must be 0 is the
	// GATED project's own presence among those calls.
	for _, c := range raw.callList() {
		if c.project == "gated-proj" {
			t.Errorf("raw (inner) provider was called for the gated project %q -- the gate must refuse before the raw provider is ever called for it", c.project)
		}
	}
}

func TestGatedProjectTextIsAssembledThenDiscarded(t *testing.T) {
	store := newFakeLoopStore()
	store.addRecords("gated-proj", eligibleUnitRecords("gated-proj", "topic/only", 3))

	raw := &fakeLoopProvider{}
	checker := &fakeProjectPolicyChecker{policies: map[string]localstore.Policy{"gated-proj": localstore.PolicyOmitted}}
	gated := NewGated(raw, checker)

	loop := NewLoop(store, gated, LoopConfig{})

	if err := loop.runCycle(context.Background()); err != nil {
		t.Fatalf("runCycle() = %v, want nil", err)
	}

	// The gate refuses INSIDE Generate, before ever calling the raw inner
	// provider -- so the prompt the loop assembled for this unit existed
	// only as a local argument to the (gated) Generate call, and never
	// reached the raw provider, let alone the network. The gate is what
	// prevented it leaving; assembling it in the first place is accepted
	// (design #4754 §2).
	if got := raw.callCount(); got != 0 {
		t.Errorf("raw provider callCount() = %d, want 0 -- the gate must refuse before the raw provider is ever called", got)
	}
	if _, ok := store.rowFor("gated-proj", "topic/only"); ok {
		t.Errorf("a gated unit must never occupy a cache row")
	}
}

// ===========================================================================
// 6.13 -- circuit break; rejected abandons immediately; failed unit caches
// nothing
// ===========================================================================

func TestThreeConsecutiveProviderErrorsAbandonTheCycle(t *testing.T) {
	store := newFakeLoopStore()
	for i := 0; i < 10; i++ {
		project := fmt.Sprintf("proj-%02d", i)
		store.addRecords(project, eligibleUnitRecords(project, "topic/only", 3))
	}
	provider := &fakeLoopProvider{errFn: func(int, string) error {
		return errors.New("simulated transient failure")
	}}
	loop := NewLoop(store, provider, LoopConfig{})

	if err := loop.runCycle(context.Background()); err != nil {
		t.Fatalf("runCycle() = %v, want nil (a provider error must never fail the CYCLE)", err)
	}
	if got := provider.callCount(); got != narrativeCircuitBreakThreshold {
		t.Fatalf("provider.callCount() = %d, want exactly %d (the circuit break)", got, narrativeCircuitBreakThreshold)
	}
	if got := store.upsertCount(); got != 0 {
		t.Errorf("store.upsertCount() = %d, want 0 (every call failed)", got)
	}

	// The next tick starts fresh: the counter resets, so a second cycle
	// against the SAME always-failing provider stops at exactly 3 MORE
	// calls, not "3 total ever".
	if err := loop.runCycle(context.Background()); err != nil {
		t.Fatalf("cycle 2: runCycle() = %v, want nil", err)
	}
	if got := provider.callCount(); got != 2*narrativeCircuitBreakThreshold {
		t.Errorf("provider.callCount() after cycle 2 = %d, want %d (the breaker resets every cycle)", got, 2*narrativeCircuitBreakThreshold)
	}
}

func TestRejectedErrorAbandonsCycleImmediately(t *testing.T) {
	store := newFakeLoopStore()
	for i := 0; i < 5; i++ {
		project := fmt.Sprintf("proj-%02d", i)
		store.addRecords(project, eligibleUnitRecords(project, "topic/only", 3))
	}
	provider := &fakeLoopProvider{errFn: func(int, string) error {
		return errGenerationRejected
	}}
	loop := NewLoop(store, provider, LoopConfig{})

	if err := loop.runCycle(context.Background()); err != nil {
		t.Fatalf("runCycle() = %v, want nil", err)
	}
	if got := provider.callCount(); got != 1 {
		t.Errorf("provider.callCount() = %d, want exactly 1 (a rejected request abandons the cycle immediately, no retry)", got)
	}
}

func TestFailedUnitCachesNothing(t *testing.T) {
	store := newFakeLoopStore()
	store.addRecords("proj-a", eligibleUnitRecords("proj-a", "topic/only", 3))
	provider := &fakeLoopProvider{errFn: func(int, string) error {
		return errGenerationEmptyOutput
	}}
	loop := NewLoop(store, provider, LoopConfig{})

	if err := loop.runCycle(context.Background()); err != nil {
		t.Fatalf("runCycle() = %v, want nil", err)
	}
	if got := store.upsertCount(); got != 0 {
		t.Errorf("store.upsertCount() = %d, want 0 -- a failed unit must never be cached (would poison the cache with a permanent hit)", got)
	}
}

// ===========================================================================
// 6.15 -- deferred-over-budget marks an existing row stale; sticky until
// regeneration; a cold run marks nothing
// ===========================================================================

func TestDeferredMissMarksExistingRowStale(t *testing.T) {
	store := newFakeLoopStore()
	store.addRecords("aaa-fresh", eligibleUnitRecords("aaa-fresh", "topic/only", 3))
	store.addRecords("zzz-existing", eligibleUnitRecords("zzz-existing", "topic/only", 3))
	store.seedRow(localstore.NarrativeRow{
		Project:     "zzz-existing",
		TopicPrefix: "topic/only",
		Body:        "stale body from a previous cycle",
		SourceHash:  "stale-hash-that-will-never-match",
		Model:       "fake-loop-model",
		GeneratedAt: time.Now().UTC(),
	})

	provider := &fakeLoopProvider{}
	// "aaa-fresh" sorts before "zzz-existing" byte-wise, so it consumes
	// the single unit of budget, leaving zzz-existing deferred.
	loop := NewLoop(store, provider, LoopConfig{MaxUnitsPerCycle: 1})

	if err := loop.runCycle(context.Background()); err != nil {
		t.Fatalf("runCycle() = %v, want nil", err)
	}

	if got := provider.callCount(); got != 1 {
		t.Fatalf("provider.callCount() = %d, want exactly 1 (only the fresh unit fits the 1-unit budget)", got)
	}
	if got := store.markStaleCallCount(); got != 1 {
		t.Fatalf("store.markStaleCallCount() = %d, want exactly 1 (ONE batched call)", got)
	}
	row, ok := store.rowFor("zzz-existing", "topic/only")
	if !ok || !row.Stale {
		t.Errorf("zzz-existing/topic/only row Stale = %v (ok=%v), want true", row.Stale, ok)
	}
}

// TestStaleIsStickyUntilRegeneration: cycle 1 defers an over-budget unit
// with an existing (now-mismatched) row and marks it stale; cycle 2 raises
// the budget so the SAME unit actually regenerates, which is the only
// thing that clears the flag. Across both cycles, MarkNarrativesStale is
// called exactly once (cycle 1) -- cycle 2 generates the unit rather than
// deferring it, so there is nothing to re-mark.
func TestStaleIsStickyUntilRegeneration(t *testing.T) {
	store := newFakeLoopStore()
	store.addRecords("aaa-decoy", eligibleUnitRecords("aaa-decoy", "topic/only", 3))
	store.addRecords("proj-a", eligibleUnitRecords("proj-a", "topic/only", 3))
	store.seedRow(localstore.NarrativeRow{
		Project:     "proj-a",
		TopicPrefix: "topic/only",
		Body:        "stale body",
		SourceHash:  "stale-hash-that-will-never-match",
		Model:       "fake-loop-model",
		GeneratedAt: time.Now().UTC(),
	})

	provider := &fakeLoopProvider{}

	loop1 := NewLoop(store, provider, LoopConfig{MaxUnitsPerCycle: 1})
	if err := loop1.runCycle(context.Background()); err != nil {
		t.Fatalf("cycle 1: runCycle() = %v, want nil", err)
	}
	row, ok := store.rowFor("proj-a", "topic/only")
	if !ok || !row.Stale {
		t.Fatalf("cycle 1: proj-a row Stale = %v (ok=%v), want true", row.Stale, ok)
	}

	loop2 := NewLoop(store, provider, LoopConfig{MaxUnitsPerCycle: 25})
	if err := loop2.runCycle(context.Background()); err != nil {
		t.Fatalf("cycle 2: runCycle() = %v, want nil", err)
	}
	row, ok = store.rowFor("proj-a", "topic/only")
	if !ok || row.Stale {
		t.Errorf("cycle 2 (after regeneration): proj-a row Stale = %v (ok=%v), want false -- regeneration must clear the flag", row.Stale, ok)
	}
	if got := store.markStaleCallCount(); got != 1 {
		t.Errorf("store.markStaleCallCount() across both cycles = %d, want exactly 1 (marked once in cycle 1; cycle 2 generated it instead of deferring it)", got)
	}
}

func TestColdRunMarksNothingStale(t *testing.T) {
	store := newFakeLoopStore()
	store.addRecords("proj-a", eligibleUnitRecords("proj-a", "topic/only", 3))
	store.addRecords("proj-b", eligibleUnitRecords("proj-b", "topic/only", 3))
	provider := &fakeLoopProvider{}
	loop := NewLoop(store, provider, LoopConfig{MaxUnitsPerCycle: 1})

	if err := loop.runCycle(context.Background()); err != nil {
		t.Fatalf("runCycle() = %v, want nil", err)
	}
	if got := store.markStaleCallCount(); got != 0 {
		t.Errorf("store.markStaleCallCount() = %d, want 0 -- a cold run (no existing rows) must never call MarkNarrativesStale", got)
	}
}

// ===========================================================================
// 6.17 -- Stop() drains only the in-flight unit; mid-cycle cancellation
// returns within one unit
// ===========================================================================

// TestStopDrainsInFlightUnitOnly proves Stop() blocks while a single unit's
// Generate call is in flight, and returns promptly once that ONE call
// unblocks -- it does not wait for the whole remaining cycle (the opposite
// of obsidian.Loop's Stop(), deliberately -- see loop.go's package doc).
func TestStopDrainsInFlightUnitOnly(t *testing.T) {
	store := newFakeLoopStore()
	store.addRecords("proj-a", eligibleUnitRecords("proj-a", "topic/only", 3))
	provider := &fakeLoopProvider{
		blockUntil: make(chan struct{}),
		entered:    make(chan struct{}),
	}
	loop := NewLoop(store, provider, LoopConfig{Interval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop.Start(ctx)

	select {
	case <-provider.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Generate() was never entered")
	}

	stopDone := make(chan struct{})
	go func() {
		loop.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
		t.Fatal("Stop() returned while Generate() was still blocked -- the in-flight unit was not drained")
	case <-time.After(200 * time.Millisecond):
	}

	close(provider.blockUntil)

	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return after the blocked Generate() call was released")
	}
}

// TestMidCycleCancellationReturnsWithinOneUnit cancels the ctx from inside
// the FIRST call's error callback (so the cancellation is guaranteed
// visible to runCycle's very next ctx.Err() check, no race window) and
// asserts the cycle stops well short of all 50 misses.
func TestMidCycleCancellationReturnsWithinOneUnit(t *testing.T) {
	store := newFakeLoopStore()
	const totalUnits = 50
	for i := 0; i < totalUnits; i++ {
		project := fmt.Sprintf("proj-%02d", i)
		store.addRecords(project, eligibleUnitRecords(project, "topic/only", 3))
	}

	ctx, cancel := context.WithCancel(context.Background())
	provider := &fakeLoopProvider{errFn: func(callNum int, _ string) error {
		if callNum == 1 {
			cancel()
		}
		return nil
	}}

	loop := NewLoop(store, provider, LoopConfig{MaxUnitsPerCycle: totalUnits})

	if err := loop.runCycle(ctx); err != nil {
		t.Fatalf("runCycle() = %v, want nil", err)
	}
	if got := provider.callCount(); got > 2 {
		t.Errorf("provider.callCount() = %d, want <= 2 (cancellation after unit 1 must stop within one more unit, not run all %d)", got, totalUnits)
	}
}

// ===========================================================================
// 6.19 -- daemon stdout silence
// ===========================================================================

// TestNarrativeLoopWritesNothingToStdout captures os.Stdout ONCE, then
// drives three separate cycles that TOGETHER exercise every one of: a
// successful generation, a budget deferral, a gated skip, a provider
// failure, and the in-cycle circuit break -- then asserts zero bytes were
// captured across all of them. These properties are not all reachable
// within a single cycle by construction (the circuit break abandons the
// cycle before any subsequent budget-deferral branch could run), so this
// test spans multiple cycles/scenarios rather than forcing an artificial
// single-cycle scenario.
func TestNarrativeLoopWritesNothingToStdout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		os.Stdout = origStdout
		w.Close()
	}
	defer restore()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// Scenario 1: successful generation + budget deferral in the same cycle.
	store1 := newFakeLoopStore()
	for i := 0; i < 5; i++ {
		project := fmt.Sprintf("proj-%02d", i)
		store1.addRecords(project, eligibleUnitRecords(project, "topic/only", 3))
	}
	provider1 := &fakeLoopProvider{}
	loop1 := NewLoop(store1, provider1, LoopConfig{MaxUnitsPerCycle: 2, Logger: logger})
	if err := loop1.runCycle(context.Background()); err != nil {
		t.Fatalf("scenario 1: runCycle() = %v", err)
	}

	// Scenario 2: a gated skip alongside a successful generation.
	store2 := newFakeLoopStore()
	store2.addRecords("gated-proj", eligibleUnitRecords("gated-proj", "topic/only", 3))
	store2.addRecords("synced-proj", eligibleUnitRecords("synced-proj", "topic/only", 3))
	raw2 := &fakeLoopProvider{}
	checker2 := &fakeProjectPolicyChecker{policies: map[string]localstore.Policy{"gated-proj": localstore.PolicyOmitted}}
	loop2 := NewLoop(store2, NewGated(raw2, checker2), LoopConfig{Logger: logger})
	if err := loop2.runCycle(context.Background()); err != nil {
		t.Fatalf("scenario 2: runCycle() = %v", err)
	}

	// Scenario 3: provider failures tripping the in-cycle circuit break.
	store3 := newFakeLoopStore()
	for i := 0; i < 5; i++ {
		project := fmt.Sprintf("proj-%02d", i)
		store3.addRecords(project, eligibleUnitRecords(project, "topic/only", 3))
	}
	provider3 := &fakeLoopProvider{errFn: func(int, string) error { return errors.New("simulated failure") }}
	loop3 := NewLoop(store3, provider3, LoopConfig{Logger: logger})
	if err := loop3.runCycle(context.Background()); err != nil {
		t.Fatalf("scenario 3: runCycle() = %v", err)
	}

	restore()

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&buf, r)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out reading stdout pipe")
	}

	if buf.Len() != 0 {
		t.Errorf("narrative loop wrote %d bytes to stdout across all scenarios, want 0: %q", buf.Len(), buf.String())
	}
}

// TestNoFmtPrintOnTheGenerationLoopPath is a GUARD: it parses every
// non-test .go file in this package with go/parser and fails if any calls
// fmt.Print, fmt.Println or fmt.Printf (all of which write to os.Stdout by
// default) -- fmt.Fprintf/fmt.Sprintf are unaffected (prompt.go already
// uses fmt.Fprintf into a strings.Builder, never stdout).
func TestNoFmtPrintOnTheGenerationLoopPath(t *testing.T) {
	fset := token.NewFileSet()
	checked := 0
	for _, path := range goFilesInPackageDir(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != "fmt" {
				return true
			}
			switch sel.Sel.Name {
			case "Print", "Println", "Printf":
				t.Errorf("%s calls fmt.%s -- writes to stdout, which is the daemon's MCP JSON-RPC channel (REQ-NARR-10); use the *slog.Logger instead",
					filepath.Base(path), sel.Sel.Name)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no production files were checked -- the guard would pass vacuously")
	}
}

// ===========================================================================
// 6.21 -- LastResult()
// ===========================================================================

func TestLoopLastResult(t *testing.T) {
	store := newFakeLoopStore()
	provider := &fakeLoopProvider{}
	loop := NewLoop(store, provider, LoopConfig{})

	zero := loop.LastResult()
	if !zero.LastCycleAt.IsZero() {
		t.Fatalf("LastResult() before any cycle = %+v, want the zero value", zero)
	}

	store.addRecords("proj-a", eligibleUnitRecords("proj-a", "topic/only", 3))
	store.addRecords("proj-b", eligibleUnitRecords("proj-b", "topic/only", 3))
	if err := loop.runCycle(context.Background()); err != nil {
		t.Fatalf("runCycle() = %v, want nil", err)
	}

	success := loop.LastResult()
	if success.LastCycleAt.IsZero() {
		t.Fatal("LastResult().LastCycleAt is zero after a successful cycle")
	}
	if success.Generated != 2 {
		t.Errorf("Generated = %d, want 2", success.Generated)
	}
	if success.Err != "" {
		t.Errorf("Err = %q, want empty after a success", success.Err)
	}

	// Force a cycle-level infrastructure failure.
	store.mu.Lock()
	store.listProjectsErr = errors.New("simulated ListProjects failure")
	store.mu.Unlock()

	if err := loop.runCycle(context.Background()); err == nil {
		t.Fatal("runCycle() = nil, want an error for a ListProjects failure")
	}

	failed := loop.LastResult()
	if failed.Err == "" {
		t.Error("LastResult().Err is empty after a failed cycle, want the error message")
	}
	if failed.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", failed.ConsecutiveFailures)
	}
	// The previous SUCCESSFUL cycle's counters must NOT be zeroed by a
	// later cycle-level failure.
	if failed.Generated != success.Generated {
		t.Errorf("Generated = %d after a failed cycle, want it preserved at %d from the prior success", failed.Generated, success.Generated)
	}
}
