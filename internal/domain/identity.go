package domain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// Identity holds the writer's stable identifier and provides HMAC-based signing.
// The mechanism is attribution-grade only — no expiry or RBAC in this slice.
type Identity struct {
	WriterID string
}

// Sign returns an HMAC-SHA256 hex signature of WriterID using secret.
func (id Identity) Sign(secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(id.WriterID))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether sig is the valid HMAC-SHA256 signature of WriterID
// using secret. Uses constant-time comparison to resist timing attacks.
func (id Identity) Verify(secret []byte, sig string) bool {
	expected := id.Sign(secret)
	expectedBytes, err := hex.DecodeString(expected)
	if err != nil {
		return false
	}
	sigBytes, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	return hmac.Equal(expectedBytes, sigBytes)
}
