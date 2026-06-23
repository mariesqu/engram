package controlapi_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
)

// memoriesStore extends mockStore with ListMemories for the memories handler tests.
type memoriesStore struct {
	mockStore
	memories []controlapi.MemorySummary
	memErr   error
}

func (m *memoriesStore) ListMemories(query, project string, limit int) ([]controlapi.MemorySummary, error) {
	return m.memories, m.memErr
}

// captureMemoriesStore is a mock that lets tests capture calls to ListMemories.
type captureMemoriesStore struct {
	mockStore
	onListMemories func(query, project string, limit int) ([]controlapi.MemorySummary, error)
}

func (m *captureMemoriesStore) ListMemories(query, project string, limit int) ([]controlapi.MemorySummary, error) {
	if m.onListMemories != nil {
		return m.onListMemories(query, project, limit)
	}
	return nil, nil
}

func TestHandleMemories_List(t *testing.T) {
	store := &memoriesStore{
		memories: []controlapi.MemorySummary{
			{ID: 1, SyncID: "sync-1", Project: "proj-a", Type: "bugfix", Title: "Fixed auth bug", Content: "root cause was X", Scope: "project", CreatedAt: "2024-01-01T00:00:00Z", UpdatedAt: "2024-01-01T00:00:00Z"},
			{ID: 2, SyncID: "sync-2", Project: "proj-b", Type: "decision", Title: "Use PostgreSQL", Content: "chosen for reliability", Scope: "project", CreatedAt: "2024-01-02T00:00:00Z", UpdatedAt: "2024-01-02T00:00:00Z"},
		},
	}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	resp := get(t, ts, "/api/v1/memories", authHeader("tok"))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	assertJSONContentType(t, resp)

	var got []controlapi.MemorySummary
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 memories, got %d", len(got))
	}
	if got[0].Title != "Fixed auth bug" {
		t.Errorf("got[0].Title = %q, want %q", got[0].Title, "Fixed auth bug")
	}
}

func TestHandleMemories_Empty(t *testing.T) {
	store := &memoriesStore{memories: nil}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	resp := get(t, ts, "/api/v1/memories", authHeader("tok"))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	// Must return an empty JSON array, never null.
	var got []controlapi.MemorySummary
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty array, got %d elements", len(got))
	}
}

func TestHandleMemories_LimitClamped(t *testing.T) {
	store := &memoriesStore{}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	// Oversized limit must not cause an error response.
	resp := get(t, ts, "/api/v1/memories?limit=9999", authHeader("tok"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200 for oversized limit, got %d", resp.StatusCode)
	}
}

func TestHandleMemories_MethodNotAllowed(t *testing.T) {
	store := &memoriesStore{}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/memories", nil)
	req.Header.Set("Authorization", authHeader("tok"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("want 405, got %d", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != "GET" {
		t.Errorf("Allow header: got %q, want %q", allow, "GET")
	}
}

func TestHandleMemories_SearchParam(t *testing.T) {
	var capturedQuery string
	store := &captureMemoriesStore{
		onListMemories: func(query, project string, limit int) ([]controlapi.MemorySummary, error) {
			capturedQuery = query
			return nil, nil
		},
	}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	resp := get(t, ts, "/api/v1/memories?q=auth+bug", authHeader("tok"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if capturedQuery != "auth bug" {
		t.Errorf("query: got %q, want %q", capturedQuery, "auth bug")
	}
}

func TestHandleMemories_Unauthorized(t *testing.T) {
	store := &memoriesStore{}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	resp := get(t, ts, "/api/v1/memories", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401 without token, got %d", resp.StatusCode)
	}
}
