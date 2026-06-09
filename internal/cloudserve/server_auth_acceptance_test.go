//go:build acceptance

// Auth acceptance tests for cloudserve.
//
// These tests use a real centralstore (via embedded-postgres) and
// cloudserve.NewKeyVerifier(store.WriterKey) to exercise the full HMAC
// verification path: key provisioned in cloud_writer_keys → server looks it up
// on every request → verifies the HMAC signature.
//
// MANUAL signing via wireauth.Sign is used here — do NOT use remote.Client
// (that is PR6b-2, the client signing integration). The HTTP requests are
// constructed by hand so we can exercise every error path independently.
package cloudserve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mariesqu/engram/internal/centralstore"
	"github.com/mariesqu/engram/internal/cloudserve"
	"github.com/mariesqu/engram/internal/domain"
	"github.com/mariesqu/engram/internal/mutation"
	"github.com/mariesqu/engram/internal/syncwire"
	"github.com/mariesqu/engram/internal/wireauth"
)

// ── Helpers specific to auth acceptance tests ─────────────────────────────────

// newAuthServer builds a cloudserve httptest.Server backed by a real store and
// NewKeyVerifier(store.WriterKey) — the production key-lookup path.
func newAuthServer(t *testing.T, store *centralstore.Store) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(cloudserve.New(store, cloudserve.NewKeyVerifier(store.WriterKey)).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// signedPushRequest builds a PushRequest body for the given mutation (with
// Payload + MutationID derived) and signs it with key for (method, path).
// Returns the body bytes and the hex signature string.
func signedPushRequest(t *testing.T, m domain.Mutation, key []byte, method, path string) (body []byte, sig string) {
	t.Helper()
	payload := mutation.CanonicalPayload(m)
	m.Payload = payload
	m.MutationID = mutation.NewMutationID(payload)
	wire := syncwire.ToWire(m)
	req := syncwire.PushRequest{Mutation: wire}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("signedPushRequest: marshal: %v", err)
	}
	return b, wireauth.Sign(key, method, path, b)
}

// doAuthPost sends a POST with the two HMAC headers set.
func doAuthPost(t *testing.T, url, writerID, sig string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("doAuthPost: new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(wireauth.HeaderWriterID, writerID)
	req.Header.Set(wireauth.HeaderSignature, sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("doAuthPost: %v", err)
	}
	return resp
}

// baseMutation returns a template domain.Mutation with valid fields. Callers
// override fields as needed.
func baseMutation(writerID, syncID, project string) domain.Mutation {
	return domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess-auth-1",
		EntityType: domain.EntityMemory,
		Type:       "manual",
		Title:      "Auth acceptance test",
		Content:    "auth content",
		Project:    project,
		Scope:      "project",
		Version:    1,
		WriterID:   writerID,
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
	}
}

// ── Auth acceptance tests ─────────────────────────────────────────────────────

// TestAuthAcceptance_SignedPush_Returns200 is the happy-path proof:
// provision a key, build a push body, sign it correctly → 200 and the mutation
// lands in the store.
func TestAuthAcceptance_SignedPush_Returns200(t *testing.T) {
	store := newIsolatedStore(t)
	srv := newAuthServer(t, store)

	writerID := "writer-auth-push"
	key, err := wireauth.NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	if err := store.UpsertWriterKey(context.Background(), writerID, key); err != nil {
		t.Fatalf("UpsertWriterKey: %v", err)
	}

	m := baseMutation(writerID, "sync-auth-push-1", "auth-project-push")
	topicKey := "auth/push/happy"
	m.TopicKey = &topicKey

	body, sig := signedPushRequest(t, m, key, http.MethodPost, "/v1/push")
	resp := doAuthPost(t, srv.URL+"/v1/push", writerID, sig, body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signed push: status = %d, want 200", resp.StatusCode)
	}

	// Verify the mutation actually landed in central.
	rec, err := store.FindByTopic(topicKey, "auth-project-push", "project")
	if err != nil {
		t.Fatalf("FindByTopic: %v", err)
	}
	if rec == nil {
		t.Fatal("FindByTopic returned nil — mutation did not land in central")
	}
	if rec.SyncID != "sync-auth-push-1" {
		t.Errorf("SyncID = %q, want %q", rec.SyncID, "sync-auth-push-1")
	}
}

// TestAuthAcceptance_WrongSignature_Returns401 proves that a valid push body
// signed with the WRONG key (different from the provisioned one) is rejected.
func TestAuthAcceptance_WrongSignature_Returns401(t *testing.T) {
	store := newIsolatedStore(t)
	srv := newAuthServer(t, store)

	writerID := "writer-auth-badsig"
	goodKey, err := wireauth.NewKey()
	if err != nil {
		t.Fatalf("NewKey goodKey: %v", err)
	}
	badKey, err := wireauth.NewKey()
	if err != nil {
		t.Fatalf("NewKey badKey: %v", err)
	}
	if err := store.UpsertWriterKey(context.Background(), writerID, goodKey); err != nil {
		t.Fatalf("UpsertWriterKey: %v", err)
	}

	m := baseMutation(writerID, "sync-auth-badsig-1", "auth-project-badsig")
	// Sign with badKey — server has goodKey.
	body, sig := signedPushRequest(t, m, badKey, http.MethodPost, "/v1/push")
	resp := doAuthPost(t, srv.URL+"/v1/push", writerID, sig, body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong sig: status = %d, want 401", resp.StatusCode)
	}
}

// TestAuthAcceptance_UnknownWriterID_Returns401 proves that a request with a
// writer_id that has no key in cloud_writer_keys is rejected.
func TestAuthAcceptance_UnknownWriterID_Returns401(t *testing.T) {
	store := newIsolatedStore(t)
	srv := newAuthServer(t, store)

	// No key provisioned for this writer.
	writerID := "writer-auth-unknown"
	key, err := wireauth.NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}

	m := baseMutation(writerID, "sync-auth-unknown-1", "auth-project-unknown")
	body, sig := signedPushRequest(t, m, key, http.MethodPost, "/v1/push")
	resp := doAuthPost(t, srv.URL+"/v1/push", writerID, sig, body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown writer: status = %d, want 401", resp.StatusCode)
	}
}

// TestAuthAcceptance_MissingHeaders_Returns401 proves that a request with no
// X-Writer-Id / X-Signature headers is rejected when real auth is in use.
func TestAuthAcceptance_MissingHeaders_Returns401(t *testing.T) {
	store := newIsolatedStore(t)
	srv := newAuthServer(t, store)

	m := baseMutation("writer-missing", "sync-auth-missing-1", "auth-project-missing")
	body, _ := signedPushRequest(t, m, []byte("unused"), http.MethodPost, "/v1/push")

	// Send with NO auth headers.
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/push", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/push: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing headers: status = %d, want 401", resp.StatusCode)
	}
}

// TestAuthAcceptance_WriterIDForgery_Returns403 proves the push forgery check:
// writer-A signs the request (valid HMAC), but the mutation body claims
// WriterID = writer-B. The server authenticates writer-A successfully (key
// lookup + HMAC passes) but then finds the mutation's WriterID does not match
// → 403.
func TestAuthAcceptance_WriterIDForgery_Returns403(t *testing.T) {
	store := newIsolatedStore(t)
	srv := newAuthServer(t, store)
	ctx := context.Background()

	writerA := "writer-auth-forger-a"
	writerB := "writer-auth-forger-b"

	keyA, err := wireauth.NewKey()
	if err != nil {
		t.Fatalf("NewKey A: %v", err)
	}
	keyB, err := wireauth.NewKey()
	if err != nil {
		t.Fatalf("NewKey B: %v", err)
	}
	if err := store.UpsertWriterKey(ctx, writerA, keyA); err != nil {
		t.Fatalf("UpsertWriterKey A: %v", err)
	}
	if err := store.UpsertWriterKey(ctx, writerB, keyB); err != nil {
		t.Fatalf("UpsertWriterKey B: %v", err)
	}

	// Build a mutation with writerB's ID (the victim), but sign it as writerA.
	m := baseMutation(writerB, "sync-auth-forgery-1", "auth-project-forgery")
	body, sig := signedPushRequest(t, m, keyA, http.MethodPost, "/v1/push")

	// Send: X-Writer-Id = writerA (HMAC verifies), mutation.WriterID = writerB.
	resp := doAuthPost(t, srv.URL+"/v1/push", writerA, sig, body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("forgery: status = %d, want 403", resp.StatusCode)
	}
}

// promptMutation returns a template domain.Mutation for EntityType=EntityPrompt.
// Unlike baseMutation (EntityMemory), prompts have no topic_key — they are
// keyed solely by sync_id in central_user_prompts.
func promptMutation(writerID, syncID, project string) domain.Mutation {
	return domain.Mutation{
		Op:         domain.OpUpsert,
		SyncID:     syncID,
		SessionID:  "sess-prompt-auth-1",
		EntityType: domain.EntityPrompt,
		Content:    "prompt auth test content",
		Project:    project,
		Scope:      "project",
		Version:    1,
		WriterID:   writerID,
		UpdatedAt:  time.Now().UTC(),
		OccurredAt: time.Now().UTC(),
	}
}

// assertCentralPromptRow checks that central_user_prompts contains exactly one
// row for syncID with the expected writer_id and content.
func assertCentralPromptRow(t *testing.T, store *centralstore.Store, syncID, wantWriter, wantContent string) {
	t.Helper()
	var gotWriter, gotContent string
	err := store.Pool().QueryRow(
		context.Background(),
		`SELECT writer_id, content FROM central_user_prompts WHERE sync_id = $1`,
		syncID,
	).Scan(&gotWriter, &gotContent)
	if err != nil {
		t.Fatalf("assertCentralPromptRow(%q): query: %v", syncID, err)
	}
	if gotWriter != wantWriter {
		t.Errorf("central_user_prompts writer_id = %q, want %q", gotWriter, wantWriter)
	}
	if gotContent != wantContent {
		t.Errorf("central_user_prompts content = %q, want %q", gotContent, wantContent)
	}
}

// assertCentralPromptAbsent checks that central_user_prompts has no row for syncID.
func assertCentralPromptAbsent(t *testing.T, store *centralstore.Store, syncID string) {
	t.Helper()
	var n int
	if err := store.Pool().QueryRow(
		context.Background(),
		`SELECT count(*) FROM central_user_prompts WHERE sync_id = $1`,
		syncID,
	).Scan(&n); err != nil {
		t.Fatalf("assertCentralPromptAbsent(%q): query: %v", syncID, err)
	}
	if n != 0 {
		t.Errorf("central_user_prompts: unexpected row for sync_id=%q (want absent)", syncID)
	}
}

// TestAuthAcceptance_PromptPush_ValidWriter_Returns200 is the happy-path
// end-to-end proof for EntityPrompt over the real HMAC transport:
//
//  1. Set up a cloudserve server with cloudserve.NewKeyVerifier (the production
//     key-lookup path — NOT AllowAllVerifier).
//  2. Provision writer-A's key in cloud_writer_keys.
//  3. Build a prompt domain.Mutation (EntityType=EntityPrompt, WriterID="writer-A",
//     a sync_id, content, project) normalized so MutationID is content-addressed.
//  4. Push it via a manually-signed HTTP request (X-Writer-Id=writer-A,
//     X-Signature=HMAC(keyA, ...)).
//  5. Assert: HTTP 200 AND the prompt is materialized in central_user_prompts with
//     the correct sync_id, writer_id, and content.
//
// This proves that the entity-agnostic forgery check LETS a correctly-signed
// prompt through end-to-end — the check has no entity-type branch that would
// accidentally reject prompts.
func TestAuthAcceptance_PromptPush_ValidWriter_Returns200(t *testing.T) {
	store := newIsolatedStore(t)
	srv := newAuthServer(t, store)
	ctx := context.Background()

	writerID := "writer-prompt-valid"
	key, err := wireauth.NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	if err := store.UpsertWriterKey(ctx, writerID, key); err != nil {
		t.Fatalf("UpsertWriterKey: %v", err)
	}

	const (
		syncID  = "sync-prompt-auth-valid-1"
		project = "prompt-auth-project"
	)
	m := promptMutation(writerID, syncID, project)
	// signedPushRequest derives the canonical Payload + content-addressed MutationID
	// (same as localWriteLocked) and signs the body — no pre-normalization needed.
	body, sig := signedPushRequest(t, m, key, http.MethodPost, "/v1/push")
	resp := doAuthPost(t, srv.URL+"/v1/push", writerID, sig, body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid prompt push: status = %d, want 200", resp.StatusCode)
	}

	// The prompt must be materialized in central_user_prompts — proving the
	// entity-agnostic forgery check (which only compares writer IDs, not entity
	// types) lets the correctly-signed prompt through to central.Apply, which then
	// routes it to applyPromptDecisionQ → applyPromptUpsertQ.
	assertCentralPromptRow(t, store, syncID, writerID, "prompt auth test content")
}

// TestAuthAcceptance_PromptPush_ForgeryRejected_Returns403 is the forgery proof
// for EntityPrompt:
//
//  1. Provision writer-A and writer-B, each with their own key in cloud_writer_keys.
//  2. Build a prompt mutation with m.WriterID = "writer-B" (the victim identity).
//  3. Sign the request as writer-A (X-Writer-Id = writer-A, HMAC with keyA).
//  4. Send: the server authenticates writer-A (keyA HMAC passes), but the
//     mutation body claims WriterID = writer-B → forgery check fires → 403.
//  5. Assert: HTTP 403 AND nothing materialized in central_user_prompts.
//
// This proves that the forgery check in handlePush (authWriterID != "" &&
// m.WriterID != authWriterID) is ENTITY-AGNOSTIC: it covers EntityPrompt
// exactly the same way it covers EntityMemory.
func TestAuthAcceptance_PromptPush_ForgeryRejected_Returns403(t *testing.T) {
	store := newIsolatedStore(t)
	srv := newAuthServer(t, store)
	ctx := context.Background()

	writerA := "writer-prompt-forger-a"
	writerB := "writer-prompt-forger-b"

	keyA, err := wireauth.NewKey()
	if err != nil {
		t.Fatalf("NewKey A: %v", err)
	}
	keyB, err := wireauth.NewKey()
	if err != nil {
		t.Fatalf("NewKey B: %v", err)
	}
	if err := store.UpsertWriterKey(ctx, writerA, keyA); err != nil {
		t.Fatalf("UpsertWriterKey A: %v", err)
	}
	if err := store.UpsertWriterKey(ctx, writerB, keyB); err != nil {
		t.Fatalf("UpsertWriterKey B: %v", err)
	}

	const (
		syncID  = "sync-prompt-auth-forged-1"
		project = "prompt-forgery-project"
	)

	// Build the mutation claiming writerB's identity — this is the forged body.
	// signedPushRequest derives Payload + MutationID from m (WriterID=writerB), so the
	// body's MutationID is over writerB and VerifyMutationID passes on the server —
	// the ONLY rejection is the writer_id forgery check (authWriterID=writerA != writerB).
	m := promptMutation(writerB, syncID, project)

	// Sign with keyA — so X-Writer-Id=writerA, HMAC(keyA) passes verification.
	// The mismatch (authWriterID=writerA, m.WriterID=writerB) is the forgery.
	body, sig := signedPushRequest(t, m, keyA, http.MethodPost, "/v1/push")

	// Send: X-Writer-Id=writerA (HMAC passes), mutation.WriterID=writerB (forgery).
	resp := doAuthPost(t, srv.URL+"/v1/push", writerA, sig, body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("forged prompt push: status = %d, want 403", resp.StatusCode)
	}

	// Nothing must have materialized — the forgery check fires BEFORE central.Apply,
	// so neither the journal (central_mutations) nor the row (central_user_prompts) is
	// written. Asserting both makes the boundary tight from both sides.
	assertCentralPromptAbsent(t, store, syncID)
	var nMut int
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM central_mutations WHERE entity_key = $1`, syncID,
	).Scan(&nMut); err != nil {
		t.Fatalf("count central_mutations: %v", err)
	}
	if nMut != 0 {
		t.Errorf("central_mutations: unexpected journal row for forged sync_id=%q (want absent)", syncID)
	}
}

// TestAuthAcceptance_SignedPull_Returns200 proves that a correctly-signed pull
// request is accepted and returns mutations.
func TestAuthAcceptance_SignedPull_Returns200(t *testing.T) {
	store := newIsolatedStore(t)
	ctx := context.Background()

	writerID := "writer-auth-pull"
	key, err := wireauth.NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	if err := store.UpsertWriterKey(ctx, writerID, key); err != nil {
		t.Fatalf("UpsertWriterKey: %v", err)
	}

	// Push one mutation without auth (via AllowAll server) so there is something to pull.
	setupSrv := httptest.NewServer(cloudserve.New(store, cloudserve.AllowAllVerifier()).Handler())
	t.Cleanup(setupSrv.Close)

	m := baseMutation(writerID, "sync-auth-pull-1", "pull-project")
	topicKey := "auth/pull/topic-1"
	m.TopicKey = &topicKey
	setupBody, _ := buildPushBody(t, m)
	setupResp, err := http.Post(setupSrv.URL+"/v1/push", "application/json", bytes.NewReader(setupBody))
	if err != nil {
		t.Fatalf("setup push: %v", err)
	}
	setupResp.Body.Close()
	if setupResp.StatusCode != http.StatusOK {
		t.Fatalf("setup push status = %d, want 200", setupResp.StatusCode)
	}

	// Now use the auth-enabled server to pull.
	authSrv := newAuthServer(t, store)

	pullReqBody, err := json.Marshal(syncwire.PullRequest{Project: "pull-project", SinceSeq: 0})
	if err != nil {
		t.Fatalf("marshal pull: %v", err)
	}
	pullSig := wireauth.Sign(key, http.MethodPost, "/v1/pull", pullReqBody)
	resp := doAuthPost(t, authSrv.URL+"/v1/pull", writerID, pullSig, pullReqBody)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signed pull: status = %d, want 200", resp.StatusCode)
	}

	var pr syncwire.PullResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode PullResponse: %v", err)
	}
	if len(pr.Mutations) == 0 {
		t.Error("pull returned no mutations — expected the pushed mutation")
	}
}
