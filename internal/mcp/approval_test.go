package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/croosparties/tether/internal/core"
)

func newApprovalTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	engine, err := core.NewEngine(dir)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })
	return NewServer(engine), dir
}

func callWrite(t *testing.T, s *Server, path, content string) string {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"path": path, "content": content})
	out, _ := s.CallTool("write_file", args)
	return out
}

// TestWriteFileBlockedWithoutApproval is the regression guard for the bypass
// that let changes land with an empty proposal table: a write must be refused
// before anything touches disk.
func TestWriteFileBlockedWithoutApproval(t *testing.T) {
	s, dir := newApprovalTestServer(t)

	out := callWrite(t, s, "notes.md", "hello\n")

	if !strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("write should be blocked, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.md")); !os.IsNotExist(err) {
		t.Fatalf("file must not exist after a blocked write")
	}
}

// TestBlockedResponseShowsWhatAndWhere proves the preview answers the two
// questions the user needs before deciding.
func TestBlockedResponseShowsWhatAndWhere(t *testing.T) {
	s, _ := newApprovalTestServer(t)

	out := callWrite(t, s, "docs/design.md", "line one\nline two\n")

	if !strings.Contains(out, "docs/design.md") {
		t.Errorf("preview must name the file, got:\n%s", out)
	}
	if !strings.Contains(out, "+ line one") {
		t.Errorf("preview must show the added content, got:\n%s", out)
	}
	if !strings.Contains(out, "new file") {
		t.Errorf("preview must say the file is new, got:\n%s", out)
	}
	if !strings.Contains(out, "approve_change") {
		t.Errorf("preview must tell the user how to approve, got:\n%s", out)
	}
}

// TestApproveAllCoversSiblingPaths is the feature the user asked for: one
// approval for a directory, then sibling writes proceed silently.
func TestApproveAllCoversSiblingPaths(t *testing.T) {
	s, dir := newApprovalTestServer(t)

	// First write is blocked and names the path.
	blocked := callWrite(t, s, "internal/core/engine.go", "package core\n")
	if !strings.Contains(blocked, "APPROVAL REQUIRED") {
		t.Fatalf("expected a block, got:\n%s", blocked)
	}

	// Grant the directory.
	args, _ := json.Marshal(map[string]string{"path": "internal/core", "scope": "all"})
	if out, _ := s.CallTool("approve_change", args); !strings.Contains(out, "Approved") {
		t.Fatalf("approve_change failed: %s", out)
	}

	// The original path is now allowed...
	out := callWrite(t, s, "internal/core/engine.go", "package core\n")
	if strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("granted path should be writable, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "internal/core/engine.go")); err != nil {
		t.Fatalf("file should exist after approval: %v", err)
	}

	// ...and so is a sibling under the granted directory.
	sibling := callWrite(t, s, "internal/core/grants.go", "package core\n")
	if strings.Contains(sibling, "APPROVAL REQUIRED") {
		t.Fatalf("sibling under granted dir should be writable, got:\n%s", sibling)
	}
}

// TestGrantDoesNotLeakToSiblingByName catches prefix matching done on raw
// strings: a grant for "docs" must not authorize "docs-secret".
func TestGrantDoesNotLeakToSiblingByName(t *testing.T) {
	s, _ := newApprovalTestServer(t)

	args, _ := json.Marshal(map[string]string{"path": "docs", "scope": "all"})
	s.CallTool("approve_change", args)

	out := callWrite(t, s, "docs-secret/key.pem", "secret\n")
	if !strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("grant for docs must not cover docs-secret, got:\n%s", out)
	}
}

// TestEditFileBlockedAndPreviewed covers the second mutating tool.
func TestEditFileBlockedAndPreviewed(t *testing.T) {
	s, dir := newApprovalTestServer(t)
	path := filepath.Join(dir, "config.go")
	if err := os.WriteFile(path, []byte("mode code\n"), 0644); err != nil {
		t.Fatal(err)
	}

	args, _ := json.Marshal(map[string]string{
		"path": "config.go", "target": "mode code", "replacement": "mode plan",
	})
	out, _ := s.CallTool("edit_file", args)

	if !strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("edit should be blocked, got:\n%s", out)
	}
	if !strings.Contains(out, "- mode code") || !strings.Contains(out, "+ mode plan") {
		t.Errorf("preview must show the before/after lines, got:\n%s", out)
	}
	// Untouched on disk.
	data, _ := os.ReadFile(path)
	if string(data) != "mode code\n" {
		t.Errorf("file changed despite block: %q", data)
	}
}

// TestRunCommandBlocked proves the shell is gated too.
func TestRunCommandBlocked(t *testing.T) {
	s, dir := newApprovalTestServer(t)

	args, _ := json.Marshal(map[string]string{"command": "touch created.txt"})
	out, _ := s.CallTool("run_command", args)

	if !strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("command should be blocked, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("command ran despite block")
	}
}

// TestSubstrateDatabaseIsExempt keeps session bookkeeping workable: the agent
// must be able to save memory without a prompt.
func TestSubstrateDatabaseIsExempt(t *testing.T) {
	s, _ := newApprovalTestServer(t)

	// Writing the substrate db directly is allowed (its contents are managed by
	// the session tools, but the gate must not block the path itself).
	out := callWrite(t, s, "session.db", "x")
	if strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("substrate db must be exempt from approval, got:\n%s", out)
	}
}

// TestReadToolsNeverAsk keeps the guard from getting in the way of inspection.
func TestReadToolsNeverAsk(t *testing.T) {
	s, dir := newApprovalTestServer(t)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0644)

	args, _ := json.Marshal(map[string]string{"path": "a.txt"})
	out, _ := s.CallTool("read_file", args)
	if strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("read must never ask for approval, got:\n%s", out)
	}
}

// TestRevokeGrantsRestoresSafeDefault proves the user can always get back to
// "ask me first".
func TestRevokeGrantsRestoresSafeDefault(t *testing.T) {
	s, _ := newApprovalTestServer(t)

	args, _ := json.Marshal(map[string]string{"path": "src", "scope": "all"})
	s.CallTool("approve_change", args)
	if out := callWrite(t, s, "src/a.go", "x"); strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("grant should allow the write")
	}

	s.CallTool("revoke_grants", nil)

	if out := callWrite(t, s, "src/b.go", "x"); !strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("revoked grant must block again, got:\n%s", out)
	}
}

// TestRejectChangeWritesNothing confirms rejection is a real refusal.
func TestRejectChangeWritesNothing(t *testing.T) {
	s, dir := newApprovalTestServer(t)

	args, _ := json.Marshal(map[string]string{"path": "x.go"})
	out, _ := s.CallTool("reject_change", args)
	if !strings.Contains(out, "rejected") {
		t.Fatalf("reject should confirm, got: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.go")); !os.IsNotExist(err) {
		t.Fatalf("file created after rejection")
	}
}
