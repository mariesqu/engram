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
	DB                  string  `json:"db_path,omitempty"`
	CentralURL          string  `json:"central_url,omitempty"`
	WriterID            string  `json:"writer_id,omitempty"`
	EncryptedWriterKey  string  `json:"writer_key,omitempty"` // base64(DPAPI blob)
	HTTPPort            int     `json:"http_port,omitempty"`
	SyncInterval        string  `json:"sync_interval,omitempty"` // e.g. "30s"
	LogLevel            string  `json:"log_level,omitempty"`
	Transport           string  `json:"transport,omitempty"`
}

// Config is the resolved, decoded in-memory configuration. The writer key is
// stored as raw bytes (decrypted) ONLY in the daemon's process memory; it is
// NEVER written to disk in plaintext. EncryptedWriterKey is the ciphertext blob
// read from disk that the daemon decrypts once at startup.
type Config struct {
	DB                 string
	CentralURL         string
	WriterID           string
	EncryptedWriterKey []byte        // DPAPI ciphertext; nil when not set or non-Windows
	HTTPPort           int
	SyncInterval       time.Duration // 0 → caller uses default
	LogLevel           string
	Transport          string
}

// RedactedConfig is the config view returned by GET /api/v1/config.
// WriterKeySet is true when an encrypted key is stored; the key itself is
// never present.
type RedactedConfig struct {
	DB           string `json:"db_path,omitempty"`
	CentralURL   string `json:"central_url,omitempty"`
	WriterID     string `json:"writer_id,omitempty"`
	WriterKey    string `json:"writer_key,omitempty"` // "***REDACTED***" or absent
	HTTPPort     int    `json:"http_port,omitempty"`
	SyncInterval string `json:"sync_interval,omitempty"`
	LogLevel     string `json:"log_level,omitempty"`
	Transport    string `json:"transport,omitempty"`
}

// ConfigPatch is a partial update applied by PUT /api/v1/config.
// Only non-nil fields are merged. writer_key and central_url are rejected
// at the handler level — they must never appear in a ConfigPatch.
type ConfigPatch struct {
	SyncInterval *string `json:"sync_interval,omitempty"`
	LogLevel     *string `json:"log_level,omitempty"`
	HTTPPort     *int    `json:"http_port,omitempty"`
	DBPath       *string `json:"db_path,omitempty"`
	Transport    *string `json:"transport,omitempty"`
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
		DB:           cfg.DB,
		CentralURL:   cfg.CentralURL,
		WriterID:     cfg.WriterID,
		HTTPPort:     cfg.HTTPPort,
		LogLevel:     cfg.LogLevel,
		Transport:    cfg.Transport,
	}

	if cfg.SyncInterval > 0 {
		fc.SyncInterval = cfg.SyncInterval.String()
	}

	if len(cfg.EncryptedWriterKey) > 0 {
		fc.EncryptedWriterKey = base64.StdEncoding.EncodeToString(cfg.EncryptedWriterKey)
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
		DB:           c.DB,
		CentralURL:   c.CentralURL,
		WriterID:     c.WriterID,
		HTTPPort:     c.HTTPPort,
		LogLevel:     c.LogLevel,
		Transport:    c.Transport,
	}
	if c.SyncInterval > 0 {
		rc.SyncInterval = c.SyncInterval.String()
	}
	if len(c.EncryptedWriterKey) > 0 {
		rc.WriterKey = "***REDACTED***"
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

	return out, restartRequired
}
