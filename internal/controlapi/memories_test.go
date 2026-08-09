package controlapi_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
)

// memoriesStore extends mockStore with ListMemoriesFiltered for the memories
// handler tests.
type memoriesStore struct {
	mockStore
	memories []controlapi.MemorySummary
	memErr   error
}

func (m *memoriesStore) ListMemoriesFiltered(opts controlapi.MemoryListOptions) ([]controlapi.MemorySummary, error) {
	return m.memories, m.memErr
}
func (m *memoriesStore) UpdateMemory(id int64, title, content, typ string) (controlapi.MemorySummary, error) {
	return controlapi.MemorySummary{}, m.memErr
}
func (m *memoriesStore) DeleteMemory(id int64) error {
	return m.memErr
}

// captureMemoriesStore is a mock that lets tests capture calls to
// ListMemoriesFiltered.
type captureMemoriesStore struct {
	mockStore
	onListMemories func(opts controlapi.MemoryListOptions) ([]controlapi.MemorySummary, error)
}

func (m *captureMemoriesStore) ListMemoriesFiltered(opts controlapi.MemoryListOptions) ([]controlapi.MemorySummary, error) {
	if m.onListMemories != nil {
		return m.onListMemories(opts)
	}
	return nil, nil
}
func (m *captureMemoriesStore) UpdateMemory(id int64, title, content, typ string) (controlapi.MemorySummary, error) {
	return controlapi.MemorySummary{}, nil
}
func (m *captureMemoriesStore) DeleteMemory(id int64) error {
	return nil
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
	var captured controlapi.MemoryListOptions
	store := &captureMemoriesStore{
		onListMemories: func(opts controlapi.MemoryListOptions) ([]controlapi.MemorySummary, error) {
			captured = opts
			return nil, nil
		},
	}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	resp := get(t, ts, "/api/v1/memories?q=auth+bug", authHeader("tok"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if captured.Query != "auth bug" {
		t.Errorf("query: got %q, want %q", captured.Query, "auth bug")
	}
}

// TestHandleMemories_ExtendedFilterParams verifies that type, scope, from,
// to, and offset query params are all forwarded onto MemoryListOptions.
func TestHandleMemories_ExtendedFilterParams(t *testing.T) {
	var captured controlapi.MemoryListOptions
	store := &captureMemoriesStore{
		onListMemories: func(opts controlapi.MemoryListOptions) ([]controlapi.MemorySummary, error) {
			captured = opts
			return nil, nil
		},
	}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	resp := get(t, ts, "/api/v1/memories?project=proj-a&type=decision&scope=project&from=2024-01-01&to=2024-01-31&offset=10", authHeader("tok"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if captured.Project != "proj-a" {
		t.Errorf("Project = %q, want proj-a", captured.Project)
	}
	if captured.Type != "decision" {
		t.Errorf("Type = %q, want decision", captured.Type)
	}
	if captured.Scope != "project" {
		t.Errorf("Scope = %q, want project", captured.Scope)
	}
	if captured.Offset != 10 {
		t.Errorf("Offset = %d, want 10", captured.Offset)
	}
	if captured.CreatedFrom.IsZero() {
		t.Error("CreatedFrom should be set from ?from=2024-01-01")
	}
	if captured.CreatedTo.IsZero() {
		t.Error("CreatedTo should be set from ?to=2024-01-31")
	}
	if !captured.CreatedTo.After(captured.CreatedFrom) {
		t.Error("CreatedTo should be after CreatedFrom")
	}
}

// TestHandleMemories_BackwardCompatible verifies the original q/project/limit
// contract still works unchanged when no new params are supplied.
func TestHandleMemories_BackwardCompatible(t *testing.T) {
	var captured controlapi.MemoryListOptions
	store := &captureMemoriesStore{
		onListMemories: func(opts controlapi.MemoryListOptions) ([]controlapi.MemorySummary, error) {
			captured = opts
			return nil, nil
		},
	}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	resp := get(t, ts, "/api/v1/memories?q=hello&project=proj-x&limit=25", authHeader("tok"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if captured.Query != "hello" || captured.Project != "proj-x" || captured.Limit != 25 {
		t.Errorf("got %+v, want Query=hello Project=proj-x Limit=25", captured)
	}
	if captured.Type != "" || captured.Scope != "" || captured.Offset != 0 {
		t.Errorf("unset new fields should default to zero values, got %+v", captured)
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
