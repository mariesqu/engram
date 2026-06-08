package localstore

import (
	"sync"
	"testing"
)

// TestWriteQueue_VersionProgressionNoCollision proves the write mutex is
// LOAD-BEARING — i.e. NOT redundant with SetMaxOpenConns(1). K concurrent
// AddObservation calls to the SAME topic each do a read-modify-write of the
// version (pre-read existing version, write existing+1). Under the mutex the
// whole sequence is atomic, so the writes apply strictly in order 1,2,…,K and
// the canonical row ends at version == K.
//
// Without the mutex the version pre-read and the write are SEPARATE statements;
// SetMaxOpenConns(1) serializes each statement but lets another writer commit
// between them, so concurrent writers read the same version and collide — the
// final version would be < K. So this assertion fails if the mutex is removed.
func TestWriteQueue_VersionProgressionNoCollision(t *testing.T) {
	s := openTempStore(t)
	const K = 40
	const topic = "writequeue/version-progression"

	var wg sync.WaitGroup
	wg.Add(K)
	for i := 0; i < K; i++ {
		go func() {
			defer wg.Done()
			if _, err := s.AddObservation(AddObservationParams{
				Title: "t", Content: "v", Project: "p", TopicKey: topic,
			}); err != nil {
				t.Errorf("AddObservation: %v", err)
			}
		}()
	}
	wg.Wait()

	rec, err := s.FindByTopic(topic, "p", "project")
	if err != nil {
		t.Fatalf("FindByTopic: %v", err)
	}
	if rec == nil {
		t.Fatal("topic row missing after concurrent writes")
	}
	if rec.Version != K {
		t.Errorf("final version = %d, want %d — a lower value means concurrent writers read a stale version and collided (the write mutex is not serializing the read-modify-write)", rec.Version, K)
	}
}
