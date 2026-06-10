# Embedding Provider Specification

## Purpose

Defines the `EmbeddingProvider` port contract, the `RemoteOpenAIProvider` concrete
implementation, and the `NoopProvider` fallback that keeps search fully functional
when no provider is configured. This is the pluggable embedding layer for PR-1.

---

## Requirements

### Requirement: EmbeddingProvider port interface

An `EmbeddingProvider` interface SHALL be defined in `internal/embedding/` with
exactly three methods:

- `Embed(ctx context.Context, texts []string) ([][]float32, error)` — embed a
  batch of texts; returns one `[]float32` per input in the same order.
- `Dimensions() int` — returns the fixed dimensionality of all vectors this
  provider produces.
- `ModelName() string` — returns a stable, human-readable name identifying the
  model (stored in `embedding_model` per row for provenance tracking).

The port MUST be defined in a package free of external dependencies beyond the
Go standard library (no `net/http`, no `encoding/json` at the port level — those
are implementation details of the concrete providers).

> Headless testable: yes — any in-process mock can satisfy the interface.

#### Scenario: Embed batch preserves input order

- GIVEN an `EmbeddingProvider` implementation
- WHEN `Embed` is called with texts `["alpha", "beta", "gamma"]`
- THEN the returned slice has exactly 3 elements
- AND element 0 corresponds to "alpha", element 1 to "beta", element 2 to "gamma"
- AND each element has length equal to `Dimensions()`

#### Scenario: Dimensions and ModelName are stable across calls

- GIVEN an `EmbeddingProvider` implementation constructed once
- WHEN `Dimensions()` is called ten times
- THEN it returns the same integer every time
- WHEN `ModelName()` is called ten times
- THEN it returns the same non-empty string every time

---

### Requirement: RemoteOpenAIProvider — OpenAI text-embedding-3-small

A `RemoteOpenAIProvider` concrete implementation SHALL satisfy `EmbeddingProvider`
and comply with all of the following:

**Request shape** — The provider SHALL POST to
`https://api.openai.com/v1/embeddings` (or an overridden base URL for testing)
with a JSON body:

```json
{"model": "text-embedding-3-small", "input": ["..."], "dimensions": 256,
 "encoding_format": "float"}
```

The `dimensions` field SHALL be `256` (matryoshka shortening). The
`encoding_format` SHALL be `"float"` (base64 encoding is not used).

**Zero new Go module imports** — The provider SHALL use only stdlib `net/http`
and `encoding/json`. No third-party OpenAI SDK or HTTP client library is
permitted.

**Dimensions()** SHALL return `256`. **ModelName()** SHALL return
`"text-embedding-3-small"`.

**Timeout** — The provider SHALL apply a per-request timeout. The timeout SHALL
be configurable at construction time and SHALL default to `30s`. A request that
exceeds the timeout SHALL return an error; the caller (embedding loop) treats
this as a transient failure and leaves affected rows for the backfill loop.

**API key sourcing** — The provider SHALL accept the API key only at construction
time (injected by the daemon wiring layer). It SHALL NOT read environment
variables or config files directly — key sourcing is the caller's responsibility.
The key MUST NOT appear in any `error` return value, log line, or HTTP response.

**Retry policy** — The provider SHALL make exactly ONE attempt per `Embed` call.
No automatic retries are performed inside the provider. Transient errors
(network, timeout, 5xx) are surfaced as errors to the caller; the backfill loop
provides the retry strategy via re-queuing rows.

**Response decoding** — The provider SHALL extract the float32 embedding vectors
from `response.data[i].embedding` and return them in input order. An HTTP
status outside 2xx SHALL be treated as an error; the response body SHALL be
discarded (not logged) to prevent accidental key or content leakage.

> Headless testable: yes — the base URL is overridable at construction time,
> enabling an `httptest.Server` fixture.

#### Scenario: Successful embedding request — correct request shape

- GIVEN a `RemoteOpenAIProvider` pointing to an `httptest.Server` fixture
- WHEN `Embed(ctx, ["some text"])` is called
- THEN the fixture receives exactly one POST with `Content-Type: application/json`
- AND the body decodes to `{"model":"text-embedding-3-small","input":["some text"],"dimensions":256,"encoding_format":"float"}`
- AND the `Authorization` header is `Bearer <key>`
- AND the returned `[][]float32` has length 1 with inner length 256

#### Scenario: Non-2xx HTTP response returns error, key not leaked

- GIVEN a `RemoteOpenAIProvider` and the remote returns `401 Unauthorized`
- WHEN `Embed(ctx, ["text"])` is called
- THEN an error is returned
- AND the error message does NOT contain the API key string

#### Scenario: Timeout returns error

- GIVEN a `RemoteOpenAIProvider` with timeout `50ms` and a server that hangs
- WHEN `Embed(ctx, ["text"])` is called
- THEN an error is returned within approximately `50ms`
- AND the error is a timeout/deadline-exceeded variant

#### Scenario: Dimensions() returns 256

- GIVEN a `RemoteOpenAIProvider`
- WHEN `Dimensions()` is called
- THEN it returns `256`

---

### Requirement: NoopProvider — FTS-only degradation

A `NoopProvider` concrete implementation SHALL satisfy `EmbeddingProvider` with
the following semantics:

- `Embed(ctx, texts)` SHALL return `(nil, nil)` — no error, no vectors.
- `Dimensions()` SHALL return `0`.
- `ModelName()` SHALL return `"noop"`.

The `NoopProvider` is the default provider when no `embedding_provider` is
configured or when no API key is available. Its presence allows all embedding
call-sites to be unconditional (no `if provider != nil` guards needed); the
`embedding IS NULL` path in search transparently degrades to FTS without any
code path distinction.

> Headless testable: yes.

#### Scenario: NoopProvider.Embed returns nil, nil

- GIVEN a `NoopProvider`
- WHEN `Embed(ctx, ["text"])` is called
- THEN the return value is `(nil, nil)` — no error, no vectors

#### Scenario: NoopProvider wired as default resolves to FTS

- GIVEN the daemon is started with no `ENGRAM_EMBEDDING_KEY` and no `encrypted_embedding_key` in config
- WHEN `mem_search` is called with `mode="hybrid"`
- THEN results are returned (FTS path, no error)
- AND the result set is identical to calling `mem_search` with `mode="fts"`

---

### Requirement: Provider failure — never block writes, never crash

A provider error (network failure, timeout, non-2xx response) during an
on-write embedding attempt SHALL NOT cause the `LocalWrite` (or equivalent
write path) to fail. The write MUST succeed; the row is left with
`embedding IS NULL`, making it eligible for the backfill loop.

Provider errors SHALL be logged at DEBUG or INFO level. They SHALL NOT be
propagated to the MCP caller or returned as a tool error.

> Headless testable: yes — inject a provider that always errors; assert write
> succeeds and row has `embedding IS NULL`.

#### Scenario: Provider error on write — write still succeeds

- GIVEN a provider that always returns an error from `Embed`
- WHEN `mem_save` writes a new observation for a `synced` project
- THEN the write succeeds (tool returns success)
- AND the `memories` row exists with `embedding IS NULL`
- AND `embedding_model IS NULL`

---

### Requirement: API key never in logs, errors, or responses

The API key (plaintext or ciphertext) SHALL NOT appear in:
- Any Go `error` value returned by the provider
- Any structured or unstructured log line
- Any HTTP response body returned by the control API or MCP server

The provider construction function SHALL accept the key as a `[]byte` or
`string` parameter and MUST NOT store it in a struct field that could be
serialized (e.g., no `json` struct tag on the key field; the field MUST be
unexported).

> Headless testable: yes — assert that error strings from error-triggering
> scenarios do not contain the test key string.

#### Scenario: Key absent from error string

- GIVEN a `RemoteOpenAIProvider` constructed with key `"sk-test-secret-key"`
- WHEN the remote server returns a `401` causing an error
- THEN the error message does not contain `"sk-test-secret-key"`

---

> **Headless testability**: All requirements in this spec are provable headlessly
> via unit tests with `httptest.Server` fixtures and in-process mock providers.
> No network access required. No tray or browser required.
