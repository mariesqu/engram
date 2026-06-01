package domain

import "testing"

func TestIdentity_HMACRoundtrip(t *testing.T) {
	id := Identity{WriterID: "writer-A"}
	secret := []byte("super-secret")

	sig := id.Sign(secret)
	if sig == "" {
		t.Fatal("Sign returned empty string")
	}

	if !id.Verify(secret, sig) {
		t.Error("Verify should return true for correct sig")
	}

	// Tampered sig must fail.
	tampered := sig[:len(sig)-2] + "ff"
	if id.Verify(secret, tampered) {
		t.Error("Verify should return false for tampered sig")
	}

	// Different secret must fail.
	if id.Verify([]byte("other-secret"), sig) {
		t.Error("Verify should return false for wrong secret")
	}
}

func TestIdentity_DifferentWritersDifferentSigs(t *testing.T) {
	a := Identity{WriterID: "writer-A"}
	b := Identity{WriterID: "writer-B"}
	secret := []byte("shared-secret")

	if a.Sign(secret) == b.Sign(secret) {
		t.Error("distinct WriterIDs must produce distinct signatures")
	}
}
