package mcp

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crossparties/tether/internal/core"
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
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"mode_set","arguments":{"mode":"code"}}}`,
	}

	inputBuf := bytes.NewBufferString(strings.Join(requests, "\n") + "\n")
	outputBuf := &bytes.Buffer{}

	err := server.ServeIO(inputBuf, outputBuf)
	if err != nil {
		t.Fatalf("ServeIO: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(outputBuf.String()), "\n")
	if len(lines) < 4 {
		t.Fatalf("expected at least 4 response lines, got %d. Output: %s", len(lines), outputBuf.String())
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

	// 2. Check streamlined 10-tool schema
	var toolsResp Response
	if err := json.Unmarshal([]byte(lines[1]), &toolsResp); err != nil {
		t.Fatalf("unmarshal tools response: %v", err)
	}
	toolsData, _ := json.Marshal(toolsResp.Result)
	var toolsList struct {
		Tools []Tool `json:"tools"`
	}
	_ = json.Unmarshal(toolsData, &toolsList)
	if len(toolsList.Tools) != 10 {
		t.Fatalf("expected exactly 10 focused core tools, got %d", len(toolsList.Tools))
	}
	wantTools := map[string]bool{
		"read_file": true, "write_file": true, "edit_file": true,
		"glob": true, "grep": true, "run_command": true,
		"kb_search": true, "todo_manage": true, "mode_set": true, "decision_add": true,
	}
	for _, tool := range toolsList.Tools {
		if !wantTools[tool.Name] {
			t.Errorf("unexpected tool in streamlined ListTools: %q", tool.Name)
		}
		delete(wantTools, tool.Name)
	}
	if len(wantTools) != 0 {
		t.Errorf("missing expected core tools: %v", wantTools)
	}

	// 3. Verify mode_set took effect in engine
	if e.GetMode() != "code" {
		t.Errorf("expected final engine mode code, got %s", e.GetMode())
	}
}

func TestMCPServer_CoreToolsReadGlobGrepTodoDecisionKB(t *testing.T) {
	e := newTestEngine(t)
	if err := os.WriteFile(filepath.Join(e.Workspace, "main.go"), []byte("package main\nfunc helloWorld() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.Workspace, "README.md"), []byte("# Tether Docs\nsqlite wal persistence\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.Workspace, ".env"), []byte("API_KEY=supersecret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.IndexDirectory(e.Workspace, nil); err != nil {
		t.Fatalf("IndexDirectory: %v", err)
	}

	server := NewServer(e)

	// read_file ALWAYS returns 1-based line numbers
	readOut, isErr := server.CallTool("read_file", json.RawMessage(`{"path":"main.go"}`))
	if isErr || !strings.Contains(readOut, "1: package main\n2: func helloWorld() {}\n") {
		t.Fatalf("read_file line numbering mismatch: isErr=%v out=%q", isErr, readOut)
	}

	// glob finds workspace files while skipping .env & .tether
	globOut, isErr := server.CallTool("glob", json.RawMessage(`{"pattern":"**/*"}`))
	if isErr || !strings.Contains(globOut, "main.go") || strings.Contains(globOut, ".env") || strings.Contains(globOut, "session.db") {
		t.Fatalf("glob output unexpected: isErr=%v out=%q", isErr, globOut)
	}

	// grep finds matches in allowed files and never leaks .env
	grepOut, isErr := server.CallTool("grep", json.RawMessage(`{"pattern":"helloWorld|supersecret"}`))
	if isErr || !strings.Contains(grepOut, "main.go:2: func helloWorld() {}") || strings.Contains(grepOut, "supersecret") {
		t.Fatalf("grep output unexpected: isErr=%v out=%q", isErr, grepOut)
	}

	// todo_manage lifecycle: add -> list -> complete -> remove
	addOut, isErr := server.CallTool("todo_manage", json.RawMessage(`{"action":"add","description":"verify phase 2"}`))
	if isErr || !strings.Contains(addOut, "todo #1 added") {
		t.Fatalf("todo_manage add failed: isErr=%v out=%q", isErr, addOut)
	}
	listOut, isErr := server.CallTool("todo_manage", json.RawMessage(`{"action":"list"}`))
	if isErr || !strings.Contains(listOut, "verify phase 2") {
		t.Fatalf("todo_manage list failed: isErr=%v out=%q", isErr, listOut)
	}
	if out, isErr := server.CallTool("todo_manage", json.RawMessage(`{"action":"complete","id":1}`)); isErr || !strings.Contains(out, "completed") {
		t.Fatalf("todo_manage complete failed: isErr=%v out=%q", isErr, out)
	}
	if out, isErr := server.CallTool("todo_manage", json.RawMessage(`{"action":"remove","id":1}`)); isErr || !strings.Contains(out, "removed") {
		t.Fatalf("todo_manage remove failed: isErr=%v out=%q", isErr, out)
	}

	// decision_add & kb_search
	if out, isErr := server.CallTool("decision_add", json.RawMessage(`{"topic":"mount","decision":"use gitignore globs","reason":"ergonomics"}`)); isErr || !strings.Contains(out, "decision recorded") {
		t.Fatalf("decision_add failed: isErr=%v out=%q", isErr, out)
	}
	if out, isErr := server.CallTool("kb_search", json.RawMessage(`{"query":"sqlite"}`)); isErr || !strings.Contains(out, "README.md") {
		t.Fatalf("kb_search failed: isErr=%v out=%q", isErr, out)
	}
}

func TestLegacyProposalAndRemovedBloatToolsReturnUnknown(t *testing.T) {
	e := newTestEngine(t)
	server := NewServer(e)

	req := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"
	out := &bytes.Buffer{}
	if err := server.ServeIO(bytes.NewBufferString(req), out); err != nil {
		t.Fatalf("ServeIO: %v", err)
	}
	removed := []string{
		"approve_proposal", "reject_proposal", "proposal_status", "subagent_spawn_rules",
		"workspace_info", "session_load", "session_save", "session_done", "setting_set",
		"pattern_add", "mode_get", "kb_index", "kb_status",
		"todo_add", "todo_list", "todo_complete", "todo_remove",
		"task_classify", "files_modified", "sql_query",
	}
	for _, name := range removed {
		if strings.Contains(out.String(), `"`+name+`"`) {
			t.Errorf("tools/list still advertises removed tool %q", name)
		}
		res, isErr := server.CallTool(name, json.RawMessage(`{}`))
		if !isErr || !strings.Contains(res, "unknown tool") {
			t.Errorf("CallTool(%q) = (%q, %v), want unknown tool error", name, res, isErr)
		}
	}
}
