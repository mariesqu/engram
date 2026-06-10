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
	failOn   int     // if >0, Embed call number N (1-based) and later return failErr
	failErr  error
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

	// Deterministic failure injection: the Nth call (1-based) and later fail.
	// Recorded BEFORE failing so callCount reflects the attempt.
	if m.failOn > 0 && len(m.Calls) >= m.failOn {
		return nil, m.failErr
	}

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

// failOnCall configures the mock to return err on the Nth Embed call (1-based)
// and every call after it. Set BEFORE the loop starts — deterministic, no
// goroutine, no race (the round-1 test injected the error from a time.Tick
// goroutine racing the loop, which both leaked the ticker and could fire
// before/after the intended call on a loaded runner).
func (m *recordingMockProvider) failOnCall(n int, err error) {
	m.mu.Lock()
	m.failOn = n
	m.failErr = err
	m.mu.Unlock()
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

// receivedTexts returns a copy of every Embed call's texts.
func (m *recordingMockProvider) receivedTexts() [][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]string, len(m.Calls))
	for i, c := range m.Calls {
		cp := make([]string, len(c))
		copy(cp, c)
		out[i] = cp
	}
	return out
}
