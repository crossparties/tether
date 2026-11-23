package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crossparties/tether/internal/core"
)

// TestMCPClientHelperProcess acts as a real stdio MCP server subprocess for unit tests.
func TestMCPClientHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_MCP_HELPER") != "1" {
		return
	}

	if logPath := strings.TrimSpace(os.Getenv("HELPER_SPAWN_LOG")); logPath != "" {
		_ = os.WriteFile(logPath, []byte("spawned\n"), 0o644)
	}

	if os.Getenv("HELPER_IGNORE_EOF") == "1" {
		// Still answer initialize/tools/list if needed, or sleep after stdin EOF to test Kill+Reap.
	}

	tools := []map[string]any{
		{"name": "find_symbol", "description": "Find symbol", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"name_path": map[string]any{"type": "string"}}}},
		{"name": "find_referencing_symbols", "description": "Find refs", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"name_path": map[string]any{"type": "string"}}}},
		{"name": "find_implementations", "description": "Find impls", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"name_path": map[string]any{"type": "string"}}}},
		{"name": "get_symbols_overview", "description": "Symbols overview", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"relative_path": map[string]any{"type": "string"}}}},
		{"name": "get_diagnostics_for_file", "description": "Diagnostics", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"relative_path": map[string]any{"type": "string"}}}},
		{"name": "replace_symbol_body", "description": "Mutate symbol", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"name_path": map[string]any{"type": "string"}}}},
		{"name": "insert_after_symbol", "description": "Insert code", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"name_path": map[string]any{"type": "string"}}}},
		{"name": "delete_symbol", "description": "Delete symbol", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"name_path": map[string]any{"type": "string"}}}},
		{"name": "write_project_file", "description": "Write file", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}},
		{"name": "run_shell", "description": "Run shell", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}}},
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if len(req.ID) == 0 || string(req.ID) == "null" {
			// Notification; ignore.
			continue
		}

		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo":      map[string]any{"name": "helper-mcp", "version": "1.0.0"},
			}
		case "tools/list":
			result = map[string]any{"tools": tools}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if os.Getenv("HELPER_OVERSIZE_CL") == "1" {
				fmt.Print("Content-Length: 99999999\r\n\r\n{}")
				os.Exit(0)
			}
			if os.Getenv("HELPER_LARGE_REPLY") == "1" {
				big := strings.Repeat("A", 128*1024)
				result = map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": "LARGE:" + big},
					},
				}
				break
			}
			token := os.Getenv("SERENA_API_TOKEN")
			argsBytes, _ := json.Marshal(p.Arguments)
			result = map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": fmt.Sprintf("called=%s token=%s args=%s", p.Name, token, string(argsBytes))},
				},
			}
		default:
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"error":   map[string]any{"code": -32601, "message": "method not found"},
			}
			b, _ := json.Marshal(resp)
			fmt.Println(string(b))
			continue
		}

		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(req.ID),
			"result":  result,
		}
		b, _ := json.Marshal(resp)
		fmt.Println(string(b))
	}
	_ = scanner.Err()

	if os.Getenv("HELPER_IGNORE_EOF") == "1" {
		time.Sleep(10 * time.Second)
	}
	os.Exit(0)
}

func helperStdioConfig(name, scope, preset string, enabled bool, extraEnv map[string]string) ExternalMCPConfig {
	env := map[string]string{"GO_WANT_MCP_HELPER": "1"}
	for k, v := range extraEnv {
		env[k] = v
	}
	return ExternalMCPConfig{
		Name:      name,
		Transport: TransportStdio,
		Command:   os.Args[0],
		Args:      []string{"-test.run=TestMCPClientHelperProcess"},
		Env:       env,
		Enabled:   enabled,
		Preset:    preset,
		Scope:     scope,
	}
}

func TestRegistryCaseInsensitiveUniqueSecretIsolationSparseUpdateAndMaskedView(t *testing.T) {
	ws := t.TempDir()
	cfgDir := t.TempDir()
	homeDir := t.TempDir()
	reg := NewClientRegistryWithDirs(ws, cfgDir, homeDir)

	// Pre-populate credentials.json with existing provider API keys to verify zero data loss.
	credPath := filepath.Join(cfgDir, "credentials.json")
	if err := os.WriteFile(credPath, []byte(`{"deepseek":"sk-ds-keep-me","openai":[{"id":"oai-1","api_key":"sk-oai-keep"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Add serena with secret env var + non-secret env var.
	added, err := reg.AddServer(ExternalMCPConfig{
		Name:      "serena",
		Transport: TransportStdio,
		Command:   "uvx",
		Args:      []string{"serena", "start-mcp-server"},
		Env: map[string]string{
			"SERENA_API_TOKEN": "tok-super-secret-123",
			"LOG_LEVEL":        "info",
		},
		Enabled: true,
		Preset:  PresetLazyReadOnly,
		Scope:   ScopeProject,
	})
	if err != nil {
		t.Fatalf("AddServer(serena): %v", err)
	}
	if added.Env["SERENA_API_TOKEN"] != "${TETHER_SECRET:env:SERENA_API_TOKEN}" {
		t.Fatalf("expected placeholder in saved config, got %q", added.Env["SERENA_API_TOKEN"])
	}

	// Case-insensitive duplicate MUST be rejected in same scope and cross scope.
	if _, err := reg.AddServer(ExternalMCPConfig{
		Name:      "Serena",
		Transport: TransportStdio,
		Command:   "uvx",
		Scope:     ScopeProject,
	}); err == nil {
		t.Fatalf("expected case-insensitive duplicate 'Serena' vs 'serena' to fail")
	}
	if _, err := reg.UpsertServer(ExternalMCPConfig{
		Name:      "SERENA",
		Transport: TransportStdio,
		Command:   "uvx",
		Scope:     ScopeGlobal,
	}, SparseUpdate{}); err == nil {
		t.Fatalf("expected cross-scope case-insensitive collision 'SERENA' vs 'serena' to fail")
	}

	// Verify .tether/mcp.json has ZERO raw secret.
	projMCPBytes, err := os.ReadFile(reg.ProjectMCPPath())
	if err != nil {
		t.Fatalf("read project mcp.json: %v", err)
	}
	if strings.Contains(string(projMCPBytes), "tok-super-secret-123") {
		t.Fatalf("project .tether/mcp.json leaked raw secret!\n%s", string(projMCPBytes))
	}
	if !strings.Contains(string(projMCPBytes), "${TETHER_SECRET:env:SERENA_API_TOKEN}") {
		t.Fatalf("expected symbolic placeholder in mcp.json, got:\n%s", string(projMCPBytes))
	}

	// Verify credentials.json is 0600, contains mcp_secrets, AND preserved provider keys.
	cInfo, err := os.Stat(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if cInfo.Mode().Perm() != 0o600 {
		t.Fatalf("credentials.json mode = %04o, want 0600", cInfo.Mode().Perm())
	}
	credBytes, _ := os.ReadFile(credPath)
	credStr := string(credBytes)
	if !strings.Contains(credStr, "sk-ds-keep-me") || !strings.Contains(credStr, "sk-oai-keep") {
		t.Fatalf("credentials.json lost existing provider keys!\n%s", credStr)
	}
	if !strings.Contains(credStr, "tok-super-secret-123") {
		t.Fatalf("credentials.json missing extracted MCP secret!\n%s", credStr)
	}

	// Verify MaskedView masks with "**********".
	masked, err := reg.MaskedView()
	if err != nil || len(masked) != 1 {
		t.Fatalf("MaskedView: len=%d err=%v", len(masked), err)
	}
	if masked[0].Env["SERENA_API_TOKEN"] != SecretMask || masked[0].Env["LOG_LEVEL"] != "info" {
		t.Fatalf("unexpected MaskedView env: %+v", masked[0].Env)
	}

	// Sparse update updating non-secret Args + passing "**********" preserves stored secret!
	updated, err := reg.UpsertServer(ExternalMCPConfig{
		Name:    "serena",
		Scope:   ScopeProject,
		Args:    []string{"serena", "start-mcp-server", "--verbose"},
		Env:     map[string]string{"SERENA_API_TOKEN": SecretMask, "LOG_LEVEL": "debug"},
		Enabled: true,
	}, SparseUpdate{})
	if err != nil {
		t.Fatalf("UpsertServer sparse update: %v", err)
	}
	if updated.Env["LOG_LEVEL"] != "debug" || len(updated.Args) != 3 {
		t.Fatalf("sparse update did not apply non-secret changes: %+v", updated)
	}

	resolved, err := reg.ResolveRuntimeSecrets(updated)
	if err != nil {
		t.Fatalf("ResolveRuntimeSecrets: %v", err)
	}
	if resolved.Env["SERENA_API_TOKEN"] != "tok-super-secret-123" {
		t.Fatalf("sparse update lost stored secret, got %q", resolved.Env["SERENA_API_TOKEN"])
	}

	// Sparse update omitting Env entirely preserves stored secret too.
	if _, err := reg.UpsertServer(ExternalMCPConfig{
		Name:    "serena",
		Scope:   ScopeProject,
		Enabled: false,
	}, SparseUpdate{}); err != nil {
		t.Fatalf("UpsertServer toggle sparse: %v", err)
	}
	srvAfter, ok, _ := reg.GetServer("serena")
	if !ok || srvAfter.Enabled {
		t.Fatalf("expected serena disabled after sparse toggle, got %+v", srvAfter)
	}
	resolved2, err := reg.ResolveRuntimeSecrets(srvAfter)
	if err != nil || resolved2.Env["SERENA_API_TOKEN"] != "tok-super-secret-123" {
		t.Fatalf("secret lost after Env-omitted sparse update: %+v err=%v", resolved2.Env, err)
	}

	// RemoveServer removes from mcp.json and cleans up mcp_secrets while preserving provider keys.
	if err := reg.RemoveServer("serena", ""); err != nil {
		t.Fatalf("RemoveServer: %v", err)
	}
	credAfterBytes, _ := os.ReadFile(credPath)
	if strings.Contains(string(credAfterBytes), "tok-super-secret-123") {
		t.Fatalf("RemoveServer left orphaned secret in credentials.json")
	}
	if !strings.Contains(string(credAfterBytes), "sk-ds-keep-me") {
		t.Fatalf("RemoveServer damaged provider keys in credentials.json")
	}
}

func TestImportFromIDEDisabledDraftsAndSecretExtraction(t *testing.T) {
	ws := t.TempDir()
	cfgDir := t.TempDir()
	homeDir := t.TempDir()

	// Seed agy, claude, cursor configs.
	agyPath := filepath.Join(homeDir, ".gemini", "config", "mcp_config.json")
	_ = os.MkdirAll(filepath.Dir(agyPath), 0o755)
	_ = os.WriteFile(agyPath, []byte(`{
  "mcpServers": {
    "tether": {"command": "/usr/local/bin/tether", "args": ["mcp"]},
    "serena": {"command": "uvx", "args": ["serena", "start"], "env": {"SERENA_API_KEY": "agy-serena-secret", "SERENA_MODE": "fast"}}
  }
}`), 0o644)

	claudePath := filepath.Join(homeDir, ".claude.json")
	_ = os.WriteFile(claudePath, []byte(`{
  "mcpServers": {
    "github-http": {"url": "https://api.example.com/mcp", "headers": {"Authorization": "Bearer claude-gh-secret", "X-Client": "tether"}}
  }
}`), 0o644)

	cursorPath := filepath.Join(homeDir, ".cursor", "mcp.json")
	_ = os.MkdirAll(filepath.Dir(cursorPath), 0o755)
	_ = os.WriteFile(cursorPath, []byte(`{
  "mcpServers": {
    "serena": {"command": "uvx", "args": ["serena-dup"]},
    "linear-sse": {"serverUrl": "https://mcp.linear.app/sse", "headers": {"X-API-Key": "cursor-linear-secret"}}
  }
}`), 0o644)

	reg := NewClientRegistryWithDirs(ws, cfgDir, homeDir)
	res, err := reg.ImportFromIDE("all", ScopeGlobal)
	if err != nil {
		t.Fatalf("ImportFromIDE: %v", err)
	}
	if len(res.Imported) != 3 {
		t.Fatalf("expected 3 imported servers (serena, github-http, linear-sse), got %+v", res)
	}

	servers, err := reg.ListServers()
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	for _, s := range servers {
		if s.Enabled {
			t.Errorf("imported server %q MUST be disabled draft (Enabled: false)", s.Name)
		}
		if s.Name == "serena" && s.Preset != PresetLazyReadOnly {
			t.Errorf("imported serena must default to lazy_readonly preset, got %q", s.Preset)
		}
		if s.Name == "linear-sse" && s.Transport != TransportSSE {
			t.Errorf("imported /sse URL should infer sse transport, got %q", s.Transport)
		}
	}

	globalBytes, _ := os.ReadFile(reg.GlobalMCPPath())
	for _, leak := range []string{"agy-serena-secret", "claude-gh-secret", "cursor-linear-secret"} {
		if strings.Contains(string(globalBytes), leak) {
			t.Fatalf("global mcp.json leaked imported secret %q!", leak)
		}
	}
	credBytes, _ := os.ReadFile(reg.CredentialsPath())
	for _, sec := range []string{"agy-serena-secret", "claude-gh-secret", "cursor-linear-secret"} {
		if !strings.Contains(string(credBytes), sec) {
			t.Fatalf("credentials.json missing extracted secret %q!", sec)
		}
	}
}

func TestStdioLazySpawnSerenaPresetFilterAndCleanClose(t *testing.T) {
	ws := t.TempDir()
	cfgDir := t.TempDir()
	homeDir := t.TempDir()
	reg := NewClientRegistryWithDirs(ws, cfgDir, homeDir)

	disabledSpawnLog := filepath.Join(t.TempDir(), "disabled.spawned")
	if _, err := reg.AddServer(helperStdioConfig("idle-server", ScopeProject, PresetFull, false, map[string]string{
		"HELPER_SPAWN_LOG": disabledSpawnLog,
	})); err != nil {
		t.Fatal(err)
	}

	serenaSpawnLog := filepath.Join(t.TempDir(), "serena.spawned")
	if _, err := reg.AddServer(helperStdioConfig("serena", ScopeProject, PresetLazyReadOnly, true, map[string]string{
		"SERENA_API_TOKEN": "stdio-secret-xyz",
		"HELPER_SPAWN_LOG": serenaSpawnLog,
	})); err != nil {
		t.Fatal(err)
	}

	mgr := NewClientManagerFromRegistry(reg)
	defer mgr.Close()

	// Lazy spawn check: constructor spawns ZERO processes!
	if conns := mgr.ActiveConnections(); len(conns) != 0 {
		t.Fatalf("expected 0 connections before use, got %v", conns)
	}
	if _, err := os.Stat(serenaSpawnLog); !os.IsNotExist(err) {
		t.Fatalf("serena spawned eagerly before ListEnabledTools/TestServer")
	}

	// ListEnabledTools spawns ONLY enabled serena, NEVER disabled idle-server.
	tools, err := mgr.ListEnabledTools(t.Context())
	if err != nil {
		t.Fatalf("ListEnabledTools: %v", err)
	}
	if _, err := os.Stat(disabledSpawnLog); !os.IsNotExist(err) {
		t.Fatalf("disabled server MUST NOT spawn subprocess!")
	}
	if _, err := os.Stat(serenaSpawnLog); err != nil {
		t.Fatalf("enabled serena should have spawned on ListEnabledTools: %v", err)
	}

	// Verify ONLY the 5 SerenaReadOnlySemanticTools are exposed out of 10 raw tools!
	if len(tools) != 5 {
		t.Fatalf("expected 5 lazy_readonly serena tools, got %d: %+v", len(tools), tools)
	}
	gotOrig := map[string]bool{}
	for _, tl := range tools {
		gotOrig[tl.OriginalName] = true
		if !strings.HasPrefix(tl.Name, "mcp_serena__") || len(tl.Name) > 64 {
			t.Errorf("invalid namespaced tool name %q", tl.Name)
		}
	}
	for _, want := range SerenaReadOnlySemanticTools {
		if !gotOrig[want] {
			t.Errorf("missing expected SerenaReadOnlySemanticTool %q", want)
		}
	}
	if gotOrig["replace_symbol_body"] || gotOrig["delete_symbol"] || gotOrig["run_shell"] {
		t.Fatalf("lazy_readonly leaked mutating tools: %+v", gotOrig)
	}

	// Invoking allowed read-only tool injects secret from credentials.json cleanly.
	out, isErr := mgr.CallTool(t.Context(), "mcp_serena__find_symbol", map[string]any{"name_path": "Runner"})
	if isErr || !strings.Contains(out, "token=stdio-secret-xyz") || !strings.Contains(out, "called=find_symbol") {
		t.Fatalf("CallTool(find_symbol) failed: isErr=%v out=%q", isErr, out)
	}

	// Direct invocation of filtered-out symbol-write tool MUST fail closed!
	if denyOut, denyErr := mgr.CallTool(t.Context(), "mcp_serena__replace_symbol_body", map[string]any{"name_path": "Runner"}); !denyErr {
		t.Fatalf("expected replace_symbol_body to be blocked under lazy_readonly, got %q", denyOut)
	}

	// TestServer on disabled draft server works and closes its temporary process cleanly.
	testRes, err := mgr.TestServer(t.Context(), "idle-server")
	if err != nil || !testRes.OK || testRes.RawToolCount != 10 {
		t.Fatalf("TestServer(idle-server): err=%v res=%+v", err, testRes)
	}

	// Test clean Close() even when child ignores stdin EOF (process group kill + reap).
	if _, err := reg.AddServer(helperStdioConfig("stubborn", ScopeProject, PresetFull, true, map[string]string{
		"HELPER_IGNORE_EOF": "1",
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.EnsureConnected(t.Context(), "stubborn"); err != nil {
		t.Fatalf("EnsureConnected(stubborn): %v", err)
	}
	closeStart := time.Now()
	if err := mgr.Close(); err != nil {
		t.Fatalf("mgr.Close(): %v", err)
	}
	if time.Since(closeStart) > 3*time.Second {
		t.Fatalf("mgr.Close() took too long (%v) on EOF-ignoring process", time.Since(closeStart))
	}
	if len(mgr.ActiveConnections()) != 0 {
		t.Fatalf("expected 0 active connections after Close()")
	}
}

func TestHTTPAndSSEResponsesBearerAuthAndSSRFValidation(t *testing.T) {
	// SSRF / scheme checks
	badURLs := []string{
		"file:///etc/passwd",
		"ftp://example.com/mcp",
		"http://169.254.169.254/latest/meta-data",
		"http://metadata.google.internal/computeMetadata/v1/",
		"https://user:pass@example.com/mcp",
		"http://0.0.0.0:8080/mcp",
	}
	for _, u := range badURLs {
		if _, err := ValidateServerURL(u); err == nil {
			t.Errorf("ValidateServerURL(%q) expected error, got nil", u)
		}
	}
	warns, err := ValidateServerURL("http://127.0.0.1:9000/mcp")
	if err != nil || len(warns) == 0 {
		t.Fatalf("expected localhost URL valid with warning, got err=%v warns=%v", err, warns)
	}

	// Mock HTTP JSON + SSE MCP server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer http-secret-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "remote", "version": "1"}}
		case "tools/list":
			result = map[string]any{
				"tools": []map[string]any{
					{"name": "remote_ping", "description": "Ping remote", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"msg": map[string]any{"type": "string"}}}},
				},
			}
		case "tools/call":
			result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("sse-ok:%v", req.Params.Arguments["msg"])}},
			}
		}

		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		if strings.HasSuffix(r.URL.Path, "/sse") {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", string(payload))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	ws := t.TempDir()
	cfgDir := t.TempDir()
	reg := NewClientRegistryWithDirs(ws, cfgDir, t.TempDir())

	for _, tc := range []struct {
		name      string
		url       string
		transport string
	}{
		{"remote-json", srv.URL + "/mcp", TransportHTTP},
		{"remote-sse", srv.URL + "/sse", TransportSSE},
	} {
		if _, err := reg.AddServer(ExternalMCPConfig{
			Name:      tc.name,
			Transport: tc.transport,
			URL:       tc.url,
			Headers:   map[string]string{"Authorization": "Bearer http-secret-token"},
			Enabled:   true,
			Scope:     ScopeProject,
		}); err != nil {
			t.Fatalf("AddServer(%s): %v", tc.name, err)
		}
	}

	mgr := NewClientManagerFromRegistry(reg)
	defer mgr.Close()

	tools, err := mgr.ListEnabledTools(t.Context())
	if err != nil || len(tools) != 2 {
		t.Fatalf("ListEnabledTools HTTP+SSE: err=%v len=%d", err, len(tools))
	}

	for _, toolName := range []string{"mcp_remote-json__remote_ping", "mcp_remote-sse__remote_ping"} {
		out, isErr := mgr.CallTool(t.Context(), toolName, map[string]any{"msg": "hello"})
		if isErr || out != "sse-ok:hello" {
			t.Fatalf("CallTool(%s) = (%q, %v), want sse-ok:hello", toolName, out, isErr)
		}
	}
}

func TestEvaluateSerenaSuggestionTriggersAndStatus(t *testing.T) {
	// 1. Small Go workspace in code mode without cross-file refactor -> NOT recommended.
	smallGo := t.TempDir()
	_ = os.WriteFile(filepath.Join(smallGo, "go.mod"), []byte("module example.com/small\ngo 1.26\n"), 0o644)
	_ = os.WriteFile(filepath.Join(smallGo, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644)
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	sugg := EvaluateSerenaSuggestion(smallGo, "code", false)
	if sugg.Recommended || sugg.Recommendation != "" || sugg.SerenaStatus != SerenaStatusNotConfigured {
		t.Fatalf("small Go repo should not suggest serena, got %+v", sugg)
	}

	// 2. Plan mode or cross-file refactor triggers suggestion + reflects disabled_draft / enabled status.
	suggPlan := EvaluateSerenaSuggestion(smallGo, "plan", false)
	if !suggPlan.Recommended || suggPlan.Recommendation == "" || suggPlan.SuggestedPreset != PresetLazyReadOnly {
		t.Fatalf("plan mode should suggest serena, got %+v", suggPlan)
	}
	suggRefactor := EvaluateSerenaSuggestion(smallGo, "code", true)
	if !suggRefactor.Recommended || suggRefactor.Recommendation == "" {
		t.Fatalf("cross-file refactor should suggest serena, got %+v", suggRefactor)
	}

	reg := NewClientRegistry(smallGo)
	if _, err := reg.AddServer(ExternalMCPConfig{
		Name:      "serena",
		Transport: TransportStdio,
		Command:   "uvx",
		Args:      []string{"serena"},
		Enabled:   false,
		Scope:     ScopeProject,
	}); err != nil {
		t.Fatal(err)
	}
	if s := EvaluateSerenaSuggestion(smallGo, "plan", true); s.SerenaStatus != SerenaStatusDisabledDraft {
		t.Fatalf("expected disabled_draft status, got %+v", s)
	}
	if _, err := reg.SetServerEnabled("serena", true); err != nil {
		t.Fatal(err)
	}
	if s := EvaluateSerenaSuggestion(smallGo, "plan", true); s.SerenaStatus != SerenaStatusEnabled {
		t.Fatalf("expected enabled status, got %+v", s)
	}

	// 3. > 250 eligible source files triggers suggestion.
	bigGo := t.TempDir()
	_ = os.WriteFile(filepath.Join(bigGo, "go.mod"), []byte("module example.com/big\ngo 1.26\n"), 0o644)
	for i := 0; i < 255; i++ {
		name := fmt.Sprintf("f_%03d.go", i)
		_ = os.WriteFile(filepath.Join(bigGo, name), []byte("package big\n"), 0o644)
	}
	suggBig := EvaluateSerenaSuggestion(bigGo, "code", false)
	if !suggBig.Recommended || suggBig.FileCount <= 250 || suggBig.Recommendation == "" {
		t.Fatalf(">250 source files should suggest serena, got %+v", suggBig)
	}

	// 4. Non-Go dominant stack from project_settings triggers suggestion.
	tsWS := t.TempDir()
	eng, err := core.NewEngine(tsWS)
	if err != nil {
		t.Fatal(err)
	}
	_ = eng.SetProjectSetting("tech_stack", "typescript,svelte")
	_ = eng.Close()
	suggTS := EvaluateSerenaSuggestion(tsWS, "code", false)
	if !suggTS.Recommended || suggTS.Recommendation == "" || !strings.Contains(suggTS.DominantStack, "typescript") {
		t.Fatalf("non-Go project_settings should suggest serena, got %+v", suggTS)
	}
}

func TestHTTPRedirectSSRFBlockedAndNoSecretLeak(t *testing.T) {
	sinkHit := 0
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sinkHit++
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	ws := t.TempDir()
	cfgDir := t.TempDir()
	reg := NewClientRegistryWithDirs(ws, cfgDir, t.TempDir())
	if _, err := reg.AddServer(ExternalMCPConfig{
		Name:      "redir",
		Transport: TransportHTTP,
		URL:       redirector.URL + "/mcp",
		Headers:   map[string]string{"Authorization": "Bearer super-secret-hdr"},
		Enabled:   true,
		Scope:     ScopeProject,
	}); err != nil {
		t.Fatal(err)
	}

	mgr := NewClientManagerFromRegistry(reg)
	defer mgr.Close()

	res, err := mgr.TestServer(t.Context(), "redir")
	if err == nil || res.OK {
		t.Fatalf("expected HTTP redirect on MCP POST to fail, got err=%v res=%+v", err, res)
	}
	if sinkHit != 0 {
		t.Fatalf("redirect target sink was contacted %d time(s); secret header may have leaked!", sinkHit)
	}
}

func TestStdioLargeReplyAndOversizeContentLengthRejected(t *testing.T) {
	ws := t.TempDir()
	cfgDir := t.TempDir()
	reg := NewClientRegistryWithDirs(ws, cfgDir, t.TempDir())

	if _, err := reg.AddServer(helperStdioConfig("serena-big", ScopeProject, PresetLazyReadOnly, true, map[string]string{
		"HELPER_LARGE_REPLY": "1",
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.AddServer(helperStdioConfig("serena-huge-cl", ScopeProject, PresetLazyReadOnly, true, map[string]string{
		"HELPER_OVERSIZE_CL": "1",
	})); err != nil {
		t.Fatal(err)
	}

	mgr := NewClientManagerFromRegistry(reg)
	defer mgr.Close()

	tools, err := mgr.ListEnabledTools(t.Context())
	if err != nil || len(tools) == 0 {
		t.Fatalf("ListEnabledTools: err=%v len=%d", err, len(tools))
	}

	// 128KB > 64KB bufio buffer succeeds cleanly!
	out, isErr := mgr.CallTool(t.Context(), "mcp_serena-big__find_symbol", map[string]any{"name": "X"})
	if isErr || len(out) < 128*1024 || !strings.HasPrefix(out, "LARGE:") {
		t.Fatalf("expected 128KB stdio reply intact, got isErr=%v len=%d", isErr, len(out))
	}

	// Oversize Content-Length (>16 MiB) fails closed and closes transport cleanly.
	errOut, errFlag := mgr.CallTool(t.Context(), "mcp_serena-huge-cl__find_symbol", map[string]any{"name": "X"})
	if !errFlag {
		t.Fatalf("expected oversize Content-Length to fail, got %q", errOut)
	}
}

func TestTruncatedNamespacedToolNameCollisionDisambiguation(t *testing.T) {
	longSrv := strings.Repeat("server_segment_", 3) // 45 chars
	n1 := NamespacedToolName(longSrv, "very_long_tool_name_alpha")
	n2 := NamespacedToolName(longSrv, "very_long_tool_name_bravo")
	if len(n1) > maxNamespacedToolLen || len(n2) > maxNamespacedToolLen {
		t.Fatalf("expected <=64 chars, got len(n1)=%d len(n2)=%d", len(n1), len(n2))
	}
	if n1 == n2 {
		t.Fatalf("expected distinct hashed namespaced names for truncated tools, both = %q", n1)
	}

	used := map[string]ExternalTool{n1: {Name: n1}}
	if dup := uniqueNamespacedName(used, longSrv, "very_long_tool_name_alpha"); dup == n1 || len(dup) > maxNamespacedToolLen {
		t.Fatalf("uniqueNamespacedName did not disambiguate collision cleanly: %q vs %q", dup, n1)
	}
}

func TestCredentialsCorruptFailSafeAndSecretMaskAndEmptyClear(t *testing.T) {
	ws := t.TempDir()
	cfgDir := t.TempDir()
	reg := NewClientRegistryWithDirs(ws, cfgDir, t.TempDir())

	// 1. Mask sentinel without existing secret MUST fail cleanly.
	if _, err := reg.UpsertServer(ExternalMCPConfig{
		Name:      "maskfail",
		Transport: TransportStdio,
		Command:   "echo",
		Env:       map[string]string{"API_KEY": SecretMask},
		Scope:     ScopeProject,
	}, SparseUpdate{}); err == nil {
		t.Fatalf("expected SecretMask without existing secret to return error")
	}

	// 2. Save real secret, then clear it via empty string "" -> deletes secret & placeholder.
	if _, err := reg.AddServer(ExternalMCPConfig{
		Name:      "clearable",
		Transport: TransportStdio,
		Command:   "echo",
		Env:       map[string]string{"API_KEY": "secret-val", "MODE": "ro"},
		Scope:     ScopeProject,
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := reg.UpsertServer(ExternalMCPConfig{
		Name:    "clearable",
		Scope:   ScopeProject,
		Env:     map[string]string{"API_KEY": "", "MODE": "ro"},
		Enabled: true,
	}, SparseUpdate{})
	if err != nil {
		t.Fatalf("clearing secret via empty string failed: %v", err)
	}
	if _, stillHas := updated.Env["API_KEY"]; stillHas {
		t.Fatalf("expected empty-string secret key removed from mcp.json, got %+v", updated.Env)
	}
	credBytes, _ := os.ReadFile(reg.CredentialsPath())
	if strings.Contains(string(credBytes), "secret-val") {
		t.Fatalf("expected secret-val deleted from credentials.json")
	}

	// 3. Corrupt credentials.json MUST NOT be silently overwritten!
	corrupt := "{broken-json"
	if err := os.WriteFile(reg.CredentialsPath(), []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.AddServer(ExternalMCPConfig{
		Name:      "wont-clobber",
		Transport: TransportStdio,
		Command:   "echo",
		Env:       map[string]string{"API_KEY": "new-secret"},
		Scope:     ScopeProject,
	}); err == nil {
		t.Fatalf("expected corrupt credentials.json to fail safe without overwriting")
	}
	gotAfter, _ := os.ReadFile(reg.CredentialsPath())
	if string(gotAfter) != corrupt {
		t.Fatalf("corrupt credentials.json was overwritten! got %q", string(gotAfter))
	}
}
