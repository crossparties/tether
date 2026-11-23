package harness

import (
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

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/mcp"
	"github.com/crossparties/tether/internal/mount"
)

type capturedAPIRequest struct {
	Key         string
	Model       string
	RawBody     string
	ToolsJSON   string
	Messages    []json.RawMessage
	PromptTok   int
	CachedTok   int
	MissTok     int
	CompTok     int
	HasCacheCtl bool
}

// mockPrefixCacheServer simulates provider server-side KV prefix caching by
// tokenizing serialized (tools + messages[]) per (apiKey, model) and computing
// exact longest-common-prefix hits across prior requests.
type mockPrefixCacheServer struct {
	mu       sync.Mutex
	srv      *httptest.Server
	prefixes map[string][][]string
	requests []capturedAPIRequest
	handler  func(callIdx int, raw map[string]any, req capturedAPIRequest) (chatMessage, string)
}

func newMockPrefixCacheServer(handler func(callIdx int, raw map[string]any, req capturedAPIRequest) (chatMessage, string)) *mockPrefixCacheServer {
	m := &mockPrefixCacheServer{
		prefixes: make(map[string][][]string),
		handler:  handler,
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serveHTTP))
	return m
}

func (m *mockPrefixCacheServer) URL() string { return m.srv.URL }
func (m *mockPrefixCacheServer) Close()      { m.srv.Close() }

func (m *mockPrefixCacheServer) Requests() []capturedAPIRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]capturedAPIRequest, len(m.requests))
	copy(out, m.requests)
	return out
}

func (m *mockPrefixCacheServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	bodyBytes, _ := io.ReadAll(r.Body)
	var raw map[string]any
	_ = json.Unmarshal(bodyBytes, &raw)

	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	model, _ := raw["model"].(string)
	cacheKey := auth + "|" + model

	var rawMsgs []json.RawMessage
	if msgRaw, err := json.Marshal(raw["messages"]); err == nil {
		_ = json.Unmarshal(msgRaw, &rawMsgs)
	}
	toolsBytes, _ := json.Marshal(raw["tools"])
	toolsJSON := string(toolsBytes)

	tokens := tokenizePrefixInput(toolsJSON, rawMsgs)

	m.mu.Lock()
	prior := m.prefixes[cacheKey]
	bestHit := 0
	for _, p := range prior {
		lcp := longestCommonPrefixLen(p, tokens)
		if lcp > bestHit {
			bestHit = lcp
		}
	}
	if bestHit > len(tokens) {
		bestHit = len(tokens)
	}
	missTok := len(tokens) - bestHit
	m.prefixes[cacheKey] = append(prior, tokens)

	callIdx := len(m.requests) + 1
	capReq := capturedAPIRequest{
		Key:         auth,
		Model:       model,
		RawBody:     string(bodyBytes),
		ToolsJSON:   toolsJSON,
		Messages:    rawMsgs,
		PromptTok:   len(tokens),
		CachedTok:   bestHit,
		MissTok:     missTok,
		HasCacheCtl: strings.Contains(string(bodyBytes), `"cache_control"`),
	}
	m.mu.Unlock()

	replyMsg, finish := m.handler(callIdx, raw, capReq)
	if finish == "" {
		finish = "stop"
	}
	compTok := len(tokenizeText(replyMsg.Content)) + len(replyMsg.ToolCalls)*12
	if compTok < 4 {
		compTok = 4
	}
	capReq.CompTok = compTok

	m.mu.Lock()
	m.requests = append(m.requests, capReq)
	m.mu.Unlock()

	resp := map[string]any{
		"choices": []map[string]any{
			{
				"message":       replyMsg,
				"finish_reason": finish,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":            capReq.PromptTok,
			"completion_tokens":        compTok,
			"prompt_cache_hit_tokens":  capReq.CachedTok,
			"prompt_cache_miss_tokens": capReq.MissTok,
			"prompt_tokens_details": map[string]any{
				"cached_tokens": capReq.CachedTok,
			},
		},
	}
	if capReq.HasCacheCtl {
		resp["usage"].(map[string]any)["input_tokens"] = capReq.MissTok
		resp["usage"].(map[string]any)["cache_read_input_tokens"] = capReq.CachedTok
		resp["usage"].(map[string]any)["output_tokens"] = compTok
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// tokenizePrefixInput converts tools + per-message canonical JSON into granular
// token chunks (~4 chars per token) so even a single changed field inside tools
// or messages[0] breaks prefix caching from that exact token onward.
func tokenizePrefixInput(toolsJSON string, rawMsgs []json.RawMessage) []string {
	out := tokenizeText("TOOLS:" + toolsJSON)
	for i, rm := range rawMsgs {
		out = append(out, tokenizeText(fmt.Sprintf("MSG%d:%s", i, string(rm)))...)
	}
	return out
}

func tokenizeText(s string) []string {
	if s == "" {
		return nil
	}
	const chunk = 4
	out := make([]string, 0, (len(s)+chunk-1)/chunk)
	for i := 0; i < len(s); i += chunk {
		end := i + chunk
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
}

func longestCommonPrefixLen(a, b []string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func Test12TurnAppendOnlyKVCacheHitAndV02BaselineComparison(t *testing.T) {
	ws := t.TempDir()
	mainSrc := strings.Repeat("// stable workspace header\n", 40) + "package main\nfunc CoreInit() {}\n"
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte(mainSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "extra.go"), []byte("package main\nfunc ExtraWork() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	mock := newMockPrefixCacheServer(func(callIdx int, raw map[string]any, req capturedAPIRequest) (chatMessage, string) {
		return chatMessage{Role: "assistant", Content: fmt.Sprintf("Turn %d completed.", callIdx)}, "stop"
	})
	defer mock.Close()

	var logBuf bytes.Buffer
	r := NewRunner(eng, Config{
		Provider: "deepseek",
		Model:    "deepseek-chat",
		BaseURL:  mock.URL(),
		APIKey:   "sk-cache-12",
		Out:      &logBuf,
	})

	// Turn 1: attach @main.go (full content injected once).
	if _, err := r.Run(context.Background(), "Inspect @main.go and explain CoreInit"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	// Turn 2: repeat @main.go unchanged -> smart deduplication avoids re-dumping full file!
	if _, err := r.Run(context.Background(), "Refactor plan for @main.go"); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	// Turn 3: user prompt starts with "/plan" while mode is explicit "code" -> MUST NOT auto-SetMode!
	if _, err := r.Run(context.Background(), "/plan outline the next steps without mutating mode"); err != nil {
		t.Fatalf("turn 3: %v", err)
	}
	if eng.GetMode() != "code" {
		t.Fatalf("Runner must NOT auto-switch mode via ClassifyTask; got %q", eng.GetMode())
	}
	// Explicit mode switch + todo update -> emitted in tail delta without mutating messages[0]!
	_ = eng.SetMode("plan")
	_, _ = eng.AddTodo("verify cache hit", "")
	if _, err := r.Run(context.Background(), "Check @extra.go in explicit plan mode"); err != nil {
		t.Fatalf("turn 4: %v", err)
	}
	_ = eng.SetMode("code")

	// Turns 5..12: sustained coding turns.
	for turn := 5; turn <= 12; turn++ {
		prompt := fmt.Sprintf("Step %d: continue implementing feature cleanly", turn)
		if turn == 8 {
			// Modify main.go on disk -> smart @file dedup must detect hash change and re-attach fresh file!
			_ = os.WriteFile(filepath.Join(ws, "main.go"), []byte(mainSrc+"func UpdatedCore() {}\n"), 0o644)
			prompt = "Check updated @main.go"
		}
		if _, err := r.Run(context.Background(), prompt); err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
	}

	reqs := mock.Requests()
	if len(reqs) != 12 {
		t.Fatalf("expected 12 API calls, got %d", len(reqs))
	}

	// Verify frozen tools + messages[0] + strict append-only prefix across all 12 turns.
	baseTools := reqs[0].ToolsJSON
	baseHead := string(reqs[0].Messages[0])
	for i := 1; i < len(reqs); i++ {
		if reqs[i].ToolsJSON != baseTools {
			t.Fatalf("turn %d mutated tools schema!", i+1)
		}
		if string(reqs[i].Messages[0]) != baseHead {
			t.Fatalf("turn %d mutated Tier 0-3 messages[0]!\nwant: %s\ngot:  %s", i+1, baseHead, string(reqs[i].Messages[0]))
		}
		// Every message from turn i-1 must be byte-for-byte identical in turn i (strict append-only).
		for j := 0; j < len(reqs[i-1].Messages); j++ {
			if string(reqs[i].Messages[j]) != string(reqs[i-1].Messages[j]) {
				t.Fatalf("turn %d mutated prior message[%d]!\nbefore=%s\nafter=%s",
					i+1, j, string(reqs[i-1].Messages[j]), string(reqs[i].Messages[j]))
			}
		}
	}

	// Verify smart @file dedup on Turn 2 vs fresh re-attachment on Turn 8.
	if !strings.Contains(string(reqs[1].Messages[len(reqs[1].Messages)-1]), `unchanged_in_active_context`) {
		t.Fatalf("expected Turn 2 repeat @main.go to be deduplicated, got: %s", string(reqs[1].Messages[len(reqs[1].Messages)-1]))
	}
	if !strings.Contains(string(reqs[7].Messages[len(reqs[7].Messages)-1]), `UpdatedCore`) {
		t.Fatalf("expected Turn 8 modified @main.go to re-attach updated content, got: %s", string(reqs[7].Messages[len(reqs[7].Messages)-1]))
	}
	// Verify mode transition & todo delta appeared in Turn 4 tail without touching messages[0].
	var turn4Msg chatMessage
	_ = json.Unmarshal(reqs[3].Messages[len(reqs[3].Messages)-1], &turn4Msg)
	if !strings.Contains(turn4Msg.Content, "Mode transition: code -> plan") || !strings.Contains(turn4Msg.Content, "verify cache hit") {
		t.Fatalf("expected mode transition & todo delta in Turn 4 tail, got: %s", turn4Msg.Content)
	}

	metrics := r.UsageMetrics("main")
	if metrics.Calls != 12 {
		t.Fatalf("expected 12 main usage calls, got %d", metrics.Calls)
	}
	if metrics.CacheHitRatio < 0.80 {
		t.Fatalf("expected >= 80%% cumulative KV-cache hit ratio over 12 turns, got %.2f%% (%+v)", metrics.CacheHitRatio*100, metrics)
	}

	// Compare objectively against simulated v0.2 mutable-messages[0] baseline on same mock server.
	v02TotalPrompt, v02TotalCached := 0, 0
	v02Srv := newMockPrefixCacheServer(func(callIdx int, raw map[string]any, req capturedAPIRequest) (chatMessage, string) {
		return chatMessage{Role: "assistant", Content: "ok"}, "stop"
	})
	defer v02Srv.Close()
	for turn := 1; turn <= 12; turn++ {
		// v0.2 anti-pattern: mutates messages[0] every turn with current_turn/dynamic task router at top.
		v02Msgs := []chatMessage{
			{Role: "system", Content: fmt.Sprintf("System head current_turn=T%d %s", turn, strings.Repeat("x", 400))},
			{Role: "user", Content: fmt.Sprintf("Turn %d", turn)},
		}
		body, _ := json.Marshal(map[string]any{"model": "deepseek-chat", "messages": v02Msgs})
		req, _ := http.NewRequest(http.MethodPost, v02Srv.URL(), bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-v02")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var res struct {
			Usage wireUsage `json:"usage"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&res)
		_ = resp.Body.Close()
		v02TotalPrompt += res.Usage.PromptTokens
		v02TotalCached += res.Usage.PromptCacheHitTokens
	}
	v02Ratio := float64(v02TotalCached) / float64(v02TotalPrompt)
	t.Logf("[12-Turn Cache Benchmark] Tether v0.3 Append-Only Hit Ratio = %.1f%% (%d/%d tokens) vs v0.2 Mutable-Head Baseline = %.1f%% (%d/%d tokens)",
		metrics.CacheHitRatio*100, metrics.CachedTokens, metrics.PromptTokens, v02Ratio*100, v02TotalCached, v02TotalPrompt)
	if metrics.CacheHitRatio <= v02Ratio+0.60 {
		t.Fatalf("expected v0.3 cache hit ratio (%.2f) to massively outperform v0.2 mutable baseline (%.2f)", metrics.CacheHitRatio, v02Ratio)
	}
}

func TestMidLoopBtwContextAwareReadToolsAndZeroMainMutation(t *testing.T) {
	ws := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws, "app.go"), []byte("package app\nfunc TargetHelper() int { return 42 }\n"), 0o644)

	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	var mu sync.Mutex
	btwCalls := 0
	mock := newMockPrefixCacheServer(func(callIdx int, raw map[string]any, req capturedAPIRequest) (chatMessage, string) {
		rawMsgs, _ := json.Marshal(raw["messages"])
		s := string(rawMsgs)
		if strings.Contains(s, "/btw side-chat") {
			mu.Lock()
			btwCalls++
			step := btwCalls
			mu.Unlock()
			switch step {
			case 1:
				// Model asks for read_file AND attempts a mutating write_file (which MUST be blocked by Tether Guard!)
				return chatMessage{
					Role: "assistant",
					ToolCalls: []toolCall{
						{ID: "btw_read", Type: "function", Function: functionCall{Name: "read_file", Arguments: `{"path":"app.go"}`}},
						{ID: "btw_evil", Type: "function", Function: functionCall{Name: "write_file", Arguments: `{"path":"app.go","content":"pwn"}`}},
					},
				}, "tool_calls"
			default:
				if !strings.Contains(s, "42") || !strings.Contains(s, "DENIED BY TETHER GUARD") {
					t.Errorf("expected /btw tool loop to receive read_file output + write_file denial, got: %s", s)
				}
				return chatMessage{Role: "assistant", Content: "TargetHelper returns 42; writes blocked in /btw."}, "stop"
			}
		}
		return chatMessage{Role: "assistant", Content: "Main turn answer."}, "stop"
	})
	defer mock.Close()

	r := NewRunner(eng, Config{
		Provider: "deepseek",
		Model:    "deepseek-chat",
		BaseURL:  mock.URL(),
		APIKey:   "sk-btw-test",
		Out:      &bytes.Buffer{},
	})

	if _, err := r.Run(context.Background(), "Working on app architecture"); err != nil {
		t.Fatalf("main turn 1: %v", err)
	}

	mainBefore := r.UsageMetrics("main")
	histBefore, _ := json.Marshal(r.history)
	headBefore := r.frozenSystemPrompt

	// Execute context-aware /btw with @app.go + read_file tool + blocked write_file attempt.
	btwAns, err := r.RunSideChat(context.Background(), "What does @app.go TargetHelper return?")
	if err != nil {
		t.Fatalf("RunSideChat: %v", err)
	}
	if !strings.Contains(btwAns, "42") {
		t.Fatalf("unexpected /btw answer: %q", btwAns)
	}

	// Verify 0 bytes mutated in Main history & head, and main usage counters untouched.
	histAfter, _ := json.Marshal(r.history)
	if !bytes.Equal(histBefore, histAfter) || r.frozenSystemPrompt != headBefore {
		t.Fatalf("/btw mutated Main history or frozen head!")
	}
	mainAfter := r.UsageMetrics("main")
	if mainAfter != mainBefore {
		t.Fatalf("/btw polluted Main usage metrics: before=%+v after=%+v", mainBefore, mainAfter)
	}
	btwMetrics := r.UsageMetrics("btw")
	if btwMetrics.Calls != 2 || btwMetrics.PromptTokens == 0 {
		t.Fatalf("expected isolated btw metrics with 2 calls, got %+v", btwMetrics)
	}

	// Verify app.go on disk was untouched.
	diskData, _ := os.ReadFile(filepath.Join(ws, "app.go"))
	if !strings.Contains(string(diskData), "TargetHelper() int { return 42 }") {
		t.Fatalf("app.go modified during /btw: %s", string(diskData))
	}

	// Next Main turn still hits Main's exact prefix cache!
	if _, err := r.Run(context.Background(), "Continue main task"); err != nil {
		t.Fatalf("main turn 2: %v", err)
	}
	if m2 := r.UsageMetrics("main"); m2.LastCacheHit == 0 {
		t.Fatalf("expected Main turn 2 to hit KV-cache after /btw interruption, got %+v", m2)
	}

	// Test PromoteBtwToMain and PinBtwAsDecision.
	if err := r.PromoteBtwToMain("", ""); err != nil {
		t.Fatalf("PromoteBtwToMain: %v", err)
	}
	if len(r.history) < 6 || !strings.Contains(r.history[len(r.history)-2].Content, "[Promoted from /btw]") {
		t.Fatalf("expected promoted /btw appended cleanly to tail of r.history")
	}
	if err := r.PinBtwAsDecision("helper", "TargetHelper returns 42", "verified via /btw"); err != nil {
		t.Fatalf("PinBtwAsDecision: %v", err)
	}
	decs, _ := eng.GetDecisions()
	if len(decs) == 0 || decs[0].Topic != "helper" {
		t.Fatalf("expected pinned decision in session.db, got %+v", decs)
	}
}

func TestSameProviderCrewWarmStartAndRecursionGuard(t *testing.T) {
	ws := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws, "core.go"), []byte("package app\nfunc MainCore() {}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(ws, "focus.go"), []byte("package app\nfunc FocusTask() {}\n"), 0o644)
	skillDir := filepath.Join(ws, ".agents", "skills", "code-reviewer")
	_ = os.MkdirAll(skillDir, 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Code Reviewer Skill\nCheck edge cases strictly.\n"), 0o644)

	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	var mu sync.Mutex
	crewSawNestedDenial := false
	mock := newMockPrefixCacheServer(func(callIdx int, raw map[string]any, req capturedAPIRequest) (chatMessage, string) {
		rawMsgs, _ := json.Marshal(raw["messages"])
		var decoded []chatMessage
		_ = json.Unmarshal(rawMsgs, &decoded)
		var joined strings.Builder
		for _, dm := range decoded {
			joined.WriteString(dm.Content)
			joined.WriteByte('\n')
		}
		s := joined.String()
		// When running inside Crew Turn 1, attempt a recursive crew_create to test depth >= 1 guard!
		if strings.Contains(s, "[Crew Assignment: reviewer]") && !strings.Contains(s, "DENIED BY TETHER GUARD") {
			return chatMessage{
				Role: "assistant",
				ToolCalls: []toolCall{
					{ID: "nested_1", Type: "function", Function: functionCall{Name: "crew_create", Arguments: `{"name":"nested","task":"recurse"}`}},
				},
			}, "tool_calls"
		}
		if strings.Contains(s, "nested crew spawning blocked at depth >= 1") {
			mu.Lock()
			crewSawNestedDenial = true
			mu.Unlock()
			return chatMessage{Role: "assistant", Content: "Review completed cleanly."}, "stop"
		}
		return chatMessage{Role: "assistant", Content: "Main baseline warmed."}, "stop"
	})
	defer mock.Close()

	r := NewRunner(eng, Config{
		Provider: "deepseek",
		Model:    "deepseek-chat",
		BaseURL:  mock.URL(),
		APIKey:   "sk-shared-key",
		Out:      &bytes.Buffer{},
	})

	// Warm Main Turn 1 (caches tools + messages[0]).
	if _, err := r.Run(context.Background(), "Start main session"); err != nil {
		t.Fatalf("main warm turn: %v", err)
	}

	// Spawn Same-Provider Crew with SKILL.md + focus_files.
	res, err := r.CreateCrew(context.Background(), CrewCreateRequest{
		Name:            "reviewer",
		SkillName:       "code-reviewer",
		FocusFiles:      []string{"focus.go"},
		PurposeCategory: "review",
		Mode:            "same-provider",
		Task:            "Audit focus.go",
	})
	if err != nil {
		t.Fatalf("CreateCrew: %v", err)
	}
	defer func() { _ = r.CloseCrew(res.CrewID, "done") }()

	if !res.SameProvider || res.Output != "Review completed cleanly." {
		for idx, rq := range mock.Requests() {
			t.Logf("req[%d] key=%s model=%s body=%s", idx, rq.Key, rq.Model, rq.RawBody)
		}
		t.Fatalf("unexpected CreateCrew result: %+v", res)
	}
	if !crewSawNestedDenial {
		t.Fatalf("expected nested crew_create at depth >= 1 to be blocked by Tether Guard")
	}

	reqs := mock.Requests()
	if len(reqs) < 3 {
		t.Fatalf("expected at least 3 API calls (main + crew step 1 + crew step 2), got %d", len(reqs))
	}
	mainReq := reqs[0]
	crewTurn1 := reqs[1]

	// Exact identical tools + messages[0] inherited -> Crew Turn 1 gets immediate cached_tokens > 0!
	if crewTurn1.ToolsJSON != mainReq.ToolsJSON {
		t.Fatalf("Same-Provider Crew tools schema drifted from Main!")
	}
	if string(crewTurn1.Messages[0]) != string(mainReq.Messages[0]) {
		t.Fatalf("Same-Provider Crew messages[0] drifted from Main!")
	}
	msg1 := string(crewTurn1.Messages[1])
	if !strings.Contains(msg1, "Code Reviewer Skill") || !strings.Contains(msg1, "FocusTask") {
		t.Fatalf("expected SKILL.md + focus_files in Crew messages[1], got: %s", msg1)
	}
	if crewTurn1.CachedTok == 0 {
		t.Fatalf("expected Same-Provider Crew Turn 1 to hit Main's KV-cache prefix (cached_tokens > 0), got 0")
	}
	crewMetrics := r.UsageMetrics("crew:" + res.CrewID)
	if crewMetrics.CachedTokens == 0 || crewMetrics.Calls != 2 {
		t.Fatalf("expected isolated crew metrics with cached hit > 0, got %+v", crewMetrics)
	}

	// Different-Provider Crew uses focused 8-tool subset (omitting crew_* and mode_set/todo_manage).
	diffCrew, err := r.CreateCrew(context.Background(), CrewCreateRequest{
		Name:     "diff-worker",
		Mode:     "different-provider",
		Provider: "openai",
		Model:    "gpt-4o",
		BaseURL:  mock.URL(),
		APIKey:   "sk-diff",
		Task:     "Quick check",
	})
	if err != nil {
		t.Fatalf("different-provider CreateCrew: %v", err)
	}
	_ = r.CloseCrew(diffCrew.CrewID, "done")
	reqsAfter := mock.Requests()
	diffReq := reqsAfter[len(reqsAfter)-1]
	if strings.Contains(diffReq.ToolsJSON, `"crew_create"`) || strings.Contains(diffReq.ToolsJSON, `"mode_set"`) {
		t.Fatalf("different-provider crew should use focused core toolset without crew_*/mode_set, got: %s", diffReq.ToolsJSON)
	}
}

func TestCapabilityGateCacheControlAnthropicVsOpenAIAndDeepSeek(t *testing.T) {
	eng, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	mock := newMockPrefixCacheServer(func(callIdx int, raw map[string]any, req capturedAPIRequest) (chatMessage, string) {
		return chatMessage{Role: "assistant", Content: "ok"}, "stop"
	})
	defer mock.Close()

	cases := []struct {
		provider string
		model    string
		wantCtl  bool
	}{
		{"anthropic", "claude-3-7-sonnet-latest", true},
		{"openrouter", "anthropic/claude-3.7-sonnet", true},
		{"deepseek", "deepseek-chat", false},
		{"openai", "gpt-4o", false},
		{"groq", "llama-3.3-70b-versatile", false},
		{"ollama", "qwen2.5-coder:7b", false},
	}

	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			r := NewRunner(eng, Config{
				Provider: tc.provider,
				Model:    tc.model,
				BaseURL:  mock.URL(),
				APIKey:   "sk-cap-" + tc.provider,
				Out:      &bytes.Buffer{},
			})
			if _, err := r.Run(context.Background(), "turn 1"); err != nil {
				t.Fatalf("turn 1 (%s): %v", tc.provider, err)
			}
			if _, err := r.Run(context.Background(), "turn 2"); err != nil {
				t.Fatalf("turn 2 (%s): %v", tc.provider, err)
			}

			reqs := mock.Requests()
			last := reqs[len(reqs)-1]
			if last.HasCacheCtl != tc.wantCtl {
				t.Fatalf("provider %s/%s cache_control=%v, want %v\nbody=%s",
					tc.provider, tc.model, last.HasCacheCtl, tc.wantCtl, last.RawBody)
			}
			// Verify messages[0].content is multipart array when wantCtl=true and string when wantCtl=false.
			var msg0 struct {
				Content json.RawMessage `json:"content"`
			}
			_ = json.Unmarshal(last.Messages[0], &msg0)
			trimmed := strings.TrimSpace(string(msg0.Content))
			if tc.wantCtl && !strings.HasPrefix(trimmed, "[") {
				t.Fatalf("expected multipart content array for %s, got: %s", tc.provider, trimmed)
			}
			if !tc.wantCtl && !strings.HasPrefix(trimmed, `"`) {
				t.Fatalf("expected plain string content for %s, got: %s", tc.provider, trimmed)
			}
		})
	}
}

func TestEpisodicCondenserDualThresholdPersistenceAndThreadReload(t *testing.T) {
	eng, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	mock := newMockPrefixCacheServer(func(callIdx int, raw map[string]any, req capturedAPIRequest) (chatMessage, string) {
		rawMsgs, _ := json.Marshal(raw["messages"])
		if strings.Contains(string(rawMsgs), "EXACTLY these 5 numbered markdown sections") {
			return chatMessage{
				Role: "assistant",
				Content: "1. Goal: Build widget\n2. Modified Files & Line State: widget.go lines 1-20\n" +
					"3. Active Errors: None\n4. Rejected/Undone Changes: None\n5. Next Step: Add unit tests",
			}, "stop"
		}
		return chatMessage{Role: "assistant", Content: fmt.Sprintf("reply %d", callIdx)}, "stop"
	})
	defer mock.Close()

	var out bytes.Buffer
	r := NewRunner(eng, Config{
		Provider:          "deepseek",
		Model:             "deepseek-chat",
		BaseURL:           mock.URL(),
		APIKey:            "sk-condenser",
		ContextWindow:     2000,
		CondenseRatio:     0.72,
		CondenseMaxEvents: 6,
		Out:               &out,
	})

	// Run 3 turns (creates 1 system + 6 user/assistant events >= max_events=6).
	for i := 1; i <= 3; i++ {
		if _, err := r.Run(context.Background(), fmt.Sprintf("Turn %d work", i)); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	headOrig := r.frozenSystemPrompt

	// Turn 4 triggers dual-threshold EpisodicCondenser cleanly.
	if _, err := r.Run(context.Background(), "Turn 4 work"); err != nil {
		t.Fatalf("turn 4: %v", err)
	}

	if r.history[0].Role != "system" || r.history[0].Content != headOrig {
		t.Fatalf("Tier 0-3 messages[0] was altered by EpisodicCondenser!")
	}
	condMetrics := r.UsageMetrics("condenser")
	if condMetrics.Calls == 0 {
		t.Fatalf("expected condenser calls tracked under usage_id='condenser', got %+v", condMetrics)
	}
	// Verify condensation event persisted in session.db conversation_events.
	evs, err := eng.ListThreadEvents(r.ThreadID(), 100)
	if err != nil {
		t.Fatal(err)
	}
	foundCond := false
	for _, ev := range evs {
		if ev.Role == "condensation" && ev.UsageID == "condenser" {
			foundCond = true
			var payload struct {
				Summary string `json:"summary"`
			}
			if err := json.Unmarshal([]byte(ev.PayloadJSON), &payload); err != nil {
				t.Fatalf("unmarshal condensation payload: %v", err)
			}
			for _, slot := range mandatoryCondenserSlots {
				if !strings.Contains(payload.Summary, slot) {
					t.Errorf("persisted condensation event missing slot %q in:\n%s", slot, payload.Summary)
				}
			}
		}
	}
	if !foundCond {
		t.Fatalf("expected persisted condensation event in conversation_events, got %+v", evs)
	}

	// Manual /compact command + /tokens command work via slash registry.
	dispatchCommand(r, "/compact")
	dispatchCommand(r, "/tokens")
	if !strings.Contains(out.String(), "condenser") || !strings.Contains(out.String(), "main") {
		t.Fatalf("expected /tokens output to include main and condenser usage IDs:\n%s", out.String())
	}

	// Reload thread into a fresh Runner -> restores condensed summary + subsequent turns!
	r2 := NewRunner(eng, Config{Provider: "deepseek", BaseURL: mock.URL(), APIKey: "sk-condenser", Out: &bytes.Buffer{}})
	if err := r2.LoadThread(r.ThreadID()); err != nil {
		t.Fatalf("LoadThread: %v", err)
	}
	if len(r2.history) < 2 || !strings.Contains(r2.history[1].Content, "Episodic Condensation Summary") {
		t.Fatalf("LoadThread did not restore condensed summary cleanly: %+v", r2.history)
	}
}

func TestStructuredPlanClarificationAndCriticStartBuilding(t *testing.T) {
	ws := t.TempDir()
	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	_ = eng.SetMode("plan")
	planMD := "# Feature Plan\n1. Add endpoint\n2. Add tests\n"
	if err := WritePlan(ws, planMD); err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	readBack, err := ReadPlan(ws)
	if err != nil || readBack != planMD {
		t.Fatalf("ReadPlan = %q err=%v", readBack, err)
	}

	qs := []ClarificationQuestion{
		{
			ID:         "q1",
			Question:   "Storage engine?",
			Options:    []PlanOption{{ID: "sqlite", Label: "SQLite WAL", Recommended: true}, {ID: "mem", Label: "In-memory"}},
			SelectedID: "sqlite",
		},
	}
	formatted := FormatClarificationAnswers(qs)
	if !strings.Contains(formatted, "Storage engine?") || !strings.Contains(formatted, "SQLite WAL") {
		t.Fatalf("unexpected FormatClarificationAnswers: %q", formatted)
	}

	criticVerdict := "BLOCKED: missing rollback step"
	mock := newMockPrefixCacheServer(func(callIdx int, raw map[string]any, req capturedAPIRequest) (chatMessage, string) {
		return chatMessage{Role: "assistant", Content: criticVerdict}, "stop"
	})
	defer mock.Close()

	r := NewRunner(eng, Config{Provider: "deepseek", Model: "deepseek-chat", BaseURL: mock.URL(), APIKey: "sk-plan", Out: &bytes.Buffer{}})

	resBlocked, err := r.StartBuilding(context.Background(), true)
	if err != nil {
		t.Fatalf("StartBuilding blocked run: %v", err)
	}
	if resBlocked.Ready || eng.GetMode() != "plan" {
		t.Fatalf("expected Critic BLOCKED to keep mode=plan, got res=%+v mode=%s", resBlocked, eng.GetMode())
	}

	criticVerdict = "APPROVED: plan is complete and safe."
	resReady, err := r.StartBuilding(context.Background(), true)
	if err != nil {
		t.Fatalf("StartBuilding approved run: %v", err)
	}
	if !resReady.Ready || eng.GetMode() != "code" {
		t.Fatalf("expected Critic APPROVED to switch mode=code, got res=%+v mode=%s", resReady, eng.GetMode())
	}
	_ = mcp.VerdictOnce
}

func TestEpisodicCondenserSingleTurnLongToolLoopForwardProgress(t *testing.T) {
	ws := t.TempDir()
	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	r := NewRunner(eng, Config{Provider: "deepseek", Model: "deepseek-chat", Out: &bytes.Buffer{}})
	msgs := []chatMessage{
		{Role: "system", Content: "Immutable sys head"},
		{Role: "user", Content: "[Episodic Condensation Summary]\n1. Goal\nKeep going"},
		{Role: "assistant", ToolCalls: []toolCall{{ID: "tc1", Type: "function", Function: functionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}}}},
		{Role: "tool", ToolCallID: "tc1", Content: strings.Repeat("huge output ", 200)},
		{Role: "assistant", ToolCalls: []toolCall{{ID: "tc2", Type: "function", Function: functionCall{Name: "read_file", Arguments: `{"path":"b.go"}`}}}},
		{Role: "tool", ToolCallID: "tc2", Content: "small output"},
	}

	out, res, err := r.condenseMessages(context.Background(), msgs, true)
	if err != nil {
		t.Fatalf("condenseMessages: %v", err)
	}
	if !res.Condensed || res.CondensedCount != 3 || len(out) != 4 {
		t.Fatalf("expected forward condensation of [summary,asst1,tool1] -> 4 msgs, got res=%+v len(out)=%d", res, len(out))
	}
	if out[2].Role != "assistant" || len(out[2].ToolCalls) != 1 || out[2].ToolCalls[0].ID != "tc2" || out[3].ToolCallID != "tc2" {
		t.Fatalf("expected latest assistant+tool pair preserved in tail, got %+v", out)
	}

	// Second call on [sys, summary, asst(tc2), tool(tc2)] MUST NOT endlessly re-condense [summary].
	out2, res2, err := r.condenseMessages(context.Background(), out, true)
	if err != nil {
		t.Fatalf("second condenseMessages: %v", err)
	}
	if res2.Condensed || len(out2) != len(out) {
		t.Fatalf("expected second call to stop cleanly (Condensed:false), got res2=%+v len=%d", res2, len(out2))
	}
}

func TestMountAccessIgnoredBlocksAtMentionAndBtw(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "visible.go"), []byte("package main\nfunc Visible() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "hidden.go"), []byte("package main\nconst Secret = \"IGNORED_SECRET_DATA\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if err := eng.Mount().SetMountedMode("hidden.go", mount.AccessIgnored, "manual"); err != nil {
		t.Fatal(err)
	}

	var seenBodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		seenBodies = append(seenBodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": chatMessage{Role: "assistant", Content: "checked"}, "finish_reason": "stop"}},
		})
	}))
	defer srv.Close()

	r := NewRunner(eng, Config{Provider: "deepseek", Model: "deepseek-chat", BaseURL: srv.URL, APIKey: "sk-ign", Out: &bytes.Buffer{}})
	if _, err := r.Run(context.Background(), "Check @hidden.go and @visible.go"); err != nil {
		t.Fatalf("Main Run: %v", err)
	}
	if _, err := r.RunSideChat(context.Background(), "Explain @hidden.go and @visible.go"); err != nil {
		t.Fatalf("RunSideChat: %v", err)
	}
	if len(seenBodies) != 2 {
		t.Fatalf("expected 2 requests (main + btw), got %d", len(seenBodies))
	}
	for idx, body := range seenBodies {
		if !strings.Contains(body, "Visible()") {
			t.Fatalf("request %d missing visible.go content: %s", idx, body)
		}
		if strings.Contains(body, "IGNORED_SECRET_DATA") {
			t.Fatalf("request %d leaked AccessIgnored hidden.go content!", idx)
		}
	}
}

func TestFailedStep0DoesNotCommitAtFileHashesOrTailState(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\nfunc CoreFeature() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	var attempt int
	var reqBodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		reqBodies = append(reqBodies, string(raw))
		attempt++
		if attempt == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"simulated step 0 error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": chatMessage{Role: "assistant", Content: "recovered cleanly"}, "finish_reason": "stop"}},
		})
	}))
	defer srv.Close()

	r := NewRunner(eng, Config{Provider: "deepseek", Model: "deepseek-chat", BaseURL: srv.URL, APIKey: "sk-tx", Out: &bytes.Buffer{}})
	r.QueueTailNotice("preserve-after-failed-step0")

	if _, err := r.Run(context.Background(), "Check @main.go"); err == nil {
		t.Fatalf("expected first attempt to fail on HTTP 400")
	}

	ans, err := r.Run(context.Background(), "Check @main.go")
	if err != nil || ans != "recovered cleanly" {
		t.Fatalf("retry Run failed: ans=%q err=%v", ans, err)
	}
	if len(reqBodies) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(reqBodies))
	}
	retryBody := reqBodies[1]
	if strings.Contains(retryBody, "unchanged_in_active_context") {
		t.Fatalf("retry erroneously deduplicated @main.go after failed step 0: %s", retryBody)
	}
	if !strings.Contains(retryBody, "CoreFeature") || !strings.Contains(retryBody, "preserve-after-failed-step0") {
		t.Fatalf("retry missing full @main.go or preserved notice: %s", retryBody)
	}
}
