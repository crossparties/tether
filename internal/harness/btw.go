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
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg.Provider
}

// ModelName returns the active LLM model identifier.
func (r *Runner) ModelName() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg.Provider != "ollama" && strings.TrimSpace(r.cfg.APIKey) == ""
}

// SaveProviderKey stores and persists the API key for the active provider.
func (r *Runner) SaveProviderKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("API key cannot be empty")
	}
	r.mu.RLock()
	prov := r.cfg.Provider
	baseURL := r.cfg.BaseURL
	model := r.cfg.Model
	pool := r.keyPool
	r.mu.RUnlock()

	if err := SaveAPIKey(prov, key); err != nil {
		return err
	}
	r.mu.Lock()
	r.cfg.APIKey = key
	r.mu.Unlock()

	if pool != nil {
		if err := pool.ReloadFromDisk(); err == nil && pool.Len() > 0 {
			for _, c := range pool.Candidates() {
				if strings.EqualFold(c.Provider, prov) && c.Entry.APIKey == key {
					r.applyKeyCandidate(c, false)
					return nil
				}
			}
		}
		entry := normalizeEntry(prov, ProviderKeyEntry{
			Label:    prov,
			APIKey:   key,
			BaseURL:  baseURL,
			Model:    model,
			Priority: 1,
			Enabled:  true,
		}, 0)
		r.mu.Lock()
		r.pinnedKeyID = entry.ID
		r.mu.Unlock()
	}
	return nil
}

// ListModels returns the curated model choices for the active provider.
func (r *Runner) ListModels() []ui.Choice {
	return modelsFor(r.ProviderName())
}

// SwitchModel validates and updates the active model for the current provider.
func (r *Runner) SwitchModel(name string) (string, error) {
	prov := r.ProviderName()
	canonical, valid := validateModel(prov, name)
	if !valid {
		choices := modelsFor(prov)
		return "", fmt.Errorf("unknown model %q for %s (options: %s)", name, prov, choiceLabels(choices))
	}
	r.mu.Lock()
	r.cfg.Model = canonical
	r.mu.Unlock()
	if r.engine != nil {
		_ = r.engine.SetProjectSetting("model", canonical)
	}
	return canonical, nil
}

// ListProviders returns the available LLM providers in stable order.
func (r *Runner) ListProviders() []ui.Choice {
	return providerChoices()
}

// SwitchProvider switches the active LLM provider and loads its default model & best saved API key.
func (r *Runner) SwitchProvider(name string) (string, string, error) {
	p := strings.ToLower(strings.TrimSpace(name))
	preset, known := presets[p]
	if !known {
		return "", "", fmt.Errorf("unknown provider %q (options: %s)", p, strings.Join(providerNames(), ", "))
	}

	r.mu.Lock()
	r.cfg.Provider = p
	r.cfg.BaseURL = preset.BaseURL
	r.cfg.Model = preset.DefaultModel
	r.cfg.APIKey = loadSavedKey(p)
	r.pinnedKeyID = ""
	pool := r.keyPool
	model := r.cfg.Model
	r.mu.Unlock()

	if pool != nil {
		if pool.persistHealth {
			_ = pool.ReloadFromDisk()
		}
		// Force re-selection for the newly chosen provider rather than returning the previous provider's pinned key.
		pool.mu.Lock()
		pool.pinnedKeyID = ""
		pool.mu.Unlock()
		if cand, ok := pool.SelectKey(context.Background(), p, model); ok && strings.EqualFold(cand.Provider, p) {
			r.applyKeyCandidate(cand, true)
		}
	}

	finalSnap := r.snapshotCallConfig()
	if r.engine != nil {
		_ = r.engine.SetProjectSetting("provider", finalSnap.provider)
		_ = r.engine.SetProjectSetting("model", finalSnap.model)
	}
	return finalSnap.provider, finalSnap.model, nil
}

// ListKeysSummary formats non-secret key pool status lines without exposing raw secrets.
func (r *Runner) ListKeysSummary() string {
	pool := r.KeyPool()
	pinned := r.PinnedKeyID()
	if pool == nil || pool.Len() == 0 {
		return "No keys configured in pool."
	}
	var b strings.Builder
	for _, c := range pool.Candidates() {
		pin := " "
		if c.Entry.ID == pinned {
			pin = "*"
		}
		healthStr := "unprobed"
		if h, ok := pool.HealthOf(c.Entry.ID); ok {
			if h.Healthy {
				healthStr = fmt.Sprintf("ok (%dms)", h.LatencyMs)
			} else {
				healthStr = fmt.Sprintf("unhealthy (%d)", h.StatusCode)
			}
		}
		fmt.Fprintf(&b, " %s %-18s provider=%-10s prio=%d enabled=%v status=%s\n",
			pin, c.Entry.ID, c.Provider, effectivePrio(c.Entry.Priority), c.Entry.Enabled, healthStr)
	}
	return strings.TrimRight(b.String(), "\n")
}

// ProbeKeysSummary runs a zero-token startup probe across configured keys and returns non-secret results.
func (r *Runner) ProbeKeysSummary(ctx context.Context) string {
	pool := r.KeyPool()
	if pool == nil {
		return "No key pool configured."
	}
	pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	results := pool.ProbeAll(pctx)
	if len(results) == 0 {
		return "No enabled keys to probe."
	}
	var b strings.Builder
	for _, h := range results {
		mark := "✔"
		if !h.Healthy {
			mark = "✖"
		}
		fmt.Fprintf(&b, "  %s %-18s (%s) status=%d latency=%dms %s\n",
			mark, h.KeyID, h.Provider, h.StatusCode, h.LatencyMs, h.Reason)
	}
	return strings.TrimRight(b.String(), "\n")
}

// SwitchMode updates the substrate mode ("code" or "plan").
func (r *Runner) SwitchMode(mode string) error {
	if r.engine == nil {
		return nil
	}
	return r.engine.SetMode(mode)
}

// ClearHistory clears the in-memory conversation buffer and session cache head while retaining .tether/session.db memory.
func (r *Runner) ClearHistory() {
	r.mu.Lock()
	r.history = nil
	r.frozenSystemPrompt = ""
	r.frozenTools = nil
	r.frozenExtTools = nil
	r.syncHooksReadOnlyExternalToolsLocked()
	r.baselineRepoMapFiles = nil
	r.activeFileHashes = nil
	r.lastPromptMode = ""
	r.lastTodoDigest = ""
	r.pendingTailNotices = nil
	r.mu.Unlock()
}

// UndoTurn restores files from the given snapshot turn (or latest when empty) and returns the output text.
func (r *Runner) UndoTurn(turn string) string {
	var buf bytes.Buffer
	r.undoTo(turn, &buf)
	return strings.TrimSpace(buf.String())
}

// StatusSummary returns a single-line summary of workspace, session, mode, KB, and provider.
func (r *Runner) StatusSummary() string {
	r.mu.RLock()
	prov := r.cfg.Provider
	mod := r.cfg.Model
	hits := r.lastCacheHitTokens
	misses := r.lastCacheMissTokens
	tracker := r.usageTracker
	r.mu.RUnlock()

	var pressurePct float64
	if tracker != nil {
		if mainMetrics, ok := tracker.Get("main"); ok && mainMetrics.Calls > 0 {
			hits = mainMetrics.LastCacheHit
			misses = mainMetrics.LastCacheMiss
			pressurePct = mainMetrics.ContextPressure * 100.0
		}
	}

	if r.engine == nil {
		return fmt.Sprintf("Provider: %s (%s)", prov, mod)
	}
	kbStats, _ := r.engine.KBStatus()
	cacheInfo := ""
	if hits > 0 || misses > 0 {
		total := hits + misses
		pct := 0
		if total > 0 {
			pct = (hits * 100) / total
		}
		cacheInfo = fmt.Sprintf(" | Cache Hit: %d/%d (%d%%)", hits, total, pct)
	}
	if pressurePct > 0 {
		cacheInfo += fmt.Sprintf(" | Ctx: %.1f%%", pressurePct)
	}
	return fmt.Sprintf("Workspace: %s | Session: %s | Mode: %s | KB: %d files | Provider: %s (%s)%s",
		r.engine.Workspace, r.engine.SessionID, r.engine.GetMode(), kbStats.TotalFiles, prov, mod, cacheInfo)
}

// SetAskPolicy installs a custom interactive approval handler on the underlying MCP server.
func (r *Runner) SetAskPolicy(fn func(mcp.Decision) (mcp.Verdict, bool)) {
	if r.mcp != nil {
		r.mcp.SetAskPolicy(fn)
	}
}

// SetAskPolicyWithReason installs a custom interactive approval handler with explicit reject reason.
func (r *Runner) SetAskPolicyWithReason(fn func(mcp.Decision) (mcp.Verdict, string, bool)) {
	if r.mcp != nil {
		r.mcp.SetAskPolicyWithReason(fn)
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
	return r.runWithOptions(ctx, prompt, out, nil)
}

// expandAtMentions reads workspace @file mentions in isolation (used by /btw without touching activeFileHashes).
func (r *Runner) expandAtMentions(prompt string) string {
	mentions := extractAtMentionPaths(prompt)
	if len(mentions) == 0 || r.engine == nil || r.engine.Workspace == "" {
		return prompt
	}
	mountEng := r.mountForRead()
	var attachments []string
	for _, rel := range mentions {
		if content, cleanRel, err := tetherctx.SafeReadWorkspaceFileWithMount(mountEng, r.engine.Workspace, rel, 32*1024); err == nil {
			attachments = append(attachments, fmt.Sprintf("<file path=%q>\n%s\n</file>", cleanRel, content))
		}
	}
	if len(attachments) == 0 {
		return prompt
	}
	return prompt + "\n\n[Attached Workspace Files via @mention]:\n" + strings.Join(attachments, "\n\n")
}

// RunSideChat executes an isolated, context-aware `/btw <question>` turn.
// It guarantees zero pollution to Main:
//  1. Takes a read-only snapshot of Main history tail (never mutates r.history).
//  2. Expands @file mentions safely via SafeReadWorkspaceFile / mount.AuthorizePath.
//  3. Permits strictly read-only tools (read_file, glob, grep, kb_search) when tools exist, blocking mutations.
//  4. Tags usage strictly under usage_id: "btw", isolated from "main" cache counters.
func (r *Runner) RunSideChat(ctx context.Context, question string) (string, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return "", fmt.Errorf("question is required for /btw")
	}
	snap := r.snapshotCallConfig()
	if snap.provider != "ollama" && strings.TrimSpace(snap.apiKey) == "" {
		return "", fmt.Errorf("missing API key for provider %q", snap.provider)
	}

	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	mainSnap := r.snapshotMainHistoryTail()
	expandedQ := r.expandAtMentions(question)

	var sysB strings.Builder
	sysB.WriteString("You are answering an isolated, read-only /btw side-chat question inside Tether. Be concise and technical. Never modify files or state.")
	if mainSnap != "" {
		sysB.WriteString("\n\n<main_context_snapshot read_only=\"true\">\n")
		sysB.WriteString(mainSnap)
		sysB.WriteString("\n</main_context_snapshot>")
	}

	messages := []chatMessage{
		{Role: "system", Content: sysB.String()},
		{Role: "user", Content: expandedQ},
	}

	readOnlyTools := r.btwReadOnlyTools(question, mainSnap)
	const maxBtwSteps = 6
	for step := 0; step < maxBtwSteps; step++ {
		msg, _, err := r.callAPIWithFailoverUsageID(callCtx, "btw", messages, readOnlyTools, nil, nil)
		if err != nil {
			return "", err
		}
		if len(msg.ToolCalls) == 0 {
			r.mu.Lock()
			r.lastBtwQuestion = question
			r.lastBtwAnswer = msg.Content
			r.mu.Unlock()
			return msg.Content, nil
		}
		messages = append(messages, msg)
		for _, tc := range msg.ToolCalls {
			if !isBtwAllowedTool(tc.Function.Name) {
				messages = append(messages, chatMessage{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content:    fmt.Sprintf("DENIED BY TETHER GUARD: mutating/unsupported tool %q is strictly forbidden in /btw side-chat.", tc.Function.Name),
				})
				continue
			}
			out, _ := r.mcp.CallTool(tc.Function.Name, json.RawMessage(tc.Function.Arguments))
			messages = append(messages, chatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    out,
			})
		}
	}
	return "", fmt.Errorf("/btw exceeded max read-only tool steps (%d)", maxBtwSteps)
}

func isBtwAllowedTool(name string) bool {
	switch name {
	case "read_file", "glob", "grep", "kb_search":
		return true
	}
	return false
}

func (r *Runner) btwReadOnlyTools(question, mainSnap string) []openAITool {
	if r.mcp == nil {
		return nil
	}
	// Preserve lean payloads for simple stateless prompts unless context or workspace inspection is relevant.
	if mainSnap == "" && !strings.Contains(question, "@") && !looksLikeCodeInspectionQuery(question) {
		return nil
	}
	var tools []openAITool
	for _, t := range r.mcp.ListTools() {
		if isBtwAllowedTool(t.Name) {
			tools = append(tools, toOpenAITool(t))
		}
	}
	return tools
}

func looksLikeCodeInspectionQuery(q string) bool {
	low := strings.ToLower(q)
	for _, kw := range []string{"file", "read", "grep", "search", "code", "func", "where", "how does", "line"} {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

func (r *Runner) snapshotMainHistoryTail() string {
	r.mu.RLock()
	if len(r.history) <= 1 {
		r.mu.RUnlock()
		return ""
	}
	cp := make([]chatMessage, len(r.history))
	copy(cp, r.history)
	r.mu.RUnlock()

	start := 1
	if len(cp)-start > 8 {
		start = len(cp) - 8
	}
	var b strings.Builder
	for i := start; i < len(cp); i++ {
		m := cp[i]
		content := clipTextRunes(strings.TrimSpace(m.Content), 500)
		if content == "" && len(m.ToolCalls) > 0 {
			var names []string
			for _, tc := range m.ToolCalls {
				names = append(names, tc.Function.Name)
			}
			content = "tool_calls: " + strings.Join(names, ", ")
		}
		if content == "" {
			continue
		}
		fmt.Fprintf(&b, "[%s] %s\n", m.Role, content)
	}
	return strings.TrimRight(b.String(), "\n")
}

// PromoteBtwToMain appends a /btw Q&A cleanly to the tail of Main's history (leaving
// all prior messages 100% immutable so existing KV-cache prefixes stay valid).
func (r *Runner) PromoteBtwToMain(question, answer string) error {
	r.mu.Lock()
	if strings.TrimSpace(question) == "" {
		question = r.lastBtwQuestion
	}
	if strings.TrimSpace(answer) == "" {
		answer = r.lastBtwAnswer
	}
	r.mu.Unlock()

	if strings.TrimSpace(answer) == "" {
		return fmt.Errorf("no /btw answer to promote")
	}
	if strings.TrimSpace(question) == "" {
		question = "Side-chat context"
	}

	head, _ := r.ensureFrozenHead()

	r.mu.Lock()
	if len(r.history) == 0 {
		r.history = []chatMessage{{Role: "system", Content: head}}
	}
	userMsg := chatMessage{Role: "user", Content: "[Promoted from /btw]\n" + question}
	asstMsg := chatMessage{Role: "assistant", Content: answer}
	r.history = append(r.history, userMsg, asstMsg)
	r.mu.Unlock()

	r.persistMessageEvent("user", "main", userMsg)
	r.persistMessageEvent("assistant", "main", asstMsg)
	return nil
}

// PinBtwAsDecision records a decision in .tether/session.db and queues a tail
// reminder for Main without mutating messages[0].
func (r *Runner) PinBtwAsDecision(topic, decision, reason string) error {
	if r == nil || r.engine == nil {
		return fmt.Errorf("runner has no active engine")
	}
	if strings.TrimSpace(topic) == "" {
		topic = "btw"
	}
	if strings.TrimSpace(decision) == "" {
		r.mu.RLock()
		decision = r.lastBtwAnswer
		r.mu.RUnlock()
	}
	if strings.TrimSpace(decision) == "" {
		return fmt.Errorf("decision text required")
	}
	if err := r.engine.AddDecision(topic, decision, reason); err != nil {
		return err
	}
	r.QueueTailNotice(fmt.Sprintf("Pinned decision from /btw [%s]: %s", topic, clipTextRunes(decision, 180)))
	return nil
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
