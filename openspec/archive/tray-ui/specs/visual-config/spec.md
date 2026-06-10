# Visual Config Specification

## Purpose

Defines the config file surface that replaces env-vars-only setup. Flags and environment variables remain valid overrides. DPAPI encryption protects the writer key at rest on Windows.

---

## Requirements

### Requirement: Config file location and schema

The config file SHALL be located at `%APPDATA%\engram\config.json` on Windows. The file is a JSON object containing at minimum the following optional fields: `db_path`, `central_url`, `writer_key` (DPAPI-encrypted ciphertext on Windows), `http_port`, `sync_interval`, `log_level`, `transport`. Absent fields indicate the setting is unset (daemon uses its hardcoded default or the corresponding flag/env value).

#### Scenario: Config file is created on first write

- GIVEN no config file exists
- WHEN the daemon first writes a config setting via `PUT /api/v1/config` or `POST /api/v1/central/connect`
- THEN the file is created at `%APPDATA%\engram\config.json` with the written fields

#### Scenario: Absent config file means daemon uses defaults

- GIVEN no config file exists and no env vars or flags are set
- WHEN the daemon starts
- THEN it starts with hardcoded defaults and no error is raised

---

### Requirement: Config precedence order

Settings are resolved in this strict precedence (highest wins):
1. **CLI flags** (e.g., `--db`, `--central-url`, `--writer-key` — EXCEPT writer key is never a flag; see secret handling below)
2. **Environment variables** (e.g., `ENGRAM_WRITER_KEY`, `ENGRAM_DB_PATH`)
3. **Config file** (`%APPDATA%\engram\config.json`)
4. **Hardcoded defaults**

The `ENGRAM_WRITER_KEY` environment variable MUST always win over any value stored in the config file, including on Windows where DPAPI encryption is available.

#### Scenario: Env var overrides config file

- GIVEN config file has `"central_url": "https://file.example.com"` and env var `ENGRAM_CENTRAL_URL=https://env.example.com` is set
- WHEN the daemon resolves the central URL
- THEN the effective value is `https://env.example.com`

#### Scenario: ENGRAM_WRITER_KEY env var overrides DPAPI-encrypted config

- GIVEN a DPAPI-encrypted writer key exists in `config.json`
- AND `ENGRAM_WRITER_KEY=env-key-value` is set in the environment
- WHEN the daemon resolves the writer key
- THEN the effective value is `env-key-value` (env wins)

---

### Requirement: DPAPI-encrypted writer key at rest (Windows)

On Windows, the writer key MUST be encrypted using DPAPI (`CryptProtectData` user-scope) before being written to `config.json`. The encrypted blob MUST be stored as a base64-encoded string. The daemon MUST decrypt it using `CryptUnprotectData` on read. DPAPI is bound to the current Windows user — the key is not recoverable by other OS users.

On non-Windows platforms, the writer key MUST NOT be stored in the config file. If the user configures a writer key on a non-Windows platform, the daemon MUST inform them that only env-var storage is supported on that platform.

#### Scenario: Writer key encrypted on config write (Windows)

- GIVEN the daemon is running on Windows
- WHEN a writer key is persisted via `POST /api/v1/central/connect`
- THEN `config.json` contains a DPAPI-encrypted blob for the key (not the raw key value)
- AND reading the config file as a text file does NOT reveal the plaintext key

#### Scenario: Writer key decrypts correctly for the same Windows user

- GIVEN a DPAPI-encrypted writer key in `config.json`
- WHEN the same Windows user restarts the daemon
- THEN the key is successfully decrypted and used for central authentication

#### Scenario: Non-Windows: writer key cannot be stored in config file

- GIVEN the daemon is running on Linux or macOS
- WHEN a user attempts to persist a writer key
- THEN the daemon informs the user to use `ENGRAM_WRITER_KEY` env var instead
- AND no key value is written to the config file

---

### Requirement: Writer key never-leak guarantees

The writer key MUST NEVER appear in:
- A CLI flag default value
- `--help` output
- Daemon log lines at any log level
- `GET /api/v1/config` response (MUST be redacted as `"***REDACTED***"`)
- Any HTTP response body or header

The existing regression guard `TestRun_DaemonHelp_DoesNotLeakWriterKey` MUST remain green after this change.

#### Scenario: --help output does not mention writer key

- GIVEN the engram binary is invoked with `--help` or `help`
- WHEN the output is captured
- THEN the output contains no writer key value, no ENGRAM_WRITER_KEY value, and no path to the key's stored location

#### Scenario: GET /api/v1/config redacts writer key

- GIVEN the writer key is set
- WHEN a client calls `GET /api/v1/config`
- THEN the response body contains `"writer_key": "***REDACTED***"` and does NOT contain the actual key

---

### Requirement: Runtime-mutable vs restart-required settings

Settings are classified for their effective scope:
- **Runtime-mutable** (take effect immediately on `PUT /api/v1/config`): `sync_interval`, `log_level`.
- **Restart-required** (flagged in the API response; daemon must be restarted for the change to take effect): `db_path`, `http_port`, `transport`.
- `central_url` and `writer_key` are managed via `POST /api/v1/central/connect|disconnect` — NOT via `PUT /api/v1/config`.

#### Scenario: Runtime-mutable setting applied without restart

- GIVEN the daemon is running with `sync_interval: "60s"`
- WHEN `PUT /api/v1/config` with `{"sync_interval": "30s"}` is sent
- THEN the response includes `"restart_required": false` and subsequent sync cycles run at 30s

#### Scenario: Restart-required setting signals need for restart

- GIVEN the daemon is running on port 7700
- WHEN `PUT /api/v1/config` with `{"http_port": 7701}` is sent
- THEN the response includes `"restart_required": true` and port remains 7700 until restart

---

### Requirement: Config file round-trip fidelity

Writing a config and re-reading it MUST yield semantically identical values for all fields. Encoding/decoding transformations (e.g., duration strings, base64 DPAPI blobs) MUST be reversible.

#### Scenario: Config round-trip — written fields match read fields

- GIVEN `{"sync_interval": "45s", "log_level": "debug"}` is written via `PUT /api/v1/config`
- WHEN the daemon is restarted and `GET /api/v1/config` is called
- THEN the response contains `"sync_interval": "45s"` and `"log_level": "debug"`

---

> **Headless testability**: All requirements except the DPAPI Windows-bound decrypt scenario are provable headlessly. DPAPI scenarios require a Windows test environment (Windows CI). The never-leak guarantee for `--help` is provable via the existing `TestRun_DaemonHelp_DoesNotLeakWriterKey` test.
