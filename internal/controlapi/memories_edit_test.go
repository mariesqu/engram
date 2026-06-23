package controlapi_test

// Tests for PUT /api/v1/memories/{id} and DELETE /api/v1/memories/{id}.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
)

// editDeleteStore extends memoriesStore to support UpdateMemory and DeleteMemory
// with configurable behavior for testing.
type editDeleteStore struct {
	memoriesStore
	updateResult controlapi.MemorySummary
	updateErr    error
	deleteErr    error
}

func (m *editDeleteStore) UpdateMemory(id int64, title, content, typ string) (controlapi.MemorySummary, error) {
	return m.updateResult, m.updateErr
}
func (m *editDeleteStore) DeleteMemory(id int64) error {
	return m.deleteErr
}

// putMemory issues a PUT /api/v1/memories/{id} request.
func putMemory(t *testing.T, ts *httptest.Server, id int64, body any, auth, origin string) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/v1/memories/%d", ts.URL, id), bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// deleteMemory issues a DELETE /api/v1/memories/{id} request.
func deleteMemory(t *testing.T, ts *httptest.Server, id int64, auth, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/v1/memories/%d", ts.URL, id), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// TestHandleMemoryPut_Success verifies that PUT /api/v1/memories/{id} returns
// 200 with the updated MemorySummary on success.
func TestHandleMemoryPut_Success(t *testing.T) {
	expected := controlapi.MemorySummary{
		ID:    42,
		Title: "new title",
		Type:  "decision",
	}
	store := &editDeleteStore{updateResult: expected}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	body := map[string]string{"title": "new title", "content": "new content"}
	resp := putMemory(t, ts, 42, body, authHeader("tok"), "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d (body: %s)", resp.StatusCode, readBody(t, resp))
	}
	assertJSONContentType(t, resp)

	var got controlapi.MemorySummary
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != expected.ID {
		t.Errorf("ID = %d, want %d", got.ID, expected.ID)
	}
	if got.Title != expected.Title {
		t.Errorf("Title = %q, want %q", got.Title, expected.Title)
	}
}

// TestHandleMemoryPut_NotFound verifies that PUT returns 404 when the store
// reports a not-found error.
func TestHandleMemoryPut_NotFound(t *testing.T) {
	store := &editDeleteStore{updateErr: errors.New("observation not found")}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	body := map[string]string{"title": "t", "content": "c"}
	resp := putMemory(t, ts, 99, body, authHeader("tok"), "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404 for not-found, got %d", resp.StatusCode)
	}
}

// TestHandleMemoryPut_MissingTitle verifies that PUT returns 400 when title is empty.
func TestHandleMemoryPut_MissingTitle(t *testing.T) {
	store := &editDeleteStore{}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	body := map[string]string{"title": "", "content": "some content"}
	resp := putMemory(t, ts, 1, body, authHeader("tok"), "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400 for missing title, got %d", resp.StatusCode)
	}
}

// TestHandleMemoryPut_MissingContent verifies that PUT returns 400 when content is empty.
func TestHandleMemoryPut_MissingContent(t *testing.T) {
	store := &editDeleteStore{}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	body := map[string]string{"title": "some title", "content": ""}
	resp := putMemory(t, ts, 1, body, authHeader("tok"), "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400 for missing content, got %d", resp.StatusCode)
	}
}

// TestHandleMemoryPut_Unauthorized verifies that PUT returns 401 without a token.
func TestHandleMemoryPut_Unauthorized(t *testing.T) {
	store := &editDeleteStore{}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	body := map[string]string{"title": "t", "content": "c"}
	resp := putMemory(t, ts, 1, body, "", "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401 without token, got %d", resp.StatusCode)
	}
}

// TestHandleMemoryPut_WrongOrigin verifies that PUT returns 403 on a mismatched
// Origin header (CSRF origin guard).
func TestHandleMemoryPut_WrongOrigin(t *testing.T) {
	store := &editDeleteStore{}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	body := map[string]string{"title": "t", "content": "c"}
	resp := putMemory(t, ts, 1, body, authHeader("tok"), "http://evil.example.com")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("want 403 on wrong Origin, got %d", resp.StatusCode)
	}
}

// TestHandleMemoryPut_MethodNotAllowed verifies that GET on the {id} route returns 405.
func TestHandleMemoryPut_MethodNotAllowed(t *testing.T) {
	store := &editDeleteStore{}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/memories/1", ts.URL), nil)
	req.Header.Set("Authorization", authHeader("tok"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("want 405 for GET on memory id route, got %d", resp.StatusCode)
	}
}

// TestHandleMemoryDelete_Success verifies that DELETE /api/v1/memories/{id}
// returns 200 {"status":"deleted"} on success.
func TestHandleMemoryDelete_Success(t *testing.T) {
	store := &editDeleteStore{}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	resp := deleteMemory(t, ts, 42, authHeader("tok"), "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d (body: %s)", resp.StatusCode, readBody(t, resp))
	}
	assertJSONContentType(t, resp)

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "deleted" {
		t.Errorf("status = %q, want %q", body["status"], "deleted")
	}
}

// TestHandleMemoryDelete_NotFound verifies that DELETE returns 404 when the
// store reports not found.
func TestHandleMemoryDelete_NotFound(t *testing.T) {
	store := &editDeleteStore{deleteErr: errors.New("observation not found")}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	resp := deleteMemory(t, ts, 99, authHeader("tok"), "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404 for not-found, got %d", resp.StatusCode)
	}
}

// TestHandleMemoryDelete_Unauthorized verifies that DELETE returns 401 without a token.
func TestHandleMemoryDelete_Unauthorized(t *testing.T) {
	store := &editDeleteStore{}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	resp := deleteMemory(t, ts, 1, "", "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401 without token, got %d", resp.StatusCode)
	}
}

// TestHandleMemoryDelete_WrongOrigin verifies that DELETE returns 403 on a
// mismatched Origin header.
func TestHandleMemoryDelete_WrongOrigin(t *testing.T) {
	store := &editDeleteStore{}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	resp := deleteMemory(t, ts, 1, authHeader("tok"), "http://evil.example.com")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("want 403 on wrong Origin, got %d", resp.StatusCode)
	}
}

// TestHandleMemoryDelete_InvalidID verifies that DELETE returns 400 for non-numeric IDs.
func TestHandleMemoryDelete_InvalidID(t *testing.T) {
	store := &editDeleteStore{}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/memories/notanumber", nil)
	req.Header.Set("Authorization", authHeader("tok"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400 for non-numeric id, got %d", resp.StatusCode)
	}
}

// TestHandleMemoryMutate_PostMethodNotAllowed verifies that POST on the {id}
// route returns 405.
func TestHandleMemoryMutate_PostMethodNotAllowed(t *testing.T) {
	store := &editDeleteStore{}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/memories/1", ts.URL), nil)
	req.Header.Set("Authorization", authHeader("tok"))
	req.Header.Set("Origin", "http://127.0.0.1:7700")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("want 405 for POST on memory id route, got %d", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != "PUT, DELETE" {
		t.Errorf("Allow header = %q, want %q", allow, "PUT, DELETE")
	}
}
