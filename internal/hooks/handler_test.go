package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/croosparties/tether/internal/core"
	"github.com/croosparties/tether/internal/snapshot"
)

func newTestEngine(t *testing.T) *core.Engine {
	t.Helper()
	dir := t.TempDir()
	e, err := core.NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func TestPreInvocation_ModeSync(t *testing.T) {
	e := newTestEngine(t)
	h := NewHandler(e)

	// 1. Initial mode should be code
	if e.GetMode() != "code" {
		t.Fatalf("expected initial mode code, got %s", e.GetMode())
	}

	// 2. PreInvocation with /plan should set mode to plan
	inputPlan := `{"prompt": "/plan refactor database substrate"}`
	outStr, err := h.Handle("PreInvocation", strings.NewReader(inputPlan))
	if err != nil {
		t.Fatalf("Handle PreInvocation: %v", err)
	}
	if e.GetMode() != "plan" {
		t.Fatalf("expected mode plan after /plan, got %s", e.GetMode())
	}
	if !strings.Contains(outStr, "PLAN MODE activated") {
		t.Errorf("expected output to contain 'PLAN MODE activated', got %s", outStr)
	}

	// 3. PreInvocation with /code should switch back to code
	inputCode := `{"prompt": "/code implement the approved database changes"}`
	outStr, err = h.Handle("PreInvocation", strings.NewReader(inputCode))
	if err != nil {
		t.Fatalf("Handle PreInvocation: %v", err)
	}
	if e.GetMode() != "code" {
		t.Fatalf("expected mode code after /code, got %s", e.GetMode())
	}
	if !strings.Contains(outStr, "CODE MODE activated") {
		t.Errorf("expected output to contain 'CODE MODE activated', got %s", outStr)
	}
}

func TestPreToolUse_PlanModeGating(t *testing.T) {
	e := newTestEngine(t)
	h := NewHandler(e)

	// Set mode to plan
	if err := e.SetMode("plan"); err != nil {
		t.Fatalf("SetMode plan: %v", err)
	}

	tests := []struct {
		name         string
		input        HookInput
		wantDecision string
	}{
		{
			name: "block write to production go file",
			input: HookInput{
				ToolName: "write_to_file",
				FilePath: "internal/auth/jwt.go",
			},
			wantDecision: "deny",
		},
		{
			name: "block replace_file_content on production code",
			input: HookInput{
				ToolName: "replace_file_content",
				FilePath: "cmd/tether/main.go",
			},
			wantDecision: "deny",
		},
		{
			name: "allow write inside docs folder",
			input: HookInput{
				ToolName: "write_to_file",
				FilePath: "docs/plans/auth-spec.md",
			},
			wantDecision: "allow",
		},
		{
			name: "allow write inside .agents folder",
			input: HookInput{
				ToolName: "write_to_file",
				FilePath: ".agents/rules/plan-review.md",
			},
			wantDecision: "allow",
		},
		{
			name: "allow write to *.plan.md file",
			input: HookInput{
				ToolName: "write_to_file",
				FilePath: "feature_implementation.plan.md",
			},
			wantDecision: "allow",
		},
		{
			name: "block destructive shell command rm",
			input: HookInput{
				ToolName: "run_command",
				Command:  "rm -rf ./internal",
			},
			wantDecision: "deny",
		},
		{
			name: "block shell redirection overwrite >",
			input: HookInput{
				ToolName: "bash",
				Command:  "cat something > main.go",
			},
			wantDecision: "deny",
		},
		{
			name: "allow read-only test verification shell command",
			input: HookInput{
				ToolName: "run_command",
				Command:  "go test ./... 2>&1",
			},
			wantDecision: "allow",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inputBytes, _ := json.Marshal(tc.input)
			outStr, err := h.Handle("PreToolUse", strings.NewReader(string(inputBytes)))
			if err != nil {
				t.Fatalf("Handle PreToolUse: %v", err)
			}

			var out HookOutput
			if err := json.Unmarshal([]byte(outStr), &out); err != nil {
				t.Fatalf("Unmarshal HookOutput: %v", err)
			}

			if out.Decision != tc.wantDecision {
				t.Errorf("got decision %q, want %q (reason: %s)", out.Decision, tc.wantDecision, out.Reason)
			}
		})
	}
}

func TestPreToolUse_CodeModeAllowsWrites(t *testing.T) {
	e := newTestEngine(t)
	h := NewHandler(e)

	// Mode is code
	_ = e.SetMode("code")

	input := HookInput{
		ToolName: "write_to_file",
		FilePath: "internal/auth/jwt.go",
	}
	inputBytes, _ := json.Marshal(input)
	outStr, err := h.Handle("PreToolUse", strings.NewReader(string(inputBytes)))
	if err != nil {
		t.Fatalf("Handle PreToolUse: %v", err)
	}

	var out HookOutput
	_ = json.Unmarshal([]byte(outStr), &out)
	if out.Decision != "allow" {
		t.Fatalf("expected allow in code mode, got %s", out.Decision)
	}
}

func TestPreInvocation_AntiPipeGuard(t *testing.T) {
	e := newTestEngine(t)
	h := NewHandler(e)

	// Add many decisions to balloon context
	for i := 0; i < 500; i++ {
		_ = e.AddDecision("topic", "long decision content that balloons the size of the injected ephemeral context payload", "reason")
	}

	outStr, err := h.Handle("PreInvocation", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("Handle PreInvocation: %v", err)
	}

	var out HookOutput
	if err := json.Unmarshal([]byte(outStr), &out); err != nil {
		t.Fatalf("Unmarshal HookOutput: %v", err)
	}

	if len(out.InjectSteps) > 0 {
		msgLen := len(out.InjectSteps[0].EphemeralMessage)
		if msgLen > 16384 {
			t.Errorf("ephemeral message exceeded 16KB pipe guard: length = %d", msgLen)
		}
	}
}

func TestPreToolUse_AntigravityPayloadAndAutoApprove(t *testing.T) {
	e := newTestEngine(t)
	h := NewHandler(e)

	// 1. In code mode, call_mcp_tool, invoke_subagent, and run_command must be allowed
	payloads := []string{
		`{"toolCall":{"name":"call_mcp_tool","args":{"ServerName":"tether","ToolName":"session_load"}}}`,
		`{"toolCall":{"name":"call_mcp_tool","args":{"ServerName":"tether","ToolName":"sql_query","Arguments":{"query":"SELECT 1"}}}}`,
		`{"toolCall":{"name":"invoke_subagent","args":{"Subagents":[{"TypeName":"tether_architect","Role":"Architect","Prompt":"Build feature"}]}}}`,
		`{"toolCall":{"name":"run_command","args":{"CommandLine":"go test ./... 2>&1"}}}`,
		`{"toolCall":{"name":"replace_file_content","args":{"TargetFile":"main.go","ReplacementContent":"package main"}}}`,
	}

	for _, p := range payloads {
		outStr, err := h.Handle("PreToolUse", strings.NewReader(p))
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		var out HookOutput
		_ = json.Unmarshal([]byte(outStr), &out)
		if out.Decision != "allow" {
			t.Errorf("expected payload %s to be allowed, got %s (reason: %s)", p, out.Decision, out.Reason)
		}
	}

	// 2. In plan mode, toolCall with TargetFile in code directory must be denied
	_ = e.SetMode("plan")
	planWrite := `{"toolCall":{"name":"replace_file_content","args":{"TargetFile":"/repo/internal/core/engine.go"}}}`
	outStr, err := h.Handle("PreToolUse", strings.NewReader(planWrite))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var out HookOutput
	_ = json.Unmarshal([]byte(outStr), &out)
	if out.Decision != "deny" {
		t.Errorf("expected plan mode write to be denied, got %s", out.Decision)
	}

	// 3. In plan mode, toolCall with docs/ or .agents/ must be allowed
	planDoc := `{"toolCall":{"name":"write_to_file","args":{"TargetFile":"/repo/docs/spec.md"}}}`
	outStr, err = h.Handle("PreToolUse", strings.NewReader(planDoc))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	_ = json.Unmarshal([]byte(outStr), &out)
	if out.Decision != "allow" {
		t.Errorf("expected plan mode docs write to be allowed, got %s", out.Decision)
	}

	// 4. Catastrophic commands must be denied even in code mode
	_ = e.SetMode("code")
	catastrophic := `{"toolCall":{"name":"run_command","args":{"CommandLine":"rm -rf / --no-preserve-root"}}}`
	outStr, err = h.Handle("PreToolUse", strings.NewReader(catastrophic))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	_ = json.Unmarshal([]byte(outStr), &out)
	if out.Decision != "deny" {
		t.Errorf("expected catastrophic command to be denied, got %s", out.Decision)
	}
}

func TestPreToolUse_SubagentGovernanceProtocol(t *testing.T) {
	e := newTestEngine(t)
	h := NewHandler(e)

	// 1. Establish main conversation ID via PreInvocation
	mainPreInv := `{"conversationId":"conv-main-001","prompt":"let's build"}`
	_, err := h.Handle("PreInvocation", strings.NewReader(mainPreInv))
	if err != nil {
		t.Fatalf("PreInvocation: %v", err)
	}

	// 2. Main agent in code mode is authorized for edits
	mainEdit := `{"conversationId":"conv-main-001","toolCall":{"name":"replace_file_content","args":{"TargetFile":"main.go"}}}`
	outStr, err := h.Handle("PreToolUse", strings.NewReader(mainEdit))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var out HookOutput
	_ = json.Unmarshal([]byte(outStr), &out)
	if out.Decision != "allow" {
		t.Errorf("expected main agent edit to be allowed, got: %s", out.Decision)
	}

	// 3. Subagent attempting file writes must be DENIED (orchestrator approval required)
	subagentWrites := []string{
		`{"conversationId":"conv-subagent-999","toolCall":{"name":"write_to_file","args":{"TargetFile":"secret.go"}}}`,
		`{"conversationId":"conv-subagent-999","toolCall":{"name":"replace_file_content","args":{"TargetFile":"cmd/main.go"}}}`,
		`{"agent_type":"tether_architect","toolCall":{"name":"write_file","args":{"TargetFile":"util.go"}}}`,
		`{"is_subagent":true,"toolCall":{"name":"multi_replace_file_content","args":{"TargetFile":"handler.go"}}}`,
	}

	for _, sw := range subagentWrites {
		outStr, err := h.Handle("PreToolUse", strings.NewReader(sw))
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		_ = json.Unmarshal([]byte(outStr), &out)
		if out.Decision != "deny" {
			t.Errorf("expected subagent write to be denied, got: %s (payload: %s)", out.Decision, sw)
		}
		if !strings.Contains(out.Reason, "Subagents cannot mutate codebase directly") {
			t.Errorf("expected reason to mention subagents cannot mutate directly, got: %s", out.Reason)
		}
	}

	// 4. Subagent attempting mutative shell commands must be DENIED
	subagentShellMutations := []string{
		`{"conversationId":"conv-subagent-999","toolCall":{"name":"run_command","args":{"CommandLine":"rm -rf internal/core"}}}`,
		`{"conversationId":"conv-subagent-999","toolCall":{"name":"run_command","args":{"CommandLine":"mkdir -p internal/newpkg"}}}`,
		`{"agent_type":"tether_qa","toolCall":{"name":"run_command","args":{"CommandLine":"cat > main.go << 'EOF'\npackage main\nEOF"}}}`,
	}

	for _, sm := range subagentShellMutations {
		outStr, err := h.Handle("PreToolUse", strings.NewReader(sm))
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		_ = json.Unmarshal([]byte(outStr), &out)
		if out.Decision != "deny" {
			t.Errorf("expected subagent shell mutation to be denied, got: %s (payload: %s)", out.Decision, sm)
		}
	}

	// 5. Subagent reading files and querying MCP must be ALLOWED
	subagentSafeActions := []string{
		`{"conversationId":"conv-subagent-999","toolCall":{"name":"view_file","args":{"AbsolutePath":"/repo/main.go"}}}`,
		`{"conversationId":"conv-subagent-999","toolCall":{"name":"read_file","args":{"FilePath":"/repo/main.go"}}}`,
		`{"conversationId":"conv-subagent-999","toolCall":{"name":"call_mcp_tool","args":{"ServerName":"tether","ToolName":"sql_query","Arguments":{"query":"SELECT * FROM sessions"}}}}`,
		`{"conversationId":"conv-subagent-999","toolCall":{"name":"call_mcp_tool","args":{"ServerName":"tether","ToolName":"session_load"}}}`,
		`{"conversationId":"conv-subagent-999","toolCall":{"name":"send_message","args":{"Recipient":"main","Message":"Proposed changes"}}}`,
	}

	for _, sa := range subagentSafeActions {
		outStr, err := h.Handle("PreToolUse", strings.NewReader(sa))
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		_ = json.Unmarshal([]byte(outStr), &out)
		if out.Decision != "allow" {
			t.Errorf("expected subagent safe action to be allowed, got: %s (payload: %s)", out.Decision, sa)
		}
	}
}



// A subagent must never be able to claim the main conversation identity.
func TestPreInvocation_SubagentCannotHijackMainConversation(t *testing.T) {
	e := newTestEngine(t)
	h := NewHandler(e)

	// The real Main Agent registers itself first.
	if _, err := h.Handle("PreInvocation", strings.NewReader(`{"conversationId":"main-123"}`)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got, _ := e.GetProjectSetting("main_conversation_id"); got != "main-123" {
		t.Fatalf("main_conversation_id = %q, want main-123", got)
	}

	// A subagent run must be ignored.
	if _, err := h.Handle("PreInvocation", strings.NewReader(`{"conversationId":"sub-999","is_subagent":true}`)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got, _ := e.GetProjectSetting("main_conversation_id"); got != "main-123" {
		t.Fatalf("subagent hijacked main_conversation_id: got %q, want main-123", got)
	}
}

// The Main Agent must be blocked from writing while a subagent proposal awaits
// user approval, and unblocked only after approve_proposal runs.
func TestPreToolUse_ProposalApprovalGate(t *testing.T) {
	e := newTestEngine(t)
	h := NewHandler(e)

	_ = e.SetMode("code")
	if _, err := h.Handle("PreInvocation", strings.NewReader(`{"conversationId":"main-1"}`)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	writeCall := `{"tool_name":"write_file","tool_call":{"name":"write_file","args":{"path":"internal/x.go","content":"package x"}}}`

	// Without a pending proposal the write is allowed.
	outStr, err := h.Handle("PreToolUse", strings.NewReader(writeCall))
	if err != nil {
		t.Fatalf("Handle PreToolUse: %v", err)
	}
	if !strings.Contains(outStr, `"allow"`) {
		t.Fatalf("expected allow with no pending proposal, got %s", outStr)
	}

	// Once a subagent proposal is recorded, the write is denied.
	id, err := e.RecordProposal("tether_qa", "propose adapter tests")
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	outStr, err = h.Handle("PreToolUse", strings.NewReader(writeCall))
	if err != nil {
		t.Fatalf("Handle PreToolUse: %v", err)
	}
	if !strings.Contains(outStr, `"deny"`) {
		t.Fatalf("expected deny while proposal #%d pending, got %s", id, outStr)
	}
	if !strings.Contains(outStr, "awaiting user approval") {
		t.Fatalf("deny reason should mention approval, got %s", outStr)
	}

	// Mutative shell commands are gated too.
	cmdCall := `{"tool_name":"run_command","command":"rm -rf build","tool_call":{"name":"run_command","args":{"command":"rm -rf build"}}}`
	outStr, err = h.Handle("PreToolUse", strings.NewReader(cmdCall))
	if err != nil {
		t.Fatalf("Handle PreToolUse: %v", err)
	}
	if !strings.Contains(outStr, `"deny"`) {
		t.Fatalf("expected deny for mutative command while proposal pending, got %s", outStr)
	}

	// Read tools are never gated.
	readCall := `{"tool_name":"read_file","tool_call":{"name":"read_file","args":{"path":"internal/x.go"}}}`
	outStr, err = h.Handle("PreToolUse", strings.NewReader(readCall))
	if err != nil {
		t.Fatalf("Handle PreToolUse: %v", err)
	}
	if !strings.Contains(outStr, `"allow"`) {
		t.Fatalf("expected read_file to stay allowed, got %s", outStr)
	}

	// After approval the write is allowed again.
	if _, err := e.ApproveProposal(); err != nil {
		t.Fatalf("ApproveProposal: %v", err)
	}
	outStr, err = h.Handle("PreToolUse", strings.NewReader(writeCall))
	if err != nil {
		t.Fatalf("Handle PreToolUse: %v", err)
	}
	if !strings.Contains(outStr, `"allow"`) {
		t.Fatalf("expected allow after approval, got %s", outStr)
	}
}

// The proposal gate must close every door that mutates the substrate, not just
// file writes — otherwise a gated agent simply writes sideways.
func TestPreToolUse_ProposalGateCoversAllMutations(t *testing.T) {
	e := newTestEngine(t)
	h := NewHandler(e)
	_ = e.SetMode("code")
	if _, err := h.Handle("PreInvocation", strings.NewReader(`{"conversationId":"main-1"}`)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, err := e.RecordProposal("tether_qa", "pending work"); err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}

	denied := []struct {
		name string
		body string
	}{
		{"write_file", `{"tool_name":"write_file","tool_call":{"name":"write_file","args":{"path":"a.go","content":"x"}}}`},
		{"edit_file", `{"tool_name":"edit_file","tool_call":{"name":"edit_file","args":{"path":"a.go"}}}`},
		{"session_save", `{"tool_name":"session_save","tool_call":{"name":"session_save","args":{"role":"user","content":"x"}}}`},
		{"setting_set", `{"tool_name":"setting_set","tool_call":{"name":"setting_set","args":{"key":"k","value":"v"}}}`},
		{"decision_add", `{"tool_name":"decision_add","tool_call":{"name":"decision_add","args":{"topic":"t"}}}`},
		{"pattern_add", `{"tool_name":"pattern_add","tool_call":{"name":"pattern_add","args":{"type":"do"}}}`},
		{"todo_add", `{"tool_name":"todo_add","tool_call":{"name":"todo_add","args":{"description":"x"}}}`},
		{"sql_query insert", `{"tool_name":"sql_query","tool_call":{"name":"sql_query","args":{"query":"INSERT INTO todos (description) VALUES ('x')"}}}`},
		{"sql_query update", `{"tool_name":"sql_query","tool_call":{"name":"sql_query","args":{"query":"update sessions set mode='plan'"}}}`},
		{"sql_query delete", `{"tool_name":"sql_query","tool_call":{"name":"sql_query","args":{"query":"DELETE FROM todos"}}}`},
		{"cmd tee", `{"tool_name":"run_command","command":"echo hi | tee out.txt","tool_call":{"name":"run_command","args":{"command":"echo hi | tee out.txt"}}}`},
		{"cmd mkdir", `{"tool_name":"run_command","command":"mkdir newdir","tool_call":{"name":"run_command","args":{"command":"mkdir newdir"}}}`},
		{"cmd cp", `{"tool_name":"run_command","command":"cp a b","tool_call":{"name":"run_command","args":{"command":"cp a b"}}}`},
		{"cmd go generate", `{"tool_name":"run_command","command":"go generate ./...","tool_call":{"name":"run_command","args":{"command":"go generate ./..."}}}`},
	}
	for _, tc := range denied {
		out, err := h.Handle("PreToolUse", strings.NewReader(tc.body))
		if err != nil {
			t.Fatalf("%s: Handle: %v", tc.name, err)
		}
		if !strings.Contains(out, `"deny"`) {
			t.Errorf("%s should be denied while a proposal is pending, got %s", tc.name, out)
		}
	}

	// Read-only work must keep flowing.
	allowed := []struct {
		name string
		body string
	}{
		{"read_file", `{"tool_name":"read_file","tool_call":{"name":"read_file","args":{"path":"a.go"}}}`},
		{"sql_query select", `{"tool_name":"sql_query","tool_call":{"name":"sql_query","args":{"query":"SELECT * FROM todos"}}}`},
		{"session_load", `{"tool_name":"session_load","tool_call":{"name":"session_load","args":{}}}`},
		{"todo_list", `{"tool_name":"todo_list","tool_call":{"name":"todo_list","args":{}}}`},
	}
	for _, tc := range allowed {
		out, err := h.Handle("PreToolUse", strings.NewReader(tc.body))
		if err != nil {
			t.Fatalf("%s: Handle: %v", tc.name, err)
		}
		if !strings.Contains(out, `"allow"`) {
			t.Errorf("%s should stay allowed, got %s", tc.name, out)
		}
	}
}

// The gate must read the command from a snake_case tool_call payload. When
// Normalized() only looked at the camelCase field, command was always empty and
// every command-based rule silently passed.
func TestGateReadsSnakeCaseToolCall(t *testing.T) {
	e := newTestEngine(t)
	h := NewHandler(e)
	_ = e.SetMode("code")
	if _, err := h.Handle("PreInvocation", strings.NewReader(`{"conversationId":"main-1"}`)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, err := e.RecordProposal("tether_qa", "pending work"); err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}

	// Sanity: the payload really is snake_case.
	var in HookInput
	if err := json.Unmarshal([]byte(`{"tool_name":"run_command","tool_call":{"name":"run_command","args":{"command":"echo x | tee f.txt"}}}`), &in); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, cmd, _ := in.Normalized(); cmd == "" {
		t.Fatal("Normalized() lost the command from a tool_call payload")
	}

	out, err := h.Handle("PreToolUse", strings.NewReader(
		`{"tool_name":"run_command","tool_call":{"name":"run_command","args":{"command":"echo x | tee f.txt"}}}`))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out, `"deny"`) {
		t.Fatalf("tee should be denied while a proposal is pending, got %s", out)
	}

	// Read-only commands stay allowed.
	out, err = h.Handle("PreToolUse", strings.NewReader(
		`{"tool_name":"run_command","tool_call":{"name":"run_command","args":{"command":"ls -la"}}}`))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out, `"allow"`) {
		t.Fatalf("ls should stay allowed, got %s", out)
	}
}

// A file write must be snapshotted before it is allowed, so the turn can be
// undone without relying on git.
func TestPreToolUse_SnapshotsBeforeWrite(t *testing.T) {
	e := newTestEngine(t)
	h := NewHandler(e)
	_ = e.SetMode("code")
	if _, err := h.Handle("PreInvocation", strings.NewReader(`{"conversationId":"main-1"}`)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	target := filepath.Join(e.Workspace, "target.txt")
	if err := os.WriteFile(target, []byte("original"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := e.SetProjectSetting("current_turn", "turn-test"); err != nil {
		t.Fatalf("set turn: %v", err)
	}

	body := `{"tool_name":"write_file","tool_call":{"name":"write_file","args":{"path":"` +
		filepath.ToSlash(target) + `","content":"changed"}}}`
	out, err := h.Handle("PreToolUse", strings.NewReader(body))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out, `"allow"`) {
		t.Fatalf("expected allow, got %s", out)
	}

	// Simulate the write and then undo it.
	if err := os.WriteFile(target, []byte("changed"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	store := snapshot.New(e.Workspace)
	if _, err := store.Restore("turn-test", e.Workspace); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	got, _ := os.ReadFile(target)
	if string(got) != "original" {
		t.Fatalf("after undo content = %q, want %q", got, "original")
	}
}

// Snapshotting must never block a write: a missing turn or an unwritable
// snapshot dir is a warning, not a denial.
func TestPreToolUse_SnapshotFailureDoesNotBlockWrite(t *testing.T) {
	e := newTestEngine(t)
	h := NewHandler(e)
	_ = e.SetMode("code")
	if _, err := h.Handle("PreInvocation", strings.NewReader(`{"conversationId":"main-1"}`)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// No current_turn set at all.
	body := `{"tool_name":"write_file","tool_call":{"name":"write_file","args":{"path":"x/y.go","content":"package y"}}}`
	out, err := h.Handle("PreToolUse", strings.NewReader(body))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out, `"allow"`) {
		t.Fatalf("write should stay allowed when snapshotting is unavailable, got %s", out)
	}
}

// State tools have no single file to snapshot and must not attempt one.
func TestIsWriteTool(t *testing.T) {
	writes := []string{"write_file", "edit_file", "write_to_file", "replace_file_content", "multi_replace_file_content"}
	for _, name := range writes {
		if !isWriteTool(name) {
			t.Errorf("%s should be treated as a file write", name)
		}
	}
	notWrites := []string{"read_file", "run_command", "sql_query", "session_save", "todo_add"}
	for _, name := range notWrites {
		if isWriteTool(name) {
			t.Errorf("%s must not be treated as a file write", name)
		}
	}
}
