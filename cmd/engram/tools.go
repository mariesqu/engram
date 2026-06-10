package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mariesqu/engram/internal/localstore"
	projectpkg "github.com/mariesqu/engram/internal/project"
	"github.com/mariesqu/engram/internal/syncer"
)

// resolveReadProject resolves the project for a READ tool call. Unlike write
// tools, read tools are LENIENT: no hard errors on ambiguous or invalid config.
// The policy mirrors old_code handleSearch/handleContext (REQ-310 lenient path):
//
//  1. Explicit project argument wins (normalized, used as-is — no store lookup).
//  2. Detect from cwd via DetectProjectFull.
//  3. On ANY detection error (ambiguous, invalid config, no .git, etc.) fall
//     back to the dir basename via DetectProject. Read tools never return a
//     project-resolution error to the agent.
//
// This contrasts with write tools (resolveSaveProject) which hard-error on
// ErrInvalidConfig and ErrAmbiguousProject.
func resolveReadProject(explicitProject string) string {
	if strings.TrimSpace(explicitProject) != "" {
		return strings.TrimSpace(explicitProject)
	}
	cwd, _ := os.Getwd()
	det := projectpkg.DetectProjectFull(cwd)
	if det.Error != nil {
		// Lenient: fall back to basename, never error.
		return projectpkg.DetectProject(cwd)
	}
	return det.Project
}

// registerTools adds the MCP tools exposed by this binary to srv. It is called
// unconditionally from buildDaemon regardless of whether the daemon is running
// in local-only or central mode — session tracking and memory writes work
// without sync.
//
// loop may be nil (local-only mode). Write handlers call loop.Trigger() only
// when loop is non-nil, so the autosync runs immediately after a local write
// when central is configured.
//
// activity must be non-nil; it is shared across all write handlers so that
// mem_save_prompt can record the current prompt and mem_save can auto-capture it.
func registerTools(srv *mcpserver.MCPServer, store *localstore.Store, loop *syncer.Loop, writerID string, activity *SessionActivity) {
	// ── mem_session_start ────────────────────────────────────────────────────
	srv.AddTool(
		mcp.NewTool("mem_session_start",
			mcp.WithDescription("Register the start of a new coding session. Call this at the beginning of a session to track activity."),
			mcp.WithTitleAnnotation("Start Session"),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithString("id",
				mcp.Required(),
				mcp.Description("Unique session identifier"),
			),
			mcp.WithString("directory",
				mcp.Description("Working directory"),
			),
		),
		handleSessionStart(store),
	)

	// ── mem_session_end ──────────────────────────────────────────────────────
	srv.AddTool(
		mcp.NewTool("mem_session_end",
			mcp.WithDescription("Mark a coding session as completed with an optional summary."),
			mcp.WithTitleAnnotation("End Session"),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithString("id",
				mcp.Required(),
				mcp.Description("Session identifier to close"),
			),
			mcp.WithString("summary",
				mcp.Description("Summary of what was accomplished"),
			),
		),
		handleSessionEnd(store, activity),
	)

	// ── mem_save ─────────────────────────────────────────────────────────────
	srv.AddTool(
		mcp.NewTool("mem_save",
			mcp.WithDescription(`Save an important observation to persistent memory. Call this PROACTIVELY after completing significant work — don't wait to be asked.

WHEN to save (call this after each of these):
- Architectural decisions or tradeoffs
- Bug fixes (what was wrong, why, how you fixed it)
- New patterns or conventions established
- Configuration changes or environment setup
- Important discoveries or gotchas
- File structure changes

FORMAT for content — use this structured format:
  **What**: [concise description of what was done]
  **Why**: [the reasoning, user request, or problem that drove it]
  **Where**: [files/paths affected, e.g. src/auth/middleware.ts, internal/store/store.go]
  **Learned**: [any gotchas, edge cases, or decisions made — omit if none]

TITLE should be short and searchable, like: "JWT auth middleware", "FTS5 query sanitization", "Fixed N+1 in user list"`),
			mcp.WithTitleAnnotation("Save Memory"),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(false),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithString("title",
				mcp.Required(),
				mcp.Description("Short, searchable title (e.g. 'JWT auth middleware', 'Fixed N+1 query')"),
			),
			mcp.WithString("content",
				mcp.Description("Structured content using **What**, **Why**, **Where**, **Learned** format"),
			),
			mcp.WithString("type",
				mcp.Description("Category: decision, architecture, bugfix, pattern, config, discovery, learning (default: manual)"),
			),
			mcp.WithString("session_id",
				mcp.Description("Session ID to associate with (default: manual-save-{project})"),
			),
			mcp.WithString("scope",
				mcp.Description("Scope for this observation: project (default) or personal"),
			),
			mcp.WithString("topic_key",
				mcp.Description("Optional topic identifier for upserts (e.g. architecture/auth-model). Reuses and updates the latest observation in same project+scope."),
			),
			mcp.WithString("project",
				mcp.Description("Optional explicit project for this memory. When omitted the project is auto-detected from the working directory."),
			),
			mcp.WithBoolean("capture_prompt",
				mcp.Description("Automatically capture the current user prompt when available (default: true). Set false for SDD artifacts or automated saves."),
			),
		),
		handleSave(store, loop, writerID, activity),
	)

	// ── mem_save_prompt ──────────────────────────────────────────────────────
	srv.AddTool(
		mcp.NewTool("mem_save_prompt",
			mcp.WithDescription("Save a user prompt to persistent memory. Use this to record what the user asked — their intent, questions, and requests — so future sessions have context about the user's goals."),
			mcp.WithTitleAnnotation("Save User Prompt"),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(false),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithString("content",
				mcp.Required(),
				mcp.Description("The user's prompt text"),
			),
			mcp.WithString("session_id",
				mcp.Description("Session ID to associate with (default: manual-save-{project})"),
			),
			mcp.WithString("project",
				mcp.Description("Optional explicit project for this prompt. When omitted the project is auto-detected from the working directory."),
			),
		),
		handleSavePrompt(store, loop, writerID, activity),
	)

	// ── mem_get_observation ──────────────────────────────────────────────────
	srv.AddTool(
		mcp.NewTool("mem_get_observation",
			mcp.WithDescription("Get the full content of a specific observation by ID. Use when you need the complete, untruncated content of an observation found via mem_search."),
			mcp.WithTitleAnnotation("Get Observation"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithNumber("id",
				mcp.Required(),
				mcp.Description("The observation ID to retrieve"),
			),
		),
		handleGetObservation(store),
	)

	// ── mem_search ───────────────────────────────────────────────────────────
	srv.AddTool(
		mcp.NewTool("mem_search",
			mcp.WithDescription("Search your persistent memory across all sessions. Use this to find past decisions, bugs fixed, patterns used, files changed, or any context from previous coding sessions."),
			mcp.WithTitleAnnotation("Search Memory"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Search query — natural language or keywords"),
			),
			mcp.WithString("type",
				mcp.Description("Filter by type: tool_use, file_change, command, file_read, search, manual, decision, architecture, bugfix, pattern"),
			),
			mcp.WithString("project",
				mcp.Description("Filter by project name"),
			),
			mcp.WithString("scope",
				mcp.Description("Filter by scope: project (default) or personal"),
			),
			mcp.WithNumber("limit",
				mcp.Description("Max results (default: 10, max: 20)"),
			),
			mcp.WithString("mode",
				mcp.Description(`Retrieval mode: "" or "fts" (keyword search, default), "semantic" (cosine only), "hybrid" (FTS + cosine fused via RRF). Semantic modes require an embedding provider to be configured; they degrade gracefully to FTS when unavailable.`),
			),
		),
		handleSearch(store),
	)

	// ── mem_context ───────────────────────────────────────────────────────────
	srv.AddTool(
		mcp.NewTool("mem_context",
			mcp.WithDescription("Get recent memory context from previous sessions. Shows recent sessions and observations to understand what was done before."),
			mcp.WithTitleAnnotation("Get Memory Context"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithString("project",
				mcp.Description("Filter by project (omit for auto-detect)"),
			),
			mcp.WithString("scope",
				mcp.Description("Filter observations by scope: project (default) or personal"),
			),
		),
		handleContext(store),
	)

	// ── mem_judge ─────────────────────────────────────────────────────────────
	srv.AddTool(
		mcp.NewTool("mem_judge",
			mcp.WithDescription(`Record a verdict on a pending memory conflict surfaced by mem_save.

WHEN TO CALL: After mem_save returns judgment_required=true, iterate candidates[] and call mem_judge once per entry using that entry's judgment_id.

PARAMS:
  judgment_id (required) — from candidates[].judgment_id in the mem_save response
  relation    (required) — one of: related, compatible, scoped, conflicts_with, supersedes, not_conflict
  reason      (optional) — free-text explanation of the verdict
  evidence    (optional) — JSON or text supporting the verdict
  confidence  (optional) — float 0..1; default 1.0

WHEN TO ASK USER:
  - confidence < 0.7: surface to user before judging
  - relation in {supersedes, conflicts_with} AND type in {architecture, policy, decision}: ask user
  - otherwise: resolve silently

SUCCESS: Returns the updated relation row with judgment_status="judged".
ERROR: Returns IsError=true if judgment_id is unknown or relation verb is invalid. Row is NOT mutated on error.

Re-judging an already-judged ID overwrites the verdict (deliberate revision).`),
			mcp.WithTitleAnnotation("Judge Memory Conflict"),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(false),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithString("judgment_id",
				mcp.Required(),
				mcp.Description("The judgment_id from candidates[] in the mem_save response (format: rel-<hex>)"),
			),
			mcp.WithString("relation",
				mcp.Required(),
				mcp.Description("Verdict: related | compatible | scoped | conflicts_with | supersedes | not_conflict"),
			),
			mcp.WithString("reason",
				mcp.Description("Free-text explanation of the verdict"),
			),
			mcp.WithString("evidence",
				mcp.Description("Supporting evidence (JSON or free text)"),
			),
			mcp.WithNumber("confidence",
				mcp.Description("Confidence score 0.0..1.0 (default: 1.0)"),
			),
		),
		handleJudge(store),
	)

	// ── mem_session_summary ──────────────────────────────────────────────────
	srv.AddTool(
		mcp.NewTool("mem_session_summary",
			mcp.WithDescription(`Save a comprehensive end-of-session summary. Call this when a session is ending or when significant work is complete.

FORMAT — use this exact structure in the content field:

## Goal
[One sentence: what were we building/working on in this session]

## Instructions
[User preferences, constraints, or context discovered during this session. Skip if nothing notable.]

## Discoveries
- [Technical finding, gotcha, or learning 1]

## Accomplished
- [Completed task 1 — with key implementation details]

## Next Steps
- [What remains to be done — for the next session]

## Relevant Files
- path/to/file.go — [what it does or what changed]`),
			mcp.WithTitleAnnotation("Save Session Summary"),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(false),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithString("content",
				mcp.Required(),
				mcp.Description("Full session summary using the Goal/Instructions/Discoveries/Accomplished/Next Steps/Relevant Files format"),
			),
			mcp.WithString("session_id",
				mcp.Description("Session ID (default: manual-save-{project})"),
			),
		),
		handleSessionSummary(store, loop, writerID),
	)
}

// handleSessionStart returns the handler for mem_session_start. It reads the
// id (required) and directory (optional) arguments, resolves the project from
// directory via internal/project.DetectProject, and calls CreateSession.
//
// Project detection (mirrors old_code handleSessionStart / REQ-308):
//   - If directory is supplied, detect from that path.
//   - If directory is empty, detect from os.Getwd().
//   - DetectProject never errors — it returns "unknown" on failure.
func handleSessionStart(store *localstore.Store) mcpserver.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		id, _ := args["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			return mcp.NewToolResultError("mem_session_start: id is required"), nil
		}

		directory, _ := args["directory"].(string)
		directory = strings.TrimSpace(directory)

		// Resolve project from directory, falling back to cwd.
		resolvedDir := directory
		if resolvedDir == "" {
			if cwd, err := os.Getwd(); err == nil {
				resolvedDir = cwd
			}
		}
		// Surface broken-config and ambiguous-project resolution errors as tool
		// errors (faithful to old_code) rather than silently storing the session
		// under a wrong/basename project. ErrInvalidConfig = malformed
		// .engram/config.json; ErrAmbiguousProject = the directory is a parent of
		// multiple repos so no single project can be chosen. Any other error falls
		// back to the basename.
		det := projectpkg.DetectProjectFull(resolvedDir)
		if det.Error != nil {
			switch {
			case errors.Is(det.Error, projectpkg.ErrInvalidConfig):
				return mcp.NewToolResultError("mem_session_start: " + det.Error.Error()), nil
			case errors.Is(det.Error, projectpkg.ErrAmbiguousProject):
				msg := "mem_session_start: " + det.Error.Error()
				if len(det.AvailableProjects) > 0 {
					msg += " (candidates: " + strings.Join(det.AvailableProjects, ", ") + "); supply a more specific directory"
				}
				return mcp.NewToolResultError(msg), nil
			default:
				det.Project = projectpkg.DetectProject(resolvedDir)
			}
		}
		project := det.Project

		// If the caller supplied a directory, use it; otherwise use the cwd we
		// detected the project from so the stored path is always meaningful.
		if directory == "" {
			directory = resolvedDir
		}

		if err := store.CreateSession(id, project, directory); err != nil {
			return mcp.NewToolResultError("Failed to start session: " + err.Error()), nil
		}

		return mcp.NewToolResultText(
			fmt.Sprintf("Session %q started for project %q", id, project),
		), nil
	}
}

// handleSessionEnd returns the handler for mem_session_end. It reads the id
// (required) and summary (optional) arguments, calls EndSession, and clears
// the session's in-memory activity so stale prompts do not leak across sessions.
func handleSessionEnd(store *localstore.Store, activity *SessionActivity) mcpserver.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		id, _ := args["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			return mcp.NewToolResultError("mem_session_end: id is required"), nil
		}

		summary, _ := args["summary"].(string)

		if err := store.EndSession(id, summary); err != nil {
			return mcp.NewToolResultError("Failed to end session: " + err.Error()), nil
		}

		activity.ClearSession(id)

		return mcp.NewToolResultText(fmt.Sprintf("Session %q completed", id)), nil
	}
}

// resolveSaveProject resolves the project for a write tool call using the same
// precedence as handleSessionStart (mirrors old_code write-tool contract):
//
//  1. Explicit "project" argument if non-empty (caller override).
//  2. DetectProjectFull(cwd) — repo config / git remote / git root / dir basename.
//
// ErrInvalidConfig and ErrAmbiguousProject are surfaced as tool errors exactly
// like handleSessionStart so agents get actionable feedback on misconfigured
// repos (faithful to old_code handleSave precedence).
//
// Conflict detection (explicit project vs store's known projects) is DEFERRED
// to a future PR.
func resolveSaveProject(store *localstore.Store, explicitProject string) (string, *mcp.CallToolResult) {
	if strings.TrimSpace(explicitProject) != "" {
		return strings.TrimSpace(explicitProject), nil
	}

	cwd, _ := os.Getwd()
	det := projectpkg.DetectProjectFull(cwd)
	if det.Error != nil {
		switch {
		case errors.Is(det.Error, projectpkg.ErrInvalidConfig):
			return "", mcp.NewToolResultError("mem_save: project resolution: " + det.Error.Error())
		case errors.Is(det.Error, projectpkg.ErrAmbiguousProject):
			msg := "mem_save: project resolution: " + det.Error.Error()
			if len(det.AvailableProjects) > 0 {
				msg += " (candidates: " + strings.Join(det.AvailableProjects, ", ") + "); pass project= explicitly"
			}
			return "", mcp.NewToolResultError(msg)
		default:
			// Other errors (e.g. no .git): fall back to basename.
			return projectpkg.DetectProject(cwd), nil
		}
	}
	return det.Project, nil
}

// handleSave returns the handler for mem_save.
//
// capture_prompt (default true): after a successful save, if the session has a
// recorded prompt via RecordPrompt that matches this project, it is persisted via
// AddPromptIfMissing. This is BEST-EFFORT: any error is logged to stderr and
// swallowed — it never alters the mem_save result or fails the save.
func handleSave(store *localstore.Store, loop *syncer.Loop, writerID string, activity *SessionActivity) mcpserver.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		title, _ := args["title"].(string)
		title = strings.TrimSpace(title)
		if title == "" {
			return mcp.NewToolResultError("mem_save: title is required"), nil
		}

		content, _ := args["content"].(string)
		typ, _ := args["type"].(string)
		sessionID, _ := args["session_id"].(string)
		sessionID = strings.TrimSpace(sessionID)
		scope, _ := args["scope"].(string)
		topicKey, _ := args["topic_key"].(string)
		explicitProject, _ := args["project"].(string)

		// capture_prompt defaults to true when absent; explicit false disables it.
		capturePrompt := true
		if v, ok := args["capture_prompt"].(bool); ok {
			capturePrompt = v
		}

		project, toolErr := resolveSaveProject(store, explicitProject)
		if toolErr != nil {
			return toolErr, nil
		}

		// Policy check: refuse writes for omitted projects BEFORE any store write.
		// Returns a clear MCP error; writes nothing (no row, no outbox entry).
		pol, polErr := store.GetPolicy(project)
		if polErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("mem_save: policy check for project %q: %v", project, polErr)), nil
		}
		if pol == localstore.PolicyOmitted {
			return mcp.NewToolResultError(fmt.Sprintf("project %q is omitted: capture refused", project)), nil
		}

		result, err := store.AddObservation(localstore.AddObservationParams{
			SessionID: sessionID,
			Type:      typ,
			Title:     title,
			Content:   content,
			Project:   project,
			Scope:     scope,
			TopicKey:  topicKey,
			WriterID:  writerID,
		})
		if err != nil {
			return mcp.NewToolResultError("mem_save: failed to save: " + err.Error()), nil
		}

		// Auto-capture the current prompt for this session+project (best-effort).
		// Errors are swallowed — they must never fail or alter the save result.
		if capturePrompt {
			if prompt, ok := activity.CurrentPrompt(sessionID, project); ok {
				if _, promptErr := store.AddPromptIfMissing(localstore.AddPromptParams{
					SessionID: sessionID,
					Content:   prompt,
					Project:   project,
					WriterID:  writerID,
				}); promptErr != nil {
					fmt.Fprintf(os.Stderr, "engram: auto prompt capture error (non-fatal): %v\n", promptErr)
				}
			}
		}

		triggerSync(loop)

		// Post-save conflict candidate detection (REQ-001).
		// Errors are logged to stderr and swallowed — detection failure MUST NOT fail
		// the save. The save already succeeded; candidate detection is advisory only.
		candidates, candErr := store.FindCandidates(result.ID, localstore.CandidateOptions{
			// nil BM25Floor → store default (-2.0); nil/0 Limit → store default (3).
		})
		if candErr != nil {
			fmt.Fprintf(os.Stderr, "engram: FindCandidates error (non-fatal): %v\n", candErr)
		}

		msg := fmt.Sprintf("Memory saved: %q (id=%d, project=%q)", title, result.ID, project)

		if len(candidates) > 0 {
			// Build judgment envelope — faithful to old_code handleSave envelope format.
			var b strings.Builder
			b.WriteString(msg)
			b.WriteString(fmt.Sprintf("\nCONFLICT REVIEW PENDING — %d candidate(s); use mem_judge to record verdicts.", len(candidates)))
			b.WriteString(fmt.Sprintf("\njudgment_required: true"))
			b.WriteString(fmt.Sprintf("\njudgment_status: pending"))
			// Top-level judgment_id is the first candidate's rel sync_id (design convenience).
			b.WriteString(fmt.Sprintf("\njudgment_id: %s", candidates[0].JudgmentID))
			b.WriteString(fmt.Sprintf("\nid: %d", result.ID))
			b.WriteString(fmt.Sprintf("\nsync_id: %s", result.SyncID))
			b.WriteString("\ncandidates:")
			for _, c := range candidates {
				b.WriteString(fmt.Sprintf("\n  - id: %d", c.ID))
				b.WriteString(fmt.Sprintf("\n    sync_id: %s", c.SyncID))
				b.WriteString(fmt.Sprintf("\n    title: %q", c.Title))
				b.WriteString(fmt.Sprintf("\n    type: %s", c.Type))
				b.WriteString(fmt.Sprintf("\n    score: %.4f", c.Score))
				b.WriteString(fmt.Sprintf("\n    judgment_id: %s", c.JudgmentID))
				if c.TopicKey != nil {
					b.WriteString(fmt.Sprintf("\n    topic_key: %s", *c.TopicKey))
				}
			}
			return mcp.NewToolResultText(b.String()), nil
		}

		return mcp.NewToolResultText(msg), nil
	}
}

// handleGetObservation returns the handler for mem_get_observation.
func handleGetObservation(store *localstore.Store) mcpserver.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		// The MCP SDK decodes JSON numbers as float64.
		rawID, ok := args["id"]
		if !ok {
			return mcp.NewToolResultError("mem_get_observation: id is required"), nil
		}
		idFloat, ok := rawID.(float64)
		if !ok {
			return mcp.NewToolResultError("mem_get_observation: id must be a number"), nil
		}
		// Reject non-integer / out-of-range floats: the MCP SDK delivers all JSON
		// numbers as float64, which cannot represent every int64 above 2^53.
		// >= float64(math.MaxInt64): float64 rounds MaxInt64 (2^63-1) UP to 2^63, so
		// the exact boundary must be rejected — int64(2^63) overflows to negative.
		if idFloat != math.Trunc(idFloat) || idFloat <= 0 || idFloat >= float64(math.MaxInt64) {
			return mcp.NewToolResultError("mem_get_observation: id must be a positive integer"), nil
		}
		id := int64(idFloat)

		rec, err := store.GetObservation(id)
		if err != nil {
			if errors.Is(err, localstore.ErrObservationNotFound) {
				return mcp.NewToolResultError(fmt.Sprintf("mem_get_observation: observation #%d not found", id)), nil
			}
			return mcp.NewToolResultError(fmt.Sprintf("mem_get_observation: %s", err)), nil
		}

		topic := ""
		if rec.TopicKey != nil {
			topic = fmt.Sprintf("\nTopic: %s", *rec.TopicKey)
		}

		text := fmt.Sprintf("#%d [%s] %s\n%s\nSession: %s\nProject: %s\nScope: %s%s\nCreated: %s",
			id, rec.Type, rec.Title,
			rec.Content,
			rec.SessionID,
			rec.Project,
			rec.Scope,
			topic,
			rec.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		)

		return mcp.NewToolResultText(text), nil
	}
}

// handleSessionSummary returns the handler for mem_session_summary.
// It saves a session_summary-typed observation using the cwd-detected project
// and optionally triggers autosync.
func handleSessionSummary(store *localstore.Store, loop *syncer.Loop, writerID string) mcpserver.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		content, _ := args["content"].(string)
		if strings.TrimSpace(content) == "" {
			return mcp.NewToolResultError("mem_session_summary: content is required"), nil
		}

		sessionID, _ := args["session_id"].(string)
		sessionID = strings.TrimSpace(sessionID)

		// Project: prefer the session's stored project (captured at mem_session_start
		// from the client's directory). The daemon is a separate process, so its cwd
		// is not a reliable per-call signal; the session row is. Fall back to cwd
		// detection only when there is no usable session project.
		var project string
		if sessionID != "" {
			if sess, gerr := store.GetSession(sessionID); gerr == nil && sess.Project != "" {
				project = sess.Project
			}
		}
		if project == "" {
			var toolErr *mcp.CallToolResult
			project, toolErr = resolveSaveProject(store, "")
			if toolErr != nil {
				return toolErr, nil
			}
		}

		// Policy check: refuse writes for omitted projects BEFORE any store write —
		// session summaries land in the memories table like any observation.
		pol, polErr := store.GetPolicy(project)
		if polErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("mem_session_summary: policy check for project %q: %v", project, polErr)), nil
		}
		if pol == localstore.PolicyOmitted {
			return mcp.NewToolResultError(fmt.Sprintf("project %q is omitted: capture refused", project)), nil
		}

		result, err := store.AddObservation(localstore.AddObservationParams{
			SessionID: sessionID,
			Type:      "session_summary",
			Title:     fmt.Sprintf("Session summary: %s", project),
			Content:   content,
			Project:   project,
			Scope:     "project",
			WriterID:  writerID,
		})
		if err != nil {
			return mcp.NewToolResultError("mem_session_summary: failed to save: " + err.Error()), nil
		}

		triggerSync(loop)

		return mcp.NewToolResultText(fmt.Sprintf(
			"Session summary saved for project %q (id=%d)", project, result.ID,
		)), nil
	}
}

// handleSearch returns the handler for mem_search. It performs a search with
// optional type/scope/mode filters, using the LENIENT read-project policy so a
// search never hard-errors on an ambiguous or misconfigured cwd.
//
// Mode values: "" / "fts" → FTS only (default, byte-identical to before);
// "semantic" → cosine only; "hybrid" → FTS + cosine fused via RRF.
// An unknown mode is treated as "fts" by SearchMemoriesFiltered.
//
// When mode is "semantic" or "hybrid" and semantic search was unavailable (no
// provider, gated project, provider error, no vectors), the result includes an
// explanatory note — but ONLY when the user explicitly requested a semantic mode.
// The "" / "fts" path NEVER emits a note (keyless byte-identical constraint).
//
// Read tools do NOT use transactions (query-only) and do NOT trigger autosync.
func handleSearch(store *localstore.Store) mcpserver.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		query, _ := args["query"].(string)
		query = strings.TrimSpace(query)
		if query == "" {
			return mcp.NewToolResultError("mem_search: query is required"), nil
		}

		typ, _ := args["type"].(string)
		explicitProject, _ := args["project"].(string)
		scope, _ := args["scope"].(string)
		mode, _ := args["mode"].(string)

		// Lenient limit: accept float64 (JSON number), default 10, cap at 20.
		limit := 10
		if raw, ok := args["limit"].(float64); ok && raw > 0 {
			limit = int(raw)
			if limit > 20 {
				limit = 20
			}
		}

		project := resolveReadProject(explicitProject)
		// REQ-391: personal-scope memories are NOT project-scoped. When scope is
		// personal and no explicit project was given, search across ALL projects so
		// personal memories saved under any project remain visible.
		if strings.EqualFold(strings.TrimSpace(scope), "personal") && strings.TrimSpace(explicitProject) == "" {
			project = ""
		}

		results, degradation, err := store.SearchMemoriesFiltered(query, project, limit, localstore.SearchFilter{
			Type:  typ,
			Scope: scope,
			Mode:  mode,
		})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("mem_search: search error: %s. Try simpler keywords.", err)), nil
		}

		if len(results) == 0 {
			return mcp.NewToolResultText(fmt.Sprintf("No memories found for: %q", query)), nil
		}

		var b strings.Builder
		fmt.Fprintf(&b, "Found %d memories:\n\n", len(results))
		anyTruncated := false
		for i, r := range results {
			preview := r.Content
			const previewLen = 300
			if len([]rune(r.Content)) > previewLen {
				anyTruncated = true
				preview = string([]rune(r.Content)[:previewLen]) + " [preview]"
			}
			fmt.Fprintf(&b, "[%d] #%d (%s) — %s\n    %s\n    project: %s | scope: %s\n",
				i+1, r.ID, r.Type, r.Title,
				preview,
				r.Project, r.Scope)
			if r.TopicKey != nil && *r.TopicKey != "" {
				fmt.Fprintf(&b, "    topic: %s\n", *r.TopicKey)
			}
			b.WriteString("\n")
		}
		if anyTruncated {
			b.WriteString("---\nResults above are previews (300 chars). To read the full content of a specific memory, call mem_get_observation(id: <ID>).\n")
		}

		// Emit the degradation note ONLY when the user explicitly requested a
		// semantic mode (never on the default "" / "fts" path — keyless users must
		// see byte-identical behavior).
		if degradation.Reason != "" && (mode == "semantic" || mode == "hybrid") {
			b.WriteString("\n(")
			b.WriteString(degradation.Reason)
			b.WriteString(")\n")
		}

		return mcp.NewToolResultText(b.String()), nil
	}
}

// handleContext returns the handler for mem_context. It assembles recent
// sessions and observations into the agent-facing context blob via
// store.FormatContext, using the LENIENT read-project policy.
//
// Read tool — no transaction, no autosync trigger.
func handleContext(store *localstore.Store) mcpserver.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		explicitProject, _ := args["project"].(string)
		scope, _ := args["scope"].(string)

		project := resolveReadProject(explicitProject)
		// REQ-391: personal-scope memories are NOT project-scoped (see handleSearch).
		if strings.EqualFold(strings.TrimSpace(scope), "personal") && strings.TrimSpace(explicitProject) == "" {
			project = ""
		}

		contextResult, err := store.FormatContext(project, scope)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("mem_context: failed to get context: %s", err)), nil
		}

		if contextResult == "" {
			return mcp.NewToolResultText("No previous session memories found."), nil
		}

		return mcp.NewToolResultText(contextResult), nil
	}
}

// handleJudge returns the handler for mem_judge. It records a verdict on a
// pending conflict_relations row surfaced by mem_save's judgment envelope.
//
// Params:
//   - judgment_id (required) — from candidates[].judgment_id in the mem_save response
//   - relation    (required) — one of the six valid verbs
//   - reason      (optional) — free-text explanation
//   - evidence    (optional) — supporting text or JSON
//   - confidence  (optional) — float64 0..1; default 1.0
//
// Returns a tool error (IsError=true) when the judgment_id is unknown or the
// relation verb is invalid. On success returns the updated row as readable text.
func handleJudge(store *localstore.Store) mcpserver.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		judgmentID, _ := args["judgment_id"].(string)
		judgmentID = strings.TrimSpace(judgmentID)
		if judgmentID == "" {
			return mcp.NewToolResultError("mem_judge: judgment_id is required"), nil
		}

		relation, _ := args["relation"].(string)
		relation = strings.TrimSpace(relation)
		if relation == "" {
			return mcp.NewToolResultError("mem_judge: relation is required — must be one of: related, compatible, scoped, conflicts_with, supersedes, not_conflict"), nil
		}

		var reasonPtr *string
		if v, ok := args["reason"].(string); ok && strings.TrimSpace(v) != "" {
			s := strings.TrimSpace(v)
			reasonPtr = &s
		}

		var evidencePtr *string
		if v, ok := args["evidence"].(string); ok && strings.TrimSpace(v) != "" {
			s := strings.TrimSpace(v)
			evidencePtr = &s
		}

		// confidence is a JSON number → float64. Default 1.0 when absent.
		// Reject out-of-range values (faithful to old_code) so the agent heuristic
		// (e.g. "confidence < 0.7 → surface to user") can never be corrupted.
		confidence := 1.0
		if v, ok := args["confidence"].(float64); ok {
			if v < 0 || v > 1 {
				return mcp.NewToolResultError("mem_judge: confidence must be between 0.0 and 1.0"), nil
			}
			confidence = v
		}
		confidencePtr := &confidence

		updated, err := store.JudgeRelation(localstore.JudgeRelationParams{
			JudgmentID: judgmentID,
			Relation:   relation,
			Reason:     reasonPtr,
			Evidence:   evidencePtr,
			Confidence: confidencePtr,
		})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("mem_judge: %s", err)), nil
		}

		msg := fmt.Sprintf("Judgment recorded: %s | status=%s | judgment_id=%s",
			updated.Relation, updated.JudgmentStatus, updated.SyncID)
		if updated.Reason != nil {
			msg += fmt.Sprintf(" | reason=%q", *updated.Reason)
		}
		return mcp.NewToolResultText(msg), nil
	}
}

// handleSavePrompt returns the handler for mem_save_prompt. It persists the
// prompt via AddPrompt (which enqueues an outbox entry for central push) and
// records it in the in-memory SessionActivity so that a subsequent mem_save
// with capture_prompt=true can auto-capture it without a re-insert (dedup).
func handleSavePrompt(store *localstore.Store, loop *syncer.Loop, writerID string, activity *SessionActivity) mcpserver.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		content, _ := args["content"].(string)
		content = strings.TrimSpace(content)
		if content == "" {
			return mcp.NewToolResultError("mem_save_prompt: content is required"), nil
		}

		sessionID, _ := args["session_id"].(string)
		sessionID = strings.TrimSpace(sessionID)

		explicitProject, _ := args["project"].(string)
		project, toolErr := resolveSaveProject(store, explicitProject)
		if toolErr != nil {
			return toolErr, nil
		}

		// Policy check: refuse writes for omitted projects BEFORE any store write.
		pol, polErr := store.GetPolicy(project)
		if polErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("mem_save_prompt: policy check for project %q: %v", project, polErr)), nil
		}
		if pol == localstore.PolicyOmitted {
			return mcp.NewToolResultError(fmt.Sprintf("project %q is omitted: capture refused", project)), nil
		}

		if _, err := store.AddPrompt(localstore.AddPromptParams{
			SessionID: sessionID,
			Content:   content,
			Project:   project,
			WriterID:  writerID,
		}); err != nil {
			return mcp.NewToolResultError("mem_save_prompt: failed to save prompt: " + err.Error()), nil
		}

		activity.RecordPrompt(sessionID, project, content)

		triggerSync(loop)

		return mcp.NewToolResultText(fmt.Sprintf("Prompt saved for project %q", project)), nil
	}
}

// triggerSync calls loop.Trigger() when loop is non-nil. It is nil-safe: in
// local-only mode the daemon has no Loop and writes must not panic.
func triggerSync(loop *syncer.Loop) {
	if loop != nil {
		loop.Trigger()
	}
}
