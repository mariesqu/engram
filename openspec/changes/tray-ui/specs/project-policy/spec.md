# Project Policy Specification

## Purpose

Defines the per-node, per-project policy machine that controls whether a project's observations are captured, kept local, or synchronized with central. Policy is node-local — central has no awareness of it.

---

## Requirements

### Requirement: Three-state policy machine

Each project on a given node SHALL have exactly one active policy state:
- `synced` — observations are written locally AND pushed to / pulled from central (default when central is configured).
- `local-only` — observations are written locally; push and pull are suppressed (outbox entries remain unacked but are never drained for this project).
- `omitted` — `mem_save` and `mem_save_prompt` MUST return an error and write NOTHING locally (no row in `memories`, no outbox entry). The error message MUST clearly indicate the project is omitted.

#### Scenario: mem_save on an omitted project returns an error and writes nothing

- GIVEN project "secret-proj" has policy `omitted`
- WHEN an agent calls `mem_save` with `project: "secret-proj"`
- THEN the tool returns an error
- AND the `memories` table contains no new row for "secret-proj"
- AND the `sync_mutations` outbox contains no new entry for "secret-proj"

#### Scenario: mem_save_prompt on an omitted project returns an error and writes nothing

- GIVEN project "secret-proj" has policy `omitted`
- WHEN an agent calls `mem_save_prompt` with `project: "secret-proj"`
- THEN the tool returns an error and writes nothing locally

#### Scenario: mem_save on a local-only project writes locally but produces no push

- GIVEN project "private-proj" has policy `local-only` and central is configured
- WHEN an agent calls `mem_save` with `project: "private-proj"`
- THEN a row is created in `memories`
- AND an outbox entry is created in `sync_mutations`
- AND after one or more sync cycles, the entry remains unacked (never drained to central)

#### Scenario: mem_save on a synced project writes and pushes normally

- GIVEN project "open-proj" has policy `synced` and central is configured
- WHEN an agent calls `mem_save` with `project: "open-proj"`
- THEN a row is created in `memories`
- AND after a sync cycle the entry is pushed to central and acked

---

### Requirement: Push-time outbox filtering

The syncer's push drain SHALL skip outbox entries whose project has policy `local-only` or `omitted`. Skipped entries MUST remain in the outbox unacked. The entries MUST NOT be deleted or marked with an error state. A subsequent policy flip to `synced` MUST allow the previously skipped entries to be drained on the next push cycle.

#### Scenario: Push skips local-only entries and leaves them unacked

- GIVEN 3 outbox entries: 2 for project "open" (synced) and 1 for project "private" (local-only)
- WHEN the push drain runs
- THEN the 2 "open" entries are pushed and acked
- AND the 1 "private" entry remains unacked in the outbox

#### Scenario: Flip to synced drains previously accumulated outbox entries

- GIVEN project "private" has 5 unacked outbox entries accumulated while policy was `local-only`
- WHEN the policy is flipped to `synced` and one sync cycle runs
- THEN all 5 previously skipped entries are pushed to central and acked
- AND no outbox schema change is required (eligibility is re-evaluated per drain)

---

### Requirement: Pull-time project exclusion

The syncer's pull step SHALL exclude projects with policy `local-only` or `omitted` from the set of projects fetched from central. No remote observations for those projects SHALL be written to the local store during that exclusion period.

#### Scenario: Pull skips local-only project

- GIVEN project "private" has policy `local-only` and central has new observations for "private"
- WHEN a pull cycle runs
- THEN the local store for "private" is not updated with those central observations

---

### Requirement: Flip-transition semantics

Policy flips MUST follow these transition rules:
- **`local-only` → `synced`**: outbox entries for the project become eligible for drain on the next push cycle; pull resumes from the existing per-project cursor. No data migration or outbox surgery required.
- **`synced` → `local-only`**: future pushes and pulls for the project are suppressed immediately; data already pushed to central MUST NOT be unpublished or deleted from central.
- **`omitted` → any**: takes effect for future writes only; `omitted` never wrote locally, so there is no backlog to drain and no outbox entries to clean up.
- **Any → `omitted`**: future writes are refused. Previously written local data for the project is NOT deleted.

#### Scenario: synced → local-only — already-pushed data stays on central

- GIVEN project "shared" is synced and has data on central
- WHEN the policy is flipped to `local-only`
- THEN future writes for "shared" are not pushed
- AND the data previously pushed to central is not affected

#### Scenario: omitted → synced — no backlog to drain

- GIVEN project "previously-omitted" has policy `omitted` and zero outbox entries (omit never wrote locally)
- WHEN the policy is flipped to `synced`
- THEN the first new `mem_save` call writes locally and is pushed on the next sync cycle
- AND no orphaned outbox cleanup is required

---

### Requirement: Default policy assignment

When a project is first encountered (first `mem_save` call for a project not yet in `project_policy`):
- If central is configured on the node: the default policy SHALL be `synced`.
- If central is NOT configured on the node: the default policy SHALL be `local-only`.

This default MUST be consistent with the schema v10 migration rule for existing projects.

#### Scenario: First write to new project with central configured — defaults to synced

- GIVEN central is configured and "new-project" has no policy row
- WHEN `mem_save` is called with `project: "new-project"`
- THEN the project is assigned policy `synced` and the observation is queued for push

#### Scenario: First write to new project without central — defaults to local-only

- GIVEN no central is configured and "new-project" has no policy row
- WHEN `mem_save` is called with `project: "new-project"`
- THEN the project is assigned policy `local-only` and the observation is written locally only

---

### Requirement: Schema — project_policy table (v10 migration)

The local SQLite schema SHALL include a `project_policy` table with at minimum: `project` (TEXT PRIMARY KEY), `policy` (TEXT NOT NULL CHECK(policy IN ('synced','local-only','omitted'))). The `PRAGMA user_version` MUST be bumped from 9 to 10. The migration MUST be idempotent (re-running on a v10 store is a no-op).

On migration from v9 to v10: the migration is O(1) — it creates the `project_policy` table and bumps `user_version`. Existing projects are NOT backfilled with rows at migration time. The default policy is computed at read time by `GetPolicy`/`ListProjectsWithPolicy`: if central is configured the default is `synced`; otherwise `local-only`. The "migration defaults existing projects" success criterion is satisfied by this read-time default, asserted via `GetPolicy` returning the correct default — NOT by counting inserted rows.

#### Scenario: v9 → v10 migration adds table and read-time default for existing projects

- GIVEN a store at schema v9 with observations for projects A and B, and central is configured
- WHEN the v10 migration runs
- THEN the `project_policy` table exists (zero rows inserted — no backfill)
- AND `PRAGMA user_version` returns 10
- AND calling `GetPolicy("A")` or `GetPolicy("B")` returns `synced` (read-time default because central is configured)

#### Scenario: Migration idempotency — re-running on v10 store is a no-op

- GIVEN a store already at schema v10
- WHEN the migration logic runs again
- THEN no error is raised, no rows are duplicated, and `user_version` remains 10

#### Scenario: CHECK constraint rejects invalid policy value

- GIVEN the `project_policy` table exists
- WHEN an INSERT or UPDATE attempts to set `policy = 'unknown'`
- THEN the SQLite CHECK constraint raises an error

---

### Requirement: Policy persistence across restarts

Policy state MUST survive daemon restarts. The `project_policy` table is the authoritative source; on startup the daemon reads it before processing any write or sync request.

#### Scenario: Policy survives restart

- GIVEN project "private" has policy `local-only` stored in `project_policy`
- WHEN the daemon restarts
- THEN `mem_save` calls for "private" after restart still write locally and are not pushed

---

> **Headless testability**: All requirements in this spec are provable headlessly via the control API and direct SQLite inspection. No tray or browser required.
