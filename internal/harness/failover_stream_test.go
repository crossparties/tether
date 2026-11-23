package harness

import (
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/crossparties/tether/internal/core"
)

func TestFailover429PrefersSameProviderBeforeCrossProvider(t *testing.T) {
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())

	var ds1Calls, ds2Calls, oaiCalls atomic.Int32
	dsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer sk-ds-1":
			ds1Calls.Add(1)
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limit"}`))
		case "Bearer sk-ds-2":
			ds2Calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"finish_reason": "stop", "message": map[string]interface{}{"role": "assistant", "content": "recovered on same provider ds-2"}},
				},
			})
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer dsSrv.Close()

	oaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oaiCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"finish_reason": "stop", "message": map[string]interface{}{"role": "assistant", "content": "wrongly switched provider"}},
			},
		})
	}))
	defer oaiSrv.Close()

	eng, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	var out bytes.Buffer
	r := NewRunner(eng, Config{Provider: "deepseek", Out: &out})
	pool := NewKeyPoolWithCandidates(eng, r.client, []KeyCandidate{
		{Provider: "deepseek", Entry: ProviderKeyEntry{ID: "ds-1", APIKey: "sk-ds-1", BaseURL: dsSrv.URL, Model: "deepseek-chat", Priority: 1, Enabled: true}},
		{Provider: "deepseek", Entry: ProviderKeyEntry{ID: "ds-2", APIKey: "sk-ds-2", BaseURL: dsSrv.URL, Model: "deepseek-chat", Priority: 2, Enabled: true}},
		{Provider: "openai", Entry: ProviderKeyEntry{ID: "oai-1", APIKey: "sk-oai-1", BaseURL: oaiSrv.URL, Model: "gpt-4o", Priority: 1, Enabled: true}},
	})
	pool.SetTTLs(5*time.Minute, 5*time.Minute, 50*time.Millisecond)
	r.SetKeyPool(pool)

	var events []string
	r.SetOnEvent(func(kind, detail string) {
		events = append(events, kind+":"+detail)
	})

	ans, err := r.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if ans != "recovered on same provider ds-2" {
		t.Fatalf("unexpected answer: %q", ans)
	}
	if ds1Calls.Load() != 1 || ds2Calls.Load() != 1 || oaiCalls.Load() != 0 {
		t.Fatalf("expected ds1=1 ds2=1 oai=0, got ds1=%d ds2=%d oai=%d", ds1Calls.Load(), ds2Calls.Load(), oaiCalls.Load())
	}
	if r.PinnedKeyID() != "ds-2" || r.ProviderName() != "deepseek" {
		t.Fatalf("expected pinned ds-2 on deepseek, got %q / %q", r.PinnedKeyID(), r.ProviderName())
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "keys.switched:") || !strings.Contains(joined, "stream.retry_reset:") {
		t.Fatalf("expected keys.switched and stream.retry_reset notices, got:\n%s", joined)
	}
}

func TestFailover402CrossProviderSanitizesMessages(t *testing.T) {
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())

	overlongID := "call_" + strings.Repeat("z", 55)
	var dsCalls int
	var oaiReqMessages []chatMessage
	var oaiAuth string

	dsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dsCalls++
		w.Header().Set("Content-Type", "application/json")
		if dsCalls == 1 {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"choices": []map[string]interface{}{
					{
						"finish_reason": "tool_calls",
						"message": map[string]interface{}{
							"role":              "assistant",
							"reasoning_content": "DeepSeek private CoT trace",
							"tool_calls": []map[string]interface{}{
								{
									"id":   overlongID,
									"type": "function",
									"function": map[string]interface{}{
										"name":      "todo_manage",
										"arguments": `{"action":"list"}`,
									},
								},
								{
									"id":   "bad:id.2",
									"type": "function",
									"function": map[string]interface{}{
										"name":      "todo_manage",
										"arguments": "",
									},
								},
							},
						},
					},
				},
			})
			return
		}
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":"insufficient balance"}`))
	}))
	defer dsSrv.Close()

	oaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oaiAuth = r.Header.Get("Authorization")
		var payload struct {
			Messages []chatMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		oaiReqMessages = payload.Messages
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"finish_reason": "stop", "message": map[string]interface{}{"role": "assistant", "content": "openai fallback succeeded"}},
			},
		})
	}))
	defer oaiSrv.Close()

	eng, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	var out bytes.Buffer
	r := NewRunner(eng, Config{Provider: "deepseek", Out: &out})
	pool := NewKeyPoolWithCandidates(eng, r.client, []KeyCandidate{
		{Provider: "deepseek", Entry: ProviderKeyEntry{ID: "ds-1", APIKey: "sk-ds-1", BaseURL: dsSrv.URL, Model: "deepseek-reasoner", Priority: 1, Enabled: true}},
		{Provider: "openai", Entry: ProviderKeyEntry{ID: "oai-fb", APIKey: "sk-openai-fb", BaseURL: oaiSrv.URL, Model: "gpt-4o", Priority: 2, Enabled: true}},
	})
	r.SetKeyPool(pool)

	ans, err := r.Run(context.Background(), "check todos")
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if ans != "openai fallback succeeded" {
		t.Fatalf("unexpected answer: %q", ans)
	}
	if oaiAuth != "Bearer sk-openai-fb" {
		t.Fatalf("expected fallback auth header, got %q", oaiAuth)
	}
	if r.ProviderName() != "openai" || r.PinnedKeyID() != "oai-fb" {
		t.Fatalf("expected switched provider openai / oai-fb, got %q / %q", r.ProviderName(), r.PinnedKeyID())
	}

	// Verify Canonical Message Sanitizer scrubbed ReasoningContent, normalized tool IDs <= 40 chars, and "{}" args!
	var sawAsst bool
	for i, m := range oaiReqMessages {
		if m.ReasoningContent != "" {
			t.Fatalf("message %d leaked ReasoningContent to OpenAI: %q", i, m.ReasoningContent)
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			sawAsst = true
			for j, tc := range m.ToolCalls {
				if !isValidToolCallID(tc.ID) {
					t.Fatalf("tool call %d has invalid ID %q (len=%d)", j, tc.ID, len(tc.ID))
				}
				if tc.Function.Arguments == "" {
					t.Fatalf("tool call %d kept empty arguments", j)
				}
			}
			// Verify corresponding tool replies match 1-to-1.
			if i+2 >= len(oaiReqMessages) ||
				oaiReqMessages[i+1].ToolCallID != m.ToolCalls[0].ID ||
				oaiReqMessages[i+2].ToolCallID != m.ToolCalls[1].ID {
				t.Fatalf("tool call IDs not synchronized 1-to-1: %+v", oaiReqMessages)
			}
		}
	}
	if !sawAsst {
		t.Fatalf("expected tool-calling assistant message in fallback payload")
	}
	if h, ok := pool.HealthOf("ds-1"); !ok || h.Healthy || h.StatusCode != 402 {
		t.Fatalf("expected ds-1 marked unhealthy 402, got ok=%v %+v", ok, h)
	}
}

func TestFailover404MarksNegativeCapabilityAndSwitches(t *testing.T) {
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())

	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"model does not exist"}`))
	}))
	defer srv1.Close()

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"finish_reason": "stop", "message": map[string]interface{}{"role": "assistant", "content": "ok from backup"}},
			},
		})
	}))
	defer srv2.Close()

	eng, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	r := NewRunner(eng, Config{Provider: "deepseek", Model: "missing-model", Out: &bytes.Buffer{}})
	pool := NewKeyPoolWithCandidates(eng, r.client, []KeyCandidate{
		{Provider: "deepseek", Entry: ProviderKeyEntry{ID: "k-bad-model", APIKey: "sk-1", BaseURL: srv1.URL, Model: "missing-model", Priority: 1, Enabled: true}},
		{Provider: "openai", Entry: ProviderKeyEntry{ID: "k-good", APIKey: "sk-2", BaseURL: srv2.URL, Model: "gpt-4o", Priority: 2, Enabled: true}},
	})
	r.SetKeyPool(pool)

	ans, err := r.Run(context.Background(), "hi")
	if err != nil || ans != "ok from backup" {
		t.Fatalf("expected ok from backup, got ans=%q err=%v", ans, err)
	}
	if !pool.IsCapabilityBlocked("k-bad-model", "missing-model") {
		t.Fatalf("expected (k-bad-model, missing-model) recorded in Negative Capability Cache")
	}
}

func TestFinishReasonLengthProseContinuationAndToolSplitNudge(t *testing.T) {
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())

	// 1. Prose continuation with prefix:true capped at <= 2 continuations.
	var proseCalls int
	var sawPrefix []bool
	proseSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proseCalls++
		var body struct {
			Messages []chatMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		last := body.Messages[len(body.Messages)-1]
		sawPrefix = append(sawPrefix, last.Prefix)

		w.Header().Set("Content-Type", "application/json")
		switch proseCalls {
		case 1:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"finish_reason": "length", "message": map[string]interface{}{"role": "assistant", "content": "Part 1 "}},
				},
			})
		case 2:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"finish_reason": "length", "message": map[string]interface{}{"role": "assistant", "content": "Part 2 "}},
				},
			})
		case 3:
			// Even if 3rd still says length, bounded <= 2 continuations stops cleanly!
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"finish_reason": "length", "message": map[string]interface{}{"role": "assistant", "content": "Part 3"}},
				},
			})
		default:
			t.Fatalf("exceeded bounded continuation limit (calls=%d)", proseCalls)
		}
	}))
	defer proseSrv.Close()

	eng, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	r := NewRunner(eng, Config{Provider: "deepseek", BaseURL: proseSrv.URL, APIKey: "sk-1", Model: "deepseek-chat", Out: &bytes.Buffer{}})
	ans, err := r.Run(context.Background(), "write long plan")
	if err != nil {
		t.Fatalf("prose continuation failed: %v", err)
	}
	if ans != "Part 1 Part 2 Part 3" || proseCalls != 3 {
		t.Fatalf("unexpected continuation result: ans=%q calls=%d", ans, proseCalls)
	}
	if sawPrefix[0] || !sawPrefix[1] || !sawPrefix[2] {
		t.Fatalf("expected prefix=true only on continuations 1 and 2, got %v", sawPrefix)
	}

	// 2. Partial tool_calls truncated with finish_reason=="length" -> split-chunk nudge (NO prefix:true, NO execution).
	var toolTurn int
	var sawNudge bool
	toolSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		toolTurn++
		var body struct {
			Messages []chatMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, m := range body.Messages {
			if m.Prefix {
				t.Fatalf("must NOT send prefix:true for truncated tool_calls")
			}
			if m.Role == "tool" && strings.Contains(m.Content, "TRUNCATED TOOL CALL") && strings.Contains(m.Content, "smaller incremental chunks") {
				sawNudge = true
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if toolTurn == 1 {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"choices": []map[string]interface{}{
					{
						"finish_reason": "length",
						"message": map[string]interface{}{
							"role": "assistant",
							"tool_calls": []map[string]interface{}{
								{"id": "call_cut", "type": "function", "function": map[string]interface{}{"name": "write_file", "arguments": `{"path":"big.go","content":"broken`}},
							},
						},
					},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"finish_reason": "stop", "message": map[string]interface{}{"role": "assistant", "content": "will split into smaller chunks"}},
			},
		})
	}))
	defer toolSrv.Close()

	r2 := NewRunner(eng, Config{Provider: "deepseek", BaseURL: toolSrv.URL, APIKey: "sk-1", Model: "deepseek-chat", Out: &bytes.Buffer{}})
	ans2, err := r2.Run(context.Background(), "create big.go")
	if err != nil {
		t.Fatalf("truncated tool call recovery failed: %v", err)
	}
	if !sawNudge || ans2 != "will split into smaller chunks" {
		t.Fatalf("expected split-chunk nudge recovery, sawNudge=%v ans=%q", sawNudge, ans2)
	}
	if _, statErr := os.Stat(filepath.Join(eng.Workspace, "big.go")); !os.IsNotExist(statErr) {
		t.Fatalf("truncated write_file must NOT have written big.go")
	}
}

func TestSSEIdleWatchdogStallFailoverAndActiveStreamPass(t *testing.T) {
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())

	// 1. Stalled stream triggers StreamIdleTimeout (90ms) -> stream.retry_reset -> fails over to key 2.
	var stalledCalls, backupCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		if r.Header.Get("Authorization") == "Bearer sk-stall" {
			stalledCalls.Add(1)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
			flusher.Flush()
			// Stall until idle watchdog cancels the request (with generous upper bound).
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
			return
		}

		backupCalls.Add(1)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"clean recovery\"},\"finish_reason\":\"stop\"}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	eng, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	var logOut bytes.Buffer
	r := NewRunner(eng, Config{
		Provider:          "deepseek",
		StreamIdleTimeout: 90 * time.Millisecond,
		Out:               &logOut,
	})
	pool := NewKeyPoolWithCandidates(eng, r.client, []KeyCandidate{
		{Provider: "deepseek", Entry: ProviderKeyEntry{ID: "ds-stall", APIKey: "sk-stall", BaseURL: srv.URL, Model: "deepseek-chat", Priority: 1, Enabled: true}},
		{Provider: "deepseek", Entry: ProviderKeyEntry{ID: "ds-backup", APIKey: "sk-backup", BaseURL: srv.URL, Model: "deepseek-chat", Priority: 2, Enabled: true}},
	})
	pool.SetTTLs(5*time.Minute, 5*time.Minute, 50*time.Millisecond)
	r.SetKeyPool(pool)

	var deltaKinds []string
	var content strings.Builder
	ans, err := r.RunWithOutputStream(context.Background(), "hello", &logOut, func(kind, text string) {
		deltaKinds = append(deltaKinds, kind)
		if kind == "content" {
			content.WriteString(text)
		}
	})
	if err != nil {
		t.Fatalf("stalled SSE failover failed: %v", err)
	}
	if ans != "clean recovery" {
		t.Fatalf("expected 'clean recovery', got %q", ans)
	}
	if stalledCalls.Load() != 1 || backupCalls.Load() != 1 {
		t.Fatalf("expected stalled=1 backup=1, got stalled=%d backup=%d", stalledCalls.Load(), backupCalls.Load())
	}
	joinedKinds := strings.Join(deltaKinds, ",")
	if !strings.Contains(joinedKinds, "stream.retry_reset") || !strings.Contains(joinedKinds, "keys.switched") {
		t.Fatalf("expected stream.retry_reset and keys.switched delta events, got: %s", joinedKinds)
	}

	// 2. Active multi-chunk stream whose total duration (~280ms) exceeds StreamIdleTimeout (220ms)
	// MUST succeed because each chunk arrives every 35ms (185ms headroom per chunk).
	activeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		chunks := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
		for _, c := range chunks {
			time.Sleep(35 * time.Millisecond)
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", c)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer activeSrv.Close()

	rActive := NewRunner(eng, Config{
		Provider:          "deepseek",
		BaseURL:           activeSrv.URL,
		APIKey:            "sk-ok",
		Model:             "deepseek-chat",
		StreamIdleTimeout: 220 * time.Millisecond,
		Out:               &bytes.Buffer{},
	})
	ansActive, err := rActive.RunWithOutputStream(context.Background(), "long active stream", nil, func(kind, text string) {})
	if err != nil {
		t.Fatalf("active long stream should NOT trigger idle watchdog: %v", err)
	}
	if ansActive != "abcdefgh" {
		t.Fatalf("expected 'abcdefgh', got %q", ansActive)
	}
}

func TestWarmCacheBoundedBudgetDisableOn429AndIdle(t *testing.T) {
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())

	var calls atomic.Int32
	var return429 atomic.Bool
	var maxTokensSeen int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		maxTokensSeen = body.MaxTokens
		if return429.Load() {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"finish_reason": "stop", "message": map[string]interface{}{"role": "assistant", "content": "."}},
			},
		})
	}))
	defer srv.Close()

	eng, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	r := NewRunner(eng, Config{
		Provider:          "deepseek",
		BaseURL:           srv.URL,
		APIKey:            "sk-main",
		Model:             "deepseek-chat",
		WarmCacheMaxPings: 2,
		WarmCacheIdleMax:  5 * time.Second,
		Out:               &bytes.Buffer{},
	})

	// Non-main agent must not run warm_cache.
	r.agentType = "crew"
	if ok, _ := r.WarmCache(context.Background()); ok {
		t.Fatalf("warm_cache must only run for main agent")
	}
	r.agentType = "main"

	// Ping 1 succeeds with max_tokens=1.
	ok, err := r.WarmCache(context.Background())
	if !ok || err != nil || maxTokensSeen != 1 {
		t.Fatalf("WarmCache ping 1 failed: ok=%v err=%v maxTokens=%d", ok, err, maxTokensSeen)
	}
	// Ping 2 succeeds.
	if ok, err := r.WarmCache(context.Background()); !ok || err != nil {
		t.Fatalf("WarmCache ping 2 failed: ok=%v err=%v", ok, err)
	}
	// Ping 3 exceeds WarmCacheMaxPings=2 -> skipped cleanly.
	if ok, _ := r.WarmCache(context.Background()); ok || calls.Load() != 2 {
		t.Fatalf("expected ping cap at 2, got ok=%v calls=%d", ok, calls.Load())
	}

	// Auto-disable on 429.
	r2 := NewRunner(eng, Config{
		Provider:          "deepseek",
		BaseURL:           srv.URL,
		APIKey:            "sk-main",
		Model:             "deepseek-chat",
		WarmCacheMaxPings: 5,
		Out:               &bytes.Buffer{},
	})
	return429.Store(true)
	if ok, err := r2.WarmCache(context.Background()); ok || err == nil {
		t.Fatalf("expected 429 error on warm_cache")
	}
	return429.Store(false)
	prevCalls := calls.Load()
	if ok, _ := r2.WarmCache(context.Background()); ok || calls.Load() != prevCalls {
		t.Fatalf("expected warm_cache disabled after 429")
	}

	// Auto-disable on idle > WarmCacheIdleMax deterministically without brittle sleeps.
	r3 := NewRunner(eng, Config{
		Provider:         "deepseek",
		BaseURL:          srv.URL,
		APIKey:           "sk-main",
		Model:            "deepseek-chat",
		WarmCacheIdleMax: 10 * time.Minute,
		Out:              &bytes.Buffer{},
	})
	r3.mu.Lock()
	r3.lastUserActivity = time.Now().Add(-time.Hour)
	r3.mu.Unlock()
	prevCalls = calls.Load()
	if ok, _ := r3.WarmCache(context.Background()); ok || calls.Load() != prevCalls {
		t.Fatalf("expected warm_cache auto-disabled after idle timeout")
	}
}

func TestParallelReadOnlyBatchingBoundedAndMutatingBarrier(t *testing.T) {
	eng, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	r := NewRunner(eng, Config{Provider: "ollama", Out: &bytes.Buffer{}})

	var active, maxActive atomic.Int32
	var mu sync.Mutex
	var timeline []string
	var once sync.Once
	parallelObserved := make(chan struct{})

	r.execTool = func(tc toolCall) string {
		cur := active.Add(1)
		for {
			prev := maxActive.Load()
			if cur <= prev || maxActive.CompareAndSwap(prev, cur) {
				break
			}
		}
		if cur >= 2 {
			once.Do(func() { close(parallelObserved) })
		}
		mu.Lock()
		timeline = append(timeline, "start:"+tc.ID)
		mu.Unlock()

		// Synchronize cleanly on first concurrent batch instead of relying on tight wall-clock timing.
		if tc.ID == "r1" || tc.ID == "r2" || tc.ID == "r3" || tc.ID == "r4" {
			select {
			case <-parallelObserved:
			case <-time.After(time.Second):
			}
		}

		mu.Lock()
		timeline = append(timeline, "end:"+tc.ID)
		mu.Unlock()
		active.Add(-1)
		return "res:" + tc.ID
	}

	calls := []toolCall{
		{ID: "r1", Function: functionCall{Name: "read_file", Arguments: `{"path":"1"}`}},
		{ID: "r2", Function: functionCall{Name: "glob", Arguments: `{"pattern":"*.go"}`}},
		{ID: "r3", Function: functionCall{Name: "grep", Arguments: `{"pattern":"x"}`}},
		{ID: "r4", Function: functionCall{Name: "kb_search", Arguments: `{"query":"y"}`}},
		{ID: "r5", Function: functionCall{Name: "todo_manage", Arguments: `{"action":"list"}`}},
		{ID: "w1", Function: functionCall{Name: "write_file", Arguments: `{"path":"a.go","content":"x"}`}},
		{ID: "r6", Function: functionCall{Name: "read_file", Arguments: `{"path":"2"}`}},
		{ID: "r7", Function: functionCall{Name: "read_file", Arguments: `{"path":"3"}`}},
	}

	outcomes := r.executeToolCallsStatus(calls, nil)

	if maxActive.Load() > 4 || maxActive.Load() < 2 {
		t.Fatalf("expected bounded parallel concurrency in [2..4], got maxActive=%d", maxActive.Load())
	}
	// Exact output order preserved matching tool call IDs.
	for i, tc := range calls {
		if outcomes[i].text != "res:"+tc.ID {
			t.Fatalf("outcomes[%d] = %q, want %q", i, outcomes[i].text, "res:"+tc.ID)
		}
	}
	// Mutating write_file acts as a strict sequential barrier between read batches.
	wStart, wEnd, r6Start := -1, -1, -1
	for idx, ev := range timeline {
		switch ev {
		case "start:w1":
			wStart = idx
		case "end:w1":
			wEnd = idx
		case "start:r6":
			r6Start = idx
		}
	}
	if wStart < 0 || wEnd != wStart+1 || r6Start < wEnd {
		t.Fatalf("write_file barrier violated! timeline=%v", timeline)
	}
}

func TestStuckDetectionStopsAtThreeIdenticalRepetitions(t *testing.T) {
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "app.go"), []byte("package main\nfunc ok() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	_ = eng.AddApprovalGrant(".", "all")

	var apiCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := apiCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{
					"finish_reason": "tool_calls",
					"message": map[string]interface{}{
						"role": "assistant",
						"tool_calls": []map[string]interface{}{
							{
								"id":   fmt.Sprintf("call_stuck_%d", n),
								"type": "function",
								"function": map[string]interface{}{
									"name":      "edit_file",
									"arguments": `{"path":"app.go","target":"nonexistent_line","replacement":"new_line"}`,
								},
							},
						},
					},
				},
			},
		})
	}))
	defer srv.Close()

	var out bytes.Buffer
	r := NewRunner(eng, Config{
		Provider: "deepseek",
		BaseURL:  srv.URL,
		APIKey:   "sk-test",
		Model:    "deepseek-chat",
		MaxSteps: 20,
		Out:      &out,
	})

	var events []string
	r.SetOnEvent(func(kind, detail string) {
		events = append(events, kind)
	})

	_, err = r.Run(context.Background(), "fix app.go")
	if err == nil || !strings.Contains(err.Error(), "stuck_detection") {
		t.Fatalf("expected stuck_detection error, got %v", err)
	}
	if apiCalls.Load() != 3 {
		t.Fatalf("expected loop to stop right at 3 identical repetitions, got %d API calls", apiCalls.Load())
	}
	if !strings.Contains(strings.Join(events, ","), "stuck_detection") {
		t.Fatalf("expected stuck_detection notice event, got %v", events)
	}
}

func TestParallelReadOnlyToolLoopEndToEnd(t *testing.T) {
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.go"), []byte("package main\n// file A\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "b.go"), []byte("package main\n// file B\n"), 0644); err != nil {
		t.Fatal(err)
	}
	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	_, _ = eng.AddTodo("parallel task", "")

	var turn int
	var toolMsgs []chatMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn++
		w.Header().Set("Content-Type", "application/json")
		if turn == 1 {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"choices": []map[string]interface{}{
					{
						"finish_reason": "tool_calls",
						"message": map[string]interface{}{
							"role": "assistant",
							"tool_calls": []map[string]interface{}{
								{"id": "t_read_a", "type": "function", "function": map[string]interface{}{"name": "read_file", "arguments": `{"path":"a.go"}`}},
								{"id": "t_read_b", "type": "function", "function": map[string]interface{}{"name": "read_file", "arguments": `{"path":"b.go"}`}},
								{"id": "t_glob", "type": "function", "function": map[string]interface{}{"name": "glob", "arguments": `{"pattern":"*.go"}`}},
								{"id": "t_grep", "type": "function", "function": map[string]interface{}{"name": "grep", "arguments": `{"pattern":"file B"}`}},
								{"id": "t_todos", "type": "function", "function": map[string]interface{}{"name": "todo_manage", "arguments": `{"action":"list"}`}},
							},
						},
					},
				},
			})
			return
		}
		var reqBody struct {
			Messages []chatMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		for _, m := range reqBody.Messages {
			if m.Role == "tool" {
				toolMsgs = append(toolMsgs, m)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"finish_reason": "stop", "message": map[string]interface{}{"role": "assistant", "content": "all 5 read-only tools complete"}},
			},
		})
	}))
	defer srv.Close()

	r := NewRunner(eng, Config{Provider: "deepseek", BaseURL: srv.URL, APIKey: "sk-test", Model: "deepseek-chat", Out: &bytes.Buffer{}})
	ans, err := r.Run(context.Background(), "inspect workspace")
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if ans != "all 5 read-only tools complete" {
		t.Fatalf("unexpected ans: %q", ans)
	}
	wantOrder := []string{"t_read_a", "t_read_b", "t_glob", "t_grep", "t_todos"}
	if len(toolMsgs) != len(wantOrder) {
		t.Fatalf("expected %d tool messages, got %d: %+v", len(wantOrder), len(toolMsgs), toolMsgs)
	}
	for i, wantID := range wantOrder {
		if toolMsgs[i].ToolCallID != wantID {
			t.Fatalf("toolMsgs[%d].ToolCallID = %q, want %q", i, toolMsgs[i].ToolCallID, wantID)
		}
	}
	if !strings.Contains(toolMsgs[0].Content, "file A") || !strings.Contains(toolMsgs[1].Content, "file B") ||
		!strings.Contains(toolMsgs[2].Content, "a.go") || !strings.Contains(toolMsgs[3].Content, "b.go:2:") ||
		!strings.Contains(toolMsgs[4].Content, "parallel task") {
		t.Fatalf("unexpected parallel tool contents: %+v", toolMsgs)
	}
}

func TestFailoverFromCustomRelayUpdatesBaseURLToBackupKey(t *testing.T) {
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())

	var relayCalls, stdCalls atomic.Int32
	relaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relayCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"relay down"}`))
	}))
	defer relaySrv.Close()

	stdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stdCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"finish_reason": "stop", "message": map[string]interface{}{"role": "assistant", "content": "served by key 2 URL"}},
			},
		})
	}))
	defer stdSrv.Close()

	eng, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	r := NewRunner(eng, Config{Provider: "deepseek", BaseURL: relaySrv.URL, APIKey: "sk-relay", Out: &bytes.Buffer{}})
	pool := NewKeyPoolWithCandidates(eng, r.client, []KeyCandidate{
		{Provider: "deepseek", Entry: ProviderKeyEntry{ID: "ds-relay", APIKey: "sk-relay", BaseURL: relaySrv.URL, Model: "deepseek-chat", Priority: 1, Enabled: true}},
		{Provider: "deepseek", Entry: ProviderKeyEntry{ID: "ds-std", APIKey: "sk-std", BaseURL: stdSrv.URL, Model: "deepseek-chat", Priority: 2, Enabled: true}},
	})
	pool.SetTTLs(5*time.Minute, 5*time.Minute, 20*time.Millisecond)
	r.SetKeyPool(pool)

	ans, err := r.Run(context.Background(), "hello")
	if err != nil || ans != "served by key 2 URL" {
		t.Fatalf("expected failover to Key 2 BaseURL, got ans=%q err=%v", ans, err)
	}
	if relayCalls.Load() != 1 || stdCalls.Load() != 1 {
		t.Fatalf("expected relay=1 std=1, got relay=%d std=%d", relayCalls.Load(), stdCalls.Load())
	}
	if r.snapshotCallConfig().baseURL != stdSrv.URL {
		t.Fatalf("expected r.cfg.BaseURL updated to %q, got %q", stdSrv.URL, r.snapshotCallConfig().baseURL)
	}

	// Verify when backup key has empty BaseURL (standard endpoint), EffectiveBaseURL() clears prior custom relay URL!
	poolEmpty := NewKeyPoolWithCandidates(eng, r.client, []KeyCandidate{
		{Provider: "deepseek", Entry: ProviderKeyEntry{ID: "ds-relay", APIKey: "sk-relay", BaseURL: relaySrv.URL, Priority: 1, Enabled: true}},
		{Provider: "deepseek", Entry: ProviderKeyEntry{ID: "ds-std-default", APIKey: "sk-std", BaseURL: "", Priority: 2, Enabled: true}},
	})
	r2 := NewRunner(eng, Config{Provider: "deepseek", Out: &bytes.Buffer{}})
	r2.SetKeyPool(poolEmpty)
	r2.applyKeyCandidate(poolEmpty.Candidates()[1], true)
	if r2.snapshotCallConfig().baseURL != presets["deepseek"].BaseURL {
		t.Fatalf("expected empty BaseURL on backup key to resolve to preset %q, got %q", presets["deepseek"].BaseURL, r2.snapshotCallConfig().baseURL)
	}
}

func TestConcurrentMainStreamBtwAndWarmCacheRaceFree(t *testing.T) {
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream    bool          `json:"stream"`
			MaxTokens int           `json:"max_tokens"`
			Messages  []chatMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			for _, tok := range []string{"main-", "chunk1-", "chunk2"} {
				time.Sleep(10 * time.Millisecond)
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", tok)
				flusher.Flush()
			}
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		reply := "btw-isolated-reply"
		if body.MaxTokens == 1 {
			reply = "."
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"finish_reason": "stop", "message": map[string]interface{}{"role": "assistant", "content": reply}},
			},
		})
	}))
	defer srv.Close()

	eng, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	r := NewRunner(eng, Config{
		Provider:          "deepseek",
		BaseURL:           srv.URL,
		APIKey:            "sk-main",
		Model:             "deepseek-chat",
		WarmCacheMaxPings: 3,
		Out:               &bytes.Buffer{},
	})

	var mainDeltas strings.Builder
	var deltaMu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(3)

	var mainAns, btwAns string
	var mainErr, btwErr error
	var warmOK bool
	var warmErr error

	go func() {
		defer wg.Done()
		var out bytes.Buffer
		mainAns, mainErr = r.RunWithOutputStream(context.Background(), "main task", &out, func(kind, text string) {
			if kind == "content" {
				deltaMu.Lock()
				mainDeltas.WriteString(text)
				deltaMu.Unlock()
			}
		})
	}()

	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		btwAns, btwErr = r.RunSideChat(context.Background(), "quick side question")
	}()

	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		warmOK, warmErr = r.WarmCache(context.Background())
	}()

	wg.Wait()
	if mainErr != nil || mainAns != "main-chunk1-chunk2" {
		t.Fatalf("Main stream failed: ans=%q err=%v", mainAns, mainErr)
	}
	if btwErr != nil || btwAns != "btw-isolated-reply" {
		t.Fatalf("/btw failed: ans=%q err=%v", btwAns, btwErr)
	}
	if warmErr != nil || !warmOK {
		t.Fatalf("WarmCache failed: ok=%v err=%v", warmOK, warmErr)
	}
	if strings.Contains(mainDeltas.String(), "btw-isolated-reply") || mainDeltas.String() != "main-chunk1-chunk2" {
		t.Fatalf("Main onDelta leaked /btw or dropped tokens: %q", mainDeltas.String())
	}
}

func TestSSENearBoundaryIdleTimeoutTriggersFailover(t *testing.T) {
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())

	var calls1, calls2 atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		if r.Header.Get("Authorization") == "Bearer sk-boundary" {
			calls1.Add(1)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"c1\"}}]}\n\n")
			flusher.Flush()
			// Wait for the client's idle watchdog to cancel the stream (or generous 600ms upper bound).
			select {
			case <-r.Context().Done():
				return
			case <-time.After(600 * time.Millisecond):
			}
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\n")
			flusher.Flush()
			return
		}
		calls2.Add(1)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"backup-ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	eng, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	r := NewRunner(eng, Config{Provider: "deepseek", StreamIdleTimeout: 70 * time.Millisecond, Out: &bytes.Buffer{}})
	pool := NewKeyPoolWithCandidates(eng, r.client, []KeyCandidate{
		{Provider: "deepseek", Entry: ProviderKeyEntry{ID: "k1", APIKey: "sk-boundary", BaseURL: srv.URL, Priority: 1, Enabled: true}},
		{Provider: "deepseek", Entry: ProviderKeyEntry{ID: "k2", APIKey: "sk-ok", BaseURL: srv.URL, Priority: 2, Enabled: true}},
	})
	pool.SetTTLs(5*time.Minute, 5*time.Minute, 20*time.Millisecond)
	r.SetKeyPool(pool)

	ans, err := r.RunWithOutputStream(context.Background(), "go", nil, func(kind, text string) {})
	if err != nil || ans != "backup-ok" {
		t.Fatalf("expected backup-ok after boundary timeout failover, got ans=%q err=%v", ans, err)
	}
	if calls1.Load() != 1 || calls2.Load() != 1 {
		t.Fatalf("expected calls1=1 calls2=1, got %d / %d", calls1.Load(), calls2.Load())
	}
}

func TestStuckDetectionAppendsAllToolOutcomesAndHashesFullOutput(t *testing.T) {
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())

	// 1. Outputs differing only past byte 120 must NOT trigger false stuck_detection.
	prefix130 := strings.Repeat("A", 130)
	sigA := toolCallSignature("read_file", `{"path":"x"}`, prefix130+"tail-1")
	sigB := toolCallSignature("read_file", `{"path":"x"}`, prefix130+"tail-2")
	if sigA == sigB {
		t.Fatalf("toolCallSignature truncated result before hashing: %q == %q", sigA, sigB)
	}

	// 2. Multi-tool turn hitting stuck_detection on call #0 must STILL append call #1's outcome to r.history.
	ws := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws, "a.txt"), []byte("alpha\n"), 0644)
	_ = os.WriteFile(filepath.Join(ws, "b.txt"), []byte("beta\n"), 0644)
	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	var step int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		step++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{
					"finish_reason": "tool_calls",
					"message": map[string]interface{}{
						"role": "assistant",
						"tool_calls": []map[string]interface{}{
							{"id": fmt.Sprintf("ca_%d", step), "type": "function", "function": map[string]interface{}{"name": "read_file", "arguments": `{"path":"a.txt"}`}},
							{"id": fmt.Sprintf("cb_%d", step), "type": "function", "function": map[string]interface{}{"name": "read_file", "arguments": `{"path":"b.txt"}`}},
						},
					},
				},
			},
		})
	}))
	defer srv.Close()

	r := NewRunner(eng, Config{Provider: "deepseek", BaseURL: srv.URL, APIKey: "sk-test", Model: "deepseek-chat", MaxSteps: 10, Out: &bytes.Buffer{}})
	_, err = r.Run(context.Background(), "loop")
	if err == nil || !strings.Contains(err.Error(), "stuck_detection") {
		t.Fatalf("expected stuck_detection error, got %v", err)
	}
	if step != 3 {
		t.Fatalf("expected stuck_detection stop at 3 batch repetitions, got %d", step)
	}
	// Every assistant tool_call in r.history MUST have a matching tool response (0 orphans!).
	r.mu.RLock()
	hist := append([]chatMessage(nil), r.history...)
	r.mu.RUnlock()
	lastAsst := hist[len(hist)-3]
	if lastAsst.Role != "assistant" || len(lastAsst.ToolCalls) != 2 ||
		hist[len(hist)-2].ToolCallID != lastAsst.ToolCalls[0].ID ||
		hist[len(hist)-1].ToolCallID != lastAsst.ToolCalls[1].ID {
		t.Fatalf("stuck_detection left orphaned tool calls in r.history: %+v", hist)
	}
}
