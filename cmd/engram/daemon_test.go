package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/wireauth"
	"github.com/mark3labs/mcp-go/mcp"
)

// ─── Flag validation: run() dispatch ────────────────────────────────────────

// TestServeErr verifies that serveErr treats nil and context.Canceled (a clean
// shutdown) as success, and surfaces only genuine errors. On SIGINT/SIGTERM the
// daemon's signal.NotifyContext (runDaemonCmd) cancels the ctx passed to
// StdioServer.Listen, which returns context.Canceled. Guards the daemon exit
// code: a normal signal stop must exit 0, not 1.
func TestServeErr(t *testing.T) {
	if err := serveErr(nil); err != nil {
		t.Errorf("serveErr(nil) = %v, want nil", err)
	}
	if err := serveErr(context.Canceled); err != nil {
		t.Errorf("serveErr(context.Canceled) = %v, want nil (clean signal shutdown)", err)
	}
	if err := serveErr(fmt.Errorf("listen: %w", context.Canceled)); err != nil {
		t.Errorf("serveErr(wrapped context.Canceled) = %v, want nil", err)
	}
	realErr := errors.New("broken pipe")
	got := serveErr(realErr)
	if got == nil || !errors.Is(got, realErr) {
		t.Errorf("serveErr(realErr) = %v, want a non-nil error wrapping realErr", got)
	}
}

// TestRunDaemon_CtxCancelUnblocks proves that ctx cancellation drives Listen to
// return, unblocking runDaemonWithIO even when stdin never closes.  A blocking
// io.Pipe read end is passed as stdin; the goroutine should unblock within 5s of
// ctx being cancelled and return nil (context.Canceled → serveErr → nil).
func TestRunDaemon_CtxCancelUnblocks(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "d.db")
	cfg := daemonCfg{
		db:           dbPath,
		centralURL:   "", // local-only
		syncInterval: 30 * time.Second,
	}

	// A blocking stdin: pr never has data written to it, so reads block
	// indefinitely.  pw is closed by defer so the goroutine's blocked Read
	// eventually sees EOF if the ctx path is broken — but the assertion below
	// expects nil within 5s, so the ctx path must fire first.
	pr, pw := io.Pipe()
	defer pw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errc := make(chan error, 1)
	go func() {
		errc <- runDaemonWithIO(ctx, cfg, pr, io.Discard)
	}()

	// Cancel the context; Listen must return context.Canceled which serveErr maps to nil.
	cancel()

	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("runDaemonWithIO after ctx cancel: got %v, want nil (ctx not wired — serve did not unblock)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemonWithIO did not return within 5s after ctx cancel — ctx not wired into serve")
	}
}

// TestRun_DaemonNegativeSyncInterval verifies that a negative --sync-interval
// value (e.g. -5s) is rejected with exit code 1.  A zero value is mapped to the
// 30s default before this check, so only negatives reach the guard.
func TestRun_DaemonNegativeSyncInterval(t *testing.T) {
	t.Setenv("ENGRAM_DB", "")
	t.Setenv("ENGRAM_CENTRAL_URL", "")
	code := run([]string{"daemon", "--db", "x", "--sync-interval", "-5s"})
	if code != 1 {
		t.Errorf("run([daemon --sync-interval -5s]): got exit code %d, want 1", code)
	}
}

// TestRun_DaemonMissingDB verifies that 'daemon' with no --db and no ENGRAM_DB
// returns exit code 1 (the "db required" validation error).
func TestRun_DaemonMissingDB(t *testing.T) {
	t.Setenv("ENGRAM_DB", "")
	t.Setenv("ENGRAM_CENTRAL_URL", "")
	code := run([]string{"daemon"})
	if code != 1 {
		t.Errorf("run([daemon]) with no db: got exit code %d, want 1", code)
	}
}

// TestRun_DaemonCentralURLMissingWriterID verifies that providing --central-url
// without --writer-id returns exit code 1.
func TestRun_DaemonCentralURLMissingWriterID(t *testing.T) {
	t.Setenv("ENGRAM_DB", t.TempDir()+"/test.db")
	t.Setenv("ENGRAM_CENTRAL_URL", "http://localhost:8080")
	t.Setenv("ENGRAM_WRITER_ID", "")
	t.Setenv("ENGRAM_WRITER_KEY", "")
	code := run([]string{"daemon"})
	if code != 1 {
		t.Errorf("run([daemon]) central-url without writer-id: got exit code %d, want 1", code)
	}
}

// TestRun_DaemonCentralURLMissingWriterKey verifies that providing
// --central-url and --writer-id but no ENGRAM_WRITER_KEY returns exit code 1.
func TestRun_DaemonCentralURLMissingWriterKey(t *testing.T) {
	t.Setenv("ENGRAM_DB", t.TempDir()+"/test.db")
	t.Setenv("ENGRAM_CENTRAL_URL", "http://localhost:8080")
	t.Setenv("ENGRAM_WRITER_ID", "writer-x")
	t.Setenv("ENGRAM_WRITER_KEY", "") // explicitly unset
	code := run([]string{"daemon"})
	if code != 1 {
		t.Errorf("run([daemon]) central-url without writer-key: got exit code %d, want 1", code)
	}
}

// TestRun_DaemonWriterKeyInvalidHex verifies that a non-hex ENGRAM_WRITER_KEY
// returns exit code 1.
func TestRun_DaemonWriterKeyInvalidHex(t *testing.T) {
	t.Setenv("ENGRAM_DB", t.TempDir()+"/test.db")
	t.Setenv("ENGRAM_CENTRAL_URL", "http://localhost:8080")
	t.Setenv("ENGRAM_WRITER_ID", "writer-x")
	t.Setenv("ENGRAM_WRITER_KEY", "not-valid-hex!!")
	code := run([]string{"daemon"})
	if code != 1 {
		t.Errorf("run([daemon]) invalid hex writer-key: got exit code %d, want 1", code)
	}
}

// TestRun_DaemonWriterKeyWrongLength verifies that an ENGRAM_WRITER_KEY with
// the correct hex encoding but wrong decoded byte length returns exit code 1.
func TestRun_DaemonWriterKeyWrongLength(t *testing.T) {
	t.Setenv("ENGRAM_DB", t.TempDir()+"/test.db")
	t.Setenv("ENGRAM_CENTRAL_URL", "http://localhost:8080")
	t.Setenv("ENGRAM_WRITER_ID", "writer-x")
	// 16 bytes hex-encoded — valid hex but wrong length (want 32)
	shortKey := hex.EncodeToString(make([]byte, 16))
	t.Setenv("ENGRAM_WRITER_KEY", shortKey)
	code := run([]string{"daemon"})
	if code != 1 {
		t.Errorf("run([daemon]) wrong-length writer-key: got exit code %d, want 1", code)
	}
}

// TestRun_DaemonWriterKeyTrimmedBeforeDecode proves ENGRAM_WRITER_KEY is trimmed
// before hex decoding: a valid 16-byte hex value with a trailing newline reaches
// the LENGTH check rather than failing hex decoding (which an untrimmed newline
// would). runDaemonCmd is called directly so the returned error can be inspected
// (run() logs via log.Printf, which does not go through the swapped os.Stderr).
func TestRun_DaemonWriterKeyTrimmedBeforeDecode(t *testing.T) {
	t.Setenv("ENGRAM_DB", t.TempDir()+"/test.db")
	t.Setenv("ENGRAM_CENTRAL_URL", "http://localhost:8080")
	t.Setenv("ENGRAM_WRITER_ID", "writer-x")
	t.Setenv("ENGRAM_WRITER_KEY", hex.EncodeToString(make([]byte, 16))+"\n")

	err := runDaemonCmd([]string{})
	if err == nil {
		t.Fatal("expected a wrong-length error, got nil")
	}
	if strings.Contains(err.Error(), "not valid hex") {
		t.Errorf("trailing newline not trimmed (hex decode failed): %v", err)
	}
	if !strings.Contains(err.Error(), "wrong length") {
		t.Errorf("expected wrong-length error (trim + decode succeeded), got: %v", err)
	}
}

// TestRun_DaemonExtraPositional verifies that extra positional arguments to
// 'daemon' return exit code 1 (rejected before opening the store).
func TestRun_DaemonExtraPositional(t *testing.T) {
	t.Setenv("ENGRAM_DB", "")
	code := run([]string{"daemon", "--db", "/tmp/test.db", "unexpected"})
	if code != 1 {
		t.Errorf("run([daemon ... unexpected]): got exit code %d, want 1", code)
	}
}

// TestRun_DaemonHelp verifies that 'daemon --help' returns exit code 0.
func TestRun_DaemonHelp(t *testing.T) {
	code := run([]string{"daemon", "--help"})
	if code != 0 {
		t.Errorf("run([daemon --help]): got exit code %d, want 0", code)
	}
}

// ─── Credential no-leak regression ───────────────────────────────────────────

// TestRun_DaemonHelp_DoesNotLeakWriterKey is a mandatory regression guard
// mirroring TestRun_KeysProvisionHelp_DoesNotLeakDSN.  It proves that
// 'daemon --help' never prints the ENGRAM_WRITER_KEY secret, because the key
// is an env-only credential resolved AFTER flag.Parse and is never registered
// as a flag (and thus never passed to PrintDefaults).
func TestRun_DaemonHelp_DoesNotLeakWriterKey(t *testing.T) {
	const secret = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	t.Setenv("ENGRAM_WRITER_KEY", secret)

	out := captureStderr(t, func() {
		if code := run([]string{"daemon", "--help"}); code != 0 {
			t.Errorf("daemon --help: exit code %d, want 0", code)
		}
	})

	if strings.Contains(out, secret) || strings.Contains(out, "aabbccddeeff") {
		t.Errorf("daemon --help leaked ENGRAM_WRITER_KEY:\n%s", out)
	}
	// Sanity: the usage mentions the key env var by name (just not its value).
	if !strings.Contains(out, "ENGRAM_WRITER_KEY") {
		t.Errorf("daemon --help should mention ENGRAM_WRITER_KEY env var; got:\n%s", out)
	}
}

// ─── buildDaemon wiring tests ─────────────────────────────────────────────────

// TestBuildDaemon_LocalOnly verifies that buildDaemon with a valid SQLite path
// and no central URL:
//   - Opens the store without error.
//   - Returns a non-nil MCP server.
//   - Returns a nil Loop (local-only mode).
//   - A trivial SearchMemories call (FTS5 available) succeeds.
func TestBuildDaemon_LocalOnly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "engram.db")
	cfg := daemonCfg{
		db:           dbPath,
		centralURL:   "", // local-only
		syncInterval: 30 * time.Second,
	}

	components, err := buildDaemon(cfg)
	if err != nil {
		t.Fatalf("buildDaemon local-only: unexpected error: %v", err)
	}
	t.Cleanup(components.Close)

	if components.store == nil {
		t.Fatal("buildDaemon: store is nil")
	}
	if components.mcpServer == nil {
		t.Fatal("buildDaemon: mcpServer is nil")
	}
	if components.loop != nil {
		t.Fatal("buildDaemon local-only: loop should be nil, got non-nil")
	}

	// Verify FTS5 is available: SearchMemories on an empty store returns no
	// results rather than an error.
	results, err := components.store.SearchMemories("test query", "test-project", 5)
	if err != nil {
		t.Errorf("buildDaemon: FTS5 SearchMemories failed: %v", err)
	}
	// Empty store → zero results.
	if len(results) != 0 {
		t.Errorf("buildDaemon: expected 0 results from empty store, got %d", len(results))
	}
}

// TestBuildDaemon_WithCentral verifies that buildDaemon with a valid central
// URL and a correct writer key returns a non-nil Loop.  It does NOT start the
// loop or make network calls — the central URL can be a dummy value.
func TestBuildDaemon_WithCentral(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "engram.db")

	// Generate a valid 32-byte HMAC key.
	key, err := wireauth.NewKey()
	if err != nil {
		t.Fatalf("wireauth.NewKey: %v", err)
	}

	cfg := daemonCfg{
		db:           dbPath,
		centralURL:   "http://localhost:19999", // dummy — no server running
		writerID:     "test-writer",
		writerKey:    key,
		syncInterval: 30 * time.Second,
	}

	components, err := buildDaemon(cfg)
	if err != nil {
		t.Fatalf("buildDaemon with-central: unexpected error: %v", err)
	}
	t.Cleanup(components.Close)

	if components.store == nil {
		t.Fatal("buildDaemon: store is nil")
	}
	if components.mcpServer == nil {
		t.Fatal("buildDaemon: mcpServer is nil")
	}
	if components.loop == nil {
		t.Fatal("buildDaemon with-central: loop should be non-nil, got nil")
	}
}

// TestBuildDaemon_MCPServerTools verifies that the MCP server built by
// buildDaemon registers exactly the five tools introduced through PR4:
// mem_session_start, mem_session_end, mem_save, mem_get_observation,
// mem_session_summary.
//
// Mechanism: mcpserver.MCPServer.ListTools() returns the registered tool map
// directly.  Asserting the exact key set ensures no accidental additions and
// no missing tools.
func TestBuildDaemon_MCPServerTools(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "engram.db")
	cfg := daemonCfg{
		db:           dbPath,
		syncInterval: 30 * time.Second,
	}

	components, err := buildDaemon(cfg)
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	if components.mcpServer == nil {
		t.Fatal("MCP server must be non-nil")
	}

	tools := components.mcpServer.ListTools()

	wantTools := []string{
		"mem_session_start",
		"mem_session_end",
		"mem_save",
		"mem_get_observation",
		"mem_session_summary",
	}
	if len(tools) != len(wantTools) {
		names := make([]string, 0, len(tools))
		for n := range tools {
			names = append(names, n)
		}
		t.Errorf("MCP server: got %d tools %v, want exactly %d: %v",
			len(tools), names, len(wantTools), wantTools)
		return
	}
	for _, name := range wantTools {
		if _, ok := tools[name]; !ok {
			t.Errorf("expected tool %q to be registered; registered tools: %v", name, tools)
		}
	}
}

// TestDaemonTool_SessionStart_CreatesRow verifies the mem_session_start handler
// end-to-end: after calling it via the registered handler the session row must
// be present in the store.
func TestDaemonTool_SessionStart_CreatesRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "engram.db")
	cfg := daemonCfg{db: dbPath, syncInterval: 30 * time.Second}

	components, err := buildDaemon(cfg)
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	tools := components.mcpServer.ListTools()
	startTool, ok := tools["mem_session_start"]
	if !ok {
		t.Fatal("mem_session_start not registered")
	}

	dir := t.TempDir()
	req := newToolRequest("mem_session_start", map[string]any{
		"id":        "test-session-1",
		"directory": dir,
	})
	result, err := startTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	sess, err := components.store.GetSession("test-session-1")
	if err != nil {
		t.Fatalf("GetSession after start: %v", err)
	}
	if sess.ID != "test-session-1" {
		t.Errorf("session ID: got %q, want %q", sess.ID, "test-session-1")
	}
	if sess.EndedAt != nil {
		t.Errorf("EndedAt should be nil after start, got %v", sess.EndedAt)
	}
	if sess.Directory != dir {
		t.Errorf("session Directory: got %q, want %q (supplied directory not stored)", sess.Directory, dir)
	}
}

// TestDaemonTool_SessionStart_InvalidConfig verifies that a malformed
// .engram/config.json surfaces as a tool error rather than silently storing the
// session under "unknown" (faithful to old_code's ErrInvalidConfig handling).
func TestDaemonTool_SessionStart_InvalidConfig(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "engram.db")
	components, err := buildDaemon(daemonCfg{db: dbPath, syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	badDir := t.TempDir()
	cfgDir := filepath.Join(badDir, ".engram")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte("{ not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	startTool := components.mcpServer.ListTools()["mem_session_start"]
	req := newToolRequest("mem_session_start", map[string]any{"id": "bad-cfg-session", "directory": badDir})
	result, err := startTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected a tool error for invalid .engram/config.json, got success: %v", result.Content)
	}
	// The session must NOT have been created (we errored before CreateSession).
	if _, err := components.store.GetSession("bad-cfg-session"); err == nil {
		t.Error("session was created despite invalid config — should have errored before CreateSession")
	}
}

// TestDaemonTool_SessionStart_AmbiguousProject verifies that a directory which is
// the parent of multiple git repos surfaces as a tool error (faithful to old_code)
// rather than silently storing the session under the parent's basename.
func TestDaemonTool_SessionStart_AmbiguousProject(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "engram.db")
	components, err := buildDaemon(daemonCfg{db: dbPath, syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	parent := t.TempDir()
	for _, name := range []string{"repo-a", "repo-b"} {
		if err := os.MkdirAll(filepath.Join(parent, name, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	startTool := components.mcpServer.ListTools()["mem_session_start"]
	req := newToolRequest("mem_session_start", map[string]any{"id": "ambig-session", "directory": parent})
	result, err := startTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected a tool error for an ambiguous (multi-repo parent) directory, got success: %v", result.Content)
	}
	if _, err := components.store.GetSession("ambig-session"); err == nil {
		t.Error("session created despite ambiguous project — should have errored before CreateSession")
	}
}

// TestDaemonTool_SessionEnd_ClosesRow verifies that calling mem_session_end via
// its registered handler sets ended_at and summary on the session row.
func TestDaemonTool_SessionEnd_ClosesRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "engram.db")
	cfg := daemonCfg{db: dbPath, syncInterval: 30 * time.Second}

	components, err := buildDaemon(cfg)
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	// First start a session directly on the store.
	if err := components.store.CreateSession("end-test", "myproject", "/src"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	tools := components.mcpServer.ListTools()
	endTool, ok := tools["mem_session_end"]
	if !ok {
		t.Fatal("mem_session_end not registered")
	}

	req := newToolRequest("mem_session_end", map[string]any{
		"id":      "end-test",
		"summary": "finished everything",
	})
	result, err := endTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	sess, err := components.store.GetSession("end-test")
	if err != nil {
		t.Fatalf("GetSession after end: %v", err)
	}
	if sess.EndedAt == nil {
		t.Fatal("EndedAt is nil after mem_session_end")
	}
	if sess.Summary == nil || *sess.Summary != "finished everything" {
		t.Errorf("Summary: got %v, want %q", sess.Summary, "finished everything")
	}
}

// TestDaemonTool_SessionStart_MissingID verifies that mem_session_start returns
// a tool error (not a transport error) when id is absent.
func TestDaemonTool_SessionStart_MissingID(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "engram.db")
	cfg := daemonCfg{db: dbPath, syncInterval: 30 * time.Second}
	components, err := buildDaemon(cfg)
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	tools := components.mcpServer.ListTools()
	startTool := tools["mem_session_start"]

	req := newToolRequest("mem_session_start", map[string]any{})
	result, err := startTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler should not return transport error, got: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected tool error for missing id, got success: %v", result.Content)
	}
}

// newToolRequest builds a minimal mcp.CallToolRequest for unit-testing handlers
// without a full MCP transport round-trip.
func newToolRequest(name string, args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      name,
			Arguments: args,
		},
	}
}

// TestBuildDaemon_Close_IsIdempotent verifies that Close can be called on a
// local-only components struct without panicking, and that the store is properly
// released (the DB file exists after close — it's just closed, not deleted).
func TestBuildDaemon_Close_IsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "engram.db")
	cfg := daemonCfg{
		db:           dbPath,
		syncInterval: 30 * time.Second,
	}

	components, err := buildDaemon(cfg)
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}

	// Call Close 3 times to exercise the idempotency contract: multiple deferred
	// Close calls (e.g. from nested defer chains) must not panic or double-free.
	components.Close()
	components.Close()
	components.Close()

	// Verify the file was created (store opened and schema applied).
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Error("expected SQLite DB file to exist after buildDaemon+Close")
	}
}

// ─── mem_save / mem_get_observation / mem_session_summary handler tests ───────

// TestDaemonTool_MemSave_CreatesObservation verifies the mem_save handler
// end-to-end: the observation is persisted and retrievable via GetObservation.
func TestDaemonTool_MemSave_CreatesObservation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "save.db")
	components, err := buildDaemon(daemonCfg{db: dbPath, syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	saveTool, ok := components.mcpServer.ListTools()["mem_save"]
	if !ok {
		t.Fatal("mem_save not registered")
	}

	req := newToolRequest("mem_save", map[string]any{
		"title":   "test observation",
		"content": "content body",
		"type":    "decision",
	})
	result, err := saveTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	// The response must mention the title.
	if len(result.Content) == 0 {
		t.Fatal("empty content in result")
	}
	text, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(text.Text, "test observation") {
		t.Errorf("result text %q does not mention the title", text.Text)
	}

	// Verify the row is in the store (id is embedded in the response text).
	// We verify by retrieving id=1 (first row on a fresh DB).
	rec, err := components.store.GetObservation(1)
	if err != nil {
		t.Fatalf("GetObservation(1): %v", err)
	}
	if rec.Title != "test observation" {
		t.Errorf("Title = %q, want %q", rec.Title, "test observation")
	}
	if rec.Type != "decision" {
		t.Errorf("Type = %q, want %q", rec.Type, "decision")
	}
}

// TestDaemonTool_MemSave_MissingTitle verifies that mem_save returns a tool
// error when title is absent (not a transport error).
func TestDaemonTool_MemSave_MissingTitle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "save_missing_title.db")
	components, err := buildDaemon(daemonCfg{db: dbPath, syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	saveTool := components.mcpServer.ListTools()["mem_save"]
	req := newToolRequest("mem_save", map[string]any{"content": "no title"})
	result, err := saveTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler should not return transport error: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected tool error for missing title, got success: %v", result.Content)
	}
}

// TestDaemonTool_MemSave_InvalidConfig verifies that mem_save surfaces
// ErrInvalidConfig as a tool error rather than panicking or succeeding silently.
func TestDaemonTool_MemSave_InvalidConfig(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "save_bad_cfg.db")
	components, err := buildDaemon(daemonCfg{db: dbPath, syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	// Create a malformed .engram/config.json in a temp dir and change cwd to it.
	badDir := t.TempDir()
	cfgDir := filepath.Join(badDir, ".engram")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte("{ not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	if err := os.Chdir(badDir); err != nil {
		t.Fatalf("chdir to badDir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	saveTool := components.mcpServer.ListTools()["mem_save"]
	req := newToolRequest("mem_save", map[string]any{"title": "bad config test", "content": "body"})
	result, err := saveTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected tool error for invalid .engram/config.json, got success: %v", result.Content)
	}
}

// TestDaemonTool_MemGetObservation_ReturnsContent verifies mem_get_observation
// returns the content of a previously saved observation.
func TestDaemonTool_MemGetObservation_ReturnsContent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "get_obs.db")
	components, err := buildDaemon(daemonCfg{db: dbPath, syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	// Save an observation directly via the store.
	obs, err := components.store.AddObservation(localstore.AddObservationParams{
		SessionID: "sess-get",
		Title:     "get obs title",
		Content:   "unique get obs content",
		Project:   "testproj",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	getTool := components.mcpServer.ListTools()["mem_get_observation"]
	req := newToolRequest("mem_get_observation", map[string]any{"id": float64(obs.ID)})
	result, err := getTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "unique get obs content") {
		t.Errorf("response %q does not contain saved content", text)
	}
}

// TestDaemonTool_MemGetObservation_NotFound verifies that requesting a
// non-existent id returns a tool error (not a transport error).
func TestDaemonTool_MemGetObservation_NotFound(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "get_obs_nf.db")
	components, err := buildDaemon(daemonCfg{db: dbPath, syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	getTool := components.mcpServer.ListTools()["mem_get_observation"]
	req := newToolRequest("mem_get_observation", map[string]any{"id": float64(99999)})
	result, err := getTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected tool error for missing id=99999, got success: %v", result.Content)
	}
}

// TestDaemonTool_MemSessionSummary_CreatesSessionSummary verifies that
// mem_session_summary creates an observation with type="session_summary".
func TestDaemonTool_MemSessionSummary_CreatesSessionSummary(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sess_sum.db")
	components, err := buildDaemon(daemonCfg{db: dbPath, syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	sumTool := components.mcpServer.ListTools()["mem_session_summary"]
	req := newToolRequest("mem_session_summary", map[string]any{
		"content": "## Goal\nTest the session summary tool.",
	})
	result, err := sumTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	// The first observation on a fresh store has id=1.
	rec, err := components.store.GetObservation(1)
	if err != nil {
		t.Fatalf("GetObservation(1): %v", err)
	}
	if rec.Type != "session_summary" {
		t.Errorf("Type = %q, want %q", rec.Type, "session_summary")
	}
	if !strings.Contains(rec.Content, "Test the session summary tool") {
		t.Errorf("Content %q does not contain expected text", rec.Content)
	}
}

// TestDaemonTool_MemSessionSummary_MissingContent verifies that
// mem_session_summary returns a tool error when content is empty.
func TestDaemonTool_MemSessionSummary_MissingContent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sess_sum_empty.db")
	components, err := buildDaemon(daemonCfg{db: dbPath, syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	sumTool := components.mcpServer.ListTools()["mem_session_summary"]
	req := newToolRequest("mem_session_summary", map[string]any{"content": "   "})
	result, err := sumTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected tool error for whitespace-only content, got success: %v", result.Content)
	}
}
