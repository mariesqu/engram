package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"syscall"

	"github.com/mariesqu/engram/internal/centralstore"
	"github.com/mariesqu/engram/internal/cloudserve"
)

const serveUsage = `Usage: engram serve [--addr <addr>] [--dsn <dsn>]

Run the central engram HTTP server backed by a Postgres store.

The server listens on plain HTTP. TLS must be terminated upstream by a load
balancer or reverse proxy — do not expose this port directly to the Internet
without TLS termination.

On SIGINT or SIGTERM the server performs a graceful HTTP shutdown (drains
in-flight requests for up to 10 seconds) before exiting.

Flags:
  --addr  Listen address (default: ENGRAM_ADDR env, then ":8080")
  --dsn   Postgres DSN (default: ENGRAM_DSN env; REQUIRED)
`

// runServeCmd parses the serve subcommand flags and delegates to runServe.
// It installs OS signal handling so SIGINT/SIGTERM trigger graceful shutdown.
func runServeCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(serveUsage) }

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

// runServe is the testable core of the serve subcommand. It opens the store,
// wires up the cloudserve server with real HMAC auth, logs the listen address,
// and blocks until ctx is cancelled (SIGINT/SIGTERM in production; test
// cancellation in acceptance tests). On context cancellation, cloudserve.Run
// performs a graceful HTTP shutdown before returning.
//
// TLS note: the server listens on plain HTTP. All TLS termination must happen
// at an upstream load balancer or reverse proxy. This is intentional: running
// the central server behind infrastructure-level TLS (e.g. an ALB with an ACM
// cert) keeps the Go binary stateless and simplifies cert rotation.
func runServe(ctx context.Context, addr, dsn string) error {
	store, err := centralstore.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	srv := cloudserve.New(store, cloudserve.NewKeyVerifier(store.WriterKey))

	log.Printf("engram serve: listening on %s (plain HTTP — terminate TLS upstream)", addr)
	return srv.Run(ctx, addr)
}
