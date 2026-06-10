// Package config provides persistent configuration for the engram resident
// daemon. Config is stored as JSON at %APPDATA%\engram\config.json on Windows
// and os.UserConfigDir()/engram/config.json elsewhere.
//
// Precedence (highest first): CLI flags > environment variables > config file >
// built-in defaults. The ENGRAM_WRITER_KEY environment variable always wins over
// any stored encrypted key — this is enforced in the daemon's flag-resolution
// layer (cmd/engram/daemon.go), not here.
//
// The writer key is never serialised in plaintext. On Windows it is stored as a
// DPAPI-encrypted, base64-encoded blob (secret_windows.go). On other platforms
// the SecretBox implementation returns ErrNoSecretStore for Seal, so the key is
// never written to disk — it must be supplied via ENGRAM_WRITER_KEY.
//
// Atomic writes: Save marshals to a temp file in the same directory then calls
// os.Rename so readers always see a complete file or the previous version.
package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrNoSecretStore is returned by SecretBox implementations that cannot
// persist secrets (e.g. the non-Windows env-only stub). Callers treat this
// as "storage unavailable — use the env var instead."
var ErrNoSecretStore = errors.New("secret store not available on this platform; use ENGRAM_WRITER_KEY env var")

// ValidEmbeddingProviders is the set of accepted embedding_provider values.
// Write-time validation (handleConfigPut) and startup validation (Load) both
// use this set — the PR-③ lesson: an invalid value persisted to disk would
// hard-error the next startup if we only validated at startup.
var ValidEmbeddingProviders = map[string]bool{
	"":       true, // zero value → NoopProvider
	"none":   true, // explicit no-op
	"openai": true,
	"ollama": true, // local sidecar; requires embedding_local_consent=true for local-only projects
}

// SecretBox is the platform-specific encryption interface for the writer key.
// Windows uses DPAPI (user-scoped CryptProtectData / CryptUnprotectData).
// Other platforms return ErrNoSecretStore so callers know to fall back to the
// env var.
type SecretBox interface {
	// Seal encrypts plaintext and returns an opaque ciphertext blob.
	// Returns ErrNoSecretStore when the platform does not support key storage.
	Seal(plaintext []byte) (ciphertext []byte, err error)

	// Open decrypts a ciphertext blob returned by Seal.
	// Returns ErrNoSecretStore when the platform does not support key storage.
	// Returns a non-nil error (not ErrNoSecretStore) when the blob is corrupt or
	// was sealed by a different user/machine — callers must handle this as
	// "key unavailable" and fall back to the env var without crashing.
	Open(ciphertext []byte) (plaintext []byte, err error)
}

// fileConfig is the JSON shape written to and read from config.json.
// EncryptedWriterKey is a base64-encoded DPAPI blob (Windows only); it is
// absent on non-Windows hosts.
type fileConfig struct {
	DB         string `json:"db_path,omitempty"`
	CentralURL string `json:"central_url,omitempty"`
	WriterID   string `json:"writer_id,omitempty"`
	// Tag deliberately "encrypted_writer_key" (NOT "writer_key"): the raw name is
	// already used by the API for the redaction sentinel and the PUT forbidden
	// list — sharing it for the on-disk ciphertext invites confusion and
	// accidental crossover in future refactors.
	EncryptedWriterKey string `json:"encrypted_writer_key,omitempty"` // base64(DPAPI blob)
	HTTPPort           int    `json:"http_port,omitempty"`
	SyncInterval       string `json:"sync_interval,omitempty"` // e.g. "30s"
	LogLevel           string `json:"log_level,omitempty"`
	Transport          string `json:"transport,omitempty"`
	// EmbeddingProvider selects the embedding backend. Valid values: "", "none",
	// "openai", "ollama". An unrecognised value causes Load to return an error
	// (startup-fatal on daemon startup).
	EmbeddingProvider string `json:"embedding_provider,omitempty"`
	// EncryptedEmbeddingKey is a base64-encoded DPAPI blob (Windows only) holding
	// the embedding API key. Absent on non-Windows or when the key has not been
	// set. The plaintext key MUST NEVER appear in this struct or any JSON output.
	// Tag is "encrypted_embedding_key" to mirror "encrypted_writer_key" and to
	// keep the raw key name ("embedding_key") available as a redaction sentinel.
	EncryptedEmbeddingKey string `json:"encrypted_embedding_key,omitempty"` // base64(DPAPI blob)
	// EmbeddingLocalConsent is the explicit opt-in for embedding local-only projects
	// with a local sidecar (ollama). When false (default), local-only project text
	// is never sent to any provider. When true, a local sidecar may embed it.
	// Has no effect for remote providers (OpenAI) — local-only is always denied there.
	EmbeddingLocalConsent bool `json:"embedding_local_consent,omitempty"`
	// EmbeddingDims is the expected embedding vector dimensionality. Default 256.
	// Must match the model's actual output dimensions; mismatched stored BLOBs are
	// treated as stale and re-embedded by the backfill loop.
	EmbeddingDims int `json:"embedding_dims,omitempty"`
	// OllamaHost is the base URL of the local Ollama sidecar. Default "http://localhost:11434".
	// Only used when EmbeddingProvider = "ollama".
	OllamaHost string `json:"ollama_host,omitempty"`
}

// Config is the resolved, decoded in-memory configuration. The writer key is
// stored as raw bytes (decrypted) ONLY in the daemon's process memory; it is
// NEVER written to disk in plaintext. EncryptedWriterKey is the ciphertext blob
// read from disk that the daemon decrypts once at startup.
type Config struct {
	DB                 string
	CentralURL         string
	WriterID           string
	EncryptedWriterKey []byte // DPAPI ciphertext; nil when not set or non-Windows
	HTTPPort           int
	SyncInterval       time.Duration // 0 → caller uses default
	LogLevel           string
	Transport          string

	// EmbeddingProvider is the embedding backend name after validation.
	// Valid values: "", "none", "openai", "ollama". Load returns an error for any other value.
	EmbeddingProvider string

	// EmbeddingLocalConsent is true when the user has explicitly opted in to
	// embedding local-only project text with a local sidecar (ollama). Default false.
	EmbeddingLocalConsent bool

	// EmbeddingDims is the configured embedding vector dimensionality. 0 means
	// "use provider default" (256 for OpenAI; 256 for Ollama when not specified).
	EmbeddingDims int

	// OllamaHost is the base URL of the local Ollama sidecar.
	// Empty means "use default" (http://localhost:11434).
	OllamaHost string

	// embeddingKeyActive records whether an embedding key is available at
	// runtime — either from ENGRAM_EMBEDDING_KEY env var OR from the stored
	// encrypted blob. Set by the daemon after resolving the key. Used by
	// Redact() to produce EmbeddingKeySet=true for either source.
	//
	// Unexported, no json tag — never serialised.
	embeddingKeyActive bool

	// encryptedEmbeddingKey is the decrypted ciphertext blob for the embedding API
	// key. The plaintext key is NEVER stored here — only the ciphertext, to be
	// opened by SecretBox.Open at daemon startup. Unexported, no json tag.
	encryptedEmbeddingKey []byte
}

// WithEmbeddingKeyActive returns a copy of c with the runtime embedding-key
// active flag set. Called by the daemon after resolving whether a key is
// available (env var wins over file). The flag drives EmbeddingKeySet in
// Redact() — true when a key is active from ANY source.
func (c Config) WithEmbeddingKeyActive(active bool) Config {
	c.embeddingKeyActive = active
	return c
}

// EmbeddingKeyActive reports whether an embedding key is currently active
// (resolved from env OR file). This is the value that Redact() surfaces as
// EmbeddingKeySet.
func (c Config) EmbeddingKeyActive() bool {
	return c.embeddingKeyActive
}

// EncryptedEmbeddingKey returns the encrypted embedding key ciphertext blob.
// This is the sealed blob, NOT the plaintext key. Used by the daemon to decrypt
// at startup via SecretBox.Open. Returns nil when not set.
func (c Config) EncryptedEmbeddingKey() []byte {
	return c.encryptedEmbeddingKey
}

// WithEncryptedEmbeddingKey returns a copy of c with the encrypted embedding
// key set. Used by Save and by test helpers that need to inject a ciphertext.
func (c Config) WithEncryptedEmbeddingKey(blob []byte) Config {
	c.encryptedEmbeddingKey = blob
	return c
}

// RedactedConfig is the config view returned by GET /api/v1/config.
// WriterKeySet is true when an encrypted key is stored; the key itself is
// never present. EmbeddingKeySet is true when an encrypted embedding API key is
// stored; the key itself is never present.
type RedactedConfig struct {
	DB               string `json:"db_path,omitempty"`
	CentralURL       string `json:"central_url,omitempty"`
	WriterID         string `json:"writer_id,omitempty"`
	WriterKey        string `json:"writer_key,omitempty"` // "***REDACTED***" or absent
	HTTPPort         int    `json:"http_port,omitempty"`
	SyncInterval     string `json:"sync_interval,omitempty"`
	LogLevel         string `json:"log_level,omitempty"`
	Transport        string `json:"transport,omitempty"`
	EmbeddingProvider string `json:"embedding_provider,omitempty"`
	EmbeddingKeySet  bool   `json:"embedding_key_set,omitempty"` // true when encrypted key present
}

// ConfigPatch is a partial update applied by PUT /api/v1/config.
// Only non-nil fields are merged. writer_key, central_url, and
// encrypted_embedding_key are rejected at the handler level — they must never
// appear in a ConfigPatch (encrypted_embedding_key must be set via the dedicated
// key-management endpoint to ensure proper sealing).
type ConfigPatch struct {
	SyncInterval          *string `json:"sync_interval,omitempty"`
	LogLevel              *string `json:"log_level,omitempty"`
	HTTPPort              *int    `json:"http_port,omitempty"`
	DBPath                *string `json:"db_path,omitempty"`
	Transport             *string `json:"transport,omitempty"`
	EmbeddingProvider     *string `json:"embedding_provider,omitempty"`
	EmbeddingLocalConsent *bool   `json:"embedding_local_consent,omitempty"`
	EmbeddingDims         *int    `json:"embedding_dims,omitempty"`
	OllamaHost            *string `json:"ollama_host,omitempty"`
}

// DefaultConfigDir returns the directory where config.json is stored:
// %APPDATA%\engram on Windows, os.UserConfigDir()/engram elsewhere.
// Returns an error when the OS cannot determine the user config directory.
func DefaultConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: cannot determine user config dir: %w", err)
	}
	return filepath.Join(base, "engram"), nil
}

// Load reads config.json from dir. If the file does not exist a zero-value
// Config is returned without an error. Returns an error only for IO or parse
// failures on an existing file.
func Load(dir string) (Config, error) {
	path := filepath.Join(dir, "config.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("config.Load: read %s: %w", path, err)
	}

	var fc fileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return Config{}, fmt.Errorf("config.Load: parse %s: %w", path, err)
	}

	cfg := Config{
		DB:         fc.DB,
		CentralURL: fc.CentralURL,
		WriterID:   fc.WriterID,
		HTTPPort:   fc.HTTPPort,
		LogLevel:   fc.LogLevel,
		Transport:  fc.Transport,
	}

	if fc.SyncInterval != "" {
		d, err := time.ParseDuration(fc.SyncInterval)
		if err != nil {
			return Config{}, fmt.Errorf("config.Load: sync_interval %q: %w", fc.SyncInterval, err)
		}
		cfg.SyncInterval = d
	}

	if fc.EncryptedWriterKey != "" {
		blob, err := base64.StdEncoding.DecodeString(fc.EncryptedWriterKey)
		if err != nil {
			return Config{}, fmt.Errorf("config.Load: writer_key base64 decode: %w", err)
		}
		cfg.EncryptedWriterKey = blob
	}

	// Validate embedding provider. An unrecognised value is startup-fatal so that
	// a bad value persisted to disk is caught immediately rather than silently
	// falling back to noop (hiding a misconfiguration).
	if !ValidEmbeddingProviders[fc.EmbeddingProvider] {
		return Config{}, fmt.Errorf("config.Load: unsupported embedding_provider %q (valid: \"\", \"none\", \"openai\")", fc.EmbeddingProvider)
	}
	cfg.EmbeddingProvider = fc.EmbeddingProvider

	if fc.EncryptedEmbeddingKey != "" {
		blob, err := base64.StdEncoding.DecodeString(fc.EncryptedEmbeddingKey)
		if err != nil {
			return Config{}, fmt.Errorf("config.Load: embedding_key base64 decode: %w", err)
		}
		cfg.encryptedEmbeddingKey = blob
	}

	cfg.EmbeddingLocalConsent = fc.EmbeddingLocalConsent
	cfg.EmbeddingDims = fc.EmbeddingDims
	cfg.OllamaHost = fc.OllamaHost

	return cfg, nil
}

// Save writes cfg to config.json in dir using an atomic temp-file + rename.
// dir is created if it does not exist (0700). The caller is responsible for
// ensuring EncryptedWriterKey is already sealed before calling Save.
func Save(dir string, cfg Config) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("config.Save: mkdir %s: %w", dir, err)
	}

	fc := fileConfig{
		DB:         cfg.DB,
		CentralURL: cfg.CentralURL,
		WriterID:   cfg.WriterID,
		HTTPPort:   cfg.HTTPPort,
		LogLevel:   cfg.LogLevel,
		Transport:  cfg.Transport,
	}

	if cfg.SyncInterval > 0 {
		fc.SyncInterval = cfg.SyncInterval.String()
	}

	if len(cfg.EncryptedWriterKey) > 0 {
		fc.EncryptedWriterKey = base64.StdEncoding.EncodeToString(cfg.EncryptedWriterKey)
	}

	fc.EmbeddingProvider = cfg.EmbeddingProvider
	fc.EmbeddingLocalConsent = cfg.EmbeddingLocalConsent
	fc.EmbeddingDims = cfg.EmbeddingDims
	fc.OllamaHost = cfg.OllamaHost

	if len(cfg.encryptedEmbeddingKey) > 0 {
		fc.EncryptedEmbeddingKey = base64.StdEncoding.EncodeToString(cfg.encryptedEmbeddingKey)
	}

	data, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return fmt.Errorf("config.Save: marshal: %w", err)
	}
	data = append(data, '\n') // trailing newline convention

	// Atomic write: temp file in the same directory, then rename.
	tmp, err := os.CreateTemp(dir, "config.*.json.tmp")
	if err != nil {
		return fmt.Errorf("config.Save: create temp: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("config.Save: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("config.Save: close temp: %w", err)
	}

	dest := filepath.Join(dir, "config.json")
	if err := os.Rename(tmpPath, dest); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("config.Save: rename to %s: %w", dest, err)
	}
	return nil
}

// Redact returns a RedactedConfig safe to expose over the API.
// If EncryptedWriterKey is set the field value is "***REDACTED***"; if absent
// the field is omitted from the JSON output (empty string with omitempty).
func (c Config) Redact() RedactedConfig {
	rc := RedactedConfig{
		DB:         c.DB,
		CentralURL: c.CentralURL,
		WriterID:   c.WriterID,
		HTTPPort:   c.HTTPPort,
		LogLevel:   c.LogLevel,
		Transport:  c.Transport,
	}
	if c.SyncInterval > 0 {
		rc.SyncInterval = c.SyncInterval.String()
	}
	if len(c.EncryptedWriterKey) > 0 {
		rc.WriterKey = "***REDACTED***"
	}
	rc.EmbeddingProvider = c.EmbeddingProvider
	// EmbeddingKeySet is true when a key is active from ANY source:
	// ENGRAM_EMBEDDING_KEY env var (embeddingKeyActive set by daemon) OR
	// encrypted blob stored in the config file.
	if c.embeddingKeyActive || len(c.encryptedEmbeddingKey) > 0 {
		rc.EmbeddingKeySet = true
	}
	return rc
}

// Patch applies patch to base and returns the updated config plus a flag
// indicating whether any restart-required field was changed.
//
// Restart-required fields: DBPath, HTTPPort, Transport.
// Runtime-mutable fields: SyncInterval, LogLevel.
func Patch(base Config, p ConfigPatch) (Config, bool) {
	restartRequired := false
	out := base

	if p.SyncInterval != nil {
		if *p.SyncInterval == "" {
			out.SyncInterval = 0
		} else {
			d, err := time.ParseDuration(*p.SyncInterval)
			if err == nil {
				out.SyncInterval = d
			}
			// Invalid duration: ignore silently (validation is the handler's job).
		}
	}

	if p.LogLevel != nil {
		out.LogLevel = *p.LogLevel
	}

	if p.HTTPPort != nil && *p.HTTPPort != base.HTTPPort {
		out.HTTPPort = *p.HTTPPort
		restartRequired = true
	}

	if p.DBPath != nil && *p.DBPath != base.DB {
		out.DB = *p.DBPath
		restartRequired = true
	}

	if p.Transport != nil && *p.Transport != base.Transport {
		out.Transport = *p.Transport
		restartRequired = true
	}

	// EmbeddingProvider is runtime-mutable (no restart needed). The handler must
	// validate against ValidEmbeddingProviders before calling Patch.
	if p.EmbeddingProvider != nil {
		out.EmbeddingProvider = *p.EmbeddingProvider
	}

	if p.EmbeddingLocalConsent != nil {
		out.EmbeddingLocalConsent = *p.EmbeddingLocalConsent
	}

	if p.EmbeddingDims != nil {
		out.EmbeddingDims = *p.EmbeddingDims
	}

	if p.OllamaHost != nil {
		out.OllamaHost = *p.OllamaHost
	}

	return out, restartRequired
}
