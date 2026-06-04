package syncwire_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/mutation"
	"github.com/mariesqu/engram/internal/syncwire"
)

// mustPtr returns a pointer to s — keeps table literals concise.
func mustPtr(s string) *string { return &s }

// mustEmptyPtr returns a pointer to an empty string — distinct from nil.
func mustEmptyPtr() *string { s := ""; return &s }

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// makeMutation builds a fully populated domain.Mutation and derives its
// Payload + MutationID so every field is ready for wire round-trip.
func makeMutation(t *testing.T, overrides func(*domain.Mutation)) domain.Mutation {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Nanosecond)
	topicKey := "test/topic"
	status := "active"
	parentSyncID := "parent-sync-abc"
	m := domain.Mutation{
		Op:           domain.OpUpsert,
		SyncID:       "sync-abc-123",
		SessionID:    "sess-xyz",
		EntityType:   domain.EntityMemory,
		Type:         "note",
		Title:        "Test Title",
		Content:      "Test content body",
		Project:      "my-project",
		Scope:        "personal",
		TopicKey:     &topicKey,
		Status:       &status,
		ParentSyncID: &parentSyncID,
		Version:      3,
		UpdatedAt:    now,
		WriterID:     "writer-001",
		OccurredAt:   now.Add(time.Second),
	}
	if overrides != nil {
		overrides(&m)
	}
	// Derive payload and MutationID as localstore.normalizeMutation does.
	m.Payload = mutation.CanonicalPayload(m)
	m.MutationID = mutation.NewMutationID(m.Payload)
	return m
}

// assertMutationsEqual compares every field that must survive a wire round-trip.
// Times are compared with .Equal() to be timezone-agnostic.
func assertMutationsEqual(t *testing.T, label string, want, got domain.Mutation) {
	t.Helper()
	fail := func(field, w, g string) {
		t.Errorf("%s: field %s: want %q got %q", label, field, w, g)
	}
	if want.MutationID != got.MutationID {
		fail("MutationID", want.MutationID, got.MutationID)
	}
	if want.Op != got.Op {
		fail("Op", string(want.Op), string(got.Op))
	}
	if want.SyncID != got.SyncID {
		fail("SyncID", want.SyncID, got.SyncID)
	}
	if want.SessionID != got.SessionID {
		fail("SessionID", want.SessionID, got.SessionID)
	}
	if want.EntityType != got.EntityType {
		fail("EntityType", string(want.EntityType), string(got.EntityType))
	}
	if want.Type != got.Type {
		fail("Type", want.Type, got.Type)
	}
	if want.Title != got.Title {
		fail("Title", want.Title, got.Title)
	}
	if want.Content != got.Content {
		fail("Content", want.Content, got.Content)
	}
	if want.Project != got.Project {
		fail("Project", want.Project, got.Project)
	}
	if want.Scope != got.Scope {
		fail("Scope", want.Scope, got.Scope)
	}
	// TopicKey: compare nil-ness and value separately for a clear error message.
	if (want.TopicKey == nil) != (got.TopicKey == nil) {
		t.Errorf("%s: field TopicKey nil-ness: want nil=%v got nil=%v", label, want.TopicKey == nil, got.TopicKey == nil)
	} else if want.TopicKey != nil && *want.TopicKey != *got.TopicKey {
		fail("TopicKey value", *want.TopicKey, *got.TopicKey)
	}
	// Status
	if (want.Status == nil) != (got.Status == nil) {
		t.Errorf("%s: field Status nil-ness: want nil=%v got nil=%v", label, want.Status == nil, got.Status == nil)
	} else if want.Status != nil && *want.Status != *got.Status {
		fail("Status value", *want.Status, *got.Status)
	}
	// ParentSyncID
	if (want.ParentSyncID == nil) != (got.ParentSyncID == nil) {
		t.Errorf("%s: field ParentSyncID nil-ness: want nil=%v got nil=%v", label, want.ParentSyncID == nil, got.ParentSyncID == nil)
	} else if want.ParentSyncID != nil && *want.ParentSyncID != *got.ParentSyncID {
		fail("ParentSyncID value", *want.ParentSyncID, *got.ParentSyncID)
	}
	if want.Version != got.Version {
		t.Errorf("%s: field Version: want %d got %d", label, want.Version, got.Version)
	}
	if want.WriterID != got.WriterID {
		fail("WriterID", want.WriterID, got.WriterID)
	}
	// UpdatedAt is INSIDE the canonical payload — must survive exactly.
	if !want.UpdatedAt.Equal(got.UpdatedAt) {
		t.Errorf("%s: field UpdatedAt: want %v got %v", label, want.UpdatedAt, got.UpdatedAt)
	}
	// OccurredAt is OUTSIDE the payload (a sibling field) — must survive exactly.
	if !want.OccurredAt.Equal(got.OccurredAt) {
		t.Errorf("%s: field OccurredAt: want %v got %v", label, want.OccurredAt, got.OccurredAt)
	}
	// Seq is OUTSIDE the payload — must survive exactly.
	if want.Seq != got.Seq {
		t.Errorf("%s: field Seq: want %d got %d", label, want.Seq, got.Seq)
	}
	// Payload bytes must be identical (so mutation_id stays valid after round-trip).
	if string(want.Payload) != string(got.Payload) {
		t.Errorf("%s: field Payload: bytes differ\n  want: %s\n   got: %s", label, want.Payload, got.Payload)
	}
}

// ---------------------------------------------------------------------------
// Full round-trip equality tests
// ---------------------------------------------------------------------------

// TestRoundTrip is the primary correctness test. For each mutation shape it
// exercises: m → ToWire → json.Marshal → json.Unmarshal → FromWire → m'.
// Every domain.Mutation field that must survive the wire is asserted.
func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		override func(*domain.Mutation)
	}{
		{
			name: "upsert with topic",
			// default makeMutation already has a topic key — no overrides.
		},
		{
			name: "delete with topic",
			override: func(m *domain.Mutation) {
				m.Op = domain.OpDelete
				m.Content = ""
			},
		},
		{
			name: "upsert nil TopicKey",
			override: func(m *domain.Mutation) {
				m.TopicKey = nil
				m.Status = nil
				m.ParentSyncID = nil
			},
		},
		{
			name: "upsert empty-string TopicKey (&\"\")",
			// &"" is a distinct value from nil at the wire level. The wire must
			// preserve it as-is (no folding to nil) — domain.NormalizeTopicKey
			// is a store-entry concern, not a codec concern.
			override: func(m *domain.Mutation) {
				m.TopicKey = mustEmptyPtr()
			},
		},
		{
			name: "pulled mutation with Seq > 0",
			override: func(m *domain.Mutation) {
				m.Seq = 42
			},
		},
		{
			name: "all optional fields nil",
			override: func(m *domain.Mutation) {
				m.TopicKey = nil
				m.Status = nil
				m.ParentSyncID = nil
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			orig := makeMutation(t, tc.override)

			// Payload and MutationID must be re-derived after any override.
			orig.Payload = mutation.CanonicalPayload(orig)
			orig.MutationID = mutation.NewMutationID(orig.Payload)

			// Step 1: ToWire.
			w := syncwire.ToWire(orig)

			// Step 2: json.Marshal → json.Unmarshal (simulates network boundary).
			raw, err := json.Marshal(w)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			var w2 syncwire.WireMutation
			if err := json.Unmarshal(raw, &w2); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}

			// Step 3: FromWire.
			got, err := syncwire.FromWire(w2)
			if err != nil {
				t.Fatalf("FromWire: %v", err)
			}

			assertMutationsEqual(t, tc.name, orig, got)
		})
	}
}

// ---------------------------------------------------------------------------
// OccurredAt format tests
// ---------------------------------------------------------------------------

// TestOccurredAtFormat asserts that the wire string is RFC3339Nano UTC and
// survives a round-trip with nanosecond precision.
func TestOccurredAtFormat(t *testing.T) {
	// Use a timestamp with sub-second precision to catch truncation bugs.
	nanos := time.Date(2025, 6, 15, 12, 30, 45, 123456789, time.UTC)
	m := makeMutation(t, func(m *domain.Mutation) {
		m.OccurredAt = nanos
	})
	m.Payload = mutation.CanonicalPayload(m)
	m.MutationID = mutation.NewMutationID(m.Payload)

	w := syncwire.ToWire(m)

	// The wire string must parse as RFC3339Nano and equal the original instant.
	parsed, err := time.Parse(time.RFC3339Nano, w.OccurredAt)
	if err != nil {
		t.Fatalf("occurred_at wire string %q not valid RFC3339Nano: %v", w.OccurredAt, err)
	}
	if !parsed.Equal(nanos) {
		t.Errorf("occurred_at round-trip: want %v got %v", nanos, parsed)
	}

	// Must include explicit UTC "Z" suffix.
	if len(w.OccurredAt) == 0 || w.OccurredAt[len(w.OccurredAt)-1] != 'Z' {
		t.Errorf("occurred_at %q does not end with Z (not UTC)", w.OccurredAt)
	}
}

// TestFromWireEmptyOccurredAt asserts that FromWire rejects an empty occurred_at.
func TestFromWireEmptyOccurredAt(t *testing.T) {
	m := makeMutation(t, nil)
	w := syncwire.ToWire(m)
	w.OccurredAt = ""
	_, err := syncwire.FromWire(w)
	if err == nil {
		t.Error("expected error for empty occurred_at, got nil")
	}
}

// TestFromWireBadOccurredAt asserts that FromWire rejects a malformed occurred_at.
func TestFromWireBadOccurredAt(t *testing.T) {
	m := makeMutation(t, nil)
	w := syncwire.ToWire(m)
	w.OccurredAt = "not-a-timestamp"
	_, err := syncwire.FromWire(w)
	if err == nil {
		t.Error("expected error for bad occurred_at, got nil")
	}
}

// ---------------------------------------------------------------------------
// VerifyMutationID tests
// ---------------------------------------------------------------------------

func TestVerifyMutationID_Valid(t *testing.T) {
	m := makeMutation(t, nil)
	w := syncwire.ToWire(m)
	if err := syncwire.VerifyMutationID(w); err != nil {
		t.Errorf("expected valid WireMutation to pass VerifyMutationID, got: %v", err)
	}
}

func TestVerifyMutationID_TamperedPayload(t *testing.T) {
	m := makeMutation(t, nil)
	w := syncwire.ToWire(m)

	// Tamper with the payload bytes — change one character in the JSON.
	raw := []byte(w.Payload)
	for i, b := range raw {
		if b == '{' {
			raw[i] = '['
			break
		}
	}
	w.Payload = json.RawMessage(raw)

	err := syncwire.VerifyMutationID(w)
	if err == nil {
		t.Error("expected error for tampered payload, got nil")
	}
}

func TestVerifyMutationID_WrongMutationID(t *testing.T) {
	m := makeMutation(t, nil)
	w := syncwire.ToWire(m)

	// Keep the payload intact but claim a different mutation_id.
	w.MutationID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	err := syncwire.VerifyMutationID(w)
	if err == nil {
		t.Error("expected error for wrong mutation_id, got nil")
	}
}

func TestVerifyMutationID_EmptyPayload(t *testing.T) {
	w := syncwire.WireMutation{
		MutationID: "anything",
		OccurredAt: time.Now().UTC().Format(time.RFC3339Nano),
		Payload:    nil,
	}
	err := syncwire.VerifyMutationID(w)
	if err == nil {
		t.Error("expected error for empty payload, got nil")
	}
}

// ---------------------------------------------------------------------------
// Envelope round-trip tests
// ---------------------------------------------------------------------------

func TestPushRequestRoundTrip(t *testing.T) {
	m := makeMutation(t, nil)
	req := syncwire.PushRequest{Mutation: syncwire.ToWire(m)}

	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal PushRequest: %v", err)
	}
	var req2 syncwire.PushRequest
	if err := json.Unmarshal(raw, &req2); err != nil {
		t.Fatalf("unmarshal PushRequest: %v", err)
	}
	if req2.Mutation.MutationID != m.MutationID {
		t.Errorf("PushRequest round-trip: mutation_id mismatch: want %q got %q", m.MutationID, req2.Mutation.MutationID)
	}
}

func TestPushResponseRoundTrip(t *testing.T) {
	resp := syncwire.PushResponse{
		Status:     "ok",
		MutationID: "abc123",
		Applied:    true,
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal PushResponse: %v", err)
	}
	var resp2 syncwire.PushResponse
	if err := json.Unmarshal(raw, &resp2); err != nil {
		t.Fatalf("unmarshal PushResponse: %v", err)
	}
	if resp2.Status != resp.Status || resp2.MutationID != resp.MutationID || resp2.Applied != resp.Applied {
		t.Errorf("PushResponse round-trip mismatch: got %+v", resp2)
	}
}

func TestPullRequestRoundTrip(t *testing.T) {
	req := syncwire.PullRequest{
		Project:  "my-project",
		SinceSeq: 77,
		Limit:    50,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal PullRequest: %v", err)
	}
	var req2 syncwire.PullRequest
	if err := json.Unmarshal(raw, &req2); err != nil {
		t.Fatalf("unmarshal PullRequest: %v", err)
	}
	if req2.Project != req.Project || req2.SinceSeq != req.SinceSeq || req2.Limit != req.Limit {
		t.Errorf("PullRequest round-trip mismatch: got %+v", req2)
	}
}

func TestPullResponseRoundTrip(t *testing.T) {
	m1 := makeMutation(t, func(m *domain.Mutation) { m.Seq = 10 })
	m1.Payload = mutation.CanonicalPayload(m1)
	m1.MutationID = mutation.NewMutationID(m1.Payload)

	m2 := makeMutation(t, func(m *domain.Mutation) {
		m.SyncID = "sync-def-456"
		m.Seq = 20
		m.TopicKey = nil
	})
	m2.Payload = mutation.CanonicalPayload(m2)
	m2.MutationID = mutation.NewMutationID(m2.Payload)

	resp := syncwire.PullResponse{
		Mutations: []syncwire.WireMutation{
			syncwire.ToWire(m1),
			syncwire.ToWire(m2),
		},
	}

	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal PullResponse: %v", err)
	}
	var resp2 syncwire.PullResponse
	if err := json.Unmarshal(raw, &resp2); err != nil {
		t.Fatalf("unmarshal PullResponse: %v", err)
	}

	if len(resp2.Mutations) != 2 {
		t.Fatalf("PullResponse round-trip: want 2 mutations got %d", len(resp2.Mutations))
	}
	for i, wm := range resp2.Mutations {
		got, err := syncwire.FromWire(wm)
		if err != nil {
			t.Fatalf("PullResponse[%d] FromWire: %v", i, err)
		}
		want := []domain.Mutation{m1, m2}[i]
		assertMutationsEqual(t, "PullResponse["+string(rune('0'+i))+"]", want, got)
	}
}

// ---------------------------------------------------------------------------
// Seq omitempty test
// ---------------------------------------------------------------------------

// TestSeqOmittedOnPush asserts that seq is absent from the JSON when Seq==0
// (push direction), and present when Seq>0 (pull direction).
func TestSeqOmittedOnPush(t *testing.T) {
	m := makeMutation(t, func(m *domain.Mutation) { m.Seq = 0 })
	m.Payload = mutation.CanonicalPayload(m)
	m.MutationID = mutation.NewMutationID(m.Payload)

	w := syncwire.ToWire(m)
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if _, ok := obj["seq"]; ok {
		t.Error("seq should be absent (omitempty) when Seq==0")
	}
}

func TestSeqPresentOnPull(t *testing.T) {
	m := makeMutation(t, func(m *domain.Mutation) { m.Seq = 99 })
	m.Payload = mutation.CanonicalPayload(m)
	m.MutationID = mutation.NewMutationID(m.Payload)

	w := syncwire.ToWire(m)
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if _, ok := obj["seq"]; !ok {
		t.Error("seq should be present when Seq>0")
	}
}

// ---------------------------------------------------------------------------
// Nil vs &"" TopicKey distinction at the wire level
// ---------------------------------------------------------------------------

// TestTopicKeyNilVsEmptyWireDistinction verifies that nil TopicKey and &""
// TopicKey produce DIFFERENT wire payloads (they hash to different mutation_ids
// because canonicalFields encodes them without omitempty).
//
// Both must individually round-trip to their exact original value — the codec
// must NOT fold &"" → nil (that normalization is store-entry only).
func TestTopicKeyNilVsEmptyWireDistinction(t *testing.T) {
	// Build nil-topic mutation.
	mNil := makeMutation(t, func(m *domain.Mutation) { m.TopicKey = nil })
	mNil.Payload = mutation.CanonicalPayload(mNil)
	mNil.MutationID = mutation.NewMutationID(mNil.Payload)

	// Build &""-topic mutation (same other fields).
	mEmpty := makeMutation(t, func(m *domain.Mutation) { m.TopicKey = mustEmptyPtr() })
	mEmpty.Payload = mutation.CanonicalPayload(mEmpty)
	mEmpty.MutationID = mutation.NewMutationID(mEmpty.Payload)

	// They must have different MutationIDs (different payloads).
	if mNil.MutationID == mEmpty.MutationID {
		t.Error("nil TopicKey and &\"\" TopicKey must produce different MutationIDs")
	}

	// nil round-trip.
	wNil := syncwire.ToWire(mNil)
	rawNil, _ := json.Marshal(wNil)
	var wNil2 syncwire.WireMutation
	json.Unmarshal(rawNil, &wNil2)
	gotNil, err := syncwire.FromWire(wNil2)
	if err != nil {
		t.Fatalf("FromWire nil-topic: %v", err)
	}
	if gotNil.TopicKey != nil {
		t.Errorf("nil TopicKey round-trip: expected nil, got &%q", *gotNil.TopicKey)
	}

	// &"" round-trip.
	wEmpty := syncwire.ToWire(mEmpty)
	rawEmpty, _ := json.Marshal(wEmpty)
	var wEmpty2 syncwire.WireMutation
	json.Unmarshal(rawEmpty, &wEmpty2)
	gotEmpty, err := syncwire.FromWire(wEmpty2)
	if err != nil {
		t.Fatalf("FromWire empty-topic: %v", err)
	}
	if gotEmpty.TopicKey == nil {
		t.Error("&\"\" TopicKey round-trip: expected &\"\", got nil")
	} else if *gotEmpty.TopicKey != "" {
		t.Errorf("&\"\" TopicKey round-trip: expected &\"\", got &%q", *gotEmpty.TopicKey)
	}
}

// ---------------------------------------------------------------------------
// Unused import guard — mustPtr is used in overrides above; silence linter.
// ---------------------------------------------------------------------------

var _ = mustPtr
