package harness

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

const defaultContextWindowTokens = 128000

// UsageMetrics captures isolated token and KV-cache telemetry for one usage_id
// ("main", "crew:<id>", "condenser", or "btw").
type UsageMetrics struct {
	UsageID          string  `json:"usage_id"`
	Calls            int     `json:"calls"`
	PromptTokens     int     `json:"prompt_tokens"`
	CachedTokens     int     `json:"cached_tokens"`
	MissTokens       int     `json:"miss_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	LastTurnTokens   int     `json:"last_turn_tokens"`
	LastCacheHit     int     `json:"last_cache_hit"`
	LastCacheMiss    int     `json:"last_cache_miss"`
	LastTTFTMs       int64   `json:"last_ttft_ms"`
	ContextPressure  float64 `json:"context_pressure"`
	CacheHitRatio    float64 `json:"cache_hit_ratio"`
}

// UsageTracker isolates cumulative and last-turn token/cache telemetry per usage_id.
type UsageTracker struct {
	mu      sync.RWMutex
	entries map[string]*UsageMetrics
}

// NewUsageTracker constructs an empty thread-safe UsageTracker.
func NewUsageTracker() *UsageTracker {
	return &UsageTracker{
		entries: make(map[string]*UsageMetrics),
	}
}

// Record adds one completed API call's usage under usageID.
func (t *UsageTracker) Record(usageID string, u apiUsage, contextWindow int) {
	if t == nil {
		return
	}
	usageID = strings.TrimSpace(usageID)
	if usageID == "" {
		usageID = "main"
	}
	if contextWindow <= 0 {
		contextWindow = defaultContextWindowTokens
	}

	prompt := u.promptTokens
	if prompt <= 0 && (u.cacheHit > 0 || u.cacheMiss > 0) {
		prompt = u.cacheHit + u.cacheMiss
	}
	miss := u.cacheMiss
	if miss == 0 && prompt >= u.cacheHit {
		miss = prompt - u.cacheHit
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.entries[usageID]
	if m == nil {
		m = &UsageMetrics{UsageID: usageID}
		t.entries[usageID] = m
	}
	m.Calls++
	m.PromptTokens += prompt
	m.CachedTokens += u.cacheHit
	m.MissTokens += miss
	m.CompletionTokens += u.completionTokens
	if prompt > 0 || u.completionTokens > 0 {
		m.LastTurnTokens = prompt + u.completionTokens
	}
	if u.cacheHit > 0 || miss > 0 {
		m.LastCacheHit = u.cacheHit
		m.LastCacheMiss = miss
	}
	if u.ttft > 0 {
		m.LastTTFTMs = u.ttft.Milliseconds()
	}
	if m.PromptTokens > 0 {
		m.CacheHitRatio = float64(m.CachedTokens) / float64(m.PromptTokens)
	}
	if contextWindow > 0 && m.LastTurnTokens > 0 {
		m.ContextPressure = float64(m.LastTurnTokens) / float64(contextWindow)
	}
}

// Get returns a snapshot of metrics for usageID.
func (t *UsageTracker) Get(usageID string) (UsageMetrics, bool) {
	if t == nil {
		return UsageMetrics{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	m := t.entries[strings.TrimSpace(usageID)]
	if m == nil {
		return UsageMetrics{}, false
	}
	return *m, true
}

// Snapshot returns a copy of all tracked usage_id metrics.
func (t *UsageTracker) Snapshot() map[string]UsageMetrics {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]UsageMetrics, len(t.entries))
	for k, v := range t.entries {
		out[k] = *v
	}
	return out
}

// Reset clears metrics for usageID (or all entries if usageID == "").
func (t *UsageTracker) Reset(usageID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if usageID == "" {
		t.entries = make(map[string]*UsageMetrics)
	} else {
		delete(t.entries, usageID)
	}
	t.mu.Unlock()
}

// UsageMetrics returns isolated metrics for the requested usage_id ("main", "btw", "condenser", "crew:<id>").
func (r *Runner) UsageMetrics(usageID string) UsageMetrics {
	if r == nil || r.usageTracker == nil {
		return UsageMetrics{UsageID: usageID}
	}
	if m, ok := r.usageTracker.Get(usageID); ok {
		return m
	}
	return UsageMetrics{UsageID: usageID}
}

// AllUsageMetrics returns a copy of metrics across every recorded usage_id.
func (r *Runner) AllUsageMetrics() map[string]UsageMetrics {
	if r == nil || r.usageTracker == nil {
		return nil
	}
	return r.usageTracker.Snapshot()
}

// TokensSummary formats context pressure and KV-cache hit ratios separated by usage_id.
func (r *Runner) TokensSummary() string {
	all := r.AllUsageMetrics()
	if len(all) == 0 {
		return "No token usage recorded yet."
	}
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ctxWin := r.condenserConfig().ContextWindowTokens
	var b strings.Builder
	for _, id := range ids {
		m := all[id]
		fmt.Fprintf(&b, "  %-14s calls=%d prompt=%d cached=%d (%.1f%%) completion=%d last_turn=%d/%d (%.1f%% pressure)\n",
			id, m.Calls, m.PromptTokens, m.CachedTokens, m.CacheHitRatio*100.0,
			m.CompletionTokens, m.LastTurnTokens, ctxWin, m.ContextPressure*100.0)
	}
	return strings.TrimRight(b.String(), "\n")
}
