package controlapi_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mariesqu/engram/internal/controlapi"
)

// These tests cover GET /api/v1/sessions/{id}, the lookup a lifecycle hook
// falls back to when its payload carries no usable cwd. The hook's alternative
// is to drop the work it was given, so the three answers this endpoint can give
// — here it is, there is no such session, this store cannot answer — all have
// to be distinguishable.

// sessionStore is mockStore plus the optional SessionLookup capability.
type sessionStore struct {
	mockStore
	sessions map[string]controlapi.SessionRef
}

func (s *sessionStore) LookupSession(id string) (controlapi.SessionRef, error) {
	if ref, ok := s.sessions[id]; ok {
		return ref, nil
	}
	return controlapi.SessionRef{}, controlapi.ErrSessionNotFound
}

// TestSessionLookup_ReturnsTheRegisteredProject is the answer the hook needs:
// the project the session was registered under at session-start, when the host
// did supply a directory.
func TestSessionLookup_ReturnsTheRegisteredProject(t *testing.T) {
	store := &sessionStore{sessions: map[string]controlapi.SessionRef{
		"sess-1": {ID: "sess-1", Project: "engram", Directory: `C:\GitLab\engram`},
	}}
	_, ts := newTestServer(t, "tok", store, &mockSyncCtrl{}, &mockCfgStore{})

	resp := get(t, ts, "/api/v1/sessions/sess-1", authHeader("tok"))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got controlapi.SessionRef
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Project != "engram" || got.ID != "sess-1" {
		t.Errorf("session = %+v, want the registered id and project", got)
	}
}

// TestSessionLookup_UnknownIDIs404 — an id nothing registered must not read
// like an empty project, which a hook would then file work under.
func TestSessionLookup_UnknownIDIs404(t *testing.T) {
	_, ts := newTestServer(t, "tok", &sessionStore{}, &mockSyncCtrl{}, &mockCfgStore{})

	resp := get(t, ts, "/api/v1/sessions/nobody", authHeader("tok"))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	assertJSONContentType(t, resp)
}

// TestSessionLookup_StoreWithoutTheCapabilityIs501 pins the reason the lookup
// is an optional interface rather than a method on Store: Store is implemented
// in several places in this tree and only one of them has sessions at all. A
// store that cannot answer says so, rather than forcing every implementation to
// carry a method it has no rows for.
func TestSessionLookup_StoreWithoutTheCapabilityIs501(t *testing.T) {
	_, ts := newTestServer(t, "tok", &mockStore{}, &mockSyncCtrl{}, &mockCfgStore{})

	resp := get(t, ts, "/api/v1/sessions/sess-1", authHeader("tok"))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}

// TestSessionLookup_RequiresAuth — the control API is loopback-only and
// token-authenticated; a session's project is still somebody's data.
func TestSessionLookup_RequiresAuth(t *testing.T) {
	_, ts := newTestServer(t, "tok", &sessionStore{}, &mockSyncCtrl{}, &mockCfgStore{})

	resp := get(t, ts, "/api/v1/sessions/sess-1", "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}
