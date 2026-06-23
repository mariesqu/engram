# Proposal: memory lifecycle (`mem_review`) + project-merge tools

Status: draft / spec
Audience: engram maintainers

## Why

The team's SDD orchestrator + `engram-convention.md` reference engram capabilities that the
v1.1.0 rewrite does not yet expose:

- **`mem_review`** (lifecycle: `active` / `needs_review`, `mark_reviewed`) — the convention's
  "Memory lifecycle rule" leans on this so stale architecture memories are verified, not trusted
  blindly. Today it's a documented no-op.
- **`mem_merge_projects`** (MCP) + **`engram projects consolidate`** (CLI) — clean up project
  name drift (`my-app` vs `myapp`). Today: not implemented (`projects` CLI is `list`/`policy`/`delete`).
- **Save-time name-drift warning** — warn when `mem_save` resolves a project that's close-but-not-equal
  to an existing one.

This spec closes those gaps, prioritized smallest-and-highest-value first.

---

## Feature 1 — `mem_review` (memory lifecycle / staleness)  ⟵ PRIORITY

### Current state (important)
The scaffolding already exists and is **dormant**:
- `memories.review_after TEXT` and `memories.expires_at TEXT` columns (`schema.go`, carried through the
  v10→v11 table rebuild).
- `domain.Record.ReviewAfter *time.Time` and `ExpiresAt *time.Time` (`internal/domain/memory.go`).
- **Nothing sets them on write and nothing reads them** (verified: no `review_after` writes in
  `observations.go`/`apply.go`). So no schema migration is required — only wiring.

### Design
1. **Set `review_after` on write.** In `AddObservation` / `UpdateMemory` (the `LocalWrite` path), stamp
   `review_after = updated_at + staleness_window`. Default window **30 days**; configurable via a new
   `config.json` key `review_window_days` (0 = lifecycle disabled → leave NULL, status always `active`).
   Apply to load-bearing types only by default (`decision`, `architecture`, `pattern`) — a config flag
   `review_all_types` opts everything in. (Ephemeral `session_summary`/`manual` notes shouldn't nag.)
2. **Computed status (read-time, no stored enum).**
   - `expired`      → `expires_at` set and `now > expires_at`
   - `needs_review` → `review_after` set and `now > review_after`
   - `active`       → otherwise (incl. NULL `review_after`)
3. **`mem_review` MCP tool.**
   - `action: "list"` — args `status` (`needs_review` default | `active` | `expired` | `all`), optional
     `project`, `limit`. Returns `id, title, type, project, status, review_after`.
   - `action: "mark_reviewed"` — args `ids: number[]` OR `topic_key: string`. Sets
     `review_after = now + window`, `expires_at` untouched. Returns count updated.
   - Read-only `list`; `mark_reviewed` is a local write (NOT synced — see Sync below). Mirror the
     id-parse + error patterns from `mem_get_observation`/`mem_update`.
4. **Surface status inline** in `mem_search` / `mem_get_observation` output (a `Status: needs_review`
   line) so agents see staleness without a separate call — this is what actually makes the convention's
   "verify before trusting" rule enforceable.
5. **CLI:** `engram memories review [--status needs_review] [--db ...]` for humans, mirroring
   `engram memories list`.

### Sync semantics (decision)
Recommend `review_after`/`expires_at` are **LOCAL-ONLY** node metadata — review is a per-node judgment,
not shared truth — so they stay OUT of the sync mutation payload (confirm `domain.Mutation` does not
carry them; the local `Record` does). `mark_reviewed` therefore creates no outbox entry. (If the team
wants shared review state later, that's a separate, larger change.)

### Effort
Small. No migration; columns + domain fields exist. ~1 tool + 1 CLI subcommand + set-on-save + status in
two output paths + tests (set-on-save, status transitions, `mark_reviewed`, list filters, window=0 off).

---

## Feature 2 — save-time name-drift warning

### Design
On `mem_save`, after resolving the project, compare it to the set of existing distinct projects
(`SELECT DISTINCT project FROM memories`). If the resolved project is **not** an exact match but is a
near-variant of an existing one — case/separator-insensitive equal, or normalized Levenshtein ≤ 2 —
append a non-blocking warning to the tool result:

```
Memory saved: "…" (id=42, project="myapp")
note: project "myapp" looks close to existing "my-app" — pass an explicit project to avoid drift,
      or run `engram projects consolidate` to merge.
```

Never block the save. Cache the distinct-project list per daemon process (cheap; invalidate on new project).

### Effort
Small. One helper + a warning line in `handleSave`. No schema.

---

## Feature 3 — `mem_merge_projects` (MCP) + `engram projects consolidate` (CLI)

### Design
Merge a source project's rows into a target (canonical) name. Local tables touched:
`memories.project`, `project_policy.project`, `pull_cursors.project`. Surface:
- MCP `mem_merge_projects(from, to)` and CLI `engram projects consolidate <from> <to> [--db] [--yes]`
  (dry-run by default, like `projects delete`).

### The hard part — sync semantics (decision)
Changing a memory's `project` is a content change → a new version → it must re-push under the new name
for other nodes to converge, and the OLD-named rows must be tombstoned/redirected centrally. Two options:
- **A. Local-only rename** (phase 1): rename locally, do NOT propagate; document that each node merges
  independently. Simple, but nodes can diverge until all run the merge.
- **B. Propagating merge** (phase 2): re-stamp each affected memory as a new version under the target
  project (enqueues outbox upserts) so the merge syncs. Larger; needs central-side handling of the
  now-orphaned source-project rows.

Recommend shipping **A** first (covers the common single-active-node drift case), **B** as a follow-up.

### Effort
Medium (A) → Large (B, sync). Defer behind Features 1–2.

---

## Sequencing
1. **`mem_review`** — smallest, highest convention value, no migration.
2. **name-drift warning** — tiny, prevents the drift Feature 3 cleans up.
3. **`mem_merge_projects` / `consolidate`** — local-rename first, propagation later.

Each ships on its own branch → adversarial review → tag (minor bumps: v1.2.0, v1.3.0, …), same rigor as v1.1.0.

## Open decisions
- Staleness window default (30d?) and per-type scope (`decision`/`architecture`/`pattern` vs all).
- `review_after`/`expires_at` local-only (recommended) vs synced.
- Merge propagation: local-only (A) acceptable for v1.x, or is B required before shipping?
