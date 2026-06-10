# Tasks: semantic-search — Gate-as-Wrapper Provider, NULL-as-Queue Backfill, Brute-Force Cosine + RRF Hybrid

## Review Workload Forecast

| Field | Value |
|-------|-------|
| Estimated changed lines | 1,100 – 1,400 (new files dominate; test battery is large) |
| 400-line budget risk | High |
| Chained PRs recommended | Yes |
| Suggested split | PR-1: embedding core (port + gate + OpenAI + Noop + codec + cosine + RRF + search mode + config keys + tests) → PR-1b (pre-authorized cut): backfill loop + status observability → PR-2: Ollama sidecar + consent + mem_similar + FindCandidates + key routes |
| Delivery strategy | chained PRs |
| Chain strategy | stacked-to-main |

Decision needed before apply: No
Chained PRs recommended: Yes
Chain strategy: stacked-to-main
400-line budget risk: High

> PR-1b seam pre-authorized: if the adversarial review flags PR-1 over ~400 lines, cut
> the backfill Loop + on-write Trigger + Status embedding_backfill observability into PR-1b.
> PR-1 without the loop still ships the gate, codec, search math, and config keys end-to-end
> (paraphrase fixture uses manually-seeded embeddings). PR-2 is unchanged regardless.

### Suggested Work Units

| Unit | Goal | Likely PR | Est. lines | Budget risk |
|------|------|-----------|------------|-------------|
| 1 | EmbeddingProvider port + RemoteOpenAIProvider + NoopProvider + gated wrapper + codec + cosine + RRF + SearchFilter.Mode + mem_search mode + config keys + key safety tests | PR-1 | ~380 | Low-Medium |
| 1b | embedding.Loop (backfill) + on-write embedLoop.Trigger() + Status embedding_backfill sub-object + backfill tests | PR-1b (cut if PR-1 > 400) | ~200 | Low |
| 2 | OllamaSidecarProvider + embedding consent + mem_similar tool + FindCandidates cosine pass + embedding_dims config + POST/DELETE /api/v1/embedding/key routes | PR-2 | ~350 | Low |

---

## PR-1: embedding core

> Specs: embedding-provider, embedding-privacy, vector-search, config-embedding (env+file only; key routes deferred to PR-2).
> Verify gate: `CGO_ENABLED=0 go build ./...`; `CGO_ENABLED=0 GOOS=linux go build ./...`; `go vet ./...`; `go vet -tags windows ./...`; `go test ./...`; `go test -tags acceptance ./...`; `TestRun_DaemonHelp_DoesNotLeakWriterKey` green; `TestRun_DaemonHelp_DoesNotLeakEmbeddingKey` green.
> go.mod: zero new modules (stdlib net/http + encoding/json only). Reviewer must confirm.

### Phase 1 — Port and gate (internal/embedding)

- [x] 1.1 `internal/embedding/provider.go`: define `EmbeddingProvider` interface (`Embed(ctx, texts)→([][]float32,error)`, `Dimensions() int`, `ModelName() string`); `PolicyChecker` interface (`GetPolicy(project) (localstore.Policy, error)`); `ErrEmbeddingGated` sentinel error. Package imports stdlib only. Test: `TestProvider_InterfaceIsSatisfied` (compile-only, all three concrete types); `TestErrEmbeddingGated_IsDistinct`.
- [x] 1.2 `internal/embedding/gated.go`: `gatedProvider` struct wrapping `inner EmbeddingProvider`, `checker PolicyChecker`, `remote bool`, `consent bool`; `NewGated(inner, checker, remote bool) EmbeddingProvider`; `Embed` calls `GetPolicy` then `EligibleForEmbedding` then delegates (or returns `ErrEmbeddingGated`). `Dimensions`/`ModelName` delegate unconditionally. `EligibleForEmbedding(policy, remote bool) bool` pure function. Test: `TestGate_AllSixCombinations` (table-test covering all policy×remote cells from privacy spec); `TestGate_SyncedRemote_Delegates`; `TestGate_OmittedRemote_ReturnsGatedError`; `TestGate_LocalOnlyRemote_ReturnsGatedError`; `TestGate_LocalOnlyLocal_ReturnsGatedError` (PR-1 consent=false).
- [x] 1.3 `internal/embedding/openai.go`: `RemoteOpenAIProvider` struct (unexported `apiKey`, `baseURL`, `timeout`, `httpClient`); `NewRemoteOpenAI(key string, opts ...Option) *RemoteOpenAIProvider` (baseURL override for testing); `Embed` posts `{"model":"text-embedding-3-small","input":[...],"dimensions":256,"encoding_format":"float"}` with `Authorization: Bearer`; decodes `response.data[i].embedding`; one attempt, no retry; non-2xx → error (body discarded). `Dimensions()→256`, `ModelName()→"text-embedding-3-small"`. Test: `TestOpenAI_RequestShape` (httptest fixture returns canned 256-dim response; assert model/dims/Authorization fields); `TestOpenAI_Non2xx_Error_KeyNotLeaked`; `TestOpenAI_Timeout_Returns_DeadlineExceeded`; `TestOpenAI_Dimensions_256`.
- [x] 1.4 `internal/embedding/provider.go` (extend): `NoopProvider` struct; `Embed→(nil,nil)`, `Dimensions→0`, `ModelName→"noop"`. Test: `TestNoop_Embed_NilNil`; `TestNoop_Dimensions_0`.
- [x] 1.5 `internal/embedding/recording_mock_test.go`: `recordingMockProvider` (test-only): records every `texts` slice passed to `Embed`; returns configurable deterministic vectors; exported only within `package embedding_test`. Used by gate privacy proofs in tasks 1.2, 2.3, and 2.5.

### Phase 2 — Vector codec + cosine + RRF (internal/localstore)

- [x] 2.1 `internal/localstore/vector.go`: `encodeVector(v []float32) []byte` (little-endian IEEE 754); `decodeVector(b []byte, dims int) ([]float32, error)` (returns error if `len(b)%4 != 0` or `len(b)/4 != dims`); `l2Normalize(v []float32) []float32`; `dot(a, b []float32) float32`. Test: `TestCodec_RoundTrip_Cosine1` (encode→decode→dot==1.0 within 1e-6); `TestCodec_DimMismatch_Error`; `TestCodec_LittleEndian_ByteLayout` (hardcode known float → assert exact bytes); `TestDot_OrthogonalVectors_Zero`; `TestDot_ParallelVectors_One`.
- [x] 2.2 `internal/localstore/vector.go` (extend): `SelectVectors(db, filter SearchFilter) ([]vectorRow, error)` — `SELECT sync_id, embedding FROM memories WHERE embedding IS NOT NULL AND deleted_at IS NULL [+ project/type/scope predicates]`; decode each BLOB with length-guard (skip/re-embed if dim mismatch, never NaN); `cosineTopK(queryVec []float32, rows []vectorRow, k int) []cosineCandidatd` — dot-product each normalized vector against normalized query, sort desc, tie-break `sync_id` asc. Test: `TestSelectVectors_FiltersNullEmbedding`; `TestCosineTopK_MostSimilarFirst`; `TestCosineTopK_ZeroMagnitude_Excluded`; `TestCosineTopK_TieBreak_SyncIDOrder`.
- [x] 2.3 `internal/localstore/vector.go` (extend): `rrfFuse(ftsRanks []string, cosineRanks []string, k int) []string` — Reciprocal Rank Fusion (`score = Σ 1/(k+rank_i)`), merge key = `sync_id`, tie-break `sync_id` asc, return top items by fused score. Test: `TestRRF_DocInBothLists_ScoresHigher` (spec scenario: doc-2 beats doc-1); `TestRRF_DocInOneList_OneContribution`; `TestRRF_TieBreak_Deterministic`; `TestRRF_EmptyList_DegradesToOther`.
- [x] 2.4 `internal/localstore/vector.go` (extend): `SelectEmbeddable(db *sql.DB, currentModel string, limit int) ([]embeddableRow, error)` — `WHERE (embedding IS NULL OR embedding_model != ?) AND deleted_at IS NULL LIMIT ?`; `UpdateEmbedding(db *sql.DB, syncID string, vec []float32, model, ts string) error` — single-row `UPDATE ... SET embedding=?,embedding_model=?,embedding_created_at=? WHERE sync_id=? AND embedding IS NULL` (no `s.mu`). Test: `TestSelectEmbeddable_PicksNullAndStale`; `TestSelectEmbeddable_SkipsAlreadyEmbedded`; `TestUpdateEmbedding_Idempotent_NullGuard`.

### Phase 3 — Search mode wiring (internal/localstore)

- [x] 3.1 `internal/localstore/search.go` (modify): add `Mode string` field to `SearchFilter`; in `SearchMemoriesFiltered` add a mode-dispatch block: `""` and `"fts"` → today's path unchanged (byte-identical); unknown mode → `"fts"` path; `"semantic"` → `cosineTopK`-only, no FTS; `"hybrid"` → run FTS (limit×2 candidates) + `cosineTopK` (limit×2), `rrfFuse` by `sync_id`, take top-K. The `Mode==""` or `"fts"` branch is literally the current code, untouched. Test: `TestSearch_ModeEmpty_ByteIdentical` (run same query with `Mode=""` and current pre-change path; assert equal results); `TestSearch_ModeFTS_EqualToEmpty`; `TestSearch_ModeHybrid_NoEmbeddings_DegradesToFTS`; `TestSearch_ModeSemantic_NoEmbeddings_EmptyResult`; `TestSearch_ModeUnknown_FallsBackToFTS`; existing `search_test.go` and `search_fixes_test.go` MUST pass unchanged.
- [x] 3.2 `cmd/engram/tools.go` (modify): in `handleSearch` extract `mode` string from tool params (optional, default `""`); set `filter.Mode = mode`; pass to `SearchMemoriesFiltered`; when mode is `semantic` or `hybrid` AND result was FTS-only (embed attempt gated/failed/no-vectors), append the degradation note to the output text. Test: `TestHandleSearch_ModeHybrid_PassedToFilter`; `TestHandleSearch_ModeDefault_NoNote`; `TestHandleSearch_ModeHybrid_DegradationNote_WhenNoEmbeddings`; existing `tools_search_test.go` tests MUST pass unchanged.

### Phase 4 — Config keys + daemon wiring

- [x] 4.1 `internal/config/config.go` (modify): add `EmbeddingProvider string` (enum: `""`, `"none"`, `"openai"`; `"ollama"` reserved PR-2) and `encryptedEmbeddingKey []byte` (unexported, no json tag) to `fileConfig`; `Redact()` returns `EmbeddingProvider` as-is + `EmbeddingKeySet bool` (true if field non-empty OR env `ENGRAM_EMBEDDING_KEY` non-empty); `ConfigPatch` gains `EmbeddingProvider *string`; `Patch()` applies it. Test: `TestConfig_EmbeddingProvider_RoundTrip`; `TestConfig_Redact_EmbeddingKeySet_True`; `TestConfig_Redact_EmbeddingKeySet_False`; `TestConfig_Patch_EmbeddingProvider`.
- [x] 4.2 `internal/config/config.go` (modify): startup enum validation — `Load()` returns error when `EmbeddingProvider` is not in `{"","none","openai"}` (caller in daemon.go treats this as fatal); add `ValidEmbeddingProviders` var. Test: `TestConfig_Load_InvalidEmbeddingProvider_ReturnsError`; `TestConfig_Load_ValidProviders_OK` (parameterised).
- [x] 4.3 `internal/controlapi/config_mutate.go` (modify): in `handleConfigPut` validate `embedding_provider` field against `ValidEmbeddingProviders`; reject with 400 if invalid before persisting; reject `encrypted_embedding_key` field in PUT body with 400 (forbidden field). Test: `TestPUT_Config_EmbeddingProvider_Valid_200`; `TestPUT_Config_EmbeddingProvider_Invalid_400`; `TestPUT_Config_EncryptedEmbeddingKey_Forbidden_400`.
- [x] 4.4 `internal/controlapi/server.go` (modify): `StatusResponse` gains `EmbeddingBackfill struct{ Pending int; Provider string }` JSON `"embedding_backfill"`; `configRead` handler returns `RedactedConfig.EmbeddingProvider` + `EmbeddingKeySet bool`. Test: `TestStatus_EmbeddingBackfill_Shape` (assert JSON key `embedding_backfill.pending` and `embedding_backfill.provider`); `TestConfigRead_EmbeddingKeySet_True`; `TestConfigRead_EmbeddingKeySet_False`.
- [x] 4.5 `cmd/engram/daemon.go` (modify): after writer-key resolution chain (line ~239-282) add embedding provider resolution: `ENGRAM_EMBEDDING_KEY` env wins → else `secretBox.Open(c.encryptedEmbeddingKey)` → else Noop; startup-fatal on `config.Load` returning unknown enum; construct `inner` (RemoteOpenAI or Noop); `gated := embedding.NewGated(inner, store, remote=true)`; pass `gated` to `registerTools`/`handleSearch`. `embedLoop` is nil in PR-1 (loop comes in PR-1b or PR-2). Test: `TestDaemon_EmbeddingKey_EnvWins`; `TestDaemon_EmbeddingKey_NoKey_ResolvesNoop`; `TestDaemon_InvalidEmbeddingProvider_FatalAtStartup`; `TestRun_DaemonHelp_DoesNotLeakEmbeddingKey` (extend existing pattern).

### Phase 5 — Privacy proof + acceptance

- [x] 5.1 `internal/embedding/gate_privacy_test.go` (new, `package embedding_test`): recording-mock proof on the GATE (write path): `TestGate_Omitted_RecordingMock_ZeroCallsOnWrite`; `TestGate_LocalOnly_Remote_RecordingMock_ZeroCallsOnWrite`; `TestGate_Synced_Remote_RecordingMock_ReceivesText`. Uses `recordingMockProvider` from task 1.5 as the `inner` of `NewGated`.
- [x] 5.2 `internal/embedding/paraphrase_acceptance_test.go` (`//go:build acceptance`): seed two memories in a real temp SQLite store — "DPAPI-sealed config key" (no keywords matching the query) with its embedding manually set to a deterministic mock vector close to the query vector, and a noise memory; assert `SearchMemoriesFiltered(mode=hybrid)` surfaces the paraphrase memory AND `mode=fts` does NOT. NO real API. Test: `TestAcceptance_Paraphrase_HybridBeats_FTS`.
- [x] 5.3 `internal/embedding/degradation_test.go`: table-test per cell of the degradation matrix (decision 5 in design): `TestDegradation_NoKey_FTSResults_NoNote`; `TestDegradation_ProviderError_QueryEmbed_FTSWithNote`; `TestDegradation_QueryProjectGated_FTSWithNote`; `TestDegradation_ModeHybrid_NoVectors_FTSWithNote`; `TestDegradation_ModeSemantic_NoVectors_FTSWithNote`; `TestDegradation_ModeFTS_NeverNote`.
- [x] 5.4 `cmd/engram/daemon_test.go` (modify): extend `TestRun_DaemonHelp_DoesNotLeakWriterKey` pattern → add `TestRun_DaemonHelp_DoesNotLeakEmbeddingKey`; add `TestDaemon_InvalidEmbeddingProvider_FatalAtStartup` (write bad json, start daemon, assert non-zero exit + error references `embedding_provider`).
- [x] 5.5 `README.md`: add "Semantic search" section documenting `mem_search mode` parameter, `embedding_provider` config, `ENGRAM_EMBEDDING_KEY` env var, privacy story (per-project policy gate), and graceful degradation to FTS when no key configured.

---

## PR-1b: backfill loop (cut from PR-1 if review flags >400 lines)

> Pre-authorized seam. Spec: embedding-backfill. Builds on PR-1 merged.
> Verify gate: same as PR-1.

- [x] 1b.1 `internal/embedding/loop.go`: `Loop` struct mirroring `syncer.Loop` (Interval, Trigger chan, Stop, backoff, Debounce); `NewLoop(store, gated EmbeddingProvider, cfg LoopConfig) *Loop`; `run()` tick: `SelectEmbeddable(currentModel, batchSize)` → per-row `GetPolicy` + `EligibleForEmbedding` gate → batch `Embed` → `UpdateEmbedding` for each → `perBatchPause`; on `ErrEmbeddingGated` skip row silently; on provider error log+continue+backoff; zero rows = idle. Defaults: batchSize=100, pause=1s, Interval=60s, Debounce=1s, BackoffMin=1s, BackoffMax=2m. Test: `TestLoop_Idempotency_TwoRuns_NoDuplicates`; `TestLoop_NoOp_WhenAllEmbedded`; `TestLoop_ModelChange_ReEmbedsStale`; `TestLoop_BatchSize_Respected` (250 rows → 3 calls with batchSize=100); `TestLoop_BatchFailure_ContinuesRemainingBatches`; `TestLoop_Resume_AfterInterrupt` (cancel mid-backfill, re-run, remaining rows embedded); `TestLoop_MixedPolicy_OnlySyncedEmbedded`.
- [x] 1b.2 `internal/embedding/loop_privacy_test.go` (`package embedding_test`): recording-mock proof on the LOOP path (mirrors task 5.1 for the loop driver): `TestLoop_Omitted_RecordingMock_ZeroCalls`; `TestLoop_LocalOnly_Remote_RecordingMock_ZeroCalls`; `TestLoop_PolicyFlip_MidBackfill_PostFlipRowsSkipped`.
- [x] 1b.3 `cmd/engram/daemon.go` (modify): construct `embedLoop := embedding.NewLoop(store, gated, embedding.Config{...})` (non-nil when provider is not Noop); `embedLoop.Start(ctx)` in both `runDaemonWithIO` and `runDaemonHTTP` beside `components.loop.Start(ctx)`; `daemonComponents.Close()` gains `if d.embedLoop != nil { d.embedLoop.Stop() }` BEFORE `store.Close()`. In `handleSave` (tools.go): add `embedLoop.Trigger()` (nil-safe) after successful `AddObservation`. Test: `TestDaemon_EmbedLoop_StartStop`; `TestHandleSave_Synced_TriggersLoop` (mock loop, assert Trigger called); `TestHandleSave_Omitted_DoesNotTriggerLoop` (existing omitted-refusal test stays green).
- [x] 1b.4 `internal/controlapi/server.go` (modify): `StatusResponse.EmbeddingBackfill.Pending` populated via `store.CountNullEmbeddings()` (new one-liner on `localstore.Store`); `EmbeddingBackfill.Provider` = `gated.ModelName()`. `internal/localstore/store.go`: add `CountNullEmbeddings() (int, error)` — `SELECT COUNT(*) FROM memories WHERE embedding IS NULL AND deleted_at IS NULL`. Test: `TestStatus_EmbeddingBackfill_PendingCount`; `TestStatus_EmbeddingBackfill_ProviderNoop`.
- [x] 1b.5 `internal/embedding/backfill_acceptance_test.go` (`//go:build acceptance`): real temp SQLite + recording mock wired as gated `inner`; write 5 rows to a synced project → trigger loop → assert all 5 have `embedding IS NOT NULL`; write 3 rows to an omitted project → run loop → assert mock received zero calls for those rows; `GET /api/v1/status` returns `embedding_backfill.pending == 0` after completion. Test: `TestAcceptance_Backfill_Suite` (≥3 sub-cases).

---

## PR-2: sidecar, consent, mem_similar, FindCandidates, key routes

> Spec: embedding-provider (Ollama), embedding-privacy (consent), vector-search (mem_similar + FindCandidates), config-embedding (key routes). Builds on PR-1 (and PR-1b if cut) merged.
> Verify gate: same as PR-1 plus `TestAcceptance_MCPSimilar_Suite`.

- [x] 2.1 `internal/embedding/ollama.go`: `OllamaSidecarProvider` — HTTP POST to `localhost:11434/api/embeddings` (configurable host); `ModelName()` from config; `Dimensions()` from config (`embedding_dims`, default 256 until overridden); same no-retry contract as OpenAI. Test: `TestOllama_RequestShape` (httptest); `TestOllama_Non2xx_Error`; `TestOllama_Timeout`.
- [x] 2.2 `internal/embedding/gated.go` (modify): add `consent bool` field; `NewGated` gains `consent bool` param; `local-only + remote=false` cell: returns `ErrEmbeddingGated` when `!consent`. Test: `TestGate_LocalOnly_Local_ConsentTrue_Embeds`; `TestGate_LocalOnly_Local_ConsentFalse_Gated` (updates task 1.2 table-test row).
- [x] 2.3 `internal/embedding/loop_privacy_test.go` (extend): `TestLoop_LocalOnly_Local_ConsentTrue_Embeds`; `TestLoop_LocalOnly_Local_ConsentFalse_ZeroCalls`.
- [x] 2.4 `internal/config/config.go` (modify): add `EmbeddingLocalConsent bool`, `EmbeddingDims int` (default 256), `OllamaHost string` fields; `ConfigPatch` gains them; `ValidEmbeddingProviders` adds `"ollama"`. Test: `TestConfig_OllamaFields_RoundTrip`.
- [x] 2.5 `cmd/engram/daemon.go` (modify): extend provider resolution to `"ollama"` case; pass `consent=c.EmbeddingLocalConsent` to `NewGated`; `embedding_dims` drives `OllamaProvider.dims`. Test: `TestDaemon_OllamaProvider_Resolves`; `TestDaemon_Consent_PassedToGate`.
- [x] 2.6 `internal/controlapi/embedding_key.go` (new): `POST /api/v1/embedding/key` — decode `{"key":...}`, `SecretBox.Seal`, persist `encrypted_embedding_key`; on `ErrNoSecretStore` → 422; `DELETE /api/v1/embedding/key` — clear field → 200. Both routes wrapped with `withAuth` + `withOrigin`. Test: `TestKeyRoute_Post_StubSecretBox_StoresSealed`; `TestKeyRoute_Post_NoSecretStore_422`; `TestKeyRoute_Delete_ClearsKey`; `TestKeyRoute_Post_MissingAuth_401`; `TestKeyRoute_Post_WrongOrigin_403`.
- [x] 2.7 `cmd/engram/tools.go` (modify): add `mem_similar` MCP tool — params: `sync_id`, `project`, `limit`; embed the source row's content via `gated.Embed`; `cosineTopK` excluding the source row; return top-K. Test: `TestMemSimilar_ReturnsNeighbours`; `TestMemSimilar_ExcludesSourceRow`; `TestMemSimilar_GatedProject_FallsBack`.
- [x] 2.8 `internal/localstore/candidates.go` (modify): `FindCandidates` gains a cosine pass — embed the candidate text via `gated.Embed` (injected as optional port); union cosine results with FTS candidate set by `sync_id`; nil gate = today's FTS-only path unchanged. Test: `TestFindCandidates_CosineSurfaces_Paraphrase`; `TestFindCandidates_NilGate_FTSOnly` (existing tests stay green).
- [x] 2.9 `internal/embedding/sidecar_acceptance_test.go` (`//go:build acceptance`): httptest Ollama stub + `local-only` project with consent=true → assert rows embedded; consent=false → zero calls. Test: `TestAcceptance_Sidecar_ConsentGate_Suite` (≥2 sub-cases).
- [x] 2.10 `README.md`: extend "Semantic search" section with `mem_similar` tool, Ollama sidecar setup, `embedding_local_consent`, `embedding_dims`, and `POST /DELETE /api/v1/embedding/key` usage.
