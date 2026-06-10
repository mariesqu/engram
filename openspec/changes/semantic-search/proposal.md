# Proposal: semantic-search — Per-Node Embeddings, Brute-Force Cosine, RRF Hybrid Search

> Phase 3, the FINAL roadmap phase. Builds on core-foundation (typed memory model + reconciliation) and tray-ui (control API + per-project policy + config). Artifact store: HYBRID.

## Intent

Today engram retrieves memories by **lexical match only**: `SearchMemoriesFiltered` (and the conflict-detector `FindCandidates`) run FTS5 BM25 over the SQLite shadow table. A memory phrased differently from the query — a paraphrase that shares no keywords with the target — is **invisible** to search even when it is the single most relevant record. The schema has carried `embedding BLOB`, `embedding_model TEXT`, `embedding_created_at TEXT` (localstore `schema.go:260-262`) and `domain.Record` has carried `Embedding []byte`, `EmbeddingModel *string`, `EmbeddingCreatedAt *time.Time` (`internal/domain/memory.go:73-75`) as **reserved-but-never-populated** columns since core-foundation. This change finally populates them and turns them into a search dimension.

This change gives engram **meaning-based retrieval** alongside its existing keyword retrieval: each node computes embeddings for its own memories via a pluggable `EmbeddingProvider` port, stores them locally, and `mem_search` gains an opt-in `mode` (`fts` | `semantic` | `hybrid`) that fuses lexical BM25 and vector cosine via Reciprocal Rank Fusion (RRF). Embeddings are **strictly per-node derived data** — they NEVER enter the sync journal, so the content-addressed `mutation_id` invariant from core-foundation stays sacred. Privacy is enforced at the embedding boundary: a project's per-node policy (`synced | local-only | omitted` from tray-ui) decides whether — and to which provider — its text may be sent.

Success looks like: a user searches for "how we keep the writer secret off disk" and `mode=hybrid` surfaces the memory titled "DPAPI-sealed config key" even though it shares no words with the query; an `omitted` or `local-only` project's text is **provably never** sent to a remote embedding API (asserted headlessly via a recording mock); and a user with no API key configured sees `mem_search` behave **byte-identically to today** because the feature degrades silently to FTS.

## Scope

### In Scope
- **`EmbeddingProvider` port** — a Go interface (`Embed`, `Dimensions`, `ModelName`) with concrete impls: `RemoteOpenAIProvider` (stdlib `net/http` JSON to `text-embedding-3-small`, 256-dim matryoshka), `OllamaSidecarProvider` (PR-2), and `NoopProvider` (FTS-only graceful degradation).
- **Local vector search** — app-side brute-force cosine over the existing `embedding BLOB` column: decode little-endian float32, compute cosine, sort top-K. No new dependency, no pgvector, no ANN index. Sub-millisecond at our scale (5k rows × 256 dims ≈ <1ms).
- **RRF hybrid retrieval** — fuse FTS5-BM25 ranks and cosine ranks via Reciprocal Rank Fusion (`k=60`), combining unbounded BM25 scores and bounded cosine scores without normalization.
- **On-write + backfill embedding** — embed each memory after `LocalWrite` (when policy permits), plus a resumable background backfill loop (`embedding IS NULL` predicate) reusing the syncer `Loop` pattern.
- **Embedding privacy gate** — before any provider call, check `GetPolicy(project)` (`internal/localstore/policy.go:67`); `omitted` is never embedded, `local-only` is never sent to a **remote** provider (local sidecar requires a SEPARATE explicit consent setting, PR-2), `synced` is embeddable with a configured provider.
- **`mem_search` `mode` parameter** — additive `mode` on the MCP tool and on `SearchFilter`; zero-value `mode` = today's FTS behavior exactly.
- **Config keys** — `embedding_provider` (enum) and the API key, sourced from `ENGRAM_EMBEDDING_KEY` env (never a flag) and optionally SecretBox-sealed at rest reusing the tray-ui (PR-③) `config.SecretBox` infrastructure (`internal/config/config.go:38`).

### Out of Scope (each = DEAD or a deliberate deferral, stated with cause)
- **Local pure-Go inference** — DEAD. Exploration confirmed every viable ONNX path (amikos-tech/pure-onnx, all-minilm-l6-v2-go, gomlx, hugot) requires either CGO at build time or a runtime DLL that breaks the single static binary. No maintained `CGO_ENABLED=0`-safe path exists as of June 2026. Deferred indefinitely, not in any PR.
- **pgvector / central-side vectors** — DEFERRED ENTIRELY. `fergusstrange/embedded-postgres` does not bundle pgvector (issue #163, no workaround/timeline), so any pgvector path is **untestable under our acceptance discipline**. Anything untestable under embedded-postgres is OUT. The `central_memories.embedding BYTEA` column stays reserved, unpopulated.
- **ANN indexes (HNSW / IVF)** — unneeded. Brute-force cosine is sub-ms at thousands of memories; ANN matters only at 100k+ rows.
- **Additional remote providers (Voyage, Gemini)** — out; OpenAI is the locked PR-1 provider, sidecar/ollama is PR-2. Others are future changes behind the same port.
- **Embedding sync** — out. Embeddings are per-node derived data; adding them to the journal payload would change the content-addressed `mutation_id` and break the reconciliation invariant. No side-channel sync either.
- **`viant/sqlite-vec`** — out for now (it is a future zero-schema-change drop-in upgrade path if the user base grows; adds a dependency we do not need today).
- **old_code "semantic" runner** — nothing to port. old_code's `SemanticRunner` was an LLM-judge CLI for conflict pairs, not embedding retrieval; the `embedding` columns there were reservation-only.

## Capabilities

### New Capabilities
- `embedding-provider`: pluggable `EmbeddingProvider` port (`Embed`/`Dimensions`/`ModelName`) with `RemoteOpenAIProvider`, `NoopProvider` (PR-1) and `OllamaSidecarProvider` (PR-2). Key via `ENGRAM_EMBEDDING_KEY`, optionally SecretBox-sealed.
- `vector-search`: app-side brute-force cosine over the existing `embedding BLOB` column + RRF hybrid fusion of BM25 and cosine. Zero new deps, fully testable under embedded-postgres.
- `embedding-backfill`: resumable, rate-limited background loop embedding unindexed rows (`embedding IS NULL`), policy-gated per row, idempotent.
- `embedding-privacy`: per-project policy gate at the embedding boundary — `omitted` never embedded; `local-only` never sent remotely; `synced` embeddable with a provider.

### Modified Capabilities
- `mem-search` (MCP tool + `localstore.SearchMemoriesFiltered` / `SearchFilter`): gains an additive `mode` parameter (`fts` default | `semantic` | `hybrid`). Zero-value = current FTS behavior, byte-identical.
- `config` (`internal/config`): gains `embedding_provider` + embedding-key handling, reusing the existing `SecretBox` Seal/Open infrastructure and the "env always wins, sealed-at-rest, never plaintext to disk" pattern.

## Approach

Each node embeds its **own** memories and queries its **own** vector index — embeddings are derived, never synced. After `LocalWrite` (and after sync pull), the daemon embeds eligible rows via the configured `EmbeddingProvider`; a background backfill loop catches up existing rows via the `embedding IS NULL` predicate. Search stays lexical-by-default: `mem_search` with no `mode` runs exactly today's FTS path; `mode=hybrid` runs FTS5 → ranked list A, brute-force cosine → ranked list B, then RRF-merges by `sync_id`. The privacy gate sits in front of every provider call: `GetPolicy(project)` decides eligibility, and `omitted`/`local-only` text never reaches a remote API. The embedding key follows the existing writer-key discipline (`ENGRAM_EMBEDDING_KEY` env wins; SecretBox-sealed at rest on Windows; never a flag, never plaintext to disk).

### Decision Table

| # | Decision | Resolution | Rationale / Flip-side |
|---|----------|------------|-----------------------|
| 1 | Embedding source (Crux A) | **Remote API — OpenAI `text-embedding-3-small` (stdlib `net/http`, zero new deps) behind `EmbeddingProvider`; ollama sidecar = PR-2; `NoopProvider` = FTS-only degradation** (LOCKED) | Local pure-Go inference is DEAD (every path = CGO or runtime DLL). OpenAI is widest-adopted at \$0.02/1M tokens. **Flip-side — provider outage / network failure**: the provider returns an error, the row is left with `embedding IS NULL`, the write still succeeds, and `mem_search` degrades to FTS. The feature is never on the write's critical path. |
| 2 | Vector store / similarity (Crux B) | **App-side brute-force cosine over the existing `embedding BLOB` column; no pgvector, no sqlite-vec, no ANN** | pgvector is untestable under embedded-postgres (issue #163). Brute-force is <1ms at 5k×256. Fully testable. **Flip-side — scale growth**: `viant/sqlite-vec` is a future drop-in with the SAME `embedding BLOB` schema if users reach 100k+ memories — a later PR, no schema change. |
| 3 | Embeddings × sync (Crux C) | **Per-node derived index; embeddings NEVER in the journal** | The `Mutation` payload is content-addressed into `mutation_id` (core-foundation invariant). Adding embeddings would break it. 6KB/memory × thousands = catastrophic for the journal, fine for a local index. **Flip-side — model skew across nodes**: each node queries only its own index, so per-node model differences are harmless; the `embedding_model` column records which model produced each vector. |
| 4 | Dimensions | **256 (matryoshka shortening of `text-embedding-3-small`)** (LOCKED) | 6× storage saving vs 1536 with ~5% MTEB drop — overkill avoided at thousands of memories. PR-2 adds an `embedding_dims` config so a different model/dim can be selected. |
| 5 | `mem_search` default mode | **`fts` remains the DEFAULT; `hybrid`/`semantic` are explicit opt-in via `mode`; silent degradation to `fts` when no provider/embeddings** (LOCKED) | Backward compatibility is a hard constraint: zero-value `mode` = today's behavior, byte-identical. No latency surprise for keyless users. |
| 6 | Privacy gate (Crux G) | **`omitted` NEVER embedded; `local-only` NEVER sent to a remote provider AND local-sidecar embedding requires a SEPARATE explicit consent setting (embedding consent ≠ sync policy); `synced` embeddable with a configured provider** (LOCKED) | This is the highest-stakes invariant. Embedding sends memory TEXT off the node — the gate is mandatory and checked before EVERY provider call (on-write and backfill). Embedding consent is decoupled from sync policy so a `local-only` user does not implicitly authorize a localhost sidecar. **Flip-side — policy flip after embedding**: flipping a `synced` project to `local-only`/`omitted` stops FUTURE remote embedding; already-computed local vectors are node-local derived data and are not "unpublished" (they never left the node). |
| 7 | Embedding key storage | **`ENGRAM_EMBEDDING_KEY` env (never a flag) + optionally SecretBox-sealed in config, REUSING the tray-ui (PR-③) `config.SecretBox` Seal/Open infra (`internal/config/config.go:38`)** | The writer-key pattern already gives us "env always wins; sealed-at-rest on Windows via DPAPI; `ErrNoSecretStore` → env-only elsewhere; never plaintext to disk; redacted in config responses". The embedding key reuses it verbatim — a new `encrypted_embedding_key` field, never a flag, never leaked in `--help`. **Flip-side — key removal**: with no env key and no sealed key, the provider resolves to `NoopProvider`; new writes get `embedding IS NULL`, search degrades to FTS, nothing errors. |
| 8 | Hybrid fusion | **Reciprocal Rank Fusion, `k=60`** | RRF operates on ranks, so unbounded BM25 and bounded `[0,1]` cosine combine directly without score normalization. Standard `k=60`. |
| 9 | Model-change re-embedding | **`embedding_model` column tracks the producing model; re-embed via a backfill predicate (`embedding_model <> ?` OR `IS NULL`)** | When the configured model/dim changes, the same idempotent backfill loop re-embeds stale rows by predicate — no separate migration tool. |

### Crux resolutions (the three from the exploration, now locked)
- **Crux A (source)**: Remote OpenAI for PR-1; sidecar PR-2; Noop = degradation. Local inference dead.
- **Crux B (vector store)**: app-side brute-force cosine, no pgvector, no new dep.
- **Crux C (sync)**: per-node derived; never in the journal.

## Affected Areas

| Area | Impact | Description |
|------|--------|-------------|
| `internal/embedding/` (new) | New | `EmbeddingProvider` port + `RemoteOpenAIProvider` (stdlib `net/http`) + `NoopProvider`; the privacy-gate helper that wraps a provider with `GetPolicy`. (`OllamaSidecarProvider` = PR-2.) |
| `internal/localstore/search.go` (`SearchFilter`, `SearchMemoriesFiltered`) | Modified | Add `Mode` to `SearchFilter` (zero-value = FTS); add cosine scan + RRF fusion path. Existing FTS path untouched when `Mode == ""`. |
| `internal/localstore/` (vector accessors, new) | New | Read embeddings (`SELECT sync_id, embedding ... WHERE embedding IS NOT NULL`), decode float32, brute-force cosine; write `embedding`/`embedding_model`/`embedding_created_at`. |
| `internal/localstore/schema.go` + `internal/domain/memory.go` | Reference only | Columns/fields already exist (schema `260-262`, domain `73-75`). **No schema migration** — only population. |
| Embedding loop / daemon wiring (`cmd/engram`) | Modified | On-write embed (post-`LocalWrite`) + background backfill ticker reusing the syncer `Loop` pattern. |
| `internal/config/config.go` | Modified | `embedding_provider` enum + `encrypted_embedding_key` field reusing `SecretBox` Seal/Open; `ENGRAM_EMBEDDING_KEY` env wins; redacted in responses. PR-2 adds `embedding_dims` + sidecar consent setting. |
| MCP `mem_search` tool handler | Modified | Surface the additive `mode` parameter; map to `SearchFilter.Mode`; default empty = FTS. |
| `internal/localstore/candidates.go` (`FindCandidates`) | Modified (PR-2) | Add a cosine pass unioned with the FTS candidate set for paraphrase-aware conflict detection. |
| MCP `mem_similar` tool (new, PR-2) | New | Explicit similarity search by `sync_id` + `project` + `limit`, top-K by cosine. |
| `old_code/` | Reference only | NEVER modified. Nothing to port (semantic runner ≠ embeddings). |

## Risks

| Risk | Likelihood | Mitigation |
|------|------------|------------|
| **Privacy-gate correctness (HIGHEST STAKES)** | Med | The gate is the single most important invariant: a bug sends `omitted`/`local-only` text to a remote API. Mitigation: the gate is a single choke-point function called before EVERY provider call (on-write AND backfill); proven headlessly with a **recording mock** provider that asserts it received NOTHING for omitted/local-only rows; embedding consent is a SEPARATE setting from sync policy so consent is explicit. |
| Provider API drift (OpenAI response shape) | Med | Provider is isolated behind `EmbeddingProvider`; the HTTP/JSON shape lives in one file; a contract test pins the request/response shape against a fixture. A drift breaks only embedding, never writes or FTS. |
| Backfill cost / rate-limit blowup | Low | Batch 100 texts/request, 1s between batches, resumable via `embedding IS NULL`. 10k memories ≈ \$0.04 total. On provider error: skip row, log, continue — never abort the loop. |
| The 400-line PR-1 squeeze | Med | PR-1 carries port + OpenAI + Noop + gate + on-write + backfill + cosine + RRF + config keys + tests. If the adversarial review flags it over budget, split the backfill loop or the config keys into a PR-1b slice; sidecar/`mem_similar` stay in PR-2 regardless. |
| Cosine performance regression if scale grows unexpectedly | Low | Brute-force is <1ms at thousands; `viant/sqlite-vec` is the documented same-schema upgrade path with no migration. |
| `embedding BLOB` decode mismatch (endianness/dim) | Low | Store and decode little-endian float32 consistently; a round-trip unit test (encode → BLOB → decode → cosine==1.0 with itself) guards the codec; `embedding_model`/dim recorded per row. |

## Rollback Plan

The feature is **purely additive** and degradation-first:
- **Per-PR**: each of the 2 PRs is independently revertible (chained stacked-to-main, adversarially review-gated, ~≤400 lines).
- **No schema migration to undo**: the `embedding*` columns already existed (reserved since core-foundation). Reverting the code leaves them populated-but-unread — inert. No down-migration needed.
- **Provider off-switch**: with no `ENGRAM_EMBEDDING_KEY` and no configured provider, the engine resolves to `NoopProvider` — **this is exactly today's engine**: writes get `embedding IS NULL`, `mem_search` runs FTS. Removing the key is a complete rollback at runtime.
- **`mem_search` compatibility**: `mode` is additive and zero-value = today's FTS. No existing caller breaks.
- **Sync untouched**: embeddings never enter the journal, so reconciliation/convergence is unaffected by this change in either direction.

## Dependencies

- **ZERO new Go modules.** OpenAI provider uses stdlib `net/http` + `encoding/json`. Cosine + RRF are hand-written Go. Privacy gate reuses existing `localstore.GetPolicy`. Key storage reuses existing `config.SecretBox`. (Verified: no new import sneaks in.)
- Pure Go, `CGO_ENABLED=0`, single static binary, no Docker — preserved throughout.
- Acceptance tests run under embedded-postgres unchanged (nothing pgvector-dependent).

## Success Criteria

> Contract for spec / design / tasks. All must be provable, most headlessly.

- [ ] **Hybrid beats-or-equals FTS on a paraphrase fixture**: a query that shares NO words with the target memory is found by `mode=hybrid` (via cosine) and MISSED by `mode=fts`; the test asserts the target appears in hybrid results and is absent from FTS results. Hybrid never ranks a true FTS-only match worse than FTS alone.
- [ ] **Privacy gate provable headlessly**: with a recording mock `EmbeddingProvider`, an `omitted` project and a `local-only` project (remote provider configured) produce ZERO calls carrying their text — assert the mock received nothing for those projects, while a `synced` project's text IS embedded. Covered for BOTH the on-write path and the backfill loop.
- [ ] **Degradation paths**: (a) no key configured → `NoopProvider`, `mem_search` returns FTS results, no error; (b) provider returns an error mid-write → write still succeeds, row left `embedding IS NULL`; (c) `mode=hybrid` with no embeddings present → silently returns FTS results.
- [ ] **Backfill idempotent + resumable**: running the backfill twice produces no duplicate embeddings and no churn; interrupting it and re-running picks up exactly the remaining `embedding IS NULL` rows; a model change re-embeds stale rows via the `embedding_model` predicate.
- [ ] **`mem_search mode=fts` byte-identical to today**: the default (zero-value `mode`) path returns results identical to the pre-change `SearchMemoriesFiltered` for the same inputs; existing search tests stay green unchanged.
- [ ] **Cosine codec round-trip**: a vector encoded to `embedding BLOB` and decoded back yields cosine 1.0 with itself; little-endian float32 layout verified.
- [ ] **Key safety**: the embedding key is sourced from `ENGRAM_EMBEDDING_KEY` (env wins), optionally SecretBox-sealed at rest, NEVER returned by the config endpoint (redacted), NEVER leaked in `--help`.

## Delivery Plan — 2 Chained PRs (stacked-to-main, each adversarially review-gated, ~≤400 lines)

> Each PR targets `main` and merges in order before the next begins (LOCKED: stacked-to-main). Every PR passes a fresh-context adversarial review.

**PR 1 — `semantic-search/embeddings-core` (~350-400 lines)** — the engine
- `EmbeddingProvider` port + `RemoteOpenAIProvider` (stdlib `net/http`, 256-dim) + `NoopProvider`.
- Privacy gate (single choke-point on `GetPolicy`) in front of every provider call.
- On-write embedding (post-`LocalWrite`) + resumable, rate-limited backfill loop (`embedding IS NULL`).
- App-side brute-force cosine over `embedding BLOB` + RRF (`k=60`) hybrid fusion wired into `SearchMemoriesFiltered` via additive `SearchFilter.Mode`; `mem_search` `mode` parameter.
- Config keys: `embedding_provider` + `encrypted_embedding_key` reusing `config.SecretBox`; `ENGRAM_EMBEDDING_KEY` env wins.
- Tests: paraphrase fixture, recording-mock privacy gate (on-write + backfill), degradation paths, cosine codec round-trip, `mode=fts` regression, key redaction. (Validates ALL PR-1 success criteria.)
- *If review flags >400 lines*: split the backfill loop or config keys into a PR-1b slice.

**PR 2 — `semantic-search/sidecar-and-similar` (~300-400 lines)** — the extensions
- `OllamaSidecarProvider` (HTTP to `localhost:11434/api/embeddings`) behind the same port.
- SEPARATE explicit embedding-consent setting for local-sidecar use on `local-only` projects (consent ≠ sync policy).
- `mem_similar` MCP tool (top-K by cosine from a `sync_id`).
- `FindCandidates` semantic pass (cosine union with FTS candidates) for paraphrase-aware conflict detection.
- `embedding_dims` config (select model/dim).
- Tests: sidecar provider, consent-gated local-only embedding, `mem_similar` ranking, `FindCandidates` paraphrase recall.

## Disagreements / Tightenings vs. the Exploration

- **Key storage TIGHTENED to a concrete reuse of `config.SecretBox`** (exploration said "stored like writer_key" loosely). Verified the PR-③ infra at `internal/config/config.go:38`: `SecretBox` Seal/Open, `ErrNoSecretStore`, `encrypted_writer_key` redaction pattern. The embedding key reuses this verbatim with a new `encrypted_embedding_key` field — env always wins, sealed-at-rest on Windows, env-only elsewhere, redacted in responses, never a flag. This is a decision the exploration left open; it is now LOCKED.
- **PR-4 (Crux A central-side) and A2/A3 framing collapsed**: the exploration enumerated A1-A4 as options; locked to A1 (OpenAI) for PR-1 + A3 (sidecar) for PR-2, A2 (local inference) DEAD, A4 (central-only) SKIP. No ambiguity remains.
- **`mode` is now an additive field on the existing `SearchFilter` struct** (exploration said "extend the signature minimally"). Verified `SearchFilter` at `internal/localstore/search.go:13` — adding a `Mode` field is strictly additive and zero-value-safe, which is cleaner than a new parameter and preserves every existing caller. Tightening, not a disagreement.
- **No schema migration at all**: the exploration said "schema unchanged (already has columns)"; verified concretely (schema `260-262`, domain `73-75`) — this change has ZERO migration surface, which strengthens the rollback story (reverting leaves columns inert, no down-migration).
- **Embedding consent decoupled from sync policy is elevated to a top-stakes invariant** (exploration listed it as decision-5 detail). Locked user decision 4 makes it explicit: local-sidecar embedding on `local-only` projects requires a SEPARATE consent setting. Flagged here as the single highest-stakes correctness invariant of the change.
