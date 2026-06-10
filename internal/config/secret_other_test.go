//go:build !windows

package config

import (
	"errors"
	"testing"
)

// TestEnvOnly_Seal_ReturnsError verifies that the non-Windows SecretBox
// implementation returns ErrNoSecretStore from Seal — signalling that the
// platform cannot store secrets in the config file.
func TestEnvOnly_Seal_ReturnsError(t *testing.T) {
	box := NewSecretBox()

	_, err := box.Seal([]byte("some-plaintext-key"))
	if err == nil {
		t.Fatal("non-Windows Seal should return ErrNoSecretStore")
	}
	if !errors.Is(err, ErrNoSecretStore) {
		t.Errorf("non-Windows Seal: got %v, want ErrNoSecretStore", err)
	}
}

// TestEnvOnly_Open_ReturnsError verifies that the non-Windows SecretBox
// implementation returns ErrNoSecretStore from Open.
func TestEnvOnly_Open_ReturnsError(t *testing.T) {
	box := NewSecretBox()

	_, err := box.Open([]byte{0x01, 0x02, 0x03})
	if err == nil {
		t.Fatal("non-Windows Open should return ErrNoSecretStore")
	}
	if !errors.Is(err, ErrNoSecretStore) {
		t.Errorf("non-Windows Open: got %v, want ErrNoSecretStore", err)
	}
}
