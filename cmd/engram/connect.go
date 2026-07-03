package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/mariesqu/engram/internal/config"
	"github.com/mariesqu/engram/internal/controlapi"
)

const connectUsage = `Usage: engram connect [--db <path>]

Bridge stdio MCP to the resident daemon's Streamable HTTP MCP endpoint.

MCP stdio transport means each client (VS Code, Claude Code, Cursor, ...)
normally spawns its own "engram daemon" process — N clients, N SQLite owners.
"engram connect" instead reads newline-delimited JSON-RPC messages from
stdin, forwards each one over HTTP to a single resident daemon's /mcp
endpoint (engram daemon --http --transport http), and writes the response
back to stdout. Clients keep a simple stdio config; one resident daemon
serves them all.

The bearer token used to authenticate against /mcp rotates on every daemon
restart. A static Authorization header baked into a client's mcp.json would
break after a restart — "engram connect" re-reads daemon.json on a 401 and
retries once, so clients never see the rotation.

Requires a resident daemon already running with --http --transport http
(see 'engram daemon --help'). "engram connect" does not start a daemon.

Flags:
  --db   Path to the local SQLite database (required; or set ENGRAM_DB, or
         set in the config file's "db_path" key — same precedence as 'engram daemon')

On stdin EOF, connect exits 0. On SIGINT/SIGTERM it exits cleanly.
`

// maxConnectMessage is the maximum size of a single newline-delimited
// JSON-RPC message read from stdin. MCP messages are normally tiny, but
// mem_save/mem_search payloads (large content blobs, big result sets) can
// legitimately exceed the bufio.Scanner default (64KB) — 10MB gives generous
// headroom without allowing unbounded memory growth from a misbehaving client.
const maxConnectMessage = 10 * 1024 * 1024

// runConnectCmd is the entry point for `engram connect`.
func runConnectCmd(args []string) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), connectUsage) }

	db := fs.String("db", "", "path to local SQLite database (required; or set ENGRAM_DB)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("connect takes no positional arguments; unexpected: %v", fs.Args())
	}

	dbPath, err := resolveConnectDBPath(*db)
	if err != nil {
		return err
	}

	bridge, err := newMCPBridge(daemonDir(dbPath))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := bridge.preflight(ctx); err != nil {
		return err
	}

	return bridge.run(ctx, os.Stdin, os.Stdout)
}

// resolveConnectDBPath resolves the DB path with the same precedence as
// 'engram daemon': flag > ENGRAM_DB env var > config file's "db_path" key.
// (engram status / engram ui only implement flag > env; connect matches the
// daemon's full chain per spec, since it needs to find the SAME daemon.json
// the daemon started with.)
func resolveConnectDBPath(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if v := envOr("ENGRAM_DB", ""); v != "" {
		return v, nil
	}
	if configDir, cerr := config.DefaultConfigDir(); cerr == nil && configDir != "" {
		fileCfg, err := config.Load(configDir)
		if err != nil {
			return "", fmt.Errorf("connect: config file error: %w", err)
		}
		if fileCfg.DB != "" {
			return fileCfg.DB, nil
		}
	}
	return "", fmt.Errorf("--db is required (or set ENGRAM_DB, or set \"db_path\" in the config file)")
}

// mcpBridge holds the state needed to forward stdio JSON-RPC messages to a
// resident daemon's /mcp endpoint over HTTP, re-reading daemon.json once on
// a 401 to survive token rotation across daemon restarts.
type mcpBridge struct {
	dir string // directory containing daemon.json (same dir as the DB)

	mu    sync.Mutex // guards port/token (read by many goroutines, written by refresh)
	port  int
	token string

	http *http.Client

	// stdoutMu serializes writes to stdout across concurrent request goroutines
	// — one goroutine per incoming stdin message, but only one writer at a time.
	stdoutMu sync.Mutex
}

// newMCPBridge constructs an mcpBridge by reading daemon.json from dir.
// Mirrors NewControlClient, but the bridge needs raw passthrough (no JSON
// decode of the response body), so it does not reuse ControlClient directly.
func newMCPBridge(dir string) (*mcpBridge, error) {
	d, err := controlapi.ReadDaemonJSON(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no resident engram daemon found in %s: %w\n"+
				"Start one first:\n"+
				"  Windows: engram tray\n"+
				"  Any OS:  engram daemon --db <path> --http --transport http", dir, ErrDaemonNotRunning)
		}
		return nil, fmt.Errorf("connect: read daemon.json: %w", err)
	}
	return &mcpBridge{
		dir:   dir,
		port:  d.Port,
		token: d.Token,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

// snapshot returns the current port and token under lock.
func (b *mcpBridge) snapshot() (int, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.port, b.token
}

// refresh re-reads daemon.json and updates port and token. Called on 401 to
// pick up a rotated token after a daemon restart (mirrors ControlClient.refresh).
func (b *mcpBridge) refresh() error {
	d, err := controlapi.ReadDaemonJSON(b.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrDaemonNotRunning
		}
		return err
	}
	b.mu.Lock()
	b.port = d.Port
	b.token = d.Token
	b.mu.Unlock()
	return nil
}

// preflight verifies a resident daemon is reachable at /mcp with the MCP
// HTTP transport enabled BEFORE the stdio loop starts, so a misconfigured
// daemon fails fast with a clear message instead of silently swallowing the
// client's first request.
//
// A 404 means the daemon is running but was started without
// --transport http (stdio-only MCP): /mcp is never mounted in that mode
// (controlapi.MountMCP is only called when cfg.mcpTransport == "http" in
// cmd/engram/daemon.go). We do not treat any other status as fatal here,
// with one exception: a 401 means daemon.json holds a stale token — mirroring
// forward's semantics, preflight re-reads daemon.json once and retries, so a
// token that rotated (daemon restart) just before this process started is
// caught immediately rather than surfacing as a confusing per-message
// failure later. A second 401 after the refresh is fatal with an actionable
// message. Every other non-2xx status is not fatal here: an empty probe body
// is itself a protocol violation the daemon may reasonably reject with a
// non-2xx MCP-layer response, and the only other thing preflight must
// positively rule out is "route absent".
func (b *mcpBridge) preflight(ctx context.Context) error {
	status, url, err := b.probe(ctx)
	if err != nil {
		return err
	}

	if status == http.StatusUnauthorized {
		if refreshErr := b.refresh(); refreshErr != nil {
			return fmt.Errorf("connect: preflight: stale token in daemon.json and refresh failed: %w", refreshErr)
		}
		status, url, err = b.probe(ctx)
		if err != nil {
			return err
		}
		if status == http.StatusUnauthorized {
			return fmt.Errorf(
				"connect: preflight: daemon at %s rejected the token even after re-reading daemon.json (401); "+
					"the resident daemon and this client disagree on the token — restart the daemon "+
					"(engram tray, or engram daemon --db <path> --http --transport http) and retry", url)
		}
	}

	if status == http.StatusNotFound {
		return fmt.Errorf(
			"connect: daemon at %s is running WITHOUT the MCP HTTP transport (got 404 for /mcp); "+
				"restart it with: engram daemon --db <path> --http --transport http", url)
	}
	return nil
}

// probe issues a single preflight POST against /mcp with the current token
// and returns the response status code and the probed URL (for error
// messages). It is a thin, preflight-specific sibling of post: preflight
// does not need the response body, only the status.
func (b *mcpBridge) probe(ctx context.Context) (int, string, error) {
	port, token := b.snapshot()
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return 0, url, fmt.Errorf("connect: preflight request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := b.http.Do(req)
	if err != nil {
		return 0, url, fmt.Errorf("connect: preflight: cannot reach daemon at %s: %w", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.StatusCode, url, nil
}

// run drives the main stdio<->HTTP loop: read newline-delimited JSON-RPC
// messages from r, POST each to /mcp, write the response body followed by a
// newline to w. One goroutine is spawned per request; stdout writes are
// serialized via stdoutMu so concurrent responses never interleave.
//
// A single message's request failure (network error, non-2xx/non-401
// status, stdout write error) is logged to stderr and otherwise ignored — it
// must not take down the whole bridge, since every other client's in-flight
// request is unrelated. Bridge-fatal is per-message, not a global streak: if
// a message's retried request, after a daemon.json refresh, still receives
// 401, the token this process holds can never become valid again without
// operator intervention, so continuing to serve would just retry-loop
// forever on every subsequent message. forward() signals this case by
// wrapping the returned error in ErrDaemonNotRunning (see isFatalConnectErr).
//
// Returns nil on stdin EOF or context cancellation (SIGINT/SIGTERM); a fatal
// error is returned to the caller, which exits the process non-zero.
func (b *mcpBridge) run(ctx context.Context, r io.Reader, w io.Writer) error {
	reader := bufio.NewReaderSize(r, 64*1024)

	var wg sync.WaitGroup
	var fatal error
	var fatalMu sync.Mutex
	recordFatal := func(err error) {
		if err == nil {
			return
		}
		fatalMu.Lock()
		if fatal == nil {
			fatal = err
		}
		fatalMu.Unlock()
	}

	dispatch := func(msg []byte) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := b.forward(ctx, msg, w)
			if err == nil {
				return
			}
			if isFatalConnectErr(err) {
				recordFatal(err)
				return
			}
			fmt.Fprintf(os.Stderr, "engram connect: request failed: %v\n", err)
		}()
	}

	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(done)
	}()

readLoop:
	for {
		select {
		case <-done:
			break readLoop
		default:
		}

		line, err := readMessage(reader, maxConnectMessage)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(line) > 0 {
					dispatch(append([]byte(nil), line...))
				}
				break readLoop
			}
			if errors.Is(err, errOversizeMessage) {
				// readMessage has already drained the rest of the offending
				// line internally, so the reader is correctly positioned at
				// the start of the next message — just log and continue.
				fmt.Fprintf(os.Stderr, "engram connect: skipping oversized message: %v\n", err)
				continue
			}
			// Any other read error (e.g. a closed/broken stdin) is not
			// recoverable — framing cannot be trusted, so the loop ends.
			fmt.Fprintf(os.Stderr, "engram connect: read stdin: %v\n", err)
			break readLoop
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue // blank line — nothing to forward
		}

		dispatch(append([]byte(nil), line...)) // own copy: reader buffer is reused
	}

	// Best-effort grace period: give in-flight requests a moment to finish
	// rather than abandoning them mid-flight, but never block shutdown
	// indefinitely on a stuck request.
	waitDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
	}

	if fatal != nil {
		return fatal
	}
	return nil // covers both clean EOF and ctx cancellation (SIGINT/SIGTERM)
}

// isFatalConnectErr reports whether err represents the bridge-fatal
// condition described in run's doc comment: a message's retried request,
// after a daemon.json refresh attempt, still receives 401. forward() wraps
// both fatal sub-cases (refresh itself failed, or refresh succeeded but the
// retried request was STILL rejected) with ErrDaemonNotRunning — every other
// error returned by forward/post (network errors, non-401 non-2xx statuses,
// stdout write failures) is a per-message failure only and must not match
// here.
func isFatalConnectErr(err error) bool {
	return errors.Is(err, ErrDaemonNotRunning)
}

// errOversizeMessage is returned by readMessage when a single
// newline-delimited message exceeds maxLen before its terminating '\n' was
// seen. It is a per-message condition, not a stream-fatal one: the caller
// drains the remainder of the offending line and resumes reading the next
// message (see run's readLoop).
var errOversizeMessage = errors.New("message exceeds max size")

// readMessage reads a single newline-delimited message, trimming the
// trailing delimiter. It reads incrementally via ReadSlice so accumulation
// genuinely stops at maxLen — at most maxLen bytes are ever buffered for a
// single message, even for a pathological/misbehaving client that never
// sends a newline, because the full (unbounded) line is never materialized
// before the cap is checked.
//
// On overflow it returns errOversizeMessage after draining the remainder of
// the offending line itself (up to and including its '\n', or EOF), so the
// reader is always left correctly positioned at the start of the next
// message — the caller does not need to do any additional framing recovery.
func readMessage(r *bufio.Reader, maxLen int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		foundNewline := err == nil
		if len(buf)+len(chunk) > maxLen {
			// Oversize: the line (with or without '\n' in this chunk) will
			// never be returned to the caller. If '\n' hasn't been seen yet,
			// drain the rest of it now so the reader ends up positioned
			// right after the line's terminating '\n' (or at EOF) — exactly
			// where the next readMessage call needs to start.
			if !foundNewline {
				if drainErr := drainLine(r); drainErr != nil && !errors.Is(drainErr, io.EOF) {
					return nil, drainErr
				}
			}
			return nil, fmt.Errorf("%w (%d bytes)", errOversizeMessage, maxLen)
		}
		buf = append(buf, chunk...)
		if foundNewline {
			// ReadSlice found '\n' within its internal buffer.
			return bytes.TrimRight(buf, "\r\n"), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			// No '\n' yet within the internal buffer; keep accumulating
			// (still within maxLen, checked above) and read more.
			continue
		}
		// io.EOF or another read error: return what was accumulated so far,
		// same contract as bufio.Reader.ReadBytes.
		return bytes.TrimRight(buf, "\r\n"), err
	}
}

// drainLine discards input up to and including the next '\n'. Used
// internally by readMessage to resync framing after detecting an oversized
// line whose terminating '\n' has not yet been seen.
func drainLine(r *bufio.Reader) error {
	for {
		_, err := r.ReadSlice('\n')
		if err == nil {
			return nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return err
	}
}

// forward POSTs a single JSON-RPC message to /mcp and writes the response to
// w. On 401 it re-reads daemon.json once (token rotation) and retries; a
// second 401 is a hard error. A 202/empty body (client notification — no
// "id" field, so no response is expected) writes nothing to w, per the MCP
// Streamable HTTP spec.
func (b *mcpBridge) forward(ctx context.Context, msg []byte, w io.Writer) error {
	body, status, err := b.post(ctx, msg)
	if err != nil {
		return err
	}

	if status == http.StatusUnauthorized {
		if refreshErr := b.refresh(); refreshErr != nil {
			return fmt.Errorf("%w (stale token; %v)", ErrDaemonNotRunning, refreshErr)
		}
		body, status, err = b.post(ctx, msg)
		if err != nil {
			return err
		}
		if status == http.StatusUnauthorized {
			return fmt.Errorf("%w (token mismatch after re-read)", ErrDaemonNotRunning)
		}
	}

	if status == http.StatusAccepted || len(bytes.TrimSpace(body)) == 0 {
		// Notification: no "id" in the request, no response expected. Write nothing.
		return nil
	}

	b.stdoutMu.Lock()
	defer b.stdoutMu.Unlock()
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("connect: write stdout: %w", err)
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return fmt.Errorf("connect: write stdout: %w", err)
	}
	return nil
}

// post issues a single POST of msg to /mcp with the current token and
// returns the raw response body and status code.
func (b *mcpBridge) post(ctx context.Context, msg []byte) ([]byte, int, error) {
	port, token := b.snapshot()
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(msg))
	if err != nil {
		return nil, 0, fmt.Errorf("connect: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := b.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("connect: request to daemon: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("connect: read response: %w", err)
	}
	return body, resp.StatusCode, nil
}
