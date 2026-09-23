package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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
	return hookDaemonFixtureDelayed(t, 0)
}

// hookDaemonFixtureDelayed is hookDaemonFixture with the control API's
// "newest memory" endpoint artificially slowed. It is how the budget tests get
// a daemon that is UP (so the hook does its full work) but slow (so what bounds
// the hook is its own deadline and nothing else).
func hookDaemonFixtureDelayed(t *testing.T, memoriesDelay time.Duration) (dbPath string, components *daemonComponents) {
	t.Helper()
	isolateHookStateDir(t)

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
	if memoriesDelay > 0 {
		// More specific than "/api/", so ServeMux routes the memories listing here
		// and everything else to the real control API.
		handler := ctrl.Handler()
		mux.HandleFunc("/api/v1/memories", func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(memoriesDelay):
			case <-r.Context().Done():
				// The client gave up: stop pretending to work on its behalf.
				return
			}
			handler.ServeHTTP(w, r)
		})
	}
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

// withUnhurriedHookBudget lifts one event's budget for the rest of the test.
//
// For tests that assert WHAT a hook stored, not how fast: under the real 200ms
// prompt budget, a loaded CI box (Windows runners especially) can spend the
// whole budget before the save goes out, the hook fails open exactly as it
// should, and the test then reads "sql: no rows" — a flake that says nothing
// about the behaviour under test. The budget contract has its own tests
// (TestHookUserPromptSubmit_NoDaemonStaysWithinBudget,
// TestHookUserPromptSubmit_SlowControlAPIStaysWithinBudget,
// TestHookBudgets_FitInsideEveryPackTimeout), which never call this.
func withUnhurriedHookBudget(t *testing.T, event string) {
	t.Helper()
	old, ok := hookBudgets[event]
	if !ok {
		t.Fatalf("no budget for hook event %q", event)
	}
	hookBudgets[event] = time.Minute
	t.Cleanup(func() { hookBudgets[event] = old })
}

// runHookRaw is runHook with a verbatim stdin body, for the malformed-input cases.
//
// It isolates the hook state directory itself rather than trusting each test to
// remember: every hook run here touches markers, and the one test that forgot
// wrote them into the developer's REAL cache directory
// (%LOCALAPPDATA%\engram\hooks), where they outlive the run and quietly decide
// the behaviour of the next one. isolateHookStateDir is idempotent — a test
// that also calls it (via hookDaemonFixture) simply gets a second temp
// directory, and nothing outside it is touched either way.
func runHookRaw(t *testing.T, event, stdin string, args ...string) string {
	t.Helper()
	isolateHookStateDir(t)

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

	if !strings.Contains(out, "ENGRAM MEMORY IS ACTIVE") {
		t.Errorf("session-start did not print the protocol pointer; got:\n%s", out)
	}
	if !strings.Contains(out, "an earlier decision") {
		t.Errorf("session-start did not print the project's memory context; got:\n%s", out)
	}
	// The MCP server already delivers the protocol through its initialize result
	// (instructions.go). Printing it here too spends several KiB of the model's
	// context on a second copy of a document it already has — on every session
	// start, and again on every compaction.
	if strings.Contains(out, "Engram provides persistent memory") {
		t.Errorf("session-start re-printed the full server protocol instead of a pointer to it; got:\n%s", out)
	}
	if !strings.Contains(out, `"hooked-repo"`) {
		t.Errorf("the pointer does not name the resolved project; got:\n%s", out)
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

	if !strings.Contains(out, "ENGRAM MEMORY IS ACTIVE") {
		t.Errorf("post-compaction did not print the protocol pointer; got:\n%s", out)
	}
	if strings.Contains(out, "Engram provides persistent memory") {
		t.Errorf("post-compaction re-printed the full server protocol; the MCP initialize result carries it:\n%s", out)
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

// TestHookSessionStart_NoDaemon_StillPrintsThePointer is the fail-open contract
// for the injection half: the pointer needs no daemon, and an agent told
// nothing calls nothing. --no-autostart is mandatory here, not incidental — the
// auto-start path shells out to os.Executable(), which under `go test` is the
// TEST binary, and spawning that as a daemon is not a thing a test may do.
func TestHookSessionStart_NoDaemon_StillPrintsThePointer(t *testing.T) {
	isolateHookStateDir(t)
	dbPath := filepath.Join(t.TempDir(), "absent.db")

	out := runHook(t, "session-start", map[string]any{
		"session_id": "hook-session-3",
		"cwd":        t.TempDir(),
	}, "--db", dbPath, "--no-autostart")

	if !strings.Contains(out, "ENGRAM MEMORY IS ACTIVE") {
		t.Errorf("session-start must print the pointer even with no daemon; got:\n%s", out)
	}
	if !strings.Contains(out, "mem_current_project") {
		t.Errorf("the pointer must name the first call to make; got:\n%s", out)
	}
	// The pointer must work whether the host exposes engram via the global MCP
	// registration (mcp__engram__X) or the Claude Code plugin install
	// (mcp__plugin_engram_engram__X) — it must not hardcode either prefix.
	if strings.Contains(out, hookToolPrefixGlobal+"mem_current_project") {
		t.Errorf("the pointer must not hardcode %q, which a plugin install does not expose; got:\n%s",
			hookToolPrefixGlobal+"mem_current_project", out)
	}
}

// TestHookUserPromptSubmit_FirstPromptBootstraps covers the first message of a
// session: the prompt is captured AND the tool bootstrap is injected, in the
// only field a UserPromptSubmit hook can reach the model through.
func TestHookUserPromptSubmit_FirstPromptBootstraps(t *testing.T) {
	withUnhurriedHookBudget(t, "user-prompt-submit") // asserts what was stored, not how fast
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
	if !strings.Contains(text, "call mem_current_project") {
		t.Errorf("bootstrap does not tell the agent to call mem_current_project first: %q", text)
	}
	// The "Available tools" list is the one place this bootstrap intentionally
	// carries fully-qualified names (for a host that defers tool loading), and
	// it must work whichever install registered the server: both the global
	// (mcp__engram__X) and the Claude Code plugin (mcp__plugin_engram_engram__X)
	// forms must be present.
	for _, want := range []string{hookToolPrefixGlobal + "mem_save", hookToolPrefixPlugin + "mem_save"} {
		if !strings.Contains(text, want) {
			t.Errorf("bootstrap does not list %q: %q", want, text)
		}
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

// TestHookUserPromptSubmit_NoSessionIDClaimsNoMarker is the permanent-marker
// bug. The marker path is the HASH of the session id, so an empty id hashed to
// one fixed name (sha256 of "") shared by every such hook on the machine — and
// nothing ever cleared it, because session-start and session-end both refuse an
// empty id too. The first payload that arrived without a session id claimed
// that file forever: every later one read "not the first prompt" and lost its
// bootstrap, and the nudge measured "session age" from whenever that stray hook
// happened to run.
//
// A session nobody can name has no state to keep. Each such prompt is its own
// first (the bootstrap is static text that needs no project), and no file is
// written — which is what makes the SECOND run below still bootstrap.
func TestHookUserPromptSubmit_NoSessionIDClaimsNoMarker(t *testing.T) {
	dbPath, _ := hookDaemonFixture(t)
	cache := isolateHookStateDir(t)
	repo := pinnedProjectDir(t, "anonymous-prompt-repo")

	for _, prompt := range []string{"first", "second"} {
		out := runHook(t, "user-prompt-submit", map[string]any{
			"cwd": repo, "prompt": prompt, // no session_id at all
		}, "--db", dbPath)
		if _, ok := decodeHookJSON(t, out)["hookSpecificOutput"]; !ok {
			t.Errorf("the %q prompt lost its bootstrap to a marker claimed by an earlier anonymous hook: %s", prompt, out)
		}
	}

	// Nothing may be left behind: the shared marker is the whole bug, and the
	// state directory is where it would be.
	entries, err := os.ReadDir(filepath.Join(cache, "engram", "hooks"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read the state directory: %v", err)
	}
	for _, entry := range entries {
		t.Errorf("a payload with no session id wrote the state file %q; nothing would ever clear it", entry.Name())
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
	// Isolated BEFORE the marker below is written: this test has no daemon
	// fixture, so nothing else would do it, and the marker would land in the
	// developer's real cache directory — where it decides the behaviour of the
	// hook this test then runs.
	isolateHookStateDir(t)
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
				if !strings.Contains(out, "ENGRAM MEMORY IS ACTIVE") {
					t.Errorf("%s with stdin %q printed no protocol pointer", event, stdin)
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
	cache := isolateHookStateDir(t)

	evil := hookStateFile("../../etc/passwd", hookStateToolsLoaded)
	if strings.Contains(evil, "..") || strings.Contains(evil, "passwd") {
		t.Errorf("state path %q embeds caller-controlled text", evil)
	}
	wantDir := filepath.Join(cache, "engram", "hooks")
	if filepath.Dir(evil) != filepath.Clean(wantDir) {
		t.Errorf("state file %q is not under the per-user cache directory %q", evil, wantDir)
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
		for _, kind := range []string{"tools-loaded", "last-nudge", "project"} {
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

// ─── a hook never files under the daemon's own directory ────────────────────

// decoyDaemonCwd points the (in-process) daemon at a directory that resolves to
// a perfectly valid, perfectly wrong project — the shape of a resident daemon
// autostarted from somebody else's repo. Every assertion below is that this
// name never appears in the store.
const decoyProject = "decoy-daemon-repo"

func decoyDaemonCwd(t *testing.T) {
	t.Helper()
	chdirTo(t, pinnedProjectDir(t, decoyProject))
}

// TestHookSubagentStop_NoCwdSavesNothing is the regression test for a hook that
// passed only "directory". With no cwd in the payload the argument is empty, the
// daemon falls back to its OWN working directory, and the subagent's report is
// filed under whatever project that resolves to — a memory that reads exactly
// like real work, in a project the user never opens. The hook now resolves the
// project first (mem_current_project) and refuses to save when the answer
// describes the daemon rather than the session.
func TestHookSubagentStop_NoCwdSavesNothing(t *testing.T) {
	dbPath, components := hookDaemonFixture(t)
	decoyDaemonCwd(t)

	out := runHook(t, "subagent-stop", map[string]any{
		"session_id":             "hook-subagent-no-cwd",
		"last_assistant_message": "Found the deadlock in the writer queue",
	}, "--db", dbPath)

	if obj := decodeHookJSON(t, out); len(obj) != 0 {
		t.Errorf("subagent-stop must still print {}; got %v", obj)
	}
	count, err := components.store.CountLiveByProject(decoyProject)
	if err != nil {
		t.Fatalf("CountLiveByProject: %v", err)
	}
	if count != 0 {
		t.Errorf("%d observation(s) landed under the DAEMON's project %q — a payload with no cwd names no workspace",
			count, decoyProject)
	}
	// Belt and braces: nothing anywhere, under any project.
	results, _, err := components.store.SearchMemoriesFiltered("deadlock", "", 10, localstore.SearchFilter{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("the report was saved under project %q; it should not have been saved at all", results[0].Project)
	}
}

// TestHookUserPromptSubmit_NoCwdCapturesNothing is the same gap on the prompt
// path, which runs on EVERY user message — so a host that omits cwd would fill
// the daemon's own project with one prompt per message.
func TestHookUserPromptSubmit_NoCwdCapturesNothing(t *testing.T) {
	dbPath, components := hookDaemonFixture(t)
	decoyDaemonCwd(t)
	sessionID := "hook-no-cwd-" + t.Name()
	cleanupHookState(t, sessionID)

	out := runHook(t, "user-prompt-submit", map[string]any{
		"session_id": sessionID,
		"prompt":     "add the missing index",
	}, "--db", dbPath)

	// The bootstrap still fires: it is static text and needs no project.
	obj := decodeHookJSON(t, out)
	if _, ok := obj["hookSpecificOutput"]; !ok {
		t.Errorf("first prompt lost its bootstrap because the project was unresolvable: %v", obj)
	}

	count, err := components.store.CountPromptsForSession(sessionID, decoyProject, "add the missing index")
	if err != nil {
		t.Fatalf("CountPromptsForSession: %v", err)
	}
	if count != 0 {
		t.Errorf("the prompt was captured under the DAEMON's project %q", decoyProject)
	}
}

// TestHookSubagentStop_RelativeCwdSavesNothing is the same refusal for a cwd
// that is PRESENT and useless. "." is the value a host (or a wrapper script, or
// a model filling in a field) writes sooner or later, and it is the dangerous
// one: it passes every "is there a cwd?" check, and the daemon resolves it with
// filepath.Abs against its OWN working directory — here, the decoy repo. The
// answer that comes back is a real, existing, confidently-reported project that
// has nothing to do with the session.
//
// On Windows a Git Bash path naming a drive that does not exist is the second
// spelling of the same mistake, and must be refused the same way: it is not a
// relative path, it just is not a path on this machine.
func TestHookSubagentStop_RelativeCwdSavesNothing(t *testing.T) {
	cases := map[string]string{"dot": "."}
	if runtime.GOOS == "windows" {
		// Deliberately a drive letter no Windows machine mounts (A: and B: are
		// floppies, Q: is unused by convention) so the translation cannot succeed.
		cases["untranslatable git bash path"] = "/q/nonexistent/repo"
	}

	for name, cwd := range cases {
		t.Run(name, func(t *testing.T) {
			dbPath, components := hookDaemonFixture(t)
			decoyDaemonCwd(t)

			out := runHook(t, "subagent-stop", map[string]any{
				"session_id":             "hook-subagent-relative-" + name,
				"cwd":                    cwd,
				"last_assistant_message": "Found the deadlock in the writer queue",
			}, "--db", dbPath)

			if obj := decodeHookJSON(t, out); len(obj) != 0 {
				t.Errorf("subagent-stop must still print {}; got %v", obj)
			}
			count, err := components.store.CountLiveByProject(decoyProject)
			if err != nil {
				t.Fatalf("CountLiveByProject: %v", err)
			}
			if count != 0 {
				t.Errorf("cwd=%q filed %d observation(s) under the DAEMON's project %q — that path names no workspace",
					cwd, count, decoyProject)
			}
			results, _, err := components.store.SearchMemoriesFiltered("deadlock", "", 10, localstore.SearchFilter{})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(results) != 0 {
				t.Errorf("the report was saved under project %q; it should not have been saved at all", results[0].Project)
			}
		})
	}
}

// TestHookSubagentStop_FallsBackToTheSessionsProject covers the other half of
// "no usable cwd": a session that WAS registered with one. session-start
// recorded the host's directory for this id, so a later event from the same
// session that arrives without a cwd is not unplaceable — it is already placed,
// and the answer is a lookup away (GET /api/v1/sessions/{id}).
//
// The daemon's cwd is the decoy throughout, so the only way to the right
// project is the session row.
func TestHookSubagentStop_FallsBackToTheSessionsProject(t *testing.T) {
	dbPath, components := hookDaemonFixture(t)
	decoyDaemonCwd(t)
	repo := pinnedProjectDir(t, "session-fallback-repo")
	sessionID := "hook-fallback-" + t.Name()

	// The session is registered WITH a cwd, exactly as a real session-start does.
	_ = runHook(t, "session-start", map[string]any{
		"session_id": sessionID, "cwd": repo,
	}, "--db", dbPath, "--no-autostart")

	// ... and the subagent report arrives without one.
	_ = runHook(t, "subagent-stop", map[string]any{
		"session_id":             sessionID,
		"last_assistant_message": "Found the deadlock in the writer queue",
	}, "--db", dbPath)

	results, _, err := components.store.SearchMemoriesFiltered("deadlock", "", 10, localstore.SearchFilter{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("saved observations = %d, want 1 — the session's own project was the answer", len(results))
	}
	if results[0].Project != "session-fallback-repo" {
		t.Errorf("project = %q, want %q", results[0].Project, "session-fallback-repo")
	}
	count, err := components.store.CountLiveByProject(decoyProject)
	if err != nil {
		t.Fatalf("CountLiveByProject: %v", err)
	}
	if count != 0 {
		t.Errorf("%d observation(s) landed under the DAEMON's project %q", count, decoyProject)
	}
}

// TestHookSubagentStop_RefusedCwdDoesNotFallBackToSessionsProject is the FUP-002
// regression test: a REFUSED cwd (present but relative/missing/ambiguous) must
// NOT fall back to the session's registration the way an EMPTY cwd does. The
// session below is registered with a real project, exactly as
// TestHookSubagentStop_FallsBackToTheSessionsProject's is — the only difference
// is that the later event carries a cwd the daemon refuses instead of no cwd at
// all, and that refusal must stand: nothing gets saved.
func TestHookSubagentStop_RefusedCwdDoesNotFallBackToSessionsProject(t *testing.T) {
	cases := map[string]string{
		"relative":          ".",
		"missing directory": filepath.Join(t.TempDir(), "no", "such", "repo"),
	}

	for name, cwd := range cases {
		t.Run(name, func(t *testing.T) {
			dbPath, components := hookDaemonFixture(t)
			decoyDaemonCwd(t)
			repo := pinnedProjectDir(t, "refused-cwd-repo-"+strings.ReplaceAll(name, " ", "-"))
			sessionID := "hook-refused-cwd-" + t.Name()

			// The session IS registered with a real project — the exact setup that
			// makes the (fixed) empty-cwd fallback succeed.
			_ = runHook(t, "session-start", map[string]any{
				"session_id": sessionID, "cwd": repo,
			}, "--db", dbPath, "--no-autostart")

			// ... but this event carries a cwd the daemon refuses, not an absent one.
			out := runHook(t, "subagent-stop", map[string]any{
				"session_id":             sessionID,
				"cwd":                    cwd,
				"last_assistant_message": "Found the deadlock in the writer queue",
			}, "--db", dbPath)

			if obj := decodeHookJSON(t, out); len(obj) != 0 {
				t.Errorf("subagent-stop must still print {}; got %v", obj)
			}
			results, _, err := components.store.SearchMemoriesFiltered("deadlock", "", 10, localstore.SearchFilter{})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(results) != 0 {
				t.Errorf("cwd=%q was refused but the report was saved under project %q anyway — "+
					"the session fallback caught a refusal it must not catch", cwd, results[0].Project)
			}
		})
	}
}

// TestHookSubagentStop_NamesTheProjectExplicitly proves the fix does not simply
// drop everything: with a cwd in the payload the report is saved, and it is
// saved under the project that cwd resolves to — not under the daemon's, which
// is a different, equally valid-looking name sitting right there.
func TestHookSubagentStop_NamesTheProjectExplicitly(t *testing.T) {
	dbPath, components := hookDaemonFixture(t)
	decoyDaemonCwd(t)
	repo := pinnedProjectDir(t, "subagent-explicit-repo")

	runHook(t, "subagent-stop", map[string]any{
		"session_id":             "hook-subagent-explicit",
		"cwd":                    repo,
		"last_assistant_message": "Found the deadlock in the writer queue",
	}, "--db", dbPath)

	results, _, err := components.store.SearchMemoriesFiltered("deadlock", "", 10, localstore.SearchFilter{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("saved observations = %d, want 1", len(results))
	}
	if results[0].Project != "subagent-explicit-repo" {
		t.Errorf("project = %q, want %q", results[0].Project, "subagent-explicit-repo")
	}
}

// ─── hook state lives in a per-user cache directory ─────────────────────────

// hookStateIsolationEnv marks a state directory this helper has already
// isolated. It is read by isolateHookStateDir alone; nothing in the binary
// knows it exists.
const hookStateIsolationEnv = "ENGRAM_TEST_HOOK_STATE_DIR"

// isolateHookStateDir points os.UserCacheDir (and, on the platforms that
// derive it from HOME, the home directory) at a temp directory for the duration
// of a test, so nothing here writes markers into the developer's real cache.
// It returns the cache root the markers must appear under.
//
// IDEMPOTENT, and that is load-bearing: runHookRaw calls it on every hook
// invocation so no test can forget, and most hook tests run several hooks whose
// whole point is the marker the previous one left (a second prompt is silent, a
// resume re-fires the bootstrap, session-end clears what user-prompt-submit
// created). Re-isolating mid-test would hand each run a fresh empty directory
// and make every prompt look like the first. The sentinel env var is how the
// second call recognises the first: t.Setenv restores all of them when the test
// ends, so the next test isolates again, into its own directory.
func isolateHookStateDir(t *testing.T) string {
	t.Helper()
	if cache := os.Getenv(hookStateIsolationEnv); cache != "" {
		return cache
	}
	cache := t.TempDir()
	t.Setenv(hookStateIsolationEnv, cache)
	t.Setenv("LOCALAPPDATA", cache)   // Windows
	t.Setenv("XDG_CACHE_HOME", cache) // Unix
	t.Setenv("HOME", cache)           // macOS ($HOME/Library/Caches) and the XDG fallback
	return cache
}

// TestHookStateDir_IsPerUserAndPrivate pins the move off os.TempDir. The system
// temp directory is world-writable on Unix and swept by tools that do not know
// what they are deleting; a marker another user can create is a marker another
// user can use to silence someone else's reminders.
func TestHookStateDir_IsPerUserAndPrivate(t *testing.T) {
	cache := isolateHookStateDir(t)

	dir := hookStateDir()

	want := filepath.Join(cache, "engram", "hooks")
	if dir != want {
		t.Fatalf("hookStateDir() = %q, want %q", dir, want)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("the state directory was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", dir)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("state directory mode = %04o, want 0700", perm)
		}
	}
}

// TestHookClaimState_IsAtomic covers the O_EXCL claim that replaced an
// exists-then-create pair: exactly ONE caller may be told it is the first.
//
// Run CONCURRENTLY, and under -race in CI, because sequential calls cannot fail
// the way this code failed: the old exists-then-create pair was perfectly
// correct one call at a time, and wrong only when two prompts landed close
// enough together that both read "absent" — which is the case the user hits
// (two messages in quick succession injecting the bootstrap twice, and the
// session's age clock reset underneath the nudge). A mutant that drops O_EXCL
// still passes a sequential test; it does not survive this one.
func TestHookClaimState_IsAtomic(t *testing.T) {
	isolateHookStateDir(t)
	path := hookStateFile("claim-session", hookStateToolsLoaded)

	const claimants = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
	)
	start := make(chan struct{})
	wg.Add(claimants)
	for range claimants {
		go func() {
			defer wg.Done()
			<-start // release them together: a staggered start tests nothing
			if hookClaimState(path) {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if granted != 1 {
		t.Errorf("%d of %d concurrent claims reported themselves as the first prompt of the session; want exactly 1",
			granted, claimants)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("no marker was left behind after %d claims: %v", claimants, err)
	}
	// And the session stays claimed afterwards, which is what every later prompt
	// reads.
	if hookClaimState(path) {
		t.Error("a claim after the race reported itself as the first prompt of the session")
	}
}

// TestHookSessionStart_ResumeReFiresTheBootstrap is the reason session-start
// clears the markers. `claude --resume` REUSES the session id: the model's
// context is brand new, so the first-prompt bootstrap has to fire again — and
// the nudge's age clock has to start from the resume, not from whenever this id
// first spoke, which may have been days ago on a machine since rebooted.
func TestHookSessionStart_ResumeReFiresTheBootstrap(t *testing.T) {
	dbPath, _ := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "resumed-repo")
	sessionID := "hook-resume-" + t.Name()

	// First prompt of the original session: bootstrap fires.
	first := runHook(t, "user-prompt-submit", map[string]any{
		"session_id": sessionID, "cwd": repo, "prompt": "one",
	}, "--db", dbPath)
	if _, ok := decodeHookJSON(t, first)["hookSpecificOutput"]; !ok {
		t.Fatalf("the first prompt did not bootstrap: %s", first)
	}
	// Second prompt: silent, as designed.
	second := runHook(t, "user-prompt-submit", map[string]any{
		"session_id": sessionID, "cwd": repo, "prompt": "two",
	}, "--db", dbPath)
	if obj := decodeHookJSON(t, second); len(obj) != 0 {
		t.Fatalf("the second prompt should be silent: %v", obj)
	}

	// The resume: same session id, new context.
	_ = runHook(t, "session-start", map[string]any{
		"session_id": sessionID, "cwd": repo,
	}, "--db", dbPath, "--no-autostart")

	resumed := runHook(t, "user-prompt-submit", map[string]any{
		"session_id": sessionID, "cwd": repo, "prompt": "three",
	}, "--db", dbPath)
	if _, ok := decodeHookJSON(t, resumed)["hookSpecificOutput"]; !ok {
		t.Errorf("after a resume the bootstrap did not fire again; the model has no idea the tools exist: %s", resumed)
	}
}

// TestHookSessionEnd_ClearsSessionState — without it the state directory grows
// one pair of files per session, forever, and nothing ever removes them.
func TestHookSessionEnd_ClearsSessionState(t *testing.T) {
	dbPath, _ := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "ending-state-repo")
	sessionID := "hook-end-state-" + t.Name()

	_ = runHook(t, "user-prompt-submit", map[string]any{
		"session_id": sessionID, "cwd": repo, "prompt": "one",
	}, "--db", dbPath)
	marker := hookStateFile(sessionID, hookStateToolsLoaded)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the first prompt left no marker: %v", err)
	}

	_ = runHook(t, "session-end", map[string]any{
		"session_id": sessionID, "cwd": repo,
	}, "--db", dbPath)

	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("session-end left %q behind (%v)", marker, err)
	}
}

// ─── FUP-003: plugin + settings.json both installed run every hook twice ────
//
// The Claude Code plugin (plugin/claude-code/hooks/hooks.json) and
// `engram setup hooks` (settings.json) can both be registered at once, and the
// host then fires every event through BOTH — two independent
// `engram hook <event>` processes, each carrying the identical payload. These
// tests simulate that by calling runHook twice with the same input, exactly as
// two separate process launches would receive it.

// TestHookUserPromptSubmit_DuplicateDeliverySavesPromptOnce is the regression
// test for a duplicate prompt save: the SAME prompt, in the SAME session,
// delivered twice, must be captured exactly once.
func TestHookUserPromptSubmit_DuplicateDeliverySavesPromptOnce(t *testing.T) {
	withUnhurriedHookBudget(t, "user-prompt-submit") // asserts what was stored, not how fast
	dbPath, components := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "dup-prompt-repo")
	sessionID := "hook-dup-prompt-" + t.Name()
	cleanupHookState(t, sessionID)

	input := map[string]any{
		"session_id": sessionID, "cwd": repo, "prompt": "add the missing index",
	}
	_ = runHook(t, "user-prompt-submit", input, "--db", dbPath)
	// Second "delivery" of the identical occurrence — the other install firing
	// the same event with the same stdin payload.
	_ = runHook(t, "user-prompt-submit", input, "--db", dbPath)

	count, err := components.store.CountPromptsForSession(sessionID, "dup-prompt-repo", "add the missing index")
	if err != nil {
		t.Fatalf("CountPromptsForSession: %v", err)
	}
	if count != 1 {
		t.Errorf("prompt saved %d time(s) across two identical deliveries, want exactly 1", count)
	}
}

// TestHookUserPromptSubmit_DedupWindowExpiryStillSaves is the FUP-003b
// regression test: the SAME prompt in the SAME session, delivered again after
// the marker has aged past hookOccurrenceDedupWindow, is a new occurrence (a
// retyped "continue"), not a duplicate delivery, and must be saved again.
func TestHookUserPromptSubmit_DedupWindowExpiryStillSaves(t *testing.T) {
	withUnhurriedHookBudget(t, "user-prompt-submit") // asserts what was stored, not how fast
	dbPath, components := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "stale-marker-prompt-repo")
	sessionID := "hook-stale-marker-prompt-" + t.Name()
	cleanupHookState(t, sessionID)

	input := map[string]any{
		"session_id": sessionID, "cwd": repo, "prompt": "continue",
	}
	_ = runHook(t, "user-prompt-submit", input, "--db", dbPath)

	marker := hookOccurrenceMarkerFile(sessionID, "user-prompt-submit", "continue")
	ageHookState(t, marker, hookOccurrenceDedupWindow+time.Second)

	_ = runHook(t, "user-prompt-submit", input, "--db", dbPath)

	count, err := components.store.CountPromptsForSession(sessionID, "stale-marker-prompt-repo", "continue")
	if err != nil {
		t.Fatalf("CountPromptsForSession: %v", err)
	}
	if count != 2 {
		t.Errorf("prompt saved %d time(s) once its marker aged past the dedup window, want 2", count)
	}
}

// TestHookUserPromptSubmit_DifferentPromptsBothSaved proves the dedup is keyed
// on the occurrence, not just the session: two DIFFERENT prompts in the same
// session must both be captured.
func TestHookUserPromptSubmit_DifferentPromptsBothSaved(t *testing.T) {
	withUnhurriedHookBudget(t, "user-prompt-submit") // asserts what was stored, not how fast
	dbPath, components := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "distinct-prompt-repo")
	sessionID := "hook-distinct-prompt-" + t.Name()
	cleanupHookState(t, sessionID)

	_ = runHook(t, "user-prompt-submit", map[string]any{
		"session_id": sessionID, "cwd": repo, "prompt": "first prompt",
	}, "--db", dbPath)
	_ = runHook(t, "user-prompt-submit", map[string]any{
		"session_id": sessionID, "cwd": repo, "prompt": "second prompt",
	}, "--db", dbPath)

	for _, prompt := range []string{"first prompt", "second prompt"} {
		count, err := components.store.CountPromptsForSession(sessionID, "distinct-prompt-repo", prompt)
		if err != nil {
			t.Fatalf("CountPromptsForSession(%q): %v", prompt, err)
		}
		if count != 1 {
			t.Errorf("prompt %q saved %d time(s), want exactly 1", prompt, count)
		}
	}
}

// TestHookSubagentStop_DuplicateDeliverySavesReportOnce is the regression test
// for a duplicate subagent report: the SAME closing message, in the SAME
// session, delivered twice, must be saved exactly once.
func TestHookSubagentStop_DuplicateDeliverySavesReportOnce(t *testing.T) {
	dbPath, components := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "dup-subagent-repo")

	input := map[string]any{
		"session_id":             "hook-dup-subagent-" + t.Name(),
		"cwd":                    repo,
		"last_assistant_message": "Found the deadlock in the writer queue",
	}
	_ = runHook(t, "subagent-stop", input, "--db", dbPath)
	_ = runHook(t, "subagent-stop", input, "--db", dbPath)

	results, _, err := components.store.SearchMemoriesFiltered("deadlock", "dup-subagent-repo", 10, localstore.SearchFilter{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("report saved %d time(s) across two identical deliveries, want exactly 1", len(results))
	}
}

// TestHookSubagentStop_DifferentReportsBothSaved is the subagent-stop
// counterpart to TestHookUserPromptSubmit_DifferentPromptsBothSaved: two
// DIFFERENT reports in the same session must both be saved.
func TestHookSubagentStop_DifferentReportsBothSaved(t *testing.T) {
	dbPath, components := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "distinct-subagent-repo")
	sessionID := "hook-distinct-subagent-" + t.Name()

	_ = runHook(t, "subagent-stop", map[string]any{
		"session_id": sessionID, "cwd": repo, "last_assistant_message": "Found the deadlock",
	}, "--db", dbPath)
	_ = runHook(t, "subagent-stop", map[string]any{
		"session_id": sessionID, "cwd": repo, "last_assistant_message": "Fixed the race condition",
	}, "--db", dbPath)

	count, err := components.store.CountLiveByProject("distinct-subagent-repo")
	if err != nil {
		t.Fatalf("CountLiveByProject: %v", err)
	}
	if count != 2 {
		t.Errorf("saved %d report(s) for two distinct messages, want exactly 2", count)
	}
}

// TestHookClaimOccurrence_EmptySessionNeverDedupes mirrors hookClaimState's own
// empty-id carve-out: the marker's session component is a hash, so an unnamed
// session would share ONE marker across every hook on the machine, and an
// unrelated later occurrence would wrongly read "already claimed".
func TestHookClaimOccurrence_EmptySessionNeverDedupes(t *testing.T) {
	isolateHookStateDir(t)

	if !hookClaimOccurrence("", "user-prompt-submit", "same text") {
		t.Error("first call with an empty session id must claim")
	}
	if !hookClaimOccurrence("", "user-prompt-submit", "same text") {
		t.Error("a second call with an empty session id must ALSO claim — no dedup without a session to key on")
	}
}

// TestHookClaimOccurrence_DedupWindowBoundary pins hookClaimOccurrence's own
// window logic directly: a marker still inside hookOccurrenceDedupWindow
// blocks the repeat, and one just past it does not.
func TestHookClaimOccurrence_DedupWindowBoundary(t *testing.T) {
	isolateHookStateDir(t)

	if !hookClaimOccurrence("window-session", "user-prompt-submit", "same text") {
		t.Fatal("first claim must succeed")
	}
	if hookClaimOccurrence("window-session", "user-prompt-submit", "same text") {
		t.Error("a claim milliseconds later must be treated as the same delivery and blocked")
	}

	marker := hookOccurrenceMarkerFile("window-session", "user-prompt-submit", "same text")
	ageHookState(t, marker, hookOccurrenceDedupWindow+time.Second)

	if !hookClaimOccurrence("window-session", "user-prompt-submit", "same text") {
		t.Error("a claim past the dedup window must be treated as a NEW occurrence and allowed")
	}
	// And the window slides: immediately after that refresh, a repeat is a
	// near-simultaneous duplicate again.
	if hookClaimOccurrence("window-session", "user-prompt-submit", "same text") {
		t.Error("the refreshed marker should immediately re-block a near-simultaneous repeat")
	}
}

// TestHookClaimState_NonExistErrorFailsOpen is the claim-dir-failure case
// hookClaimOccurrence inherits from hookClaimState unchanged: a creation
// failure that is NOT os.ErrExist must report true (proceed with the save), not
// false (silently skip it). A NUL byte is invalid in a path on every OS Go
// supports and is rejected by the os package itself before any syscall — a
// portable stand-in for a permission error or a hostile antivirus lock, which
// this test cannot reliably provoke by name.
func TestHookClaimState_NonExistErrorFailsOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad\x00path")

	if !hookClaimState(path) {
		t.Errorf("hookClaimState(%q) = false, want true — a non-ErrExist failure must fail OPEN "+
			"(duplicates are better than a lost save)", path)
	}
}

// TestHookSessionEnd_ClearsOccurrenceMarkers proves hookClearState's cleanup
// covers occurrence markers too: without it, the state directory would grow one
// file per distinct prompt/report per session, forever, exactly as the comment
// on hookClearState warns.
func TestHookSessionEnd_ClearsOccurrenceMarkers(t *testing.T) {
	dbPath, _ := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "occ-cleanup-repo")
	sessionID := "hook-occ-cleanup-" + t.Name()

	_ = runHook(t, "user-prompt-submit", map[string]any{
		"session_id": sessionID, "cwd": repo, "prompt": "clean me up",
	}, "--db", dbPath)

	marker := hookOccurrenceMarkerFile(sessionID, "user-prompt-submit", "clean me up")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the prompt left no occurrence marker: %v", err)
	}

	_ = runHook(t, "session-end", map[string]any{
		"session_id": sessionID, "cwd": repo,
	}, "--db", dbPath)

	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("session-end left the occurrence marker %q behind (%v)", marker, err)
	}
}

// TestHookSweepStaleOccurrenceMarkers_RemovesOnlyStaleOnes is the TTL backstop
// for a session that never reaches session-end (a crashed agent). It must
// remove a marker older than hookOccurrenceMarkerTTL and leave a fresh one
// alone — the sweep runs on every occurrence claim, so a sweep that deleted
// everything would make the dedup it backs up worthless.
func TestHookSweepStaleOccurrenceMarkers_RemovesOnlyStaleOnes(t *testing.T) {
	isolateHookStateDir(t)

	stale := hookOccurrenceMarkerFile("stale-session", "user-prompt-submit", "old")
	fresh := hookOccurrenceMarkerFile("fresh-session", "user-prompt-submit", "new")
	if !hookClaimState(stale) {
		t.Fatal("could not create the stale marker fixture")
	}
	if !hookClaimState(fresh) {
		t.Fatal("could not create the fresh marker fixture")
	}
	ageHookState(t, stale, hookOccurrenceMarkerTTL+time.Hour)

	// Force the sweep to run now regardless of its own cooldown.
	if err := os.Remove(hookOccurrenceSweepMarkerFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("could not reset the sweep cooldown: %v", err)
	}
	hookSweepStaleOccurrenceMarkers()

	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the sweep left the stale marker behind (%v)", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the sweep removed a marker well inside its TTL: %v", err)
	}
}

// ─── budgets ────────────────────────────────────────────────────────────────

// TestHookBudgets_FitInsideEveryPackTimeout is the guard the Codex session-end
// budget needed: it was 4 seconds against a pack that declares a 3-second
// timeout, so the host killed the hook a full second BEFORE the binary intended
// to give up — precisely the case the margin exists to prevent, and for the
// JSON events a kill mid-write is a parse error on the host's side.
//
// What it asserts is hookStdinDeadline + budget < timeout, not budget alone.
// The budget covers stdin today (one clock, started in runHookCmd), but the
// stdin deadline is a SEPARATE constant that a future change could put back in
// front of the budget — which is exactly how 4 seconds of session-end work
// became a 6-second process against a 3-second timeout. Asserting the sum keeps
// the margin true under both arrangements, and costs one second of headroom.
//
// The table is generated from engramHookPack, so a pack that lowers a timeout
// fails here instead of in somebody's terminal.
func TestHookBudgets_FitInsideEveryPackTimeout(t *testing.T) {
	for _, agent := range []string{"claude-code", "codex"} {
		t.Run(agent, func(t *testing.T) {
			for _, item := range engramHookPack(agent).Events {
				for _, entry := range item.Group.Hooks {
					event := strings.TrimPrefix(entry.Command, "engram hook ")
					budget, ok := hookBudgets[event]
					if !ok {
						t.Errorf("pack command %q has no entry in hookBudgets — the binary would run it with no budget at all",
							entry.Command)
						continue
					}
					if entry.Timeout <= 0 {
						t.Errorf("%s hook %q declares no timeout", item.Event, entry.Command)
						continue
					}
					timeout := time.Duration(entry.Timeout) * time.Second
					if worst := hookStdinDeadline + budget; worst >= timeout {
						t.Errorf("%s hook %q: stdin deadline %s + budget %s = %s >= declared timeout %s — "+
							"the host can kill the hook before it prints its fallback",
							item.Event, entry.Command, hookStdinDeadline, budget, worst, timeout)
					}
				}
			}
		})
	}
}

// TestHookStdinBound_NeverOutlivesTheBudget covers the min() that keeps the
// prompt hook honest: its whole budget is 200ms, so the 1-second ceiling would
// blow it five times over on a host that leaves stdin open.
func TestHookStdinBound_NeverOutlivesTheBudget(t *testing.T) {
	cases := []struct {
		name      string
		remaining time.Duration
		want      time.Duration
	}{
		{"a long budget gets the ceiling", hookBudgetSessionStart, hookStdinDeadline},
		{"exactly the ceiling", hookStdinDeadline, hookStdinDeadline},
		{"the prompt budget wins", hookBudgetPrompt, hookBudgetPrompt},
		{"nothing left means do not wait", -time.Second, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hookStdinBound(tc.remaining); got != tc.want {
				t.Errorf("hookStdinBound(%s) = %s, want %s", tc.remaining, got, tc.want)
			}
		})
	}
}

// TestHookSessionEnd_WholeProcessFitsTheBudget is the end-to-end half of the
// same fix: the clock starts at process entry, so a hook whose stdin never
// closes must still be done within its budget — not stdin PLUS its budget,
// which is what the host's timeout was being blown by.
//
// The pipe's write end is deliberately left open (no payload, no EOF), and
// there is no daemon: the hook has to give up on stdin, print its fallback and
// return, all inside the session-end budget.
func TestHookSessionEnd_WholeProcessFitsTheBudget(t *testing.T) {
	isolateHookStateDir(t)
	dbPath := filepath.Join(t.TempDir(), "no-daemon.db") // no daemon.json beside it

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { _ = w.Close(); _ = r.Close(); os.Stdin = oldStdin })
	os.Stdin = r

	start := time.Now()
	out, _ := captureHookStreams(t, func() {
		if err := runHookCmd([]string{"session-end", "--db", dbPath}); err != nil {
			t.Errorf("hook session-end returned an error: %v", err)
		}
	})
	elapsed := time.Since(start)

	if obj := decodeHookJSON(t, out); len(obj) != 0 {
		t.Errorf("session-end must print {} when it has no payload; got %v", obj)
	}
	// The budget itself, with process slack — NOT stdin + budget, which is the
	// arithmetic that used to exceed the Codex pack's 3-second timeout.
	if limit := hookBudgetSessionEnd + 500*time.Millisecond; elapsed > limit {
		t.Errorf("session-end took %v on an unclosed stdin, want at most %v (its whole budget)", elapsed, limit)
	}
}

// ─── stdin and prompt bounds ────────────────────────────────────────────────

// TestReadHookInput_ReturnsOnAnUnclosedStdin covers the hang nobody sees until
// it happens: io.ReadAll waits for EOF, so a host that hands the hook a pipe it
// never closes (a wrapper script, a shell holding the write end, a parent that
// died on Windows) blocks the hook forever — not for its budget, forever, with
// the user's prompt behind it.
func TestReadHookInput_ReturnsOnAnUnclosedStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { _ = w.Close(); _ = r.Close() })

	// Write a complete payload and DO NOT close the write end.
	if _, err := w.WriteString(`{"session_id":"never-closed"}`); err != nil {
		t.Fatalf("write: %v", err)
	}

	start := time.Now()
	in := readHookInput(r, hookStdinDeadline)
	elapsed := time.Since(start)

	if elapsed > 2*hookStdinDeadline {
		t.Errorf("readHookInput took %v on an unclosed stdin, want ~%v", elapsed, hookStdinDeadline)
	}
	// The payload is not required to survive — the deadline fires before EOF, so
	// the zero value is the documented outcome. What matters is that it returns.
	_ = in
}

// TestReadHookInput_StillReadsAClosedStdinImmediately — the deadline must not
// cost the normal path anything.
func TestReadHookInput_StillReadsAClosedStdinImmediately(t *testing.T) {
	in := readHookInput(strings.NewReader(`{"session_id":"s","prompt":"p"}`), hookStdinDeadline)
	if in.SessionID != "s" || in.Prompt != "p" {
		t.Errorf("readHookInput = %+v, want the payload decoded", in)
	}
}

// TestHookUserPromptSubmit_TruncatesAHugePrompt — a prompt can carry a pasted
// file. Saved whole it becomes a memory nobody can read, and mem_save then
// attaches it to an observation, spending the model's context on it twice.
//
// It asserts the stored bytes, so it runs without the 200ms budget: under that
// budget a loaded Windows CI runner timed out the save (fail-open, correctly)
// and this read "sql: no rows". See withUnhurriedHookBudget.
func TestHookUserPromptSubmit_TruncatesAHugePrompt(t *testing.T) {
	withUnhurriedHookBudget(t, "user-prompt-submit")
	dbPath, components := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "huge-prompt-repo")
	sessionID := "hook-huge-" + t.Name()

	huge := strings.Repeat("x", hookContextLimit*2)
	_ = runHook(t, "user-prompt-submit", map[string]any{
		"session_id": sessionID, "cwd": repo, "prompt": huge,
	}, "--db", dbPath)

	// Read the row directly: the store exposes no list-by-session helper, and
	// what is under test is the exact bytes that reached the table.
	var content string
	if err := components.store.DB().QueryRow(
		`SELECT content FROM user_prompts WHERE session_id = ?`, sessionID).Scan(&content); err != nil {
		t.Fatalf("read the stored prompt: %v", err)
	}
	if len(content) > hookContextLimit {
		t.Errorf("stored prompt is %d bytes, want at most %d", len(content), hookContextLimit)
	}
	if !strings.Contains(content, "[context truncated") {
		t.Error("the truncation is not disclosed in the stored prompt")
	}
}

// ─── the prompt budget bounds the control call too ──────────────────────────

// TestHookUserPromptSubmit_SlowControlAPIStaysWithinBudget puts a live but SLOW
// daemon behind the nudge's one control-API call. The hook runs between the
// user pressing Enter and their message being sent: whatever the daemon is
// doing, the hook's own deadline is what decides when it stops waiting.
func TestHookUserPromptSubmit_SlowControlAPIStaysWithinBudget(t *testing.T) {
	dbPath, _ := hookDaemonFixtureDelayed(t, 2*time.Second)
	repo := pinnedProjectDir(t, "slow-control-repo")
	sessionID := "hook-slow-" + t.Name()

	// Not the first prompt, and old enough to reach the nudge — which is the
	// only thing that calls the control API.
	hookTouchState(hookStateFile(sessionID, hookStateToolsLoaded))
	ageHookState(t, hookStateFile(sessionID, hookStateToolsLoaded), 30*time.Minute)

	start := time.Now()
	out := runHook(t, "user-prompt-submit", map[string]any{
		"session_id": sessionID, "cwd": repo, "prompt": "hello",
	}, "--db", dbPath)
	elapsed := time.Since(start)

	if obj := decodeHookJSON(t, out); len(obj) != 0 {
		t.Errorf("with an unusable control API the hook must print {}; got %v", obj)
	}
	// Process slack, not budget slack: the assertion that matters is that this
	// is nowhere near the server's 2s sleep.
	if limit := time.Second; elapsed > limit {
		t.Errorf("hook took %v against a daemon that sleeps 2s, want well under %v", elapsed, limit)
	}
}

// ─── the per-session project cache ──────────────────────────────────────────

// TestHookSessionStart_CachesProjectForThePromptHook — session-start resolves
// the workspace inside an 8s budget; the prompt hook must not pay for that
// again (two git spawns in the daemon) inside its 200ms one.
func TestHookSessionStart_CachesProjectForThePromptHook(t *testing.T) {
	dbPath, _ := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "cached-start-repo")
	sessionID := "hook-cache-start-" + t.Name()
	cleanupHookState(t, sessionID)

	_ = runHook(t, "session-start", map[string]any{"session_id": sessionID, "cwd": repo},
		"--db", dbPath, "--no-autostart")

	if got := hookCachedProject(sessionID, repo); got != "cached-start-repo" {
		t.Errorf("cached project after session-start = %q, want %q", got, "cached-start-repo")
	}
}

// TestHookUserPromptSubmit_UsesCachedProject proves a cache hit skips the
// resolution entirely: the cache names a project the workspace would NOT
// resolve to, so a prompt filed under it can only have come from the cache.
// A cache written for a different cwd is a different question and must miss.
func TestHookUserPromptSubmit_UsesCachedProject(t *testing.T) {
	withUnhurriedHookBudget(t, "user-prompt-submit") // asserts what was stored, not how fast
	dbPath, components := hookDaemonFixture(t)
	repo := pinnedProjectDir(t, "live-repo")

	hit := "hook-cache-hit-" + t.Name()
	cleanupHookState(t, hit)
	hookCacheProject(hit, repo, "cached-repo")
	_ = runHook(t, "user-prompt-submit", map[string]any{
		"session_id": hit, "cwd": repo, "prompt": "from the cache",
	}, "--db", dbPath)

	miss := "hook-cache-miss-" + t.Name()
	cleanupHookState(t, miss)
	hookCacheProject(miss, t.TempDir(), "cached-repo")
	_ = runHook(t, "user-prompt-submit", map[string]any{
		"session_id": miss, "cwd": repo, "prompt": "resolved live",
	}, "--db", dbPath)

	for _, tc := range []struct {
		session, project, prompt string
		want                     int
	}{
		{hit, "cached-repo", "from the cache", 1},
		{hit, "live-repo", "from the cache", 0},
		{miss, "live-repo", "resolved live", 1},
		{miss, "cached-repo", "resolved live", 0},
	} {
		count, err := components.store.CountPromptsForSession(tc.session, tc.project, tc.prompt)
		if err != nil {
			t.Fatalf("CountPromptsForSession: %v", err)
		}
		if count != tc.want {
			t.Errorf("prompt %q under project %q: %d, want %d", tc.prompt, tc.project, count, tc.want)
		}
	}
	// The live answer is cached for the session's next prompt.
	if got := hookCachedProject(miss, repo); got != "live-repo" {
		t.Errorf("cached project after a live resolution = %q, want %q", got, "live-repo")
	}
}

// TestHookProjectCache_RefusalsAreNotCachedAndClearStateRemovesIt — an empty
// answer is a refused workspace, and caching it would outlive whatever made
// it; and a resumed or ended session must resolve afresh.
func TestHookProjectCache_RefusalsAreNotCachedAndClearStateRemovesIt(t *testing.T) {
	isolateHookStateDir(t)
	const sessionID = "hook-cache-clear"
	cwd := t.TempDir()

	hookCacheProject(sessionID, cwd, "")
	if _, err := os.Stat(hookStateFile(sessionID, hookStateProject)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an empty (refused) project was cached: stat err = %v", err)
	}

	hookCacheProject(sessionID, cwd, "p")
	if got := hookCachedProject(sessionID, cwd); got != "p" {
		t.Fatalf("hookCachedProject = %q, want %q", got, "p")
	}
	hookClearState(sessionID)
	if got := hookCachedProject(sessionID, cwd); got != "" {
		t.Errorf("hookClearState left the cached project %q behind", got)
	}
}
