package harness

// Subagent roles, limits, and the command set a worker may use.
//
// The shape of this file follows gotalab/cc-sdd v3.0.2 rather than taste. Two
// things from that source decided it:
//
//  1. Roles are a closed set. cc-sdd ships fixed markdown prompts per role
//     (implementer, reviewer, debugger); a role name the parent invents has no
//     prompt behind it and no capability behind it either. Accepting arbitrary
//     strings would let the tool description drift away from what the runtime
//     actually enforces.
//
//  2. The parent keeps the full command set and passes a task-relevant subset
//     down (kiro-impl/SKILL.md, Preflight: "Keep the full command set in the
//     parent context, and pass only the task-relevant subset to implementer and
//     reviewer subagents"). Workers are expected to run tests and read diffs --
//     the reviewer prompt says to verify "NOT by trusting the implementer's
//     self-report" -- so blanket-denying the shell would break the protocol,
//     while leaving it open would let a worker commit.

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// DefaultSubagentRole is used when the caller omits a role. It matches the
// default the tool description advertises.
const DefaultSubagentRole = "tether_architect"

// allowedSubagentRoles is the closed set of worker roles. The value is the
// one-line description shown in tool output; it is not used for enforcement.
var allowedSubagentRoles = map[string]string{
	"tether_architect": "Architecture design and diff composition",
	"tether_qa":        "Test planning and verification script design",
	"tether_frontend":  "Frontend component design and styling diffs",
	"tether_product":   "Product specification and planning",
}

// Subagent limits. These are corrections Tether makes on its own: cc-sdd does
// not specify a timeout or a recursion cap because its workers are host-managed
// processes, while here a runaway worker would hang the REPL with no way to
// interrupt it.
const (
	SubagentTimeout  = 10 * time.Minute
	SubagentMaxSteps = 40
	MaxSubagentDepth = 2
)

// DefaultSubagentCommands is the canonical validation set derived from this
// repository, mirroring cc-sdd's Preflight step (read the manifest, then the
// project's own settings). Tether's manifest is go.mod and its settings are
// build_cmd=go build ./... and test_cmd=go test ./...
//
// git is deliberately absent: commit is parent-only in cc-sdd, and a worker
// that could stage files could bypass the selective-staging rule entirely.
var DefaultSubagentCommands = []string{
	"go test",
	"go build",
	"go vet",
}

// evidenceCommands may be run by any worker. These only observe state and are
// the commands the reviewer prompt explicitly requires ("Run `git diff` to see
// the actual code changes. This is your primary input").
var evidenceCommands = []string{
	"git diff",
	"git status",
	"git log",
	"git show",
}

// deniedGitVerbs are refused even when they appear as the git subcommand of an
// otherwise allowed invocation. cc-sdd is absolute about this: "Do NOT create
// commits" and "NEVER use `git add -A` or `git add .`".
var deniedGitVerbs = map[string]bool{
	"add":      true,
	"commit":   true,
	"reset":    true,
	"checkout": true,
	"stash":    true,
	"push":     true,
	"merge":    true,
	"rebase":   true,
	"clean":    true,
}

// ValidateSubagentRole resolves role against the closed set. An empty role
// keeps the historical default so existing call sites and prompts do not break,
// but an unknown name is an error rather than a silent fallback: a worker whose
// name the runtime does not know has no command set and no description.
func ValidateSubagentRole(role string) (string, error) {
	role = strings.TrimSpace(role)
	if role == "" {
		return DefaultSubagentRole, nil
	}
	if _, ok := allowedSubagentRoles[role]; ok {
		return role, nil
	}
	return "", fmt.Errorf("unknown subagent role %q (valid: %s)", role, SubagentRoleList())
}

// SubagentRoleNames returns the sorted role names, so callers can build menus
// and verify the list stays closed.
func SubagentRoleNames() []string {
	names := make([]string, 0, len(allowedSubagentRoles))
	for name := range allowedSubagentRoles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// SubagentRoleList returns the role names as one line for error messages and
// tool descriptions, generated from the same source of truth.
func SubagentRoleList() string {
	return strings.Join(SubagentRoleNames(), ", ")
}

// SubagentCommandAllowed reports whether a worker may run cmd. The role is a
// parameter because capability is per-role in the source protocol even when the
// command table is not: a reviewer needs git diff, an implementer needs the
// test suite, and neither needs a commit.
//
// extra is the parent-provided subset. It can widen the validation set, but it
// can never widen it past the commit prohibition -- otherwise the controller
// could hand back the one capability cc-sdd reserves for itself.
func SubagentCommandAllowed(role, cmd string, extra []string) (bool, string) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false, "empty command"
	}

	// Chaining is checked first. "go test ./... && git commit -m x" starts with
	// an allowed prefix and would otherwise sail through a prefix test, which is
	// exactly the bypass the source rule exists to prevent.
	for _, segment := range splitCommandSegments(cmd) {
		if allowed, reason := segmentAllowed(role, segment, extra); !allowed {
			return false, reason
		}
	}
	return true, ""
}

// segmentAllowed applies the rules to one command in a chain.
func segmentAllowed(role, segment string, extra []string) (bool, string) {
	if verb, ok := gitVerb(segment); ok {
		if deniedGitVerbs[verb] {
			return false, fmt.Sprintf("git %s is parent-only: subagents must propose the commit to the Main Agent", verb)
		}
		for _, allowed := range evidenceCommands {
			if hasCommandPrefix(segment, allowed) {
				return true, ""
			}
		}
		return false, fmt.Sprintf("git %s is not an evidence command (allowed: %s)", verb, strings.Join(evidenceCommands, ", "))
	}

	// An explicit subset replaces the default rather than extending it. The
	// source protocol has the parent, not the runtime, decide what a worker may
	// validate: if the controller narrowed the set to pytest it meant to drop
	// go test, and quietly keeping the default would silently widen the grant.
	//
	// git evidence commands are checked before this point and are unaffected:
	// they are how a reviewer sees the change, not part of the validation set.
	allowed := extra
	if len(allowed) == 0 {
		allowed = DefaultSubagentCommands
	}
	for _, prefix := range allowed {
		if hasCommandPrefix(segment, prefix) {
			return true, ""
		}
	}
	return false, fmt.Sprintf("command %q is outside the parent-provided validation set for %s", segment, role)
}

// splitCommandSegments breaks a shell line on the operators that start a new
// command. It is intentionally dumb about quoting: a segment split inside a
// quoted argument still has to clear the rules, so the failure mode is a false
// denial rather than a false approval.
func splitCommandSegments(cmd string) []string {
	var segments []string
	var current strings.Builder

	flush := func() {
		s := strings.TrimSpace(current.String())
		// Strip a leading pipe or ampersand left by the split itself.
		s = strings.TrimLeft(s, "|&;")
		s = strings.TrimSpace(s)
		if s != "" {
			segments = append(segments, s)
		}
		current.Reset()
	}

	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case c == ';':
			flush()
		case c == '&' || c == '|':
			// Consume the second character of && and ||
			if i+1 < len(cmd) && (cmd[i+1] == '&' || cmd[i+1] == '|') {
				i++
			}
			flush()
		case c == '\n':
			flush()
		default:
			current.WriteByte(c)
		}
	}
	flush()
	return segments
}

// gitVerb extracts the subcommand from a git invocation, skipping global flags
// such as -C and --no-pager and the values they consume.
func gitVerb(cmd string) (string, bool) {
	fields := strings.Fields(cmd)
	if len(fields) < 2 || fields[0] != "git" {
		return "", false
	}
	for i := 1; i < len(fields); i++ {
		f := fields[i]
		if f == "-C" || f == "--git-dir" || f == "--work-tree" {
			i++ // skip the flag's value
			continue
		}
		if strings.HasPrefix(f, "-") {
			continue
		}
		return f, true
	}
	return "", false
}

// hasCommandPrefix reports whether cmd begins with the whole-word prefix.
func hasCommandPrefix(cmd, prefix string) bool {
	if cmd == prefix {
		return true
	}
	return strings.HasPrefix(cmd, prefix+" ") || strings.HasPrefix(cmd, prefix+"\t")
}
