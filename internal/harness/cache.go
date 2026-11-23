package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	tetherctx "github.com/crossparties/tether/internal/context"
	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/mcp"
	"github.com/crossparties/tether/internal/mount"
	"github.com/crossparties/tether/internal/repomap"
)

// SupportsCacheControl reports whether provider/baseURL/model supports explicit
// Anthropic/OpenRouter-style multipart cache_control breakpoints. DeepSeek, OpenAI,
// Groq, Gemini, xAI, and Ollama keep plain string message content.
func SupportsCacheControl(provider, baseURL, model string) bool {
	prov := strings.ToLower(strings.TrimSpace(provider))
	base := strings.ToLower(strings.TrimSpace(baseURL))
	mod := strings.ToLower(strings.TrimSpace(model))

	if prov == "anthropic" || strings.Contains(base, "anthropic.com") {
		return true
	}
	if prov == "openrouter" || strings.Contains(base, "openrouter.ai") {
		return true
	}
	if strings.HasPrefix(mod, "anthropic/") || strings.HasPrefix(mod, "claude-") {
		return true
	}
	return false
}

type wireCacheControl struct {
	Type string `json:"type"`
}

type wireContentBlock struct {
	Type         string            `json:"type"`
	Text         string            `json:"text"`
	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
}

type wireMessage struct {
	Role             string             `json:"role"`
	Content          []wireContentBlock `json:"content,omitempty"`
	ReasoningContent string             `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCall         `json:"tool_calls,omitempty"`
	ToolCallID       string             `json:"tool_call_id,omitempty"`
	Prefix           bool               `json:"prefix,omitempty"`
}

func buildWireMessages(messages []chatMessage, useCacheControl bool) any {
	if !useCacheControl {
		return messages
	}
	bpIdx := -1
	if len(messages) >= 3 {
		for i := len(messages) - 2; i >= 1; i-- {
			if strings.TrimSpace(messages[i].Content) != "" {
				bpIdx = i
				break
			}
		}
	}

	out := make([]wireMessage, len(messages))
	for i, m := range messages {
		wm := wireMessage{
			Role:             m.Role,
			ReasoningContent: m.ReasoningContent,
			ToolCalls:        m.ToolCalls,
			ToolCallID:       m.ToolCallID,
			Prefix:           m.Prefix,
		}
		if m.Content != "" {
			block := wireContentBlock{
				Type: "text",
				Text: m.Content,
			}
			if i == 0 || i == bpIdx {
				block.CacheControl = &wireCacheControl{Type: "ephemeral"}
			}
			wm.Content = []wireContentBlock{block}
		}
		out[i] = wm
	}
	return out
}

// buildFrozenTier0to3Head constructs the immutable session system prompt (Tier 0-3)
// once per thread/session so KV-cache prefixes stay identical across turns.
func (r *Runner) buildFrozenTier0to3Head() (string, map[string]bool) {
	var b strings.Builder

	// Tier 0: Immutable base instructions & substrate protocol.
	b.WriteString("You are Tether, a local-first autonomous coding agent. Be concise, direct, and technical. Avoid filler.\n\n")
	b.WriteString("## Tier 0: Tether Substrate Protocol\n")
	b.WriteString("- Never use shell/python commands to read or mutate .tether/session.db or .tether/cache.db.\n")
	b.WriteString("- Always use Tether core tools (`read_file`, `write_file`, `edit_file`, `glob`, `grep`, `run_command`, `kb_search`, `todo_manage`, `mode_set`, `decision_add`).\n")
	b.WriteString("- Dynamic mode transitions, updated todos, undo notices, and focused symbol deltas arrive in per-turn tail reminders.\n")

	if r.engine == nil {
		return strings.TrimRight(b.String(), "\n"), nil
	}

	// Tier 1: Static project metadata & initial decisions/patterns baseline (frozen at session start).
	b.WriteString("\n## Tier 1: Project Baseline\n")
	fmt.Fprintf(&b, "- Workspace: %s\n", r.engine.Workspace)
	if r.engine.Branch != "" {
		fmt.Fprintf(&b, "- Branch: %s\n", r.engine.Branch)
	}
	if settings, err := r.engine.GetAllProjectSettings(); err == nil && len(settings) > 0 {
		keys := make([]string, 0, len(settings))
		for k := range settings {
			if k == "current_turn" {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "- %s: %s\n", k, clipTextRunes(settings[k], 240))
		}
	}
	if summary, err := r.engine.GetSessionSummary(); err == nil && strings.TrimSpace(summary) != "" {
		fmt.Fprintf(&b, "- Session summary: %s\n", clipTextRunes(summary, 300))
	}
	if decisions, err := r.engine.InjectDecisions(5); err == nil && len(decisions) > 0 {
		b.WriteString("\n### Pinned Decisions Baseline\n")
		for _, d := range decisions {
			fmt.Fprintf(&b, "- [%s] %s (reason: %s)\n", d.Topic, clipTextRunes(d.Decision, 260), clipTextRunes(d.Reason, 200))
		}
	}
	if patterns, err := r.engine.InjectPatterns(5); err == nil && len(patterns) > 0 {
		b.WriteString("\n### Learned Patterns Baseline\n")
		for _, p := range patterns {
			fmt.Fprintf(&b, "- [%s] %s\n", p.Type, clipTextRunes(p.Description, 240))
		}
	}

	// Tier 2: Static governance & workflow rules.
	b.WriteString("\n## Tier 2: Governance Rules\n")
	b.WriteString("- `code` mode: workspace edits allowed (protected by snapshots, .tether/ignore, secret rules, and approval policy).\n")
	b.WriteString("- `plan` mode: codebase mutations blocked; planning writes allowed only in `.tether/PLAN.md`, `docs/**`, `.agents/**`, and `*.plan.md`.\n")
	if r.depth == 0 {
		b.WriteString("- Use `crew_create`, `crew_message`, `crew_close` to delegate isolated sub-tasks with explicit file ownership leases when helpful.\n")
	}

	// Tier 3: Deterministic Baseline RepoMap (~800-1024 tokens).
	var covered map[string]bool
	if r.engine.Workspace != "" {
		builder := repomap.NewBuilder(r.engine.Workspace, r.engine.CacheDB(), r.engine.Mount())
		if baseline, cov := builder.BuildBaselineDetailed(repomap.DefaultBaselineTokens); baseline != "" {
			b.WriteString("\n## Tier 3: Baseline RepoMap\n")
			b.WriteString(baseline)
			b.WriteByte('\n')
			covered = cov
		}
	}

	return strings.TrimRight(b.String(), "\n"), covered
}

// ensureFrozenHead returns the session's immutable system prompt and frozen tool schema.
func (r *Runner) ensureFrozenHead() (string, []openAITool) {
	r.mu.Lock()
	if r.frozenSystemPrompt == "" {
		head, cov := r.buildFrozenTier0to3Head()
		r.frozenSystemPrompt = head
		r.baselineRepoMapFiles = cov
	}
	needTools := len(r.frozenTools) == 0
	extMgr := r.extMCP
	depth := r.depth
	sameProv := r.sameProviderCrew
	roCrew := r.readOnlyCrew
	r.mu.Unlock()

	var extTools []mcp.ExternalTool
	if needTools && extMgr != nil && (depth == 0 || sameProv || roCrew) {
		connCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		extTools, _ = extMgr.ListEnabledTools(connCtx)
		cancel()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.frozenTools) == 0 {
		tools, extMap := r.buildSessionToolsLocked(extTools)
		r.frozenTools = tools
		r.frozenExtTools = extMap
		r.syncHooksReadOnlyExternalToolsLocked()
	}
	toolsCopy := make([]openAITool, len(r.frozenTools))
	copy(toolsCopy, r.frozenTools)
	return r.frozenSystemPrompt, toolsCopy
}

func (r *Runner) syncHooksReadOnlyExternalToolsLocked() {
	if r.hooks == nil {
		return
	}
	if len(r.frozenExtTools) == 0 {
		r.hooks.SetExternalReadOnlyTools(nil)
		return
	}
	ro := make(map[string]bool)
	for name, t := range r.frozenExtTools {
		if t.ReadOnly {
			ro[name] = true
		}
	}
	r.hooks.SetExternalReadOnlyTools(ro)
}

func (r *Runner) buildSessionToolsLocked(extTools []mcp.ExternalTool) ([]openAITool, map[string]mcp.ExternalTool) {
	mcpTools := r.mcp.ListTools()
	var selected []openAITool
	var extMap map[string]mcp.ExternalTool

	addExt := func(onlyReadOnly bool) {
		if len(extTools) == 0 {
			return
		}
		for _, et := range extTools {
			if onlyReadOnly && !et.ReadOnly {
				continue
			}
			if extMap == nil {
				extMap = make(map[string]mcp.ExternalTool)
			}
			extMap[et.Name] = et
			selected = append(selected, externalToOpenAITool(et))
		}
	}

	switch {
	case r.readOnlyCrew:
		allowed := map[string]bool{"read_file": true, "glob": true, "grep": true, "kb_search": true}
		for _, t := range mcpTools {
			if allowed[t.Name] {
				selected = append(selected, toOpenAITool(t))
			}
		}
		addExt(true)
	case r.depth >= 1 && !r.sameProviderCrew:
		// Mode 2 (Different-Provider Crew): focused core toolset without crew_* or mode/todo bloat.
		allowed := map[string]bool{
			"read_file": true, "write_file": true, "edit_file": true,
			"glob": true, "grep": true, "run_command": true,
			"kb_search": true, "decision_add": true,
		}
		for _, t := range mcpTools {
			if allowed[t.Name] {
				selected = append(selected, toOpenAITool(t))
			}
		}
	default:
		// Main (depth == 0) AND Same-Provider Crew (depth >= 1): identical 10 core + 3 crew + frozen external MCP schema!
		for _, t := range mcpTools {
			selected = append(selected, toOpenAITool(t))
		}
		selected = append(selected, crewToolsSchema()...)
		addExt(false)
	}
	return selected, extMap
}

func externalToOpenAITool(t mcp.ExternalTool) openAITool {
	params := any(t.InputSchema)
	if len(t.InputSchema) == 0 {
		params = mcp.InputSchema{Type: "object", Properties: map[string]mcp.Property{}}
	}
	return openAITool{
		Type: "function",
		Function: openAIFunction{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
		},
	}
}

func toOpenAITool(t mcp.Tool) openAITool {
	return openAITool{
		Type: "function",
		Function: openAIFunction{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		},
	}
}

// QueueTailNotice appends a short notification to be injected into the tail of the next turn.
func (r *Runner) QueueTailNotice(notice string) {
	notice = strings.TrimSpace(notice)
	if notice == "" {
		return
	}
	r.mu.Lock()
	r.pendingTailNotices = append(r.pendingTailNotices, notice)
	r.mu.Unlock()
}

type stagedTurnDelta struct {
	stagedHashes   map[string]string
	nextMode       string
	hasNextMode    bool
	nextTodoDigest string
	hasTodoDigest  bool
	consumedNotice int
}

func (r *Runner) mountForRead() *mount.Engine {
	if r != nil && r.engine != nil {
		return r.engine.Mount()
	}
	return nil
}

// prepareTurnUserContent expands @file mentions with SHA-256 deduplication and
// appends a compact per-turn tail reminder (mode transitions, todos delta,
// undo/external-edit notices, and Focused Delta RepoMap) without mutating
// activeFileHashes, lastPromptMode, lastTodoDigest, or pendingTailNotices
// until step 0 succeeds cleanly.
func (r *Runner) prepareTurnUserContent(prompt string) (string, stagedTurnDelta) {
	ws := ""
	if r.engine != nil {
		ws = r.engine.Workspace
	}

	expandedPrompt, focusFiles, stagedHashes := r.expandAtMentionsWithDedup(ws, prompt)
	staged := stagedTurnDelta{
		stagedHashes: stagedHashes,
	}

	var tailLines []string
	curMode := r.ModeName()

	r.mu.RLock()
	lastMode := r.lastPromptMode
	lastTodo := r.lastTodoDigest
	notices := append([]string(nil), r.pendingTailNotices...)
	baselineCov := r.baselineRepoMapFiles
	r.mu.RUnlock()

	if lastMode == "" {
		if curMode != "code" {
			tailLines = append(tailLines, fmt.Sprintf("- Mode: %s (codebase writes restricted)", curMode))
		}
		staged.nextMode = curMode
		staged.hasNextMode = true
	} else if curMode != lastMode {
		detail := "codebase mutations unlocked"
		if curMode == "plan" {
			detail = "codebase writes restricted to plan artifacts"
		} else if curMode == "review" {
			detail = "read-only review active"
		}
		tailLines = append(tailLines, fmt.Sprintf("- Mode transition: %s -> %s (%s)", lastMode, curMode, detail))
		staged.nextMode = curMode
		staged.hasNextMode = true
	}

	if r.engine != nil {
		if todos, err := r.engine.ListTodos(false); err == nil {
			digest := formatTodoDigest(todos)
			if digest != lastTodo {
				if digest == "" {
					tailLines = append(tailLines, "- Todos updated: all pending todos cleared/completed.")
				} else {
					tailLines = append(tailLines, "- Updated Pending Todos:\n"+digest)
				}
				staged.nextTodoDigest = digest
				staged.hasTodoDigest = true
			}
		}
	}

	if len(notices) > 0 {
		for _, n := range notices {
			tailLines = append(tailLines, "- Notice: "+n)
		}
		staged.consumedNotice = len(notices)
	}

	if ws != "" && len(focusFiles) > 0 && r.engine != nil {
		delta := repomap.NewBuilder(ws, r.engine.CacheDB(), r.engine.Mount()).
			BuildDelta(focusFiles, baselineCov, repomap.DefaultDeltaTokens)
		if delta != "" {
			tailLines = append(tailLines, "<focused_repomap>\n"+delta+"\n</focused_repomap>")
		}
	}

	if len(tailLines) == 0 {
		return expandedPrompt, staged
	}
	return expandedPrompt + "\n\n<tether_turn_context>\n" + strings.Join(tailLines, "\n") + "\n</tether_turn_context>", staged
}

func (r *Runner) commitTurnDelta(staged stagedTurnDelta) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(staged.stagedHashes) > 0 {
		if r.activeFileHashes == nil {
			r.activeFileHashes = make(map[string]string, len(staged.stagedHashes))
		}
		for k, v := range staged.stagedHashes {
			r.activeFileHashes[k] = v
		}
	}
	if staged.hasNextMode {
		r.lastPromptMode = staged.nextMode
	}
	if staged.hasTodoDigest {
		r.lastTodoDigest = staged.nextTodoDigest
	}
	if staged.consumedNotice > 0 {
		if len(r.pendingTailNotices) <= staged.consumedNotice {
			r.pendingTailNotices = nil
		} else {
			r.pendingTailNotices = append([]string(nil), r.pendingTailNotices[staged.consumedNotice:]...)
		}
	}
}

func extractAtMentionPaths(prompt string) []string {
	if !strings.Contains(prompt, "@") {
		return nil
	}
	words := strings.Fields(prompt)
	var out []string
	seen := make(map[string]bool)
	for _, w := range words {
		if !strings.HasPrefix(w, "@") || len(w) <= 1 {
			continue
		}
		rel := strings.TrimRight(strings.TrimPrefix(w, "@"), ",.;:!?)]}\"'")
		if rel == "" || seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, rel)
	}
	return out
}

func (r *Runner) expandAtMentionsWithDedup(workspace, prompt string) (string, []string, map[string]string) {
	mentions := extractAtMentionPaths(prompt)
	if len(mentions) == 0 || workspace == "" {
		return prompt, nil, nil
	}
	var attachments []string
	var focusFiles []string
	stagedHashes := make(map[string]string)
	mountEng := r.mountForRead()

	for _, rel := range mentions {
		content, cleanRel, err := tetherctx.SafeReadWorkspaceFileWithMount(mountEng, workspace, rel, 32*1024)
		if err != nil {
			continue
		}
		focusFiles = append(focusFiles, cleanRel)

		sum := sha256.Sum256([]byte(content))
		hashHex := hex.EncodeToString(sum[:12])

		prevHash := stagedHashes[cleanRel]
		if prevHash == "" {
			r.mu.RLock()
			if r.activeFileHashes != nil {
				prevHash = r.activeFileHashes[cleanRel]
			}
			r.mu.RUnlock()
		}
		if prevHash == hashHex {
			attachments = append(attachments, fmt.Sprintf("<file path=%q status=\"unchanged_in_active_context\" />", cleanRel))
			continue
		}
		stagedHashes[cleanRel] = hashHex
		attachments = append(attachments, fmt.Sprintf("<file path=%q>\n%s\n</file>", cleanRel, content))
	}

	if len(attachments) == 0 {
		return prompt, focusFiles, stagedHashes
	}
	return prompt + "\n\n[Attached Workspace Files via @mention]:\n" + strings.Join(attachments, "\n\n"), focusFiles, stagedHashes
}

func formatTodoDigest(todos []core.Todo) string {
	if len(todos) == 0 {
		return ""
	}
	var b strings.Builder
	for _, t := range todos {
		fmt.Fprintf(&b, "  - [#%d] %s\n", t.ID, clipTextRunes(t.Description, 140))
	}
	return strings.TrimRight(b.String(), "\n")
}

func clipTextRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}
