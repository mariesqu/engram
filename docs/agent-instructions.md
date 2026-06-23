# Engram Memory Protocol

Copy this block into your agent's instruction file (CLAUDE.md for Claude Code, AGENTS.md, .cursorrules for Cursor, or whichever file your MCP client reads as system-level instructions).

---

## Engram Persistent Memory — Protocol

You have access to Engram, a persistent memory system exposed over MCP. It survives across sessions and context compactions. This protocol is **always active** — do not wait for the user to ask you to use it.

### Tools available

| Tool | Purpose |
|------|---------|
| `mem_session_start` | Register the start of a coding session |
| `mem_session_end` | Mark a session as completed with an optional summary |
| `mem_save` | Save an observation (decision, bug fix, discovery, …) to persistent memory |
| `mem_save_prompt` | Save the user's prompt so `mem_save` can auto-attach it to the next observation |
| `mem_get_observation` | Retrieve the full untruncated content of an observation by numeric ID |
| `mem_search` | Full-text, semantic, or hybrid search across observations |
| `mem_similar` | Find observations semantically nearest a given memory (by sync_id) |
| `mem_context` | Assemble recent sessions and observations into a context summary |
| `mem_session_summary` | Save a structured end-of-session summary |
| `mem_judge` | Record a verdict on a conflict candidate surfaced by `mem_save` |

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
- `type`: `decision` | `bugfix` | `architecture` | `pattern` | `config` | `discovery`
- `content`: structured — **What** / **Why** / **Where** / **Learned** (omit Learned if none)

---

### Search to recall

When the user references past work ("remember…", "how did we…", "what was the reason for…") or when you are starting a task that may have prior context:

1. Call `mem_context` — assembles recent sessions and observations (fast, cheap)
2. If not found, call `mem_search` with relevant keywords
3. If a result looks relevant, call `mem_get_observation` with its numeric ID to get the full untruncated content (search results are truncated)

Also search **proactively** at the start of a session when the user's first message references a project, feature, or problem — call `mem_search` before responding.

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
