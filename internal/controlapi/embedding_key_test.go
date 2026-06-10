package controlapi_test

// embedding_key_test.go — task 2.6 key-route unit tests
//
// POST /api/v1/embedding/key  — stores sealed key, never echoes it.
// DELETE /api/v1/embedding/key — clears stored key.
// Security proofs: missing auth → 401, wrong origin → 403.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
)

// ── mock EmbeddingKeyStore ─────────────────────────────────────────────────────

// mockKeyStore records seal/clear calls for inspection.
type mockKeyStore struct {
	sealed  []byte // last sealed plaintext (for testing only — real impl NEVER stores plaintext)
	cleared bool
	err     error // non-nil → returned from all operations
}

func (m *mockKeyStore) SealEmbeddingKey(plaintext []byte) error {
	if m.err != nil {
		return m.err
	}
	m.sealed = append([]byte(nil), plaintext...)
	return nil
}

func (m *mockKeyStore) ClearEmbeddingKey() error {
	if m.err != nil {
		return m.err
	}
	m.cleared = true
	m.sealed = nil
	return nil
}

// errNoSecretStoreTest is controlapi.ErrNoSecretStore — the handler checks with
// errors.Is against the exported sentinel.
var errNoSecretStoreTest = controlapi.ErrNoSecretStore

// ── helpers ────────────────────────────────────────────────────────────────────

// newKeyTestServer creates a Server+httptest.Server wired with the given keyStore.
func newKeyTestServer(t *testing.T, ks controlapi.EmbeddingKeyStore) *httptest.Server {
	t.Helper()
	srv := controlapi.New("test-token", 7700,
		&mockStore{}, &mockSyncCtrl{}, &mockCfgStore{}, "v1", ks,
	)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// authPost sends an authenticated POST with the correct loopback origin.
func authPost(t *testing.T, ts *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(b)
	} else {
		r = strings.NewReader("{}")
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, r)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Origin", "http://127.0.0.1:7700")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// authDelete sends an authenticated DELETE with the correct loopback origin.
func authDelete(t *testing.T, ts *httptest.Server, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Origin", "http://127.0.0.1:7700")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// ── tests ──────────────────────────────────────────────────────────────────────

// TestKeyRoute_Post_StubSecretBox_StoresSealed verifies that POST stores the key
// via SealEmbeddingKey and returns 200. The response body must NEVER contain
// the plaintext key.
func TestKeyRoute_Post_StubSecretBox_StoresSealed(t *testing.T) {
	ks := &mockKeyStore{}
	ts := newKeyTestServer(t, ks)

	const fakeKey = "sk-test-super-secret"
	resp := authPost(t, ts, "/api/v1/embedding/key", map[string]string{"key": fakeKey})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /api/v1/embedding/key: got %d, want 200; body: %s", resp.StatusCode, body)
	}

	// The response body must not contain the plaintext key.
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), fakeKey) {
		t.Errorf("response body leaks the plaintext key: %s", body)
	}

	// The key store must have received the key.
	if string(ks.sealed) != fakeKey {
		t.Errorf("keyStore.SealEmbeddingKey not called with expected plaintext; got: %q", ks.sealed)
	}
}

// TestKeyRoute_Post_NoSecretStore_422 verifies that when SealEmbeddingKey
// returns errNoSecretStore, the handler returns 422 Unprocessable Entity.
func TestKeyRoute_Post_NoSecretStore_422(t *testing.T) {
	ks := &mockKeyStore{err: errNoSecretStoreTest}
	ts := newKeyTestServer(t, ks)

	resp := authPost(t, ts, "/api/v1/embedding/key", map[string]string{"key": "sk-any"})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST with no-secret-store: got %d, want 422; body: %s", resp.StatusCode, body)
	}
}

// TestKeyRoute_Delete_ClearsKey verifies that DELETE calls ClearEmbeddingKey
// and returns 200.
func TestKeyRoute_Delete_ClearsKey(t *testing.T) {
	ks := &mockKeyStore{}
	ts := newKeyTestServer(t, ks)

	resp := authDelete(t, ts, "/api/v1/embedding/key")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("DELETE /api/v1/embedding/key: got %d, want 200; body: %s", resp.StatusCode, body)
	}

	if !ks.cleared {
		t.Error("keyStore.ClearEmbeddingKey was not called")
	}
}

// TestKeyRoute_Post_MissingAuth_401 verifies that a POST without an Authorization
// header is rejected with 401 (WithAuthAndOrigin enforces token auth).
func TestKeyRoute_Post_MissingAuth_401(t *testing.T) {
	ks := &mockKeyStore{}
	ts := newKeyTestServer(t, ks)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/embedding/key",
		strings.NewReader(`{"key":"sk-test"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Deliberately no Authorization header.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no auth POST: got %d, want 401", resp.StatusCode)
	}

	// Key store must not have been called.
	if ks.sealed != nil {
		t.Error("keyStore.SealEmbeddingKey must NOT be called on unauthenticated request")
	}
}

// TestKeyRoute_Post_WrongOrigin_403 verifies that a POST with a non-loopback
// Origin is rejected with 403 (CSRF / origin check).
func TestKeyRoute_Post_WrongOrigin_403(t *testing.T) {
	ks := &mockKeyStore{}
	ts := newKeyTestServer(t, ks)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/embedding/key",
		strings.NewReader(`{"key":"sk-test"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Origin", "http://evil.example.com") // wrong origin
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("wrong origin POST: got %d, want 403", resp.StatusCode)
	}

	// Key store must not have been called.
	if ks.sealed != nil {
		t.Error("keyStore.SealEmbeddingKey must NOT be called on wrong-origin request")
	}
}

// TestKeyRoute_Post_EmptyKey_400 verifies that an empty key field returns 400.
func TestKeyRoute_Post_EmptyKey_400(t *testing.T) {
	ks := &mockKeyStore{}
	ts := newKeyTestServer(t, ks)

	resp := authPost(t, ts, "/api/v1/embedding/key", map[string]string{"key": ""})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("empty key POST: got %d, want 400; body: %s", resp.StatusCode, body)
	}

	// Key store must not have been called.
	if ks.sealed != nil {
		t.Error("keyStore.SealEmbeddingKey must NOT be called for empty key")
	}
}
