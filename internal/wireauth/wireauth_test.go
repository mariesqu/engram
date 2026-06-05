package wireauth_test

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/mariesqu/engram/internal/wireauth"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// testKey returns a fixed 32-byte key for deterministic tests.
func testKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1) // 0x01 … 0x20 — arbitrary but nonzero
	}
	return key
}

// ── Sign / Verify round-trip ─────────────────────────────────────────────────

func TestSignVerify_RoundTrip(t *testing.T) {
	key := testKey()
	body := []byte(`{"project":"test"}`)

	sig := wireauth.Sign(key, "POST", "/push", body)
	if sig == "" {
		t.Fatal("Sign returned empty string")
	}
	if !wireauth.Verify(key, "POST", "/push", body, sig) {
		t.Error("Verify returned false for a just-signed request (expected true)")
	}
}

func TestSignVerify_EmptyBody(t *testing.T) {
	key := testKey()
	sig := wireauth.Sign(key, "GET", "/pull", nil)
	if !wireauth.Verify(key, "GET", "/pull", nil, sig) {
		t.Error("Verify returned false for empty body round-trip")
	}
}

// ── Wrong-key rejection ───────────────────────────────────────────────────────

func TestVerify_WrongKey_ReturnsFalse(t *testing.T) {
	rightKey := testKey()
	wrongKey := make([]byte, 32)
	for i := range wrongKey {
		wrongKey[i] = byte(255 - i) // different from testKey
	}
	body := []byte("some body")

	sig := wireauth.Sign(rightKey, "POST", "/push", body)
	if wireauth.Verify(wrongKey, "POST", "/push", body, sig) {
		t.Error("Verify returned true with a wrong key (expected false)")
	}
}

// ── Tampered-body rejection ───────────────────────────────────────────────────

func TestVerify_TamperedBody_ReturnsFalse(t *testing.T) {
	key := testKey()
	originalBody := []byte(`{"op":"upsert"}`)
	tamperedBody := []byte(`{"op":"delete"}`)

	sig := wireauth.Sign(key, "POST", "/push", originalBody)
	if wireauth.Verify(key, "POST", "/push", tamperedBody, sig) {
		t.Error("Verify returned true for a tampered body (expected false)")
	}
}

// ── Tampered-method rejection ─────────────────────────────────────────────────

func TestVerify_TamperedMethod_ReturnsFalse(t *testing.T) {
	key := testKey()
	body := []byte("body")

	sig := wireauth.Sign(key, "POST", "/push", body)
	if wireauth.Verify(key, "GET", "/push", body, sig) {
		t.Error("Verify returned true for a tampered method (expected false)")
	}
}

// ── Tampered-path rejection ───────────────────────────────────────────────────

func TestVerify_TamperedPath_ReturnsFalse(t *testing.T) {
	key := testKey()
	body := []byte("body")

	sig := wireauth.Sign(key, "POST", "/push", body)
	if wireauth.Verify(key, "POST", "/pull", body, sig) {
		t.Error("Verify returned true for a tampered path (expected false)")
	}
}

// ── Malformed (non-hex) signature rejection ───────────────────────────────────

func TestVerify_MalformedSig_ReturnsFalse(t *testing.T) {
	key := testKey()
	body := []byte("body")

	cases := []struct {
		name string
		sig  string
	}{
		{"empty", ""},
		{"non-hex chars", "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"},
		{"odd length", "abc"},
		{"spaces", "ab cd ef"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if wireauth.Verify(key, "POST", "/push", body, tc.sig) {
				t.Errorf("Verify returned true for malformed sig %q (expected false)", tc.sig)
			}
		})
	}
}

// ── Constant-time comparison: hmac.Equal is used, not == ─────────────────────

// TestVerify_UsesHmacEqual_ConstantTimeSemantics asserts the behavioral contract
// of Verify: a signature that differs in any byte (first or last) returns false,
// and the correct signature returns true.
//
// NOTE: this does NOT prove constant-time behavior — a naive string == on the hex
// would pass it identically. It documents intent and guards the behavioral
// contract; the actual constant-time guarantee comes from hmac.Equal in the
// stdlib (used by Verify) and is established by code review, not by this test.
//
// We cannot measure timing from a unit test, so we assert behavior only:
//   - sig with last byte flipped → false
//   - sig with first byte flipped → false
//   - correct sig → true
func TestVerify_UsesHmacEqual_ConstantTimeSemantics(t *testing.T) {
	key := testKey()
	body := []byte("timing test body")
	sig := wireauth.Sign(key, "POST", "/push", body)

	// Decode, flip last byte, re-encode.
	raw, err := hex.DecodeString(sig)
	if err != nil {
		t.Fatalf("hex.DecodeString(sig): %v", err)
	}

	flipLast := make([]byte, len(raw))
	copy(flipLast, raw)
	flipLast[len(flipLast)-1] ^= 0xFF
	sigLastFlipped := hex.EncodeToString(flipLast)

	flipFirst := make([]byte, len(raw))
	copy(flipFirst, raw)
	flipFirst[0] ^= 0xFF
	sigFirstFlipped := hex.EncodeToString(flipFirst)

	if wireauth.Verify(key, "POST", "/push", body, sigLastFlipped) {
		t.Error("Verify: last-byte-flipped sig returned true (expected false)")
	}
	if wireauth.Verify(key, "POST", "/push", body, sigFirstFlipped) {
		t.Error("Verify: first-byte-flipped sig returned true (expected false)")
	}
	// Correct sig must still pass.
	if !wireauth.Verify(key, "POST", "/push", body, sig) {
		t.Error("Verify: correct sig returned false after flip tests")
	}
}

// ── NewKey ────────────────────────────────────────────────────────────────────

func TestNewKey_Returns32Bytes(t *testing.T) {
	k, err := wireauth.NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	if len(k) != 32 {
		t.Errorf("NewKey: got %d bytes, want 32", len(k))
	}
}

func TestNewKey_DistinctAcrossCalls(t *testing.T) {
	const n = 10
	keys := make([][]byte, n)
	for i := range keys {
		k, err := wireauth.NewKey()
		if err != nil {
			t.Fatalf("NewKey[%d]: %v", i, err)
		}
		keys[i] = k
	}
	// All 10 keys must be distinct.
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if bytes.Equal(keys[i], keys[j]) {
				t.Errorf("NewKey: keys[%d] == keys[%d] (collision — expected distinct random values)", i, j)
			}
		}
	}
}

// ── Header name constants ─────────────────────────────────────────────────────

func TestHeaderConstants(t *testing.T) {
	if wireauth.HeaderWriterID == "" {
		t.Error("HeaderWriterID must not be empty")
	}
	if wireauth.HeaderSignature == "" {
		t.Error("HeaderSignature must not be empty")
	}
	// Canonical HTTP header name form: first letter of each word capitalized.
	if !strings.HasPrefix(wireauth.HeaderWriterID, "X-") {
		t.Errorf("HeaderWriterID = %q; expected X- prefix", wireauth.HeaderWriterID)
	}
	if !strings.HasPrefix(wireauth.HeaderSignature, "X-") {
		t.Errorf("HeaderSignature = %q; expected X- prefix", wireauth.HeaderSignature)
	}
}

// ── Canonical: separator robustness ──────────────────────────────────────────

// TestCanonical_DistinctInputsProduceDistinctStrings verifies that swapping
// method↔path, or changing body boundary, produces a different canonical string.
// This documents the "body-last, no trailing separator" design.
func TestCanonical_DistinctInputsProduceDistinctStrings(t *testing.T) {
	cases := []struct {
		name    string
		aMethod string
		aPath   string
		aBody   []byte
		bMethod string
		bPath   string
		bBody   []byte
	}{
		{
			name:    "method_path swapped",
			aMethod: "POST", aPath: "/push", aBody: []byte("x"),
			bMethod: "/push", bPath: "POST", bBody: []byte("x"),
		},
		{
			name:    "body boundary: empty vs newline",
			aMethod: "POST", aPath: "/push", aBody: nil,
			bMethod: "POST", bPath: "/push", bBody: []byte("\n"),
		},
		{
			name:    "body split across path",
			aMethod: "POST", aPath: "/push", aBody: []byte("abc"),
			bMethod: "POST", bPath: "/pushabc", bBody: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := wireauth.Canonical(tc.aMethod, tc.aPath, tc.aBody)
			b := wireauth.Canonical(tc.bMethod, tc.bPath, tc.bBody)
			if bytes.Equal(a, b) {
				t.Errorf("Canonical(%q,%q,%q) == Canonical(%q,%q,%q): expected distinct",
					tc.aMethod, tc.aPath, tc.aBody,
					tc.bMethod, tc.bPath, tc.bBody)
			}
		})
	}
}
