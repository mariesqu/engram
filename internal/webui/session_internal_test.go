package webui

import (
	"testing"
	"time"
)

// TestSessionStore_ServerSideExpiry: the cookie MaxAge is client-side advice
// only — the SERVER must reject a session value older than the TTL (a stolen
// raw cookie value must not stay valid for the daemon's entire lifetime).
func TestSessionStore_ServerSideExpiry(t *testing.T) {
	s := &sessionStore{}
	s.set("sess-abc", "csrf-xyz")

	if !s.valid("sess-abc") {
		t.Fatal("fresh session should be valid")
	}

	// Backdate past the TTL — the same value must now be rejected.
	s.mu.Lock()
	s.issuedAt = time.Now().Add(-sessionCookieTTL - time.Minute)
	s.mu.Unlock()

	if s.valid("sess-abc") {
		t.Error("expired session accepted — server-side expiry not enforced")
	}
}

// TestSessionStore_EmptyValueNeverValid: an empty stored session ("" — no
// exchange yet) must not validate an empty cookie value.
func TestSessionStore_EmptyValueNeverValid(t *testing.T) {
	s := &sessionStore{}
	if s.valid("") {
		t.Error("empty value validated against empty store")
	}
	s.set("real", "csrf-real")
	if s.valid("") {
		t.Error("empty cookie value validated")
	}
}
