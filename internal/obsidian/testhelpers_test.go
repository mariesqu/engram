package obsidian

import (
	"fmt"
	"strings"
	"time"

	"github.com/mariesqu/engram/internal/domain"
)

// mockStore is a minimal, hand-rolled fake satisfying StoreReader — no
// mocking framework exists in this repo (design constraint 6: stdlib
// testing only). byProject holds the records to return per project, keyed
// by exact project name; listProjects controls what ListProjects() returns.
type mockStore struct {
	listProjects []string
	byProject    map[string][]*domain.Record
	listErr      error
	recentObsErr error
}

func (m *mockStore) ListProjects() ([]string, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.listProjects, nil
}

// RecentObservations matches project CASE-INSENSITIVELY, aggregating across
// every byProject key that case-insensitively equals project — mirroring
// the real store's "LOWER(project) = ?" query (internal/localstore/search.go)
// where a single call can return rows whose OWN stored Project field carries
// whatever casing that particular row happens to have. This is what makes it
// possible to reproduce bugfix/listprojects-case-variant-double-read at the
// mock level: a fixture with records filed under both the "gitlab" and
// "GitLab" keys must return ALL of them regardless of which spelling is
// queried, exactly like the real database does.
func (m *mockStore) RecentObservations(project, scope string, limit int) ([]*domain.Record, error) {
	if m.recentObsErr != nil {
		return nil, m.recentObsErr
	}
	var recs []*domain.Record
	if project == "" {
		for _, rs := range m.byProject {
			recs = append(recs, rs...)
		}
	} else {
		for key, rs := range m.byProject {
			if strings.EqualFold(key, project) {
				recs = append(recs, rs...)
			}
		}
	}
	if limit > 0 && limit < len(recs) {
		recs = recs[:limit]
	}
	// Return a copy so callers mutating the slice (e.g. via append in the
	// caller) never corrupt the fixture between test cases/runs.
	out := make([]*domain.Record, len(recs))
	copy(out, recs)
	return out, nil
}

// newTestRecord builds a domain.Record with sensible test defaults —
// CreatedAt/UpdatedAt both set to ts — so incremental-filter tests can
// override just the timestamp per fixture. Fields are overridden by the
// caller after construction as needed.
func newTestRecord(id int64, project, typ, sessionID, content string, ts time.Time) *domain.Record {
	return &domain.Record{
		ID:        id,
		SyncID:    fmt.Sprintf("sync-%d", id),
		SessionID: sessionID,
		Type:      typ,
		Title:     content,
		Content:   content,
		Project:   project,
		Scope:     "project",
		CreatedAt: ts,
		UpdatedAt: ts,
	}
}
