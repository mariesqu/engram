package remote_test

// Tests for Client.OriginalCreatedAt — mirrors the ListProjects/PullSince
// test patterns in client_listprojects_test.go/client_test.go.
// Route: POST /v1/created-at.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/remote"
	"github.com/mariesqu/engram/internal/syncwire"
)

// TestClient_OriginalCreatedAt_200_DecodesAndParsesEntries proves a 200
// response is decoded into []domain.CreatedAtEntry with CreatedAt correctly
// PARSED from the wire's RFC3339Nano string into a time.Time — the wire DTO
// never leaks past this client method (see domain.CreatedAtEntry's doc
// comment on why PullSince follows the identical pattern).
func TestClient_OriginalCreatedAt_200_DecodesAndParsesEntries(t *testing.T) {
	want := time.Date(2020, 3, 15, 9, 0, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/created-at" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		var req syncwire.CreatedAtRequest
		if err := json.Unmarshal(b, &req); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if req.Project != "proj-a" || req.After != "sync-9" || req.Limit != 250 {
			t.Errorf("request = %+v, want project=proj-a after=sync-9 limit=250", req)
		}

		writeJSON(w, http.StatusOK, syncwire.CreatedAtResponse{
			Entries: []syncwire.CreatedAtEntry{
				{SyncID: "sync-10", CreatedAt: want.Format(time.RFC3339Nano)},
			},
		})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil, "writer-test", testKey())
	got, err := c.OriginalCreatedAt(context.Background(), "proj-a", "sync-9", 250)
	if err != nil {
		t.Fatalf("OriginalCreatedAt: %v", err)
	}
	if len(got) != 1 || got[0].SyncID != "sync-10" {
		t.Fatalf("entries = %+v, want exactly [sync-10]", got)
	}
	if !got[0].CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", got[0].CreatedAt, want)
	}
}

// TestClient_OriginalCreatedAt_200_Empty proves an empty Entries slice — the
// backfill's "drained" signal — round-trips as a nil/empty slice, not an error.
func TestClient_OriginalCreatedAt_200_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, syncwire.CreatedAtResponse{})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil, "writer-test", testKey())
	got, err := c.OriginalCreatedAt(context.Background(), "proj-a", "", 0)
	if err != nil {
		t.Fatalf("OriginalCreatedAt: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("entries = %+v, want empty", got)
	}
}

// TestClient_OriginalCreatedAt_404_ReturnsStatusError proves an older
// central's 404 (unknown route) survives as a *StatusError with StatusCode()
// 404 — the signal syncer.isCreatedAtUnsupported keys on.
func TestClient_OriginalCreatedAt_404_ReturnsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found: /v1/created-at"})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil, "writer-test", testKey())
	_, err := c.OriginalCreatedAt(context.Background(), "proj-a", "", 0)
	if err == nil {
		t.Fatal("OriginalCreatedAt: want an error for a 404 response")
	}
	var se *remote.StatusError
	if !asStatusError(t, err, &se) {
		return
	}
	if se.Code != http.StatusNotFound {
		t.Errorf("StatusError.Code = %d, want 404", se.Code)
	}
}

// TestClient_OriginalCreatedAt_501_ReturnsStatusError proves a Central
// lacking the createdAtLister capability's 501 also survives intact.
func TestClient_OriginalCreatedAt_501_ReturnsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "created_at backfill not supported"})
	}))
	defer srv.Close()

	c := remote.New(srv.URL, nil, "writer-test", testKey())
	_, err := c.OriginalCreatedAt(context.Background(), "proj-a", "", 0)
	if err == nil {
		t.Fatal("OriginalCreatedAt: want an error for a 501 response")
	}
	var se *remote.StatusError
	if !asStatusError(t, err, &se) {
		return
	}
	if se.Code != http.StatusNotImplemented {
		t.Errorf("StatusError.Code = %d, want 501", se.Code)
	}
}

// asStatusError is a small errors.As wrapper that fails the test (rather than
// panicking) when err is not a *remote.StatusError.
func asStatusError(t *testing.T, err error, target **remote.StatusError) bool {
	t.Helper()
	se, ok := err.(*remote.StatusError)
	if !ok {
		t.Fatalf("err = %v (%T), want *remote.StatusError", err, err)
		return false
	}
	*target = se
	return true
}
