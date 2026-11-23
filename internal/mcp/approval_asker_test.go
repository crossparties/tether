package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests in this file pin the interactive path: when an asker is installed a
// blocked write becomes a decision, and the verdict must actually let the retry
// through. Without these, the gate could keep returning the old refusal text
// while every unit test still passed.

// withAsker installs a canned answer for the duration of the test.
func withAsker(t *testing.T, s *Server, v Verdict, asked *int) {
	t.Helper()
	s.SetAskPolicy(func(d Decision) (Verdict, bool) {
		if asked != nil {
			*asked++
			if d.Tool == "" {
				t.Errorf("decision reached the asker without a tool name")
			}
		}
		return v, true
	})
}

func TestApproveOnceAllowsTheRetry(t *testing.T) {
	s, dir := newApprovalTestServer(t)
	asked := 0
	withAsker(t, s, VerdictOnce, &asked)

	out := callWrite(t, s, "notes.md", "hello\n")
	if asked != 1 {
		t.Fatalf("user should have been asked exactly once, was asked %d times", asked)
	}
	if strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("gate should have answered, not printed the old refusal:\n%s", out)
	}
	if !strings.Contains(out, "Approved") {
		t.Fatalf("tool result should tell the model it may retry, got:\n%s", out)
	}

	// The model retries, and now the write lands.
	if out := callWrite(t, s, "notes.md", "hello\n"); strings.Contains(out, "Approved") {
		t.Fatalf("retry should not ask again, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.md")); err != nil {
		t.Fatalf("file should exist after approval: %v", err)
	}
	if asked != 1 {
		t.Fatalf("retry must not re-ask, asker ran %d times", asked)
	}
}

func TestRejectLeavesTheWorkspaceUntouched(t *testing.T) {
	s, dir := newApprovalTestServer(t)
	withAsker(t, s, VerdictReject, nil)

	out := callWrite(t, s, "notes.md", "hello\n")
	if !strings.Contains(out, "Rejected by the user") {
		t.Fatalf("model must be told the user rejected, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.md")); !os.IsNotExist(err) {
		t.Fatalf("file must not exist after a rejection")
	}

	// A rejection is not a grant: the next attempt asks again.
	asked := 0
	withAsker(t, s, VerdictReject, &asked)
	callWrite(t, s, "notes.md", "hello\n")
	if asked != 1 {
		t.Fatalf("rejection must not be remembered as a grant, asker ran %d times", asked)
	}
}

func TestApproveFolderStopsAskingInThatFolder(t *testing.T) {
	s, _ := newApprovalTestServer(t)
	first := 0
	withAsker(t, s, VerdictFolder, &first)

	callWrite(t, s, "internal/a.go", "package internal\n")
	if first != 1 {
		t.Fatalf("first write should ask once, was %d", first)
	}

	// A sibling file in the same folder must not ask again.
	second := 0
	withAsker(t, s, VerdictReject, &second) // would reject if consulted
	if out := callWrite(t, s, "internal/b.go", "package internal\n"); strings.Contains(out, "Approved") {
		t.Fatalf("second write should proceed silently, got:\n%s", out)
	}
	if second != 0 {
		t.Fatalf("folder grant should suppress the prompt, asker ran %d times", second)
	}
}

func TestApproveEverythingSilencesLaterPrompts(t *testing.T) {
	s, _ := newApprovalTestServer(t)
	withAsker(t, s, VerdictSession, nil)

	callWrite(t, s, "one.go", "package one\n")

	// Any later change, in any folder, must go through without asking.
	second := 0
	s.SetAskPolicy(func(Decision) (Verdict, bool) {
		second++
		return VerdictReject, true
	})
	if out := callWrite(t, s, "elsewhere/two.go", "package two\n"); strings.Contains(out, "Approved") {
		t.Fatalf("session grant should apply silently, got:\n%s", out)
	}
	if second != 0 {
		t.Fatalf("asker ran %d times after a session-wide approval", second)
	}
}

// TestNoAskerKeepsMachineBehaviour is the contract for hook and JSON-RPC
// callers: no asker means the old refusal, never a prompt.
func TestNoAskerKeepsMachineBehaviour(t *testing.T) {
	s, dir := newApprovalTestServer(t)
	s.SetAskPolicy(nil)

	out := callWrite(t, s, "notes.md", "hello\n")
	if !strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("without an asker the gate must refuse with instructions, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.md")); !os.IsNotExist(err) {
		t.Fatalf("nothing may be written without an asker")
	}
}

// TestCancelIsRejection pins what happens when the asker reports ok=false: the
// change must be refused, not treated as an approval.
func TestCancelIsRejection(t *testing.T) {
	s, dir := newApprovalTestServer(t)
	s.SetAskPolicy(func(Decision) (Verdict, bool) { return VerdictOnce, false })

	out := callWrite(t, s, "notes.md", "hello\n")
	if !strings.Contains(out, "Rejected by the user") {
		t.Fatalf("cancelled asker must reject, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.md")); !os.IsNotExist(err) {
		t.Fatalf("cancelled approval must not write")
	}
}

func TestAutoApproveExecutesInSingleStepWithoutAsking(t *testing.T) {
	s, dir := newApprovalTestServer(t)
	asked := 0
	withAsker(t, s, VerdictReject, &asked) // would reject if asker were ever consulted
	s.SetAutoApprovePolicy(func(d Decision) bool {
		return d.Tool == "write_file" && !strings.HasSuffix(d.Path, "go.mod")
	})

	// Auto-approved write executes immediately on first call with zero retry error.
	out, isErr := func() (string, bool) {
		raw := `{"path":"internal/demo.txt","content":"auto ok\n"}`
		return s.CallTool("write_file", []byte(raw))
	}()
	if isErr {
		t.Fatalf("auto-approved write should execute cleanly without error, got: %s", out)
	}
	if strings.Contains(out, "APPROVAL REQUIRED") || strings.Contains(out, "Repeat") {
		t.Fatalf("auto-approved write must not emit retry/approval notice, got:\n%s", out)
	}
	if asked != 0 {
		t.Fatalf("asker should not run for auto-approved call, ran %d times", asked)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "internal/demo.txt")); err != nil || string(data) != "auto ok\n" {
		t.Fatalf("expected file written in single step, got data=%q err=%v", string(data), err)
	}

	// Critical target bypasses auto-approve and consults asker -> rejects.
	crit := callWrite(t, s, "go.mod", "module evil\n")
	if asked != 1 {
		t.Fatalf("critical write should consult asker once, ran %d times", asked)
	}
	if !strings.Contains(crit, "Rejected") {
		t.Fatalf("expected critical write to reach asker and reject, got:\n%s", crit)
	}
}
