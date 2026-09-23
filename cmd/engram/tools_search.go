package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/mariesqu/engram/internal/controlapi"
	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/syncer"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func registerSearchTools(srv *mcpserver.MCPServer, store *localstore.Store, loop *syncer.Loop, embedLoop *embedding.Loop, gated embedding.EmbeddingProvider, writerID string, activity *SessionActivity, daemonCwdIsWorkspace bool) {
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
				mcp.Description("Filter by type: tool_use, file_change, command, file_read, search, manual, decision, architecture, bugfix, pattern, config, discovery, learning"),
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
			mcp.WithNumber("offset",
				mcp.Description("Skip the first N results — page 2 of limit=10 is offset=10. Must be a non-negative integer; default 0. Paging past the end returns no results rather than page one."),
			),
			mcp.WithString("created_from",
				mcp.Description(`Only return memories created on or after this instant. RFC3339 ("2024-06-01T09:00:00Z") or a plain date ("2024-06-01", read as UTC midnight).`),
			),
			mcp.WithString("created_to",
				mcp.Description(`Only return memories created on or before this instant. RFC3339 ("2024-06-30T23:59:59Z") or a plain date ("2024-06-30", which covers the WHOLE day). Must not be earlier than created_from.`),
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

// toolObservationID decodes a REQUIRED numeric "id" argument into an int64,
// returning a caller-facing error string prefixed with the tool name.
//
// The numeric validation itself is parseObservationID's — this wrapper adds only
// the two things that are specific to "id is a required argument of THIS tool":
// the missing-key check and the tool prefix. Duplicating the float64 rules here
// (which is what this used to do) means two copies of a subtle boundary check —
// float64(math.MaxInt64) rounds UP to 2^63, so the exact boundary must be
// rejected or int64() overflows negative — that can drift apart silently.
func toolObservationID(args map[string]any, tool string) (int64, string) {
	raw, ok := args["id"]
	if !ok {
		return 0, tool + ": id is required"
	}
	id, err := parseObservationID(raw)
	if err != nil {
		return 0, tool + ": id " + err.Error()
	}
	return id, ""
}

// toolSearchOffset decodes mem_search's optional "offset" argument. Absent is 0
// (page one). Present-but-unusable is an ERROR rather than a silent 0: answering
// "give me rows 50-59" with rows 0-9 is indistinguishable from a correct answer
// on the caller's side, and an agent paging through results would loop forever
// on page one without ever being told why.
func toolSearchOffset(args map[string]any) (int, string) {
	raw, ok := args["offset"]
	if !ok {
		return 0, ""
	}
	f, ok := raw.(float64)
	if !ok {
		return 0, "mem_search: offset must be a number"
	}
	// A fractional or negative offset is a caller bug, not a page.
	if f != math.Trunc(f) || f < 0 {
		return 0, "mem_search: offset must be a non-negative integer"
	}
	// The ceiling gets its OWN message. int is 32-bit on some builds, so the
	// bound is real — but "must be a non-negative integer" told a caller who
	// passed a well-formed integer to pass an integer, which is advice they
	// cannot act on. Naming the actual problem is the difference between a caller
	// that shrinks the offset and one that retries the same value forever.
	if f > float64(math.MaxInt32) {
		return 0, fmt.Sprintf("mem_search: offset is too large (max %d)", math.MaxInt32)
	}
	return int(f), ""
}

// toolSearchTime decodes one of mem_search's optional date-bound arguments,
// accepting RFC3339 or a bare "YYYY-MM-DD" date. The zero time.Time means the
// bound is unset.
//
// endOfDay makes a DATE-only value cover the whole day, and it is what the
// "created_to" bound passes: SearchFilter's bounds are inclusive, so reading
// "2024-06-30" as UTC midnight would silently exclude everything saved that day
// — the exact day the caller asked to include. The same helper backs the web UI's
// filter bar (controlapi.InclusiveDayEnd), so the two surfaces cannot drift into
// different meanings for the same date string.
//
// A malformed value is an error, not an ignored filter. Silently dropping the
// bound would answer a question about last week with the entire corpus, and the
// caller has no way to see that it happened.
func toolSearchTime(args map[string]any, key string, endOfDay bool) (time.Time, string) {
	raw, ok := args[key]
	if !ok {
		return time.Time{}, ""
	}
	str, ok := raw.(string)
	if !ok {
		return time.Time{}, "mem_search: " + key + " must be a string"
	}
	str = strings.TrimSpace(str)
	if str == "" {
		return time.Time{}, ""
	}

	if t, err := time.Parse(time.RFC3339, str); err == nil {
		return t.UTC(), ""
	}
	if t, err := time.Parse("2006-01-02", str); err == nil {
		if endOfDay {
			return controlapi.InclusiveDayEnd(t.UTC()), ""
		}
		return t.UTC(), ""
	}
	return time.Time{}, "mem_search: " + key + ` must be RFC3339 ("2024-06-01T09:00:00Z") or a date ("2024-06-01")`
}

// handleSearch returns the handler for mem_search. It performs a search with
// optional type/scope/mode filters, date bounds and offset paging, using the
// LENIENT read-project policy so a search never hard-errors on an ambiguous or
// misconfigured cwd.
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

		// offset / created_from / created_to are STRICT where limit is lenient, and
		// the difference is deliberate: a bad limit still answers the caller's
		// question (with a different number of rows), while a bad offset or date
		// silently answers a DIFFERENT question — page one instead of page five, or
		// the whole corpus instead of last week. Those are the answers an agent
		// cannot tell apart from the right one, so they are refused out loud.
		offset, errMsg := toolSearchOffset(args)
		if errMsg != "" {
			return mcp.NewToolResultError(errMsg), nil
		}
		createdFrom, errMsg := toolSearchTime(args, "created_from", false)
		if errMsg != "" {
			return mcp.NewToolResultError(errMsg), nil
		}
		createdTo, errMsg := toolSearchTime(args, "created_to", true)
		if errMsg != "" {
			return mcp.NewToolResultError(errMsg), nil
		}
		// An inverted window is empty by construction: the store ANDs the two
		// bounds, so this search can only ever return nothing. Answering "no
		// memories found" would be true and useless — the caller would go on
		// believing the corpus is empty for that window instead of seeing that
		// they swapped their arguments.
		if !createdFrom.IsZero() && !createdTo.IsZero() && createdFrom.After(createdTo) {
			return mcp.NewToolResultError("mem_search: created_from is after created_to"), nil
		}

		project := resolveReadProject(explicitProject, directory)
		// REQ-391: personal-scope memories are NOT project-scoped. When scope is
		// personal and no explicit project was given, search across ALL projects so
		// personal memories saved under any project remain visible.
		if strings.EqualFold(strings.TrimSpace(scope), "personal") && strings.TrimSpace(explicitProject) == "" {
			project = ""
		}

		results, degradation, err := store.SearchMemoriesFiltered(query, project, limit, localstore.SearchFilter{
			Type:        typ,
			Scope:       scope,
			Mode:        mode,
			Offset:      offset,
			CreatedFrom: createdFrom,
			CreatedTo:   createdTo,
		})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("mem_search: search error: %s. Try simpler keywords.", err)), nil
		}

		if len(results) == 0 {
			return mcp.NewToolResultText(fmt.Sprintf("No memories found for: %q", query)), nil
		}

		var b strings.Builder
		// Page one renders exactly as it always has. Past it the header names the
		// row range and the numbering continues from the offset, because "Found 10
		// memories" over items [1]–[10] describes page one, page three and page
		// nine identically — and an agent walking pages has no other way to tell
		// which one it is holding. The count stays the count of THIS page; the
		// range is what says where the page sits.
		if offset > 0 {
			fmt.Fprintf(&b, "Found %d memories (rows %d–%d):\n\n",
				len(results), offset+1, offset+len(results))
		} else {
			fmt.Fprintf(&b, "Found %d memories:\n\n", len(results))
		}
		anyTruncated := false
		for i, r := range results {
			preview := r.Content
			const previewLen = 300
			if len([]rune(r.Content)) > previewLen {
				anyTruncated = true
				preview = string([]rune(r.Content)[:previewLen]) + " [preview]"
			}
			fmt.Fprintf(&b, "[%d] #%d (%s) — %s\n    %s\n    project: %s | scope: %s\n",
				offset+i+1, r.ID, r.Type, r.Title,
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
