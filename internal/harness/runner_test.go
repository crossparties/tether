package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/croosparties/tether/internal/core"
)

func TestRunner_DeepSeekToolLoopAndPlanGuardrail(t *testing.T) {
	dir := t.TempDir()
	engine, err := core.NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer engine.Close()

	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn++
		w.Header().Set("Content-Type", "application/json")

		switch turn {
		case 1:
			// Turn 1: Model emits reasoning_content + calls write_file in code mode
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"choices": []map[string]interface{}{
					{
						"message": map[string]interface{}{
							"role":              "assistant",
							"reasoning_content": "Creating hello.go first.",
							"tool_calls": []map[string]interface{}{
								{
									"id":   "call_1",
									"type": "function",
									"function": map[string]interface{}{
										"name":      "write_file",
										"arguments": `{"path":"hello.go","content":"package main\n"}`,
									},
								},
							},
						},
					},
				},
			})
		case 2:
			// Turn 2: Model switches to plan mode and attempts to mutate hello.go (should be blocked by Tether Guard!)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"choices": []map[string]interface{}{
					{
						"message": map[string]interface{}{
							"role": "assistant",
							"tool_calls": []map[string]interface{}{
								{
									"id":   "call_2",
									"type": "function",
									"function": map[string]interface{}{
										"name":      "mode_set",
										"arguments": `{"mode":"plan"}`,
									},
								},
								{
									"id":   "call_3",
									"type": "function",
									"function": map[string]interface{}{
										"name":      "write_file",
										"arguments": `{"path":"hello.go","content":"package mutated\n"}`,
									},
								},
							},
						},
					},
				},
			})
		default:
			// Turn 3: Verify tool denial was received by the model and finish
			var reqBody struct {
				Messages []chatMessage `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&reqBody)
			lastMsg := reqBody.Messages[len(reqBody.Messages)-1]
			if !strings.Contains(lastMsg.Content, "DENIED BY TETHER GUARD") {
				t.Errorf("expected tool output to contain DENIED BY TETHER GUARD, got: %s", lastMsg.Content)
			}

			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"choices": []map[string]interface{}{
					{
						"message": map[string]interface{}{
							"role":    "assistant",
							"content": "Completed with Tether Guard enforced.",
						},
					},
				},
			})
		}
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	runner := NewRunner(engine, Config{
		Provider: "deepseek",
		BaseURL:  srv.URL,
		APIKey:   "sk-test-key",
		Model:    "deepseek-reasoner",
		Out:      &logBuf,
	})

	ans, err := runner.Run(context.Background(), "Write hello.go and test plan lock")
	if err != nil {
		t.Fatalf("Runner.Run: %v", err)
	}
	if ans != "Completed with Tether Guard enforced." {
		t.Errorf("unexpected final answer: %q", ans)
	}

	// Verify hello.go was created in Turn 1 (code mode) and NOT overwritten in Turn 2 (plan mode)
	data, err := os.ReadFile(filepath.Join(dir, "hello.go"))
	if err != nil {
		t.Fatalf("read hello.go: %v", err)
	}
	if string(data) != "package main\n" {
		t.Errorf("hello.go was mutated during plan mode! got: %q", string(data))
	}
}

func TestFuzzyEditAndLineRangeRead(t *testing.T) {
	dir := t.TempDir()
	engine, err := core.NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer engine.Close()

	runner := NewRunner(engine, Config{Provider: "ollama"})

	// Create a Go file indented with tabs, nested if block, blank line inside function, and duplicate "return nil"
	initial := "package main\n\nfunc a() error {\n\treturn nil\n}\n\nfunc greet(ok bool) error {\n\tfmt.Println(\"old\")\n\n\tif ok {\n\t\tfmt.Println(\"inner\")\n\t}\n\treturn nil\n}\n"
	_, isErr := runner.mcp.CallTool("write_file", json.RawMessage(`{"path":"main.go","content":`+mustQuote(initial)+`}`))
	if isErr {
		t.Fatalf("write_file failed")
	}

	// 1. Ambiguous exact match ("return nil" appears twice) MUST be rejected!
	ambigArgs, _ := json.Marshal(map[string]string{
		"path":        "main.go",
		"target":      "\treturn nil",
		"replacement": "\treturn nil // edited",
	})
	ambigRes, isErr := runner.mcp.CallTool("edit_file", ambigArgs)
	if !isErr || !strings.Contains(ambigRes, "ambiguous target") {
		t.Fatalf("expected ambiguous exact match to be rejected, got isErr=%v res=%s", isErr, ambigRes)
	}

	// 2. LLM sends target with 4-space indentation AND omits the blank line between Println and if,
	//    and includes a nested block with 8 spaces ("        ").
	editArgs := map[string]string{
		"path":        "main.go",
		"target":      "    fmt.Println(\"old\")\n    if ok {\n        fmt.Println(\"inner\")\n    }",
		"replacement": "    fmt.Println(\"new\")\n    if ok {\n        fmt.Println(\"nested-tab-preserved\")\n    }",
	}
	rawEdit, _ := json.Marshal(editArgs)
	res, isErr := runner.mcp.CallTool("edit_file", rawEdit)
	if isErr {
		t.Fatalf("fuzzy edit_file failed: %s", res)
	}
	if !strings.Contains(res, "match:fuzzy-indent") {
		t.Errorf("expected fuzzy-indent tier, got: %s", res)
	}

	// 3. Read lines 7-13 via line-ranged read_file and verify nested tabs (\t\t) were preserved
	readRes, isErr := runner.mcp.CallTool("read_file", json.RawMessage(`{"path":"main.go","start_line":7,"end_line":13}`))
	if isErr {
		t.Fatalf("read_file range failed: %s", readRes)
	}
	if !strings.Contains(readRes, "\t\tfmt.Println(\"nested-tab-preserved\")") {
		t.Errorf("expected preserved nested double-tab indentation, got:\n%s", readRes)
	}
}

func TestCompactMessages(t *testing.T) {
	// Include multi-byte UTF-8 characters (✓ = 3 bytes) to verify rune-safe truncation
	longUTF8Output := strings.Repeat("✓", 1000)
	msgs := []chatMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "user"},
		{Role: "assistant", ReasoningContent: "old reasoning", Content: "turn 1"},
		{Role: "tool", ToolCallID: "1", Content: longUTF8Output},
		{Role: "assistant", Content: "turn 2"},
		{Role: "tool", ToolCallID: "2a", Content: "ok"},
		{Role: "tool", ToolCallID: "2b", Content: "ok"},
		{Role: "assistant", ReasoningContent: "recent reasoning", Content: "turn 3"},
		{Role: "tool", ToolCallID: "3a", Content: longUTF8Output},
		{Role: "tool", ToolCallID: "3b", Content: longUTF8Output},
		{Role: "tool", ToolCallID: "3c", Content: longUTF8Output},
	}

	compacted := compactMessages(msgs)

	// Turn 1 (index 2 & 3) is outside the last 2 assistant turns: should be compacted
	if compacted[2].ReasoningContent != "" {
		t.Errorf("expected old reasoning_content to be stripped, got %q", compacted[2].ReasoningContent)
	}
	if !strings.Contains(compacted[3].Content, "[compacted 400 chars") {
		t.Errorf("expected old UTF-8 tool output to be compacted by rune count, got: %s", compacted[3].Content[:50])
	}
	// Turn 3 has 3 parallel tool outputs: ALL of them must stay 100% intact because window is turn-based
	if len([]rune(compacted[8].Content)) != 1000 || len([]rune(compacted[10].Content)) != 1000 {
		t.Errorf("expected all parallel tool outputs in recent turn to stay intact")
	}
}

func mustQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

