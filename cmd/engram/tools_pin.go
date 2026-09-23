package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/syncer"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func registerPinTools(srv *mcpserver.MCPServer, store *localstore.Store, loop *syncer.Loop, embedLoop *embedding.Loop, gated embedding.EmbeddingProvider, writerID string, activity *SessionActivity, daemonCwdIsWorkspace bool) {
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
