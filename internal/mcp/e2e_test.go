package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEndToEndApprovalFlow walks the exact sequence a user experiences: a write
// is refused, the preview explains it, "approve all on this path" is granted,
// and subsequent writes under that path proceed without asking.
func TestEndToEndApprovalFlow(t *testing.T) {
	s, _ := newApprovalTestServer(t)

	write := func(path, content string) string {
		args, _ := json.Marshal(map[string]string{"path": path, "content": content})
		out, _ := s.CallTool("write_file", args)
		return out
	}

	t.Log("step 1: first write is refused")
	first := write("internal/mcp/server.go", "package mcp\n")
	if !strings.Contains(first, "APPROVAL REQUIRED") {
		t.Fatalf("expected refusal, got:\n%s", first)
	}
	t.Log(first)

	t.Log("step 2: user grants the directory")
	args, _ := json.Marshal(map[string]string{"path": "internal/mcp", "scope": "all"})
	grant, _ := s.CallTool("approve_change", args)
	t.Log(grant)
	if !strings.Contains(grant, "Approved") {
		t.Fatalf("grant failed: %s", grant)
	}

	t.Log("step 3: repeating the write now succeeds")
	if out := write("internal/mcp/server.go", "package mcp\n"); strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("write should proceed after grant, got:\n%s", out)
	}

	t.Log("step 4: a sibling file under the granted path also proceeds")
	if out := write("internal/mcp/approval.go", "package mcp\n"); strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("sibling should proceed, got:\n%s", out)
	}

	t.Log("step 5: a path outside the grant still asks")
	if out := write("cmd/tether/main.go", "package main\n"); !strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("outside path should ask, got:\n%s", out)
	}
}
