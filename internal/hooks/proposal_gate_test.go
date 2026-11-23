package hooks

// This test exists because the bug it pins was invisible to every existing
// test. ApproveProposalByID worked perfectly when called directly, so the
// engine tests passed. What was broken was the path a real caller takes: the
// guard's PreToolUse hook ran first, saw a pending proposal, and denied the
// call because approve_proposal was listed among the gated state tools.
//
// The result was a deadlock: the only way to clear a pending proposal was a
// tool that the pending proposal blocked. The first subagent proposal locked
// the session permanently, and had to be resolved from the CLI, which reaches
// the engine without passing through the hook.
//
// So this test drives the guard the way the server does, through the hook, and
// asserts the exit is not walled off. A test that called the engine directly
// would pass on the broken code and prove nothing.

import (
	"encoding/json"
	"strings"
	"testing"
)

// pendingProposalHookInput builds the PreToolUse payload the MCP server sends.
// The shape matters: the guard reads argument-derived gates from the nested
// tool_call args, so a payload that puts them at the top level looks like a
// gated tool with no arguments and silently passes. Getting this wrong is how
// a test can prove nothing while appearing to run.
func pendingProposalHookInput(toolName string) string {
	in := map[string]interface{}{
		"tool_name": toolName,
		"tool_call": map[string]interface{}{
			"name": toolName,
			"args": map[string]interface{}{
				"path":        "internal/foo.go",
				"command":     "echo hi",
				"CommandLine": "echo hi",
			},
		},
	}
	b, _ := json.Marshal(in)
	return string(b)
}

// TestProposalControlToolsAreNotGatedByPendingProposal is the regression test
// for the deadlock. With a proposal pending, the tools that resolve proposals
// must be allowed through the guard.
func TestProposalControlToolsAreNotGatedByPendingProposal(t *testing.T) {
	e := newTestEngine(t)
	h := NewHandler(e)

	// A subagent proposal is pending. This is the state that used to wall off
	// every mutation, including the ones that end the state.
	if _, err := e.RecordProposal("tether_architect", "proposal body"); err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	if p, err := e.GetPendingProposal(); err != nil || p == nil {
		t.Fatalf("expected a pending proposal, got p=%v err=%v", p, err)
	}

	for _, tool := range []string{"approve_proposal", "reject_proposal", "proposal_status"} {
		out, err := h.Handle("PreToolUse", strings.NewReader(pendingProposalHookInput(tool)))
		if err != nil {
			t.Fatalf("%s: Handle: %v", tool, err)
		}
		// The guard signals a denial by writing the reason; an allowed call
		// produces no denial text. Asserting on the denial text rather than on
		// an exit code keeps this independent of how the guard is wired.
		if strings.Contains(out, "still awaiting user approval") {
			t.Errorf("%s was denied by the pending-proposal gate: that is the deadlock, since %s is the only way to clear the proposal", tool, tool)
		}
	}
}

// TestPendingProposalStillBlocksOrdinaryMutations is the other half: relaxing
// the gate for proposal control must not relax it for everything. This is what
// stops the fix from becoming "the guard no longer guards".
func TestPendingProposalStillBlocksOrdinaryMutations(t *testing.T) {
	e := newTestEngine(t)
	h := NewHandler(e)

	if _, err := e.RecordProposal("tether_architect", "proposal body"); err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}

	for _, tool := range []string{"write_file", "edit_file"} {
		out, err := h.Handle("PreToolUse", strings.NewReader(pendingProposalHookInput(tool)))
		if err != nil {
			t.Fatalf("%s: Handle: %v", tool, err)
		}
		if !strings.Contains(out, "still awaiting user approval") {
			t.Errorf("%s was NOT blocked by a pending proposal; the gate stopped guarding mutations", tool)
		}
	}

	// run_command is deliberately narrower: read-only commands (ls, git status)
	// stay available, and only destructive shapes are gated. Pin that split,
	// because collapsing it either way is a real regression: gating every
	// command would make the agent useless, gating none would let a write
	// through the shell.
	if out, err := h.Handle("PreToolUse", strings.NewReader(
		pendingProposalHookInput("run_command"))); err != nil {
		t.Fatalf("run_command (harmless): Handle: %v", err)
	} else if strings.Contains(out, "still awaiting user approval") {
		t.Errorf("a read-only run_command was blocked by a pending proposal")
	}

	destructive := `{"tool_name":"run_command","tool_call":{"name":"run_command","args":{"CommandLine":"rm -rf internal/"}}}`
	if out, err := h.Handle("PreToolUse", strings.NewReader(destructive)); err != nil {
		t.Fatalf("run_command (destructive): Handle: %v", err)
	} else if !strings.Contains(out, "still awaiting user approval") {
		t.Errorf("a destructive run_command was NOT blocked by a pending proposal")
	}
}
