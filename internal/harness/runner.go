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
	"strconv"
	"strings"
	"time"

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/hooks"
	"github.com/crossparties/tether/internal/mcp"
	"github.com/crossparties/tether/internal/snapshot"
	"github.com/crossparties/tether/internal/ui"
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
		cfg.MaxSteps = 80
		// Project setting > built-in default, but an explicit flag still wins
		// because it arrives with MaxSteps already set.
		if v, err := engine.GetProjectSetting("max_steps"); err == nil && v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.MaxSteps = n
			}
		}
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
	engine    *core.Engine
	hooks     *hooks.Handler
	mcp       *mcp.Server
	cfg       Config
	client    *http.Client
	history   []chatMessage
	agentType string

	// DeepSeek Context Caching metrics from the latest API call
	lastCacheHitTokens  int
	lastCacheMissTokens int

	// onDelta, when set, switches callAPI to SSE streaming and receives live token
	// deltas (kind: "content" | "reasoning" | "endstep").
	onDelta func(kind, text string)
}

func NewRunner(engine *core.Engine, cfg Config) *Runner {
	bootstrapSubstrate(engine)
	return &Runner{
		engine:    engine,
		hooks:     hooks.NewHandler(engine),
		mcp:       mcp.NewServer(engine),
		cfg:       ResolveConfig(engine, cfg),
		client:    &http.Client{Timeout: 120 * time.Second},
		agentType: "main",
	}
}

// bootstrapSubstrate ensures project settings and FTS5 Markdown docs KB are populated on first launch
func bootstrapSubstrate(engine *core.Engine) {
	if settings, err := engine.GetAllProjectSettings(); err == nil && len(settings) == 0 {
		for k, v := range core.DetectProjectSettings(engine.Workspace) {
			_ = engine.SetProjectSetting(k, v)
		}
	}
	if stats, err := engine.KBStatus(); err == nil && stats.TotalFiles == 0 {
		_, _ = engine.IndexDirectory(engine.Workspace, nil)
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

// undo restores the files captured during a turn. When turn is empty the most
// recently snapshotted turn is used. It reports progress on the REPL output and
// never returns an error: an undo that cannot run is a message, not a crash.
func (r *Runner) undo(turn string) {
	store := snapshot.New(r.engine.Workspace)

	if turn == "" {
		last, err := store.LastTurn()
		if err != nil {
			fmt.Fprintf(r.cfg.Out, "✖ Could not read snapshots: %v\n\n", err)
			return
		}
		if last == "" {
			fmt.Fprintln(r.cfg.Out, "ℹ Nothing to undo — no snapshot recorded yet.")
			fmt.Fprintln(r.cfg.Out, "  Snapshots are taken before each file write, so run the agent first.")
			fmt.Fprintln(r.cfg.Out)
			return
		}
		turn = last
	}

	restored, err := store.Restore(turn, r.engine.Workspace)
	if len(restored) == 0 && err == nil {
		fmt.Fprintf(r.cfg.Out, "ℹ Turn %s had no captured files.\n\n", ui.Bold(turn))
		return
	}
	for _, path := range restored {
		fmt.Fprintf(r.cfg.Out, "↩ restored %s\n", path)
	}
	if err != nil {
		fmt.Fprintf(r.cfg.Out, "⚠ Undo partially failed for turn %s: %v\n", ui.Bold(turn), err)
		fmt.Fprintln(r.cfg.Out, "  Files listed above were restored; the rest were left untouched.")
		fmt.Fprintln(r.cfg.Out)
		return
	}
	fmt.Fprintf(r.cfg.Out, "✓ Undo complete — %d file(s) restored from turn %s.\n\n", len(restored), ui.Bold(turn))
}

// Run executes a prompt through PreInvocation -> LLM Tool Loop (PreToolUse -> CallTool -> PostToolUse)
func (r *Runner) Run(ctx context.Context, prompt string) (string, error) {
	if r.cfg.Provider != "ollama" && r.cfg.APIKey == "" {
		return "", fmt.Errorf("missing API key for provider %q (set %s or TETHER_API_KEY)", r.cfg.Provider, presets[r.cfg.Provider].EnvKey)
	}

	// 1. PreInvocation Hook: surgical context injection & task mode classification
	// Mark the turn boundary so PreToolUse can snapshot the pre-write state of
	// each file.
	_ = r.engine.SetProjectSetting("current_turn", time.Now().UTC().Format("20060102T150405.000000000"))

	preInvIn, _ := json.Marshal(hooks.HookInput{Prompt: prompt, AgentType: r.agentType})
	preInvRaw, _ := r.hooks.Handle("PreInvocation", bytes.NewReader(preInvIn))
	var preInvOut hooks.HookOutput
	_ = json.Unmarshal([]byte(preInvRaw), &preInvOut)

	sysPrompt := "You are Tether, a local-first autonomous coding agent CLI. Be concise, direct, and technical. Avoid filler or repetitive preamble."
	if len(preInvOut.InjectSteps) > 0 && preInvOut.InjectSteps[0].EphemeralMessage != "" {
		sysPrompt += "\n\n" + preInvOut.InjectSteps[0].EphemeralMessage
	}

	_ = r.engine.AddContext(r.cfg.Provider, "user", prompt)

	var messages []chatMessage
	if len(r.history) == 0 {
		messages = []chatMessage{
			{Role: "system", Content: sysPrompt},
			{Role: "user", Content: prompt},
		}
	} else {
		messages = make([]chatMessage, len(r.history))
		copy(messages, r.history)
		messages[0] = chatMessage{Role: "system", Content: sysPrompt}
		// Strip prior-turn reasoning_content before starting a new user turn (DeepSeek API spec)
		for i := range messages {
			if messages[i].Role == "assistant" {
				messages[i].ReasoningContent = ""
			}
		}
		messages = append(messages, chatMessage{Role: "user", Content: prompt})
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

	keepTurns, maxRunes := r.compactionLimits()

	// 2. Agentic Tool-Execution Loop
	for step := 0; step < r.cfg.MaxSteps; step++ {
		messages = compactMessagesWithLimits(messages, keepTurns, maxRunes)
		msg, err := r.callAPI(ctx, messages, apiTools)
		if err != nil {
			return "", err
		}

		// While streaming, reasoning and intermediate text were already delivered live
		// through onDelta; printing them again would duplicate them in the transcript.
		if msg.ReasoningContent != "" && r.onDelta == nil {
			fmt.Fprintf(r.cfg.Out, "💭 [reasoning] %s\n", strings.TrimSpace(msg.ReasoningContent))
		}
		if len(msg.ToolCalls) > 0 && strings.TrimSpace(msg.Content) != "" && r.onDelta == nil {
			fmt.Fprintf(r.cfg.Out, "\n%s\n", strings.TrimSpace(msg.Content))
		}
		messages = append(messages, msg)

		if len(msg.ToolCalls) == 0 {
			r.history = compactMessagesWithLimits(messages, keepTurns, maxRunes)
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

	// Persist the full working history so typing "continue" actually resumes the
	// same task. Compaction is deliberately skipped here: dropping the tool
	// results the model had just produced was what turned a pause into a restart.
	r.history = messages
	_ = r.engine.AddContext(r.cfg.Provider, "assistant", fmt.Sprintf("[paused after %d tool steps — type 'continue' to resume]", r.cfg.MaxSteps))
	return "", fmt.Errorf("paused after %d steps (type 'continue' or 'lanjutkan' to resume)", r.cfg.MaxSteps)
}

func (r *Runner) compactionLimits() (int, int) {
	keepTurns := 2
	maxRunes := 800
	if v, err := r.engine.GetProjectSetting("keep_assistant_turns"); err == nil && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			keepTurns = n
		}
	}
	if v, err := r.engine.GetProjectSetting("max_stale_tool_runes"); err == nil && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 200 {
			maxRunes = n
		}
	}
	return keepTurns, maxRunes
}

func (r *Runner) executeGuardedTool(tc toolCall) string {
	var argMap map[string]interface{}
	rawArgs := strings.TrimSpace(tc.Function.Arguments)
	if rawArgs != "" && rawArgs != "{}" {
		if err := json.Unmarshal([]byte(rawArgs), &argMap); err != nil {
			// DeepSeek API best practice: strictly validate tool call JSON before execution
			// so hallucinated/truncated JSON is immediately reported back for self-correction.
			return fmt.Sprintf("invalid tool arguments JSON for %s: %v. Please retry with valid JSON.", tc.Function.Name, err)
		}
	}

	hookIn := hooks.HookInput{
		ToolName:  tc.Function.Name,
		AgentType: r.agentType,
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
		fmt.Fprintf(r.cfg.Out, "🛑 [Tether Guard Blocked %s (%s)] %s\n", tc.Function.Name, r.agentType, preOut.Reason)
		return "DENIED BY TETHER GUARD: " + preOut.Reason
	}

	r.printToolAction(tc.Function.Name, argMap)
	res, isErr := r.mcp.CallTool(tc.Function.Name, json.RawMessage(tc.Function.Arguments))
	if tc.Function.Name == "edit_file" || tc.Function.Name == "write_file" {
		if isErr {
			fmt.Fprintf(r.cfg.Out, "  ✖ %s\n", res)
		} else {
			fmt.Fprintf(r.cfg.Out, "  ↳ %s\n", res)
		}
	}

	// PostToolUse Auditor
	_, _ = r.hooks.Handle("PostToolUse", mustJSONReader(hookIn))
	return res
}

func (r *Runner) printToolAction(name string, args map[string]interface{}) {
	// File writes are labelled explicitly. A diff printed here reflects a
	// change that is about to be applied automatically; it is a log, not a
	// request for consent.
	switch name {
	case "edit_file":
		path, _ := args["path"].(string)
		target, _ := args["target"].(string)
		replacement, _ := args["replacement"].(string)
		fmt.Fprintf(r.cfg.Out, "%s %s\n", ui.Dim("WRITE"), ui.Path(path))
		for _, line := range clipLines(target, 12) {
			fmt.Fprintf(r.cfg.Out, "  %s\n", ui.DiffDel("- "+line))
		}
		for _, line := range clipLines(replacement, 12) {
			fmt.Fprintf(r.cfg.Out, "  %s\n", ui.DiffAdd("+ "+line))
		}
	case "write_file":
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
		fmt.Fprintf(r.cfg.Out, "%s %s %s\n",
			ui.Dim("WRITE"), ui.Path(path), ui.Dim(fmt.Sprintf("(%d lines)", len(lines))))
		for _, line := range clipLines(content, 8) {
			fmt.Fprintf(r.cfg.Out, "  %s\n", ui.DiffAdd("+ "+line))
		}
	case "read_file":
		path, _ := args["path"].(string)
		if s, ok := args["start_line"].(float64); ok && s > 0 {
			e, _ := args["end_line"].(float64)
			fmt.Fprintf(r.cfg.Out, "🔧 [read_file] %s (lines %.0f-%.0f)\n", path, s, e)
		} else {
			fmt.Fprintf(r.cfg.Out, "🔧 [read_file] %s\n", path)
		}
	case "glob":
		pat, _ := args["pattern"].(string)
		p, _ := args["path"].(string)
		if p != "" && p != "." {
			fmt.Fprintf(r.cfg.Out, "🔧 [glob] %s (in %s)\n", pat, p)
		} else {
			fmt.Fprintf(r.cfg.Out, "🔧 [glob] %s\n", pat)
		}
	case "grep":
		pat, _ := args["pattern"].(string)
		if pat == "" {
			pat, _ = args["query"].(string)
		}
		p, _ := args["path"].(string)
		if p != "" && p != "." {
			fmt.Fprintf(r.cfg.Out, "🔧 [grep] %q in %s\n", pat, p)
		} else {
			fmt.Fprintf(r.cfg.Out, "🔧 [grep] %q\n", pat)
		}
	case "run_command":
		cmd, _ := args["command"].(string)
		fmt.Fprintf(r.cfg.Out, "🔧 [run_command] $ %s\n", cmd)
	case "kb_search":
		q, _ := args["query"].(string)
		fmt.Fprintf(r.cfg.Out, "🔧 [kb_search] %q\n", q)
	case "todo_manage":
		act, _ := args["action"].(string)
		d, _ := args["description"].(string)
		id, _ := args["id"].(float64)
		switch {
		case d != "":
			fmt.Fprintf(r.cfg.Out, "🔧 [todo_manage] %s: %s\n", act, d)
		case id > 0:
			fmt.Fprintf(r.cfg.Out, "🔧 [todo_manage] %s #%.0f\n", act, id)
		default:
			fmt.Fprintf(r.cfg.Out, "🔧 [todo_manage] %s\n", act)
		}
	case "mode_set":
		m, _ := args["mode"].(string)
		fmt.Fprintf(r.cfg.Out, "🔧 [mode_set] %s\n", m)
	case "decision_add":
		t, _ := args["topic"].(string)
		d, _ := args["decision"].(string)
		fmt.Fprintf(r.cfg.Out, "🔧 [decision_add] [%s] %s\n", t, d)
	default:
		fmt.Fprintf(r.cfg.Out, "🔧 [tool:%s]\n", name)
	}
}

func clipLines(s string, max int) []string {
	lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(s, "\r\n", "\n"), "\n"), "\n")
	if len(lines) > max {
		out := append([]string{}, lines[:max]...)
		return append(out, fmt.Sprintf("... (%d more lines)", len(lines)-max))
	}
	return lines
}

func (r *Runner) callAPI(ctx context.Context, messages []chatMessage, tools []openAITool) (chatMessage, error) {
	payload := map[string]interface{}{
		"model":    r.cfg.Model,
		"messages": messages,
	}
	// DeepSeek API spec: omit "tools" completely when empty (sending "tools": null or [] is discouraged)
	if len(tools) > 0 {
		payload["tools"] = tools
	}

	// DeepSeek API Parameter Optimization:
	// - In Thinking Mode (deepseek-reasoner), temperature/top_p/penalties are unsupported and ignored,
	//   while max_tokens should be generous (8192) so CoT + tool call JSON never truncates.
	// - In Non-Thinking Mode (deepseek-chat), temperature=0.0 is recommended by DeepSeek for
	//   deterministic coding & function calling.
	modelLower := strings.ToLower(r.cfg.Model)
	isReasoner := strings.Contains(modelLower, "reasoner") || strings.Contains(modelLower, "r1")
	if strings.EqualFold(r.cfg.Provider, "deepseek") {
		if isReasoner {
			payload["max_tokens"] = 8192
		} else {
			payload["temperature"] = 0.0
			payload["max_tokens"] = 4096
		}
	}

	// Live token streaming (SSE): used only when a delta consumer is attached.
	if r.onDelta != nil {
		payload["stream"] = true
		payload["stream_options"] = map[string]interface{}{"include_usage": true}
		return r.streamRequest(ctx, payload)
	}

	reqBody, _ := json.Marshal(payload)

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
		Usage struct {
			PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
			PromptTokensDetails   struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Choices) == 0 {
		return chatMessage{}, fmt.Errorf("invalid API response: %s", string(body))
	}
	hit := parsed.Usage.PromptCacheHitTokens
	if hit == 0 {
		hit = parsed.Usage.PromptTokensDetails.CachedTokens
	}
	if hit > 0 || parsed.Usage.PromptCacheMissTokens > 0 {
		r.lastCacheHitTokens = hit
		r.lastCacheMissTokens = parsed.Usage.PromptCacheMissTokens
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

	// Connect the approval gate to this terminal. Without it a blocked write is
	// only a message to the model, and the user is never asked. Hook and
	// JSON-RPC entry points never reach this line, so they keep the
	// refuse-with-instructions behaviour.
	r.mcp.SetAutoApprovePolicy(r.DefaultAutoApprovePolicy())
	r.mcp.SetAskPolicy(newAsker(r.cfg.Out, scanner))

	fmt.Fprintf(r.cfg.Out, "⚓ Tether Interactive Agent (%s / %s | mode: %s)\n", r.cfg.Provider, r.cfg.Model, r.engine.GetMode())
	// Derived from the registry, not typed by hand. The hand-written line that
	// used to sit here had already drifted: it advertised /plan, /code, /model,
	// /provider, /key, /status and /exit, and omitted /quit, /clear and /undo,
	// all of which the REPL handled.
	fmt.Fprintf(r.cfg.Out, "Commands: %s\n", commandNameList())
	fmt.Fprintf(r.cfg.Out, "Type %s for the menu, %s for the full table.\n\n", ui.Dim("/"), ui.Dim("/help"))

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
			// A bare "/" opens the command menu instead of being an unknown
			// command: users type "/" to look things up, and answering that
			// with "unknown command" teaches them nothing. Picking a number
			// runs the command through the same registry as typing its name,
			// so the two paths cannot diverge.
			if line == "/" {
				if chosen, ok := chooseCommand(r.cfg.Out, scanner); ok {
					line = "/" + chosen
				} else {
					continue
				}
			}

			done, known := dispatchCommand(r, line)
			if done {
				return nil
			}
			if !known {
				fmt.Fprintf(r.cfg.Out, "✖ unknown command %q. Type / for the menu, or /help.\n", strings.Fields(line)[0])
			}
			continue
		}

		// Print the answer first, then the mark that closes it. On failure the
		// error is the answer, and the mark still follows so the outcome is
		// never left implicit.
		res := runTurn(r, ctx, line)
		if res.panicMsg != nil {
			fmt.Fprintf(r.cfg.Out, "✖ panic: %v\n", res.panicMsg)
		}
		if res.err != nil {
			fmt.Fprintf(r.cfg.Out, "✖ %v\n", res.err)
		} else {
			fmt.Fprintf(r.cfg.Out, "\n%s\n", res.answer)
		}
		fmt.Fprintf(r.cfg.Out, "%s\n\n", DoneRule(res.outcome, res.elapsed))
	}
}

// compactMessagesWithLimits implements surgical turn-aware context management:
//   - Keeps system prompt (idx 0), user prompt (idx 1), and the last keepAssistantTurns full turns
//     (including all parallel tool responses in those turns) at 100% fidelity.
//   - For older turns outside the active window: strips stale reasoning_content and compacts
//     large tool outputs (>maxStaleToolRunes) using UTF-8 rune-safe head+tail summarization.
func compactMessagesWithLimits(messages []chatMessage, keepAssistantTurns, maxStaleToolRunes int) []chatMessage {

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

	// Find the index of the latest user turn. Per DeepSeek API specification for
	// Thinking Mode + Tool Calls, every assistant message with tool_calls inside
	// the active user turn MUST retain its reasoning_content, whereas older
	// completed user turns (or non-tool assistant messages outside the window)
	// should have reasoning_content stripped.
	lastUserIdx := 1
	for i := len(out) - 1; i >= 1; i-- {
		if out[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}

	for i := 2; i < cutoff; i++ {
		if out[i].Role == "assistant" && out[i].ReasoningContent != "" {
			if i < lastUserIdx || len(out[i].ToolCalls) == 0 {
				out[i].ReasoningContent = ""
			}
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
