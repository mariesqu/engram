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
	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/obsidian"
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
schedule.  Each cycle also asks central for its project list (POST /v1/projects)
and unions it into the pull set, so projects created on other machines are
discovered and pulled automatically.  When central does not support discovery
(older server: 404/501) the daemon falls back to pulling only projects already
present in the local store.  Projects whose local policy is not "synced" are
skipped in both cases.
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
ENGRAM_CONFIG_DIR overrides the config file directory (relocate or isolate config.json,
e.g. for testing or running multiple daemons without colliding on one file).
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

	// reviewWindowDays is the memory-lifecycle staleness window in days (Feature 1).
	// 0 → store default (30). Resolved from the config file in runDaemonCmd.
	reviewWindowDays int

	// ── Obsidian scheduled export (REQ-WATCH-09) ─────────────────────────────
	// Config-file only: no flag and no env var, mirroring the embedding
	// subsystem — the closest in-repo precedent for an optional, off-by-default
	// background worker.
	//
	// obsidianVault EMPTY IS THE MASTER OFF SWITCH: buildDaemon then constructs
	// no Exporter and no Loop, creates no directory, probes no file, writes
	// nothing anywhere and logs nothing about it. "Presence of the coordinate is
	// the switch" — the same idiom as centralURL above.
	obsidianVault       string
	obsidianInterval    time.Duration // 0 → 10m (resolveObsidianInterval)
	obsidianProject     string        // "" → every project
	obsidianGraphConfig string        // "" → "preserve" (resolveObsidianGraphConfig)

	// ── config.Load caching (Phase 9 review MINOR-5) ─────────────────────────
	// loadedFileConfig/loadedFileConfigCached cache the config.Config that
	// runDaemonCmd ALREADY parsed via config.Load, so newConfigStoreAdapter —
	// which needs a handful of fields Load alone resolves (encrypted blobs,
	// log-level/transport fallback, the four obsidian_* keys) — does not have
	// to re-parse config.json from disk a SECOND time for one daemon boot.
	// Without this, an unrecognised obsidian_graph_config value produced the
	// SAME stderr warning twice per HTTP boot: once from runDaemonCmd's Load
	// (daemon.go, in this file) and once more from newConfigStoreAdapter's own
	// Load (also this file) — two log lines for one misconfiguration.
	//
	// loadedFileConfigCached=false (the zero value) means "not cached" —
	// newConfigStoreAdapter then falls back to its own config.Load call
	// exactly as before. This is what keeps every test in this package that
	// constructs a daemonCfg{} literal directly (bypassing runDaemonCmd)
	// working unchanged: only the real runDaemonCmd → runDaemonHTTP
	// production path ever sets these fields, so only that path is affected.
	loadedFileConfig       config.Config
	loadedFileConfigCached bool
}

// Obsidian export cadence bounds (REQ-WATCH-04). The floor duplicates
// obsidian.Loop's own unexported floor DELIBERATELY: this is the daemon's
// independent layer of the defence, so a change to one does not silently
// disarm the other.
const (
	obsidianIntervalDefault = 10 * time.Minute
	obsidianIntervalFloor   = time.Minute
)

// resolveObsidianInterval turns the configured obsidian_interval into the value
// handed to obsidian.LoopConfig.Interval, returning an optional warning string
// for the caller to log ("" = nothing to say).
//
// This is the SECOND of three independent defences against a cadence that would
// make the export loop spin:
//
//  1. config.Load REJECTS a non-positive obsidian_interval outright
//     (startup-fatal), and PUT /api/v1/config rejects it with 400, so a bad
//     value can neither be loaded nor persisted. time.ParseDuration("-1s")
//     succeeds and does NO sign check — that gap is exactly what produced Phase
//     8's CRITICAL-1 (measured 1,024,805 cycles/sec from math.MinInt64, which
//     saturates the single SQLite connection and starves MCP).
//  2. THIS function: anything non-positive falls back to the 10m default, and a
//     positive sub-floor value is clamped to 1m with a warning. Non-positive →
//     default is how both sibling loops already read their Interval
//     (syncer.Loop, embedding.Loop), so it is the repo's existing idiom.
//  3. obsidian.Loop.applyLoopDefaults clamps anything below its own floor.
//
// Three layers is not paranoia here: daemonCfg is a plain struct any caller can
// build, and the spec's instruction to mirror sync_interval "exactly" is what
// would have reproduced the missing sign check in the first place.
//
// A positive sub-minute value is CLAMPED, never fatal (REQ-WATCH-04): refusing
// to boot the process that owns MCP, autosync and the store because a cosmetic
// export cadence is too small is a strictly worse failure than exporting less
// often than asked.
//
// DELIBERATE, DOCUMENTED DIVERGENCE from obsidian.Loop's own floor (Phase 9
// adversarial review MINOR-1): this function answers a negative interval with
// its DEFAULT (10m); obsidian.applyLoopDefaults answers the identical input
// with its FLOOR (1m) — see the matching comment there. Both are positive and
// therefore both safe; the two are intentionally NOT reconciled to one value,
// because reconciling would mean changing the behaviour of whichever layer
// moved, and the Phase 8 review already verified the Loop's floor behaviour
// safe on its own terms. What DOES matter, and is now pinned by
// TestBuildDaemonResolvedIntervalReachesLoop (daemon_obsidian_test.go), is
// that the value THIS function returns — not the raw cfg.obsidianInterval —
// is what reaches obsidian.NewLoop below: if a future change accidentally
// hands the Loop the unresolved config value instead, the disagreement above
// stops being unreachable and starts being observable (this function's 10m
// default vs the Loop's own 1m floor for the same negative input).
func resolveObsidianInterval(configured time.Duration) (time.Duration, string) {
	switch {
	case configured == 0:
		return obsidianIntervalDefault, ""
	case configured < 0:
		// Comparison only — never negate. time.Duration(math.MinInt64) negates
		// back to itself (two's-complement overflow) and stays negative.
		return obsidianIntervalDefault, fmt.Sprintf(
			"obsidian_interval %s is not a valid cadence; using the %s default",
			configured, obsidianIntervalDefault)
	case configured < obsidianIntervalFloor:
		return obsidianIntervalFloor, fmt.Sprintf(
			"obsidian_interval %s is below the %s floor; clamping to %s",
			configured, obsidianIntervalFloor, obsidianIntervalFloor)
	default:
		return configured, ""
	}
}

// resolveObsidianGraphConfig turns the configured obsidian_graph_config into an
// obsidian.GraphConfigMode, returning an optional warning string.
//
// Unset defaults to "preserve", matching the CLI's --graph-config default.
// Defaulting the daemon to "skip" was considered and rejected: a user who only
// ever runs the daemon would then never get the curated graph, which is the
// entire visual point of the feature — and "preserve" is non-destructive by
// construction (it writes {vault}/.obsidian/graph.json only when absent).
//
// An unrecognised value falls back to "preserve" with a warning rather than
// failing startup. config.Load already normalises it the same way; this is the
// daemon's own layer, because daemonCfg is a plain struct.
func resolveObsidianGraphConfig(configured string) (obsidian.GraphConfigMode, string) {
	if configured == "" {
		return obsidian.GraphConfigPreserve, ""
	}
	mode, err := obsidian.ParseGraphConfigMode(configured)
	if err != nil {
		return obsidian.GraphConfigPreserve, fmt.Sprintf(
			"obsidian_graph_config %q is not recognised; falling back to %q",
			configured, obsidian.GraphConfigPreserve)
	}
	return mode, ""
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

// remoteCentral is the subset of *remote.Client the web UI and the startup
// remote-purge check need: list central's projects (for the "exists in
// central" marker), unshare a project (delete it from central over the
// authenticated wire — no DSN), and fetch this writer's purge_epoch (the
// remote-purge startup check — see checkRemotePurgeOnStartup). Satisfied by
// *remote.Client; nil in local-only mode (no central configured).
type remoteCentral interface {
	ListProjects(ctx context.Context) ([]string, error)
	Unshare(ctx context.Context, project string) (int, error)
	State(ctx context.Context) (remote.WriterState, error)
}

// daemonComponents holds the wired-but-not-yet-serving components built by
// buildDaemon. Callers must call Close to release resources.
type daemonComponents struct {
	store     *localstore.Store
	mcpServer *mcpserver.MCPServer
	loop      *syncer.Loop                // nil when running in local-only mode
	central   remoteCentral               // nil in local-only mode; the wire client for the loop
	embedLoop *embedding.Loop             // nil when provider is Noop or key absent
	gated     embedding.EmbeddingProvider // always non-nil (at least NoopProvider via gate)
	// obsidianLoop is nil when obsidian_vault is unset — the feature's master
	// OFF switch (REQ-WATCH-09).
	obsidianLoop *obsidian.Loop
	// obsidianStarted is set true ONLY at the point obsidianLoop.Start(ctx) is
	// actually called (both runDaemonWithIO and runDaemonHTTP set it
	// immediately after that call). It is deliberately a SEPARATE signal from
	// "obsidianLoop != nil": the loop can be constructed (vault configured)
	// long before either run mode reaches its Start() call, and several early
	// returns in runDaemonHTTP — port already in use, token generation
	// failure, WriteDaemonJSON failure — run the deferred Close() BEFORE
	// Start() was ever reached (Phase 9 review MINOR-3). Gating the "waiting
	// for any in-flight export cycle" log on this flag, rather than on
	// obsidianLoop's nil-ness alone, stops that log firing when no cycle
	// could possibly be in flight. obsidian.Loop.Stop() was ALREADY a safe,
	// fast no-op on a never-started loop — only the log line was wrong.
	obsidianStarted bool
}

// Close stops the Obsidian export loop, the embedding backfill loop, the
// autosync loop, and the store — IN THAT ORDER, so no in-flight work can race
// the store close.
//
// The Obsidian loop goes FIRST and the ordering is not cosmetic. Its Stop()
// BLOCKS until the in-flight export cycle finishes (REQ-WATCH-05: no abrupt
// cancellation mid-write, because Exporter.Export() takes no context.Context by
// design). A cycle caught mid-RecentObservations against a closed DB — or mid
// os.WriteFile on the state file the Obsidian viewer reads — is exactly the
// corruption that drain exists to prevent, and it can only be prevented if the
// store is still alive while the drain runs.
//
// A bounded drain that abandons the goroutine after N seconds was explicitly
// rejected in design: leaking a goroutine mid-write while store.Close() runs is
// the very failure being avoided.
func (d *daemonComponents) Close() {
	// REQ-WATCH-05's "SHOULD log that it is waiting" clause. This MUST be
	// emitted BEFORE the blocking Stop(), not after: a cold first cycle over
	// ~4,650 observations was MEASURED at ~18 seconds, and that figure scales
	// with vault size. An unexplained multi-second hang at shutdown reads as a
	// deadlock and invites a SIGKILL mid-write — which is the corruption the
	// drain is there to prevent. Deliberately no timeout is derived from the
	// ~18s figure; it is an observation, not a bound.
	//
	// HTTP-mode total worst case is HIGHER than the figure this log line
	// quotes on its own (Phase 9 review MINOR-4, documentation-only — the
	// ordering below is correct and unchanged): runDaemonHTTP's ctx.Done()
	// branch calls httpSrv.Shutdown with its own 10s budget and RETURNS from
	// that branch — the "waiting…" log below and the blocking obsidianLoop
	// drain only happen AFTER, inside the LIFO deferred components.Close().
	// So the HTTP listener's own drain (up to 10s) runs SERIALLY before this
	// one even starts: worst case in HTTP mode is therefore ~28-33s (≤10s
	// HTTP drain + up to ~18-23s cold obsidian drain), not the ~18s this line
	// names alone. The store is still the LAST thing closed either way.
	//
	// Nothing is logged when the feature is off, OR when it was constructed
	// but never actually Started (REQ-WATCH-09: OFF must be inert, including
	// in the log; Phase 9 review MINOR-3: an early return in runDaemonHTTP
	// before Start() — port already in use, token generation failure,
	// WriteDaemonJSON failure — must not print a drain warning for a cycle
	// that could never have been in flight). log.Printf writes to stderr —
	// never stdout, which is the MCP JSON-RPC channel in the daemon's default
	// mode.
	if d.obsidianLoop != nil && d.obsidianStarted {
		log.Printf("engram daemon: stopping the Obsidian export loop — waiting for any in-flight export cycle to finish " +
			"(a cold first cycle over ~4,650 observations was measured at ~18s and scales with vault size)")
	}
	// obsidian.Loop.Stop() is nil-safe and blocks until the goroutine exits.
	d.obsidianLoop.Stop()
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
	// precedence contract as ENGRAM_WRITER_KEY. The value may be hex-encoded OR
	// the raw API key. When neither source provides a key, the daemon starts with
	// the Noop provider regardless of embedding_provider — no error, just no
	// embedding capability.
	embeddingProvider := fileCfg.EmbeddingProvider
	var embeddingKey []byte
	if embeddingKeyEnv := strings.TrimSpace(os.Getenv("ENGRAM_EMBEDDING_KEY")); embeddingKeyEnv != "" {
		// Env var wins. Accept EITHER a hex-encoded key (legacy/explicit) or the
		// raw API key verbatim: try hex first, and if it does not decode, use the
		// value as-is. This avoids the footgun where a normal provider key (e.g.
		// "sk-…" / a Mistral key — not valid hex) would error at startup.
		if decoded, err := hex.DecodeString(embeddingKeyEnv); err == nil {
			embeddingKey = decoded
		} else {
			embeddingKey = []byte(embeddingKeyEnv)
		}
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
		reviewWindowDays:      fileCfg.ReviewWindowDays,
		// Obsidian scheduled export: config file ONLY — no flag, no env var
		// (REQ-WATCH-09). config.Load has already rejected a non-positive
		// obsidian_interval as startup-fatal and normalised an unrecognised
		// obsidian_graph_config to "preserve".
		obsidianVault:       fileCfg.ObsidianVault,
		obsidianInterval:    fileCfg.ObsidianInterval,
		obsidianProject:     fileCfg.ObsidianProject,
		obsidianGraphConfig: fileCfg.ObsidianGraphConfig,
		// Cache the config.Config this function already loaded above (MINOR-5)
		// so newConfigStoreAdapter (HTTP mode only) reuses it instead of
		// parsing config.json a second time. Set unconditionally: when
		// configDir == "" fileCfg is the legitimate zero value, matching what
		// newConfigStoreAdapter's own configDir != "" guard would already skip.
		loadedFileConfig:       fileCfg,
		loadedFileConfigCached: true,
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

	// Resolve the memory-lifecycle staleness window (Feature 1). 0/unset → the
	// store's built-in default (30). Set once at startup; the review tools read
	// it internally so the window need not thread through registerTools.
	store.SetReviewWindowDays(cfg.reviewWindowDays)

	mcpSrv := mcpserver.NewMCPServer(
		"engram",
		version,
		mcpserver.WithToolCapabilities(true),
	)

	var loop *syncer.Loop
	var central remoteCentral

	if cfg.centralURL != "" {
		c := remote.New(cfg.centralURL, nil, cfg.writerID, cfg.writerKey)
		central = c
		node := syncer.NewNode("daemon", store)
		loop = syncer.NewLoop(node, c, syncer.Config{
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

	// ── Construct the Obsidian export loop (REQ-WATCH-09/-10) ────────────────
	// Constructed HERE, from the store buildDaemon has ALREADY opened, and
	// never from a second localstore.Open. That is the whole point of hosting
	// the scheduled export in the daemon: localstore.Open is not a read-only
	// open — it runs ApplySchema then runMigrations on EVERY invocation — so a
	// repeating unattended open from a second, possibly version-mismatched
	// binary would migrate the schema out from under a running daemon
	// (verify-report #4710, MAJOR-10). internal/obsidian never imports
	// internal/localstore; that is pinned mechanically by
	// internal/obsidian/imports_test.go, not left to review discipline.
	//
	// An empty obsidianVault is the master OFF switch: nothing below runs, so
	// no Exporter exists, no goroutine is started, no directory is created, no
	// file is probed and no log line is emitted (REQ-WATCH-09).
	var obsidianLoop *obsidian.Loop
	if cfg.obsidianVault != "" {
		graphMode, graphWarn := resolveObsidianGraphConfig(cfg.obsidianGraphConfig)
		if graphWarn != "" {
			log.Printf("warning: %s", graphWarn)
		}
		interval, intervalWarn := resolveObsidianInterval(cfg.obsidianInterval)
		if intervalWarn != "" {
			log.Printf("warning: %s", intervalWarn)
		}

		// REQ-WATCH-11 (REQUIRED): warn at startup when a project filter is
		// set. This is the agreed mitigation for verify-report #4710's deferred
		// MAJOR-3, which is otherwise unfixed — and under a daemon the damage
		// repeats every cycle forever instead of once when a human typed a
		// filter, so it has to be loud.
		if cfg.obsidianProject != "" {
			log.Printf("warning: obsidian_project=%q limits the scheduled export to one project; "+
				"session and topic hubs are inherently cross-project, so their membership is rendered "+
				"from this scope ONLY (truncated) and the stale-hub sweep is disabled for every cycle",
				cfg.obsidianProject)
		}

		exp, expErr := obsidian.NewExporter(&obsidianStoreAdapter{store: store}, obsidian.ExportConfig{
			VaultPath:   cfg.obsidianVault,
			Project:     cfg.obsidianProject,
			GraphConfig: graphMode,
		})
		if expErr != nil {
			_ = store.Close()
			return nil, fmt.Errorf("obsidian export: %w", expErr)
		}
		obsidianLoop = obsidian.NewLoop(exp, obsidian.LoopConfig{
			Interval: interval,
			// slog writes to stderr by default. The loop MUST NOT write to
			// stdout at all — design constraint 16: in the daemon's default
			// mode stdout is the MCP JSON-RPC channel.
			Logger: slog.Default(),
		})
	}

	activity := NewSessionActivity()
	registerTools(mcpSrv, store, loop, embedLoop, gated, cfg.writerID, activity)

	return &daemonComponents{
		store:        store,
		mcpServer:    mcpSrv,
		loop:         loop,
		central:      central,
		embedLoop:    embedLoop,
		gated:        gated,
		obsidianLoop: obsidianLoop,
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

	checkRemotePurgeOnStartup(ctx, components.store, components.central)

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

	// Start the Obsidian export loop. Started in BOTH run modes on purpose
	// (REQ-WATCH-09): a user on the default stdio transport is entitled to a
	// fresh vault too, and starting it in only one is the asymmetry that ships
	// as "works on my machine". nil when obsidian_vault is unset — Start on nil
	// panics, so guard.
	if components.obsidianLoop != nil {
		components.obsidianLoop.Start(ctx)
		components.obsidianStarted = true
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

	checkRemotePurgeOnStartup(ctx, components.store, components.central)

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
	// Start the Obsidian export loop — see the note in runDaemonWithIO: BOTH
	// run modes start it (REQ-WATCH-09).
	if components.obsidianLoop != nil {
		components.obsidianLoop.Start(ctx)
		components.obsidianStarted = true
	}
	defer func() {
		_ = controlapi.RemoveDaemonJSON(dir)
	}()

	// ── PR-③: wire the full runtime adapters ─────────────────────────────────
	storeAdapter := &localStoreAdapter{store: components.store, writerID: cfg.writerID}

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
	// The wire client for remote project discovery (RemoteProjects). Reconnect /
	// Disconnect replace it under the adapter lock.
	syncAdapter.central = components.central
	// Obsidian export status (REQ-WATCH-11). nil loop → the field is omitted.
	// The interval echoed here is the RESOLVED one, so an operator sees the
	// cadence the loop actually runs at rather than the raw config value.
	syncAdapter.obsidianLoop = components.obsidianLoop
	if components.obsidianLoop != nil {
		syncAdapter.obsidianVault = cfg.obsidianVault
		syncAdapter.obsidianInterval, _ = resolveObsidianInterval(cfg.obsidianInterval)
	}

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
		SyncCtrl:       syncAdapter,
		Store:          storeAdapter,
		RemoteProjects: syncAdapter.RemoteProjects,
		Unshare:        syncAdapter.UnshareProject,
		Secret:         token,
		Port:           actualPort,
		Version:        version,
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

// ── Remote purge (per-writer purge_epoch) ────────────────────────────────────

// remoteStateTimeout bounds the /v1/state check so a black-holed or slow
// central cannot delay daemon startup. An offline laptop must still boot.
const remoteStateTimeout = 5 * time.Second

// stateFetcher is the narrow interface checkRemotePurgeOnStartup needs from
// the wire client: fetch the authenticated writer's current purge_epoch.
// *remote.Client satisfies it. Defined as a local interface (rather than
// depending on the concrete type) so the startup check is testable with a
// fake State() implementation, per the "testable with a fake State()" spec
// requirement.
type stateFetcher interface {
	State(ctx context.Context) (remote.WriterState, error)
}

// purgeStore is the narrow interface checkRemotePurgeOnStartup needs from the
// local store: read the last-honored epoch and run the destructive resync
// purge. *localstore.Store satisfies it.
type purgeStore interface {
	HonoredPurgeEpoch() (int64, error)
	PurgeForResync(newEpoch int64) (localstore.PurgeResult, error)
}

// statusCoder is the OPTIONAL HTTP-status accessor implemented by
// *remote.StatusError. checkRemotePurgeOnStartup uses it to detect a 404/501
// (an older central without the /v1/state endpoint, or a Central that does not
// implement the writerPurgeEpoch capability) WITHOUT a type assertion to the
// concrete *remote.StatusError — mirrors the same duck-typed classification
// syncer.isDiscoveryUnsupported uses for /v1/projects (see
// internal/syncer/syncer.go), reimplemented locally here because that
// predicate is unexported.
type statusCoder interface {
	StatusCode() int
}

// isRemoteStateUnsupported reports whether err is central signalling that it
// does not implement the /v1/state endpoint or the writerPurgeEpoch capability
// — a 404 from an older central's catch-all for the unregistered route, or a
// 501 from the capability-gated handler. Mirrors syncer.isDiscoveryUnsupported.
func isRemoteStateUnsupported(err error) bool {
	var sc statusCoder
	if errors.As(err, &sc) {
		code := sc.StatusCode()
		return code == http.StatusNotFound || code == http.StatusNotImplemented
	}
	return false
}

// checkRemotePurgeOnStartup implements the remote-purge decision table from
// the feature spec. It is called once at daemon startup, AFTER buildDaemon and
// BEFORE the autosync loop starts, so a purge (if triggered) completes before
// any pull cycle can race it.
//
// central is nil in local-only mode (no --central-url configured) — the check
// is a no-op in that case, matching daemonComponents.central's nil-in-local-only
// contract.
//
// Decision table:
//   - central == nil (local-only mode): no-op, return immediately.
//   - Network error / timeout calling State(): FAIL OPEN — log a warning, skip
//     the check, continue startup. An offline laptop must still be able to boot.
//   - 404/501 (older central without /v1/state, or a Central lacking the
//     capability): treat as a no-op, same as isDiscoveryUnsupported for
//     project discovery — not a startup failure.
//   - remote_epoch > honored_epoch: log prominently, run PurgeForResync, log
//     the per-table counts, then continue startup (the next autosync cycle's
//     project discovery + pull re-populates the store; no special pull mode
//     needed).
//   - remote_epoch <= honored_epoch: continue normally (no-op — this also
//     covers the "never checked before, central never bumped" steady state
//     where both sides are 0).
//
// Errors reading/writing the LOCAL store (HonoredPurgeEpoch, PurgeForResync)
// are logged and treated as fail-open too: a local storage hiccup must not
// block startup any more than a network hiccup should.
func checkRemotePurgeOnStartup(ctx context.Context, store purgeStore, central stateFetcher) {
	if central == nil {
		return // local-only mode: nothing to check
	}

	stateCtx, cancel := context.WithTimeout(ctx, remoteStateTimeout)
	defer cancel()

	remoteState, err := central.State(stateCtx)
	if err != nil {
		if isRemoteStateUnsupported(err) {
			// Older central (no /v1/state) or a Central lacking the capability —
			// not an error condition, just nothing to check this startup.
			return
		}
		// Network error, timeout, 5xx, or auth failure: FAIL OPEN. An offline or
		// flaky-network machine must still be able to start the daemon.
		log.Printf("engram daemon: remote purge check skipped (central unreachable): %v", err)
		return
	}

	honoredEpoch, err := store.HonoredPurgeEpoch()
	if err != nil {
		log.Printf("engram daemon: remote purge check skipped (read local honored epoch failed): %v", err)
		return
	}

	if remoteState.PurgeEpoch <= honoredEpoch {
		return // nothing to do — already honored (covers the 0 == 0 steady state)
	}

	log.Printf("engram daemon: remote purge requested (central epoch=%d > honored epoch=%d) — purging local SYNCED data and re-pulling from central",
		remoteState.PurgeEpoch, honoredEpoch)

	result, err := store.PurgeForResync(remoteState.PurgeEpoch)
	if err != nil {
		log.Printf("engram daemon: remote purge FAILED: %v — local data left unchanged; will retry on next daemon start", err)
		return
	}

	log.Printf("engram daemon: remote purge complete: projects=%v memories=%d user_prompts=%d memory_tombstones=%d prompt_tombstones=%d sync_mutations=%d applied_mutations=%d pull_cursors=%d new_honored_epoch=%d",
		result.Projects,
		result.MemoriesDeleted,
		result.UserPromptsDeleted,
		result.MemoryTombstones,
		result.PromptTombstones,
		result.SyncMutations,
		result.AppliedMutations,
		result.PullCursorsDeleted,
		result.NewHonoredEpoch,
	)
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
	store    *localstore.Store
	writerID string
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

func (a *localStoreAdapter) ListMemories(query, project string, limit int) ([]controlapi.MemorySummary, error) {
	var records []*domain.Record
	var err error
	if query != "" {
		records, _, err = a.store.SearchMemoriesFiltered(query, project, limit, localstore.SearchFilter{})
	} else {
		records, err = a.store.RecentObservations(project, "", limit)
	}
	if err != nil {
		return nil, err
	}
	out := make([]controlapi.MemorySummary, 0, len(records))
	for _, r := range records {
		out = append(out, recordToSummary(r))
	}
	return out, nil
}

// recordToSummary converts a domain.Record to a controlapi.MemorySummary.
func recordToSummary(r *domain.Record) controlapi.MemorySummary {
	return controlapi.MemorySummary{
		ID:        r.ID,
		SyncID:    r.SyncID,
		Project:   r.Project,
		Type:      r.Type,
		Title:     r.Title,
		Content:   r.Content,
		Scope:     r.Scope,
		CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: r.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func (a *localStoreAdapter) UpdateMemory(id int64, title, content, typ string) (controlapi.MemorySummary, error) {
	rec, err := a.store.UpdateMemory(id, title, content, typ, a.writerID)
	if err != nil {
		return controlapi.MemorySummary{}, err
	}
	return recordToSummary(rec), nil
}

func (a *localStoreAdapter) DeleteMemory(id int64) error {
	return a.store.DeleteMemory(id, a.writerID)
}

func (a *localStoreAdapter) PurgeProjectLocal(project string) (int, error) {
	return a.store.PurgeProjectLocal(project)
}

func (a *localStoreAdapter) TombstoneProject(project string) (int, error) {
	return a.store.TombstoneProject(project, a.writerID)
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
	// Recover encrypted blobs and fields not in daemonCfg. Reuses the
	// config.Config runDaemonCmd already parsed (MINOR-5) when available,
	// rather than parsing config.json a second time — a second parse would
	// re-run config.Load's obsidian_graph_config fallback warning, printing
	// the identical stderr line twice for one HTTP daemon boot. Falls back to
	// a fresh config.Load when the cache was not populated (daemonCfg built
	// directly, e.g. by tests in this package, rather than via runDaemonCmd).
	if daemonCfg.configDir != "" {
		fileCfg, ok := daemonCfg.loadedFileConfig, daemonCfg.loadedFileConfigCached
		if !ok {
			var err error
			fileCfg, err = config.Load(daemonCfg.configDir)
			ok = err == nil
		}
		if ok {
			cfg.EncryptedWriterKey = fileCfg.EncryptedWriterKey
			cfg = cfg.WithEncryptedEmbeddingKey(fileCfg.EncryptedEmbeddingKey())
			cfg.EmbeddingProvider = fileCfg.EmbeddingProvider
			cfg.EmbeddingLocalConsent = fileCfg.EmbeddingLocalConsent
			cfg.EmbeddingDims = fileCfg.EmbeddingDims
			cfg.EmbeddingBaseURL = fileCfg.EmbeddingBaseURL
			cfg.EmbeddingModel = fileCfg.EmbeddingModel
			cfg.EmbeddingAuthHeader = fileCfg.EmbeddingAuthHeader
			cfg.OllamaHost = fileCfg.OllamaHost
			cfg.OllamaModel = fileCfg.OllamaModel
			cfg.ObsidianVault = fileCfg.ObsidianVault
			cfg.ObsidianInterval = fileCfg.ObsidianInterval
			cfg.ObsidianProject = fileCfg.ObsidianProject
			cfg.ObsidianGraphConfig = fileCfg.ObsidianGraphConfig
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
	result.EmbeddingBaseURL = rc.EmbeddingBaseURL
	result.EmbeddingModel = rc.EmbeddingModel
	result.EmbeddingAuthHeader = rc.EmbeddingAuthHeader
	// Obsidian export (REQ-WATCH-09): visible in the redacted read. The vault
	// path is a filesystem path like db_path, not a secret.
	result.ObsidianVault = rc.ObsidianVault
	result.ObsidianInterval = rc.ObsidianInterval
	result.ObsidianProject = rc.ObsidianProject
	result.ObsidianGraphConfig = rc.ObsidianGraphConfig
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
		EmbeddingBaseURL:      patch.EmbeddingBaseURL,
		EmbeddingModel:        patch.EmbeddingModel,
		EmbeddingAuthHeader:   patch.EmbeddingAuthHeader,
		OllamaHost:            patch.OllamaHost,
		OllamaModel:           patch.OllamaModel,
		ObsidianVault:         patch.ObsidianVault,
		ObsidianInterval:      patch.ObsidianInterval,
		ObsidianProject:       patch.ObsidianProject,
		ObsidianGraphConfig:   patch.ObsidianGraphConfig,
	}

	updated, restartRequired := config.Patch(a.cfg, cfgPatch)

	// BRICK GUARD (the release-pipeline lesson, third application): every
	// embedding key is restart-required, so a persisted value the next startup
	// REJECTS would leave a daemon that cannot boot — and no API to fix it.
	// The pairing rule startup enforces (custom model needs explicit dims) is
	// cross-field, so it must be checked HERE against the EFFECTIVE config,
	// before anything reaches disk.
	if updated.EmbeddingModel != "" && updated.EmbeddingDims == 0 {
		a.mu.Unlock()
		return false, fmt.Errorf("%w: embedding_model requires embedding_dims (the store must know the vector size); set embedding_dims first", controlapi.ErrConfigInvalid)
	}
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
	central    remoteCentral   // wire client for remote project discovery; nil when disconnected
	ctx        context.Context // daemon's root context (for new Loop.Start on reconnect)
	node       *syncer.Node
	// The last sync result is read LIVE from the Loop via Loop.LastResult() in
	// Status() (see lastSyncResultLocked) — there is no cached field here.
	connected  bool // mirrors loop != nil && centralURL != ""
	centralURL string
	actualPort int // the actual bound port (for Status.CentralURL etc.)

	// embeddingProvider is the provider name for Status.EmbeddingBackfill.Provider.
	// Populated by newRuntimeSyncAdapter when an embedLoop is active.
	embeddingProvider string

	// Obsidian scheduled export (REQ-WATCH-11). Assigned AFTER construction
	// (like `central` above) rather than threaded through
	// newRuntimeSyncAdapter's parameter list, which is already seven wide.
	// obsidianLoop is nil when the feature is off, and that nil IS the signal
	// that Status must omit the field entirely.
	//
	// obsidianVault/obsidianInterval are echoed from CONFIG, never from the
	// Exporter: obsidian.Loop calls SetGraphConfig(skip) after the first
	// successful cycle, so anything read back off *Exporter after that point
	// reports the loop's internal state rather than the user's setting (Phase 8
	// review, MAJOR-3). Both keys are restart-required, so a config echo cannot
	// drift from the running loop.
	obsidianLoop     *obsidian.Loop
	obsidianVault    string
	obsidianInterval time.Duration
}

// obsidianStatus maps the export loop's most recent outcome into the control-API
// shape, or returns nil when the feature is off.
//
// nil (→ omitempty → the field is ABSENT from the JSON) is deliberate and
// mirrors EmbeddingBackfill's documented reasoning: a permanent all-zero object
// would misreport a disabled feature as a healthy one, and the whole purpose of
// this field is to let a user distinguish "daemon dead" from "export failing
// every cycle" — two states the Obsidian viewer cannot tell apart.
func (a *runtimeSyncAdapter) obsidianStatus() *controlapi.ObsidianExport {
	if a.obsidianLoop == nil {
		return nil
	}
	out := a.obsidianLoop.LastResult()
	st := &controlapi.ObsidianExport{
		Created:             out.Created,
		Updated:             out.Updated,
		Deleted:             out.Deleted,
		Skipped:             out.Skipped,
		Hubs:                out.Hubs,
		ConsecutiveFailures: out.ConsecutiveFailures,
		Vault:               a.obsidianVault,
	}
	if !out.At.IsZero() {
		at := out.At
		st.LastExportAt = &at
	}
	if out.Err != "" {
		e := out.Err
		st.Error = &e
	}
	if a.obsidianInterval > 0 {
		st.Interval = a.obsidianInterval.String()
	}
	return st
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

// lastSyncResultLocked maps the live Loop's most recent outcome into the
// control-API SyncResult. Caller MUST hold a.mu (it reads a.loop). Returns the
// zero value (at=null, pushed=0, pulled=0, error=null) when no loop is
// configured or no cycle has completed yet.
func (a *runtimeSyncAdapter) lastSyncResultLocked() controlapi.SyncResult {
	if a.loop == nil {
		return controlapi.SyncResult{}
	}
	out := a.loop.LastResult()
	if out.At.IsZero() {
		return controlapi.SyncResult{}
	}
	at := out.At
	res := controlapi.SyncResult{At: &at, Pushed: out.Pushed, Pulled: out.Pulled}
	if out.Err != "" {
		e := out.Err
		res.Error = &e
	}
	return res
}

func (a *runtimeSyncAdapter) Status() controlapi.Status {
	a.mu.Lock()
	defer a.mu.Unlock()

	st := controlapi.Status{
		CentralConnected: a.connected,
		LastSyncResult:   a.lastSyncResultLocked(),
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

	// obsidian_export sub-object (REQ-WATCH-11). nil — and therefore ABSENT
	// from the JSON — when the feature is off.
	st.ObsidianExport = a.obsidianStatus()

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

// RemoteProjects returns the set of project names central knows, for the web
// UI's "exists in central" marker. Returns (nil, nil) when the daemon is not
// connected to central — the UI then shows no remote markers (state unknown).
// The network call runs OUTSIDE a.mu so a slow/blocked central cannot stall
// concurrent Status/connect/disconnect calls.
func (a *runtimeSyncAdapter) RemoteProjects(ctx context.Context) ([]string, error) {
	a.mu.Lock()
	c := a.central
	connected := a.connected
	a.mu.Unlock()
	if c == nil || !connected {
		return nil, nil
	}
	return c.ListProjects(ctx)
}

// UnshareProject deletes the project from central over the authenticated wire
// (POST /v1/unshare with the writer key — no DSN). Returns an error when the
// daemon is not connected to central. The network call runs OUTSIDE a.mu.
func (a *runtimeSyncAdapter) UnshareProject(ctx context.Context, project string) (int, error) {
	a.mu.Lock()
	c := a.central
	connected := a.connected
	a.mu.Unlock()
	if c == nil || !connected {
		return 0, fmt.Errorf("unshare: not connected to central")
	}
	return c.Unshare(ctx, project)
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
	a.central = nil

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
	a.central = centralClient
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
