package webui

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tetherctx "github.com/crossparties/tether/internal/context"
	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/doctor"
	"github.com/crossparties/tether/internal/harness"
	"github.com/crossparties/tether/internal/mcp"
	"github.com/crossparties/tether/internal/mount"
	"github.com/crossparties/tether/internal/preview"
	"github.com/crossparties/tether/internal/snapshot"
)

const secretMask = mcp.SecretMask

// --- Auth & Health ---

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	eng, _ := s.activeState()
	ws := ""
	mode := ""
	if eng != nil {
		ws = eng.Workspace
		mode = eng.GetMode()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"status":    "ok",
		"version":   s.version,
		"workspace": ws,
		"mode":      mode,
	})
}

// --- Setup, Keys & Project ---

type maskedKeyView struct {
	ID        string                  `json:"id"`
	Provider  string                  `json:"provider"`
	Label     string                  `json:"label,omitempty"`
	BaseURL   string                  `json:"base_url,omitempty"`
	Model     string                  `json:"model,omitempty"`
	Priority  int                     `json:"priority"`
	Enabled   bool                    `json:"enabled"`
	HasKey    bool                    `json:"has_key"`
	MaskedKey string                  `json:"masked_key,omitempty"`
	Pinned    bool                    `json:"pinned,omitempty"`
	Health    *harness.KeyHealthEntry `json:"health,omitempty"`
}

func buildMaskedKeys(run *harness.Runner) []maskedKeyView {
	creds, _ := harness.LoadAllCredentials()
	var pool *harness.KeyPool
	pinned := ""
	if run != nil {
		pool = run.KeyPool()
		pinned = run.PinnedKeyID()
	}

	providers := make([]string, 0, len(creds))
	for p := range creds {
		providers = append(providers, p)
	}
	sort.Strings(providers)

	var out []maskedKeyView
	for _, p := range providers {
		for _, e := range creds[p] {
			view := maskedKeyView{
				ID:       e.ID,
				Provider: p,
				Label:    e.Label,
				BaseURL:  e.BaseURL,
				Model:    e.Model,
				Priority: e.Priority,
				Enabled:  e.Enabled,
				HasKey:   strings.TrimSpace(e.APIKey) != "",
				Pinned:   pinned != "" && pinned == e.ID,
			}
			if view.HasKey {
				view.MaskedKey = secretMask
			}
			if pool != nil {
				if h, ok := pool.HealthOf(e.ID); ok {
					hc := h
					view.Health = &hc
				}
			}
			out = append(out, view)
		}
	}
	return out
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, _ *http.Request) {
	eng, run := s.activeState()
	reg := s.activeRegistry()

	keys := buildMaskedKeys(run)
	hasGlobalKeys := false
	for _, k := range keys {
		if k.Enabled && (k.HasKey || k.Provider == "ollama") {
			hasGlobalKeys = true
			break
		}
	}

	ws := ""
	mode := "code"
	branch := ""
	var settings map[string]string
	if eng != nil {
		ws = eng.Workspace
		mode = eng.GetMode()
		branch = eng.Branch
		settings, _ = eng.GetAllProjectSettings()
		delete(settings, "api_key")
	}

	hasTetherDir := ws != "" && fileExists(filepath.Join(ws, ".tether", "session.db"))
	hasIgnore := ws != "" && (fileExists(filepath.Join(ws, ".tether", "ignore")) || fileExists(filepath.Join(ws, ".tetherignore")))
	hasGitignore := ws != "" && fileExists(filepath.Join(ws, ".gitignore"))
	serena := mcp.EvaluateSerenaSuggestionWithRegistry(reg, mode, false)

	activeProv := ""
	activeModel := ""
	pinnedKey := ""
	if run != nil {
		activeProv = run.ProviderName()
		activeModel = run.ModelName()
		pinnedKey = run.PinnedKeyID()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"headless":          IsHeadlessEnvironment(),
		"has_global_keys":   hasGlobalKeys,
		"keys":              keys,
		"presets":           harness.ListProviderPresets(),
		"active_provider":   activeProv,
		"active_model":      activeModel,
		"pinned_key_id":     pinnedKey,
		"workspace":         ws,
		"branch":            branch,
		"mode":              mode,
		"project_ready":     hasTetherDir && hasIgnore,
		"has_tether_dir":    hasTetherDir,
		"has_ignore":        hasIgnore,
		"has_gitignore":     hasGitignore,
		"project_settings":  settings,
		"serena_suggestion": serena,
	})
}

func (s *Server) handleKeysProbe(w http.ResponseWriter, r *http.Request) {
	eng, run := s.activeState()

	var req struct {
		Provider string `json:"provider"`
		APIKey   string `json:"api_key"`
		BaseURL  string `json:"base_url"`
		Model    string `json:"model"`
	}
	if err := decodeOptionalJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if strings.TrimSpace(req.Provider) != "" {
		prov := strings.ToLower(strings.TrimSpace(req.Provider))
		tempPool := harness.NewKeyPoolWithCandidates(eng, nil, []harness.KeyCandidate{{
			Provider: prov,
			Entry: harness.ProviderKeyEntry{
				Label:    prov,
				APIKey:   strings.TrimSpace(req.APIKey),
				BaseURL:  strings.TrimSpace(req.BaseURL),
				Model:    strings.TrimSpace(req.Model),
				Priority: 1,
				Enabled:  true,
			},
		}})
		results := tempPool.ProbeAll(ctx)
		writeJSON(w, http.StatusOK, map[string]any{
			"results": results,
		})
		return
	}

	if run == nil {
		writeError(w, http.StatusServiceUnavailable, "runner unavailable")
		return
	}
	pool := run.KeyPool()
	if pool == nil {
		pool = harness.NewKeyPool(eng, nil)
	}
	results := pool.ProbeAll(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"results": results,
		"summary": run.ListKeysSummary(),
	})
}

func (s *Server) handleKeysPut(w http.ResponseWriter, r *http.Request) {
	_, run := s.activeState()

	var req struct {
		Provider string                     `json:"provider"`
		APIKey   string                     `json:"api_key"`
		Label    string                     `json:"label"`
		BaseURL  string                     `json:"base_url"`
		Model    string                     `json:"model"`
		Priority int                        `json:"priority"`
		Enabled  *bool                      `json:"enabled"`
		Entries  []harness.ProviderKeyEntry `json:"entries"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	prov := strings.ToLower(strings.TrimSpace(req.Provider))
	if prov == "" {
		writeError(w, http.StatusBadRequest, "provider is required")
		return
	}

	existingCreds, err := harness.LoadAllCredentials()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	existingByID := make(map[string]harness.ProviderKeyEntry)
	for _, e := range existingCreds[prov] {
		existingByID[e.ID] = e
	}

	if req.Entries != nil {
		merged := make([]harness.ProviderKeyEntry, 0, len(req.Entries))
		for _, e := range req.Entries {
			if strings.TrimSpace(e.APIKey) == secretMask {
				if prev, ok := existingByID[strings.TrimSpace(e.ID)]; ok {
					e.APIKey = prev.APIKey
				} else {
					writeError(w, http.StatusBadRequest, fmt.Sprintf("cannot preserve masked key for unknown id %q", e.ID))
					return
				}
			}
			merged = append(merged, e)
		}
		if err := harness.SaveProviderKeys(prov, merged); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		key := strings.TrimSpace(req.APIKey)
		if key == "" && prov != "ollama" {
			writeError(w, http.StatusBadRequest, "api_key or entries is required")
			return
		}
		if key == secretMask {
			writeError(w, http.StatusBadRequest, "provide a concrete api_key or entries[] with existing key id")
			return
		}
		if req.BaseURL != "" || req.Model != "" || req.Label != "" || req.Priority > 0 || req.Enabled != nil {
			enabled := true
			if req.Enabled != nil {
				enabled = *req.Enabled
			}
			prio := req.Priority
			if prio <= 0 {
				prio = 1
			}
			entries := append([]harness.ProviderKeyEntry{{
				Label:    req.Label,
				APIKey:   key,
				BaseURL:  req.BaseURL,
				Model:    req.Model,
				Priority: prio,
				Enabled:  enabled,
			}}, existingCreds[prov]...)
			if err := harness.SaveProviderKeys(prov, entries); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		} else if err := harness.SaveAPIKey(prov, key); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	if run != nil {
		_ = run.ReloadKeyPool()
		if strings.EqualFold(run.ProviderName(), prov) && strings.TrimSpace(req.Model) != "" {
			_, _ = run.SwitchModel(req.Model)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"provider": prov,
		"keys":     buildMaskedKeys(run),
	})
}

func (s *Server) handleProjectGet(w http.ResponseWriter, _ *http.Request) {
	eng, run := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	reg := s.activeRegistry()
	settings, _ := eng.GetAllProjectSettings()
	delete(settings, "api_key")
	kbStats, _ := eng.KBStatus()
	mode := eng.GetMode()

	resp := map[string]any{
		"workspace":         eng.Workspace,
		"branch":            eng.Branch,
		"session_id":        eng.SessionID,
		"mode":              mode,
		"settings":          settings,
		"kb_stats":          kbStats,
		"has_tether_dir":    fileExists(filepath.Join(eng.Workspace, ".tether", "session.db")),
		"has_ignore_file":   fileExists(filepath.Join(eng.Workspace, ".tether", "ignore")) || fileExists(filepath.Join(eng.Workspace, ".tetherignore")),
		"serena_suggestion": mcp.EvaluateSerenaSuggestionWithRegistry(reg, mode, false),
	}
	if run != nil {
		resp["provider"] = run.ProviderName()
		resp["model"] = run.ModelName()
		resp["pinned_key_id"] = run.PinnedKeyID()
		resp["thread_id"] = run.ThreadID()
	}
	writeJSON(w, http.StatusOK, resp)
}

func expandUserHome(raw string) string {
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if raw == "~" {
				return home
			}
			return filepath.Join(home, raw[2:])
		}
	}
	return raw
}

func (s *Server) handleProjectOpen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		Init bool   `json:"init"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rawPath := expandUserHome(strings.TrimSpace(req.Path))
	if rawPath == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	absPath, err := filepath.Abs(rawPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	info, err := os.Stat(absPath)
	if err != nil || !info.IsDir() {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("directory %q does not exist", absPath))
		return
	}

	ws, err := core.DetectWorkspace(absPath)
	if err != nil || ws == "" {
		ws = absPath
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	select {
	case <-s.shutdownCh:
		writeError(w, http.StatusServiceUnavailable, "server is shutting down")
		return
	default:
	}

	s.abortActiveChat("project switched")
	s.closeAllTerminals()

	newEng, err := core.NewEngine(ws)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("open workspace: %v", err))
		return
	}
	if req.Init {
		_ = newEng.EnsureGitignore()
		_, _, _ = mount.InitIgnoreFile(ws, newEng.WriteDB())
		for k, v := range core.DetectProjectSettings(ws) {
			_ = newEng.SetProjectSetting(k, v)
		}
	}

	newCfg := s.baseRunnerCfg
	newRun := harness.NewRunner(newEng, newCfg)
	s.bindRunner(newRun)
	newReg := mcp.NewClientRegistry(ws)

	s.stateMu.Lock()
	oldRun := s.runner
	oldEng := s.engine
	s.engine = newEng
	s.runner = newRun
	s.registry = newReg
	s.ownsWorkspace = true
	s.stateMu.Unlock()
	s.clearAllMCPTested()

	if oldRun != nil {
		_ = oldRun.Close()
	}
	if oldEng != nil {
		_ = oldEng.Close()
	}

	s.hub.Publish("mount.updated", map[string]any{"workspace": ws})
	s.hub.Publish("plan.updated", map[string]any{"workspace": ws, "mode": newEng.GetMode()})
	s.hub.Publish("diff.updated", map[string]any{"workspace": ws})

	s.handleProjectGet(w, r)
}

func (s *Server) handleProjectBrowse(w http.ResponseWriter, r *http.Request) {
	eng, _ := s.activeState()
	raw := strings.TrimSpace(r.URL.Query().Get("path"))
	if raw == "" {
		if eng != nil && eng.Workspace != "" {
			raw = eng.Workspace
		} else if home, err := os.UserHomeDir(); err == nil {
			raw = home
		} else {
			raw = "."
		}
	}
	raw = expandUserHome(raw)
	abs, err := filepath.Abs(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	type browseEntry struct {
		Name            string `json:"name"`
		Path            string `json:"path"`
		IsTetherProject bool   `json:"is_tether_project"`
		IsGitRepo       bool   `json:"is_git_repo"`
	}
	var out []browseEntry
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		full := filepath.Join(abs, entry.Name())
		out = append(out, browseEntry{
			Name:            entry.Name(),
			Path:            full,
			IsTetherProject: fileExists(filepath.Join(full, ".tether", "session.db")),
			IsGitRepo:       fileExists(filepath.Join(full, ".git")),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	parent := filepath.Dir(abs)
	if parent == abs {
		parent = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current": abs,
		"parent":  parent,
		"entries": out,
	})
}

// --- Mounts, Files & Ignore ---

func (s *Server) handleMountsList(w http.ResponseWriter, r *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	m := eng.Mount()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	idx := tetherctx.NewIndexer(eng.Workspace)
	var files []tetherctx.FileEntry
	if q != "" {
		files = idx.FuzzyFilter(q, 200)
	} else {
		files, _ = idx.Scan()
	}

	leases, _ := m.ListActiveLeases()
	leaseByPath := make(map[string]mount.LeaseInfo)
	for _, l := range leases {
		for _, f := range l.Files {
			leaseByPath[f] = l
		}
	}

	type mountedFileView struct {
		RelPath        string           `json:"rel_path"`
		Ext            string           `json:"ext"`
		SizeBytes      int64            `json:"size_bytes"`
		SizeHuman      string           `json:"size_human"`
		AccessMode     mount.AccessMode `json:"access_mode"`
		Source         string           `json:"source"`
		LeaseCrewID    string           `json:"lease_crew_id,omitempty"`
		LeaseExpiresAt string           `json:"lease_expires_at,omitempty"`
	}

	items := make([]mountedFileView, 0, len(files))
	for _, f := range files {
		mode := mount.AccessRW
		source := "default"
		if ov, ok := m.GetMountedMode(f.RelPath); ok {
			mode = ov
			source = "manual"
		} else if mount.IsPolicyConfigRel(f.RelPath) {
			mode = mount.AccessGuarded
			source = "builtin"
		}
		item := mountedFileView{
			RelPath:    f.RelPath,
			Ext:        f.Ext,
			SizeBytes:  f.SizeBytes,
			SizeHuman:  f.SizeHuman,
			AccessMode: mode,
			Source:     source,
		}
		if l, ok := leaseByPath[f.RelPath]; ok {
			item.LeaseCrewID = l.CrewID
			item.LeaseExpiresAt = l.ExpiresAt.Format(time.RFC3339)
		}
		items = append(items, item)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"workspace":        eng.Workspace,
		"mode":             eng.GetMode(),
		"files":            items,
		"leases":           leases,
		"builtin_patterns": mount.BuiltinSecretPatterns(),
		"custom_rules":     m.CustomRules(),
	})
}

func (s *Server) handleMountsModePatch(w http.ResponseWriter, r *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	var req struct {
		Path       string           `json:"path"`
		AccessMode mount.AccessMode `json:"access_mode"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := rejectSymlinkSegments(eng.Workspace, req.Path); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	relSlash, _, isOutside, err := mount.ResolveWithinWorkspace(req.Path, eng.Workspace)
	if err != nil || isOutside {
		writeError(w, http.StatusForbidden, "path escapes workspace")
		return
	}
	if rRule, secret := mount.MatchesBuiltinSecret(relSlash); secret {
		writeError(w, http.StatusForbidden, fmt.Sprintf("cannot override built-in protected path %q (%s)", relSlash, rRule.Pattern))
		return
	}

	if err := eng.Mount().SetMountedMode(relSlash, req.AccessMode, "manual"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.hub.Publish("mount.updated", map[string]any{
		"path":        relSlash,
		"access_mode": req.AccessMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"path":        relSlash,
		"access_mode": req.AccessMode,
	})
}

func (s *Server) handleFileContent(w http.ResponseWriter, r *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	rawPath := strings.TrimSpace(r.URL.Query().Get("path"))
	if rawPath == "" {
		writeError(w, http.StatusBadRequest, "path query parameter required")
		return
	}
	if err := rejectSymlinkSegments(eng.Workspace, rawPath); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}

	dec := eng.Mount().AuthorizePath(rawPath, mount.OpRead, eng.GetMode())
	if !dec.Allowed {
		writeError(w, http.StatusForbidden, dec.Reason)
		return
	}
	if dec.IsDir {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%q is a directory", dec.RelPath))
		return
	}
	if dec.IsMissing {
		writeError(w, http.StatusNotFound, fmt.Sprintf("file %q not found", dec.RelPath))
		return
	}

	const maxBytes = 512 * 1024
	content, cleanRel, err := tetherctx.SafeReadWorkspaceFileWithMount(eng.Mount(), eng.Workspace, dec.RelPath, maxBytes)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	info, _ := os.Stat(dec.CanonicalPath)
	var size int64
	if info != nil {
		size = info.Size()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":        cleanRel,
		"access_mode": dec.AccessMode,
		"content":     content,
		"size_bytes":  size,
		"truncated":   size > maxBytes,
	})
}

func (s *Server) handleIgnoreGet(w http.ResponseWriter, _ *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	srcPath, exists, rules, errs := mount.InspectIgnoreFile(eng.Workspace)
	content := ""
	if srcPath != "" {
		if raw, err := os.ReadFile(srcPath); err == nil {
			content = string(raw)
		}
	}
	var errMsgs []string
	for _, err := range errs {
		errMsgs = append(errMsgs, err.Error())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source_path":      srcPath,
		"exists":           exists,
		"content":          content,
		"builtin_patterns": mount.BuiltinSecretPatterns(),
		"custom_rules":     rules,
		"parse_errors":     errMsgs,
	})
}

func (s *Server) handleIgnorePost(w http.ResponseWriter, r *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	var req struct {
		Content *string      `json:"content"`
		Rules   []mount.Rule `json:"rules"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	rules := req.Rules
	if req.Content != nil {
		parsed, errs := mount.ParseIgnoreContent(*req.Content)
		if len(errs) > 0 {
			msgs := make([]string, len(errs))
			for i, e := range errs {
				msgs[i] = e.Error()
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":        strings.Join(msgs, "; "),
				"parse_errors": msgs,
			})
			return
		}
		rules = parsed
	}

	m := eng.Mount()
	if err := m.SaveCustomRules(rules); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	purged, recErr := eng.ReconcileKB(func(relPath string) bool {
		return !m.AuthorizePath(relPath, mount.OpRead, "code").Allowed
	})
	if recErr != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("saved ignore rules, but KB reconcile failed: %v", recErr))
		return
	}

	s.hub.Publish("mount.updated", map[string]any{
		"ignore_updated": true,
		"purged_kb":      purged,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"custom_rules": m.CustomRules(),
		"purged_kb":    purged,
	})
}

func (s *Server) handleIgnoreTest(w http.ResponseWriter, r *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	var req struct {
		Pattern string       `json:"pattern"`
		Content string       `json:"content"`
		Rules   []mount.Rule `json:"rules"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var res mount.SimulationResult
	var err error
	switch {
	case strings.TrimSpace(req.Pattern) != "":
		res, err = mount.SimulateRule(eng.Workspace, req.Pattern)
	case strings.TrimSpace(req.Content) != "":
		parsed, errs := mount.ParseIgnoreContent(req.Content)
		res, err = mount.SimulateIgnoreRules(eng.Workspace, parsed)
		for _, pe := range errs {
			res.InvalidPatterns = append(res.InvalidPatterns, pe.Error())
		}
	default:
		res, err = mount.SimulateIgnoreRules(eng.Workspace, req.Rules)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleIgnoreConvertGitignore(w http.ResponseWriter, r *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	var req struct {
		Apply bool `json:"apply"`
	}
	if err := decodeOptionalJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	converted, skipped, err := mount.ConvertGitignoreToTetherIgnore(eng.Workspace)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, ".gitignore not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	purged := 0
	if req.Apply {
		rules, _ := mount.ParseIgnoreContent(converted)
		if err := eng.Mount().SaveCustomRules(rules); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		purged, _ = eng.ReconcileKB(func(relPath string) bool {
			return !eng.Mount().AuthorizePath(relPath, mount.OpRead, "code").Allowed
		})
		s.hub.Publish("mount.updated", map[string]any{
			"ignore_updated": true,
			"purged_kb":      purged,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"converted": converted,
		"skipped":   skipped,
		"applied":   req.Apply,
		"purged_kb": purged,
	})
}

// --- Chat, Threads, Approvals & Condenser ---

func (s *Server) handleThreadsList(w http.ResponseWriter, _ *http.Request) {
	eng, run := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	activeID := eng.SessionID
	if run != nil && run.ThreadID() != "" {
		activeID = run.ThreadID()
		_, _ = eng.EnsureThread(activeID, "Main Thread")
	}
	threads, err := eng.ListThreads(50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active_thread_id": activeID,
		"threads":          threads,
	})
}

func (s *Server) handleThreadsCreateOrSelect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Mode  string `json:"mode"`
	}
	if err := decodeOptionalJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if s.activeTurnCancel != nil {
		writeError(w, http.StatusConflict, errTurnAlreadyActive.Error())
		return
	}

	eng, run := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}

	if strings.TrimSpace(req.ID) != "" {
		if run != nil {
			if err := run.LoadThread(req.ID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					writeError(w, http.StatusNotFound, "thread not found")
					return
				}
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		th, err := eng.GetThread(req.ID)
		if err != nil {
			writeError(w, http.StatusNotFound, "thread not found")
			return
		}
		writeJSON(w, http.StatusOK, th)
		return
	}

	th, err := eng.CreateThread("", req.Title, req.Mode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if run != nil {
		run.ClearHistory()
		run.SetThreadID(th.ID)
	}
	writeJSON(w, http.StatusCreated, th)
}

func (s *Server) handleThreadEvents(w http.ResponseWriter, r *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "thread id required")
		return
	}
	th, err := eng.GetThread(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "thread not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	events, err := eng.ListThreadEventsForActor(id, "", 500)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"thread": th,
		"events": events,
	})
}

func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt   string `json:"prompt"`
		ThreadID string `json:"thread_id"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}

	turnID, err := s.startChatTurn(prompt, req.ThreadID)
	if err != nil {
		if errors.Is(err, errTurnAlreadyActive) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	_, run := s.activeState()
	threadID := ""
	if run != nil {
		threadID = run.ThreadID()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"accepted":  true,
		"turn_id":   turnID,
		"thread_id": threadID,
	})
}

func (s *Server) handleChatAbort(w http.ResponseWriter, _ *http.Request) {
	aborted := s.abortActiveChat("aborted by user")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"aborted": aborted,
	})
}

func (s *Server) handleChatBtw(w http.ResponseWriter, r *http.Request) {
	_, run := s.activeState()
	if run == nil {
		writeError(w, http.StatusServiceUnavailable, "runner unavailable")
		return
	}
	var req struct {
		Question string `json:"question"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ans, err := run.RunSideChat(r.Context(), req.Question)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"question": strings.TrimSpace(req.Question),
		"answer":   ans,
		"usage":    run.UsageMetrics("btw"),
	})
}

func (s *Server) handleChatBtwPromote(w http.ResponseWriter, r *http.Request) {
	_, run := s.activeState()
	if run == nil {
		writeError(w, http.StatusServiceUnavailable, "runner unavailable")
		return
	}
	var req struct {
		Question string `json:"question"`
		Answer   string `json:"answer"`
	}
	if err := decodeOptionalJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := run.PromoteBtwToMain(req.Question, req.Answer); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleChatBtwPin(w http.ResponseWriter, r *http.Request) {
	_, run := s.activeState()
	if run == nil {
		writeError(w, http.StatusServiceUnavailable, "runner unavailable")
		return
	}
	var req struct {
		Topic    string `json:"topic"`
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := decodeOptionalJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := run.PinBtwAsDecision(req.Topic, req.Decision, req.Reason); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleChatApproval(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      string `json:"id"`
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	verdictStr := strings.ToLower(strings.TrimSpace(req.Verdict))
	var v mcp.Verdict
	switch verdictStr {
	case "once", "approve", "allow":
		v = mcp.VerdictOnce
	case "folder", "prefix":
		v = mcp.VerdictFolder
	case "session", "all":
		v = mcp.VerdictSession
	case "reject", "deny":
		v = mcp.VerdictReject
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid verdict %q (expected once, folder, session, reject)", req.Verdict))
		return
	}

	s.approvalMu.Lock()
	var target *pendingApproval
	id := strings.TrimSpace(req.ID)
	if id != "" {
		target = s.pendingApprovals[id]
	} else if len(s.pendingApprovals) == 1 {
		for _, item := range s.pendingApprovals {
			target = item
			id = item.ID
		}
	}
	if target != nil {
		delete(s.pendingApprovals, id)
	}
	s.approvalMu.Unlock()

	if target == nil {
		writeError(w, http.StatusNotFound, "pending approval request not found")
		return
	}

	select {
	case target.replyCh <- approvalReply{verdict: v, reason: strings.TrimSpace(req.Reason), ok: true}:
	default:
	}

	s.hub.Publish("approval.resolved", map[string]any{
		"id":      id,
		"verdict": verdictStr,
		"reason":  strings.TrimSpace(req.Reason),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"id":      id,
		"verdict": verdictStr,
	})
}

func (s *Server) handleChatCompact(w http.ResponseWriter, r *http.Request) {
	_, run := s.activeState()
	if run == nil {
		writeError(w, http.StatusServiceUnavailable, "runner unavailable")
		return
	}
	force := true
	var req struct {
		Force *bool `json:"force"`
	}
	if err := decodeOptionalJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Force != nil {
		force = *req.Force
	}

	res, err := run.CondenseHistory(r.Context(), force)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- Crews ---

func (s *Server) handleCrewsList(w http.ResponseWriter, r *http.Request) {
	eng, run := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	threadID := ""
	if r.URL.Query().Get("all") != "1" && run != nil {
		threadID = run.ThreadID()
	}
	crews, err := eng.ListCrewSessions(threadID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	leases, _ := eng.Mount().ListActiveLeases()
	writeJSON(w, http.StatusOK, map[string]any{
		"crews":  crews,
		"leases": leases,
	})
}

func (s *Server) handleCrewMessages(w http.ResponseWriter, r *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	cs, err := eng.GetCrewSession(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "crew session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	msgs, err := eng.ListThreadEventsForActor(cs.ParentThreadID, id, 500)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"crew":     cs,
		"messages": msgs,
	})
}

func (s *Server) handleCrewAbort(w http.ResponseWriter, r *http.Request) {
	eng, run := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if _, err := eng.GetCrewSession(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "crew session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if run != nil {
		if err := run.AbortCrew(id, "aborted via Web UI"); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		_ = eng.Mount().ReleaseLeaseLock(id)
		_ = eng.CloseCrewSession(id, "aborted", "aborted via Web UI")
		s.hub.Publish("lease.updated", map[string]any{"crew_id": id, "action": "released"})
		s.hub.Publish("crew.status", map[string]any{"crew_id": id, "status": "aborted"})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"crew_id": id,
	})
}

func (s *Server) handleCrewReleaseLock(w http.ResponseWriter, r *http.Request) {
	eng, run := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "crew id required")
		return
	}
	if run != nil {
		if err := run.ForceReleaseCrewLease(id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		if err := eng.Mount().ReleaseLeaseLock(id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.hub.Publish("lease.updated", map[string]any{"crew_id": id, "action": "released"})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"crew_id": id,
	})
}

// --- Diffs & Safe 1-Click Undo ---

func (s *Server) handleDiffsGet(w http.ResponseWriter, _ *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	store := s.activeSnapshotStore()
	indexes, err := store.ListIndexes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type diffEntryView struct {
		Path               string         `json:"path"`
		State              snapshot.State `json:"state"`
		CrewID             string         `json:"crew_id,omitempty"`
		PostHash           string         `json:"post_hash,omitempty"`
		ActualHash         string         `json:"actual_hash,omitempty"`
		ExternallyModified bool           `json:"externally_modified"`
		Redacted           bool           `json:"redacted,omitempty"`
		Diff               string         `json:"diff,omitempty"`
	}
	type diffTurnView struct {
		Turn    string          `json:"turn"`
		CrewID  string          `json:"crew_id,omitempty"`
		Created time.Time       `json:"created"`
		Entries []diffEntryView `json:"entries"`
	}

	turns := make([]diffTurnView, 0, len(indexes))
	for _, idx := range indexes {
		tv := diffTurnView{
			Turn:    idx.Turn,
			CrewID:  idx.CrewID,
			Created: idx.Created,
			Entries: make([]diffEntryView, 0, len(idx.Entries)),
		}
		for _, e := range idx.Entries {
			dec := eng.Mount().AuthorizePath(e.Path, mount.OpRead, "code")
			if !dec.Allowed {
				tv.Entries = append(tv.Entries, diffEntryView{
					Path:     e.Path,
					State:    e.State,
					CrewID:   e.CrewID,
					PostHash: e.PostHash,
					Redacted: true,
				})
				continue
			}
			extMod, actualHash := store.CheckFileStatus(e.Path, e.PostHash, eng.Workspace)
			before := ""
			if e.Backup != "" {
				if b, berr := store.ReadBackup(e.Backup); berr == nil {
					before = b
				}
			}
			after := ""
			if !dec.IsDir && !dec.IsMissing {
				if cur, _, rerr := tetherctx.SafeReadWorkspaceFileWithMount(eng.Mount(), eng.Workspace, dec.RelPath, 256*1024); rerr == nil {
					after = cur
				}
			}
			tv.Entries = append(tv.Entries, diffEntryView{
				Path:               e.Path,
				State:              e.State,
				CrewID:             e.CrewID,
				PostHash:           e.PostHash,
				ActualHash:         actualHash,
				ExternallyModified: extMod,
				Diff:               preview.Diff(before, after),
			})
		}
		turns = append(turns, tv)
	}

	gitStatus, gitDiff := readGitDiffSummary(eng.Workspace)
	writeJSON(w, http.StatusOK, map[string]any{
		"turns":      turns,
		"git_status": gitStatus,
		"git_diff":   gitDiff,
	})
}

func readGitDiffSummary(workspace string) (statusOut, diffOut string) {
	if !fileExists(filepath.Join(workspace, ".git")) {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stCmd := exec.CommandContext(ctx, "git", "status", "--short")
	stCmd.Dir = workspace
	if b, err := stCmd.Output(); err == nil {
		statusOut = strings.TrimSpace(string(b))
	}
	dfCmd := exec.CommandContext(ctx, "git", "diff", "--no-ext-diff")
	dfCmd.Dir = workspace
	if b, err := dfCmd.Output(); err == nil {
		const maxDiffBytes = 64 * 1024
		if len(b) > maxDiffBytes {
			diffOut = string(b[:maxDiffBytes]) + "\n... [git diff truncated] ..."
		} else {
			diffOut = string(b)
		}
	}
	return statusOut, diffOut
}

func writeUndoError(w http.ResponseWriter, err error, defaultStatus int, forceConflict bool, fields map[string]any) {
	conflict := forceConflict || errors.Is(err, snapshot.ErrExternalModification)
	status := defaultStatus
	if conflict {
		status = http.StatusConflict
	}
	if fields == nil {
		fields = map[string]any{}
	}
	fields["error"] = err.Error()
	fields["conflict"] = conflict
	writeJSON(w, status, fields)
}

func (s *Server) handleDiffsUndoTurn(w http.ResponseWriter, r *http.Request) {
	eng, run := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	var req struct {
		Turn  string `json:"turn"`
		Force bool   `json:"force"`
	}
	if err := decodeOptionalJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	store := s.activeSnapshotStore()
	turn := strings.TrimSpace(req.Turn)
	if turn == "" {
		last, err := store.LastTurn()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if last == "" {
			writeError(w, http.StatusNotFound, "no snapshot turn recorded yet")
			return
		}
		turn = last
	}

	results, err := store.RestoreTurn(turn, eng.Workspace, req.Force)
	if err != nil {
		writeUndoError(w, err, http.StatusInternalServerError, false, map[string]any{
			"turn":    turn,
			"results": results,
		})
		return
	}

	if run != nil && len(results) > 0 {
		run.QueueTailNotice(fmt.Sprintf("User undid turn %s via Web UI (%d file(s))", turn, len(results)))
	}
	s.hub.Publish("diff.updated", map[string]any{"turn": turn})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"turn":    turn,
		"results": results,
	})
}

func (s *Server) handleDiffsUndoFile(w http.ResponseWriter, r *http.Request) {
	eng, run := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	var req struct {
		Turn  string `json:"turn"`
		Path  string `json:"path"`
		Force bool   `json:"force"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	relPath := strings.TrimSpace(req.Path)
	if relPath == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}

	store := s.activeSnapshotStore()
	turn := strings.TrimSpace(req.Turn)
	if turn == "" {
		norm := filepath.ToSlash(filepath.Clean(relPath))
		indexes, err := store.ListIndexes()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, idx := range indexes {
			for _, e := range idx.Entries {
				if e.Path == norm || e.Path == relPath {
					turn = idx.Turn
					break
				}
			}
			if turn != "" {
				break
			}
		}
		if turn == "" {
			writeError(w, http.StatusNotFound, fmt.Sprintf("no snapshot found for %q", relPath))
			return
		}
	}

	res, err := store.RestoreFile(turn, relPath, eng.Workspace, req.Force)
	if err != nil {
		writeUndoError(w, err, http.StatusBadRequest, res.Conflict, map[string]any{
			"result": res,
		})
		return
	}

	if run != nil {
		run.QueueTailNotice(fmt.Sprintf("User restored %s from snapshot turn %s via Web UI", res.Path, turn))
	}
	s.hub.Publish("diff.updated", map[string]any{"turn": turn, "path": res.Path})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"result": res,
	})
}

func (s *Server) handleDiffsUndoCrew(w http.ResponseWriter, r *http.Request) {
	eng, run := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	var req struct {
		CrewID string `json:"crew_id"`
		Force  bool   `json:"force"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	crewID := strings.TrimSpace(req.CrewID)
	if crewID == "" {
		writeError(w, http.StatusBadRequest, "crew_id is required")
		return
	}

	store := s.activeSnapshotStore()
	results, err := store.RestoreCrew(crewID, eng.Workspace, req.Force)
	if err != nil {
		writeUndoError(w, err, http.StatusInternalServerError, false, map[string]any{
			"crew_id": crewID,
			"results": results,
		})
		return
	}

	if run != nil && len(results) > 0 {
		run.QueueTailNotice(fmt.Sprintf("User undid crew %s changes (%d file(s)) via Web UI", crewID, len(results)))
	}
	s.hub.Publish("diff.updated", map[string]any{"crew_id": crewID})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"crew_id": crewID,
		"results": results,
	})
}

// --- Plan, Todos, Tokens & Grants ---

func (s *Server) handlePlanGet(w http.ResponseWriter, _ *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	text, err := harness.ReadPlanWithMount(eng.Mount(), eng.Workspace)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":    ".tether/PLAN.md",
		"mode":    eng.GetMode(),
		"content": text,
	})
}

func (s *Server) handlePlanPut(w http.ResponseWriter, r *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	var req struct {
		Content   string                          `json:"content"`
		Questions []harness.ClarificationQuestion `json:"questions"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	content := req.Content
	if len(req.Questions) > 0 {
		answers := harness.FormatClarificationAnswers(req.Questions)
		if answers != "" {
			if strings.TrimSpace(content) == "" {
				existing, _ := harness.ReadPlanWithMount(eng.Mount(), eng.Workspace)
				content = existing
			}
			if strings.TrimSpace(content) != "" {
				content = strings.TrimRight(content, "\n") + "\n\n" + answers + "\n"
			} else {
				content = answers + "\n"
			}
		}
	}
	if err := harness.WritePlanWithMount(eng.Mount(), eng.Workspace, content); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	s.hub.Publish("plan.updated", map[string]any{
		"bytes": len(content),
		"mode":  eng.GetMode(),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"bytes":   len(content),
		"content": content,
	})
}

func (s *Server) handlePlanStartBuilding(w http.ResponseWriter, r *http.Request) {
	_, run := s.activeState()
	if run == nil {
		writeError(w, http.StatusServiceUnavailable, "runner unavailable")
		return
	}
	var req struct {
		RunCritic bool `json:"run_critic"`
	}
	if err := decodeOptionalJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := run.StartBuilding(r.Context(), req.RunCritic)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.hub.Publish("plan.updated", map[string]any{
		"mode":  res.Mode,
		"ready": res.Ready,
	})
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleTodosList(w http.ResponseWriter, r *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	includeCompleted := r.URL.Query().Get("all") != "0"
	todos, err := eng.ListTodos(includeCompleted)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"todos": todos,
	})
}

func (s *Server) handleTokensGet(w http.ResponseWriter, _ *http.Request) {
	_, run := s.activeState()
	if run == nil {
		writeError(w, http.StatusServiceUnavailable, "runner unavailable")
		return
	}
	all := run.AllUsageMetrics()
	if all == nil {
		all = map[string]harness.UsageMetrics{}
	}
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	entries := make([]harness.UsageMetrics, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, all[id])
	}
	mainMetrics := run.UsageMetrics("main")
	writeJSON(w, http.StatusOK, map[string]any{
		"by_usage_id":      all,
		"entries":          entries,
		"main":             mainMetrics,
		"usage_id":         mainMetrics.UsageID,
		"cache_hit_tokens": mainMetrics.CachedTokens,
		"context_window":   run.ContextWindowTokens(),
		"provider":         run.ProviderName(),
		"model":            run.ModelName(),
		"pinned_key_id":    run.PinnedKeyID(),
		"summary":          run.TokensSummary(),
	})
}

func (s *Server) handleGrantsList(w http.ResponseWriter, _ *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	grants, err := eng.ListApprovalGrants()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": grants})
}

func (s *Server) handleGrantsDelete(w http.ResponseWriter, _ *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	revoked, err := eng.RevokeApprovalGrants()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": revoked})
}

// --- Customize (/mcp, /skills, /memory), Settings & Doctor ---

func (s *Server) handleMCPList(w http.ResponseWriter, _ *http.Request) {
	eng, run := s.activeState()
	reg := s.activeRegistry()
	if reg == nil {
		writeError(w, http.StatusServiceUnavailable, "mcp registry unavailable")
		return
	}
	masked, err := reg.MaskedView()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if masked == nil {
		masked = []mcp.ExternalMCPConfig{}
	}

	mode := "code"
	if eng != nil {
		mode = eng.GetMode()
	}
	var activeConns []string
	var loadErrs map[string]string
	if run != nil && run.ExternalMCP() != nil {
		activeConns = run.ExternalMCP().ActiveConnections()
		loadErrs = run.ExternalMCP().LoadErrors()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"servers":            masked,
		"active_connections": activeConns,
		"load_errors":        loadErrs,
		"serena_suggestion":  mcp.EvaluateSerenaSuggestionWithRegistry(reg, mode, false),
	})
}

type mcpPatchRequest struct {
	Transport      string            `json:"transport"`
	Command        string            `json:"command"`
	Args           []string          `json:"args"`
	Env            map[string]string `json:"env"`
	URL            string            `json:"url"`
	Headers        map[string]string `json:"headers"`
	Enabled        *bool             `json:"enabled"`
	Preset         string            `json:"preset"`
	EnabledTools   []string          `json:"enabled_tools"`
	DisabledTools  []string          `json:"disabled_tools"`
	Scope          string            `json:"scope"`
	ReplaceSecrets bool              `json:"replace_secrets"`
}

func (s *Server) handleMCPCreate(w http.ResponseWriter, r *http.Request) {
	reg := s.activeRegistry()
	if reg == nil {
		writeError(w, http.StatusServiceUnavailable, "mcp registry unavailable")
		return
	}
	var req struct {
		mcp.ExternalMCPConfig
		RequireTest bool `json:"require_test"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Enabled {
		fp := s.mcpConfigFingerprint(reg, req.ExternalMCPConfig, nil, false, false)
		if !s.isMCPTested(req.Name, fp) {
			writeError(w, http.StatusBadRequest, "test MCP server connection before enabling")
			return
		}
	}

	saved, err := reg.AddServer(req.ExternalMCPConfig)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "conflicts case-insensitively") {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, saved.MaskedView())
}

func (s *Server) handleMCPPatch(w http.ResponseWriter, r *http.Request) {
	reg := s.activeRegistry()
	if reg == nil {
		writeError(w, http.StatusServiceUnavailable, "mcp registry unavailable")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	existing, ok, err := reg.GetServer(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("MCP server %q not found", name))
		return
	}

	var req mcpPatchRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg := mcp.ExternalMCPConfig{
		Name:          existing.Name,
		Scope:         existing.Scope,
		Transport:     req.Transport,
		Command:       req.Command,
		Args:          req.Args,
		Env:           req.Env,
		URL:           req.URL,
		Headers:       req.Headers,
		Preset:        req.Preset,
		EnabledTools:  req.EnabledTools,
		DisabledTools: req.DisabledTools,
	}
	if req.Scope != "" {
		cfg.Scope = req.Scope
	}

	oldFP := s.mcpConfigFingerprint(reg, existing, &existing, false, false)
	candidate := mergeMCPPatchCandidate(existing, req)
	preserveEnv := len(req.Env) > 0 && !req.ReplaceSecrets
	preserveHdr := len(req.Headers) > 0 && !req.ReplaceSecrets
	newFP := s.mcpConfigFingerprint(reg, candidate, &existing, preserveEnv, preserveHdr)

	nextEnabled := existing.Enabled
	if req.Enabled != nil {
		nextEnabled = *req.Enabled
	}
	if nextEnabled && !s.isMCPTested(existing.Name, newFP) {
		msg := "test MCP server connection before enabling"
		if newFP != oldFP {
			msg = "test modified MCP server connection before enabling"
		}
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	updated, err := reg.UpsertServer(cfg, mcp.SparseUpdate{
		Enabled:           req.Enabled,
		ReplaceAllFields:  false,
		ReplaceAllSecrets: req.ReplaceSecrets,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if newFP != oldFP && !s.isMCPTested(existing.Name, newFP) {
		s.clearMCPTested(existing.Name)
	}
	writeJSON(w, http.StatusOK, updated.MaskedView())
}

func (s *Server) handleMCPDelete(w http.ResponseWriter, r *http.Request) {
	reg := s.activeRegistry()
	if reg == nil {
		writeError(w, http.StatusServiceUnavailable, "mcp registry unavailable")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if err := reg.RemoveServer(name, scope); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.clearMCPTested(name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

func (s *Server) handleMCPTest(w http.ResponseWriter, r *http.Request) {
	reg := s.activeRegistry()
	if reg == nil {
		writeError(w, http.StatusServiceUnavailable, "mcp registry unavailable")
		return
	}
	var req mcp.ExternalMCPConfig
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	mgr := mcp.NewClientManagerFromRegistry(reg)
	defer func() { _ = mgr.Close() }()

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	var res mcp.ServerTestResult
	var err error
	var testedFP string
	if strings.TrimSpace(req.Command) != "" || strings.TrimSpace(req.URL) != "" {
		res, err = mgr.TestServerConfig(ctx, req)
		if err == nil && res.OK {
			var existingPtr *mcp.ExternalMCPConfig
			if existing, ok, gerr := reg.GetServer(req.Name); gerr == nil && ok {
				existingPtr = &existing
			}
			testedFP = s.mcpConfigFingerprint(reg, req, existingPtr, false, false)
		}
	} else if strings.TrimSpace(req.Name) != "" {
		res, err = mgr.TestServer(ctx, req.Name)
		if err == nil && res.OK {
			if existing, ok, gerr := reg.GetServer(res.Name); gerr == nil && ok {
				testedFP = s.mcpConfigFingerprint(reg, existing, &existing, false, false)
			}
		}
	} else {
		writeError(w, http.StatusBadRequest, "name, command, or url required")
		return
	}

	if err != nil && !res.OK {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusBadGateway, res)
		return
	}
	if res.OK && strings.TrimSpace(res.Name) != "" && testedFP != "" {
		s.setMCPTested(res.Name, testedFP)
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleMCPImport(w http.ResponseWriter, r *http.Request) {
	reg := s.activeRegistry()
	if reg == nil {
		writeError(w, http.StatusServiceUnavailable, "mcp registry unavailable")
		return
	}
	var req struct {
		Source string `json:"source"`
		Scope  string `json:"scope"`
	}
	if err := decodeOptionalJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Source) == "" {
		req.Source = "all"
	}
	if strings.TrimSpace(req.Scope) == "" {
		req.Scope = mcp.ScopeGlobal
	}
	res, err := reg.ImportFromIDE(req.Source, req.Scope)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleMCPToggle(w http.ResponseWriter, r *http.Request) {
	reg := s.activeRegistry()
	if reg == nil {
		writeError(w, http.StatusServiceUnavailable, "mcp registry unavailable")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	existing, ok, err := reg.GetServer(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("MCP server %q not found", name))
		return
	}

	var req struct {
		Enabled     *bool `json:"enabled"`
		RequireTest bool  `json:"require_test"`
	}
	if err := decodeOptionalJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	nextEnabled := !existing.Enabled
	if req.Enabled != nil {
		nextEnabled = *req.Enabled
	}
	if nextEnabled {
		fp := s.mcpConfigFingerprint(reg, existing, &existing, false, false)
		if !s.isMCPTested(existing.Name, fp) {
			writeError(w, http.StatusBadRequest, "test MCP server connection before enabling")
			return
		}
	}

	updated, err := reg.SetServerEnabled(existing.Name, nextEnabled)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated.MaskedView())
}

func (s *Server) isMCPTested(name, fingerprint string) bool {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" || fingerprint == "" {
		return false
	}
	s.mcpTestMu.Lock()
	defer s.mcpTestMu.Unlock()
	return s.testedMCP[key] == fingerprint
}

func (s *Server) setMCPTested(name, fingerprint string) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" || fingerprint == "" {
		return
	}
	s.mcpTestMu.Lock()
	s.testedMCP[key] = fingerprint
	s.mcpTestMu.Unlock()
}

func (s *Server) clearMCPTested(name string) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return
	}
	s.mcpTestMu.Lock()
	delete(s.testedMCP, key)
	s.mcpTestMu.Unlock()
}

func (s *Server) clearAllMCPTested() {
	s.mcpTestMu.Lock()
	clear(s.testedMCP)
	s.mcpTestMu.Unlock()
}

func mergeMCPPatchCandidate(existing mcp.ExternalMCPConfig, req mcpPatchRequest) mcp.ExternalMCPConfig {
	merged := mcp.ExternalMCPConfig{
		Name:      existing.Name,
		Scope:     existing.Scope,
		Transport: strings.TrimSpace(req.Transport),
		Command:   req.Command,
		Args:      req.Args,
		Env:       req.Env,
		URL:       req.URL,
		Headers:   req.Headers,
		Preset:    req.Preset,
	}
	if strings.TrimSpace(req.Scope) != "" {
		merged.Scope = req.Scope
	}
	if merged.Transport == "" {
		merged.Transport = existing.Transport
	}
	if merged.Command == "" && merged.URL == "" {
		merged.Command = existing.Command
		if merged.Args == nil {
			merged.Args = existing.Args
		}
		merged.URL = existing.URL
	} else if merged.Command != "" && merged.Args == nil && existing.Command == merged.Command {
		merged.Args = existing.Args
	}
	if merged.Env == nil {
		merged.Env = existing.Env
	}
	if merged.Headers == nil {
		merged.Headers = existing.Headers
	}
	return merged
}

func (s *Server) mcpConfigFingerprint(reg *mcp.ClientRegistry, cfg mcp.ExternalMCPConfig, existing *mcp.ExternalMCPConfig, preserveEnvOmitted, preserveHdrOmitted bool) string {
	scope := strings.ToLower(strings.TrimSpace(cfg.Scope))
	if scope == "" && existing != nil {
		scope = strings.ToLower(strings.TrimSpace(existing.Scope))
	}
	if scope == "" {
		scope = mcp.ScopeProject
	}

	transport := strings.ToLower(strings.TrimSpace(cfg.Transport))
	if transport == "streamable-http" || transport == "streamable_http" {
		transport = mcp.TransportHTTP
	}
	if transport == "" {
		switch {
		case strings.TrimSpace(cfg.Command) != "":
			transport = mcp.TransportStdio
		case strings.TrimSpace(cfg.URL) != "":
			lowerURL := strings.ToLower(strings.TrimSpace(cfg.URL))
			if strings.HasSuffix(lowerURL, "/sse") || strings.Contains(lowerURL, "/sse?") {
				transport = mcp.TransportSSE
			} else {
				transport = mcp.TransportHTTP
			}
		}
	}

	var existingResolved mcp.ExternalMCPConfig
	if existing != nil && reg != nil {
		existingResolved, _ = reg.ResolveRuntimeSecrets(*existing)
	}
	var exEnvRaw, exHeadersRaw, exEnvRes, exHeadersRes map[string]string
	if existing != nil {
		exEnvRaw = existing.Env
		exHeadersRaw = existing.Headers
		exEnvRes = existingResolved.Env
		exHeadersRes = existingResolved.Headers
	}
	envMap := resolveMCPMapForFingerprint(cfg.Env, exEnvRaw, exEnvRes, preserveEnvOmitted, false)
	hdrMap := resolveMCPMapForFingerprint(cfg.Headers, exHeadersRaw, exHeadersRes, preserveHdrOmitted, true)

	h := sha256.New()
	writeField := func(s string) {
		_, _ = fmt.Fprintf(h, "%d:%s\x00", len(s), s)
	}
	writeField(scope)
	writeField(transport)
	writeField(strings.TrimSpace(cfg.Command))
	writeField(fmt.Sprintf("%d", len(cfg.Args)))
	for _, a := range cfg.Args {
		writeField(a)
	}
	writeField(strings.TrimSpace(cfg.URL))

	writeSortedMap := func(m map[string]string) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		writeField(fmt.Sprintf("%d", len(keys)))
		for _, k := range keys {
			writeField(k)
			writeField(m[k])
		}
	}
	writeSortedMap(envMap)
	writeSortedMap(hdrMap)
	return hex.EncodeToString(h.Sum(nil))
}

func resolveMCPMapForFingerprint(incoming, existingRaw, existingResolved map[string]string, preserveOmitted, isHeader bool) map[string]string {
	out := make(map[string]string)
	cleared := make(map[string]bool)
	lookupExisting := func(k string) (string, bool) {
		if existingResolved == nil {
			return "", false
		}
		if v, ok := existingResolved[k]; ok {
			return v, true
		}
		if isHeader {
			for ek, ev := range existingResolved {
				if strings.EqualFold(ek, k) {
					return ev, true
				}
			}
		}
		return "", false
	}
	isSecretPlaceholder := func(v string) bool {
		return strings.HasPrefix(strings.TrimSpace(v), "${TETHER_SECRET:")
	}

	for rawK, v := range incoming {
		k := strings.TrimSpace(rawK)
		if k == "" {
			continue
		}
		switch {
		case v == mcp.SecretMask || isSecretPlaceholder(v):
			if prev, ok := lookupExisting(k); ok {
				out[k] = prev
			} else {
				out[k] = v
			}
		case v == "":
			isSecret := mcp.IsSecretEnvKey(k)
			if isHeader {
				isSecret = mcp.IsSecretHeaderKey(k)
			}
			if isSecret || (existingRaw != nil && isSecretPlaceholder(existingRaw[k])) {
				cleared[k] = true
			} else {
				out[k] = ""
			}
		default:
			out[k] = v
		}
	}

	if preserveOmitted && existingRaw != nil {
		for k, origV := range existingRaw {
			if _, ok := out[k]; ok || cleared[k] {
				continue
			}
			isSecret := mcp.IsSecretEnvKey(k)
			if isHeader {
				isSecret = mcp.IsSecretHeaderKey(k)
			}
			if isSecret || isSecretPlaceholder(origV) {
				if prev, ok := lookupExisting(k); ok {
					out[k] = prev
				}
			}
		}
	}
	return out
}

func (s *Server) handleSkillsList(w http.ResponseWriter, _ *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}

	skillsDir := filepath.Join(eng.Workspace, ".agents", "skills")
	if info, err := os.Lstat(filepath.Join(eng.Workspace, ".agents")); err == nil && info.Mode()&os.ModeSymlink != 0 {
		writeError(w, http.StatusForbidden, "refusing symlinked .agents directory")
		return
	}
	if info, err := os.Lstat(skillsDir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		writeError(w, http.StatusForbidden, "refusing symlinked .agents/skills directory")
		return
	}

	type skillView struct {
		Name        string `json:"name"`
		RelPath     string `json:"rel_path"`
		Description string `json:"description,omitempty"`
		Content     string `json:"content"`
		Bytes       int    `json:"bytes"`
	}

	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{"skills": []skillView{}})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	m := eng.Mount()
	var out []skillView
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		name := entry.Name()
		if err := mcp.ValidateServerName(name); err != nil {
			continue
		}
		rel := ".agents/skills/" + name + "/SKILL.md"
		if err := rejectSymlinkSegments(eng.Workspace, rel); err != nil {
			continue
		}
		dec := m.AuthorizePath(rel, mount.OpRead, "code")
		if !dec.Allowed || dec.IsDir || dec.IsMissing || !strings.HasPrefix(dec.RelPath, ".agents/skills/") {
			continue
		}
		data, rerr := os.ReadFile(dec.CanonicalPath)
		if rerr != nil {
			continue
		}
		body := string(data)
		out = append(out, skillView{
			Name:        name,
			RelPath:     dec.RelPath,
			Description: parseSkillDescription(body),
			Content:     body,
			Bytes:       len(data),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	writeJSON(w, http.StatusOK, map[string]any{"skills": out})
}

func parseSkillDescription(markdown string) string {
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	for _, line := range lines[1:] {
		t := strings.TrimSpace(line)
		if t == "---" {
			break
		}
		if strings.HasPrefix(strings.ToLower(t), "description:") {
			return strings.Trim(strings.TrimSpace(t[len("description:"):]), `"'`)
		}
	}
	return ""
}

func (s *Server) handleSkillsPut(w http.ResponseWriter, r *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	var req struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "." || name == ".." || strings.Contains(name, "..") {
		writeError(w, http.StatusBadRequest, "skill name contains invalid path traversal")
		return
	}
	if err := mcp.ValidateServerName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rel := ".agents/skills/" + name + "/SKILL.md"
	if err := rejectSymlinkSegments(eng.Workspace, rel); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	dec := eng.Mount().AuthorizePath(rel, mount.OpWrite, "code")
	if !dec.Allowed {
		writeError(w, http.StatusForbidden, dec.Reason)
		return
	}
	if dec.RelPath != rel {
		writeError(w, http.StatusForbidden, "skill path outside .agents/skills/<name>/SKILL.md")
		return
	}
	if err := os.MkdirAll(filepath.Dir(dec.CanonicalPath), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(dec.CanonicalPath, []byte(req.Content), 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"name":     name,
		"rel_path": dec.RelPath,
	})
}

func (s *Server) handleMemoryGet(w http.ResponseWriter, _ *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	decisions, err := eng.GetDecisions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	patterns, err := eng.GetPatterns()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"decisions": decisions,
		"patterns":  patterns,
	})
}

func (s *Server) handleMemoryPost(w http.ResponseWriter, r *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	var req struct {
		Kind        string `json:"kind"` // "decision" | "pattern"
		Topic       string `json:"topic"`
		Decision    string `json:"decision"`
		Reason      string `json:"reason"`
		PatternType string `json:"pattern_type"`
		Description string `json:"description"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	switch strings.ToLower(strings.TrimSpace(req.Kind)) {
	case "", "decision":
		if strings.TrimSpace(req.Decision) == "" {
			writeError(w, http.StatusBadRequest, "decision is required")
			return
		}
		topic := strings.TrimSpace(req.Topic)
		if topic == "" {
			topic = "architecture"
		}
		if err := eng.AddDecision(topic, strings.TrimSpace(req.Decision), strings.TrimSpace(req.Reason)); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	case "pattern":
		if strings.TrimSpace(req.Description) == "" {
			writeError(w, http.StatusBadRequest, "description is required")
			return
		}
		pt := strings.TrimSpace(req.PatternType)
		if pt == "" {
			pt = "convention"
		}
		if err := eng.AddPattern(pt, strings.TrimSpace(req.Description)); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid kind %q (expected decision or pattern)", req.Kind))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func (s *Server) handleMemoryDelete(w http.ResponseWriter, r *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}

	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	idStr := strings.TrimSpace(r.URL.Query().Get("id"))
	var id int64
	if idStr != "" {
		parsed, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid id")
			return
		}
		id = parsed
	} else {
		var req struct {
			Kind string `json:"kind"`
			ID   int64  `json:"id"`
		}
		if err := decodeOptionalJSONBody(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.TrimSpace(req.Kind) != "" {
			kind = req.Kind
		}
		id = req.ID
	}
	if id <= 0 {
		writeError(w, http.StatusBadRequest, "positive id required")
		return
	}

	var err error
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", "decision":
		err = eng.DeleteDecision(id)
	case "pattern":
		err = eng.DeletePattern(id)
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid kind %q", kind))
		return
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "memory item not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

func globalConfigPath() string {
	return filepath.Join(mcp.DefaultConfigDir(), "config.json")
}

func readGlobalPrefs() map[string]any {
	data, err := os.ReadFile(globalConfigPath())
	if err != nil {
		return map[string]any{}
	}
	var prefs map[string]any
	if err := json.Unmarshal(data, &prefs); err != nil || prefs == nil {
		return map[string]any{}
	}
	return prefs
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, _ *http.Request) {
	eng, run := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	proj, err := eng.GetAllProjectSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	delete(proj, "api_key")

	resp := map[string]any{
		"mode":             eng.GetMode(),
		"project_settings": proj,
		"global":           readGlobalPrefs(),
	}
	if run != nil {
		resp["provider"] = run.ProviderName()
		resp["model"] = run.ModelName()
		resp["pinned_key_id"] = run.PinnedKeyID()
		resp["context_window"] = run.ContextWindowTokens()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	eng, run := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	var req struct {
		Mode            string            `json:"mode"`
		Provider        string            `json:"provider"`
		Model           string            `json:"model"`
		ProjectSettings map[string]string `json:"project_settings"`
		Global          map[string]any    `json:"global"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	nextMode := strings.ToLower(strings.TrimSpace(req.Mode))
	if nextMode != "" && nextMode != "code" && nextMode != "plan" && nextMode != "review" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid mode %q", req.Mode))
		return
	}

	if run != nil && (strings.TrimSpace(req.Provider) != "" || strings.TrimSpace(req.Model) != "") {
		if _, _, err := run.SwitchProviderAndModel(req.Provider, req.Model); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	if nextMode != "" {
		if err := eng.SetMode(nextMode); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.hub.Publish("plan.updated", map[string]any{"mode": nextMode})
	}

	for k, v := range req.ProjectSettings {
		key := strings.TrimSpace(k)
		if key == "" || strings.EqualFold(key, "api_key") {
			continue
		}
		if err := eng.SetProjectSetting(key, v); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	if len(req.Global) > 0 {
		cur := readGlobalPrefs()
		maps.Copy(cur, req.Global)
		if err := mcp.WriteSecureJSON(globalConfigPath(), cur); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	s.handleSettingsGet(w, r)
}

func (s *Server) handleDoctorPost(w http.ResponseWriter, r *http.Request) {
	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "engine unavailable")
		return
	}
	var req struct {
		Fix bool `json:"fix"`
	}
	if err := decodeOptionalJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	rep := doctor.Run(eng.Workspace, req.Fix, s.version)
	writeJSON(w, http.StatusOK, rep)
}
