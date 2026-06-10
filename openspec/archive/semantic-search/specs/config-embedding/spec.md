# Config Embedding Specification

## Purpose

Defines the new configuration keys for the embedding feature, their persistence
format, precedence rules, validation at PUT /api/v1/config, and the redaction
contract for config read endpoints. The embedding key reuses the existing
`config.SecretBox` Seal/Open infrastructure from PR-③ (tray-ui) verbatim —
no new encryption primitive is introduced.

---

## Requirements

### Requirement: New config fields — embedding_provider enum

A new string enum field `embedding_provider` SHALL be added to the persistent
config with the following valid values:

| Value | Meaning |
|---|---|
| `""` (empty, zero value) | No provider; resolves to `NoopProvider` at startup |
| `"none"` | Explicit no-provider; same effect as empty |
| `"openai"` | Use `RemoteOpenAIProvider` with `text-embedding-3-small`, 256 dims |

PR-2 will add `"ollama"` to this enum. No other values are valid in PR-1.

The zero value `""` and the explicit `"none"` are behaviourally identical: the
daemon resolves to `NoopProvider`, embeddings are not produced, and
`mem_search` degrades to FTS.

> Headless testable: yes.

#### Scenario: empty embedding_provider resolves to NoopProvider

- GIVEN a config file with no `embedding_provider` field (or `"none"`)
- WHEN the daemon starts
- THEN the active `EmbeddingProvider` is `NoopProvider`
- AND `mem_search` with `mode="hybrid"` returns FTS results without error

#### Scenario: "openai" embedding_provider with key resolves to RemoteOpenAIProvider

- GIVEN a config with `embedding_provider="openai"` and `ENGRAM_EMBEDDING_KEY` set
- WHEN the daemon starts
- THEN the active `EmbeddingProvider` is `RemoteOpenAIProvider`
- AND `Dimensions()` returns `256`

---

### Requirement: Embedding API key — env wins, optionally sealed at rest

The embedding API key SHALL be sourced according to the following precedence
(highest to lowest):

1. `ENGRAM_EMBEDDING_KEY` environment variable — always wins; no sealed key is
   read if this env var is present and non-empty.
2. `encrypted_embedding_key` field in `config.json` — a base64-encoded ciphertext
   blob produced by `config.SecretBox.Seal`. The daemon calls `SecretBox.Open` at
   startup to decrypt it.
3. No key — provider resolves to `NoopProvider`.

The `ENGRAM_EMBEDDING_KEY` variable name SHALL NOT appear in `--help` output or
any flag definition. It is documented in the manual only.

The `encrypted_embedding_key` field in `config.json` MUST be stored as a
base64-encoded DPAPI blob on Windows. On non-Windows platforms `SecretBox.Seal`
returns `ErrNoSecretStore`; in that case the key is NOT written to disk and MUST
be supplied via `ENGRAM_EMBEDDING_KEY` on every daemon start.

The key SHALL be stored in an unexported struct field in the in-memory `Config`
type. It MUST NOT appear in any marshalled JSON (no `json` tag that would expose
it).

> Headless testable: yes (env var path); sealed-at-rest path requires Windows
> (manual or Windows-only CI).

#### Scenario: ENGRAM_EMBEDDING_KEY env var wins over sealed key

- GIVEN a config file with `encrypted_embedding_key` set to a sealed blob
- AND `ENGRAM_EMBEDDING_KEY=sk-from-env` is set in the environment
- WHEN the daemon starts
- THEN the active provider is initialized with `"sk-from-env"` (not the sealed key)
- AND `SecretBox.Open` is NOT called

#### Scenario: No key — provider resolves to NoopProvider

- GIVEN no `ENGRAM_EMBEDDING_KEY` env var
- AND no `encrypted_embedding_key` in config
- WHEN the daemon starts
- THEN the active `EmbeddingProvider` is `NoopProvider`

---

### Requirement: Key redaction in GET /api/v1/config

`GET /api/v1/config` SHALL include an `embedding_key_set` boolean in its
response:

- `true` when an `encrypted_embedding_key` is present in the config (or
  `ENGRAM_EMBEDDING_KEY` is set — caller can treat either as "key is configured")
- `false` otherwise

The raw key value (plaintext or ciphertext) SHALL NEVER appear in the response.
The `encrypted_embedding_key` blob SHALL NOT be included in the response body
under any field name.

The `embedding_provider` value SHALL be returned as-is (it is not sensitive).

> Headless testable: yes.

#### Scenario: config response includes embedding_provider and embedding_key_set

- GIVEN a config with `embedding_provider="openai"` and `ENGRAM_EMBEDDING_KEY` set
- WHEN `GET /api/v1/config` is called
- THEN the response body includes `"embedding_provider": "openai"`
- AND `"embedding_key_set": true`
- AND the response body does NOT contain the key string

#### Scenario: config response when no key configured

- GIVEN a config with no key and no env var
- WHEN `GET /api/v1/config` is called
- THEN `"embedding_key_set": false` appears in the response

---

### Requirement: PUT /api/v1/config — validation at write time

The `PUT /api/v1/config` handler SHALL accept and validate the following new
fields:

| Field | Type | Validation |
|---|---|---|
| `embedding_provider` | string | MUST be one of `""`, `"none"`, `"openai"` — anything else returns `400` |

The `encrypted_embedding_key` field SHALL NOT be settable via `PUT /api/v1/config`.
The embedding key is managed exclusively via a dedicated endpoint
`POST /api/v1/embedding/key` (see below) or via the env var.

An unknown field in the PUT body SHALL return `400 Bad Request` (consistent with
the existing handler's unknown-key rejection).

**PR-③ lesson applied**: any value that is persisted to `config.json` and then
read at daemon startup MUST be validated at write time. An invalid
`embedding_provider` value written to disk would cause the next daemon startup to
error (hard-fail on unrecognised enum value). The PUT handler MUST reject invalid
enum values with a clear 400 error message before persisting.

> Headless testable: yes.

#### Scenario: PUT with valid embedding_provider succeeds

- GIVEN the daemon is running
- WHEN `PUT /api/v1/config` with body `{"embedding_provider": "openai"}` is called
- THEN the response is `200 OK`
- AND `GET /api/v1/config` subsequently returns `"embedding_provider": "openai"`

#### Scenario: PUT with invalid embedding_provider returns 400

- GIVEN the daemon is running
- WHEN `PUT /api/v1/config` with body `{"embedding_provider": "voyage"}` is called
- THEN the response is `400 Bad Request`
- AND the error message indicates the field is invalid
- AND the config file is NOT written (the invalid value is not persisted)

#### Scenario: PUT with unknown field returns 400

- GIVEN the daemon is running
- WHEN `PUT /api/v1/config` with body `{"encrypted_embedding_key": "..."}` is called
- THEN the response is `400 Bad Request`
- AND the error message references the unknown/forbidden field

---

### Requirement: POST /api/v1/embedding/key — set the embedding key (DEFERRED to PR-2)

> **PR-1 scope**: The embedding key is supplied exclusively via `ENGRAM_EMBEDDING_KEY`
> env var or an already-sealed `encrypted_embedding_key` written directly to
> `config.json`. The management routes below are out of scope for PR-1 to keep the
> PR-1 diff within the ~400-line budget. They are implemented in PR-2 alongside the
> Ollama sidecar and the separate consent setting.

A new route `POST /api/v1/embedding/key` SHALL accept a JSON body
`{"key": "<plaintext-api-key>"}`, seal it via `SecretBox.Seal`, and persist the
ciphertext as `encrypted_embedding_key` in `config.json`. The plaintext key is
discarded immediately after sealing.

On platforms where `SecretBox` returns `ErrNoSecretStore`, the handler SHALL
return `422 Unprocessable Entity` with a message instructing the user to set
`ENGRAM_EMBEDDING_KEY` instead.

The route SHALL require bearer-token auth AND Origin validation
(WithAuthAndOrigin).

A `DELETE /api/v1/embedding/key` route SHALL remove the `encrypted_embedding_key`
field from `config.json` and return `200 OK`. This is the mechanism to clear a
stored key.

> Headless testable: yes (for `ErrNoSecretStore` path and for the delete path).
> The seal path requires platform-specific `SecretBox` (Windows: DPAPI;
> non-Windows: tests using a stub `SecretBox`).

#### Scenario: POST /api/v1/embedding/key stores sealed key

- GIVEN a stub `SecretBox` that seals deterministically (test environment)
- WHEN `POST /api/v1/embedding/key` with body `{"key": "sk-test"}` is called
- THEN the response is `200 OK`
- AND `config.json` contains `encrypted_embedding_key` (non-empty)
- AND `GET /api/v1/config` returns `"embedding_key_set": true`
- AND the plaintext `"sk-test"` does NOT appear anywhere in `config.json`

#### Scenario: POST /api/v1/embedding/key on non-secret-store platform returns 422

- GIVEN a `SecretBox` stub that always returns `ErrNoSecretStore`
- WHEN `POST /api/v1/embedding/key` is called
- THEN the response is `422 Unprocessable Entity`
- AND the body instructs the user to use `ENGRAM_EMBEDDING_KEY`

#### Scenario: DELETE /api/v1/embedding/key clears the stored key

- GIVEN `encrypted_embedding_key` is present in config
- WHEN `DELETE /api/v1/embedding/key` is called
- THEN the response is `200 OK`
- AND `GET /api/v1/config` returns `"embedding_key_set": false`
- AND the next daemon restart resolves to `NoopProvider`

---

### Requirement: Startup validation — invalid embedding_provider in config.json is a fatal error

When the daemon reads `config.json` at startup and finds an `embedding_provider`
value that is not in the known enum, the daemon SHALL log a clear error and
refuse to start. This mirrors the existing `transport` enum validation at startup.

This requirement reinforces the PUT validation: the two gates together prevent
an unrecognised value from ever reaching the disk (PUT) or, if it somehow did
(e.g., manual file edit), from causing a silent mis-configuration (startup).

> Headless testable: yes — write an invalid value directly to config.json and
> assert daemon startup fails with a clear error.

#### Scenario: Invalid embedding_provider in config.json fails daemon startup

- GIVEN `config.json` contains `"embedding_provider": "voyage"` (invalid)
- WHEN the daemon attempts to start
- THEN it exits with a non-zero status
- AND the error output references `embedding_provider` and the invalid value

---

> **Headless testability**: All requirements in this spec are provable headlessly
> via the control API acceptance test infrastructure and a stub `SecretBox`. The
> DPAPI seal path (Windows-only) is manual or tested in a Windows CI job;
> all other scenarios are cross-platform. No tray or browser required.
