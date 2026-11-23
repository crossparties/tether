package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	tetherctx "github.com/crossparties/tether/internal/context"
	"github.com/crossparties/tether/internal/mcp"
	"github.com/crossparties/tether/internal/safety"
	"github.com/crossparties/tether/internal/ui"
)

func (r *Runner) sideChatCtx() context.Context {
	return context.Background()
}

// ProviderName returns the active LLM provider identifier.
func (r *Runner) ProviderName() string {
	return r.cfg.Provider
}

// ModelName returns the active LLM model identifier.
func (r *Runner) ModelName() string {
	return r.cfg.Model
}

// ModeName returns the active substrate governance mode ("code" or "plan").
func (r *Runner) ModeName() string {
	if r.engine == nil {
		return "code"
	}
	return r.engine.GetMode()
}

// NeedsAPIKey reports whether the active provider requires an API key that is not yet configured.
func (r *Runner) NeedsAPIKey() bool {
	return r.cfg.Provider != "ollama" && strings.TrimSpace(r.cfg.APIKey) == ""
}

// SaveProviderKey stores and persists the API key for the active provider.
func (r *Runner) SaveProviderKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("API key cannot be empty")
	}
	r.cfg.APIKey = key
	return SaveAPIKey(r.cfg.Provider, key)
}

// ListModels returns the curated model choices for the active provider.
func (r *Runner) ListModels() []ui.Choice {
	return modelsFor(r.cfg.Provider)
}

// SwitchModel validates and updates the active model for the current provider.
func (r *Runner) SwitchModel(name string) (string, error) {
	canonical, valid := validateModel(r.cfg.Provider, name)
	if !valid {
		choices := modelsFor(r.cfg.Provider)
		return "", fmt.Errorf("unknown model %q for %s (options: %s)", name, r.cfg.Provider, choiceLabels(choices))
	}
	r.cfg.Model = canonical
	if r.engine != nil {
		_ = r.engine.SetProjectSetting("model", r.cfg.Model)
	}
	return r.cfg.Model, nil
}

// ListProviders returns the available LLM providers in stable order.
func (r *Runner) ListProviders() []ui.Choice {
	return providerChoices()
}

// SwitchProvider switches the active LLM provider and loads its default model & saved API key.
func (r *Runner) SwitchProvider(name string) (string, string, error) {
	p := strings.ToLower(strings.TrimSpace(name))
	preset, known := presets[p]
	if !known {
		return "", "", fmt.Errorf("unknown provider %q (options: %s)", p, strings.Join(providerNames(), ", "))
	}
	r.cfg.Provider = p
	r.cfg.BaseURL = preset.BaseURL
	r.cfg.Model = preset.DefaultModel
	r.cfg.APIKey = loadSavedKey(p)
	if r.engine != nil {
		_ = r.engine.SetProjectSetting("provider", p)
		_ = r.engine.SetProjectSetting("model", r.cfg.Model)
	}
	return r.cfg.Provider, r.cfg.Model, nil
}

// SwitchMode updates the substrate mode ("code" or "plan").
func (r *Runner) SwitchMode(mode string) error {
	if r.engine == nil {
		return nil
	}
	return r.engine.SetMode(mode)
}

// ClearHistory clears the in-memory conversation buffer while retaining .tether/session.db memory.
func (r *Runner) ClearHistory() {
	r.history = nil
}

// UndoTurn restores files from the given snapshot turn (or latest when empty) and returns the output text.
func (r *Runner) UndoTurn(turn string) string {
	var buf bytes.Buffer
	prev := r.cfg.Out
	r.cfg.Out = &buf
	r.undo(turn)
	r.cfg.Out = prev
	return strings.TrimSpace(buf.String())
}

// StatusSummary returns a single-line summary of workspace, session, mode, KB, and provider.
func (r *Runner) StatusSummary() string {
	if r.engine == nil {
		return fmt.Sprintf("Provider: %s (%s)", r.cfg.Provider, r.cfg.Model)
	}
	kbStats, _ := r.engine.KBStatus()
	cacheInfo := ""
	if r.lastCacheHitTokens > 0 || r.lastCacheMissTokens > 0 {
		total := r.lastCacheHitTokens + r.lastCacheMissTokens
		pct := 0
		if total > 0 {
			pct = (r.lastCacheHitTokens * 100) / total
		}
		cacheInfo = fmt.Sprintf(" | Cache Hit: %d/%d (%d%%)", r.lastCacheHitTokens, total, pct)
	}
	return fmt.Sprintf("Workspace: %s | Session: %s | Mode: %s | KB: %d files | Provider: %s (%s)%s",
		r.engine.Workspace, r.engine.SessionID, r.engine.GetMode(), kbStats.TotalFiles, r.cfg.Provider, r.cfg.Model, cacheInfo)
}

// SetAskPolicy installs a custom interactive approval handler on the underlying MCP server.
func (r *Runner) SetAskPolicy(fn func(mcp.Decision) (mcp.Verdict, bool)) {
	if r.mcp != nil {
		r.mcp.SetAskPolicy(fn)
	}
}

// SetAutoApprovePolicy installs a non-critical single-step auto-approval predicate on the underlying MCP server.
func (r *Runner) SetAutoApprovePolicy(fn func(mcp.Decision) bool) {
	if r.mcp != nil {
		r.mcp.SetAutoApprovePolicy(fn)
	}
}

// DefaultAutoApprovePolicy returns Tether's standard interactive auto-approval
// rule: non-critical operations (read-only & validation shell commands, routine
// workspace edits in CODE mode protected by /undo snapshots, and plan artifacts
// in PLAN mode) execute in a single step without interrupting the user.
func (r *Runner) DefaultAutoApprovePolicy() func(mcp.Decision) bool {
	return func(d mcp.Decision) bool {
		mode := "code"
		workspace := "."
		if r.engine != nil {
			mode = r.engine.GetMode()
			if r.engine.Workspace != "" {
				workspace = r.engine.Workspace
			}
		}
		target := d.Path
		if target == "" {
			target = d.Command
		}
		if target == "" {
			target = d.Label
		}
		ok, _ := safety.IsNonCriticalTool(mode, d.Tool, target, d.Command, workspace)
		return ok
	}
}

// RunWithOutput executes a prompt while streaming intermediate tool logs to out.
// It also expands any `@rel/path` file mentions inside the prompt using SafeReadWorkspaceFile.
func (r *Runner) RunWithOutput(ctx context.Context, prompt string, out io.Writer) (string, error) {
	prev := r.cfg.Out
	if out != nil {
		r.cfg.Out = out
	}
	defer func() {
		r.cfg.Out = prev
	}()
	expanded := r.expandAtMentions(prompt)
	return r.Run(ctx, expanded)
}

func (r *Runner) expandAtMentions(prompt string) string {
	if !strings.Contains(prompt, "@") || r.engine == nil || r.engine.Workspace == "" {
		return prompt
	}
	words := strings.Fields(prompt)
	var attachments []string
	seen := map[string]bool{}
	for _, w := range words {
		if !strings.HasPrefix(w, "@") || len(w) <= 1 {
			continue
		}
		rel := strings.TrimRight(strings.TrimPrefix(w, "@"), ",.;:!?)]}\"'")
		if rel == "" || seen[rel] {
			continue
		}
		if content, cleanRel, err := tetherctx.SafeReadWorkspaceFile(r.engine.Workspace, rel, 32*1024); err == nil {
			seen[rel] = true
			attachments = append(attachments, fmt.Sprintf("<file path=%q>\n%s\n</file>", cleanRel, content))
		}
	}
	if len(attachments) == 0 {
		return prompt
	}
	return prompt + "\n\n[Attached Workspace Files via @mention]:\n" + strings.Join(attachments, "\n\n")
}

// RunSideChat executes an isolated `/btw <question>` turn (PRD §3.5).
// It guarantees zero context pollution:
//  1. It never reads or mutates r.history.
//  2. It never records entries in .tether/session.db via r.engine.AddContext.
//  3. It strips all mutative tools (only stateless Q&A is performed).
func (r *Runner) RunSideChat(ctx context.Context, question string) (string, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return "", fmt.Errorf("question is required for /btw")
	}
	if r.cfg.Provider != "ollama" && r.cfg.APIKey == "" {
		return "", fmt.Errorf("missing API key for provider %q", r.cfg.Provider)
	}

	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	messages := []chatMessage{
		{
			Role:    "system",
			Content: "You are answering an isolated /btw side-chat question inside Tether. Be concise and technical. This conversation is ephemeral and does not modify the active workspace task.",
		},
		{
			Role:    "user",
			Content: question,
		},
	}

	msg, err := r.callAPI(callCtx, messages, nil)
	if err != nil {
		return "", err
	}
	return msg.Content, nil
}

// ReadWorkspaceFile reads a file relative to the runner's workspace while
// enforcing path-traversal protection (`..` and symlink escape blocking).
func (r *Runner) ReadWorkspaceFile(relPath string) (string, error) {
	relPath = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(relPath), "@"))
	if relPath == "" {
		return "", fmt.Errorf("empty file path")
	}
	workspace := "."
	if r.engine != nil && r.engine.Workspace != "" {
		workspace = r.engine.Workspace
	}
	content, _, err := tetherctx.SafeReadWorkspaceFile(workspace, relPath, 64*1024)
	return content, err
}

// ExecuteGatedCommand classifies a shell command via the Two-Tiered Safety Policy
// (PRD §3.6) and executes it through the guarded MCP tool pipeline.
func (r *Runner) ExecuteGatedCommand(ctx context.Context, cmdStr string) (string, error) {
	_ = ctx
	cmdStr = strings.TrimSpace(cmdStr)
	if cmdStr == "" {
		return "", fmt.Errorf("empty command")
	}

	_, reason := safety.ClassifyRisk("run_command", cmdStr)
	_ = reason

	rawArgs, _ := json.Marshal(map[string]interface{}{
		"command": cmdStr,
	})
	res := r.executeGuardedTool(toolCall{
		ID:   "exec-cli",
		Type: "function",
		Function: functionCall{
			Name:      "run_command",
			Arguments: string(rawArgs),
		},
	})
	if strings.HasPrefix(res, "DENIED BY TETHER GUARD:") {
		return "", fmt.Errorf("%s", res)
	}
	return res, nil
}
