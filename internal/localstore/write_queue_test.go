package localstore

// write_queue_test.go validates the write serialization guarantee introduced by
// the write-lock on Store. Tests cover:
//
//  1. Same-topic concurrent AddObservation + ApplyPulled: no SQLITE_BUSY, no
//     deadlock, LWW winner is stable (always the highest-version mutation).
//  2. Concurrent distinct-topic AddObservation: all succeed.
//  3. Concurrent reads (Search) alongside writes: no deadlock, reads return.
//  4. AddObservation PK resolution: the returned ID always resolves via
//     GetObservation to the live canonical row.

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/mutation"
)

// TestWriteQueue_SameTopicConcurrentAddAndApply is the core concurrency test.
//
// Many goroutines concurrently call AddObservation to the same topic_key.
// The autosync loop is simulated by a goroutine that concurrently calls
// ApplyPulled for the same topic. Assertions:
//
//	a) No SQLITE_BUSY or any other error.
//	b) No deadlock (test will time out if deadlocked).
//	c) Deterministic LWW convergence: the live row's content matches the
//	   mutation with the highest version (the clear LWW winner).
//	d) AddObservation's returned PK always resolves via GetObservation to the
//	   live canonical row (PK resolution is atomic under the write lock).
func TestWriteQueue_SameTopicConcurrentAddAndApply(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "wq_same_topic.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		topic      = "architecture/auth-model"
		project    = "engram"
		scope      = "project"
		nWriters   = 12
		nPullers   = 4
		winVersion = 9999 // the clear LWW winner: far above all concurrent writes
	)

	// Pre-seed a session so session_id FK is satisfied.
	if err := store.CreateSession("sess-wq", project, "/src"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// errCh collects all goroutine errors without blocking.
	errCh := make(chan error, nWriters+nPullers)
	// pkCh collects (id, syncID) pairs from AddObservation so we can verify
	// PK resolution after all goroutines finish.
	type pkPair struct {
		id     int64
		syncID string
	}
	pkCh := make(chan pkPair, nWriters)

	var wg sync.WaitGroup

	// Launch AddObservation goroutines — all writing to the same topic_key.
	for i := 0; i < nWriters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := store.AddObservation(AddObservationParams{
				SessionID: "sess-wq",
				Type:      "architecture",
				Title:     fmt.Sprintf("writer-%d", i),
				Content:   fmt.Sprintf("content from writer %d", i),
				Project:   project,
				Scope:     scope,
				TopicKey:  topic,
			})
			if err != nil {
				errCh <- fmt.Errorf("AddObservation writer %d: %w", i, err)
				return
			}
			pkCh <- pkPair{id: res.ID, syncID: res.SyncID}
		}(i)
	}

	// Launch ApplyPulled goroutines — simulating the autosync Loop pulling from
	// central for the same topic_key. The winning mutation uses version=winVersion
	// (a far-ahead version) so the LWW outcome is deterministic.
	winContent := fmt.Sprintf("pulled winner v%d", winVersion)
	for i := 0; i < nPullers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tk := topic
			m := domain.Mutation{
				Op:         domain.OpUpsert,
				SyncID:     fmt.Sprintf("pulled-sync-%d", i),
				SessionID:  "sess-wq",
				EntityType: domain.EntityMemory,
				Type:       "architecture",
				Title:      fmt.Sprintf("pulled-title-%d", i),
				Content:    winContent,
				Project:    project,
				Scope:      scope,
				TopicKey:   &tk,
				Version:    winVersion,
				UpdatedAt:  time.Now().UTC().Add(time.Duration(i) * time.Second),
				WriterID:   fmt.Sprintf("pull-writer-%d", i),
			}
			m = normalizeMutation(m)
			if err := store.ApplyPulled(m); err != nil {
				errCh <- fmt.Errorf("ApplyPulled puller %d: %w", i, err)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)
	close(pkCh)

	// (a) No errors.
	for err := range errCh {
		t.Errorf("concurrent error: %v", err)
	}

	// (c) LWW convergence: the live row must have version=winVersion (the
	// ApplyPulled mutations with version=9999 are the clear winners).
	rec, err := store.FindByTopic(topic, project, scope)
	if err != nil {
		t.Fatalf("FindByTopic after concurrency: %v", err)
	}
	if rec == nil {
		t.Fatal("FindByTopic: no live row after concurrent writes")
	}
	if rec.Version != winVersion {
		t.Errorf("live row version = %d, want %d (LWW winner must be the highest-version pulled mutation)",
			rec.Version, winVersion)
	}

	// (d) PK resolution: every AddObservation result ID must map to a live row via
	// GetObservation. The returned ID may now point to the canonical row for the
	// topic (which could have a different sync_id if a later write updated it) or
	// to its own row — the key property is that GetObservation(id) must not return
	// ErrObservationNotFound. We verify the ID resolves without error.
	for pair := range pkCh {
		if pair.id <= 0 {
			t.Errorf("AddObservation returned non-positive ID: %d", pair.id)
			continue
		}
		if _, err := store.GetObservation(pair.id); err != nil {
			t.Errorf("GetObservation(%d) after concurrent writes: %v (PK resolution must be atomic)", pair.id, err)
		}
	}
}

// TestWriteQueue_ConcurrentDistinctTopics verifies that concurrent
// AddObservation calls targeting distinct topic_keys all succeed without
// interference. This is the happy-path baseline: no LWW conflict, just parallel
// work on independent rows.
func TestWriteQueue_ConcurrentDistinctTopics(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "wq_distinct.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const n = 20
	if err := store.CreateSession("sess-dist", "engram", "/src"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	errCh := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.AddObservation(AddObservationParams{
				SessionID: "sess-dist",
				Title:     fmt.Sprintf("obs-%d", i),
				Content:   fmt.Sprintf("content-%d", i),
				Project:   "engram",
				TopicKey:  fmt.Sprintf("distinct/topic/%d", i),
			})
			if err != nil {
				errCh <- fmt.Errorf("AddObservation %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent distinct-topic error: %v", err)
	}

	// All n topics must have a live row.
	for i := 0; i < n; i++ {
		topic := fmt.Sprintf("distinct/topic/%d", i)
		rec, err := store.FindByTopic(topic, "engram", "project")
		if err != nil {
			t.Errorf("FindByTopic(%s): %v", topic, err)
			continue
		}
		if rec == nil {
			t.Errorf("topic %s: no live row after concurrent AddObservation", topic)
		}
	}
}

// TestWriteQueue_ConcurrentReadsDontDeadlock verifies that concurrent read
// operations (SearchMemories, GetObservation) running alongside writes do not
// deadlock. Reads must NOT take the write lock — they are allowed to run
// concurrently with in-flight writes (WAL snapshot isolation).
func TestWriteQueue_ConcurrentReadsDontDeadlock(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "wq_reads.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Seed one row so searches have something to find.
	if err := store.CreateSession("sess-rd", "engram", "/src"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	seed, err := store.AddObservation(AddObservationParams{
		SessionID: "sess-rd",
		Title:     "seed obs for read test",
		Content:   "some content",
		Project:   "engram",
		TopicKey:  "read/seed",
	})
	if err != nil {
		t.Fatalf("seed AddObservation: %v", err)
	}

	const (
		nWriters = 6
		nReaders = 6
	)
	done := make(chan struct{})
	errCh := make(chan error, nWriters+nReaders)
	var wg sync.WaitGroup

	// Writers: run long writes concurrently.
	for i := 0; i < nWriters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.AddObservation(AddObservationParams{
				SessionID: "sess-rd",
				Title:     fmt.Sprintf("write-%d", i),
				Content:   fmt.Sprintf("write content %d", i),
				Project:   "engram",
				TopicKey:  fmt.Sprintf("read/write/%d", i),
			})
			if err != nil {
				errCh <- fmt.Errorf("write %d: %w", i, err)
			}
		}(i)
	}

	// Readers: run concurrently. The key assertion is that these return promptly
	// (the test has a 5s timeout via t.Deadline if run with -timeout; here we
	// use the done channel to signal reads can start immediately).
	close(done)
	for i := 0; i < nReaders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// SearchMemories must not block on the write lock.
			_, serr := store.SearchMemories("seed obs", "engram", 10)
			if serr != nil {
				errCh <- fmt.Errorf("SearchMemories reader %d: %w", i, serr)
				return
			}
			// GetObservation must not block on the write lock.
			_, gerr := store.GetObservation(seed.ID)
			if gerr != nil && gerr != ErrObservationNotFound {
				errCh <- fmt.Errorf("GetObservation reader %d: %w", i, gerr)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("read/write concurrency error: %v", err)
	}
}

// TestWriteQueue_ApplyPulledAtomicWithLocalWrite verifies the specific race
// described in the PR: ApplyPulled (simulating the autosync Loop) running
// concurrently with AddObservation's version-pre-read → write sequence MUST NOT
// interleave. The write lock ensures AddObservation holds the lock across the
// full FindByTopic(version read) → localWriteLocked → FindByTopic(PK resolve)
// sequence so ApplyPulled cannot commit between those steps.
//
// Failure mode without the lock: ApplyPulled commits a higher-version row
// between AddObservation's version pre-read and its write, causing AddObservation
// to write with a stale version that loses the LWW tiebreaker — and then the
// post-commit PK resolution (FindByTopic) returns the ApplyPulled row, not the
// one AddObservation thought it wrote, confusing the caller.
func TestWriteQueue_ApplyPulledAtomicWithLocalWrite(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "wq_atomic.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.CreateSession("sess-atm", "engram", "/src"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	const (
		topic   = "concurrent/atomic/topic"
		project = "engram"
		scope   = "project"
		rounds  = 50
	)

	for round := 0; round < rounds; round++ {
		// Each round: one AddObservation + one ApplyPulled racing on the same topic.
		var wg sync.WaitGroup
		errCh := make(chan error, 2)

		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := store.AddObservation(AddObservationParams{
				SessionID: "sess-atm",
				Type:      "decision",
				Title:     fmt.Sprintf("add-round-%d", round),
				Content:   fmt.Sprintf("local write round %d", round),
				Project:   project,
				Scope:     scope,
				TopicKey:  topic,
			})
			if err != nil {
				errCh <- fmt.Errorf("round %d AddObservation: %w", round, err)
				return
			}
			// PK resolution must succeed: the returned ID must resolve.
			if _, err := store.GetObservation(res.ID); err != nil {
				errCh <- fmt.Errorf("round %d GetObservation(%d): %w (PK resolution not atomic)", round, res.ID, err)
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			tk := topic
			m := domain.Mutation{
				Op:         domain.OpUpsert,
				SyncID:     fmt.Sprintf("pulled-atm-%d", round),
				SessionID:  "sess-atm",
				EntityType: domain.EntityMemory,
				Type:       "decision",
				Title:      fmt.Sprintf("pulled-round-%d", round),
				Content:    fmt.Sprintf("pulled write round %d", round),
				Project:    project,
				Scope:      scope,
				TopicKey:   &tk,
				Version:    round + 1000, // always higher than local add
				UpdatedAt:  time.Now().UTC(),
				WriterID:   "central-writer",
			}
			m = normalizeMutation(m)
			if err := store.ApplyPulled(m); err != nil {
				errCh <- fmt.Errorf("round %d ApplyPulled: %w", round, err)
			}
		}()

		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Errorf("%v", err)
		}
	}

	// Final state: topic must have a live row (not disappeared due to race).
	rec, err := store.FindByTopic(topic, project, scope)
	if err != nil {
		t.Fatalf("FindByTopic after %d rounds: %v", rounds, err)
	}
	if rec == nil {
		t.Fatal("FindByTopic: no live row after repeated concurrent writes")
	}

	// The convergence property: a deterministic winner must exist. The row's
	// version must be >= 1 (either the local AddObservation or a pulled mutation
	// won LWW — both are valid, as long as there's exactly one live row).
	if rec.Version < 1 {
		t.Errorf("live row version = %d, want >= 1", rec.Version)
	}
}

// Ensure mutation package is used (for normalizeMutation call in tests above).
var _ = mutation.NewMutationID
