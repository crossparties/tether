package mcp

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
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

func TestApplyFuzzyEditDidYouMeanAndMaxReflections(t *testing.T) {
	e := newTestEngine(t)
	if err := e.AddApprovalGrant(".", "all"); err != nil {
		t.Fatal(err)
	}
	content := strings.Join([]string{
		"package demo",
		"",
		"func alpha() int {",
		"\treturn 1",
		"}",
		"",
		"func computeTotalScore(items []int, multiplier int) int {",
		"\ttotal := 0",
		"\tfor _, v := range items {",
		"\t\ttotal += v * multiplier",
		"\t}",
		"\treturn total",
		"}",
	}, "\n") + "\n"

	if err := os.WriteFile(filepath.Join(e.Workspace, "calc.go"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if _, _, sLine, eLine, err := applyFuzzyEditWithHint(content, "func computeTotalScore(items []int, factor int) int {", ""); err == nil || sLine != 7 || eLine != 7 || !strings.Contains(err.Error(), "Did you mean (around lines 7-7)?") {
		t.Fatalf("expected applyFuzzyEditWithHint direct Did you mean hint, got lines=%d-%d err=%v", sLine, eLine, err)
	}

	server := NewServer(e)
	missArgs, _ := json.Marshal(map[string]string{
		"path":        "calc.go",
		"target":      "func computeTotalScore(items []int, factor int) int {\n\ttotal := 0",
		"replacement": "func computeTotalScore(items []int, factor int) int {\n\ttotal := 1",
	})

	// Miss #1 -> returns "Did you mean (around lines 7-8)?" with 1-based line numbers.
	res1, isErr := server.CallTool("edit_file", missArgs)
	if !isErr || !strings.Contains(res1, "Did you mean (around lines 7-8)?") || !strings.Contains(res1, "7: func computeTotalScore") {
		t.Fatalf("expected Did you mean hint with lines 7-8, got isErr=%v res:\n%s", isErr, res1)
	}
	if strings.Contains(res1, "max_reflections=3") {
		t.Fatalf("miss #1 should not hit max_reflections=3 yet:\n%s", res1)
	}

	// Miss #2 -> still shows Did you mean without max_reflections cap.
	res2, isErr := server.CallTool("edit_file", missArgs)
	if !isErr || strings.Contains(res2, "max_reflections=3") {
		t.Fatalf("miss #2 should not hit max_reflections=3 yet, got:\n%s", res2)
	}

	// Miss #3 -> hits max_reflections=3 and advises read_file with exact line range!
	res3, isErr := server.CallTool("edit_file", missArgs)
	if !isErr || !strings.Contains(res3, "max_reflections=3") || !strings.Contains(res3, `read_file(path="calc.go", start_line=`) {
		t.Fatalf("miss #3 expected max_reflections=3 + read_file guidance, got:\n%s", res3)
	}

	// Reading the file resets consecutive edit-miss reflection counter.
	if readRes, rErr := server.CallTool("read_file", json.RawMessage(`{"path":"calc.go","start_line":7,"end_line":13}`)); rErr || !strings.Contains(readRes, "7: func computeTotalScore") {
		t.Fatalf("read_file failed: err=%v out=%s", rErr, readRes)
	}
	resAfterRead, isErr := server.CallTool("edit_file", missArgs)
	if !isErr || strings.Contains(resAfterRead, "max_reflections=3") {
		t.Fatalf("reflection counter should have reset after read_file, got:\n%s", resAfterRead)
	}

	// Successful edit also resets counter.
	okArgs, _ := json.Marshal(map[string]string{
		"path":        "calc.go",
		"target":      "\treturn total",
		"replacement": "\treturn total + 0",
	})
	if okRes, okErr := server.CallTool("edit_file", okArgs); okErr {
		t.Fatalf("expected valid edit to succeed, got: %s", okRes)
	}
	resAfterOk, isErr := server.CallTool("edit_file", missArgs)
	if !isErr || strings.Contains(resAfterOk, "max_reflections=3") {
		t.Fatalf("reflection counter should have reset after successful edit, got:\n%s", resAfterOk)
	}
}

func TestAutoVerifyMultiStack(t *testing.T) {
	e := newTestEngine(t)
	if err := e.AddApprovalGrant(".", "all"); err != nil {
		t.Fatal(err)
	}
	server := NewServer(e)

	// 1. TypeScript/Svelte file with no installed tooling/node_modules skips quietly with zero noise.
	_ = e.SetProjectSetting("tech_stack", "svelte")
	_ = e.SetProjectSetting("frontend_dir", "web")
	_ = e.SetProjectSetting("check_cmd", "cd web && npm run check")
	if err := os.MkdirAll(filepath.Join(e.Workspace, "web"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.Workspace, "web", "package.json"), []byte(`{"scripts":{"check":"svelte-check"}}`), 0644); err != nil {
		t.Fatal(err)
	}

	svelteRes, isErr := server.CallTool("write_file", json.RawMessage(`{"path":"web/App.svelte","content":"<h1>Hello</h1>\n"}`))
	if isErr || strings.Contains(svelteRes, "failed") {
		t.Fatalf("expected missing frontend tooling to skip quietly, got isErr=%v res=%q", isErr, svelteRes)
	}

	// 2. When frontend check tooling & node_modules are present, configured check_cmd runs cleanly.
	fakeBin := t.TempDir()
	fakeNpm := filepath.Join(fakeBin, "npm")
	if err := os.WriteFile(fakeNpm, []byte("#!/bin/sh\necho 'svelte check ok'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.Workspace, "web", "node_modules"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	svelteRunRes, isErr := server.CallTool("write_file", json.RawMessage(`{"path":"web/Page.svelte","content":"<p>ok</p>\n"}`))
	if isErr || !strings.Contains(svelteRunRes, "npm run check passed") {
		t.Fatalf("expected configured npm run check to pass, got isErr=%v res=%q", isErr, svelteRunRes)
	}

	// 2b. Fail-closed on escaping `cd ../outside && npm run check`.
	if err := os.WriteFile(filepath.Join(e.Workspace, "package.json"), []byte(`{"scripts":{"check":"svelte-check"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.Workspace, "node_modules"), 0755); err != nil {
		t.Fatal(err)
	}
	_ = e.SetProjectSetting("check_cmd", "cd ../outside && npm run check")
	escapeRes, isErr := server.CallTool("write_file", json.RawMessage(`{"path":"App.svelte","content":"<p>x</p>\n"}`))
	if isErr || strings.Contains(escapeRes, "npm run check") {
		t.Fatalf("expected escaping cd in check_cmd to fail closed, got isErr=%v res=%q", isErr, escapeRes)
	}

	// 3. Python syntax check (when python3 binary is installed).
	if _, err := exec.LookPath("python3"); err == nil {
		pyOK, isErr := server.CallTool("write_file", json.RawMessage(`{"path":"ok.py","content":"def add(a, b):\n    return a + b\n"}`))
		if isErr || !strings.Contains(pyOK, "py_compile passed") {
			t.Fatalf("expected py_compile passed, got isErr=%v res=%q", isErr, pyOK)
		}
		pyBad, isErr := server.CallTool("write_file", json.RawMessage(`{"path":"bad.py","content":"def broken(:\n    pass\n"}`))
		if isErr || !strings.Contains(pyBad, "py_compile failed") {
			t.Fatalf("expected py_compile failure notice for syntax error, got isErr=%v res=%q", isErr, pyBad)
		}
		if _, cacheErr := os.Stat(filepath.Join(e.Workspace, "__pycache__")); !os.IsNotExist(cacheErr) {
			t.Fatalf("autoVerify should not pollute workspace with __pycache__")
		}
	}
}
