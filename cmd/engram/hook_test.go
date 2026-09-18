package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/localstore"
)

// These tests cover `engram hook <event>` — the lifecycle hooks. Two contracts
// matter more than anything the hooks actually save:
//
//  1. stdout shape. SessionStart-family events emit plain text the host injects
//     as context; the others emit ONE JSON object. A hook that prints a log line
//     to stdout has just written it into the model's context, or broken the
//     host's JSON parse.
//  2. fail-open. A hook that errors blocks the user's prompt. Every failure
//     mode — no daemon, wedged daemon, garbage stdin — must still produce the
//     event's fallback output, quickly.

// hookDaemonFixture boots a real daemon behind a real MCP HTTP endpoint and
// writes the daemon.json the hooks discover it through, exactly as `engram
// connect` would find it. Returns the DB path the hook commands take via --db.
func hookDaemonFixture(t *testing.T) (dbPath string, components *daemonComponents) {
	t.Helper()

	dir := t.TempDir()
	dbPath = filepath.Join(dir, "hooks.db")
	components, err := buildDaemon(daemonCfg{db: dbPath, syncInterval: 30 * time.Second})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	const token = "hook-token"
	mux := http.NewServeMux()
	// Both planes, on one listener, exactly as runDaemonHTTP mounts them: /mcp
	// for the tool calls and /api/ for the control API (the hooks read the
	// newest memory's timestamp there, and the auto-start probe reads
	// /api/v1/status — a fixture without it would make session-start think no
	// daemon is running and try to SPAWN one).
	controlapi.MountMCP(mux, token, mcpserver.NewStreamableHTTPServer(
		components.mcpServer,
		mcpserver.WithStateLess(true),
	))
	ctrl := controlapi.New(token, 0, &localStoreAdapter{store: components.store},
		stubSyncController{}, stubConfigStore{}, version)
	mux.Handle("/api/", ctrl.Handler())

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	if err := controlapi.WriteDaemonJSON(dir, token, mustParsePort(t, ts.URL), os.Getpid()); err != nil {
		t.Fatalf("WriteDaemonJSON: %v", err)
	}
	return dbPath, components
}

// runHook invokes one hook with the given stdin payload and returns its stdout.
// It asserts the always-exit-0 contract on the way through.
func runHook(t *testing.T, event string, input map[string]any, args ...string) string {
	t.Helper()

	body, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal hook input: %v", err)
	}
	return runHookRaw(t, event, string(body), args...)
}

// runHookRaw is runHook with a verbatim stdin body, for the malformed-input cases.
func runHookRaw(t *testing.T, event, stdin string, args ...string) string {
	t.Helper()

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		_, _ = w.WriteString(stdin)
		_ = w.Close()
	}()

	out, errOut := captureHookStreams(t, func() {
		if err := runHookCmd(append([]string{event}, args...)); err != nil {
			t.Errorf("hook %s returned an error (hooks must never fail closed): %v", event, err)
		}
	})
	_ = r.Close()
	if errOut != "" {
		t.Logf("hook %s stderr: %s", event, strings.TrimSpace(errOut))
	}
	return out
}

// captureHookStreams runs f with stdout and stderr redirected, DRAINING both
// concurrently.
//
// The concurrency is the point, and it is not premature: captureStdout in
// main_test.go reads its pipe only after f returns, which is fine for the
// two-token output of `engram version` and deadlocks instantly here — a
// session-start hook writes the whole memory protocol (several KiB) and a
// Windows pipe buffer is 4 KiB. The hook blocks in fmt.Print, the test blocks
// waiting for the hook, and the package hangs until the test timeout.
func captureHookStreams(t *testing.T, f func()) (stdout, stderr string) {
	t.Helper()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW

	var wg sync.WaitGroup
	var outBuf, errBuf bytes.Buffer
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&outBuf, outR) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&errBuf, errR) }()

	f()

	os.Stdout, os.Stderr = oldOut, oldErr
	_ = outW.Close()
	_ = errW.Close()
	wg.Wait()
	_ = outR.Close()
	_ = errR.Close()

	return outBuf.String(), errBuf.String()
}

// decodeHookJSON parses a JSON-object hook's stdout, failing the test when the
// output is not exactly one JSON object — the contract for every event except
// the SessionStart family.
func decodeHookJSON(t *testing.T, out string) map[string]any {
	t.Helper()

	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		t.Fatal("hook printed nothing; the host expects a JSON object")
	}
	if strings.Count(trimmed, "\n") > 0 {
		t.Fatalf("hook printed more than one line to stdout: %q", out)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		t.Fatalf("hook stdout is not a JSON object (%v): %q", err, out)
	}
	return obj
}

// TestHookSessionStart_PrintsProtocolAndContext covers the whole session-start
// path against a live daemon: the session row is created under the resolved
// project, the protocol text is injected, and the project's existing memory
// comes back with it.
func TestHookSessionStart_PrintsProtocolAndContext(t *testing.T) {
	dbPath, components := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "hooked-repo")

	saveTool := components.mcpServer.ListTools()["mem_save"]
	if _, err := saveTool.Handler(t.Context(), newToolRequest("mem_save", map[string]any{
		"title": "an earlier decision", "content": "body", "directory": repo,
	})); err != nil {
		t.Fatalf("seed mem_save: %v", err)
	}

	out := runHook(t, "session-start", map[string]any{
		"session_id": "hook-session-1",
		"cwd":        repo,
	}, "--db", dbPath)

	if !strings.Contains(out, "Engram provides persistent memory") {
		t.Errorf("session-start did not print the protocol text; got:\n%s", out)
	}
	if !strings.Contains(out, "an earlier decision") {
		t.Errorf("session-start did not print the project's memory context; got:\n%s", out)
	}
	if strings.Contains(out, "CRITICAL INSTRUCTION POST-COMPACTION") {
		t.Error("session-start printed the post-compaction steps; those belong to post-compaction only")
	}

	sess, err := components.store.GetSession("hook-session-1")
	if err != nil {
		t.Fatalf("the hook did not register the session: %v", err)
	}
	if sess.Project != "hooked-repo" {
		t.Errorf("session project = %q, want %q", sess.Project, "hooked-repo")
	}
}

// TestHookPostCompaction_PrintsRecoverySteps pins the one thing post-compaction
// adds, and the reason it exists: after a compaction the model has lost the
// context that would tell it to save anything, so the recovery steps are
// unconditional and name the project explicitly.
func TestHookPostCompaction_PrintsRecoverySteps(t *testing.T) {
	dbPath, _ := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "compacted-repo")

	out := runHook(t, "post-compaction", map[string]any{
		"session_id": "hook-session-2",
		"cwd":        repo,
	}, "--db", dbPath)

	if !strings.Contains(out, "Engram provides persistent memory") {
		t.Errorf("post-compaction did not print the protocol text; got:\n%s", out)
	}
	for _, want := range []string{
		"CRITICAL INSTRUCTION POST-COMPACTION",
		"mem_session_summary",
		"mem_context",
		"mem_search",
		`"compacted-repo"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("post-compaction output is missing %q; got:\n%s", want, out)
		}
	}
}

// TestHookSessionStart_NoDaemon_StillPrintsProtocol is the fail-open contract
// for the injection half: the protocol text needs no daemon, and an agent told
// nothing calls nothing. --no-autostart is mandatory here, not incidental — the
// auto-start path shells out to os.Executable(), which under `go test` is the
// TEST binary, and spawning that as a daemon is not a thing a test may do.
func TestHookSessionStart_NoDaemon_StillPrintsProtocol(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "absent.db")

	out := runHook(t, "session-start", map[string]any{
		"session_id": "hook-session-3",
		"cwd":        t.TempDir(),
	}, "--db", dbPath, "--no-autostart")

	if !strings.Contains(out, "Engram provides persistent memory") {
		t.Errorf("session-start must print the protocol even with no daemon; got:\n%s", out)
	}
}

// TestHookUserPromptSubmit_FirstPromptBootstraps covers the first message of a
// session: the prompt is captured AND the tool bootstrap is injected, in the
// only field a UserPromptSubmit hook can reach the model through.
func TestHookUserPromptSubmit_FirstPromptBootstraps(t *testing.T) {
	dbPath, components := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "prompt-repo")
	sessionID := "hook-prompt-" + t.Name()
	cleanupHookState(t, sessionID)

	out := runHook(t, "user-prompt-submit", map[string]any{
		"session_id": sessionID,
		"cwd":        repo,
		"prompt":     "add the missing index",
	}, "--db", dbPath)

	obj := decodeHookJSON(t, out)
	specific, ok := obj["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("first prompt did not inject a bootstrap: %v", obj)
	}
	if specific["hookEventName"] != "UserPromptSubmit" {
		t.Errorf("hookEventName = %v, want UserPromptSubmit", specific["hookEventName"])
	}
	text, _ := specific["additionalContext"].(string)
	if !strings.Contains(text, "mcp__engram__mem_current_project") {
		t.Errorf("bootstrap does not tell the agent to call mem_current_project first: %q", text)
	}
	if !strings.Contains(text, "mcp__engram__mem_save") {
		t.Errorf("bootstrap does not list the tool names: %q", text)
	}
	if _, ok := obj["systemMessage"]; ok {
		t.Error("bootstrap used systemMessage, which is rendered in the terminal and never reaches the model")
	}

	// The prompt itself must have been captured for the session.
	count, err := components.store.CountPromptsForSession(sessionID, "prompt-repo", "add the missing index")
	if err != nil {
		t.Fatalf("CountPromptsForSession: %v", err)
	}
	if count != 1 {
		t.Errorf("captured prompts = %d, want 1", count)
	}
}

// TestHookUserPromptSubmit_SecondPromptIsSilent pins the debounce: the
// bootstrap fires once per session, and a fresh session with nothing stale to
// report says nothing at all. A hook that injected on every message would spend
// the user's context window on reminders.
func TestHookUserPromptSubmit_SecondPromptIsSilent(t *testing.T) {
	dbPath, _ := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "prompt-repo-2")
	sessionID := "hook-prompt-2-" + t.Name()
	cleanupHookState(t, sessionID)

	input := map[string]any{"session_id": sessionID, "cwd": repo, "prompt": "first"}
	_ = runHook(t, "user-prompt-submit", input, "--db", dbPath)

	input["prompt"] = "second"
	out := runHook(t, "user-prompt-submit", input, "--db", dbPath)

	if obj := decodeHookJSON(t, out); len(obj) != 0 {
		t.Errorf("second prompt of a fresh session should print {}; got %v", obj)
	}
}

// TestHookUserPromptSubmit_NudgeAfterStaleSession covers the reminder path with
// its two clocks wound forward: an old session (the state file's mtime) and a
// project whose newest memory is older than the staleness bar.
func TestHookUserPromptSubmit_NudgeAfterStaleSession(t *testing.T) {
	dbPath, _ := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "stale-repo")
	sessionID := "hook-nudge-" + t.Name()
	cleanupHookState(t, sessionID)

	// First prompt creates the state file; age it past both thresholds. No
	// memory exists for this project, so the session's own age is the clock.
	_ = runHook(t, "user-prompt-submit", map[string]any{
		"session_id": sessionID, "cwd": repo, "prompt": "first",
	}, "--db", dbPath)
	ageHookState(t, hookStateFile(sessionID, "tools-loaded"), 30*time.Minute)

	out := runHook(t, "user-prompt-submit", map[string]any{
		"session_id": sessionID, "cwd": repo, "prompt": "second",
	}, "--db", dbPath)

	obj := decodeHookJSON(t, out)
	specific, ok := obj["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("expected a save reminder on a stale session; got %v", obj)
	}
	text, _ := specific["additionalContext"].(string)
	if !strings.Contains(text, "MEMORY REMINDER") || !strings.Contains(text, "stale-repo") {
		t.Errorf("reminder text = %q, want it to name the project", text)
	}

	// Cooldown: the very next prompt must be silent, or an agent with nothing to
	// save gets nagged on every message forever.
	out = runHook(t, "user-prompt-submit", map[string]any{
		"session_id": sessionID, "cwd": repo, "prompt": "third",
	}, "--db", dbPath)
	if obj := decodeHookJSON(t, out); len(obj) != 0 {
		t.Errorf("reminder repeated inside its cooldown; got %v", obj)
	}
}

// TestHookUserPromptSubmit_RecentSaveSuppressesNudge proves the second clock is
// real: an old session whose project was saved to a moment ago has nothing to
// be reminded about.
func TestHookUserPromptSubmit_RecentSaveSuppressesNudge(t *testing.T) {
	dbPath, components := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "fresh-save-repo")
	sessionID := "hook-fresh-" + t.Name()
	cleanupHookState(t, sessionID)

	saveTool := components.mcpServer.ListTools()["mem_save"]
	if _, err := saveTool.Handler(t.Context(), newToolRequest("mem_save", map[string]any{
		"title": "just saved", "directory": repo,
	})); err != nil {
		t.Fatalf("seed mem_save: %v", err)
	}

	_ = runHook(t, "user-prompt-submit", map[string]any{
		"session_id": sessionID, "cwd": repo, "prompt": "first",
	}, "--db", dbPath)
	ageHookState(t, hookStateFile(sessionID, "tools-loaded"), 30*time.Minute)

	out := runHook(t, "user-prompt-submit", map[string]any{
		"session_id": sessionID, "cwd": repo, "prompt": "second",
	}, "--db", dbPath)

	if obj := decodeHookJSON(t, out); len(obj) != 0 {
		t.Errorf("a project saved to seconds ago must not trigger a reminder; got %v", obj)
	}
}

// TestHookUserPromptSubmit_NoDaemonStaysWithinBudget is the budget contract.
// The hook sits between the user pressing Enter and the message being sent, so
// an unreachable daemon must cost milliseconds, not a timeout. The deadline is
// generous (4x the 200ms budget) to survive a loaded CI box while still failing
// loudly if the hook ever starts waiting on a network timeout.
func TestHookUserPromptSubmit_NoDaemonStaysWithinBudget(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "absent.db")
	sessionID := "hook-budget-" + t.Name()
	cleanupHookState(t, sessionID)
	// Not the first prompt: the first-prompt path short-circuits before the
	// nudge work, which is the expensive half.
	hookTouchState(hookStateFile(sessionID, "tools-loaded"))
	ageHookState(t, hookStateFile(sessionID, "tools-loaded"), 30*time.Minute)

	start := time.Now()
	out := runHook(t, "user-prompt-submit", map[string]any{
		"session_id": sessionID, "cwd": t.TempDir(), "prompt": "hello",
	}, "--db", dbPath)
	elapsed := time.Since(start)

	if obj := decodeHookJSON(t, out); len(obj) != 0 {
		t.Errorf("with no daemon the hook must print {}; got %v", obj)
	}
	if budget := 4 * hookBudgetPrompt; elapsed > budget {
		t.Errorf("hook took %v with no daemon, want under %v — it is blocking the user's prompt", elapsed, budget)
	}
}

// TestHookSubagentStop_SavesReport covers passive capture: a subagent's context
// dies with it, so its closing report is the only copy of what it found.
func TestHookSubagentStop_SavesReport(t *testing.T) {
	dbPath, components := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "subagent-repo")

	out := runHook(t, "subagent-stop", map[string]any{
		"session_id":             "hook-subagent-1",
		"cwd":                    repo,
		"last_assistant_message": "Found the deadlock in the writer queue\nDetails follow.",
	}, "--db", dbPath)

	if obj := decodeHookJSON(t, out); len(obj) != 0 {
		t.Errorf("subagent-stop must print {}; got %v", obj)
	}

	results, _, err := components.store.SearchMemoriesFiltered("deadlock", "subagent-repo", 10, localstore.SearchFilter{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("saved observations = %d, want 1", len(results))
	}
	if !strings.HasPrefix(results[0].Title, "subagent-stop:") {
		t.Errorf("title = %q, want it to name the source — nobody reviewed this text", results[0].Title)
	}
}

// TestHookSubagentStop_EmptyMessageSavesNothing — an empty report is not an
// observation. Saving one would fill the project with untitled noise.
func TestHookSubagentStop_EmptyMessageSavesNothing(t *testing.T) {
	dbPath, components := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "subagent-empty-repo")

	out := runHook(t, "subagent-stop", map[string]any{
		"session_id": "hook-subagent-2", "cwd": repo, "last_assistant_message": "   ",
	}, "--db", dbPath)

	if obj := decodeHookJSON(t, out); len(obj) != 0 {
		t.Errorf("subagent-stop must print {}; got %v", obj)
	}
	count, err := components.store.CountLiveByProject("subagent-empty-repo")
	if err != nil {
		t.Fatalf("CountLiveByProject: %v", err)
	}
	if count != 0 {
		t.Errorf("observations = %d, want 0 for an empty report", count)
	}
}

// TestHookSessionEnd_ClosesSession covers the close, and the summary it must
// NOT invent: the agent writes that with mem_session_summary, and a hook-filled
// "session ended" would overwrite the one thing the next session reads.
func TestHookSessionEnd_ClosesSession(t *testing.T) {
	dbPath, components := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "ending-repo")

	if err := components.store.CreateSession("hook-session-end", "ending-repo", repo); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	out := runHook(t, "session-end", map[string]any{
		"session_id": "hook-session-end", "cwd": repo,
	}, "--db", dbPath)

	if obj := decodeHookJSON(t, out); len(obj) != 0 {
		t.Errorf("session-end must print {}; got %v", obj)
	}
	sess, err := components.store.GetSession("hook-session-end")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.EndedAt == nil {
		t.Error("session-end did not close the session")
	}
	if sess.Summary != nil && strings.TrimSpace(*sess.Summary) != "" {
		t.Errorf("session-end invented a summary (%q); that field belongs to mem_session_summary", *sess.Summary)
	}
}

// TestHook_MalformedStdinFailsOpen walks every event with input no host would
// send. None may error, and each must still honour its stdout contract — a hook
// that dies on unexpected input takes the user's prompt with it.
func TestHook_MalformedStdinFailsOpen(t *testing.T) {
	dbPath, _ := hookDaemonFixture(t)

	for _, stdin := range []string{"", "   ", "not json at all", `["array","not","object"]`, `{"session_id":`} {
		for _, event := range []string{"session-start", "post-compaction", "user-prompt-submit", "subagent-stop", "session-end"} {
			out := runHookRaw(t, event, stdin, "--db", dbPath)
			switch event {
			case "session-start", "post-compaction":
				if !strings.Contains(out, "Engram provides persistent memory") {
					t.Errorf("%s with stdin %q printed no protocol text", event, stdin)
				}
			default:
				decodeHookJSON(t, out) // fails the test if it is not exactly one JSON object
			}
		}
	}
}

// TestHook_UnknownEventIsAUsageError is the one place a hook is allowed to
// fail: an event name that does not exist can only come from a hand-edited
// settings file, and a silent no-op would hide that typo forever.
func TestHook_UnknownEventIsAUsageError(t *testing.T) {
	if err := runHookCmd([]string{"not-an-event"}); err == nil {
		t.Error("an unknown hook event must be reported, not silently ignored")
	}
	if err := runHookCmd(nil); err == nil {
		t.Error("a missing hook event must be reported")
	}
}

// TestHookToolNames_MatchRegisteredTools is the anti-drift guard for the
// first-prompt bootstrap. The list is what a tool-deferring host uses to load
// engram's tools: a name that the daemon does not serve is a tool the agent
// tries and fails to call, and a tool missing from the list is one it never
// discovers.
func TestHookToolNames_MatchRegisteredTools(t *testing.T) {
	components, err := buildDaemon(daemonCfg{
		db:           filepath.Join(t.TempDir(), "tool_names.db"),
		syncInterval: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	registered := map[string]bool{}
	for name := range components.mcpServer.ListTools() {
		registered[name] = true
	}
	listed := map[string]bool{}
	for _, name := range hookMCPToolNames {
		listed[name] = true
		if !registered[name] {
			t.Errorf("hookMCPToolNames names %q, which the daemon does not register", name)
		}
	}
	for name := range registered {
		if !listed[name] {
			t.Errorf("tool %q is registered but missing from hookMCPToolNames — a deferring host will never load it", name)
		}
	}
}

// TestParseToolResult_AcceptsSSEAndJSON pins the response unwrapping. The MCP
// Streamable HTTP transport may answer a tools/call with a bare JSON-RPC object
// or with an SSE frame, and the hooks consume the payload themselves rather
// than piping it to a client that would.
func TestParseToolResult_AcceptsSSEAndJSON(t *testing.T) {
	const payload = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"hello"}]}}`

	for name, body := range map[string]string{
		"plain json": payload,
		"sse":        "event: message\ndata: " + payload + "\n\n",
		"sse crlf":   "event: message\r\ndata: " + payload + "\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseToolResult("mem_context", []byte(body))
			if err != nil {
				t.Fatalf("parseToolResult: %v", err)
			}
			if got != "hello" {
				t.Errorf("text = %q, want %q", got, "hello")
			}
		})
	}
}

// TestParseToolResult_SurfacesToolErrors — a tool error carries the tool's own
// message, which is the only thing that distinguishes "ambiguous project" from
// "daemon is down" in a stderr line someone has to debug from.
func TestParseToolResult_SurfacesToolErrors(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"ambiguous project"}]}}`
	_, err := parseToolResult("mem_save", []byte(body))
	if err == nil || !strings.Contains(err.Error(), "ambiguous project") {
		t.Errorf("error = %v, want it to carry the tool's message", err)
	}

	body = `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`
	if _, err := parseToolResult("mem_save", []byte(body)); err == nil ||
		!strings.Contains(err.Error(), "method not found") {
		t.Errorf("error = %v, want it to carry the JSON-RPC error", err)
	}
}

// TestHookStateFile_IsHashedAndStable — the session id is host-supplied text
// that ends up in a filesystem path. Hashing it makes traversal impossible and
// the length fixed, while keeping the per-session identity the debounce needs.
func TestHookStateFile_IsHashedAndStable(t *testing.T) {
	evil := hookStateFile("../../etc/passwd", "tools-loaded")
	if strings.Contains(evil, "..") || strings.Contains(evil, "passwd") {
		t.Errorf("state path %q embeds caller-controlled text", evil)
	}
	if filepath.Dir(evil) != filepath.Clean(os.TempDir()) {
		t.Errorf("state file %q escaped the temp directory", evil)
	}
	if again := hookStateFile("../../etc/passwd", "tools-loaded"); again != evil {
		t.Errorf("state path is not stable: %q vs %q", evil, again)
	}
	if same := hookStateFile("other-session", "tools-loaded"); same == evil {
		t.Error("two different sessions share one state file")
	}
}

// TestTruncateForHook_CutsOnRuneBoundary — the context cap must not split a
// multi-byte rune, which would put U+FFFD in the model's context.
func TestTruncateForHook_CutsOnRuneBoundary(t *testing.T) {
	long := strings.Repeat("é", 4000) // 8000 bytes
	got := truncateForHook(long, 1024)
	if len(got) > 1024 {
		t.Errorf("truncated length = %d, want <= 1024", len(got))
	}
	if !strings.Contains(got, "[context truncated") {
		t.Errorf("truncation is not disclosed: %q", got[len(got)-80:])
	}
	if strings.ContainsRune(got, '�') {
		t.Error("truncation split a multi-byte rune")
	}
	if short := "small"; truncateForHook(short, 1024) != short {
		t.Error("input under the limit must be returned unchanged")
	}
}

// ── test helpers ────────────────────────────────────────────────────────────

// stubSyncController is the minimum /api/v1/status needs to look like a real
// daemon to the auto-start probe: a Status with a non-empty daemon version
// (the server fills that in from its own version, so an empty struct is
// enough). Nothing the hooks do touches the other methods.
type stubSyncController struct{}

func (stubSyncController) Status() controlapi.Status                { return controlapi.Status{} }
func (stubSyncController) TriggerNow(context.Context) error         { return nil }
func (stubSyncController) Disconnect() error                        { return nil }
func (stubSyncController) Reconnect(controlapi.CentralConfig) error { return nil }

// stubConfigStore satisfies the config port; the hooks never read config.
type stubConfigStore struct{}

func (stubConfigStore) Load() (controlapi.RedactedConfig, error) {
	return controlapi.RedactedConfig{}, nil
}
func (stubConfigStore) Apply(controlapi.ConfigPatch) (bool, error) { return false, nil }

// cleanupHookState removes both markers of a session before and after a test,
// so a rerun on the same machine starts from the same state a fresh session
// would (these files live in the real os.TempDir by design).
func cleanupHookState(t *testing.T, sessionID string) {
	t.Helper()
	remove := func() {
		for _, kind := range []string{"tools-loaded", "last-nudge"} {
			_ = os.Remove(hookStateFile(sessionID, kind))
		}
	}
	remove()
	t.Cleanup(remove)
}

// ageHookState backdates a state file's modification time, which is how the
// hook measures session age and nudge cooldown.
func ageHookState(t *testing.T, path string, by time.Duration) {
	t.Helper()
	when := time.Now().Add(-by)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("Chtimes %s: %v", path, err)
	}
}
