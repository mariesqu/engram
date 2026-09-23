package main

import (
	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
	"github.com/mariesqu/engram/internal/syncer"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

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
//
// daemonCwdIsWorkspace is true only for a per-client `engram daemon --transport
// stdio` (README.md's documented setup: the MCP client spawns the daemon IN
// the project directory, so its cwd genuinely IS that client's workspace) —
// see buildDaemon, which sets it from cfg.mcpTransport == "stdio". It is false
// for the SHARED resident daemon (`--transport http`, what `engram connect`
// bridges to), whose cwd is wherever autostart/tray happened to launch it
// from and is never trustworthy. Threaded down to resolveSaveProject,
// handleSessionStart and currentProjectEnvelope, the only places that decide
// whether a dirSourceDaemonCwd directory is refused.
func registerTools(srv *mcpserver.MCPServer, store *localstore.Store, loop *syncer.Loop, embedLoop *embedding.Loop, gated embedding.EmbeddingProvider, writerID string, activity *SessionActivity, daemonCwdIsWorkspace bool) {
	registerCurrentProjectTools(srv, store, loop, embedLoop, gated, writerID, activity, daemonCwdIsWorkspace)
	registerSessionTools(srv, store, loop, embedLoop, gated, writerID, activity, daemonCwdIsWorkspace)
	registerSaveTools(srv, store, loop, embedLoop, gated, writerID, activity, daemonCwdIsWorkspace)
	registerSearchTools(srv, store, loop, embedLoop, gated, writerID, activity, daemonCwdIsWorkspace)
	registerPinTools(srv, store, loop, embedLoop, gated, writerID, activity, daemonCwdIsWorkspace)
	registerJudgeSimilarTools(srv, store, loop, embedLoop, gated, writerID, activity, daemonCwdIsWorkspace)
	registerReviewTools(srv, store, loop, embedLoop, gated, writerID, activity, daemonCwdIsWorkspace)
	registerDoctorTools(srv, store, loop, embedLoop, gated, writerID, activity, daemonCwdIsWorkspace)
}
