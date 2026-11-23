package hooks

// Access tests for subagents.
//
// The regression this file guards is specific: the subagent branch used to end
// in an unconditional "allow", so the only thing standing between a worker and
// the repository was the list of things someone had already thought of. Each
// test below names a way past that list.

import (
	"encoding/json"
	"strings"
	"testing"
)

// subagentDecision drives the real handler, not the helper, so a change in how
// the handler routes subagent calls is caught here too.
//
// The main conversation is established first, exactly as the REPL does at
// startup: CheckIsSubagent decides by comparing conversation IDs against the
// stored main_conversation_id, so without one every payload below would look
// like a main agent and bypass the subagent branch entirely.
const subagentTestMainConv = "conv-main-sub-test"

func subagentDecision(t *testing.T, payload string) HookOutput {
	t.Helper()
	h := NewHandler(newTestEngine(t))
	if _, err := h.Handle("PreInvocation",
		strings.NewReader(`{"conversationId":"`+subagentTestMainConv+`","prompt":"setup"}`)); err != nil {
		t.Fatalf("PreInvocation: %v", err)
	}
	out, err := h.Handle("PreToolUse", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var res HookOutput
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal output %q: %v", out, err)
	}
	return res
}

func TestSubagentCannotTouchWorkingTree(t *testing.T) {
	// cc-sdd: "Commit (parent-only, selective staging)" and
	// "Do NOT create commits". Before this test the guard had no git rule for
	// subagents at all, so `git add -A` was permitted.
	commands := []string{
		"git add -A",
		"git add internal/hooks/handler.go",
		"git commit -m 'wip'",
		"git reset --hard HEAD~1",
		"git stash",
		"git checkout -- .",
		"git push origin main",
		// Chained forms: a prefix check on the whole line would miss these.
		"go test ./... && git commit -m x",
		"git diff; git commit -m x",
		"echo done | git apply patch.diff",
	}

	for _, cmd := range commands {
		payload := `{"conversationId":"conv-sub-worker","toolCall":{"name":"run_command","args":{"CommandLine":"` + cmd + `"}}}`
		got := subagentDecision(t, payload)
		if got.Decision != "deny" {
			t.Errorf("%q: decision = %s, want deny", cmd, got.Decision)
		}
		if !strings.Contains(got.Reason, "parent-only") {
			t.Errorf("%q: reason should cite the parent-only rule, got: %s", cmd, got.Reason)
		}
	}
}

func TestSubagentMayRunValidationCommands(t *testing.T) {
	// cc-sdd requires workers to run tests and read diffs. A ban on shell would
	// satisfy "no commits" and break verification.
	allowed := []string{
		"go test ./...",
		"go test ./internal/hooks/ -run TestFoo",
		"go build ./...",
		"go vet ./...",
		"git diff",
		"git diff --stat",
		"git status --porcelain",
		"git log --oneline -5",
	}

	for _, cmd := range allowed {
		payload := `{"conversationId":"conv-sub-worker","toolCall":{"name":"run_command","args":{"CommandLine":"` + cmd + `"}}}`
		got := subagentDecision(t, payload)
		if got.Decision != "allow" {
			t.Errorf("%q: decision = %s, want allow (reason: %s)", cmd, got.Decision, got.Reason)
		}
	}
}

func TestSubagentDeniedUnknownCommand(t *testing.T) {
	// The point of the change: an unlisted command is denied, not allowed.
	denied := []string{
		"curl http://example.com",
		"python3 -c 'print(1)'",
		"npm install",
		"cat /etc/passwd",
		"go test ./... > /tmp/out.txt",
		"tee results.log",
	}

	for _, cmd := range denied {
		payload := `{"conversationId":"conv-sub-worker","toolCall":{"name":"run_command","args":{"CommandLine":"` + cmd + `"}}}`
		got := subagentDecision(t, payload)
		if got.Decision != "deny" {
			t.Errorf("%q: decision = %s, want deny", cmd, got.Decision)
		}
	}
}

func TestSubagentDeniedUnknownTool(t *testing.T) {
	// A tool nobody anticipated. Under the old fallback this was allowed by
	// default; under an allowlist it is denied until someone adds it on purpose.
	payload := `{"conversationId":"conv-sub-worker","toolCall":{"name":"some_future_tool","args":{}}}`
	got := subagentDecision(t, payload)
	if got.Decision != "deny" {
		t.Fatalf("decision = %s, want deny", got.Decision)
	}
	if !strings.Contains(got.Reason, "read_file") {
		t.Errorf("reason should list allowed reads so the model can correct itself, got: %s", got.Reason)
	}
}

func TestSubagentSQLIsReadOnly(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		wantDec string
	}{
		{name: "select allowed", query: "SELECT * FROM todos", wantDec: "allow"},
		{name: "insert denied", query: "INSERT INTO todos VALUES (1)", wantDec: "deny"},
		{name: "update denied", query: "UPDATE todos SET status='x'", wantDec: "deny"},
		{name: "delete denied", query: "DELETE FROM todos", wantDec: "deny"},
		{name: "drop denied", query: "DROP TABLE todos", wantDec: "deny"},
		{
			// A worker must not be able to promote itself by editing the
			// substrate that records the user's decisions.
			name:    "update hidden after select",
			query:   "SELECT 1; UPDATE grants SET scope='.'",
			wantDec: "deny",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"conversationId":"conv-sub-worker","toolCall":{"name":"sql_query","args":{"query":` +
				mustJSON(tc.query) + `}}}`
			got := subagentDecision(t, payload)
			if got.Decision != tc.wantDec {
				t.Errorf("decision = %s, want %s (reason: %s)", got.Decision, tc.wantDec, got.Reason)
			}
		})
	}
}

func TestSubagentMCPWrapperIsUnwrapped(t *testing.T) {
	// The wrapper carries the real tool name in Args.ToolName. Checking only
	// the wrapper's name would let the inner tool inherit its verdict.
	tests := []struct {
		name       string
		inner      string
		innerArgs  string
		wantDenied bool
	}{
		{name: "read through wrapper allowed", inner: "read_file", innerArgs: `{"path":"a.go"}`, wantDenied: false},
		{name: "write through wrapper denied", inner: "write_file", innerArgs: `{"path":"a.go"}`, wantDenied: true},
		{name: "sql write through wrapper denied", inner: "sql_query", innerArgs: `{"query":"DELETE FROM todos"}`, wantDenied: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"conversationId":"conv-sub-worker","toolCall":{"name":"call_mcp_tool","args":{"ServerName":"tether","ToolName":` +
				mustJSON(tc.inner) + `,"Arguments":` + tc.innerArgs + `}}}`
			got := subagentDecision(t, payload)

			denied := got.Decision == "deny"
			if denied != tc.wantDenied {
				t.Fatalf("decision = %s, want denied=%v (reason: %s)", got.Decision, tc.wantDenied, got.Reason)
			}

			// SQL written through the wrapper must be caught by the SQL rule,
			// not by an incidental denial, or the wrapper becomes an approved
			// route for the very writes the allowlist forbids.
			if tc.inner == "sql_query" && !strings.Contains(got.Reason, "read") {
				t.Errorf("sql denial should cite the read-only rule, got: %s", got.Reason)
			}
		})
	}
}

func TestSubagentWriteToolsStillDenied(t *testing.T) {
	// The original rule must survive the rewrite.
	tools := []string{"write_file", "write_to_file", "edit_file", "replace_file_content", "multi_replace_file_content"}
	for _, tool := range tools {
		payload := `{"conversationId":"conv-sub-worker","toolCall":{"name":"` + tool + `","args":{"path":"x.go"}}}`
		got := subagentDecision(t, payload)
		if got.Decision != "deny" {
			t.Errorf("%s: decision = %s, want deny", tool, got.Decision)
		}
	}
}

func TestMainAgentUnaffectedBySubagentAllowlist(t *testing.T) {
	// The allowlist applies to subagents only. A main-agent command the worker
	// would be denied must still reach its own (different) gate rather than be
	// rejected by this one. The conversation ID is the main one, so the
	// subagent branch must not be reached at all.
	payload := `{"conversationId":"` + subagentTestMainConv + `","toolCall":{"name":"run_command","args":{"CommandLine":"echo hello"}}}`
	got := subagentDecision(t, payload)
	if strings.Contains(got.Reason, "subagent") {
		t.Fatalf("main agent was judged by the subagent allowlist: %s", got.Reason)
	}
}

// mustJSON quotes a string for embedding in a hand-built payload.
func mustJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
