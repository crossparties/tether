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
