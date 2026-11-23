package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crossparties/tether/internal/core"
)

func TestCredentialsDualFormatPermissionsAndZeroSQLiteSecrets(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("TETHER_CONFIG_DIR", cfgDir)

	// Seed mixed credentials.json: legacy string for openai + multi-key array for deepseek (with loose perms 0644 initially).
	credPath := filepath.Join(cfgDir, "credentials.json")
	seed := `{
  "openai": "sk-openai-legacy-secret",
  "deepseek": [
    {"id": "ds-backup", "label": "Backup", "api_key": "sk-ds-backup-secret", "priority": 2, "enabled": true},
    {"id": "ds-primary", "label": "Primary", "api_key": "sk-ds-primary-secret", "priority": 1},
    {"id": "ds-disabled", "label": "Disabled", "api_key": "sk-ds-disabled-secret", "priority": 0, "enabled": false}
  ]
}`
	if err := os.WriteFile(credPath, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	creds, err := LoadAllCredentials()
	if err != nil {
		t.Fatalf("LoadAllCredentials: %v", err)
	}
	if len(creds["openai"]) != 1 || creds["openai"][0].APIKey != "sk-openai-legacy-secret" || !creds["openai"][0].Enabled {
		t.Fatalf("legacy string parsing failed: %+v", creds["openai"])
	}
	if len(creds["deepseek"]) != 3 {
		t.Fatalf("expected 3 deepseek entries, got %d", len(creds["deepseek"]))
	}
	// Omitted "enabled" defaults to true; explicit false remains false.
	if !creds["deepseek"][1].Enabled || creds["deepseek"][2].Enabled {
		t.Fatalf("enabled defaults/overrides wrong: %+v", creds["deepseek"])
	}
	if got := loadSavedKey("deepseek"); got != "sk-ds-primary-secret" {
		t.Fatalf("loadSavedKey(deepseek) = %q, want priority-1 enabled key", got)
	}

	// Save a new key via SaveAPIKey and verify 0600 permissions + preservation of backup keys.
	if err := SaveAPIKey("deepseek", "sk-ds-new-secret"); err != nil {
		t.Fatalf("SaveAPIKey: %v", err)
	}
	info, err := os.Stat(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("credentials.json mode = %04o, want 0600", info.Mode().Perm())
	}

	// Verify SQLite api_keys_meta + key_health.json NEVER store raw API secrets.
	ws := t.TempDir()
	engine, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	pool := NewKeyPool(engine, nil)
	pool.RecordSuccess("ds-primary", "deepseek", "Primary", 42*time.Millisecond, 100, 20)
	pool.MarkCapabilityFailure("ds-primary", "bad-model", "404 not found")

	healthPath := filepath.Join(cfgDir, "key_health.json")
	hInfo, err := os.Stat(healthPath)
	if err != nil {
		t.Fatalf("expected key_health.json created: %v", err)
	}
	if hInfo.Mode().Perm() != 0600 {
		t.Fatalf("key_health.json mode = %04o, want 0600", hInfo.Mode().Perm())
	}
	hBytes, _ := os.ReadFile(healthPath)
	for _, secret := range []string{"sk-ds-primary-secret", "sk-ds-backup-secret", "sk-openai-legacy-secret", "sk-ds-new-secret"} {
		if strings.Contains(string(hBytes), secret) {
			t.Fatalf("key_health.json leaked raw API secret %q!", secret)
		}
	}

	metas, err := engine.ListAPIKeysMeta()
	if err != nil || len(metas) == 0 {
		t.Fatalf("ListAPIKeysMeta: err=%v len=%d", err, len(metas))
	}
	for _, m := range metas {
		rawMeta, _ := json.Marshal(m)
		for _, secret := range []string{"sk-ds-primary-secret", "sk-ds-backup-secret", "sk-openai-legacy-secret", "sk-ds-new-secret"} {
			if strings.Contains(string(rawMeta), secret) {
				t.Fatalf("api_keys_meta leaked raw API secret %q!", secret)
			}
		}
	}
	// Extra full-table check on session.db tables used for keys/pinning.
	rows, err := engine.ReadDB().Query(`SELECT key_id, provider, label FROM api_keys_meta`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, prov, lbl string
		_ = rows.Scan(&id, &prov, &lbl)
		if strings.HasPrefix(id, "sk-") || strings.HasPrefix(lbl, "sk-") {
			t.Fatalf("api_keys_meta contains sk- secret: id=%q label=%q", id, lbl)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
}

func TestKeyPoolZeroTokenProbeAndNegativeCapabilityCache(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("TETHER_CONFIG_DIR", cfgDir)

	var completionsCalled atomic.Int32
	var dsModelsCalls, dsBalanceCalls, oaiModelsCalls, ollamaTagsCalls atomic.Int32

	dsHealthySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			dsModelsCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": []interface{}{}})
		case "/user/balance":
			dsBalanceCalls.Add(1)
			if r.Header.Get("Authorization") == "Bearer sk-empty-bal" {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"is_available": false})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"is_available": true})
		case "/chat/completions":
			completionsCalled.Add(1)
			http.Error(w, "probe must never call chat/completions", http.StatusInternalServerError)
		}
	}))
	defer dsHealthySrv.Close()

	oaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			oaiModelsCalls.Add(1)
			if r.Header.Get("Authorization") == "Bearer sk-bad-401" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": []interface{}{}})
			return
		}
		completionsCalled.Add(1)
	}))
	defer oaiSrv.Close()

	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			ollamaTagsCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"models": []interface{}{}})
			return
		}
		completionsCalled.Add(1)
	}))
	defer ollamaSrv.Close()

	ws := t.TempDir()
	engine, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	pool := NewKeyPoolWithCandidates(engine, nil, []KeyCandidate{
		{Provider: "deepseek", Entry: ProviderKeyEntry{ID: "ds-ok", APIKey: "sk-ok", BaseURL: dsHealthySrv.URL + "/chat/completions", Enabled: true}},
		{Provider: "deepseek", Entry: ProviderKeyEntry{ID: "ds-nobal", APIKey: "sk-empty-bal", BaseURL: dsHealthySrv.URL + "/chat/completions", Enabled: true}},
		{Provider: "openai", Entry: ProviderKeyEntry{ID: "oai-401", APIKey: "sk-bad-401", BaseURL: oaiSrv.URL + "/v1/chat/completions", Enabled: true}},
		{Provider: "ollama", Entry: ProviderKeyEntry{ID: "oll-1", BaseURL: ollamaSrv.URL + "/v1/chat/completions", Enabled: true}},
	})
	pool.SetTTLs(5*time.Minute, 80*time.Millisecond, 50*time.Millisecond)

	results := pool.ProbeAll(context.Background())
	if len(results) != 4 {
		t.Fatalf("expected 4 probe results, got %d", len(results))
	}
	if completionsCalled.Load() != 0 {
		t.Fatalf("0-token probe called /chat/completions %d times!", completionsCalled.Load())
	}
	if dsModelsCalls.Load() != 2 || dsBalanceCalls.Load() != 2 || oaiModelsCalls.Load() != 1 || ollamaTagsCalls.Load() != 1 {
		t.Fatalf("unexpected probe endpoint counts: dsModels=%d dsBal=%d oai=%d ollama=%d",
			dsModelsCalls.Load(), dsBalanceCalls.Load(), oaiModelsCalls.Load(), ollamaTagsCalls.Load())
	}

	if h, ok := pool.HealthOf("ds-ok"); !ok || !h.Healthy {
		t.Fatalf("expected ds-ok healthy, got ok=%v %+v", ok, h)
	}
	if h, ok := pool.HealthOf("ds-nobal"); !ok || h.Healthy || h.StatusCode != 402 {
		t.Fatalf("expected ds-nobal unhealthy 402, got ok=%v %+v", ok, h)
	}
	if h, ok := pool.HealthOf("oai-401"); !ok || h.Healthy || h.StatusCode != 401 {
		t.Fatalf("expected oai-401 unhealthy 401, got ok=%v %+v", ok, h)
	}
	if h, ok := pool.HealthOf("oll-1"); !ok || !h.Healthy {
		t.Fatalf("expected oll-1 healthy, got ok=%v %+v", ok, h)
	}

	// EnsureProbed skips cached entries within TTL.
	pool.EnsureProbed(context.Background())
	if dsModelsCalls.Load() != 2 {
		t.Fatalf("EnsureProbed should have used TTL cache, got dsModelsCalls=%d", dsModelsCalls.Load())
	}

	// Negative Capability Cache (key_id, model) with TTL expiry.
	pool.MarkCapabilityFailure("ds-ok", "deepseek-reasoner", "404 unsupported")
	if !pool.IsCapabilityBlocked("ds-ok", "deepseek-reasoner") {
		t.Fatalf("expected (ds-ok, deepseek-reasoner) blocked in negative capability cache")
	}
	if pool.IsCapabilityBlocked("ds-ok", "deepseek-chat") {
		t.Fatalf("other model deepseek-chat should NOT be blocked on ds-ok")
	}
	time.Sleep(110 * time.Millisecond)
	if pool.IsCapabilityBlocked("ds-ok", "deepseek-reasoner") {
		t.Fatalf("expected negative capability entry to expire after TTL")
	}
}

func TestKeyPoolK1BypassAndKGreater1StickyPinning(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("TETHER_CONFIG_DIR", cfgDir)

	ws := t.TempDir()
	engine, err := core.NewEngine(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	// 1. K == 1 Bypass
	pool1 := NewKeyPoolWithCandidates(engine, nil, []KeyCandidate{
		{Provider: "deepseek", Entry: ProviderKeyEntry{ID: "only-key", APIKey: "sk-only", Enabled: true}},
	})
	chosen1, ok := pool1.SelectKey(context.Background(), "deepseek", "deepseek-chat")
	if !ok || chosen1.Entry.ID != "only-key" {
		t.Fatalf("K==1 SelectKey failed: ok=%v chosen=%+v", ok, chosen1)
	}
	if !pool1.LastSelectionBypassedRanking() {
		t.Fatalf("expected K==1 to bypass ranking logic")
	}

	// 2. K > 1 Auto-Rank + Session-Sticky Key Pinning
	ws2 := t.TempDir()
	eng2, err := core.NewEngine(ws2)
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()

	// Seed api_keys_meta: key-fast has lowest TTFT + 0 errors + high cache hit, key-slow has higher TTFT + errors.
	_ = eng2.UpsertAPIKeyMeta(core.APIKeyMeta{KeyID: "ds-slow", Provider: "deepseek", AvgTTFTMs: 900, ToolErrorRate: 0.4, CacheHitRatio: 0.1})
	_ = eng2.UpsertAPIKeyMeta(core.APIKeyMeta{KeyID: "ds-fast", Provider: "deepseek", AvgTTFTMs: 110, ToolErrorRate: 0.0, CacheHitRatio: 0.9})

	pool2 := NewKeyPoolWithCandidates(eng2, nil, []KeyCandidate{
		{Provider: "deepseek", Entry: ProviderKeyEntry{ID: "ds-slow", APIKey: "sk-slow", Priority: 1, Enabled: true}},
		{Provider: "deepseek", Entry: ProviderKeyEntry{ID: "ds-fast", APIKey: "sk-fast", Priority: 1, Enabled: true}},
		{Provider: "openai", Entry: ProviderKeyEntry{ID: "oai-alt", APIKey: "sk-oai", Priority: 2, Enabled: true}},
	})

	winner, ok := pool2.SelectKey(context.Background(), "", "")
	if !ok || winner.Entry.ID != "ds-fast" {
		t.Fatalf("K>1 SelectKey picked %q, want ds-fast", winner.Entry.ID)
	}
	if pool2.LastSelectionBypassedRanking() {
		t.Fatalf("K>1 should not bypass ranking")
	}
	pinnedDB, _ := eng2.GetPinnedKeyID()
	if pinnedDB != "ds-fast" {
		t.Fatalf("expected pinned_key_id persisted as ds-fast, got %q", pinnedDB)
	}

	// Even if oai-alt becomes slightly faster in api_keys_meta later, sticky pinning MUST stay on ds-fast!
	_ = eng2.UpsertAPIKeyMeta(core.APIKeyMeta{KeyID: "oai-alt", Provider: "openai", AvgTTFTMs: 25, ToolErrorRate: 0.0, CacheHitRatio: 1.0})
	pool2.RecordSuccess("oai-alt", "openai", "oai-alt", 20*time.Millisecond, 100, 0)
	nextTurn, ok := pool2.SelectKey(context.Background(), "", "")
	if !ok || nextTurn.Entry.ID != "ds-fast" {
		t.Fatalf("sticky pinning violated: flipped from ds-fast to %q!", nextTurn.Entry.ID)
	}
}

func TestRunnerSwitchProviderSaveKeyAndKeyCommand(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("TETHER_CONFIG_DIR", cfgDir)

	if err := SaveProviderKeys("deepseek", []ProviderKeyEntry{
		{ID: "ds-1", Label: "DS1", APIKey: "sk-ds-1", Priority: 1, Enabled: true},
		{ID: "ds-2", Label: "DS2", APIKey: "sk-ds-2", Priority: 2, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := SaveAPIKey("openai", "sk-oai-1"); err != nil {
		t.Fatal(err)
	}

	eng, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	var out bytes.Buffer
	r := NewRunner(eng, Config{Provider: "deepseek", Out: &out})
	if r.cfg.APIKey != "sk-ds-1" || r.PinnedKeyID() != "ds-1" {
		t.Fatalf("unexpected initial key selection: key=%q pinned=%q", r.cfg.APIKey, r.PinnedKeyID())
	}

	prov, mod, err := r.SwitchProvider("openai")
	if err != nil {
		t.Fatalf("SwitchProvider: %v", err)
	}
	if prov != "openai" || mod != "gpt-4o" || r.cfg.APIKey != "sk-oai-1" {
		t.Fatalf("SwitchProvider mismatch: prov=%q mod=%q key=%q", prov, mod, r.cfg.APIKey)
	}

	if err := r.SaveProviderKey("sk-oai-2"); err != nil {
		t.Fatalf("SaveProviderKey: %v", err)
	}
	if r.cfg.APIKey != "sk-oai-2" {
		t.Fatalf("expected active key updated to sk-oai-2, got %q", r.cfg.APIKey)
	}

	// /key list must never reveal raw secrets.
	out.Reset()
	done, known := dispatchCommand(r, "/key list")
	if done || !known {
		t.Fatalf("/key list failed dispatch: done=%v known=%v", done, known)
	}
	if strings.Contains(out.String(), "sk-ds-1") || strings.Contains(out.String(), "sk-oai-2") {
		t.Fatalf("/key list leaked raw API key in output:\n%s", out.String())
	}

	// Verify atomic writeSecureJSON left zero temp files in TETHER_CONFIG_DIR.
	entries, err := os.ReadDir(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatalf("unexpected leftover temp file %q in config dir", entry.Name())
		}
	}
}

func TestKeyPoolSelectKeyRespectsProviderIsolation(t *testing.T) {
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())

	eng, err := core.NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	pool := NewKeyPoolWithCandidates(eng, nil, []KeyCandidate{
		{Provider: "deepseek", Entry: ProviderKeyEntry{ID: "ds-1", APIKey: "sk-ds-1", Enabled: true}},
	})

	// Explicit provider "ollama" MUST NOT silently select a DeepSeek key when no Ollama key exists.
	if cand, ok := pool.SelectKey(context.Background(), "ollama", ""); ok {
		t.Fatalf("expected SelectKey(ollama) to return false when only DeepSeek exists, got %+v", cand)
	}
	// Empty preferredProvider selects across all usable candidates.
	if cand, ok := pool.SelectKey(context.Background(), "", ""); !ok || cand.Entry.ID != "ds-1" {
		t.Fatalf("expected SelectKey('') to pick ds-1, got ok=%v cand=%+v", ok, cand)
	}
}
