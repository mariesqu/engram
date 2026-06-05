package cloudserve_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/mariesqu/engram/internal/wireauth"
)

// TestAllowAllVerifier_PushNoHeaders proves AllowAllVerifier is a true bypass:
// a push with NO auth headers (no X-Writer-Id, no X-Signature) still returns 200
// when AllowAllVerifier is in use. This also verifies that the forgery check is
// skipped when authWriterID is "" (the value stashed by AllowAll's Verify).
func TestAllowAllVerifier_PushNoHeaders(t *testing.T) {
	central := &mockCentral{}
	ts := newTestServer(t, central)

	body, _ := validPushBody(t)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/push", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Deliberately omit X-Writer-Id and X-Signature.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/push: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("AllowAll: status = %d, want 200 (auth headers absent but AllowAll must bypass)", resp.StatusCode)
	}
}

// TestAllowAllVerifier_PullNoHeaders proves AllowAllVerifier passes header-less
// pull requests through without a 401.
func TestAllowAllVerifier_PullNoHeaders(t *testing.T) {
	central := &mockCentral{}
	ts := newTestServer(t, central)

	pullBody := []byte(`{"project":"p","since_seq":0}`)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/pull", bytes.NewReader(pullBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Deliberately omit X-Writer-Id and X-Signature.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/pull: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("AllowAll: status = %d, want 200 (auth headers absent but AllowAll must bypass)", resp.StatusCode)
	}
}

// TestAllowAllVerifier_IgnoresWriterIDHeader proves AllowAllVerifier fully bypasses
// auth even when an X-Writer-Id header is present and DIFFERS from the mutation's
// writer_id: the forgery check must be skipped (200, not 403), because AllowAll
// establishes no authenticated identity (its Verify returns ""). This is the
// regression guard for the bug where withAuth stashed the raw header value.
func TestAllowAllVerifier_IgnoresWriterIDHeader(t *testing.T) {
	central := &mockCentral{}
	ts := newTestServer(t, central)

	body, _ := validPushBody(t) // the mutation carries its own writer_id

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/push", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// A writer_id header that does NOT match the mutation's writer_id. Under the old
	// (buggy) behavior this drove a spurious 403; under AllowAll it must be ignored.
	req.Header.Set(wireauth.HeaderWriterID, "a-different-writer")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/push: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("AllowAll with mismatching X-Writer-Id: status = %d, want 200 (forgery check must be skipped)", resp.StatusCode)
	}
}
