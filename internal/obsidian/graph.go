package obsidian

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

// defaultGraphTemplate is the locked-in default Obsidian graph view config
// (source #1226), embedded verbatim — REQ-GRAPH-05. Do not re-tune,
// re-order, round, or "improve" any value in graph.json; see
// TestEmbeddedGraphTemplate for the exact pinned values.
//
//go:embed graph.json
var defaultGraphTemplate []byte

// GraphConfigMode controls how WriteGraphConfig treats an existing
// {vault}/.obsidian/graph.json (REQ-GRAPH-01).
type GraphConfigMode string

const (
	// GraphConfigPreserve writes the embedded default template only when
	// {vault}/.obsidian/graph.json is absent (REQ-GRAPH-02); an existing
	// file is left byte-identical. Default for both the CLI's
	// --graph-config flag and the daemon's obsidian_graph_config config
	// key.
	GraphConfigPreserve GraphConfigMode = "preserve"

	// GraphConfigForce overwrites {vault}/.obsidian/graph.json
	// unconditionally with the embedded default template (REQ-GRAPH-03).
	GraphConfigForce GraphConfigMode = "force"

	// GraphConfigSkip never touches {vault}/.obsidian/ at all — no log
	// line, no read, no write (REQ-GRAPH-04). This is also what an UNSET
	// ExportConfig.GraphConfig (the zero value, "") behaves as: Export()
	// treats "" identically to GraphConfigSkip so a caller that never
	// mentions graph config gets an inert Exporter, and it is what
	// obsidian.Loop (Phase 8) downgrades to after the first successful
	// cycle of the daemon process (REQ-WATCH-06).
	GraphConfigSkip GraphConfigMode = "skip"
)

// ParseGraphConfigMode parses s into a GraphConfigMode.
//
// The empty string is deliberately NOT an error: it resolves to
// GraphConfigSkip (design decision "GraphConfig empty-string default").
// Both callers that read a user-facing setting — the CLI's --graph-config
// flag and the daemon's obsidian_graph_config config key — default THEIR
// OWN unset value to "preserve" before it reaches here, so the only way
// ExportConfig.GraphConfig is ever "" is when nobody configured anything at
// all, and that case must be inert rather than silently write a file.
func ParseGraphConfigMode(s string) (GraphConfigMode, error) {
	switch GraphConfigMode(s) {
	case "":
		return GraphConfigSkip, nil
	case GraphConfigPreserve, GraphConfigForce, GraphConfigSkip:
		return GraphConfigMode(s), nil
	default:
		return "", fmt.Errorf("obsidian: invalid graph-config mode %q: want one of preserve, force, skip", s)
	}
}

// WriteGraphConfig applies mode to {vaultPath}/.obsidian/graph.json —
// REQ-GRAPH-06, the ONE sanctioned write outside {vault}/engram/.
//
//   - preserve (REQ-GRAPH-02): writes the embedded template only when the
//     file is absent; an existing file is left byte-identical.
//   - force (REQ-GRAPH-03): overwrites unconditionally.
//   - skip (REQ-GRAPH-04): touches nothing at all — no {vault}/.obsidian/
//     directory is created, no file is read or written, no log line.
//
// This deliberately does NOT go through Exporter.resolveVaultPath: that
// helper enforces the {vault}/engram/ namespace by design and would refuse
// a path outside it. containedPath is still used directly here, rooted at
// the VAULT ROOT rather than engram/, so the write stays pinned to exactly
// {vault}/.obsidian/graph.json and cannot be steered anywhere else.
func WriteGraphConfig(vaultPath string, mode GraphConfigMode) error {
	if mode == GraphConfigSkip {
		return nil
	}

	target, err := containedPath(vaultPath, filepath.Join(".obsidian", "graph.json"))
	if err != nil {
		return fmt.Errorf("obsidian: graph config path: %w", err)
	}

	if mode == GraphConfigPreserve {
		if _, err := os.Stat(target); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("obsidian: stat graph config %s: %w", target, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return fmt.Errorf("obsidian: create .obsidian dir: %w", err)
	}
	if err := os.WriteFile(target, defaultGraphTemplate, 0644); err != nil {
		return fmt.Errorf("obsidian: write graph config %s: %w", target, err)
	}
	return nil
}
