package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
	projectpkg "github.com/mariesqu/engram/internal/project"
	"github.com/mariesqu/engram/internal/syncer"
	"github.com/mariesqu/engram/internal/topickey"
)

// directoryArgDescription is the shared doc string for the optional "directory"
// argument of every directory-aware tool. It is deliberately identical across
// tools — agents should leave "directory" to the transport — so it is worded to
// hold for all of them. Every directory-aware tool also accepts "project", which
// outranks it (see TestRegisterTools_DirectoryAwareToolsDeclareProject).
// TestRegisterTools_DirectoryAwareToolsDeclareDirectory pins that every
// directory-aware tool ships exactly this text.
const directoryArgDescription = "Directory to resolve the project from. Normally injected automatically by 'engram connect' " +
	"(the client's working directory); set it by hand only to target a different checkout. " +
	"Where a tool also accepts 'project', prefer that."

// cwdArgDescription is the shared doc string for the "cwd" alias of the
// "directory" argument. Every directory-aware tool declares it and every
// directory-aware handler reads it through readDirectoryArg, so the alias the
// injected agent protocol tells models to use ("call it with the workspace in
// cwd") means the same thing everywhere instead of on mem_current_project only.
// TestRegisterTools_DirectoryAwareToolsDeclareCwdAlias pins the text.
const cwdArgDescription = "Alias for \"directory\", read ONLY when \"directory\" is absent or blank — " +
	"'engram connect' injects the real client directory into \"directory\", and that value must keep " +
	"winning over a hand-written path."

// Directory-source labels. They answer the question an agent cannot otherwise
// ask — WHOSE idea was the directory this answer describes? — and are reported
// verbatim in mem_current_project's directory_source field.
const (
	// dirSourceArgument: the "directory" argument decided. Normally that is the
	// CLIENT's working directory, injected by `engram connect`.
	dirSourceArgument = "argument"
	// dirSourceCwdAlias: the "cwd" alias decided, i.e. a path the MODEL supplied.
	// Weaker evidence than dirSourceArgument by construction — see readDirectoryArg.
	dirSourceCwdAlias = "cwd_alias"
	// dirSourceDaemonCwd: nothing reached the daemon, so the answer describes the
	// SHARED daemon's own working directory — the original junk-project misfile.
	dirSourceDaemonCwd = "daemon_cwd"
	// dirSourceInvalid: "directory" was present but not a JSON string. Never
	// silently downgraded to the alias — see readDirectoryArg.
	dirSourceInvalid = "invalid_directory_argument"
)

// directoryArg is the outcome of reading the directory a tool call resolves its
// project from. Source is one of the dirSource* labels; Err is non-nil only for
// dirSourceInvalid.
type directoryArg struct {
	Directory string
	Source    string
	Err       error
}

// readDirectoryArg extracts the directory a directory-aware tool call resolves
// its project from, applying one precedence for ALL of them:
//
//  1. "directory", when it is a non-blank string. `engram connect` injects the
//     CLIENT process's working directory here — the only value in the chain that
//     is OBSERVED rather than guessed.
//  2. "cwd", the alias the injected agent protocol tells models to fill in. It is
//     consulted ONLY when the "directory" KEY is absent, JSON null, or a blank
//     string, so a hallucinated path can never re-point a session that already
//     carries a real directory.
//  3. Neither — the caller gets "" and resolveProjectDir falls back to the
//     daemon's own cwd.
//
// A "directory" that is PRESENT but not a string is a caller error, reported as
// such (Err non-nil) rather than treated as absent. That case is the one the
// bridge cannot help with: injectClientDirectory deliberately suppresses
// injection for a non-string "directory" (hasNonEmptyStringArg in connect.go —
// "the caller's error to see, not ours to paper over"), so silently falling
// through to "cwd" here would resolve the session from a model-supplied path
// while the caller believes their own argument is in force.
func readDirectoryArg(args map[string]any) directoryArg {
	if raw, present := args["directory"]; present && raw != nil {
		dir, ok := raw.(string)
		if !ok {
			return directoryArg{
				Source: dirSourceInvalid,
				Err: fmt.Errorf("directory must be a string, got %T; "+
					"drop it and let 'engram connect' inject the client directory, or pass project explicitly", raw),
			}
		}
		if dir = strings.TrimSpace(dir); dir != "" {
			return directoryArg{Directory: dir, Source: dirSourceArgument}
		}
	}
	if raw, ok := args["cwd"].(string); ok {
		if cwd := strings.TrimSpace(raw); cwd != "" {
			return directoryArg{Directory: cwd, Source: dirSourceCwdAlias}
		}
	}
	return directoryArg{Source: dirSourceDaemonCwd}
}

// toolError renders a dirSourceInvalid directoryArg as the MCP tool error the
// calling tool returns. mem_current_project is the one caller that does NOT use
// it: it never errors, and reports the same condition in its envelope instead.
func (d directoryArg) toolError(tool string) *mcp.CallToolResult {
	return mcp.NewToolResultError(tool + ": " + d.Err.Error())
}

// directoryAwareTools names the MCP tools whose handlers resolve the project
// from a directory (resolveProjectDir → resolveReadProject / resolveSaveProject
// / handleSessionStart). It is the single source of truth shared by the daemon
// (which declares the optional "directory" argument on exactly these tools'
// schemas, see registerTools) and by the `engram connect` bridge running in the
// CLIENT process (which injects the client's working directory into exactly
// these tools/call frames, see injectClientDirectory in connect.go).
//
// Tools absent from this map never see a "directory" argument on the wire —
// their project is either irrelevant (mem_get_observation, mem_update,
// mem_judge, …) or derived from the data itself (mem_similar reads the source
// row's project).
var directoryAwareTools = map[string]bool{
	"mem_current_project": true,
	"mem_session_start":   true,
	"mem_session_summary": true,
	"mem_save":            true,
	"mem_save_prompt":     true,
	"mem_search":          true,
	"mem_context":         true,
	"mem_review":          true,
}

// resolveProjectDir returns the directory that project detection must run
// against, applying the precedence every tool shares:
//
//  1. The caller-supplied "directory" argument, when non-empty. `engram connect`
//     injects the CLIENT process's working directory here (see connect.go) —
//     the daemon is SHARED and typically resident, so its own cwd is whatever
//     directory the autostart/tray happened to run from (on Windows commonly
//     C:\Windows\system32), never the repo the agent is working in.
//  2. The daemon's own working directory. This is the correct answer for a
//     per-client `engram daemon --transport stdio`, which the MCP client spawns
//     in the project directory itself.
//
// The result is ABSOLUTE and Clean: detection's own basename fallback reads
// filepath.Base(dir) verbatim, so a relative "." or "./repo" — which an agent
// filling in the "cwd" alias will write sooner or later — would otherwise
// resolve to the project "unknown" or "repo" while every neighbouring tool
// answered from a different name. Abs failing (an unreadable cwd) leaves the
// input untouched rather than inventing a path.
//
// A "" return (cwd unavailable) is passed through to DetectProjectFull, which
// treats it as ".".
func resolveProjectDir(directory string) string {
	dir := strings.TrimSpace(directory)
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if dir == "" {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs // filepath.Abs Cleans its result
	}
	return filepath.Clean(dir)
}

// resolveReadProject resolves the project for a READ tool call. Unlike write
// tools, read tools are LENIENT: no hard errors on ambiguous or invalid config.
// The policy mirrors the legacy predecessor's handleSearch/handleContext
// (REQ-310 lenient path):
//
//  1. Explicit project argument wins (normalized, used as-is — no store lookup).
//  2. Detect from the resolved directory (forwarded "directory" argument, else
//     the daemon's cwd — see resolveProjectDir) via DetectProjectFull.
//  3. On ANY detection error (ambiguous, invalid config, no .git, etc.) fall
//     back to the dir basename via DetectProject. Read tools never return a
//     project-resolution error to the agent.
//
// This contrasts with write tools (resolveSaveProject) which hard-error on
// ErrInvalidConfig and ErrAmbiguousProject.
func resolveReadProject(explicitProject, directory string) string {
	if strings.TrimSpace(explicitProject) != "" {
		return strings.TrimSpace(explicitProject)
	}
	dir := resolveProjectDir(directory)
	det := projectpkg.DetectProjectFull(dir)
	if det.Error != nil {
		// Lenient: fall back to basename, never error.
		return projectpkg.DetectProject(dir)
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
// embedLoop may be nil (Noop provider / no key). Write handlers call
// embedLoop.Trigger() nil-safely after a successful save so the backfill loop
// picks up newly written rows without waiting for the next periodic tick.
//
// activity must be non-nil; it is shared across all write handlers so that
// mem_save_prompt can record the current prompt and mem_save can auto-capture it.
func registerTools(srv *mcpserver.MCPServer, store *localstore.Store, loop *syncer.Loop, embedLoop *embedding.Loop, gated embedding.EmbeddingProvider, writerID string, activity *SessionActivity) {
	// ── mem_current_project ──────────────────────────────────────────────────
	// Registered first because it is meant to be CALLED first: agent protocols
	// (gentle-ai's ODD protocol among them) open a session by asking which
	// project this caller resolves to, before any read or write.
	srv.AddTool(
		mcp.NewTool("mem_current_project",
			mcp.WithDescription(`Detect the project Engram resolves for THIS caller, and how it got there. Returns project, project_source, project_path (the canonical directory of the project — repo root, config directory, or the resolved cwd), cwd (absolute, cleaned), cwd_input (what you passed, verbatim), directory_source, directory_exists, available_projects, fallback, writes_blocked and optional hints. NEVER errors — use it for discovery before writing. Recommended as the first call when starting a new session.

Three fields say "do not trust this name blindly":
  fallback=true         — the project name is a GUESS (a directory basename, or a lenient fallback after a resolution error). Pass an explicit project on later calls if that is not the name you want.
  writes_blocked=true   — mem_save/mem_session_start/mem_session_summary will REFUSE this directory (ambiguous, misconfigured, or an omitted project) until you pass project explicitly.
  directory_exists=false — the resolved directory does not exist, so any name here is invented from its basename. Pass a real directory or an explicit project.`),
			mcp.WithTitleAnnotation("Detect Current Project"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithString("project",
				mcp.Description("Optional explicit project. When set it is echoed back verbatim with project_source=\"explicit_override\" and no detection runs — the way to confirm the exact name your later calls will use."),
			),
			mcp.WithString("directory",
				mcp.Description(directoryArgDescription),
			),
			mcp.WithString("cwd",
				mcp.Description(cwdArgDescription),
			),
		),
		handleCurrentProject(store),
	)

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
			mcp.WithString("directory",
				mcp.Description(directoryArgDescription),
			),
			mcp.WithString("cwd",
				mcp.Description(cwdArgDescription),
			),
			mcp.WithBoolean("capture_prompt",
				mcp.Description("Automatically capture the current user prompt when available (default: true). Set false for SDD artifacts or automated saves."),
			),
		),
		handleSave(store, loop, embedLoop, gated, writerID, activity),
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
			mcp.WithString("directory",
				mcp.Description(directoryArgDescription),
			),
			mcp.WithString("cwd",
				mcp.Description(cwdArgDescription),
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

	// ── mem_update ───────────────────────────────────────────────────────────
	srv.AddTool(
		mcp.NewTool("mem_update",
			mcp.WithDescription(`Edit an existing memory in place by its observation ID. Use this to correct or revise a SPECIFIC memory you already know the ID of (from mem_search or mem_get_observation) — e.g. fixing a wrong detail or refining wording.

Provide the id plus the field(s) to change: title and/or content (and optionally type). Omitted fields keep their current value. The edit is versioned (version+1), propagates to central on the next sync, and the content is re-embedded for semantic search.

For an EVOLVING topic, prefer mem_save with a topic_key (upsert). Use mem_update when you need to edit one specific observation by its ID.`),
			mcp.WithTitleAnnotation("Update Memory"),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(false),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithNumber("id",
				mcp.Required(),
				mcp.Description("The observation ID to edit (from mem_search or mem_get_observation)"),
			),
			mcp.WithString("title",
				mcp.Description("New title. Omit to keep the current title."),
			),
			mcp.WithString("content",
				mcp.Description("New content. Omit to keep the current content."),
			),
			mcp.WithString("type",
				mcp.Description("New type/category (decision, bugfix, pattern, …). Omit to keep the current type."),
			),
		),
		handleUpdate(store, loop, embedLoop, writerID),
	)

	// ── mem_suggest_topic_key ────────────────────────────────────────────────
	srv.AddTool(
		mcp.NewTool("mem_suggest_topic_key",
			mcp.WithDescription(`Suggest a STABLE topic_key for a memory you are about to save, so re-saving the same topic in a later session UPSERTS the existing chain instead of creating a near-duplicate.

The suggestion is deterministic — the same title/type/content always yields the same "family/segment" key (e.g. "architecture/auth-model"). Call this when you intend to use a topic_key but want a consistent one across sessions, then pass the returned value as mem_save's topic_key.`),
			mcp.WithTitleAnnotation("Suggest Topic Key"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithString("title",
				mcp.Required(),
				mcp.Description("The memory's title — the primary source of the key segment"),
			),
			mcp.WithString("type",
				mcp.Description("The memory's type/category (decision, bugfix, architecture, …) — informs the key family"),
			),
			mcp.WithString("content",
				mcp.Description("Optional content; helps infer the family and is a fallback segment when the title is empty"),
			),
		),
		handleSuggestTopicKey(),
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
			mcp.WithString("directory",
				mcp.Description(directoryArgDescription),
			),
			mcp.WithString("cwd",
				mcp.Description(cwdArgDescription),
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
			mcp.WithString("directory",
				mcp.Description(directoryArgDescription),
			),
			mcp.WithString("cwd",
				mcp.Description(cwdArgDescription),
			),
		),
		handleContext(store),
	)

	// ── mem_pin / mem_unpin ───────────────────────────────────────────────────
	// Registered next to mem_context because that is where a pin is CASHED IN:
	// pinned memories render in their own section ahead of recent observations.
	srv.AddTool(
		mcp.NewTool("mem_pin",
			mcp.WithDescription("Pin a memory so it is surfaced in mem_context ahead of recent observations, and ranked slightly higher in keyword search. Use it for the handful of facts that must stay in front of you — the stack decision, the one gotcha that keeps biting. Pinned state is LOCAL to this machine and is never synced."),
			mcp.WithTitleAnnotation("Pin Memory"),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithNumber("id",
				mcp.Required(),
				mcp.Description("The observation ID to pin (from mem_search or mem_get_observation)"),
			),
		),
		handlePin(store, true),
	)

	srv.AddTool(
		mcp.NewTool("mem_unpin",
			mcp.WithDescription("Unpin a memory so it returns to normal recency order in mem_context and loses its search boost. Pinned state is LOCAL to this machine and is never synced."),
			mcp.WithTitleAnnotation("Unpin Memory"),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithNumber("id",
				mcp.Required(),
				mcp.Description("The observation ID to unpin"),
			),
		),
		handlePin(store, false),
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

	// ── mem_similar ──────────────────────────────────────────────────────────
	srv.AddTool(
		mcp.NewTool("mem_similar",
			mcp.WithDescription("Find memories semantically similar to a source memory, using its stored embedding vector. Requires an embedding provider to be configured and the source memory to have an embedding."),
			mcp.WithTitleAnnotation("Find Similar Memories"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithString("sync_id",
				mcp.Required(),
				mcp.Description("The sync_id of the source memory whose neighbours to find"),
			),
			mcp.WithString("project",
				mcp.Description("Filter results to a specific project (default: same project as the source memory)"),
			),
			mcp.WithNumber("limit",
				mcp.Description("Max results to return (default: 5, max: 20)"),
			),
		),
		handleMemSimilar(store, gated),
	)

	// ── mem_review ────────────────────────────────────────────────────────────
	srv.AddTool(
		mcp.NewTool("mem_review",
			mcp.WithDescription(`Review the lifecycle/staleness of saved memories so stale architecture/decision notes are VERIFIED before being trusted, not trusted blindly.

action="list": list memories by review status — status filter is one of:
  needs_review (default) | active | expired | all. Optional project and limit.
  Returns id, title, type, project, status, review_after.

action="mark_reviewed": reset the staleness clock on memories you have verified.
  Provide ids (a number array) OR a topic_key (resolves to its current observation).
  The new due date is recomputed from the memory's TYPE — decision +6 months,
  policy +12, preference +3, anything else + the staleness window. Returns the
  count updated.

Status is computed at read time: a memory is "needs_review" once past its review_after (set at save time for decision/policy/preference) or, when it has none, once it ages past the staleness window; "expired" once past its expires_at; else "active". mark_reviewed is a LOCAL-ONLY write (it does not sync).`),
			mcp.WithTitleAnnotation("Review Memory Lifecycle"),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(false),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithString("action",
				mcp.Required(),
				mcp.Description("Action: \"list\" or \"mark_reviewed\""),
			),
			mcp.WithString("status",
				mcp.Description("list filter: needs_review (default) | active | expired | all"),
			),
			mcp.WithArray("ids",
				mcp.Description("mark_reviewed: observation IDs (numbers) to mark as reviewed"),
				mcp.Items(map[string]any{"type": "number"}),
			),
			mcp.WithString("topic_key",
				mcp.Description("mark_reviewed: alternative to ids — resolve a topic_key (in scope \"project\") to its current observation and mark it reviewed; for personal-scope memories use ids"),
			),
			mcp.WithString("project",
				mcp.Description("Filter (list) / resolution scope (mark_reviewed via topic_key). Omit to auto-detect."),
			),
			mcp.WithNumber("limit",
				mcp.Description("list: max results (default 50, max 200)"),
			),
			mcp.WithString("directory",
				mcp.Description(directoryArgDescription),
			),
			mcp.WithString("cwd",
				mcp.Description(cwdArgDescription),
			),
		),
		handleReview(store),
	)

	// ── mem_merge_projects ────────────────────────────────────────────────────
	srv.AddTool(
		mcp.NewTool("mem_merge_projects",
			mcp.WithDescription(`Merge a source project's local memories into a target (canonical) project name — cleans up project name drift (e.g. "myapp" → "my-app").

Renames every local memory under "from" to live under "to", and dedups the per-project policy and pull-cursor rows. This is a LOCAL-ONLY rename: it does not propagate to central (each node merges independently). from and to are both required and must differ.`),
			mcp.WithTitleAnnotation("Merge Projects"),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithIdempotentHintAnnotation(false),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithString("from",
				mcp.Required(),
				mcp.Description("Source project name to merge FROM (its rows are renamed)"),
			),
			mcp.WithString("to",
				mcp.Required(),
				mcp.Description("Target (canonical) project name to merge INTO"),
			),
		),
		handleMergeProjects(store),
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
		handleSessionSummary(store, loop, writerID),
	)
}

// handleCurrentProject returns the handler for mem_current_project — the
// session-bootstrap probe. It answers one question: which project will THIS
// caller's tool calls resolve to, and how was that name derived?
//
// It reports the LENIENT read resolution (the resolveReadProject chain:
// explicit project → detection from the resolved directory → basename), because
// that is what mem_search / mem_context / mem_review actually answer from. What
// it adds on top is the part an agent cannot otherwise see until something goes
// wrong — the ways that name is a GUESS rather than an identity:
//
//   - fallback=true: the project is only a directory basename, or a lenient
//     fallback after a resolution error. Nothing declared it (no
//     .engram/config.json, no git remote, no git root), so it changes the day
//     the folder is renamed.
//   - directory_source="daemon_cwd": no directory reached the daemon, so the
//     answer describes the DAEMON's working directory. The daemon is shared and
//     typically resident from wherever autostart or the tray launched it (on
//     Windows commonly C:\Windows\system32) — the original junk-project misfile.
//   - directory_source="cwd_alias": the directory came from the MODEL, not from
//     `engram connect`. It is an assertion about the workspace, not an
//     observation of it.
//   - directory_exists=false: the resolved directory is not there at all, so the
//     basename it yields names nothing that exists.
//
// Plus writes_blocked=true for the read/write asymmetry: an ambiguous
// directory, a broken .engram/config.json, a missing directory, or an omitted
// project all answer reads from the basename while making every write tool
// refuse. Without the flag an agent learns that only from a failed mem_save,
// halfway through a session.
//
// store is read ONLY for the project's sync policy (PolicyOmitted ⇒ writes are
// refused before any row is written — see handleSave). It may be nil, which
// skips that check; every other field is filesystem-derived.
//
// Like its upstream counterpart it NEVER returns a tool error: a discovery call
// that fails is a discovery call an agent stops making. That holds even for a
// non-string "directory", which every OTHER directory-aware tool rejects as a
// caller error — here it is reported in the envelope
// (directory_source="invalid_directory_argument") so the agent can still see
// what the daemon would resolve.
func handleCurrentProject(store *localstore.Store) mcpserver.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		explicitProject, _ := args["project"].(string)
		dirArg := readDirectoryArg(args)

		envelope := currentProjectEnvelope(store, explicitProject, dirArg)

		out, err := json.Marshal(envelope)
		if err != nil {
			// Unreachable: the envelope holds only strings, bools and []string.
			// Degrade to the one field that matters rather than to an error — the
			// never-errors contract is the point of this tool.
			return mcp.NewToolResultText(fmt.Sprintf("project: %v", envelope["project"])), nil
		}
		return mcp.NewToolResultText(string(out)), nil
	}
}

// currentProjectEnvelope builds the mem_current_project response body. It is
// separated from the MCP plumbing so the resolution contract can be tested
// directly; it reads the filesystem (detection), the store (policy), and — only
// when no directory reached the daemon — the process working directory.
//
// Field contract. Always present:
//
//	project, project_source, project_path (CANONICAL directory of the project:
//	the repo root for the git cases, the directory holding .engram/config.json
//	for a pinned one, else the resolved cwd), cwd (absolute and cleaned),
//	cwd_input (the caller's value verbatim, "" when none), directory_source,
//	directory_exists, available_projects (never null), fallback, writes_blocked.
//
// Advisory, present only when they have something to say: warning, error_hint,
// hints. hints is an ARRAY — the reasons a name is untrustworthy compose (a
// daemon-cwd answer for a missing directory under an omitted project is three
// separate problems), and a joined sentence makes an agent parse prose to tell
// them apart.
func currentProjectEnvelope(store *localstore.Store, explicitProject string, dirArg directoryArg) map[string]any {
	dir := resolveProjectDir(dirArg.Directory)

	env := map[string]any{
		"project":            "",
		"project_source":     "",
		"project_path":       "",
		"cwd":                dir,
		"cwd_input":          dirArg.Directory,
		"directory_source":   dirArg.Source,
		"directory_exists":   false,
		"available_projects": []string{},
		"fallback":           false,
		"writes_blocked":     false,
	}

	var hints []string
	if dirArg.Err != nil {
		// The one caller error this tool reports instead of raising: the bridge
		// leaves a non-string "directory" alone (see injectClientDirectory), so
		// nothing else in the chain would ever mention it.
		env["error_hint"] = dirArg.Err.Error()
		hints = append(hints, "the \"directory\" argument was not a string and was IGNORED (the \"cwd\" alias is "+
			"deliberately not consulted for it) — this answer describes the daemon's own directory")
	}

	// Does the directory the answer is about actually exist? Detection happily
	// derives a basename from a path that is not there, which is how a typo'd
	// ENGRAM_CLIENT_DIR or a hallucinated "cwd" invents a brand-new project.
	info, statErr := os.Stat(dir)
	switch {
	case statErr == nil && info.IsDir():
		env["directory_exists"] = true
	case dir == "":
		hints = append(hints, "the daemon could not read its own working directory, so no directory could be resolved — pass directory or project explicitly")
	default:
		hints = append(hints, "the resolved directory does not exist (or is not a directory), so any project name here is invented from its basename — pass a real directory or an explicit project")
	}

	// An explicit project wins outright in every other tool (resolveReadProject /
	// resolveSaveProject), so no detection runs here either: reporting a detected
	// project beside an explicit one would only invite an agent to second-guess
	// the name it just supplied. The policy check below still applies — an
	// explicit name does not make an omitted project writable.
	if explicitProject = strings.TrimSpace(explicitProject); explicitProject != "" {
		env["project"] = explicitProject
		env["project_source"] = projectpkg.SourceExplicitOverride
		applyPolicyBlock(store, env, &hints)
		setHints(env, hints)
		return env
	}

	if !env["directory_exists"].(bool) {
		// Detection would answer from the basename of a path that is not there.
		// Report the name every read tool will use, but never as a normal answer:
		// missing_directory says the name describes nothing on this machine.
		env["project"] = projectpkg.DetectProject(dir)
		env["project_source"] = sourceMissingDirectory
		env["project_path"] = dir
		env["fallback"] = true
		env["writes_blocked"] = true
		hints = append(hints, "writes (mem_save, mem_session_start, mem_session_summary) should not file memories under a directory that does not exist — pass project explicitly")
		applyPolicyBlock(store, env, &hints)
		setHints(env, hints)
		return env
	}

	det := projectpkg.DetectProjectFull(dir)
	env["project"] = det.Project
	env["project_source"] = det.Source
	env["project_path"] = det.Path
	if len(det.AvailableProjects) > 0 {
		env["available_projects"] = det.AvailableProjects
	}
	if det.Warning != "" {
		env["warning"] = det.Warning
	}

	if det.Error != nil {
		// Lenient, exactly like resolveReadProject: reads answer from the basename
		// rather than erroring. project_source keeps det.Source — a broken
		// .engram/config.json is still a CONFIG answer, and relabelling it
		// "dir_basename" would send the agent looking for a missing config file
		// instead of the malformed one it has.
		env["project"] = projectpkg.DetectProject(dir)
		env["project_path"] = dir
		env["fallback"] = true
		env["error_hint"] = det.Error.Error()
		if errors.Is(det.Error, projectpkg.ErrInvalidConfig) || errors.Is(det.Error, projectpkg.ErrAmbiguousProject) {
			env["writes_blocked"] = true
			hints = append(hints, "reads fall back to the directory basename but writes (mem_save, mem_session_start, mem_session_summary) will REFUSE this directory — pass project explicitly")
		}
		if errors.Is(det.Error, projectpkg.ErrInvalidConfig) {
			hints = append(hints, "the .engram/config.json in this directory is present but unusable — fix its project_name, or pass project explicitly")
		}
	}
	if env["project_source"] == projectpkg.SourceDirBasename {
		env["fallback"] = true
		hints = append(hints, "the project name is only a directory basename (no .engram/config.json, git remote or git root) — pass project explicitly if that is not the name you want")
	}
	switch dirArg.Source {
	case dirSourceDaemonCwd:
		hints = append(hints, "no directory reached the daemon, so this is the DAEMON's own working directory and typically NOT your repo — restart the resident daemon on a current binary, set ENGRAM_CLIENT_DIR, or pass directory/project explicitly")
	case dirSourceCwdAlias:
		hints = append(hints, "this directory came from the \"cwd\" alias you supplied, not from 'engram connect' — if it is not the workspace you are actually in, every later call is filed under the wrong project")
	}

	applyPolicyBlock(store, env, &hints)
	setHints(env, hints)
	return env
}

// sourceMissingDirectory is the project_source reported when the resolved
// directory does not exist. It is deliberately NOT one of the projectpkg
// Source* constants: those all describe evidence found on disk, and there is
// none here.
const sourceMissingDirectory = "missing_directory"

// applyPolicyBlock sets writes_blocked when the resolved project's sync policy
// is "omitted", the one reason a write is refused that no amount of filesystem
// evidence can reveal: mem_save, mem_save_prompt and mem_session_summary check
// GetPolicy and return "capture refused" BEFORE writing anything, whether the
// project was detected or named explicitly. A nil store (direct unit tests)
// skips the check; a failed lookup is reported as a hint rather than swallowed.
func applyPolicyBlock(store *localstore.Store, env map[string]any, hints *[]string) {
	project, _ := env["project"].(string)
	if store == nil || strings.TrimSpace(project) == "" {
		return
	}
	pol, err := store.GetPolicy(project)
	if err != nil {
		*hints = append(*hints, fmt.Sprintf("could not read the sync policy for project %q (%v) — writes may still be refused", project, err))
		return
	}
	if pol == localstore.PolicyOmitted {
		env["writes_blocked"] = true
		*hints = append(*hints, fmt.Sprintf("project %q has policy \"omitted\": every write tool refuses it (capture refused) — "+
			"change it with 'engram projects policy %s local-only' or save under a different project", project, project))
	}
}

// setHints attaches the advisory hints array, omitting the key entirely when
// there is nothing to say (the shape the response-schema contract pins).
func setHints(env map[string]any, hints []string) {
	if len(hints) > 0 {
		env["hints"] = hints
	}
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
// This tool's optional "directory" argument is the model every other
// directory-aware tool now follows; `engram connect` fills it with the client's
// working directory when the caller left both it and "project" empty.
func handleSessionStart(store *localstore.Store) mcpserver.ToolHandlerFunc {
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

// resolveSaveProject resolves the project for a write tool call using the same
// precedence as handleSessionStart (mirrors the legacy predecessor's write-tool
// contract):
//
//  1. Explicit "project" argument if non-empty (caller override).
//  2. DetectProjectFull on the resolved directory (forwarded "directory"
//     argument, else the daemon's cwd — see resolveProjectDir): repo config /
//     git remote / git root / dir basename.
//
// ErrInvalidConfig and ErrAmbiguousProject are surfaced as tool errors exactly
// like handleSessionStart so agents get actionable feedback on misconfigured
// repos (faithful to the legacy predecessor's handleSave precedence) — they
// fire against the resolved directory, so a forwarded directory is diagnosed
// exactly like a cwd would be.
//
// Conflict detection (explicit project vs store's known projects) is DEFERRED
// to a future PR.
func resolveSaveProject(store *localstore.Store, explicitProject, directory string) (string, *mcp.CallToolResult) {
	if strings.TrimSpace(explicitProject) != "" {
		return strings.TrimSpace(explicitProject), nil
	}

	dir := resolveProjectDir(directory)
	det := projectpkg.DetectProjectFull(dir)
	if det.Error != nil {
		switch {
		case errors.Is(det.Error, projectpkg.ErrInvalidConfig):
			return "", mcp.NewToolResultError("mem_save: project resolution: " + det.Error.Error())
		case errors.Is(det.Error, projectpkg.ErrAmbiguousProject):
			msg := "mem_save: project resolution: " + det.Error.Error()
			if len(det.AvailableProjects) > 0 {
				msg += " (candidates: " + strings.Join(det.AvailableProjects, ", ") +
					"); pass project= explicitly or supply a more specific directory"
			}
			return "", mcp.NewToolResultError(msg)
		default:
			// Other errors (e.g. no .git): fall back to basename.
			return projectpkg.DetectProject(dir), nil
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
//
// embedLoop (may be nil): after a successful save, embedLoop.Trigger() is called
// nil-safely so the backfill loop picks up the new row without waiting for the
// next periodic 60s tick. The Trigger is non-blocking (coalesced, size-1 channel).
func handleSave(store *localstore.Store, loop *syncer.Loop, embedLoop *embedding.Loop, gated embedding.EmbeddingProvider, writerID string, activity *SessionActivity) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
		dirArg := readDirectoryArg(args)
		if dirArg.Err != nil {
			return dirArg.toolError("mem_save"), nil
		}
		directory := dirArg.Directory

		// capture_prompt defaults to true when absent; explicit false disables it.
		capturePrompt := true
		if v, ok := args["capture_prompt"].(bool); ok {
			capturePrompt = v
		}

		project, toolErr := resolveSaveProject(store, explicitProject, directory)
		if toolErr != nil {
			return toolErr, nil
		}

		// Default session_id to "manual-save-{project}" when omitted, using the
		// FINAL resolved project (auto-detected or explicit) — matches the tool
		// description's documented default.
		if sessionID == "" {
			sessionID = fmt.Sprintf("manual-save-%s", project)
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
		// Nudge the embedding backfill loop so the new row is embedded promptly
		// without waiting for the next 60s periodic tick. Nil-safe: no-op when
		// no embedding provider is configured (embedLoop is nil).
		embedLoop.Trigger()

		// Post-save conflict candidate detection (REQ-001).
		// Errors are logged to stderr and swallowed — detection failure MUST NOT fail
		// the save. The save already succeeded; candidate detection is advisory only.
		// The cosine paraphrase pass is wired through the SAME gated provider as
		// search and backfill (gated/omitted projects: the embed errors and the
		// pass silently degrades to FTS-only candidates). A bounded context keeps
		// a stalled local sidecar from hanging the save path.
		candCtx, candCancel := context.WithTimeout(ctx, 10*time.Second)
		candidates, candErr := store.FindCandidates(candCtx, result.ID, localstore.CandidateOptions{
			// nil BM25Floor → store default (-2.0); nil/0 Limit → store default (3).
			EmbedFn:   gated.Embed,
			EmbedDims: gated.Dimensions(),
		})
		candCancel()
		if candErr != nil {
			fmt.Fprintf(os.Stderr, "engram: FindCandidates error (non-fatal): %v\n", candErr)
		}

		msg := fmt.Sprintf("Memory saved: %q (id=%d, project=%q)", title, result.ID, project)

		// Save-time name-drift warning (Feature 2): if the resolved project is not
		// an exact match to an existing one but IS a near-variant, append a
		// non-blocking note. NEVER blocks the save; any error degrades to no note.
		driftNote := ""
		if existing, derr := store.DistinctProjects(); derr == nil {
			if near, ok := nearVariantProject(project, existing); ok {
				driftNote = fmt.Sprintf(
					"\nnote: project %q looks close to existing %q — pass an explicit project, or run 'engram projects consolidate'",
					project, near,
				)
			}
		}

		if len(candidates) > 0 {
			// Build judgment envelope — faithful to the legacy predecessor's
			// handleSave envelope format.
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
			b.WriteString(driftNote)
			return mcp.NewToolResultText(b.String()), nil
		}

		return mcp.NewToolResultText(msg + driftNote), nil
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

		// Surface the review/staleness status inline so an agent sees whether a
		// memory should be re-verified before trusting it (Feature 1). Best-effort:
		// a status lookup error never blocks returning the observation.
		statusLine := ""
		if st, serr := store.ReviewStatusForID(id); serr == nil && st != "" {
			statusLine = fmt.Sprintf("\nStatus: %s", st)
		}

		text := fmt.Sprintf("#%d [%s] %s\n%s\nSession: %s\nProject: %s\nScope: %s%s%s\nCreated: %s",
			id, rec.Type, rec.Title,
			rec.Content,
			rec.SessionID,
			rec.Project,
			rec.Scope,
			topic,
			statusLine,
			rec.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		)

		return mcp.NewToolResultText(text), nil
	}
}

// toolObservationID decodes a required numeric "id" argument into an int64,
// returning a caller-facing error string when it is missing or not a usable
// positive integer. The MCP SDK delivers every JSON number as a float64, which
// cannot represent every int64 above 2^53 — and float64(math.MaxInt64) rounds UP
// to 2^63, so the exact boundary must be rejected or int64() overflows negative.
func toolObservationID(args map[string]any, tool string) (int64, string) {
	raw, ok := args["id"]
	if !ok {
		return 0, tool + ": id is required"
	}
	f, ok := raw.(float64)
	if !ok {
		return 0, tool + ": id must be a number"
	}
	if f != math.Trunc(f) || f <= 0 || f >= float64(math.MaxInt64) {
		return 0, tool + ": id must be a positive integer"
	}
	return int64(f), ""
}

// handlePin returns the handler for mem_pin (pinned=true) and mem_unpin
// (pinned=false) — one implementation, since the two differ only in the value
// they write.
//
// Pinning is LOCAL-ONLY: it writes the memories.pinned column directly, enqueues
// no mutation, and never reaches central. A pin is "what I want in front of me
// on THIS machine", which has no business reordering a teammate's context.
//
// An id that names no LIVE row is an error, not a silent success: telling the
// caller a deleted memory is now pinned would leave it waiting for something
// that can never surface.
func handlePin(store *localstore.Store, pinned bool) mcpserver.ToolHandlerFunc {
	tool := "mem_unpin"
	if pinned {
		tool = "mem_pin"
	}

	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, errMsg := toolObservationID(req.GetArguments(), tool)
		if errMsg != "" {
			return mcp.NewToolResultError(errMsg), nil
		}

		state, err := store.SetPinned(id, pinned)
		if err != nil {
			if errors.Is(err, localstore.ErrObservationNotFound) {
				return mcp.NewToolResultError(fmt.Sprintf("%s: observation #%d not found", tool, id)), nil
			}
			return mcp.NewToolResultError(fmt.Sprintf("%s: %s", tool, err)), nil
		}

		// Read the row back for its sync_id so the response identifies the memory
		// the same way every other tool does. A read-back failure does not undo a
		// successful pin — report the state we know rather than an error.
		syncID := ""
		if rec, rerr := store.GetObservation(id); rerr == nil {
			syncID = rec.SyncID
		}

		word := "unpinned"
		if state {
			word = "pinned"
		}
		out, err := json.Marshal(map[string]any{
			"result":  fmt.Sprintf("Memory #%d %s", id, word),
			"id":      id,
			"sync_id": syncID,
			"pinned":  state,
		})
		if err != nil {
			// Unreachable: the map holds only strings, an int64 and a bool.
			return mcp.NewToolResultText(fmt.Sprintf("Memory #%d %s", id, word)), nil
		}
		return mcp.NewToolResultText(string(out)), nil
	}
}

// handleUpdate returns the handler for mem_update. It edits a live observation
// in place by ID, filling any omitted field from the current record, then writes
// a versioned OpUpsert via store.UpdateMemory (materialized + enqueued for push).
// The sync and embedding-backfill triggers are nil-safe (local-only / no-provider).
func handleUpdate(store *localstore.Store, loop *syncer.Loop, embedLoop *embedding.Loop, writerID string) mcpserver.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		// id parsing mirrors mem_get_observation: the MCP SDK delivers JSON numbers
		// as float64, so reject non-integer / out-of-range values explicitly.
		rawID, ok := args["id"]
		if !ok {
			return mcp.NewToolResultError("mem_update: id is required"), nil
		}
		idFloat, ok := rawID.(float64)
		if !ok {
			return mcp.NewToolResultError("mem_update: id must be a number"), nil
		}
		if idFloat != math.Trunc(idFloat) || idFloat <= 0 || idFloat >= float64(math.MaxInt64) {
			return mcp.NewToolResultError("mem_update: id must be a positive integer"), nil
		}
		id := int64(idFloat)

		title, _ := args["title"].(string)
		content, _ := args["content"].(string)
		typ, _ := args["type"].(string)
		if strings.TrimSpace(title) == "" && strings.TrimSpace(content) == "" && strings.TrimSpace(typ) == "" {
			return mcp.NewToolResultError("mem_update: provide at least one of title, content, or type to change"), nil
		}

		// Fetch the current record to confirm it is live and to fill omitted fields.
		rec, err := store.GetObservation(id)
		if err != nil {
			if errors.Is(err, localstore.ErrObservationNotFound) {
				return mcp.NewToolResultError(fmt.Sprintf("mem_update: observation #%d not found", id)), nil
			}
			return mcp.NewToolResultError(fmt.Sprintf("mem_update: %s", err)), nil
		}
		if strings.TrimSpace(title) == "" {
			title = rec.Title
		}
		if strings.TrimSpace(content) == "" {
			content = rec.Content
		}
		// typ "" → UpdateMemory preserves the existing type.

		updated, err := store.UpdateMemory(id, title, content, typ, writerID)
		if err != nil {
			if errors.Is(err, localstore.ErrObservationNotFound) {
				return mcp.NewToolResultError(fmt.Sprintf("mem_update: observation #%d not found", id)), nil
			}
			return mcp.NewToolResultError(fmt.Sprintf("mem_update: failed to update: %s", err)), nil
		}

		// Propagate to central and re-embed the changed content (both nil-safe).
		triggerSync(loop)
		embedLoop.Trigger()

		topic := ""
		if updated.TopicKey != nil {
			topic = fmt.Sprintf("\nTopic: %s", *updated.TopicKey)
		}
		return mcp.NewToolResultText(fmt.Sprintf(
			"Updated memory #%d [%s] %s\nVersion: %d\nProject: %s%s",
			id, updated.Type, updated.Title, updated.Version, updated.Project, topic,
		)), nil
	}
}

// handleSuggestTopicKey returns the handler for mem_suggest_topic_key. It is a
// pure, read-only suggestion (no store access), so independent sessions converge
// on the same deterministic topic_key for the same input.
func handleSuggestTopicKey() mcpserver.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		title, _ := args["title"].(string)
		if strings.TrimSpace(title) == "" {
			return mcp.NewToolResultError("mem_suggest_topic_key: title is required"), nil
		}
		typ, _ := args["type"].(string)
		content, _ := args["content"].(string)
		return mcp.NewToolResultText(topickey.Suggest(typ, title, content)), nil
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
func handleSessionSummary(store *localstore.Store, loop *syncer.Loop, writerID string) mcpserver.ToolHandlerFunc {
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
			directory := dirArg.Directory
			var toolErr *mcp.CallToolResult
			// Returns explicitProject verbatim when set; otherwise detects from the
			// forwarded directory / daemon cwd and may hard-error.
			project, toolErr = resolveSaveProject(store, explicitProject, directory)
			if toolErr != nil {
				return toolErr, nil
			}
		}

		// Default session_id to "manual-save-{project}" when omitted, using the
		// FINAL resolved project — matches the tool description's documented
		// default. Applied AFTER the session-lookup above so an explicit empty
		// session_id still resolves the project from cwd rather than a session row.
		if sessionID == "" {
			sessionID = fmt.Sprintf("manual-save-%s", project)
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
		dirArg := readDirectoryArg(args)
		if dirArg.Err != nil {
			return dirArg.toolError("mem_search"), nil
		}
		directory := dirArg.Directory
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

		project := resolveReadProject(explicitProject, directory)
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
		dirArg := readDirectoryArg(args)
		if dirArg.Err != nil {
			return dirArg.toolError("mem_context"), nil
		}
		directory := dirArg.Directory
		scope, _ := args["scope"].(string)

		project := resolveReadProject(explicitProject, directory)
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
		// Reject out-of-range values (faithful to the legacy predecessor) so the
		// agent heuristic (e.g. "confidence < 0.7 → surface to user") can never be
		// corrupted.
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
		dirArg := readDirectoryArg(args)
		if dirArg.Err != nil {
			return dirArg.toolError("mem_save_prompt"), nil
		}
		project, toolErr := resolveSaveProject(store, explicitProject, dirArg.Directory)
		if toolErr != nil {
			return toolErr, nil
		}

		// Default session_id to "manual-save-{project}" when omitted, using the
		// FINAL resolved project (auto-detected or explicit) — matches the tool
		// description's documented default.
		if sessionID == "" {
			sessionID = fmt.Sprintf("manual-save-%s", project)
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

// handleMemSimilar returns the handler for mem_similar.
//
// It looks up the stored embedding for the source row (by sync_id), then runs
// a cosine top-K against all other live rows in the same project, returning
// the nearest neighbours.
//
// Error cases:
//   - sync_id not found in the store → tool error
//   - source row has no embedding vector → tool error (clear message)
//   - no embedding provider configured (dims=0) → tool error
//
// Project policy for the LOOKUP (reading vectors) is LOCAL — no text crosses a
// provider boundary, so the gate is not involved in the similarity scan itself.
// The stored vectors are already derived data on this node.
func handleMemSimilar(store *localstore.Store, gated embedding.EmbeddingProvider) mcpserver.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		syncID, _ := args["sync_id"].(string)
		syncID = strings.TrimSpace(syncID)
		if syncID == "" {
			return mcp.NewToolResultError("mem_similar: sync_id is required"), nil
		}

		explicitProject, _ := args["project"].(string)

		limit := 5
		if raw, ok := args["limit"].(float64); ok && raw > 0 {
			limit = int(raw)
			if limit > 20 {
				limit = 20
			}
		}

		dims := gated.Dimensions()
		if dims <= 0 {
			return mcp.NewToolResultError("mem_similar: no embedding provider configured; mem_similar requires vectors"), nil
		}

		// Retrieve the source row's stored embedding.
		srcVec, err := localstore.GetEmbeddingBySyncID(store.RawDB(), syncID, dims)
		if err != nil {
			if errors.Is(err, localstore.ErrNoEmbedding) {
				return mcp.NewToolResultError(fmt.Sprintf("mem_similar: observation %q has no embedding vector yet; wait for the backfill loop or check embedding_provider config", syncID)), nil
			}
			return mcp.NewToolResultError(fmt.Sprintf("mem_similar: %s", err)), nil
		}

		// Resolve the project for scoping: explicit arg, or source row's project.
		project := strings.TrimSpace(explicitProject)
		if project == "" {
			// Look up the source row to get its project.
			if rec, recErr := store.FindBySyncID(syncID); recErr == nil && rec != nil {
				project = rec.Project
			}
		}

		// Fetch all embeddings scoped to project.
		rows, selErr := localstore.SelectVectors(store.RawDB(), project, localstore.SearchFilter{}, dims)
		if selErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("mem_similar: vector scan error: %s", selErr)), nil
		}

		// Exclude the source row itself.
		filtered := rows[:0]
		for _, r := range rows {
			if r.SyncID() != syncID {
				filtered = append(filtered, r)
			}
		}

		candidates := localstore.CosineTopK(srcVec, filtered, limit)
		if len(candidates) == 0 {
			return mcp.NewToolResultText(fmt.Sprintf("No similar memories found for sync_id %q", syncID)), nil
		}

		var b strings.Builder
		fmt.Fprintf(&b, "Found %d similar memories (cosine similarity):\n\n", len(candidates))
		for i, c := range candidates {
			fmt.Fprintf(&b, "[%d] sync_id=%s score=%.4f\n", i+1, c.SyncID(), c.Score())
		}
		return mcp.NewToolResultText(b.String()), nil
	}
}

// parseObservationID converts a single MCP JSON number into a positive int64
// observation ID, mirroring the rigor in mem_get_observation / mem_update: the
// MCP SDK delivers all JSON numbers as float64, which cannot exactly represent
// every int64 above 2^53, so non-integer, non-positive, and out-of-range values
// are rejected. Returns a descriptive error suitable for the tool error text.
func parseObservationID(raw any) (int64, error) {
	f, ok := raw.(float64)
	if !ok {
		return 0, fmt.Errorf("must be a number")
	}
	if f != math.Trunc(f) || f <= 0 || f >= float64(math.MaxInt64) {
		return 0, fmt.Errorf("must be a positive integer")
	}
	return int64(f), nil
}

// handleReview returns the handler for mem_review. action="list" lists memories
// by review status; action="mark_reviewed" resets the staleness clock on the
// given ids (or the row resolved from topic_key). mark_reviewed is a LOCAL-ONLY
// write — it never enqueues an outbox entry, so no sync trigger is needed.
func handleReview(store *localstore.Store) mcpserver.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		action, _ := args["action"].(string)
		action = strings.TrimSpace(strings.ToLower(action))
		dirArg := readDirectoryArg(args)
		if dirArg.Err != nil {
			return dirArg.toolError("mem_review"), nil
		}
		directory := dirArg.Directory
		switch action {
		case "list":
			explicitProject, _ := args["project"].(string)
			project := resolveReadProject(explicitProject, directory)

			status, _ := args["status"].(string)

			limit := 50
			if raw, ok := args["limit"].(float64); ok && raw > 0 {
				limit = int(raw)
			}

			rows, err := store.ListForReview(status, project, limit)
			if err != nil {
				return mcp.NewToolResultError("mem_review: " + err.Error()), nil
			}
			if len(rows) == 0 {
				return mcp.NewToolResultText("No memories match the requested review status."), nil
			}

			var b strings.Builder
			fmt.Fprintf(&b, "Found %d memories:\n\n", len(rows))
			for _, r := range rows {
				reviewAfter := "—"
				if r.ReviewAfter != nil {
					reviewAfter = r.ReviewAfter.UTC().Format("2006-01-02T15:04:05Z")
				}
				fmt.Fprintf(&b, "#%d [%s] %s\n    project: %s | status: %s | review_after: %s\n",
					r.ID, r.Type, r.Title, r.Project, r.Status, reviewAfter)
			}
			return mcp.NewToolResultText(b.String()), nil

		case "mark_reviewed":
			// Resolve the target ids: explicit ids[] OR a topic_key (resolved to its
			// current observation's id). Exactly one source must be supplied.
			var ids []int64

			if rawIDs, ok := args["ids"].([]any); ok && len(rawIDs) > 0 {
				for i, raw := range rawIDs {
					id, err := parseObservationID(raw)
					if err != nil {
						return mcp.NewToolResultError(fmt.Sprintf("mem_review: ids[%d] %s", i, err)), nil
					}
					ids = append(ids, id)
				}
			}

			topicKey, _ := args["topic_key"].(string)
			topicKey = strings.TrimSpace(topicKey)
			if topicKey != "" {
				explicitProject, _ := args["project"].(string)
				project := resolveReadProject(explicitProject, directory)
				id, err := store.IDByTopicKey(topicKey, project, "project")
				if err != nil {
					if errors.Is(err, localstore.ErrObservationNotFound) {
						return mcp.NewToolResultError(fmt.Sprintf("mem_review: no live memory for topic_key %q in project %q", topicKey, project)), nil
					}
					return mcp.NewToolResultError("mem_review: " + err.Error()), nil
				}
				ids = append(ids, id)
			}

			if len(ids) == 0 {
				return mcp.NewToolResultError("mem_review: mark_reviewed requires ids (number array) or topic_key"), nil
			}

			n, err := store.MarkReviewed(ids)
			if err != nil {
				return mcp.NewToolResultError("mem_review: " + err.Error()), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("Marked %d memory(ies) as reviewed.", n)), nil

		default:
			return mcp.NewToolResultError("mem_review: action is required — must be \"list\" or \"mark_reviewed\""), nil
		}
	}
}

// handleMergeProjects returns the handler for mem_merge_projects. It renames a
// source project's local rows to the target project name (LOCAL-ONLY — no sync
// propagation). from and to are both required.
func handleMergeProjects(store *localstore.Store) mcpserver.ToolHandlerFunc {
	return func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		from, _ := args["from"].(string)
		from = strings.TrimSpace(from)
		if from == "" {
			return mcp.NewToolResultError("mem_merge_projects: from is required"), nil
		}
		to, _ := args["to"].(string)
		to = strings.TrimSpace(to)
		if to == "" {
			return mcp.NewToolResultError("mem_merge_projects: to is required"), nil
		}

		mem, pol, cur, err := store.MergeProject(from, to)
		if err != nil {
			return mcp.NewToolResultError("mem_merge_projects: " + err.Error()), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf(
			"Merged project %q into %q: %d memories, %d policy row(s), %d pull-cursor(s) moved.",
			from, to, mem, pol, cur,
		)), nil
	}
}

// nearVariantProject reports whether candidate is a near-variant — but NOT an
// exact match — of any project in existing. It returns the first matching
// existing project name. Two names are near-variants when, after stripping case
// and separators (-, _, spaces), they are equal, OR their normalized
// Levenshtein distance is <= 2. An exact match (case-sensitive equality) is
// never a drift warning (it is the same project), so it returns ok=false.
func nearVariantProject(candidate string, existing []string) (string, bool) {
	candNorm := normalizeForDrift(candidate)
	if candNorm == "" {
		return "", false
	}
	for _, e := range existing {
		if e == candidate {
			return "", false // exact match — same project, no drift
		}
		// High-precision: warn ONLY on case/separator-only differences (my-app vs
		// myapp vs My_App), which collapse to the same normalized key. A fuzzy
		// edit-distance match was deliberately dropped — for short or intentionally
		// similar names (api/app, cli/ci, service-a/service-b) it false-positived
		// and would train users to ignore the note.
		if normalizeForDrift(e) == candNorm {
			return e, true
		}
	}
	return "", false
}

// normalizeForDrift lowercases s and removes separators (-, _, spaces) so that
// case/separator-only differences ("my-app" vs "myapp" vs "My_App") collapse to
// the same key for the near-variant comparison.
func normalizeForDrift(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	r := strings.NewReplacer("-", "", "_", "", " ", "")
	return r.Replace(s)
}

// (levenshtein/min3 removed: the name-drift warning is now case/separator-only,
// see nearVariantProject.)
