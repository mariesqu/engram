package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
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
//
// The budget covers the WHOLE process, stdin included: the clock starts in
// runHookCmd before anything is read (see hookStdinDeadline). The invariant
// TestHookBudgets_FitInsideEveryPackTimeout pins is one notch stricter still —
// hookStdinDeadline + budget < the pack's timeout — so the margin survives even
// if a future change moves the stdin read back outside the event's clock.
const (
	// 8s, not 9s: both packs declare a 10-second timeout for the SessionStart
	// family, and 9s left no room for the stdin deadline in front of it.
	hookBudgetSessionStart = 8 * time.Second
	hookBudgetPrompt       = 200 * time.Millisecond
	hookBudgetSubagent     = 8 * time.Second
	// 1.5s, not 4s: the Codex pack gives SessionEnd a 3-second timeout, so a 4s
	// budget meant the host killed the hook a full second before the binary
	// intended to give up — the one case the margin exists to prevent. 2s was the
	// first correction and still did not fit the stdin deadline in front of it.
	// TestHookBudgets_FitInsideEveryPackTimeout keeps the three in step.
	hookBudgetSessionEnd = 1500 * time.Millisecond
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
//
// The event's clock starts HERE, on the first line, and the same start feeds
// both the stdin read and the event's context. It used to start after stdin:
// the read had its own 2-second deadline and the event then took its full
// budget on top, so the wall time a host actually saw was stdin PLUS budget —
// up to 4 seconds for a session-end the Codex pack kills at 3. One clock, one
// number, and the number is the budget.
func runHookCmd(args []string) error {
	start := time.Now()
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

	budget, known := hookBudgets[event]
	if !known {
		// Checked BEFORE stdin is touched: an event nobody serves has no budget to
		// read a payload under, and there is nothing to do with the payload anyway.
		fmt.Fprint(os.Stderr, hookUsage)
		return fmt.Errorf("hook: unknown event %q", event)
	}

	in := readHookInput(os.Stdin, hookStdinBound(budget-time.Since(start)))

	ctx, cancel := context.WithDeadline(context.Background(), start.Add(budget))
	defer cancel()

	switch event {
	case "session-start":
		hookSessionStart(ctx, *db, in, false, !*noAutostart)
	case "post-compaction":
		hookSessionStart(ctx, *db, in, true, !*noAutostart)
	case "user-prompt-submit":
		hookUserPromptSubmit(ctx, *db, in)
	case "subagent-stop":
		hookSubagentStop(ctx, *db, in)
	case "session-end":
		hookSessionEnd(ctx, *db, in)
	}
	return nil
}

// hookStdinMaxBytes bounds the payload: a hook carries a prompt or an assistant
// message, not a file. 1 MiB is far past either and still cheap to hold.
const hookStdinMaxBytes = 1 << 20

// hookStdinDeadline is the CEILING on how long readHookInput waits for the host
// to finish writing — and, critically, CLOSING — its payload. io.ReadAll returns
// when it sees EOF, so a host that hands the hook an inherited pipe it never
// closes (a wrapper script, a shell that keeps the write end open, a terminated
// parent on Windows) hangs the hook forever: not for its budget, forever,
// holding up the user's prompt with it. Every event degrades gracefully on
// empty fields, so continuing without the payload is strictly better than not
// continuing.
//
// 1s, not 2s: the wait is for a payload the host has already written (the read
// returns the instant it sees EOF, so a well-behaved host never spends any of
// this), and it has to fit INSIDE the tightest pack timeout together with the
// event's budget — Codex gives UserPromptSubmit 2 seconds in total.
const hookStdinDeadline = time.Second

// hookStdinBound is how long the stdin read may take for an event with
// remaining left of its budget: the ceiling, or what is left if that is less.
//
// The prompt hook is the case that makes this a min() rather than a constant —
// its whole budget is 200ms, so a 1-second stdin wait would blow it five times
// over before the hook did anything. A non-positive remaining (a machine so
// loaded that flag parsing outlived the budget) yields zero, which readHookInput
// treats as "do not wait at all" rather than "wait forever".
func hookStdinBound(remaining time.Duration) time.Duration {
	if remaining < hookStdinDeadline {
		if remaining < 0 {
			return 0
		}
		return remaining
	}
	return hookStdinDeadline
}

// readHookInput decodes the host's hook JSON under the deadline its caller
// computed. Every failure mode — no stdin, an empty body, a truncated object, a
// JSON array, a stdin that never closes — yields the zero value rather than an
// error: the events all degrade gracefully on empty fields, and a hook that
// refused to run because the host sent something unexpected would be worse than
// one that quietly does nothing.
//
// The read happens on its own goroutine so the deadline can be enforced. On
// timeout that goroutine is LEAKED, deliberately: there is no portable way to
// interrupt a blocked read on an inherited handle, and this process is about to
// print one line and exit.
func readHookInput(r io.Reader, deadline time.Duration) hookInput {
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

	timer := time.NewTimer(deadline)
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
			deadline)
		return hookInput{}
	}
}
