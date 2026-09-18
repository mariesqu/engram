package main

// instructions.go carries the agent protocol engram ships through the MCP
// `instructions` channel — the text an MCP client receives in the initialize
// result and (for clients that honour it) prepends to the model's system
// prompt.
//
// Why the server sends it at all: gentle-ai's IsVerifiedSlimAdapter
// (internal/components/engram/protocol.go) now gates on `engram version`
// parsing to a bare semver at or above its floor, and once it does it injects
// only its SLIM protocol section into CLAUDE.md — explicitly delegating "the
// full protocol (save format, lifecycle, search flow, after-compaction steps)"
// to "the Engram MCP server instructions". A server that ships no instructions
// leaves that half of the contract unfulfilled: the agent gets the slim
// reminders and nothing that tells it HOW to save, search, or recover.
//
// The text below is the condensed form of docs/agent-instructions.md. Keep the
// two in sync — and keep BOTH honest about the tool surface actually registered
// in registerTools. Naming a tool here that the server does not register is
// worse than omitting it: the agent will call it and get a protocol error.

// serverInstructions is the protocol text handed to MCP clients in the
// initialize result. It is a package-level constant so the acceptance tests can
// assert on it without booting a daemon.
//
// Shape notes:
//   - The tool roster comes first because an agent that does not know a tool
//     exists never calls it.
//   - The proactive-save rule is stated as an imperative, not a suggestion:
//     the whole value of engram collapses when saves are opt-in.
//   - Nothing here is client-specific. Claude Code, Codex, Cursor and the web
//     UI all read the same text.
const serverInstructions = `Engram provides persistent memory that survives across sessions and context compactions. This protocol is ALWAYS ACTIVE — do not wait to be asked.

TOOLS
  mem_current_project  — which project THIS caller resolves to, and how. Recommended FIRST call of a session; never errors.
  mem_save             — save a decision, bug fix, discovery, or convention. Call PROACTIVELY.
  mem_save_prompt      — record the user's prompt so the next mem_save can attach it.
  mem_search           — full-text, semantic, or hybrid search across observations.
  mem_similar          — observations semantically nearest a given memory (by sync_id).
  mem_get_observation  — full untruncated content of one observation by numeric id (search results are truncated).
  mem_update           — edit one observation in place by id; omitted fields keep their value.
  mem_suggest_topic_key— a stable topic_key so re-saves UPSERT one chain instead of duplicating.
  mem_context          — recent sessions and observations. Call at session start and after any compaction.
  mem_review           — list memories by lifecycle status (active|needs_review|expired), or mark_reviewed to reset the clock.
  mem_judge            — record a verdict on a conflict candidate surfaced by mem_save.
  mem_merge_projects   — merge a source project's memories into a target name to fix name drift.
  mem_session_start    — register the start of a coding session.
  mem_session_summary  — save the structured end-of-session summary.
  mem_session_end      — mark a session completed.

CONFIRM THE PROJECT FIRST
Open with mem_current_project. Two fields decide what you do next:
  fallback=true       — the name is only a directory basename; pass an explicit project later if that is not the name you want.
  writes_blocked=true — reads answer from the basename, but mem_save/mem_session_start will REFUSE until you pass project explicitly.

PROACTIVE SAVE RULE
Call mem_save immediately — without being asked — after any of: an architecture or design decision, a tradeoff chosen, a bug fixed (root cause + file), a convention or pattern established, a tool/library choice, a configuration change, a non-obvious discovery, a gotcha or edge case.
Self-check after EVERY task: "Did I decide something, fix a bug, learn something non-obvious, or establish a convention? If yes, call mem_save now."

SAVE FORMAT
  title   — verb + what, short and searchable ("Fixed N+1 query in UserList")
  type    — decision | bugfix | architecture | pattern | config | discovery | learning
  scope   — project (default) | personal
  topic_key (recommended for an evolving topic) — a stable key such as architecture/auth-model, so re-saves UPSERT instead of duplicating
  content — structured: **What** / **Why** / **Where** / **Learned** (omit Learned if there is none)

SEARCH TO RECALL
On any reference to past work ("remember…", "how did we…", "why did we…"), and proactively when the user's first message names a project, feature, or problem:
  1. mem_context   — recent sessions and observations (fast, cheap)
  2. mem_search    — keywords, when context does not answer it
  3. mem_get_observation — the numeric id of a promising hit, for the full text

LIFECYCLE
An observation is active, needs_review, or expired. Treat a needs_review memory as STALE CONTEXT, not a trusted fact: surface it and verify against current evidence before relying on it. Never call mem_review with mark_reviewed on your own — only after the user confirms.

CONFLICTS
When mem_save returns judgment_required, a similarity scan found candidates. Call mem_judge with the judgment_id from the response and one of: related, compatible, scoped, conflicts_with, supersedes, not_conflict.

SESSION CLOSE
Before saying "done", "that's it", or the equivalent, call mem_session_summary with sections: ## Goal, ## Discoveries, ## Accomplished, ## Next Steps, ## Relevant Files. This is not optional — without it the next session starts blind.

AFTER COMPACTION
On a compaction notice or a cleared context: call mem_session_summary with the compacted summary, then mem_context to recover prior sessions, and only then continue working.

DELIVERY GUARANTEE
Memory work is internal bookkeeping, never the user-facing answer. Finish the required saves BEFORE composing your reply, then send the complete answer as the final message of the turn with no tool calls after it. If a memory call fails, deliver the answer anyway and note the failure briefly.`
