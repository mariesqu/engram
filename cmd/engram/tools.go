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

// registerTools adds the MCP tools exposed by this binary to srv. It is called
// unconditionally from buildDaemon regardless of whether the daemon is running
// in local-only or central mode — session tracking and memory writes work
// without sync.
//
// loop may be nil (local-only mode). Write handlers call loop.Trigger() only
// when loop is non-nil, so the autosync runs immediately after a local write
// when central is configured.
func registerTools(srv *mcpserver.MCPServer, store *localstore.Store, loop *syncer.Loop, writerID string) {
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
		handleSessionEnd(store),
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
		),
		handleSave(store, loop, writerID),
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
// (required) and summary (optional) arguments and calls EndSession.
func handleSessionEnd(store *localstore.Store) mcpserver.ToolHandlerFunc {
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
func handleSave(store *localstore.Store, loop *syncer.Loop, writerID string) mcpserver.ToolHandlerFunc {
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
		scope, _ := args["scope"].(string)
		topicKey, _ := args["topic_key"].(string)
		explicitProject, _ := args["project"].(string)

		project, toolErr := resolveSaveProject(store, explicitProject)
		if toolErr != nil {
			return toolErr, nil
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

		triggerSync(loop)

		return mcp.NewToolResultText(fmt.Sprintf(
			"Memory saved: %q (id=%d, project=%q)", title, result.ID, project,
		)), nil
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
		if idFloat != math.Trunc(idFloat) || idFloat <= 0 || idFloat > math.MaxInt64 {
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

// triggerSync calls loop.Trigger() when loop is non-nil. It is nil-safe: in
// local-only mode the daemon has no Loop and writes must not panic.
func triggerSync(loop *syncer.Loop) {
	if loop != nil {
		loop.Trigger()
	}
}
