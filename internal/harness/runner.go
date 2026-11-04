package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/croosparties/tether/internal/core"
	"github.com/croosparties/tether/internal/hooks"
	"github.com/croosparties/tether/internal/mcp"
)

// Config holds Universal API Harness settings
type Config struct {
	Provider string
	BaseURL  string
	APIKey   string
	Model    string
	MaxSteps int
	Out      io.Writer
}

type providerPreset struct {
	BaseURL      string
	DefaultModel string
	EnvKey       string
}

var presets = map[string]providerPreset{
	"deepseek":   {BaseURL: "https://api.deepseek.com/chat/completions", DefaultModel: "deepseek-chat", EnvKey: "DEEPSEEK_API_KEY"},
	"openrouter": {BaseURL: "https://openrouter.ai/api/v1/chat/completions", DefaultModel: "deepseek/deepseek-chat", EnvKey: "OPENROUTER_API_KEY"},
	"openai":     {BaseURL: "https://api.openai.com/v1/chat/completions", DefaultModel: "gpt-4o", EnvKey: "OPENAI_API_KEY"},
	"groq":       {BaseURL: "https://api.groq.com/openai/v1/chat/completions", DefaultModel: "llama-3.3-70b-versatile", EnvKey: "GROQ_API_KEY"},
	"gemini":     {BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", DefaultModel: "gemini-2.5-pro", EnvKey: "GEMINI_API_KEY"},
	"xai":        {BaseURL: "https://api.x.ai/v1/chat/completions", DefaultModel: "grok-2-latest", EnvKey: "XAI_API_KEY"},
	"ollama":     {BaseURL: "http://localhost:11434/v1/chat/completions", DefaultModel: "qwen2.5-coder:7b", EnvKey: ""},
}

// ResolveConfig merges CLI flags, session.db settings, and environment variables
func ResolveConfig(engine *core.Engine, cfg Config) Config {
	if cfg.Provider == "" {
		cfg.Provider, _ = engine.GetProjectSetting("provider")
	}
	if cfg.Provider == "" {
		cfg.Provider = os.Getenv("TETHER_PROVIDER")
	}
	if cfg.Provider == "" {
		// Auto-detect from available API key environment variables
		for _, p := range []string{"deepseek", "openrouter", "openai", "groq", "gemini", "xai"} {
			if os.Getenv(presets[p].EnvKey) != "" {
				cfg.Provider = p
				break
			}
		}
	}
	if cfg.Provider == "" {
		cfg.Provider = "deepseek"
	}

	preset, ok := presets[strings.ToLower(cfg.Provider)]
	if cfg.BaseURL == "" {
		cfg.BaseURL, _ = engine.GetProjectSetting("base_url")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = os.Getenv("TETHER_BASE_URL")
	}
	if cfg.BaseURL == "" && ok {
		cfg.BaseURL = preset.BaseURL
	}

	if cfg.Model == "" {
		cfg.Model, _ = engine.GetProjectSetting("model")
	}
	if cfg.Model == "" {
		cfg.Model = os.Getenv("TETHER_MODEL")
	}
	if cfg.Model == "" && ok {
		cfg.Model = preset.DefaultModel
	}

	if cfg.APIKey == "" && ok && preset.EnvKey != "" {
		cfg.APIKey = os.Getenv(preset.EnvKey)
	}
	if cfg.APIKey == "" {
		cfg.APIKey = os.Getenv("TETHER_API_KEY")
	}
	if cfg.APIKey == "" {
		cfg.APIKey, _ = engine.GetProjectSetting("api_key")
	}
	if cfg.APIKey == "" {
		cfg.APIKey = loadSavedKey(cfg.Provider)
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 15
	}
	if cfg.Out == nil {
		cfg.Out = os.Stdout
	}
	return cfg
}

func credentialsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "tether", "credentials.json")
}

func loadSavedKey(provider string) string {
	data, err := os.ReadFile(credentialsPath())
	if err != nil {
		return ""
	}
	var creds map[string]string
	if err := json.Unmarshal(data, &creds); err != nil {
		return ""
	}
	return creds[strings.ToLower(provider)]
}

// SaveAPIKey persists the provider's API key to ~/.config/tether/credentials.json (0600)
func SaveAPIKey(provider, key string) error {
	path := credentialsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	creds := make(map[string]string)
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &creds)
	}
	creds[strings.ToLower(provider)] = strings.TrimSpace(key)
	out, _ := json.MarshalIndent(creds, "", "  ")
	return os.WriteFile(path, out, 0600)
}

// Runner executes the Tether-anchored autonomous agent loop for any LLM API
type Runner struct {
	engine  *core.Engine
	hooks   *hooks.Handler
	mcp     *mcp.Server
	cfg     Config
	client  *http.Client
}

func NewRunner(engine *core.Engine, cfg Config) *Runner {
	return &Runner{
		engine: engine,
		hooks:  hooks.NewHandler(engine),
		mcp:    mcp.NewServer(engine),
		cfg:    ResolveConfig(engine, cfg),
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

type chatMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  mcp.InputSchema `json:"parameters"`
}

// Run executes a prompt through PreInvocation -> LLM Tool Loop (PreToolUse -> CallTool -> PostToolUse)
func (r *Runner) Run(ctx context.Context, prompt string) (string, error) {
	if r.cfg.Provider != "ollama" && r.cfg.APIKey == "" {
		return "", fmt.Errorf("missing API key for provider %q (set %s or TETHER_API_KEY)", r.cfg.Provider, presets[r.cfg.Provider].EnvKey)
	}

	// 1. PreInvocation Hook: surgical context injection & task mode classification
	preInvIn, _ := json.Marshal(hooks.HookInput{Prompt: prompt, AgentType: "main"})
	preInvRaw, _ := r.hooks.Handle("PreInvocation", bytes.NewReader(preInvIn))
	var preInvOut hooks.HookOutput
	_ = json.Unmarshal([]byte(preInvRaw), &preInvOut)

	sysPrompt := "You are an autonomous coding agent anchored by Tether. Follow workspace guardrails strictly."
	if len(preInvOut.InjectSteps) > 0 && preInvOut.InjectSteps[0].EphemeralMessage != "" {
		sysPrompt += "\n\n" + preInvOut.InjectSteps[0].EphemeralMessage
	}

	_ = r.engine.AddContext(r.cfg.Provider, "user", prompt)

	messages := []chatMessage{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: prompt},
	}

	var apiTools []openAITool
	for _, t := range r.mcp.ListTools() {
		apiTools = append(apiTools, openAITool{
			Type: "function",
			Function: openAIFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	// 2. Agentic Tool-Execution Loop
	for step := 0; step < r.cfg.MaxSteps; step++ {
		messages = compactMessages(messages)
		msg, err := r.callAPI(ctx, messages, apiTools)
		if err != nil {
			return "", err
		}

		if msg.ReasoningContent != "" {
			fmt.Fprintf(r.cfg.Out, "💭 [reasoning] %s\n", strings.TrimSpace(msg.ReasoningContent))
		}
		messages = append(messages, msg)

		if len(msg.ToolCalls) == 0 {
			_ = r.engine.AddContext(r.cfg.Provider, "assistant", msg.Content)
			return msg.Content, nil
		}

		for _, tc := range msg.ToolCalls {
			resultText := r.executeGuardedTool(tc)
			messages = append(messages, chatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    resultText,
			})
		}
	}

	return "", fmt.Errorf("exceeded max agent steps (%d)", r.cfg.MaxSteps)
}

func (r *Runner) executeGuardedTool(tc toolCall) string {
	var argMap map[string]interface{}
	_ = json.Unmarshal([]byte(tc.Function.Arguments), &argMap)

	hookIn := hooks.HookInput{
		ToolName:  tc.Function.Name,
		AgentType: "main",
		ToolCall:  &hooks.ToolCall{Name: tc.Function.Name, Args: argMap},
	}
	if cmd, ok := argMap["command"].(string); ok {
		hookIn.Command = cmd
	}
	if fp, ok := argMap["path"].(string); ok {
		hookIn.FilePath = fp
	}

	// PreToolUse Gatekeeper (Hard runtime enforcement)
	preRaw, _ := r.hooks.Handle("PreToolUse", mustJSONReader(hookIn))
	var preOut hooks.HookOutput
	_ = json.Unmarshal([]byte(preRaw), &preOut)
	if preOut.Decision == "deny" {
		fmt.Fprintf(r.cfg.Out, "🛑 [Tether Guard Blocked %s] %s\n", tc.Function.Name, preOut.Reason)
		return "DENIED BY TETHER GUARD: " + preOut.Reason
	}

	fmt.Fprintf(r.cfg.Out, "🔧 [tool:%s]\n", tc.Function.Name)
	res, _ := r.mcp.CallTool(tc.Function.Name, json.RawMessage(tc.Function.Arguments))

	// PostToolUse Auditor
	_, _ = r.hooks.Handle("PostToolUse", mustJSONReader(hookIn))
	return res
}

func (r *Runner) callAPI(ctx context.Context, messages []chatMessage, tools []openAITool) (chatMessage, error) {
	reqBody, _ := json.Marshal(map[string]interface{}{
		"model":    r.cfg.Model,
		"messages": messages,
		"tools":    tools,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.BaseURL, bytes.NewReader(reqBody))
	if err != nil {
		return chatMessage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.APIKey)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return chatMessage{}, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return chatMessage{}, fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		Choices []struct {
			Message chatMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Choices) == 0 {
		return chatMessage{}, fmt.Errorf("invalid API response: %s", string(body))
	}
	return parsed.Choices[0].Message, nil
}

func mustJSONReader(v interface{}) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

// RunInteractive starts an Aider/Claude-Code style interactive REPL session
func (r *Runner) RunInteractive(ctx context.Context, in io.Reader) error {
	scanner := bufio.NewScanner(in)

	fmt.Fprintf(r.cfg.Out, "⚓ Tether Interactive Agent (%s / %s | mode: %s)\n", r.cfg.Provider, r.cfg.Model, r.engine.GetMode())
	fmt.Fprintf(r.cfg.Out, "Commands: /plan, /code, /model <name>, /provider <name>, /key <api_key>, /status, /exit\n\n")

	if r.cfg.Provider != "ollama" && r.cfg.APIKey == "" {
		fmt.Fprintf(r.cfg.Out, "🔑 Enter %s API Key (saved to ~/.config/tether/credentials.json): ", r.cfg.Provider)
		if scanner.Scan() {
			key := strings.TrimSpace(scanner.Text())
			if key != "" {
				r.cfg.APIKey = key
				_ = SaveAPIKey(r.cfg.Provider, key)
				fmt.Fprintf(r.cfg.Out, "✓ API key saved.\n\n")
			}
		}
	}

	for {
		fmt.Fprintf(r.cfg.Out, "[%s:%s]> ", r.cfg.Model, r.engine.GetMode())
		if !scanner.Scan() {
			fmt.Fprintln(r.cfg.Out)
			return nil
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "/") {
			parts := strings.Fields(line)
			switch parts[0] {
			case "/exit", "/quit":
				return nil
			case "/plan":
				_ = r.engine.SetMode("plan")
				fmt.Fprintln(r.cfg.Out, "🔵 Switched to PLAN mode (codebase writes locked).")
				continue
			case "/code":
				_ = r.engine.SetMode("code")
				fmt.Fprintln(r.cfg.Out, "🟢 Switched to CODE mode (codebase writes unlocked).")
				continue
			case "/model":
				if len(parts) >= 2 {
					r.cfg.Model = parts[1]
					_ = r.engine.SetProjectSetting("model", r.cfg.Model)
					fmt.Fprintf(r.cfg.Out, "✓ Model set to %s\n", r.cfg.Model)
				} else {
					fmt.Fprintf(r.cfg.Out, "Current model: %s (e.g. /model deepseek-reasoner or /model deepseek-chat)\n", r.cfg.Model)
				}
				continue
			case "/provider":
				if len(parts) >= 2 {
					p := strings.ToLower(parts[1])
					r.cfg.Provider = p
					if preset, ok := presets[p]; ok {
						r.cfg.BaseURL = preset.BaseURL
						r.cfg.Model = preset.DefaultModel
					}
					r.cfg.APIKey = loadSavedKey(p)
					_ = r.engine.SetProjectSetting("provider", p)
					fmt.Fprintf(r.cfg.Out, "✓ Provider set to %s (%s)\n", r.cfg.Provider, r.cfg.Model)
				}
				continue
			case "/key":
				if len(parts) >= 2 {
					r.cfg.APIKey = parts[1]
					_ = SaveAPIKey(r.cfg.Provider, r.cfg.APIKey)
					fmt.Fprintf(r.cfg.Out, "✓ API key saved for %s\n", r.cfg.Provider)
				}
				continue
			case "/status":
				fmt.Fprintf(r.cfg.Out, "Workspace: %s | Session: %s | Mode: %s | Provider: %s (%s)\n",
					r.engine.Workspace, r.engine.SessionID, r.engine.GetMode(), r.cfg.Provider, r.cfg.Model)
				continue
			}
		}

		ans, err := r.Run(ctx, line)
		if err != nil {
			fmt.Fprintf(r.cfg.Out, "✖ Error: %v\n\n", err)
			continue
		}
		fmt.Fprintf(r.cfg.Out, "\n%s\n\n", ans)
	}
}

// compactMessages implements surgical turn-aware context management:
// - Keeps system prompt (idx 0), user prompt (idx 1), and the last 2 full assistant turns
//   (including all parallel tool responses in those turns) at 100% fidelity.
// - For older turns outside the active window: strips stale reasoning_content and compacts
//   large tool outputs (>800 runes) using UTF-8 rune-safe head+tail summarization.
func compactMessages(messages []chatMessage) []chatMessage {
	const keepAssistantTurns = 2
	const maxStaleToolRunes = 800

	cutoff := len(messages)
	turnsSeen := 0
	for i := len(messages) - 1; i >= 2; i-- {
		if messages[i].Role == "assistant" {
			turnsSeen++
			if turnsSeen == keepAssistantTurns {
				cutoff = i
				break
			}
		}
	}
	if turnsSeen < keepAssistantTurns || cutoff <= 2 {
		return messages
	}

	out := make([]chatMessage, len(messages))
	copy(out, messages)

	for i := 2; i < cutoff; i++ {
		if out[i].Role == "assistant" && out[i].ReasoningContent != "" {
			out[i].ReasoningContent = ""
		}
		if out[i].Role == "tool" && !strings.Contains(out[i].Content, "[compacted ") {
			runes := []rune(out[i].Content)
			if len(runes) > maxStaleToolRunes {
				omitted := len(runes) - 600
				out[i].Content = fmt.Sprintf(
					"%s\n... [compacted %d chars of stale tool output by Tether Context Manager] ...\n%s",
					string(runes[:300]),
					omitted,
					string(runes[len(runes)-300:]),
				)
			}
		}
	}
	return out
}
