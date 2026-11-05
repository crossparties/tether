package mcp

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This is an end-to-end test of the approval gate across a real process
// boundary: it builds the tether binary, speaks the stdio JSON-RPC protocol to
// it, and asserts on what a user would actually see.
//
// The in-process tests (approval_test.go) exercise Server.CallTool directly,
// which cannot catch a bug in how the CLI wires tool arguments through. The two
// bugs this guards against were both argument-plumbing bugs:
//
//   - approve_change dropped the kind, so a command grant was stored as a path
//     grant and never authorized anything.
//   - a grant scoped to "." matched nothing, so "approve the whole workspace"
//     wrote a grant that never took effect.
//
// Both looked fine in-process because those tests pass the struct fields
// directly.

// e2eServer is a running `tether mcp` process driven over stdio.
type e2eServer struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  *bufio.Writer
	stdout *bufio.Scanner
	nextID int
}

func startE2EServer(t *testing.T, dir string) *e2eServer {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "tether-e2e")
	build := exec.Command("go", "build", "-o", bin, "github.com/croosparties/tether/cmd/tether")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build tether: %v\n%s", err, out)
	}

	cmd := exec.Command(bin, "mcp")
	cmd.Dir = dir
	// Isolate HOME so the run cannot touch the developer's real session.db.
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tether mcp: %v", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	s := &e2eServer{t: t, cmd: cmd, stdin: bufio.NewWriter(stdin), stdout: scanner}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	s.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "e2e", "version": "1"},
	})
	return s
}

// call sends one JSON-RPC request and returns the concatenated result text.
func (s *e2eServer) call(method string, params map[string]any) string {
	s.t.Helper()
	s.nextID++
	req := map[string]any{"jsonrpc": "2.0", "id": s.nextID, "method": method}
	if params != nil {
		req["params"] = params
	}
	body, err := json.Marshal(req)
	if err != nil {
		s.t.Fatalf("marshal request: %v", err)
	}
	if _, err := s.stdin.Write(append(body, '\n')); err != nil {
		s.t.Fatalf("write request: %v", err)
	}
	if err := s.stdin.Flush(); err != nil {
		s.t.Fatalf("flush request: %v", err)
	}

	if !s.stdout.Scan() {
		s.t.Fatalf("%s: no response (server exited? err=%v)", method, s.stdout.Err())
	}
	line := s.stdout.Text()

	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		s.t.Fatalf("%s: bad response %q: %v", method, line, err)
	}
	if resp.Error != nil {
		s.t.Fatalf("%s: rpc error: %s", method, resp.Error.Message)
	}
	var text strings.Builder
	for _, c := range resp.Result.Content {
		text.WriteString(c.Text)
	}
	return text.String()
}

// tool calls a Tether MCP tool and returns its text payload.
func (s *e2eServer) tool(name string, args map[string]any) string {
	s.t.Helper()
	return s.call("tools/call", map[string]any{"name": name, "arguments": args})
}

// TestE2ECommandPrefixGrant is the real-CLI regression test for the approval
// gate. It proves a prefix grant survives the stdio boundary, authorizes
// matching commands, and still refuses commands outside the prefix.
func TestE2ECommandPrefixGrant(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e builds a binary")
	}

	dir := t.TempDir()
	s := startE2EServer(t, dir)

	// Step 1: an unapproved command is refused and explains itself.
	out := s.tool("run_command", map[string]any{"command": "echo first"})
	if !strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("unapproved command should be blocked, got:\n%s", out)
	}
	// The offered prefix must be the binary ("echo"), not the whole line
	// ("echo first"): a grant on the full line would never fire again.
	if !strings.Contains(out, `allow every "echo" command`) {
		t.Errorf("block message should offer an \"echo\" prefix grant, got:\n%s", out)
	}
	if strings.Contains(out, `allow every "echo first"`) {
		t.Errorf("prefix grant must not widen to the full command line, got:\n%s", out)
	}

	// Step 2: the user approves every command starting with "echo".
	// kind MUST be command-prefix; without it the grant is stored as a path
	// grant and authorizes nothing, which is the bug this test exists for.
	s.tool("approve_change", map[string]any{
		"path":  "echo",
		"scope": "all",
		"kind":  "command-prefix",
	})

	// Step 3: the same command now runs without asking.
	out = s.tool("run_command", map[string]any{"command": "echo first"})
	if strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("granted command should run, got:\n%s", out)
	}
	if !strings.Contains(out, "first") {
		t.Errorf("command output missing, got:\n%s", out)
	}

	// Step 4: the grant is a prefix, so a sibling command runs too.
	if out := s.tool("run_command", map[string]any{"command": "echo second"}); strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("prefix grant should cover sibling commands, got:\n%s", out)
	}

	// Step 5: a command outside the prefix still asks.
	if out := s.tool("run_command", map[string]any{"command": "pwd"}); !strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("command outside the prefix should still ask, got:\n%s", out)
	}

	// Step 6: a command grant must never authorize a file write.
	if out := s.tool("write_file", map[string]any{"path": "leak.txt", "content": "x"}); !strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("command grant must not authorize writes, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "leak.txt")); !os.IsNotExist(err) {
		t.Fatal("write must not have happened after a blocked call")
	}
}

// TestE2EWorkspaceRootGrant covers the "." bug: choosing "approve this whole
// workspace" used to write a grant that matched nothing, so every write kept
// asking even though the user had consented.
func TestE2EWorkspaceRootGrant(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e builds a binary")
	}

	dir := t.TempDir()
	s := startE2EServer(t, dir)

	if out := s.tool("write_file", map[string]any{"path": "a.txt", "content": "1"}); !strings.Contains(out, "APPROVAL REQUIRED") {
		t.Fatalf("write should ask first, got:\n%s", out)
	}

	s.tool("approve_change", map[string]any{"path": ".", "scope": "all"})

	for _, p := range []string{"a.txt", "nested/b.txt"} {
		if out := s.tool("write_file", map[string]any{"path": p, "content": "1"}); strings.Contains(out, "APPROVAL REQUIRED") {
			t.Fatalf("workspace-root grant should cover %s, got:\n%s", p, out)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatalf("a.txt should exist after an approved write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nested", "b.txt")); err != nil {
		t.Fatalf("nested/b.txt should exist after an approved write: %v", err)
	}
}

// TestE2EListGrantsShowsCommandKind makes sure the grant is recorded with the
// kind the user asked for. A path grant stored under a command name would pass
// the write tests but silently not authorize commands.
func TestE2EListGrantsShowsCommandKind(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e builds a binary")
	}

	dir := t.TempDir()
	s := startE2EServer(t, dir)

	s.tool("approve_change", map[string]any{"path": "make", "kind": "command-prefix", "scope": "all"})

	out := s.tool("list_grants", nil)
	if !strings.Contains(out, "command-prefix") {
		t.Fatalf("grant should be listed as command-prefix, got:\n%s", out)
	}
	if !strings.Contains(out, "make") {
		t.Fatalf("grant scope should be listed, got:\n%s", out)
	}
}
