package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/croosparties/tether/internal/core"
)

// Handler processes lifecycle hook events from agents
type Handler struct {
	engine *core.Engine
}

func NewHandler(engine *core.Engine) *Handler {
	return &Handler{engine: engine}
}

// ToolCall represents the Antigravity/Gemini toolCall object
type ToolCall struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args"`
}

// HookInput is the JSON payload agents send to hooks via stdin
type HookInput struct {
	ToolCall       *ToolCall `json:"toolCall,omitempty"`
	ToolName       string    `json:"tool_name,omitempty"`
	Command        string    `json:"command,omitempty"`
	FilePath       string    `json:"file_path,omitempty"`
	Content        string    `json:"content,omitempty"`
	AgentType      string    `json:"agent_type,omitempty"`
	Prompt         string    `json:"prompt,omitempty"`
	ConversationID string    `json:"conversationId,omitempty"`
	IsSubagent     bool      `json:"is_subagent,omitempty"`
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

func (h *HookInput) Normalized() (toolName, command, filePath string) {
	toolName = h.ToolName
	command = h.Command
	filePath = h.FilePath

	if h.ToolCall != nil {
		if h.ToolCall.Name != "" {
			toolName = h.ToolCall.Name
		}
		if h.ToolCall.Args != nil {
			if cmd, ok := h.ToolCall.Args["CommandLine"].(string); ok && cmd != "" {
				command = cmd
			} else if cmd, ok := h.ToolCall.Args["command"].(string); ok && cmd != "" {
				command = cmd
			}

			for _, key := range []string{"TargetFile", "AbsolutePath", "FilePath", "path", "target_file"} {
				if fp, ok := h.ToolCall.Args[key].(string); ok && fp != "" {
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
	if input.ConversationID != "" {
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

	recent, _ := h.engine.GetRecentContext(5)
	if len(recent) > 0 {
		context += "\n### Recent Session Context\n"
		for _, entry := range recent {
			context += fmt.Sprintf("- [%s/%s] %s\n", entry.Agent, entry.Role, entry.Content)
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

	// 2. Subagent Gating: Subagents must NOT mutate codebase or run mutative commands directly.
	// They must propose changes to the Main Agent so the user can approve in the main session.
	if isSubagent {
		writeTools := map[string]bool{
			"write_to_file":              true,
			"write_file":                 true,
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
			cmdLower := strings.ToLower(command)
			sanitized := strings.ReplaceAll(cmdLower, "2>&1", "")
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
			cmd := strings.ToLower(command)
			sanitized := strings.ReplaceAll(cmd, "2>&1", "")
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
	// MCP calls (access to session.db) and dev tools are auto-approved for frictionless development
	return HookOutput{Decision: "allow", Reason: "Tether: main agent authorized"}
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

