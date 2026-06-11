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
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mariesqu/engram/internal/config"
	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/remote"
	"github.com/mariesqu/engram/internal/syncer"
	"github.com/mariesqu/engram/internal/webui"
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

When --http is set together with --transport http, the daemon ALSO mounts an MCP
Streamable HTTP server at /mcp on the same listener. MCP clients can connect to
http://127.0.0.1:<port>/mcp using the bearer token from daemon.json as an
Authorization header.  stdio MCP remains the default transport (--transport stdio).

On SIGINT or SIGTERM the daemon stops the autosync loop (if running), closes the
store, and exits cleanly.  In HTTP mode daemon.json is removed on clean shutdown.

Config file: %APPDATA%\engram\config.json (Windows) or $XDG_CONFIG_HOME/engram/config.json.
Precedence: flags > env vars > config file > defaults.
Writer key is DPAPI-encrypted at rest on Windows. Use ENGRAM_WRITER_KEY env var on other platforms.

Flags:
  --db              Path to the local SQLite database file (required; or set ENGRAM_DB)
  --central-url     Central server URL, e.g. http://localhost:8080 (optional; or set ENGRAM_CENTRAL_URL)
  --writer-id       Writer identity sent to the central server (required when --central-url is set; or set ENGRAM_WRITER_ID)
  --sync-interval   Autosync cadence (default: ENGRAM_SYNC_INTERVAL env, then config file, then 30s)
  --http            Enable resident HTTP control plane (default: false — stdio MCP mode)
  --http-port       TCP port for the HTTP control plane (default: 7700)
  --transport       MCP transport: "stdio" (default) or "http" (mounts /mcp on the HTTP listener; requires --http)

  ENGRAM_WRITER_KEY (env only — never a flag): hex-encoded 32-byte HMAC key.
    Required when --central-url is set.  Must never appear in flag defaults or
    --help output; setting it as a flag default would leak the secret via
    PrintDefaults.
`

// daemonCfg holds the validated, resolved configuration for the daemon.
// PR-③: configDir is the directory where config.json lives. The config file
// is consulted as a lower-precedence source than flags and env vars.
type daemonCfg struct {
	db         string
	centralURL string // empty → local-only mode
	writerID   string
	writerKey  []byte // nil → local-only mode (decrypted, in-memory only)
	// HTTP resident-mode flags (added in PR-①).
	httpMode     bool // true → bind control API instead of stdio MCP
	httpPort     int  // TCP port for the control API (default 7700)
	syncInterval time.Duration
	// PR-③: config file directory (same as DB dir by default, or os.UserConfigDir()/engram).
	configDir string
	// PR-⑥: MCP transport selection. "stdio" (default) | "http".
	// When "http" and httpMode=true, /mcp is mounted on the top-level ServeMux.
	mcpTransport string

	// Embedding provider fields.
	// embeddingProvider is the validated provider name ("", "none", "openai", "ollama").
	// embeddingKey is the plaintext API key resolved at startup (in-memory only;
	// NEVER written to disk, never logged). nil/empty → noop provider.
	embeddingProvider     string
	embeddingKey          []byte // plaintext; nil when not needed
	embeddingLocalConsent bool   // PR-2: explicit consent for local-only projects with sidecar
	embeddingDims         int    // 0 → provider default (256)
	ollamaHost            string // "" → "http://localhost:11434"
	ollamaModel           string // "" → "nomic-embed-text"
	embeddingBaseURL      string // "" → https://api.openai.com
	embeddingModel        string // "" → text-embedding-3-small
	embeddingAuthHeader   string // "" or "authorization" → Bearer; "api-key" → api-key header
}

// resolveTransport resolves the MCP transport with the standard precedence
// chain: explicit flag > ENGRAM_TRANSPORT env > config file > default "stdio".
// flagVal is "" when --transport was not passed (the flag default is empty so
// an EXPLICIT --transport stdio beats an env/file "http"). Any resolved value
// outside {stdio, http} is a hard startup error — including a bad value coming
// from the config file.
func resolveTransport(flagVal, envVal, fileVal string) (string, error) {
	v := flagVal
	if v == "" {
		v = envVal
	}
	if v == "" {
		v = fileVal
	}
	if v == "" {
		v = "stdio"
	}
	switch v {
	case "stdio", "http":
		return v, nil
	default:
		return "", fmt.Errorf("transport: unknown value %q (must be \"stdio\" or \"http\")", v)
	}
}

// buildOpenAIOpts converts the embedding-related fields of daemonCfg into
// Option values for NewRemoteOpenAI. Separated to keep buildDaemon readable.
func buildOpenAIOpts(cfg daemonCfg) []embedding.Option {
	var opts []embedding.Option
	if cfg.embeddingBaseURL != "" {
		opts = append(opts, embedding.WithBaseURL(cfg.embeddingBaseURL))
	}
	if cfg.embeddingModel != "" {
		opts = append(opts, embedding.WithModel(cfg.embeddingModel))
	}
	if cfg.embeddingDims != 0 {
		opts = append(opts, embedding.WithDims(cfg.embeddingDims))
	}
	switch cfg.embeddingAuthHeader {
	case "api-key":
		opts = append(opts, embedding.WithAuthHeader(embedding.AuthHeaderAPIKey))
	default:
		// "" or "authorization" → default Bearer (no option needed; it is the zero value)
	}
	return opts
}

// daemonComponents holds the wired-but-not-yet-serving components built by
// buildDaemon. Callers must call Close to release resources.
type daemonComponents struct {
	store     *localstore.Store
	mcpServer *mcpserver.MCPServer
	loop      *syncer.Loop                // nil when running in local-only mode
	embedLoop *embedding.Loop             // nil when provider is Noop or key absent
	gated     embedding.EmbeddingProvider // always non-nil (at least NoopProvider via gate)
}

// Close stops the embedding backfill loop, the autosync loop, and the store —
// in that order so no in-flight UpdateEmbedding can race the store close.
func (d *daemonComponents) Close() {
	// embedding.Loop.Stop() is nil-safe and blocks until the goroutine exits.
	d.embedLoop.Stop()
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
//
// PR-③: Config file is loaded here with lowest precedence (flags > env > file > defaults).
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
	httpMode := fs.Bool("http", false, "enable resident HTTP control plane (default: stdio MCP mode)")
	httpPort := fs.Int("http-port", 0, "TCP port for the HTTP control plane (default: 7700, or config file)")

	// --transport: MCP transport selector (PR-⑥). EMPTY default so an explicit
	// --transport stdio is distinguishable from "not set" and wins the
	// precedence chain (flag > ENGRAM_TRANSPORT env > config file > "stdio");
	// resolveTransport applies the chain + validation after Parse.
	mcpTransport := fs.String("transport", "", `MCP transport: "stdio" (default) or "http" (requires --http; or set ENGRAM_TRANSPORT / config file)`)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // --help printed usage; successful early-exit (exit 0)
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("daemon takes no positional arguments; unexpected: %v", fs.Args())
	}

	// ── Determine config file directory ──────────────────────────────────────
	// Default config dir is os.UserConfigDir()/engram. Use this as the lowest-
	// precedence source. If it fails (no home dir) we proceed without a config file.
	configDir, _ := config.DefaultConfigDir() // "" on error → Load returns zero Config

	// ── Load config file (lowest precedence) ─────────────────────────────────
	var fileCfg config.Config
	if configDir != "" {
		var loadErr error
		fileCfg, loadErr = config.Load(configDir)
		if loadErr != nil {
			// An invalid embedding_provider (or any other enum/parse error) is a
			// hard startup failure. A misconfigured value that silently falls back to
			// noop would hide a configuration error — surface it immediately.
			// (Missing file: Load returns (Config{}, nil) — never reaches here.)
			return fmt.Errorf("daemon: config file error: %w", loadErr)
		}
	}

	// ── Resolve DB path: flag > env > file ───────────────────────────────────
	if *db == "" {
		*db = envOr("ENGRAM_DB", "")
	}
	if *db == "" && fileCfg.DB != "" {
		*db = fileCfg.DB
	}
	if *db == "" {
		return fmt.Errorf("--db is required (or set ENGRAM_DB)")
	}

	// ── Resolve central URL: flag > env > file ───────────────────────────────
	if *centralURL == "" {
		*centralURL = envOr("ENGRAM_CENTRAL_URL", "")
	}
	if *centralURL == "" && fileCfg.CentralURL != "" {
		*centralURL = fileCfg.CentralURL
	}

	// ── Resolve writer ID: flag > env > file ─────────────────────────────────
	if *writerID == "" {
		*writerID = envOr("ENGRAM_WRITER_ID", "")
	}
	if *writerID == "" && fileCfg.WriterID != "" {
		*writerID = fileCfg.WriterID
	}

	// ── Resolve HTTP port: flag > file > default ─────────────────────────────
	if *httpPort == 0 && fileCfg.HTTPPort > 0 {
		*httpPort = fileCfg.HTTPPort
	}
	if *httpPort == 0 {
		*httpPort = 7700
	}

	// ── Resolve writer key: ENGRAM_WRITER_KEY env always wins over file ───────
	//
	// ENGRAM_WRITER_KEY env ALWAYS wins over any value stored in the config file,
	// including on Windows where DPAPI is available. This is a hard constraint
	// documented in the spec.
	var writerKey []byte
	if *centralURL != "" {
		if *writerID == "" {
			return fmt.Errorf("--writer-id is required when --central-url is set (or set ENGRAM_WRITER_ID)")
		}

		keyHex := strings.TrimSpace(os.Getenv("ENGRAM_WRITER_KEY"))
		if keyHex != "" {
			// Env var wins — decode and use it directly.
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
		} else if len(fileCfg.EncryptedWriterKey) > 0 {
			// No env var — try to decrypt the config file blob.
			secretBox := config.NewSecretBox()
			var decryptErr error
			writerKey, decryptErr = secretBox.Open(fileCfg.EncryptedWriterKey)
			if decryptErr != nil {
				// Decrypt failure: log a warning, fall back to "no key".
				// The daemon starts in local-only mode; the status endpoint will
				// report "writer key required" so the UI can prompt a re-enter.
				// This is the design-mandated behavior: never crash on decrypt failure.
				log.Printf("warning: DPAPI decrypt failed for stored writer key (user/machine may have changed): %v", decryptErr)
				log.Printf("  → daemon will start in local-only mode; reconnect via the UI or set ENGRAM_WRITER_KEY")
				writerKey = nil
				*centralURL = "" // fall back to local-only since we have no key
			}
		} else {
			return fmt.Errorf("ENGRAM_WRITER_KEY env var is required when --central-url is set (or store it via engram central connect)")
		}
	}

	// ── Resolve sync interval: flag > env > file > default ───────────────────
	if *syncInterval == 0 {
		if raw := envOr("ENGRAM_SYNC_INTERVAL", ""); raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("ENGRAM_SYNC_INTERVAL is not a valid duration %q: %w", raw, err)
			}
			*syncInterval = d
		}
	}
	if *syncInterval == 0 && fileCfg.SyncInterval > 0 {
		*syncInterval = fileCfg.SyncInterval
	}
	if *syncInterval == 0 {
		*syncInterval = 30 * time.Second
	}
	if *syncInterval <= 0 {
		return fmt.Errorf("--sync-interval must be positive (got %s)", *syncInterval)
	}

	// ── Resolve + validate transport (flag > env > file > default) ───────────
	transport, err := resolveTransport(*mcpTransport, envOr("ENGRAM_TRANSPORT", ""), fileCfg.Transport)
	if err != nil {
		return err
	}
	if transport == "http" && !*httpMode {
		return fmt.Errorf("--transport http requires --http (the HTTP control plane must be enabled)")
	}

	// ── Resolve embedding key: ENGRAM_EMBEDDING_KEY env always wins ──────────
	//
	// ENGRAM_EMBEDDING_KEY env ALWAYS wins over any stored ciphertext — same
	// precedence contract as ENGRAM_WRITER_KEY. The key is hex-encoded.
	// When neither source provides a key, the daemon starts with the Noop provider
	// regardless of embedding_provider — no error, just no embedding capability.
	embeddingProvider := fileCfg.EmbeddingProvider
	var embeddingKey []byte
	if embeddingKeyHex := strings.TrimSpace(os.Getenv("ENGRAM_EMBEDDING_KEY")); embeddingKeyHex != "" {
		// Env var wins — decode and use directly. Invalid hex is a startup error.
		decoded, err := hex.DecodeString(embeddingKeyHex)
		if err != nil {
			return fmt.Errorf("ENGRAM_EMBEDDING_KEY is not valid hex: %w", err)
		}
		embeddingKey = decoded
	} else if blob := fileCfg.EncryptedEmbeddingKey(); len(blob) > 0 {
		// No env var — attempt to decrypt the stored ciphertext.
		secretBox := config.NewSecretBox()
		plaintext, decryptErr := secretBox.Open(blob)
		if decryptErr != nil {
			// Decrypt failure is non-fatal for embedding: log a warning and fall
			// back to Noop. Embedding is optional; a bad key should not prevent
			// the daemon from starting — the user can still use FTS search.
			log.Printf("warning: DPAPI decrypt failed for embedding key: %v", decryptErr)
			log.Printf("  → embedding will use Noop provider; set ENGRAM_EMBEDDING_KEY or re-configure via the UI")
			embeddingKey = nil
		} else {
			embeddingKey = plaintext
		}
	}

	cfg := daemonCfg{
		db:                    *db,
		centralURL:            *centralURL,
		writerID:              *writerID,
		writerKey:             writerKey,
		syncInterval:          *syncInterval,
		httpMode:              *httpMode,
		httpPort:              *httpPort,
		configDir:             configDir,
		mcpTransport:          transport,
		embeddingProvider:     embeddingProvider,
		embeddingKey:          embeddingKey,
		embeddingLocalConsent: fileCfg.EmbeddingLocalConsent,
		embeddingDims:         fileCfg.EmbeddingDims,
		ollamaHost:            fileCfg.OllamaHost,
		ollamaModel:           fileCfg.OllamaModel,
		embeddingBaseURL:      fileCfg.EmbeddingBaseURL,
		embeddingModel:        fileCfg.EmbeddingModel,
		embeddingAuthHeader:   fileCfg.EmbeddingAuthHeader,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return runDaemon(ctx, cfg)
}

// buildDaemon opens the local store, constructs the MCP server, registers the
// MCP tools, and — when cfg.centralURL is non-empty — wires the signing remote
// client and an autosync Loop.  It does NOT serve or start the Loop; that is
// runDaemon's responsibility.
func buildDaemon(cfg daemonCfg) (*daemonComponents, error) {
	store, err := localstore.Open(cfg.db)
	if err != nil {
		return nil, fmt.Errorf("open store %q: %w", cfg.db, err)
	}

	mcpSrv := mcpserver.NewMCPServer(
		"engram",
		version,
		mcpserver.WithToolCapabilities(true),
	)

	var loop *syncer.Loop

	if cfg.centralURL != "" {
		central := remote.New(cfg.centralURL, nil, cfg.writerID, cfg.writerKey)
		node := syncer.NewNode("daemon", store)
		loop = syncer.NewLoop(node, central, syncer.Config{
			Interval: cfg.syncInterval,
		})
	}

	// Wire the central-configured closure for policy default computation.
	centralURL := cfg.centralURL
	store.SetCentralConfiguredFn(func() bool { return centralURL != "" })

	// ── Wire embedding provider ───────────────────────────────────────────────
	// Build the inner raw provider (OpenAI, Ollama, or Noop), then wrap it with
	// the privacy gate. The gate is the ONLY path to the inner provider — raw
	// providers never escape buildDaemon. store satisfies PolicyChecker.
	//
	// remote=true for OpenAI (text leaves the node).
	// remote=false for Ollama (local sidecar — text stays on the node).
	// consent=cfg.embeddingLocalConsent is passed to the gate for local providers.
	var innerProvider embedding.EmbeddingProvider
	isRemote := false
	switch cfg.embeddingProvider {
	case "openai":
		isRemote = true
		// A custom base URL is still remote: text leaves the machine regardless of
		// which endpoint it is sent to. The gate posture is unchanged.
		if len(cfg.embeddingKey) > 0 {
			opts := buildOpenAIOpts(cfg)
			innerProvider = embedding.NewRemoteOpenAI(string(cfg.embeddingKey), opts...)
		} else {
			// Provider is configured but key is not available — log a warning and
			// fall back to Noop. This is not a fatal error.
			log.Printf("warning: embedding_provider=openai but no key found (ENGRAM_EMBEDDING_KEY not set and no stored key); falling back to noop")
			innerProvider = embedding.NoopProvider{}
		}
	case "ollama":
		// Ollama is a local sidecar — remote=false. The gate requires
		// embedding_local_consent=true for local-only projects.
		ollamaHost := cfg.ollamaHost
		if ollamaHost == "" {
			ollamaHost = "http://localhost:11434"
		}
		ollamaModel := cfg.ollamaModel
		if ollamaModel == "" {
			ollamaModel = "nomic-embed-text"
		}
		dims := cfg.embeddingDims
		innerProvider = embedding.NewOllamaSidecar(ollamaModel, dims, embedding.WithOllamaHost(ollamaHost))
	default:
		// "", "none", or any value that passed validation → Noop.
		innerProvider = embedding.NoopProvider{}
	}
	gated := embedding.NewGated(innerProvider, store, isRemote, cfg.embeddingLocalConsent)
	store.SetEmbedFn(gated.Embed, gated.Dimensions())

	// ── Construct embedding backfill loop (PR-①b) ─────────────────────────────
	// The embedLoop is non-nil only when the provider is not Noop (i.e., a real
	// key+provider is active). A nil embedLoop means the feature is inert: Trigger()
	// and Stop() on a nil *embedding.Loop are no-ops (nil-safe by design).
	var embedLoop *embedding.Loop
	if _, isNoop := innerProvider.(embedding.NoopProvider); !isNoop {
		embedLoop = embedding.NewLoop(store, gated, embedding.LoopConfig{
			// Use the same sync interval for embedding as for sync, capped to 60s min.
			// Production default is 60s; tests override via Config fields.
			Interval:   60 * time.Second,
			BatchPause: 1 * time.Second, // rate-limit guard: 1s between batch Embed calls
		})
	}

	activity := NewSessionActivity()
	registerTools(mcpSrv, store, loop, embedLoop, gated, cfg.writerID, activity)

	return &daemonComponents{
		store:     store,
		mcpServer: mcpSrv,
		loop:      loop,
		embedLoop: embedLoop,
		gated:     gated,
	}, nil
}

// runDaemon dispatches to runDaemonHTTP or runDaemonWithIO.
func runDaemon(ctx context.Context, cfg daemonCfg) error {
	if cfg.httpMode {
		return runDaemonHTTP(ctx, cfg)
	}
	return runDaemonWithIO(ctx, cfg, os.Stdin, os.Stdout)
}

// runDaemonWithIO is the testable core of the stdio MCP daemon.
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

	// Start the embedding backfill loop alongside the autosync loop.
	// embedLoop is nil when provider is Noop — Start on nil panics, so guard.
	if components.embedLoop != nil {
		components.embedLoop.Start(ctx)
	}

	log.Printf("engram daemon: MCP over stdio (db=%s, autosync=%s)", cfg.db, autosync)

	return serveErr(mcpserver.NewStdioServer(components.mcpServer).Listen(ctx, stdin, stdout))
}

// ── HTTP resident-mode (PR-①, extended by PR-③) ──────────────────────────────

// runDaemonHTTP starts the resident daemon in HTTP control-plane mode.
func runDaemonHTTP(ctx context.Context, cfg daemonCfg) error {
	components, err := buildDaemon(cfg)
	if err != nil {
		return err
	}
	defer components.Close()

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return fmt.Errorf("daemon HTTP: generate token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)

	dir := filepath.Dir(cfg.db)

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.httpPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if probeErr := probeDaemon(dir, cfg.httpPort); probeErr == nil {
			return fmt.Errorf("daemon already running on :%d (probe succeeded); refusing to start a second SQLite owner", cfg.httpPort)
		}
		return fmt.Errorf("daemon HTTP: listen %s: %w", addr, err)
	}
	defer ln.Close()

	// Resolve the ACTUAL bound port (important when --http-port 0).
	actualPort := ln.Addr().(*net.TCPAddr).Port

	if err := controlapi.WriteDaemonJSON(dir, token, actualPort, os.Getpid()); err != nil {
		return fmt.Errorf("daemon HTTP: write daemon.json: %w", err)
	}

	if components.loop != nil {
		components.loop.Start(ctx)
	}
	// Start the embedding backfill loop alongside the autosync loop.
	if components.embedLoop != nil {
		components.embedLoop.Start(ctx)
	}
	defer func() {
		_ = controlapi.RemoveDaemonJSON(dir)
	}()

	// ── PR-③: wire the full runtime adapters ─────────────────────────────────
	storeAdapter := &localStoreAdapter{store: components.store}

	// configStoreAdapter wraps internal/config for the ConfigStore port.
	// actualPort: report the bound port (not the pre-bind flag, e.g. 0).
	cfgAdapter := newConfigStoreAdapter(cfg, actualPort)

	// runtimeSyncAdapter replaces the PR-① loopSyncAdapter with a real
	// runtime-mutable adapter that supports Disconnect and Reconnect.
	// ctx: the daemon's root signal context — runtime-created loops are started
	// on it so daemon shutdown also stops a loop created via /central/connect.
	syncAdapter := newRuntimeSyncAdapter(
		ctx,
		cfg,
		components.store,
		components.loop,
		cfgAdapter,
		actualPort,
		components.gated,
	)

	ctrlSrv := controlapi.New(token, actualPort, storeAdapter, syncAdapter, cfgAdapter, version, cfgAdapter)

	// ── PR-④a: build the top-level mux (one listener, path-routed) ───────────
	// The control API and the web UI share a single net.Listener. We mount:
	//   /api/v1/…  → controlapi.Handler() (bearer-token auth)
	//   /ui/…      → webui.Mount (session-cookie auth, token→cookie exchange)
	//   /mcp       → StreamableHTTPServer (opt-in, --transport http only, PR-⑥)
	topMux := http.NewServeMux()

	// Mount all /api/v1/ routes from the control API handler.
	// We re-register each route from the controlapi mux rather than mounting the
	// mux as a sub-handler, so that the top-level mux has the exact same routing
	// behaviour. The simplest approach: let controlapi.Handler() own /api/v1/ and
	// register the webui on /ui/ — both on the SAME top-level mux.
	ctrlHandler := ctrlSrv.Handler()
	topMux.Handle("/api/", ctrlHandler)

	// Mount the web UI on the same mux and listener.
	webui.Mount(topMux, webui.WebUIDeps{
		SyncCtrl: syncAdapter,
		Store:    storeAdapter,
		Secret:   token,
		Port:     actualPort,
		Version:  version,
	})

	// ── PR-⑥: opt-in MCP HTTP transport ──────────────────────────────────────
	// When --transport http is set, mount the Streamable HTTP MCP server at /mcp
	// on the SAME top-level mux (same loopback listener, same port — no new port).
	// STATELESS mode: no server-side session state for a single-user loopback daemon.
	// Auth: MountMCP wraps the handler with the same bearer-token check as /api/v1/*.
	if cfg.mcpTransport == "http" {
		streamableHandler := mcpserver.NewStreamableHTTPServer(
			components.mcpServer,
			mcpserver.WithStateLess(true),
		)
		controlapi.MountMCP(topMux, token, streamableHandler)
		log.Printf("engram daemon: MCP HTTP transport mounted at /mcp (stateless, bearer-token auth)")
	}

	httpSrv := &http.Server{
		Handler:           topMux,
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

// probeDaemon probes an existing daemon on the given port.
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

// ── Port adapters ─────────────────────────────────────────────────────────────

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

// ── configStoreAdapter (PR-③) ─────────────────────────────────────────────────

// configStoreAdapter adapts internal/config.Config to controlapi.ConfigStore.
// It holds the live resolved Config and persists changes via config.Save.
// Apply calls back into the runtimeSyncAdapter (via the applyCb closure) for
// runtime-mutable fields like SyncInterval so the loop interval is updated live.
type configStoreAdapter struct {
	mu        sync.RWMutex
	cfg       config.Config // live resolved config
	configDir string
	secretBox config.SecretBox
	// applyCb is called by Apply for runtime-mutable changes (e.g. SyncInterval).
	// It is wired to runtimeSyncAdapter.applyLiveConfig after construction.
	applyCb func(patch controlapi.ConfigPatch, updated config.Config)
}

func newConfigStoreAdapter(daemonCfg daemonCfg, actualPort int) *configStoreAdapter {
	// Reconstruct a config.Config from the resolved daemonCfg so Load() reports
	// the actual live values — actualPort is the BOUND port from ln.Addr(), not
	// the pre-bind flag value (which is 0 under --http-port 0).
	httpPort := daemonCfg.httpPort
	if actualPort > 0 {
		httpPort = actualPort
	}
	cfg := config.Config{
		DB:           daemonCfg.db,
		CentralURL:   daemonCfg.centralURL,
		WriterID:     daemonCfg.writerID,
		HTTPPort:     httpPort,
		SyncInterval: daemonCfg.syncInterval,
	}
	// Re-load the file to recover encrypted blobs and fields not in daemonCfg.
	if daemonCfg.configDir != "" {
		if fileCfg, err := config.Load(daemonCfg.configDir); err == nil {
			cfg.EncryptedWriterKey = fileCfg.EncryptedWriterKey
			cfg = cfg.WithEncryptedEmbeddingKey(fileCfg.EncryptedEmbeddingKey())
			cfg.EmbeddingProvider = fileCfg.EmbeddingProvider
			cfg.EmbeddingLocalConsent = fileCfg.EmbeddingLocalConsent
			cfg.EmbeddingDims = fileCfg.EmbeddingDims
			cfg.OllamaHost = fileCfg.OllamaHost
			if cfg.LogLevel == "" {
				cfg.LogLevel = fileCfg.LogLevel
			}
			if cfg.Transport == "" {
				cfg.Transport = fileCfg.Transport
			}
		}
	}
	// Mark the key active when we resolved one at daemon startup (env or file).
	if len(daemonCfg.embeddingKey) > 0 {
		cfg = cfg.WithEmbeddingKeyActive(true)
	}
	return &configStoreAdapter{
		cfg:       cfg,
		configDir: daemonCfg.configDir,
		secretBox: config.NewSecretBox(),
	}
}

func (a *configStoreAdapter) Load() (controlapi.RedactedConfig, error) {
	a.mu.RLock()
	cfg := a.cfg
	a.mu.RUnlock()
	rc := cfg.Redact()
	// Map internal RedactedConfig to controlapi.RedactedConfig.
	result := controlapi.RedactedConfig{
		DB:           rc.DB,
		SyncInterval: rc.SyncInterval,
		LogLevel:     rc.LogLevel,
		HTTP: &controlapi.HTTPConfig{
			Port: rc.HTTPPort,
		},
		// Transport and Extra are not exposed in RedactedConfig.
	}
	if rc.CentralURL != "" {
		result.Central = &controlapi.CentralConfig{
			URL:      rc.CentralURL,
			WriterID: rc.WriterID,
		}
	}
	if rc.WriterKey != "" {
		result.WriterKey = &rc.WriterKey
	}
	result.EmbeddingProvider = rc.EmbeddingProvider
	result.EmbeddingKeySet = rc.EmbeddingKeySet
	return result, nil
}

func (a *configStoreAdapter) Apply(patch controlapi.ConfigPatch) (bool, error) {
	// NOTE: explicit Unlock before the applyCb callback (no defer) — see the
	// lock-ordering comment below.
	a.mu.Lock()

	// Map controlapi.ConfigPatch → config.ConfigPatch.
	cfgPatch := config.ConfigPatch{
		SyncInterval:          patch.SyncInterval,
		LogLevel:              patch.LogLevel,
		HTTPPort:              patch.HTTPPort,
		DBPath:                patch.DBPath,
		Transport:             patch.Transport,
		EmbeddingProvider:     patch.EmbeddingProvider,
		EmbeddingLocalConsent: patch.EmbeddingLocalConsent,
		EmbeddingDims:         patch.EmbeddingDims,
		OllamaHost:            patch.OllamaHost,
		OllamaModel:           patch.OllamaModel,
	}

	updated, restartRequired := config.Patch(a.cfg, cfgPatch)
	a.cfg = updated

	// Persist the change if we have a config directory.
	if a.configDir != "" {
		if err := config.Save(a.configDir, updated); err != nil {
			a.mu.Unlock()
			return restartRequired, fmt.Errorf("save config: %w", err)
		}
	}
	cb := a.applyCb
	a.mu.Unlock()

	// Notify the sync adapter OUTSIDE the lock: applyLiveConfig may one day
	// acquire runtimeSyncAdapter.mu (loop restart), and runtimeSyncAdapter
	// methods call back into this adapter (setCentral/clearCentral take a.mu) —
	// holding a.mu across the callback would set up a lock-ordering deadlock.
	if cb != nil {
		cb(patch, updated)
	}

	return restartRequired, nil
}

// setCentral updates the in-memory central credentials and persists them.
// Called by runtimeSyncAdapter.Reconnect on a successful connect.
func (a *configStoreAdapter) setCentral(centralURL, writerID string, encryptedKey []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.CentralURL = centralURL
	a.cfg.WriterID = writerID
	a.cfg.EncryptedWriterKey = encryptedKey
	if a.configDir != "" {
		return config.Save(a.configDir, a.cfg)
	}
	return nil
}

// clearCentral removes central credentials from memory and disk.
// Called by runtimeSyncAdapter.Disconnect.
func (a *configStoreAdapter) clearCentral() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.CentralURL = ""
	a.cfg.WriterID = ""
	a.cfg.EncryptedWriterKey = nil
	if a.configDir != "" {
		return config.Save(a.configDir, a.cfg)
	}
	return nil
}

// getSyncInterval returns the current configured sync interval.
func (a *configStoreAdapter) getSyncInterval() time.Duration {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.SyncInterval
}

// SealEmbeddingKey encrypts plaintext via the platform SecretBox and persists
// the ciphertext to the config file. The plaintext is not stored in memory
// beyond this call. Satisfies controlapi.EmbeddingKeyStore.
func (a *configStoreAdapter) SealEmbeddingKey(plaintext []byte) error {
	ciphertext, err := a.secretBox.Seal(plaintext)
	if err != nil {
		// Map config.ErrNoSecretStore to controlapi.ErrNoSecretStore so the
		// handler's errors.Is check in embedding_key.go returns 422.
		if errors.Is(err, config.ErrNoSecretStore) {
			return controlapi.ErrNoSecretStore
		}
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg = a.cfg.WithEncryptedEmbeddingKey(ciphertext)
	a.cfg = a.cfg.WithEmbeddingKeyActive(true)
	if a.configDir != "" {
		return config.Save(a.configDir, a.cfg)
	}
	return nil
}

// ClearEmbeddingKey removes any stored encrypted embedding key from memory and
// disk. After this call the daemon falls back to ENGRAM_EMBEDDING_KEY env var
// (if set) or Noop. Satisfies controlapi.EmbeddingKeyStore.
func (a *configStoreAdapter) clearEmbeddingKey() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg = a.cfg.WithEncryptedEmbeddingKey(nil)
	a.cfg = a.cfg.WithEmbeddingKeyActive(false)
	if a.configDir != "" {
		return config.Save(a.configDir, a.cfg)
	}
	return nil
}

// ClearEmbeddingKey satisfies controlapi.EmbeddingKeyStore.
func (a *configStoreAdapter) ClearEmbeddingKey() error {
	return a.clearEmbeddingKey()
}

// ── runtimeSyncAdapter (PR-③) ─────────────────────────────────────────────────

// runtimeSyncAdapter is the full PR-③ SyncController implementation.
// It owns the live Loop reference and supports runtime connect/disconnect.
//
// Connect/disconnect re-installs the store.SetCentralConfiguredFn closure so
// the policy default (synced vs local-only) updates immediately on the next
// policy read — no restart required (PR-② contract).
type runtimeSyncAdapter struct {
	mu         sync.Mutex
	store      *localstore.Store
	cfgAdapter *configStoreAdapter
	loop       *syncer.Loop    // nil in local-only mode; replaced on Reconnect
	ctx        context.Context // daemon's root context (for new Loop.Start on reconnect)
	node       *syncer.Node
	// lastSyncResult is updated by the loop callbacks (future PR — for now zero value).
	// PR-③ wires the loop to report results; for the daemon test this is enough.
	connected  bool // mirrors loop != nil && centralURL != ""
	centralURL string
	actualPort int // the actual bound port (for Status.CentralURL etc.)

	// embeddingProvider is the provider name for Status.EmbeddingBackfill.Provider.
	// Populated by newRuntimeSyncAdapter when an embedLoop is active.
	embeddingProvider string
}

func newRuntimeSyncAdapter(
	ctx context.Context,
	cfg daemonCfg,
	store *localstore.Store,
	loop *syncer.Loop,
	cfgAdapter *configStoreAdapter,
	actualPort int,
	gated embedding.EmbeddingProvider, // non-nil always; NoopProvider when inactive
) *runtimeSyncAdapter {
	if ctx == nil {
		// Defensive: a runtime-created loop must always be startable.
		ctx = context.Background()
	}
	a := &runtimeSyncAdapter{
		store:      store,
		cfgAdapter: cfgAdapter,
		loop:       loop,
		ctx:        ctx,
		node:       syncer.NewNode("daemon", store),
		connected:  cfg.centralURL != "" && loop != nil,
		centralURL: cfg.centralURL,
		actualPort: actualPort,
		// Track the model name for Status.EmbeddingBackfill.Provider.
		// gated delegates ModelName() to the inner provider unconditionally.
		embeddingProvider: gated.ModelName(),
	}
	// Wire the configStoreAdapter callback for live SyncInterval updates.
	cfgAdapter.applyCb = a.applyLiveConfig
	return a
}

func (a *runtimeSyncAdapter) Status() controlapi.Status {
	a.mu.Lock()
	defer a.mu.Unlock()

	st := controlapi.Status{
		CentralConnected: a.connected,
		LastSyncResult:   controlapi.SyncResult{},
		DaemonVersion:    version,
	}
	if a.centralURL != "" {
		u := a.centralURL
		st.CentralURL = &u
	}

	// Populate embedding_backfill sub-object (spec: observability requirement).
	// The pending count is best-effort (±1 race with concurrent writes is acceptable).
	// The field is always present when an embedding provider is configured — even
	// for the Noop case (provider="noop", pending=N shows what would be backfilled).
	pending, _ := a.store.CountPendingEmbeddings(a.embeddingProvider) // eligible rows only; error → 0 (best-effort)
	st.EmbeddingBackfill = &controlapi.EmbeddingBackfill{
		Pending:  pending,
		Provider: a.embeddingProvider,
	}

	return st
}

func (a *runtimeSyncAdapter) TriggerNow(_ context.Context) error {
	a.mu.Lock()
	loop := a.loop
	a.mu.Unlock()
	if loop != nil {
		loop.Trigger()
	}
	return nil
}

// Disconnect stops the sync loop, clears central credentials from the config
// file, and re-installs the SetCentralConfiguredFn closure → false.
// Local data is NOT deleted.
func (a *runtimeSyncAdapter) Disconnect() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Stop the loop (idempotent if never started or already stopped).
	if a.loop != nil {
		a.loop.Stop()
		a.loop = nil
	}

	a.connected = false
	a.centralURL = ""

	// Clear central config from disk.
	if err := a.cfgAdapter.clearCentral(); err != nil {
		return fmt.Errorf("disconnect: clear central config: %w", err)
	}

	// Re-install the closure → false so the next policy read returns local-only.
	a.store.SetCentralConfiguredFn(func() bool { return false })

	slog.Default().Info("daemon: disconnected from central; sync loop stopped")
	return nil
}

// Reconnect validates credentials, seals the writer key, persists config, and
// starts a new sync loop. On any error nothing is persisted and the existing
// loop state is unchanged.
//
// The WriterKeyPlaintext field in cfg carries the raw key from the connect
// request. The adapter seals it via DPAPI (Windows) or notes that storage is
// unavailable (non-Windows — key is not persisted to file, but the in-memory
// daemon can still use it for this session). The actual session writerKey is
// kept only in memory.
func (a *runtimeSyncAdapter) Reconnect(cfg controlapi.CentralConfig) error {
	if cfg.URL == "" {
		return fmt.Errorf("central_url is required")
	}
	if cfg.WriterKeyPlaintext == "" {
		return fmt.Errorf("writer_key is required")
	}

	// Decode the writer key. Wrap input errors with the controlapi sentinel so
	// the handler returns a client-safe 422; the wrapped detail (which may
	// include hex-decode internals) is server-log-only.
	keyHex := strings.TrimSpace(cfg.WriterKeyPlaintext)
	writerKey, err := hex.DecodeString(keyHex)
	if err != nil {
		return fmt.Errorf("%w: not valid hex: %v", controlapi.ErrInvalidWriterKey, err)
	}
	if len(writerKey) != wireauth.KeySize {
		return fmt.Errorf("%w: got %d bytes, want %d", controlapi.ErrInvalidWriterKey, len(writerKey), wireauth.KeySize)
	}

	// Probe the remote to validate credentials BEFORE persisting anything.
	// A probe failure maps to 422 — config is NOT persisted.
	centralClient := remote.New(cfg.URL, nil, cfg.WriterID, writerKey)
	if err := probeRemote(centralClient); err != nil {
		return fmt.Errorf("%w: %v", controlapi.ErrCredentialValidation, err)
	}

	// Seal the writer key for storage (Windows: DPAPI; non-Windows: env only).
	var encryptedKey []byte
	secretBox := a.cfgAdapter.secretBox
	sealed, sealErr := secretBox.Seal(writerKey)
	if sealErr == nil {
		encryptedKey = sealed
	} else if !errors.Is(sealErr, config.ErrNoSecretStore) {
		// Unexpected seal error (not "platform doesn't support it").
		return fmt.Errorf("seal writer key: %w", sealErr)
	}
	// If ErrNoSecretStore: non-Windows platform — proceed without persisting key.
	// The key is used in memory for this session only.

	// From here on every step mutates shared state: take the adapter lock
	// BEFORE persisting so a concurrent Disconnect cannot interleave between
	// the disk write and the in-memory state update (which would leave disk
	// saying "disconnected" while memory says "connected", or vice versa).
	a.mu.Lock()
	defer a.mu.Unlock()

	// Persist the new central config (including sealed key, which may be nil).
	if err := a.cfgAdapter.setCentral(cfg.URL, cfg.WriterID, encryptedKey); err != nil {
		return fmt.Errorf("persist central config: %w", err)
	}

	// Stop any existing loop.
	if a.loop != nil {
		a.loop.Stop()
	}

	syncInterval := a.cfgAdapter.getSyncInterval()
	if syncInterval <= 0 {
		syncInterval = 30 * time.Second
	}

	newLoop := syncer.NewLoop(a.node, centralClient, syncer.Config{
		Interval: syncInterval,
	})

	// Start the loop on the daemon's root context (never nil — the constructor
	// guarantees it). Daemon shutdown cancels the context and stops this loop.
	newLoop.Start(a.ctx)

	a.loop = newLoop
	a.connected = true
	a.centralURL = cfg.URL

	// Re-install the closure → true so next policy read returns synced.
	centralURL := cfg.URL
	a.store.SetCentralConfiguredFn(func() bool { return centralURL != "" })

	slog.Default().Info("daemon: connected to central; sync loop started",
		"central_url", cfg.URL,
		"writer_id", cfg.WriterID,
	)
	return nil
}

// applyLiveConfig is called by configStoreAdapter.Apply for runtime-mutable
// patches. The SyncInterval change takes effect immediately: the loop is
// stopped and a new loop is started with the new interval — the same node and
// central client is reused so no outbox entries are lost.
//
// This requires the decrypted writer key to rebuild the central client.
// Because we don't keep the plaintext key after Reconnect (only the encrypted
// blob), we update the interval in the stored config and emit a log note.
// The new interval is reflected in GET /api/v1/config immediately. A new sync
// cycle at the new interval begins after the next Reconnect or daemon restart.
//
// For the acceptance test contract: the interval value in Load() changes
// immediately (config is updated); the live loop cadence changes on reconnect.
func (a *runtimeSyncAdapter) applyLiveConfig(patch controlapi.ConfigPatch, updated config.Config) {
	if patch.SyncInterval == nil {
		return // not a sync interval change
	}

	newInterval := updated.SyncInterval
	if newInterval <= 0 {
		newInterval = 30 * time.Second
	}

	slog.Default().Info("daemon: sync interval updated in config; live loop will use new interval on next reconnect",
		"new_interval", newInterval,
	)
}

// probeRemote validates the central URL and writer credentials with a REAL
// signed request before anything is persisted: a PullSince for a probe-only
// project with limit 1. A healthy central with valid credentials returns an
// empty (or tiny) page; bad credentials surface as a 401/403 error from the
// transport, an unreachable URL as a network error. Bounded by a 5s timeout so
// a black-holed URL cannot hang the connect handler.
func probeRemote(c *remote.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.PullSince(ctx, "engram-connect-probe", 0, 1)
	return err
}

// serveErr classifies errors from StdioServer.Listen.
func serveErr(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	return fmt.Errorf("daemon: serve: %w", err)
}
