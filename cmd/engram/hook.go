package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mariesqu/engram/internal/controlapi"
)

// hook.go implements `engram hook <event>` — the lifecycle hooks an agent host
// (Claude Code, Codex) fires around a session.
//
// Why Go subcommands and not shell scripts. The upstream plugin ships one bash
// script per event plus a ~600-line helper library that hand-rolls a JSON
// parser, a UTF-8 encoder and a URL encoder in pure bash, because Git Bash on
// Windows cannot be relied on to have jq — and a second PowerShell copy of two
// of those scripts for the same reason. Every one of those lines is a
// reimplementation of something the binary already does correctly, in a
// language with no tests, on a host where forking `date` is itself a hazard.
// The binary is already installed (the hooks call it), it already speaks MCP to
// the daemon, and it already resolves projects the same way the tools do. So
// the hooks are subcommands: one implementation, one language, one test suite,
// identical on every platform, and no `commandWindows` fork in the pack.
//
// Contract every event shares:
//
//   - stdin is the host's hook JSON. A malformed or empty body is not an error;
//     it yields zero-value fields and the hook degrades to its no-op output.
//   - stdout is the host's channel. SessionStart-family events write PLAIN TEXT
//     (the host injects it as context); the others write a JSON object, `{}`
//     when there is nothing to say. Nothing else may ever reach stdout — a
//     stray log line becomes model context or breaks the host's JSON parse.
//   - stderr is for diagnostics, and is the only place errors go.
//   - the exit code is 0 for every known event, whatever happened. A hook that
//     fails closed blocks the user's prompt; engram being unreachable is never
//     a reason to stop someone from working.
//   - each event has a time budget and every call inherits it, so a wedged
//     daemon costs the budget, not the session.
const hookUsage = `Usage: engram hook <event> [--db <path>]

Run one lifecycle hook. The hook JSON is read from stdin; the result is written
to stdout in the shape the event's host expects. Errors go to stderr and the
exit code is always 0 — a hook must never block the agent.

Events:
  session-start        Register the session and print the project's recent memory context.
  post-compaction      Same, plus the mandatory post-compaction recovery steps.
  user-prompt-submit   Capture the prompt; on the first prompt of a session, inject
                       the tool bootstrap. Prints JSON. Budget: 200ms.
  subagent-stop        Save the subagent's closing report as a passive observation.
  session-end          Close the session.

Flags:
  --db             Path to the local SQLite database (or set ENGRAM_DB, or "db_path"
                   in the config file — same precedence as 'engram connect').
  --no-autostart   Never auto-start a resident daemon. Only session-start and
                   post-compaction would, and only when none is running; pass this
                   when you manage the daemon's lifecycle yourself.

Install the hooks into your agent's settings with:
  engram setup hooks --agent claude-code
  engram setup hooks --agent codex
`

// Per-event time budgets. They mirror the timeouts declared in the shipped
// hook packs (plugin/*/hooks/hooks.json) with a safety margin, so the binary
// gives up and prints its fallback output BEFORE the host kills it — a hook
// killed mid-write hands the host a truncated line, which for the JSON events
// is a parse error.
const (
	hookBudgetSessionStart = 9 * time.Second
	hookBudgetPrompt       = 200 * time.Millisecond
	hookBudgetSubagent     = 9 * time.Second
	// 2s, not 4s: the Codex pack gives SessionEnd a 3-second timeout, so a 4s
	// budget meant the host killed the hook a full second before the binary
	// intended to give up — the one case the margin exists to prevent.
	// TestHookBudgets_FitInsideEveryPackTimeout keeps the two in step.
	hookBudgetSessionEnd = 2 * time.Second
)

// hookBudgets maps a hook event (the `engram hook <event>` subcommand) to the
// budget that event runs under. It is the table the dispatcher and the
// pack-timeout test both read, so a budget can never drift away from the
// timeout the shipped hook pack declares for the same command.
var hookBudgets = map[string]time.Duration{
	"session-start":      hookBudgetSessionStart,
	"post-compaction":    hookBudgetSessionStart,
	"user-prompt-submit": hookBudgetPrompt,
	"subagent-stop":      hookBudgetSubagent,
	"session-end":        hookBudgetSessionEnd,
}

// Nudge thresholds for user-prompt-submit. A reminder that fires too early is
// noise; one that never fires is a feature nobody has.
const (
	// hookNudgeMinSessionAge — how long a session must have been running before
	// a save reminder is reasonable. Nothing worth saving has happened in the
	// first few minutes.
	hookNudgeMinSessionAge = 5 * time.Minute
	// hookNudgeMinSaveAge — how stale the project's newest memory must be.
	hookNudgeMinSaveAge = 15 * time.Minute
	// hookNudgeCooldown — how long the reminder stays quiet after firing. Without
	// it, an agent that genuinely has nothing to save never resets the clock, so
	// the reminder would fire on every message forever.
	hookNudgeCooldown = 15 * time.Minute
)

// hookContextLimit caps the memory context a session-start hook injects. The
// context is model context: uncapped, a long-lived project's recent
// observations crowd out the conversation the user came to have.
const hookContextLimit = 16 * 1024

// hookInput is the host's hook payload. Claude Code and Codex agree on
// session_id/cwd/prompt; the subagent payload differs only in which key carries
// the closing message, so both are accepted.
type hookInput struct {
	SessionID            string `json:"session_id"`
	CWD                  string `json:"cwd"`
	Prompt               string `json:"prompt"`
	LastAssistantMessage string `json:"last_assistant_message"`
	Stdout               string `json:"stdout"`
}

// message returns the subagent's closing text from whichever key carried it.
func (in hookInput) message() string {
	if s := strings.TrimSpace(in.LastAssistantMessage); s != "" {
		return s
	}
	return strings.TrimSpace(in.Stdout)
}

// runHookCmd is the entry point for `engram hook <event>`.
//
// It returns an error ONLY for a usage mistake (missing/unknown event, bad
// flag) — that can only come from a hand-edited settings file, and a silent
// no-op would hide it forever. Every runtime failure inside a known event is
// swallowed after a line on stderr, because the hook's job is to never be the
// reason a prompt does not go through.
func runHookCmd(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, hookUsage)
		return errors.New("hook: an event name is required")
	}
	event := strings.TrimSpace(args[0])
	if event == "-h" || event == "--help" || event == "help" {
		fmt.Fprint(os.Stderr, hookUsage)
		return nil
	}

	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), hookUsage) }
	db := fs.String("db", "", "path to local SQLite database (or set ENGRAM_DB)")
	noAutostart := fs.Bool("no-autostart", false, "never auto-start a resident daemon")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("hook %s takes no positional arguments; unexpected: %v", event, fs.Args())
	}

	in := readHookInput(os.Stdin)

	switch event {
	case "session-start":
		hookSessionStart(*db, in, false, !*noAutostart)
	case "post-compaction":
		hookSessionStart(*db, in, true, !*noAutostart)
	case "user-prompt-submit":
		hookUserPromptSubmit(*db, in)
	case "subagent-stop":
		hookSubagentStop(*db, in)
	case "session-end":
		hookSessionEnd(*db, in)
	default:
		fmt.Fprint(os.Stderr, hookUsage)
		return fmt.Errorf("hook: unknown event %q", event)
	}
	return nil
}

// hookStdinMaxBytes bounds the payload: a hook carries a prompt or an assistant
// message, not a file. 1 MiB is far past either and still cheap to hold.
const hookStdinMaxBytes = 1 << 20

// hookStdinDeadline bounds how long readHookInput waits for the host to finish
// writing — and, critically, CLOSING — its payload. io.ReadAll returns when it
// sees EOF, so a host that hands the hook an inherited pipe it never closes
// (a wrapper script, a shell that keeps the write end open, a terminated parent
// on Windows) hangs the hook forever: not for its budget, forever, holding up
// the user's prompt with it. Every event degrades gracefully on empty fields,
// so continuing without the payload is strictly better than not continuing.
const hookStdinDeadline = 2 * time.Second

// readHookInput decodes the host's hook JSON. Every failure mode — no stdin, an
// empty body, a truncated object, a JSON array, a stdin that never closes —
// yields the zero value rather than an error: the events all degrade gracefully
// on empty fields, and a hook that refused to run because the host sent
// something unexpected would be worse than one that quietly does nothing.
//
// The read happens on its own goroutine so the deadline can be enforced. On
// timeout that goroutine is LEAKED, deliberately: there is no portable way to
// interrupt a blocked read on an inherited handle, and this process is about to
// print one line and exit.
func readHookInput(r io.Reader) hookInput {
	if r == nil {
		return hookInput{}
	}

	type read struct {
		body []byte
		err  error
	}
	done := make(chan read, 1) // buffered: the goroutine must never block on a timed-out receiver
	go func() {
		body, err := io.ReadAll(io.LimitReader(r, hookStdinMaxBytes))
		done <- read{body, err}
	}()

	timer := time.NewTimer(hookStdinDeadline)
	defer timer.Stop()
	select {
	case res := <-done:
		if res.err != nil || len(bytes.TrimSpace(res.body)) == 0 {
			return hookInput{}
		}
		var in hookInput
		if err := json.Unmarshal(res.body, &in); err != nil {
			fmt.Fprintf(os.Stderr, "engram hook: ignoring unparseable hook input: %v\n", err)
			return hookInput{}
		}
		return in
	case <-timer.C:
		fmt.Fprintf(os.Stderr, "engram hook: stdin was still open after %s; continuing without the hook payload\n",
			hookStdinDeadline)
		return hookInput{}
	}
}

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

// dialHook resolves the DB path, optionally auto-starts a resident daemon, and
// returns a client for it.
//
// autostart is deliberately NOT universal. session-start and post-compaction
// may spawn a daemon (they are the first thing that runs in a session, and they
// have a budget that can absorb it; the spawn is detached, so even a hook that
// runs out of budget leaves a daemon behind for the next one). The others never
// do: spawning a SQLite owner to record the end of a session, or inside a
// 200ms prompt budget, trades the thing the user is doing for bookkeeping.
func dialHook(ctx context.Context, dbFlag string, autostart bool, timeout time.Duration) (*mcpBridge, error) {
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
	return newToolClient(dir, timeout)
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

// ── project resolution ──────────────────────────────────────────────────────

// hookResolveProject asks the daemon which project this directory resolves to,
// through the very tool agents are told to call first (mem_current_project), so
// a hook and the agent it is wrapping can never disagree about the name.
//
// It returns "" — meaning "do not claim a project" — whenever the answer is not
// trustworthy: a directory that does not exist, a resolution error, or a
// project every write tool would refuse anyway. A hook that guessed here would
// register sessions and file observations under a name nothing else uses, which
// is the exact failure the probe was built to expose.
//
// An EMPTY cwd is refused without asking. The host is the only thing that knows
// where the session is, so a payload without one leaves nothing to resolve —
// and the daemon's answer in that case describes the DAEMON's own working
// directory, which is a real, existing, confidently-reported project that has
// nothing to do with the session. Filing a subagent report or a prompt there is
// worse than not filing it: it is indistinguishable from real work. The
// directory_source check below catches the same answer arriving the long way
// round (a blank-after-trim cwd, or a relative one resolved against the daemon).
func hookResolveProject(ctx context.Context, client *mcpBridge, cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		fmt.Fprintf(os.Stderr, "engram hook: the hook payload carried no cwd, so there is no workspace to resolve\n")
		return ""
	}
	out, err := client.callTool(ctx, "mem_current_project", map[string]any{"directory": cwd})
	if err != nil {
		fmt.Fprintf(os.Stderr, "engram hook: resolve project: %v\n", err)
		return ""
	}
	var env struct {
		Project         string `json:"project"`
		Source          string `json:"project_source"`
		DirectorySource string `json:"directory_source"`
		ErrorHint       string `json:"error_hint"`
		WritesBlocked   bool   `json:"writes_blocked"`
		DirExists       bool   `json:"directory_exists"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		fmt.Fprintf(os.Stderr, "engram hook: mem_current_project returned non-JSON: %v\n", err)
		return ""
	}
	answersAboutTheDaemon := env.DirectorySource == dirSourceDaemonCwd || env.DirectorySource == dirSourceRelativePath
	if strings.TrimSpace(env.Project) == "" || env.ErrorHint != "" || env.WritesBlocked || !env.DirExists ||
		answersAboutTheDaemon {
		fmt.Fprintf(os.Stderr, "engram hook: no usable project for %q (source=%q, directory_source=%q, writes_blocked=%v)\n",
			cwd, env.Source, env.DirectorySource, env.WritesBlocked)
		return ""
	}
	return env.Project
}

// ── session-start / post-compaction ─────────────────────────────────────────

// hookSessionStart registers the session and prints the project's recent
// context, preceded by a short pointer to the protocol. Both are PLAIN TEXT on
// stdout, which the host injects into the model's context.
//
// What it deliberately does NOT print is the protocol itself. The MCP server
// already delivers serverInstructions through the initialize result (see
// instructions.go), so a hook that printed it again would inject several KiB of
// identical text into the model's context on every session start and every
// compaction — the one budget this feature is spending on the user's behalf.
// Two copies of a protocol do not make an agent follow it twice; they make the
// context window smaller. The pointer is here for the host whose client ignores
// server instructions, and it is three lines because three lines is what such
// an agent needs to find the rest.
//
// compaction adds the post-compaction recovery steps. They are unconditional
// and numbered because after a compaction the model has lost the reason to do
// any of them: the summary of the work it just did is the only copy left, and
// it disappears the moment the model starts answering instead of saving.
//
// The pointer is printed even when the daemon is unreachable. It is static, it
// is the half of the session bootstrap that does not need a daemon, and an
// agent that knows the tools exist will call them once the daemon is back —
// whereas an agent that was told nothing will not.
func hookSessionStart(dbFlag string, in hookInput, compaction, autostart bool) {
	ctx, cancel := context.WithTimeout(context.Background(), hookBudgetSessionStart)
	defer cancel()

	// A session id is REUSED across a `claude --resume` (and across a compaction,
	// which fires this same hook): the bootstrap must fire again for the new
	// context, and the nudge clock must start from now rather than from whenever
	// the original session first spoke. Clearing both markers is what makes that
	// true — see hookClearState.
	hookClearState(in.SessionID)

	project, memoryContext := "", ""
	client, err := dialHook(ctx, dbFlag, autostart, hookBudgetSessionStart)
	if err != nil {
		fmt.Fprintf(os.Stderr, "engram hook session-start: %v\n", err)
	} else {
		project = hookResolveProject(ctx, client, in.CWD)
		if project != "" && strings.TrimSpace(in.SessionID) != "" {
			if _, err := client.callTool(ctx, "mem_session_start", map[string]any{
				"id":        in.SessionID,
				"project":   project,
				"directory": in.CWD,
			}); err != nil {
				fmt.Fprintf(os.Stderr, "engram hook session-start: %v\n", err)
			}
		}
		if project != "" {
			memoryContext = hookMemoryContext(ctx, client, project)
		}
	}

	var out strings.Builder
	out.WriteString(hookProtocolPointer(project))
	out.WriteString("\n")
	if compaction {
		out.WriteString("\n")
		out.WriteString(postCompactionSteps(project))
	}
	if memoryContext != "" {
		out.WriteString("\n")
		out.WriteString(memoryContext)
		out.WriteString("\n")
	}
	fmt.Print(out.String())
}

// hookProtocolPointer is the short block a session-start hook prints in place
// of the full protocol: what is active, where the rest of it comes from, and
// the two calls that open a session. Naming the project matters when one was
// resolved — it is the answer to the first question the pointer tells the model
// to ask, and after a compaction it is the only place that answer survives.
func hookProtocolPointer(project string) string {
	var b strings.Builder
	b.WriteString("ENGRAM MEMORY IS ACTIVE. The full protocol (save format, lifecycle, search flow, " +
		"after-compaction steps) is delivered by the Engram MCP server's own instructions — follow it " +
		"without being asked.\n")
	b.WriteString("Open with mcp__engram__mem_current_project to confirm which project this session is filed " +
		"under, then mcp__engram__mem_context for prior history.\n")
	b.WriteString("Save proactively with mcp__engram__mem_save after any decision, bug fix, convention or " +
		"non-obvious discovery — and close with mcp__engram__mem_session_summary.\n")
	if project != "" {
		fmt.Fprintf(&b, "This session is registered under project %q.\n", project)
	}
	return b.String()
}

// hookMemoryContext fetches the project's context blob and bounds it. The empty
// string is returned both for a failure and for a project with no history —
// there is nothing useful to inject in either case, and the daemon's "No
// previous session memories found." sentence is an answer to a question the
// model never asked.
func hookMemoryContext(ctx context.Context, client *mcpBridge, project string) string {
	out, err := client.callTool(ctx, "mem_context", map[string]any{"project": project})
	if err != nil {
		fmt.Fprintf(os.Stderr, "engram hook: %v\n", err)
		return ""
	}
	out = strings.TrimSpace(out)
	if out == "" || strings.HasPrefix(out, "No previous session memories found.") {
		return ""
	}
	return truncateForHook(out, hookContextLimit)
}

// truncateForHook cuts s to at most limit bytes on a rune boundary, leaving a
// marker. A silent cut mid-sentence reads like a memory that ends mid-thought.
func truncateForHook(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const marker = "\n\n[context truncated — call mem_search or mem_context for the rest]"
	cut := limit - len(marker)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}

// isRuneStart reports whether b can begin a UTF-8 encoded rune.
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// postCompactionSteps returns the recovery block for a compacted session,
// naming the project so the model does not have to re-derive it from a context
// it has just lost.
func postCompactionSteps(project string) string {
	target := project
	if target == "" {
		// No project resolved: telling the model to pass a name nobody has would
		// invite it to invent one. Send it to the probe instead.
		target = "the project reported by mem_current_project"
	} else {
		target = fmt.Sprintf("%q", project)
	}
	return fmt.Sprintf(`CRITICAL INSTRUCTION POST-COMPACTION — follow these steps IN ORDER:

1. FIRST: call mem_session_summary with the content of the compacted summary above, for project %[1]s.
   That summary is the only record of what happened before the compaction; it disappears the moment you start answering instead of saving.

2. THEN: call mem_context for project %[1]s to recover recent sessions and observations.
   Read what comes back before doing anything else — it tells you what was being worked on.

3. If a detail is still missing, call mem_search with keywords from the user's request.

4. Only THEN continue the work the user asked for.

All 4 steps are MANDATORY. Skipping them means continuing blind on a task you can no longer see.
`, target)
}

// ── user-prompt-submit ──────────────────────────────────────────────────────

// hookUserPromptSubmit runs on every user message, inside a 200ms budget: it is
// in the critical path of someone pressing Enter, so it is bounded end-to-end
// and prints its fallback (`{}`) rather than waiting for anything.
//
// It does three things, in decreasing order of importance:
//
//  1. Captures the prompt (mem_save_prompt), so a later mem_save can attach the
//     request that produced it.
//  2. On the FIRST prompt of a session, injects the bootstrap: call
//     mem_current_project first, and here are the tool names. Hosts that defer
//     MCP tool loading will not surface engram's tools until something asks for
//     them by name.
//  3. Afterwards, at most one save reminder per cooldown, and only for a
//     session old enough and a project whose newest memory is stale enough to
//     deserve it.
func hookUserPromptSubmit(dbFlag string, in hookInput) {
	start := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(hookBudgetPrompt))
	defer cancel()

	// Claimed BEFORE any network work, and atomically: two prompts submitted in
	// quick succession must not both count as the first one, and the file's
	// modification time is what later calls use as the session's age.
	stateFile := hookStateFile(in.SessionID, hookStateToolsLoaded)
	firstPrompt := hookClaimState(stateFile)

	// No autostart: spawning a SQLite owner is seconds of work inside a 200ms
	// budget. A session-start hook (or the first tools/call from the agent)
	// brings the daemon up.
	client, err := dialHook(ctx, dbFlag, false, hookBudgetPrompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "engram hook user-prompt-submit: %v\n", err)
		hookPrintPromptOutput(firstPrompt, "")
		return
	}

	// Resolved at most ONCE per hook run, lazily: the capture below and the nudge
	// need the same answer, and a mem_current_project round trip inside a 200ms
	// budget is not something to pay for twice — or at all on a prompt that has
	// nothing to save and nothing to remind about.
	project := onceProject(ctx, client, in.CWD)

	if prompt := strings.TrimSpace(in.Prompt); prompt != "" && strings.TrimSpace(in.SessionID) != "" {
		// project, not directory: the daemon is a separate process and a payload
		// without a cwd would otherwise file the prompt under the DAEMON's own
		// directory. An unresolvable workspace means the prompt is dropped, with a
		// line on stderr — a prompt filed under the wrong project is worse than a
		// prompt nobody kept.
		if p := project(); p == "" {
			fmt.Fprintf(os.Stderr, "engram hook user-prompt-submit: no usable project for %q; the prompt was not captured\n", in.CWD)
		} else if _, err := client.callTool(ctx, "mem_save_prompt", map[string]any{
			// Capped at the same 16 KiB as a subagent report. A prompt can carry a
			// pasted file, and a memory nobody can read is not worth the write it
			// costs — nor the bytes it takes back out of the model's context when
			// mem_save attaches it.
			"content":    truncateForHook(in.Prompt, hookContextLimit),
			"session_id": in.SessionID,
			"project":    p,
			"directory":  in.CWD,
		}); err != nil {
			// Fire-and-forget by design: a prompt that was not captured costs a
			// later mem_save its attachment, nothing more.
			fmt.Fprintf(os.Stderr, "engram hook user-prompt-submit: %v\n", err)
		}
	}

	if firstPrompt {
		hookPrintPromptOutput(true, "")
		return
	}
	hookPrintPromptOutput(false, hookSaveNudge(ctx, client, in, stateFile, project))
}

// onceProject memoizes hookResolveProject for one hook run. The zero answer is
// memoized too: a workspace that could not be resolved once will not resolve on
// a second call, and retrying it inside a 200ms budget spends the budget twice
// to learn the same thing.
func onceProject(ctx context.Context, client *mcpBridge, cwd string) func() string {
	var (
		project string
		done    bool
	)
	return func() string {
		if !done {
			project = hookResolveProject(ctx, client, cwd)
			done = true
		}
		return project
	}
}

// hookPrintPromptOutput writes the single JSON object a UserPromptSubmit hook is
// allowed to produce. additionalContext is the ONLY field that reaches the
// model — a systemMessage is rendered in the terminal and never seen by it, so
// a reminder sent that way is a reminder nobody reads.
func hookPrintPromptOutput(firstPrompt bool, nudge string) {
	text := nudge
	if firstPrompt {
		text = hookBootstrapContext()
	}
	if strings.TrimSpace(text) == "" {
		fmt.Println("{}")
		return
	}
	out, err := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": text,
		},
	})
	if err != nil {
		fmt.Println("{}")
		return
	}
	fmt.Println(string(out))
}

// hookMCPToolNames are engram's MCP tool names as the host exposes them
// (mcp__<server>__<tool> for a server registered as "engram"). The bootstrap
// lists them because a host that defers tool loading will not surface a tool
// until something names it.
//
// TestHookToolNames_MatchRegisteredTools pins this list against the tools the
// daemon actually registers — a name here that the server does not serve is a
// tool the agent will try and fail to call.
var hookMCPToolNames = []string{
	"mem_current_project",
	"mem_session_start",
	"mem_session_end",
	"mem_session_summary",
	"mem_save",
	"mem_save_prompt",
	"mem_search",
	"mem_similar",
	"mem_context",
	"mem_get_observation",
	"mem_update",
	"mem_suggest_topic_key",
	"mem_review",
	"mem_pin",
	"mem_unpin",
	"mem_judge",
	"mem_merge_projects",
	"mem_doctor",
}

// hookBootstrapContext is the first-prompt injection: what to call first, and
// what exists to be called.
func hookBootstrapContext() string {
	prefixed := make([]string, 0, len(hookMCPToolNames))
	for _, name := range hookMCPToolNames {
		prefixed = append(prefixed, "mcp__engram__"+name)
	}
	return "ENGRAM MEMORY IS ACTIVE. Before answering, call mcp__engram__mem_current_project to confirm " +
		"which project this session is filed under (it never errors; read fallback, writes_blocked and " +
		"directory_exists), then mcp__engram__mem_context for prior session history.\n\n" +
		"Available tools: " + strings.Join(prefixed, ", ")
}

// hookSaveNudge returns the reminder text for this prompt, or "" for silence.
//
// Three gates, all of which must open:
//
//   - the session has been running for at least hookNudgeMinSessionAge (the
//     age is the modification time of the first-prompt state file — the local
//     evidence of when this session started talking);
//   - the project's newest memory is at least hookNudgeMinSaveAge old, or there
//     is no memory at all and the session itself is that old;
//   - the reminder has not fired within hookNudgeCooldown.
//
// Any failure — unresolvable project, unreachable daemon, unreadable state —
// returns silence. A nudge is the least important thing this hook does.
func hookSaveNudge(ctx context.Context, client *mcpBridge, in hookInput, stateFile string, resolveProject func() string) string {
	sessionAge, ok := hookStateAge(stateFile)
	if !ok || sessionAge < hookNudgeMinSessionAge {
		return ""
	}
	project := resolveProject()
	if project == "" {
		return ""
	}

	saveAge, found, err := hookLastSaveAge(ctx, client.dir, project)
	if err != nil {
		fmt.Fprintf(os.Stderr, "engram hook user-prompt-submit: last save: %v\n", err)
		return ""
	}
	if found && saveAge < hookNudgeMinSaveAge {
		return ""
	}
	if !found && sessionAge < hookNudgeMinSaveAge {
		// No memory yet for this project: hold the first reminder to the same
		// staleness bar, measured against the session instead.
		return ""
	}

	nudgeFile := hookStateFile(in.SessionID, hookStateLastNudge)
	if age, ok := hookStateAge(nudgeFile); ok && age < hookNudgeCooldown {
		return ""
	}
	hookTouchState(nudgeFile)

	return fmt.Sprintf("MEMORY REMINDER: nothing has been saved to project %q in over %d minutes. "+
		"If decisions were made, a bug was fixed, or something non-obvious was learned since then, call "+
		"mcp__engram__mem_save now — then answer the user.",
		project, int(hookNudgeMinSaveAge.Minutes()))
}

// hookLastSaveAge returns how long ago the newest memory of project was
// written, and whether there is one at all. It reads the daemon's control API
// (GET /api/v1/memories, newest first) rather than an MCP tool because no tool
// returns a machine-readable timestamp — mem_search and mem_review print prose
// for a model, and parsing prose to decide whether to nag someone is not a
// contract worth having.
func hookLastSaveAge(ctx context.Context, dir, project string) (time.Duration, bool, error) {
	client, err := NewControlClient(dir)
	if err != nil {
		return 0, false, err
	}
	// Bound the control call by whatever is left of the hook's budget; the
	// client's own 5s default would outlive the budget several times over. The
	// CONTEXT is what actually enforces it — the client's Timeout is per request,
	// so the 401-refresh-and-retry inside GetContext would otherwise get a second
	// full helping of a budget that is already spent.
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, false, context.DeadlineExceeded
		}
		client.http.Timeout = remaining
	}

	var memories []struct {
		CreatedAt string `json:"created_at"`
	}
	path := "/api/v1/memories?limit=1&project=" + urlQueryEscape(project)
	if err := client.GetContext(ctx, path, &memories); err != nil {
		return 0, false, err
	}
	if len(memories) == 0 {
		return 0, false, nil
	}
	created, err := time.Parse(time.RFC3339, memories[0].CreatedAt)
	if err != nil {
		return 0, false, fmt.Errorf("parse created_at %q: %w", memories[0].CreatedAt, err)
	}
	return time.Since(created), true, nil
}

// ── subagent-stop ───────────────────────────────────────────────────────────

// hookSubagentStop files the subagent's closing report as a passive
// observation. A subagent's context dies with it — this is the only chance to
// keep what it found.
//
// The observation is deliberately marked in its title: it is a REPORT, written
// by a process nobody reviewed, not a decision anyone made. capture_prompt is
// off for the same reason — the user's prompt belongs to the parent session's
// work, not to a subagent's transcript.
func hookSubagentStop(dbFlag string, in hookInput) {
	defer fmt.Println("{}")

	message := in.message()
	if message == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), hookBudgetSubagent)
	defer cancel()

	client, err := dialHook(ctx, dbFlag, false, hookBudgetSubagent)
	if err != nil {
		fmt.Fprintf(os.Stderr, "engram hook subagent-stop: %v\n", err)
		return
	}

	// Resolve BEFORE saving and name the project explicitly. A payload without a
	// cwd (Codex sends none for some subagent shapes) would otherwise resolve to
	// the resident daemon's own working directory — and a subagent report filed
	// under a junk project reads exactly like a real memory, in a project the
	// user never opens.
	project := hookResolveProject(ctx, client, in.CWD)
	if project == "" {
		fmt.Fprintf(os.Stderr, "engram hook subagent-stop: no usable project for %q; the report was not saved\n", in.CWD)
		return
	}

	args := map[string]any{
		"title":          hookSubagentTitle(message),
		"content":        truncateForHook(message, hookContextLimit),
		"type":           "discovery",
		"capture_prompt": false,
		"project":        project,
		"directory":      in.CWD,
	}
	if id := strings.TrimSpace(in.SessionID); id != "" {
		args["session_id"] = id
	}
	if _, err := client.callTool(ctx, "mem_save", args); err != nil {
		fmt.Fprintf(os.Stderr, "engram hook subagent-stop: %v\n", err)
	}
}

// hookSubagentTitle builds a searchable title from the report's first
// meaningful line, tagged with its source so a reader can tell at a glance that
// no human saw it.
func hookSubagentTitle(message string) string {
	const maxTitle = 80
	line := ""
	for _, candidate := range strings.Split(message, "\n") {
		candidate = strings.TrimSpace(strings.TrimLeft(candidate, "#*-> \t"))
		if candidate != "" {
			line = candidate
			break
		}
	}
	if line == "" {
		line = "report"
	}
	if runes := []rune(line); len(runes) > maxTitle {
		line = strings.TrimSpace(string(runes[:maxTitle])) + "…"
	}
	return "subagent-stop: " + line
}

// ── session-end ─────────────────────────────────────────────────────────────

// hookSessionEnd closes the session row. No summary is invented here: the
// agent writes that with mem_session_summary, and a hook that filled the field
// with "session ended" would overwrite the one place the next session looks.
func hookSessionEnd(dbFlag string, in hookInput) {
	defer fmt.Println("{}")

	id := strings.TrimSpace(in.SessionID)
	if id == "" {
		return
	}
	// The session is over: its markers are scratch for a session that no longer
	// exists. Removing them here is what keeps the state directory from
	// accumulating one pair of files per session forever.
	hookClearState(id)

	ctx, cancel := context.WithTimeout(context.Background(), hookBudgetSessionEnd)
	defer cancel()

	client, err := dialHook(ctx, dbFlag, false, hookBudgetSessionEnd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "engram hook session-end: %v\n", err)
		return
	}
	if _, err := client.callTool(ctx, "mem_session_end", map[string]any{"id": id}); err != nil {
		fmt.Fprintf(os.Stderr, "engram hook session-end: %v\n", err)
	}
}

// ── state files ─────────────────────────────────────────────────────────────

// The two per-session markers, named once so a typo cannot silently create a
// third kind that nothing ever reads.
const (
	// hookStateToolsLoaded marks that the first prompt of a session has been
	// seen; its modification time is also how the nudge measures session age.
	hookStateToolsLoaded = "tools-loaded"
	// hookStateLastNudge marks when the save reminder last fired.
	hookStateLastNudge = "last-nudge"
)

// hookStateDir returns the directory the per-session markers live in:
// <UserCacheDir>/engram/hooks, created 0700.
//
// NOT os.TempDir. These files are scratch, but they are scratch that decides
// behaviour — whether the bootstrap has fired, when the session started
// talking, when the nudge last spoke — and the system temp directory is
// world-readable, world-WRITABLE on Unix, and swept by tools that have no idea
// what they are deleting. A marker another user can create is a marker another
// user can use to silence someone else's reminders, and the whole tree was a
// flat list of engram-hook-* files in a directory shared with every other
// program on the machine. The user cache directory is per-user, 0700 here, and
// the documented home for exactly this: regenerable state that must survive a
// crashed agent (so not memory) without living in the user's config.
//
// A failure to resolve or create it falls back to os.TempDir — the old home.
// Degrading a debounce is acceptable; refusing to run a hook is not.
func hookStateDir() string {
	base, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(base) == "" {
		return os.TempDir()
	}
	dir := filepath.Join(base, "engram", "hooks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "engram hook: cannot use %s (%v); falling back to the temp directory\n", dir, err)
		return os.TempDir()
	}
	return dir
}

// hookStateFile returns the path of a per-session marker file. The session id
// is HASHED rather than embedded: it is host-supplied text that ends up in a
// filesystem path, and a hash is both traversal-proof and fixed-length on hosts
// with short path limits. The tradeoff — you cannot eyeball which session a
// file belongs to — costs nothing, since nothing reads these but this binary.
func hookStateFile(sessionID, kind string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(hookStateDir(), "engram-hook-"+hex.EncodeToString(sum[:8])+"-"+kind)
}

// hookClearState removes both markers of a session.
//
// It runs at session-start/post-compaction and at session-end, for two
// different reasons that happen to want the same thing. A `--resume` (and a
// compaction, which fires the same hook) REUSES the session id: the model's
// context is new, so the bootstrap has to fire again, and the age clock the
// nudge reads has to start from now rather than from whenever this id first
// spoke — days ago, on a machine that has since been rebooted. At session-end
// it is plain hygiene: without it the directory grows one pair of files per
// session, forever.
func hookClearState(sessionID string) {
	if strings.TrimSpace(sessionID) == "" {
		return
	}
	for _, kind := range []string{hookStateToolsLoaded, hookStateLastNudge} {
		if err := os.Remove(hookStateFile(sessionID, kind)); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "engram hook: could not clear session state: %v\n", err)
		}
	}
}

// hookClaimState creates the marker and reports whether THIS call created it —
// the atomic form of "is this the first prompt of the session?".
//
// O_EXCL is what makes it atomic. The old exists-then-create pair let two
// prompts submitted in quick succession both read "absent" and both claim to be
// the first, which injects the bootstrap twice and resets the session's age
// clock underneath the nudge.
//
// A creation failure that is NOT "already exists" (an unwritable state
// directory) reports true: with no marker to read, the choice is between
// injecting the bootstrap on every prompt and never injecting it at all, and
// the first at least leaves a working session.
func hookClaimState(path string) bool {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		_ = f.Close()
		return true
	}
	if errors.Is(err, os.ErrExist) {
		return false
	}
	fmt.Fprintf(os.Stderr, "engram hook: could not record session state at %s: %v\n", path, err)
	return true
}

// hookStateAge returns how long ago the marker was last written. A missing or
// unreadable marker reports ok=false, which every caller treats as "do not act".
func hookStateAge(path string) (time.Duration, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return time.Since(info.ModTime()), true
}

// hookTouchState creates or refreshes the marker. Failures are ignored: an
// unwritable state directory degrades the nudge debounce, it does not break the
// hook.
//
// The create is O_EXCL and an "already exists" is retried as a Chtimes, so two
// hooks racing on the same marker cannot have one TRUNCATE the file the other
// is using as a clock.
func hookTouchState(path string) {
	now := time.Now()
	if err := os.Chtimes(path, now, now); err == nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		_ = f.Close()
		return
	}
	if errors.Is(err, os.ErrExist) {
		_ = os.Chtimes(path, now, now)
	}
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
