package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/croosparties/tether/internal/core"
	"github.com/croosparties/tether/internal/snapshot"
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
	IsSubagent     bool      `json:"is_subagent,omitempty"`

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

func (h *HookInput) CheckIsSubagent(mainConversationID string) bool {
	if h.IsSubagent {
		return true
	}
	if h.AgentType != "" && h.AgentType != "main" && h.AgentType != "unknown" {
		return true
	}
	if mainConversationID != "" && h.ConversationID != "" && h.ConversationID != mainConversationID {
		return true
	}
	return false
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

// handlePreInvocation injects workspace context + task routing hint before model runs
func (h *Handler) handlePreInvocation(input HookInput) HookOutput {
	// A subagent must never claim the main conversation identity. Without this
	// guard every subagent run overwrote main_conversation_id, after which the
	// real Main Agent was itself classified as a subagent and lost its
	// authorization path (see handlePreToolUse).
	if input.ConversationID != "" && !input.IsSubagent {
		_ = h.engine.SetProjectSetting("main_conversation_id", input.ConversationID)
	}

	settings, _ := h.engine.GetAllProjectSettings()
	decisions, _ := h.engine.GetDecisions()
	patterns, _ := h.engine.GetPatterns()
	summary, _ := h.engine.GetSessionSummary()

	context := "## Tether Context (auto-injected)\n"
	context += fmt.Sprintf("- Workspace: %s\n", h.engine.Workspace)
	context += fmt.Sprintf("- Session: %s\n", h.engine.SessionID)
	context += fmt.Sprintf("- Mode: %s (writes %s)\n", h.engine.GetMode(),
		map[bool]string{true: "allowed", false: "BLOCKED"}[h.engine.IsWriteAllowed()])

	if summary != "" {
		context += fmt.Sprintf("- Session summary: %s\n", summary)
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
			text := entry.Content
			if runes := []rune(text); len(runes) > 300 {
				text = string(runes[:300]) + "..."
			}
			context += fmt.Sprintf("- [%s/%s] %s\n", entry.Agent, entry.Role, text)
		}
	}

	if len(settings) > 0 {
		context += "\n### Project Settings\n"
		for k, v := range settings {
			context += fmt.Sprintf("- %s: %s\n", k, v)
		}
	}

	if len(decisions) > 0 {
		context += "\n### Previous Decisions\n"
		for _, d := range decisions {
			context += fmt.Sprintf("- [%s] %s (reason: %s)\n", d.Topic, d.Decision, d.Reason)
		}
	}

	if len(patterns) > 0 {
		context += "\n### Learned Patterns\n"
		for _, p := range patterns {
			context += fmt.Sprintf("- [%s] %s\n", p.Type, p.Description)
		}
	}

	todos, _ := h.engine.ListTodos(false)
	if len(todos) > 0 {
		context += "\n### Project Tasks / Todos (Pending)\n"
		for _, t := range todos {
			context += fmt.Sprintf("- [#%d] %s\n", t.ID, t.Description)
		}
	}

	context += "\n### ⚠️ TETHER SUBSTRATE MEMORY PROTOCOL\n"
	context += "- NEVER use python3/bash scripts to query or edit session.db.\n"
	context += "- ALWAYS use Tether MCP tools (`sql_query`, `session_load`, `session_save`, `todo_*`) or inspect auto-injected context above.\n"


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

	// Subagent Governance Protocol: Specialist workers propose changes, Main Agent requests user approval
	context += "\n### ⚠️ SUBAGENT GOVERNANCE & APPROVAL PROTOCOL (MANDATORY)\n"
	context += "- Subagents are SPECIALIST WORKERS: analyze, research, and compose proposed changes.\n"
	context += "- Subagents MUST NOT mutate the codebase or run destructive commands directly.\n"
	context += "- Subagents MUST return their proposed diff/commands in a message to the Main Agent.\n"
	context += "- The Main Agent presents the proposal to the user in the main chat for approval.\n"
	context += "- Only after user approval in the main session are the mutations executed.\n"

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

	// 1. Catastrophic command protection (always active across all agents and modes)
	if toolName == "run_command" || toolName == "bash" {
		cmdLower := strings.ToLower(command)
		catastrophic := []string{"rm -rf /", "rm -rf ~", "rm -rf /*", "mkfs"}
		for _, cat := range catastrophic {
			if strings.Contains(cmdLower, cat) {
				return HookOutput{
					Decision: "deny",
					Reason:   fmt.Sprintf("Tether Guard: Catastrophic shell command '%s' is strictly blocked.", cat),
				}
			}
		}

		// Prevent raw bash sqlite3/session.db hacks that trigger CLI permission prompts
		if strings.Contains(cmdLower, "session.db") || (strings.Contains(cmdLower, "sqlite3") && strings.Contains(cmdLower, ".db")) {
			return HookOutput{
				Decision: "deny",
				Reason:   "Tether Guard: Direct bash access to session.db via sqlite3/python is blocked. Use Tether MCP tools (session_load, sql_query, todo_list) which run in-process without permission dialogs.",
			}
		}
	}

	mainConvID, _ := h.engine.GetProjectSetting("main_conversation_id")
	isSubagent := input.CheckIsSubagent(mainConvID)

	// 2. Proposal Gate: while a subagent proposal awaits the user's approval,
	// the Main Agent itself must not mutate the codebase. The protocol used to
	// live only in the system prompt, so nothing actually stopped a write. This
	// gate makes it enforceable: approve_proposal (after the user says yes) or
	// reject_proposal (after the user says no) is required first.
	if !isSubagent {
		writeTools := map[string]bool{
			"write_to_file":              true,
			"write_file":                 true,
			"edit_file":                  true,
			"replace_file_content":       true,
			"multi_replace_file_content": true,
		}

		// State-mutating tools that are not file writes but still change the
		// substrate. Leaving them open would let a gated agent write "sideways":
		// recording a decision or doctoring todos instead of waiting.
		stateTools := map[string]bool{
			"session_save":    true,
			"setting_set":     true,
			"decision_add":    true,
			"pattern_add":     true,
			"todo_add":        true,
			"todo_complete":   true,
			"todo_remove":     true,
			"approve_proposal": true,
			"reject_proposal":  true,
		}

		blocked := writeTools[toolName] || stateTools[toolName]

		// sql_query can mutate just as effectively as a file write.
		if !blocked && toolName == "sql_query" {
			q := strings.ToLower(strings.TrimSpace(input.ArgText("query")))
			for _, verb := range []string{"insert", "update", "delete", "drop", "alter", "create", "replace", "truncate"} {
				if strings.HasPrefix(q, verb) || strings.Contains(q, " "+verb+" ") {
					blocked = true
					break
				}
			}
		}

		if !blocked && (toolName == "run_command" || toolName == "bash") {
			sanitized := sanitizeRedirections(strings.ToLower(command))
			for _, sig := range []string{
				">", ">>", "sed -i", "rm ", "mv ", "cp ", "tee ", "chmod ", "chown ",
				"mkdir ", "touch ", "truncate ", "dd ", "git checkout", "git reset",
				"git push", "git apply", "go generate", "make ", "npm install", "pip install",
			} {
				if strings.Contains(sanitized, sig) {
					blocked = true
					break
				}
			}
		}

		if blocked {
			pending, err := h.engine.GetPendingProposal()
			if err == nil && pending != nil {
				preview := pending.Proposal
				if len(preview) > 400 {
					preview = preview[:400] + "... [truncated]"
				}
				return HookOutput{
					Decision: "deny",
					Reason: fmt.Sprintf(
						"Tether Guard: a subagent proposal is still awaiting user approval (proposal #%d from %s). "+
							"Present it to the user in the main chat, then call approve_proposal once they confirm "+
							"(or reject_proposal if they decline). Mutations stay blocked until then.\n---\n%s",
						pending.ID, pending.Role, preview),
				}
			}
		}
	}

	// 3. Subagent Gating: Subagents must NOT mutate codebase or run mutative commands directly.
	// They must propose changes to the Main Agent so the user can approve in the main session.
	if isSubagent {
		writeTools := map[string]bool{
			"write_to_file":              true,
			"write_file":                 true,
			"edit_file":                  true,
			"replace_file_content":       true,
			"multi_replace_file_content": true,
		}

		if writeTools[toolName] {
			return HookOutput{
				Decision: "deny",
				Reason:   "Tether Guard: Subagents cannot mutate codebase directly without user approval. Send your proposed diff/patch to the Main Agent. The Main Agent will present it to the user for approval in the main session before execution.",
			}
		}

		if toolName == "run_command" || toolName == "bash" {
			sanitized := sanitizeRedirections(strings.ToLower(command))
			mutativeSignals := []string{">", ">>", "sed -i", "rm ", "mv ", "git checkout", "git reset", "git push", "mkdir "}
			for _, sig := range mutativeSignals {
				if strings.Contains(sanitized, sig) {
					return HookOutput{
						Decision: "deny",
						Reason:   fmt.Sprintf("Tether Guard: Subagent shell mutation '%s' requires user approval. Send your proposed command to the Main Agent for approval in the main session.", sig),
					}
				}
			}
		}

		// Read tools and MCP tools are allowed for subagents
		return HookOutput{Decision: "allow", Reason: "Subagent read/query action allowed"}
	}

	// 3. Main Agent: Planning Mode Gating
	if h.engine.GetMode() == "plan" {
		writeTools := map[string]bool{
			"write_to_file":              true,
			"write_file":                 true,
			"edit_file":                  true,
			"replace_file_content":       true,
			"multi_replace_file_content": true,
		}

		if writeTools[toolName] {
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

	// 4. Main Agent in Code Mode:
	// MCP calls (access to session.db) and dev tools are auto-approved for frictionless development.
	// Snapshot file writes first so the turn can be undone without git.
	if isWriteTool(toolName) {
		h.captureBeforeWrite(filePath)
	}
	return HookOutput{Decision: "allow", Reason: "Tether: main agent authorized"}
}

func sanitizeRedirections(cmd string) string {
	for _, r := range []string{"2>&1", ">/dev/null", "> /dev/null"} {
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
