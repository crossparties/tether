package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crossparties/tether/internal/core"
)

func sse(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func TestStreamingToolLoopDeliversLiveDeltas(t *testing.T) {
	engine, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	var mu sync.Mutex
	call := 0
	var sawStream []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		call++
		n := call
		s, _ := body["stream"].(bool)
		sawStream = append(sawStream, s)
		mu.Unlock()
		if n == 1 {
			// reasoning + text + a tool call whose JSON arguments arrive split across chunks
			sse(w,
				`{"choices":[{"delta":{"reasoning_content":"think "}}]}`,
				`{"choices":[{"delta":{"content":"Checking "}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"todo_manage","arguments":"{\"action\":"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"list\"}"}}]}}]}`,
			)
			return
		}
		sse(w,
			`{"choices":[{"delta":{"content":"All "}}]}`,
			`{"choices":[{"delta":{"content":"done\nline2"}}]}`,
			`{"choices":[],"usage":{"prompt_cache_hit_tokens":90,"prompt_cache_miss_tokens":10}}`,
		)
	}))
	defer srv.Close()

	r := NewRunner(engine, Config{Provider: "deepseek", Model: "deepseek-chat", BaseURL: srv.URL, APIKey: "k", Out: &strings.Builder{}})
	var content, reasoning strings.Builder
	endsteps := 0
	ans, err := r.RunWithOutputStream(context.Background(), "go", nil, func(kind, text string) {
		switch kind {
		case "content":
			content.WriteString(text)
		case "reasoning":
			reasoning.WriteString(text)
		case "endstep":
			endsteps++
		}
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ans != "All done\nline2" {
		t.Fatalf("final answer = %q", ans)
	}
	if content.String() != "Checking All done\nline2" || reasoning.String() != "think " || endsteps != 1 {
		t.Fatalf("deltas wrong: content=%q reasoning=%q endsteps=%d", content.String(), reasoning.String(), endsteps)
	}
	for i, s := range sawStream {
		if !s {
			t.Fatalf("request %d did not ask for stream=true", i+1)
		}
	}
	if !strings.Contains(r.StatusSummary(), "Cache Hit: 90/100 (90%)") {
		t.Fatalf("streamed usage telemetry missing: %s", r.StatusSummary())
	}
}

func TestStreamingFallsBackToPlainJSON(t *testing.T) {
	engine, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"choices": []map[string]interface{}{{"message": map[string]interface{}{"role": "assistant", "content": "plain"}}}})
	}))
	defer srv.Close()
	r := NewRunner(engine, Config{Provider: "deepseek", Model: "deepseek-chat", BaseURL: srv.URL, APIKey: "k", Out: &strings.Builder{}})
	var got string
	ans, err := r.RunWithOutputStream(context.Background(), "x", nil, func(k, s string) {
		if k == "content" {
			got += s
		}
	})
	if err != nil || ans != "plain" || got != "plain" {
		t.Fatalf("fallback failed: ans=%q got=%q err=%v", ans, got, err)
	}
}

func TestStreamingCancelStopsPromptly(t *testing.T) {
	engine, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-req.Context().Done() // hang until the client goes away
	}))
	defer srv.Close()
	r := NewRunner(engine, Config{Provider: "deepseek", Model: "deepseek-chat", BaseURL: srv.URL, APIKey: "k", Out: &strings.Builder{}})
	ctx, cancel := context.WithCancel(context.Background())
	_, err = r.RunWithOutputStream(ctx, "x", nil, func(k, s string) {
		if k == "content" {
			cancel() // Esc pressed mid-stream
		}
	})
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("expected interrupted error, got %v", err)
	}
}
