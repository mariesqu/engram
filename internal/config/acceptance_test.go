//go:build acceptance

package config_test

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/config"
)

// TestAcceptance_Config_Suite runs the full config acceptance suite.
// Cases:
//  1. Config round-trip identical after restart simulation.
//  2. Redact() never returns the raw key.
//  3. DPAPI seal/open round-trip on Windows (guarded by runtime.GOOS).
//  4. Decrypt failure degrades gracefully (no crash, ErrNoSecretStore or
//     other non-nil error, caller can fall back to env).
func TestAcceptance_Config_Suite(t *testing.T) {
	t.Run("RoundTrip_IdenticalAfterReload", func(t *testing.T) {
		dir := t.TempDir()
		original := config.Config{
			DB:           "/data/engram.db",
			CentralURL:   "https://central.example.com",
			WriterID:     "acceptance-writer",
			HTTPPort:     7700,
			SyncInterval: 45 * time.Second,
			LogLevel:     "info",
			Transport:    "stdio",
		}

		if err := config.Save(dir, original); err != nil {
			t.Fatalf("Save: %v", err)
		}

		// Simulate restart: load from disk.
		loaded, err := config.Load(dir)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		if loaded.DB != original.DB {
			t.Errorf("DB mismatch: got %q, want %q", loaded.DB, original.DB)
		}
		if loaded.CentralURL != original.CentralURL {
			t.Errorf("CentralURL mismatch: got %q, want %q", loaded.CentralURL, original.CentralURL)
		}
		if loaded.SyncInterval != original.SyncInterval {
			t.Errorf("SyncInterval mismatch: got %v, want %v", loaded.SyncInterval, original.SyncInterval)
		}
		if loaded.LogLevel != original.LogLevel {
			t.Errorf("LogLevel mismatch: got %q, want %q", loaded.LogLevel, original.LogLevel)
		}
	})

	t.Run("Redact_NeverReturnsKey", func(t *testing.T) {
		cfg := config.Config{
			DB:                 "/data/engram.db",
			EncryptedWriterKey: []byte{0x01, 0x02, 0x03, 0x04},
		}

		rc := cfg.Redact()

		if rc.WriterKey == "" {
			t.Error("Redact should include writer_key field when key is set (as '***REDACTED***')")
		}
		// The raw bytes must NEVER appear.
		if rc.WriterKey != "***REDACTED***" {
			t.Errorf("Redact: WriterKey = %q; want %q", rc.WriterKey, "***REDACTED***")
		}
	})

	t.Run("Redact_KeyAbsent_OmitsField", func(t *testing.T) {
		cfg := config.Config{DB: "/data/engram.db"}
		rc := cfg.Redact()
		if rc.WriterKey != "" {
			t.Errorf("Redact with no key: WriterKey = %q; want empty", rc.WriterKey)
		}
	})

	if runtime.GOOS == "windows" {
		t.Run("DPAPI_SealOpen_RoundTrip", func(t *testing.T) {
			box := config.NewSecretBox()
			plaintext := []byte("acceptance-writer-key-32bytesXXX")

			sealed, err := box.Seal(plaintext)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}

			opened, err := box.Open(sealed)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}

			if string(opened) != string(plaintext) {
				t.Errorf("Round-trip: got %q, want %q", opened, plaintext)
			}
		})

		t.Run("DPAPI_DecryptFailure_Degrades", func(t *testing.T) {
			box := config.NewSecretBox()

			// Corrupt blob simulates user/machine change.
			corrupt := []byte("not-a-valid-dpapi-blob")
			_, err := box.Open(corrupt)
			if err == nil {
				t.Fatal("Open of corrupt blob should error")
			}
			// Must not be ErrNoSecretStore (that means "no store", not "bad blob").
			if errors.Is(err, config.ErrNoSecretStore) {
				t.Error("Open of corrupt blob should not return ErrNoSecretStore")
			}
		})
	} else {
		t.Run("NonWindows_Seal_ReturnsErrNoSecretStore", func(t *testing.T) {
			box := config.NewSecretBox()
			_, err := box.Seal([]byte("key"))
			if !errors.Is(err, config.ErrNoSecretStore) {
				t.Errorf("Non-Windows Seal: want ErrNoSecretStore, got %v", err)
			}
		})
	}
}
