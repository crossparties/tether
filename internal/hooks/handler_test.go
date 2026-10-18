package hooks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/croosparties/tether/internal/core"
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


