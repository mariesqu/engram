//go:build !windows

package config

// EnvOnlySecretBox is the non-Windows implementation of SecretBox.
// It cannot persist secrets to disk — Seal and Open both return
// ErrNoSecretStore so callers know to use ENGRAM_WRITER_KEY instead.
//
// This is NOT a security weakness: on non-Windows platforms the writer key
// is expected to come from the environment (or a secrets manager), not from
// the config file. This stub exists solely to satisfy the SecretBox interface
// so the config package compiles and links without DPAPI on Linux/macOS.
type EnvOnlySecretBox struct{}

// NewSecretBox returns an EnvOnlySecretBox. The return type is SecretBox so
// the daemon can call this via the platform-selected constructor without a
// type assertion.
func NewSecretBox() SecretBox {
	return EnvOnlySecretBox{}
}

// Seal always returns ErrNoSecretStore. Non-Windows hosts cannot encrypt
// the writer key; use ENGRAM_WRITER_KEY env var instead.
func (EnvOnlySecretBox) Seal(_ []byte) ([]byte, error) {
	return nil, ErrNoSecretStore
}

// Open always returns ErrNoSecretStore. Non-Windows hosts cannot decrypt a
// writer key from the config file; use ENGRAM_WRITER_KEY env var instead.
func (EnvOnlySecretBox) Open(_ []byte) ([]byte, error) {
	return nil, ErrNoSecretStore
}
