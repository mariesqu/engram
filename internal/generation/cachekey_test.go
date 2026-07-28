package generation

import (
	"math/rand"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

func hashRecord(syncID, title, content, writerID string, updatedAt time.Time) *domain.Record {
	return &domain.Record{
		SyncID:    syncID,
		Title:     title,
		Content:   content,
		WriterID:  writerID,
		UpdatedAt: updatedAt,
	}
}

func baseHashRecords() []*domain.Record {
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	return []*domain.Record{
		hashRecord("sync-1", "Title one", "Content one", "writer-a", base),
		hashRecord("sync-2", "Title two", "Content two", "writer-b", base.Add(time.Hour)),
		hashRecord("sync-3", "Title three", "Content three", "writer-a", base.Add(2*time.Hour)),
	}
}

// TestSourceHashLengthPrefixCollision is the highest-value assertion in
// this phase. A delimiter-joined ("|") field composition -- e.g.
// strings.Join(fields, "|") -- lets a "|" inside one field's content shift
// the field boundary, so two genuinely different field lists can hash
// identically: []string{"a|b"} (one field) and []string{"a", "b"} (two
// fields) both naively join to the identical string "a|b". A collision here
// is a poisoned cache entry: a permanent hit silently serving wrong prose,
// forever. Length-prefixing (8-byte big-endian length, then bytes, per
// field) must keep these two inputs distinct.
func TestSourceHashLengthPrefixCollision(t *testing.T) {
	sum1 := lengthPrefixedSHA256([]string{"a|b"})
	sum2 := lengthPrefixedSHA256([]string{"a", "b"})
	if sum1 == sum2 {
		t.Fatalf("lengthPrefixedSHA256 collision: []string{%q} and []string{%q,%q} both hash to %x -- "+
			"delimiter-joined field composition lets a \"|\" inside content shift the field boundary, "+
			"poisoning the cache permanently", "a|b", "a", "b", sum1)
	}
}

// TestSourceHashIsStableAcrossRecordOrder pins REQ-NARR-01: the hash sorts
// records by SyncID ascending, BYTE-WISE, never by store read order.
// RecentObservations orders "created_at DESC, id DESC" and that id tiebreak
// is replica-divergent across writer machines -- the hash must not depend
// on which order the caller happened to pass records in.
func TestSourceHashIsStableAcrossRecordOrder(t *testing.T) {
	recs := baseHashRecords()
	want := SourceHash("pt-1", "model-x", "proj", "topic", recs)

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 10; i++ {
		shuffled := make([]*domain.Record, len(recs))
		copy(shuffled, recs)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })

		got := SourceHash("pt-1", "model-x", "proj", "topic", shuffled)
		if got != want {
			t.Errorf("shuffle %d: SourceHash = %s, want %s (order must not matter)", i, got, want)
		}
	}
}

// TestSourceHashExcludesRecordID pins the exclusion of domain.Record.ID:
// domain/memory.go:36-40 documents ID as the local-store autoincrement
// primary key, replica-divergent and NOT part of the canonical payload --
// two record sets differing ONLY in ID must hash identically.
func TestSourceHashExcludesRecordID(t *testing.T) {
	recs := baseHashRecords()
	withIDs := make([]*domain.Record, len(recs))
	for i, r := range recs {
		cp := *r
		cp.ID = int64(i + 1000)
		withIDs[i] = &cp
	}

	got := SourceHash("pt-1", "model-x", "proj", "topic", withIDs)
	want := SourceHash("pt-1", "model-x", "proj", "topic", recs)
	if got != want {
		t.Errorf("SourceHash changed when only Record.ID differed: %s != %s", got, want)
	}
}

// TestSourceHashExcludesWallClock pins that SourceHash reads no wall clock
// anywhere in its computation: two calls over the identical input, however
// far apart in real time, must produce the identical hash. This is what
// makes "unchanged corpus -> zero HTTP requests on the second cycle"
// possible at all.
func TestSourceHashExcludesWallClock(t *testing.T) {
	recs := baseHashRecords()
	first := SourceHash("pt-1", "model-x", "proj", "topic", recs)
	time.Sleep(5 * time.Millisecond)
	second := SourceHash("pt-1", "model-x", "proj", "topic", recs)
	if first != second {
		t.Errorf("SourceHash is wall-clock dependent: %s != %s", first, second)
	}
}

// TestSourceHashChangesWithModelName mirrors the verified re-embed
// predicate in internal/embedding: rows whose stored embedding_model
// differs from the current ModelName() are re-embedded. The narrative
// cache key must apply the same discipline when narrative_model changes.
func TestSourceHashChangesWithModelName(t *testing.T) {
	recs := baseHashRecords()
	a := SourceHash("pt-1", "model-x", "proj", "topic", recs)
	b := SourceHash("pt-1", "model-y", "proj", "topic", recs)
	if a == b {
		t.Error("SourceHash did not change when modelName changed")
	}
}

// TestSourceHashChangesWithTemplateVersion pins that a prompt-template
// change (a later slice bumping promptTemplateVersion) invalidates every
// cache entry on the next cycle rather than silently serving prose
// generated under the old template.
func TestSourceHashChangesWithTemplateVersion(t *testing.T) {
	recs := baseHashRecords()
	a := SourceHash("pt-1", "model-x", "proj", "topic", recs)
	b := SourceHash("pt-2", "model-x", "proj", "topic", recs)
	if a == b {
		t.Error("SourceHash did not change when templateVersion changed")
	}
}

// TestSourceHashChangesWithContentWriterIDOrUpdatedAt tables one mutated
// field per case, holding everything else constant: content, writer_id and
// updated_at are all part of a record's narratable state and must each
// independently move the hash.
func TestSourceHashChangesWithContentWriterIDOrUpdatedAt(t *testing.T) {
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	baseline := []*domain.Record{hashRecord("sync-1", "Title", "Content", "writer-a", base)}
	baselineHash := SourceHash("pt-1", "model-x", "proj", "topic", baseline)

	tests := []struct {
		name string
		recs []*domain.Record
	}{
		{"content changed", []*domain.Record{hashRecord("sync-1", "Title", "DIFFERENT CONTENT", "writer-a", base)}},
		{"writer_id changed", []*domain.Record{hashRecord("sync-1", "Title", "Content", "writer-DIFFERENT", base)}},
		{"updated_at changed", []*domain.Record{hashRecord("sync-1", "Title", "Content", "writer-a", base.Add(time.Second))}},
		{"title changed", []*domain.Record{hashRecord("sync-1", "DIFFERENT TITLE", "Content", "writer-a", base)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SourceHash("pt-1", "model-x", "proj", "topic", tt.recs)
			if got == baselineHash {
				t.Errorf("SourceHash did not change for %s", tt.name)
			}
		})
	}
}

// TestSourceHashExcludesRendererVersion pins design's deliberate DIVERGENCE
// from the proposal (discrepancy 4, tasks.md): renderer_version is
// deliberately NOT part of the cache key, because a cosmetic vault-layout
// tweak must not force a paid regeneration -- writeIfChanged already
// catches byte-level render changes for free. SourceHash's signature itself
// has no rendererVersion parameter; this compile-time assertion pins that
// absence so a future change cannot silently add one back in.
func TestSourceHashExcludesRendererVersion(t *testing.T) {
	var _ func(string, string, string, string, []*domain.Record) string = SourceHash
}
