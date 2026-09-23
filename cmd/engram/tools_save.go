package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
	projectpkg "github.com/mariesqu/engram/internal/project"
	"github.com/mariesqu/engram/internal/syncer"
	"github.com/mariesqu/engram/internal/topickey"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func registerSaveTools(srv *mcpserver.MCPServer, store *localstore.Store, loop *syncer.Loop, embedLoop *embedding.Loop, gated embedding.EmbeddingProvider, writerID string, activity *SessionActivity, daemonCwdIsWorkspace bool) {
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
		handleSave(store, loop, embedLoop, gated, writerID, activity, daemonCwdIsWorkspace),
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
		handleSavePrompt(store, loop, writerID, activity, daemonCwdIsWorkspace),
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
// Before any detection runs it applies the three refusals mem_current_project
// advertises as writes_blocked and nothing used to enforce:
//
//   - a RELATIVE directory (dirSourceRelativePath): filepath.Abs resolves it
//     against the SHARED daemon's cwd, so the memory would be filed under
//     whatever folder the autostart or tray happened to launch from;
//   - no directory at all (dirSourceDaemonCwd): the project would be detected
//     from the daemon's OWN working directory — %APPDATA%\engram for the
//     resident daemon, see spawnWorkingDir — not the caller's; SKIPPED when
//     daemonCwdIsWorkspace is true, i.e. this is a per-client `engram daemon
//     --transport stdio` (README.md's documented setup), whose cwd genuinely
//     IS the caller's workspace — see registerTools;
//   - a directory that is not on this machine: detection derives a basename
//     from any string, so a typo'd path silently creates a brand-new project.
//
// All three are skipped when the caller named a project. An explicit name is
// the remedy every writes_blocked hint offers, and honouring it here is what
// makes that advice true.
//
// tool is the caller's tool name, used verbatim in the error text: an agent
// that reads "mem_save: …" after calling mem_session_summary learns the wrong
// thing about which call failed.
//
// Conflict detection (explicit project vs store's known projects) is DEFERRED
// to a future PR.
func resolveSaveProject(store *localstore.Store, tool, explicitProject string, dirArg directoryArg, daemonCwdIsWorkspace bool) (string, *mcp.CallToolResult) {
	if strings.TrimSpace(explicitProject) != "" {
		return strings.TrimSpace(explicitProject), nil
	}
	if dirArg.Relative {
		return "", dirArg.relativeError(tool)
	}
	if dirArg.Source == dirSourceDaemonCwd && !daemonCwdIsWorkspace {
		return "", dirArg.daemonCwdError(tool)
	}

	dir := resolveProjectDir(dirArg.Directory)
	if !directoryExists(dir) {
		return "", missingDirectoryError(tool, dir)
	}
	det := projectpkg.DetectProjectFull(dir)
	if det.Error != nil {
		switch {
		case errors.Is(det.Error, projectpkg.ErrInvalidConfig):
			return "", mcp.NewToolResultError(tool + ": project resolution: " + det.Error.Error())
		case errors.Is(det.Error, projectpkg.ErrAmbiguousProject):
			msg := tool + ": project resolution: " + det.Error.Error()
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
func handleSave(store *localstore.Store, loop *syncer.Loop, embedLoop *embedding.Loop, gated embedding.EmbeddingProvider, writerID string, activity *SessionActivity, daemonCwdIsWorkspace bool) mcpserver.ToolHandlerFunc {
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

		// capture_prompt defaults to true when absent; explicit false disables it.
		capturePrompt := true
		if v, ok := args["capture_prompt"].(bool); ok {
			capturePrompt = v
		}

		project, toolErr := resolveSaveProject(store, "mem_save", explicitProject, dirArg, daemonCwdIsWorkspace)
		if toolErr != nil {
			return toolErr, nil
		}

		// Default session_id to "manual-save-{project}" when omitted, using the
		// FINAL resolved project (auto-detected or explicit) — matches the tool
		// description's documented default. The id is minted from the store's own
		// constant so mem_doctor can recognise it instead of reporting it as an
		// orphaned session (see localstore.ManualSaveSessionPrefix).
		if sessionID == "" {
			sessionID = localstore.DefaultManualSessionID(project)
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

// handleSavePrompt returns the handler for mem_save_prompt. It persists the
// prompt via AddPrompt (which enqueues an outbox entry for central push) and
// records it in the in-memory SessionActivity so that a subsequent mem_save
// with capture_prompt=true can auto-capture it without a re-insert (dedup).
func handleSavePrompt(store *localstore.Store, loop *syncer.Loop, writerID string, activity *SessionActivity, daemonCwdIsWorkspace bool) mcpserver.ToolHandlerFunc {
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
		project, toolErr := resolveSaveProject(store, "mem_save_prompt", explicitProject, dirArg, daemonCwdIsWorkspace)
		if toolErr != nil {
			return toolErr, nil
		}

		// Default session_id to "manual-save-{project}" when omitted, using the
		// FINAL resolved project (auto-detected or explicit) — matches the tool
		// description's documented default (localstore.ManualSaveSessionPrefix).
		if sessionID == "" {
			sessionID = localstore.DefaultManualSessionID(project)
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
