//go:build windows

package config

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WindowsSecretBox implements SecretBox using the Windows Data Protection API
// (DPAPI). Secrets are encrypted with CryptProtectData (user-scope) and
// decrypted with CryptUnprotectData. The binding is user-scoped: the sealed
// blob can only be opened by the same Windows user account that sealed it.
//
// Pure syscall — CGO_ENABLED=0 compatible. Uses golang.org/x/sys/windows
// which is already a direct dependency of this module (promoted in PR-①).
type WindowsSecretBox struct{}

// NewSecretBox returns a WindowsSecretBox. The return type is SecretBox so
// platform-agnostic code can call this via the platform-selected constructor
// without a type assertion.
func NewSecretBox() SecretBox {
	return WindowsSecretBox{}
}

// Seal encrypts plaintext using DPAPI CryptProtectData (current-user scope).
// The returned ciphertext can be stored in config.json (base64-encoded by
// the caller) and later decrypted by the same Windows user with Open.
func (WindowsSecretBox) Seal(plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, fmt.Errorf("dpapi Seal: empty plaintext")
	}

	input := windows.DataBlob{
		Size: uint32(len(plaintext)),
		Data: &plaintext[0],
	}

	var output windows.DataBlob

	// CryptProtectData: user-scope (no CRYPTPROTECT_LOCAL_MACHINE flag).
	// The description parameter (second arg) is optional; passing nil omits it.
	err := windows.CryptProtectData(&input, nil, nil, 0, nil, 0, &output)
	if err != nil {
		return nil, fmt.Errorf("dpapi Seal: CryptProtectData: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data))) //nolint:errcheck

	// Copy the DPAPI output from LocalAlloc'd memory into a Go-managed slice.
	result := make([]byte, output.Size)
	copy(result, unsafe.Slice(output.Data, output.Size))
	return result, nil
}

// Open decrypts a ciphertext blob produced by Seal using DPAPI
// CryptUnprotectData. Returns a non-nil error (not ErrNoSecretStore) when the
// blob is corrupt or was sealed by a different Windows user or machine.
// Callers must treat any error here as "key unavailable" and fall back to the
// env var without crashing.
func (WindowsSecretBox) Open(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 {
		return nil, fmt.Errorf("dpapi Open: empty ciphertext")
	}

	input := windows.DataBlob{
		Size: uint32(len(ciphertext)),
		Data: &ciphertext[0],
	}

	var output windows.DataBlob

	err := windows.CryptUnprotectData(&input, nil, nil, 0, nil, 0, &output)
	if err != nil {
		return nil, fmt.Errorf("dpapi Open: CryptUnprotectData: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data))) //nolint:errcheck

	result := make([]byte, output.Size)
	copy(result, unsafe.Slice(output.Data, output.Size))
	return result, nil
}
