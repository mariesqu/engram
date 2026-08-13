package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/localstore"
)

// These tests cover the client-directory forwarding contract end to end: the
// `engram connect` bridge injects ITS working directory into directory-aware
// tools/call frames (client side, injectClientDirectory), and the daemon's
// project resolution prefers that forwarded directory over its own cwd
// (daemon side, resolveProjectDir → resolveReadProject / resolveSaveProject).
//
// The incident they exist to keep dead: every tool handler runs in the SHARED
// resident daemon, whose cwd is wherever autostart/tray launched it from
// (C:\Windows\system32 in the field). Auto-detected projects were therefore
// filed under that directory's basename, collecting memories from every repo
// into one junk project.

// ─── client side: injectClientDirectory ──────────────────────────────────────

const testClientDir = `/home/dev/repos/prophet`

// decodeToolArgs extracts params.arguments from a tools/call frame. It reports
// a decode failure with t.Errorf (never t.Fatalf) and returns a nil map,
// because the bridge tests call it from the httptest handler's goroutine, where
// a Fatalf would abort the wrong goroutine.
func decodeToolArgs(t *testing.T, msg []byte) map[string]any {
	t.Helper()
	var frame struct {
		Params struct {
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(msg, &frame); err != nil {
		t.Errorf("decode frame %s: %v", msg, err)
		return nil
	}
	return frame.Params.Arguments
}

// TestInjectClientDirectory_AddsDirectoryWhenAbsent verifies the core case: a
// tools/call for a directory-aware tool that carries neither project nor
// directory gets the client's working directory injected, with every other
// argument left intact.
func TestInjectClientDirectory_AddsDirectoryWhenAbsent(t *testing.T) {
	msg := []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call",` +
		`"params":{"name":"mem_save","arguments":{"title":"a title","content":"body"}}}`)

	got := injectClientDirectory(msg, testClientDir)

	args := decodeToolArgs(t, got)
	if args["directory"] != testClientDir {
		t.Errorf("directory = %v, want %q", args["directory"], testClientDir)
	}
	if args["title"] != "a title" || args["content"] != "body" {
		t.Errorf("existing arguments were mangled: %v", args)
	}
	if !bytes.Contains(got, []byte(`"id":7`)) {
		t.Errorf("JSON-RPC id must survive injection; got %s", got)
	}
}

// TestInjectClientDirectory_AddsArgumentsWhenAbsent verifies that a tools/call
// with NO arguments object at all still gets one — mem_context takes only
// optional arguments, so a client may legitimately omit it entirely.
func TestInjectClientDirectory_AddsArgumentsWhenAbsent(t *testing.T) {
	msg := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mem_context"}}`)

	got := injectClientDirectory(msg, testClientDir)

	if args := decodeToolArgs(t, got); args["directory"] != testClientDir {
		t.Errorf("directory = %v, want %q (arguments should be created when absent)", args["directory"], testClientDir)
	}
}

// injectNoPanic calls injectClientDirectory, converting a panic into a test
// failure instead of taking the whole test binary down with it. Injection runs
// in a per-message dispatch goroutine in production, where nothing recovers —
// so "does not panic" is a real assertion, not defensive plumbing.
func injectNoPanic(t *testing.T, msg []byte, clientDir string) (out []byte) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("injectClientDirectory panicked on %s: %v", msg, r)
			out = nil
		}
	}()
	return injectClientDirectory(msg, clientDir)
}

// TestInjectClientDirectory_NullArgumentsIsTreatedAsAbsent covers the frame
// that used to take the bridge down: json.Unmarshal of a literal null into a
// map NILS the map and reports NO error, so the decode guard never fired and
// the following write panicked with "assignment to entry in nil map".
//
// Contract: null means absent (the daemon sees a nil arguments map either way),
// so the directory is injected rather than the frame passed through.
func TestInjectClientDirectory_NullArgumentsIsTreatedAsAbsent(t *testing.T) {
	msg := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mem_save","arguments":null}}`)

	got := injectNoPanic(t, msg, testClientDir)

	if args := decodeToolArgs(t, got); args["directory"] != testClientDir {
		t.Errorf("directory = %v, want %q (null arguments must be treated as absent)", args["directory"], testClientDir)
	}
}

// TestMCPBridge_NullArguments_BridgeSurvives is the process-level half of the
// same regression: one bad frame on stdin must not kill the stdio MCP server
// that a whole editor session depends on. The panic used to happen inside a
// dispatch goroutine, where no recover() exists anywhere in cmd/engram.
func TestMCPBridge_NullArguments_BridgeSurvives(t *testing.T) {
	dir := t.TempDir()
	const token = "null-args-token"

	var gotDir string
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		gotDir, _ = decodeToolArgs(t, body)["directory"].(string)
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
	b.clientDir = testClientDir

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
		`"params":{"name":"mem_save","arguments":null}}` + "\n")
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := b.run(ctx, in, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), `"result":{"ok":true}`) {
		t.Errorf("bridge did not complete the round trip; stdout = %q", out.String())
	}
	if gotDir != testClientDir {
		t.Errorf("daemon received directory %q, want %q", gotDir, testClientDir)
	}
}

// TestInjectClientDirectory_ExplicitProjectWins verifies that a caller-supplied
// project is never second-guessed: the frame is forwarded byte-for-byte, with
// no directory added that could compete with it.
func TestInjectClientDirectory_ExplicitProjectWins(t *testing.T) {
	msg := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call",` +
		`"params":{"name":"mem_save","arguments":{"title":"t","project":"engram"}}}`)

	got := injectClientDirectory(msg, testClientDir)

	if !bytes.Equal(got, msg) {
		t.Errorf("frame with an explicit project must be forwarded verbatim;\n got %s\nwant %s", got, msg)
	}
}

// TestInjectClientDirectory_NonStringProjectDoesNotSuppress pins the asymmetry
// between the two suppression checks. A JSON number is not a project: the daemon
// reads the argument as args["project"].(string) and gets "", so suppressing on
// it would forward a call with NEITHER a project nor a directory — resolved from
// the daemon's own cwd, the junk-project misfile this feature exists to kill.
// The caller's value itself is still left alone; only the decision ignores it.
func TestInjectClientDirectory_NonStringProjectDoesNotSuppress(t *testing.T) {
	msg := []byte(`{"jsonrpc":"2.0","id":4,"method":"tools/call",` +
		`"params":{"name":"mem_save","arguments":{"title":"t","project":123}}}`)

	got := injectClientDirectory(msg, testClientDir)

	args := decodeToolArgs(t, got)
	if args["directory"] != testClientDir {
		t.Errorf("directory = %v, want %q (a non-string project is semantically absent)", args["directory"], testClientDir)
	}
	if args["project"] != float64(123) {
		t.Errorf("project = %v, want the caller's 123 forwarded untouched", args["project"])
	}
	if args["title"] != "t" {
		t.Errorf("existing arguments were mangled: %v", args)
	}
}

// TestInjectClientDirectory_CallerDirectoryNotOverwritten verifies that a
// directory the caller set deliberately (e.g. an agent registering a session
// for another checkout) is never replaced by the injected one.
func TestInjectClientDirectory_CallerDirectoryNotOverwritten(t *testing.T) {
	msg := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call",` +
		`"params":{"name":"mem_session_start","arguments":{"id":"s1","directory":"/elsewhere"}}}`)

	got := injectClientDirectory(msg, testClientDir)

	if !bytes.Equal(got, msg) {
		t.Errorf("caller-supplied directory must be forwarded verbatim;\n got %s\nwant %s", got, msg)
	}
}

// TestInjectClientDirectory_LeavesUnrelatedFramesVerbatim covers every
// pass-through branch: non-tools/call methods, tools that do not resolve a
// project from a directory, malformed JSON, and JSON that is not an object
// (a JSON-RPC batch array). All must reach the daemon untouched.
func TestInjectClientDirectory_LeavesUnrelatedFramesVerbatim(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{"ping", `{"jsonrpc":"2.0","id":1,"method":"ping"}`},
		{"initialize", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`},
		{"tools/list", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
		{"notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`},
		{"tool without directory resolution", `{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
			`"params":{"name":"mem_judge","arguments":{"judgment_id":"rel-ab","relation":"related"}}}`},
		{"unknown tool", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mem_future","arguments":{}}}`},
		{"arguments not an object", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mem_save","arguments":"nope"}}`},
		{"batch array", `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mem_save","arguments":{}}}]`},
		{"malformed json", `{"jsonrpc":"2.0","id":1,`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := []byte(tc.msg)
			if got := injectClientDirectory(msg, testClientDir); !bytes.Equal(got, msg) {
				t.Errorf("must be forwarded verbatim;\n got %s\nwant %s", got, msg)
			}
		})
	}
}

// TestInjectClientDirectory_NoClientDirIsIdentity verifies the back-compat
// escape hatch: when the client's cwd is unreadable, injection is skipped
// entirely and the daemon's own cwd fallback stays in charge.
func TestInjectClientDirectory_NoClientDirIsIdentity(t *testing.T) {
	msg := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mem_save","arguments":{"title":"t"}}}`)

	for _, dir := range []string{"", "   "} {
		if got := injectClientDirectory(msg, dir); !bytes.Equal(got, msg) {
			t.Errorf("clientDir=%q: must be forwarded verbatim;\n got %s\nwant %s", dir, got, msg)
		}
	}
}

// TestInjectClientDirectory_PreservesLargeRequestID pins the reason the frame
// is re-encoded through json.RawMessage rather than a plain map[string]any: an
// id above 2^53 would round-trip through float64 and come back a DIFFERENT
// number, silently orphaning the client's pending request.
func TestInjectClientDirectory_PreservesLargeRequestID(t *testing.T) {
	const bigID = "9007199254740993" // 2^53 + 1 — not representable as a float64
	msg := []byte(`{"jsonrpc":"2.0","id":` + bigID + `,"method":"tools/call",` +
		`"params":{"name":"mem_search","arguments":{"query":"q"}}}`)

	got := injectClientDirectory(msg, testClientDir)

	// Guard against a vacuous pass: a frame that was never touched trivially
	// still contains its own id, so assert the injection actually happened
	// before crediting the re-encoding with preserving anything.
	if bytes.Equal(got, msg) {
		t.Fatal("expected injection to modify the frame; the id assertion below would be vacuous")
	}
	if !bytes.Contains(got, []byte(bigID)) {
		t.Errorf("request id %s was not preserved exactly; got %s", bigID, got)
	}
}

// TestMCPBridge_ForwardsOwnDirectory drives a real tools/call through run()
// and asserts the daemon received the bridge's directory on the wire.
func TestMCPBridge_ForwardsOwnDirectory(t *testing.T) {
	dir := t.TempDir()
	const token = "dir-forward-token"

	var gotDir string
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		if d, ok := decodeToolArgs(t, body)["directory"].(string); ok {
			gotDir = d
		}
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
	// newMCPBridge captures os.Getwd(); pin a known value so the assertion does
	// not depend on where `go test` was invoked.
	b.clientDir = testClientDir

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
		`"params":{"name":"mem_save","arguments":{"title":"t"}}}` + "\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := b.run(ctx, in, &bytes.Buffer{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotDir != testClientDir {
		t.Errorf("daemon received directory %q, want %q", gotDir, testClientDir)
	}
}

// TestMCPBridge_ConcurrentClientsKeepOwnDirectory proves the injection is
// per-call, not per-daemon: two bridges in different repos talking to the SAME
// resident daemon must never see each other's directory. A daemon-side global
// would fail this.
func TestMCPBridge_ConcurrentClientsKeepOwnDirectory(t *testing.T) {
	dir := t.TempDir()
	const token = "concurrent-dir-token"

	var mu sync.Mutex
	seen := map[string]string{} // title → forwarded directory

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		args := decodeToolArgs(t, body)
		title, _ := args["title"].(string)
		gotDir, _ := args["directory"].(string)
		mu.Lock()
		seen[title] = gotDir
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	port := mustParsePort(t, ts.URL)
	if err := controlapi.WriteDaemonJSON(dir, token, port, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	clients := map[string]string{ // title → that client's working directory
		"from-alpha": `/repos/alpha`,
		"from-beta":  `/repos/beta`,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for title, clientDir := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := newMCPBridge(dir)
			if err != nil {
				t.Errorf("newMCPBridge: %v", err)
				return
			}
			b.clientDir = clientDir
			in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
				`"params":{"name":"mem_save","arguments":{"title":"` + title + `"}}}` + "\n")
			if err := b.run(ctx, in, &bytes.Buffer{}); err != nil {
				t.Errorf("run(%s): %v", title, err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for title, want := range clients {
		if seen[title] != want {
			t.Errorf("call %q carried directory %q, want %q — clients cross-contaminated", title, seen[title], want)
		}
	}
}

// ─── daemon side: forwarded directory beats the daemon's cwd ─────────────────

// pinnedProjectDir creates a directory whose .engram/config.json pins the
// project name, so detection is deterministic without needing git.
func pinnedProjectDir(t *testing.T, projectName string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".engram"), 0o755); err != nil {
		t.Fatalf("mkdir .engram: %v", err)
	}
	body := []byte(`{"project_name": "` + projectName + `"}`)
	if err := os.WriteFile(filepath.Join(dir, ".engram", "config.json"), body, 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
	return dir
}

// chdirTo changes the process working directory for the duration of the test,
// restoring it afterwards. It stands in for the resident daemon's cwd.
func chdirTo(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

// chdirToJunkDir points the process cwd at a non-repo directory literally named
// "system32", reproducing the resident daemon's cwd in the field incident. It
// returns the project name cwd detection would produce from there.
func chdirToJunkDir(t *testing.T) string {
	t.Helper()
	junk := filepath.Join(t.TempDir(), "system32")
	if err := os.MkdirAll(junk, 0o755); err != nil {
		t.Fatalf("mkdir junk dir: %v", err)
	}
	chdirTo(t, junk)
	return "system32"
}

// TestDaemonTool_MemSave_ForwardedDirectoryBeatsDaemonCwd is the regression
// test for the field incident: with the daemon's cwd at a junk directory, a
// mem_save carrying the CLIENT's directory must be filed under the client
// repo's project, never under the cwd basename.
func TestDaemonTool_MemSave_ForwardedDirectoryBeatsDaemonCwd(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "fwd_save.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	junkProject := chdirToJunkDir(t)
	clientDir := pinnedProjectDir(t, "forwarded-repo")

	saveTool := components.mcpServer.ListTools()["mem_save"]
	req := newToolRequest("mem_save", map[string]any{
		"title":     "saved from the client's repo",
		"content":   "body",
		"directory": clientDir,
	})
	result, err := saveTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	rec, err := components.store.GetObservation(1)
	if err != nil {
		t.Fatalf("GetObservation(1): %v", err)
	}
	if rec.Project != "forwarded-repo" {
		t.Errorf("Project = %q, want %q (the forwarded directory must beat the daemon's cwd %q)",
			rec.Project, "forwarded-repo", junkProject)
	}
}

// TestDaemonTool_MemSave_NoDirectoryStillUsesCwd pins the back-compat path: an
// older `engram connect`, or a per-client `engram daemon --transport stdio`
// (whose cwd IS the project), forwards nothing and must behave exactly as
// before — project detected from os.Getwd().
func TestDaemonTool_MemSave_NoDirectoryStillUsesCwd(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "cwd_save.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	chdirTo(t, pinnedProjectDir(t, "cwd-repo"))

	saveTool := components.mcpServer.ListTools()["mem_save"]
	req := newToolRequest("mem_save", map[string]any{"title": "saved from cwd", "content": "body"})
	result, err := saveTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	rec, err := components.store.GetObservation(1)
	if err != nil {
		t.Fatalf("GetObservation(1): %v", err)
	}
	if rec.Project != "cwd-repo" {
		t.Errorf("Project = %q, want %q (no forwarded directory → cwd detection)", rec.Project, "cwd-repo")
	}
}

// TestDaemonTool_MemSave_ExplicitProjectBeatsDirectory verifies the top of the
// precedence chain: an explicit project argument is never overridden by a
// forwarded directory.
func TestDaemonTool_MemSave_ExplicitProjectBeatsDirectory(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "explicit_save.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	chdirToJunkDir(t)

	saveTool := components.mcpServer.ListTools()["mem_save"]
	req := newToolRequest("mem_save", map[string]any{
		"title":     "explicit wins",
		"content":   "body",
		"project":   "chosen-by-the-agent",
		"directory": pinnedProjectDir(t, "forwarded-repo"),
	})
	result, err := saveTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	rec, err := components.store.GetObservation(1)
	if err != nil {
		t.Fatalf("GetObservation(1): %v", err)
	}
	if rec.Project != "chosen-by-the-agent" {
		t.Errorf("Project = %q, want %q (explicit project must always win)", rec.Project, "chosen-by-the-agent")
	}
}

// TestDaemonTool_MemSave_ForwardedDirectoryInvalidConfigErrors verifies that
// resolveSaveProject's hard errors fire against the FORWARDED directory: a
// broken .engram/config.json in the client's repo must surface as a tool error
// even though the daemon's own cwd is perfectly healthy.
func TestDaemonTool_MemSave_ForwardedDirectoryInvalidConfigErrors(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "fwd_bad_cfg.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	chdirTo(t, pinnedProjectDir(t, "healthy-daemon-cwd"))

	badDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(badDir, ".engram"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, ".engram", "config.json"), []byte("{ not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	saveTool := components.mcpServer.ListTools()["mem_save"]
	req := newToolRequest("mem_save", map[string]any{
		"title":     "bad forwarded config",
		"content":   "body",
		"directory": badDir,
	})
	result, err := saveTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected a tool error for the forwarded directory's invalid config, got success: %v", result.Content)
	}
}

// TestDaemonTool_MemSessionStart_ExplicitProjectBeatsDaemonCwd covers the
// documented "always pass project" workaround, which used to be the WORST thing
// an agent could do here: a project argument suppresses the client-directory
// injection (see injectClientDirectory), so a tool that ignored the argument
// resolved from the daemon's cwd and filed the session under the junk project —
// the exact bug the workaround was supposed to dodge.
func TestDaemonTool_MemSessionStart_ExplicitProjectBeatsDaemonCwd(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "start_explicit.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	junkProject := chdirToJunkDir(t)

	startTool := components.mcpServer.ListTools()["mem_session_start"]
	req := newToolRequest("mem_session_start", map[string]any{
		"id":      "sess-explicit-project",
		"project": "chosen-by-the-agent",
	})
	result, err := startTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	sess, err := components.store.GetSession("sess-explicit-project")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Project != "chosen-by-the-agent" {
		t.Errorf("Project = %q, want %q (explicit project must beat the daemon's cwd %q)",
			sess.Project, "chosen-by-the-agent", junkProject)
	}
}

// TestDaemonTool_MemSessionStart_ForwardedDirectoryBeatsDaemonCwd is the
// session-tool half of the field incident: with no project named, the CLIENT's
// directory decides, never the resident daemon's cwd.
func TestDaemonTool_MemSessionStart_ForwardedDirectoryBeatsDaemonCwd(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "start_fwd.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	junkProject := chdirToJunkDir(t)
	clientDir := pinnedProjectDir(t, "forwarded-repo")

	startTool := components.mcpServer.ListTools()["mem_session_start"]
	req := newToolRequest("mem_session_start", map[string]any{
		"id":        "sess-forwarded-dir",
		"directory": clientDir,
	})
	result, err := startTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	sess, err := components.store.GetSession("sess-forwarded-dir")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Project != "forwarded-repo" {
		t.Errorf("Project = %q, want %q (the forwarded directory must beat the daemon's cwd %q)",
			sess.Project, "forwarded-repo", junkProject)
	}
	if sess.Directory != clientDir {
		t.Errorf("Directory = %q, want the forwarded %q", sess.Directory, clientDir)
	}
}

// TestDaemonTool_MemSessionSummary_ExplicitProjectBeatsSessionRow pins the top
// of this tool's precedence chain: the session row is a FALLBACK, not an
// override. An agent naming a project (the only lever it has left once the
// directory injection is suppressed) must not have it silently replaced by
// whatever a stale or foreign session_id points at.
func TestDaemonTool_MemSessionSummary_ExplicitProjectBeatsSessionRow(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "summary_explicit.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	chdirToJunkDir(t)
	if err := components.store.CreateSession("sess-summary-explicit", "session-row-project", "/some/other/checkout"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	sumTool := components.mcpServer.ListTools()["mem_session_summary"]
	req := newToolRequest("mem_session_summary", map[string]any{
		"content":    "## Goal\nverify the explicit project wins",
		"session_id": "sess-summary-explicit",
		"project":    "chosen-by-the-agent",
	})
	result, err := sumTool.Handler(t.Context(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	rec, err := components.store.GetObservation(1)
	if err != nil {
		t.Fatalf("GetObservation(1): %v", err)
	}
	if rec.Project != "chosen-by-the-agent" {
		t.Errorf("Project = %q, want %q (explicit project must beat the session row %q)",
			rec.Project, "chosen-by-the-agent", "session-row-project")
	}
}

// TestDaemonTool_MemSearch_ForwardedDirectoryScopesResults proves the read path
// honours the forwarded directory too: a memory in the client's project is
// found when the directory is forwarded, and invisible when it is not (the
// daemon's junk cwd scopes the search elsewhere).
func TestDaemonTool_MemSearch_ForwardedDirectoryScopesResults(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "fwd_search.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	chdirToJunkDir(t)
	clientDir := pinnedProjectDir(t, "forwarded-repo")

	saveTool := components.mcpServer.ListTools()["mem_save"]
	saveReq := newToolRequest("mem_save", map[string]any{
		"title":     "hexagonal architecture decision",
		"content":   "chose hexagonal over layered",
		"directory": clientDir,
	})
	if result, err := saveTool.Handler(t.Context(), saveReq); err != nil {
		t.Fatalf("mem_save transport error: %v", err)
	} else if result.IsError {
		t.Fatalf("mem_save tool error: %v", result.Content)
	}

	searchTool := components.mcpServer.ListTools()["mem_search"]

	withDir, err := searchTool.Handler(t.Context(), newToolRequest("mem_search", map[string]any{
		"query":     "hexagonal",
		"directory": clientDir,
	}))
	if err != nil {
		t.Fatalf("mem_search transport error: %v", err)
	}
	if text := withDir.Content[0].(mcp.TextContent).Text; !strings.Contains(text, "hexagonal") {
		t.Errorf("forwarded directory should scope the search to the client's project; got:\n%s", text)
	}

	withoutDir, err := searchTool.Handler(t.Context(), newToolRequest("mem_search", map[string]any{"query": "hexagonal"}))
	if err != nil {
		t.Fatalf("mem_search transport error: %v", err)
	}
	if text := withoutDir.Content[0].(mcp.TextContent).Text; !strings.Contains(text, "No memories found") {
		t.Errorf("without a forwarded directory the search is scoped to the daemon's cwd project; got:\n%s", text)
	}
}

// TestDaemonTool_MemSearch_ForwardedDirectoryKeepsPersonalScopeCrossProject
// guards REQ-391 against the injection: a forwarded directory is NOT an
// explicit project, so a scope=personal search must still span every project
// rather than being narrowed to the client's repo.
func TestDaemonTool_MemSearch_ForwardedDirectoryKeepsPersonalScopeCrossProject(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "fwd_personal.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	if _, err := components.store.AddObservation(localstore.AddObservationParams{
		Title: "personal note", Content: "personal-cross-project-token", Project: "some-other-project", Scope: "personal",
	}); err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	searchTool := components.mcpServer.ListTools()["mem_search"]
	result, err := searchTool.Handler(t.Context(), newToolRequest("mem_search", map[string]any{
		"query":     "personal-cross-project-token",
		"scope":     "personal",
		"directory": pinnedProjectDir(t, "forwarded-repo"),
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}
	if text := result.Content[0].(mcp.TextContent).Text; !strings.Contains(text, "personal note") {
		t.Errorf("a forwarded directory must not narrow a scope=personal search:\n%s", text)
	}
}

// TestRegisterTools_DirectoryAwareToolsDeclareDirectory keeps the client and
// daemon halves in lockstep: every tool `engram connect` injects into must
// declare the optional "directory" argument in its schema, so the injected
// value is documented and survives any future strict-schema validation.
func TestRegisterTools_DirectoryAwareToolsDeclareDirectory(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "schema.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	registered := components.mcpServer.ListTools()
	for name := range directoryAwareTools {
		tool, ok := registered[name]
		if !ok {
			t.Errorf("directoryAwareTools names %q, which is not a registered tool", name)
			continue
		}
		prop, ok := tool.Tool.InputSchema.Properties["directory"]
		if !ok {
			t.Errorf("tool %q is directory-aware but its schema does not declare a \"directory\" property", name)
			continue
		}
		// The description is deliberately identical across tools; assert the text
		// so a hand-written variant cannot drift back in (mem_session_start
		// carried its own "Working directory" wording before the unification).
		schema, ok := prop.(map[string]any)
		if !ok {
			t.Errorf("tool %q: directory property is %T, want a schema object", name, prop)
			continue
		}
		if got := schema["description"]; got != directoryArgDescription {
			t.Errorf("tool %q directory description =\n  %v\nwant\n  %v", name, got, directoryArgDescription)
		}
	}
}

// TestRegisterTools_DirectoryAwareToolsDeclareProject is the other half of the
// lockstep, and the schema-level statement of the precedence contract: `engram
// connect` SUPPRESSES the injection whenever a call carries a project, so a
// directory-aware tool that does not accept "project" would answer that call
// from the daemon's own cwd — the junk-project bug, reached by following the
// documented "always pass project" advice. Every tool the bridge injects into
// must therefore declare it (mem_session_start and mem_session_summary did not).
func TestRegisterTools_DirectoryAwareToolsDeclareProject(t *testing.T) {
	components, err := buildDaemon(daemonCfg{db: filepath.Join(t.TempDir(), "schema_project.db"), syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	registered := components.mcpServer.ListTools()
	for name := range directoryAwareTools {
		tool, ok := registered[name]
		if !ok {
			t.Errorf("directoryAwareTools names %q, which is not a registered tool", name)
			continue
		}
		prop, ok := tool.Tool.InputSchema.Properties["project"]
		if !ok {
			t.Errorf("tool %q is directory-aware but its schema does not declare a \"project\" property — "+
				"an explicit project suppresses the directory injection, so it MUST be honoured", name)
			continue
		}
		schema, ok := prop.(map[string]any)
		if !ok {
			t.Errorf("tool %q: project property is %T, want a schema object", name, prop)
			continue
		}
		if got := schema["type"]; got != "string" {
			t.Errorf("tool %q project type = %v, want \"string\"", name, got)
		}
	}
}

// ─── ENGRAM_CLIENT_DIR escape hatch ──────────────────────────────────────────

// TestResolveClientDir_DefaultsToCwd pins the normal path: with no override,
// the forwarded directory is this process's own cwd.
func TestResolveClientDir_DefaultsToCwd(t *testing.T) {
	t.Setenv("ENGRAM_CLIENT_DIR", "")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if got := resolveClientDir(); got != cwd {
		t.Errorf("resolveClientDir() = %q, want the process cwd %q", got, cwd)
	}
}

// TestResolveClientDir_EnvOverride covers the escape hatch for MCP hosts that
// spawn their servers outside the workspace: the override wins over cwd.
func TestResolveClientDir_EnvOverride(t *testing.T) {
	want := t.TempDir()
	t.Setenv("ENGRAM_CLIENT_DIR", want)

	if got := resolveClientDir(); got != want {
		t.Errorf("resolveClientDir() = %q, want the override %q", got, want)
	}
}

// TestResolveClientDir_DisableSentinel verifies that "none" (the same explicit
// -noop spelling the embedding provider uses) turns forwarding off, restoring
// the daemon's own cwd fallback.
func TestResolveClientDir_DisableSentinel(t *testing.T) {
	for _, v := range []string{"none", "NONE", "  none  "} {
		t.Setenv("ENGRAM_CLIENT_DIR", v)
		if got := resolveClientDir(); got != "" {
			t.Errorf("ENGRAM_CLIENT_DIR=%q: resolveClientDir() = %q, want \"\" (forwarding disabled)", v, got)
		}
	}
}

// TestNewMCPBridge_HonoursClientDirOverride wires the override through the
// constructor, since that is the only place the value is resolved.
func TestNewMCPBridge_HonoursClientDirOverride(t *testing.T) {
	dir := t.TempDir()
	if err := controlapi.WriteDaemonJSON(dir, "tok", 1, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}

	want := t.TempDir()
	t.Setenv("ENGRAM_CLIENT_DIR", want)

	b, err := newMCPBridge(dir)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}
	if b.clientDir != want {
		t.Errorf("bridge clientDir = %q, want %q", b.clientDir, want)
	}
}

// TestNewMCPBridge_ClientDirDisabledSkipsInjection proves the sentinel reaches
// all the way to the wire: with forwarding off, a tools/call is forwarded
// byte-for-byte, exactly as it was before this feature existed.
func TestNewMCPBridge_ClientDirDisabledSkipsInjection(t *testing.T) {
	dir := t.TempDir()
	if err := controlapi.WriteDaemonJSON(dir, "tok", 1, os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}
	t.Setenv("ENGRAM_CLIENT_DIR", clientDirDisabled)

	b, err := newMCPBridge(dir)
	if err != nil {
		t.Fatalf("newMCPBridge: %v", err)
	}
	if b.clientDir != "" {
		t.Fatalf("bridge clientDir = %q, want \"\" with forwarding disabled", b.clientDir)
	}

	msg := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mem_save","arguments":{"title":"t"}}}`)
	if got := injectClientDirectory(msg, b.clientDir); !bytes.Equal(got, msg) {
		t.Errorf("disabled forwarding must leave the frame verbatim;\n got %s\nwant %s", got, msg)
	}
}
