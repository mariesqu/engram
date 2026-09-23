//go:build acceptance

// Acceptance test for OriginalCreatedAt (FUP-005) against a REAL Postgres
// instance (embedded-postgres, or ENGRAM_TEST_PG_DSN if set — see
// store_acceptance_test.go's TestMain).
package centralstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/mutation"
)

// applyWithOccurredAt is testMutation's shape but with an explicit,
// content-addressed mutation, so OccurredAt can be set to a specific
// historical instant (testMutation's dummy `{}` payload does not need a real
// hash, but Apply's own NUL-rejection path does decode the payload, so a real
// canonical one is used here throughout).
func applyWithOccurredAt(t *testing.T, ctx context.Context, store applyer, syncID, project string, version int, occurredAt time.Time) {
	t.Helper()
	m := domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "title",
		Content:    "content",
		Project:    project,
		Scope:      "project",
		Version:    version,
		WriterID:   "writer-" + syncID,
		UpdatedAt:  occurredAt,
		OccurredAt: occurredAt,
	}
	m.Payload = mutation.CanonicalPayload(m)
	m.MutationID = mutation.NewMutationID(m.Payload)
	if err := store.Apply(ctx, m); err != nil {
		t.Fatalf("Apply(%s, v%d): %v", syncID, version, err)
	}
}

// applyer is the minimal surface this file needs from *centralstore.Store —
// named locally so applyWithOccurredAt does not have to import the package
// under a second alias (this IS package centralstore_test already).
type applyer interface {
	Apply(ctx context.Context, m domain.Mutation) error
}

// TestOriginalCreatedAt_ReturnsEarliestOccurredAtPerSyncID is the core FUP-005
// server-side proof: for a sync_id pushed twice (an insert, then a later
// revision with a MUCH later OccurredAt), OriginalCreatedAt must report the
// FIRST (earliest) occurred_at — the true original creation time — not the
// most recent revision's.
func TestOriginalCreatedAt_ReturnsEarliestOccurredAtPerSyncID(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	original := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	revision := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

	applyWithOccurredAt(t, ctx, store, "sync-created-a", "proj-created", 1, original)
	applyWithOccurredAt(t, ctx, store, "sync-created-a", "proj-created", 2, revision)

	entries, err := store.OriginalCreatedAt(ctx, "proj-created", "", 100)
	if err != nil {
		t.Fatalf("OriginalCreatedAt: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want exactly 1 (one sync_id)", entries)
	}
	if got := entries[0].CreatedAt; !got.Equal(original) {
		t.Errorf("CreatedAt = %v, want the EARLIEST occurred_at %v, not the revision's %v", got, original, revision)
	}
}

// TestOriginalCreatedAt_KeysetPagingCoversEveryEntryExactlyOnce proves the
// keyset cursor (After) is correct and complete: paging through with a small
// limit must visit every sync_id in the project exactly once, in ascending
// order, and terminate with an empty page.
func TestOriginalCreatedAt_KeysetPagingCoversEveryEntryExactlyOnce(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	const project = "proj-paging"
	syncIDs := []string{"sync-p-1", "sync-p-2", "sync-p-3", "sync-p-4", "sync-p-5"}
	for i, id := range syncIDs {
		applyWithOccurredAt(t, ctx, store, id, project, 1,
			time.Date(2022, 1, i+1, 0, 0, 0, 0, time.UTC))
	}

	var seen []string
	after := ""
	for i := 0; i < 10; i++ { // hard cap so a paging bug fails fast, not by timeout
		page, err := store.OriginalCreatedAt(ctx, project, after, 2)
		if err != nil {
			t.Fatalf("OriginalCreatedAt(after=%q): %v", after, err)
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			seen = append(seen, e.SyncID)
		}
		after = page[len(page)-1].SyncID
	}

	if len(seen) != len(syncIDs) {
		t.Fatalf("paged through %d entries, want %d: %v", len(seen), len(syncIDs), seen)
	}
	for i, id := range syncIDs {
		if seen[i] != id {
			t.Errorf("seen[%d] = %q, want %q (ascending sync_id order): %v", i, seen[i], id, seen)
			break
		}
	}
}

// TestOriginalCreatedAt_ScopesToProject proves a project filter is applied:
// entries from a DIFFERENT project must never leak into the result.
func TestOriginalCreatedAt_ScopesToProject(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	applyWithOccurredAt(t, ctx, store, "sync-scope-a", "proj-scope-a", 1, time.Now().UTC())
	applyWithOccurredAt(t, ctx, store, "sync-scope-b", "proj-scope-b", 1, time.Now().UTC())

	entries, err := store.OriginalCreatedAt(ctx, "proj-scope-a", "", 100)
	if err != nil {
		t.Fatalf("OriginalCreatedAt: %v", err)
	}
	if len(entries) != 1 || entries[0].SyncID != "sync-scope-a" {
		t.Fatalf("entries = %+v, want exactly [sync-scope-a]", entries)
	}
}
