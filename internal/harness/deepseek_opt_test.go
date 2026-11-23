package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/croosparties/tether/internal/core"
	"github.com/croosparties/tether/internal/hooks"
)

func TestDeepSeekOptimizationAndTokenEfficiency(t *testing.T) {
	dir := t.TempDir()
	engine, err := core.NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer engine.Close()

	_ = engine.SetProjectSetting("tech_stack", "go")
	_ = engine.SetProjectSetting("build_cmd", "go build ./...")
	_ = engine.SetProjectSetting("test_cmd", "go test ./...")

	// 1. Verify Deterministic Prefix Stability for DeepSeek 64-Token Context Caching
	h := hooks.NewHandler(engine)
	_ = engine.SetProjectSetting("current_turn", "20261106T170001.000000000")
	in1, _ := json.Marshal(hooks.HookInput{Prompt: "analyze architecture", AgentType: "main"})
	raw1, _ := h.Handle("PreInvocation", bytes.NewReader(in1))

	// Change volatile current_turn and re-run multiple times to ensure map order never scrambles prefix
	_ = engine.SetProjectSetting("current_turn", "20261106T170599.999999999")
	in2, _ := json.Marshal(hooks.HookInput{Prompt: "summarize findings", AgentType: "main"})
	raw2, _ := h.Handle("PreInvocation", bytes.NewReader(in2))

	var out1, out2 hooks.HookOutput
	_ = json.Unmarshal([]byte(raw1), &out1)
	_ = json.Unmarshal([]byte(raw2), &out2)

	msg1 := out1.InjectSteps[0].EphemeralMessage
	msg2 := out2.InjectSteps[0].EphemeralMessage

	// Calculate shared static prefix length before Task Router
	sharedPrefix := 0
	minLen := len(msg1)
	if len(msg2) < minLen {
		minLen = len(msg2)
	}
	for i := 0; i < minLen; i++ {
		if msg1[i] != msg2[i] {
			break
		}
		sharedPrefix++
	}
	prefixRatio := float64(sharedPrefix) * 100.0 / float64(minLen)
	t.Logf("[DeepSeek Prefix Cache] Shared deterministic system prefix: %d / %d bytes (%.1f%% cacheable prefix)", sharedPrefix, minLen, prefixRatio)
	if prefixRatio < 85.0 {
		t.Fatalf("expected >=85%% deterministic prefix stability for DeepSeek Context Caching, got %.1f%%", prefixRatio)
	}

	// 2. Measure Sub-Agent Tool Schema Token Savings (6 read-only tools vs all MCP tools)
	runner := NewRunner(engine, Config{Provider: "deepseek", Model: "deepseek-chat", APIKey: "sk-test"})
	allTools := runner.mcp.ListTools()
	allToolsJSON, _ := json.Marshal(allTools)

	subagentAllowedTools := map[string]bool{
		"read_file":    true,
		"kb_search":    true,
		"kb_status":    true,
		"session_load": true,
		"todo_list":    true,
		"run_command":  true,
	}
	var filteredTools []interface{}
	for _, tool := range allTools {
		if subagentAllowedTools[tool.Name] {
			filteredTools = append(filteredTools, tool)
		}
	}
	filteredJSON, _ := json.Marshal(filteredTools)

	oldEstTokens := len(allToolsJSON) / 4
	newEstTokens := len(filteredJSON) / 4
	savedPct := (float64(oldEstTokens-newEstTokens) * 100.0) / float64(oldEstTokens)
	t.Logf("[Sub-Agent Tool Schema] Before: %d tools (~%d tokens/step) -> After: %d tools (~%d tokens/step) | Saved: %.1f%% per API call",
		len(allTools), oldEstTokens, len(filteredTools), newEstTokens, savedPct)
	if savedPct < 50.0 {
		t.Fatalf("expected at least 50%% tool schema token reduction for subagents, got %.1f%%", savedPct)
	}

	// 3. Verify DeepSeek-Reasoner Active Tool-Loop reasoning_content Preservation vs Old Turn Compaction
	longToolOutput := strings.Repeat("package tui // line of code\n", 120) // ~3360 chars
	conversation := []chatMessage{
		{Role: "system", Content: "You are Main Orchestrator."},
		// Completed Turn 1 (older user turn: reasoning_content MUST be stripped)
		{Role: "user", Content: "Turn 1 question"},
		{Role: "assistant", ReasoningContent: strings.Repeat("Old thinking trace from turn 1. ", 40), Content: "Turn 1 answer"},
		// Active Turn 2 (current user turn with 3 consecutive tool-call steps: reasoning_content MUST be preserved!)
		{Role: "user", Content: "Turn 2 multi-step tool task"},
		{
			Role:             "assistant",
			ReasoningContent: "Step 1 active CoT trace that DeepSeek API requires for tool continuity.",
			ToolCalls:        []toolCall{{ID: "c1", Type: "function", Function: functionCall{Name: "kb_search", Arguments: `{"query":"tui"}`}}},
		},
		{Role: "tool", ToolCallID: "c1", Content: longToolOutput},
		{
			Role:             "assistant",
			ReasoningContent: "Step 2 active CoT trace.",
			ToolCalls:        []toolCall{{ID: "c2", Type: "function", Function: functionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}}},
		},
		{Role: "tool", ToolCallID: "c2", Content: "file a"},
		{
			Role:             "assistant",
			ReasoningContent: "Step 3 active CoT trace.",
			ToolCalls:        []toolCall{{ID: "c3", Type: "function", Function: functionCall{Name: "read_file", Arguments: `{"path":"b.go"}`}}},
		},
		{Role: "tool", ToolCallID: "c3", Content: "file b"},
	}

	rawBefore, _ := json.Marshal(conversation)
	compacted := compactMessagesWithLimits(conversation, 2, 800)
	rawAfter, _ := json.Marshal(compacted)

	// Old completed user turn (index 2) must have ReasoningContent stripped
	if compacted[2].ReasoningContent != "" {
		t.Fatalf("expected old user turn reasoning_content to be stripped, got %q", compacted[2].ReasoningContent)
	}
	// Active user turn Step 1 (index 4) has ToolCalls and is after lastUserIdx (index 3), so its ReasoningContent MUST be preserved!
	if compacted[4].ReasoningContent == "" {
		t.Fatalf("expected active turn Step 1 reasoning_content with tool_calls to be preserved for deepseek-reasoner")
	}
	compactionSavings := (float64(len(rawBefore)-len(rawAfter)) * 100.0) / float64(len(rawBefore))
	t.Logf("[Context Compaction] Payload before: %d bytes (~%d tokens) -> After: %d bytes (~%d tokens) | Saved: %.1f%%",
		len(rawBefore), len(rawBefore)/4, len(rawAfter), len(rawAfter)/4, compactionSavings)

	// 4. Verify DeepSeek API Wire Format (temperature=0.0 for deepseek-chat, no temperature for deepseek-reasoner, and cache hit telemetry)
	var capturedChatReq, capturedReasonerReq map[string]interface{}
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if callCount == 1 {
			capturedChatReq = body
		} else {
			capturedReasonerReq = body
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]interface{}{"role": "assistant", "content": "OK"}},
			},
			"usage": map[string]interface{}{
				"prompt_cache_hit_tokens":  1920,
				"prompt_cache_miss_tokens": 128,
			},
		})
	}))
	defer srv.Close()

	chatRunner := NewRunner(engine, Config{
		Provider: "deepseek",
		Model:    "deepseek-chat",
		BaseURL:  srv.URL,
		APIKey:   "sk-test",
	})
	_, err = chatRunner.RunSideChat(context.Background(), "quick question")
	if err != nil {
		t.Fatalf("RunSideChat: %v", err)
	}
	if _, hasTools := capturedChatReq["tools"]; hasTools {
		t.Fatalf("expected /btw side-chat to omit 'tools' field completely")
	}
	if temp, ok := capturedChatReq["temperature"].(float64); !ok || temp != 0.0 {
		t.Fatalf("expected deepseek-chat temperature=0.0, got %v", capturedChatReq["temperature"])
	}

	reasonerRunner := NewRunner(engine, Config{
		Provider: "deepseek",
		Model:    "deepseek-reasoner",
		BaseURL:  srv.URL,
		APIKey:   "sk-test",
	})
	_, err = reasonerRunner.RunSideChat(context.Background(), "reasoner check")
	if err != nil {
		t.Fatalf("RunSideChat reasoner: %v", err)
	}
	if _, hasTemp := capturedReasonerReq["temperature"]; hasTemp {
		t.Fatalf("expected deepseek-reasoner to omit unsupported temperature parameter")
	}
	if maxTok, ok := capturedReasonerReq["max_tokens"].(float64); !ok || int(maxTok) != 8192 {
		t.Fatalf("expected deepseek-reasoner max_tokens=8192, got %v", capturedReasonerReq["max_tokens"])
	}
	status := reasonerRunner.StatusSummary()
	t.Logf("[Cache Telemetry] StatusSummary output: %s", status)
	if !strings.Contains(status, "Cache Hit: 1920/2048 (93%)") {
		t.Fatalf("expected cache hit telemetry in StatusSummary, got: %s", status)
	}
}
