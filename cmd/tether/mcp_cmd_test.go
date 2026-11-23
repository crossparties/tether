package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crossparties/tether/internal/mcp"
)

func TestCLIMCPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_CLI_MCP_HELPER") != "1" {
		return
	}
	tools := make([]map[string]any, 0, 10)
	for _, name := range append(append([]string{}, mcp.SerenaReadOnlySemanticTools...),
		"replace_symbol_body", "insert_after_symbol", "delete_symbol", "write_project_file", "run_shell") {
		tools = append(tools, map[string]any{
			"name":        name,
			"description": "tool " + name,
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil || len(req.ID) == 0 || string(req.ID) == "null" {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "cli-helper", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": tools}
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID), "result": result})
		fmt.Println(string(b))
	}
	_ = scanner.Err()
	os.Exit(0)
}

func TestCLIMCPAddImportListTestEnableDisableToolsRemove(t *testing.T) {
	ws := t.TempDir()
	cfgDir := t.TempDir()
	homeDir := t.TempDir()
	t.Setenv("TETHER_CONFIG_DIR", cfgDir)
	t.Setenv("HOME", homeDir)

	// Help output tags server bridge clearly as [Deferred / Legacy Bridge].
	var helpOut, helpErr bytes.Buffer
	if code := runMCPCommand(ws, []string{"--help"}, &helpOut, &helpErr); code != 0 {
		t.Fatalf("mcp --help failed: %s", helpErr.String())
	}
	if !strings.Contains(helpOut.String(), "[Deferred / Legacy Bridge]") {
		t.Fatalf("expected [Deferred / Legacy Bridge] note in mcp help, got:\n%s", helpOut.String())
	}

	// 1. Add serena via CLI with secret env var.
	var addOut, addErr bytes.Buffer
	code := runMCPCommand(ws, []string{
		"add", "serena",
		"--command", os.Args[0],
		"--arg", "-test.run=TestCLIMCPHelperProcess",
		"--env", "GO_WANT_CLI_MCP_HELPER=1",
		"--env", "SERENA_API_KEY=cli-secret-999",
		"--scope", "project",
		"--preset", "lazy_readonly",
	}, &addOut, &addErr)
	if code != 0 {
		t.Fatalf("mcp add serena failed (code=%d): %s", code, addErr.String())
	}

	// Case-insensitive duplicate via CLI MUST fail!
	var dupOut, dupErr bytes.Buffer
	if code := runMCPCommand(ws, []string{"add", "Serena", "--command", "echo"}, &dupOut, &dupErr); code == 0 {
		t.Fatalf("expected case-insensitive duplicate 'Serena' to fail")
	}

	// Verify secret is ONLY in 0600 credentials.json, NEVER in .tether/mcp.json!
	projBytes, err := os.ReadFile(filepath.Join(ws, ".tether", "mcp.json"))
	if err != nil {
		t.Fatalf("read .tether/mcp.json: %v", err)
	}
	if strings.Contains(string(projBytes), "cli-secret-999") {
		t.Fatalf(".tether/mcp.json leaked raw secret!\n%s", string(projBytes))
	}
	credPath := filepath.Join(cfgDir, "credentials.json")
	credInfo, err := os.Stat(credPath)
	if err != nil || credInfo.Mode().Perm() != 0o600 {
		t.Fatalf("credentials.json missing or wrong mode: err=%v info=%v", err, credInfo)
	}
	credBytes, _ := os.ReadFile(credPath)
	if !strings.Contains(string(credBytes), "cli-secret-999") {
		t.Fatalf("credentials.json missing stored MCP secret")
	}

	// 2. Import from local IDE configs (agy + cursor) as disabled drafts with secrets extracted.
	agyPath := filepath.Join(homeDir, ".gemini", "config", "mcp_config.json")
	_ = os.MkdirAll(filepath.Dir(agyPath), 0o755)
	_ = os.WriteFile(agyPath, []byte(`{
  "mcpServers": {
    "github-mcp": {"url": "https://api.example.com/mcp", "headers": {"Authorization": "Bearer ide-imported-secret"}}
  }
}`), 0o644)

	var impOut, impErr bytes.Buffer
	if code := runMCPCommand(ws, []string{"import", "--source", "agy", "--scope", "global"}, &impOut, &impErr); code != 0 {
		t.Fatalf("mcp import failed: %s", impErr.String())
	}
	if !strings.Contains(impOut.String(), "github-mcp") {
		t.Fatalf("expected github-mcp in import output, got:\n%s", impOut.String())
	}
	globalBytes, _ := os.ReadFile(filepath.Join(cfgDir, "mcp.json"))
	if strings.Contains(string(globalBytes), "ide-imported-secret") {
		t.Fatalf("global mcp.json leaked imported secret!")
	}

	// 3. list & list --json mask ALL secrets as **********!
	var listJSONOut, listJSONErr bytes.Buffer
	if code := runMCPCommand(ws, []string{"list", "--json"}, &listJSONOut, &listJSONErr); code != 0 {
		t.Fatalf("mcp list --json failed: %s", listJSONErr.String())
	}
	if strings.Contains(listJSONOut.String(), "cli-secret-999") || strings.Contains(listJSONOut.String(), "ide-imported-secret") {
		t.Fatalf("mcp list --json leaked raw secret!\n%s", listJSONOut.String())
	}
	if !strings.Contains(listJSONOut.String(), mcp.SecretMask) {
		t.Fatalf("mcp list --json missing ********** mask!\n%s", listJSONOut.String())
	}

	// 4. test serena runs initialize + tools/list and reports 10 raw -> 5 filtered under lazy_readonly!
	var testOut, testErr bytes.Buffer
	if code := runMCPCommand(ws, []string{"test", "serena"}, &testOut, &testErr); code != 0 {
		t.Fatalf("mcp test serena failed: %s", testErr.String())
	}
	if !strings.Contains(testOut.String(), "10 discovered, 5 active") {
		t.Fatalf("expected '10 discovered, 5 active' in test output, got:\n%s", testOut.String())
	}

	// 5. enable, tools, disable, remove all work cleanly and preserve secrets across toggles.
	var outBuf, errBuf bytes.Buffer
	if code := runMCPCommand(ws, []string{"enable", "serena"}, &outBuf, &errBuf); code != 0 {
		t.Fatalf("mcp enable failed: %s", errBuf.String())
	}
	if code := runMCPCommand(ws, []string{"tools", "serena", "--disable", "get_diagnostics_for_file"}, &outBuf, &errBuf); code != 0 {
		t.Fatalf("mcp tools failed: %s", errBuf.String())
	}
	if code := runMCPCommand(ws, []string{"disable", "serena"}, &outBuf, &errBuf); code != 0 {
		t.Fatalf("mcp disable failed: %s", errBuf.String())
	}

	// Verify secret survived enable/tools/disable toggles!
	reg := mcp.NewClientRegistry(ws)
	srv, ok, err := reg.GetServer("serena")
	if err != nil || !ok || srv.Enabled {
		t.Fatalf("unexpected serena state after disable: srv=%+v ok=%v err=%v", srv, ok, err)
	}
	resolved, err := reg.ResolveRuntimeSecrets(srv)
	if err != nil || resolved.Env["SERENA_API_KEY"] != "cli-secret-999" {
		t.Fatalf("secret lost across CLI toggles: resolved=%+v err=%v", resolved.Env, err)
	}

	// Remove serena and github-mcp.
	if code := runMCPCommand(ws, []string{"remove", "serena"}, &outBuf, &errBuf); code != 0 {
		t.Fatalf("mcp remove serena failed: %s", errBuf.String())
	}
	if code := runMCPCommand(ws, []string{"remove", "github-mcp"}, &outBuf, &errBuf); code != 0 {
		t.Fatalf("mcp remove github-mcp failed: %s", errBuf.String())
	}
	rem, _ := reg.ListServers()
	if len(rem) != 0 {
		t.Fatalf("expected 0 remaining servers after remove, got %+v", rem)
	}
}
