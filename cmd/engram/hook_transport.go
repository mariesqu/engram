package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mariesqu/engram/internal/controlapi"
)

// ── daemon access ───────────────────────────────────────────────────────────

// newToolClient builds a one-shot MCP client over the resident daemon's HTTP
// transport, reusing `engram connect`'s bridge wholesale: same daemon.json
// discovery, same bearer token, same 401-refresh-and-retry on rotation. There
// is no second daemon protocol anywhere in the hooks.
//
// Unlike newMCPBridge it does NOT resolve ENGRAM_CLIENT_DIR: a hook knows the
// workspace exactly (the host puts it in the payload) and names it explicitly
// on every call, so the variable that exists to guess it has nothing to say
// here — and a stale value pointing at a deleted checkout must not take the
// hook down.
func newToolClient(dir string, timeout time.Duration) (*mcpBridge, error) {
	d, err := controlapi.ReadDaemonJSON(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: no daemon.json in %s", ErrDaemonNotRunning, dir)
		}
		return nil, fmt.Errorf("hook: read daemon.json: %w", err)
	}
	return &mcpBridge{
		dir:   dir,
		port:  d.Port,
		token: d.Token,
		http:  &http.Client{Timeout: timeout},
	}, nil
}

// hookRemaining is how much of the event's budget is left. It is what the
// hooks' HTTP clients get as their per-request Timeout: ctx is what actually
// bounds the hook, and a client handed the WHOLE budget would let a retry (the
// 401-refresh path in callTool) spend a second helping of time already gone.
//
// A context with no deadline yields the longest budget rather than zero — a
// caller that did not bound the hook did not mean "give up immediately".
func hookRemaining(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return hookBudgetSessionStart
	}
	if remaining := time.Until(deadline); remaining > 0 {
		return remaining
	}
	// Spent. Anything positive keeps http.Client from reading this as "no
	// timeout"; the context is already done, so the call fails on it regardless.
	return time.Millisecond
}

// dialHook resolves the DB path, optionally auto-starts a resident daemon, and
// returns a client for it. The client's per-request timeout is whatever is left
// of the event's budget (see hookRemaining).
//
// autostart is deliberately NOT universal. session-start and post-compaction
// may spawn a daemon (they are the first thing that runs in a session, and they
// have a budget that can absorb it; the spawn is detached, so even a hook that
// runs out of budget leaves a daemon behind for the next one). The others never
// do: spawning a SQLite owner to record the end of a session, or inside a
// 200ms prompt budget, trades the thing the user is doing for bookkeeping.
func dialHook(ctx context.Context, dbFlag string, autostart bool) (*mcpBridge, error) {
	dbPath, err := resolveConnectDBPath(dbFlag)
	if err != nil {
		return nil, err
	}
	dir := daemonDir(dbPath)

	if autostart {
		if err := ensureConnectDaemon(ctx, dir, dbPath); err != nil {
			// Not fatal on its own: a daemon may have become healthy anyway (a
			// concurrent client won the race), so try the client before giving up.
			fmt.Fprintf(os.Stderr, "engram hook: auto-start: %v\n", err)
		}
	}
	return newToolClient(dir, hookRemaining(ctx))
}

// callTool performs one MCP tools/call over the daemon's HTTP transport and
// returns the text content of the result. A tool error (isError) is returned as
// a Go error carrying the tool's own message — the hooks treat both the same
// way (log, degrade), but the distinction matters in the log.
func (b *mcpBridge) callTool(ctx context.Context, name string, args map[string]any) (string, error) {
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	})
	if err != nil {
		return "", fmt.Errorf("%s: encode request: %w", name, err)
	}

	body, status, err := b.post(ctx, frame)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if status == http.StatusUnauthorized {
		// The token rotates on every daemon restart; re-read and retry once,
		// exactly as the stdio bridge does for a forwarded frame.
		if refreshErr := b.refresh(); refreshErr != nil {
			return "", fmt.Errorf("%s: %w (stale token; %v)", name, ErrDaemonNotRunning, refreshErr)
		}
		if body, status, err = b.post(ctx, frame); err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
	}
	if status < 200 || status > 299 {
		return "", fmt.Errorf("%s: daemon returned HTTP %d", name, status)
	}
	return parseToolResult(name, body)
}

// parseToolResult extracts the text content from a tools/call response body.
//
// The body may arrive as a bare JSON-RPC object or as a Server-Sent Events
// frame ("event: message\ndata: {…}"), because the MCP Streamable HTTP
// transport is free to choose either for a request that accepts both — and the
// bridge accepts both, since a stdio client downstream wants whatever the
// daemon sent. A hook consumes the payload itself, so it has to unwrap it.
func parseToolResult(tool string, body []byte) (string, error) {
	payload := jsonFromMCPBody(body)
	if len(payload) == 0 {
		return "", fmt.Errorf("%s: empty response from daemon", tool)
	}

	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		return "", fmt.Errorf("%s: decode response: %w", tool, err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("%s: JSON-RPC error %d: %s", tool, resp.Error.Code, resp.Error.Message)
	}

	var text strings.Builder
	for _, c := range resp.Result.Content {
		if c.Type == "text" || c.Type == "" {
			text.WriteString(c.Text)
		}
	}
	if resp.Result.IsError {
		return "", fmt.Errorf("%s: %s", tool, strings.TrimSpace(text.String()))
	}
	return text.String(), nil
}

// jsonFromMCPBody returns the JSON payload of an MCP HTTP response, unwrapping
// an SSE frame when it sees one. An SSE body may carry several "data:" lines
// for one event; per the spec they concatenate with newlines.
func jsonFromMCPBody(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] == '{' || trimmed[0] == '[' {
		return trimmed
	}
	var data [][]byte
	for _, line := range bytes.Split(trimmed, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if after, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, bytes.TrimSpace(after))
		}
	}
	return bytes.Join(data, []byte("\n"))
}

// urlQueryEscape percent-encodes a query-string value. Kept local (and
// minimal) so the hook path pulls in nothing beyond what it uses.
func urlQueryEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
