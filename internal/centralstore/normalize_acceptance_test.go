//go:build acceptance

// Package centralstore_test — acceptance coverage for TopicKey normalization.
//
// These tests prove that central.Apply with TopicKey=&"" stores topic_key as
// SQL NULL (not '') in both central_memories and central_tombstones, and that
// two independent no-topic records coexist without a UNIQUE violation on
// central_memories_topic_uidx / central_tombstones_topic_uidx.
package centralstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// noTopicMutCentral builds a minimal no-topic upsert mutation for central Apply tests.
// topicKey is the raw *string the caller wants to pass (nil, &"", or a real key).
func noTopicMutCentral(mutID, syncID, project string, topicKey *string) domain.Mutation {
	return domain.Mutation{
		MutationID: mutID,
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess-normalize",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "no-topic central",
		Content:    "no-topic content",
		Project:    project,
		Scope:      "project",
		TopicKey:   topicKey,
		Version:    1,
		Seq:        0,
		WriterID:   "writer-normalize",
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
		Payload:    []byte(`{}`),
	}
}

// noTopicDeleteMutCentral builds a minimal no-topic delete mutation for central Apply tests.
func noTopicDeleteMutCentral(mutID, syncID, project string, topicKey *string, version int) domain.Mutation {
	m := noTopicMutCentral(mutID, syncID, project, topicKey)
	m.Op = domain.OpDelete
	m.Version = version
	m.UpdatedAt = m.UpdatedAt.Add(time.Duration(version) * time.Second)
	return m
}

// TestApply_EmptyTopicKey_StoresNULL_Memory verifies that Apply (upsert) with
// TopicKey=&"" stores central_memories.topic_key as SQL NULL, not ''.
func TestApply_EmptyTopicKey_StoresNULL_Memory(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()
	empty := ""

	m := noTopicMutCentral("mut-nt-null-up", "sync-nt-null-up", "proj", &empty)
	if err := store.Apply(ctx, m); err != nil {
		t.Fatalf("Apply upsert with &\"\": %v", err)
	}

	var topicKey *string
	err := store.Pool().QueryRow(ctx,
		`SELECT topic_key FROM central_memories WHERE sync_id = $1`,
		"sync-nt-null-up",
	).Scan(&topicKey)
	if err != nil {
		t.Fatalf("SELECT topic_key: %v", err)
	}
	if topicKey != nil {
		t.Errorf("central_memories.topic_key = %q; want SQL NULL (not empty string)", *topicKey)
	}
}

// TestApply_EmptyTopicKey_StoresNULL_Tombstone verifies that Apply (delete) with
// TopicKey=&"" stores central_tombstones.topic_key as SQL NULL, not ''.
func TestApply_EmptyTopicKey_StoresNULL_Tombstone(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()
	empty := ""

	// Seed a live row first.
	mUp := noTopicMutCentral("mut-nt-null-del-up", "sync-nt-null-del", "proj", &empty)
	if err := store.Apply(ctx, mUp); err != nil {
		t.Fatalf("Apply upsert: %v", err)
	}

	// Now delete it.
	mDel := noTopicDeleteMutCentral("mut-nt-null-del", "sync-nt-null-del", "proj", &empty, 2)
	if err := store.Apply(ctx, mDel); err != nil {
		t.Fatalf("Apply delete with &\"\": %v", err)
	}

	var topicKey *string
	err := store.Pool().QueryRow(ctx,
		`SELECT topic_key FROM central_tombstones WHERE sync_id = $1`,
		"sync-nt-null-del",
	).Scan(&topicKey)
	if err != nil {
		t.Fatalf("SELECT tombstone topic_key: %v", err)
	}
	if topicKey != nil {
		t.Errorf("central_tombstones.topic_key = %q; want SQL NULL (not empty string)", *topicKey)
	}
}

// TestApply_TwoNoTopicUpserts_NoCollision verifies that two independent no-topic
// upserts (different sync_ids, &"" TopicKey) both land without a UNIQUE
// violation on central_memories_topic_uidx.
func TestApply_TwoNoTopicUpserts_NoCollision(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()
	empty := ""

	m1 := noTopicMutCentral("mut-nt-coll-1", "sync-nt-coll-1", "proj", &empty)
	m2 := noTopicMutCentral("mut-nt-coll-2", "sync-nt-coll-2", "proj", &empty)
	m2.UpdatedAt = m1.UpdatedAt.Add(time.Second)

	if err := store.Apply(ctx, m1); err != nil {
		t.Fatalf("Apply m1: %v", err)
	}
	if err := store.Apply(ctx, m2); err != nil {
		t.Fatalf("Apply m2 (second no-topic upsert): %v", err)
	}

	// Both rows must be live.
	var count int
	err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM central_memories
		 WHERE topic_key IS NULL AND project = 'proj' AND scope = 'project' AND deleted_at IS NULL`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count no-topic live rows: %v", err)
	}
	if count != 2 {
		t.Errorf("live no-topic rows = %d; want 2 (no central_memories_topic_uidx collision)", count)
	}
}

// TestApply_TwoNoTopicDeletes_NoCollision verifies that two independent no-topic
// deletes (different sync_ids, &"" TopicKey) produce two tombstone rows without
// a UNIQUE violation on central_tombstones_topic_uidx.
func TestApply_TwoNoTopicDeletes_NoCollision(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()
	empty := ""

	// Seed two live rows.
	for i, syncID := range []string{"sync-nt-ndel-1", "sync-nt-ndel-2"} {
		mutID := "mut-nt-ndel-up-" + syncID
		m := noTopicMutCentral(mutID, syncID, "proj", &empty)
		m.UpdatedAt = m.UpdatedAt.Add(time.Duration(i) * time.Second)
		if err := store.Apply(ctx, m); err != nil {
			t.Fatalf("Apply upsert %s: %v", syncID, err)
		}
	}

	// Delete both.
	for i, syncID := range []string{"sync-nt-ndel-1", "sync-nt-ndel-2"} {
		mutID := "mut-nt-ndel-del-" + syncID
		del := noTopicDeleteMutCentral(mutID, syncID, "proj", &empty, 2+i)
		if err := store.Apply(ctx, del); err != nil {
			t.Fatalf("Apply delete %s: %v", syncID, err)
		}
	}

	// Both tombstones must exist with NULL topic_key.
	var count int
	err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM central_tombstones
		 WHERE topic_key IS NULL AND project = 'proj' AND scope = 'project'`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count no-topic tombstones: %v", err)
	}
	if count != 2 {
		t.Errorf("no-topic tombstone rows = %d; want 2 (no central_tombstones_topic_uidx collision)", count)
	}
}
