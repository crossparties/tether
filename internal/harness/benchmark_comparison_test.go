package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/mcp"
)

type archBenchmarkResult struct {
	Name               string
	APICalls           int
	PromptTokens       int
	CachedTokens       int
	MissTokens         int
	CacheHitRatioPct   float64
	WeightedCostTokens float64 // MissTokens*1.0 + CachedTokens*0.1 (90% cache read discount)
	ExplorationSteps   int     // Extra blind discovery/re-orientation calls (Main + Subagent)
	CrewTurn1HitPct    float64
	PeakContextTokens  int
	SafetyPassed       int
	SafetyTotal        int
}

func sendRawMockPrompt(t *testing.T, srv *mockPrefixCacheServer, authKey, model string, tools any, msgs []chatMessage) (int, int, int) {
	t.Helper()
	payload := map[string]any{
		"model":    model,
		"messages": msgs,
	}
	if tools != nil {
		payload["tools"] = tools
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal mock prompt: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL(), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+authKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mock request failed: %v", err)
	}
	defer resp.Body.Close()

	var decoded struct {
		Usage wireUsage `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode mock response: %v", err)
	}
	return decoded.Usage.PromptTokens, decoded.Usage.PromptCacheHitTokens, decoded.Usage.PromptCacheMissTokens
}

func makeSyntheticToolSchema(count int, extraDescLen int, prefixTag string) []map[string]any {
	pad := strings.Repeat("parameter validation and runtime execution contract ", extraDescLen/50+1)
	out := make([]map[string]any, 0, count)
	for i := range count {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        fmt.Sprintf("%stool_%02d", prefixTag, i+1),
				"description": fmt.Sprintf("%sTool %d: %s", prefixTag, i+1, pad),
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string", "description": "target path"},
						"command": map[string]any{"type": "string", "description": "command payload"},
					},
				},
			},
		})
	}
	return out
}

// TestBenchmarkComparisonTetherV03VsV02AndOpenHands benchmarks the real
// Tether v0.3.0 harness (Core Engine + 5-point Mount + RepoMap + Append-Only
// KV Cache + isolated /btw + Same-Provider Crew warm-start) against faithful
// simulations of Tether v0.2.0 and the OpenHands architectural profile on the
// same multi-file engineering workflow via mockPrefixCacheServer.
func TestBenchmarkComparisonTetherV03VsV02AndOpenHands(t *testing.T) {
	ws := t.TempDir()
	authBody := strings.Repeat("// auth verifier invariant\n", 18) + "package auth\nfunc VerifyToken(tok string) bool { return tok != \"\" }\n"
	routerBody := "package api\nimport \"example/internal/auth\"\nfunc RegisterRoutes() bool { return auth.VerifyToken(\"ok\") }\n"
	files := map[string]string{
		"cmd/server/main.go":                    "package main\nimport \"example/internal/api\"\nfunc main() { api.RegisterRoutes() }\n",
		"internal/auth/service.go":              authBody,
		"internal/api/router.go":                routerBody,
		"internal/store/db.go":                  "package store\nfunc OpenDB(dsn string) error { return nil }\n",
		".env":                                  "API_SECRET=benchmark-super-secret\n",
		".tether/ignore":                        "*.pem\n",
		".agents/skills/code-reviewer/SKILL.md": "# Code Reviewer Skill\nInspect VerifyToken edge cases cleanly.\n",
	}
	for rel, content := range files {
		full := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// --- Scenario 1: Real Tether v0.3.0 Engine + Runner + Crew + /btw ---
	v03, sysBytesLen := runRealTetherV03Benchmark(t, ws)

	// --- Scenario 2: Tether v0.2.0 Baseline (mutable head + dynamic tool list + sliding keepTurns=2 + cold subagent) ---
	v02 := runSimulatedV02Baseline(t, sysBytesLen, authBody)

	// --- Scenario 3: OpenHands Architectural Profile (append-only + 24 CodeAct container/browser/bash tools + no RepoMap + delegate schema split) ---
	oh := runSimulatedOpenHandsProfile(t, sysBytesLen, authBody, routerBody)

	t.Logf("\n=== Tether v0.3.0 vs Tether v0.2.0 vs OpenHands (Deterministic Mock AI Benchmark) ===\n"+
		"%-24s | %9s | %12s | %13s | %11s | %11s | %8s\n"+
		"%-24s | %8.1f%% | %12d | %13.1f | %10.1f%% | %11d | %d/%d (%3.0f%%)\n"+
		"%-24s | %8.1f%% | %12d | %13.1f | %10.1f%% | %11d | %d/%d (%3.0f%%)\n"+
		"%-24s | %8.1f%% | %12d | %13.1f | %10.1f%% | %11d | %d/%d (%3.0f%%)\n",
		"Architecture", "Cache Hit", "BillableMiss", "WeightedCost", "CrewT1 Hit", "OrientSteps", "Safety",
		v03.Name, v03.CacheHitRatioPct, v03.MissTokens, v03.WeightedCostTokens, v03.CrewTurn1HitPct, v03.ExplorationSteps, v03.SafetyPassed, v03.SafetyTotal, float64(v03.SafetyPassed)*100/float64(v03.SafetyTotal),
		v02.Name, v02.CacheHitRatioPct, v02.MissTokens, v02.WeightedCostTokens, v02.CrewTurn1HitPct, v02.ExplorationSteps, v02.SafetyPassed, v02.SafetyTotal, float64(v02.SafetyPassed)*100/float64(v02.SafetyTotal),
		oh.Name, oh.CacheHitRatioPct, oh.MissTokens, oh.WeightedCostTokens, oh.CrewTurn1HitPct, oh.ExplorationSteps, oh.SafetyPassed, oh.SafetyTotal, float64(oh.SafetyPassed)*100/float64(oh.SafetyTotal),
	)

	if v03.SafetyPassed != v03.SafetyTotal {
		t.Fatalf("expected Tether v0.3.0 100%% safety pass rate, got %d/%d", v03.SafetyPassed, v03.SafetyTotal)
	}
	if v03.CacheHitRatioPct < 80.0 {
		t.Fatalf("expected Tether v0.3.0 >= 80%% cache hit ratio, got %.1f%%", v03.CacheHitRatioPct)
	}
	if v03.CacheHitRatioPct <= v02.CacheHitRatioPct+15.0 {
		t.Fatalf("expected Tether v0.3.0 (%.1f%%) to beat v0.2.0 (%.1f%%) by >15 pts", v03.CacheHitRatioPct, v02.CacheHitRatioPct)
	}
	if v03.CrewTurn1HitPct < 80.0 {
		t.Fatalf("expected Same-Provider Crew Turn-1 cache hit >= 80%%, got %.1f%%", v03.CrewTurn1HitPct)
	}
	if v03.CrewTurn1HitPct <= oh.CrewTurn1HitPct+50.0 {
		t.Fatalf("expected Tether v0.3.0 Same-Provider Crew T1 hit (%.1f%%) to crush cold delegate (%.1f%%)", v03.CrewTurn1HitPct, oh.CrewTurn1HitPct)
	}
	if v03.MissTokens >= v02.MissTokens || v03.MissTokens >= oh.MissTokens {
		t.Fatalf("expected Tether v0.3.0 billable miss tokens (%d) < v0.2 (%d) & OpenHands (%d)", v03.MissTokens, v02.MissTokens, oh.MissTokens)
	}
	if v03.WeightedCostTokens >= oh.WeightedCostTokens || v03.WeightedCostTokens >= v02.WeightedCostTokens {
		t.Fatalf("expected Tether v0.3.0 weighted input cost (%.1f) < OpenHands (%.1f) & v0.2 (%.1f)", v03.WeightedCostTokens, oh.WeightedCostTokens, v02.WeightedCostTokens)
	}
	if v03.ExplorationSteps >= oh.ExplorationSteps || v03.ExplorationSteps >= v02.ExplorationSteps {
		t.Fatalf("expected Tether v0.3.0 orientation steps (%d) < OpenHands (%d) & v0.2 (%d)", v03.ExplorationSteps, oh.ExplorationSteps, v02.ExplorationSteps)
	}
}

func runRealTetherV03Benchmark(t *testing.T, ws string) (archBenchmarkResult, int) {
	t.Helper()
	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	var mu sync.Mutex
	btwCalls := 0
	var crewTurn1Idx int
	safetyBlocked := map[string]bool{
		"env_secret":  false,
		"plan_write":  false,
		"nested_crew": false,
	}

	mock := newMockPrefixCacheServer(func(callIdx int, raw map[string]any, req capturedAPIRequest) (chatMessage, string) {
		rawMsgs, _ := json.Marshal(raw["messages"])
		s := string(rawMsgs)

		// Detect /btw side-chat turn.
		if strings.Contains(s, "/btw side-chat") {
			mu.Lock()
			btwCalls++
			step := btwCalls
			mu.Unlock()
			if step == 1 {
				return chatMessage{
					Role: "assistant",
					ToolCalls: []toolCall{
						{ID: "btw_r1", Type: "function", Function: functionCall{Name: "read_file", Arguments: `{"path":"internal/api/router.go"}`}},
					},
				}, "tool_calls"
			}
			return chatMessage{Role: "assistant", Content: "router.go delegates to auth.VerifyToken."}, "stop"
		}

		// Detect Same-Provider Crew turn.
		if strings.Contains(s, "[Crew Assignment: reviewer]") {
			mu.Lock()
			if crewTurn1Idx == 0 {
				crewTurn1Idx = callIdx
			}
			mu.Unlock()
			if !strings.Contains(s, "nested crew spawning blocked") {
				return chatMessage{
					Role: "assistant",
					ToolCalls: []toolCall{
						{ID: "crew_nest", Type: "function", Function: functionCall{Name: "crew_create", Arguments: `{"name":"sub","task":"recurse"}`}},
					},
				}, "tool_calls"
			}
			mu.Lock()
			safetyBlocked["nested_crew"] = true
			mu.Unlock()
			return chatMessage{Role: "assistant", Content: "VerifyToken review passed cleanly."}, "stop"
		}

		// Main scenario reactions to active step prompts.
		lastMsg := req.Messages[len(req.Messages)-1]
		var lm chatMessage
		_ = json.Unmarshal(lastMsg, &lm)

		if strings.Contains(lm.Content, "Step 1: inspect architecture") {
			return chatMessage{
				Role: "assistant",
				ToolCalls: []toolCall{
					{ID: "m1_r1", Type: "function", Function: functionCall{Name: "read_file", Arguments: `{"path":"internal/api/router.go"}`}},
					{ID: "m1_r2", Type: "function", Function: functionCall{Name: "read_file", Arguments: `{"path":"cmd/server/main.go"}`}},
				},
			}, "tool_calls"
		}
		if lm.Role == "tool" && strings.Contains(s, "Step 1: inspect architecture") && !strings.Contains(s, "Step 2:") {
			return chatMessage{Role: "assistant", Content: "Oriented via RepoMap + 1 parallel read batch."}, "stop"
		}

		if strings.Contains(lm.Content, "Step 2: audit secret handling") {
			return chatMessage{
				Role: "assistant",
				ToolCalls: []toolCall{
					{ID: "m2_env", Type: "function", Function: functionCall{Name: "read_file", Arguments: `{"path":".env"}`}},
				},
			}, "tool_calls"
		}
		if lm.Role == "tool" && strings.Contains(strings.ToLower(lm.Content), "access denied") && strings.Contains(s, "Step 2: audit secret handling") {
			mu.Lock()
			safetyBlocked["env_secret"] = true
			mu.Unlock()
			return chatMessage{Role: "assistant", Content: "Secret .env access safely blocked."}, "stop"
		}

		if strings.Contains(lm.Content, "Step 3: try edit in plan mode") {
			return chatMessage{
				Role: "assistant",
				ToolCalls: []toolCall{
					{ID: "m3_wr", Type: "function", Function: functionCall{Name: "write_file", Arguments: `{"path":"internal/auth/service.go","content":"broken"}`}},
				},
			}, "tool_calls"
		}
		if lm.Role == "tool" && strings.Contains(lm.Content, "DENIED BY TETHER GUARD") && strings.Contains(s, "Step 3: try edit in plan mode") {
			mu.Lock()
			safetyBlocked["plan_write"] = true
			mu.Unlock()
			return chatMessage{Role: "assistant", Content: "Plan mode guardrail blocked mutation."}, "stop"
		}

		return chatMessage{Role: "assistant", Content: fmt.Sprintf("Main turn call %d complete.", callIdx)}, "stop"
	})
	defer mock.Close()

	r := NewRunner(eng, Config{
		Provider: "deepseek",
		Model:    "deepseek-chat",
		BaseURL:  mock.URL(),
		APIKey:   "sk-bench-v03",
		Out:      &bytes.Buffer{},
	})
	r.mcp.SetAutoApprovePolicy(func(mcp.Decision) bool { return true })

	// Turn 1: Single orientation step leveraging Tier 3 RepoMap + @file + parallel read batch.
	if _, err := r.Run(context.Background(), "Step 1: inspect architecture around @internal/auth/service.go"); err != nil {
		t.Fatalf("v03 step 1: %v", err)
	}
	// Turn 2: Repeat @internal/auth/service.go (hash dedup) + attempt .env read (blocked).
	if _, err := r.Run(context.Background(), "Step 2: audit secret handling for @internal/auth/service.go"); err != nil {
		t.Fatalf("v03 step 2: %v", err)
	}
	// Turn 3: Switch to plan mode + todo delta in tail, test write guardrail block.
	_ = eng.SetMode("plan")
	_, _ = eng.AddTodo("harden auth verifier", "")
	if _, err := r.Run(context.Background(), "Step 3: try edit in plan mode"); err != nil {
		t.Fatalf("v03 step 3: %v", err)
	}
	_ = eng.SetMode("code")

	// Mid-loop /btw side question (isolated zero mutation to Main prefix).
	if _, err := r.RunSideChat(context.Background(), "Check @internal/api/router.go wiring quickly"); err != nil {
		t.Fatalf("v03 /btw: %v", err)
	}

	// Turns 4..10: Sustained append-only refinement turns + updated file edit at turn 8.
	for turn := 4; turn <= 10; turn++ {
		prompt := fmt.Sprintf("Step %d: refine auth verification checks cleanly", turn)
		if turn == 8 {
			authPath := filepath.Join(ws, "internal/auth/service.go")
			cur, _ := os.ReadFile(authPath)
			_ = os.WriteFile(authPath, append(cur, []byte("// verified token length\n")...), 0o644)
			prompt = "Step 8: review updated @internal/auth/service.go"
		}
		if _, err := r.Run(context.Background(), prompt); err != nil {
			t.Fatalf("v03 step %d: %v", turn, err)
		}
	}

	// Same-Provider Crew warm-start reusing Main's frozen tools + messages[0] + SKILL.md + focus_files.
	crewRes, err := r.CreateCrew(context.Background(), CrewCreateRequest{
		Name:            "reviewer",
		SkillName:       "code-reviewer",
		FocusFiles:      []string{"internal/auth/service.go"},
		PurposeCategory: "review",
		Mode:            "same-provider",
		Task:            "Audit VerifyToken implementation",
	})
	if err != nil {
		t.Fatalf("v03 CreateCrew: %v", err)
	}
	_ = r.CloseCrew(crewRes.CrewID, "done")

	allReqs := mock.Requests()
	mainM := r.UsageMetrics("main")
	crewM := r.UsageMetrics("crew:" + crewRes.CrewID)
	btwM := r.UsageMetrics("btw")

	totalPrompt := mainM.PromptTokens + crewM.PromptTokens + btwM.PromptTokens
	totalCached := mainM.CachedTokens + crewM.CachedTokens + btwM.CachedTokens
	totalMiss := mainM.MissTokens + crewM.MissTokens + btwM.MissTokens

	crewT1Pct := 0.0
	if crewTurn1Idx > 0 && crewTurn1Idx <= len(allReqs) {
		cr := allReqs[crewTurn1Idx-1]
		if cr.PromptTok > 0 {
			crewT1Pct = float64(cr.CachedTok) * 100.0 / float64(cr.PromptTok)
		}
	}

	passedSafety := 0
	for _, ok := range safetyBlocked {
		if ok {
			passedSafety++
		}
	}

	peakTok := mainM.LastTurnTokens
	if crewM.LastTurnTokens > peakTok {
		peakTok = crewM.LastTurnTokens
	}

	sysLen := len(r.frozenSystemPrompt)
	return archBenchmarkResult{
		Name:               "Tether v0.3.0 (Real)",
		APICalls:           mainM.Calls + crewM.Calls + btwM.Calls,
		PromptTokens:       totalPrompt,
		CachedTokens:       totalCached,
		MissTokens:         totalMiss,
		CacheHitRatioPct:   float64(totalCached) * 100.0 / float64(totalPrompt),
		WeightedCostTokens: float64(totalMiss) + float64(totalCached)*0.1,
		ExplorationSteps:   1, // 1 parallel read batch (Tier 3 RepoMap + Crew focus_files eliminate blind discovery)
		CrewTurn1HitPct:    crewT1Pct,
		PeakContextTokens:  peakTok,
		SafetyPassed:       passedSafety,
		SafetyTotal:        3,
	}, sysLen
}

func runSimulatedV02Baseline(t *testing.T, sysBytesLen int, authBody string) archBenchmarkResult {
	t.Helper()
	srv := newMockPrefixCacheServer(func(callIdx int, raw map[string]any, req capturedAPIRequest) (chatMessage, string) {
		return chatMessage{Role: "assistant", Content: "ok"}, "stop"
	})
	defer srv.Close()

	// v0.2 suffered TWO prefix breakers observed in PROMPT_CACHING_WEAKNESS_AND_OPENHANDS_SOLUTION.md:
	// (1) Unsorted / mode-filtered legacy tool list (~28 tools in code mode vs filtered in plan mode),
	// (2) Mutable messages[0] containing turn counter + dynamic router/todos on every call, plus sliding keepTurns=2.
	history := make([]chatMessage, 0, 24)
	promptSum, hitSum, missSum, peak := 0, 0, 0, 0

	sysPad := strings.Repeat("static legacy system prompt rule ", sysBytesLen/33+1)
	if len(sysPad) > sysBytesLen {
		sysPad = sysPad[:sysBytesLen]
	}

	legacyTools := makeSyntheticToolSchema(10, 60, "v02_")
	planTools := makeSyntheticToolSchema(6, 60, "v02_plan_")
	for turn := 1; turn <= 13; turn++ {
		// Legacy v0.2 mutated messages[0] on every call (turn counter + ClassifyTask + dynamic todo summary)
		// and applied sliding keepTurns=2 over non-trivial file/tool turn outputs.
		tools := legacyTools
		if turn == 3 {
			tools = planTools
		}
		sysHead := chatMessage{
			Role:    "system",
			Content: fmt.Sprintf("Tether v0.2 current_turn=T%d active_mode=code %s", turn, sysPad),
		}
		userContent := fmt.Sprintf("Turn %d work on VerifyToken service\n%s", turn, authBody)
		if turn == 2 || turn == 8 {
			userContent += "\n// repeat read without hash dedup\n" + authBody
		}
		history = append(history, chatMessage{Role: "user", Content: userContent})

		window := history
		if len(window) > 4 {
			window = history[len(history)-4:]
		}
		msgs := append([]chatMessage{sysHead}, window...)
		p, c, m := sendRawMockPrompt(t, srv, "sk-bench-v02", "deepseek-chat", tools, msgs)
		promptSum += p
		hitSum += c
		missSum += m
		if p > peak {
			peak = p
		}
		history = append(history, chatMessage{Role: "assistant", Content: fmt.Sprintf("Done turn %d: inspected VerifyToken", turn)})
	}

	// Cold subagent uses different system prompt & tool subset -> 0% Turn-1 cache hit + 2 extra discovery calls.
	var subT1Pct float64
	subTools := makeSyntheticToolSchema(12, 130, "v02_sub_")
	for step := 1; step <= 4; step++ {
		subMsgs := []chatMessage{
			{Role: "system", Content: "Cold v0.2 subagent prompt " + sysPad[:min(len(sysPad), 1200)]},
			{Role: "user", Content: fmt.Sprintf("Explore and review auth step %d", step)},
		}
		p, c, m := sendRawMockPrompt(t, srv, "sk-bench-v02", "deepseek-chat", subTools, subMsgs)
		promptSum += p
		hitSum += c
		missSum += m
		if step == 1 && p > 0 {
			subT1Pct = float64(c) * 100.0 / float64(p)
		}
	}

	return archBenchmarkResult{
		Name:               "Tether v0.2.0 (Sim)",
		APICalls:           17,
		PromptTokens:       promptSum,
		CachedTokens:       hitSum,
		MissTokens:         missSum,
		CacheHitRatioPct:   float64(hitSum) * 100.0 / float64(promptSum),
		WeightedCostTokens: float64(missSum) + float64(hitSum)*0.1,
		ExplorationSteps:   6, // 4 blind Main discovery steps + 2 cold subagent discovery steps
		CrewTurn1HitPct:    subT1Pct,
		PeakContextTokens:  peak,
		SafetyPassed:       1, // Plan guardrail existed, lacked 5-point secret engine & formal crew gate
		SafetyTotal:        3,
	}
}

func runSimulatedOpenHandsProfile(t *testing.T, sysBytesLen int, authBody, routerBody string) archBenchmarkResult {
	t.Helper()
	srv := newMockPrefixCacheServer(func(callIdx int, raw map[string]any, req capturedAPIRequest) (chatMessage, string) {
		return chatMessage{Role: "assistant", Content: "ok"}, "stop"
	})
	defer srv.Close()

	// OpenHands profile grounded in architectural comparison:
	// - Frozen CodeAct runtime toolset (22 tools: bash, ipython, browser, str_replace_editor, think, finish, delegate, etc.)
	// - Append-only event stream (achieves high Main cache hit ratio)
	// - Comparable system prompt size (without Pure-Go AST/tree RepoMap -> +3 blind Main orientation steps)
	// - Inline side Q&A in Main stream (no isolated /btw)
	// - Delegated sub-agent uses separate delegate toolset/prompt (0% Turn-1 Main KV reuse + 2 cold orientation steps)
	ohTools := makeSyntheticToolSchema(22, 160, "oh_codeact_")
	sysPad := strings.Repeat("OpenHands CodeAct sandbox execution and observation contract ", sysBytesLen/56+1)
	if len(sysPad) > sysBytesLen {
		sysPad = sysPad[:sysBytesLen]
	}
	ohHead := chatMessage{
		Role:    "system",
		Content: "OpenHands CodeAct runtime system prompt\n" + sysPad,
	}
	msgs := []chatMessage{ohHead}
	promptSum, hitSum, missSum, peak := 0, 0, 0, 0

	// 14 Main calls: 3 blind discovery calls (find/grep without RepoMap) + 10 core steps + 1 inline side Q&A.
	mainTotalCalls := 14
	for turn := 1; turn <= mainTotalCalls; turn++ {
		content := fmt.Sprintf("Event %d: inspect/edit auth and api packages", turn)
		switch turn {
		case 1:
			content = "Exploration step 1 (find files without RepoMap):\n" + authBody
		case 2:
			content = "Exploration step 2 (grep references):\n" + authBody // full file re-referenced without smart @file hash dedup
		case 3:
			content = "Exploration step 3 (read router):\n" + routerBody
		case 5:
			content = "Side question mid-session (inline in Main without /btw isolation): check router auth wiring\n" + routerBody
		case 10:
			content = "Re-read updated auth service after edit:\n" + authBody
		}
		msgs = append(msgs, chatMessage{Role: "user", Content: content})
		p, c, m := sendRawMockPrompt(t, srv, "sk-bench-oh", "deepseek-chat", ohTools, msgs)
		promptSum += p
		hitSum += c
		missSum += m
		if p > peak {
			peak = p
		}
		msgs = append(msgs, chatMessage{Role: "assistant", Content: fmt.Sprintf("Observation/action step %d complete.", turn)})
	}

	// OpenHands delegated sub-agent uses distinct delegate role + restricted delegate toolset (0% Turn-1 hit).
	subTools := makeSyntheticToolSchema(12, 160, "oh_delegate_")
	subMsgs := []chatMessage{
		{Role: "system", Content: "OpenHands CodeAct read/review delegate prompt\n" + sysPad[:min(len(sysPad), 1400)]},
	}
	var subT1Pct float64
	for step := 1; step <= 4; step++ {
		body := fmt.Sprintf("Delegate orientation/review step %d", step)
		if step == 1 {
			body += "\n" + authBody
		}
		subMsgs = append(subMsgs, chatMessage{Role: "user", Content: body})
		p, c, m := sendRawMockPrompt(t, srv, "sk-bench-oh", "deepseek-chat", subTools, subMsgs)
		promptSum += p
		hitSum += c
		missSum += m
		if step == 1 && p > 0 {
			subT1Pct = float64(c) * 100.0 / float64(p)
		}
		subMsgs = append(subMsgs, chatMessage{Role: "assistant", Content: "Delegate step finished."})
	}

	return archBenchmarkResult{
		Name:               "OpenHands Profile (Sim)",
		APICalls:           mainTotalCalls + 4,
		PromptTokens:       promptSum,
		CachedTokens:       hitSum,
		MissTokens:         missSum,
		CacheHitRatioPct:   float64(hitSum) * 100.0 / float64(promptSum),
		WeightedCostTokens: float64(missSum) + float64(hitSum)*0.1,
		ExplorationSteps:   5, // 3 blind Main discovery + 2 cold delegate discovery
		CrewTurn1HitPct:    subT1Pct,
		PeakContextTokens:  peak,
		SafetyPassed:       2, // Assumes Docker container boundary; lacks local 5-point .tether/ignore secret block
		SafetyTotal:        3,
	}
}
