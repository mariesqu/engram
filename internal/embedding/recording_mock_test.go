package embedding_test

import (
	"context"
	"sync"
)

// recordingMockProvider is a test-only EmbeddingProvider that records every
// texts slice passed to Embed and returns configurable deterministic vectors.
//
// It satisfies EmbeddingProvider (via the embedding package's interface) and
// is used by gate privacy proofs to assert that certain projects never reach
// the provider. The design rule: the mock is wired as the 'inner' of NewGated;
// if the gate is correct, omitted/local-only texts never appear in Calls.
type recordingMockProvider struct {
	mu       sync.Mutex
	Calls    [][]string // all Embed calls, each entry is the texts slice
	dims     int
	vecValue float32 // value to fill returned vectors with (default 0.5)
	err      error   // if non-nil, returned from every Embed call
}

func newRecordingMock(dims int) *recordingMockProvider {
	return &recordingMockProvider{dims: dims, vecValue: 0.5}
}

func (m *recordingMockProvider) Embed(_ context.Context, _ string, texts []string) ([][]float32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.err != nil {
		return nil, m.err
	}

	// Record the texts slice (copy so caller mutations do not affect the record).
	cp := make([]string, len(texts))
	copy(cp, texts)
	m.Calls = append(m.Calls, cp)

	// Build deterministic vectors filled with m.vecValue.
	vecs := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, m.dims)
		for j := range v {
			v[j] = m.vecValue
		}
		vecs[i] = v
	}
	return vecs, nil
}

func (m *recordingMockProvider) Dimensions() int   { return m.dims }
func (m *recordingMockProvider) ModelName() string { return "mock" }

// totalTexts returns the flat count of all texts ever received across all calls.
func (m *recordingMockProvider) totalTexts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.Calls {
		n += len(c)
	}
	return n
}

// callCount returns the number of Embed invocations.
func (m *recordingMockProvider) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Calls)
}

// setError configures the mock to return err from all subsequent Embed calls.
func (m *recordingMockProvider) setError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// setVecValue changes the fill value for future Embed results.
func (m *recordingMockProvider) setVecValue(v float32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vecValue = v
}
