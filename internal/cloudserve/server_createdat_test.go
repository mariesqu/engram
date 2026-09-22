package cloudserve_test

// Tests for Server.handleCreatedAt (POST /v1/created-at, FUP-005). Mirrors
// the capability-gating pattern in server_state_test.go/server_projects_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mariesqu/engram/internal/cloudserve"
	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/syncwire"
)

// createdAtCentral is a test Central that implements BOTH the core transport
// interface AND the optional createdAtLister capability (structurally — the
// interface itself is unexported in package cloudserve).
type createdAtCentral struct {
	*mockCentral

	// entriesByProject maps project → the full ordered entry set OriginalCreatedAt
	// paginates over (keyset by SyncID, exactly like the real implementation).
	entriesByProject map[string][]syncwire.CreatedAtEntry
	err              error

	gotProject string
	gotAfter   string
	gotLimit   int
}

func (c *createdAtCentral) Apply(ctx context.Context, m domain.Mutation) error {
	return c.mockCentral.Apply(ctx, m)
}

func (c *createdAtCentral) PullSince(ctx context.Context, project string, sinceSeq int64, limit int) ([]domain.Mutation, error) {
	return c.mockCentral.PullSince(ctx, project, sinceSeq, limit)
}

func (c *createdAtCentral) OriginalCreatedAt(_ context.Context, project, after string, limit int) ([]syncwire.CreatedAtEntry, error) {
	c.gotProject, c.gotAfter, c.gotLimit = project, after, limit
	if c.err != nil {
		return nil, c.err
	}
	all := c.entriesByProject[project]
	var page []syncwire.CreatedAtEntry
	for _, e := range all {
		if after != "" && e.SyncID <= after {
			continue
		}
		page = append(page, e)
		if limit > 0 && len(page) >= limit {
			break
		}
	}
	return page, nil
}

func newCreatedAtServer(t *testing.T, central *createdAtCentral) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(cloudserve.New(central, cloudserve.AllowAllVerifier()).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func postCreatedAt(t *testing.T, url string, req syncwire.CreatedAtRequest) (*http.Response, syncwire.CreatedAtResponse) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := http.Post(url+"/v1/created-at", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/created-at: %v", err)
	}
	defer resp.Body.Close()

	var out syncwire.CreatedAtResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return resp, out
}

// TestHandleCreatedAt_ReturnsPage covers the success path: project/after/limit
// are forwarded to OriginalCreatedAt verbatim, and the page comes back intact.
func TestHandleCreatedAt_ReturnsPage(t *testing.T) {
	central := &createdAtCentral{
		mockCentral: &mockCentral{},
		entriesByProject: map[string][]syncwire.CreatedAtEntry{
			"proj-a": {
				{SyncID: "sync-1", CreatedAt: "2020-01-01T00:00:00Z"},
				{SyncID: "sync-2", CreatedAt: "2021-06-15T00:00:00Z"},
			},
		},
	}
	ts := newCreatedAtServer(t, central)

	resp, err := http.Post(ts.URL+"/v1/created-at", "application/json",
		bytes.NewReader(mustMarshal(t, syncwire.CreatedAtRequest{Project: "proj-a", After: "sync-0", Limit: 50})))
	if err != nil {
		t.Fatalf("POST /v1/created-at: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got syncwire.CreatedAtResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Entries) != 2 || got.Entries[0].SyncID != "sync-1" || got.Entries[1].SyncID != "sync-2" {
		t.Fatalf("entries = %+v, want both fixture entries in order", got.Entries)
	}
	if central.gotProject != "proj-a" || central.gotAfter != "sync-0" || central.gotLimit != 50 {
		t.Errorf("forwarded (project=%q, after=%q, limit=%d), want (proj-a, sync-0, 50)",
			central.gotProject, central.gotAfter, central.gotLimit)
	}
}

// TestHandleCreatedAt_KeysetPagingReachesEmptyPage covers the "drained"
// contract the client's backfill loop relies on: paging with After set to the
// last-seen sync_id eventually returns an EMPTY (but 200 OK) page.
func TestHandleCreatedAt_KeysetPagingReachesEmptyPage(t *testing.T) {
	central := &createdAtCentral{
		mockCentral: &mockCentral{},
		entriesByProject: map[string][]syncwire.CreatedAtEntry{
			"proj-b": {
				{SyncID: "sync-1", CreatedAt: "2020-01-01T00:00:00Z"},
				{SyncID: "sync-2", CreatedAt: "2020-01-02T00:00:00Z"},
			},
		},
	}
	ts := newCreatedAtServer(t, central)

	resp1, page1 := postCreatedAt(t, ts.URL, syncwire.CreatedAtRequest{Project: "proj-b", Limit: 1})
	if resp1.StatusCode != http.StatusOK || len(page1.Entries) != 1 || page1.Entries[0].SyncID != "sync-1" {
		t.Fatalf("page 1 = status=%d entries=%+v, want [sync-1]", resp1.StatusCode, page1.Entries)
	}

	resp2, page2 := postCreatedAt(t, ts.URL, syncwire.CreatedAtRequest{Project: "proj-b", After: "sync-1", Limit: 1})
	if resp2.StatusCode != http.StatusOK || len(page2.Entries) != 1 || page2.Entries[0].SyncID != "sync-2" {
		t.Fatalf("page 2 = status=%d entries=%+v, want [sync-2]", resp2.StatusCode, page2.Entries)
	}

	resp3, page3 := postCreatedAt(t, ts.URL, syncwire.CreatedAtRequest{Project: "proj-b", After: "sync-2", Limit: 1})
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("page 3 status = %d, want 200 (an empty page is still success)", resp3.StatusCode)
	}
	if len(page3.Entries) != 0 {
		t.Errorf("page 3 entries = %+v, want empty (drained)", page3.Entries)
	}
}

// TestHandleCreatedAt_RequiresProject mirrors handleUnshare's validation.
func TestHandleCreatedAt_RequiresProject(t *testing.T) {
	central := &createdAtCentral{mockCentral: &mockCentral{}}
	ts := newCreatedAtServer(t, central)

	resp, _ := postCreatedAt(t, ts.URL, syncwire.CreatedAtRequest{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (project is required)", resp.StatusCode)
	}
}

// TestHandleCreatedAt_501_WhenCentralLacksCapability is the compatibility
// contract FUP-005 needs: a Central (e.g. in a functional test harness, or
// conceivably a future non-Postgres backend) that does not implement
// createdAtLister must answer 501, not crash or 500 — the SAME signal an OLD
// client sees from a genuinely older server's unknown-route 404, letting the
// backfill's "skip quietly and retry later" path treat both alike.
func TestHandleCreatedAt_501_WhenCentralLacksCapability(t *testing.T) {
	central := &mockCentral{} // does NOT implement OriginalCreatedAt
	ts := newTestServer(t, central)

	resp, _ := postCreatedAt(t, ts.URL, syncwire.CreatedAtRequest{Project: "proj-a"})
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 (Central lacks createdAtLister)", resp.StatusCode)
	}
}

// TestHandleCreatedAt_500_OnStoreError covers the plain failure path.
func TestHandleCreatedAt_500_OnStoreError(t *testing.T) {
	central := &createdAtCentral{mockCentral: &mockCentral{}, err: errors.New("db down")}
	ts := newCreatedAtServer(t, central)

	resp, _ := postCreatedAt(t, ts.URL, syncwire.CreatedAtRequest{Project: "proj-a"})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
