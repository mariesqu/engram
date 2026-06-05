//go:build acceptance

package centralstore_test

import (
	"context"
	"testing"

	"github.com/mariesqu/engram/internal/centralstore"
)

// TestWriterKey_UpsertAndRetrieve verifies the basic provision flow: after
// UpsertWriterKey the same secret is returned by WriterKey.
func TestWriterKey_UpsertAndRetrieve(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	secret := []byte("super-secret-hmac-key-32-bytes!!")
	if err := store.UpsertWriterKey(ctx, "writer-a", secret); err != nil {
		t.Fatalf("UpsertWriterKey: %v", err)
	}

	got, err := store.WriterKey(ctx, "writer-a")
	if err != nil {
		t.Fatalf("WriterKey: %v", err)
	}
	if string(got) != string(secret) {
		t.Errorf("WriterKey: got %q, want %q", got, secret)
	}
}

// TestWriterKey_UnknownID_ReturnsNotFound verifies that WriterKey returns
// ErrWriterKeyNotFound for a writer_id that was never provisioned.
func TestWriterKey_UnknownID_ReturnsNotFound(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	_, err := store.WriterKey(ctx, "nonexistent-writer")
	if err == nil {
		t.Fatal("WriterKey for unknown ID: expected ErrWriterKeyNotFound, got nil")
	}
	if err != centralstore.ErrWriterKeyNotFound {
		t.Errorf("WriterKey for unknown ID: got %v, want ErrWriterKeyNotFound", err)
	}
}

// TestWriterKey_Rotate verifies that a second UpsertWriterKey call with a new
// secret replaces the old one: WriterKey returns the NEW secret.
func TestWriterKey_Rotate(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	oldSecret := []byte("old-hmac-key-exactly-32-bytes!!!")
	newSecret := []byte("new-hmac-key-exactly-32-bytes!!!")

	if err := store.UpsertWriterKey(ctx, "writer-rotate", oldSecret); err != nil {
		t.Fatalf("UpsertWriterKey (initial): %v", err)
	}

	// Rotate: upsert again with a different secret.
	if err := store.UpsertWriterKey(ctx, "writer-rotate", newSecret); err != nil {
		t.Fatalf("UpsertWriterKey (rotate): %v", err)
	}

	got, err := store.WriterKey(ctx, "writer-rotate")
	if err != nil {
		t.Fatalf("WriterKey after rotate: %v", err)
	}
	if string(got) != string(newSecret) {
		t.Errorf("WriterKey after rotate: got %q, want %q", got, newSecret)
	}
	if string(got) == string(oldSecret) {
		t.Error("WriterKey after rotate: returned old secret (rotation did not apply)")
	}
}

// TestWriterKey_DeactivateRevokesKey verifies that after DeactivateWriterKey,
// WriterKey returns ErrWriterKeyNotFound for that writer_id.
func TestWriterKey_DeactivateRevokesKey(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	secret := []byte("revocable-hmac-key-32-bytes!!!!!")
	if err := store.UpsertWriterKey(ctx, "writer-revoke", secret); err != nil {
		t.Fatalf("UpsertWriterKey: %v", err)
	}

	// Confirm it is retrievable before revocation.
	if _, err := store.WriterKey(ctx, "writer-revoke"); err != nil {
		t.Fatalf("WriterKey before deactivate: %v", err)
	}

	if err := store.DeactivateWriterKey(ctx, "writer-revoke"); err != nil {
		t.Fatalf("DeactivateWriterKey: %v", err)
	}

	_, err := store.WriterKey(ctx, "writer-revoke")
	if err == nil {
		t.Fatal("WriterKey after deactivate: expected ErrWriterKeyNotFound, got nil")
	}
	if err != centralstore.ErrWriterKeyNotFound {
		t.Errorf("WriterKey after deactivate: got %v, want ErrWriterKeyNotFound", err)
	}
}

// TestWriterKey_UpsertReactivatesAfterDeactivate verifies that calling
// UpsertWriterKey on a deactivated writer re-activates it and installs the new
// secret. This mirrors the "rotate after revoke" scenario.
func TestWriterKey_UpsertReactivatesAfterDeactivate(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	secret1 := []byte("first-secret-hmac-key-32-bytes!!")
	secret2 := []byte("second-secret-hmac-key-32-bytes!")

	if err := store.UpsertWriterKey(ctx, "writer-reactivate", secret1); err != nil {
		t.Fatalf("UpsertWriterKey (initial): %v", err)
	}
	if err := store.DeactivateWriterKey(ctx, "writer-reactivate"); err != nil {
		t.Fatalf("DeactivateWriterKey: %v", err)
	}

	// Re-provision with a new secret — must re-activate.
	if err := store.UpsertWriterKey(ctx, "writer-reactivate", secret2); err != nil {
		t.Fatalf("UpsertWriterKey (reactivate): %v", err)
	}

	got, err := store.WriterKey(ctx, "writer-reactivate")
	if err != nil {
		t.Fatalf("WriterKey after reactivate: %v", err)
	}
	if string(got) != string(secret2) {
		t.Errorf("WriterKey after reactivate: got %q, want %q", got, secret2)
	}
}
