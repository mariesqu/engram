package generation

// loop.go -- Phase 6 (internal/generation.Loop, tasks 6.1-6.24). The
// narrative-generation loop: it lists eligible (project, topic_prefix)
// units, finds cache misses against the current model + content hash
// (REQ-NARR-01), enforces the per-cycle budget ceilings BEFORE building any
// prompt or calling the provider (REQ-GEN-06), classifies provider errors,
// marks over-budget units that already have a row as stale (REQ-NARR-06),
// and never writes to stdout (REQ-NARR-10).
//
// Shaped after this repo's other two daemon-hosted background workers,
// embedding.Loop and obsidian.Loop: NewLoop(store, provider, cfg),
// Start(ctx) launching exactly one goroutine, a blocking Stop(), a
// time.Timer select loop, zero-value LoopConfig defaulting, and a
// *slog.Logger. Two deliberate divergences from obsidian.Loop, the more
// recently built sibling:
//
//  1. Cancellation IS observed MID-cycle, the opposite of obsidian.Loop.
//     obsidian.Exporter.Export() takes no ctx because a mid-write abort
//     would corrupt the state file the vault viewer reads (REQ-WATCH-05).
//     This loop has no multi-file commit: each unit is one provider call
//     followed by one single-row UPSERT, and the store is fully consistent
//     between units. Generate therefore takes a ctx, and runCycle checks
//     ctx.Err() between units. Without this, Stop() would block for
//     "HTTP timeout x remaining units" -- up to ~50 minutes of daemon
//     shutdown at 120s x 25 units. Do not mechanically copy obsidian.Loop's
//     ctx-free shape here.
//
//  2. NO exponential backoff -- an IN-CYCLE circuit break instead. Unlike
//     obsidian.Loop this loop DOES call a remote provider, so a run of
//     consecutive provider failures needs a defence. But the base interval
//     is already 30m; stacking exponential backoff on top produces
//     multi-hour silences that read as breakage. Instead: 3 consecutive
//     provider errors within a cycle abandons the REST of that cycle (log,
//     return, wait for the next tick) -- see narrativeCircuitBreakThreshold.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/localstore"
)

// --- error taxonomy ----------------------------------------------------
//
// Declared here rather than waiting for internal/generation/mistral.go
// (Phase 10) to exist: the Loop's in-cycle circuit break needs to classify
// provider errors NOW, fully tested against a fake provider, with zero
// network code written. RemoteMistralProvider (a later phase) is expected
// to find these sentinels already declared and simply return them from its
// own HTTP response classification -- see design #4754 §7's error taxonomy
// table, reproduced here as the authority for what each sentinel means.
var (
	// errGenerationRateLimited marks a rate-limited provider response
	// (HTTP 429, in a later phase). Counts toward the in-cycle circuit
	// break; does not abandon the cycle on its own.
	errGenerationRateLimited = errors.New("generation: rate limited")

	// errGenerationUnavailable marks a provider outage (5xx, network
	// failure, or timeout, in a later phase). Counts toward the circuit
	// break.
	errGenerationUnavailable = errors.New("generation: provider unavailable")

	// errGenerationRejected marks a request the provider refused outright
	// (a 4xx response other than 429, in a later phase): the request shape
	// or auth is wrong, and retrying a malformed request repeatedly is
	// pure waste. Unlike the two sentinels above, this ABANDONS the cycle
	// immediately rather than counting toward the 3-error threshold.
	errGenerationRejected = errors.New("generation: request rejected")

	// errGenerationEmptyOutput marks a successful-looking response that
	// carried no usable content. A unit that fails with this error is
	// NEVER cached -- caching an empty body would poison the cache with a
	// permanent hit.
	errGenerationEmptyOutput = errors.New("generation: empty output")
)

// narrativeCircuitBreakThreshold is the number of consecutive provider
// errors within ONE cycle that abandons the rest of that cycle. This is
// deliberately NOT exponential backoff -- see the package doc comment.
const narrativeCircuitBreakThreshold = 3

// rendererVersion is a DESCRIPTIVE-ONLY column value (never part of
// SourceHash -- see cachekey.go's doc comment on why renderer_version is
// excluded from the hash). internal/obsidian's narrative renderer does not
// exist yet (Phase 8); this is the placeholder value every row in this
// phase is stamped with, and Phase 8 may introduce its own versioning
// scheme when the renderer is built.
const rendererVersion = "rv-1"

// narrativeRecordsFetchLimit is the explicit "uncapped" sentinel passed to
// NarrativeStore.RecentObservations, mirroring obsidian.Exporter's own
// documented convention (exporter.go: "RecentObservations... silently
// rewrites limit <= 0 to 20 -- passing the zero value straight through
// would silently truncate every project to 20 rows and look like data
// loss. math.MaxInt is the explicit uncapped sentinel").
const narrativeRecordsFetchLimit = math.MaxInt

// --- defaults ------------------------------------------------------------

// defaultLoopInterval is the zero-value default cadence (REQ-GEN-05:
// narrative_interval defaults to "higher than obsidian_interval's default,
// e.g. 30m").
const defaultLoopInterval = 30 * time.Minute

// loopIntervalFloor is the production minimum interval enforced by
// applyLoopDefaults. UNLIKE obsidian.Loop's 1-minute floor, this floor is
// 5 minutes (REQ-GEN-05 AMENDED, design #4754 §5): this loop makes PAID
// remote calls, so a much more conservative floor is a deliberate,
// documented divergence from Phase A's export loop -- not an inconsistency
// to reconcile.
const loopIntervalFloor = 5 * time.Minute

const (
	defaultMaxUnitsPerCycle      = 25
	defaultMaxInputCharsPerCycle = 600_000
	defaultMinObservations       = 3
	defaultMinCombinedChars      = 1500
)

// --- NarrativeStore --------------------------------------------------------

// NarrativeStore is the narrow read/write surface the Loop needs from
// localstore, satisfied by *localstore.Store in production. Declared here
// (rather than depending on *localstore.Store directly) so Loop tests can
// drive a hand-rolled fake -- no mocking framework exists in this repo
// (design constraint 6, carried forward from Phase A).
type NarrativeStore interface {
	// ListProjects enumerates every known project, mirroring
	// obsidian.StoreReader's identical method.
	ListProjects() ([]string, error)

	// RecentObservations returns the live records for one project. The
	// Loop always calls this with narrativeRecordsFetchLimit (uncapped) --
	// a partial per-project view would make AssembleUnits silently narrate
	// only a fraction of the corpus.
	RecentObservations(project, scope string, limit int) ([]*domain.Record, error)

	// NarrativeCacheKeys returns every stored row's folded key mapped to
	// its source_hash -- ONE full-table scan per cycle, backing the
	// cache-miss test (REQ-NARR-01).
	NarrativeCacheKeys() (map[string]string, error)

	// UpsertNarrative persists one generated (or regenerated) unit.
	UpsertNarrative(row localstore.NarrativeRow) error

	// MarkNarrativesStale batches the stale flag onto every row whose
	// folded key is deferred over budget while its stored source_hash no
	// longer matches (REQ-NARR-06). An empty slice issues no statement.
	MarkNarrativesStale(keys []string) error

	// MostRecentSessionDirectory returns the directory of the most
	// recently started session known for project, or "" if none is known.
	// This backs REQ-PROV-02's GENERATION-TIME path verification: runCycle
	// extracts path-like tokens from a unit's generated prose (paths.go's
	// ExtractPaths) and os.Stats each one relative to this directory
	// (paths.go's VerifyPaths) before persisting the row -- never at
	// render time, so the rendered bytes stay a pure function of the row
	// (design #4754 §6/§8). "" is a valid not-found result, not an error;
	// VerifyPaths already treats an empty session directory as "mark
	// every path unverified" rather than silently presenting them as
	// confirmed.
	MostRecentSessionDirectory(project string) (string, error)
}

// ensure *localstore.Store satisfies NarrativeStore at compile time.
var _ NarrativeStore = (*localstore.Store)(nil)

// --- LoopConfig ------------------------------------------------------------

// LoopConfig holds tunables for a Loop. Zero values are replaced by
// defaults via applyLoopDefaults.
type LoopConfig struct {
	// Interval is the cadence between narrative-generation cycles.
	//
	//   - 0          -> defaults to 30 minutes (defaultLoopInterval), no
	//     warning logged: this is simply "unset, use the default cadence".
	//   - non-zero and < floor -> CLAMPED to the floor (5 minutes in
	//     production), with a logged warning. This applies to EVERY
	//     non-zero value below the floor, POSITIVE OR NEGATIVE, with no
	//     sign-based exception -- obsidian.Loop's CRITICAL-1 (a negative
	//     Interval treated as a test-only sentinel that bypassed the floor
	//     entirely, causing time.Duration(math.MinInt64) to negate back to
	//     itself under two's-complement overflow and spin forever) is NOT
	//     re-earned here. Clamping only ever COMPARES against the floor
	//     and REPLACES with it -- it never negates or inspects the sign.
	Interval time.Duration

	// MaxUnitsPerCycle caps the number of units generated in one cycle
	// (REQ-GEN-06). 0 -> 25 (defaultMaxUnitsPerCycle).
	MaxUnitsPerCycle int

	// MaxInputCharsPerCycle caps the cumulative input characters sent to
	// the provider in one cycle (REQ-GEN-06). 0 -> 600000
	// (defaultMaxInputCharsPerCycle). Whichever of the two ceilings binds
	// first stops the cycle from issuing further requests.
	MaxInputCharsPerCycle int

	// MinObservations is the eligibility threshold's observation-count
	// half (REQ-NARR-04). 0 -> 3 (defaultMinObservations).
	MinObservations int

	// MinCombinedChars is the eligibility threshold's character-count half
	// (REQ-NARR-04). 0 -> 1500 (defaultMinCombinedChars).
	MinCombinedChars int

	// Logger receives one structured record per cycle plus warnings.
	// Default: slog.Default(). The loop NEVER writes to stdout
	// (REQ-NARR-10): in the daemon's default mode, stdout is the MCP
	// JSON-RPC channel, and a stray print here would corrupt every
	// connected client's framing.
	Logger *slog.Logger

	// floorOverride, when greater than zero, replaces loopIntervalFloor
	// for this Loop's defaulting. UNEXPORTED, so only this package's own
	// tests can set it -- the same mechanism obsidian.LoopConfig uses to
	// make the unsafe fast-cadence state unrepresentable from production
	// configuration. A caller in another package (a later phase's
	// daemon/config wiring) can only ever set the exported Interval field,
	// and every Interval value of any sign is subject to loopIntervalFloor
	// with no bypass available to it.
	floorOverride time.Duration
}

// applyLoopDefaults fills in zero (unset) LoopConfig fields with their
// defaults and applies the interval floor described on LoopConfig.Interval.
func applyLoopDefaults(c LoopConfig) LoopConfig {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.MaxUnitsPerCycle <= 0 {
		c.MaxUnitsPerCycle = defaultMaxUnitsPerCycle
	}
	if c.MaxInputCharsPerCycle <= 0 {
		c.MaxInputCharsPerCycle = defaultMaxInputCharsPerCycle
	}
	if c.MinObservations <= 0 {
		c.MinObservations = defaultMinObservations
	}
	if c.MinCombinedChars <= 0 {
		c.MinCombinedChars = defaultMinCombinedChars
	}

	floor := loopIntervalFloor
	if c.floorOverride > 0 {
		floor = c.floorOverride
	}

	switch {
	case c.Interval == 0:
		c.Interval = defaultLoopInterval
	case c.Interval < floor:
		// Covers every non-zero value below the floor, POSITIVE OR
		// NEGATIVE (including time.Duration(math.MinInt64)) -- no
		// sign-based fast path, no arithmetic negation, so no overflow is
		// possible here (obsidian.Loop's CRITICAL-1 is not re-earned).
		c.Logger.Warn("generation.Loop: interval below the 5-minute floor, clamping",
			"requested", c.Interval,
			"clamped_to", floor,
		)
		c.Interval = floor
	}
	return c
}

// --- CycleOutcome ------------------------------------------------------

// CycleOutcome is the result of the most recent completed narrative cycle,
// surfaced via Loop.LastResult for the daemon status endpoint (a later
// phase). The zero value (LastCycleAt.IsZero()) means no cycle has run yet.
//
// A FAILED cycle (a cycle-level infrastructure error -- ListProjects or
// NarrativeCacheKeys failing) updates LastCycleAt, Err and
// ConsecutiveFailures but deliberately leaves
// Generated/Cached/Deferred/Gated/Failed untouched, mirroring
// obsidian.ExportOutcome's identical rationale: overwriting a healthy prior
// cycle's counts with zeros on every transient failure would make them look
// lost the moment one blip occurs.
//
// A SUCCESSFUL cycle (including one that hit a budget ceiling, gated some
// units, or tripped the in-cycle circuit break -- none of those are
// treated as cycle failures, REQ-GEN-06) resets Err to "" and
// ConsecutiveFailures to 0 and replaces every counter with the fresh
// result.
type CycleOutcome struct {
	LastCycleAt time.Time
	Generated   int
	Cached      int
	Deferred    int
	Gated       int
	Failed      int
	Err         string

	ConsecutiveFailures int
}

// --- Loop ------------------------------------------------------------------

// Loop runs narrative-generation cycles in the background on a fixed
// schedule. Construct with NewLoop; start with Start(ctx); stop with
// Stop().
type Loop struct {
	store    NarrativeStore
	provider GenerationProvider
	cfg      LoopConfig

	// mu serializes Start/Stop lifecycle state, mirroring both sibling
	// loops.
	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc

	// done is closed by run() just before it exits, letting Stop() block
	// until the goroutine -- and therefore any in-flight cycle -- has
	// fully finished.
	done chan struct{}

	// resultMu guards lastResult and consecutiveFailures, SEPARATE from mu
	// (the lifecycle mutex), mirroring both sibling loops' resultMu split.
	// This is the split most likely to surface something on CI's ubuntu
	// -race leg (RACE-FIRST-ON-CI -- go test -race cannot run on this
	// Windows dev machine, no CGO).
	resultMu            sync.Mutex
	lastResult          CycleOutcome
	consecutiveFailures int
}

// NewLoop constructs a narrative-generation Loop. cfg zero values are
// replaced by defaults via applyLoopDefaults.
//
// provider MUST be the output of NewGated -- never a raw provider --
// mirroring embedding.NewLoop's identical contract (there is no way to
// enforce this at the type level without exposing gatedProvider, which
// would defeat the entire point of the gate; TestNoCallSiteConstructsARawProviderOutsideTheGate
// is the mechanical guard for this, gated_test.go).
func NewLoop(store NarrativeStore, provider GenerationProvider, cfg LoopConfig) *Loop {
	return &Loop{
		store:    store,
		provider: provider,
		cfg:      applyLoopDefaults(cfg),
		done:     make(chan struct{}),
	}
}

// Start launches the background narrative-generation goroutine and returns
// immediately. The first cycle runs immediately, before any interval
// elapses. Calling Start more than once panics, mirroring both sibling
// loops.
func (l *Loop) Start(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started {
		panic("generation.Loop: Start called more than once")
	}
	l.started = true
	runCtx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	go l.run(runCtx)
}

// Stop signals the goroutine to exit and BLOCKS until it does. UNLIKE
// obsidian.Loop.Stop, this does NOT wait for an entire in-flight cycle to
// finish -- the cancelled ctx is threaded into every in-flight Generate
// call and checked between units (see the package doc comment), so Stop()
// only ever blocks for the current single unit's provider call, never for
// the whole remaining cycle.
//
// Idempotent and nil-safe, mirroring both sibling loops.
func (l *Loop) Stop() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if !l.started {
		l.mu.Unlock()
		return
	}
	if !l.stopped {
		l.stopped = true
		l.cancel()
	}
	l.mu.Unlock()

	<-l.done
}

// LastResult returns the outcome of the most recent completed cycle. The
// zero value (LastCycleAt.IsZero()) means no cycle has run yet. Safe for
// concurrent use.
func (l *Loop) LastResult() CycleOutcome {
	l.resultMu.Lock()
	defer l.resultMu.Unlock()
	return l.lastResult
}

// run is the goroutine body. It runs one cycle immediately, then waits on
// either ctx cancellation or the next tick.
func (l *Loop) run(ctx context.Context) {
	defer close(l.done)

	select {
	case <-ctx.Done():
		return
	default:
	}

	_ = l.runCycle(ctx)

	timer := time.NewTimer(l.cfg.Interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			_ = l.runCycle(ctx)
			timer.Reset(l.cfg.Interval)
		}
	}
}

// missUnit pairs a cache-miss Unit with its identity key and freshly
// computed content hash, so both are computed exactly once per unit per
// cycle.
type missUnit struct {
	unit Unit
	key  string
	hash string
}

// runCycle executes exactly one narrative-generation cycle and records its
// outcome (both LastResult() and a structured log line) before returning.
// It is UNEXPORTED but deliberately callable directly by this package's own
// tests: the starvation proof (task 6.9) drives it repeatedly without
// depending on wall-clock ticking, which is what keeps that test off the
// timing-dependent list.
//
// runCycle returns a non-nil error ONLY for a cycle-level infrastructure
// failure (ListProjects or NarrativeCacheKeys). Every "soft" outcome --
// hitting a budget ceiling, the in-cycle circuit break tripping, a
// rejected request abandoning the cycle -- is recorded in the resulting
// CycleOutcome's counters and the cycle log line, and is NEVER surfaced as
// an error (REQ-GEN-06: hitting a ceiling MUST NOT be treated as an
// error).
func (l *Loop) runCycle(ctx context.Context) error {
	start := time.Now()

	projects, err := l.store.ListProjects()
	if err != nil {
		wrapped := fmt.Errorf("generation loop: ListProjects: %w", err)
		l.recordFailure(wrapped)
		l.cfg.Logger.Warn("narrative loop cycle failed", "error", wrapped)
		return wrapped
	}

	var allRecords []*domain.Record
	for _, p := range projects {
		recs, rerr := l.store.RecentObservations(p, "", narrativeRecordsFetchLimit)
		if rerr != nil {
			l.cfg.Logger.Warn("narrative loop: RecentObservations failed, skipping project",
				"project", p, "error", rerr)
			continue
		}
		allRecords = append(allRecords, recs...)
	}

	units := AssembleUnits(allRecords)

	var eligible []Unit
	for _, u := range units {
		if u.Eligible(l.cfg.MinObservations, l.cfg.MinCombinedChars) {
			eligible = append(eligible, u)
		}
	}

	cacheKeys, err := l.store.NarrativeCacheKeys()
	if err != nil {
		wrapped := fmt.Errorf("generation loop: NarrativeCacheKeys: %w", err)
		l.recordFailure(wrapped)
		l.cfg.Logger.Warn("narrative loop cycle failed", "error", wrapped)
		return wrapped
	}

	modelName := l.provider.ModelName()

	var misses []missUnit
	cachedCount := 0
	for _, u := range eligible {
		key := UnitKey(u.ProjectKey, u.TopicPrefixKey)
		hash := SourceHash(promptTemplateVersion, modelName, u.ProjectKey, u.TopicPrefixKey, u.Records)
		if existing, ok := cacheKeys[key]; ok && existing == hash {
			cachedCount++
			continue
		}
		misses = append(misses, missUnit{unit: u, key: key, hash: hash})
	}

	// Deterministic (projectKey, topicPrefixKey) order: UnitKey is
	// lower(project) + "\x00" + lower(prefix), and NUL sorts before every
	// printable character, so a plain string sort exactly reproduces
	// tuple ordering. This is what makes the miss set shrink
	// monotonically across cycles (task 6.9) rather than starving: a
	// generated unit becomes a cache hit, so it drops out of next cycle's
	// misses, and the next N lexicographically-ordered units advance in
	// its place.
	sort.Slice(misses, func(i, j int) bool { return misses[i].key < misses[j].key })

	var (
		generated, gated, failed, deferred int
		unitsUsed, charsUsed               int
		consecutiveErrors                  int
		staleKeys                          []string
	)

	// sessionDirCache resolves REQ-PROV-02's generation-time path
	// verification: MostRecentSessionDirectory is queried AT MOST ONCE per
	// project per cycle (mirroring the "query shape is the real answer,
	// bounded, does not grow with note count" discipline design #4754 §5
	// already holds every other store call in this loop to), never once
	// per unit. comma-ok on this map alone tracks "already queried this
	// cycle" -- an empty-string result is cached exactly like a non-empty
	// one, so a project with no known session is not re-queried on every
	// one of its units either.
	sessionDirCache := map[string]string{}

cycleLoop:
	for i, m := range misses {
		if ctx.Err() != nil {
			// Mid-cycle cancellation: the remaining misses are simply not
			// processed this cycle. They stay cache misses and are
			// rediscovered by the same miss test next cycle -- this is
			// NOT logged as a budget deferral (cancellation is not a
			// ceiling).
			break cycleLoop
		}

		if unitsUsed >= l.cfg.MaxUnitsPerCycle || charsUsed+m.unit.InputChars > l.cfg.MaxInputCharsPerCycle {
			// Budget ceiling reached BEFORE building this unit's prompt or
			// calling the provider (REQ-GEN-06). Every remaining miss --
			// this one included -- is deferred; any of them that already
			// has a stored row is marked stale with ONE batched call
			// after this loop (REQ-NARR-06).
			deferred = len(misses) - i
			for _, rest := range misses[i:] {
				if _, exists := cacheKeys[rest.key]; exists {
					staleKeys = append(staleKeys, rest.key)
				}
			}
			break cycleLoop
		}

		prompt := BuildPrompt(m.unit)
		text, genErr := l.provider.Generate(ctx, m.unit.ProjectKey, prompt)

		if errors.Is(genErr, ErrGenerationGated) {
			// A gated unit consumes NO budget -- it cost no tokens. The
			// prompt above was already assembled in process memory before
			// this call; the gate is what prevented it from leaving
			// (design #4754 §2's accepted, stated property).
			gated++
			continue
		}

		// Every other outcome (success, or any provider error) charges
		// the budget: it reached the provider.
		unitsUsed++
		charsUsed += m.unit.InputChars

		if genErr != nil {
			failed++
			if errors.Is(genErr, errGenerationRejected) {
				l.cfg.Logger.Warn("narrative loop: request rejected, abandoning cycle",
					"project", m.unit.ProjectKey, "topic_prefix", m.unit.TopicPrefixKey, "error", genErr)
				break cycleLoop
			}
			consecutiveErrors++
			l.cfg.Logger.Warn("narrative loop: generation failed",
				"project", m.unit.ProjectKey, "topic_prefix", m.unit.TopicPrefixKey,
				"error", genErr, "consecutive_errors", consecutiveErrors)
			if consecutiveErrors >= narrativeCircuitBreakThreshold {
				l.cfg.Logger.Warn("narrative loop: consecutive provider errors, abandoning cycle",
					"threshold", narrativeCircuitBreakThreshold)
				break cycleLoop
			}
			// Failed unit caches NOTHING (stays a miss, retried first next
			// cycle) -- caching an empty/failed body would poison the
			// cache with a permanent hit.
			continue
		}

		consecutiveErrors = 0
		generated++

		// REQ-PROV-02: extract path-like tokens from the model's own prose
		// and os.Stat each one against the project's most recently known
		// session directory, NOW at generation time -- never deferred to
		// render time, which would make the rendered bytes depend on the
		// filesystem AT RENDER TIME (paths.go's VerifyPaths doc comment;
		// design #4754 §6/§8). A path that cannot be resolved is listed,
		// never silently dropped and never silently presented as
		// confirmed.
		var unverifiedPaths []string
		if candidates := ExtractPaths(text); len(candidates) > 0 {
			sessionDir, cached := sessionDirCache[m.unit.ProjectKey]
			if !cached {
				dir, serr := l.store.MostRecentSessionDirectory(m.unit.ProjectKey)
				if serr != nil {
					l.cfg.Logger.Warn("narrative loop: MostRecentSessionDirectory failed, treating as unknown",
						"project", m.unit.ProjectKey, "error", serr)
					dir = ""
				}
				sessionDirCache[m.unit.ProjectKey] = dir
				sessionDir = dir
			}
			_, unverifiedPaths = VerifyPaths(sessionDir, candidates)
		}

		row := localstore.NarrativeRow{
			Project:         m.unit.ProjectKey,
			TopicPrefix:     m.unit.TopicPrefixKey,
			Body:            text,
			SourceHash:      m.hash,
			Model:           modelName,
			TemplateVersion: promptTemplateVersion,
			RendererVersion: rendererVersion,
			SourceCount:     len(m.unit.Records),
			SourceWriters:   joinedSourceWriters(m.unit.Records),
			UnverifiedPaths: strings.Join(unverifiedPaths, "\n"),
			// Truncated remains hard-coded false: GenerationProvider.Generate
			// returns (string, error) only -- there is no channel for a
			// provider to signal it truncated its own output.
			// REQ-GEN-07's client-side output bound is explicitly Phase 10
			// scope (tasks 10.7/10.8, internal/generation/mistral.go, the
			// provider that will actually decode and bound an HTTP
			// response) -- see the gap-closing apply report's port-impact
			// note on whether that requires widening this signature.
			Truncated:   false,
			Stale:       false,
			GeneratedAt: time.Now().UTC(),
		}
		if uerr := l.store.UpsertNarrative(row); uerr != nil {
			l.cfg.Logger.Warn("narrative loop: UpsertNarrative failed",
				"project", row.Project, "topic_prefix", row.TopicPrefix, "error", uerr)
		}
	}

	if len(staleKeys) > 0 {
		if serr := l.store.MarkNarrativesStale(staleKeys); serr != nil {
			l.cfg.Logger.Warn("narrative loop: MarkNarrativesStale failed", "error", serr)
		}
	}

	outcome := CycleOutcome{
		Generated: generated,
		Cached:    cachedCount,
		Deferred:  deferred,
		Gated:     gated,
		Failed:    failed,
	}
	l.recordSuccess(outcome)

	l.cfg.Logger.Info("narrative loop cycle",
		"generated", generated,
		"cached", cachedCount,
		"deferred", deferred,
		"gated", gated,
		"failed", failed,
		"duration", time.Since(start),
	)

	return nil
}

// recordSuccess records a completed cycle's outcome (including one that
// deferred, gated, or failed some individual units -- none of those make
// the CYCLE itself a failure, REQ-GEN-06), clearing any prior
// cycle-level failure state.
func (l *Loop) recordSuccess(o CycleOutcome) {
	l.resultMu.Lock()
	defer l.resultMu.Unlock()
	l.consecutiveFailures = 0
	o.LastCycleAt = time.Now().UTC()
	// Err and ConsecutiveFailures are left at their zero values by this
	// full-struct replace -- a success clears both.
	l.lastResult = o
}

// recordFailure records a cycle-level infrastructure failure (ListProjects
// or NarrativeCacheKeys erroring). It deliberately does NOT touch
// Generated/Cached/Deferred/Gated/Failed: overwriting those with zeros here
// would make a healthy prior cycle's counts look lost the moment one
// transient failure occurs.
func (l *Loop) recordFailure(err error) {
	l.resultMu.Lock()
	defer l.resultMu.Unlock()
	l.consecutiveFailures++
	l.lastResult.LastCycleAt = time.Now().UTC()
	l.lastResult.Err = err.Error()
	l.lastResult.ConsecutiveFailures = l.consecutiveFailures
}

// joinedSourceWriters returns the comma-joined, SORTED, DISTINCT writer IDs
// across recs -- matching localstore.NarrativeRow.SourceWriters's documented
// storage shape.
func joinedSourceWriters(recs []*domain.Record) string {
	seen := make(map[string]bool, len(recs))
	var writers []string
	for _, r := range recs {
		if r.WriterID == "" || seen[r.WriterID] {
			continue
		}
		seen[r.WriterID] = true
		writers = append(writers, r.WriterID)
	}
	sort.Strings(writers)
	return strings.Join(writers, ",")
}
