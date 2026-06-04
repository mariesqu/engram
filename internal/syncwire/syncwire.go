// Package syncwire defines the JSON wire format for the engram push/pull
// transport. It is the shared contract between the cloud-serve server (PR3)
// and the HTTP client (PR4).
//
// Design note — payload encoding:
//
// WireMutation carries the canonical payload as raw JSON embedded directly in
// the outer JSON object (json.RawMessage). This preserves the exact bytes that
// NewMutationID(payload) hashes without any re-encoding step, so the
// mutation_id computed by the sender equals the one recomputed by
// VerifyMutationID on the receiver — no base64 inflation, no double-marshaling,
// no escaping surprises.
//
// Field split:
//
//   IN the canonical payload (reconstructed by mutation.FromCanonicalPayload):
//     Op, SyncID, SessionID, EntityType, Type, Title, Content, Project, Scope,
//     TopicKey, Status, ParentSyncID, Version, UpdatedAt, WriterID.
//
//   OUTSIDE the payload (siblings on the wire):
//     mutation_id  — SHA-256 of the payload bytes.
//     occurred_at  — RFC3339Nano UTC string; the SENDER's local write time
//                    (set by LocalWrite/normalizeMutation), not part of the
//                    payload. Required on the wire: ToWire always emits it and
//                    FromWire rejects an empty value.
//     seq          — central_mutations BIGSERIAL; 0 / omitted on push,
//                    positive on pull.
//     payload      — the raw canonical JSON bytes (embedded as a JSON sub-object).
package syncwire

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/mutation"
)

// WireMutation is the JSON DTO for one mutation on the push/pull wire.
//
// payload is a json.RawMessage so the canonical JSON bytes travel without any
// extra encoding layer (no base64, no double-escape). This is critical for
// mutation_id fidelity: the receiver recomputes NewMutationID(w.Payload) and
// compares it to w.MutationID — the bytes must be identical to the ones the
// sender hashed.
//
// occurred_at is an RFC3339Nano UTC string with a Z suffix — the canonical form
// ToWire emits (never empty after ToWire; a zero OccurredAt encodes as the Go
// zero instant "0001-01-01T00:00:00Z"). FromWire ENFORCES the UTC/Z form and
// rejects an explicit offset rather than silently normalizing it. Receivers that
// also require a non-zero OccurredAt should validate that separately.
//
// seq is 0 on push (client → server) and a positive central BIGSERIAL on pull
// (server → client). It is omitted from JSON when zero (omitempty).
type WireMutation struct {
	MutationID string          `json:"mutation_id"`
	OccurredAt string          `json:"occurred_at"`
	Seq        int64           `json:"seq,omitempty"`
	Payload    json.RawMessage `json:"payload"`
}

// ToWire converts a domain.Mutation into a WireMutation ready for JSON
// marshaling.
//
// If m.Payload is already set (e.g. reconstructed from the outbox by
// DrainOutbox) it is used; otherwise CanonicalPayload(m) derives the bytes. The
// payload bytes are COPIED into the WireMutation so it never aliases the
// caller's m.Payload (a later mutation of m.Payload must not change the DTO).
//
// mutation_id is content-addressed. When m.MutationID is empty (the caller did
// not run normalizeMutation), it is derived as NewMutationID(payload) so a
// WireMutation produced by ToWire ALWAYS passes VerifyMutationID. A non-empty
// m.MutationID is forwarded as-is (it is the content hash by construction).
// OccurredAt is the sender's local write time, formatted RFC3339Nano UTC.
func ToWire(m domain.Mutation) WireMutation {
	payload := m.Payload
	if len(payload) == 0 {
		payload = mutation.CanonicalPayload(m)
	}
	// Copy so the DTO never aliases the caller's m.Payload.
	payloadCopy := append([]byte(nil), payload...)

	mutationID := m.MutationID
	if mutationID == "" {
		mutationID = mutation.NewMutationID(payloadCopy)
	}
	return WireMutation{
		MutationID: mutationID,
		OccurredAt: m.OccurredAt.UTC().Format(time.RFC3339Nano),
		Seq:        m.Seq,
		Payload:    json.RawMessage(payloadCopy),
	}
}

// FromWire converts a WireMutation back into a domain.Mutation.
//
// It calls mutation.FromCanonicalPayload to reconstruct the content fields
// (Op, SyncID, EntityType, Type, Title, Content, Project, Scope, TopicKey,
// Status, ParentSyncID, Version, UpdatedAt, WriterID, SessionID), then fills
// in the sibling fields that live outside the payload:
//
//   - m.MutationID ← w.MutationID
//   - m.Payload    ← a COPY of the raw payload bytes (does not alias w.Payload)
//   - m.OccurredAt ← parsed from w.OccurredAt (RFC3339Nano UTC)
//   - m.Seq        ← w.Seq (0 on push; positive on pull)
//
// An error is returned when payload or mutation_id is empty, the payload is
// malformed, or occurred_at is empty, unparseable, or not UTC (Z suffix).
// FromWire does NOT verify mutation_id against the payload — that content-address
// integrity check is VerifyMutationID's job (the server calls it separately).
func FromWire(w WireMutation) (domain.Mutation, error) {
	if len(w.Payload) == 0 {
		return domain.Mutation{}, fmt.Errorf("syncwire.FromWire: payload is empty")
	}
	if w.MutationID == "" {
		return domain.Mutation{}, fmt.Errorf("syncwire.FromWire: mutation_id is empty")
	}

	m, err := mutation.FromCanonicalPayload([]byte(w.Payload))
	if err != nil {
		return domain.Mutation{}, fmt.Errorf("syncwire.FromWire: decode payload: %w", err)
	}

	// Parse occurred_at — it must be a valid RFC3339Nano timestamp in UTC (Z suffix).
	if w.OccurredAt == "" {
		return domain.Mutation{}, fmt.Errorf("syncwire.FromWire: occurred_at is empty")
	}
	occurredAt, err := time.Parse(time.RFC3339Nano, w.OccurredAt)
	if err != nil {
		return domain.Mutation{}, fmt.Errorf("syncwire.FromWire: parse occurred_at %q: %w", w.OccurredAt, err)
	}
	// Enforce the wire contract: occurred_at must be UTC with a Z suffix (what
	// ToWire emits). time.Parse maps a "Z" suffix to the time.UTC location; any
	// explicit offset (even +00:00) yields a fixed zone instead. Reject non-UTC
	// loudly rather than silently normalizing — that would hide a client that sent
	// the wrong zone.
	if occurredAt.Location() != time.UTC {
		return domain.Mutation{}, fmt.Errorf("syncwire.FromWire: occurred_at %q must be UTC RFC3339Nano with a Z suffix", w.OccurredAt)
	}

	m.MutationID = w.MutationID
	m.Payload = append([]byte(nil), w.Payload...) // copy: never alias w.Payload
	m.OccurredAt = occurredAt.UTC()
	m.Seq = w.Seq

	return m, nil
}

// VerifyMutationID checks that w.MutationID equals NewMutationID(w.Payload).
//
// The server (PR3) calls this on every push to reject tampered payloads before
// writing to central_mutations. Because mutation_id is content-addressed
// (SHA-256 of the canonical JSON), any byte-level change to the payload
// produces a different hash and this function returns an error.
//
// A WireMutation produced by ToWire always passes this check: ToWire preserves
// the exact canonical bytes and derives mutation_id from them when m.MutationID
// is unset, so the hash is always stable.
func VerifyMutationID(w WireMutation) error {
	if len(w.Payload) == 0 {
		return fmt.Errorf("syncwire.VerifyMutationID: payload is empty")
	}
	computed := mutation.NewMutationID([]byte(w.Payload))
	if computed != w.MutationID {
		return fmt.Errorf("syncwire.VerifyMutationID: mutation_id mismatch: got %q, payload hashes to %q", w.MutationID, computed)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Request / response envelopes
// ---------------------------------------------------------------------------

// PushRequest is the body of a POST /push request (client → server).
// One mutation per push, matching the spike's Push call pattern.
type PushRequest struct {
	Mutation WireMutation `json:"mutation"`
}

// PushResponse is the body returned by the server after a push.
//
//   - Status     — "ok" on success, "duplicate" when the mutation_id was already
//                  present (idempotent re-push), "rejected" for validation failure.
//   - MutationID — the mutation_id the server stored (echoes the request's).
//   - Applied    — true when the server ran domain.Decide and wrote the mutation;
//                  false when the mutation was a duplicate (already in
//                  applied_mutations) and was skipped.
type PushResponse struct {
	Status     string `json:"status"`
	MutationID string `json:"mutation_id"`
	Applied    bool   `json:"applied"`
}

// PullRequest is the query payload for a GET/POST /pull request
// (client → server). The server returns mutations with seq > SinceSeq for the
// given Project, up to Limit rows (0 means server default).
type PullRequest struct {
	Project  string `json:"project"`
	SinceSeq int64  `json:"since_seq"`
	Limit    int    `json:"limit,omitempty"`
}

// PullResponse is the body returned by the server for a pull request.
// Mutations are ordered by seq ASC; the client advances its pull cursor to
// the highest seq in the slice.
type PullResponse struct {
	Mutations []WireMutation `json:"mutations"`
}
