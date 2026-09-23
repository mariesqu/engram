package main

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/mariesqu/engram/internal/diagnostic"
	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/syncer"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func registerDoctorTools(srv *mcpserver.MCPServer, store *localstore.Store, loop *syncer.Loop, embedLoop *embedding.Loop, gated embedding.EmbeddingProvider, writerID string, activity *SessionActivity, daemonCwdIsWorkspace bool) {
	// ── mem_doctor ───────────────────────────────────────────────────────────
	srv.AddTool(
		mcp.NewTool("mem_doctor",
			mcp.WithDescription(`Run read-only operational diagnostics over the local store and return a structured report.

Answers the questions that otherwise surface as confusing symptoms much later: observations whose session no longer exists, a session filed under a project its directory no longer resolves to, several sessions still "open" for the same directory, projects syncing on a default nobody chose, SQLite lock contention, a review backlog that has swallowed the lifecycle signal, and an outbox that is not draining.

Response: {status, project, summary{total,ok,warnings,blocked,errors}, checks[{check_id, result, severity, reason_code, message, why, evidence, safe_next_step, requires_confirmation, findings[]}]}. status rolls up worst-first: error > blocked > warning > ok.

It NEVER modifies your memories: it reports, it does not repair. (Not a pure reader of the FILE — the lock probe runs PRAGMA wal_checkpoint(PASSIVE), which may move pages out of the WAL. No row, session or project is touched.) Every finding carries a safe_next_step for YOU to run deliberately — the conditions it reports are the ones where the right fix depends on context the store does not have.

A finding with severity "error" means one CHECK could not answer, not that the call failed; the other checks still report. Only an unrunnable request (an unknown check id) comes back as a tool error.`),
			mcp.WithTitleAnnotation("Run Diagnostics"),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithString("project",
				mcp.Description("Optional explicit project to scope the per-project checks to. When omitted it is auto-detected from the working directory, exactly as mem_search and mem_context resolve it. The node-wide checks (sqlite_lock_contention, sync_backlog) report the same either way."),
			),
			mcp.WithString("check",
				mcp.Description("Optional single check to run: "+strings.Join(diagnostic.RegisteredCodes(), ", ")+". Omit to run all of them."),
			),
			mcp.WithString("directory",
				mcp.Description(directoryArgDescription),
			),
			mcp.WithString("cwd",
				mcp.Description(cwdArgDescription),
			),
		),
		handleDoctor(store),
	)
}

// handleDoctor returns the handler for mem_doctor — the read-only diagnostics
// pass over this node's store.
//
// Project scope follows the LENIENT read resolution (resolveReadProject), the
// same chain mem_search and mem_context answer from, so the per-project checks
// describe the project the agent's other calls are actually using. The two
// node-wide checks (sqlite_lock_contention, sync_backlog) ignore the scope by
// construction: a held lock and an undrained outbox belong to the machine.
//
// The result is the diagnostic.Report envelope as JSON. It is marked IsError
// only for a RUN-LEVEL failure (Report.IsRunLevelFailure: an unknown check
// code, a scope the runner could not use) — never for what the report found. A
// report full of warnings is a SUCCESSFUL diagnosis, and so is one where a
// single probe could not read its own pragma and said so with severity=error:
// six other checks answered. Flagging either as a tool error teaches an agent
// that running the doctor is something that fails, and the agent stops running
// it — on exactly the store that needed it.
func handleDoctor(store *localstore.Store) mcpserver.ToolHandlerFunc {
	return handleDoctorWithRunner(store, diagnostic.NewRunner())
}

// handleDoctorWithRunner is handleDoctor with an injectable runner, so the
// dispatch (all checks vs exactly one) can be tested against a registry that
// counts what it was asked to run. With the default registry that assertion is
// indirect at best: every real check answers ok on a clean store, so a handler
// that ran ALL of them and then ran one again looked identical in the response
// — which is precisely how it shipped.
func handleDoctorWithRunner(store *localstore.Store, runner diagnostic.Runner) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		explicitProject, _ := args["project"].(string)
		dirArg := readDirectoryArg(args)
		if dirArg.Err != nil {
			return dirArg.toolError("mem_doctor"), nil
		}
		check, _ := args["check"].(string)

		scope := diagnostic.Scope{
			Store:   store,
			Project: resolveReadProject(explicitProject, dirArg.Directory),
			Now:     time.Now().UTC(),
		}

		// if/else, not run-then-overwrite: the previous version ran the whole
		// registry and THREW THE REPORT AWAY whenever check was supplied, so asking
		// for one check cost a full diagnostic pass — including the WAL checkpoint
		// probe and two scans of the sessions table.
		var report diagnostic.Report
		if check = strings.TrimSpace(check); check != "" {
			report = runner.RunOne(ctx, scope, check)
		} else {
			report = runner.RunAll(ctx, scope)
		}

		out, err := json.Marshal(report)
		if err != nil {
			// Unreachable in practice (the report holds strings, ints and
			// pre-marshaled evidence), but a diagnostics tool that dies on its own
			// formatting would be a poor advertisement for diagnostics.
			return mcp.NewToolResultError("mem_doctor: encode report: " + err.Error()), nil
		}
		result := mcp.NewToolResultText(string(out))
		result.IsError = report.IsRunLevelFailure()
		return result, nil
	}
}
