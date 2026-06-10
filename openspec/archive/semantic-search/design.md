# Design: semantic-search — Gate-as-Wrapper Provider, NULL-as-Queue Backfill, Brute-Force Cosine + RRF Hybrid

> Module path: `github.com/mariesqu/engram` (go.mod:1). Grounded in the CURRENT tree
> (`old_code/` is read-only reference only). Builds on core-foundation (typed memory
> model, reconciliation, the reserved `embedding*` columns) and tray-ui (control API,
> per-project `GetPolicy`, `config.SecretBox`, the runtime adapters in `cmd/engram/daemon.go`).

## Technical Approach

Populate the **reserved-but-empty** embedding columns (`memories.embedding BLOB`,
`embedding_model TEXT`, `embedding_created_at TEXT` — schema.go:260-262; `domain.Record`
fields memory.go:73-75) and turn them into a search dimension, WITHOUT touching the write
critical path, the sync journal, or the existing FTS behavior.

Three structural commitments make the change safe and un-bypassable:

1. **The privacy gate is a WRAPPER `EmbeddingProvider`, not a helper call.** Raw providers
   (`RemoteOpenAIProvider`, `NoopProvider`) are NEVER handed to callers. The constructor
   returns a `gatedProvider` that runs `GetPolicy(project)` before delegating to the inner
   provider. There is exactly ONE place text can leave the node — inside the gate — and it
   is structurally impossible to call a raw provider because the wiring never exposes one.

2. **`embedding IS NULL` IS the work queue.** The write path is UNTOUCHED. A new row lands
   with `embedding = NULL` (the column default). There is no on-write embed call, no new
   in-process queue, no extra state. After a successful write the handler nudges the backfill
   loop's `Trigger()` channel (exactly the `triggerSync(loop)` pattern already in `handleSave`);
   the loop SELECTs `embedding IS NULL` rows, gates each by policy, embeds, and UPDATEs. A crash,
   a provider outage, or a missing key simply leaves rows NULL — they are retried on the next tick.

3. **Embeddings are per-node derived data, NEVER in the journal.** The `embedding` UPDATE is a
   single-statement, single-row write that does NOT go through `localWriteLocked`, produces NO
   `Mutation`, and enqueues NO outbox row. The content-addressed `mutation_id` invariant stays
   sacred (core-foundation). Each node embeds and queries its OWN index.

Search stays lexical-by-default: `SearchFilter.Mode == ""` runs today's exact FTS path
(search.go:42-99), byte-identical. `mode=hybrid`/`semantic` is explicit opt-in: embed the
query, brute-force cosine over the decoded BLOBs, RRF-merge (`k=60`) by `sync_id`, top-K.

Zero new Go modules: OpenAI provider is stdlib `net/http` + `encoding/json`; cosine + RRF are
hand-written; the gate reuses `localstore.GetPolicy`; the key reuses `config.SecretBox`.

## Architecture Decisions

| # | Decision | Choice | Rejected | Rationale |
|---|----------|--------|----------|-----------|
| 1 | Package layout & gate locus | New `internal/embedding`: port + providers + the gate as a **wrapper provider**. The gate wraps the inner provider so raw providers are never handed out. `Policy` lookup via a small `PolicyChecker` port (satisfied by `*localstore.Store`) so `embedding` does NOT import `localstore` upward. | Port in `domain`; gate as a free `func mayEmbed(...)` helper called at each call site | A free helper is bypassable — a future call site can forget it. A wrapper makes the gate the ONLY edge text crosses: the daemon wiring constructs `gated := embedding.NewGated(inner, store)` and hands `gated` everywhere; no code path ever holds an ungated provider. `domain` stays dependency-free (it must not import a provider). |
| 2 | On-write embed mechanics | Write marks NOTHING new; `embedding IS NULL` (column default) IS the queue. After a successful `AddObservation`, `handleSave` calls `loop`-style `embedBacklog.Trigger()` (nil-safe, exactly like `triggerSync`). Embedding happens in the backfill loop, never inline. | (a) Inline/sync embed inside `handleSave`; (b) a new in-process channel/queue of pending IDs | Inline embed puts a network call on the write critical path — a 5xx or timeout would fail or stall a `mem_save`. A separate queue is NEW durable state that can drift from the DB and is lost on crash. NULL-as-queue is zero new state, crash-safe (NULL survives restart), and reuses the proven `Trigger()` debounce. The write path (observations.go) is literally unchanged. |
| 3 | Backfill loop topology | A dedicated `embedding.Loop` mirroring `syncer.Loop` (Interval + `Trigger()` + `Stop()` + backoff + Debounce). Batch `SELECT sync_id, project, type, title, content FROM memories WHERE embedding IS NULL AND deleted_at IS NULL [AND (embedding_model IS NULL OR embedding_model <> ?)] LIMIT N`. Per-row `GetPolicy` (cached map lookup) — NOT a SQL JOIN. The `embedding` UPDATE does **NOT** take `s.mu`. | (a) Bare ticker goroutine; (b) SQL JOIN against `project_policy`; (c) take `s.mu` for the UPDATE | The `syncer.Loop` is the house pattern for exactly this shape (debounced trigger + periodic + backoff + clean `Stop`) — copying it gives lifecycle parity and a known-correct shutdown. JOIN is rejected for the SAME reason tray-ui rejected it: project lives only in the row, `GetPolicy` is a cached hash lookup, and the read-time default depends on runtime central state a JOIN can't see (policy.go:45-50). The `s.mu` mutex guards the multi-statement **read-modify-write** of `localWriteLocked` (version pre-read → write → PK resolve, observations.go:113-117); a lone single-row `UPDATE ... SET embedding=? WHERE sync_id=? AND embedding IS NULL` is atomic in SQLite by itself and touches columns no reconciliation logic reads — taking `s.mu` would needlessly serialize embedding behind every write. The `AND embedding IS NULL` clause in the UPDATE makes it idempotent under a concurrent write to the same row. |
| 4 | Cosine + storage codec | `[]float32 ↔ BLOB` little-endian codec lives in `internal/localstore/vector.go` (next to the rows it serializes). Vectors are stored **L2-normalized** so cosine = dot product (no per-query magnitude division). Query flow: embed query → `SELECT sync_id, embedding WHERE embedding IS NOT NULL [policy/project filters]` → dot-product each → sort desc → take top-K → RRF-merge with the FTS rank list by `sync_id`. Deterministic tie-break: equal score → `sync_id` ascending. | Store raw vectors + divide by magnitude per query; codec in `internal/embedding`; tie-break by `id` | Normalizing at write time turns cosine into a single dot product — simpler and faster on the hot path. The codec belongs in `localstore` because it is a column (de)serializer, like `scanRecord`; `embedding` should not know the storage layout. `sync_id` tie-break (not the local autoincrement `id`) is node-portable and matches how RRF fuses by `sync_id`. |
| 5 | Degradation matrix | Silent degradation to FTS in every failure cell; the ONLY honest signal is an optional one-line `(semantic search unavailable: <reason>; showing keyword results)` note appended to `mem_search` output when the user explicitly asked for `mode=semantic`/`hybrid` and got FTS-only. `mode=fts` (and zero-value) NEVER emits a note. | A `degraded:true` structured field; erroring on degradation; always-on note | The proposal's hard constraint is "keyless users see byte-identical behavior" — so a note must NEVER appear on the default path. But a user who TYPED `mode=hybrid` and silently got FTS deserves to know why (otherwise "hybrid is broken" bug reports). The note is text-only, opt-in by the user's own `mode` request, and carries a terse reason. See the per-cell table below. |
| 6 | Config plumbing | `fileConfig` gains `embedding_provider string` (enum: `""`/`openai`/`noop`; `ollama` reserved for PR-2) and `encrypted_embedding_key string` (base64 SecretBox blob, mirrors `encrypted_writer_key`). `ENGRAM_EMBEDDING_KEY` env ALWAYS wins (resolved AFTER flag.Parse, never a flag → no `--help` leak). `RedactedConfig` gains `EmbeddingProvider` + `EmbeddingKeySet bool` (key value NEVER returned). `ConfigPatch` gains `EmbeddingProvider *string` with **write-time enum validation in `handleConfigPut`** (the PR-⑥ brick lesson). **Startup-fatal on unrecognised enum** (mirrors `transport` validation): daemon reads `config.json`, finds unknown `embedding_provider` → log error + refuse to start. `GET /status` gains `embedding_backfill` sub-object `{"pending": int, "provider": string}` (count of `embedding IS NULL` live rows + current `ModelName()`); `EmbeddingProvider` flat field dropped from status in favour of the sub-object. `POST /api/v1/embedding/key` + `DELETE /api/v1/embedding/key` (WithAuthAndOrigin) are **deferred to PR-2** — PR-1 key sourcing is env+file only. | Put the key in a flag; validate the enum only at startup; surface nothing in status | The PR-⑥ lesson (config_mutate.go:81-89) is explicit: an invalid enum persisted via PUT hard-errors the NEXT startup — so the enum MUST be validated write-time AND at startup. `embedding_backfill` sub-object (spec authority) is more extensible than a flat `EmbeddingPending` int and adds the provider name alongside the count — minimal overhead, no design change elsewhere. Key handling is a verbatim reuse of the `encrypted_writer_key` discipline (config.go:54-67, daemon.go:239-282, Reconnect seal at daemon.go:882-893). |
| 7 | Model-change story | `embedding_model` column records the producing model per row. The backfill predicate is `embedding IS NULL OR embedding_model IS NULL OR embedding_model <> ?currentModel`. A model/dim change re-embeds stale rows through the SAME idempotent loop — no migration tool. Decode guards on length: a stored BLOB whose `len/4 != currentDims` is treated as stale (re-embed) and is NEVER cosine-compared against a different-dim query vector. | A separate re-embed migration; comparing mixed-dim vectors | One predicate + one loop is the whole story; reusing the backfill loop means model change is "just more NULL-equivalent rows." Length-guarding the decode prevents a silent garbage-cosine when dims change mid-flight before the re-embed catches up. |
| 8 | Hybrid fusion (LOCKED) | Reciprocal Rank Fusion, `k=60`: `score(d) = Σ 1/(k + rank_i(d))` over the FTS list and the cosine list, ranks 1-based. Fuse by `sync_id`; a doc in only one list contributes one term. Sort by fused score desc, tie-break `sync_id` asc, take top-K. | Score normalization + weighted sum; CombSUM | RRF operates on RANKS, so unbounded BM25 and bounded `[0,1]` cosine combine with no normalization or tuning. `k=60` is the standard. Locked in the proposal. |

## Package & Module Layout

```
internal/embedding/                 # NEW — zero new deps
├── provider.go        # EmbeddingProvider interface (Embed, Dimensions, ModelName);
│                      #   PolicyChecker port (GetPolicy(project) (Policy, error));
│                      #   NoopProvider (Dimensions=0, Embed→nil,nil)
├── gated.go           # gatedProvider WRAPPER: the single privacy choke-point.
│                      #   NewGated(inner, checker, consent) → EmbeddingProvider
├── openai.go          # RemoteOpenAIProvider: stdlib net/http POST to
│                      #   /v1/embeddings, model text-embedding-3-small, dimensions=256
├── loop.go            # embedding.Loop: mirrors syncer.Loop (Interval/Trigger/Stop/backoff);
│                      #   batch SELECT embedding IS NULL → gate → embed → UPDATE
└── *_test.go          # recording-mock provider, httptest OpenAI fixture, cosine/RRF units

internal/localstore/
├── vector.go          # NEW: encodeVector([]float32)→[]byte, decodeVector([]byte,dims)→[]float32,
│                      #   cosineNormalize, dot; SelectEmbeddable(model,limit), SelectVectors(filter),
│                      #   UpdateEmbedding(syncID, vec, model, ts) — the no-s.mu single-row UPDATE
├── search.go          # MODIFIED: SearchFilter gains Mode; SearchMemoriesFiltered branches
│                      #   on Mode (zero-value = today's FTS path, untouched)
├── schema.go          # REFERENCE ONLY — columns already exist (260-262). NO migration.
└── policy.go          # REFERENCE ONLY — GetPolicy is the gate's PolicyChecker

internal/config/config.go            # MODIFIED: embedding_provider + encrypted_embedding_key
                                     #   fields, Redact, Patch; reuse SecretBox
internal/controlapi/
├── config_mutate.go   # MODIFIED: embedding_provider enum write-time validation
└── server.go          # MODIFIED: Status gains embedding_backfill sub-object
                       #   {pending int, provider string} (spec authority; replaces
                       #   flat EmbeddingPending+EmbeddingProvider);
                       #   RedactedConfig.EmbeddingProvider + EmbeddingKeySet bool

cmd/engram/
├── daemon.go          # MODIFIED: resolve embedding provider/key; build gated provider;
│                      #   construct embedding.Loop; Start in both runDaemonWithIO and
│                      #   runDaemonHTTP; daemonComponents.Close() stops it
└── tools.go           # MODIFIED: handleSave nudges the embed loop after a write;
                       #   handleSearch surfaces `mode`; query embedding via gated provider
```

## The Privacy Gate — Wrapper Provider (HIGHEST-STAKES INVARIANT)

```go
// internal/embedding/provider.go
type EmbeddingProvider interface {
    Embed(ctx context.Context, project string, texts []string) ([][]float32, error)
    Dimensions() int
    ModelName() string
}

type PolicyChecker interface { // satisfied by *localstore.Store
    GetPolicy(project string) (localstore.Policy, error)
}

// internal/embedding/gated.go — the ONLY edge text can cross.
type gatedProvider struct {
    inner   EmbeddingProvider // RemoteOpenAIProvider or NoopProvider — never exposed
    checker PolicyChecker
    remote  bool // true when inner sends text off-node (OpenAI); false for a local sidecar
    consent bool // PR-2: separate sidecar-consent flag, decoupled from sync policy
}

func (g *gatedProvider) Embed(ctx context.Context, project string, texts []string) ([][]float32, error) {
    pol, err := g.checker.GetPolicy(project)
    if err != nil { return nil, err }
    switch pol {
    case localstore.PolicyOmitted:
        return nil, ErrEmbeddingGated          // NEVER embed — omitted text never leaves
    case localstore.PolicyLocalOnly:
        if g.remote { return nil, ErrEmbeddingGated } // never to a REMOTE provider
        if !g.consent { return nil, ErrEmbeddingGated } // PR-2 sidecar requires explicit consent
    }
    return g.inner.Embed(ctx, project, texts) // synced (or consented-local) → delegate
}
```

The daemon constructs ONE `gatedProvider` and passes it to BOTH the embed loop and the
query path. No code anywhere holds `inner`. This is the structural guarantee behind the
recording-mock proof: the mock is the `inner`; the test asserts it received NOTHING for
`omitted`/`local-only` projects on BOTH the on-write nudge and the backfill loop.

## On-Write Path — write untouched, NULL is the queue

```
handleSave:
    project := resolveSaveProject(...)
    if GetPolicy(project) == Omitted { refuse }          // existing tray-ui guard
    AddObservation(...)                                   // UNCHANGED — embedding stays NULL
    triggerSync(loop)                                     // existing
    embedLoop.Trigger()                                   // NEW — nil-safe, debounced
```

`AddObservation`/`localWriteLocked` are NOT modified. The row's `embedding` column is NULL by
schema default. The loop is what fills it. If `embedLoop` is nil (Noop provider / no key), the
`Trigger()` is a no-op and rows stay NULL forever — which is exactly today's engine.

## Backfill Loop — mirrors syncer.Loop

```
embedding.Loop.run():
    on tick OR Trigger (debounced):
        rows := store.SelectEmbeddable(currentModel, batchSize)   // embedding IS NULL OR model<>cur
        if len(rows) == 0 { idle until next tick }                // zero-rows = no work, no error
        for each batch of <=batchSize:
            // gate is INSIDE provider.Embed — one GetPolicy per project group
            vecs, err := gated.Embed(ctx, project, texts)
            if errors.Is(err, ErrEmbeddingGated) { skip these rows, continue } // policy denied
            if err != nil { log, backoff (retryable), leave rows NULL, continue } // 5xx/timeout
            for each (row, vec): store.UpdateEmbedding(row.SyncID, normalize(vec), currentModel, now)
            sleep(perBatchPause)                                   // rate limit
```

**Rate-limit numbers (PR-1 defaults, all overridable via Config zero-value defaults like syncer):**
batch size `100` texts/request; `perBatchPause` `1s` between batches; loop `Interval` `60s`;
`Debounce` `1s`; backoff `BackoffMin 1s` / `BackoffMax 2m` (reuse `syncer.applyDefaults` shape).
10k rows ≈ 100 batches ≈ ~100s wall + ~$0.04. On any provider error the loop NEVER aborts — it
backs off and the NULL rows are retried next pass (resumable by construction).

**`s.mu` analysis (decision 3, expanded):** `AddObservation` holds `s.mu` across version
pre-read → `localWriteLocked` → PK resolve because that is a multi-statement read-modify-write
where an interleaved `ApplyPulled` would corrupt the LWW version (observations.go:113-134).
`UpdateEmbedding` is a single statement — `UPDATE memories SET embedding=?, embedding_model=?,
embedding_created_at=? WHERE sync_id=? AND embedding IS NULL` — atomic in SQLite, touches only
derived columns that NO reconciliation path reads, and the `AND embedding IS NULL` guard makes it
a safe no-op if a concurrent re-write of the same row already cleared/filled it. It therefore does
NOT take `s.mu`. (WAL + `SetMaxOpenConns(1)` already serialize the physical write.)

## Lifecycle Wiring (cmd/engram/daemon.go)

- **Resolve** (in `runDaemonCmd`, mirroring the writer-key chain at daemon.go:239-282):
  `embedding_provider` flag-less; `ENGRAM_EMBEDDING_KEY` env wins, else decrypt
  `encrypted_embedding_key` via `config.NewSecretBox().Open(...)`, else Noop. Decrypt failure →
  log warning, fall back to Noop (NEVER crash — same discipline as the writer key).
- **Build** (in `buildDaemon`): construct `inner` (`RemoteOpenAIProvider` when key+provider
  resolve, else `NoopProvider`); `gated := embedding.NewGated(inner, store, remote=true)`;
  `embedLoop := embedding.NewLoop(store, gated, embedding.Config{...})` (nil when Noop, so the
  whole feature is inert with no key). Pass `embedLoop` into `registerTools` so `handleSave` can
  `Trigger()` it, and into `handleSearch` (via the gated provider) for query embedding.
- **Start**: `embedLoop.Start(ctx)` in BOTH `runDaemonWithIO` (line ~388) and `runDaemonHTTP`
  (line ~436), right beside `components.loop.Start(ctx)`.
- **Stop**: `daemonComponents.Close()` (daemon.go:136-143) gains `if d.embedLoop != nil {
  d.embedLoop.Stop() }` BEFORE `store.Close()` — `Stop()` blocks until the goroutine exits
  (syncer.Loop:179-193 contract), so the store is never closed under an in-flight UPDATE.

## Search Flow (mode-aware SearchMemoriesFiltered)

```go
type SearchFilter struct {
    Type, Scope, TopicKey string
    Mode string // NEW: "" (=fts, today) | "fts" | "semantic" | "hybrid"
}
```

- `Mode == "" || "fts"` → today's exact path (search.go:53-99). Byte-identical. No query embed.
- `Mode == "semantic"` → embed the query (gated, by the QUERY's project), cosine top-K, no FTS.
- `Mode == "hybrid"` → run BOTH lists, RRF-merge by `sync_id`, top-K.

**Query-project policy (resolved here):** the QUERY is gated by the SAME `GetPolicy` as a write.
An `omitted` query project → the gate refuses to embed the QUERY → semantic/hybrid silently
degrade to FTS for that query (we will not send the query text of an omitted project to a remote
API either). `local-only` query + remote provider → same degrade-to-FTS. This is consistent: the
gate is symmetric for write text and query text.

Cosine candidates come from `SELECT sync_id, embedding FROM memories WHERE embedding IS NOT NULL
AND deleted_at IS NULL [+ project/type/scope filters mirroring the FTS WHERE]`, decoded with the
length-guard (decision 7), dot-producted against the normalized query vector.

## Degradation Matrix (decision 5)

| Cell | Behavior | User-visible note? (only when user asked semantic/hybrid) |
|------|----------|-----------------------------------------------------------|
| No key / Noop provider | `mem_search` runs FTS; writes leave `embedding NULL` | "semantic search unavailable: not configured; showing keyword results" |
| Provider 5xx (backfill) | Row left NULL, loop backs off, retries next tick; write already succeeded | (write path: none — `mem_save` succeeds silently) |
| Provider 5xx / timeout (query embed) | Degrade that query to FTS | "semantic search unavailable: provider error; showing keyword results" |
| Query project gated (omitted / local-only+remote) | Query not embedded; FTS only | "semantic search unavailable for this project's policy; showing keyword results" |
| Zero embedded rows yet (`mode=hybrid`, backfill not caught up) | Cosine list empty → RRF = FTS list | "semantic results not ready (N pending); showing keyword results" |
| `mode=semantic`, no vectors at all | Empty cosine, empty result set → fall back to FTS rather than return nothing | "semantic results not ready; showing keyword results" |
| `mode=fts` or zero-value | Today's FTS, byte-identical | NEVER (hard constraint) |

The note is a single trailing line appended to the existing `mem_search` text output
(tools.go:735-758) — never a structured error, never on the default path.

## Testing Strategy (per PR — headless; NO test may require a real API key)

| PR | Layer | Approach |
|----|-------|----------|
| 1 | Privacy proof (unit) | **Recording-mock** `EmbeddingProvider` as the `inner` of `gatedProvider`. Assert: `omitted` project → mock received ZERO texts; `local-only`+`remote=true` → ZERO; `synced` → received exactly the row texts. Cover BOTH the on-write nudge AND the backfill loop (same mock, two drivers). |
| 1 | OpenAI contract (httptest) | `httptest.Server` returning a canned `text-embedding-3-small` 256-dim response; assert request shape (model, `dimensions:256`, `Authorization: Bearer`) and that the JSON unmarshal maps to `[]float32`. Pins the response-shape contract; drift breaks only this test. |
| 1 | Cosine + RRF (unit) | Hand-computed vectors: codec round-trip (`encode→BLOB→decode→dot==1.0` with itself, little-endian asserted byte-for-byte); cosine ranks on orthogonal/parallel vectors; RRF on two hand-made rank lists with hand-computed fused scores + `sync_id` tie-break. |
| 1 | Hybrid-beats-FTS (acceptance) | Paraphrase fixture: seed two rows, one sharing NO query words with the target. Seed the target's `embedding` via the **mock** provider (deterministic vector close to the query's mock vector). Assert `mode=hybrid` surfaces the target; `mode=fts` does NOT. NO real API. |
| 1 | Backfill idempotency/resume/rate | Run loop twice → no churn (second pass selects zero rows); interrupt mid-batch (cancel ctx) then re-run → picks up exactly the remaining NULL rows; model-change predicate re-embeds stale rows; assert `perBatchPause` is honored (fake clock or call-count). |
| 1 | Degradation matrix (table-test) | One table per cell of decision-5: assert FTS results returned, no error, and the correct (or absent) trailing note. Includes `mode=fts` byte-identical regression against pre-change `SearchMemoriesFiltered`. |
| 1 | Key safety | `ENGRAM_EMBEDDING_KEY` wins; `Redact()` never returns the key (`EmbeddingKeySet bool` only); `--help` does not contain the key (extend the existing `TestRun_DaemonHelp_DoesNotLeak*`); PUT with bad `embedding_provider` enum → 400 (the PR-⑥ brick-prevention test). |
| 1 | Acceptance (full daemon) | Real temp SQLite store + a **mock provider via env/wiring**, end-to-end: write → loop embeds → `mem_search mode=hybrid` returns the seeded paraphrase target. No network. |
| 2 | Sidecar + consent | `OllamaSidecarProvider` via `httptest`; `local-only` project embeds ONLY with the separate consent flag set (`remote=false, consent=true`), and is refused without it. |
| 2 | mem_similar / FindCandidates | top-K-by-cosine ranking from a `sync_id`; `FindCandidates` cosine-union recalls a paraphrase the FTS candidate set misses. |

## PR-1 / PR-1b / PR-2 Seam (pre-authorized cut)

PR-1 carries: port + gate + OpenAI + Noop + on-write nudge + backfill loop + cosine + RRF +
`SearchFilter.Mode` + `mem_search mode` + config keys + status counts + all PR-1 tests.

**If the adversarial review flags >400 lines, cut at the loop boundary (pre-authorized PR-1b):**
- **PR-1** keeps: port, gate, OpenAi, Noop, codec (`vector.go`), cosine + RRF, `SearchFilter.Mode`,
  `mem_search mode`, config keys + enum validation + key safety, the **on-write nudge is a no-op
  stub** (provider present, loop absent), and the SEARCH-side tests (paraphrase via *manually
  seeded* embeddings, codec, RRF, degradation, key safety). This proves the read path end-to-end
  with hand-seeded vectors and ships the whole gate.
- **PR-1b** adds: the `embedding.Loop` (backfill + the real on-write `Trigger`), `SelectEmbeddable`,
  the rate-limit numbers, `Status.EmbeddingPending`, and the backfill idempotency/resume/rate +
  recording-mock-on-the-LOOP tests. The gate's recording-mock proof for the on-write/loop path
  moves here with the loop.
- **PR-2** is unchanged: sidecar + consent + `mem_similar` + `FindCandidates` cosine pass + `embedding_dims`.

This seam is clean because the gate, codec, and search math (PR-1) have no dependency on the loop
(PR-1b) — the loop is purely the *producer* of vectors the search path consumes.

## Failure Modes & Mitigations

| Failure | Effect | Mitigation |
|---------|--------|------------|
| **Gate bug sends omitted/local-only text remotely (HIGHEST STAKES)** | Privacy breach | Gate is a WRAPPER — raw providers are NEVER handed out, so no call site can bypass it (decision 1). Recording-mock asserts ZERO texts for omitted/local-only on both write and loop. |
| Provider 5xx / timeout on a write | — | Embed is NEVER on the write path (decision 2). Write succeeds; row stays NULL; loop retries with backoff. |
| OpenAI response-shape drift | Embedding breaks; FTS/writes fine | Shape isolated in `openai.go`; httptest contract test pins it; degrades to NULL rows + FTS. |
| Backfill rate-limit blowup | API cost / 429 | Batch 100, 1s pause, 60s interval; on 429/5xx back off, never abort. |
| BLOB decode mismatch (endianness / dim) | Garbage cosine | Codec round-trip unit test; decode length-guard (`len/4 != dims` → treat row as stale, skip cosine) (decision 7). |
| Model change mid-flight | Mixed-dim vectors | `embedding_model <> ?` predicate re-embeds; length-guard prevents cross-dim cosine before catch-up. |
| Invalid `embedding_provider` persisted via PUT | Next startup hard-errors | Write-time enum validation in `handleConfigPut` (the PR-⑥ brick lesson, config_mutate.go:84-91). |
| Key decrypt fails (user/machine change) | No remote provider | Treat as absent → Noop → FTS; log warning; never crash (mirrors writer-key behavior, daemon.go:269-278). |
| Store closed under in-flight UPDATE | Corruption / panic | `embedLoop.Stop()` (blocks until goroutine exits) runs in `Close()` BEFORE `store.Close()`. |
| `mode=hybrid` before backfill catches up | Cosine empty | RRF degenerates to the FTS list; optional "N pending" note (decision 5). |

## Disagreements / Tightenings vs. Proposal

- **Gate elevated from "helper" to WRAPPER provider (tightening).** The proposal says
  "single choke-point function called before EVERY provider call." A *function* is still
  bypassable by a future call site. Making the gate a wrapper `EmbeddingProvider` that owns the
  only reference to a raw provider makes bypass STRUCTURALLY impossible — strictly stronger than
  a discipline rule. This is the single most important design choice for the highest-stakes invariant.
- **On-write embed = NULL-as-queue + loop `Trigger()`, NOT a post-write embed call (tightening).**
  The proposal lists "embed each memory after `LocalWrite`." I resolve the *mechanics* to: write
  marks nothing, `embedding IS NULL` is the queue, the handler nudges the backfill loop. This
  keeps the write path literally unchanged (observations.go untouched), is crash-safe, and reuses
  the existing `triggerSync` nudge pattern — zero new durable state. Faithful to the intent
  ("writes never block on network"), tighter on the how.
- **Query text is gated symmetrically with write text (new explicit rule).** The proposal's gate
  language targets the embedding of stored memories. I extend it explicitly to the QUERY: an
  `omitted` (or `local-only`+remote) query project does NOT get its query text embedded either —
  semantic/hybrid silently degrade to FTS for that query. Closes a hole the proposal left implicit.
- **Vectors stored L2-normalized (tightening).** Proposal says "compute cosine." Normalizing at
  write turns the hot path into a dot product. Decided here.
- **`UpdateEmbedding` does NOT take `s.mu` (resolved, with rationale).** The proposal flags the
  question. Resolved: single-statement single-row UPDATE on derived columns is atomic and outside
  the multi-statement read-modify-write that `s.mu` exists to protect — so it must NOT take `s.mu`.
- **Honest degradation note ONLY when the user typed semantic/hybrid (tightening).** The proposal
  says "silent degradation." I keep it byte-identically silent on the default/`fts` path, but add a
  terse trailing note ONLY when the user explicitly asked for semantic/hybrid and got FTS — minimal
  honest signal without violating the keyless byte-identical constraint.

## Open Questions (do not block tasks)

- [ ] Exact OpenAI base URL override knob (for the httptest fixture to point at `127.0.0.1`):
      a `RemoteOpenAIProvider.baseURL` field defaulting to `https://api.openai.com` — pick the
      field name during PR-1 apply (both work; no dep).
- [ ] Whether `Status.EmbeddingPending` counts ALL NULL rows or only policy-eligible ones —
      lean toward ALL NULL (cheap `COUNT(*)`), decide if the UI wants the eligible subset.
- [ ] PR-2 sidecar-consent config key name (`embedding_local_consent` vs `embedding_sidecar_consent`)
      — name it in PR-2 design; out of PR-1 scope.
