package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mariesqu/engram/internal/centralstore"
	"github.com/mariesqu/engram/internal/cloudserve"
)

// Retry schedule for the initial store connection (see retryOpen). Starts at
// 5s, doubles each failed attempt, caps at 60s. No jitter: a single `engram
// serve` process retrying against one Postgres instance has no thundering-herd
// peer to stagger against — jitter would only add test noise.
const (
	serveRetryInitialDelay = 5 * time.Second
	serveRetryMaxDelay     = 60 * time.Second
)

const serveUsage = `Usage: engram serve [--addr <addr>] [--dsn <dsn>]

Run the central engram HTTP server backed by a Postgres store.

The server listens on plain HTTP. TLS must be terminated upstream by a load
balancer or reverse proxy — do not expose this port directly to the Internet
without TLS termination.

On SIGINT or SIGTERM the server performs a graceful HTTP shutdown (drains
in-flight requests for up to 10 seconds) before exiting.

If the initial connection to Postgres fails for a connectivity reason (DNS
not yet resolvable, network unreachable, connection refused, ...), engram
serve retries with exponential backoff (5s, doubling, capped at 60s) instead
of exiting — SIGINT/SIGTERM abort the wait cleanly (exit 0) instead of
retrying. A malformed --dsn, a Postgres authentication failure, or an
insufficient-privilege error (role authenticates but lacks required grants)
exits immediately with an error: retrying cannot fix any of those.

Flags:
  --addr  Listen address (default: ENGRAM_ADDR env, then ":8080")
  --dsn   Postgres DSN (default: ENGRAM_DSN env; REQUIRED)
`

// runServeCmd parses the serve subcommand flags and delegates to runServe.
// It installs OS signal handling so SIGINT/SIGTERM trigger graceful shutdown.
func runServeCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), serveUsage) }

	addr := fs.String("addr", envOr("ENGRAM_ADDR", ":8080"), "listen address")
	// --dsn defaults to EMPTY (not envOr): a Postgres DSN carries credentials, and
	// flag error output / PrintDefaults print a flag's default value. Resolving
	// ENGRAM_DSN here would bake the secret into the flag default and leak it via
	// --help. ENGRAM_DSN is resolved after Parse instead.
	dsn := fs.String("dsn", "", "Postgres DSN (required; or set ENGRAM_DSN)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // --help printed usage; successful early-exit (exit 0)
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("serve takes no positional arguments; unexpected: %v", fs.Args())
	}
	if *dsn == "" {
		*dsn = envOr("ENGRAM_DSN", "") // resolve env AFTER parse so the secret never enters the flag default
	}
	if *dsn == "" {
		return fmt.Errorf("--dsn is required (or set ENGRAM_DSN)")
	}

	// Install signal context here (the OS/signal layer). runServe itself receives
	// a plain context so tests can pass their own cancellable context without
	// touching signal machinery.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return runServe(ctx, *addr, *dsn)
}

// runServe is the testable core of the serve subcommand. It opens the store
// (retrying transient connectivity failures — see retryOpen/shouldRetryOpenErr),
// wires up the cloudserve server with real HMAC auth, logs the listen address,
// and blocks until ctx is cancelled (SIGINT/SIGTERM in production; test
// cancellation in acceptance tests). On context cancellation, cloudserve.Run
// performs a graceful HTTP shutdown before returning.
//
// A SIGINT/SIGTERM that arrives WHILE still retrying the initial connection
// (Postgres down at startup) is also a normal, requested shutdown — not a
// startup failure — so it must exit 0 (see run's exit-code table in main.go),
// matching the exit code of the already-running-server graceful-shutdown path.
//
// TLS note: the server listens on plain HTTP. All TLS termination must happen
// at an upstream load balancer or reverse proxy. This is intentional: running
// the central server behind infrastructure-level TLS (e.g. an ALB with an ACM
// cert) keeps the Go binary stateless and simplifies cert rotation.
func runServe(ctx context.Context, addr, dsn string) error {
	var store *centralstore.Store
	openFn := func() error {
		s, err := centralstore.Open(ctx, dsn)
		if err != nil {
			return err
		}
		store = s
		return nil
	}
	if err := retryOpen(ctx, openFn, sleepCtx, shouldRetryOpenErr, log.Printf); err != nil {
		// retryOpen returns a context-cancellation error only when ctx itself
		// was cancelled (SIGINT/SIGTERM while Postgres was still unreachable),
		// never as a wrap of a genuine store-open failure. Report that as a
		// clean shutdown (nil, exit 0) instead of a startup error (exit 1) —
		// the operator asked the process to stop; it did.
		//
		// This decision MUST come from err itself, never from ambient ctx
		// state (e.g. ctx.Err() != nil). retryOpen guarantees a fatal,
		// non-retryable error (bad DSN, class-28 auth, 42501) is returned as
		// itself even when ctx is simultaneously cancelled (SIGINT racing the
		// failure) — checking ctx.Err() here too would swallow that fatal
		// error as a clean shutdown, hiding the real cause from the operator
		// and breaking the fatal-beats-cancellation invariant retryOpen
		// establishes above.
		//
		// DeadlineExceeded is unreachable in production (runServeCmd's
		// signal.NotifyContext carries no deadline); it is matched for
		// callers that inject a timeout-bound ctx directly, e.g. tests.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			log.Printf("engram serve: shutdown requested during startup retry")
			return nil
		}
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	srv := cloudserve.New(store, cloudserve.NewKeyVerifier(store.WriterKey))

	log.Printf("engram serve: listening on %s (plain HTTP — terminate TLS upstream)", addr)
	return srv.Run(ctx, addr)
}

// retryOpen calls openFn until it returns a nil error, sleeping via sleepFn
// (exponential backoff: serveRetryInitialDelay doubling up to
// serveRetryMaxDelay) between attempts whose error shouldRetry accepts. It
// gives up immediately — without sleeping — the moment shouldRetry rejects an
// error, or the moment ctx is cancelled (checked right after every failed
// attempt, in addition to sleepFn's own ctx-awareness so a cancellation during
// the backoff wait itself is not missed).
//
// Ordering: shouldRetry(err) is evaluated BEFORE the ctx.Err() check. A fatal,
// non-retryable error (bad DSN, auth/privilege failure) must be reported as
// such even if ctx happened to be cancelled in the same instant (e.g. SIGINT
// arriving right as a fatal openFn error comes back) — the operator needs to
// see the real cause, not have it masked by an incidental "context canceled".
// A plain ctx cancellation on an otherwise-retryable error still surfaces as
// ctx.Err() via the check below (runServe treats that as a clean shutdown).
//
// sleepFn and logf are seams: production passes sleepCtx and log.Printf; tests
// inject fakes so the backoff schedule and retry/give-up decisions are
// exercised without real wall-clock delay — mirrors the sleepFn/probeFn seam
// style established in daemonspawn.go's waitForHealthyDaemon.
func retryOpen(
	ctx context.Context,
	openFn func() error,
	sleepFn func(context.Context, time.Duration) error,
	shouldRetry func(error) bool,
	logf func(format string, args ...any),
) error {
	delay := serveRetryInitialDelay
	for {
		err := openFn()
		if err == nil {
			return nil
		}
		if !shouldRetry(err) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logf("engram serve: open store failed, retrying in %s: %v", delay, err)
		if sleepErr := sleepFn(ctx, delay); sleepErr != nil {
			return sleepErr
		}
		delay *= 2
		if delay > serveRetryMaxDelay {
			delay = serveRetryMaxDelay
		}
	}
}

// sleepCtx blocks for d or until ctx is cancelled, whichever comes first,
// returning ctx.Err() in the latter case so a SIGINT/SIGTERM received during
// the retry backoff aborts the wait immediately instead of running it out.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// shouldRetryOpenErr reports whether err from centralstore.Open looks like a
// transient connectivity problem worth retrying (DNS not yet resolvable —
// "no such host", the exact failure that motivated this fix — connection
// refused, dial/handshake timeouts, or a schema-apply failure caused by the
// connection dropping mid-statement) as opposed to a genuinely fatal,
// retry-proof configuration error.
//
// Three classes are treated as fatal (no retry):
//
//  1. A malformed DSN. centralstore.Open wraps a pgxpool.ParseConfig failure
//     with the literal prefix "parse dsn:" BEFORE any network attempt is
//     made. The DSN string is fixed for the lifetime of this process, so no
//     amount of retrying can change that outcome.
//  2. A Postgres-reported login/authentication failure — these surface as a
//     *pgconn.PgError whose SQLState is in class 28 ("Invalid Authorization
//     Specification": 28000 invalid_authorization_specification, 28P01
//     invalid_password). Class 28 is about the connection attempt itself
//     failing (wrong password, role does not exist) — NOT about a role that
//     authenticated fine but lacks privileges; that is class 42 below.
//     Retrying a wrong password forever is pointless noise that also hides
//     an operator mistake needing a fix, not a wait.
//  3. SQLState 42501 (insufficient_privilege, class 42 "Syntax Error or
//     Access Rule Violation"). This is a role that DOES authenticate
//     successfully but lacks the schema/table grants centralstore needs
//     (e.g. during ApplySchema). Retrying cannot heal a missing GRANT any
//     more than it can heal a wrong password — it needs an operator to fix
//     the role's privileges, so treat it the same as a class-28 failure.
//
// Everything else — including pgxpool.NewWithConfig failures and other
// ApplySchema failures, which pgx's error taxonomy does not let us reliably
// separate from a mid-connection network drop — is treated as retryable.
// This intentionally errs toward "keep retrying": the incident this fix
// closes was a laptop booting off-network, and a false "fatal" classification
// here would silently resurrect that exact bug for any error shape we failed
// to anticipate.
func shouldRetryOpenErr(err error) bool {
	if err == nil {
		return false
	}
	if strings.Contains(err.Error(), "parse dsn:") {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if len(pgErr.Code) >= 2 && pgErr.Code[:2] == "28" {
			return false // class 28: login/authentication failure
		}
		if pgErr.Code == "42501" {
			return false // insufficient_privilege: authenticated fine, but lacks grants
		}
	}
	return true
}
