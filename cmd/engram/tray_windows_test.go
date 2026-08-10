//go:build windows

package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/tray"
)

// TestEnsureDaemonRetrying_SecondAttemptSucceeds verifies that a first
// ensureFn failure does not stop the loop: ensureDaemonRetrying retries and
// returns the TrayConfig from the attempt that finally succeeds, instead of
// propagating the first error (the bug this fix closes — see
// tray_windows.go's runTrayCmd doc comment).
func TestEnsureDaemonRetrying_SecondAttemptSucceeds(t *testing.T) {
	want := tray.TrayConfig{Port: 4242, Token: "tok", DBDir: "dir", Version: "v1"}

	var attempts int
	ensureFn := func(dbDir, dbPath string, running func() bool) (tray.TrayConfig, func() bool, error) {
		attempts++
		if attempts == 1 {
			return tray.TrayConfig{}, nil, errors.New("daemon did not become healthy within 10s after auto-launch")
		}
		return want, nil, nil
	}

	var sleeps []time.Duration
	sleepFn := func(d time.Duration) { sleeps = append(sleeps, d) }

	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, format) }

	got, err := ensureDaemonRetrying("dir", "dir/test.db", ensureFn, sleepFn, logf)

	if err != nil {
		t.Fatalf("ensureDaemonRetrying: unexpected error %v", err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if attempts != 2 {
		t.Errorf("attempts=%d, want exactly 2 (fail once, succeed on retry)", attempts)
	}
	if len(sleeps) != 1 {
		t.Fatalf("sleeps=%v, want exactly 1 backoff sleep between the two attempts", sleeps)
	}
	if sleeps[0] != trayEnsureRetryInitialDelay {
		t.Errorf("first backoff delay = %s, want the initial delay %s", sleeps[0], trayEnsureRetryInitialDelay)
	}
	if len(logs) != 1 {
		t.Errorf("logf calls=%d, want exactly 1 (log once per failure)", len(logs))
	}
}

// TestEnsureDaemonRetrying_SucceedsOnFirstAttempt verifies the quiet path:
// when ensureFn succeeds immediately, ensureDaemonRetrying returns without
// ever sleeping or logging — the common case (daemon already up, or a
// normal fast auto-launch) must not pay any backoff cost or emit any noise.
func TestEnsureDaemonRetrying_SucceedsOnFirstAttempt(t *testing.T) {
	want := tray.TrayConfig{Port: 7, Token: "t", DBDir: "d", Version: "v"}

	var attempts int
	ensureFn := func(dbDir, dbPath string, running func() bool) (tray.TrayConfig, func() bool, error) {
		attempts++
		return want, nil, nil
	}

	var sleeps []time.Duration
	sleepFn := func(d time.Duration) { sleeps = append(sleeps, d) }

	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, format) }

	got, err := ensureDaemonRetrying("dir", "dir/test.db", ensureFn, sleepFn, logf)

	if err != nil {
		t.Fatalf("ensureDaemonRetrying: unexpected error %v", err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if attempts != 1 {
		t.Errorf("attempts=%d, want exactly 1", attempts)
	}
	if len(sleeps) != 0 {
		t.Errorf("sleeps=%v, want zero (no backoff on immediate success)", sleeps)
	}
	if len(logs) != 0 {
		t.Errorf("logs=%v, want zero (no log calls on immediate success)", logs)
	}
}

// TestEnsureDaemonRetrying_KeepsRetryingWithGrowingDelay verifies that
// repeated ensureFn failures keep the loop alive (up to the attempt cap) and
// that the backoff delay doubles each time, capping at trayEnsureRetryMaxDelay
// — mirrors serve.go's retryOpen schedule (5s, 10s, 20s, ... capped at 60s).
func TestEnsureDaemonRetrying_KeepsRetryingWithGrowingDelay(t *testing.T) {
	want := tray.TrayConfig{Port: 1, Token: "t", DBDir: "d", Version: "v"}

	// Fail enough times that the delay would exceed the cap if left
	// unbounded, to prove the ceiling is enforced, but stay well under
	// trayEnsureRetryMaxAttempts so this test exercises the "keeps
	// retrying" path, not the "gives up" path (see
	// TestEnsureDaemonRetrying_GivesUpAfterMaxAttempts for that).
	const failuresBeforeSuccess = 6
	var attempts int
	ensureFn := func(dbDir, dbPath string, running func() bool) (tray.TrayConfig, func() bool, error) {
		attempts++
		if attempts <= failuresBeforeSuccess {
			return tray.TrayConfig{}, nil, errors.New("still unhealthy")
		}
		return want, nil, nil
	}

	var sleeps []time.Duration
	sleepFn := func(d time.Duration) { sleeps = append(sleeps, d) }
	logf := func(format string, args ...any) {}

	got, err := ensureDaemonRetrying("dir", "dir/test.db", ensureFn, sleepFn, logf)

	if err != nil {
		t.Fatalf("ensureDaemonRetrying: unexpected error %v", err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if attempts != failuresBeforeSuccess+1 {
		t.Errorf("attempts=%d, want %d (the loop must keep retrying until success)", attempts, failuresBeforeSuccess+1)
	}
	if len(sleeps) != failuresBeforeSuccess {
		t.Fatalf("sleeps=%v, want exactly %d backoff sleeps", sleeps, failuresBeforeSuccess)
	}

	wantDelay := trayEnsureRetryInitialDelay
	for i, got := range sleeps {
		if got != wantDelay {
			t.Errorf("sleep[%d] = %s, want %s", i, got, wantDelay)
		}
		wantDelay *= 2
		if wantDelay > trayEnsureRetryMaxDelay {
			wantDelay = trayEnsureRetryMaxDelay
		}
	}
	// With 6 failures (5s,10s,20s,40s,60s,60s) the delay must have hit the
	// cap by the last sleep.
	if sleeps[len(sleeps)-1] != trayEnsureRetryMaxDelay {
		t.Errorf("last sleep = %s, want the cap %s to have been reached", sleeps[len(sleeps)-1], trayEnsureRetryMaxDelay)
	}
}

// TestEnsureDaemonRetrying_GivesUpAfterMaxAttempts verifies the CRITICAL
// fix: a PERMANENT failure (ensureFn never succeeds) must not retry
// invisibly forever. The loop stops after exactly
// trayEnsureRetryMaxAttempts attempts and returns the last error, so
// runTrayCmd can propagate it and exit non-zero (visible in Task
// Scheduler/Event Viewer) instead of hanging with no tray icon and no
// diagnostic trace.
func TestEnsureDaemonRetrying_GivesUpAfterMaxAttempts(t *testing.T) {
	wantErr := errors.New("permanently broken")

	var attempts int
	ensureFn := func(dbDir, dbPath string, running func() bool) (tray.TrayConfig, func() bool, error) {
		attempts++
		return tray.TrayConfig{}, nil, wantErr
	}

	var sleeps []time.Duration
	sleepFn := func(d time.Duration) { sleeps = append(sleeps, d) }

	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, format) }

	_, err := ensureDaemonRetrying("dir", "dir/test.db", ensureFn, sleepFn, logf)

	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want it to wrap %v", err, wantErr)
	}
	if attempts != trayEnsureRetryMaxAttempts {
		t.Errorf("attempts=%d, want exactly %d (the loop must stop at the cap)", attempts, trayEnsureRetryMaxAttempts)
	}
	// One sleep between each pair of attempts, none after the final one.
	if len(sleeps) != trayEnsureRetryMaxAttempts-1 {
		t.Errorf("sleeps=%d, want exactly %d (no sleep after the final attempt)", len(sleeps), trayEnsureRetryMaxAttempts-1)
	}
	if len(logs) != trayEnsureRetryMaxAttempts {
		t.Errorf("logs=%d, want exactly %d (one log per attempt, including the final giving-up line)", len(logs), trayEnsureRetryMaxAttempts)
	}
	if len(logs) > 0 && !strings.Contains(logs[len(logs)-1], "giving up") {
		t.Errorf("final log = %q, want it to mention giving up", logs[len(logs)-1])
	}
}

// TestEnsureDaemonWith_SpawnDecision verifies the fix at the core of the
// CRITICAL "re-spawns a new daemon every retry cycle" bug: when a previous
// retry's spawned child is STILL running, ensureDaemonWith must not spawn a
// second one (it only re-runs the health wait), so a slow-to-start daemon
// never accumulates a second, third, ... Nth concurrent process — one more
// orphan per retry cycle. Once that child has exited (running() == false),
// or on the very first attempt (running == nil), a new spawn is expected.
func TestEnsureDaemonWith_SpawnDecision(t *testing.T) {
	tests := []struct {
		name       string
		running    func() bool
		wantSpawns int
	}{
		{name: "no previous spawn (first attempt)", running: nil, wantSpawns: 1},
		{name: "previous child already exited", running: func() bool { return false }, wantSpawns: 1},
		{name: "previous child still running", running: func() bool { return true }, wantSpawns: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dbDir := t.TempDir() // no daemon.json here — always reports "not yet healthy"
			dbPath := filepath.Join(dbDir, "test.db")

			var spawnCalls int
			spawnFn := func(exe, dbPath string) (func() bool, error) {
				spawnCalls++
				return func() bool { return true }, nil
			}
			sleepFn := func(time.Duration) {} // no real delay — waitForHealthyDaemon still runs its full attempt budget
			probeHTTPFn := func(port int, token string) error { return errors.New("unreachable") }

			_, _, err := ensureDaemonWith(dbDir, dbPath, tt.running, spawnFn, sleepFn, probeHTTPFn)

			if err == nil {
				t.Fatal("want an error (daemon.json never appears in this test), got nil")
			}
			if spawnCalls != tt.wantSpawns {
				t.Errorf("spawnFn called %d times, want %d", spawnCalls, tt.wantSpawns)
			}
		})
	}
}

// TestEnsureDaemonRetrying_ThreadsRunningTracker_SpawnsOnce is an
// integration-style test that wires ensureDaemonRetrying to the REAL
// ensureDaemonWith (not a hand-rolled ensureFn stub) via fake spawnFn/
// sleepFn/probeHTTPFn seams, proving the tracker returned by attempt N is
// actually threaded into attempt N+1's running parameter across several
// retry iterations — TestEnsureDaemonWith_SpawnDecision only exercises a
// single ensureDaemonWith call in isolation and never proves the threading
// through ensureDaemonRetrying's loop itself.
func TestEnsureDaemonRetrying_ThreadsRunningTracker_SpawnsOnce(t *testing.T) {
	dbDir := t.TempDir() // no daemon.json ever appears here, and
	// waitForHealthyDaemon short-circuits on the ReadDaemonJSON error before
	// ever calling probeHTTPFn — the missing daemon.json alone keeps every
	// attempt unhealthy, so ensureDaemonRetrying retries all the way to
	// trayEnsureRetryMaxAttempts. (probeHTTPFn below is a required seam but
	// is never reached in this scenario.)
	dbPath := filepath.Join(dbDir, "test.db")

	var spawnCalls int
	spawnFn := func(exe, dbPath string) (func() bool, error) {
		spawnCalls++
		return func() bool { return true }, nil // the spawned child never exits
	}
	probeHTTPFn := func(port int, token string) error { return errors.New("connection refused") }
	innerSleepFn := func(time.Duration) {} // waitForHealthyDaemon's per-attempt sleep, faked

	ensureFn := func(dbDir, dbPath string, running func() bool) (tray.TrayConfig, func() bool, error) {
		return ensureDaemonWith(dbDir, dbPath, running, spawnFn, innerSleepFn, probeHTTPFn)
	}

	var sleeps []time.Duration
	sleepFn := func(d time.Duration) { sleeps = append(sleeps, d) } // ensureDaemonRetrying's own backoff sleep, faked
	logf := func(format string, args ...any) {}

	_, err := ensureDaemonRetrying(dbDir, dbPath, ensureFn, sleepFn, logf)

	if err == nil {
		t.Fatal("want an error (daemon.json never appears; probeHTTPFn always fails), got nil")
	}
	if len(sleeps) != trayEnsureRetryMaxAttempts-1 {
		t.Fatalf("sleeps=%d, want exactly %d — the loop must have run all %d attempts",
			len(sleeps), trayEnsureRetryMaxAttempts-1, trayEnsureRetryMaxAttempts)
	}
	if spawnCalls != 1 {
		t.Errorf("spawnFn called %d times across %d retry attempts, want exactly 1 — the still-running tracker from attempt 1 must suppress every later spawn",
			spawnCalls, trayEnsureRetryMaxAttempts)
	}
}
