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

// HookInput is the JSON payload agents send to hooks via stdin
type HookInput struct {
	ToolName  string `json:"tool_name,omitempty"`
	Command   string `json:"command,omitempty"`
	FilePath  string `json:"file_path,omitempty"`
	Content   string `json:"content,omitempty"`
	AgentType string `json:"agent_type,omitempty"`
	Prompt    string `json:"prompt,omitempty"`
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

	// Always inject subagent write rules to prevent stuck waiting_for_input
	context += "\n### ⚠️ SUBAGENT FILE WRITE RULES (MANDATORY)\n"
	context += "- NEVER use `write_to_file` for project files — it only writes to artifact dir.\n"
	context += "- NEVER use `TypeName: \"self\"` for write tasks — no write-safe system prompt.\n"
	context += "- For NEW files: use `run_command` with heredoc, WaitMsBeforeAsync >= 5000:\n"
	context += "  `cat > /path/to/file << 'EOF'\\n...content...\\nEOF`\n"
	context += "- For MODIFYING files: use `replace_file_content`.\n"
	context += "- For directories: `run_command` with `mkdir -p /path`, WaitMsBeforeAsync >= 3000.\n"
	context += "- All `run_command` must be NON-INTERACTIVE. Set WaitMsBeforeAsync >= 5000.\n"
	context += "- After all changes: `go build ./... 2>&1` to verify before reporting done.\n"

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

// handlePreToolUse gates write operations during planning mode
func (h *Handler) handlePreToolUse(input HookInput) HookOutput {
	if h.engine.GetMode() == "plan" {
		writeTools := map[string]bool{
			"write_to_file":              true,
			"write_file":                 true,
			"replace_file_content":       true,
			"multi_replace_file_content": true,
		}

		if writeTools[input.ToolName] {
			targetPath := input.FilePath
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
		if input.ToolName == "run_command" || input.ToolName == "bash" {
			cmd := strings.ToLower(input.Command)
			// Ignore standard fd redirections like 2>&1
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

	return HookOutput{Decision: "allow", Reason: "ok"}
}

// handlePostToolUse logs tool usage + tracks modified files to session
func (h *Handler) handlePostToolUse(input HookInput) HookOutput {
	if input.ToolName == "" {
		return HookOutput{}
	}

	agent := input.AgentType
	if agent == "" {
		agent = "unknown"
	}

	// Track file modifications for cross-agent context
	isWriteTool := input.ToolName == "write_to_file" ||
		input.ToolName == "replace_file_content" ||
		input.ToolName == "write_file" ||
		input.ToolName == "multi_replace_file_content"

	if isWriteTool && input.FilePath != "" {
		h.engine.AddContext(agent, "file_modified", input.FilePath)
	}

	// Log tool usage
	entry := fmt.Sprintf("tool:%s", input.ToolName)
	if input.FilePath != "" {
		entry += fmt.Sprintf(" file:%s", input.FilePath)
	}
	h.engine.AddContext(agent, "tool_use", entry)

	return HookOutput{}
}
