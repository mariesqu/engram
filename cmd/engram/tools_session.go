package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
	projectpkg "github.com/mariesqu/engram/internal/project"
	"github.com/mariesqu/engram/internal/syncer"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func registerSessionTools(srv *mcpserver.MCPServer, store *localstore.Store, loop *syncer.Loop, embedLoop *embedding.Loop, gated embedding.EmbeddingProvider, writerID string, activity *SessionActivity, daemonCwdIsWorkspace bool) {
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
			mcp.WithString("project",
				mcp.Description("Optional explicit project for this session. When omitted the project is auto-detected from the working directory. Re-registering a known id with an explicit project UPDATES the stored project — the way to correct a session that was registered under the wrong one. It corrects the SESSION only: observations already saved under the wrong project keep it, use mem_merge_projects for those."),
			),
			mcp.WithString("directory",
				mcp.Description(directoryArgDescription),
			),
			mcp.WithString("cwd",
				mcp.Description(cwdArgDescription),
			),
		),
		handleSessionStart(store, daemonCwdIsWorkspace),
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
			mcp.WithString("project",
				mcp.Description("Optional explicit project for this summary. When omitted the project comes from the session row, else it is auto-detected from the working directory."),
			),
			mcp.WithString("directory",
				mcp.Description(directoryArgDescription),
			),
			mcp.WithString("cwd",
				mcp.Description(cwdArgDescription),
			),
		),
		handleSessionSummary(store, loop, writerID, daemonCwdIsWorkspace),
	)
}

// handleSessionStart returns the handler for mem_session_start. It reads the
// id (required) plus the optional project and directory arguments, resolves the
// project, and calls CreateSession.
//
// Project precedence — the same chain every directory-aware tool follows
// (resolveSaveProject / resolveReadProject):
//
//  1. An explicit "project" argument, when non-empty. It wins outright: no
//     detection runs, so an ambiguous or misconfigured directory cannot fail a
//     call that already named its project. This is what makes "always pass
//     project" a real workaround rather than a way to fall back to the daemon's
//     cwd — `engram connect` suppresses the directory injection whenever a
//     project is present (see injectClientDirectory in connect.go). It is also
//     CORRECTIVE: it rewrites the stored project of an id that already exists
//     (CreateSessionWithProject), which a detected project never does.
//  2. Detection from the resolved directory (mirrors the legacy predecessor's
//     handleSessionStart, REQ-308): the supplied "directory" if any, else
//     os.Getwd() — see resolveProjectDir.
//
// It is a WRITE tool and refuses everything the other write tools refuse: a
// relative directory, no directory at all (dirSourceDaemonCwd), a missing one
// (resolveSaveProject's three guards, reimplemented here because this handler
// resolves its own project to keep the corrective CreateSessionWithProject
// path), and an "omitted" project. Registering a
// session for a project that cannot accept a single memory is a session row
// whose only effect is to make mem_context report activity that produced
// nothing.
//
// This tool's optional "directory" argument is the model every other
// directory-aware tool now follows; `engram connect` fills it with the client's
// working directory when the caller left both it and "project" empty.
func handleSessionStart(store *localstore.Store, daemonCwdIsWorkspace bool) mcpserver.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		id, _ := args["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			return mcp.NewToolResultError("mem_session_start: id is required"), nil
		}

		explicitProject, _ := args["project"].(string)
		explicitProject = strings.TrimSpace(explicitProject)
		dirArg := readDirectoryArg(args)
		if dirArg.Err != nil {
			return dirArg.toolError("mem_session_start"), nil
		}
		directory := dirArg.Directory

		// The directory the session row records, and — absent an explicit
		// project — the one detection runs against.
		resolvedDir := resolveProjectDir(directory)

		project := explicitProject
		if project == "" {
			// The same three refusals resolveSaveProject applies, in the same order
			// and for the same reason: an explicit project skips all of them (it is
			// the remedy every writes_blocked hint offers), everything else must not
			// invent one.
			if dirArg.Relative {
				return dirArg.relativeError("mem_session_start"), nil
			}
			if dirArg.Source == dirSourceDaemonCwd && !daemonCwdIsWorkspace {
				return dirArg.daemonCwdError("mem_session_start"), nil
			}
			if !directoryExists(resolvedDir) {
				return missingDirectoryError("mem_session_start", resolvedDir), nil
			}
			// Surface broken-config and ambiguous-project resolution errors as tool
			// errors (faithful to the legacy predecessor) rather than silently storing
			// the session under a wrong/basename project. ErrInvalidConfig = malformed
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
						msg += " (candidates: " + strings.Join(det.AvailableProjects, ", ") +
							"); pass project= explicitly or supply a more specific directory"
					}
					return mcp.NewToolResultError(msg), nil
				default:
					det.Project = projectpkg.DetectProject(resolvedDir)
				}
			}
			project = det.Project
		}

		// Policy check: an "omitted" project refuses capture, so registering a
		// session for one promises a place to save that does not exist. Applied
		// AFTER resolution so an explicit project is checked too — naming a project
		// skips detection, not the policy (mem_current_project reports exactly this
		// as writes_blocked, for mem_session_start by name).
		pol, polErr := store.GetPolicy(project)
		if polErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("mem_session_start: policy check for project %q: %v", project, polErr)), nil
		}
		if pol == localstore.PolicyOmitted {
			return mcp.NewToolResultError(fmt.Sprintf("project %q is omitted: capture refused", project)), nil
		}

		// If the caller supplied a directory, use it; otherwise use the cwd we
		// detected the project from so the stored path is always meaningful.
		if directory == "" {
			directory = resolvedDir
		}

		// An explicit project is CORRECTIVE: re-registering an id that was already
		// stored under a misdetected project rewrites it (CreateSessionWithProject),
		// whereas a detected project never overwrites a populated row (REQ-308).
		// Without the split, "just re-run mem_session_start with project=X" — the
		// only remedy left once a project suppresses the directory injection —
		// would report success and change nothing.
		create := store.CreateSession
		if explicitProject != "" {
			create = store.CreateSessionWithProject
		}
		if err := create(id, project, directory); err != nil {
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

// handleSessionSummary returns the handler for mem_session_summary. It saves a
// session_summary-typed observation and optionally triggers autosync.
//
// Project precedence, highest first — the uniform chain, with the session row
// wedged in where this tool has one:
//
//  1. An explicit "project" argument (delegated to resolveSaveProject, which
//     returns it untouched). It outranks the session row too: an agent that
//     names a project must not have it silently overridden by whatever a
//     stale/foreign session_id happens to point at.
//  2. The session's stored project (captured at mem_session_start from the
//     client's directory). The daemon is a separate process, so its cwd is not
//     a reliable per-call signal; the session row is.
//  3. Detection from the resolved directory (forwarded "directory", else the
//     daemon's cwd).
func handleSessionSummary(store *localstore.Store, loop *syncer.Loop, writerID string, daemonCwdIsWorkspace bool) mcpserver.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		content, _ := args["content"].(string)
		if strings.TrimSpace(content) == "" {
			return mcp.NewToolResultError("mem_session_summary: content is required"), nil
		}

		sessionID, _ := args["session_id"].(string)
		sessionID = strings.TrimSpace(sessionID)
		explicitProject, _ := args["project"].(string)
		explicitProject = strings.TrimSpace(explicitProject)

		// Session row: consulted only when the caller named no project, so an
		// explicit one wins outright (precedence rule 1 above).
		var project string
		if explicitProject == "" && sessionID != "" {
			if sess, gerr := store.GetSession(sessionID); gerr == nil && sess.Project != "" {
				project = sess.Project
			}
		}
		if project == "" {
			dirArg := readDirectoryArg(args)
			if dirArg.Err != nil {
				return dirArg.toolError("mem_session_summary"), nil
			}
			var toolErr *mcp.CallToolResult
			// Returns explicitProject verbatim when set; otherwise detects from the
			// forwarded directory / daemon cwd and may hard-error.
			project, toolErr = resolveSaveProject(store, "mem_session_summary", explicitProject, dirArg, daemonCwdIsWorkspace)
			if toolErr != nil {
				return toolErr, nil
			}
		}

		// Default session_id to "manual-save-{project}" when omitted, using the
		// FINAL resolved project — matches the tool description's documented
		// default (localstore.ManualSaveSessionPrefix). Applied AFTER the
		// session-lookup above so an explicit empty session_id still resolves the
		// project from cwd rather than a session row.
		if sessionID == "" {
			sessionID = localstore.DefaultManualSessionID(project)
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
