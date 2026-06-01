package domain

// mockReader is an in-process map-backed implementation of Reader.
// It is used exclusively in unit tests for Decide() — no database required.
type mockReader struct {
	// byTopic maps "topicKey|project|scope" -> *Record (live records only).
	byTopic map[string]*Record
	// bySyncID maps sync_id -> *Record.
	bySyncID map[string]*Record
	// tombstones maps "syncID" or "topicKey|project|scope" -> *Tombstone.
	tombstones map[string]*Tombstone
	// applied is the set of mutation IDs already applied (INV5).
	applied map[string]bool
}

func newMockReader() *mockReader {
	return &mockReader{
		byTopic:    make(map[string]*Record),
		bySyncID:   make(map[string]*Record),
		tombstones: make(map[string]*Tombstone),
		applied:    make(map[string]bool),
	}
}

// topicKey builds the composite key used in byTopic.
func topicKey(tk, project, scope string) string {
	return tk + "|" + project + "|" + scope
}

// seedRecord inserts r into both lookup maps (simulates an existing stored row).
func (m *mockReader) seedRecord(r *Record) {
	m.bySyncID[r.SyncID] = r
	if r.TopicKey != nil && *r.TopicKey != "" {
		m.byTopic[topicKey(*r.TopicKey, r.Project, r.Scope)] = r
	}
}

// seedTombstone inserts ts into the tombstone map.
func (m *mockReader) seedTombstone(ts *Tombstone) {
	// Index by sync_id (primary) and topic key if present.
	m.tombstones[ts.SyncID] = ts
	if ts.TopicKey != nil && *ts.TopicKey != "" {
		m.tombstones[topicKey(*ts.TopicKey, ts.Project, ts.Scope)] = ts
	}
}

// markApplied records a mutation ID as already applied.
func (m *mockReader) markApplied(mutationID string) {
	m.applied[mutationID] = true
}

// --- domain.Reader implementation ---

func (m *mockReader) FindByTopic(tk, project, scope string) (*Record, error) {
	r := m.byTopic[topicKey(tk, project, scope)]
	return r, nil
}

func (m *mockReader) FindBySyncID(syncID string) (*Record, error) {
	r := m.bySyncID[syncID]
	return r, nil
}

func (m *mockReader) FindTombstone(syncID string, tk *string, project, scope string) (*Tombstone, error) {
	// Check by sync_id first.
	if ts, ok := m.tombstones[syncID]; ok {
		return ts, nil
	}
	// Fall back to topic key composite.
	if tk != nil && *tk != "" {
		if ts, ok := m.tombstones[topicKey(*tk, project, scope)]; ok {
			return ts, nil
		}
	}
	return nil, nil
}

func (m *mockReader) MutationApplied(mutationID string) (bool, error) {
	return m.applied[mutationID], nil
}
