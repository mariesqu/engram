package obsidian

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Exportable is the interface obsidian.Loop drives once per cycle. It is
// satisfied by *Exporter with no adapter — SetGraphConfig was already added
// to *Exporter in Phase 7 (task 7.2) for exactly this purpose. Kept as its
// own narrow interface (rather than depending on *Exporter directly) so loop
// tests can drive a hand-rolled counting/blocking fake — no mocking
// framework exists in this repo (design constraint 6).
type Exportable interface {
	// Export runs one export cycle. Deliberately takes NO context.Context —
	// design constraint 15 / REQ-WATCH-05. This is not an omission: it is
	// the mechanism that makes Loop.Stop()'s drain guarantee true by
	// construction rather than by extra bookkeeping. A ctx-aware Export
	// could observe cancellation between writing observation notes and
	// writing .engram-sync-state.json, which is exactly the corruption
	// REQ-WATCH-05 forbids on exactly the file the Obsidian viewer reads.
	// Because Export takes no ctx, a cancellation delivered while a cycle
	// is running simply cannot be observed until the cycle returns on its
	// own — Loop's run loop only ever checks ctx BETWEEN cycles (see run).
	Export() (*ExportResult, error)

	// SetGraphConfig lets the loop downgrade the graph-config bootstrap to
	// GraphConfigSkip after the first successful cycle of this process
	// (REQ-WATCH-06) without the loop needing to know anything about vault
	// paths or the embedded template.
	//
	// There is deliberately no GraphConfig() accessor on this interface:
	// Loop never reads one back (it only ever writes), and adding one would
	// be dead surface with no non-test caller (Phase 8 adversarial review,
	// MAJOR-3). *Exporter still exposes its own GraphConfig() method for
	// callers that need it directly — see the warning on that method about
	// what it reports after cycle 1.
	SetGraphConfig(GraphConfigMode)
}

// errNilExportResult is recorded as the failure when an Exportable returns
// (nil, nil) — a contract violation *Exporter never commits (verified), but
// Exportable is an exported interface any implementation could get wrong.
// Without this guard, runCycle would hand a nil *ExportResult to
// recordSuccess, which dereferences it unconditionally and panics — and an
// unrecovered panic in this background goroutine kills the whole daemon
// process (Phase 8 adversarial review, MINOR-1).
var errNilExportResult = errors.New("obsidian: Exportable.Export returned (nil, nil)")

// loopIntervalFloor is the production minimum interval enforced by
// applyLoopDefaults (REQ-WATCH-04). Tests that need faster cycling do not
// mutate this constant — that would require save/restore boilerplate around
// every fast test and would race if tests ever ran in parallel — they
// instead set LoopConfig.floorOverride (see below).
const loopIntervalFloor = time.Minute

// LoopConfig holds tunables for a Loop. Zero values are replaced by
// defaults via applyLoopDefaults.
type LoopConfig struct {
	// Interval is the cadence between export cycles (REQ-WATCH-04).
	//
	//   - 0        -> defaults to 10 minutes.
	//   - i < floor -> CLAMPED to the floor (loopIntervalFloor, 1 minute in
	//     production), with a logged warning. This applies to EVERY value
	//     below the floor, POSITIVE OR NEGATIVE, with no sign-based
	//     exception: a prior version of this code treated a negative
	//     Interval as a test-only sentinel that bypassed the floor and the
	//     warning entirely (using its absolute value verbatim) — that was
	//     CRITICAL-1 from the Phase 8 adversarial review.
	//     time.Duration(math.MinInt64) negated back to itself (two's
	//     complement overflow) and stayed negative, so time.NewTimer fired
	//     instantly forever: a pure spin that saturated the single DB
	//     connection (localstore.Store.SetMaxOpenConns(1)) and starved MCP.
	//     Clamping is deliberately NOT a startup-fatal error: refusing to
	//     boot the daemon (which also owns MCP and the other background
	//     loops) over a cosmetic export cadence would be a strictly worse
	//     failure than exporting slightly less often than asked.
	Interval time.Duration

	// Logger receives one structured record per cycle (REQ-WATCH-11) plus
	// warnings. Default: slog.Default(). The loop NEVER writes to stdout —
	// design constraint 16: in the daemon's default mode, stdout is the MCP
	// JSON-RPC channel (cmd/engram/daemon.go's runDaemonWithIO), and a
	// stray print here would corrupt every connected client's framing.
	Logger *slog.Logger

	// floorOverride, when greater than zero, replaces loopIntervalFloor for
	// this Loop's defaulting. It is UNEXPORTED, so only code inside this
	// package — i.e. this package's own tests — can ever set it. This is
	// what makes the unsafe state unrepresentable from production
	// configuration: a caller in another package (Phase 9's daemon/config
	// wiring) can only ever set the exported Interval field, and every
	// Interval value of any sign is subject to loopIntervalFloor with no
	// bypass available to it. Tests that need a fast cadence set both a
	// small positive Interval and a small floorOverride, instead of relying
	// on Interval's sign to escape the floor.
	floorOverride time.Duration
}

// applyLoopDefaults fills in zero (unset) LoopConfig fields with their
// defaults and applies the interval floor described on LoopConfig.Interval.
func applyLoopDefaults(c LoopConfig) LoopConfig {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}

	floor := loopIntervalFloor
	if c.floorOverride > 0 {
		floor = c.floorOverride
	}

	// DELIBERATE, DOCUMENTED DIVERGENCE (Phase 9 adversarial review MINOR-1):
	// this floor treats EVERY sub-floor value the same way regardless of
	// sign — a negative Interval is clamped to floor (1m in production), same
	// as a tiny positive one. cmd/engram's resolveObsidianInterval, the
	// daemon's OWN independent layer in front of this one, treats a negative
	// value differently: it is not "too small", it is "not a valid cadence at
	// all", so the daemon falls back to its 10m DEFAULT rather than its 1m
	// FLOOR. Both outcomes are equally safe (a positive, >= 1-minute timer
	// either way) — this is not a bug, and the two are NOT reconciled to a
	// single behaviour, for two reasons: (1) they answer different questions
	// — this package's floor has no notion of a "default", only a safety
	// minimum, because it must remain correct for ANY caller, not only the
	// one daemon that happens to resolve first; (2) changing either
	// arrangement now would alter behaviour the Phase 8 review already
	// verified safe, for a difference that is provably unreachable through
	// today's only caller (config.Load rejects a non-positive
	// obsidian_interval outright, so a negative value can never even reach
	// resolveObsidianInterval from a config file). See
	// TestBuildDaemonResolvedIntervalReachesLoop (cmd/engram) and
	// Loop.ConfiguredInterval, which exist so a future change that
	// accidentally lets a raw, unresolved value reach this floor is caught by
	// a real assertion instead of by re-reading this comment.
	switch {
	case c.Interval == 0:
		c.Interval = 10 * time.Minute
	case c.Interval < floor:
		// Covers both a positive sub-floor value AND any negative value
		// (including time.Duration(math.MinInt64)) — no sign-based fast
		// path, no arithmetic negation, so no overflow is possible here.
		c.Logger.Warn("obsidian.Loop: interval below the 1-minute floor, clamping",
			"requested", c.Interval,
			"clamped_to", floor,
		)
		c.Interval = floor
	}
	return c
}

// ExportOutcome is the result of the most recent export cycle, surfaced via
// Loop.LastResult for the daemon status endpoint (Phase 9). The zero value
// (At.IsZero(), empty Err) means no cycle has completed yet.
//
// A FAILED cycle updates At, Err and ConsecutiveFailures but deliberately
// leaves Created/Updated/Deleted/Skipped/Hubs untouched — Export() returns
// no partial ExportResult on error, so overwriting the counters with zeros
// on every transient failure would make a healthy prior cycle's counts look
// lost the moment one blip occurs. A SUCCESSFUL cycle resets Err to "" and
// ConsecutiveFailures to 0 and replaces every counter with the fresh result.
type ExportOutcome struct {
	At                                       time.Time
	Created, Updated, Deleted, Skipped, Hubs int
	Err                                      string
	ConsecutiveFailures                      int
}

// Loop runs Exportable.Export() in the background on a fixed schedule
// (REQ-WATCH-03/-04), shaped after this repo's two existing daemon-hosted
// background workers, syncer.Loop and embedding.Loop: NewLoop(deps, Config),
// Start(ctx) launching exactly one goroutine, Stop() cancelling and BLOCKING
// on a done channel so the caller can close the store immediately
// afterwards, a time.Timer select loop, a Config struct with zero-value
// defaulting, and a *slog.Logger. Renamed from the recovered plan's
// "Watcher" deliberately: it watches nothing (no fsnotify, no inotify) — it
// ticks.
//
// Three deliberate divergences from syncer.Loop/embedding.Loop, each with
// its reason:
//
//  1. NO exponential backoff. Both sibling loops back off because they call
//     a REMOTE provider that can be rate-limited or down. This loop's
//     failure modes are local disk and local SQLite (through the store the
//     daemon already owns) — REQ-WATCH-08 says outright: "The loop MUST NOT
//     enter a tight retry: a failed cycle waits for the next scheduled tick
//     like any other." A fixed cadence with no backoff is not a missing
//     feature here, it is the specified behaviour.
//
//  2. NO Trigger(). Both siblings have one because both have real callers
//     (mem_save -> embedding's trigger; a local write -> syncer's trigger).
//     Nothing calls this one, and a per-save trigger would be actively
//     harmful: every cycle re-lists EVERY project's full record set (4,600+
//     on the real store), so triggering on writes would turn a 10-minute
//     job into a per-keystroke one. Purely additive later if ever wanted.
//
//  3. Cancellation is observed only BETWEEN cycles, never mid-cycle. This
//     falls out of Exportable.Export() taking no context.Context (see that
//     interface's doc comment) — run's select loop only ever waits on
//     ctx.Done() while idle between ticks, never while a cycle is actually
//     executing, so Stop() always drains the in-flight cycle to completion
//     (REQ-WATCH-05).
//
// Construct with NewLoop; start with Start(ctx); stop with Stop().
type Loop struct {
	exp Exportable
	cfg LoopConfig

	// mu serializes Start/Stop lifecycle state, exactly like both sibling
	// loops: it guards started, stopped and cancel so a concurrent Stop can
	// never observe started==true while cancel is still nil.
	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc

	// done is closed by run() just before it exits, letting Stop() block
	// until the goroutine — and therefore any in-flight export cycle — has
	// fully finished.
	done chan struct{}

	// resultMu guards lastResult and is SEPARATE from mu (the lifecycle
	// mutex), mirroring syncer.Loop's resultMu: run() only ever takes
	// resultMu, briefly, to record each cycle's outcome, and never touches
	// the lifecycle lock.
	resultMu   sync.Mutex
	lastResult ExportOutcome
}

// NewLoop constructs a Loop. cfg zero values are replaced by defaults.
func NewLoop(exp Exportable, cfg LoopConfig) *Loop {
	return &Loop{
		exp:  exp,
		cfg:  applyLoopDefaults(cfg),
		done: make(chan struct{}),
	}
}

// Start launches the background export goroutine and returns immediately.
// The first export cycle runs immediately, before any interval elapses
// (REQ-WATCH-03). Calling Start more than once panics, mirroring both
// sibling loops.
func (l *Loop) Start(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started {
		panic("obsidian.Loop: Start called more than once")
	}
	l.started = true
	runCtx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	go l.run(runCtx)
}

// Stop signals the goroutine to exit and BLOCKS until it does — including
// waiting out any export cycle currently in progress (REQ-WATCH-05). A
// bounded drain that abandons the goroutine was deliberately rejected in
// design: leaking a goroutine mid os.WriteFile while the daemon closes the
// store is precisely the corruption class this requirement exists to
// prevent.
//
// Stop is idempotent and nil-safe (mirroring embedding.Loop.Stop): calling
// it on a nil *Loop, or a Loop that was never Started, is a no-op.
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

// LastResult returns the outcome of the most recent completed export cycle.
// The zero value (At.IsZero()) means no cycle has run yet. Safe for
// concurrent use.
func (l *Loop) LastResult() ExportOutcome {
	l.resultMu.Lock()
	defer l.resultMu.Unlock()
	return l.lastResult
}

// ConfiguredInterval returns the cadence this Loop was actually constructed
// with, AFTER applyLoopDefaults — i.e. after this package's own floor has
// already been applied. l.cfg is set once in NewLoop and never mutated
// afterwards (Start/Stop/run never touch it), so this is safe to read without
// a lock at any point in the Loop's lifetime, including before Start.
//
// Exists purely for tests outside this package (cmd/engram's daemon wiring)
// that need to pin WHICH value actually reached the Loop: the daemon has its
// own independent interval resolver (resolveObsidianInterval in
// cmd/engram/daemon.go) that is meant to run BEFORE a value ever reaches
// NewLoop, and a caller that skipped it (passing a raw, unresolved config
// value straight through) would be invisible from outside this package
// without this accessor — see that function's doc comment for why the two
// layers deliberately disagree on how they treat a negative interval.
func (l *Loop) ConfiguredInterval() time.Duration {
	return l.cfg.Interval
}

// run is the goroutine body. It runs one cycle immediately (REQ-WATCH-03),
// then waits on either ctx cancellation or the next tick — cancellation is
// only ever observed here, between cycles, never during l.exp.Export()
// itself (see Exportable's doc comment and divergence 3 above).
//
// A ctx that is ALREADY cancelled when run starts (e.g. Start(ctx) was
// called with a ctx cancelled during daemon startup) skips the first cycle
// entirely instead of running one unconditionally (Phase 8 adversarial
// review, MAJOR-1): a cancelled context means the loop is not starting, so
// running a full cycle first — and forcing Stop() to block for its entire
// duration — is not "running one cycle immediately when it starts"
// (REQ-WATCH-03); it is starting anyway despite being told not to.
func (l *Loop) run(ctx context.Context) {
	defer close(l.done)

	select {
	case <-ctx.Done():
		l.cfg.Logger.Info("obsidian export loop stopped")
		return
	default:
	}

	var (
		consecutiveFailures int
		graphConfigApplied  bool // true once the first cycle of THIS PROCESS to return without error has run (REQ-WATCH-06)
	)

	l.runCycle(&consecutiveFailures, &graphConfigApplied)

	timer := time.NewTimer(l.cfg.Interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			l.cfg.Logger.Info("obsidian export loop stopped")
			return
		case <-timer.C:
			l.runCycle(&consecutiveFailures, &graphConfigApplied)
			timer.Reset(l.cfg.Interval)
		}
	}
}

// runCycle executes exactly one export cycle and records its outcome. It
// never returns an error itself: every failure is logged and swallowed
// here, so REQ-WATCH-08 ("a failing cycle MUST NOT terminate the loop") is
// enforced structurally — run's select loop always proceeds to the next
// tick regardless of what happened in this one.
func (l *Loop) runCycle(consecutiveFailures *int, graphConfigApplied *bool) {
	start := time.Now()
	result, err := l.exp.Export()
	duration := time.Since(start)

	// Guard against a contract-violating Exportable that returns (nil, nil):
	// *Exporter never does this (verified), but Exportable is an exported
	// interface any implementation could get wrong, and recordSuccess
	// dereferences result unconditionally. Treat it as a failure instead of
	// letting a nil-pointer panic kill the daemon (Phase 8 adversarial
	// review, MINOR-1).
	if err == nil && result == nil {
		err = errNilExportResult
	}

	if err != nil {
		*consecutiveFailures++
		l.recordFailure(err, *consecutiveFailures)
		l.cfg.Logger.Warn("obsidian export cycle failed",
			"error", err,
			"consecutive_failures", *consecutiveFailures,
			"duration", duration,
		)
		return
	}

	*consecutiveFailures = 0
	l.recordSuccess(result)
	l.cfg.Logger.Info("obsidian export cycle",
		"created", result.Created,
		"updated", result.Updated,
		"deleted", result.Deleted,
		"skipped", result.Skipped,
		"hubs", result.Hubs,
		"duration", duration,
	)

	// REQ-WATCH-06: the graph-config bootstrap applies on the first cycle of
	// THIS PROCESS only, and only a cycle that returned WITHOUT error — a
	// transient first-cycle failure must not permanently forfeit the
	// bootstrap for the whole process. Once applied, every later cycle in
	// this process is downgraded to GraphConfigSkip regardless of whatever
	// mode the caller originally configured on the Exportable.
	//
	// WARNING for Phase 9: this call PERMANENTLY overwrites the Exporter's
	// configured graph-config mode (*Exporter.SetGraphConfig mutates
	// e.cfg.GraphConfig in place — see exporter.go). After cycle 1 of this
	// process, *Exporter.GraphConfig() reports GraphConfigSkip regardless of
	// what the user originally configured. This is CORRECT for REQ-WATCH-06
	// and must NOT change — but it means a future daemon status endpoint
	// must not source a "configured graph mode" field from
	// Exportable/*Exporter.GraphConfig() after the loop has run; that
	// accessor only ever reflects the NEXT cycle's bootstrap mode, not the
	// user's original setting.
	if !*graphConfigApplied {
		*graphConfigApplied = true
		l.exp.SetGraphConfig(GraphConfigSkip)
	}
}

// recordSuccess records a successful cycle's outcome, clearing any prior
// failure state.
func (l *Loop) recordSuccess(r *ExportResult) {
	l.resultMu.Lock()
	defer l.resultMu.Unlock()
	l.lastResult = ExportOutcome{
		At:      time.Now().UTC(),
		Created: r.Created,
		Updated: r.Updated,
		Deleted: r.Deleted,
		Skipped: r.Skipped,
		Hubs:    r.Hubs,
		// Err and ConsecutiveFailures are reset to their zero values by this
		// full-struct replace — a success clears both.
	}
}

// recordFailure records a failed cycle's outcome. It deliberately does NOT
// touch Created/Updated/Deleted/Skipped/Hubs: Export() returns no partial
// ExportResult on error, and overwriting those counters with zeros here
// would make LastResult() (and the daemon's GET /api/v1/status, Phase 9)
// misreport a healthy prior cycle's counts as lost the moment one transient
// failure occurs.
func (l *Loop) recordFailure(err error, consecutiveFailures int) {
	l.resultMu.Lock()
	defer l.resultMu.Unlock()
	l.lastResult.At = time.Now().UTC()
	l.lastResult.Err = err.Error()
	l.lastResult.ConsecutiveFailures = consecutiveFailures
}
