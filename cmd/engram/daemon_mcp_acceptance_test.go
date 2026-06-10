//go:build acceptance

package main

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mariesqu/engram/internal/controlapi"
)

// TestAcceptance_MCPHTTPTransport_RoundTrip starts the daemon with
// --transport http, then drives a full mem_save + mem_search tool round-trip
// via the Streamable HTTP MCP transport.
//
// Proves: the /mcp endpoint is reachable with the bearer token, the tool
// surface is identical to stdio (same 9 tools), and a saved observation is
// returned by a subsequent search.
func TestAcceptance_MCPHTTPTransport_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mcp_http.db")

	freePort, err := findFreePort()
	if err != nil {
		t.Fatalf("findFreePort: %v", err)
	}

	cfg := daemonCfg{
		db:           dbPath,
		syncInterval: 30 * time.Second,
		httpMode:     true,
		httpPort:     freePort,
		mcpTransport: "http",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	errc := make(chan error, 1)
	go func() {
		errc <- runDaemonHTTP(ctx, cfg)
	}()

	// Wait for daemon.json to appear (daemon ready to serve).
	var dj controlapi.DaemonJSON
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		dj, err = controlapi.ReadDaemonJSON(dir)
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("daemon.json not written within 10s: %v", err)
	}

	mcpURL := fmt.Sprintf("http://127.0.0.1:%d/mcp", dj.Port)
	tok := dj.Token

	// ── Build a Streamable HTTP MCP client ────────────────────────────────────
	trans, err := transport.NewStreamableHTTP(
		mcpURL,
		transport.WithHTTPHeaders(map[string]string{
			"Authorization": "Bearer " + tok,
		}),
	)
	if err != nil {
		cancel()
		t.Fatalf("NewStreamableHTTP: %v", err)
	}

	cli := mcpclient.NewClient(trans)

	initCtx, initCancel := context.WithTimeout(ctx, 10*time.Second)
	defer initCancel()

	_, err = cli.Initialize(initCtx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "engram-acceptance-test",
				Version: "0.0.1",
			},
		},
	})
	if err != nil {
		cancel()
		t.Fatalf("MCP Initialize: %v", err)
	}

	// ── tools/list: identical-tool-surface proof ──────────────────────────────
	toolsCtx, toolsCancel := context.WithTimeout(ctx, 10*time.Second)
	defer toolsCancel()

	toolsResult, err := cli.ListTools(toolsCtx, mcp.ListToolsRequest{})
	if err != nil {
		cancel()
		t.Fatalf("ListTools: %v", err)
	}

	wantTools := []string{
		"mem_session_start",
		"mem_session_end",
		"mem_save",
		"mem_get_observation",
		"mem_session_summary",
		"mem_search",
		"mem_context",
		"mem_judge",
		"mem_save_prompt",
	}
	if len(toolsResult.Tools) != len(wantTools) {
		names := make([]string, len(toolsResult.Tools))
		for i, tool := range toolsResult.Tools {
			names[i] = tool.Name
		}
		t.Errorf("HTTP transport: got %d tools %v, want %d: %v",
			len(toolsResult.Tools), names, len(wantTools), wantTools)
	} else {
		toolMap := make(map[string]bool, len(toolsResult.Tools))
		for _, tool := range toolsResult.Tools {
			toolMap[tool.Name] = true
		}
		for _, name := range wantTools {
			if !toolMap[name] {
				t.Errorf("tool %q missing from HTTP transport tool listing", name)
			}
		}
	}

	// Prove identical tool surface vs stdio: build the MCP server the same way
	// buildDaemon does and compare tool names.
	stdioCfg := daemonCfg{db: filepath.Join(dir, "stdio_compare.db"), syncInterval: 30 * time.Second}
	stdioComponents, err := buildDaemon(stdioCfg)
	if err != nil {
		cancel()
		t.Fatalf("buildDaemon (stdio comparison): %v", err)
	}
	defer stdioComponents.Close()

	stdioTools := stdioComponents.mcpServer.ListTools()
	if len(stdioTools) != len(toolsResult.Tools) {
		t.Errorf("tool surface mismatch: HTTP transport has %d tools, stdio server has %d tools",
			len(toolsResult.Tools), len(stdioTools))
	}
	for name := range stdioTools {
		found := false
		for _, httpTool := range toolsResult.Tools {
			if httpTool.Name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("tool %q present in stdio but missing from HTTP transport", name)
		}
	}

	// ── mem_save round-trip ───────────────────────────────────────────────────
	saveCtx, saveCancel := context.WithTimeout(ctx, 10*time.Second)
	defer saveCancel()

	saveResult, err := cli.CallTool(saveCtx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "mem_save",
			Arguments: map[string]any{
				"title":   "HTTP MCP transport acceptance",
				"content": "Verified over Streamable HTTP",
				"type":    "decision",
				"project": "acceptance-proj",
			},
		},
	})
	if err != nil {
		cancel()
		t.Fatalf("CallTool(mem_save): %v", err)
	}
	if saveResult.IsError {
		cancel()
		t.Fatalf("mem_save returned IsError=true: %v", saveResult.Content)
	}

	// ── mem_search: verify saved observation is returned ─────────────────────
	searchCtx, searchCancel := context.WithTimeout(ctx, 10*time.Second)
	defer searchCancel()

	searchResult, err := cli.CallTool(searchCtx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "mem_search",
			Arguments: map[string]any{
				"query":   "HTTP MCP transport acceptance",
				"project": "acceptance-proj",
			},
		},
	})
	if err != nil {
		cancel()
		t.Fatalf("CallTool(mem_search): %v", err)
	}
	if searchResult.IsError {
		cancel()
		t.Fatalf("mem_search returned IsError=true: %v", searchResult.Content)
	}
	if len(searchResult.Content) == 0 {
		cancel()
		t.Fatal("mem_search returned no content")
	}
	searchText := ""
	for _, c := range searchResult.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			searchText += tc.Text
		}
	}
	if !strings.Contains(searchText, "HTTP MCP transport acceptance") {
		t.Errorf("mem_search result does not contain the saved title; got:\n%s", searchText)
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil && !strings.Contains(err.Error(), "context") {
			t.Errorf("runDaemonHTTP: unexpected error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("daemon did not stop within 5s")
	}
}

// TestAcceptance_MCPHTTPTransport_OmittedRefusal verifies that policy
// enforcement applies on the HTTP MCP transport: saving to a project with
// policy "omitted" returns a tool error and writes nothing.
//
// The policy is set directly on the store BEFORE the daemon starts so we
// exercise the tool-layer policy check without relying on the control API
// PUT /api/v1/projects/{project}/policy path (which is tested separately in
// internal/controlapi/acceptance_test.go).
func TestAcceptance_MCPHTTPTransport_OmittedRefusal(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mcp_omit.db")

	// Pre-seed the policy directly on the store so the daemon sees it on boot.
	// This avoids a control-API PUT round-trip and removes the race between
	// policy persistence and the first MCP save.
	{
		st, err := buildDaemon(daemonCfg{db: dbPath, syncInterval: 30 * time.Second})
		if err != nil {
			t.Fatalf("pre-seed store open: %v", err)
		}
		if err := st.store.SetPolicy("omitted-proj", "omitted"); err != nil {
			st.Close()
			t.Fatalf("pre-seed SetPolicy: %v", err)
		}
		st.Close()
	}

	freePort, err := findFreePort()
	if err != nil {
		t.Fatalf("findFreePort: %v", err)
	}

	cfg := daemonCfg{
		db:           dbPath,
		syncInterval: 30 * time.Second,
		httpMode:     true,
		httpPort:     freePort,
		mcpTransport: "http",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	errc := make(chan error, 1)
	go func() {
		errc <- runDaemonHTTP(ctx, cfg)
	}()

	var dj controlapi.DaemonJSON
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		dj, err = controlapi.ReadDaemonJSON(dir)
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("daemon.json not written within 10s: %v", err)
	}

	// Connect via MCP HTTP and attempt to save to the omitted project.
	mcpURL := fmt.Sprintf("http://127.0.0.1:%d/mcp", dj.Port)
	trans, err := transport.NewStreamableHTTP(
		mcpURL,
		transport.WithHTTPHeaders(map[string]string{
			"Authorization": "Bearer " + dj.Token,
		}),
	)
	if err != nil {
		cancel()
		t.Fatalf("NewStreamableHTTP: %v", err)
	}

	cli := mcpclient.NewClient(trans)

	initCtx, initCancel := context.WithTimeout(ctx, 10*time.Second)
	defer initCancel()

	_, err = cli.Initialize(initCtx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "engram-omit-test",
				Version: "0.0.1",
			},
		},
	})
	if err != nil {
		cancel()
		t.Fatalf("MCP Initialize: %v", err)
	}

	saveCtx, saveCancel := context.WithTimeout(ctx, 10*time.Second)
	defer saveCancel()

	saveResult, err := cli.CallTool(saveCtx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "mem_save",
			Arguments: map[string]any{
				"title":   "should be refused",
				"content": "this write must be rejected",
				"project": "omitted-proj",
			},
		},
	})
	if err != nil {
		cancel()
		t.Fatalf("CallTool(mem_save) transport error: %v", err)
	}
	// The tool must return IsError=true (policy refused).
	if !saveResult.IsError {
		cancel()
		t.Fatalf("mem_save to omitted project: expected IsError=true, got success: %v", saveResult.Content)
	}

	// Verify nothing was written to the store via mem_search.
	// Search for a phrase from the CONTENT (not the title/query string) to avoid
	// the mem_search "No memories found for: <query>" message falsely matching.
	searchCtx, searchCancel := context.WithTimeout(ctx, 10*time.Second)
	defer searchCancel()

	searchResult, err := cli.CallTool(searchCtx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "mem_search",
			Arguments: map[string]any{
				"query":   "write must be rejected omitted",
				"project": "omitted-proj",
			},
		},
	})
	if err != nil {
		cancel()
		t.Fatalf("CallTool(mem_search) transport error: %v", err)
	}
	// Search should return zero results (nothing was saved).
	// A non-error response means the store responded; check the content doesn't
	// include the specific body text we tried to save.
	if !searchResult.IsError {
		for _, c := range searchResult.Content {
			if tc, ok := c.(mcp.TextContent); ok {
				if strings.Contains(tc.Text, "this write must be rejected") {
					t.Errorf("omitted project: found saved content in search — write must have been rejected; response:\n%s", tc.Text)
				}
			}
		}
	}

	// Cleanup
	cancel()
	select {
	case err := <-errc:
		if err != nil && !strings.Contains(err.Error(), "context") {
			t.Errorf("runDaemonHTTP: unexpected error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("daemon did not stop within 5s")
	}
}

// TestAcceptance_MCPHTTPTransport_APIAndUIUnchanged verifies that /api/v1/status
// and /ui/ continue to work normally in --transport http mode (regression smoke).
func TestAcceptance_MCPHTTPTransport_APIAndUIUnchanged(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mcp_reg.db")

	freePort, err := findFreePort()
	if err != nil {
		t.Fatalf("findFreePort: %v", err)
	}

	cfg := daemonCfg{
		db:           dbPath,
		syncInterval: 30 * time.Second,
		httpMode:     true,
		httpPort:     freePort,
		mcpTransport: "http",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	errc := make(chan error, 1)
	go func() {
		errc <- runDaemonHTTP(ctx, cfg)
	}()

	var dj controlapi.DaemonJSON
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		dj, err = controlapi.ReadDaemonJSON(dir)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("daemon.json not written within 8s: %v", err)
	}

	// /api/v1/status with correct token → 200.
	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/api/v1/status", dj.Port), nil)
	req.Header.Set("Authorization", "Bearer "+dj.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET /api/v1/status: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Errorf("GET /api/v1/status in http-transport mode: got %d, want 200", resp.StatusCode)
	}

	// /ui/ without session cookie → 401 (requires token-exchange).
	// The web UI is still mounted; unauthenticated access is rejected.
	uiResp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/ui/", dj.Port))
	if err != nil {
		cancel()
		t.Fatalf("GET /ui/: %v", err)
	}
	uiResp.Body.Close()
	// Any non-200 response confirms the route exists and auth is enforced.
	// 401 or redirect (302) to token exchange are both valid.
	if uiResp.StatusCode == http.StatusNotFound {
		cancel()
		t.Errorf("GET /ui/ returned 404 in http-transport mode — web UI must remain mounted")
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil && !strings.Contains(err.Error(), "context") {
			t.Errorf("runDaemonHTTP: unexpected error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("daemon did not stop within 5s")
	}
}
