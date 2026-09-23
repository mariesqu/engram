package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestInitializeResult_CarriesServerInstructions drives a real JSON-RPC
// `initialize` through the daemon's MCP server and asserts the response carries
// a non-empty `instructions` field naming the two tools an agent cannot open a
// session without: mem_current_project (which project am I?) and mem_save (the
// whole point).
//
// This is the contract gentle-ai's slim CLAUDE.md section depends on — it stops
// shipping the full protocol the moment `engram version` parses at or above its
// floor, on the stated assumption that the MCP server supplies it instead. If
// this test ever goes red, every Claude Code session on a current engram silently
// loses the save format, the lifecycle rules and the after-compaction steps.
func TestInitializeResult_CarriesServerInstructions(t *testing.T) {
	components, err := buildDaemon(daemonCfg{
		db:           filepath.Join(t.TempDir(), "instructions.db"),
		syncInterval: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
		`"protocolVersion":"2025-03-26","capabilities":{},` +
		`"clientInfo":{"name":"test","version":"0.0.0"}}}`)

	resp := components.mcpServer.HandleMessage(t.Context(), raw)
	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal initialize response: %v", err)
	}

	var envelope struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatalf("unmarshal initialize response %s: %v", encoded, err)
	}
	if envelope.Error != nil {
		t.Fatalf("initialize returned an error: %s", envelope.Error.Message)
	}

	got := envelope.Result.Instructions
	if strings.TrimSpace(got) == "" {
		t.Fatalf("initialize result carries no instructions; response was %s", encoded)
	}
	for _, want := range []string{"mem_current_project", "mem_save"} {
		if !strings.Contains(got, want) {
			t.Errorf("instructions do not mention %q", want)
		}
	}
}

// TestServerInstructions_OnlyNameRegisteredTools guards the one way this text
// can actively harm an agent: naming a mem_* tool the server does not register.
// The agent trusts the roster, calls the tool, and gets a protocol error it has
// no way to recover from — strictly worse than never hearing about the tool.
func TestServerInstructions_OnlyNameRegisteredTools(t *testing.T) {
	components, err := buildDaemon(daemonCfg{
		db:           filepath.Join(t.TempDir(), "instructions_roster.db"),
		syncInterval: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildDaemon: %v", err)
	}
	t.Cleanup(components.Close)

	registered := components.mcpServer.ListTools()

	// Scan the instructions for every mem_* token and check each one resolves.
	// Tokens are delimited by anything that is not a word character, so trailing
	// punctuation ("mem_save,") does not defeat the lookup.
	for _, token := range strings.FieldsFunc(serverInstructions, func(r rune) bool {
		return !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) {
		if !strings.HasPrefix(token, "mem_") {
			continue
		}
		if _, ok := registered[token]; !ok {
			t.Errorf("instructions name %q, which registerTools does not register", token)
		}
	}
}
