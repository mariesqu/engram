package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/remote"
	"github.com/mariesqu/engram/internal/syncer"
	"github.com/mariesqu/engram/internal/wireauth"
)

const daemonUsage = `Usage: engram daemon [flags]

Run the engram local daemon — an MCP server (stdio transport) backed by a local
SQLite store.  The daemon exposes mem_session_start and mem_session_end in this
release; additional tools land in subsequent PRs.

When --central-url is set the daemon wires an autosync Loop that pushes local
writes to the central server and pulls remote mutations back on a periodic
schedule.  Pulls cover only projects already present in the local store, so a
fresh/empty database pulls nothing until a local write first creates a project.
Without --central-url the daemon runs in LOCAL-ONLY mode: no network traffic,
no HMAC credentials required.

On SIGINT or SIGTERM the daemon stops the autosync loop (if running), closes the
store, and exits cleanly.

Flags:
  --db              Path to the local SQLite database file (required; or set ENGRAM_DB)
  --central-url     Central server URL, e.g. http://localhost:8080 (optional; or set ENGRAM_CENTRAL_URL)
  --writer-id       Writer identity sent to the central server (required when --central-url is set; or set ENGRAM_WRITER_ID)
  --sync-interval   Autosync cadence (default: ENGRAM_SYNC_INTERVAL env, then 30s)

  ENGRAM_WRITER_KEY (env only — never a flag): hex-encoded 32-byte HMAC key.
    Required when --central-url is set.  Must never appear in flag defaults or
    --help output; setting it as a flag default would leak the secret via
    PrintDefaults.
`

// daemonCfg holds the validated, resolved configuration for the daemon.
type daemonCfg struct {
	db           string
	centralURL   string // empty → local-only mode
	writerID     string
	writerKey    []byte // nil → local-only mode
	syncInterval time.Duration
}

// daemonComponents holds the wired-but-not-yet-serving components built by
// buildDaemon. Callers must call Close to release resources.
type daemonComponents struct {
	store     *localstore.Store
	mcpServer *mcpserver.MCPServer
	loop      *syncer.Loop // nil when running in local-only mode
}

// Close stops the autosync loop (if any) and closes the local store.
func (d *daemonComponents) Close() {
	if d.loop != nil {
		d.loop.Stop()
	}
	if d.store != nil {
		_ = d.store.Close()
	}
}

// runDaemonCmd parses the daemon subcommand flags, resolves ENGRAM_WRITER_KEY
// from the environment (AFTER flag.Parse — never in a flag default, to avoid
// leaking the secret via --help / PrintDefaults), installs signal context, and
// delegates to runDaemon.
func runDaemonCmd(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), daemonUsage) }

	// --db: EMPTY default; resolved from ENGRAM_DB after Parse so the path is
	// never baked into flag metadata printed by --help.
	db := fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")

	// --central-url: optional.
	centralURL := fs.String("central-url", "", "central server URL (optional; or set ENGRAM_CENTRAL_URL)")

	// --writer-id: optional at parse time; required-iff-central-url-set validated below.
	writerID := fs.String("writer-id", "", "writer identity (required when --central-url is set; or set ENGRAM_WRITER_ID)")

	// --sync-interval: sensible default, overridable.
	syncInterval := fs.Duration("sync-interval", 0, "autosync cadence (default: ENGRAM_SYNC_INTERVAL env, then 30s)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // --help printed usage; successful early-exit (exit 0)
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("daemon takes no positional arguments; unexpected: %v", fs.Args())
	}

	// Resolve env vars AFTER parse so they never enter flag defaults and are
	// therefore never printed by PrintDefaults.
	if *db == "" {
		*db = envOr("ENGRAM_DB", "")
	}
	if *db == "" {
		return fmt.Errorf("--db is required (or set ENGRAM_DB)")
	}

	if *centralURL == "" {
		*centralURL = envOr("ENGRAM_CENTRAL_URL", "")
	}
	if *writerID == "" {
		*writerID = envOr("ENGRAM_WRITER_ID", "")
	}

	// ENGRAM_WRITER_KEY: env ONLY — resolved after Parse.  A 32-byte HMAC key
	// encoded as hex MUST NOT appear in flag defaults or --help output.
	var writerKey []byte
	if *centralURL != "" {
		if *writerID == "" {
			return fmt.Errorf("--writer-id is required when --central-url is set (or set ENGRAM_WRITER_ID)")
		}
		// TrimSpace: ENGRAM_WRITER_KEY is often injected from a file/CI secret with a
		// trailing newline, which would otherwise fail hex decoding.
		keyHex := strings.TrimSpace(os.Getenv("ENGRAM_WRITER_KEY"))
		if keyHex == "" {
			return fmt.Errorf("ENGRAM_WRITER_KEY env var is required when --central-url is set")
		}
		var err error
		writerKey, err = hex.DecodeString(keyHex)
		if err != nil {
			return fmt.Errorf("ENGRAM_WRITER_KEY is not valid hex: %w", err)
		}
		if len(writerKey) != wireauth.KeySize {
			return fmt.Errorf(
				"ENGRAM_WRITER_KEY has wrong length: got %d bytes, want %d",
				len(writerKey), wireauth.KeySize,
			)
		}
	}

	// Resolve sync interval: flag > env > default.
	if *syncInterval == 0 {
		if raw := envOr("ENGRAM_SYNC_INTERVAL", ""); raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("ENGRAM_SYNC_INTERVAL is not a valid duration %q: %w", raw, err)
			}
			*syncInterval = d
		}
	}
	if *syncInterval == 0 {
		*syncInterval = 30 * time.Second
	}
	if *syncInterval <= 0 {
		return fmt.Errorf("--sync-interval must be positive (got %s)", *syncInterval)
	}

	cfg := daemonCfg{
		db:           *db,
		centralURL:   *centralURL,
		writerID:     *writerID,
		writerKey:    writerKey,
		syncInterval: *syncInterval,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return runDaemon(ctx, cfg)
}

// buildDaemon opens the local store, constructs the MCP server, registers the
// MCP tools, and — when cfg.centralURL is non-empty — wires the signing remote
// client and an autosync Loop.  It does NOT serve or start the Loop; that is
// runDaemon's responsibility.  This factoring mirrors serve.go / runServe and
// allows unit tests to assert wiring correctness without blocking on stdio.
//
// Ordering: the Loop (if any) is constructed BEFORE registerTools so that write
// handlers can receive a non-nil loop and call loop.Trigger() immediately after
// a local write.  In local-only mode loop is nil and write handlers skip the
// trigger (triggerSync is nil-safe).
func buildDaemon(cfg daemonCfg) (*daemonComponents, error) {
	store, err := localstore.Open(cfg.db)
	if err != nil {
		return nil, fmt.Errorf("open store %q: %w", cfg.db, err)
	}

	mcpSrv := mcpserver.NewMCPServer(
		"engram",
		"0.1.0",
		mcpserver.WithToolCapabilities(true),
	)

	var loop *syncer.Loop

	if cfg.centralURL != "" {
		// Central mode: wire the signing remote client and autosync Loop BEFORE
		// registerTools so the loop reference is available to write handlers.
		central := remote.New(cfg.centralURL, nil, cfg.writerID, cfg.writerKey)
		node := syncer.NewNode("daemon", store)

		// The Loop drives SyncAllProjects per tick: Push once (outbox is
		// project-agnostic), then Pull each project using its own per-project
		// cursor. No project parameter is needed — projects are discovered via
		// localstore.ListProjects() at each tick.
		loop = syncer.NewLoop(node, central, syncer.Config{
			Interval: cfg.syncInterval,
		})
	}

	// Register all MCP tools. Pass loop (may be nil) so write handlers can
	// trigger an immediate sync after each local write.
	registerTools(mcpSrv, store, loop, cfg.writerID)

	return &daemonComponents{
		store:     store,
		mcpServer: mcpSrv,
		loop:      loop,
	}, nil
}

// runDaemon is the entry point called by runDaemonCmd.  It delegates to
// runDaemonWithIO with the real os.Stdin and os.Stdout so that the signal
// context (ctx) is the single shutdown source.
func runDaemon(ctx context.Context, cfg daemonCfg) error {
	return runDaemonWithIO(ctx, cfg, os.Stdin, os.Stdout)
}

// runDaemonWithIO is the testable core of the daemon subcommand.  It builds all
// components via buildDaemon, starts the autosync Loop (when present), then
// blocks on StdioServer.Listen until the context is cancelled (SIGINT/SIGTERM in
// production; test cancellation in tests) or stdin is closed (normal MCP client
// disconnect).  On return it stops the Loop and closes the store.
//
// Wiring ctx into Listen (rather than using ServeStdio, which creates its own
// internal context) makes the daemon's NotifyContext the SINGLE shutdown source:
// ctx cancellation causes Listen to return context.Canceled, which serveErr maps
// to a clean exit.
func runDaemonWithIO(ctx context.Context, cfg daemonCfg, stdin io.Reader, stdout io.Writer) error {
	components, err := buildDaemon(cfg)
	if err != nil {
		return err
	}
	defer components.Close()

	autosync := "off"
	if components.loop != nil {
		components.loop.Start(ctx)
		autosync = "on"
	}

	log.Printf("engram daemon: MCP over stdio (db=%s, autosync=%s)", cfg.db, autosync)

	return serveErr(mcpserver.NewStdioServer(components.mcpServer).Listen(ctx, stdin, stdout))
}

// serveErr classifies the error returned by StdioServer.Listen. Listen returns
// context.Canceled when the ctx passed to it is cancelled (SIGINT/SIGTERM in
// production, test cancel in tests) and nil on stdin EOF (normal MCP client
// disconnect). Both are SUCCESSFUL exits. Without this, a normal Ctrl-C / `kill`
// would exit non-zero and log a spurious "context canceled", which a process
// supervisor reads as a crash. Only a genuine I/O failure is surfaced.
func serveErr(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	return fmt.Errorf("daemon: serve: %w", err)
}
