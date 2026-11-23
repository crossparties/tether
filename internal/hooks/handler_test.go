package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/snapshot"
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

	// 1. In code mode, call_mcp_tool and run_command must be allowed
	payloads := []string{
		`{"toolCall":{"name":"call_mcp_tool","args":{"ServerName":"tether","ToolName":"session_load"}}}`,
		`{"toolCall":{"name":"call_mcp_tool","args":{"ServerName":"tether","ToolName":"sql_query","Arguments":{"query":"SELECT 1"}}}}`,
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
	planWrite := `{"toolCall":{"name":"replace_file_content","args":{"TargetFile":` + mustQuoteTestStr(filepath.Join(e.Workspace, "internal", "core", "engine.go")) + `}}}`
	outStr, err := h.Handle("PreToolUse", strings.NewReader(planWrite))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var out HookOutput
	_ = json.Unmarshal([]byte(outStr), &out)
	if out.Decision != "deny" {
		t.Errorf("expected plan mode write to be denied, got %s", out.Decision)
	}

	// 3. In plan mode, toolCall with docs/ or .agents/ inside workspace must be allowed
	planDoc := `{"toolCall":{"name":"write_to_file","args":{"TargetFile":` + mustQuoteTestStr(filepath.Join(e.Workspace, "docs", "spec.md")) + `}}}`
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

// The gate must read the command from a snake_case tool_call payload. When
// Normalized() only looked at the camelCase field, command was always empty and
// every command-based rule silently passed.
func TestGateReadsSnakeCaseToolCall(t *testing.T) {
	e := newTestEngine(t)
	h := NewHandler(e)
	_ = e.SetMode("plan")

	// Sanity: the payload really is snake_case.
	var in HookInput
	if err := json.Unmarshal([]byte(`{"tool_name":"run_command","tool_call":{"name":"run_command","args":{"command":"echo x > f.txt"}}}`), &in); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, cmd, _ := in.Normalized(); cmd == "" {
		t.Fatal("Normalized() lost the command from a tool_call payload")
	}

	out, err := h.Handle("PreToolUse", strings.NewReader(
		`{"tool_name":"run_command","tool_call":{"name":"run_command","args":{"command":"echo x > f.txt"}}}`))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(out, `"deny"`) {
		t.Fatalf("mutative redirection should be denied in plan mode, got %s", out)
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

func TestPreToolUse_BlocksDirectDatabaseShellAccess(t *testing.T) {
	e := newTestEngine(t)
	h := NewHandler(e)
	_ = e.SetMode("code")

	for _, cmd := range []string{
		"sqlite3 .tether/session.db 'SELECT * FROM sessions'",
		"python3 -c 'import sqlite3; sqlite3.connect(\".tether/cache.db\")'",
		"cat session.db",
	} {
		payload := `{"tool_name":"run_command","tool_call":{"name":"run_command","args":{"command":` + mustQuoteTestStr(cmd) + `}}}`
		out, err := h.Handle("PreToolUse", strings.NewReader(payload))
		if err != nil {
			t.Fatalf("Handle(%q): %v", cmd, err)
		}
		if !strings.Contains(out, `"deny"`) {
			t.Errorf("expected direct db command %q to be denied, got %s", cmd, out)
		}
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

func TestPreToolUse_PlanAllowsSafeQuotedAndStderrCommands(t *testing.T) {
	e := newTestEngine(t)
	h := NewHandler(e)
	if err := e.SetMode("plan"); err != nil {
		t.Fatalf("SetMode plan: %v", err)
	}
	if _, err := h.Handle("PreInvocation", strings.NewReader(`{"conversationId":"main-1"}`)); err != nil {
		t.Fatalf("PreInvocation: %v", err)
	}

	safeCmds := []string{
		`grep "->" internal/tui/view.go 2>/dev/null`,
		`cd internal && git status --short | head -n 10`,
		`CGO_ENABLED=0 go test ./... | tail -n 5`,
	}
	for _, cmd := range safeCmds {
		payload := `{"conversationId":"main-1","tool_name":"run_command","tool_call":{"name":"run_command","args":{"command":` + mustQuoteTestStr(cmd) + `}}}`
		out, err := h.Handle("PreToolUse", strings.NewReader(payload))
		if err != nil {
			t.Fatalf("Handle(%q): %v", cmd, err)
		}
		if !strings.Contains(out, `"allow"`) {
			t.Errorf("expected read/validation command %q to be allowed in plan mode, got %s", cmd, out)
		}
	}
}

func TestPreToolUse_BlocksProtectedSubstrateWritesAndMutatingShellInCodeMode(t *testing.T) {
	e := newTestEngine(t)
	h := NewHandler(e)
	if err := e.SetMode("code"); err != nil {
		t.Fatalf("SetMode code: %v", err)
	}

	// Create symlink pointing into .tether/session.db to verify symlink resolution.
	evilLink := filepath.Join(e.Workspace, "evil.db")
	if err := os.Symlink(filepath.Join(e.Workspace, ".tether", "session.db"), evilLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	blockedWrites := []string{
		".tether/session.db",
		".tether/session.db-wal",
		".tether/cache.db",
		".tether/cache.db-shm",
		"session.db",
		"cache.db-wal",
		".tether/snapshots/turn1/main.go",
		"docs/../.tether/session.db",
		filepath.Join(e.Workspace, ".tether", "session.db"),
		evilLink,
	}
	for _, target := range blockedWrites {
		payload := `{"tool_name":"write_file","tool_call":{"name":"write_file","args":{"path":` + mustQuoteTestStr(target) + `}}}`
		out, err := h.Handle("PreToolUse", strings.NewReader(payload))
		if err != nil {
			t.Fatalf("Handle write %q: %v", target, err)
		}
		if !strings.Contains(out, `"deny"`) {
			t.Errorf("expected write to protected substrate path %q to be denied in code mode, got %s", target, out)
		}
	}

	// .tether/PLAN.md must be denied in code mode and allowed ONLY in plan mode (even via absolute workspace path).
	planPayload := `{"tool_name":"write_file","tool_call":{"name":"write_file","args":{"path":".tether/PLAN.md"}}}`
	if out, err := h.Handle("PreToolUse", strings.NewReader(planPayload)); err != nil || !strings.Contains(out, `"deny"`) {
		t.Errorf("expected .tether/PLAN.md write to be denied in code mode, got %s (err=%v)", out, err)
	}
	absPlanPayload := `{"tool_name":"write_file","tool_call":{"name":"write_file","args":{"path":` + mustQuoteTestStr(filepath.Join(e.Workspace, ".tether", "PLAN.md")) + `}}}`
	if err := e.SetMode("plan"); err != nil {
		t.Fatalf("SetMode plan: %v", err)
	}
	if out, err := h.Handle("PreToolUse", strings.NewReader(planPayload)); err != nil || !strings.Contains(out, `"allow"`) {
		t.Errorf("expected relative .tether/PLAN.md write to be allowed in plan mode, got %s (err=%v)", out, err)
	}
	if out, err := h.Handle("PreToolUse", strings.NewReader(absPlanPayload)); err != nil || !strings.Contains(out, `"allow"`) {
		t.Errorf("expected absolute .tether/PLAN.md write to be allowed in plan mode, got %s (err=%v)", out, err)
	}
	if out, err := h.Handle("PreToolUse", strings.NewReader(`{"tool_name":"read_file","tool_call":{"name":"read_file","args":{"path":`+mustQuoteTestStr(filepath.Join(e.Workspace, ".tether", "PLAN.md"))+`}}}`)); err != nil || !strings.Contains(out, `"allow"`) {
		t.Errorf("expected absolute .tether/PLAN.md read to be allowed in plan mode, got %s (err=%v)", out, err)
	}

	// Outside-workspace absolute paths must ALWAYS be denied (even if basename/folder matches /docs/).
	for _, outsideRead := range []string{"/etc/shadow", "/etc/passwd"} {
		p := `{"tool_name":"read_file","tool_call":{"name":"read_file","args":{"path":` + mustQuoteTestStr(outsideRead) + `}}}`
		if out, err := h.Handle("PreToolUse", strings.NewReader(p)); err != nil || !strings.Contains(out, `"deny"`) {
			t.Errorf("expected outside read %q to be denied, got %s (err=%v)", outsideRead, out, err)
		}
	}
	outsideDocWrite := `{"tool_name":"write_to_file","tool_call":{"name":"write_to_file","args":{"path":"/tmp/docs/x.md"}}}`
	if out, err := h.Handle("PreToolUse", strings.NewReader(outsideDocWrite)); err != nil || !strings.Contains(out, `"deny"`) {
		t.Errorf("expected /tmp/docs/x.md write to be denied in plan mode, got %s (err=%v)", out, err)
	}

	// Dependency manifests and ignore policies remain blocked in plan mode.
	for _, planDenied := range []string{"go.mod", "docs/package.json", ".tetherignore"} {
		p := `{"tool_name":"write_file","tool_call":{"name":"write_file","args":{"path":` + mustQuoteTestStr(planDenied) + `}}}`
		if out, err := h.Handle("PreToolUse", strings.NewReader(p)); err != nil || !strings.Contains(out, `"deny"`) {
			t.Errorf("expected %q write to be denied in plan mode, got %s (err=%v)", planDenied, out, err)
		}
	}

	// Traversal into .tether/session.db via docs/../ must also be denied in plan mode.
	travPayload := `{"tool_name":"write_to_file","tool_call":{"name":"write_to_file","args":{"path":"docs/../.tether/session.db"}}}`
	if out, err := h.Handle("PreToolUse", strings.NewReader(travPayload)); err != nil || !strings.Contains(out, `"deny"`) {
		t.Errorf("expected docs/../.tether/session.db traversal to be denied in plan mode, got %s (err=%v)", out, err)
	}
	_ = e.SetMode("code")

	// Custom .tether/ignore rule must block shell access in PreToolUse (Exit 2 contract).
	if err := os.WriteFile(filepath.Join(e.Workspace, ".tether", "ignore"), []byte("private_custom.txt\nre:^internal_key.*$\n"), 0644); err != nil {
		t.Fatalf("write .tether/ignore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(e.Workspace, "private_custom.txt"), []byte("secret\n"), 0600); err != nil {
		t.Fatalf("write private_custom.txt: %v", err)
	}
	for _, cmd := range []string{"cat private_custom.txt", "cat --file=private_custom.txt", "echo x > internal_key_1.txt"} {
		payload := `{"tool_name":"run_command","tool_call":{"name":"run_command","args":{"command":` + mustQuoteTestStr(cmd) + `}}}`
		if out, err := h.Handle("PreToolUse", strings.NewReader(payload)); err != nil || !strings.Contains(out, `"deny"`) {
			t.Errorf("expected custom .tether/ignore rule to deny %q in PreToolUse, got %s (err=%v)", cmd, out, err)
		}
	}

	for _, cmd := range []string{"rm -rf .tether", "mv .tether .tether.bak", "rm -rf .tether/snapshots"} {
		payload := `{"tool_name":"run_command","tool_call":{"name":"run_command","args":{"command":` + mustQuoteTestStr(cmd) + `}}}`
		out, err := h.Handle("PreToolUse", strings.NewReader(payload))
		if err != nil {
			t.Fatalf("Handle cmd %q: %v", cmd, err)
		}
		if !strings.Contains(out, `"deny"`) {
			t.Errorf("expected mutating .tether command %q to be denied, got %s", cmd, out)
		}
	}

	// Safe read-only inspections (even if substring '.tether' appears in '.tetherignore') stay allowed.
	for _, cmd := range []string{"cat .tetherignore", "ls -la .tetherignore"} {
		roPayload := `{"tool_name":"run_command","tool_call":{"name":"run_command","args":{"command":` + mustQuoteTestStr(cmd) + `}}}`
		if out, err := h.Handle("PreToolUse", strings.NewReader(roPayload)); err != nil || !strings.Contains(out, `"allow"`) {
			t.Errorf("expected read-only command %q to stay allowed, got %s (err=%v)", cmd, out, err)
		}
	}

	// Point 2 Shell Guard in PreToolUse: block secret globs, subshells, and redirections even in code mode.
	for _, cmd := range []string{"cat .env", "cat .e*", "head *.pem", "cat $(echo .env)", "echo secret > .env"} {
		payload := `{"tool_name":"run_command","tool_call":{"name":"run_command","args":{"command":` + mustQuoteTestStr(cmd) + `}}}`
		out, err := h.Handle("PreToolUse", strings.NewReader(payload))
		if err != nil || !strings.Contains(out, `"deny"`) {
			t.Errorf("expected Point 2 shell guard to deny %q in PreToolUse, got %s (err=%v)", cmd, out, err)
		}
	}
}

func mustQuoteTestStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
