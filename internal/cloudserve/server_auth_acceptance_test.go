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
