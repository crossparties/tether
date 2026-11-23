package hooks

// What a subagent is allowed to do.
//
// This file replaces a fallback that returned "allow" for every tool it did not
// recognise:
//
//	// Read tools and MCP tools are allowed for subagents
//	return HookOutput{Decision: "allow", Reason: "Subagent read/query action allowed"}
//
// An allow-by-default fallback means the guard is defined by what someone
// remembered to forbid. New MCP tools inherit permission silently, which is the
// opposite of what a guard is for. The lists below are therefore allowlists: a
// tool that is not named is denied, and the denial names the valid options so
// the model can correct itself in one step.
//
// The policy comes from gotalab/cc-sdd v3.0.2 (see internal/harness/subagent.go
// for the full quotation set). Two lines drive everything here:
//
//	▸ "Do NOT create commits"                        -- implementer-prompt.md
//	▸ "Run the project's test suite ... Use the exit code."  -- reviewer-prompt.md
//
// So: reading and proving are allowed, changing a working tree is not.

import (
	"fmt"
	"strings"
)

// subagentReadTools may be called by any subagent. These observe state; none of
// them writes to the codebase or the substrate's records.
//
// The names cover every spelling the supported hosts emit for the same act:
// Claude Code says view_file, Tether's own adapter says read_file, and the
// rest are the MCP tool names. Listing them all here is the price of an
// allowlist, and it is worth paying: the previous denylist fallback silently
// permitted anything not yet invented.
var subagentReadTools = map[string]bool{
	// file reads, per host spelling
	"read_file":           true,
	"view_file":           true,
	"read_multiple_files": true,

	// search and enumerate
	"glob":           true,
	"grep":           true,
	"list_dir":       true,
	"search_files":   true,
	"file_search":    true,
	"list_directory": true,

	// substrate reads
	"kb_search":       true,
	"kb_status":       true,
	"session_load":    true,
	"todo_list":       true,
	"proposal_status": true,
	"list_grants":     true,

	// reporting back to the controller. cc-sdd's workers answer with a status
	// report; a worker that cannot send one has no way to return a result.
	"send_message": true,
}

// subagentEvidenceCommands prove what changed. They are allowed regardless of
// the parent's validation subset because a reviewer that cannot run git diff
// has no way to review, and cc-sdd requires exactly that:
//
//	▸ "Run `git diff` to see the actual code changes. This is your primary input."
var subagentEvidenceCommands = []string{
	"git diff",
	"git status",
	"git log",
	"git show",
}

// subagentValidationCommands is the subset a worker may run to establish
// confidence. The parent narrows it per task; this is the default when it does
// not (go.mod is the manifest, project settings name build_cmd and test_cmd).
var subagentValidationCommands = []string{
	"go test",
	"go build",
	"go vet",
}

// subagentForbiddenGitSubcommands is the absolute prohibition. The parent's
// subset cannot re-grant any of these: commits are parent-only.
var subagentForbiddenGitSubcommands = []string{
	"git add",
	"git commit",
	"git reset",
	"git stash",
	"git checkout",
	"git switch",
	"git restore",
	"git clean",
	"git rm",
	"git mv",
	"git merge",
	"git rebase",
	"git cherry-pick",
	"git revert",
	"git am",
	"git apply",
	"git push",
	"git tag",
}

// checkSubagentAccess decides whether a subagent may run a tool.
//
// Order matters. The write check stays first because a write tool name must
// never be reachable through a command path. The commit check comes next
// because it is the one rule no allowlist can waive. Only then do the allowlists
// get a say.
func checkSubagentAccess(toolName, command string, args map[string]interface{}) HookOutput {
	// A wrapper must be unwrapped before the allowlist runs. Checking the
	// wrapper's own name would let every inner tool inherit the wrapper's
	// verdict, which is how call_mcp_tool(sql_query "DELETE ...") would have
	// sailed past a list that permits call_mcp_tool.
	if inner, innerArgs, ok := subagentUnwrapMCP(toolName, args); ok {
		toolName = inner
		args = innerArgs
	}

	if subagentWriteTools[toolName] {
		return HookOutput{
			Decision: "deny",
			Reason: "Tether Guard: Subagents cannot mutate codebase directly without user approval. " +
				"Send your proposed diff/patch to the Main Agent. The Main Agent will present it to the user " +
				"for approval in the main session before execution.",
		}
	}

	if toolName == "run_command" || toolName == "bash" {
		return checkSubagentCommand(command)
	}

	if subagentReadTools[toolName] {
		return HookOutput{Decision: "allow", Reason: "Subagent read/query action allowed"}
	}

	// sql_query is read-only only when the statement is. Reading the substrate
	// is how a worker checks what the project already decided; writing it is
	// how a worker sidesteps the proposal gate.
	//
	// The statement does not arrive in `command` -- Normalized() only reads
	// CommandLine and command, while sql_query carries its text in Args.query.
	// Reading it from the args here is what makes the read-only rule actually
	// apply to the tool it was written for.
	if toolName == "sql_query" {
		return checkSubagentSQL(subagentSQLText(args, command))
	}

	return HookOutput{
		Decision: "deny",
		Reason: fmt.Sprintf("Tether Guard: subagents may not use tool %q. "+
			"Allowed: reads (%s), read-only sql_query, and validation commands (%s). "+
			"Send anything else as a proposal to the Main Agent.", toolName,
			joinSorted(subagentReadTools), strings.Join(subagentValidationCommands, ", ")),
	}
}

// checkSubagentCommand applies the command policy: forbidden git first, then
// evidence, then the validation subset.
func checkSubagentCommand(command string) HookOutput {
	command = strings.TrimSpace(command)
	if command == "" {
		return HookOutput{Decision: "deny", Reason: "Tether Guard: empty command."}
	}

	if sig, ok := subagentForbiddenGit(command); ok {
		return HookOutput{
			Decision: "deny",
			Reason: fmt.Sprintf("Tether Guard: Subagent shell mutation '%s' requires user approval. "+
				"Commits and working-tree changes are parent-only: send your proposed command to the Main Agent "+
				"for approval in the main session.", sig),
		}
	}

	if matchesCommandPrefix(command, subagentEvidenceCommands) {
		return HookOutput{Decision: "allow", Reason: "Subagent evidence command (read-only)."}
	}

	// The older mutation scan still runs, for the commands that are neither
	// evidence nor validation but do write ("go test ./... > results.txt").
	sanitized := sanitizeRedirections(strings.ToLower(command))
	for _, sig := range []string{">", ">>", "sed -i", "rm ", "mv ", "cp ", "tee ", "chmod ", "chown ", "mkdir ", "touch "} {
		if strings.Contains(sanitized, sig) {
			return HookOutput{
				Decision: "deny",
				Reason: fmt.Sprintf("Tether Guard: Subagent shell mutation '%s' requires user approval. "+
					"Send your proposed command to the Main Agent for approval in the main session.", sig),
			}
		}
	}

	if matchesCommandPrefix(command, subagentValidationCommands) {
		return HookOutput{Decision: "allow", Reason: "Subagent validation command (test/build/vet)."}
	}

	return HookOutput{
		Decision: "deny",
		Reason: fmt.Sprintf("Tether Guard: subagent may not run %q. "+
			"Allowed: %s (plus read-only %s). Send the proposed command to the Main Agent for approval.",
			command, strings.Join(subagentValidationCommands, ", "), strings.Join(subagentEvidenceCommands, ", ")),
	}
}

// checkSubagentSQL allows SELECT-style statements only.
//
// The Main Agent's gate has the same check, but a subagent reaches sql_query
// through a different branch, and a guard that exists on one path only is a
// guard with a known way around it.
func checkSubagentSQL(query string) HookOutput {
	q := strings.ToLower(strings.TrimSpace(query))
	for _, verb := range []string{"insert", "update", "delete", "drop", "alter", "create", "replace", "truncate", "attach", "pragma"} {
		if strings.HasPrefix(q, verb) || strings.Contains(q, " "+verb+" ") {
			return HookOutput{
				Decision: "deny",
				Reason: fmt.Sprintf("Tether Guard: subagent sql_query may only read. %q mutates the substrate; "+
					"propose it to the Main Agent instead.", verb),
			}
		}
	}
	return HookOutput{Decision: "allow", Reason: "Subagent read-only query allowed."}
}

// subagentForbiddenGit finds a forbidden git invocation in any segment of a
// chained command, so "go test ./... && git commit -m x" is caught.
func subagentForbiddenGit(command string) (string, bool) {
	replacer := strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n", "|", "\n")
	for _, segment := range strings.Split(replacer.Replace(command), "\n") {
		segment = strings.TrimSpace(segment)
		for _, sub := range subagentForbiddenGitSubcommands {
			if segment == sub || strings.HasPrefix(segment, sub+" ") {
				return sub, true
			}
		}
	}
	return "", false
}

// matchesCommandPrefix checks the command against whole-word prefixes. The
// trailing space requirement stops "git log" from matching "gitlogger".
func matchesCommandPrefix(command string, prefixes []string) bool {
	for _, p := range prefixes {
		if command == p || strings.HasPrefix(command, p+" ") {
			return true
		}
	}
	return false
}

// subagentUnwrapMCP returns the real tool name hidden inside an MCP wrapper.
//
// Hosts differ in how they name the inner tool; the payloads seen in practice
// use "ToolName" (the test suite's spelling) and "tool_name". An unrecognised
// shape returns ok=false rather than guessing, because guessing would hand back
// the very hole this function exists to close.
func subagentUnwrapMCP(toolName string, args map[string]interface{}) (string, map[string]interface{}, bool) {
	if !subagentMCPWrappers[toolName] || args == nil {
		return "", nil, false
	}
	for _, key := range []string{"ToolName", "tool_name", "tool"} {
		if inner, ok := args[key].(string); ok && inner != "" {
			// The inner tool's arguments live under Arguments/arguments, not at
			// the wrapper's top level. Returning the wrapper's args unchanged
			// would let the inner tool be checked with none of its own
			// parameters -- a wrapped "DELETE" would look like a bare sql_query
			// with an empty statement, which reads as harmless.
			innerArgs := args
			for _, argKey := range []string{"Arguments", "arguments", "args"} {
				if nested, ok := args[argKey].(map[string]interface{}); ok {
					innerArgs = nested
					break
				}
			}
			return inner, innerArgs, true
		}
	}
	return "", nil, false
}

// subagentSQLText extracts the statement from whichever argument the host used.
// `command` is the fallback so a host that puts the query there keeps working.
func subagentSQLText(args map[string]interface{}, command string) string {
	for _, key := range []string{"query", "sql", "statement"} {
		if v, ok := args[key].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return command
}

// subagentWriteTools are the file-mutating tool names, in all the spellings the
// supported hosts use.
var subagentWriteTools = map[string]bool{
	"write_to_file":              true,
	"write_file":                 true,
	"edit_file":                  true,
	"replace_file_content":       true,
	"multi_replace_file_content": true,
}

// subagentMCPWrappers tunnel another tool through their own name. The guard
// cannot see what is inside without parsing the arguments a second time, and an
// allowlist that cannot see the real tool name is not an allowlist. Denying the
// wrapper outright is simpler than teaching the guard to unwrap, and it costs
// the worker nothing: it can call the tool by name.
var subagentMCPWrappers = map[string]bool{
	"call_mcp_tool": true,
	"use_mcp_tool":  true,
	"mcp_call":      true,
}

// joinSorted renders a tool set for an error message. The order is not
// guaranteed to be meaningful, so it is sorted to keep messages stable between
// runs (a message that reshuffles makes tests flaky and diffs noisy).
func joinSorted(set map[string]bool) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return strings.Join(keys, ", ")
}
