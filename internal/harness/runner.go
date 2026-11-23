package harness

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/hooks"
	"github.com/crossparties/tether/internal/mcp"
	"github.com/crossparties/tether/internal/safety"
	"github.com/crossparties/tether/internal/snapshot"
	"github.com/crossparties/tether/internal/ui"
)

const (
	defaultMaxParallelReadTools = 4
	defaultMaxLengthContinue    = 2
	defaultStuckRepeatLimit     = 3
	defaultWarmCacheInterval    = 295 * time.Second
	defaultWarmCacheMaxPings    = 4
	defaultWarmCacheIdleMax     = 10 * time.Minute
)

// Config holds Universal API Harness settings
type Config struct {
	Provider          string
	BaseURL           string
	APIKey            string
	Model             string
	MaxSteps          int
	ContextWindow     int
	CondenseRatio     float64
	CondenseMaxEvents int
	Out               io.Writer
	StreamIdleTimeout time.Duration
	WarmCacheInterval time.Duration
	WarmCacheMaxPings int
	WarmCacheIdleMax  time.Duration
	ExternalMCP       *mcp.ClientManager
}

type callSnapshot struct {
	provider    string
	baseURL     string
	apiKey      string
	model       string
	pinnedKeyID string
	idleTimeout time.Duration
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

// ResolveConfig merges CLI flags, session.db settings, environment variables, and credentials.json
func ResolveConfig(engine *core.Engine, cfg Config) Config {
	explicitProvider := cfg.Provider != ""
	if cfg.Provider == "" && engine != nil {
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
	if cfg.Provider == "" && cfg.APIKey == "" && cfg.BaseURL == "" {
		if creds, err := LoadAllCredentials(); err == nil && len(creds) > 0 {
			for _, p := range []string{"deepseek", "openrouter", "openai", "groq", "gemini", "xai", "ollama"} {
				if entries := creds[p]; len(entries) > 0 {
					for _, e := range entries {
						if e.Enabled && (e.APIKey != "" || p == "ollama") {
							cfg.Provider = p
							break
						}
					}
					if cfg.Provider != "" {
						break
					}
				}
			}
		}
	}
	if cfg.Provider == "" {
		cfg.Provider = "deepseek"
	}

	preset, ok := presets[strings.ToLower(cfg.Provider)]
	var savedEntry *ProviderKeyEntry
	if !explicitProvider || cfg.APIKey == "" {
		if creds, err := LoadAllCredentials(); err == nil {
			entries := creds[strings.ToLower(cfg.Provider)]
			sort.SliceStable(entries, func(i, j int) bool {
				return effectivePrio(entries[i].Priority) < effectivePrio(entries[j].Priority)
			})
			for i := range entries {
				if entries[i].Enabled && (entries[i].APIKey != "" || strings.EqualFold(cfg.Provider, "ollama")) {
					savedEntry = &entries[i]
					break
				}
			}
		}
	}

	if cfg.BaseURL == "" && engine != nil {
		cfg.BaseURL, _ = engine.GetProjectSetting("base_url")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = os.Getenv("TETHER_BASE_URL")
	}
	if cfg.BaseURL == "" && savedEntry != nil && savedEntry.BaseURL != "" {
		cfg.BaseURL = savedEntry.BaseURL
	}
	if cfg.BaseURL == "" && ok {
		cfg.BaseURL = preset.BaseURL
	}

	if cfg.Model == "" && engine != nil {
		cfg.Model, _ = engine.GetProjectSetting("model")
	}
	if cfg.Model == "" {
		cfg.Model = os.Getenv("TETHER_MODEL")
	}
	if cfg.Model == "" && savedEntry != nil && savedEntry.Model != "" {
		cfg.Model = savedEntry.Model
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
	if cfg.APIKey == "" && engine != nil {
		cfg.APIKey, _ = engine.GetProjectSetting("api_key")
	}
	if cfg.APIKey == "" && savedEntry != nil {
		cfg.APIKey = savedEntry.APIKey
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 80
		// Project setting > built-in default, but an explicit flag still wins
		// because it arrives with MaxSteps already set.
		if engine != nil {
			if v, err := engine.GetProjectSetting("max_steps"); err == nil && v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					cfg.MaxSteps = n
				}
			}
		}
	}
	if cfg.Out == nil {
		cfg.Out = os.Stdout
	}
	return cfg
}

// Runner executes the Tether-anchored autonomous agent loop for any LLM API
type Runner struct {
	mu        sync.RWMutex
	engine    *core.Engine
	hooks     *hooks.Handler
	mcp       *mcp.Server
	cfg       Config
	client    *http.Client
	history   []chatMessage
	agentType string

	depth            int
	crewID           string
	usageID          string
	threadID         string
	turnSeq          atomic.Uint64
	sameProviderCrew bool
	readOnlyCrew     bool

	// Immutable Tier 0-3 session head & frozen tool schema (KV-cache head).
	frozenSystemPrompt   string
	frozenTools          []openAITool
	frozenExtTools       map[string]mcp.ExternalTool
	baselineRepoMapFiles map[string]bool
	activeFileHashes     map[string]string
	lastPromptMode       string
	lastTodoDigest       string
	pendingTailNotices   []string

	extMCP       *mcp.ClientManager
	keyPool      *KeyPool
	pinnedKeyID  string
	usageTracker *UsageTracker
	crews        map[string]*crewHandle

	lastBtwQuestion string
	lastBtwAnswer   string

	// DeepSeek Context Caching & TTFT metrics from the latest API call
	lastTTFT            time.Duration
	lastCacheHitTokens  int
	lastCacheMissTokens int

	outMu sync.Mutex

	lastUserActivity  time.Time
	warmCacheMu       sync.Mutex
	warmCachePings    int
	warmCacheDisabled bool

	onEvent  func(kind, detail string)
	execTool func(toolCall) string
}

func NewRunner(engine *core.Engine, cfg Config) *Runner {
	bootstrapSubstrate(engine)
	explicit := cfg.BaseURL != "" || cfg.APIKey != ""
	resolved := ResolveConfig(engine, cfg)
	client := &http.Client{Timeout: 120 * time.Second}

	mcpSrv := mcp.NewServer(engine)
	mcpSrv.SetActorID("main")

	threadID := ""
	if engine != nil {
		threadID = engine.SessionID
		_, _ = engine.EnsureThread(threadID, "Main Thread")
	}

	r := &Runner{
		engine:           engine,
		hooks:            hooks.NewHandler(engine),
		mcp:              mcpSrv,
		cfg:              resolved,
		client:           client,
		agentType:        "main",
		usageID:          "main",
		threadID:         threadID,
		usageTracker:     NewUsageTracker(),
		activeFileHashes: make(map[string]string),
		crews:            make(map[string]*crewHandle),
		lastUserActivity: time.Now(),
	}

	if cfg.ExternalMCP != nil {
		r.extMCP = cfg.ExternalMCP
	} else if engine != nil && engine.Workspace != "" {
		r.extMCP = mcp.NewClientManager(engine.Workspace)
	}

	if explicit && os.Getenv("TETHER_CONFIG_DIR") == "" {
		// Isolate explicit test/flag configurations from user's global ~/.config/tether.
		r.keyPool = NewKeyPoolWithCandidates(engine, client, []KeyCandidate{{
			Provider: resolved.Provider,
			Entry: ProviderKeyEntry{
				Label:    resolved.Provider,
				APIKey:   resolved.APIKey,
				BaseURL:  resolved.BaseURL,
				Model:    resolved.Model,
				Priority: 1,
				Enabled:  true,
			},
		}})
	} else {
		r.keyPool = NewKeyPool(engine, client)
		if resolved.APIKey != "" && !poolHasKey(r.keyPool, resolved.Provider, resolved.APIKey) {
			cands := append([]KeyCandidate{{
				Provider: resolved.Provider,
				Entry: ProviderKeyEntry{
					Label:    resolved.Provider,
					APIKey:   resolved.APIKey,
					BaseURL:  resolved.BaseURL,
					Model:    resolved.Model,
					Priority: 1,
					Enabled:  true,
				},
			}}, r.keyPool.Candidates()...)
			r.keyPool = NewKeyPoolWithCandidates(engine, client, cands)
		}
	}

	if r.keyPool != nil && r.keyPool.Len() > 0 {
		if cand, ok := r.keyPool.SelectKey(context.Background(), r.cfg.Provider, r.cfg.Model); ok {
			r.applyKeyCandidate(cand, !explicit)
		}
	}
	return r
}

func poolHasKey(pool *KeyPool, provider, apiKey string) bool {
	if pool == nil {
		return false
	}
	for _, c := range pool.Candidates() {
		if strings.EqualFold(c.Provider, provider) && c.Entry.APIKey == apiKey {
			return true
		}
	}
	return false
}

// SetKeyPool attaches a custom KeyPool and selects/pins its initial key.
func (r *Runner) SetKeyPool(pool *KeyPool) {
	r.mu.Lock()
	r.keyPool = pool
	r.pinnedKeyID = ""
	prov, model := r.cfg.Provider, r.cfg.Model
	r.mu.Unlock()
	if pool != nil && pool.Len() > 0 {
		if cand, ok := pool.SelectKey(context.Background(), prov, model); ok {
			r.applyKeyCandidate(cand, true)
		}
	}
}

// SetExternalMCP replaces the optional external MCP client manager.
// Already-frozen session tools remain immutable to preserve KV-cache stability.
func (r *Runner) SetExternalMCP(mgr *mcp.ClientManager) {
	r.mu.Lock()
	r.extMCP = mgr
	r.mu.Unlock()
}

// ExternalMCP returns the Runner's external MCP client manager.
func (r *Runner) ExternalMCP() *mcp.ClientManager {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.extMCP
}

// Close releases active Crew leases and shuts down external MCP connections cleanly.
func (r *Runner) Close() error {
	if r == nil {
		return nil
	}
	r.CloseAllCrews()
	r.mu.RLock()
	depth := r.depth
	mgr := r.extMCP
	r.mu.RUnlock()
	if depth == 0 && mgr != nil {
		return mgr.Close()
	}
	return nil
}

// KeyPool returns the active KeyPool attached to the runner.
func (r *Runner) KeyPool() *KeyPool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.keyPool
}

// PinnedKeyID returns the currently pinned session key ID.
func (r *Runner) PinnedKeyID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.pinnedKeyID
}

// Engine returns the Runner's attached substrate Engine.
func (r *Runner) Engine() *core.Engine {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.engine
}

// ContextWindowTokens returns the active configured context window budget.
func (r *Runner) ContextWindowTokens() int {
	if r == nil {
		return defaultContextWindowTokens
	}
	return r.condenserConfig().ContextWindowTokens
}

// ReloadKeyPool reloads credentials.json into the Runner's KeyPool and applies
// an initial healthy key if the active key is unset or no longer present.
func (r *Runner) ReloadKeyPool() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	pool := r.keyPool
	eng := r.engine
	client := r.client
	prov := r.cfg.Provider
	model := r.cfg.Model
	if pool == nil {
		pool = NewKeyPool(eng, client)
		r.keyPool = pool
	}
	r.mu.Unlock()

	if err := pool.ReloadFromDisk(); err != nil {
		return err
	}
	if cand, ok := pool.SelectKey(context.Background(), prov, model); ok {
		r.applyKeyCandidate(cand, false)
	} else {
		r.mu.Lock()
		r.cfg.APIKey = loadSavedKey(prov)
		r.mu.Unlock()
	}
	return nil
}

// ProviderPresetInfo exposes non-secret provider metadata and curated models.
type ProviderPresetInfo struct {
	Name         string      `json:"name"`
	BaseURL      string      `json:"base_url"`
	DefaultModel string      `json:"default_model"`
	EnvKey       string      `json:"env_key,omitempty"`
	Models       []ui.Choice `json:"models,omitempty"`
}

// ListProviderPresets returns all built-in provider presets in stable order.
func ListProviderPresets() []ProviderPresetInfo {
	names := providerNames()
	out := make([]ProviderPresetInfo, 0, len(names))
	for _, name := range names {
		p := presets[name]
		out = append(out, ProviderPresetInfo{
			Name:         name,
			BaseURL:      p.BaseURL,
			DefaultModel: p.DefaultModel,
			EnvKey:       p.EnvKey,
			Models:       modelsFor(name),
		})
	}
	return out
}

// SetOnEvent installs an optional structured event observer (e.g. for keys.switched, stream.retry_reset).
func (r *Runner) SetOnEvent(fn func(kind, detail string)) {
	r.mu.Lock()
	r.onEvent = fn
	r.mu.Unlock()
}

func (r *Runner) hasOnEvent() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	ok := r.onEvent != nil
	r.mu.RUnlock()
	return ok
}

func (r *Runner) notifyEvent(kind, detail string) {
	if r == nil {
		return
	}
	r.mu.RLock()
	cb := r.onEvent
	r.mu.RUnlock()
	if cb != nil {
		cb(kind, detail)
	}
}

func (r *Runner) snapshotCallConfig() callSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	idle := r.cfg.StreamIdleTimeout
	if idle <= 0 {
		idle = defaultStreamIdleTimeout
	}
	return callSnapshot{
		provider:    r.cfg.Provider,
		baseURL:     r.cfg.BaseURL,
		apiKey:      r.cfg.APIKey,
		model:       r.cfg.Model,
		pinnedKeyID: r.pinnedKeyID,
		idleTimeout: idle,
	}
}

func (r *Runner) effectiveThreadIDLocked() string {
	if r.threadID != "" {
		return r.threadID
	}
	if r.engine != nil {
		return r.engine.SessionID
	}
	return ""
}

// ThreadID returns the active conversation thread ID.
func (r *Runner) ThreadID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.effectiveThreadIDLocked()
}

// SetThreadID switches the active thread ID without mutating history.
func (r *Runner) SetThreadID(threadID string) {
	r.mu.Lock()
	r.threadID = strings.TrimSpace(threadID)
	r.mu.Unlock()
}

// LoadThread loads persisted conversation events for threadID into r.history
// while preserving the frozen Tier 0-3 session head.
func (r *Runner) LoadThread(threadID string) error {
	if r.engine == nil {
		return fmt.Errorf("runner has no active engine")
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return fmt.Errorf("thread_id required")
	}
	if _, err := r.engine.GetThread(threadID); err != nil {
		return err
	}
	events, err := r.engine.ListThreadEventsForLoad(threadID, r.crewID, 500)
	if err != nil {
		return err
	}

	r.ClearHistory()
	r.SetThreadID(threadID)
	head, _ := r.ensureFrozenHead()

	// Find the latest condensation event so we load the condensed summary + subsequent events.
	startIdx := 0
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Role == "condensation" {
			startIdx = i
			break
		}
	}

	restored := []chatMessage{{Role: "system", Content: head}}
	for _, ev := range events[startIdx:] {
		if ev.Role == "condensation" {
			var p struct {
				Summary string `json:"summary"`
			}
			if json.Unmarshal([]byte(ev.PayloadJSON), &p) == nil && p.Summary != "" {
				restored = append(restored, chatMessage{
					Role:    "user",
					Content: "[Episodic Condensation Summary]\n" + p.Summary,
				})
			}
			continue
		}
		var msg chatMessage
		if json.Unmarshal([]byte(ev.PayloadJSON), &msg) == nil && msg.Role != "" {
			if len(restored) == 1 && msg.Role == "tool" {
				continue
			}
			restored = append(restored, msg)
		}
	}
	if len(restored) > 1 {
		r.mu.Lock()
		r.history = restored
		r.mu.Unlock()
	}
	return nil
}

func (r *Runner) persistMessageEvent(role, usageID string, msg chatMessage) {
	r.mu.RLock()
	eng := r.engine
	threadID := r.effectiveThreadIDLocked()
	crewID := r.crewID
	r.mu.RUnlock()
	if eng == nil || threadID == "" {
		return
	}
	if usageID == "" {
		usageID = "main"
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return
	}
	_, _ = eng.AppendThreadEvent(core.ConversationEvent{
		ThreadID:    threadID,
		CrewID:      crewID,
		Role:        role,
		UsageID:     usageID,
		PayloadJSON: string(payload),
		TokenEst:    EstimateMessagesTokens([]chatMessage{msg}),
	})
}

func (r *Runner) recordCallUsageWithID(usageID string, u apiUsage) {
	ctxWin := r.condenserConfig().ContextWindowTokens
	r.mu.Lock()
	if u.ttft > 0 {
		r.lastTTFT = u.ttft
	}
	if u.cacheHit > 0 || u.cacheMiss > 0 {
		r.lastCacheHitTokens = u.cacheHit
		r.lastCacheMissTokens = u.cacheMiss
	}
	tracker := r.usageTracker
	r.mu.Unlock()

	if tracker != nil {
		tracker.Record(usageID, u, ctxWin)
	}
}

func (r *Runner) outWriter() io.Writer {
	r.mu.RLock()
	out := r.cfg.Out
	r.mu.RUnlock()
	return out
}

func (r *Runner) writef(out io.Writer, format string, args ...any) {
	if out == nil {
		out = r.outWriter()
	}
	if out == nil {
		return
	}
	r.outMu.Lock()
	defer r.outMu.Unlock()
	fmt.Fprintf(out, format, args...)
}

func (r *Runner) emitNotice(kind, detail string, out io.Writer, onDelta func(kind, text string)) {
	switch kind {
	case "keys.switched":
		r.writef(out, "🔑 [keys.switched] %s\n", detail)
	case "stream.retry_reset":
		r.writef(out, "↻ [stream.retry_reset] %s\n", detail)
	case "stuck_detection":
		r.writef(out, "🛑 [stuck_detection] %s\n", detail)
	default:
		r.writef(out, "ℹ [%s] %s\n", kind, detail)
	}
	emitDelta(onDelta, kind, detail)
	r.mu.RLock()
	cb := r.onEvent
	r.mu.RUnlock()
	if cb != nil {
		cb(kind, detail)
	}
}

func (r *Runner) applyKeyCandidate(cand KeyCandidate, updateModelAndURL bool) {
	r.mu.Lock()
	prevProvider := r.cfg.Provider
	r.cfg.Provider = cand.Provider
	r.cfg.APIKey = cand.Entry.APIKey
	if updateModelAndURL || cand.Entry.BaseURL != "" || !strings.EqualFold(prevProvider, cand.Provider) || r.cfg.BaseURL == "" {
		if effURL := cand.EffectiveBaseURL(); effURL != "" {
			r.cfg.BaseURL = effURL
		}
	}
	pool := r.keyPool
	modelBlocked := pool != nil && r.cfg.Model != "" && pool.IsCapabilityBlocked(cand.Entry.ID, r.cfg.Model)
	if !strings.EqualFold(prevProvider, cand.Provider) || cand.Entry.Model != "" || r.cfg.Model == "" || modelBlocked {
		if effModel := cand.EffectiveModel(); effModel != "" {
			r.cfg.Model = effModel
		}
	}
	r.pinnedKeyID = cand.Entry.ID
	r.mu.Unlock()

	if pool != nil {
		pool.SetPinnedKeyID(cand.Entry.ID)
	}
}

// bootstrapSubstrate ensures project settings and FTS5 Markdown docs KB are populated on first launch
func bootstrapSubstrate(engine *core.Engine) {
	if engine == nil {
		return
	}
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
	Prefix           bool       `json:"prefix,omitempty"`
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
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// undo restores the files captured during a turn. When turn is empty the most
// recently snapshotted turn is used. It reports progress on the REPL output and
// never returns an error: an undo that cannot run is a message, not a crash.
func (r *Runner) undo(turn string) {
	r.undoTo(turn, r.outWriter())
}

func (r *Runner) undoTo(turn string, out io.Writer) {
	store := snapshot.New(r.engine.Workspace)

	if turn == "" {
		last, err := store.LastTurn()
		if err != nil {
			r.writef(out, "✖ Could not read snapshots: %v\n\n", err)
			return
		}
		if last == "" {
			r.writef(out, "ℹ Nothing to undo — no snapshot recorded yet.\n")
			r.writef(out, "  Snapshots are taken before each file write, so run the agent first.\n\n")
			return
		}
		turn = last
	}

	restored, err := store.Restore(turn, r.engine.Workspace)
	if len(restored) == 0 && err == nil {
		r.writef(out, "ℹ Turn %s had no captured files.\n\n", ui.Bold(turn))
		return
	}
	for _, path := range restored {
		r.writef(out, "↩ restored %s\n", path)
	}
	if len(restored) > 0 {
		r.QueueTailNotice(fmt.Sprintf("User ran /undo for turn %s; restored %d file(s): %s", turn, len(restored), strings.Join(restored, ", ")))
	}
	if err != nil {
		r.writef(out, "⚠ Undo partially failed for turn %s: %v\n", ui.Bold(turn), err)
		r.writef(out, "  Files listed above were restored; the rest were left untouched.\n\n")
		return
	}
	r.writef(out, "✓ Undo complete — %d file(s) restored from turn %s.\n\n", len(restored), ui.Bold(turn))
}

func (r *Runner) ensureActiveKey(ctx context.Context) error {
	r.mu.RLock()
	pool := r.keyPool
	snap := callSnapshot{
		provider:    r.cfg.Provider,
		baseURL:     r.cfg.BaseURL,
		apiKey:      r.cfg.APIKey,
		model:       r.cfg.Model,
		pinnedKeyID: r.pinnedKeyID,
	}
	r.mu.RUnlock()

	if pool != nil && pool.Len() > 0 && (snap.pinnedKeyID == "" || (snap.apiKey == "" && !strings.EqualFold(snap.provider, "ollama"))) {
		if cand, ok := pool.SelectKey(ctx, snap.provider, snap.model); ok {
			r.applyKeyCandidate(cand, snap.baseURL == "")
			snap = r.snapshotCallConfig()
		}
	}
	if !strings.EqualFold(snap.provider, "ollama") && strings.TrimSpace(snap.apiKey) == "" {
		return fmt.Errorf("missing API key for provider %q (set %s or TETHER_API_KEY)", snap.provider, presets[snap.provider].EnvKey)
	}
	return nil
}

func (r *Runner) nextTurnID() string {
	seq := r.turnSeq.Add(1)
	ts := time.Now().UTC().Format("20060102T150405.000")
	r.mu.RLock()
	crewID := r.crewID
	r.mu.RUnlock()
	if crewID != "" {
		return fmt.Sprintf("%s-%s-%d", crewID, ts, seq)
	}
	return fmt.Sprintf("turn-%s-%d", ts, seq)
}

// Run executes a prompt through the immutable Tier 0-3 + Append-Only LLM Tool Loop.
func (r *Runner) Run(ctx context.Context, prompt string) (string, error) {
	return r.runWithOptions(ctx, prompt, nil, nil)
}

func (r *Runner) runWithOptions(ctx context.Context, prompt string, out io.Writer, onDelta func(kind, text string)) (string, error) {
	if err := r.ensureActiveKey(ctx); err != nil {
		return "", err
	}

	turnID := r.nextTurnID()
	head, apiTools := r.ensureFrozenHead()
	userContent, staged := r.prepareTurnUserContent(prompt)

	r.mu.Lock()
	r.lastUserActivity = time.Now()
	effectiveOut := out
	if effectiveOut == nil {
		effectiveOut = r.cfg.Out
	}
	maxSteps := r.cfg.MaxSteps
	snapProv := r.cfg.Provider
	crewID := r.crewID
	usageID := r.usageID
	if usageID == "" {
		usageID = "main"
	}
	historyCopy := make([]chatMessage, len(r.history))
	copy(historyCopy, r.history)
	r.mu.Unlock()

	if r.mcp != nil && r.engine != nil {
		r.mcp.SetSnapshotContext(snapshot.New(r.engine.Workspace), turnID, crewID)
	}

	userMsg := chatMessage{Role: "user", Content: userContent}
	var messages []chatMessage
	if len(historyCopy) == 0 {
		messages = []chatMessage{
			{Role: "system", Content: head},
			userMsg,
		}
	} else {
		// Strict Append-Only: NEVER overwrite messages[0] or mutate prior history entries.
		messages = append(historyCopy, userMsg)
	}

	if should, _ := r.ShouldCondense(messages); should {
		if condensed, res, cerr := r.condenseMessages(ctx, messages, false); cerr == nil && res.Condensed {
			messages = condensed
			r.mu.Lock()
			if len(messages) > 1 {
				r.history = append([]chatMessage(nil), messages[:len(messages)-1]...)
			}
			r.invalidateCondensedFileHashesLocked(messages)
			r.mu.Unlock()
		}
	}

	var lastCallSig, lastErrSig, lastStepSig string
	callStreak, errStreak, stepStreak := 0, 0, 0
	step0Committed := false

	// 2. Agentic Tool-Execution Loop (strictly append-only; no per-step sliding compaction)
	for step := 0; step < maxSteps; step++ {
		if step > 0 {
			if should, _ := r.ShouldCondense(messages); should {
				if condensed, res, cerr := r.condenseMessages(ctx, messages, false); cerr == nil && res.Condensed {
					messages = condensed
					r.mu.Lock()
					r.invalidateCondensedFileHashesLocked(messages)
					r.mu.Unlock()
				}
			}
		}

		msg, finishReason, err := r.callAPIWithFailoverUsageID(ctx, usageID, messages, apiTools, effectiveOut, onDelta)
		if err != nil {
			return "", err
		}
		if !step0Committed {
			step0Committed = true
			r.commitTurnDelta(staged)
			if r.engine != nil {
				_ = r.engine.AddContext(snapProv, "user", prompt)
			}
			r.persistMessageEvent("user", usageID, userMsg)
		}

		// Handle finish_reason == "length" prose continuation on supported endpoints.
		curSnap := r.snapshotCallConfig()
		if finishReason == "length" && len(msg.ToolCalls) == 0 && supportsPrefixContinuation(curSnap.provider) && strings.TrimSpace(msg.Content) != "" {
			for range defaultMaxLengthContinue {
				contMsgs := append(append([]chatMessage{}, messages...), chatMessage{
					Role:    "assistant",
					Content: msg.Content,
					Prefix:  true,
				})
				nextMsg, nextFinish, cerr := r.callAPIWithFailoverUsageID(ctx, usageID, contMsgs, apiTools, effectiveOut, onDelta)
				if cerr != nil {
					return "", cerr
				}
				if nextMsg.ReasoningContent != "" {
					msg.ReasoningContent += nextMsg.ReasoningContent
				}
				msg.Content += nextMsg.Content
				if len(nextMsg.ToolCalls) > 0 {
					msg.ToolCalls = nextMsg.ToolCalls
					finishReason = nextFinish
					break
				}
				finishReason = nextFinish
				if finishReason != "length" {
					break
				}
			}
		}

		// While streaming, reasoning and intermediate text were already delivered live
		// through onDelta; printing them again would duplicate them in the transcript.
		if msg.ReasoningContent != "" && onDelta == nil {
			r.writef(effectiveOut, "💭 [reasoning] %s\n", strings.TrimSpace(msg.ReasoningContent))
		}
		if len(msg.ToolCalls) > 0 && strings.TrimSpace(msg.Content) != "" && onDelta == nil {
			r.writef(effectiveOut, "\n%s\n", strings.TrimSpace(msg.Content))
		}
		messages = append(messages, msg)
		r.persistMessageEvent("assistant", usageID, msg)

		curSnap = r.snapshotCallConfig()
		if len(msg.ToolCalls) == 0 {
			r.mu.Lock()
			r.history = messages
			r.mu.Unlock()
			if r.engine != nil {
				_ = r.engine.AddContext(curSnap.provider, "assistant", msg.Content)
			}
			return msg.Content, nil
		}

		// Handle finish_reason == "length" inside partial tool_calls: do NOT prefill partial JSON;
		// emit an explicit split-chunk nudge so the model retries with smaller tool calls.
		if finishReason == "length" {
			r.writef(effectiveOut, "⚠ [length-limit] tool_calls truncated (finish_reason=length); requesting split-chunk retry\n")
			msgRef := &messages[len(messages)-1]
			for i := range msgRef.ToolCalls {
				if msgRef.ToolCalls[i].ID == "" {
					msgRef.ToolCalls[i].ID = fmt.Sprintf("call_trunc_%d_%d", step, i)
				}
				toolMsg := chatMessage{
					Role:       "tool",
					ToolCallID: msgRef.ToolCalls[i].ID,
					Content: fmt.Sprintf(
						"TRUNCATED TOOL CALL (finish_reason=length) for %s: output hit max_tokens inside tool_calls arguments. Do NOT prefill partial JSON and do NOT repeat a monolithic payload; split into smaller incremental chunks and retry.",
						msgRef.ToolCalls[i].Function.Name,
					),
				}
				messages = append(messages, toolMsg)
				r.persistMessageEvent("tool", usageID, toolMsg)
			}
			continue
		}

		outcomes := r.executeToolCallsStatusWithCtx(ctx, msg.ToolCalls, effectiveOut)
		stepSigs := make([]string, len(msg.ToolCalls))
		var stuckTool string
		for i, tc := range msg.ToolCalls {
			toolMsg := chatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    outcomes[i].text,
			}
			messages = append(messages, toolMsg)
			r.persistMessageEvent("tool", usageID, toolMsg)
			sig := toolCallSignature(tc.Function.Name, tc.Function.Arguments, outcomes[i].text)
			stepSigs[i] = sig
			if sig == lastCallSig {
				callStreak++
			} else {
				lastCallSig = sig
				callStreak = 1
			}
			if outcomes[i].isErr {
				esig := toolCallErrSignature(tc.Function.Name, tc.Function.Arguments)
				if esig == lastErrSig {
					errStreak++
				} else {
					lastErrSig = esig
					errStreak = 1
				}
			} else {
				lastErrSig = ""
				errStreak = 0
			}
			if stuckTool == "" && (callStreak >= defaultStuckRepeatLimit || errStreak >= defaultStuckRepeatLimit) {
				stuckTool = tc.Function.Name
			}
		}
		if stuckTool != "" {
			r.mu.Lock()
			r.history = messages
			r.mu.Unlock()
			r.emitNotice("stuck_detection", fmt.Sprintf("stopped after %d identical repetitions of %s", defaultStuckRepeatLimit, stuckTool), effectiveOut, onDelta)
			return "", fmt.Errorf("stuck_detection: aborted after %d identical repetitions of %s", defaultStuckRepeatLimit, stuckTool)
		}

		stepSig := strings.Join(stepSigs, "|")
		if stepSig != "" && stepSig == lastStepSig {
			stepStreak++
		} else {
			lastStepSig = stepSig
			stepStreak = 1
		}
		if stepStreak >= defaultStuckRepeatLimit {
			r.mu.Lock()
			r.history = messages
			r.mu.Unlock()
			r.emitNotice("stuck_detection", fmt.Sprintf("stopped after %d identical tool-batch repetitions", stepStreak), effectiveOut, onDelta)
			return "", fmt.Errorf("stuck_detection: aborted after %d identical tool-batch repetitions", stepStreak)
		}
	}

	// Persist the full working history so typing "continue" actually resumes the
	// same task. Compaction is deliberately skipped here: dropping the tool
	// results the model had just produced was what turned a pause into a restart.
	r.mu.Lock()
	r.history = messages
	prov := r.cfg.Provider
	r.mu.Unlock()
	if r.engine != nil {
		_ = r.engine.AddContext(prov, "assistant", fmt.Sprintf("[paused after %d tool steps — type 'continue' to resume]", maxSteps))
	}
	return "", fmt.Errorf("paused after %d steps (type 'continue' or 'lanjutkan' to resume)", maxSteps)
}

func supportsPrefixContinuation(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "deepseek")
}

func toolCallSignature(name, rawArgs, result string) string {
	argSum := sha256.Sum256([]byte(strings.TrimSpace(rawArgs)))
	resSum := sha256.Sum256([]byte(strings.TrimSpace(result)))
	return fmt.Sprintf("%s:%x:%x", name, argSum[:8], resSum[:8])
}

func toolCallErrSignature(name, rawArgs string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(rawArgs)))
	return fmt.Sprintf("err:%s:%x", name, sum[:8])
}

type toolOutcome struct {
	text  string
	isErr bool
}

func isReadOnlyToolCall(tc toolCall) bool {
	switch tc.Function.Name {
	case "read_file", "glob", "grep", "kb_search":
		return true
	case "todo_manage":
		var p struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(tc.Function.Arguments)), &p); err == nil {
			return strings.EqualFold(strings.TrimSpace(p.Action), "list")
		}
		return false
	default:
		return false
	}
}

// executeToolCallsStatus executes contiguous read-only tool calls concurrently (bounded <= 4)
// while preserving exact output order and strictly serializing mutating tools.
func (r *Runner) executeToolCallsStatus(calls []toolCall, out io.Writer) []toolOutcome {
	return r.executeToolCallsStatusWithCtx(context.Background(), calls, out)
}

func (r *Runner) executeToolCallsStatusWithCtx(ctx context.Context, calls []toolCall, out io.Writer) []toolOutcome {
	var execFn func(toolCall) toolOutcome
	if r.execTool != nil {
		execFn = func(tc toolCall) toolOutcome {
			return toolOutcome{text: r.execTool(tc)}
		}
	} else {
		execFn = func(tc toolCall) toolOutcome {
			txt, isErr := r.executeGuardedToolStatusWithCtx(ctx, tc, out)
			return toolOutcome{text: txt, isErr: isErr}
		}
	}
	results := make([]toolOutcome, len(calls))
	for i := 0; i < len(calls); {
		if !isReadOnlyToolCall(calls[i]) {
			results[i] = execFn(calls[i])
			i++
			continue
		}
		j := i + 1
		for j < len(calls) && isReadOnlyToolCall(calls[j]) {
			j++
		}
		if j-i == 1 {
			results[i] = execFn(calls[i])
		} else {
			sem := make(chan struct{}, defaultMaxParallelReadTools)
			var wg sync.WaitGroup
			for k := i; k < j; k++ {
				wg.Add(1)
				go func(idx int, tc toolCall) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					results[idx] = execFn(tc)
				}(k, calls[k])
			}
			wg.Wait()
		}
		i = j
	}
	return results
}

func (r *Runner) executeGuardedTool(tc toolCall) string {
	res, _ := r.executeGuardedToolStatus(tc, r.outWriter())
	return res
}

func (r *Runner) executeGuardedToolStatus(tc toolCall, out io.Writer) (string, bool) {
	return r.executeGuardedToolStatusWithCtx(context.Background(), tc, out)
}

func (r *Runner) executeGuardedToolStatusWithCtx(ctx context.Context, tc toolCall, out io.Writer) (string, bool) {
	var argMap map[string]any
	rawArgs := strings.TrimSpace(tc.Function.Arguments)
	if rawArgs != "" && rawArgs != "{}" {
		if err := json.Unmarshal([]byte(rawArgs), &argMap); err != nil {
			// DeepSeek API best practice: strictly validate tool call JSON before execution
			// so hallucinated/truncated JSON is immediately reported back for self-correction.
			return fmt.Sprintf("invalid tool arguments JSON for %s: %v. Please retry with valid JSON.", tc.Function.Name, err), true
		}
	}

	r.mu.RLock()
	roCrew := r.readOnlyCrew
	r.mu.RUnlock()
	if roCrew && r.isMutatingRunnerToolForRunner(tc.Function.Name, argMap) {
		return fmt.Sprintf("DENIED BY TETHER GUARD: read-only crew cannot invoke %s", tc.Function.Name), true
	}

	if isCrewToolName(tc.Function.Name) {
		r.printToolAction(out, tc.Function.Name, argMap)
		return r.executeCrewTool(ctx, tc.Function.Name, rawArgs)
	}

	r.mu.RLock()
	agentType := r.agentType
	r.mu.RUnlock()

	hookIn := hooks.HookInput{
		ToolName:  tc.Function.Name,
		AgentType: agentType,
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
		r.writef(out, "🛑 [Tether Guard Blocked %s (%s)] %s\n", tc.Function.Name, agentType, preOut.Reason)
		return "DENIED BY TETHER GUARD: " + preOut.Reason, true
	}

	if strings.HasPrefix(tc.Function.Name, "mcp_") {
		extTool, extMgr, ok := r.resolveFrozenExternalTool(tc.Function.Name)
		if !ok || extMgr == nil {
			return fmt.Sprintf("unknown or disabled external MCP tool: %s", tc.Function.Name), true
		}
		readOnlySemantic := extTool.ReadOnly
		if allowed, authErr := r.mcp.AuthorizeExternalTool(tc.Function.Name, extTool.ServerName, extTool.OriginalName, readOnlySemantic, json.RawMessage(rawArgs)); !allowed {
			r.writef(out, "🛑 [Tether Guard Blocked %s (%s)] %s\n", tc.Function.Name, agentType, authErr)
			return authErr, true
		}
		r.printToolAction(out, tc.Function.Name, argMap)
		res, isErr := extMgr.CallTool(ctx, tc.Function.Name, argMap)
		_, _ = r.hooks.Handle("PostToolUse", mustJSONReader(hookIn))
		return res, isErr
	}

	r.printToolAction(out, tc.Function.Name, argMap)
	res, isErr := r.mcp.CallTool(tc.Function.Name, json.RawMessage(tc.Function.Arguments))
	if tc.Function.Name == "edit_file" || tc.Function.Name == "write_file" {
		if isErr {
			r.writef(out, "  ✖ %s\n", res)
		} else {
			r.writef(out, "  ↳ %s\n", res)
		}
	}

	// PostToolUse Auditor
	_, _ = r.hooks.Handle("PostToolUse", mustJSONReader(hookIn))
	return res, isErr
}

func (r *Runner) resolveFrozenExternalTool(name string) (mcp.ExternalTool, *mcp.ClientManager, bool) {
	r.mu.RLock()
	mgr := r.extMCP
	hasFrozen := len(r.frozenTools) > 0
	extTool, ok := r.frozenExtTools[name]
	r.mu.RUnlock()
	if ok && mgr != nil {
		return extTool, mgr, true
	}
	if !hasFrozen && mgr != nil {
		if t, found := mgr.LookupTool(name); found {
			return t, mgr, true
		}
	}
	return mcp.ExternalTool{}, nil, false
}

func (r *Runner) isReadOnlyExternalTool(name string) bool {
	if !strings.HasPrefix(name, "mcp_") {
		return false
	}
	if extTool, _, ok := r.resolveFrozenExternalTool(name); ok {
		return extTool.ReadOnly
	}
	return false
}

func (r *Runner) isMutatingRunnerToolForRunner(name string, args map[string]any) bool {
	if strings.HasPrefix(name, "mcp_") {
		return !r.isReadOnlyExternalTool(name)
	}
	return isMutatingRunnerTool(name, args)
}

func isMutatingRunnerTool(name string, args map[string]any) bool {
	switch name {
	case "write_file", "edit_file", "mode_set", "decision_add":
		return true
	case "todo_manage":
		action, _ := args["action"].(string)
		return !strings.EqualFold(strings.TrimSpace(action), "list")
	case "run_command":
		cmd, _ := args["command"].(string)
		ok, _ := safety.IsReadOnlyCommand(cmd)
		return !ok
	default:
		return false
	}
}

func (r *Runner) printToolAction(out io.Writer, name string, args map[string]any) {
	// File writes are labelled explicitly. A diff printed here reflects a
	// change that is about to be applied automatically; it is a log, not a
	// request for consent.
	if out == nil {
		return
	}
	r.outMu.Lock()
	defer r.outMu.Unlock()
	switch name {
	case "edit_file":
		path, _ := args["path"].(string)
		target, _ := args["target"].(string)
		replacement, _ := args["replacement"].(string)
		fmt.Fprintf(out, "%s %s\n", ui.Dim("WRITE"), ui.Path(path))
		for _, line := range clipLines(target, 12) {
			fmt.Fprintf(out, "  %s\n", ui.DiffDel("- "+line))
		}
		for _, line := range clipLines(replacement, 12) {
			fmt.Fprintf(out, "  %s\n", ui.DiffAdd("+ "+line))
		}
	case "write_file":
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
		fmt.Fprintf(out, "%s %s %s\n",
			ui.Dim("WRITE"), ui.Path(path), ui.Dim(fmt.Sprintf("(%d lines)", len(lines))))
		for _, line := range clipLines(content, 8) {
			fmt.Fprintf(out, "  %s\n", ui.DiffAdd("+ "+line))
		}
	case "read_file":
		path, _ := args["path"].(string)
		if s, ok := args["start_line"].(float64); ok && s > 0 {
			e, _ := args["end_line"].(float64)
			fmt.Fprintf(out, "🔧 [read_file] %s (lines %.0f-%.0f)\n", path, s, e)
		} else {
			fmt.Fprintf(out, "🔧 [read_file] %s\n", path)
		}
	case "glob":
		pat, _ := args["pattern"].(string)
		p, _ := args["path"].(string)
		if p != "" && p != "." {
			fmt.Fprintf(out, "🔧 [glob] %s (in %s)\n", pat, p)
		} else {
			fmt.Fprintf(out, "🔧 [glob] %s\n", pat)
		}
	case "grep":
		pat, _ := args["pattern"].(string)
		if pat == "" {
			pat, _ = args["query"].(string)
		}
		p, _ := args["path"].(string)
		if p != "" && p != "." {
			fmt.Fprintf(out, "🔧 [grep] %q in %s\n", pat, p)
		} else {
			fmt.Fprintf(out, "🔧 [grep] %q\n", pat)
		}
	case "run_command":
		cmd, _ := args["command"].(string)
		fmt.Fprintf(out, "🔧 [run_command] $ %s\n", cmd)
	case "kb_search":
		q, _ := args["query"].(string)
		fmt.Fprintf(out, "🔧 [kb_search] %q\n", q)
	case "todo_manage":
		act, _ := args["action"].(string)
		d, _ := args["description"].(string)
		id, _ := args["id"].(float64)
		switch {
		case d != "":
			fmt.Fprintf(out, "🔧 [todo_manage] %s: %s\n", act, d)
		case id > 0:
			fmt.Fprintf(out, "🔧 [todo_manage] %s #%.0f\n", act, id)
		default:
			fmt.Fprintf(out, "🔧 [todo_manage] %s\n", act)
		}
	case "mode_set":
		m, _ := args["mode"].(string)
		fmt.Fprintf(out, "🔧 [mode_set] %s\n", m)
	case "decision_add":
		t, _ := args["topic"].(string)
		d, _ := args["decision"].(string)
		fmt.Fprintf(out, "🔧 [decision_add] [%s] %s\n", t, d)
	default:
		fmt.Fprintf(out, "🔧 [tool:%s]\n", name)
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

func isCapabilityMismatch(aerr *apiError) bool {
	if aerr == nil {
		return false
	}
	if aerr.StatusCode == 404 {
		return true
	}
	if aerr.StatusCode == 400 {
		low := strings.ToLower(aerr.Body)
		for _, cue := range []string{"model", "does not exist", "not found", "unsupported", "tool", "function", "reasoning_content"} {
			if strings.Contains(low, cue) {
				return true
			}
		}
	}
	return false
}

func (r *Runner) keyLabel(pool *KeyPool, keyID, fallbackProvider string) string {
	if pool != nil && keyID != "" {
		if cand, ok := pool.CandidateByID(keyID); ok {
			return cand.Entry.Label
		}
	}
	return fallbackProvider
}

func (r *Runner) callAPIWithFailoverUsageID(ctx context.Context, usageID string, messages []chatMessage, tools []openAITool, out io.Writer, onDelta func(kind, text string)) (chatMessage, string, error) {
	pool := r.KeyPool()
	maxAttempts := 3
	if pool != nil && pool.Len()+2 > maxAttempts {
		maxAttempts = pool.Len() + 2
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if ctx.Err() != nil {
			if lastErr != nil {
				return chatMessage{}, "", lastErr
			}
			return chatMessage{}, "", ctx.Err()
		}

		snap := r.snapshotCallConfig()
		reqMsgs := SanitizeMessages(messages, SanitizeOptions{
			TargetProvider: snap.provider,
			TargetModel:    snap.model,
		})

		curKeyID := snap.pinnedKeyID
		curLabel := r.keyLabel(pool, curKeyID, snap.provider)

		callStart := time.Now()
		msg, finishReason, usage, err := r.callAPI(ctx, snap, reqMsgs, tools, onDelta)
		if err == nil {
			if usage.ttft <= 0 {
				usage.ttft = time.Since(callStart)
			}
			r.recordCallUsageWithID(usageID, usage)
			if pool != nil && curKeyID != "" {
				pool.RecordSuccess(curKeyID, snap.provider, curLabel, usage.ttft, usage.cacheHit, usage.cacheMiss)
			}
			return msg, finishReason, nil
		}
		lastErr = err

		if ctx.Err() != nil {
			return chatMessage{}, "", err
		}

		var aerr *apiError
		if !errors.As(err, &aerr) {
			return chatMessage{}, "", err
		}

		if pool != nil && curKeyID != "" {
			pool.RecordError(curKeyID, snap.provider, curLabel)
		}

		isTransient := aerr.IdleTimeout || aerr.StatusCode == 429 || aerr.StatusCode == 503 || aerr.StatusCode == 502 || aerr.StatusCode == 504
		isAuthQuota := aerr.StatusCode == 401 || aerr.StatusCode == 402 || aerr.StatusCode == 403
		isCapErr := isCapabilityMismatch(aerr)

		if !isTransient && !isAuthQuota && !isCapErr {
			return chatMessage{}, "", err
		}

		if pool == nil || pool.Len() == 0 {
			if aerr.IdleTimeout && attempt < maxAttempts-1 {
				r.emitNotice("stream.retry_reset", aerr.Error(), out, onDelta)
				continue
			}
			return chatMessage{}, "", err
		}

		switch {
		case isTransient:
			cd := aerr.RetryAfter
			if cd <= 0 {
				cd = pool.CooldownDefault()
			}
			pool.MarkCooldown(curKeyID, cd)
			// Prefer another enabled healthy key on the SAME provider first.
			if next, ok := pool.SelectFailoverKey(curKeyID, snap.provider, snap.model, true); ok && next.Entry.ID != curKeyID {
				r.emitNotice("stream.retry_reset", fmt.Sprintf("%s (retrying on %s/%s)", aerr.Error(), next.Provider, next.Entry.ID), out, onDelta)
				r.applyKeyCandidate(next, true)
				r.emitNotice("keys.switched", fmt.Sprintf("%s:%s -> %s:%s (transient %d)", snap.provider, curKeyID, next.Provider, next.Entry.ID, aerr.StatusCode), out, onDelta)
				continue
			}
			// No backup key available: honor brief bounded cooldown before retrying.
			if attempt < maxAttempts-1 {
				waitDur := min(cd, 2*time.Second)
				r.emitNotice("stream.retry_reset", fmt.Sprintf("%s (cooldown %s)", aerr.Error(), waitDur), out, onDelta)
				select {
				case <-time.After(waitDur):
				case <-ctx.Done():
					return chatMessage{}, "", ctx.Err()
				}
				continue
			}
			return chatMessage{}, "", err

		case isAuthQuota:
			pool.MarkUnhealthy(curKeyID, aerr.StatusCode, aerr.Error())
			next, ok := pool.SelectFailoverKey(curKeyID, snap.provider, snap.model, true)
			if !ok || next.Entry.ID == curKeyID {
				return chatMessage{}, "", err
			}
			if onDelta != nil {
				r.emitNotice("stream.retry_reset", fmt.Sprintf("resetting stream after HTTP %d on %s", aerr.StatusCode, curKeyID), out, onDelta)
			}
			r.applyKeyCandidate(next, true)
			r.emitNotice("keys.switched", fmt.Sprintf("%s:%s -> %s:%s (HTTP %d)", snap.provider, curKeyID, next.Provider, next.Entry.ID, aerr.StatusCode), out, onDelta)
			continue

		case isCapErr:
			pool.MarkCapabilityFailure(curKeyID, snap.model, aerr.Error())
			next, ok := pool.SelectFailoverKey(curKeyID, snap.provider, snap.model, true)
			if !ok || next.Entry.ID == curKeyID {
				return chatMessage{}, "", err
			}
			if onDelta != nil {
				r.emitNotice("stream.retry_reset", fmt.Sprintf("resetting stream after HTTP %d capability mismatch", aerr.StatusCode), out, onDelta)
			}
			r.applyKeyCandidate(next, true)
			r.emitNotice("keys.switched", fmt.Sprintf("%s:%s -> %s:%s (HTTP %d model=%s)", snap.provider, curKeyID, next.Provider, next.Entry.ID, aerr.StatusCode, snap.model), out, onDelta)
			continue
		}
	}

	return chatMessage{}, "", lastErr
}

func (r *Runner) prepareChatPayload(snap callSnapshot, messages []chatMessage, tools []openAITool, maxTokensOverride int, stream bool) (string, map[string]any) {
	useCacheCtrl := SupportsCacheControl(snap.provider, snap.baseURL, snap.model)
	payload := map[string]any{
		"model":    snap.model,
		"messages": buildWireMessages(messages, useCacheCtrl),
	}
	// DeepSeek API spec: omit "tools" completely when empty (sending "tools": null or [] is discouraged)
	if len(tools) > 0 {
		payload["tools"] = tools
	}

	targetURL := snap.baseURL
	if len(messages) > 0 && messages[len(messages)-1].Prefix && targetURL == "https://api.deepseek.com/chat/completions" {
		targetURL = "https://api.deepseek.com/beta/chat/completions"
	}

	// DeepSeek API Parameter Optimization:
	// - In Thinking Mode (deepseek-reasoner), temperature/top_p/penalties are unsupported and ignored,
	//   while max_tokens should be generous (8192) so CoT + tool call JSON never truncates.
	// - In Non-Thinking Mode (deepseek-chat), temperature=0.0 is recommended by DeepSeek for
	//   deterministic coding & function calling.
	modelLower := strings.ToLower(snap.model)
	isReasoner := strings.Contains(modelLower, "reasoner") || strings.Contains(modelLower, "r1")
	if strings.EqualFold(snap.provider, "deepseek") {
		if isReasoner {
			payload["max_tokens"] = 8192
		} else {
			payload["temperature"] = 0.0
			payload["max_tokens"] = 4096
		}
	}
	if maxTokensOverride > 0 {
		payload["max_tokens"] = maxTokensOverride
	}
	if stream {
		payload["stream"] = true
		payload["stream_options"] = map[string]any{"include_usage": true}
	} else if maxTokensOverride > 0 {
		payload["stream"] = false
	}
	return targetURL, payload
}

func (r *Runner) callAPI(ctx context.Context, snap callSnapshot, messages []chatMessage, tools []openAITool, onDelta func(kind, text string)) (chatMessage, string, apiUsage, error) {
	targetURL, payload := r.prepareChatPayload(snap, messages, tools, 0, onDelta != nil)
	if onDelta != nil {
		return r.streamRequest(ctx, snap, targetURL, payload, onDelta)
	}

	reqStart := time.Now()
	resp, err := r.doPostJSON(ctx, snap, targetURL, payload, false)
	if err != nil {
		return chatMessage{}, "", apiUsage{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return chatMessage{}, "", apiUsage{}, readAPIError(resp)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPICompletionBytes))
	if err != nil {
		return chatMessage{}, "", apiUsage{}, fmt.Errorf("read API response: %w", err)
	}
	msg, finishReason, usage, perr := parseBufferedCompletion(body, nil)
	usage.ttft = time.Since(reqStart)
	return msg, finishReason, usage, perr
}

// WarmCache sends a bounded 1-token prefix keepalive exclusively for the Main Rank #1 key,
// auto-disabling on HTTP 429 or user inactivity exceeding WarmCacheIdleMax (default 10m).
func (r *Runner) WarmCache(ctx context.Context) (bool, error) {
	r.mu.RLock()
	agentType := r.agentType
	snap := callSnapshot{
		provider:    r.cfg.Provider,
		baseURL:     r.cfg.BaseURL,
		apiKey:      r.cfg.APIKey,
		model:       r.cfg.Model,
		pinnedKeyID: r.pinnedKeyID,
		idleTimeout: r.cfg.StreamIdleTimeout,
	}
	lastAct := r.lastUserActivity
	maxPings := r.cfg.WarmCacheMaxPings
	idleMax := r.cfg.WarmCacheIdleMax
	pool := r.keyPool
	var msgs []chatMessage
	if len(r.history) >= 2 {
		msgs = []chatMessage{r.history[0], r.history[1]}
	} else {
		msgs = []chatMessage{
			{Role: "system", Content: "Tether warm cache keepalive."},
			{Role: "user", Content: "ping"},
		}
	}
	r.mu.RUnlock()

	if agentType != "main" {
		return false, nil
	}
	if maxPings <= 0 {
		maxPings = defaultWarmCacheMaxPings
	}
	if idleMax <= 0 {
		idleMax = defaultWarmCacheIdleMax
	}

	warmSnap := snap
	if pool != nil && pool.Len() > 0 {
		rank1, ok := pool.Rank1Candidate(snap.provider, snap.model)
		if !ok {
			return false, nil
		}
		if snap.pinnedKeyID != "" && rank1.Entry.ID != snap.pinnedKeyID {
			return false, nil
		}
		warmSnap.provider = rank1.Provider
		warmSnap.apiKey = rank1.Entry.APIKey
		if u := rank1.EffectiveBaseURL(); u != "" {
			warmSnap.baseURL = u
		}
		if m := rank1.EffectiveModel(); m != "" && (rank1.Entry.Model != "" || warmSnap.model == "") {
			warmSnap.model = m
		}
	}

	if !strings.EqualFold(warmSnap.provider, "ollama") && strings.TrimSpace(warmSnap.apiKey) == "" {
		return false, nil
	}

	r.warmCacheMu.Lock()
	if r.warmCacheDisabled || r.warmCachePings >= maxPings {
		r.warmCacheMu.Unlock()
		return false, nil
	}
	if !lastAct.IsZero() && time.Since(lastAct) > idleMax {
		r.warmCacheDisabled = true
		r.warmCacheMu.Unlock()
		return false, nil
	}
	r.warmCachePings++
	r.warmCacheMu.Unlock()

	msgs = SanitizeMessages(msgs, SanitizeOptions{TargetProvider: warmSnap.provider, TargetModel: warmSnap.model})
	targetURL, payload := r.prepareChatPayload(warmSnap, msgs, nil, 1, false)
	resp, err := r.doPostJSON(ctx, warmSnap, targetURL, payload, false)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 16*1024))

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode == http.StatusForbidden {
		r.warmCacheMu.Lock()
		r.warmCacheDisabled = true
		r.warmCacheMu.Unlock()
		return false, &apiError{
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Err:        fmt.Errorf("warm_cache disabled after HTTP %d", resp.StatusCode),
		}
	}
	if resp.StatusCode >= 400 {
		return false, fmt.Errorf("warm_cache HTTP %d", resp.StatusCode)
	}
	return true, nil
}

// StartWarmCacheLoop periodically runs WarmCache (default 295s) until stopped, disabled, or ctx ends.
func (r *Runner) StartWarmCacheLoop(ctx context.Context) (stop func()) {
	r.mu.RLock()
	interval := r.cfg.WarmCacheInterval
	maxPings := r.cfg.WarmCacheMaxPings
	r.mu.RUnlock()
	if interval <= 0 {
		interval = defaultWarmCacheInterval
	}
	if maxPings <= 0 {
		maxPings = defaultWarmCacheMaxPings
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				_, _ = r.WarmCache(loopCtx)
				r.warmCacheMu.Lock()
				disabled := r.warmCacheDisabled || r.warmCachePings >= maxPings
				r.warmCacheMu.Unlock()
				if disabled {
					return
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func mustJSONReader(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

// RunInteractive starts an Aider/Claude-Code style interactive REPL session
func (r *Runner) RunInteractive(ctx context.Context, in io.Reader) error {
	scanner := bufio.NewScanner(in)
	out := r.outWriter()

	stopWarm := r.StartWarmCacheLoop(ctx)
	defer stopWarm()
	defer r.CloseAllCrews()

	// Connect the approval gate to this terminal. Without it a blocked write is
	// only a message to the model, and the user is never asked. Hook and
	// JSON-RPC entry points never reach this line, so they keep the
	// refuse-with-instructions behaviour.
	r.mcp.SetAutoApprovePolicy(r.DefaultAutoApprovePolicy())
	r.mcp.SetAskPolicy(newAsker(out, scanner))

	snap := r.snapshotCallConfig()
	fmt.Fprintf(out, "⚓ Tether Interactive Agent (%s / %s | mode: %s)\n", snap.provider, snap.model, r.engine.GetMode())
	// Derived from the registry, not typed by hand. The hand-written line that
	// used to sit here had already drifted: it advertised /plan, /code, /model,
	// /provider, /key, /status and /exit, and omitted /quit, /clear and /undo,
	// all of which the REPL handled.
	fmt.Fprintf(out, "Commands: %s\n", commandNameList())
	fmt.Fprintf(out, "Type %s for the menu, %s for the full table.\n\n", ui.Dim("/"), ui.Dim("/help"))

	if snap.provider != "ollama" && snap.apiKey == "" {
		fmt.Fprintf(out, "🔑 Enter %s API Key (saved to ~/.config/tether/credentials.json): ", snap.provider)
		if scanner.Scan() {
			key := strings.TrimSpace(scanner.Text())
			if key != "" {
				_ = r.SaveProviderKey(key)
				fmt.Fprintf(out, "✓ API key saved.\n\n")
			}
		}
	}

	for {
		cur := r.snapshotCallConfig()
		fmt.Fprintf(out, "[%s:%s]> ", cur.model, r.engine.GetMode())
		if !scanner.Scan() {
			fmt.Fprintln(out)
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
				if chosen, ok := chooseCommand(out, scanner); ok {
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
				fmt.Fprintf(out, "✖ unknown command %q. Type / for the menu, or /help.\n", strings.Fields(line)[0])
			}
			continue
		}

		// Print the answer first, then the mark that closes it. On failure the
		// error is the answer, and the mark still follows so the outcome is
		// never left implicit.
		res := runTurn(r, ctx, line)
		if res.panicMsg != nil {
			fmt.Fprintf(out, "✖ panic: %v\n", res.panicMsg)
		}
		if res.err != nil {
			fmt.Fprintf(out, "✖ %v\n", res.err)
		} else {
			fmt.Fprintf(out, "\n%s\n", res.answer)
		}
		fmt.Fprintf(out, "%s\n\n", DoneRule(res.outcome, res.elapsed))
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
