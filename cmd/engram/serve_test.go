package main

// Fast, no-network unit tests for the serve subcommand's retry-on-startup-
// failure logic (retryOpen / shouldRetryOpenErr / sleepCtx). These do not spin
// a real Postgres — see serve_acceptance_test.go (build tag acceptance) for
// the end-to-end runServe proof against embedded-postgres.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ── shouldRetryOpenErr ────────────────────────────────────────────────────────

// TestShouldRetryOpenErr_DSNParseError_NotRetryable verifies that a wrapped
// "parse dsn:" error (centralstore.Open's exact wrap for a pgxpool.ParseConfig
// failure) is classified fatal — retrying can never fix a malformed DSN.
func TestShouldRetryOpenErr_DSNParseError_NotRetryable(t *testing.T) {
	err := fmt.Errorf("centralstore.Open: parse dsn: %w", errors.New("invalid dsn syntax"))
	if shouldRetryOpenErr(err) {
		t.Error("want false (not retryable) for a DSN parse error, got true")
	}
}

// TestShouldRetryOpenErr_AuthFailure_NotRetryable verifies that a *pgconn.PgError
// with a class-28 (Invalid Authorization Specification) SQLState — e.g. wrong
// password (28P01) — is classified fatal, even after fmt.Errorf wrapping.
func TestShouldRetryOpenErr_AuthFailure_NotRetryable(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "28P01", Message: "password authentication failed"}
	err := fmt.Errorf("centralstore.Open: ping: %w", pgErr)
	if shouldRetryOpenErr(err) {
		t.Error("want false (not retryable) for a class-28 auth PgError, got true")
	}
}

// TestShouldRetryOpenErr_InsufficientPrivilege_NotRetryable verifies that a
// *pgconn.PgError with SQLState 42501 (insufficient_privilege) — a role that
// authenticates fine but lacks the schema/table grants centralstore needs —
// is classified fatal. This is class 42 ("Syntax Error or Access Rule
// Violation"), distinct from the class-28 login/authentication failures
// above; retrying can heal neither a wrong password nor a missing GRANT.
func TestShouldRetryOpenErr_InsufficientPrivilege_NotRetryable(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "42501", Message: "permission denied for schema engram"}
	err := fmt.Errorf("centralstore.Open: ApplySchema: %w", pgErr)
	if shouldRetryOpenErr(err) {
		t.Error("want false (not retryable) for a 42501 insufficient_privilege PgError, got true")
	}
}

// TestShouldRetryOpenErr_ConnectivityErrors_Retryable verifies that ordinary
// connectivity failures (the "no such host" case from the motivating incident,
// connection refused, a bare pgconn wrapping error) are retried.
func TestShouldRetryOpenErr_ConnectivityErrors_Retryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"dns not resolvable", fmt.Errorf("centralstore.Open: ping: %w", errors.New("dial tcp: lookup central.example.com: no such host"))},
		{"connection refused", fmt.Errorf("centralstore.Open: ping: %w", errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"))},
		{"pgxpool.NewWithConfig failure", fmt.Errorf("centralstore.Open: pgxpool.NewWithConfig: %w", errors.New("boom"))},
		{"ApplySchema failure", fmt.Errorf("centralstore.Open: ApplySchema: %w", errors.New("connection reset"))},
		{"non-auth PgError (e.g. undefined table)", fmt.Errorf("centralstore.Open: ApplySchema: %w", &pgconn.PgError{Code: "42P01", Message: "undefined table"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !shouldRetryOpenErr(tt.err) {
				t.Errorf("want true (retryable) for %v, got false", tt.err)
			}
		})
	}
}

// TestShouldRetryOpenErr_Nil verifies the nil guard: a nil error is not
// "retryable" — callers only ask this question after a failed open.
func TestShouldRetryOpenErr_Nil(t *testing.T) {
	if shouldRetryOpenErr(nil) {
		t.Error("want false for nil error")
	}
}

// ── retryOpen ─────────────────────────────────────────────────────────────────

// alwaysRetry and neverRetry are shouldRetry stand-ins for tests that don't
// care about error classification, only about the loop/backoff mechanics.
func alwaysRetry(error) bool { return true }
func neverRetry(error) bool  { return false }

// noopLogf discards log lines so tests stay quiet.
func noopLogf(string, ...any) {}

// TestRetryOpen_SucceedsFirstTry verifies the zero-retry happy path: openFn
// succeeds immediately, no sleep, no error.
func TestRetryOpen_SucceedsFirstTry(t *testing.T) {
	var sleeps int
	sleepFn := func(context.Context, time.Duration) error { sleeps++; return nil }

	openFn := func() error { return nil }

	if err := retryOpen(context.Background(), openFn, sleepFn, alwaysRetry, noopLogf); err != nil {
		t.Fatalf("retryOpen: %v", err)
	}
	if sleeps != 0 {
		t.Errorf("sleeps = %d, want 0 (must not sleep on first-try success)", sleeps)
	}
}

// TestRetryOpen_SucceedsAfterRetries verifies that retryOpen retries a
// retryable error until openFn succeeds, and that the backoff delay passed to
// sleepFn doubles each attempt (5s, 10s, ...).
func TestRetryOpen_SucceedsAfterRetries(t *testing.T) {
	var delays []time.Duration
	sleepFn := func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	}

	attempts := 0
	openFn := func() error {
		attempts++
		if attempts < 4 {
			return errors.New("transient")
		}
		return nil
	}

	if err := retryOpen(context.Background(), openFn, sleepFn, alwaysRetry, noopLogf); err != nil {
		t.Fatalf("retryOpen: %v", err)
	}
	if attempts != 4 {
		t.Errorf("attempts = %d, want 4", attempts)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}
	if len(delays) != len(want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	for i, d := range want {
		if delays[i] != d {
			t.Errorf("delays[%d] = %s, want %s", i, delays[i], d)
		}
	}
}

// TestRetryOpen_BackoffCapsAtMax verifies the exponential backoff stops
// doubling once it would exceed serveRetryMaxDelay, holding steady at the cap.
func TestRetryOpen_BackoffCapsAtMax(t *testing.T) {
	var delays []time.Duration
	sleepFn := func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	}

	attempts := 0
	const wantAttempts = 8 // enough iterations to blow past the 60s cap (5,10,20,40,60,60,60 sleeps)
	openFn := func() error {
		attempts++
		if attempts < wantAttempts {
			return errors.New("still down")
		}
		return nil
	}

	if err := retryOpen(context.Background(), openFn, sleepFn, alwaysRetry, noopLogf); err != nil {
		t.Fatalf("retryOpen: %v", err)
	}
	if len(delays) == 0 {
		t.Fatal("no delays recorded")
	}
	for i, d := range delays {
		if d > serveRetryMaxDelay {
			t.Errorf("delays[%d] = %s, exceeds cap %s", i, d, serveRetryMaxDelay)
		}
	}
	last := delays[len(delays)-1]
	if last != serveRetryMaxDelay {
		t.Errorf("last delay = %s, want cap %s (should have saturated by attempt %d)", last, serveRetryMaxDelay, wantAttempts)
	}
}

// TestRetryOpen_FatalError_NoRetry verifies that when shouldRetry rejects the
// error, retryOpen returns it immediately without sleeping or retrying.
func TestRetryOpen_FatalError_NoRetry(t *testing.T) {
	var sleeps int
	sleepFn := func(context.Context, time.Duration) error { sleeps++; return nil }

	wantErr := errors.New("bad dsn")
	attempts := 0
	openFn := func() error { attempts++; return wantErr }

	err := retryOpen(context.Background(), openFn, sleepFn, neverRetry, noopLogf)
	if !errors.Is(err, wantErr) {
		t.Fatalf("retryOpen: got %v, want %v", err, wantErr)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (must not retry a fatal error)", attempts)
	}
	if sleeps != 0 {
		t.Errorf("sleeps = %d, want 0 (must not sleep before giving up)", sleeps)
	}
}

// TestRetryOpen_ContextCancelledDuringSleep verifies that a ctx cancellation
// observed by sleepFn (simulating SIGINT/SIGTERM arriving during the backoff
// wait) aborts the retry loop and surfaces the context error.
func TestRetryOpen_ContextCancelledDuringSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sleeps := 0
	sleepFn := func(ctx context.Context, _ time.Duration) error {
		sleeps++
		cancel()
		return ctx.Err()
	}

	attempts := 0
	openFn := func() error { attempts++; return errors.New("still down") }

	err := retryOpen(ctx, openFn, sleepFn, alwaysRetry, noopLogf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("retryOpen: got %v, want context.Canceled", err)
	}
	if sleeps != 1 {
		t.Errorf("sleeps = %d, want 1 (must stop immediately once cancelled)", sleeps)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
}

// TestRetryOpen_ContextCancelledBetweenAttempts verifies that retryOpen also
// notices ctx cancellation checked right after a failed attempt (before
// deciding to sleep again), not only inside sleepFn.
func TestRetryOpen_ContextCancelledBetweenAttempts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var sleeps int
	sleepFn := func(context.Context, time.Duration) error { sleeps++; return nil }

	attempts := 0
	openFn := func() error {
		attempts++
		if attempts == 1 {
			cancel() // cancelled AFTER the first failed attempt returns
		}
		return errors.New("still down")
	}

	err := retryOpen(ctx, openFn, sleepFn, alwaysRetry, noopLogf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("retryOpen: got %v, want context.Canceled", err)
	}
	if sleeps != 0 {
		t.Errorf("sleeps = %d, want 0 (cancellation must be checked before sleeping)", sleeps)
	}
}

// TestRetryOpen_FatalErrorWinsOverSimultaneousCancellation verifies the
// check ordering: when a fatal (non-retryable) error and an already-cancelled
// ctx occur together, retryOpen must return the fatal error, not ctx.Err() —
// a genuine misconfiguration must never be masked by an incidental
// cancellation (e.g. SIGINT landing in the same instant as a bad-DSN error).
func TestRetryOpen_FatalErrorWinsOverSimultaneousCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before retryOpen's first attempt even runs

	var sleeps int
	sleepFn := func(context.Context, time.Duration) error { sleeps++; return nil }

	wantErr := errors.New("bad dsn")
	attempts := 0
	openFn := func() error { attempts++; return wantErr }

	err := retryOpen(ctx, openFn, sleepFn, neverRetry, noopLogf)
	if !errors.Is(err, wantErr) {
		t.Fatalf("retryOpen: got %v, want %v (fatal error must win over cancellation)", err, wantErr)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("retryOpen returned context.Canceled instead of the fatal error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if sleeps != 0 {
		t.Errorf("sleeps = %d, want 0 (must not sleep before giving up)", sleeps)
	}
}

// ── sleepCtx ──────────────────────────────────────────────────────────────────

// TestSleepCtx_ReturnsNilAfterDuration verifies the normal (non-cancelled) path.
func TestSleepCtx_ReturnsNilAfterDuration(t *testing.T) {
	start := time.Now()
	if err := sleepCtx(context.Background(), 10*time.Millisecond); err != nil {
		t.Fatalf("sleepCtx: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
		t.Errorf("sleepCtx returned after %s, want >= 10ms", elapsed)
	}
}

// TestSleepCtx_ReturnsCtxErrOnCancel verifies that a pre-cancelled context
// aborts sleepCtx immediately with ctx.Err(), instead of waiting out d.
func TestSleepCtx_ReturnsCtxErrOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := sleepCtx(ctx, 5*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepCtx: got %v, want context.Canceled", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("sleepCtx took %s to notice cancellation, want near-instant", elapsed)
	}
}

// ── runServe (SIGINT/SIGTERM during startup retry) ─────────────────────────────

// TestRunServe_ContextCancelledDuringStartupRetry_ReturnsNilCleanly verifies
// that a ctx cancelled before the store ever finishes connecting (simulating
// SIGINT/SIGTERM arriving while Postgres is down at startup) makes runServe
// return nil — a clean shutdown (exit 0 via main.go's run) — instead of
// wrapping the cancellation as an "open store" error (which used to exit 1).
//
// No real Postgres is needed: the DSN parses fine, but the pre-cancelled ctx
// makes centralstore.Open's pool.Ping fail immediately, which retryOpen
// (via the ctx.Err() check, reached because a bare ping failure is not
// classified fatal by shouldRetryOpenErr) surfaces as ctx.Err().
func TestRunServe_ContextCancelledDuringStartupRetry_ReturnsNilCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate SIGINT/SIGTERM landing before the store ever connects

	done := make(chan error, 1)
	go func() {
		done <- runServe(ctx, ":0", "postgres://user:pass@127.0.0.1:5432/db")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runServe: got %v, want nil (clean shutdown)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServe did not return promptly for an already-cancelled ctx")
	}
}

// TestRunServe_FatalErrorWithSimultaneousCancellation_ReturnsError verifies
// that runServe's post-retryOpen guard decides purely from the returned
// error, never from ambient ctx state. A pre-cancelled ctx combined with a
// DSN that fails pgxpool.ParseConfig makes centralstore.Open return a fatal
// "parse dsn:"-prefixed error (shouldRetryOpenErr classifies this fatal, so
// retryOpen returns it immediately, before ever consulting ctx.Err()). Even
// though ctx is simultaneously cancelled (simulating SIGINT/SIGTERM racing
// the failure), runServe must surface the fatal cause — NOT swallow it as a
// clean shutdown (nil, exit 0) just because ctx.Err() != nil.
func TestRunServe_FatalErrorWithSimultaneousCancellation_ReturnsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate SIGINT/SIGTERM landing in the same instant as the fatal error

	done := make(chan error, 1)
	go func() {
		done <- runServe(ctx, ":0", "not a valid dsn ://???")
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("runServe: got nil, want a non-nil error for a fatal DSN-parse failure")
		}
		if errors.Is(err, context.Canceled) {
			t.Errorf("runServe: got %v, want the fatal cause, not context.Canceled", err)
		}
		if !strings.Contains(err.Error(), "parse dsn:") {
			t.Errorf("runServe: got %v, want it to contain the fatal cause (\"parse dsn:\")", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServe did not return promptly for an already-cancelled ctx")
	}
}
