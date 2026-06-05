package cloudserve_test

import (
	"bytes"
	"net/http"
	"testing"
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
