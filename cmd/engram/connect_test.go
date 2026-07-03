package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/controlapi"
)

// TestMCPBridge_MissingDaemonJSON verifies that newMCPBridge returns a clear,
// wrapped ErrDaemonNotRunning error when no daemon.json exists in the
// directory — the "no resident daemon" preflight failure from the spec.
func TestMCPBridge_MissingDaemonJSON(t *testing.T) {
	dir := t.TempDir()
	_, err := newMCPBridge(dir)
	if err == nil {
		t.Fatal("want error for missing daemon.json, got nil")
	}
	if !errors.Is(err, ErrDaemonNotRunning) {
		t.Errorf("want ErrDaemonNotRunning, got: %v", err)
	}
	if !strings.Contains(err.Error(), "engram tray") || !strings.Contains(err.Error(), "engram daemon") {
		t.Errorf("error should name both remedies (tray, daemon --http --transport http); got: %v", err)
	}
}

// TestMCPBridge_Preflight_404MeansTransportOff verifies that a 404 response
// from /mcp (route absent — daemon running without --transport http) is
// reported as a clear, actionable error naming the remedy.
func TestMCPBridge_Preflight_404MeansTransportOff(t *testing.T) {
	dir := t.TempDir()
	const token = "tok"

	ts := httptest.NewServer(http.NewServeMux()) // no /mcp route registered → 404
	defer ts.Close()

	port := mustParsePort(t, ts.URL)
	if err := controlapi.WriteDaemonJSON(dir, token, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	b, err := newMCPBridge(dir)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}

	err = b.preflight(context.Background())
	if err == nil {
		t.Fatal("want error for 404 /mcp, got nil")
	}
	if !strings.Contains(err.Error(), "--transport http") {
		t.Errorf("404 error should name the --transport http remedy; got: %v", err)
	}
}

// TestMCPBridge_Preflight_OK verifies that a non-404 response from /mcp
// (transport enabled) passes preflight.
func TestMCPBridge_Preflight_OK(t *testing.T) {
	dir := t.TempDir()
	const token = "tok"

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	port := mustParsePort(t, ts.URL)
	if err := controlapi.WriteDaemonJSON(dir, token, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	b, err := newMCPBridge(dir)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}
	if err := b.preflight(context.Background()); err != nil {
		t.Errorf("preflight: want nil, got %v", err)
	}
}

// TestMCPBridge_Preflight_401ThenRetry verifies that preflight catches a
// token that is already stale at startup: a 401 on the first probe triggers
// a re-read of daemon.json, and the retried probe with the refreshed token
// succeeds — mirroring forward's 401-then-retry semantics, but at startup
// instead of mid-session.
func TestMCPBridge_Preflight_401ThenRetry(t *testing.T) {
	dir := t.TempDir()
	const staleToken = "stale-preflight-token"
	const freshToken = "fresh-preflight-token"

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+freshToken {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	port := mustParsePort(t, ts.URL)
	if err := controlapi.WriteDaemonJSON(dir, staleToken, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON (stale): %v", err)
	}

	b, err := newMCPBridge(dir)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}

	// Rotate daemon.json mid-test: the daemon restarted between when this
	// process cached the stale token and when preflight runs.
	if err := controlapi.WriteDaemonJSON(dir, freshToken, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON (fresh): %v", err)
	}

	if err := b.preflight(context.Background()); err != nil {
		t.Errorf("preflight: want nil (refresh should pick up the fresh token), got %v", err)
	}
}

// TestMCPBridge_Preflight_AlwaysUnauthorized_Fails verifies that preflight
// fails with a clear, actionable error when the daemon rejects the token
// even after the refresh-and-retry (a second 401), rather than proceeding
// into the stdio loop with a token that can never work.
func TestMCPBridge_Preflight_AlwaysUnauthorized_Fails(t *testing.T) {
	dir := t.TempDir()
	const badToken = "always-bad-preflight-token"

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	port := mustParsePort(t, ts.URL)
	if err := controlapi.WriteDaemonJSON(dir, badToken, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	b, err := newMCPBridge(dir)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}

	err = b.preflight(context.Background())
	if err == nil {
		t.Fatal("want error when daemon always returns 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("preflight error should be clear about the 401 rejection; got: %v", err)
	}
}

// TestMCPBridge_HappyRoundTrip drives a single JSON-RPC message through
// run() against an httptest server and verifies the raw response body is
// written to stdout followed by a newline.
func TestMCPBridge_HappyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	const token = "round-trip-token"

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := readAll(r)
		if !strings.Contains(string(body), `"method":"ping"`) {
			t.Errorf("server did not receive expected request body; got %q", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	port := mustParsePort(t, ts.URL)
	if err := controlapi.WriteDaemonJSON(dir, token, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	b, err := newMCPBridge(dir)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	var out bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := b.run(ctx, in, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, `"result":{"ok":true}`) {
		t.Errorf("stdout output: got %q, want it to contain the daemon's response body", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("stdout output must end with a newline; got %q", got)
	}
}

// TestMCPBridge_401ThenRetry verifies that a 401 on the first attempt
// triggers a re-read of daemon.json and a single retry with the refreshed
// token, succeeding on the second attempt (token rotation mid-session).
func TestMCPBridge_401ThenRetry(t *testing.T) {
	dir := t.TempDir()
	const staleToken = "stale-mcp-token"
	const freshToken = "fresh-mcp-token"

	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("Authorization") == "Bearer "+freshToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	port := mustParsePort(t, ts.URL)
	if err := controlapi.WriteDaemonJSON(dir, staleToken, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON (stale): %v", err)
	}

	b, err := newMCPBridge(dir)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}

	// Simulate a daemon restart: rotate the token file AFTER the bridge has
	// already cached the stale one but BEFORE it forwards the message.
	if err := controlapi.WriteDaemonJSON(dir, freshToken, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON (fresh): %v", err)
	}

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	var out bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := b.run(ctx, in, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("call count: got %d, want 2 (initial 401 + retry with fresh token)", got)
	}
	if !strings.Contains(out.String(), `"result":{"ok":true}`) {
		t.Errorf("stdout after retry: got %q, want success response", out.String())
	}
}

// TestMCPBridge_DoubleStale401_Errors verifies that two consecutive 401s
// (daemon restarted again, or genuinely misconfigured) produce a clear
// non-nil error wrapping ErrDaemonNotRunning, rather than looping forever.
func TestMCPBridge_DoubleStale401_Errors(t *testing.T) {
	dir := t.TempDir()
	const badToken = "always-bad-token"

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	port := mustParsePort(t, ts.URL)
	if err := controlapi.WriteDaemonJSON(dir, badToken, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	b, err := newMCPBridge(dir)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	var out bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = b.run(ctx, in, &out)
	if err == nil {
		t.Fatal("want error after double 401, got nil")
	}
	if !errors.Is(err, ErrDaemonNotRunning) {
		t.Errorf("want ErrDaemonNotRunning, got: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout should be empty after a failed round-trip; got %q", out.String())
	}
}

// TestMCPBridge_Notification_WritesNothing verifies that a 202 Accepted
// response (client notification — no "id", no response expected per the MCP
// Streamable HTTP spec) writes nothing to stdout.
func TestMCPBridge_Notification_WritesNothing(t *testing.T) {
	dir := t.TempDir()
	const token = "notif-token"

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	port := mustParsePort(t, ts.URL)
	if err := controlapi.WriteDaemonJSON(dir, token, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	b, err := newMCPBridge(dir)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}

	in := strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	var out bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := b.run(ctx, in, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("notification round-trip should write nothing to stdout; got %q", out.String())
	}
}

// TestMCPBridge_LargeMessage_Survives verifies that a message well over the
// 64KB bufio.Scanner default (but under maxConnectMessage) survives framing
// intact — proves the reader uses a growable buffer, not a fixed-size Scanner.
func TestMCPBridge_LargeMessage_Survives(t *testing.T) {
	dir := t.TempDir()
	const token = "large-msg-token"

	// > 64KB payload embedded in the JSON-RPC params.
	bigPayload := strings.Repeat("A", 70*1024)

	var receivedLen int64
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		atomic.StoreInt64(&receivedLen, int64(len(body)))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	port := mustParsePort(t, ts.URL)
	if err := controlapi.WriteDaemonJSON(dir, token, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	b, err := newMCPBridge(dir)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}

	msg := `{"jsonrpc":"2.0","id":1,"method":"mem_save","params":{"content":"` + bigPayload + `"}}`
	in := strings.NewReader(msg + "\n")
	var out bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := b.run(ctx, in, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := atomic.LoadInt64(&receivedLen); got != int64(len(msg)) {
		t.Errorf("server received %d bytes, want %d (message must survive framing intact)", got, len(msg))
	}
	if !strings.Contains(out.String(), `"result":{"ok":true}`) {
		t.Errorf("stdout after large message: got %q, want success response", out.String())
	}
}

// TestMCPBridge_OversizedLine_SkippedAndBridgeSurvives verifies that a single
// newline-delimited line exceeding maxConnectMessage does not kill the
// bridge: it is logged to stderr and skipped (framing resynced by draining
// the rest of the offending line), and a subsequent valid message on the
// same stdin stream is still forwarded and answered normally.
func TestMCPBridge_OversizedLine_SkippedAndBridgeSurvives(t *testing.T) {
	dir := t.TempDir()
	const token = "oversize-token"

	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		body, _ := readAll(r)
		if !strings.Contains(string(body), `"method":"ping"`) {
			t.Errorf("server did not receive expected request body; got %q", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"ok":true}}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	port := mustParsePort(t, ts.URL)
	if err := controlapi.WriteDaemonJSON(dir, token, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	b, err := newMCPBridge(dir)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}

	// One line well over maxConnectMessage (10MB), followed by a valid
	// message on the next line.
	oversizedLine := strings.Repeat("A", maxConnectMessage+1024)
	validMsg := `{"jsonrpc":"2.0","id":2,"method":"ping"}`
	in := strings.NewReader(oversizedLine + "\n" + validMsg + "\n")
	var out bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var runErr error
	stderr := captureStderr(t, func() {
		runErr = b.run(ctx, in, &out)
	})
	if runErr != nil {
		t.Fatalf("run: %v", runErr)
	}

	if !strings.Contains(stderr, "engram connect") || !strings.Contains(stderr, "oversized") {
		t.Errorf("stderr should log the skipped oversized message; got %q", stderr)
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("daemon call count: got %d, want 1 (only the valid message should be forwarded)", got)
	}

	got := out.String()
	if got != `{"jsonrpc":"2.0","id":2,"result":{"ok":true}}`+"\n" {
		t.Errorf("stdout: got %q, want only the valid message's response", got)
	}
}

// TestMCPBridge_StdinEOF_ExitsCleanly verifies that an empty stdin (immediate
// EOF, no messages) causes run() to return nil.
func TestMCPBridge_StdinEOF_ExitsCleanly(t *testing.T) {
	dir := t.TempDir()
	const token = "eof-token"

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		t.Error("daemon should not receive any request when stdin is empty")
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	port := mustParsePort(t, ts.URL)
	if err := controlapi.WriteDaemonJSON(dir, token, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	b, err := newMCPBridge(dir)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}

	in := strings.NewReader("")
	var out bytes.Buffer

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := b.run(ctx, in, &out); err != nil {
		t.Fatalf("run on empty stdin: want nil, got %v", err)
	}
}

// TestCLI_Connect_DBFlagWins verifies --db takes precedence over ENGRAM_DB,
// matching the flag > env > config-file precedence used by 'engram daemon'
// (resolveConnectDBPath matches the daemon's full chain, not status.go's
// flag > env only — connect needs to find the SAME daemon.json the daemon
// started with, which the daemon may have resolved from the config file).
func TestCLI_Connect_DBFlagWins(t *testing.T) {
	envDir := t.TempDir()
	t.Setenv("ENGRAM_DB", filepath.Join(envDir, "env.db"))
	t.Setenv("ENGRAM_CONFIG_DIR", t.TempDir()) // empty dir → no config.json

	flagDir := t.TempDir()
	flagDB := filepath.Join(flagDir, "flag.db")

	err := runConnectCmd([]string{"--db", flagDB})
	if err == nil {
		t.Fatal("want error for no daemon, got nil")
	}
	if !errors.Is(err, ErrDaemonNotRunning) {
		t.Errorf("want ErrDaemonNotRunning, got: %v", err)
	}
	// The error names the directory it looked in — assert it used the flag's
	// directory (flagDir), not the env var's directory (envDir).
	if !strings.Contains(err.Error(), flagDir) {
		t.Errorf("error should reference the --db directory %q (flag must win over env); got: %v", flagDir, err)
	}
}

// TestCLI_Connect_DBEnvFallback verifies ENGRAM_DB is used when --db is omitted.
func TestCLI_Connect_DBEnvFallback(t *testing.T) {
	envDir := t.TempDir()
	t.Setenv("ENGRAM_DB", filepath.Join(envDir, "env.db"))
	t.Setenv("ENGRAM_CONFIG_DIR", t.TempDir()) // empty dir → no config.json

	err := runConnectCmd(nil)
	if err == nil {
		t.Fatal("want error for no daemon, got nil")
	}
	if !errors.Is(err, ErrDaemonNotRunning) {
		t.Errorf("want ErrDaemonNotRunning, got: %v", err)
	}
	if !strings.Contains(err.Error(), envDir) {
		t.Errorf("error should reference the ENGRAM_DB directory %q; got: %v", envDir, err)
	}
}

// TestCLI_Connect_MissingDBErrors verifies a clear error when neither --db,
// ENGRAM_DB, nor the config file's "db_path" key supplies a path.
func TestCLI_Connect_MissingDBErrors(t *testing.T) {
	t.Setenv("ENGRAM_DB", "")
	t.Setenv("ENGRAM_CONFIG_DIR", t.TempDir()) // empty dir → no config.json → no fallback
	err := runConnectCmd(nil)
	if err == nil {
		t.Fatal("want error when no db path is resolvable, got nil")
	}
	if !strings.Contains(err.Error(), "--db is required") {
		t.Errorf("error should mention --db; got: %v", err)
	}
}

// TestResolveConnectDBPath_ConfigFileFallback verifies the third precedence
// tier: when neither --db nor ENGRAM_DB is set, the config file's "db_path"
// key is used (matching 'engram daemon' — see cmd/engram/daemon.go's
// "Resolve DB path: flag > env > file" chain).
func TestResolveConnectDBPath_ConfigFileFallback(t *testing.T) {
	t.Setenv("ENGRAM_DB", "")
	configDir := t.TempDir()
	t.Setenv("ENGRAM_CONFIG_DIR", configDir)

	wantDB := filepath.Join(t.TempDir(), "from-config-file.db")
	configJSON := `{"db_path":"` + strings.ReplaceAll(wantDB, `\`, `\\`) + `"}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(configJSON), 0o600); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	got, err := resolveConnectDBPath("")
	if err != nil {
		t.Fatalf("resolveConnectDBPath: %v", err)
	}
	if got != wantDB {
		t.Errorf("got %q, want config file's db value %q", got, wantDB)
	}
}

// TestCLI_Connect_NoDaemon verifies that `engram connect` returns
// ErrDaemonNotRunning when no daemon.json is present.
func TestCLI_Connect_NoDaemon(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	err := runConnectCmd([]string{"--db", dbPath})
	if err == nil {
		t.Fatal("want error for no daemon, got nil")
	}
	if !errors.Is(err, ErrDaemonNotRunning) {
		t.Errorf("want ErrDaemonNotRunning, got: %v", err)
	}
}

// ─── test helpers ───────────────────────────────────────────────────────────

// mustParsePort extracts the TCP port from an httptest.Server URL, failing
// the test on error. Reuses parseTestServerURL/parsePort from controlclient_test.go.
func mustParsePort(t *testing.T, rawURL string) int {
	t.Helper()
	var port int
	if _, err := parseTestServerURL(rawURL, &port); err != nil {
		t.Fatalf("parse test server URL %q: %v", rawURL, err)
	}
	return port
}

// readAll reads and returns the full request body.
func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	br := bufio.NewReader(r.Body)
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(br); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
