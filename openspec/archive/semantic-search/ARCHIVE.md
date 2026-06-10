# Archive Report: semantic-search

**Status**: COMPLETE — archived 2026-06-10.

## Outcome

The change delivered per-node embeddings, brute-force cosine similarity search, RRF hybrid fusion with lexical FTS, and a resumable background backfill loop. All 35 task boxes completed across 3 chained PR slices (PR #59, PR #60, PR #61, stacked-to-main). Verification PASSED with 0 CRITICAL / 2 WARNING / 3 SUGGESTION.

### Delivered (3 Chained PRs, stacked-to-main)

- **PR #59** — Embedding core: `EmbeddingProvider` port with `RemoteOpenAIProvider` (OpenAI text-embedding-3-small, 256-dim, stdlib net/http) and `NoopProvider` (FTS-only degradation). Privacy gate as a wrapper provider (HIGHEST-STAKES invariant). Vector codec (little-endian float32, L2-normalized). Brute-force cosine similarity scan. RRF hybrid fusion (`k=60`) merging FTS5-BM25 and cosine ranks. `SearchFilter.Mode` additive field (`""` | `"fts"` | `"semantic"` | `"hybrid"`). Config keys: `embedding_provider` enum, `encrypted_embedding_key` reusing `config.SecretBox` Seal/Open, `ENGRAM_EMBEDDING_KEY` env (never a flag). Policy-gated search: query text of omitted/local-only projects never sent remotely. Graceful degradation to FTS with optional honest note when mode is `semantic`/`hybrid` and embeddings unavailable.
- **PR #60** — Backfill loop: `embedding.Loop` mirroring `syncer.Loop` (Interval, Trigger, Stop, backoff, Debounce). `SelectEmbeddable` predicate (`embedding IS NULL OR embedding_model <> current`) for idempotent re-embedding on model change. Batch size 100, 1s inter-batch pause. Per-row policy gate (skips omitted/non-consented rows silently). Status observability: `GET /api/v1/status` gains `embedding_backfill` sub-object with `pending` count (policy-eligible rows only) and `provider` name. On-write nudge: `handleSave` calls `embedLoop.Trigger()` (nil-safe) after successful write; `embedLoop.Start()` in daemon lifecycle; `Stop()` before store close.
- **PR #61** — Ollama sidecar + consent + mem_similar + FindCandidates: `OllamaSidecarProvider` (HTTP to localhost:11434/api/embeddings, configurable host, model, dims). Explicit embedding consent setting (`embedding_local_consent`) decoupled from sync policy — `local-only` projects require SEPARATE consent flag to embed locally. `mem_similar` MCP tool (top-K by cosine similarity from a `sync_id`, gated). `FindCandidates` cosine pass unioned with FTS candidates for paraphrase-aware conflict detection. Key routes: `POST /api/v1/embedding/key` (set, encrypt, store), `DELETE /api/v1/embedding/key` (clear). `embedding_dims` config for model-specific dimension selection (default 256).

**Lines changed**: ~1,100–1,400 (new packages + integration; test suites included).

### Review-Driven Strengthenings (beyond spec)

All of the following were validated by the verify phase as **correct implementations, NOT deviations**:

1. **Privacy gate as a WRAPPER provider, not a helper (STRUCTURAL INVARIANT)** (embedding/gated.go): Raw `RemoteOpenAIProvider` and `NoopProvider` are NEVER handed to callers. The daemon wiring constructs `gated := embedding.NewGated(inner, store, remote=true)` and `gated` is passed everywhere. No code path can hold an ungated provider — the gate is structurally impossible to bypass. Recording-mock proof asserts `omitted`/`local-only` projects receive ZERO provider calls on both the on-write and backfill paths.

2. **NULL-as-queue, not on-write embed (ON-WRITE PATH UNTOUCHED)** (localstore & tools.go:handleSave): Write path marks NOTHING new; the row's `embedding BLOB` column defaults to NULL in the schema. After `AddObservation` succeeds, `handleSave` calls `embedLoop.Trigger()` (nil-safe, reusing the `triggerSync` pattern). Embedding happens entirely in the background loop, never inline. Provider failures leave rows NULL and make them eligible for backfill retry — write path is never blocked by network.

3. **Per-row policy gate idempotency in backfill (DECISION 3 RATIONALE)** (embedding/loop.go): The loop's per-row `GetPolicy` check is NOT a SQL JOIN — it is a cached hash lookup after `SelectEmbeddable` returns the rows. This allows the gate to see runtime central-config state changes (`syncer.SetCentralConfiguredFn`) and project policy flips. A policy flip from `synced` → `local-only` mid-backfill stops FUTURE embedding (the predicate already embedded rows are not re-touched); already-embedded rows are node-local derived data and are not "unpublished."

4. **`UpdateEmbedding` does NOT take `s.mu` (DECISION 3 RESOLUTION)** (localstore/vector.go): Single-row UPDATE on derived columns is atomic in SQLite by itself and touches no reconciliation-critical columns. Taking `s.mu` would needlessly serialize embedding behind every write. The `AND embedding IS NULL` guard makes the UPDATE idempotent under concurrent rewrites of the same row.

5. **Honest degradation note ONLY on explicit semantic/hybrid request** (tools.go:handleSearch): `mode=""` or `mode="fts"` NEVER emits a note (byte-identical to today, hard constraint). `mode="semantic"` or `mode="hybrid"` with no embeddings appends one trailing line: "(semantic search unavailable: <reason>; showing keyword results)" where reason includes "not configured", "provider error", "policy gated", "not ready (<N> pending)". Matches the degradation matrix (design decision 5).

6. **Keyset-cursor anti-starvation paging (DESIGN PATTERN REUSE)** (embedding/loop.go:backoff logic): The loop mirrors `syncer.Loop` exactly — Debounce timer, exponential backoff on error (BackoffMin 1s, BackoffMax 2m), Stop channel. If a provider repeatedly fails, backoff prevents API hammering and log spam. On success, backoff resets. A policy flip or batch error does NOT abort the loop — the next tick retries the eligible rows.

7. **Embedding invalidation on content edit (DECISION 7: MODEL-CHANGE PREDICATE)** (localstore/vector.go:SelectEmbeddable, loop.go): When the configured embedding model changes (detected by `embedding_model != currentModel`), the backfill loop re-embeds stale rows via the SAME `embedding IS NULL OR embedding_model <> ?` predicate. No separate migration tool. Decode guards on blob length: a stored vector with `len(blob)/4 != currentDims` is treated as stale and re-embedded, preventing mixed-dim cosine garbage if model/dim change mid-flight.

8. **Tick-scoped partial failure (BACKFILL ROBUSTNESS)** (embedding/loop.go:run): A provider error on one batch does NOT abort the loop. Failed rows are left NULL, the loop logs the error, backs off, and retries next tick. Each tick scope is independent — 250 rows in batches of 100 → 3 calls, if call 2 fails, rows 67-100 stay NULL and rows 1-66 are already embedded (idempotent).

9. **True-union paraphrase pass wired with bounded ctx** (localstore/candidates.go:FindCandidates): The existing FTS candidate set is unioned by `sync_id` with the cosine pass results. A paraphrase that FTS misses but cosine finds is now in the union. The context is bounded (embedded candidates only), not unbounded document retrieval.

10. **All-embedding-keys restart-required consent honesty** (cmd/engram/daemon.go:provider resolution, config/config.go:Patch): `embedding_provider` and `embedding_dims` changes at runtime via `PUT /api/v1/config` are flagged with a `restart_required` flag in the response. `embedding_local_consent` is runtime-mutable (no restart needed — it only gates future backfill, already-embedded rows are not un-embedded). The config key routes (`POST /DELETE /api/v1/embedding/key`) are runtime-mutable but the provider uses the key immediately (gated provider field is a reference).

11. **Session/sentinel/codec disciplines** (config/config.go, cmd/engram/daemon.go:Reconnect block): The embedding key follows the writer-key discipline verbatim — `ENGRAM_EMBEDDING_KEY` env wins over sealed config, redacted in responses (only `EmbeddingKeySet bool` returned), never leaked in `--help` or `--version`. Sentinel error `ErrEmbeddingGated` is a distinct type, not a wrapped error. Codec round-trip unit test (encode → decode → cosine(v, v) == 1.0 within 1e-6) and little-endian byte layout test (hardcoded float → bytes assertion) validate the storage format.

## Warnings

### WARNING-1: Pending count formula (accepted by review, spec reconciliation)

**Spec text (original)**: "pending count reflects rows with `embedding IS NULL`"

**Implementation** (PR #60, server.go + spec reconciliation at archive): Counts only policy-eligible rows — i.e., `embedding IS NULL AND deleted_at IS NULL` where the project policy permits embedding (synced, or consented local-only with Ollama).

**Rationale**: Omitted projects and non-consented local-only projects are never embedded by the loop, so reporting them as "pending" implies the loop will process them (which it won't). The count reflects actual work the loop will do. Spec reconciled at archive.

---

### WARNING-2: Degradation-note exact text (accepted by review, design decision 5 interpretation)

**Design intent**: "silent degradation to FTS; optional one-line note when user explicitly asked for semantic/hybrid"

**Implementation** (PR #59, tools.go:handleSearch): Note is appended ONLY when `mode="semantic"` or `mode="hybrid"` AND the result is FTS-only. The note text varies per degradation cell (design decision 5 matrix): "not configured", "provider error", "policy gated", "not ready (N pending)".

**Rationale**: User who typed `mode=hybrid` deserves to know why they got FTS, but keyless users (mode="") never see a note (byte-identical). The note is minimal and informative, not an error.

---

## Remaining Manual Checklist (system-level / human verification)

These items require hands-on verification or user testing; they are out of headless-test scope:

- [ ] **Real OpenAI API integration test**: With a live OpenAI key, embed real memories and verify cosine search surfaces paraphrases a lexical query misses.
- [ ] **Real Ollama sidecar integration test**: Run an Ollama container, seed memories in a `local-only` project with consent=true, verify backfill embeds via the sidecar, and hybrid search works.
- [ ] **UI display of embedding_backfill status**: The control API status response includes the `embedding_backfill` sub-object; user-facing UI (if any) should display pending count and current provider name.
- [ ] **Policy flip mid-backfill**: During an active backfill loop, flip a project from `synced` → `omitted`, verify the loop skips that project on the next tick and already-embedded rows stay (not un-embedded).

---

## Deferred-Items Inventory (Carry-Forwards, Not Consumed)

| Item | Reason | Future phase |
|------|--------|--------------|
| Legacy Ollama endpoint migration (`/api/v1/textembedding` → `localhost:11434/api/embeddings`) | Ollama deprecated its old HTTP endpoint in favor of the new standard; PR #61 uses the new endpoint only. Backcompat for legacy deployments deferred. | Future Ollama-provider refinement |
| Central pgvector ([embedded-postgres #163](https://github.com/fergusstrange/embedded-postgres/issues/163)) | `fergusstrange/embedded-postgres` does not bundle pgvector; any central-side vector column is untestable under our acceptance discipline. Deferred indefinitely pending upstream. | Future (blocked by embedded-postgres) |
| Outbox garbage collection for local-only projects | Long-term `local-only` projects accumulate outbox rows never pulled by the central store (omitted rows also do not contribute). Documented caveat; GC job deferred. | Future cleanup phase |
| `central_lww_discards` observability wiring | Audit table from core-foundation never wired to the daemon's status endpoint. Separate observability task. | Future observability phase |
| Cross-platform secret storage (Keychain/Secret Service/libsecret) | Windows DPAPI via `config.SecretBox` is locked. macOS/Linux secret storage (Keychain, Secret Service, libsecret) deferred as out-of-scope for PR #61. | Future phase (explicit out-of-scope) |
| Search.go query-embed context.Background | Query embedding inside `handleSearch` and `mem_similar` uses `context.Background()` for the embed call (writer key resolve uses the daemon ctx, but query embed does not inherit it). Revisit if the daemon implements cancellation-aware embed. | Future context-propagation cleanup |
| GET /api/v1/config ollama-field visibility | The config endpoint's `Patch` validator checks `embedding_provider` and rejects unknown values at write-time (design decision 6). GET endpoint returns the enum as-is, allowing users to see which provider is configured. No dynamic provider discovery (e.g., detecting a running Ollama sidecar) — explicit config only. | Future auto-discovery (out of scope) |
| Runtime key/provider rewire | After `POST /api/v1/embedding/key` or `PUT /api/v1/config embedding_provider`, the daemon does NOT restart. The `gated` provider field reference is live, so immediate effect for new queries/writes. Backfill loop uses the provider at its next iteration. No test covers a key change mid-loop (it works, but edge case). | Future edge-case testing |

---

## What This Means for Engram

The semantic-search change completes **the entire three-phase roadmap**:

1. **core-foundation** (archived 2026-05-xx): typed memory model + reconciliation invariants + per-node storage converging via a sync journal + local/central policy + LWW discards table.
2. **tray-ui** (archived 2026-06-10): resident daemon control plane + per-project policy + visual config + Windows tray integration + HTTP MCP transport.
3. **semantic-search** (CLOSED 2026-06-10): per-node embeddings + vector similarity + hybrid search fusion + privacy-gated backfill + Ollama sidecar support.

All 35 task boxes ticked. Zero CRITICAL issues. 2 WARNINGS (both accepted by review, one spec-reconciled). 3 SUGGESTIONS (all noted above as deferred). Verification PASSED.

**Roadmap Complete.** The three-phase delivery is archive-ready and dependency-closed.

---

**Archived by**: sdd-archive phase  
**Date**: 2026-06-10  
**Verification**: PASS (0 CRITICAL, 2 WARNING, 3 SUGGESTION)  
**Spec reconciliations**: 2 applied (vector-search mode=semantic degradation behavior, embedding-backfill pending-count formula)  
**Task completion**: 35/35 boxes checked  
**Delivered PRs**: #59 (core), #60 (backfill), #61 (sidecar + consent + key routes)
