package hooks

// Hardening tests for the proposal gate. The deadlock these guard against was
// shipped, not hypothetical: approve_proposal was listed among the tools the
// gate blocked, and approve_proposal is the only way out of the gated state.
// The session locked on the first subagent proposal and had to be resolved by
// hand from the CLI, which reaches the engine without passing through the hook.
//
// Every test below drives the real JSON pipe rather than calling the internal
// handler: a test that builds HookInput in-process cannot catch a payload-shape
// regression, and payload shape is exactly what the guard's argument-derived
// gates depend on.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crossparties/tether/internal/core"
)

// callTool sends a PreToolUse payload in the shape the guard actually reads
// (toolCall.args) and returns the decision the guard emitted.
func callTool(t *testing.T, h *Handler, toolName string, args map[string]interface{}) (decision, reason string) {
	t.Helper()
	payload := map[string]interface{}{
		"event": "PreToolUse",
		"toolCall": map[string]interface{}{
			"name": toolName,
			"args": args,
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	out, err := h.Handle("PreToolUse", strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("Handle(%s): %v", toolName, err)
	}
	var parsed struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("unmarshal output %q: %v", out, err)
	}
	return parsed.Decision, parsed.Reason
}

// pendingEngine returns an engine whose gate is armed, and fails loudly if the
// fixture did not actually arm it. Without that check every "allowed"
// assertion below would pass vacuously.
func pendingEngine(t *testing.T) *core.Engine {
	t.Helper()
	e := newTestEngine(t)
	if _, err := e.RecordProposal("tether_architect", "proposed: refactor the gate"); err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	p, err := e.GetPendingProposal()
	if err != nil {
		t.Fatalf("GetPendingProposal: %v", err)
	}
	if p == nil {
		t.Fatal("fixture did not arm the gate: no pending proposal")
	}
	return e
}

// TestProposalGateResolversAreNotStateTools is the tripwire for the deadlock.
// The proposal-control tools are the only exit from the pending state; if any
// of them is put back into stateTools, the gate blocks its own exit again and
// the session locks until the row is resolved by hand.
func TestProposalGateResolversAreNotStateTools(t *testing.T) {
	e := pendingEngine(t)
	h := NewHandler(e)

	// Data-driven on purpose: a future resolver added to stateTools fails here
	// rather than in production.
	resolvers := []string{"approve_proposal", "reject_proposal", "proposal_status"}
	for _, tool := range resolvers {
		decision, reason := callTool(t, h, tool, map[string]interface{}{})
		if decision == "deny" {
			t.Errorf("%s is denied while a proposal is pending: the gate is blocking its own exit; reason: %s",
				tool, reason)
		}
	}
}

// TestProposalGateMutatorsStillBlocked pins the other half of the contract. If
// someone "fixes" the deadlock by emptying stateTools, the resolvers pass and
// the session mutates underneath a proposal it should be honouring.
func TestProposalGateMutatorsStillBlocked(t *testing.T) {
	mutators := []struct {
		tool string
		args map[string]interface{}
	}{
		{"session_save", map[string]interface{}{"role": "assistant", "content": "sneaking a write in"}},
		{"setting_set", map[string]interface{}{"key": "mode", "value": "code"}},
		{"decision_add", map[string]interface{}{"topic": "x", "decision": "y", "reason": "z"}},
		{"pattern_add", map[string]interface{}{"type": "do", "description": "z"}},
		{"todo_add", map[string]interface{}{"description": "z"}},
		{"todo_complete", map[string]interface{}{"id": float64(1)}},
		{"todo_remove", map[string]interface{}{"id": float64(1)}},
		{"write_file", map[string]interface{}{"path": "internal/x.go", "content": "package x"}},
		{"sql_query", map[string]interface{}{"query": "UPDATE sessions SET summary='x'"}},
	}

	e := pendingEngine(t)
	h := NewHandler(e)

	for _, tc := range mutators {
		if decision, _ := callTool(t, h, tc.tool, tc.args); decision != "deny" {
			t.Errorf("%s must be denied while a proposal is pending, got %q", tc.tool, decision)
		}
	}
}

// TestProposalGateRunCommandReadOnlyVsDestructive pins the split for
// run_command. Read-only inspection is how the agent gathers evidence for the
// user's decision, so it must pass; anything that writes, deletes or installs
// must not. Collapsing this either way is a real regression: gating every
// command makes the agent useless, gating none lets a write through the shell.
func TestProposalGateRunCommandReadOnlyVsDestructive(t *testing.T) {
	readOnly := []string{
		"git status",
		"git diff HEAD",
		"ls -la",
		"grep -rn Tether internal/hooks",
		"cat internal/core/engine.go",
		"go build ./...",
	}
	destructive := []string{
		"rm -rf internal/hooks",
		"echo evil > internal/hooks/handler.go",
		"sed -i 's/deny/allow/' internal/hooks/handler.go",
		"git checkout -- internal/hooks/handler.go",
		"git reset --hard",
		"npm install left-pad",
	}

	e := pendingEngine(t)
	h := NewHandler(e)

	for _, cmd := range readOnly {
		if decision, reason := callTool(t, h, "run_command", map[string]interface{}{"command": cmd}); decision == "deny" {
			t.Errorf("read-only command %q should be allowed under a pending proposal; reason: %s", cmd, reason)
		}
	}
	for _, cmd := range destructive {
		if decision, _ := callTool(t, h, "run_command", map[string]interface{}{"command": cmd}); decision != "deny" {
			t.Errorf("destructive command %q must be denied under a pending proposal, got %q", cmd, decision)
		}
	}
}

// TestProposalGateIsSessionScoped asserts a proposal from another session does
// not freeze this one. GetPendingProposal filters on session_id, so a foreign
// row must be invisible here. This pins the scoping so one stuck session cannot
// halt every other one.
//
// The foreign row is created by rewriting session_id through the DB handle,
// because RecordProposal stamps the current session; that rewrite is the only
// way to express "another session wrote this".
func TestProposalGateIsSessionScoped(t *testing.T) {
	e := pendingEngine(t)
	h := NewHandler(e)

	if decision, _ := callTool(t, h, "decision_add", map[string]interface{}{
		"topic": "x", "decision": "y", "reason": "z",
	}); decision != "deny" {
		t.Fatalf("precondition failed: own-session proposal did not block, got %q", decision)
	}

	if _, err := e.DB.Exec(
		`UPDATE pending_proposals SET session_id = ? WHERE session_id = ?`,
		"some-other-session", e.SessionID,
	); err != nil {
		t.Fatalf("reassign proposal to a foreign session: %v", err)
	}

	p, err := e.GetPendingProposal()
	if err != nil {
		t.Fatalf("GetPendingProposal: %v", err)
	}
	if p != nil {
		t.Fatalf("GetPendingProposal leaked a foreign-session proposal: #%d owned by %q", p.ID, p.SessionID)
	}

	if decision, reason := callTool(t, h, "decision_add", map[string]interface{}{
		"topic": "x", "decision": "y", "reason": "z",
	}); decision == "deny" {
		t.Fatalf("a proposal from another session must not block this one; reason: %s", reason)
	}
}
