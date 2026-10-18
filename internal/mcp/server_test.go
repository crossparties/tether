package mcp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/croosparties/tether/internal/core"
)

func newTestEngine(t *testing.T) *core.Engine {
	t.Helper()
	dir := t.TempDir()
	e, err := core.NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func TestMCPServer_Lifecycle(t *testing.T) {
	e := newTestEngine(t)
	server := NewServer(e)

	// Construct client request stream (ndjson)
	requests := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"mode_set","arguments":{"mode":"plan"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"mode_get"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"mode_set","arguments":{"mode":"code"}}}`,
	}

	inputBuf := bytes.NewBufferString(strings.Join(requests, "\n") + "\n")
	outputBuf := &bytes.Buffer{}

	err := server.ServeIO(inputBuf, outputBuf)
	if err != nil {
		t.Fatalf("ServeIO: %v", err)
	}

	// Parse responses line by line
	lines := strings.Split(strings.TrimSpace(outputBuf.String()), "\n")
	if len(lines) < 5 {
		t.Fatalf("expected at least 5 response lines, got %d. Output: %s", len(lines), outputBuf.String())
	}

	// 1. Check initialize response
	var initResp Response
	if err := json.Unmarshal([]byte(lines[0]), &initResp); err != nil {
		t.Fatalf("unmarshal init response: %v", err)
	}
	initData, _ := json.Marshal(initResp.Result)
	var initResult InitializeResult
	_ = json.Unmarshal(initData, &initResult)
	if initResult.ServerInfo.Name != "tether" {
		t.Errorf("expected server name 'tether', got %q", initResult.ServerInfo.Name)
	}

	// 2. Check tools/list response
	var toolsResp Response
	if err := json.Unmarshal([]byte(lines[1]), &toolsResp); err != nil {
		t.Fatalf("unmarshal tools response: %v", err)
	}
	toolsData, _ := json.Marshal(toolsResp.Result)
	var toolsList struct {
		Tools []Tool `json:"tools"`
	}
	_ = json.Unmarshal(toolsData, &toolsList)
	if len(toolsList.Tools) < 10 {
		t.Errorf("expected at least 10 tools, got %d", len(toolsList.Tools))
	}

	// 3. Verify mode_set to plan took effect in engine
	if e.GetMode() != "code" { // note: after request #5 it was switched back to code
		t.Errorf("expected final engine mode code, got %s", e.GetMode())
	}
}

func TestMCPServer_SQLQuery(t *testing.T) {
	e := newTestEngine(t)
	server := NewServer(e)

	// 1. Insert a decision via engine
	_ = e.AddDecision("auth", "use jwt", "stateless")

	// 2. Call sql_query tool via MCP
	callReq := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sql_query","arguments":{"query":"SELECT topic, decision FROM decisions WHERE topic = 'auth'"}}}`
	inputBuf := bytes.NewBufferString(callReq + "\n")
	outputBuf := &bytes.Buffer{}

	err := server.ServeIO(inputBuf, outputBuf)
	if err != nil {
		t.Fatalf("ServeIO: %v", err)
	}

	var resp Response
	if err := json.Unmarshal(outputBuf.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("RPC error: %s", resp.Error.Message)
	}

	toolResult, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("expected ToolResult map, got %T", resp.Result)
	}
	content := toolResult["content"].([]interface{})
	firstBlock := content[0].(map[string]interface{})
	text := firstBlock["text"].(string)

	if !strings.Contains(text, "use jwt") {
		t.Errorf("expected sql result to contain 'use jwt', got: %s", text)
	}
}

