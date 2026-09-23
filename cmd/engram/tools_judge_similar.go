package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/syncer"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func registerJudgeSimilarTools(srv *mcpserver.MCPServer, store *localstore.Store, loop *syncer.Loop, embedLoop *embedding.Loop, gated embedding.EmbeddingProvider, writerID string, activity *SessionActivity, daemonCwdIsWorkspace bool) {
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
