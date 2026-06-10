package main

// tools_similar_test.go — task 2.7 unit tests
//
// Tests for the mem_similar MCP tool handler:
//   - Returns nearest neighbours by cosine similarity.
//   - Source row excluded from results.
//   - No embedding provider (dims=0) → clear tool error.

import (
	"context"
	"encoding/binary"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mariesqu/engram/internal/embedding"
	"github.com/mariesqu/engram/internal/localstore"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// litVec encodes a float32 slice as a little-endian byte slice — mirrors
// localstore.encodeVector (unexported) so tests can write embedding BLOBs
// directly without going through the backfill loop.
func litVec(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// l2Norm returns the L2-normalised copy of v (same semantics as localstore.L2Normalize).
func l2Norm(v []float32) []float32 {
	return localstore.L2Normalize(v)
}

// insertSimilarMemory inserts a bare row into the store's raw DB.
func insertSimilarMemory(t *testing.T, components *daemonComponents, syncID, title, project string) {
	t.Helper()
	db := components.store.RawDB()
	if _, err := db.Exec(`
		INSERT INTO memories
		  (sync_id, session_id, entity_type, type, title, content, project, scope,
		   version, writer_id, last_write_mutation_id, created_at, updated_at)
		VALUES (?, 'sess1', 'memory', 'manual', ?, '', ?, 'project',
		        1, 'w1', 'mut-1', datetime('now'), datetime('now'))`,
		syncID, title, project,
	); err != nil {
		t.Fatalf("insertSimilarMemory %q: %v", syncID, err)
	}
}

// writeEmbedding writes a pre-built L2-normalised BLOB directly to the row.
func writeEmbedding(t *testing.T, components *daemonComponents, syncID string, vec []float32) {
	t.Helper()
	db := components.store.RawDB()
	blob := litVec(vec)
	if _, err := db.Exec(
		`UPDATE memories SET embedding=?, embedding_model='test', embedding_created_at='2025-01-01T00:00:00Z' WHERE sync_id=?`,
		blob, syncID,
	); err != nil {
		t.Fatalf("writeEmbedding %q: %v", syncID, err)
	}
}

// noopChecker satisfies embedding.PolicyChecker and allows all projects.
type noopChecker struct{}

func (noopChecker) GetPolicy(_ string) (localstore.Policy, error) {
	return localstore.PolicySynced, nil
}

// fixedDimProvider satisfies embedding.EmbeddingProvider with a configurable
// Dimensions() value. Embed is never called by mem_similar (it reads stored
// vectors, not re-embeds), but the interface requires the method.
type fixedDimProvider struct{ dims int }

func (f fixedDimProvider) Embed(_ context.Context, _ string, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, f.dims)
	}
	return out, nil
}
func (f fixedDimProvider) Dimensions() int   { return f.dims }
func (f fixedDimProvider) ModelName() string { return "mock" }

// ── tests ─────────────────────────────────────────────────────────────────────

// TestMemSimilar_ReturnsNeighbours verifies that mem_similar returns rows with
// high cosine similarity to the source and excludes rows with low similarity.
func TestMemSimilar_ReturnsNeighbours(t *testing.T) {
	components, err := buildDaemon(daemonCfg{
		db:           filepath.Join(t.TempDir(), "sim.db"),
		syncInterval: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	const dims = 4
	insertSimilarMemory(t, components, "src-1", "authentication design", "proj")
	insertSimilarMemory(t, components, "close-1", "auth session design", "proj")
	insertSimilarMemory(t, components, "far-1", "network infrastructure", "proj")

	writeEmbedding(t, components, "src-1", l2Norm([]float32{0.9, 0.1, 0.1, 0.1}))
	writeEmbedding(t, components, "close-1", l2Norm([]float32{0.85, 0.12, 0.09, 0.08}))
	writeEmbedding(t, components, "far-1", l2Norm([]float32{0.0, 0.0, 0.0, 1.0}))

	// Wire a gated provider with dims=4 so handleMemSimilar does not bail with
	// "no embedding provider configured".
	inner := fixedDimProvider{dims: dims}
	gated := embedding.NewGated(inner, noopChecker{}, false)

	tool := handleMemSimilar(components.store, gated)
	req := newToolRequest("mem_similar", map[string]any{
		"sync_id": "src-1",
		"limit":   float64(5),
	})

	result, err := tool(t.Context(), req)
	if err != nil {
		t.Fatalf("mem_similar transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("mem_similar tool error: %s", result.Content[0].(mcp.TextContent).Text)
	}

	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "close-1") {
		t.Errorf("expected close-1 in results; got:\n%s", text)
	}
}

// TestMemSimilar_ExcludesSourceRow verifies that the source sync_id is never
// included in its own similarity results.
func TestMemSimilar_ExcludesSourceRow(t *testing.T) {
	components, err := buildDaemon(daemonCfg{
		db:           filepath.Join(t.TempDir(), "excl.db"),
		syncInterval: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	const dims = 4
	insertSimilarMemory(t, components, "self-row", "self content", "proj")
	insertSimilarMemory(t, components, "other-row", "other content", "proj")

	vec := l2Norm([]float32{1, 0, 0, 0})
	writeEmbedding(t, components, "self-row", vec)
	writeEmbedding(t, components, "other-row", vec)

	inner := fixedDimProvider{dims: dims}
	gated := embedding.NewGated(inner, noopChecker{}, false)

	tool := handleMemSimilar(components.store, gated)
	req := newToolRequest("mem_similar", map[string]any{
		"sync_id": "self-row",
	})

	result, err := tool(t.Context(), req)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content[0].(mcp.TextContent).Text)
	}

	text := result.Content[0].(mcp.TextContent).Text
	if strings.Contains(text, "self-row") {
		t.Errorf("source row must be excluded from its own results; got:\n%s", text)
	}
}

// TestMemSimilar_NoDims_ToolError verifies that when gated.Dimensions()==0
// (Noop provider), mem_similar returns a clear tool error.
func TestMemSimilar_NoDims_ToolError(t *testing.T) {
	components, err := buildDaemon(daemonCfg{
		db:           filepath.Join(t.TempDir(), "nodims.db"),
		syncInterval: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	// NoopProvider has Dimensions()=0.
	tool := handleMemSimilar(components.store, embedding.NoopProvider{})
	req := newToolRequest("mem_similar", map[string]any{
		"sync_id": "any-sync-id",
	})

	result, err := tool(t.Context(), req)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for zero dims, got success")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "embedding provider") {
		t.Errorf("error should mention 'embedding provider'; got: %s", text)
	}
}
