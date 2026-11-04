package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestMCPServer_Lifecycle(t *testing.T) {
	e := newTestEngine(t)
	server := NewServer(e)

	// Construct client request stream (ndjson)
	requests := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"mode_set","arguments":{"mode":"plan"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"mode_get"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"mode_set","arguments":{"mode":"code"}}}`,
	}

	inputBuf := bytes.NewBufferString(strings.Join(requests, "\n") + "\n")
	outputBuf := &bytes.Buffer{}

	err := server.ServeIO(inputBuf, outputBuf)
	if err != nil {
		t.Fatalf("ServeIO: %v", err)
	}

	// Parse responses line by line
	lines := strings.Split(strings.TrimSpace(outputBuf.String()), "\n")
	if len(lines) < 5 {
		t.Fatalf("expected at least 5 response lines, got %d. Output: %s", len(lines), outputBuf.String())
	}

	// 1. Check initialize response
	var initResp Response
	if err := json.Unmarshal([]byte(lines[0]), &initResp); err != nil {
		t.Fatalf("unmarshal init response: %v", err)
	}
	initData, _ := json.Marshal(initResp.Result)
	var initResult InitializeResult
	_ = json.Unmarshal(initData, &initResult)
	if initResult.ServerInfo.Name != "tether" {
		t.Errorf("expected server name 'tether', got %q", initResult.ServerInfo.Name)
	}

	// 2. Check tools/list response
	var toolsResp Response
	if err := json.Unmarshal([]byte(lines[1]), &toolsResp); err != nil {
		t.Fatalf("unmarshal tools response: %v", err)
	}
	toolsData, _ := json.Marshal(toolsResp.Result)
	var toolsList struct {
		Tools []Tool `json:"tools"`
	}
	_ = json.Unmarshal(toolsData, &toolsList)
	if len(toolsList.Tools) < 10 {
		t.Errorf("expected at least 10 tools, got %d", len(toolsList.Tools))
	}

	// 3. Verify mode_set to plan took effect in engine
	if e.GetMode() != "code" { // note: after request #5 it was switched back to code
		t.Errorf("expected final engine mode code, got %s", e.GetMode())
	}
}

func TestMCPServer_SQLQuery(t *testing.T) {
	e := newTestEngine(t)
	server := NewServer(e)

	// 1. Insert a decision via engine
	_ = e.AddDecision("auth", "use jwt", "stateless")

	// 2. Call sql_query tool via MCP
	callReq := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sql_query","arguments":{"query":"SELECT topic, decision FROM decisions WHERE topic = 'auth'"}}}`
	inputBuf := bytes.NewBufferString(callReq + "\n")
	outputBuf := &bytes.Buffer{}

	err := server.ServeIO(inputBuf, outputBuf)
	if err != nil {
		t.Fatalf("ServeIO: %v", err)
	}

	var resp Response
	if err := json.Unmarshal(outputBuf.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("RPC error: %s", resp.Error.Message)
	}

	toolResult, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("expected ToolResult map, got %T", resp.Result)
	}
	content := toolResult["content"].([]interface{})
	firstBlock := content[0].(map[string]interface{})
	text := firstBlock["text"].(string)

	if !strings.Contains(text, "use jwt") {
		t.Errorf("expected sql result to contain 'use jwt', got: %s", text)
	}
}


func TestProposalApprovalTools(t *testing.T) {
	e := newTestEngine(t)
	server := NewServer(e)

	run := func(tool string) string {
		t.Helper()
		out, isErr := server.CallTool(tool, json.RawMessage(`{}`))
		if isErr {
			t.Fatalf("%s returned error: %s", tool, out)
		}
		return out
	}

	// No proposal yet.
	if got := run("proposal_status"); !strings.Contains(got, `"pending": false`) {
		t.Fatalf("proposal_status with no proposal = %s", got)
	}
	if got := run("approve_proposal"); !strings.Contains(got, "no pending proposal") {
		t.Fatalf("approve_proposal with no proposal = %s", got)
	}

	// A pending proposal blocks writes.
	id, err := e.RecordProposal("tether_qa", "propose tests")
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	status := run("proposal_status")
	if !strings.Contains(status, `"pending": true`) || !strings.Contains(status, `"write_blocked": true`) {
		t.Fatalf("proposal_status with proposal = %s", status)
	}
	if !strings.Contains(status, "tether_qa") {
		t.Fatalf("proposal_status should name the role, got %s", status)
	}

	// Approving lifts the block.
	approved := run("approve_proposal")
	if !strings.Contains(approved, `"write_blocked": false`) || !strings.Contains(approved, "approved") {
		t.Fatalf("approve_proposal = %s", approved)
	}
	if blocked, _, err := e.ProposalGateBlocked(); err != nil || blocked {
		t.Fatalf("gate should be open after approve (blocked=%v err=%v)", blocked, err)
	}

	// Reject path works too.
	if _, err := e.RecordProposal("tether_architect", "rewrite"); err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	rejected := run("reject_proposal")
	if !strings.Contains(rejected, "rejected") || !strings.Contains(rejected, `"write_blocked": false`) {
		t.Fatalf("reject_proposal = %s", rejected)
	}
	if got := run("proposal_status"); !strings.Contains(got, `"pending": false`) {
		t.Fatalf("proposal_status after reject = %s", got)
	}

	_ = id
}

// The approval tools must be advertised to clients.
func TestProposalToolsListed(t *testing.T) {
	e := newTestEngine(t)
	server := NewServer(e)

	req := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"
	out := &bytes.Buffer{}
	if err := server.ServeIO(bytes.NewBufferString(req), out); err != nil {
		t.Fatalf("ServeIO: %v", err)
	}
	for _, want := range []string{"approve_proposal", "reject_proposal", "proposal_status"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("tools/list did not advertise %s", want)
		}
	}
}

// Approving by id must affect only that proposal, and the decision must be
// journalled so the audit trail survives.
func TestProposalResolveByID(t *testing.T) {
	e := newTestEngine(t)
	server := NewServer(e)

	id1, err := e.RecordProposal("tether_qa", "first")
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	id2, err := e.RecordProposal("tether_architect", "second")
	if err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}

	out, isErr := server.CallTool("approve_proposal", json.RawMessage(fmt.Sprintf(`{"proposal_id":%d}`, id1)))
	if isErr {
		t.Fatalf("approve_proposal: %s", out)
	}
	if !strings.Contains(out, `"status": "approved"`) {
		t.Fatalf("unexpected output: %s", out)
	}

	// #2 must still be pending and still block writes.
	pending, err := e.ListProposals("pending")
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != id2 {
		t.Fatalf("pending = %+v, want only #%d", pending, id2)
	}
	if blocked, _, _ := e.ProposalGateBlocked(); !blocked {
		t.Fatal("gate should remain closed while #2 is undecided")
	}

	// Re-approving the same id is reported, not silently repeated.
	if out, _ := server.CallTool("approve_proposal", json.RawMessage(fmt.Sprintf(`{"proposal_id":%d}`, id1))); !strings.Contains(out, "already approved") {
		t.Fatalf("expected 'already approved', got %s", out)
	}

	// Unknown id is reported.
	if out, _ := server.CallTool("approve_proposal", json.RawMessage(`{"proposal_id":424242}`)); !strings.Contains(out, "no proposal #424242") {
		t.Fatalf("expected 'no proposal', got %s", out)
	}

	// The decision trail is recorded in the session context.
	rows, err := e.DB.Query(`SELECT content FROM session_context WHERE role = 'decision' ORDER BY id`)
	if err != nil {
		t.Fatalf("query decisions: %v", err)
	}
	defer rows.Close()
	var found bool
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if strings.Contains(content, "first") && strings.Contains(content, "approved") {
			found = true
		}
	}
	if !found {
		t.Fatal("approval decision was not journalled to session_context")
	}
}
