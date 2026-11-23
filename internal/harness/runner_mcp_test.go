package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/mcp"
	"github.com/crossparties/tether/internal/safety"
)

func TestHarnessExternalMCPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HARNESS_MCP_HELPER") != "1" {
		return
	}
	if logPath := strings.TrimSpace(os.Getenv("HELPER_SPAWN_LOG")); logPath != "" {
		_ = os.WriteFile(logPath, []byte("spawned\n"), 0o644)
	}

	mode := os.Getenv("HELPER_SERVER_MODE")
	var tools []map[string]any
	if mode == "custom" {
		tools = []map[string]any{
			{
				"name":        "write_remote",
				"description": "Mutating external MCP tool",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "data": map[string]any{"type": "string"}}},
			},
			{
				"name":        "delete_remote",
				"description": "Destructive external MCP tool",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
			},
		}
	} else {
		tools = []map[string]any{
			{
				"name":        "find_symbol",
				"description": "Find symbol semantically",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"relative_path": map[string]any{"type": "string"}, "name": map[string]any{"type": "string"}}},
			},
			{
				"name":        "replace_symbol_body",
				"description": "Rewrite symbol body",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"relative_path": map[string]any{"type": "string"}}},
			},
		}
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "harness-helper", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": tools}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			b, _ := json.Marshal(p.Arguments)
			result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("helper:%s:%s", p.Name, string(b))}},
			}
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID), "result": result})
		fmt.Println(string(out))
	}
	_ = scanner.Err()
	os.Exit(0)
}

func harnessMCPHelperCfg(name, mode string, enabled bool, spawnLog string) mcp.ExternalMCPConfig {
	env := map[string]string{
		"GO_WANT_HARNESS_MCP_HELPER": "1",
		"HELPER_SERVER_MODE":         mode,
	}
	if spawnLog != "" {
		env["HELPER_SPAWN_LOG"] = spawnLog
	}
	preset := mcp.PresetFull
	if strings.EqualFold(name, "serena") {
		preset = mcp.PresetLazyReadOnly
	}
	return mcp.ExternalMCPConfig{
		Name:      name,
		Transport: mcp.TransportStdio,
		Command:   os.Args[0],
		Args:      []string{"-test.run=TestHarnessExternalMCPHelperProcess"},
		Env:       env,
		Enabled:   enabled,
		Preset:    preset,
		Scope:     mcp.ScopeProject,
	}
}

func TestRunnerSessionToolFreezeAndLazySpawn(t *testing.T) {
	ws := t.TempDir()
	cfgDir := t.TempDir()
	t.Setenv("TETHER_CONFIG_DIR", cfgDir)
	t.Setenv("HOME", t.TempDir())

	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	reg := mcp.NewClientRegistryWithDirs(ws, cfgDir, t.TempDir())
	disabledLog := filepath.Join(t.TempDir(), "disabled.log")
	if _, err := reg.AddServer(harnessMCPHelperCfg("serena", "serena", true, "")); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.AddServer(harnessMCPHelperCfg("idle", "custom", false, disabledLog)); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var capturedToolsPerTurn []string
	turn := 0
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Tools    json.RawMessage `json:"tools"`
			Messages []chatMessage   `json:"messages"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		mu.Lock()
		turn++
		curTurn := turn
		capturedToolsPerTurn = append(capturedToolsPerTurn, string(body.Tools))
		mu.Unlock()

		var msg chatMessage
		if curTurn == 1 {
			msg = chatMessage{
				Role: "assistant",
				ToolCalls: []toolCall{{
					ID:   "call_ext_1",
					Type: "function",
					Function: functionCall{
						Name:      "mcp_serena__find_symbol",
						Arguments: `{"name":"Main"}`,
					},
				}},
			}
		} else {
			msg = chatMessage{Role: "assistant", Content: "Turn complete."}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": msg, "finish_reason": "stop"}},
		})
	}))
	defer llmSrv.Close()

	mgr := mcp.NewClientManagerFromRegistry(reg)
	runner := NewRunner(eng, Config{
		Provider:    "deepseek",
		Model:       "deepseek-chat",
		BaseURL:     llmSrv.URL,
		APIKey:      "sk-freeze-test",
		Out:         &bytes.Buffer{},
		ExternalMCP: mgr,
	})
	defer runner.Close()

	// Lazy spawn: before first Run, zero external MCP processes spawned.
	if conns := mgr.ActiveConnections(); len(conns) != 0 {
		t.Fatalf("expected lazy startup with 0 active MCP connections, got %v", conns)
	}

	out1, err := runner.Run(context.Background(), "Find symbol Main")
	if err != nil {
		t.Fatalf("Turn 1 Run: %v", err)
	}
	if !strings.Contains(out1, "Turn complete") {
		t.Fatalf("unexpected Turn 1 output: %q", out1)
	}
	if _, err := os.Stat(disabledLog); !os.IsNotExist(err) {
		t.Fatalf("disabled MCP server spawned during Runner session!")
	}

	// Mutate MCP config mid-session: enable 'idle' AND add a brand new enabled server 'midadd'.
	if _, err := reg.SetServerEnabled("idle", true); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.AddServer(harnessMCPHelperCfg("midadd", "custom", true, "")); err != nil {
		t.Fatal(err)
	}

	// Turn 2 MUST keep the exact frozen tool schema from Turn 1 for KV-cache integrity!
	if _, err := runner.Run(context.Background(), "Follow up"); err != nil {
		t.Fatalf("Turn 2 Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(capturedToolsPerTurn) < 3 {
		t.Fatalf("expected at least 3 LLM calls across 2 turns, got %d", len(capturedToolsPerTurn))
	}
	toolsTurn1 := capturedToolsPerTurn[0]
	toolsTurn2 := capturedToolsPerTurn[len(capturedToolsPerTurn)-1]
	if !strings.Contains(toolsTurn1, `"mcp_serena__find_symbol"`) {
		t.Fatalf("Turn 1 missing mcp_serena__find_symbol in tools:\n%s", toolsTurn1)
	}
	if strings.Contains(toolsTurn1, `"mcp_serena__replace_symbol_body"`) {
		t.Fatalf("Turn 1 leaked blocked replace_symbol_body tool!")
	}
	if toolsTurn1 != toolsTurn2 {
		t.Fatalf("frozenTools mutated mid-session after MCP toggle!\nTurn1: %s\nTurn2: %s", toolsTurn1, toolsTurn2)
	}
	if strings.Contains(toolsTurn2, "mcp_idle__") || strings.Contains(toolsTurn2, "mcp_midadd__") {
		t.Fatalf("mid-session MCP toggle leaked into active session tools schema!")
	}
}

func TestRunnerExternalMCPSafetyGatingAndReadOnlyCrew(t *testing.T) {
	if r, _ := safety.ClassifyRisk("mcp_custom__write_remote", "main.go"); r != safety.RiskHigh {
		t.Fatalf("ClassifyRisk must classify external MCP tools as RiskHigh, got %s", r)
	}
	if r, _ := safety.ClassifyRisk("mcp_serena__find_symbol", "main.go"); r != safety.RiskHigh {
		t.Fatalf("ClassifyRisk must classify mcp_serena__find_symbol as RiskHigh, got %s", r)
	}

	ws := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws, ".env"), []byte("SECRET=123\n"), 0o600)
	_ = os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644)

	cfgDir := t.TempDir()
	t.Setenv("TETHER_CONFIG_DIR", cfgDir)
	t.Setenv("HOME", t.TempDir())

	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	reg := mcp.NewClientRegistryWithDirs(ws, cfgDir, t.TempDir())
	if _, err := reg.AddServer(harnessMCPHelperCfg("serena", "serena", true, "")); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.AddServer(harnessMCPHelperCfg("custom", "custom", true, "")); err != nil {
		t.Fatal(err)
	}

	mgr := mcp.NewClientManagerFromRegistry(reg)
	r := NewRunner(eng, Config{
		Provider:    "ollama",
		Out:         &bytes.Buffer{},
		ExternalMCP: mgr,
	})
	defer r.Close()

	// Freeze head tools first.
	_, frozen := r.ensureFrozenHead()
	hasCustom := false
	for _, tl := range frozen {
		if tl.Function.Name == "mcp_custom__write_remote" {
			hasCustom = true
		}
	}
	if !hasCustom {
		t.Fatalf("expected mcp_custom__write_remote in frozen tools")
	}

	// 1. Read-only serena tool succeeds on safe path AND "." / "./", while blocking .env path boundaries!
	for _, args := range []string{
		`{"relative_path":"main.go","name":"main"}`,
		`{"relative_path":".","name":"main"}`,
		`{"relative_path":"./","name":"main"}`,
	} {
		res, isErr := r.executeGuardedToolStatusWithCtx(context.Background(), toolCall{
			Function: functionCall{Name: "mcp_serena__find_symbol", Arguments: args},
		}, &bytes.Buffer{})
		if isErr || !strings.Contains(res, "helper:find_symbol") {
			t.Fatalf("expected safe mcp_serena__find_symbol(%s) to succeed, got isErr=%v res=%q", args, isErr, res)
		}
	}

	for _, badArgs := range []string{
		`{"relative_path":".env","name":"SECRET"}`,
		`{"target_path":".env"}`,
		`{"paths":["main.go",".env"]}`,
	} {
		resEnv, isErrEnv := r.executeGuardedToolStatusWithCtx(context.Background(), toolCall{
			Function: functionCall{Name: "mcp_serena__find_symbol", Arguments: badArgs},
		}, &bytes.Buffer{})
		if !isErrEnv || !strings.Contains(strings.ToLower(resEnv), "access denied") {
			t.Fatalf("expected .env path in %s to be denied, got isErr=%v res=%q", badArgs, isErrEnv, resEnv)
		}
	}

	// 2. Mutating external MCP tool MUST be blocked when unapproved (even with DefaultAutoApprovePolicy!).
	r.SetAutoApprovePolicy(r.DefaultAutoApprovePolicy())
	resUnapproved, isErrUnapproved := r.executeGuardedToolStatusWithCtx(context.Background(), toolCall{
		Function: functionCall{Name: "mcp_custom__write_remote", Arguments: `{"path":"main.go","data":"x"}`},
	}, &bytes.Buffer{})
	if !isErrUnapproved || !strings.Contains(resUnapproved, "APPROVAL REQUIRED") {
		t.Fatalf("expected unapproved mutating external MCP tool blocked, got isErr=%v res=%q", isErrUnapproved, resUnapproved)
	}

	// OpWrite on "." MUST still be denied even for approved paths.
	resDotWrite, isErrDotWrite := r.executeGuardedToolStatusWithCtx(context.Background(), toolCall{
		Function: functionCall{Name: "mcp_custom__write_remote", Arguments: `{"path":".","data":"x"}`},
	}, &bytes.Buffer{})
	if !isErrDotWrite || !strings.Contains(strings.ToLower(resDotWrite), "access denied") {
		t.Fatalf("expected OpWrite on '.' denied, got isErr=%v res=%q", isErrDotWrite, resDotWrite)
	}

	// 3. Mutating external MCP tool MUST be blocked in PLAN mode even if AskPolicy would allow!
	_ = eng.SetMode("plan")
	r.SetAskPolicy(func(d mcp.Decision) (mcp.Verdict, bool) {
		return mcp.VerdictOnce, true
	})
	resPlan, isErrPlan := r.executeGuardedToolStatusWithCtx(context.Background(), toolCall{
		Function: functionCall{Name: "mcp_custom__write_remote", Arguments: `{"path":"main.go","data":"x"}`},
	}, &bytes.Buffer{})
	if !isErrPlan || !strings.Contains(resPlan, "PLAN mode") {
		t.Fatalf("expected mutating external MCP tool blocked in PLAN mode, got isErr=%v res=%q", isErrPlan, resPlan)
	}

	// 4. In CODE mode, narrowed VerdictFolder covers ONLY same tool + directory scope!
	_ = eng.SetMode("code")
	_ = os.MkdirAll(filepath.Join(ws, "sub"), 0o755)
	_ = os.MkdirAll(filepath.Join(ws, "other"), 0o755)
	askCount := 0
	r.SetAskPolicy(func(d mcp.Decision) (mcp.Verdict, bool) {
		askCount++
		if askCount == 1 {
			return mcp.VerdictFolder, true
		}
		return mcp.VerdictReject, false
	})
	// First call on sub/a.go triggers AskPolicy -> VerdictFolder grants ext:mcp_custom__write_remote:sub.
	resFolder1, isErrF1 := r.executeGuardedToolStatusWithCtx(context.Background(), toolCall{
		Function: functionCall{Name: "mcp_custom__write_remote", Arguments: `{"path":"sub/a.go","data":"ok"}`},
	}, &bytes.Buffer{})
	if isErrF1 || !strings.Contains(resFolder1, "helper:write_remote") {
		t.Fatalf("expected first VerdictFolder call to succeed, got isErr=%v res=%q", isErrF1, resFolder1)
	}
	// Sibling sub/b.go on same tool reuses folder grant (askCount remains 1).
	resFolder2, isErrF2 := r.executeGuardedToolStatusWithCtx(context.Background(), toolCall{
		Function: functionCall{Name: "mcp_custom__write_remote", Arguments: `{"path":"sub/b.go","data":"ok"}`},
	}, &bytes.Buffer{})
	if isErrF2 || !strings.Contains(resFolder2, "helper:write_remote") || askCount != 1 {
		t.Fatalf("expected sub/b.go covered by folder grant without re-prompt, got isErr=%v askCount=%d res=%q", isErrF2, askCount, resFolder2)
	}
	// Different directory other/c.go MUST ask again (rejected by askCount>1)!
	if _, errOther := r.executeGuardedToolStatusWithCtx(context.Background(), toolCall{
		Function: functionCall{Name: "mcp_custom__write_remote", Arguments: `{"path":"other/c.go","data":"x"}`},
	}, &bytes.Buffer{}); !errOther {
		t.Fatalf("expected folder grant on sub/ NOT to cover other/c.go")
	}
	// Different tool mcp_custom__delete_remote on sub/a.go MUST ask again (NOT covered by write_remote's grant)!
	if _, errOtherTool := r.executeGuardedToolStatusWithCtx(context.Background(), toolCall{
		Function: functionCall{Name: "mcp_custom__delete_remote", Arguments: `{"path":"sub/a.go"}`},
	}, &bytes.Buffer{}); !errOtherTool {
		t.Fatalf("expected folder grant on write_remote NOT to cover delete_remote")
	}

	// 5. Real CreateCrew(..., Mode: "readonly") with alias-named Serena server ("py-lsp" running serena)!
	if _, err := reg.AddServer(mcp.ExternalMCPConfig{
		Name:      "py-lsp",
		Transport: mcp.TransportStdio,
		Command:   os.Args[0],
		Args:      []string{"-test.run=TestHarnessExternalMCPHelperProcess", "--", "serena-alias"},
		Env:       map[string]string{"GO_WANT_HARNESS_MCP_HELPER": "1", "HELPER_SERVER_MODE": "serena"},
		Enabled:   true,
		Preset:    mcp.PresetLazyReadOnly,
		Scope:     mcp.ScopeProject,
	}); err != nil {
		t.Fatal(err)
	}
	r.ClearHistory()
	_, _ = r.ensureFrozenHead()

	var roMu sync.Mutex
	var roToolNames []string
	var roMutateReply string
	roStep := 0
	roLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Tools    []openAITool  `json:"tools"`
			Messages []chatMessage `json:"messages"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		roMu.Lock()
		roStep++
		step := roStep
		if step == 1 {
			for _, tl := range body.Tools {
				roToolNames = append(roToolNames, tl.Function.Name)
			}
		}
		roMu.Unlock()

		var msg chatMessage
		switch step {
		case 1:
			msg = chatMessage{
				Role: "assistant",
				ToolCalls: []toolCall{{
					ID:       "ro_alias_1",
					Type:     "function",
					Function: functionCall{Name: "mcp_py-lsp__find_symbol", Arguments: `{"relative_path":"main.go","name":"main"}`},
				}},
			}
		case 2:
			msg = chatMessage{
				Role: "assistant",
				ToolCalls: []toolCall{{
					ID:       "ro_mut_2",
					Type:     "function",
					Function: functionCall{Name: "mcp_custom__write_remote", Arguments: `{"path":"main.go","data":"pwn"}`},
				}},
			}
		default:
			roMu.Lock()
			roMutateReply = body.Messages[len(body.Messages)-1].Content
			roMu.Unlock()
			msg = chatMessage{Role: "assistant", Content: "readonly audit done"}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": msg, "finish_reason": "stop"}},
		})
	}))
	defer roLLM.Close()

	crewRes, err := r.CreateCrew(context.Background(), CrewCreateRequest{
		Name:    "ro-auditor",
		Mode:    "readonly",
		BaseURL: roLLM.URL,
		APIKey:  "sk-ro",
		Model:   "deepseek-chat",
		Task:    "Inspect via alias serena and try mutating external tool",
	})
	if err != nil {
		t.Fatalf("CreateCrew(readonly): %v", err)
	}
	_ = r.CloseCrew(crewRes.CrewID, "done")

	hasAliasRead := false
	for _, n := range roToolNames {
		if n == "mcp_py-lsp__find_symbol" {
			hasAliasRead = true
		}
		if n == "mcp_custom__write_remote" || n == "mcp_custom__delete_remote" {
			t.Fatalf("readonly crew tools schema exposed mutating external tool %q", n)
		}
	}
	if !hasAliasRead {
		t.Fatalf("readonly crew missing alias serena read-only tool mcp_py-lsp__find_symbol: %v", roToolNames)
	}
	if !strings.Contains(strings.ToLower(roMutateReply), "denied") && !strings.Contains(strings.ToLower(roMutateReply), "read-only") {
		t.Fatalf("expected readonly crew to deny mcp_custom__write_remote, got: %q", roMutateReply)
	}
}

func TestCrossPackageCredentialsInteropAndCLIFlagsPreserveGlobalMCP(t *testing.T) {
	cfgDir := t.TempDir()
	homeDir := t.TempDir()
	globalCfgDir := filepath.Join(homeDir, ".config", "tether")
	t.Setenv("HOME", homeDir)
	t.Setenv("TETHER_CONFIG_DIR", cfgDir)

	// 1. Interop: SaveAPIKey + SaveProviderKeys + mcp.ClientRegistry sharing exact same credentials.json.
	if err := SaveAPIKey("deepseek", "sk-ds-interop"); err != nil {
		t.Fatalf("SaveAPIKey: %v", err)
	}
	reg := mcp.NewClientRegistryWithDirs(t.TempDir(), cfgDir, homeDir)
	if _, err := reg.AddServer(mcp.ExternalMCPConfig{
		Name:      "gh",
		Transport: mcp.TransportHTTP,
		URL:       "https://api.example.com/mcp",
		Headers:   map[string]string{"Authorization": "Bearer mcp-interop-token"},
		Scope:     mcp.ScopeGlobal,
	}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if err := SaveProviderKeys("openai", []ProviderKeyEntry{{ID: "oai-1", APIKey: "sk-oai-interop", Enabled: true}}); err != nil {
		t.Fatalf("SaveProviderKeys: %v", err)
	}

	creds, err := LoadAllCredentials()
	if err != nil || len(creds["deepseek"]) != 1 || len(creds["openai"]) != 1 {
		t.Fatalf("LoadAllCredentials lost provider keys after MCP write: creds=%+v err=%v", creds, err)
	}
	srv, ok, err := reg.GetServer("gh")
	if err != nil || !ok {
		t.Fatalf("GetServer(gh) failed after SaveProviderKeys: ok=%v err=%v", ok, err)
	}
	resolved, err := reg.ResolveRuntimeSecrets(srv)
	if err != nil || resolved.Headers["Authorization"] != "Bearer mcp-interop-token" {
		t.Fatalf("ResolveRuntimeSecrets lost MCP secret after SaveProviderKeys: %+v err=%v", resolved.Headers, err)
	}

	// 2. CLI --api-key / --base-url (explicit flags without TETHER_CONFIG_DIR) MUST preserve global MCP registry!
	t.Setenv("TETHER_CONFIG_DIR", "")
	globalReg := mcp.NewClientRegistryWithDirs("", globalCfgDir, homeDir)
	if _, err := globalReg.AddServer(mcp.ExternalMCPConfig{
		Name:      "global-gh",
		Transport: mcp.TransportHTTP,
		URL:       "https://api.example.com/mcp",
		Headers:   map[string]string{"Authorization": "Bearer global-secret"},
		Enabled:   true,
		Scope:     mcp.ScopeGlobal,
	}); err != nil {
		t.Fatal(err)
	}

	eng, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	runner := NewRunner(eng, Config{
		Provider: "deepseek",
		APIKey:   "sk-explicit-flag",
		BaseURL:  "https://api.deepseek.com/chat/completions",
		Out:      &bytes.Buffer{},
	})
	defer runner.Close()

	gotGlobal, found, err := runner.ExternalMCP().Registry().GetServer("global-gh")
	if err != nil || !found {
		t.Fatalf("NewRunner with explicit APIKey/BaseURL dropped global MCP registry: found=%v err=%v", found, err)
	}
	resolvedGlobal, err := runner.ExternalMCP().Registry().ResolveRuntimeSecrets(gotGlobal)
	if err != nil || resolvedGlobal.Headers["Authorization"] != "Bearer global-secret" {
		t.Fatalf("NewRunner dropped global MCP secret: %+v err=%v", resolvedGlobal.Headers, err)
	}
}
