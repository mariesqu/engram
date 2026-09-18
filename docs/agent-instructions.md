# Engram Memory Protocol

Copy this block into your agent's instruction file (CLAUDE.md for Claude Code, AGENTS.md, .cursorrules for Cursor, or whichever file your MCP client reads as system-level instructions).

> You may not have to. The daemon already ships a condensed version of this protocol through the MCP `instructions` channel, which clients that honour it prepend to the model's system prompt. And `engram setup hooks --agent claude-code|codex` (see [Lifecycle hooks](../README.md#lifecycle-hooks)) installs a `session-start` hook that injects the same protocol **plus the project's recent memory** at the start of every session — including after a compaction, which is exactly when a file the model can no longer see stops helping. Copy this block when you want the long form, or when your client supports neither.

---

## Engram Persistent Memory — Protocol

You have access to Engram, a persistent memory system exposed over MCP. It survives across sessions and context compactions. This protocol is **always active** — do not wait for the user to ask you to use it.

### Tools available

| Tool | Purpose |
|------|---------|
| `mem_current_project` | Report which project THIS caller resolves to, and how (`fallback`/`writes_blocked` flag a guess) — the recommended first call of a session |
| `mem_session_start` | Register the start of a coding session |
| `mem_session_end` | Mark a session as completed with an optional summary |
| `mem_save` | Save an observation (decision, bug fix, discovery, …) to persistent memory |
| `mem_suggest_topic_key` | Suggest a stable, deterministic `topic_key` so re-saves UPSERT the same chain instead of duplicating |
| `mem_save_prompt` | Save the user's prompt so `mem_save` can auto-attach it to the next observation |
| `mem_get_observation` | Retrieve the full untruncated content of an observation by numeric ID |
| `mem_update` | Edit a specific observation in place by ID (omitted fields keep their value; versioned and re-synced) |
| `mem_search` | Full-text, semantic, or hybrid search across observations |
| `mem_similar` | Find observations semantically nearest a given memory (by sync_id) |
| `mem_review` | List memories by lifecycle/staleness status, or `mark_reviewed` to reset the clock (local-only) |
| `mem_context` | Assemble recent sessions and observations into a context summary |
| `mem_pin` | Pin a memory so it leads `mem_context` and ranks higher in keyword search (local-only) |
| `mem_unpin` | Unpin a memory, returning it to normal recency order (local-only) |
| `mem_session_summary` | Save a structured end-of-session summary |
| `mem_judge` | Record a verdict on a conflict candidate surfaced by `mem_save` |
| `mem_merge_projects` | Merge a source project's memories into a target name to fix name drift (local-only) |

---

### Confirm the project first

Start a session with `mem_current_project`. It never errors, and it tells you which project every later call will be filed under plus how that name was derived. Three fields decide what you do next:

- `fallback: true` — the name is a GUESS (a directory basename, or a lenient fallback after a resolution error). Nothing declared it, so pass an explicit `project` on later calls if it is not the name you want.
- `writes_blocked: true` — `mem_save` / `mem_session_start` / `mem_session_summary` will refuse this directory until you pass `project` explicitly. Reads still answer from the basename. Causes: an ambiguous directory (a parent of several repos), a malformed `.engram/config.json`, a directory that does not exist, or a project whose policy is `omitted`.
- `directory_exists: false` — the resolved directory is not on this machine, so any name here was invented from its basename. Pass a real directory or an explicit `project`.

The response also carries `hints` — an ARRAY of one plain-language sentence per reason the answer is untrustworthy — plus `directory_source` (`argument` = injected by `engram connect`; `cwd_alias` = the path *you* supplied; `daemon_cwd` = nobody supplied one, so this describes the daemon's own directory, typically NOT your repo), `cwd` (absolute, cleaned), `cwd_input` (what you passed, verbatim) and `project_path` (the project's canonical directory).

Pass the workspace in `directory` when you have to name one; `cwd` is accepted as an alias by every project-resolving tool and is read only when `directory` is absent or blank.

---

### Proactive save triggers (do NOT wait to be asked)

Call `mem_save` immediately after any of the following — without the user asking:

- Architecture or design decision made
- Tradeoff chosen between two approaches
- Bug fixed (include root cause and affected file)
- New convention or pattern established
- Tool, library, or framework choice made
- Configuration or environment change completed
- Non-obvious discovery about the codebase
- Gotcha, edge case, or unexpected behavior found

**Self-check after every task:** "Did I make a decision, fix a bug, learn something non-obvious, or establish a convention? If yes, call `mem_save` now."

**Format for `mem_save`:**
- `title`: short, searchable — verb + what (e.g. "Fixed N+1 query in UserList")
- `type`: `decision` | `bugfix` | `architecture` | `pattern` | `config` | `discovery` | `learning`
- `content`: structured — **What** / **Why** / **Where** / **Learned** (omit Learned if none)

---

### Search to recall

When the user references past work ("remember…", "how did we…", "what was the reason for…") or when you are starting a task that may have prior context:

1. Call `mem_context` — assembles recent sessions and observations (fast, cheap)
2. If not found, call `mem_search` with relevant keywords
3. If a result looks relevant, call `mem_get_observation` with its numeric ID to get the full untruncated content (search results are truncated)

Also search **proactively** at the start of a session when the user's first message references a project, feature, or problem — call `mem_search` before responding.

---

### Pinning

`mem_pin` keeps a memory in front of you: pinned observations render in their own `### Pinned` section at the top of `mem_context` (ahead of recent observations, which exclude them) and get a small ranking boost in keyword search. Reserve it for the handful of facts that must not scroll away — the stack decision, the gotcha that keeps biting. `mem_unpin` reverses it.

Pinned state is **local to this machine** and never syncs: it is your judgment about your own context, not shared truth.

---

### Prompt capture

If you can observe the user's prompt before saving derived memories, call `mem_save_prompt` first. This records the prompt so that subsequent `mem_save` calls can auto-attach it to the observation.

---

### Session lifecycle

- **On session start:** call `mem_session_start` to register the session and resolve the project name.
- **Before ending a session** (when saying "done", "that's it", or equivalent): call `mem_session_summary` with a structured summary before closing.

**Format for `mem_session_summary`:**

```
## Goal
[What we were working on this session]

## Discoveries
- [Technical findings, gotchas, non-obvious learnings]

## Accomplished
- [Completed items with key details]

## Next Steps
- [What remains — for the next session]

## Relevant Files
- path/to/file — [what it does or what changed]
```

This is not optional. Without it, the next session starts with no context.

---

### Conflict resolution

When `mem_save` returns a response containing `judgment_required: true`, a post-save similarity scan found candidate conflicts. Call `mem_judge` with the `judgment_id` from the response and one of: `related`, `compatible`, `scoped`, `conflicts_with`, `supersedes`, or `not_conflict`.

---

### After compaction

If you see a compaction notice or a "context cleared" event:

1. Call `mem_context` to recover recent session history
2. If you need more detail on a specific observation, call `mem_get_observation` by ID
3. Only then continue working

The persistent store survives compaction — the agent just needs to re-read it.
