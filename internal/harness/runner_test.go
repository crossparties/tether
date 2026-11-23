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

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/snapshot"
	"github.com/crossparties/tether/internal/ui"
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

	// The approval gate requires a grant before any write. Grant the workspace
	// for this test so the guard does not mask what is being verified here.
	if err := engine.AddApprovalGrant(".", "all"); err != nil {
		t.Fatalf("grant: %v", err)
	}

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

	// Approve writes for this workspace; the test exercises edit matching, not
	// the approval gate.
	if err := engine.AddApprovalGrant(".", "all"); err != nil {
		t.Fatalf("grant: %v", err)
	}

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

	compacted := compactMessagesWithLimits(msgs, 2, 800)

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

// The step budget must be configurable, and a project setting must win over the
// built-in default so operators can tune it without a rebuild.
func TestMaxStepsResolution(t *testing.T) {
	dir := t.TempDir()
	e, err := core.NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer e.Close()

	def := ResolveConfig(e, Config{})
	if def.MaxSteps != 80 {
		t.Fatalf("default MaxSteps = %d, want 80", def.MaxSteps)
	}

	if err := e.SetProjectSetting("max_steps", "150"); err != nil {
		t.Fatalf("SetProjectSetting: %v", err)
	}
	got := ResolveConfig(e, Config{})
	if got.MaxSteps != 150 {
		t.Fatalf("MaxSteps = %d, want 150 from the project setting", got.MaxSteps)
	}

	// An explicit flag must beat the stored setting.
	explicit := ResolveConfig(e, Config{MaxSteps: 7})
	if explicit.MaxSteps != 7 {
		t.Fatalf("explicit MaxSteps = %d, want 7 (flag must win)", explicit.MaxSteps)
	}

	// Garbage in the setting must not brick the runner.
	_ = e.SetProjectSetting("max_steps", "not-a-number")
	if got := ResolveConfig(e, Config{}); got.MaxSteps != 80 {
		t.Fatalf("invalid setting should fall back to 80, got MaxSteps=%d", got.MaxSteps)
	}
}

// A pause must leave the working history intact so "continue" resumes the task
// instead of starting over without the tool results the model just produced.
func TestPausePersistsFullHistory(t *testing.T) {
	src, err := os.ReadFile("runner.go")
	if err != nil {
		t.Fatalf("read runner.go: %v", err)
	}
	text := string(src)

	// Locate the pause branch narrowly: from the last history assignment that
	// precedes the pause message, up to the message itself.
	idx := strings.Index(text, "[paused after %d tool steps")
	if idx < 0 {
		t.Fatal("pause branch not found")
	}
	body := text[:idx]
	if cut := strings.LastIndex(body, "r.history = "); cut >= 0 {
		body = body[cut:]
	}

	if strings.Contains(body, "r.history = compactMessagesWithLimits(messages, keepTurns, maxRunes)") {
		t.Fatal("the pause branch compacts history before saving it; resuming would lose the tool results just produced")
	}
	if !strings.Contains(body, "r.history = messages") {
		t.Fatal("the pause branch does not persist the full working history")
	}
}

// A write log must be labelled WRITE, never APPROVAL.
func TestPrintToolActionLabelsWrite(t *testing.T) {
	ui.SetEnabled(false)
	t.Cleanup(ui.ClearOverride)

	dir := t.TempDir()
	e, err := core.NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer e.Close()

	var buf bytes.Buffer
	r := &Runner{engine: e, cfg: Config{Out: &buf}}
	r.printToolAction(&buf, "write_file", map[string]interface{}{
		"path":    "internal/x.go",
		"content": "package x\n",
	})

	out := buf.String()
	if !strings.Contains(out, "WRITE") {
		t.Fatalf("write log must be labelled WRITE, got:\n%s", out)
	}
	if strings.Contains(out, "APPROVAL") {
		t.Fatalf("a write log must not claim to be an approval request:\n%s", out)
	}
	if !strings.Contains(out, "internal/x.go") {
		t.Fatalf("write log must name the file:\n%s", out)
	}
}

// TestUndoReportsNothingWhenNoSnapshotExists proves /undo degrades to a message
// rather than an error on a fresh workspace, which is the state most users hit
// the first time they type it.
func TestUndoReportsNothingWhenNoSnapshotExists(t *testing.T) {
	dir := t.TempDir()
	engine, err := core.NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer engine.Close()

	var buf bytes.Buffer
	r := NewRunner(engine, Config{Out: &buf})
	r.undo("")

	out := buf.String()
	if !strings.Contains(out, "Nothing to undo") {
		t.Fatalf("expected a friendly empty-state message, got:\n%s", out)
	}
	if strings.Contains(out, "✓ Undo complete") {
		t.Fatalf("must not claim success when nothing was restored:\n%s", out)
	}
}

// TestUndoRestoresModifiedFile drives the full path: a captured file is changed
// on disk, then /undo puts the original contents back.
func TestUndoRestoresModifiedFile(t *testing.T) {
	dir := t.TempDir()
	engine, err := core.NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer engine.Close()

	target := filepath.Join(dir, "config.go")
	if err := os.WriteFile(target, []byte("ORIGINAL"), 0644); err != nil {
		t.Fatalf("write original: %v", err)
	}

	store := snapshot.New(engine.Workspace)
	if err := store.Capture("turn-undo", "config.go"); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := os.WriteFile(target, []byte("MODIFIED"), 0644); err != nil {
		t.Fatalf("write modified: %v", err)
	}

	var buf bytes.Buffer
	r := NewRunner(engine, Config{Out: &buf})
	r.undo("")

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(got) != "ORIGINAL" {
		t.Fatalf("file was not restored, contents = %q", string(got))
	}
	if out := buf.String(); !strings.Contains(out, "Undo complete") {
		t.Fatalf("expected a success message, got:\n%s", out)
	}
}
