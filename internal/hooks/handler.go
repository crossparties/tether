package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/mount"
	"github.com/crossparties/tether/internal/safety"
	"github.com/crossparties/tether/internal/snapshot"
)

// Handler processes lifecycle hook events from agents
type Handler struct {
	engine      *core.Engine
	mount       *mount.Engine
	snapshot    *snapshot.Store
	actorID     string
	readOnly    bool
	extReadOnly map[string]bool
}

func NewHandler(engine *core.Engine) *Handler {
	var m *mount.Engine
	ws := "."
	if engine != nil {
		ws = engine.Workspace
		m = engine.Mount()
	}
	return &Handler{
		engine:   engine,
		mount:    m,
		snapshot: snapshot.New(ws),
	}
}

// SetActorID configures the actor ("main" or Crew ID) for File Ownership Lease checks.
func (h *Handler) SetActorID(actorID string) {
	if h == nil {
		return
	}
	h.actorID = strings.TrimSpace(actorID)
	if h.mount != nil {
		h.mount.SetActorID(h.actorID)
	}
}

// SetReadOnly locks the hook handler into read-only review mode regardless of DB mode.
func (h *Handler) SetReadOnly(ro bool) {
	if h != nil {
		h.readOnly = ro
	}
}

// SetExternalReadOnlyTools installs the runner-verified set of read-only external
// MCP tool names so alias-named Serena servers and truncated names behave consistently.
func (h *Handler) SetExternalReadOnlyTools(tools map[string]bool) {
	if h == nil {
		return
	}
	if len(tools) == 0 {
		h.extReadOnly = nil
		return
	}
	cp := make(map[string]bool, len(tools))
	for k, v := range tools {
		if v {
			cp[k] = true
		}
	}
	h.extReadOnly = cp
}

func (h *Handler) mountEngine() *mount.Engine {
	var m *mount.Engine
	if h.mount != nil {
		m = h.mount
	} else if h.engine != nil {
		m = h.engine.Mount()
	} else {
		m = mount.NewEngine(".")
	}
	if h.actorID != "" {
		m.SetActorID(h.actorID)
	}
	return m
}

func (h *Handler) activeMode() string {
	if h != nil && h.readOnly {
		return "review"
	}
	if h.engine != nil {
		return h.engine.GetMode()
	}
	return "code"
}

func (h *Handler) shellAuthorizer() func(rawPath string, write bool) (bool, string) {
	m := h.mountEngine()
	mode := h.activeMode()
	return func(rawPath string, write bool) (bool, string) {
		op := mount.OpRead
		if write {
			op = mount.OpWrite
		}
		d := m.AuthorizePath(rawPath, op, mode)
		return d.Allowed, d.Reason
	}
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
	context += "- ALWAYS use Tether core tools (`kb_search`, `todo_manage`, `decision_add`, `mode_set`) or inspect auto-injected context above.\n"

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
		context += fmt.Sprintf("\n### Previous Decisions (newest %d)\n", len(decisions))
		for _, d := range decisions {
			context += fmt.Sprintf("- [%s] %s (reason: %s)\n",
				d.Topic, truncate(d.Decision, injectEntryMaxRunes), truncate(d.Reason, injectEntryMaxRunes))
		}
	}

	if len(patterns) > 0 {
		context += fmt.Sprintf("\n### Learned Patterns (newest %d)\n", len(patterns))
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
	ws := "."
	if h.engine != nil {
		ws = h.engine.Workspace
	}
	mode := h.activeMode()

	if h.readOnly {
		if strings.HasPrefix(toolName, "mcp_") && !h.isReadOnlyExternalHookTool(toolName) {
			return HookOutput{Decision: "deny", Reason: fmt.Sprintf("Tether Guard: read-only crew cannot invoke external MCP tool %s.", toolName)}
		}
		if isWriteTool(toolName) || toolName == "mode_set" || toolName == "decision_add" {
			return HookOutput{Decision: "deny", Reason: "Tether Guard: read-only crew cannot mutate files or governance state."}
		}
		if toolName == "todo_manage" && !strings.EqualFold(strings.TrimSpace(input.ArgText("action")), "list") {
			return HookOutput{Decision: "deny", Reason: "Tether Guard: read-only crew cannot mutate todos."}
		}
		if toolName == "run_command" || toolName == "bash" || toolName == "sh" || toolName == "shell" || toolName == "exec" {
			if ok, _ := safety.IsReadOnlyCommand(command); !ok {
				return HookOutput{Decision: "deny", Reason: "Tether Guard: read-only crew may only run strictly read-only commands."}
			}
		}
	}

	if strings.HasPrefix(toolName, "mcp_") {
		readOnlyExt := h.isReadOnlyExternalHookTool(toolName)
		op := mount.OpWrite
		if readOnlyExt {
			op = mount.OpRead
		}
		for _, p := range extractHookExternalPaths(input, filePath) {
			d := h.mountEngine().AuthorizePath(p, op, mode)
			if !d.Allowed && !(readOnlyExt && d.RelPath == ".") {
				return HookOutput{Decision: "deny", Reason: d.Reason}
			}
		}
		if mode == "plan" && !readOnlyExt {
			return HookOutput{
				Decision: "deny",
				Reason:   fmt.Sprintf("Tether Guard: external MCP tool %s is blocked in PLAN mode.", toolName),
			}
		}
	}

	// 1. Catastrophic command, Point-2 shell guard & protected substrate protection
	if toolName == "run_command" || toolName == "bash" || toolName == "sh" || toolName == "shell" || toolName == "exec" {
		if d := classifyCommand(command); d.level == deny {
			return HookOutput{Decision: "deny", Reason: d.describe()}
		}
		if ok, reason := safety.CheckShellCommandWithMount(command, ws, h.shellAuthorizer()); !ok {
			return HookOutput{Decision: "deny", Reason: reason}
		}
		cmdLower := strings.ToLower(command)
		isDirectDB := strings.Contains(cmdLower, "session.db") || strings.Contains(cmdLower, "cache.db") ||
			(strings.Contains(cmdLower, "sqlite3") && strings.Contains(cmdLower, ".db"))
		if isDirectDB {
			return HookOutput{
				Decision: "deny",
				Reason:   "Tether Guard: Direct bash access to .tether/session.db or .tether/cache.db is blocked. Use Tether core tools (kb_search, todo_manage, decision_add) instead.",
			}
		}
	}

	if isReadTool(toolName) && filePath != "" {
		if d := h.mountEngine().AuthorizePath(filePath, mount.OpRead, mode); !d.Allowed {
			return HookOutput{Decision: "deny", Reason: d.Reason}
		}
	}

	if isWriteTool(toolName) {
		if filePath == "" {
			if mode == "plan" {
				return HookOutput{
					Decision: "deny",
					Reason:   "Tether Guard: Project is in PLAN mode. Mutation to production codebase is blocked. Allowed: write plans to .tether/PLAN.md or docs/. To unlock: run 'tether mode code' or call MCP mode_set(mode=\"code\").",
				}
			}
		} else {
			d := h.mountEngine().AuthorizePath(filePath, mount.OpWrite, mode)
			if !d.Allowed {
				return HookOutput{Decision: "deny", Reason: d.Reason}
			}
			if d.IsDir {
				return HookOutput{Decision: "deny", Reason: fmt.Sprintf("access denied: %q is a directory", d.RelPath)}
			}
			if mode == "plan" {
				if mount.IsDependencyManifest(d.RelPath) || !mount.IsPlanArtifactRel(d.RelPath) {
					return HookOutput{
						Decision: "deny",
						Reason:   "Tether Guard: Project is in PLAN mode. Mutation to production codebase is blocked. Allowed: write plans to .tether/PLAN.md or docs/. To unlock: run 'tether mode code' or call MCP mode_set(mode=\"code\").",
					}
				}
				return HookOutput{Decision: "allow", Reason: "Plan artifact writing allowed in .tether/PLAN.md, docs/, and .agents/"}
			}
		}
	}

	// 2. Planning Mode shell guardrails
	if mode == "plan" {

		// Shell mutative command risk tiering
		if toolName == "run_command" || toolName == "bash" || toolName == "sh" || toolName == "shell" || toolName == "exec" {
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

// writeToolNames are the tools that overwrite a single known file. todo_manage,
// decision_add, and mode_set mutate substrate state via MCP tools rather than raw
// workspace files, so they are excluded here.
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

func isReadTool(name string) bool {
	switch name {
	case "read_file", "view_file", "read":
		return true
	}
	return false
}

func (h *Handler) isReadOnlyExternalHookTool(name string) bool {
	if !strings.HasPrefix(name, "mcp_") {
		return false
	}
	if h != nil && h.extReadOnly != nil && h.extReadOnly[name] {
		return true
	}
	srv, raw, ok := strings.Cut(strings.TrimPrefix(name, "mcp_"), "__")
	if !ok || !strings.Contains(strings.ToLower(srv), "serena") {
		return false
	}
	switch raw {
	case "find_symbol", "find_referencing_symbols", "find_implementations", "get_symbols_overview", "get_diagnostics_for_file":
		return true
	}
	return false
}

func extractHookExternalPaths(input HookInput, primary string) []string {
	var out []string
	if strings.TrimSpace(primary) != "" {
		out = append(out, strings.TrimSpace(primary))
	}
	if tc := input.toolCallValue(); tc != nil && tc.Args != nil {
		for _, k := range []string{"relative_path", "file_path", "file", "dir", "directory", "path", "target_file", "target_path", "paths"} {
			val, ok := tc.Args[k]
			if !ok {
				continue
			}
			switch v := val.(type) {
			case string:
				if trimmed := strings.TrimSpace(v); trimmed != "" {
					out = append(out, trimmed)
				}
			case []any:
				for _, item := range v {
					if s, ok := item.(string); ok {
						if trimmed := strings.TrimSpace(s); trimmed != "" {
							out = append(out, trimmed)
						}
					}
				}
			case []string:
				for _, s := range v {
					if trimmed := strings.TrimSpace(s); trimmed != "" {
						out = append(out, trimmed)
					}
				}
			}
		}
	}
	return out
}
