package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/croosparties/tether/internal/preview"
)

func callCommand(t *testing.T, s *Server, cmd string) string {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"command": cmd})
	out, _ := s.CallTool("run_command", args)
	return out
}

// TestRunCommandFullCycle is the regression guard for the dead end where
// run_command could be blocked but never approved: the message told the user to
// "run it again with approval recorded for this session" while no tool existed
// that could record it. The cycle must close: block, approve, run.
func TestRunCommandFullCycle(t *testing.T) {
	s, dir := newApprovalTestServer(t)

	blocked := callCommand(t, s, "touch made.txt")
	if !strings.Contains(blocked, "APPROVAL REQUIRED") {
		t.Fatalf("command should be blocked, got:\n%s", blocked)
	}
	if _, err := os.Stat(filepath.Join(dir, "made.txt")); !os.IsNotExist(err) {
		t.Fatalf("command ran despite block")
	}

	// The block message must name a real escape hatch.
	if !strings.Contains(blocked, "approve_change") {
		t.Fatalf("block message must tell the user how to approve, got:\n%s", blocked)
	}

	// Approve the exact command once.
	args, _ := json.Marshal(map[string]string{"path": "touch made.txt", "kind": "command"})
	out, _ := s.CallTool("approve_change", args)
	if !strings.Contains(out, "Approved") {
		t.Fatalf("approve_change should accept a command grant, got:\n%s", out)
	}

	// Now it runs.
	ran := callCommand(t, s, "touch made.txt")
	if strings.Contains(ran, "APPROVAL REQUIRED") {
		t.Fatalf("approved command must run, got:\n%s", ran)
	}
	if _, err := os.Stat(filepath.Join(dir, "made.txt")); err != nil {
		t.Fatalf("command should have created the file: %v", err)
	}
}

// TestCommandOnceGrantIsConsumed proves "once" is not "forever": a one-shot
// grant must not authorize the second identical call.
func TestCommandOnceGrantIsConsumed(t *testing.T) {
	s, _ := newApprovalTestServer(t)

	callCommand(t, s, "touch a.txt")
	args, _ := json.Marshal(map[string]string{"path": "touch a.txt", "kind": "command"})
	s.CallTool("approve_change", args)

	if out := callCommand(t, s, "touch a.txt"); strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("first run after a once grant must proceed, got:\n%s", out)
	}
	if out := callCommand(t, s, "touch a.txt"); !strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("second run must ask again once the grant is consumed, got:\n%s", out)
	}
}

// TestCommandPrefixGrantCoversArguments is the "stop asking about go test"
// case: approving the prefix lets later invocations with arguments through.
func TestCommandPrefixGrantCoversArguments(t *testing.T) {
	s, _ := newApprovalTestServer(t)

	args, _ := json.Marshal(map[string]string{"path": "go test", "kind": "command-prefix"})
	s.CallTool("approve_change", args)

	for _, cmd := range []string{"go test", "go test ./...", "go test -run TestFoo ./internal/mcp"} {
		if out := callCommand(t, s, cmd); strings.Contains(out, "APPROVAL REQUIRED") {
			t.Fatalf("%q should be covered by the prefix grant, got:\n%s", cmd, out)
		}
	}
}

// TestCommandPrefixDoesNotMatchSubstring catches prefix matching done on raw
// strings: a grant for "go test" must not authorize "go testall" or "gofmt".
func TestCommandPrefixDoesNotMatchSubstring(t *testing.T) {
	s, _ := newApprovalTestServer(t)

	args, _ := json.Marshal(map[string]string{"path": "go test", "kind": "command-prefix"})
	s.CallTool("approve_change", args)

	for _, cmd := range []string{"go testall", "gofmt -l .", "echo go test"} {
		if out := callCommand(t, s, cmd); !strings.Contains(out, "APPROVAL REQUIRED") {
			t.Fatalf("%q must NOT be covered by a 'go test' prefix grant, got:\n%s", cmd, out)
		}
	}
}

// TestFileGrantDoesNotAuthorizeCommand is the cross-namespace guard: approving
// writes in a directory must never let the agent start running shell commands.
func TestFileGrantDoesNotAuthorizeCommand(t *testing.T) {
	s, _ := newApprovalTestServer(t)

	args, _ := json.Marshal(map[string]string{"path": ".", "scope": "all"})
	s.CallTool("approve_change", args)

	// Same path grant lets writes through...
	if out := callWrite(t, s, "ok.txt", "x"); strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("path grant should allow writes, got:\n%s", out)
	}
	// ...but not commands.
	if out := callCommand(t, s, "rm -rf /"); !strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("path grant must not authorize shell commands, got:\n%s", out)
	}
}

// TestCommandGrantDoesNotAuthorizeWrite is the mirror guard: a command grant
// whose text looks like a path must not leak into file permissions.
func TestCommandGrantDoesNotAuthorizeWrite(t *testing.T) {
	s, _ := newApprovalTestServer(t)

	// A command that happens to start with a directory-like word.
	args, _ := json.Marshal(map[string]string{"path": "internal go", "kind": "command-prefix"})
	s.CallTool("approve_change", args)

	if out := callWrite(t, s, "internal/x.go", "package x\n"); !strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("command grant must not authorize file writes, got:\n%s", out)
	}
}

// TestBlockMessageDistinguishesCommandFromFile keeps the two proposals from
// looking identical, since the remedy differs.
func TestBlockMessageDistinguishesCommandFromFile(t *testing.T) {
	s, _ := newApprovalTestServer(t)

	m := s.approvalRequest(&mutation{
		command: "git status",
		change:  preview.Change{Kind: preview.KindCommand, Before: "git status"},
	})
	if !strings.Contains(m, "command") {
		t.Fatalf("command block must mention commands, got:\n%s", m)
	}
	if strings.Contains(m, "no path grant applies") {
		t.Fatalf("dead-end message must be gone, got:\n%s", m)
	}
}

// TestCommandPrefixSuggestion keeps the offered prefix from swallowing
// arguments, which would make "all" meaningless.
func TestCommandPrefixSuggestion(t *testing.T) {
	cases := map[string]string{
		"go test ./...":       "go test",
		"git commit -m x":     "git commit",
		"make":                "make",
		"npm run build":       "npm run",
		"rm -rf node_modules": "rm",
		// Binaries that take a positional argument, not a subcommand. Widening
		// these to two words yields a prefix that matches nothing else.
		"echo first":   "echo",
		"echo hello":   "echo",
		"cat a.txt":    "cat",
		"grep -rn x .": "grep",
		// Subcommand tools still widen.
		"go build ./...": "go build",
		"docker ps -a":   "docker ps",
		"cargo test":     "cargo test",
		// Leading cd / env prefixes preserve the actual verb instead of bare "cd".
		"cd sub && python3 app.py": "cd sub && python3",
		"cd /app && rm -rf build":  "cd /app && rm",
	}
	for cmd, want := range cases {
		if got := commandPrefix(cmd); got != want {
			t.Errorf("commandPrefix(%q) = %q, want %q", cmd, got, want)
		}
	}
}
