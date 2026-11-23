package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/safety"
	"github.com/crossparties/tether/internal/snapshot"
)

// Handler processes lifecycle hook events from agents
type Handler struct {
	engine   *core.Engine
	snapshot *snapshot.Store
}

func NewHandler(engine *core.Engine) *Handler {
	return &Handler{engine: engine, snapshot: snapshot.New(engine.Workspace)}
}

// captureBeforeWrite records the pre-write state of a file so the turn can be
// undone. A failure here is reported but never blocks the write: the snapshot
// exists to make mistakes recoverable, so it must not become a new way for
// work to fail.
func (h *Handler) captureBeforeWrite(filePath string) {
	if filePath == "" || h.snapshot == nil {
		return
	}
	turn, err := h.engine.GetProjectSetting("current_turn")
	if err != nil || turn == "" {
		return
	}
	if err := h.snapshot.Capture(turn, filePath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not snapshot %s: %v\n", filePath, err)
	}
}

// ToolCall represents the Antigravity/Gemini toolCall object
type ToolCall struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args"`
}

// HookInput is the JSON payload agents send to hooks via stdin
type HookInput struct {
	// Accept both spellings: the runner marshals "toolCall" while tests and
	// external agents send "tool_call". Without this, ToolCall silently
	// unmarshals to nil and every argument-derived gate (notably sql_query)
	// sees nothing to inspect.
	ToolCall       *ToolCall `json:"toolCall,omitempty"`
	ToolName       string    `json:"tool_name,omitempty"`
	Command        string    `json:"command,omitempty"`
	FilePath       string    `json:"file_path,omitempty"`
	Content        string    `json:"content,omitempty"`
	AgentType      string    `json:"agent_type,omitempty"`
	Prompt         string    `json:"prompt,omitempty"`
	ConversationID string    `json:"conversationId,omitempty"`

	// ToolCallSnake captures the snake_case variant some clients send.
	ToolCallSnake *ToolCall `json:"tool_call,omitempty"`
}

// toolCallValue returns whichever tool-call payload was supplied.
func (h *HookInput) toolCallValue() *ToolCall {
	if h.ToolCall != nil {
		return h.ToolCall
	}
	return h.ToolCallSnake
}

// ArgText returns a string tool argument by name, reading the nested toolCall
// args first and falling back to nothing. Used for arguments the main gating
// path does not normalise (e.g. the SQL text of sql_query).
func (h *HookInput) ArgText(key string) string {
	if tc := h.toolCallValue(); tc != nil {
		if v, ok := tc.Args[key].(string); ok {
			return v
		}
	}
	return ""
}

func (h *HookInput) Normalized() (toolName, command, filePath string) {
	toolName = h.ToolName
	command = h.Command
	filePath = h.FilePath

	if tc := h.toolCallValue(); tc != nil {
		if tc.Name != "" {
			toolName = tc.Name
		}
		if tc.Args != nil {
			if cmd, ok := tc.Args["CommandLine"].(string); ok && cmd != "" {
				command = cmd
			} else if cmd, ok := tc.Args["command"].(string); ok && cmd != "" {
				command = cmd
			}

			for _, key := range []string{"TargetFile", "AbsolutePath", "FilePath", "path", "target_file"} {
				if fp, ok := tc.Args[key].(string); ok && fp != "" {
					filePath = fp
					break
				}
			}
		}
	}
	return
}

// HookOutput is the JSON response returned to the agent
type HookOutput struct {
	Decision    string       `json:"decision,omitempty"` // allow | deny | ask
	Reason      string       `json:"reason,omitempty"`
	InjectSteps []InjectStep `json:"injectSteps,omitempty"`
}

type InjectStep struct {
	EphemeralMessage string `json:"ephemeralMessage,omitempty"`
}

// Handle processes a hook event and returns JSON output
func (h *Handler) Handle(hookType string, input io.Reader) (string, error) {
	data, err := io.ReadAll(input)
	if err != nil {
		return "", fmt.Errorf("read input: %w", err)
	}

	var hookInput HookInput
	if len(data) > 0 {
		json.Unmarshal(data, &hookInput) // best effort
	}

	var output HookOutput

	switch hookType {
	case "PreInvocation":
		output = h.handlePreInvocation(hookInput)
	case "PreToolUse":
		output = h.handlePreToolUse(hookInput)
	case "PostToolUse":
		output = h.handlePostToolUse(hookInput)
	default:
		return "", fmt.Errorf("unknown hook type: %s", hookType)
	}

	out, err := json.Marshal(output)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// Prompt-injection budget.
//
// Everything below is re-sent to the model on every turn of every session, so
// it is a cache rather than an archive. Recent context was already capped; the
// decisions and patterns queries were not, which made the prompt grow without
// bound as a project accumulated memory. The full history stays queryable
// through sql_query, so capping here costs nothing except a lookup.
const (
	injectDecisionLimit = 5
	injectPatternLimit  = 5
	injectEntryMaxRunes = 300
)

// truncate shortens s to at most max runes, appending an ellipsis when it cuts.
// Runes rather than bytes so multi-byte text is never split mid-character.
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}

// handlePreInvocation injects workspace context + task routing hint before model runs
func (h *Handler) handlePreInvocation(input HookInput) HookOutput {
	settings, _ := h.engine.GetAllProjectSettings()
	decisions, _ := h.engine.InjectDecisions(injectDecisionLimit)
	patterns, _ := h.engine.InjectPatterns(injectPatternLimit)
	summary, _ := h.engine.GetSessionSummary()

	// DeepSeek Context Caching Optimization (Prefix Stability):
	// Place static protocol instructions and deterministically sorted project settings
	// at the very beginning of the system prompt so 64-token prefix blocks stay cached
	// across turns and tool-loop steps. Keep dynamic recent context at the end.
	context := "## Tether Context (auto-injected)\n"
	context += fmt.Sprintf("- Workspace: %s\n", h.engine.Workspace)
	context += fmt.Sprintf("- Session: %s\n", h.engine.SessionID)
	context += fmt.Sprintf("- Mode: %s (writes %s)\n", h.engine.GetMode(),
		map[bool]string{true: "allowed", false: "BLOCKED"}[h.engine.IsWriteAllowed()])

	context += "\n### ⚠️ TETHER SUBSTRATE MEMORY PROTOCOL\n"
	context += "- NEVER use python3/bash scripts to query or edit .tether/session.db or .tether/cache.db.\n"
	context += "- ALWAYS use Tether MCP tools (`sql_query`, `session_load`, `session_save`, `todo_*`) or inspect auto-injected context above.\n"

	if len(settings) > 0 {
		var keys []string
		for k := range settings {
			// Skip volatile per-turn timestamp so DeepSeek prefix cache is not invalidated every turn
			if k == "current_turn" {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			context += "\n### Project Settings\n"
			for _, k := range keys {
				context += fmt.Sprintf("- %s: %s\n", k, truncate(settings[k], 240))
			}
		}
	}

	if summary != "" {
		context += fmt.Sprintf("- Session summary: %s\n", summary)
	}

	if len(decisions) > 0 {
		context += fmt.Sprintf("\n### Previous Decisions (newest %d; full history via sql_query)\n", len(decisions))
		for _, d := range decisions {
			context += fmt.Sprintf("- [%s] %s (reason: %s)\n",
				d.Topic, truncate(d.Decision, injectEntryMaxRunes), truncate(d.Reason, injectEntryMaxRunes))
		}
	}

	if len(patterns) > 0 {
		context += fmt.Sprintf("\n### Learned Patterns (newest %d; full history via sql_query)\n", len(patterns))
		for _, p := range patterns {
			context += fmt.Sprintf("- [%s] %s\n", p.Type, truncate(p.Description, injectEntryMaxRunes))
		}
	}

	todos, _ := h.engine.ListTodos(false)
	if len(todos) > 0 {
		context += "\n### Project Tasks / Todos (Pending)\n"
		for _, t := range todos {
			context += fmt.Sprintf("- [#%d] %s\n", t.ID, t.Description)
		}
	}

	recent, _ := h.engine.GetRecentContext(50)
	var meaningful []core.ContextEntry
	for _, entry := range recent {
		if entry.Role != "tool_use" {
			meaningful = append(meaningful, entry)
			if len(meaningful) >= 6 {
				break
			}
		}
	}
	if len(meaningful) > 0 {
		context += "\n### Recent Session Context\n"
		for _, entry := range meaningful {
			context += fmt.Sprintf("- [%s/%s] %s\n", entry.Agent, entry.Role, truncate(entry.Content, 300))
		}
	}

	if input.Prompt != "" {
		mode := core.ClassifyTask(input.Prompt)
		var routeHint string
		switch mode {
		case "tether_plan":
			_ = h.engine.SetMode("plan")
			routeHint = "🔵 PLAN MODE activated → Planning & specification active. Writes to codebase restricted."
		case "tether_code":
			_ = h.engine.SetMode("code")
			routeHint = "🟢 CODE MODE activated → Codebase mutation unlocked."
		case "tether_review":
			routeHint = "🟡 REVIEW MODE detected → Read-only codebase audit and verification."
		case "tether_direct":
			routeHint = "🟢 DIRECT MODE active → Full coding and tool execution enabled."
		default:
			routeHint = "🟢 DIRECT MODE active → Default coding mode. Use /plan for design phase."
		}
		context += fmt.Sprintf("\n### Task Router\n%s\n", routeHint)
	}

	if h.engine.GetMode() == "plan" {
		context += "\n### ⚠️ PLANNING MODE ACTIVE\n"
		context += "- Codebase mutation is BLOCKED.\n"
		context += "- Plan artifacts in docs/ and .agents/ are ALLOWED.\n"
		context += "- Present plan inline in response.\n"
		context += "- Run 'tether mode code' or call MCP mode_set(mode=\"code\") when ready to implement.\n"
	}

	// Anti-Pipe Deadlock: Hard cap at 16KB
	if len(context) > 16384 {
		context = context[:16300] + "\n... [truncated by Tether pipe guard]\n"
	}

	return HookOutput{
		InjectSteps: []InjectStep{
			{EphemeralMessage: context},
		},
	}
}

// handlePreToolUse gates operations according to Tether governance rules
func (h *Handler) handlePreToolUse(input HookInput) HookOutput {
	toolName, command, filePath := input.Normalized()

	// 1. Catastrophic command & protected substrate protection (always active across all modes)
	if toolName == "run_command" || toolName == "bash" {
		if d := classifyCommand(command); d.level == deny {
			return HookOutput{Decision: "deny", Reason: d.describe()}
		}
		cmdLower := strings.ToLower(command)

		// Prevent raw bash access to .tether/*.db or mutative commands touching .tether/.
		isDirectDB := strings.Contains(cmdLower, "session.db") || strings.Contains(cmdLower, "cache.db") ||
			(strings.Contains(cmdLower, "sqlite3") && strings.Contains(cmdLower, ".db"))
		isMutatingTether := false
		if strings.Contains(cmdLower, ".tether") {
			okRO, _ := safety.IsReadOnlyCommand(command)
			isMutatingTether = !okRO
		}
		if isDirectDB || isMutatingTether {
			return HookOutput{
				Decision: "deny",
				Reason:   "Tether Guard: Direct bash access to .tether/session.db, .tether/cache.db, or mutating .tether/ state is blocked. Use Tether MCP tools (session_load, sql_query, todo_list) instead.",
			}
		}
	}

	if isWriteTool(toolName) {
		ws := ""
		if h.engine != nil {
			ws = h.engine.Workspace
		}
		if isProtectedSubstratePath(ws, filePath) {
			return HookOutput{
				Decision: "deny",
				Reason:   "Tether Guard: Direct writes to .tether/ databases or snapshots are blocked. Use Tether MCP tools (session_load, sql_query, todo_list, undo) instead.",
			}
		}
	}

	// 2. Planning Mode Gating
	if h.engine.GetMode() == "plan" {
		if isWriteTool(toolName) {
			targetPath := filePath
			if targetPath != "" {
				resolvedPath, err := filepath.EvalSymlinks(targetPath)
				if err != nil {
					resolvedPath = targetPath
				}
				cleanPath := filepath.ToSlash(resolvedPath)
				// Path-aware plan sandboxing: allow writes to docs/ and .agents/
				if strings.Contains(cleanPath, "/docs/") || strings.HasPrefix(cleanPath, "docs/") ||
					strings.Contains(cleanPath, "/.agents/") || strings.HasPrefix(cleanPath, ".agents/") ||
					strings.HasSuffix(cleanPath, ".plan.md") {
					return HookOutput{Decision: "allow", Reason: "Plan artifact writing allowed in docs/ and .agents/"}
				}
			}

			return HookOutput{
				Decision: "deny",
				Reason:   "Tether Guard: Project is in PLAN mode. Mutation to production codebase is blocked. Allowed: write plans to docs/ or present inline. To unlock: run 'tether mode code' or call MCP mode_set(mode=\"code\").",
			}
		}

		// Shell mutative command risk tiering
		if toolName == "run_command" || toolName == "bash" {
			if okRO, _ := safety.IsReadOnlyCommand(command); !okRO {
				if okVal, _ := safety.IsValidationCommand(command); !okVal {
					sanitized := sanitizeRedirections(strings.ToLower(command))
					mutativeSignals := []string{">", ">>", "sed -i", "rm ", "mv ", "git checkout", "git reset", "git push"}
					for _, sig := range mutativeSignals {
						if strings.Contains(sanitized, sig) {
							return HookOutput{
								Decision: "deny",
								Reason:   fmt.Sprintf("Tether Guard: Mutative shell operation '%s' blocked in PLAN mode. Allowed: write plans to docs/. To unlock: run 'tether mode code' or call MCP mode_set(mode=\"code\").", sig),
							}
						}
					}
				}
			}
		}
	}

	// 3. Code Mode:
	// Snapshot file writes first so the turn can be undone without git.
	if isWriteTool(toolName) {
		h.captureBeforeWrite(filePath)
	}
	return HookOutput{Decision: "allow", Reason: "Tether: main agent authorized"}
}

func sanitizeRedirections(cmd string) string {
	for _, r := range []string{"2>&1", "2>/dev/null", "2> /dev/null", "1>/dev/null", "1> /dev/null", ">/dev/null", "> /dev/null"} {
		cmd = strings.ReplaceAll(cmd, r, "")
	}
	return cmd
}

// handlePostToolUse logs tool usage + tracks modified files to session
func (h *Handler) handlePostToolUse(input HookInput) HookOutput {
	toolName, _, filePath := input.Normalized()
	if toolName == "" {
		return HookOutput{}
	}

	agent := input.AgentType
	if agent == "" {
		agent = "unknown"
	}

	// Track file modifications for cross-agent context
	isWriteTool := toolName == "write_to_file" ||
		toolName == "replace_file_content" ||
		toolName == "write_file" ||
		toolName == "edit_file" ||
		toolName == "multi_replace_file_content"

	if isWriteTool && filePath != "" {
		h.engine.AddContext(agent, "file_modified", filePath)
	}

	// Log tool usage
	entry := fmt.Sprintf("tool:%s", toolName)
	if filePath != "" {
		entry += fmt.Sprintf(" file:%s", filePath)
	}
	h.engine.AddContext(agent, "tool_use", entry)

	return HookOutput{}
}

// writeToolNames are the tools that overwrite a single known file. sql_query,
// session_save and the todo tools mutate state but have no single file to
// snapshot, so they are deliberately excluded.
var writeToolNames = map[string]bool{
	"write_to_file":              true,
	"write_file":                 true,
	"edit_file":                  true,
	"replace_file_content":       true,
	"multi_replace_file_content": true,
}

// isWriteTool reports whether a tool writes to a single file path.
func isWriteTool(name string) bool {
	return writeToolNames[name]
}

// isProtectedSubstratePath reports whether targetPath refers (directly, via
// traversal, or via symlinks) to Tether's root or .tether/ SQLite databases or
// snapshot backups. Non-DB files like .tether/PLAN.md are deliberately excluded.
func isProtectedSubstratePath(workspace, targetPath string) bool {
	trimmed := strings.TrimSpace(targetPath)
	if trimmed == "" {
		return false
	}

	isSubstrateRel := func(rel string) bool {
		s := strings.ToLower(strings.TrimPrefix(filepath.ToSlash(filepath.Clean(rel)), "./"))
		return strings.HasPrefix(s, "session.db") ||
			strings.HasPrefix(s, "cache.db") ||
			strings.HasPrefix(s, ".tether/session.db") ||
			strings.HasPrefix(s, ".tether/cache.db") ||
			s == ".tether/snapshots" || strings.HasPrefix(s, ".tether/snapshots/")
	}

	if !filepath.IsAbs(trimmed) && isSubstrateRel(trimmed) {
		return true
	}
	if workspace == "" {
		return false
	}

	absRoot, err := filepath.Abs(workspace)
	if err != nil {
		return false
	}
	canonRoot := resolvePathWithAncestor(absRoot)

	var candidate string
	if filepath.IsAbs(trimmed) {
		candidate = filepath.Clean(trimmed)
	} else {
		candidate = filepath.Join(absRoot, trimmed)
	}
	if rel, err := filepath.Rel(absRoot, candidate); err == nil && isSubstrateRel(rel) {
		return true
	}

	canonTarget := resolvePathWithAncestor(candidate)
	if rel, err := filepath.Rel(canonRoot, canonTarget); err == nil && isSubstrateRel(rel) {
		return true
	}
	return false
}

func resolvePathWithAncestor(p string) string {
	clean := filepath.Clean(p)
	var suffix []string
	cur := clean
	symlinkHops := 0
	for {
		if info, err := os.Lstat(cur); err == nil {
			if resolved, rerr := filepath.EvalSymlinks(cur); rerr == nil {
				for i := len(suffix) - 1; i >= 0; i-- {
					resolved = filepath.Join(resolved, suffix[i])
				}
				return filepath.Clean(resolved)
			}
			if info.Mode()&os.ModeSymlink != 0 && symlinkHops < 16 {
				if linkTarget, lerr := os.Readlink(cur); lerr == nil {
					if !filepath.IsAbs(linkTarget) {
						linkTarget = filepath.Join(filepath.Dir(cur), linkTarget)
					}
					for i := len(suffix) - 1; i >= 0; i-- {
						linkTarget = filepath.Join(linkTarget, suffix[i])
					}
					cur = filepath.Clean(linkTarget)
					suffix = nil
					symlinkHops++
					continue
				}
			}
			return clean
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return clean
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
}
