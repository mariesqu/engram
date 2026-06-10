# Control API Specification

## Purpose

Defines the loopback HTTP control plane that owns node-local engram state. The control API is the single authority for status, config, project policy, and sync operations. The tray, web UI, and CLI are all thin clients of this API — none of them access SQLite or the sync loop directly.

---

## Requirements

### Requirement: Loopback-only bind

The control API server SHALL bind exclusively to `127.0.0.1` on a configurable port (default 7700). It MUST NOT bind to `0.0.0.0` or any external interface.

#### Scenario: Server rejects connections from non-loopback origin

- GIVEN the control API is running on `127.0.0.1:7700`
- WHEN a TCP connection arrives from any address other than `127.0.0.1`
- THEN the server MUST refuse or drop the connection before processing the request

---

### Requirement: Bearer-token authentication

Every request to the control API MUST carry a valid bearer token in the `Authorization: Bearer <token>` header. The token is generated at daemon startup, written to a user-ACL'd file alongside the database, and regenerated on each bind (including after a port change). Requests without a valid token MUST receive `401 Unauthorized`.

#### Scenario: Valid token is accepted

- GIVEN the daemon has started and the token file exists
- WHEN a client sends `Authorization: Bearer <correct-token>` on any endpoint
- THEN the server processes the request normally

#### Scenario: Missing or wrong token is rejected

- GIVEN the control API is running
- WHEN a client sends a request with no `Authorization` header or an incorrect token
- THEN the server responds `401 Unauthorized` with no further processing

---

### Requirement: Origin check on mutating requests

All mutating endpoints (POST, PUT) MUST validate the `Origin` header against `http://127.0.0.1:<port>`. Requests whose `Origin` does not match MUST receive `403 Forbidden`. GET endpoints MUST NOT require an Origin header.

#### Scenario: Correct origin on mutation is accepted

- GIVEN a POST request carries `Origin: http://127.0.0.1:7700` and a valid token
- WHEN the server processes the request
- THEN the request proceeds to the handler

#### Scenario: Wrong origin on mutation is rejected

- GIVEN a POST request carries `Origin: http://evil.example.com`
- WHEN the server processes the request
- THEN the server responds `403 Forbidden` before invoking any handler logic

---

### Requirement: GET /api/v1/status — connectivity and sync result

`GET /api/v1/status` SHALL return a JSON document containing:
- `central_connected` (bool): whether the daemon currently holds an active connection to central.
- `central_url` (string, omitted when not configured): the configured central URL.
- `last_sync_result` (object): outcome of the most recent sync cycle — `at` (RFC3339 timestamp or null), `error` (string or null), `pushed` (int), `pulled` (int).
- `daemon_version` (string): binary version.

The response MUST reflect actual runtime state, not cached configuration.

#### Scenario: Status when connected and last sync succeeded

- GIVEN the daemon is connected to central and the last sync cycle completed without error
- WHEN a client calls `GET /api/v1/status`
- THEN the response contains `central_connected: true`, a non-null `last_sync_result.at`, and `last_sync_result.error: null`

#### Scenario: Status when central URL is not configured

- GIVEN no central URL is configured
- WHEN a client calls `GET /api/v1/status`
- THEN the response contains `central_connected: false` and `central_url` is absent from the response

#### Scenario: Status after a failed sync cycle

- GIVEN the last sync cycle returned an error
- WHEN a client calls `GET /api/v1/status`
- THEN `last_sync_result.error` is a non-empty string describing the failure

---

### Requirement: GET /api/v1/config — redacted config read

`GET /api/v1/config` SHALL return the current effective configuration as JSON. The writer key MUST be redacted: if set, the field MUST appear as `"writer_key": "***REDACTED***"`; if unset, it MUST be omitted entirely. The raw key value MUST NEVER appear in the response body, response headers, or server logs.

#### Scenario: Config returned with key redacted

- GIVEN the writer key is configured
- WHEN a client calls `GET /api/v1/config`
- THEN the response body contains `"writer_key": "***REDACTED***"` and does NOT contain the actual key value anywhere

#### Scenario: Config returned when no key is set

- GIVEN no writer key is configured
- WHEN a client calls `GET /api/v1/config`
- THEN the `writer_key` field is absent from the response

---

### Requirement: PUT /api/v1/config — runtime-mutable vs restart-required settings

`PUT /api/v1/config` SHALL accept a **partial merge-patch** JSON body (RFC 7396 semantics: omitted fields are left unchanged, `null` clears a field) and update the persistent config file. Settings are classified as:
- **Runtime-mutable**: `sync_interval`, `log_level` — take effect immediately without restart.
- **Restart-required**: `db_path`, `http_port`, `transport` — the response MUST include `"restart_required": true` when any such field is changed.

`central_url` and `writer_key` are managed exclusively through `POST /api/v1/central/connect` and `POST /api/v1/central/disconnect`; they MUST NOT be accepted by `PUT /api/v1/config`. If the request body contains `writer_key` or `central_url`, the server MUST respond `400 Bad Request`.

#### Scenario: Mutating a runtime-mutable setting takes effect immediately

- GIVEN the daemon is running
- WHEN a client sends `PUT /api/v1/config` with `{"sync_interval": "30s"}`
- THEN the server responds `200` with `"restart_required": false` and the sync interval is updated without restart

#### Scenario: Mutating a restart-required setting signals the client

- GIVEN the daemon is running
- WHEN a client sends `PUT /api/v1/config` with `{"http_port": 7701}`
- THEN the server responds `200` with `"restart_required": true`

#### Scenario: Writer key in PUT config is rejected

- GIVEN the daemon is running
- WHEN a client sends `PUT /api/v1/config` with a body containing `"writer_key"`
- THEN the server responds `400 Bad Request`

---

### Requirement: POST /api/v1/central/connect and /disconnect

`POST /api/v1/central/connect` SHALL accept `{"central_url": "...", "writer_key": "..."}`, persist the central URL and encrypted writer key to config, and attempt to establish connectivity. On success it MUST return `200` with updated status. On credential failure it MUST return `422 Unprocessable Entity` with an error message; the config MUST NOT be persisted on failure.

`POST /api/v1/central/disconnect` SHALL clear the central URL and writer key from config and halt all push/pull activity. The endpoint MUST return `200`; existing local data MUST NOT be deleted.

#### Scenario: Connect with valid credentials

- GIVEN no central is configured
- WHEN a client posts `{"central_url": "https://central.example.com", "writer_key": "valid-key"}`
- THEN the daemon connects, persists config, and `GET /api/v1/status` returns `central_connected: true`

#### Scenario: Connect with invalid credentials — config not persisted

- GIVEN no central is configured
- WHEN a client posts credentials that fail the remote handshake
- THEN the server responds `422`, the config file retains its prior state, and `GET /api/v1/status` still shows `central_connected: false`

#### Scenario: Disconnect clears credentials and halts sync

- GIVEN the daemon is connected to central
- WHEN a client posts `POST /api/v1/central/disconnect`
- THEN the server responds `200`, the config file no longer contains central credentials, and subsequent sync cycles are not attempted

---

### Requirement: GET /api/v1/projects — project list with policy

`GET /api/v1/projects` SHALL return a JSON array of all projects known to the local store, each including the project name and its current policy (`synced`, `local-only`, or `omitted`). Projects with no explicit policy row MUST be presented using the default policy rule.

#### Scenario: Projects listed with correct policy

- GIVEN the local store contains observations for projects A, B, C where B has policy `local-only`
- WHEN a client calls `GET /api/v1/projects`
- THEN the response includes all three projects with A and C showing `synced` (default) and B showing `local-only`

---

### Requirement: PUT /api/v1/projects/{project}/policy — policy update

`PUT /api/v1/projects/{project}/policy` SHALL accept `{"policy": "synced"|"local-only"|"omitted"}` and persist the policy for the named project. An unknown policy value MUST be rejected with `400 Bad Request`. The change MUST take effect immediately for subsequent writes and sync cycles.

#### Scenario: Valid policy update is persisted

- GIVEN project "my-project" has policy `synced`
- WHEN a client sends `PUT /api/v1/projects/my-project/policy` with `{"policy": "local-only"}`
- THEN the server responds `200` and `GET /api/v1/projects` shows `my-project` with `local-only`

#### Scenario: Unknown policy value is rejected

- GIVEN the control API is running
- WHEN a client sends `PUT /api/v1/projects/my-project/policy` with `{"policy": "invalid-value"}`
- THEN the server responds `400 Bad Request`

---

### Requirement: POST /api/v1/sync/trigger — on-demand sync

`POST /api/v1/sync/trigger` SHALL initiate an immediate sync cycle and return `202 Accepted` with the sync job ID. If central is not configured or not connected, it MUST return `409 Conflict` with a reason.

#### Scenario: Trigger sync when connected

- GIVEN the daemon is connected to central
- WHEN a client posts `POST /api/v1/sync/trigger`
- THEN the server responds `202 Accepted` and a sync cycle begins

#### Scenario: Trigger sync when disconnected is rejected

- GIVEN no central is configured
- WHEN a client posts `POST /api/v1/sync/trigger`
- THEN the server responds `409 Conflict` with reason `"central not configured"`

---

### Requirement: Uniform error model

All error responses MUST be JSON with at least `{"error": "<message>"}`. `4xx` responses MUST include a human-readable reason. `5xx` responses MUST NOT include stack traces or internal paths. API responses MUST carry `Cache-Control: no-store`.

#### Scenario: Error responses are JSON

- GIVEN the control API receives a request resulting in an error
- WHEN the server sends a 4xx or 5xx response
- THEN the response body is valid JSON containing an `"error"` key and the `Content-Type` is `application/json`

---

> **Headless testability**: All requirements in this spec are provable headlessly via HTTP against the running loopback server. No tray or browser required.
