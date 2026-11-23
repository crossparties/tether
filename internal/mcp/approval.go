package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/mount"
	"github.com/crossparties/tether/internal/preview"
	"github.com/crossparties/tether/internal/safety"
	"github.com/crossparties/tether/internal/ui"
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
// approved it, either for this one call or for the whole path. Direct writes to
// .tether/ databases are not exempt from approval.

// mutation describes a tool call that changes the workspace.
type mutation struct {
	tool       string
	paths      []string
	scope      string // the path a standing grant would cover
	accessMode mount.AccessMode
	command    string // set for run_command, empty otherwise
	canonPath  string
	relPath    string
	content    string
	target     string
	replace    string
	change     preview.Change
}

// prepareMutation evaluates a mutating tool invocation once against mount & shell
// security boundaries and constructs its preview before the approval gate runs.
func (s *Server) prepareMutation(tool string, args json.RawMessage) (mut *mutation, securityErr string, paramErr string) {
	switch tool {
	case "write_file":
		var p struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(args, &p); err != nil || strings.TrimSpace(p.Path) == "" {
			return nil, "", "invalid params: path and content required"
		}
		d := s.mountEngine().AuthorizePath(p.Path, mount.OpWrite, s.mode())
		if !d.Allowed {
			return nil, d.Reason, ""
		}
		if d.IsDir {
			return nil, fmt.Sprintf("access denied: %q is a directory", d.RelPath), ""
		}
		before, existed := readIfExists(d.CanonicalPath)
		kind := preview.KindCreate
		if existed {
			kind = preview.KindUpdate
		}
		return &mutation{
			tool:       tool,
			paths:      []string{d.CanonicalPath},
			scope:      d.RelPath,
			accessMode: d.AccessMode,
			canonPath:  d.CanonicalPath,
			relPath:    d.RelPath,
			content:    p.Content,
			change: preview.Change{
				Kind:      kind,
				Path:      d.RelPath,
				Before:    before,
				After:     p.Content,
				HasBefore: existed,
			},
		}, "", ""

	case "edit_file":
		var p struct {
			Path        string `json:"path"`
			Target      string `json:"target"`
			Replacement string `json:"replacement"`
		}
		if err := json.Unmarshal(args, &p); err != nil || strings.TrimSpace(p.Path) == "" || p.Target == "" {
			return nil, "", "invalid params: path and target required"
		}
		d := s.mountEngine().AuthorizePath(p.Path, mount.OpWrite, s.mode())
		if !d.Allowed {
			return nil, d.Reason, ""
		}
		if d.IsDir {
			return nil, fmt.Sprintf("access denied: %q is a directory", d.RelPath), ""
		}
		before, existed := readIfExists(d.CanonicalPath)
		after := before
		if existed {
			after = strings.Replace(before, p.Target, p.Replacement, 1)
		}
		return &mutation{
			tool:       tool,
			paths:      []string{d.CanonicalPath},
			scope:      d.RelPath,
			accessMode: d.AccessMode,
			canonPath:  d.CanonicalPath,
			relPath:    d.RelPath,
			target:     p.Target,
			replace:    p.Replacement,
			change: preview.Change{
				Kind:      preview.KindUpdate,
				Path:      d.RelPath,
				Before:    before,
				After:     after,
				HasBefore: existed,
				Detail:    "matching text: " + preview.RelativeTo(s.workspace(), p.Target),
			},
		}, "", ""

	case "run_command":
		var p struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(args, &p); err != nil || strings.TrimSpace(p.Command) == "" {
			return nil, "", "invalid params: command required"
		}
		cmd := strings.TrimSpace(p.Command)
		if ok, reason := safety.CheckShellCommandWithMount(cmd, s.workspace(), s.shellAuthorizer()); !ok {
			return nil, reason, ""
		}
		ws := s.workspace()
		return &mutation{
			tool:    tool,
			command: cmd,
			change: preview.Change{
				Kind:   preview.KindCommand,
				Path:   ws,
				Before: p.Command,
				Detail: preview.RelativeTo(".", ws),
			},
		}, "", ""
	}
	return nil, "", ""
}

// authorizeMutation checks the approval gate for a validated mutation. It returns
// an empty string when execution may proceed, or the denial/approval message.
func (s *Server) authorizeMutation(m *mutation) string {
	if m == nil {
		return ""
	}
	if s.IsReadOnly() {
		if m.command != "" {
			if ok, _ := safety.IsReadOnlyCommand(m.command); ok {
				return ""
			}
		}
		return fmt.Sprintf("Tether Guard: read-only crew cannot mutate via %s.", m.tool)
	}
	if s.approvesAll() {
		return ""
	}
	dec := s.mutationDecision(m)

	// Shell commands are authorized by command grants, never by path grants:
	// saying "you may write in ./internal" must not imply "you may run rm there".
	if m.command != "" {
		if dec.ForceAsk {
			if s.hasForceAskGrant(m) {
				return ""
			}
			return s.gate(m)
		}
		if s.engine != nil {
			if ok, err := s.engine.HasCommandGrant(m.command); err == nil && ok {
				return ""
			}
		}
		if auto := s.AutoApprovePolicy(); auto != nil && auto(dec) {
			return ""
		}
		return s.gate(m)
	}

	if dec.ForceAsk {
		if s.hasForceAskGrant(m) {
			return ""
		}
		return s.gate(m)
	}

	if m.scope != "" && s.engine != nil {
		if ok, err := s.engine.HasApprovalGrant(m.scope); err == nil && ok {
			return ""
		}
	}

	if auto := s.AutoApprovePolicy(); auto != nil && m.accessMode != mount.AccessGuarded && auto(dec) {
		return ""
	}

	return s.gate(m)
}

func (s *Server) recordForceAskGrant(key string) {
	if !s.forcesAskWrites() || key == "" {
		return
	}
	s.askMu.Lock()
	if s.forceAskGrants == nil {
		s.forceAskGrants = make(map[string]bool)
	}
	s.forceAskGrants[key] = true
	s.askMu.Unlock()
}

func (s *Server) hasForceAskGrant(m *mutation) bool {
	if m == nil {
		return false
	}
	s.askMu.Lock()
	defer s.askMu.Unlock()
	if len(s.forceAskGrants) == 0 {
		return false
	}
	if m.command != "" {
		if s.forceAskGrants["cmd:"+m.command] {
			return true
		}
		if prefix := commandPrefix(m.command); prefix != "" && s.forceAskGrants["cmd_prefix:"+prefix] {
			return true
		}
		return false
	}
	if m.scope == "" {
		return false
	}
	if s.forceAskGrants["path:"+m.scope] {
		return true
	}
	cur := path.Dir(m.scope)
	for cur != "" && cur != "." && cur != "/" {
		if s.forceAskGrants["dir:"+cur] {
			return true
		}
		next := path.Dir(cur)
		if next == cur {
			break
		}
		cur = next
	}
	return s.forceAskGrants["dir:."]
}

func (s *Server) mutationDecision(m *mutation) Decision {
	dec := m.decision()
	if s.forcesAskWrites() {
		if m.command == "" {
			dec.ForceAsk = true
		} else if ok, _ := safety.IsReadOnlyCommand(m.command); !ok {
			dec.ForceAsk = true
		}
	}
	return dec
}

// gate turns a blocked mutation into a decision. When an interactive asker is
// attached the user is asked directly and the verdict is stored as a grant, so
// the corrected tool call passes on the retry that follows. Without an asker --
// JSON-RPC, hooks, any non-interactive caller -- the request is refused with the
// instructions it always carried, because there is nobody to ask.
func (s *Server) gate(m *mutation) string {
	if s.IsReadOnly() {
		return fmt.Sprintf("Tether Guard: read-only crew cannot mutate via %s.", m.tool)
	}
	if s.approvesAll() {
		// The user chose "approve everything": apply without asking.
		return ""
	}
	if ask := s.askPolicyWithReason(); ask != nil {
		if v, reason, ok := ask(s.mutationDecision(m)); ok {
			return s.applyVerdict(m, v, reason)
		} else if strings.TrimSpace(reason) != "" {
			return "Rejected by the user: " + strings.TrimSpace(reason) + ". Nothing was written. Do not retry the same change; ask the user what they would prefer instead."
		}
		return "Rejected by the user. Nothing was written. Do not retry the same change; " +
			"ask the user what they would prefer instead."
	}
	return s.approvalRequest(m)
}

// approvesAll reports whether the user granted blanket approval for the session.
func (s *Server) approvesAll() bool {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	return s.approveAll
}

// decision describes a blocked mutation to the interactive asker. It is inert
// data: rendering and choice live in the caller so this package stays free of
// terminal concerns.
func (m *mutation) decision() Decision {
	label := m.scope
	if m.command != "" {
		label = m.command
	}
	return Decision{
		Tool:        m.tool,
		Command:     m.command,
		Path:        m.scope,
		Label:       label,
		Preview:     m.change.Render(),
		FullPreview: m.change.RenderFull(),
	}
}

// applyVerdict records the user's answer as a grant and explains it in the
// tool result, so the model knows the retry will go through.
func (s *Server) applyVerdict(m *mutation, v Verdict, reason string) string {
	switch v {
	case VerdictSession:
		// Blanket approval lives on the server, not in the asker: the real asker
		// stays installed so a later SetAskPolicy(nil) still restores the
		// non-interactive contract for machine callers.
		s.askMu.Lock()
		s.approveAll = true
		s.askMu.Unlock()
		return "Approved for the rest of this session. Every change will be applied without asking.\n" +
			"Repeat the original tool call to apply this change."
	case VerdictFolder:
		if m.command != "" {
			prefix := commandPrefix(m.command)
			s.recordForceAskGrant("cmd_prefix:" + prefix)
			if err := s.engine.AddCommandGrant(prefix, true); err != nil {
				return fmt.Sprintf("error recording approval: %v", err)
			}
			return fmt.Sprintf("Approved: every command starting with %q is allowed until %s.\nRepeat the original tool call to run it.",
				prefix, core.GrantExpiryHint())
		}
		dir := path.Dir(m.scope)
		if dir == "." || dir == "/" {
			dir = m.scope
		}
		s.recordForceAskGrant("dir:" + dir)
		if err := s.engine.AddApprovalGrant(dir, "path"); err != nil {
			return fmt.Sprintf("error recording approval: %v", err)
		}
		return fmt.Sprintf("Approved: all changes under %q are allowed until %s (session-scoped).\nRepeat the original tool call to apply this change.",
			dir, core.GrantExpiryHint())
	case VerdictReject:
		if strings.TrimSpace(reason) != "" {
			return "Rejected by the user: " + strings.TrimSpace(reason) + ". Nothing was written. Do not retry the same change; " +
				"ask the user what they would prefer instead."
		}
		return "Rejected by the user. Nothing was written. Do not retry the same change; " +
			"ask the user what they would prefer instead."
	default: // VerdictOnce
		if m.command != "" {
			s.recordForceAskGrant("cmd:" + m.command)
			if err := s.engine.AddCommandGrant(m.command, false); err != nil {
				return fmt.Sprintf("error recording approval: %v", err)
			}
			return fmt.Sprintf("Approved: %q may run until %s.\nRepeat the original tool call to run it.",
				m.command, core.GrantExpiryHint())
		}
		s.recordForceAskGrant("path:" + m.scope)
		if err := s.engine.AddApprovalGrant(m.scope, "path"); err != nil {
			return fmt.Sprintf("error recording approval: %v", err)
		}
		return fmt.Sprintf("Approved: %q for this path until %s.\nRepeat the original tool call to apply the change.",
			m.scope, core.GrantExpiryHint())
	}
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

// CommandPrefix picks the token run a prefix grant should cover. Offering the
// full command would make "allow every X command" identical to "once", so the
// suggestion keeps the leading command words and drops the arguments:
//
//	"go test ./..."        -> "go test"
//	"git commit -m 'x'"    -> "git commit"
//	"make"                 -> "make"
//	"cd sub && rm -rf out" -> "cd sub && rm"
//
// It stops at the first argument-looking token so a grant never silently widens
// to an entire binary the user did not mean to trust.
func CommandPrefix(cmd string) string {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" {
		return ""
	}
	var prefixTokens []string
	rest := trimmed
	if strings.HasPrefix(rest, "cd ") || strings.HasPrefix(rest, "cd\t") {
		if idx := strings.Index(rest, "&&"); idx > 0 {
			cdPart := strings.TrimSpace(rest[:idx])
			cdFields := strings.Fields(cdPart)
			if len(cdFields) >= 1 && len(cdFields) <= 2 && cdFields[0] == "cd" {
				prefixTokens = append(prefixTokens, strings.Join(cdFields, " "), "&&")
				rest = strings.TrimSpace(rest[idx+2:])
			}
		}
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		if len(prefixTokens) > 0 {
			return trimmed
		}
		return ""
	}
	i := 0
	for i < len(fields) && strings.Contains(fields[i], "=") && !strings.HasPrefix(fields[i], "=") && !strings.HasPrefix(fields[i], "-") {
		prefixTokens = append(prefixTokens, fields[i])
		i++
	}
	if i >= len(fields) {
		return trimmed
	}
	cmdFields := fields[i:]
	keep := 1
	// Only widen to a subcommand when the binary actually takes one. "echo
	// first" must grant "echo", not "echo first": a prefix of "echo first"
	// matches that one line and nothing else, so the user would approve a
	// grant that never fires again.
	if len(cmdFields) > 1 && takesSubcommand(path.Base(cmdFields[0])) && !looksLikeArg(cmdFields[1]) {
		keep = 2
	}
	prefixTokens = append(prefixTokens, cmdFields[:keep]...)
	return strings.Join(prefixTokens, " ")
}

func commandPrefix(cmd string) string {
	return CommandPrefix(cmd)
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

// readIfExists returns file contents and whether the file was present.
func readIfExists(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(data), true
}
