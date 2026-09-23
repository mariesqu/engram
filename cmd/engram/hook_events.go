package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

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
func hookSessionStart(ctx context.Context, dbFlag string, in hookInput, compaction, autostart bool) {
	// A session id is REUSED across a `claude --resume` (and across a compaction,
	// which fires this same hook): the bootstrap must fire again for the new
	// context, and the nudge clock must start from now rather than from whenever
	// the original session first spoke. Clearing both markers is what makes that
	// true — see hookClearState.
	hookClearState(in.SessionID)

	project, memoryContext := "", ""
	client, err := dialHook(ctx, dbFlag, autostart)
	if err != nil {
		fmt.Fprintf(os.Stderr, "engram hook session-start: %v\n", err)
	} else {
		// Paid for here, inside an 8s budget, so the 200ms prompt hook does not
		// have to pay for it again on every message. The fingerprint is taken
		// BEFORE resolving; see hookCacheProject.
		fingerprint := hookProjectFingerprint(in.CWD)
		project = hookResolveProject(ctx, client, in.CWD)
		hookCacheProject(in.SessionID, in.CWD, project, fingerprint)
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
	b.WriteString("Open with mem_current_project (provided by the engram MCP server) to confirm which project " +
		"this session is filed under, then mem_context for prior history.\n")
	b.WriteString("Save proactively with mem_save after any decision, bug fix, convention or " +
		"non-obvious discovery — and close with mem_session_summary.\n")
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
func hookUserPromptSubmit(ctx context.Context, dbFlag string, in hookInput) {
	// Claimed BEFORE any network work, and atomically: two prompts submitted in
	// quick succession must not both count as the first one, and the file's
	// modification time is what later calls use as the session's age.
	//
	// A payload with NO session id claims nothing. The marker path is the hash of
	// the id, so an empty one hashes to a single fixed name shared by every such
	// hook on the machine — and nothing ever clears it, because hookClearState
	// (session-start, session-end) refuses an empty id too. The first payload
	// that arrived without a session id therefore claimed a permanent marker,
	// after which every later one read "not the first prompt" and lost its
	// bootstrap, and the nudge started measuring a "session age" from whenever
	// that stray hook ran. A session we cannot name has no state to keep: treat
	// each one as a first prompt (the bootstrap is static text that needs no
	// project) and skip the nudge machinery entirely, since every gate it opens
	// is read from the file this branch does not write.
	stateFile, firstPrompt := "", true
	if strings.TrimSpace(in.SessionID) != "" {
		stateFile = hookStateFile(in.SessionID, hookStateToolsLoaded)
		firstPrompt = hookClaimState(stateFile)
	}

	// No autostart: spawning a SQLite owner is seconds of work inside a 200ms
	// budget. A session-start hook (or the first tools/call from the agent)
	// brings the daemon up.
	client, err := dialHook(ctx, dbFlag, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "engram hook user-prompt-submit: %v\n", err)
		hookPrintPromptOutput(firstPrompt, "")
		return
	}

	// Resolved at most ONCE per hook run, lazily: the capture below and the nudge
	// need the same answer, and a mem_current_project round trip inside a 200ms
	// budget is not something to pay for twice — or at all on a prompt that has
	// nothing to save and nothing to remind about.
	project := onceProject(ctx, client, in)

	if prompt := strings.TrimSpace(in.Prompt); prompt != "" && strings.TrimSpace(in.SessionID) != "" {
		// project, not directory: the daemon is a separate process and a payload
		// without a cwd would otherwise file the prompt under the DAEMON's own
		// directory. An unresolvable workspace means the prompt is dropped, with a
		// line on stderr — a prompt filed under the wrong project is worse than a
		// prompt nobody kept.
		if p := project(); p == "" {
			fmt.Fprintf(os.Stderr, "engram hook user-prompt-submit: no usable project for %q; the prompt was not captured\n", in.CWD)
		} else if !hookClaimOccurrence(in.SessionID, "user-prompt-submit", prompt) {
			// The plugin and `engram setup hooks` both installed: this exact prompt
			// was already captured by the other invocation. See FUP-003.
			fmt.Fprintf(os.Stderr, "engram hook user-prompt-submit: this prompt was already captured for "+
				"session %q (duplicate hook install?); skipping\n", in.SessionID)
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

// onceProject memoizes hookProject for one hook run. The zero answer is
// memoized too: a workspace that could not be resolved once will not resolve on
// a second call, and retrying it inside a 200ms budget spends the budget twice
// to learn the same thing.
//
// Across runs it goes through the per-session cache (hookCachedProject): a hit
// costs one small file read plus a few stats that revalidate it, instead of a
// daemon round trip that spawns git twice, and a live answer is cached for the
// next prompt of the same session.
// A session whose session-start hook ran therefore never resolves here at all.
func onceProject(ctx context.Context, client *mcpBridge, in hookInput) func() string {
	var (
		project string
		done    bool
	)
	return func() string {
		if !done {
			var fingerprint string
			if project, fingerprint = hookCachedProject(in.SessionID, in.CWD); project == "" {
				if fingerprint == "" {
					fingerprint = hookProjectFingerprint(in.CWD)
				}
				project = hookProject(ctx, client, in)
				hookCacheProject(in.SessionID, in.CWD, project, fingerprint)
			}
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

// hookToolPrefixGlobal and hookToolPrefixPlugin are the two fully-qualified
// forms a host may register engram's tools under: mcp__<server>__<tool> for a
// direct/global MCP registration (server name "engram"), and
// mcp__plugin_<plugin>_<server>__<tool> for the Claude Code plugin install
// (plugin/claude-code/.claude-plugin/plugin.json names the plugin "engram",
// plugin/claude-code/.mcp.json names the server "engram" too, giving
// "plugin_engram_engram"). Nothing at hook-execution time says which one a
// given host used, so hookBootstrapContext lists both — see its own doc.
const (
	hookToolPrefixGlobal = "mcp__engram__"
	hookToolPrefixPlugin = "mcp__plugin_engram_engram__"
)

// hookMCPToolNames are engram's MCP tool names, UNPREFIXED — the form every
// narrative sentence in this file uses (see hookProtocolPointer,
// hookSaveNudge): a bare name is valid regardless of which prefix the host
// actually exposes, where a hardcoded prefix would name a tool that does not
// exist under the other install.
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
//
// The "call X first" sentence uses BARE names (valid under any install), but
// the "Available tools" list is the one place in this file that lists the
// FULLY-QUALIFIED forms: a host that defers tool loading surfaces a tool only
// once something names it as the host itself registered it, and nothing at
// hook-execution time says whether that host used the global registration or
// the plugin install. Listing both prefixes costs one longer line and
// guarantees the real name is in there either way; a model that tries the
// other form and gets "tool not found" still has its own tool list to fall
// back to.
func hookBootstrapContext() string {
	prefixed := make([]string, 0, len(hookMCPToolNames)*2)
	for _, name := range hookMCPToolNames {
		prefixed = append(prefixed, hookToolPrefixGlobal+name, hookToolPrefixPlugin+name)
	}
	return "ENGRAM MEMORY IS ACTIVE. Before answering, call mem_current_project (provided by the engram MCP " +
		"server) to confirm which project this session is filed under (it never errors; read fallback, " +
		"writes_blocked and directory_exists), then mem_context for prior session history.\n\n" +
		"Available tools (exact name depends on how your host registered the \"engram\" MCP server): " + strings.Join(prefixed, ", ")
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
		"mem_save now — then answer the user.",
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
func hookSubagentStop(ctx context.Context, dbFlag string, in hookInput) {
	defer fmt.Println("{}")

	message := in.message()
	if message == "" {
		return
	}

	client, err := dialHook(ctx, dbFlag, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "engram hook subagent-stop: %v\n", err)
		return
	}

	// Resolve BEFORE saving and name the project explicitly. A payload without a
	// cwd (Codex sends none for some subagent shapes) would otherwise resolve to
	// the resident daemon's own working directory — and a subagent report filed
	// under a junk project reads exactly like a real memory, in a project the
	// user never opens. When there is no cwd to resolve, the session's own
	// registration answers instead (hookProject); when nothing answers, the
	// report is dropped.
	project := hookProject(ctx, client, in)
	if project == "" {
		fmt.Fprintf(os.Stderr, "engram hook subagent-stop: no usable project for %q; the report was not saved\n", in.CWD)
		return
	}

	if !hookClaimOccurrence(in.SessionID, "subagent-stop", message) {
		// The plugin and `engram setup hooks` both installed: this exact report
		// was already saved by the other invocation. See FUP-003.
		fmt.Fprintf(os.Stderr, "engram hook subagent-stop: this report was already saved for session %q "+
			"(duplicate hook install?); skipping\n", in.SessionID)
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
func hookSessionEnd(ctx context.Context, dbFlag string, in hookInput) {
	defer fmt.Println("{}")

	id := strings.TrimSpace(in.SessionID)
	if id == "" {
		return
	}
	// The session is over: its markers are scratch for a session that no longer
	// exists. Removing them here is what keeps the state directory from
	// accumulating one pair of files per session forever.
	hookClearState(id)

	client, err := dialHook(ctx, dbFlag, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "engram hook session-end: %v\n", err)
		return
	}
	if _, err := client.callTool(ctx, "mem_session_end", map[string]any{"id": id}); err != nil {
		fmt.Fprintf(os.Stderr, "engram hook session-end: %v\n", err)
	}
}
