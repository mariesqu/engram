// Package wireauth provides HMAC-SHA256 request-signing primitives for the
// engram HTTP transport. It is intentionally dependency-free — no HTTP layer,
// no storage — so it can be imported by both the client and the server without
// pulling in transport or database packages.
//
// # Canonicalization
//
// Canonical assembles the signing string from the request method, URL path, and
// body:
//
//	method + "\n" + path + "\n" + body
//
// Design invariants that make this unambiguous:
//   - method (e.g. "POST") and path (e.g. "/push") are controlled by the caller
//     and must never contain a "\n" — the transport layer guarantees this.
//   - body is placed LAST, so no trailing delimiter is needed: any length of body
//     (including zero bytes) produces a distinct canonical string because the two
//     preceding "\n" separators fix the boundary between path and body.
//
// # Constant-time comparison
//
// Verify decodes the hex signature to raw bytes and compares the recomputed MAC
// to the decoded bytes using hmac.Equal, which runs in constant time regardless
// of byte values. String-level == comparisons are NOT used because they short-
// circuit on the first differing byte and leak timing information.
//
// # Key generation
//
// NewKey generates 32 cryptographically random bytes from crypto/rand. Keys must
// be provisioned out-of-band (e.g. stored in cloud_writer_keys) and shared
// securely between the server and each writer. The DB is the trust boundary;
// server-side key wrapping (envelope encryption) is a future enhancement.
//
// # Header names
//
//	X-Writer-Id  — carries the writer's identifier; tells the server which key to load
//	X-Signature  — carries the hex-encoded HMAC-SHA256 of the canonical request
package wireauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// Header name constants used by both the signing client and the verifying server.
const (
	// HeaderWriterID is the HTTP request header that identifies the writer.
	// The server uses this value to look up the per-writer HMAC key from
	// cloud_writer_keys before calling Verify.
	HeaderWriterID = "X-Writer-Id"

	// HeaderSignature is the HTTP request header that carries the hex-encoded
	// HMAC-SHA256 signature over the canonical request string.
	HeaderSignature = "X-Signature"
)

// Canonical assembles the byte slice that Sign and Verify hash. It concatenates:
//
//	method + "\n" + path + "\n" + body
//
// method and path must not contain "\n" (the HTTP transport guarantees this).
// body is appended last without a trailing separator; any body length (including
// zero) produces a distinct canonical string because the preceding separators
// fix the method and path boundaries.
func Canonical(method, path string, body []byte) []byte {
	// Pre-size: len(method) + 1 + len(path) + 1 + len(body).
	size := len(method) + 1 + len(path) + 1 + len(body)
	b := make([]byte, 0, size)
	b = append(b, method...)
	b = append(b, '\n')
	b = append(b, path...)
	b = append(b, '\n')
	b = append(b, body...)
	return b
}

// Sign returns the hex-encoded HMAC-SHA256 of Canonical(method, path, body)
// under key. The returned string is lowercase hex (64 characters for SHA-256).
func Sign(key []byte, method, path string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(Canonical(method, path, body)) //nolint:errcheck // hash.Write never errors
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify recomputes the HMAC-SHA256 of Canonical(method, path, body) under key
// and compares it to sig in constant time.
//
// sig must be a lowercase-hex string. Verify returns false on any hex-decode
// error (e.g. non-hex characters, odd length) and on any MAC mismatch.
//
// The comparison uses hmac.Equal (constant time over raw bytes) — NOT == on the
// hex strings, which would short-circuit and leak timing information.
func Verify(key []byte, method, path string, body []byte, sig string) bool {
	sigBytes, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, key)
	mac.Write(Canonical(method, path, body)) //nolint:errcheck // hash.Write never errors
	expected := mac.Sum(nil)

	// hmac.Equal compares two MACs in constant time, preventing timing side-channels.
	return hmac.Equal(expected, sigBytes)
}

// NewKey generates a fresh 32-byte HMAC key from crypto/rand. 32 bytes provides
// 256 bits of entropy — appropriate for HMAC-SHA256. Returns an error only if
// crypto/rand fails, which should not happen in practice on any supported OS.
func NewKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}
