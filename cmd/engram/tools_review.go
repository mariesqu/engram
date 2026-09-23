package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/syncer"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func registerReviewTools(srv *mcpserver.MCPServer, store *localstore.Store, loop *syncer.Loop, embedLoop *embedding.Loop, gated embedding.EmbeddingProvider, writerID string, activity *SessionActivity, daemonCwdIsWorkspace bool) {
	// ── mem_review ────────────────────────────────────────────────────────────
	srv.AddTool(
		mcp.NewTool("mem_review",
			mcp.WithDescription(`Review the lifecycle/staleness of saved memories so stale architecture/decision notes are VERIFIED before being trusted, not trusted blindly.

action="list": list memories by review status — status filter is one of:
  needs_review (default) | active | expired | all. Optional project and limit.
  Returns id, title, type, project, status, review_after.

action="mark_reviewed": reset the staleness clock on memories you have verified.
  Provide ids (a number array, max 200 per call) OR a topic_key (resolves to its
  current observation). The new due date is recomputed from the memory's TYPE —
  decision +6 months, policy +12, preference +3, anything else + the staleness
  window. Returns the count updated.

Status is computed at read time: a memory is "needs_review" once past its review_after (set for decision/policy/preference, and the clock runs from the LAST SAVE OR REVISION — rewriting a memory restarts it, exactly as marking it reviewed does) or, when it has none, once it ages past the staleness window; "expired" once past its expires_at; else "active". mark_reviewed is a LOCAL-ONLY write (it does not sync).`),
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
				mcp.Description("mark_reviewed: observation IDs (numbers) to mark as reviewed — at most 200 per call"),
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
}

// reviewIDsPerCall caps mark_reviewed's ids[] array, mirroring the 200-row
// ceiling ListForReview puts on its own limit.
const reviewIDsPerCall = 200

// handleReview returns the handler for mem_review. action="list" lists memories
// by review status; action="mark_reviewed" resets the staleness clock on the
// given ids (or the row resolved from topic_key). mark_reviewed is a LOCAL-ONLY
// write — it never enqueues an outbox entry, so no sync trigger is needed.
//
// Why it resolves its project the lenient READ way (resolveReadProject) even
// though mark_reviewed writes. Every other write tool goes through
// resolveSaveProject, which REFUSES a relative directory, because a wrong
// project there invents a name and files a new memory under it — a fabrication
// nobody asked for. mem_review cannot do that: both project uses are LOOKUPS
// scoped by project (ListForReview filters by it, IDByTopicKey resolves a topic
// inside it), so a wrong project finds nothing and the call is a no-op with an
// empty list or a "no live memory for topic_key" error. It cannot mark someone
// else's memory reviewed, because ids are explicit and topic keys are
// project-scoped. A refusal here would buy nothing and would block the listing
// half of the tool for callers whose directory the daemon cannot resolve.
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
				// Same 200 ceiling ListForReview caps `limit` at, for the same
				// reason: mark_reviewed is the action you take on a list you just
				// read, so a batch can never legitimately exceed a page of it. It
				// REFUSES rather than truncating — silently marking the first 200
				// of 500 ids and reporting success would leave the caller believing
				// 300 memories were verified that were not.
				if len(rawIDs) > reviewIDsPerCall {
					return mcp.NewToolResultError(fmt.Sprintf(
						"mem_review: mark_reviewed accepts at most %d ids per call (got %d) — split it into pages",
						reviewIDsPerCall, len(rawIDs))), nil
				}
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
