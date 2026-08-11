package syncer_test

// End-to-end proof for the fat-batch pull path: in-memory Central → cloudserve →
// HTTP → remote.Client → syncer.Pull → real SQLite localstore.
//
// This is the scenario that wedged v1.5.2 in the field. A project whose mutations
// are large produces a pull response bigger than the client will buffer (4 MiB);
// the client rejected the body, the cursor never advanced, and the daemon
// re-requested the identical window forever. Three mechanisms now cooperate to make
// it converge, and only a test that runs all three together can prove they agree:
//
//	server  — bounds each response by pullResponseByteBudget (3 MiB) and truncates;
//	client  — reports a genuine overflow as transport.ErrResponseTooLarge;
//	syncer  — drains until an EMPTY batch, halving on overflow, growing on full batches.
//
// It deliberately uses an in-memory Central rather than the embedded-postgres
// harness used by the other wire acceptance tests: the mechanism under test is
// response SIZING, which Postgres contributes nothing to, and staying in memory
// keeps this in the default `go test ./...` run instead of behind a build tag.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/cloudserve"
	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/mutation"
	"github.com/mariesqu/engram/internal/remote"
	"github.com/mariesqu/engram/internal/syncer"
)

// memCentral is an in-process transport.Central holding a fixed, seq-ascending
// mutation list. It honors sinceSeq and limit exactly as a real central must —
// cloudserve's chunked accumulation walks a cursor across several queries, so a
// Central that ignored sinceSeq would loop instead of converging.
type memCentral struct {
	mu   sync.Mutex
	muts []domain.Mutation
}

func (c *memCentral) Apply(_ context.Context, m domain.Mutation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.muts = append(c.muts, m)
	return nil
}

func (c *memCentral) PullSince(_ context.Context, _ string, sinceSeq int64, limit int) ([]domain.Mutation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]domain.Mutation, 0, limit)
	for _, m := range c.muts {
		if m.Seq <= sinceSeq {
			continue
		}
		if len(out) >= limit {
			break
		}
		out = append(out, m)
	}
	return out, nil
}

// fatMutation builds a valid, fully-formed mutation whose content is contentBytes
// long. The bulk lands in the canonical payload, which is what actually crosses the
// wire — so these rows are fat everywhere it matters (server budget, client cap).
func fatMutation(project string, seq int64, contentBytes int) domain.Mutation {
	id := project + "-fat-" + strconv.FormatInt(seq, 10)
	m := domain.Mutation{
		Op:         domain.OpUpsert,
		Seq:        seq,
		SyncID:     "sync-" + id,
		SessionID:  "sess-fat",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "fat " + strconv.FormatInt(seq, 10),
		Content:    strings.Repeat("a", contentBytes),
		Project:    project,
		Scope:      "project",
		Version:    1,
		WriterID:   "writer-fat",
		UpdatedAt:  time.Date(2025, 1, 1, 0, 0, int(seq), 0, time.UTC),
		OccurredAt: time.Date(2025, 1, 1, 0, 0, int(seq), 0, time.UTC),
	}
	m.Payload = mutation.CanonicalPayload(m)
	m.MutationID = mutation.NewMutationID(m.Payload)
	return m
}

// TestPull_FatBatch_ConvergesOverTheWire drives the whole stack against a backlog
// several times larger than any single response may carry, and asserts it lands
// completely and exactly once.
func TestPull_FatBatch_ConvergesOverTheWire(t *testing.T) {
	ctx := context.Background()

	const (
		project      = "fatproject"
		total        = 8
		contentBytes = 512 << 10 // 512 KiB each → ~4 MiB total, over the 3 MiB budget
	)

	central := &memCentral{}
	for seq := int64(1); seq <= total; seq++ {
		central.muts = append(central.muts, fatMutation(project, seq, contentBytes))
	}

	// Count HTTP pulls so the test can prove the response was actually split —
	// a single round-trip would mean the server never truncated and the whole
	// scenario silently degenerated into the easy case.
	var pullRequests atomic.Int64
	handler := cloudserve.New(central, cloudserve.AllowAllVerifier()).Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/pull" {
			pullRequests.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	client := remote.New(srv.URL, nil, "writer-fat", make([]byte, 32))
	node := openNode(t, "fat-batch-node")

	pulled, err := syncer.Pull(ctx, node, client, project)
	if err != nil {
		t.Fatalf("Pull over the wire: %v", err)
	}
	if pulled != total {
		t.Errorf("pulled = %d; want %d (the whole fat backlog must land in one call)", pulled, total)
	}

	// The request COUNT is the sharp end of this test, bounded on both sides.
	//
	// Lower bound (>= 3): the backlog must actually have been split — two truncated
	// batches plus the empty confirmation. A count of 1 would mean the rows were not
	// fat enough and every other assertion here is proving the easy case.
	//
	// Upper bound (<= 5): the client must never have had to HALVE. When the server
	// bounds its own response, the client's 4 MiB cap is never hit, so syncer.Pull
	// keeps asking at its full limit and the conversation stays short. If the server
	// stopped budgeting, the client would still converge — its halving ladder walks
	// 1000 → 500 → … → 3 first — but it would take ~9 extra round-trips to do it.
	// That is the difference between the fix and the workaround, and this bound is
	// what tells them apart.
	if got := pullRequests.Load(); got < 3 {
		t.Errorf("HTTP /v1/pull requests = %d; want >= 3 (truncated batches + the empty "+
			"confirmation) — the byte budget did not split this backlog", got)
	} else if got > 5 {
		t.Errorf("HTTP /v1/pull requests = %d; want <= 5. The client had to halve its way down, "+
			"which means the server did not bound its own response — the wedge is only being "+
			"worked around, not prevented", got)
	}

	// Convergence: every mutation applied exactly once, cursor at the last seq.
	live, err := node.Store.CountLiveByProject(project)
	if err != nil {
		t.Fatalf("CountLiveByProject: %v", err)
	}
	if live != total {
		t.Errorf("live memories = %d; want %d (no gaps, no duplicates)", live, total)
	}
	cursor, err := node.Store.PullCursorFor(project)
	if err != nil {
		t.Fatalf("PullCursorFor: %v", err)
	}
	if cursor != total {
		t.Errorf("pull cursor = %d; want %d", cursor, total)
	}

	// Idempotence: a second call is a no-op that costs exactly one empty round-trip.
	before := pullRequests.Load()
	again, err := syncer.Pull(ctx, node, client, project)
	if err != nil {
		t.Fatalf("Pull (second call): %v", err)
	}
	if again != 0 {
		t.Errorf("second Pull returned %d; want 0 (already drained)", again)
	}
	if got := pullRequests.Load() - before; got != 1 {
		t.Errorf("second Pull issued %d HTTP requests; want exactly 1 (the empty confirmation)", got)
	}
}
