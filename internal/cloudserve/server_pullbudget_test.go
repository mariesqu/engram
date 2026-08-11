package cloudserve_test

// Tests for handlePull's response byte budget — the server half of the v1.5.2
// pull-wedge fix.
//
// Client-side halving (syncer.Pull + transport.ErrResponseTooLarge) works around an
// oversized response; this budget prevents one. The difference matters for the
// failure the client cannot see: a response the server never finishes building
// (OOM, write timeout) reaches the client as a NETWORK error, not as the overflow
// sentinel, so no amount of client-side shrinking is ever triggered. Bounding the
// response at the source is the durable fix.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/syncwire"
)

// clientResponseCap mirrors remote.maxResponseBytes (4 MiB) — the cap the
// reference client applies to a pull response. The server's budget must keep every
// response it builds under this number; that is the invariant these tests pin.
const clientResponseCap = 4 << 20

// smallMutations builds n mutations with ascending seq (1..n) and tiny payloads,
// the ordinary case where the row limit — never the byte budget — is what bounds a
// response.
func smallMutations(n int) []domain.Mutation {
	out := make([]domain.Mutation, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, domain.Mutation{
			MutationID: "mut-small-" + strconv.Itoa(i),
			Seq:        int64(i),
			OccurredAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			Payload:    []byte(`{"i":` + strconv.Itoa(i) + `}`),
		})
	}
	return out
}

// fatMutations builds n mutations with ascending seq (1..n) whose payloads are
// payloadBytes of valid JSON each, so a handful of them blows past the response
// budget. This models the real shape of the bug: central accepts mutations up to
// the 1 MiB ingest cap, so a batch of them dwarfs any sane response cap.
func fatMutations(n, payloadBytes int) []domain.Mutation {
	out := make([]domain.Mutation, 0, n)
	for i := 1; i <= n; i++ {
		blob := bytes.Repeat([]byte("a"), payloadBytes)
		payload := fmt.Appendf(nil, `{"blob":%q}`, blob)
		out = append(out, domain.Mutation{
			MutationID: "mut-fat-" + strconv.Itoa(i),
			Seq:        int64(i),
			OccurredAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			Payload:    payload,
		})
	}
	return out
}

// postPull issues one /v1/pull and returns the decoded response plus the raw body
// size — the size is the point of these tests, so it is measured, not assumed.
func postPull(t *testing.T, url, project string, sinceSeq int64, limit int) (syncwire.PullResponse, int) {
	t.Helper()
	body, err := json.Marshal(syncwire.PullRequest{Project: project, SinceSeq: sinceSeq, Limit: limit})
	if err != nil {
		t.Fatalf("marshal pull request: %v", err)
	}
	resp, err := http.Post(url+"/v1/pull", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/pull: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %.200s)", resp.StatusCode, raw)
	}
	var pr syncwire.PullResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatalf("decode PullResponse: %v", err)
	}
	return pr, len(raw)
}

// TestHandlePull_ByteBudget_TruncatesAndResumes is the core proof: a backlog of fat
// mutations comes back as a bounded PREFIX, and the client drains the rest by
// re-asking from the last seq it saw. Every response must fit under the client's
// 4 MiB cap — a response above it is exactly the wedge, since the client discards
// the body and its cursor never advances.
//
// The test drains the whole backlog rather than pinning "the first response holds
// exactly N rows": the row count depends on JSON escaping and is not the contract.
// The contract is prefix + bounded + resumable + no gaps or duplicates.
func TestHandlePull_ByteBudget_TruncatesAndResumes(t *testing.T) {
	const (
		total       = 10
		payloadSize = 512 << 10 // 512 KiB each → ~5 MiB total, well over the 3 MiB budget
	)
	central := &mockCentral{pullResult: fatMutations(total, payloadSize)}
	ts := newTestServer(t, central)

	first, firstSize := postPull(t, ts.URL, "p", 0, 1000)
	if len(first.Mutations) == 0 {
		t.Fatal("first pull returned 0 mutations; the budget must never starve a non-empty backlog")
	}
	if len(first.Mutations) >= total {
		t.Fatalf("first pull returned all %d mutations (%d bytes); the byte budget did not truncate",
			len(first.Mutations), firstSize)
	}
	if firstSize > clientResponseCap {
		t.Errorf("first pull body = %d bytes, over the client's %d-byte cap — this response would be rejected "+
			"and the cursor would never advance", firstSize, clientResponseCap)
	}

	// Drain: keep asking from the last seq seen until a pull comes back empty.
	// Empty-batch termination is the wire contract — there is no has_more flag.
	seen := append([]int64(nil), seqsOf(first.Mutations)...)
	cursor := seen[len(seen)-1]
	for round := 2; ; round++ {
		if round > total+2 {
			t.Fatalf("drain did not terminate after %d rounds; seen=%v", round, seen)
		}
		next, size := postPull(t, ts.URL, "p", cursor, 1000)
		if len(next.Mutations) == 0 {
			break // drained
		}
		if size > clientResponseCap {
			t.Errorf("round %d body = %d bytes, over the client's %d-byte cap", round, size, clientResponseCap)
		}
		seen = append(seen, seqsOf(next.Mutations)...)
		cursor = seen[len(seen)-1]
	}

	// Strictly ascending, no gaps, no duplicates: exactly 1..total.
	if len(seen) != total {
		t.Fatalf("drained %d mutations (seqs %v), want %d", len(seen), seen, total)
	}
	for i, seq := range seen {
		if seq != int64(i+1) {
			t.Fatalf("drained seqs = %v; want the strict ascending prefix 1..%d", seen, total)
		}
	}
}

// seqsOf extracts the seq of each wire mutation, in order.
func seqsOf(muts []syncwire.WireMutation) []int64 {
	out := make([]int64, 0, len(muts))
	for _, m := range muts {
		out = append(out, m.Seq)
	}
	return out
}

// TestHandlePull_ByteBudget_AlwaysReturnsAtLeastOne pins the escape hatch. A single
// mutation larger than the whole budget must still be returned, because an empty
// response MEANS "drained" on this wire — withholding the row would stall that
// project's cursor forever, which is the very wedge being fixed. One over-budget
// response is recoverable (the client caps and reports it); a stalled cursor is not.
func TestHandlePull_ByteBudget_AlwaysReturnsAtLeastOne(t *testing.T) {
	// One row whose payload alone exceeds pullResponseByteBudget (3 MiB), while
	// still landing under the client's 4 MiB cap so the test asserts the server's
	// behaviour and not the client's rejection.
	central := &mockCentral{pullResult: fatMutations(2, (7<<20)/2)} // 3.5 MiB payloads
	ts := newTestServer(t, central)

	got, size := postPull(t, ts.URL, "p", 0, 1000)
	if len(got.Mutations) != 1 {
		t.Fatalf("returned %d mutations, want exactly 1 (the at-least-one guarantee: never 0, "+
			"and the second row must not be added on top of an already over-budget response)",
			len(got.Mutations))
	}
	if got.Mutations[0].Seq != 1 {
		t.Errorf("returned seq = %d, want 1 (the lowest unseen seq)", got.Mutations[0].Seq)
	}
	if size <= 3<<20 {
		t.Errorf("body = %d bytes; expected an over-budget single row (the point of this test)", size)
	}

	// And the cursor still moves: asking again from seq 1 yields the second row.
	next, _ := postPull(t, ts.URL, "p", 1, 1000)
	if len(next.Mutations) != 1 || next.Mutations[0].Seq != 2 {
		t.Errorf("follow-up pull = %v, want exactly seq 2 — progress must continue past a fat row",
			seqsOf(next.Mutations))
	}
}

// TestHandlePull_SmallBatch_SingleFetch guards the common path against a
// performance regression: when everything fits, chunking must be invisible — one
// query, every row returned. If this starts failing with more calls than expected,
// the chunk loop is re-querying a drained central.
func TestHandlePull_SmallBatch_SingleFetch(t *testing.T) {
	central := &mockCentral{pullResult: smallMutations(5)}
	ts := newTestServer(t, central)

	got, size := postPull(t, ts.URL, "p", 0, 0) // limit 0 → server default (100)
	if len(got.Mutations) != 5 {
		t.Fatalf("returned %d mutations, want 5", len(got.Mutations))
	}
	if size > clientResponseCap {
		t.Errorf("body = %d bytes, over the client cap", size)
	}
	// 5 rows < the 100-row chunk request → central is drained in ONE query.
	if len(central.pullCalls) != 1 {
		t.Errorf("PullSince calls = %d (%v), want 1: a short chunk already proves central is drained",
			len(central.pullCalls), central.pullCalls)
	}
	if central.pullCalls[0].limit != 100 {
		t.Errorf("first chunk limit = %d, want 100 (min(pullChunkSize, clamped limit))", central.pullCalls[0].limit)
	}
}
