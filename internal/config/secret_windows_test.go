//go:build windows

package config

import (
	"errors"
	"testing"
)

// TestDPAPI_SealOpen_RoundTrip verifies that Seal followed by Open recovers
// the original plaintext on Windows. This test requires a Windows environment
// to run (guarded by //go:build windows).
func TestDPAPI_SealOpen_RoundTrip(t *testing.T) {
	box := NewSecretBox()

	plaintext := []byte("test-writer-key-value-32bytesXXX")

	sealed, err := box.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if len(sealed) == 0 {
		t.Fatal("Seal returned empty ciphertext")
	}
	// Ciphertext must not equal plaintext.
	if string(sealed) == string(plaintext) {
		t.Error("Seal returned plaintext unchanged — no encryption")
	}

	opened, err := box.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(opened) != string(plaintext) {
		t.Errorf("Open: got %q, want %q", opened, plaintext)
	}
}

// TestDPAPI_Open_InvalidBlob_ReturnsError verifies that Open returns a
// non-nil error (NOT ErrNoSecretStore) when given a corrupt ciphertext blob.
// This simulates the user/machine-change scenario: the daemon must not crash.
func TestDPAPI_Open_InvalidBlob_ReturnsError(t *testing.T) {
	box := NewSecretBox()

	corrupt := []byte("this is definitely not a valid dpapi blob 0xDEADBEEF")

	_, err := box.Open(corrupt)
	if err == nil {
		t.Fatal("Open of invalid blob should return an error")
	}
	// The error must NOT be ErrNoSecretStore (that means "no store available",
	// not "corrupt blob"). The caller needs to distinguish the two cases.
	if errors.Is(err, ErrNoSecretStore) {
		t.Errorf("Open of invalid blob returned ErrNoSecretStore; want a decryption error")
	}
}
