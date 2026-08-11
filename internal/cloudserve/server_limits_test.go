package cloudserve_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mariesqu/engram/internal/syncwire"
)

// TestHandlePull_LimitClamping asserts the server clamps the requested row limit:
// <=0 → default (100), >max (1000) → cap, in-range → as-is.
// (pullDefaultLimit=100 and pullMaxLimit=1000 are unexported constants in the package.)
//
// The assertion moved from "the limit handed to a single PullSince call" to "how
// many mutations come back", because handlePull now fetches in chunks of
// pullChunkSize: the clamped limit is a TOTAL spread over several queries, so no
// single call carries it. Asserting the total is the better test anyway — it pins
// the observable contract instead of an internal call parameter — and it still
// fails loudly if the clamp is removed, since central here always has more rows
// available (1200) than the largest clamp.
func TestHandlePull_LimitClamping(t *testing.T) {
	cases := []struct {
		name         string
		limit        int
		wantReturned int
	}{
		{"zero_uses_default", 0, 100},
		{"negative_uses_default", -7, 100},
		{"over_max_is_capped", 5000, 1000},
		{"exactly_max_passes_through", 1000, 1000},
		{"in_range_passes_through", 50, 50},
	}
	available := smallMutations(1200)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			central := &mockCentral{pullResult: available}
			ts := newTestServer(t, central)

			body, err := json.Marshal(syncwire.PullRequest{Project: "p", SinceSeq: 0, Limit: tc.limit})
			if err != nil {
				t.Fatalf("marshal pull request: %v", err)
			}
			resp, err := http.Post(ts.URL+"/v1/pull", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("POST /v1/pull: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var got syncwire.PullResponse
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if len(got.Mutations) != tc.wantReturned {
				t.Errorf("returned mutations = %d, want %d (requested limit %d)",
					len(got.Mutations), tc.wantReturned, tc.limit)
			}
			// No single chunk query may exceed pullChunkSize (100): that bound is what
			// keeps the server's working set small regardless of the clamped total.
			for i, c := range central.pullCalls {
				if c.limit > 100 {
					t.Errorf("pullCalls[%d].limit = %d; want <= pullChunkSize (100)", i, c.limit)
				}
			}
		})
	}
}

// TestTrailingSlashPath_NoRedirect proves a trailing-slash path (e.g. /v1/push/) is
// NOT 301-redirected by ServeMux: /v1/push is an EXACT pattern, so /v1/push/ does
// not match it and falls through to the "/" catch-all, which returns a JSON 404 —
// honoring the "all non-2xx responses are JSON" contract.
func TestTrailingSlashPath_NoRedirect(t *testing.T) {
	central := &mockCentral{}
	ts := newTestServer(t, central)

	// Observe the raw response without following redirects.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for _, path := range []string{"/v1/push/", "/v1/pull/"} {
		t.Run(path, func(t *testing.T) {
			resp, err := client.Post(ts.URL+path, "application/json", bytes.NewReader([]byte("{}")))
			if err != nil {
				t.Fatalf("POST %s: %v", path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404 (no 301 redirect)", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
		})
	}
}

// TestHandlePush_BodyTooLarge_Returns413 proves the request-body size cap
// (maxRequestBytes = 1 MiB): a valid-JSON body that exceeds it trips
// http.MaxBytesReader mid-decode, and the handler returns 413 (not a 400).
func TestHandlePush_BodyTooLarge_Returns413(t *testing.T) {
	central := &mockCentral{}
	ts := newTestServer(t, central)

	// Valid-JSON prefix with an oversized string value so MaxBytesReader trips while
	// the decoder is still reading (1 MiB + 64 KiB of 'a', plus the wrapper bytes).
	var buf bytes.Buffer
	buf.WriteString(`{"mutation":{"payload":"`)
	buf.Write(bytes.Repeat([]byte("a"), (1<<20)+(64<<10)))
	buf.WriteString(`"}}`)

	resp, err := http.Post(ts.URL+"/v1/push", "application/json", bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("POST /v1/push: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

// TestHandlePush_TrailingData_Returns400 proves the server enforces a single JSON
// document per request: a second JSON value, or trailing non-whitespace junk, after
// the first document is rejected with 400 (not silently ignored).
func TestHandlePush_TrailingData_Returns400(t *testing.T) {
	central := &mockCentral{}
	ts := newTestServer(t, central)

	valid, _ := validPushBody(t)
	cases := map[string][]byte{
		"second_json_object": append(append([]byte{}, valid...), []byte(`{"extra":"junk"}`)...),
		"trailing_garbage":   append(append([]byte{}, valid...), []byte("garbage")...),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp, err := http.Post(ts.URL+"/v1/push", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("POST /v1/push: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for trailing data", resp.StatusCode)
			}
		})
	}
}
