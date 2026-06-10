package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestConfig_RoundTrip verifies that Save+Load produces the same Config.
func TestConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	original := Config{
		DB:           filepath.Join(dir, "test.db"),
		CentralURL:   "https://central.example.com",
		WriterID:     "test-writer",
		HTTPPort:     7700,
		SyncInterval: 45 * time.Second,
		LogLevel:     "debug",
		Transport:    "stdio",
	}

	if err := Save(dir, original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.DB != original.DB {
		t.Errorf("DB: got %q, want %q", loaded.DB, original.DB)
	}
	if loaded.CentralURL != original.CentralURL {
		t.Errorf("CentralURL: got %q, want %q", loaded.CentralURL, original.CentralURL)
	}
	if loaded.WriterID != original.WriterID {
		t.Errorf("WriterID: got %q, want %q", loaded.WriterID, original.WriterID)
	}
	if loaded.HTTPPort != original.HTTPPort {
		t.Errorf("HTTPPort: got %d, want %d", loaded.HTTPPort, original.HTTPPort)
	}
	if loaded.SyncInterval != original.SyncInterval {
		t.Errorf("SyncInterval: got %v, want %v", loaded.SyncInterval, original.SyncInterval)
	}
	if loaded.LogLevel != original.LogLevel {
		t.Errorf("LogLevel: got %q, want %q", loaded.LogLevel, original.LogLevel)
	}
	if loaded.Transport != original.Transport {
		t.Errorf("Transport: got %q, want %q", loaded.Transport, original.Transport)
	}
}

// TestConfig_AbsentFile_ReturnsDefault verifies that Load returns a zero-value
// Config when no file exists, without an error.
func TestConfig_AbsentFile_ReturnsDefault(t *testing.T) {
	dir := t.TempDir()

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load of absent file returned error: %v", err)
	}
	if cfg.DB != "" || cfg.CentralURL != "" || cfg.HTTPPort != 0 || cfg.SyncInterval != 0 {
		t.Errorf("Load of absent file returned non-zero Config: %+v", cfg)
	}
}

// TestConfig_Redact_KeySet verifies that Redact returns "***REDACTED***"
// when EncryptedWriterKey is present.
func TestConfig_Redact_KeySet(t *testing.T) {
	cfg := Config{
		DB:                 "/tmp/test.db",
		EncryptedWriterKey: []byte{0x01, 0x02, 0x03},
	}
	rc := cfg.Redact()
	if rc.WriterKey != "***REDACTED***" {
		t.Errorf("Redact with key set: got %q, want %q", rc.WriterKey, "***REDACTED***")
	}
	// Must not expose DB via WriterKey — sanity check
	if rc.DB != cfg.DB {
		t.Errorf("Redact: DB lost: got %q, want %q", rc.DB, cfg.DB)
	}
}

// TestConfig_Redact_KeyAbsent verifies that Redact omits writer_key when no
// key is stored (EncryptedWriterKey is nil).
func TestConfig_Redact_KeyAbsent(t *testing.T) {
	cfg := Config{DB: "/tmp/test.db"}
	rc := cfg.Redact()
	if rc.WriterKey != "" {
		t.Errorf("Redact with no key: got %q, want empty (omitted)", rc.WriterKey)
	}
}

// TestConfig_Patch_RuntimeMutable_NoRestart verifies that patching runtime-
// mutable fields (SyncInterval, LogLevel) returns restartRequired=false and
// applies the change.
func TestConfig_Patch_RuntimeMutable_NoRestart(t *testing.T) {
	base := Config{
		SyncInterval: 60 * time.Second,
		LogLevel:     "info",
		HTTPPort:     7700,
	}

	interval := "30s"
	level := "debug"
	p := ConfigPatch{
		SyncInterval: &interval,
		LogLevel:     &level,
	}

	updated, restart := Patch(base, p)
	if restart {
		t.Error("Patch of runtime-mutable fields should not require restart")
	}
	if updated.SyncInterval != 30*time.Second {
		t.Errorf("SyncInterval: got %v, want 30s", updated.SyncInterval)
	}
	if updated.LogLevel != "debug" {
		t.Errorf("LogLevel: got %q, want %q", updated.LogLevel, "debug")
	}
	// Unchanged field must be preserved.
	if updated.HTTPPort != 7700 {
		t.Errorf("HTTPPort should be unchanged: got %d, want 7700", updated.HTTPPort)
	}
}

// TestConfig_Patch_RestartRequired verifies that patching restart-required
// fields (HTTPPort, DBPath, Transport) sets restartRequired=true.
func TestConfig_Patch_RestartRequired(t *testing.T) {
	base := Config{
		HTTPPort:  7700,
		DB:        "/old/path.db",
		Transport: "stdio",
	}

	port := 7701
	p := ConfigPatch{HTTPPort: &port}

	_, restart := Patch(base, p)
	if !restart {
		t.Error("Patch of HTTPPort should require restart")
	}

	dbPath := "/new/path.db"
	p2 := ConfigPatch{DBPath: &dbPath}
	_, restart2 := Patch(base, p2)
	if !restart2 {
		t.Error("Patch of DBPath should require restart")
	}

	transport := "http"
	p3 := ConfigPatch{Transport: &transport}
	_, restart3 := Patch(base, p3)
	if !restart3 {
		t.Error("Patch of Transport should require restart")
	}
}

// TestConfig_Patch_NoChange_NoRestart verifies that patching a restart-required
// field with the same value does NOT set restartRequired.
func TestConfig_Patch_NoChange_NoRestart(t *testing.T) {
	base := Config{HTTPPort: 7700}
	port := 7700
	p := ConfigPatch{HTTPPort: &port}

	_, restart := Patch(base, p)
	if restart {
		t.Error("Patch with same HTTPPort value should not require restart")
	}
}

// TestConfig_AtomicWrite verifies that Save is atomic: a concurrent reader
// never sees a partial write. We verify atomicity by checking that the file
// is always valid JSON after Save returns.
func TestConfig_AtomicWrite(t *testing.T) {
	dir := t.TempDir()

	for i := range 5 {
		cfg := Config{
			HTTPPort:     7700 + i,
			SyncInterval: time.Duration(i+1) * 10 * time.Second,
		}
		if err := Save(dir, cfg); err != nil {
			t.Fatalf("iteration %d: Save: %v", i, err)
		}
		// Read back immediately — must never fail.
		loaded, err := Load(dir)
		if err != nil {
			t.Fatalf("iteration %d: Load after Save: %v", i, err)
		}
		if loaded.HTTPPort != cfg.HTTPPort {
			t.Errorf("iteration %d: HTTPPort: got %d, want %d", i, loaded.HTTPPort, cfg.HTTPPort)
		}
	}
}

// TestConfig_RoundTrip_WithEncryptedKey verifies that a config with an
// EncryptedWriterKey (arbitrary bytes) round-trips correctly through
// Save+Load (base64 encoding).
func TestConfig_RoundTrip_WithEncryptedKey(t *testing.T) {
	dir := t.TempDir()

	// Simulate a sealed key blob (not actual DPAPI output — just bytes).
	fakeBlob := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x01, 0x02, 0x03}

	cfg := Config{
		DB:                 "/test.db",
		EncryptedWriterKey: fakeBlob,
	}

	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if string(loaded.EncryptedWriterKey) != string(fakeBlob) {
		t.Errorf("EncryptedWriterKey round-trip mismatch: got %v, want %v",
			loaded.EncryptedWriterKey, fakeBlob)
	}
}

// TestConfig_Save_CreatesDir verifies that Save creates the config directory
// when it does not exist.
func TestConfig_Save_CreatesDir(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "subdir", "engram")

	if err := Save(dir, Config{DB: "/test.db"}); err != nil {
		t.Fatalf("Save with missing dir: %v", err)
	}

	cfgPath := filepath.Join(dir, "config.json")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("config.json not created: %v", err)
	}
}

// --- Embedding provider tests (task 4.1) ---

// TestConfig_EmbeddingProvider_RoundTrip verifies that EmbeddingProvider and
// encryptedEmbeddingKey survive a Save+Load cycle.
func TestConfig_EmbeddingProvider_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	fakeBlob := []byte{0xCA, 0xFE, 0xBA, 0xBE}
	original := Config{
		DB:                "test.db",
		EmbeddingProvider: "openai",
	}
	original = original.WithEncryptedEmbeddingKey(fakeBlob)

	if err := Save(dir, original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.EmbeddingProvider != "openai" {
		t.Errorf("EmbeddingProvider: got %q, want %q", loaded.EmbeddingProvider, "openai")
	}
	if string(loaded.EncryptedEmbeddingKey()) != string(fakeBlob) {
		t.Errorf("EncryptedEmbeddingKey round-trip mismatch: got %v, want %v",
			loaded.EncryptedEmbeddingKey(), fakeBlob)
	}
}

// TestConfig_Redact_EmbeddingKeySet_True verifies EmbeddingKeySet=true when
// an encrypted embedding key blob is present.
func TestConfig_Redact_EmbeddingKeySet_True(t *testing.T) {
	cfg := Config{EmbeddingProvider: "openai"}
	cfg = cfg.WithEncryptedEmbeddingKey([]byte{0x01, 0x02})

	rc := cfg.Redact()
	if !rc.EmbeddingKeySet {
		t.Error("Redact: EmbeddingKeySet should be true when encrypted key is present")
	}
	if rc.EmbeddingProvider != "openai" {
		t.Errorf("Redact: EmbeddingProvider: got %q, want %q", rc.EmbeddingProvider, "openai")
	}
}

// TestConfig_Redact_EmbeddingKeySet_False verifies EmbeddingKeySet=false when
// no encrypted embedding key is stored.
func TestConfig_Redact_EmbeddingKeySet_False(t *testing.T) {
	cfg := Config{EmbeddingProvider: "openai"}

	rc := cfg.Redact()
	if rc.EmbeddingKeySet {
		t.Error("Redact: EmbeddingKeySet should be false when no encrypted key is stored")
	}
}

// TestConfig_Patch_EmbeddingProvider verifies that patching EmbeddingProvider
// is runtime-mutable (no restart required) and applies the change.
func TestConfig_Patch_EmbeddingProvider(t *testing.T) {
	base := Config{EmbeddingProvider: ""}

	provider := "openai"
	p := ConfigPatch{EmbeddingProvider: &provider}

	updated, restart := Patch(base, p)
	if restart {
		t.Error("Patch of EmbeddingProvider should not require restart")
	}
	if updated.EmbeddingProvider != "openai" {
		t.Errorf("EmbeddingProvider: got %q, want %q", updated.EmbeddingProvider, "openai")
	}
}

// TestConfig_Load_ValidProviders_OK verifies that all valid embedding_provider
// values are accepted by Load without error.
func TestConfig_Load_ValidProviders_OK(t *testing.T) {
	validProviders := []string{"", "none", "openai"}

	for _, provider := range validProviders {
		t.Run("provider="+provider, func(t *testing.T) {
			dir := t.TempDir()
			cfg := Config{EmbeddingProvider: provider}
			if err := Save(dir, cfg); err != nil {
				t.Fatalf("Save: %v", err)
			}
			loaded, err := Load(dir)
			if err != nil {
				t.Fatalf("Load with provider %q: %v", provider, err)
			}
			if loaded.EmbeddingProvider != provider {
				t.Errorf("EmbeddingProvider: got %q, want %q", loaded.EmbeddingProvider, provider)
			}
		})
	}
}

// TestConfig_OllamaFields_RoundTrip verifies that the three new PR-2 fields
// (EmbeddingLocalConsent, EmbeddingDims, OllamaHost) survive a Save/Load cycle.
func TestConfig_OllamaFields_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Build a config with all three new fields set.
	cfg := Config{
		EmbeddingProvider:     "ollama",
		EmbeddingLocalConsent: true,
		EmbeddingDims:         768,
		OllamaHost:            "http://localhost:11434",
	}

	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.EmbeddingProvider != "ollama" {
		t.Errorf("EmbeddingProvider: got %q, want %q", loaded.EmbeddingProvider, "ollama")
	}
	if !loaded.EmbeddingLocalConsent {
		t.Error("EmbeddingLocalConsent: got false, want true")
	}
	if loaded.EmbeddingDims != 768 {
		t.Errorf("EmbeddingDims: got %d, want 768", loaded.EmbeddingDims)
	}
	if loaded.OllamaHost != "http://localhost:11434" {
		t.Errorf("OllamaHost: got %q, want %q", loaded.OllamaHost, "http://localhost:11434")
	}
}

// TestConfig_EmbeddingKeySet_FromEnv verifies that WithEmbeddingKeyActive(true)
// causes Redact() to return EmbeddingKeySet=true even when no encrypted key is
// stored in the file. This is the carry-forward fix: an env-var key must also
// set the flag.
func TestConfig_EmbeddingKeySet_FromEnv(t *testing.T) {
	cfg := Config{}
	cfg = cfg.WithEmbeddingKeyActive(true)

	redacted := cfg.Redact()
	if !redacted.EmbeddingKeySet {
		t.Error("EmbeddingKeySet should be true when embeddingKeyActive=true (env-var key)")
	}
}

// TestConfig_EmbeddingKeySet_FromFile verifies that EmbeddingKeySet=true is
// set when an encrypted key blob is present in the config (file-stored key).
func TestConfig_EmbeddingKeySet_FromFile(t *testing.T) {
	cfg := Config{}
	cfg = cfg.WithEncryptedEmbeddingKey([]byte("some-ciphertext"))

	redacted := cfg.Redact()
	if !redacted.EmbeddingKeySet {
		t.Error("EmbeddingKeySet should be true when encryptedEmbeddingKey is non-empty")
	}
}

// TestConfig_EmbeddingKeySet_Neither verifies EmbeddingKeySet=false when
// neither env-var nor file key is active.
func TestConfig_EmbeddingKeySet_Neither(t *testing.T) {
	cfg := Config{}
	redacted := cfg.Redact()
	if redacted.EmbeddingKeySet {
		t.Error("EmbeddingKeySet should be false when no key is active")
	}
}

// TestConfig_Load_InvalidEmbeddingProvider_ReturnsError verifies that Load
// returns an error (startup-fatal) when an unrecognised embedding_provider is
// present in the config file. This prevents silent noop fallback on
// misconfiguration.
func TestConfig_Load_InvalidEmbeddingProvider_ReturnsError(t *testing.T) {
	dir := t.TempDir()

	// Write a config file directly with an invalid provider value.
	raw := `{"embedding_provider": "gpt-embeddings"}` // not a valid provider
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(raw), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load with invalid embedding_provider should return error, got nil")
	}
}
