package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/croosparties/tether/internal/preview"
	"github.com/croosparties/tether/internal/ui"
)

// This file is the approval gate for the MCP path.
//
// Governance used to live only in internal/hooks, which is invoked by external
// agents through `tether hook PreToolUse`. The server's own tools called
// os.WriteFile and exec.Command directly, so every gate (catastrophic command,
// approval, plan mode) was bypassed whenever the Main Agent used Tether's own
// tools. That is why changes could land without a single proposal.
//
// The rule now enforced here is simple: a change happens only when the user has
// approved it, either for this one call or for the whole path. The only
// exemption is the substrate database, which is governed by its own MCP tools
// and must stay writable or the agent could not record what it is doing.

// substrateDBName is exempt from approval. Writing it is how the agent saves
// memory, and that path is already gated by dedicated tools.
const substrateDBName = "session.db"

// mutation describes a tool call that changes the workspace.
type mutation struct {
	tool    string
	paths   []string
	scope   string // the path a standing grant would cover
	command string // set for run_command, empty otherwise
	change  preview.Change
}

// toolRequiresApproval reports whether a tool mutates the workspace and what it
// would touch. Tools that only read never reach the gate.
func (s *Server) buildMutation(tool string, args json.RawMessage) (*mutation, error) {
	switch tool {
	case "write_file":
		var p struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.Path == "" {
			return nil, fmt.Errorf("invalid params: path and content required")
		}
		full := s.resolvePath(p.Path)
		before, existed := readIfExists(full)
		kind := preview.KindCreate
		if existed {
			kind = preview.KindUpdate
		}
		return &mutation{
			tool:  tool,
			paths: []string{full},
			scope: s.grantScope(p.Path),
			change: preview.Change{
				Kind:      kind,
				Path:      s.displayPath(full),
				Before:    before,
				After:     p.Content,
				HasBefore: existed,
			},
		}, nil

	case "edit_file":
		var p struct {
			Path        string `json:"path"`
			Target      string `json:"target"`
			Replacement string `json:"replacement"`
		}
		if err := json.Unmarshal(args, &p); err != nil || p.Path == "" || p.Target == "" {
			return nil, fmt.Errorf("invalid params: path, target and replacement required")
		}
		full := s.resolvePath(p.Path)
		before, existed := readIfExists(full)
		after := before
		if existed {
			after = strings.Replace(before, p.Target, p.Replacement, 1)
		}
		return &mutation{
			tool:  tool,
			paths: []string{full},
			scope: s.grantScope(p.Path),
			change: preview.Change{
				Kind:      preview.KindUpdate,
				Path:      s.displayPath(full),
				Before:    before,
				After:     after,
				HasBefore: existed,
				Detail:    "matching text: " + preview.RelativeTo(s.engine.Workspace, p.Target),
			},
		}, nil

	case "run_command":
		var p struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(args, &p); err != nil || strings.TrimSpace(p.Command) == "" {
			return nil, fmt.Errorf("invalid params: command required")
		}
		return &mutation{
			tool:    tool,
			paths:   nil,
			scope:   "",
			command: strings.TrimSpace(p.Command),
			change: preview.Change{
				Kind:   preview.KindCommand,
				Path:   s.engine.Workspace,
				Before: p.Command,
				Detail: preview.RelativeTo(".", s.engine.Workspace),
			},
		}, nil
	}
	return nil, nil
}

// requireApproval checks the gate for a mutating tool. It returns an empty
// string when the call may proceed, or a message explaining that approval is
// required and how to grant it.
func (s *Server) requireApproval(tool string, args json.RawMessage) string {
	m, err := s.buildMutation(tool, args)
	if err != nil {
		// Let the tool itself report the malformed arguments.
		return ""
	}
	if m == nil {
		return ""
	}

	// The substrate database is the single exemption.
	if s.isSubstrateOnly(m) {
		return ""
	}

	// Shell commands are authorized by command grants, never by path grants:
	// saying "you may write in ./internal" must not imply "you may run rm there".
	if m.command != "" {
		if ok, err := s.engine.HasCommandGrant(m.command); err == nil && ok {
			return ""
		}
		return s.approvalRequest(m)
	}

	if m.scope != "" {
		if ok, err := s.engine.HasApprovalGrant(m.scope); err == nil && ok {
			return ""
		}
	}

	return s.approvalRequest(m)
}

// approvalTag identifies one approval request so a reader can tell a fresh
// request from a repeat of an older one.
//
// Without it every block opened with the same words and the same boilerplate,
// so a run of blocked calls read as one undifferentiated wall: the user could
// not say which request they were looking at, or whether they had already
// answered it. The sequence number orders requests; the fingerprint survives a
// retry, so re-asking about an identical change is visibly the same request.
type approvalTag struct {
	seq  int
	fp   string // short fingerprint of what would change
	seen bool   // this exact change was already asked about
}

// tag records this request and reports how to label it.
func (s *Server) tag(m *mutation) approvalTag {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.approvalSeq++
	key := m.tool + "\x00" + m.scope + "\x00" + m.command + "\x00" +
		m.change.Path + "\x00" + m.change.Before + "\x00" + m.change.After

	sum := sha256.Sum256([]byte(key))
	fp := hex.EncodeToString(sum[:4]) // 8 hex chars is plenty to disambiguate

	if s.approvalSeen == nil {
		s.approvalSeen = make(map[string]bool)
	}
	seen := s.approvalSeen[key]
	s.approvalSeen[key] = true

	return approvalTag{seq: s.approvalSeq, fp: fp, seen: seen}
}

// approvalRequest renders the blocking message. It must tell the user what will
// change, where, and exactly what to call to allow it.
//
// The block is fenced by a coloured rule carrying the request identity. A plain
// header line was not enough: the message is emitted between tool output, so
// without a rule at both ends a run of blocked calls blends into the output
// above and below it and the user cannot see where one request ends and the
// next begins. The rule carries the sequence number and fingerprint so the
// fence also says which request it is closing.
func (s *Server) approvalRequest(m *mutation) string {
	t := s.tag(m)

	var b strings.Builder
	b.WriteString(approvalRule(fmt.Sprintf("approval #%d  %s", t.seq, t.fp)))
	b.WriteString("\n")

	if t.seen {
		fmt.Fprintf(&b, "APPROVAL REQUIRED #%d %s — repeated request, same change as before.\n", t.seq, t.fp)
	} else {
		fmt.Fprintf(&b, "APPROVAL REQUIRED #%d %s — nothing has been written yet.\n", t.seq, t.fp)
	}
	b.WriteString("Proposed change:\n")
	b.WriteString(m.change.Render())
	b.WriteString("\n\n")

	if m.scope != "" {
		dir := path.Dir(m.scope)
		if dir == "." || dir == "/" {
			dir = m.scope
		}
		fmt.Fprintf(&b, "To proceed, the user must choose one:\n")
		fmt.Fprintf(&b, "  approve this file:   approve_change(path=%q)\n", m.scope)
		fmt.Fprintf(&b, "  approve this folder: approve_change(path=%q, scope=\"all\")\n", dir)
		fmt.Fprintf(&b, "  reject:              reject_change(path=%q)\n\n", m.scope)
		fmt.Fprintf(&b, "\"approve this folder\" covers every file under %q until it expires,\n", dir)
		b.WriteString("so later changes there will not ask again.\n")
	} else if m.command != "" {
		// A shell command cannot be approved by path, so it needs its own
		// grant kinds. Say exactly what to call: the old message here told the
		// user to approve "for this session" without claiming a tool that
		// could actually do it, which left the agent permanently blocked.
		prefix := commandPrefix(m.command)
		b.WriteString("To proceed, the user must choose one:\n")
		fmt.Fprintf(&b, "  approve this command once:   approve_change(path=%q)\n", m.command)
		fmt.Fprintf(&b, "  allow every %q command:      approve_change(path=%q, scope=\"all\")\n\n", prefix, prefix)
		b.WriteString("A command grant only authorizes shell commands; it never authorizes\n")
		b.WriteString("writes to files, and a file grant never authorizes shell commands.\n")
	} else {
		b.WriteString("This change cannot be approved because it targets no known path or command.\n")
	}

	b.WriteString("\nRepeat the same tool call after approval; the guard re-checks the grant each time.\n")

	// Close the block with a matching rule. The trailing newline after it keeps
	// the next tool's output from butting against the fence.
	b.WriteString(approvalRule(fmt.Sprintf("end of approval #%d  %s", t.seq, t.fp)))
	b.WriteString("\n")
	return b.String()
}

// approvalRule renders a full-width coloured rule with an inline label, e.g.
//
//	━━━━━━━━━━━━━━━━━━━━ approval #7  7486658a ━━━━━━━━━━━━━━━━━━━━
//
// It is built with the same rune-width accounting as the rest of the CLI, so
// the rule stays the same length whether or not colour is enabled.
func approvalRule(label string) string {
	return ui.Warn(ui.Rule(label))
}

// commandPrefix picks the token run a prefix grant should cover. Offering the
// full command would make "allow every X command" identical to "once", so the
// suggestion keeps the leading command words and drops the arguments:
//
//	"go test ./..."        -> "go test"
//	"git commit -m 'x'"    -> "git commit"
//	"make"                 -> "make"
//
// It stops at the first argument-looking token so a grant never silently widens
// to an entire binary the user did not mean to trust.
func commandPrefix(cmd string) string {
	fields := strings.Fields(strings.TrimSpace(cmd))
	if len(fields) == 0 {
		return ""
	}
	keep := 1
	// Only widen to a subcommand when the binary actually takes one. "echo
	// first" must grant "echo", not "echo first": a prefix of "echo first"
	// matches that one line and nothing else, so the user would approve a
	// grant that never fires again.
	if len(fields) > 1 && takesSubcommand(fields[0]) && !looksLikeArg(fields[1]) {
		keep = 2
	}
	return strings.Join(fields[:keep], " ")
}

// takesSubcommand reports whether name is a tool whose second bare word is a
// subcommand ("git commit") rather than a positional argument ("echo hello").
// The list is deliberately small: widening incorrectly hands out a prefix that
// authorizes less than it claims, which is worse than granting one token less.
func takesSubcommand(name string) bool {
	switch name {
	case "git", "go", "npm", "yarn", "pnpm", "cargo", "docker", "kubectl",
		"make", "systemctl", "apt", "apt-get", "brew", "pip", "pip3", "poetry",
		"terraform", "helm", "rustc", "gcloud", "aws", "gh":
		return true
	}
	return false
}

// looksLikeArg reports whether a token is an argument rather than a subcommand.
// Flags and paths are arguments; bare words like "test" or "commit" are not.
func looksLikeArg(tok string) bool {
	if tok == "" {
		return true
	}
	if strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, ".") {
		return true
	}
	return strings.ContainsAny(tok, "=*/.")
}

// isSubstrateOnly reports whether every path in m is the substrate database.
func (s *Server) isSubstrateOnly(m *mutation) bool {
	if len(m.paths) == 0 {
		return false
	}
	dbPath := filepath.Join(s.engine.Workspace, substrateDBName)
	for _, p := range m.paths {
		if filepath.Clean(p) != filepath.Clean(dbPath) {
			return false
		}
	}
	return true
}

// grantScope converts a tool path into the prefix a standing grant would cover.
// Grants are path-scoped, so the scope is the path itself; "all under a
// directory" is expressed by granting the directory.
func (s *Server) grantScope(p string) string {
	return preview.RelativeTo(s.engine.Workspace, s.resolvePath(p))
}

// displayPath shortens a path for the preview.
func (s *Server) displayPath(full string) string {
	return preview.RelativeTo(s.engine.Workspace, full)
}

// readIfExists returns file contents and whether the file was present.
func readIfExists(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(data), true
}
