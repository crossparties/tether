package harness

import (
	"reflect"
	"strings"
	"testing"
)

func TestSanitizeMessagesToolIDsArgsOrphansReasoningMergeAndIdempotency(t *testing.T) {
	overlongID := "call_" + strings.Repeat("x", 60)
	invalidCharID := "functions.read_file:0/bad@id"

	input := []chatMessage{
		{Role: "system", Content: "sys-a"},
		{Role: "system", Content: "sys-b"},
		{Role: "user", Content: "first question"},
		// DeepSeek assistant message with ONLY reasoning_content (will become empty when stripped -> deleted).
		{Role: "assistant", ReasoningContent: "only thinking, no content"},
		{Role: "user", Content: "follow up"},
		// Assistant with overlong ID, invalid-char ID, empty ID, empty/whitespace arguments, and missing 2nd tool response (orphan!).
		{
			Role:             "assistant",
			ReasoningContent: "deep reasoning trace",
			ToolCalls: []toolCall{
				{ID: overlongID, Type: "", Function: functionCall{Name: "read_file", Arguments: ""}},
				{ID: invalidCharID, Type: "function", Function: functionCall{Name: "glob", Arguments: "   "}},
				{ID: "", Type: "function", Function: functionCall{Name: "todo_manage", Arguments: `{"action":"list"}`}},
			},
		},
		// Only the 1st and 3rd tool responses arrived before abort; 2nd (invalidCharID) is orphaned!
		{Role: "tool", ToolCallID: overlongID, Content: "file content"},
		{Role: "tool", ToolCallID: "", Content: "todo list"},
		// Valid short ID should remain untouched for prefix cache stability.
		{
			Role: "assistant",
			ToolCalls: []toolCall{
				{ID: "call_valid_1", Type: "function", Function: functionCall{Name: "grep", Arguments: `{"pattern":"main"}`}},
			},
		},
		{Role: "tool", ToolCallID: "call_valid_1", Content: "main.go:1"},
	}

	got := SanitizeMessages(input, SanitizeOptions{TargetProvider: "openai"})

	// 1. Consecutive system turns merged.
	if got[0].Role != "system" || got[0].Content != "sys-a\n\nsys-b" {
		t.Fatalf("expected merged system messages, got: %+v", got[0])
	}

	// 2. Empty assistant deleted -> consecutive user turns merged.
	if got[1].Role != "user" || got[1].Content != "first question\n\nfollow up" {
		t.Fatalf("expected empty assistant deleted & user turns merged, got: %+v", got[1])
	}

	// 3. Cross-provider ReasoningContent stripped, tool_call_ids <= 40 chars [a-zA-Z0-9_-], empty args -> "{}".
	asst := got[2]
	if asst.Role != "assistant" || asst.ReasoningContent != "" {
		t.Fatalf("expected assistant with stripped ReasoningContent, got: %+v", asst)
	}
	if len(asst.ToolCalls) != 3 {
		t.Fatalf("expected 3 tool calls, got %d", len(asst.ToolCalls))
	}
	for i, tc := range asst.ToolCalls {
		if !isValidToolCallID(tc.ID) {
			t.Fatalf("tool call %d has invalid ID %q (len=%d)", i, tc.ID, len(tc.ID))
		}
		if tc.Type != "function" {
			t.Fatalf("tool call %d type = %q, want function", i, tc.Type)
		}
	}
	if asst.ToolCalls[0].Function.Arguments != "{}" || asst.ToolCalls[1].Function.Arguments != "{}" {
		t.Fatalf("empty/whitespace arguments not normalized to {}: %+v", asst.ToolCalls)
	}

	// 4. 1-to-1 tool_call_id synchronization + orphaned 2nd tool call repaired with synthetic tool reply!
	if got[3].Role != "tool" || got[3].ToolCallID != asst.ToolCalls[0].ID || got[3].Content != "file content" {
		t.Fatalf("tool[0] mismatch: %+v vs %s", got[3], asst.ToolCalls[0].ID)
	}
	if got[4].Role != "tool" || got[4].ToolCallID != asst.ToolCalls[2].ID || got[4].Content != "todo list" {
		t.Fatalf("tool[1] (bound empty ID) mismatch: %+v vs %s", got[4], asst.ToolCalls[2].ID)
	}
	if got[5].Role != "tool" || got[5].ToolCallID != asst.ToolCalls[1].ID || !strings.Contains(got[5].Content, "interrupted") {
		t.Fatalf("expected synthetic orphan reply for tool call 1 (%s), got: %+v", asst.ToolCalls[1].ID, got[5])
	}

	// 5. Existing valid ID preserved untouched.
	if got[6].ToolCalls[0].ID != "call_valid_1" || got[7].ToolCallID != "call_valid_1" {
		t.Fatalf("valid ID should be preserved, got %q / %q", got[6].ToolCalls[0].ID, got[7].ToolCallID)
	}

	// 6. Idempotency check.
	pass2 := SanitizeMessages(got, SanitizeOptions{TargetProvider: "openai"})
	if !reflect.DeepEqual(got, pass2) {
		t.Fatalf("SanitizeMessages is not idempotent!\npass1=%+v\npass2=%+v", got, pass2)
	}
}

func TestSanitizeMessagesPreservesDeepSeekReasoning(t *testing.T) {
	msgs := []chatMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "go"},
		{
			Role:             "assistant",
			ReasoningContent: "active CoT",
			ToolCalls:        []toolCall{{ID: "c1", Type: "function", Function: functionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}}},
		},
		// No tool response yet -> orphan repair inserts one while keeping ReasoningContent intact.
	}

	out := SanitizeMessages(msgs, SanitizeOptions{TargetProvider: "deepseek"})
	if out[2].ReasoningContent != "active CoT" {
		t.Fatalf("DeepSeek target must preserve ReasoningContent, got %q", out[2].ReasoningContent)
	}
	if len(out) != 4 || out[3].Role != "tool" || out[3].ToolCallID != "c1" {
		t.Fatalf("expected orphaned tool response repaired on DeepSeek too, got: %+v", out)
	}
}

func TestSanitizeMessagesDuplicateNonEmptyToolCallIDsFIFO(t *testing.T) {
	msgs := []chatMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "run"},
		{
			Role: "assistant",
			ToolCalls: []toolCall{
				{ID: "dup_id", Type: "function", Function: functionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}},
				{ID: "dup_id", Type: "function", Function: functionCall{Name: "read_file", Arguments: `{"path":"b.go"}`}},
			},
		},
		{Role: "tool", ToolCallID: "dup_id", Content: "first reply"},
		{Role: "tool", ToolCallID: "dup_id", Content: "second reply"},
	}

	out := SanitizeMessages(msgs, SanitizeOptions{TargetProvider: "openai"})
	if len(out) != 5 {
		t.Fatalf("expected exactly 5 messages (no spurious orphan replies), got %d: %+v", len(out), out)
	}
	id0 := out[2].ToolCalls[0].ID
	id1 := out[2].ToolCalls[1].ID
	if id0 == id1 || !isValidToolCallID(id0) || !isValidToolCallID(id1) {
		t.Fatalf("expected two distinct valid IDs, got %q and %q", id0, id1)
	}
	if out[3].ToolCallID != id0 || out[4].ToolCallID != id1 {
		t.Fatalf("FIFO 1-to-1 pairing broken: tool[0]=%q (want %q), tool[1]=%q (want %q)", out[3].ToolCallID, id0, out[4].ToolCallID, id1)
	}
}
