package harness

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crossparties/tether/internal/core"
)

const (
	defaultProbeTTL      = 5 * time.Minute
	defaultNegCapTTL     = 30 * time.Minute
	defaultCooldownDur   = 3 * time.Second
	maxRetryAfterHonored = 15 * time.Second
	maxProbeTimeout      = 5 * time.Second
	maxProbeConcurrency  = 4
	defaultUnsetKeyPrio  = 100
)

func configDir() string {
	if d := strings.TrimSpace(os.Getenv("TETHER_CONFIG_DIR")); d != "" {
		return filepath.Clean(d)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "tether")
}

func credentialsPath() string {
	return filepath.Join(configDir(), "credentials.json")
}

func keyHealthPath() string {
	return filepath.Join(configDir(), "key_health.json")
}

// ProviderKeyEntry configures one API key entry inside credentials.json.
type ProviderKeyEntry struct {
	ID       string `json:"id"`
	Label    string `json:"label,omitempty"`
	APIKey   string `json:"api_key"`
	BaseURL  string `json:"base_url,omitempty"`
	Model    string `json:"model,omitempty"`
	Priority int    `json:"priority,omitempty"`
	Enabled  bool   `json:"enabled"`
}

// UnmarshalJSON defaults Enabled to true when omitted from JSON objects.
func (e *ProviderKeyEntry) UnmarshalJSON(data []byte) error {
	type alias ProviderKeyEntry
	aux := struct {
		*alias
		Enabled *bool `json:"enabled"`
	}{
		alias: (*alias)(e),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.Enabled == nil {
		e.Enabled = true
	} else {
		e.Enabled = *aux.Enabled
	}
	return nil
}

func deriveKeyID(provider, apiKey string, idx int) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		p = "custom"
	}
	if strings.TrimSpace(apiKey) == "" {
		if idx > 0 {
			return fmt.Sprintf("%s-local-%d", p, idx+1)
		}
		return p + "-local"
	}
	sum := sha256.Sum256([]byte(p + ":" + apiKey))
	return fmt.Sprintf("%s-%x", p, sum[:5])
}

func normalizeEntry(provider string, e ProviderKeyEntry, idx int) ProviderKeyEntry {
	e.APIKey = strings.TrimSpace(e.APIKey)
	e.ID = strings.TrimSpace(e.ID)
	e.Label = strings.TrimSpace(e.Label)
	e.BaseURL = strings.TrimSpace(e.BaseURL)
	e.Model = strings.TrimSpace(e.Model)
	if e.ID == "" || (e.APIKey != "" && e.ID == e.APIKey) {
		e.ID = deriveKeyID(provider, e.APIKey, idx)
	}
	if e.Label == "" || (e.APIKey != "" && e.Label == e.APIKey) {
		e.Label = e.ID
	}
	return e
}

// LoadAllCredentials reads credentials.json supporting both legacy single-string
// provider values and multi-key []ProviderKeyEntry arrays.
func LoadAllCredentials() (map[string][]ProviderKeyEntry, error) {
	path := credentialsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][]ProviderKeyEntry{}, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string][]ProviderKeyEntry{}, nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	out := make(map[string][]ProviderKeyEntry, len(raw))
	for k, v := range raw {
		p := strings.ToLower(strings.TrimSpace(k))
		trimmed := strings.TrimSpace(string(v))
		if trimmed == "" || trimmed == "null" {
			continue
		}
		switch trimmed[0] {
		case '"':
			var key string
			if err := json.Unmarshal(v, &key); err == nil {
				key = strings.TrimSpace(key)
				if key != "" {
					out[p] = []ProviderKeyEntry{
						normalizeEntry(p, ProviderKeyEntry{
							Label:    p,
							APIKey:   key,
							Priority: 1,
							Enabled:  true,
						}, 0),
					}
				}
			}
		case '[':
			var entries []ProviderKeyEntry
			if err := json.Unmarshal(v, &entries); err == nil {
				seenIDs := map[string]int{}
				clean := make([]ProviderKeyEntry, 0, len(entries))
				for i, entry := range entries {
					ne := normalizeEntry(p, entry, i)
					if seenIDs[ne.ID] > 0 {
						ne.ID = fmt.Sprintf("%s-%d", ne.ID, i+1)
					}
					seenIDs[ne.ID]++
					clean = append(clean, ne)
				}
				if len(clean) > 0 {
					out[p] = clean
				}
			}
		}
	}
	return out, nil
}

func loadSavedKey(provider string) string {
	creds, err := LoadAllCredentials()
	if err != nil {
		return ""
	}
	entries := creds[strings.ToLower(strings.TrimSpace(provider))]
	if len(entries) == 0 {
		return ""
	}
	sorted := make([]ProviderKeyEntry, 0, len(entries))
	for _, e := range entries {
		if e.Enabled && e.APIKey != "" {
			sorted = append(sorted, e)
		}
	}
	if len(sorted) == 0 {
		return ""
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		pi, pj := effectivePrio(sorted[i].Priority), effectivePrio(sorted[j].Priority)
		if pi != pj {
			return pi < pj
		}
		return sorted[i].ID < sorted[j].ID
	})
	return sorted[0].APIKey
}

func readRawCredentialsMap(path string) map[string]json.RawMessage {
	raw := make(map[string]json.RawMessage)
	if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		_ = json.Unmarshal(data, &raw)
	}
	return raw
}

func writeSecureJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0700)
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// SaveProviderKeys persists a multi-key array for provider into credentials.json (0600).
func SaveProviderKeys(provider string, entries []ProviderKeyEntry) error {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		return fmt.Errorf("provider name cannot be empty")
	}
	path := credentialsPath()
	raw := readRawCredentialsMap(path)

	norm := make([]ProviderKeyEntry, 0, len(entries))
	for i, e := range entries {
		norm = append(norm, normalizeEntry(p, e, i))
	}
	encoded, err := json.Marshal(norm)
	if err != nil {
		return err
	}
	raw[p] = encoded
	return writeSecureJSON(path, raw)
}

// SaveAPIKey persists the provider's API key to credentials.json (0600).
// If provider already uses a multi-key array, the saved key is upserted at Priority 1
// while preserving existing backup entries.
func SaveAPIKey(provider, key string) error {
	p := strings.ToLower(strings.TrimSpace(provider))
	key = strings.TrimSpace(key)
	if p == "" {
		return fmt.Errorf("provider name cannot be empty")
	}

	path := credentialsPath()
	raw := readRawCredentialsMap(path)

	if existing, ok := raw[p]; ok {
		trimmed := strings.TrimSpace(string(existing))
		if strings.HasPrefix(trimmed, "[") {
			var entries []ProviderKeyEntry
			if err := json.Unmarshal(existing, &entries); err == nil {
				found := false
				for i := range entries {
					if entries[i].APIKey == key {
						entries[i].Enabled = true
						entries[i].Priority = 1
						found = true
					} else if entries[i].Priority > 0 && entries[i].Priority == 1 {
						entries[i].Priority = 2
					}
				}
				if !found {
					newEntry := normalizeEntry(p, ProviderKeyEntry{
						APIKey:   key,
						Priority: 1,
						Enabled:  true,
					}, len(entries))
					entries = append([]ProviderKeyEntry{newEntry}, entries...)
				}
				return SaveProviderKeys(p, entries)
			}
		}
	}

	encoded, err := json.Marshal(key)
	if err != nil {
		return err
	}
	raw[p] = encoded
	return writeSecureJSON(path, raw)
}

// KeyCandidate pairs a provider name with one configured ProviderKeyEntry.
type KeyCandidate struct {
	Provider string
	Entry    ProviderKeyEntry
}

// EffectiveBaseURL returns the candidate's BaseURL or the provider's preset URL.
func (c KeyCandidate) EffectiveBaseURL() string {
	if c.Entry.BaseURL != "" {
		return c.Entry.BaseURL
	}
	if p, ok := presets[strings.ToLower(c.Provider)]; ok {
		return p.BaseURL
	}
	return ""
}

// EffectiveModel returns the candidate's Model or the provider's default model.
func (c KeyCandidate) EffectiveModel() string {
	if c.Entry.Model != "" {
		return c.Entry.Model
	}
	if p, ok := presets[strings.ToLower(c.Provider)]; ok {
		return p.DefaultModel
	}
	return ""
}

// KeyHealthEntry records the latest zero-token startup probe result for one key.
type KeyHealthEntry struct {
	KeyID      string    `json:"key_id"`
	Provider   string    `json:"provider"`
	Healthy    bool      `json:"healthy"`
	StatusCode int       `json:"status_code"`
	LatencyMs  int64     `json:"latency_ms"`
	Reason     string    `json:"reason,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// NegativeCapEntry caches a hard model/tool capability failure for (key_id, model).
type NegativeCapEntry struct {
	KeyID     string    `json:"key_id"`
	Model     string    `json:"model"`
	Reason    string    `json:"reason,omitempty"`
	BlockedAt time.Time `json:"blocked_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// KeyHealthCache persists probe results and negative capability blocks (no raw secrets).
type KeyHealthCache struct {
	Keys    map[string]KeyHealthEntry   `json:"keys"`
	NegCaps map[string]NegativeCapEntry `json:"negative_capabilities"`
}

// KeyPool manages multi-key selection, startup 0-token probing, negative capability
// caching, session-sticky pinning, and hierarchical failover.
type KeyPool struct {
	mu               sync.RWMutex
	engine           *core.Engine
	client           *http.Client
	candidates       []KeyCandidate
	health           KeyHealthCache
	cooldowns        map[string]time.Time
	meta             map[string]core.APIKeyMeta
	pinnedKeyID      string
	probeTTL         time.Duration
	negCapTTL        time.Duration
	cooldownDefault  time.Duration
	lastBypassedRank bool
	persistHealth    bool
}

// NewKeyPool loads all credentials from credentials.json, cached probe health from
// key_health.json, and non-secret metrics from SQLite api_keys_meta.
func NewKeyPool(engine *core.Engine, client *http.Client) *KeyPool {
	creds, _ := LoadAllCredentials()
	var cands []KeyCandidate
	providers := make([]string, 0, len(creds))
	for p := range creds {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	for _, p := range providers {
		for _, entry := range creds[p] {
			cands = append(cands, KeyCandidate{Provider: p, Entry: entry})
		}
	}
	p := newKeyPoolBase(engine, client, cands, true)
	p.loadHealthFromDisk()
	return p
}

// NewKeyPoolWithCandidates constructs a pool directly from explicit candidates.
// Health persistence is enabled automatically when TETHER_CONFIG_DIR is set.
func NewKeyPoolWithCandidates(engine *core.Engine, client *http.Client, candidates []KeyCandidate) *KeyPool {
	persist := strings.TrimSpace(os.Getenv("TETHER_CONFIG_DIR")) != ""
	norm := make([]KeyCandidate, 0, len(candidates))
	for i, c := range candidates {
		prov := strings.ToLower(strings.TrimSpace(c.Provider))
		if prov == "" {
			prov = "custom"
		}
		entry := normalizeEntry(prov, c.Entry, i)
		norm = append(norm, KeyCandidate{Provider: prov, Entry: entry})
	}
	p := newKeyPoolBase(engine, client, norm, persist)
	if persist {
		p.loadHealthFromDisk()
	}
	return p
}

func newKeyPoolBase(engine *core.Engine, client *http.Client, cands []KeyCandidate, persistHealth bool) *KeyPool {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	p := &KeyPool{
		engine:          engine,
		client:          client,
		candidates:      cands,
		cooldowns:       make(map[string]time.Time),
		meta:            make(map[string]core.APIKeyMeta),
		probeTTL:        defaultProbeTTL,
		negCapTTL:       defaultNegCapTTL,
		cooldownDefault: defaultCooldownDur,
		persistHealth:   persistHealth,
		health: KeyHealthCache{
			Keys:    make(map[string]KeyHealthEntry),
			NegCaps: make(map[string]NegativeCapEntry),
		},
	}
	if engine != nil {
		if metas, err := engine.ListAPIKeysMeta(); err == nil {
			for _, m := range metas {
				p.meta[m.KeyID] = m
			}
		}
		if pinned, err := engine.GetPinnedKeyID(); err == nil && pinned != "" {
			p.pinnedKeyID = pinned
		}
	}
	return p
}

// SetTTLs overrides probe, negative capability, and cooldown TTLs (useful for unit tests).
func (p *KeyPool) SetTTLs(probeTTL, negCapTTL, cooldownDefault time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if probeTTL > 0 {
		p.probeTTL = probeTTL
	}
	if negCapTTL > 0 {
		p.negCapTTL = negCapTTL
	}
	if cooldownDefault > 0 {
		p.cooldownDefault = cooldownDefault
	}
}

// Candidates returns a snapshot of configured key candidates.
func (p *KeyPool) Candidates() []KeyCandidate {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]KeyCandidate, len(p.candidates))
	copy(out, p.candidates)
	return out
}

// Len returns the number of candidates in the pool.
func (p *KeyPool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.candidates)
}

// PinnedKeyID returns the currently pinned session key ID.
func (p *KeyPool) PinnedKeyID() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pinnedKeyID
}

// SetPinnedKeyID pins keyID in memory and persists it to .tether/session.db.
func (p *KeyPool) SetPinnedKeyID(keyID string) {
	p.mu.Lock()
	p.pinnedKeyID = keyID
	eng := p.engine
	p.mu.Unlock()
	if eng != nil && keyID != "" {
		_ = eng.SetPinnedKeyID(keyID)
	}
}

// LastSelectionBypassedRanking reports whether the last SelectKey call used the K==1 fast-path.
func (p *KeyPool) LastSelectionBypassedRanking() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastBypassedRank
}

// ReloadFromDisk refreshes candidates from credentials.json while keeping health/cooldowns.
func (p *KeyPool) ReloadFromDisk() error {
	creds, err := LoadAllCredentials()
	if err != nil {
		return err
	}
	providers := make([]string, 0, len(creds))
	for prov := range creds {
		providers = append(providers, prov)
	}
	sort.Strings(providers)
	var cands []KeyCandidate
	for _, prov := range providers {
		for _, entry := range creds[prov] {
			cands = append(cands, KeyCandidate{Provider: prov, Entry: entry})
		}
	}
	p.mu.Lock()
	p.candidates = cands
	p.mu.Unlock()
	return nil
}

func (p *KeyPool) loadHealthFromDisk() {
	data, err := os.ReadFile(keyHealthPath())
	if err != nil {
		return
	}
	var cache KeyHealthCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return
	}
	if cache.Keys == nil {
		cache.Keys = make(map[string]KeyHealthEntry)
	}
	if cache.NegCaps == nil {
		cache.NegCaps = make(map[string]NegativeCapEntry)
	}
	p.health = cache
}

func (p *KeyPool) saveHealthLocked() {
	if !p.persistHealth {
		return
	}
	_ = writeSecureJSON(keyHealthPath(), p.health)
}

func negCapKey(keyID, model string) string {
	return keyID + "::" + strings.ToLower(strings.TrimSpace(model))
}

// MarkCapabilityFailure records a negative capability entry for (keyID, model) with TTL.
func (p *KeyPool) MarkCapabilityFailure(keyID, model, reason string) {
	if strings.TrimSpace(keyID) == "" || strings.TrimSpace(model) == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.health.NegCaps[negCapKey(keyID, model)] = NegativeCapEntry{
		KeyID:     keyID,
		Model:     model,
		Reason:    reason,
		BlockedAt: now,
		ExpiresAt: now.Add(p.negCapTTL),
	}
	p.saveHealthLocked()
}

// IsCapabilityBlocked reports whether (keyID, model) has an unexpired negative capability block.
func (p *KeyPool) IsCapabilityBlocked(keyID, model string) bool {
	if strings.TrimSpace(keyID) == "" || strings.TrimSpace(model) == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	k := negCapKey(keyID, model)
	entry, ok := p.health.NegCaps[k]
	if !ok {
		return false
	}
	if time.Now().After(entry.ExpiresAt) {
		delete(p.health.NegCaps, k)
		p.saveHealthLocked()
		return false
	}
	return true
}

// MarkCooldown temporarily puts keyID on cooldown after a transient error (429/503/timeout).
func (p *KeyPool) MarkCooldown(keyID string, d time.Duration) {
	if strings.TrimSpace(keyID) == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if d <= 0 {
		d = p.cooldownDefault
	}
	p.cooldowns[keyID] = time.Now().Add(d)
}

// MarkUnhealthy marks keyID unhealthy in key_health.json for ProbeTTL after auth/quota failure.
func (p *KeyPool) MarkUnhealthy(keyID string, statusCode int, reason string) {
	if strings.TrimSpace(keyID) == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	prov := ""
	for _, c := range p.candidates {
		if c.Entry.ID == keyID {
			prov = c.Provider
			break
		}
	}
	now := time.Now()
	p.health.Keys[keyID] = KeyHealthEntry{
		KeyID:      keyID,
		Provider:   prov,
		Healthy:    false,
		StatusCode: statusCode,
		Reason:     reason,
		CheckedAt:  now,
		ExpiresAt:  now.Add(p.probeTTL),
	}
	p.saveHealthLocked()
}

// HealthOf returns the cached health entry for keyID if present and unexpired.
func (p *KeyPool) HealthOf(keyID string) (KeyHealthEntry, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	h, ok := p.health.Keys[keyID]
	if !ok || time.Now().After(h.ExpiresAt) {
		return KeyHealthEntry{}, false
	}
	return h, true
}

// ProbeAll runs zero-token probes concurrently against all enabled candidates.
func (p *KeyPool) ProbeAll(ctx context.Context) []KeyHealthEntry {
	return p.probeCandidates(ctx, false)
}

// EnsureProbed runs zero-token probes only for candidates missing a fresh TTL cache entry.
func (p *KeyPool) EnsureProbed(ctx context.Context) []KeyHealthEntry {
	return p.probeCandidates(ctx, true)
}

func (p *KeyPool) probeCandidates(ctx context.Context, skipFresh bool) []KeyHealthEntry {
	p.mu.RLock()
	var targets []KeyCandidate
	now := time.Now()
	for _, c := range p.candidates {
		if !c.Entry.Enabled {
			continue
		}
		if skipFresh {
			if h, ok := p.health.Keys[c.Entry.ID]; ok && now.Before(h.ExpiresAt) {
				continue
			}
		}
		targets = append(targets, c)
	}
	p.mu.RUnlock()

	if len(targets) == 0 {
		return nil
	}

	results := make([]KeyHealthEntry, len(targets))
	sem := make(chan struct{}, maxProbeConcurrency)
	var wg sync.WaitGroup
	for i, cand := range targets {
		wg.Add(1)
		go func(idx int, c KeyCandidate) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[idx] = p.ProbeEntry(ctx, c)
		}(i, cand)
	}
	wg.Wait()
	return results
}

func probeEndpoints(provider, baseURL string) (modelsURL, balanceURL string) {
	p := strings.ToLower(strings.TrimSpace(provider))
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		if preset, ok := presets[p]; ok {
			raw = preset.BaseURL
		}
	}
	root := strings.TrimRight(raw, "/")
	root = strings.TrimSuffix(root, "/chat/completions")
	root = strings.TrimRight(root, "/")
	if root == "" {
		return "", ""
	}
	if p == "ollama" {
		base := strings.TrimSuffix(root, "/v1")
		return base + "/api/tags", ""
	}
	modelsURL = root + "/models"
	if p == "deepseek" {
		balBase := strings.TrimSuffix(root, "/v1")
		balanceURL = balBase + "/user/balance"
	}
	return modelsURL, balanceURL
}

// ProbeEntry performs a zero-token GET probe (/models, DeepSeek /user/balance, or Ollama /api/tags),
// updates key_health.json, and seeds initial latency into api_keys_meta when absent.
func (p *KeyPool) ProbeEntry(ctx context.Context, cand KeyCandidate) KeyHealthEntry {
	modelsURL, balanceURL := probeEndpoints(cand.Provider, cand.EffectiveBaseURL())
	now := time.Now()

	p.mu.RLock()
	ttl := p.probeTTL
	client := p.client
	p.mu.RUnlock()
	if client == nil {
		client = &http.Client{Timeout: maxProbeTimeout}
	}

	entry := KeyHealthEntry{
		KeyID:     cand.Entry.ID,
		Provider:  cand.Provider,
		CheckedAt: now,
		ExpiresAt: now.Add(ttl),
	}

	if modelsURL == "" {
		entry.Healthy = false
		entry.Reason = "missing base URL"
		p.storeProbeResult(cand, entry)
		return entry
	}

	probeCtx, cancel := context.WithTimeout(ctx, maxProbeTimeout)
	defer cancel()

	doGET := func(u string) (int, []byte, int64, error) {
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, u, nil)
		if err != nil {
			return 0, nil, 0, err
		}
		if cand.Entry.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+cand.Entry.APIKey)
		}
		start := time.Now()
		resp, err := client.Do(req)
		lat := time.Since(start).Milliseconds()
		if err != nil {
			return 0, nil, lat, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return resp.StatusCode, body, lat, nil
	}

	code, body, lat, err := doGET(modelsURL)
	entry.StatusCode = code
	entry.LatencyMs = lat
	if err != nil {
		entry.Healthy = false
		entry.Reason = err.Error()
		p.storeProbeResult(cand, entry)
		return entry
	}

	if code == 401 || code == 402 || code == 403 {
		entry.Healthy = false
		entry.Reason = fmt.Sprintf("http %d: %s", code, strings.TrimSpace(string(body)))
		p.storeProbeResult(cand, entry)
		return entry
	}

	if strings.EqualFold(cand.Provider, "deepseek") && balanceURL != "" {
		bCode, bBody, bLat, bErr := doGET(balanceURL)
		if bErr == nil {
			if bLat > 0 && entry.LatencyMs == 0 {
				entry.LatencyMs = bLat
			}
			if bCode == 401 || bCode == 402 || bCode == 403 {
				entry.Healthy = false
				entry.StatusCode = bCode
				entry.Reason = fmt.Sprintf("balance http %d", bCode)
				p.storeProbeResult(cand, entry)
				return entry
			}
			if bCode >= 200 && bCode < 300 {
				var bal struct {
					IsAvailable *bool `json:"is_available"`
				}
				if json.Unmarshal(bBody, &bal) == nil && bal.IsAvailable != nil && !*bal.IsAvailable {
					entry.Healthy = false
					entry.StatusCode = http.StatusPaymentRequired
					entry.Reason = "deepseek balance unavailable"
					p.storeProbeResult(cand, entry)
					return entry
				}
				entry.Healthy = true
				entry.StatusCode = bCode
				p.storeProbeResult(cand, entry)
				return entry
			}
		}
	}

	if code >= 200 && code < 300 {
		entry.Healthy = true
	} else {
		entry.Healthy = false
		entry.Reason = fmt.Sprintf("probe http %d", code)
	}
	p.storeProbeResult(cand, entry)
	return entry
}

func (p *KeyPool) storeProbeResult(cand KeyCandidate, entry KeyHealthEntry) {
	p.mu.Lock()
	p.health.Keys[entry.KeyID] = entry
	p.saveHealthLocked()
	existingMeta, hasMeta := p.meta[entry.KeyID]
	if entry.Healthy && (!hasMeta || existingMeta.AvgTTFTMs == 0) {
		existingMeta.KeyID = entry.KeyID
		existingMeta.Provider = cand.Provider
		existingMeta.Label = cand.Entry.Label
		existingMeta.AvgTTFTMs = entry.LatencyMs
		p.meta[entry.KeyID] = existingMeta
	}
	eng := p.engine
	p.mu.Unlock()

	if eng != nil && entry.Healthy && (!hasMeta || existingMeta.AvgTTFTMs == entry.LatencyMs) {
		_ = eng.UpsertAPIKeyMeta(existingMeta)
	}
}

func effectivePrio(prio int) int {
	if prio <= 0 {
		return defaultUnsetKeyPrio
	}
	return prio
}

func (p *KeyPool) isCandidateUsableLocked(c KeyCandidate, targetModel string, now time.Time) bool {
	if !c.Entry.Enabled {
		return false
	}
	if !strings.EqualFold(c.Provider, "ollama") && strings.TrimSpace(c.Entry.APIKey) == "" {
		return false
	}
	if cd, ok := p.cooldowns[c.Entry.ID]; ok && now.Before(cd) {
		return false
	}
	if h, ok := p.health.Keys[c.Entry.ID]; ok && now.Before(h.ExpiresAt) && !h.Healthy {
		return false
	}
	effModel := targetModel
	if effModel == "" {
		effModel = c.EffectiveModel()
	}
	if effModel != "" {
		if nc, ok := p.health.NegCaps[negCapKey(c.Entry.ID, effModel)]; ok && now.Before(nc.ExpiresAt) {
			return false
		}
	}
	if c.Entry.Model != "" {
		if nc, ok := p.health.NegCaps[negCapKey(c.Entry.ID, c.Entry.Model)]; ok && now.Before(nc.ExpiresAt) {
			return false
		}
	}
	return true
}

func (p *KeyPool) CooldownDefault() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cooldownDefault
}

func (p *KeyPool) CandidateByID(keyID string) (KeyCandidate, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, c := range p.candidates {
		if c.Entry.ID == keyID {
			return c, true
		}
	}
	return KeyCandidate{}, false
}

func (p *KeyPool) scoreCandidateLocked(c KeyCandidate, now time.Time) float64 {
	prio := effectivePrio(c.Entry.Priority)
	var ttft int64
	var errRate, cacheRatio float64
	if m, ok := p.meta[c.Entry.ID]; ok {
		ttft = m.AvgTTFTMs
		errRate = m.ToolErrorRate
		cacheRatio = m.CacheHitRatio
	}
	if ttft == 0 {
		if h, ok := p.health.Keys[c.Entry.ID]; ok && now.Before(h.ExpiresAt) {
			ttft = h.LatencyMs
		}
	}
	return float64(prio)*50.0 + float64(ttft) + errRate*2000.0 - cacheRatio*300.0
}

func (p *KeyPool) sortCandidatesLocked(cands []KeyCandidate, now time.Time) {
	sort.SliceStable(cands, func(i, j int) bool {
		si := p.scoreCandidateLocked(cands[i], now)
		sj := p.scoreCandidateLocked(cands[j], now)
		if si != sj {
			return si < sj
		}
		pi, pj := effectivePrio(cands[i].Entry.Priority), effectivePrio(cands[j].Entry.Priority)
		if pi != pj {
			return pi < pj
		}
		return cands[i].Entry.ID < cands[j].Entry.ID
	})
}

// SelectKey chooses the active key using Session-Sticky Pinning when already pinned,
// K==1 ranking bypass when only one usable key exists, or K>1 ranking at session start.
func (p *KeyPool) SelectKey(_ context.Context, preferredProvider, targetModel string) (KeyCandidate, bool) {
	p.mu.Lock()
	now := time.Now()
	prefProv := strings.ToLower(strings.TrimSpace(preferredProvider))

	// Session-Sticky Key Pinning: once pinned, stay on the pinned key across turns
	// unless it becomes unhealthy, cooled down, or blocked for targetModel.
	if p.pinnedKeyID != "" {
		for _, c := range p.candidates {
			if c.Entry.ID == p.pinnedKeyID {
				checkModel := targetModel
				if checkModel == "" {
					checkModel = c.EffectiveModel()
				}
				if (prefProv == "" || strings.EqualFold(c.Provider, prefProv)) && p.isCandidateUsableLocked(c, checkModel, now) {
					p.mu.Unlock()
					return c, true
				}
				break
			}
		}
	}

	var allUsable []KeyCandidate
	var provUsable []KeyCandidate
	for _, c := range p.candidates {
		checkModel := targetModel
		if checkModel == "" || (prefProv != "" && !strings.EqualFold(c.Provider, prefProv)) {
			checkModel = c.EffectiveModel()
		}
		if !p.isCandidateUsableLocked(c, checkModel, now) {
			continue
		}
		allUsable = append(allUsable, c)
		if prefProv != "" && strings.EqualFold(c.Provider, prefProv) {
			provUsable = append(provUsable, c)
		}
	}

	scope := allUsable
	if prefProv != "" {
		scope = provUsable
	}
	if len(scope) == 0 {
		p.mu.Unlock()
		return KeyCandidate{}, false
	}

	var chosen KeyCandidate
	if len(scope) == 1 {
		// K == 1 Bypass: skip ranking logic completely and use Rank #1 directly.
		p.lastBypassedRank = true
		chosen = scope[0]
	} else {
		p.lastBypassedRank = false
		p.sortCandidatesLocked(scope, now)
		chosen = scope[0]
	}

	p.pinnedKeyID = chosen.Entry.ID
	eng := p.engine
	p.mu.Unlock()

	if eng != nil {
		_ = eng.SetPinnedKeyID(chosen.Entry.ID)
	}
	return chosen, true
}

// Rank1Candidate returns the highest-ranked usable candidate for preferredProvider
// without mutating pinnedKeyID (safe for concurrent WarmCache inspection).
func (p *KeyPool) Rank1Candidate(preferredProvider, targetModel string) (KeyCandidate, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	prefProv := strings.ToLower(strings.TrimSpace(preferredProvider))

	var scope []KeyCandidate
	for _, c := range p.candidates {
		if prefProv != "" && !strings.EqualFold(c.Provider, prefProv) {
			continue
		}
		checkModel := targetModel
		if checkModel == "" {
			checkModel = c.EffectiveModel()
		}
		if p.isCandidateUsableLocked(c, checkModel, now) {
			scope = append(scope, c)
		}
	}
	if len(scope) == 0 {
		return KeyCandidate{}, false
	}
	if len(scope) > 1 {
		p.sortCandidatesLocked(scope, now)
	}
	return scope[0], true
}

// SelectFailoverKey chooses the next healthy key after failedKeyID fails.
// When preferSameProvider is true, enabled healthy backup keys on failedProvider are
// prioritized ahead of cross-provider fallback keys.
func (p *KeyPool) SelectFailoverKey(failedKeyID, failedProvider, targetModel string, preferSameProvider bool) (KeyCandidate, bool) {
	p.mu.Lock()
	now := time.Now()
	failedProv := strings.ToLower(strings.TrimSpace(failedProvider))

	var sameProv []KeyCandidate
	var otherProv []KeyCandidate
	for _, c := range p.candidates {
		if c.Entry.ID == failedKeyID {
			continue
		}
		isSame := failedProv != "" && strings.EqualFold(c.Provider, failedProv)
		checkModel := c.EffectiveModel()
		if isSame && targetModel != "" {
			checkModel = targetModel
		}
		if !p.isCandidateUsableLocked(c, checkModel, now) {
			continue
		}
		if isSame {
			sameProv = append(sameProv, c)
		} else {
			otherProv = append(otherProv, c)
		}
	}

	var chosen KeyCandidate
	switch {
	case preferSameProvider && len(sameProv) > 0:
		if len(sameProv) == 1 {
			p.lastBypassedRank = true
		} else {
			p.lastBypassedRank = false
			p.sortCandidatesLocked(sameProv, now)
		}
		chosen = sameProv[0]
	default:
		combined := append(sameProv, otherProv...)
		if len(combined) == 0 {
			p.mu.Unlock()
			return KeyCandidate{}, false
		}
		if len(combined) == 1 {
			p.lastBypassedRank = true
		} else {
			p.lastBypassedRank = false
			p.sortCandidatesLocked(combined, now)
		}
		chosen = combined[0]
	}

	p.pinnedKeyID = chosen.Entry.ID
	eng := p.engine
	p.mu.Unlock()

	if eng != nil {
		_ = eng.SetPinnedKeyID(chosen.Entry.ID)
	}
	return chosen, true
}

// RecordSuccess updates non-secret latency, error rate, and cache hit telemetry in SQLite api_keys_meta.
func (p *KeyPool) RecordSuccess(keyID, provider, label string, ttft time.Duration, cacheHit, cacheMiss int) {
	if strings.TrimSpace(keyID) == "" {
		return
	}
	p.mu.Lock()
	m := p.meta[keyID]
	m.KeyID = keyID
	if provider != "" {
		m.Provider = strings.ToLower(provider)
	}
	if label != "" {
		m.Label = label
	} else if m.Label == "" {
		m.Label = keyID
	}
	ms := ttft.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	if m.AvgTTFTMs == 0 {
		m.AvgTTFTMs = ms
	} else {
		m.AvgTTFTMs = int64(0.7*float64(m.AvgTTFTMs) + 0.3*float64(ms))
	}
	m.ToolErrorRate = 0.8 * m.ToolErrorRate
	if total := cacheHit + cacheMiss; total > 0 {
		ratio := float64(cacheHit) / float64(total)
		if m.CacheHitRatio == 0 {
			m.CacheHitRatio = ratio
		} else {
			m.CacheHitRatio = 0.7*m.CacheHitRatio + 0.3*ratio
		}
	}
	p.meta[keyID] = m
	eng := p.engine
	p.mu.Unlock()

	if eng != nil {
		_ = eng.UpsertAPIKeyMeta(m)
	}
}

// RecordError updates non-secret tool/API error rate telemetry in SQLite api_keys_meta.
func (p *KeyPool) RecordError(keyID, provider, label string) {
	if strings.TrimSpace(keyID) == "" {
		return
	}
	p.mu.Lock()
	m := p.meta[keyID]
	m.KeyID = keyID
	if provider != "" {
		m.Provider = strings.ToLower(provider)
	}
	if label != "" {
		m.Label = label
	} else if m.Label == "" {
		m.Label = keyID
	}
	m.ToolErrorRate = 0.8*m.ToolErrorRate + 0.2
	p.meta[keyID] = m
	eng := p.engine
	p.mu.Unlock()

	if eng != nil {
		_ = eng.UpsertAPIKeyMeta(m)
	}
}
