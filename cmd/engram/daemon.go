package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/remote"
	"github.com/mariesqu/engram/internal/syncer"
	"github.com/mariesqu/engram/internal/wireauth"
)

const daemonUsage = `Usage: engram daemon [flags]

Run the engram local daemon — an MCP server (stdio transport) backed by a local
SQLite store.  The daemon exposes the full memory toolset: session lifecycle
(mem_session_start, mem_session_end, mem_session_summary), writes (mem_save,
mem_save_prompt), reads (mem_search, mem_context, mem_get_observation), and
conflict judgment (mem_judge).

When --central-url is set the daemon wires an autosync Loop that pushes local
writes to the central server and pulls remote mutations back on a periodic
schedule.  Pulls cover only projects already present in the local store, so a
fresh/empty database pulls nothing until a local write first creates a project.
Without --central-url the daemon runs in LOCAL-ONLY mode: no network traffic,
no HMAC credentials required.

When --http is set the daemon starts as a resident HTTP control plane instead of
serving stdio MCP. It binds to 127.0.0.1:<port> (default 7700) and writes a
daemon.json discovery file next to the database. CLI subcommands (engram status,
engram ui) read daemon.json to connect. A second daemon start on the same port
will probe the running daemon; if it is healthy it will refuse to start.

On SIGINT or SIGTERM the daemon stops the autosync loop (if running), closes the
store, and exits cleanly.  In HTTP mode daemon.json is removed on clean shutdown.

Flags:
  --db              Path to the local SQLite database file (required; or set ENGRAM_DB)
  --central-url     Central server URL, e.g. http://localhost:8080 (optional; or set ENGRAM_CENTRAL_URL)
  --writer-id       Writer identity sent to the central server (required when --central-url is set; or set ENGRAM_WRITER_ID)
  --sync-interval   Autosync cadence (default: ENGRAM_SYNC_INTERVAL env, then 30s)
  --http            Enable resident HTTP control plane (default: false — stdio MCP mode)
  --http-port       TCP port for the HTTP control plane (default: 7700)

  ENGRAM_WRITER_KEY (env only — never a flag): hex-encoded 32-byte HMAC key.
    Required when --central-url is set.  Must never appear in flag defaults or
    --help output; setting it as a flag default would leak the secret via
    PrintDefaults.
`

// daemonCfg holds the validated, resolved configuration for the daemon.
type daemonCfg struct {
	db         string
	centralURL string // empty → local-only mode
	writerID   string
	writerKey  []byte // nil → local-only mode
	// HTTP resident-mode flags (added in PR-①).
	httpMode     bool // true → bind control API instead of stdio MCP
	httpPort     int  // TCP port for the control API (default 7700)
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

	// --http / --http-port: opt-in resident mode (PR-①).
	// NOTE: --http is intentionally NOT wired to ENGRAM_WRITER_KEY-style resolution
	// because it is a mode flag, not a secret. It appears in --help (flag default "false").
	httpMode := fs.Bool("http", false, "enable resident HTTP control plane (default: stdio MCP mode)")
	httpPort := fs.Int("http-port", 7700, "TCP port for the HTTP control plane")

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
		httpMode:     *httpMode,
		httpPort:     *httpPort,
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
		daemonVersion,
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

	// Create in-memory session activity tracker. Shared across all write handlers
	// so mem_save_prompt can record the current prompt and mem_save can auto-capture it.
	activity := NewSessionActivity()

	// Register all MCP tools. Pass loop (may be nil) so write handlers can
	// trigger an immediate sync after each local write.
	registerTools(mcpSrv, store, loop, cfg.writerID, activity)

	return &daemonComponents{
		store:     store,
		mcpServer: mcpSrv,
		loop:      loop,
	}, nil
}

// runDaemon is the entry point called by runDaemonCmd. It dispatches to:
//   - runDaemonHTTP when cfg.httpMode is true (resident control plane, PR-①)
//   - runDaemonWithIO otherwise (stdio MCP mode — the pre-existing path)
func runDaemon(ctx context.Context, cfg daemonCfg) error {
	if cfg.httpMode {
		return runDaemonHTTP(ctx, cfg)
	}
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

// daemonVersion is the single source of truth for the version reported by the
// MCP server and the control API. Build-time injection via -ldflags is a
// release-process follow-up; until then bump it here.
const daemonVersion = "0.1.0"

// ── HTTP resident-mode (PR-①) ─────────────────────────────────────────────────

// runDaemonHTTP starts the resident daemon in HTTP control-plane mode.
//
// It:
//  1. Opens the local store and wires the autosync loop (same as stdio mode).
//  2. Generates a 32-byte-hex bearer token and writes daemon.json atomically.
//  3. Binds 127.0.0.1:<port> — if the port is already in use it probes the
//     existing process; a healthy engram daemon → refuse with a clear error.
//  4. Serves the control API until ctx is cancelled (SIGINT/SIGTERM).
//  5. On clean shutdown, removes daemon.json.
//
// stdio MCP is NOT started in this mode — the HTTP control plane owns the process.
func runDaemonHTTP(ctx context.Context, cfg daemonCfg) error {
	components, err := buildDaemon(cfg)
	if err != nil {
		return err
	}
	defer components.Close()

	// Generate a fresh 32-byte (64 hex char) token. Token rotates on every start.
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return fmt.Errorf("daemon HTTP: generate token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)

	// The discovery file lives next to the database.
	dir := filepath.Dir(cfg.db)

	// Attempt to bind before writing daemon.json so we never write a stale
	// file if the port is already in use.
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.httpPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Port in use — probe the existing occupant.
		if probeErr := probeDaemon(dir, cfg.httpPort); probeErr == nil {
			return fmt.Errorf("daemon already running on :%d (probe succeeded); refusing to start a second SQLite owner", cfg.httpPort)
		}
		return fmt.Errorf("daemon HTTP: listen %s: %w", addr, err)
	}
	defer ln.Close()

	// Resolve the ACTUAL bound port: with --http-port 0 the OS assigns an
	// ephemeral port, and daemon.json / the Origin allowlist must record that
	// real port or clients would dial port 0.
	actualPort := ln.Addr().(*net.TCPAddr).Port

	// Write daemon.json AFTER successful bind so clients that read the file can
	// immediately connect. The write is atomic (temp+rename) inside WriteDaemonJSON.
	if err := controlapi.WriteDaemonJSON(dir, token, actualPort, os.Getpid()); err != nil {
		return fmt.Errorf("daemon HTTP: write daemon.json: %w", err)
	}

	// Start the autosync loop only after every step that can fail (bind,
	// discovery write) has succeeded — a startup failure must not have to wait
	// for a mid-flight sync cycle to stop.
	if components.loop != nil {
		components.loop.Start(ctx)
	}
	defer func() {
		// Remove daemon.json on clean shutdown so stale discovery files are not left behind.
		_ = controlapi.RemoveDaemonJSON(dir)
	}()

	// Wire the control API server. The store adapter bridges localstore.Store to
	// the controlapi.Store interface.
	storeAdapter := &localStoreAdapter{store: components.store}
	syncAdapter := &loopSyncAdapter{
		loop:       components.loop,
		centralURL: cfg.centralURL,
	}
	cfgAdapter := &daemonCfgAdapter{cfg: cfg}

	ctrlSrv := controlapi.New(token, actualPort, storeAdapter, syncAdapter, cfgAdapter, daemonVersion)

	httpSrv := &http.Server{
		Handler:           ctrlSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("engram daemon: HTTP control plane on 127.0.0.1:%d (db=%s)", actualPort, cfg.db)

	errCh := make(chan error, 1)
	go func() {
		if serveErr := httpSrv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	}
}

// probeDaemon probes an existing daemon on the given port using the token from
// daemon.json in dir. Returns nil ONLY when the occupant is verifiably a
// healthy engram daemon: a 200 JSON status response with daemon_version set.
//
// Decision matrix (caller refuses startup on nil, falls back to the raw bind
// error otherwise — both paths fail to start, only the message differs):
//   - daemon.json records a DIFFERENT port → the daemon that wrote it is not
//     the occupant of this port; skip the probe (foreign port conflict).
//   - 200 with daemon_version → engram daemon confirmed → refuse second owner.
//   - 200 without daemon_version → some foreign HTTP server → bind error.
//   - 401 → the occupant rejects the recorded token. A LIVE engram daemon
//     rewrites daemon.json with its current token on start, so a mismatch
//     means the file is stale relative to the occupant → treat as foreign.
func probeDaemon(dir string, port int) error {
	d, err := controlapi.ReadDaemonJSON(dir)
	if err != nil {
		return fmt.Errorf("read daemon.json: %w", err)
	}
	if d.Port != port {
		return fmt.Errorf("probe: daemon.json records port %d, not %d (foreign process on the contested port)", d.Port, port)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/api/v1/status", port), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+d.Token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe: got %d", resp.StatusCode)
	}
	var st controlapi.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil || st.DaemonVersion == "" {
		return fmt.Errorf("probe: 200 but not an engram status response")
	}
	return nil
}

// ── Port adapters (wire localstore and syncer to controlapi interfaces) ───────

// localStoreAdapter adapts *localstore.Store to controlapi.Store.
type localStoreAdapter struct {
	store *localstore.Store
}

func (a *localStoreAdapter) ListProjectsWithPolicy() ([]controlapi.ProjectPolicy, error) {
	lpp, err := a.store.ListProjectsWithPolicy()
	if err != nil {
		return nil, err
	}
	out := make([]controlapi.ProjectPolicy, len(lpp))
	for i, p := range lpp {
		out[i] = controlapi.ProjectPolicy{
			Name:   p.Name,
			Policy: controlapi.Policy(p.Policy),
		}
	}
	return out, nil
}

func (a *localStoreAdapter) SetPolicy(project string, p controlapi.Policy) error {
	return a.store.SetPolicy(project, localstore.Policy(p))
}

func (a *localStoreAdapter) GetPolicy(project string) (controlapi.Policy, error) {
	p, err := a.store.GetPolicy(project)
	return controlapi.Policy(p), err
}

// loopSyncAdapter adapts *syncer.Loop to controlapi.SyncController.
// In PR-① this is a minimal stub; PR-③ will extend it with full sync state.
type loopSyncAdapter struct {
	loop       *syncer.Loop // nil → local-only mode
	centralURL string
}

func (a *loopSyncAdapter) Status() controlapi.Status {
	var url *string
	if a.centralURL != "" {
		u := a.centralURL
		url = &u
	}
	return controlapi.Status{
		CentralConnected: a.centralURL != "" && a.loop != nil,
		CentralURL:       url,
		LastSyncResult:   controlapi.SyncResult{},
		DaemonVersion:    daemonVersion,
	}
}

func (a *loopSyncAdapter) TriggerNow(_ context.Context) error {
	if a.loop != nil {
		a.loop.Trigger()
	}
	return nil
}

func (a *loopSyncAdapter) Disconnect() error {
	// PR-③ will implement full disconnect. For PR-① this is a stub.
	return fmt.Errorf("disconnect not implemented until PR-③")
}

func (a *loopSyncAdapter) Reconnect(_ controlapi.CentralConfig) error {
	// PR-③ will implement reconnect.
	return fmt.Errorf("reconnect not implemented until PR-③")
}

// daemonCfgAdapter adapts the daemon config to controlapi.ConfigStore.
// PR-③ will replace this with a real internal/config.Store adapter.
type daemonCfgAdapter struct {
	cfg daemonCfg
}

func (a *daemonCfgAdapter) Load() (controlapi.RedactedConfig, error) {
	cfg := controlapi.RedactedConfig{
		DB: a.cfg.db,
		HTTP: &controlapi.HTTPConfig{
			Port: a.cfg.httpPort,
		},
		SyncInterval: a.cfg.syncInterval.String(),
	}
	if a.cfg.centralURL != "" {
		cfg.Central = &controlapi.CentralConfig{
			URL:      a.cfg.centralURL,
			WriterID: a.cfg.writerID,
		}
	}
	if len(a.cfg.writerKey) > 0 {
		redacted := "***REDACTED***"
		cfg.WriterKey = &redacted
	}
	return cfg, nil
}

func (a *daemonCfgAdapter) Apply(_ controlapi.ConfigPatch) (bool, error) {
	// PR-③ will implement config mutation.
	return false, fmt.Errorf("config mutation not implemented until PR-③")
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
