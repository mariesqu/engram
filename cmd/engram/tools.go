package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mariesqu/engram/internal/localstore"
	projectpkg "github.com/mariesqu/engram/internal/project"
)

// registerTools adds the MCP tools exposed by this binary to srv. It is called
// unconditionally from buildDaemon regardless of whether the daemon is running
// in local-only or central mode — session tracking works without sync.
//
// This function establishes the tool-registration pattern for subsequent PRs:
//   - Each tool is a mcp.NewTool(...) with its parameter schema.
//   - The handler is a closure over store.
//   - Handlers return (*mcp.CallToolResult, nil) — the SDK treats a returned
//     error as a transport-level failure; tool-level failures use
//     mcp.NewToolResultError so the error is visible to the agent.
func registerTools(srv *mcpserver.MCPServer, store *localstore.Store) {
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
		project := projectpkg.DetectProject(resolvedDir)

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
