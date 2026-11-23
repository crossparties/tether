package webui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/harness"
	"github.com/crossparties/tether/internal/mcp"
)

func TestWebUIMCPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_WEBUI_MCP_HELPER") != "1" {
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
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "webui-mcp-helper", "version": "1.0"},
			}
		case "tools/list":
			result = map[string]any{"tools": tools}
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID), "result": result})
		fmt.Println(string(b))
	}
	_ = scanner.Err()
	os.Exit(0)
}

func TestEndToEndMockAIWebFlowApprovalUndoCrewTokensAndMCP(t *testing.T) {
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	ws := t.TempDir()
	cfgPath := filepath.Join(ws, "config.txt")
	workerPath := filepath.Join(ws, "worker.go")
	leasedPath := filepath.Join(ws, "leased.go")
	if err := os.WriteFile(cfgPath, []byte("v0-original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workerPath, []byte("package main\nfunc OldWorker() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leasedPath, []byte("package main\nfunc Leased() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var aiMu sync.Mutex
	aiStep := 0
	var sawRejectReason bool

	mockAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role       string `json:"role"`
				Content    string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		rawMsgs, _ := json.Marshal(body.Messages)
		joined := string(rawMsgs)

		// Handle /btw isolated request cleanly without advancing main mock steps.
		if strings.Contains(joined, "isolated, read-only /btw") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message":       map[string]any{"role": "assistant", "content": "BTW_OK: config uses v2"},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{"prompt_tokens": 40, "completion_tokens": 10},
			})
			return
		}

		aiMu.Lock()
		aiStep++
		step := aiStep
		if strings.Contains(joined, "keep v2 format") {
			sawRejectReason = true
		}
		aiMu.Unlock()

		// Stream deltas on step 1 to exercise live SSE chat.delta!
		if body.Stream && step == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			chunks := []string{
				`data: {"choices":[{"delta":{"content":"Checking "}}]}`,
				`data: {"choices":[{"delta":{"content":"config..."}}]}`,
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tc_1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"config.txt\",\"content\":\"v1-bad\"}"}}]}}]}`,
				`data: {"choices":[{"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":150,"completion_tokens":20,"prompt_cache_hit_tokens":100,"prompt_cache_miss_tokens":50}}`,
				`data: [DONE]`,
			}
			for _, line := range chunks {
				fmt.Fprint(w, line+"\n\n")
				if fl != nil {
					fl.Flush()
				}
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		var msg map[string]any
		finishReason := "stop"
		switch step {
		case 1:
			msg = map[string]any{
				"role":    "assistant",
				"content": "Checking config...",
				"tool_calls": []map[string]any{{
					"id":   "tc_1",
					"type": "function",
					"function": map[string]any{
						"name":      "write_file",
						"arguments": `{"path":"config.txt","content":"v1-bad"}`,
					},
				}},
			}
			finishReason = "tool_calls"
		case 2:
			// After rejection with reason "keep v2 format", propose v2-good.
			msg = map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"id":   "tc_2",
					"type": "function",
					"function": map[string]any{
						"name":      "write_file",
						"arguments": `{"path":"config.txt","content":"v2-good"}`,
					},
				}},
			}
			finishReason = "tool_calls"
		case 3:
			// Approval granted ("once") -> repeat write_file(config.txt, v2-good) so it executes.
			msg = map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"id":   "tc_3",
					"type": "function",
					"function": map[string]any{
						"name":      "write_file",
						"arguments": `{"path":"config.txt","content":"v2-good"}`,
					},
				}},
			}
			finishReason = "tool_calls"
		case 4:
			// Main spawns a Crew with lease_files=["worker.go"] and delegate_critical="true".
			msg = map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"id":   "tc_crew",
					"type": "function",
					"function": map[string]any{
						"name":      "crew_create",
						"arguments": `{"name":"worker-crew","task":"update worker.go","lease_files":"worker.go","delegate_critical":"true"}`,
					},
				}},
			}
			finishReason = "tool_calls"
		case 5:
			// Crew runs write_file(worker.go).
			msg = map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"id":   "tc_crew_w",
					"type": "function",
					"function": map[string]any{
						"name":      "write_file",
						"arguments": `{"path":"worker.go","content":"package main\nfunc NewWorker() {}\n"}`,
					},
				}},
			}
			finishReason = "tool_calls"
		case 6:
			msg = map[string]any{"role": "assistant", "content": "Crew finished worker.go"}
		default:
			msg = map[string]any{"role": "assistant", "content": "All done!"}
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       msg,
				"finish_reason": finishReason,
			}},
			"usage": map[string]any{
				"prompt_tokens":            160,
				"completion_tokens":        15,
				"prompt_cache_hit_tokens":  120,
				"prompt_cache_miss_tokens": 40,
			},
		})
	}))
	defer mockAI.Close()

	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	run := harness.NewRunner(eng, harness.Config{
		Provider: "deepseek",
		Model:    "deepseek-chat",
		BaseURL:  mockAI.URL,
		APIKey:   "sk-e2e-mock",
		Out:      &bytes.Buffer{},
	})
	defer func() { _ = run.Close() }()

	srv, err := NewServer(Config{Engine: eng, Runner: run})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		ts.Close()
	}()

	wc := &testWebClient{
		t:    t,
		base: ts.URL,
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	wc.redeemBootstrap(srv.BootstrapToken())

	// Mark config.txt as guarded so write_file escalates via approval.required!
	mResp := wc.do(http.MethodPatch, "/api/v1/mounts/mode", map[string]any{
		"path":        "config.txt",
		"access_mode": "guarded",
	}, nil)
	mResp.Body.Close()
	if mResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /api/v1/mounts/mode failed: %d", mResp.StatusCode)
	}

	// Open multiplexed SSE stream.
	sseCtx, sseCancel := context.WithCancel(context.Background())
	defer sseCancel()
	sseReq, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, ts.URL+"/api/v1/events", nil)
	sseReq.AddCookie(wc.cookie)
	sseResp, err := (&http.Client{}).Do(sseReq)
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	defer sseResp.Body.Close()

	type sseEvt struct {
		event string
		data  string
	}
	evtCh := make(chan sseEvt, 128)
	go func() {
		sc := bufio.NewScanner(sseResp.Body)
		var cur sseEvt
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if cur.event != "" || cur.data != "" {
					evtCh <- cur
					cur = sseEvt{}
				}
				continue
			}
			if v, ok := strings.CutPrefix(line, "event:"); ok {
				cur.event = strings.TrimSpace(v)
			} else if v, ok := strings.CutPrefix(line, "data:"); ok {
				cur.data = strings.TrimSpace(v)
			}
		}
		_ = sc.Err()
	}()

	waitEvent := func(want string) sseEvt {
		t.Helper()
		deadline := time.After(6 * time.Second)
		for {
			select {
			case ev := <-evtCh:
				if ev.event == want {
					return ev
				}
			case <-deadline:
				t.Fatalf("timed out waiting for SSE event %q", want)
			}
		}
	}

	waitEvent("connected")

	// 1. Trigger chat turn -> SSE chat.delta -> approval.required -> reject -> approval.required -> approve -> chat.done!
	sendResp := wc.do(http.MethodPost, "/api/v1/chat/send", map[string]any{
		"prompt": "Update config.txt and delegate worker.go",
	}, nil)
	sendResp.Body.Close()
	if sendResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/v1/chat/send expected 202, got %d", sendResp.StatusCode)
	}

	deltaEv := waitEvent("chat.delta")
	if !strings.Contains(deltaEv.data, "Checking") && !strings.Contains(deltaEv.data, "config") {
		t.Fatalf("unexpected chat.delta data: %s", deltaEv.data)
	}

	apr1 := waitEvent("approval.required")
	var aprPayload1 struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	_ = json.Unmarshal([]byte(apr1.data), &aprPayload1)
	if aprPayload1.ID == "" || aprPayload1.Path != "config.txt" {
		t.Fatalf("unexpected first approval.required payload: %s", apr1.data)
	}

	// Reject first proposal with explicit reason!
	rejResp := wc.do(http.MethodPost, "/api/v1/chat/approval", map[string]any{
		"id":      aprPayload1.ID,
		"verdict": "reject",
		"reason":  "keep v2 format",
	}, nil)
	rejResp.Body.Close()
	if rejResp.StatusCode != http.StatusOK {
		t.Fatalf("reject approval failed: %d", rejResp.StatusCode)
	}

	// Second proposal arrives; approve once!
	apr2 := waitEvent("approval.required")
	var aprPayload2 struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(apr2.data), &aprPayload2)
	okResp := wc.do(http.MethodPost, "/api/v1/chat/approval", map[string]any{
		"id":      aprPayload2.ID,
		"verdict": "once",
	}, nil)
	okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("approve once failed: %d", okResp.StatusCode)
	}

	doneEv := waitEvent("chat.done")
	if !strings.Contains(doneEv.data, "All done!") {
		t.Fatalf("unexpected chat.done payload: %s", doneEv.data)
	}

	aiMu.Lock()
	gotRejectReason := sawRejectReason
	aiMu.Unlock()
	if !gotRejectReason {
		t.Fatal("expected Mock AI to observe reject reason 'keep v2 format'")
	}

	gotCfg, _ := os.ReadFile(cfgPath)
	if string(gotCfg) != "v2-good" {
		t.Fatalf("expected config.txt to contain v2-good, got %q", string(gotCfg))
	}

	// 2. Undo file guard: manual edit triggers 409 Conflict, then force=true restores!
	if err := os.WriteFile(cfgPath, []byte("manual-user-edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	undoConflict := wc.do(http.MethodPost, "/api/v1/diffs/undo-file", map[string]any{
		"path":  "config.txt",
		"force": false,
	}, nil)
	undoConflict.Body.Close()
	if undoConflict.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on manual edit undo, got %d", undoConflict.StatusCode)
	}
	stillManual, _ := os.ReadFile(cfgPath)
	if string(stillManual) != "manual-user-edit\n" {
		t.Fatalf("409 Conflict should not overwrite disk file, got %q", string(stillManual))
	}

	undoForce := wc.do(http.MethodPost, "/api/v1/diffs/undo-file", map[string]any{
		"path":  "config.txt",
		"force": true,
	}, nil)
	undoForce.Body.Close()
	if undoForce.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on force=true undo, got %d", undoForce.StatusCode)
	}
	restoredCfg, _ := os.ReadFile(cfgPath)
	if string(restoredCfg) != "v0-original\n" {
		t.Fatalf("expected restored v0-original, got %q", string(restoredCfg))
	}

	// 3. Verify GET /api/v1/tokens reports cache_hit_tokens & usage_id.
	tokResp := wc.do(http.MethodGet, "/api/v1/tokens", nil, nil)
	var tokBody struct {
		UsageID        string                          `json:"usage_id"`
		CacheHitTokens int                             `json:"cache_hit_tokens"`
		ByUsageID      map[string]harness.UsageMetrics `json:"by_usage_id"`
	}
	_ = json.NewDecoder(tokResp.Body).Decode(&tokBody)
	tokResp.Body.Close()
	if tokResp.StatusCode != http.StatusOK || tokBody.UsageID != "main" || tokBody.CacheHitTokens == 0 {
		t.Fatalf("unexpected /api/v1/tokens response: status=%d %+v", tokResp.StatusCode, tokBody)
	}
	if m, ok := tokBody.ByUsageID["main"]; !ok || m.CachedTokens == 0 {
		t.Fatalf("expected main usage metrics with CachedTokens > 0, got %+v", tokBody.ByUsageID)
	}

	// 4. Crew list, Crew undo, and manual release-lock escape hatch!
	crewsResp := wc.do(http.MethodGet, "/api/v1/crews?all=1", nil, nil)
	var crewsBody struct {
		Crews []core.CrewSession `json:"crews"`
	}
	_ = json.NewDecoder(crewsResp.Body).Decode(&crewsBody)
	crewsResp.Body.Close()
	if len(crewsBody.Crews) == 0 {
		t.Fatal("expected at least 1 recorded crew session")
	}
	crewID := crewsBody.Crews[0].ID

	crewMsgResp := wc.do(http.MethodGet, "/api/v1/crews/"+crewID+"/messages", nil, nil)
	crewMsgResp.Body.Close()
	if crewMsgResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/crews/%s/messages failed: %d", crewID, crewMsgResp.StatusCode)
	}

	undoCrew := wc.do(http.MethodPost, "/api/v1/diffs/undo-crew", map[string]any{"crew_id": crewID}, nil)
	undoCrew.Body.Close()
	if undoCrew.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/diffs/undo-crew failed: %d", undoCrew.StatusCode)
	}

	if err := eng.Mount().AcquireLeaseLock("crew-stalled", []string{"leased.go"}, 5*time.Minute); err != nil {
		t.Fatalf("AcquireLeaseLock: %v", err)
	}
	for _, cid := range []string{crewID, "crew-stalled"} {
		relResp := wc.do(http.MethodPost, "/api/v1/crews/"+cid+"/release-lock", map[string]any{}, nil)
		relResp.Body.Close()
		if relResp.StatusCode != http.StatusOK {
			t.Fatalf("POST /api/v1/crews/%s/release-lock failed: %d", cid, relResp.StatusCode)
		}
	}
	leases, _ := eng.Mount().ListActiveLeases()
	if len(leases) != 0 {
		t.Fatalf("expected 0 active leases after release-lock, got %+v", leases)
	}

	abortCrewResp := wc.do(http.MethodPost, "/api/v1/crews/"+crewID+"/abort", map[string]any{}, nil)
	abortCrewResp.Body.Close()
	if abortCrewResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/crews/%s/abort failed: %d", crewID, abortCrewResp.StatusCode)
	}

	// Verify diffs, threads, todos, and grants endpoints cleanly.
	for _, ep := range []string{"/api/v1/diffs", "/api/v1/threads", "/api/v1/todos", "/api/v1/grants"} {
		r := wc.do(http.MethodGet, ep, nil, nil)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("GET %s failed: %d", ep, r.StatusCode)
		}
	}
	delGrants := wc.do(http.MethodDelete, "/api/v1/grants", nil, nil)
	delGrants.Body.Close()
	if delGrants.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /api/v1/grants failed: %d", delGrants.StatusCode)
	}

	// 5. /btw + promote + pin + compact.
	btwResp := wc.do(http.MethodPost, "/api/v1/chat/btw", map[string]any{"question": "what format does config use?"}, nil)
	btwBytes, _ := io.ReadAll(btwResp.Body)
	btwResp.Body.Close()
	if btwResp.StatusCode != http.StatusOK || !strings.Contains(string(btwBytes), "BTW_OK") {
		t.Fatalf("POST /api/v1/chat/btw failed: status=%d body=%s", btwResp.StatusCode, string(btwBytes))
	}
	promResp := wc.do(http.MethodPost, "/api/v1/chat/btw/promote", map[string]any{}, nil)
	promResp.Body.Close()
	if promResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/chat/btw/promote failed: %d", promResp.StatusCode)
	}
	pinResp := wc.do(http.MethodPost, "/api/v1/chat/btw/pin", map[string]any{"topic": "config"}, nil)
	pinResp.Body.Close()
	if pinResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/chat/btw/pin failed: %d", pinResp.StatusCode)
	}
	compResp := wc.do(http.MethodPost, "/api/v1/chat/compact", map[string]any{"force": true}, nil)
	compResp.Body.Close()
	if compResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/chat/compact failed: %d", compResp.StatusCode)
	}

	// 6. Full MCP CRUD with secret masking ("**********"), unique check, mandatory fingerprinted test gate, sparse patch, toggle, delete.
	mcpUntested := wc.do(http.MethodPost, "/api/v1/mcp", map[string]any{
		"name":    "untested-server",
		"command": "echo",
		"enabled": true,
	}, nil)
	mcpUntested.Body.Close()
	if mcpUntested.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 rejecting untested enabled=true MCP create, got %d", mcpUntested.StatusCode)
	}

	mcpCreate := wc.do(http.MethodPost, "/api/v1/mcp", map[string]any{
		"name":    "serena",
		"command": os.Args[0],
		"args":    []string{"-test.run=TestWebUIMCPHelperProcess"},
		"env": map[string]string{
			"GO_WANT_WEBUI_MCP_HELPER": "1",
			"SERENA_API_KEY":           "super-secret-xyz",
		},
		"scope":   "project",
		"preset":  "lazy_readonly",
		"enabled": false,
	}, nil)
	createRaw, _ := io.ReadAll(mcpCreate.Body)
	mcpCreate.Body.Close()
	if mcpCreate.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/mcp failed (%d): %s", mcpCreate.StatusCode, string(createRaw))
	}
	if strings.Contains(string(createRaw), "super-secret-xyz") || !strings.Contains(string(createRaw), mcp.SecretMask) {
		t.Fatalf("POST /api/v1/mcp secret masking failure: %s", string(createRaw))
	}

	// Enabling a saved draft before running /api/v1/mcp/test is blocked even without require_test flag.
	mcpPrematureToggle := wc.do(http.MethodPost, "/api/v1/mcp/serena/toggle", map[string]any{
		"enabled": true,
	}, nil)
	mcpPrematureToggle.Body.Close()
	if mcpPrematureToggle.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 rejecting untested MCP toggle, got %d", mcpPrematureToggle.StatusCode)
	}

	// Case-insensitive duplicate rejected 409.
	mcpDup := wc.do(http.MethodPost, "/api/v1/mcp", map[string]any{
		"name":    "Serena",
		"command": "echo",
	}, nil)
	mcpDup.Body.Close()
	if mcpDup.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on duplicate Serena, got %d", mcpDup.StatusCode)
	}

	// GET /api/v1/mcp masks all secrets.
	mcpList := wc.do(http.MethodGet, "/api/v1/mcp", nil, nil)
	listRaw, _ := io.ReadAll(mcpList.Body)
	mcpList.Body.Close()
	if strings.Contains(string(listRaw), "super-secret-xyz") || !strings.Contains(string(listRaw), mcp.SecretMask) {
		t.Fatalf("GET /api/v1/mcp secret masking failure: %s", string(listRaw))
	}

	// Test MCP connection -> 10 raw tools, 5 lazy_readonly active tools.
	mcpTest := wc.do(http.MethodPost, "/api/v1/mcp/test", map[string]any{"name": "serena"}, nil)
	var testRes mcp.ServerTestResult
	_ = json.NewDecoder(mcpTest.Body).Decode(&testRes)
	mcpTest.Body.Close()
	if mcpTest.StatusCode != http.StatusOK || !testRes.OK || testRes.RawToolCount != 10 || testRes.FilteredToolCount != 5 {
		t.Fatalf("POST /api/v1/mcp/test unexpected: status=%d %+v", mcpTest.StatusCode, testRes)
	}

	// Sparse PATCH with "**********" preserves stored secret AND invalidates prior test fingerprint.
	mcpPatch := wc.do(http.MethodPatch, "/api/v1/mcp/serena", map[string]any{
		"env": map[string]string{
			"GO_WANT_WEBUI_MCP_HELPER": "1",
			"SERENA_API_KEY":           mcp.SecretMask,
			"EXTRA_FLAG":               "1",
		},
	}, nil)
	patchRaw, _ := io.ReadAll(mcpPatch.Body)
	mcpPatch.Body.Close()
	if mcpPatch.StatusCode != http.StatusOK || strings.Contains(string(patchRaw), "super-secret-xyz") {
		t.Fatalf("PATCH /api/v1/mcp/serena failed/leaked (%d): %s", mcpPatch.StatusCode, string(patchRaw))
	}

	// Toggle enabled after PATCH changed env MUST fail until re-tested!
	mcpStaleToggle := wc.do(http.MethodPost, "/api/v1/mcp/serena/toggle", map[string]any{
		"enabled": true,
	}, nil)
	mcpStaleToggle.Body.Close()
	if mcpStaleToggle.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 rejecting toggle after config-changing PATCH, got %d", mcpStaleToggle.StatusCode)
	}

	// Re-test updated config -> now toggle enabled succeeds cleanly.
	mcpRetest := wc.do(http.MethodPost, "/api/v1/mcp/test", map[string]any{"name": "serena"}, nil)
	mcpRetest.Body.Close()
	if mcpRetest.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/mcp/test after PATCH failed: %d", mcpRetest.StatusCode)
	}
	mcpToggle := wc.do(http.MethodPost, "/api/v1/mcp/serena/toggle", map[string]any{
		"enabled": true,
	}, nil)
	mcpToggle.Body.Close()
	if mcpToggle.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/mcp/serena/toggle failed: %d", mcpToggle.StatusCode)
	}

	// Modifying env on an already-enabled server without setting enabled=false requires testing modified config first.
	mcpPatchEnabled := wc.do(http.MethodPatch, "/api/v1/mcp/serena", map[string]any{
		"env": map[string]string{
			"GO_WANT_WEBUI_MCP_HELPER": "1",
			"SERENA_API_KEY":           mcp.SecretMask,
			"EXTRA_FLAG":               "2",
		},
	}, nil)
	mcpPatchEnabled.Body.Close()
	if mcpPatchEnabled.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 rejecting untested PATCH while enabled=true, got %d", mcpPatchEnabled.StatusCode)
	}

	reg := mcp.NewClientRegistry(ws)
	srvCfg, ok, _ := reg.GetServer("serena")
	if !ok || !srvCfg.Enabled {
		t.Fatalf("expected serena enabled in registry, got %+v ok=%v", srvCfg, ok)
	}
	resolved, err := reg.ResolveRuntimeSecrets(srvCfg)
	if err != nil || resolved.Env["SERENA_API_KEY"] != "super-secret-xyz" {
		t.Fatalf("stored secret lost across sparse PATCH/toggle: resolved=%+v err=%v", resolved.Env, err)
	}

	mcpDel := wc.do(http.MethodDelete, "/api/v1/mcp/serena", nil, nil)
	mcpDel.Body.Close()
	if mcpDel.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /api/v1/mcp/serena failed: %d", mcpDel.StatusCode)
	}
}
