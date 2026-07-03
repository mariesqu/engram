//go:build acceptance

package centralstore_test

// Tests for the remote-purge generation counter: WriterPurgeEpoch,
// BumpPurgeEpoch, BumpAllPurgeEpochs.

import (
	"context"
	"testing"

	"github.com/mariesqu/engram/internal/centralstore"
)

// TestWriterPurgeEpoch_DefaultsToZero verifies that a freshly-provisioned
// writer starts with purge_epoch = 0.
func TestWriterPurgeEpoch_DefaultsToZero(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	secret := []byte("purge-epoch-default-hmac-key-32b")
	if err := store.UpsertWriterKey(ctx, "writer-epoch-default", secret); err != nil {
		t.Fatalf("UpsertWriterKey: %v", err)
	}

	epoch, err := store.WriterPurgeEpoch(ctx, "writer-epoch-default")
	if err != nil {
		t.Fatalf("WriterPurgeEpoch: %v", err)
	}
	if epoch != 0 {
		t.Errorf("WriterPurgeEpoch = %d; want 0 for a freshly-provisioned writer", epoch)
	}
}

// TestBumpPurgeEpoch_Increments verifies that BumpPurgeEpoch increments by 1
// each call and returns the new value.
func TestBumpPurgeEpoch_Increments(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	secret := []byte("purge-epoch-bump-hmac-key-32byte")
	if err := store.UpsertWriterKey(ctx, "writer-epoch-bump", secret); err != nil {
		t.Fatalf("UpsertWriterKey: %v", err)
	}

	got, err := store.BumpPurgeEpoch(ctx, "writer-epoch-bump")
	if err != nil {
		t.Fatalf("BumpPurgeEpoch (1st): %v", err)
	}
	if got != 1 {
		t.Errorf("BumpPurgeEpoch (1st) = %d; want 1", got)
	}

	got, err = store.BumpPurgeEpoch(ctx, "writer-epoch-bump")
	if err != nil {
		t.Fatalf("BumpPurgeEpoch (2nd): %v", err)
	}
	if got != 2 {
		t.Errorf("BumpPurgeEpoch (2nd) = %d; want 2", got)
	}

	// WriterPurgeEpoch must reflect the same value.
	read, err := store.WriterPurgeEpoch(ctx, "writer-epoch-bump")
	if err != nil {
		t.Fatalf("WriterPurgeEpoch after bumps: %v", err)
	}
	if read != 2 {
		t.Errorf("WriterPurgeEpoch after 2 bumps = %d; want 2", read)
	}
}

// TestBumpPurgeEpoch_UnknownWriter_NotFound verifies BumpPurgeEpoch returns
// ErrWriterKeyNotFound for a writer_id that was never provisioned.
func TestBumpPurgeEpoch_UnknownWriter_NotFound(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	_, err := store.BumpPurgeEpoch(ctx, "writer-epoch-never-provisioned")
	if err == nil {
		t.Fatal("BumpPurgeEpoch for unknown writer: expected ErrWriterKeyNotFound, got nil")
	}
	if err != centralstore.ErrWriterKeyNotFound {
		t.Errorf("BumpPurgeEpoch for unknown writer: got %v, want ErrWriterKeyNotFound", err)
	}
}

// TestBumpPurgeEpoch_RevokedWriter_NotFound verifies that a revoked (inactive)
// writer cannot have its epoch bumped — BumpPurgeEpoch's WHERE active = true
// excludes it, so it returns ErrWriterKeyNotFound.
func TestBumpPurgeEpoch_RevokedWriter_NotFound(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	secret := []byte("purge-epoch-revoked-hmac-key-32b")
	if err := store.UpsertWriterKey(ctx, "writer-epoch-revoked", secret); err != nil {
		t.Fatalf("UpsertWriterKey: %v", err)
	}
	if err := store.DeactivateWriterKey(ctx, "writer-epoch-revoked"); err != nil {
		t.Fatalf("DeactivateWriterKey: %v", err)
	}

	_, err := store.BumpPurgeEpoch(ctx, "writer-epoch-revoked")
	if err == nil {
		t.Fatal("BumpPurgeEpoch for revoked writer: expected ErrWriterKeyNotFound, got nil")
	}
	if err != centralstore.ErrWriterKeyNotFound {
		t.Errorf("BumpPurgeEpoch for revoked writer: got %v, want ErrWriterKeyNotFound", err)
	}

	// WriterPurgeEpoch must ALSO refuse the revoked writer (same not-found
	// semantics as WriterKey — a revoked writer must not be probeable).
	_, err = store.WriterPurgeEpoch(ctx, "writer-epoch-revoked")
	if err != centralstore.ErrWriterKeyNotFound {
		t.Errorf("WriterPurgeEpoch for revoked writer: got %v, want ErrWriterKeyNotFound", err)
	}
}

// TestBumpAllPurgeEpochs_ExcludesGivenWriters verifies that BumpAllPurgeEpochs
// bumps every ACTIVE writer except those listed in except, and leaves inactive
// (revoked) writers untouched regardless of the exclusion list.
func TestBumpAllPurgeEpochs_ExcludesGivenWriters(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	writers := []string{"writer-all-a", "writer-all-b", "writer-all-c"}
	for _, w := range writers {
		if err := store.UpsertWriterKey(ctx, w, []byte("purge-epoch-all-hmac-key-32byte")); err != nil {
			t.Fatalf("UpsertWriterKey(%q): %v", w, err)
		}
	}

	// Revoke writer-all-c so we can assert inactive writers are unaffected too.
	if err := store.DeactivateWriterKey(ctx, "writer-all-c"); err != nil {
		t.Fatalf("DeactivateWriterKey(writer-all-c): %v", err)
	}

	n, err := store.BumpAllPurgeEpochs(ctx, []string{"writer-all-b"})
	if err != nil {
		t.Fatalf("BumpAllPurgeEpochs: %v", err)
	}
	// Only writer-all-a is active and not excluded (writer-all-b excluded,
	// writer-all-c inactive).
	if n != 1 {
		t.Errorf("BumpAllPurgeEpochs affected = %d; want 1 (only writer-all-a)", n)
	}

	gotA, err := store.WriterPurgeEpoch(ctx, "writer-all-a")
	if err != nil {
		t.Fatalf("WriterPurgeEpoch(writer-all-a): %v", err)
	}
	if gotA != 1 {
		t.Errorf("writer-all-a epoch = %d; want 1 (bumped)", gotA)
	}

	gotB, err := store.WriterPurgeEpoch(ctx, "writer-all-b")
	if err != nil {
		t.Fatalf("WriterPurgeEpoch(writer-all-b): %v", err)
	}
	if gotB != 0 {
		t.Errorf("writer-all-b epoch = %d; want 0 (excluded, must not be bumped)", gotB)
	}
}

// TestBumpAllPurgeEpochs_NilExcept_BumpsEveryActiveWriter verifies the nil-vs-
// empty-slice pgx binding edge case: a nil except must bump EVERY active
// writer (not zero, which would happen if nil encoded as SQL NULL and
// `x = ANY(NULL)` silently matched nothing).
func TestBumpAllPurgeEpochs_NilExcept_BumpsEveryActiveWriter(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	writers := []string{"writer-nil-a", "writer-nil-b"}
	for _, w := range writers {
		if err := store.UpsertWriterKey(ctx, w, []byte("purge-epoch-nil-hmac-key-32byte")); err != nil {
			t.Fatalf("UpsertWriterKey(%q): %v", w, err)
		}
	}

	n, err := store.BumpAllPurgeEpochs(ctx, nil)
	if err != nil {
		t.Fatalf("BumpAllPurgeEpochs(nil): %v", err)
	}
	if n != len(writers) {
		t.Errorf("BumpAllPurgeEpochs(nil) affected = %d; want %d (every active writer)", n, len(writers))
	}

	for _, w := range writers {
		epoch, err := store.WriterPurgeEpoch(ctx, w)
		if err != nil {
			t.Fatalf("WriterPurgeEpoch(%q): %v", w, err)
		}
		if epoch != 1 {
			t.Errorf("%s epoch = %d; want 1", w, epoch)
		}
	}
}
