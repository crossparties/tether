package mcp

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/mount"
)

const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
	TransportSSE   = "sse"

	ScopeGlobal  = "global"
	ScopeProject = "project"

	PresetLazyReadOnly = "lazy_readonly"
	PresetFull         = "full"
	PresetCustom       = "custom"

	SecretMask          = "**********"
	secretRefPrefix     = "${TETHER_SECRET:"
	credentialsMCPSecrt = "mcp_secrets"

	SerenaStatusNotConfigured = "not_configured"
	SerenaStatusDisabledDraft = "disabled_draft"
	SerenaStatusEnabled       = "enabled"
)

// SerenaReadOnlySemanticTools defines the strictly read-only semantic navigation
// subset allowed when serena runs under the lazy_readonly preset.
var SerenaReadOnlySemanticTools = []string{
	"find_symbol",
	"find_referencing_symbols",
	"find_implementations",
	"get_symbols_overview",
	"get_diagnostics_for_file",
}

// ExternalMCPConfig defines one external MCP server entry compatible with mcpServers JSON.
type ExternalMCPConfig struct {
	Name          string            `json:"name,omitempty"`
	Transport     string            `json:"transport,omitempty"`
	Command       string            `json:"command,omitempty"`
	Args          []string          `json:"args,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	URL           string            `json:"url,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	Enabled       bool              `json:"enabled"`
	Preset        string            `json:"preset,omitempty"`
	EnabledTools  []string          `json:"enabled_tools,omitempty"`
	DisabledTools []string          `json:"disabled_tools,omitempty"`
	Scope         string            `json:"scope,omitempty"`
}

// SparseUpdate controls secret preservation and partial-field patching in UpsertServer.
// Its zero value preserves omitted secrets and zero-valued fields safely.
type SparseUpdate struct {
	Enabled           *bool
	ReplaceAllSecrets bool
	ReplaceAllFields  bool
}

// ImportedServerInfo summarizes one imported IDE MCP draft server.
type ImportedServerInfo struct {
	Name           string `json:"name"`
	Source         string `json:"source"`
	Scope          string `json:"scope"`
	Transport      string `json:"transport"`
	Preset         string `json:"preset,omitempty"`
	SecretsMoved   int    `json:"secrets_moved"`
	ConfigFilePath string `json:"config_file_path"`
}

// SkippedServerInfo explains why an IDE MCP entry was skipped during import.
type SkippedServerInfo struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// ImportResult reports the outcome of ImportFromIDE.
type ImportResult struct {
	Imported []ImportedServerInfo `json:"imported,omitempty"`
	Skipped  []SkippedServerInfo  `json:"skipped,omitempty"`
}

// SerenaSuggestion describes whether smart serena escalation is recommended.
type SerenaSuggestion struct {
	Recommended     bool     `json:"recommended"`
	SerenaStatus    string   `json:"serena_status"`
	SerenaName      string   `json:"serena_name,omitempty"`
	Recommendation  string   `json:"recommendation,omitempty"`
	Reasons         []string `json:"reasons,omitempty"`
	FileCount       int      `json:"file_count"`
	DominantStack   string   `json:"dominant_stack,omitempty"`
	SuggestedPreset string   `json:"suggested_preset,omitempty"`
}

// ClientRegistry manages external MCP server definitions in project and global
// mcp.json files while isolating secrets strictly in 0600 credentials.json.
type ClientRegistry struct {
	workspace string
	configDir string
	homeDir   string
}

// DefaultConfigDir resolves Tether's global config directory (respecting TETHER_CONFIG_DIR).
func DefaultConfigDir() string {
	if d := strings.TrimSpace(os.Getenv("TETHER_CONFIG_DIR")); d != "" {
		return filepath.Clean(d)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "tether")
}

// NewClientRegistry constructs a ClientRegistry rooted at workspace using DefaultConfigDir.
func NewClientRegistry(workspace string) *ClientRegistry {
	home, _ := os.UserHomeDir()
	return NewClientRegistryWithDirs(workspace, DefaultConfigDir(), home)
}

// NewClientRegistryWithDirs constructs a ClientRegistry with explicit configDir and homeDir
// (empty configDir disables global config/credentials access for isolated tests).
func NewClientRegistryWithDirs(workspace, configDir, homeDir string) *ClientRegistry {
	ws := strings.TrimSpace(workspace)
	if ws != "" {
		if abs, err := filepath.Abs(ws); err == nil {
			ws = abs
		}
	}
	return &ClientRegistry{
		workspace: ws,
		configDir: strings.TrimSpace(configDir),
		homeDir:   strings.TrimSpace(homeDir),
	}
}

func (r *ClientRegistry) Workspace() string {
	if r == nil {
		return ""
	}
	return r.workspace
}

func (r *ClientRegistry) ProjectMCPPath() string {
	if r == nil || r.workspace == "" {
		return ""
	}
	return filepath.Join(r.workspace, ".tether", "mcp.json")
}

func (r *ClientRegistry) GlobalMCPPath() string {
	if r == nil || r.configDir == "" {
		return ""
	}
	return filepath.Join(r.configDir, "mcp.json")
}

func (r *ClientRegistry) CredentialsPath() string {
	if r == nil || r.configDir == "" {
		return ""
	}
	return filepath.Join(r.configDir, "credentials.json")
}

func (r *ClientRegistry) scopePath(scope string) (string, string, error) {
	s := strings.ToLower(strings.TrimSpace(scope))
	if s == "" {
		s = ScopeProject
	}
	switch s {
	case ScopeProject:
		p := r.ProjectMCPPath()
		if p == "" {
			return "", "", fmt.Errorf("project workspace is not set for project-scoped mcp.json")
		}
		return ScopeProject, p, nil
	case ScopeGlobal:
		p := r.GlobalMCPPath()
		if p == "" {
			return "", "", fmt.Errorf("config directory is not configured for global mcp.json")
		}
		return ScopeGlobal, p, nil
	default:
		return "", "", fmt.Errorf("invalid scope %q (expected global or project)", scope)
	}
}

// IsSecretEnvKey reports whether an environment variable name holds sensitive credentials.
func IsSecretEnvKey(key string) bool {
	upper := strings.ToUpper(strings.TrimSpace(key))
	if upper == "" {
		return false
	}
	for _, kw := range []string{
		"TOKEN", "SECRET", "PASSWORD", "PASSWD", "API_KEY", "APIKEY",
		"ACCESS_KEY", "PRIVATE_KEY", "AUTH", "BEARER", "CREDENTIAL",
		"COOKIE", "SIGNING_KEY",
	} {
		if strings.Contains(upper, kw) {
			return true
		}
	}
	return upper == "KEY" || strings.HasSuffix(upper, "_KEY") ||
		strings.HasSuffix(upper, "_PWD") || strings.HasSuffix(upper, "_PASS")
}

// IsSecretHeaderKey reports whether an HTTP header name carries authentication or secrets.
func IsSecretHeaderKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	if lower == "" {
		return false
	}
	for _, kw := range []string{
		"authorization", "token", "secret", "key", "auth",
		"password", "credential", "bearer", "cookie",
	} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func looksLikeSecretValue(val string) bool {
	v := strings.TrimSpace(val)
	if v == "" || v == SecretMask || isSecretRef(v) {
		return false
	}
	lower := strings.ToLower(v)
	for _, pfx := range []string{"sk-", "ghp_", "gho_", "github_pat_", "glpat-", "xoxb-", "xoxp-", "akia", "bearer "} {
		if strings.HasPrefix(lower, pfx) {
			return true
		}
	}
	return false
}

func isSecretRef(val string) bool {
	v := strings.TrimSpace(val)
	return strings.HasPrefix(v, secretRefPrefix) && strings.HasSuffix(v, "}")
}

func formatSecretPlaceholder(kind, key string) string {
	return fmt.Sprintf("%s%s:%s}", secretRefPrefix, kind, key)
}

func secretStoreKey(scope, serverName, kind, key string) string {
	normKey := strings.TrimSpace(key)
	if kind == "header" {
		normKey = strings.ToLower(normKey)
	}
	return fmt.Sprintf("%s:%s:%s:%s", strings.ToLower(strings.TrimSpace(scope)), strings.ToLower(strings.TrimSpace(serverName)), kind, normKey)
}

func parseSecretRef(val string) (kind, key string, ok bool) {
	v := strings.TrimSpace(val)
	if !isSecretRef(v) {
		return "", "", false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(v, secretRefPrefix), "}")
	k, name, found := strings.Cut(inner, ":")
	if !found || strings.TrimSpace(name) == "" {
		return "", "", false
	}
	return strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(name), true
}

// ValidateServerName enforces safe server identifiers without secret-key delimiters.
func ValidateServerName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return fmt.Errorf("server name cannot be empty")
	}
	if trimmed == "." || trimmed == ".." {
		return fmt.Errorf("invalid server name %q: cannot be '.' or '..'", name)
	}
	if strings.HasPrefix(trimmed, "-") {
		return fmt.Errorf("invalid server name %q: cannot start with '-'", name)
	}
	for _, r := range trimmed {
		isOK := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '-' || r == '.'
		if !isOK {
			return fmt.Errorf("invalid server name %q: only [a-zA-Z0-9._-] allowed", name)
		}
	}
	return nil
}

// IsSerenaServer reports whether cfg refers to a serena MCP server.
func IsSerenaServer(cfg ExternalMCPConfig) bool {
	if strings.Contains(strings.ToLower(strings.TrimSpace(cfg.Name)), "serena") {
		return true
	}
	if strings.Contains(strings.ToLower(cfg.Command), "serena") {
		return true
	}
	for _, a := range cfg.Args {
		if strings.Contains(strings.ToLower(a), "serena") {
			return true
		}
	}
	return false
}

// IsReadOnlySerenaToolName reports whether toolName is in the 5-tool read-only semantic allowlist.
func IsReadOnlySerenaToolName(toolName string) bool {
	return slices.Contains(SerenaReadOnlySemanticTools, strings.TrimSpace(toolName))
}

func cloneStringSlice(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	return maps.Clone(in)
}

// Clone returns a deep copy of ExternalMCPConfig.
func (c ExternalMCPConfig) Clone() ExternalMCPConfig {
	out := c
	out.Args = cloneStringSlice(c.Args)
	out.Env = cloneStringMap(c.Env)
	out.Headers = cloneStringMap(c.Headers)
	out.EnabledTools = cloneStringSlice(c.EnabledTools)
	out.DisabledTools = cloneStringSlice(c.DisabledTools)
	return out
}

// MaskedView returns a deep copy of ExternalMCPConfig with all secret env vars
// and headers replaced with "**********".
func (c ExternalMCPConfig) MaskedView() ExternalMCPConfig {
	out := c.Clone()
	for k, v := range out.Env {
		if IsSecretEnvKey(k) || isSecretRef(v) || looksLikeSecretValue(v) || v == SecretMask {
			out.Env[k] = SecretMask
		}
	}
	for k, v := range out.Headers {
		if IsSecretHeaderKey(k) || isSecretRef(v) || looksLikeSecretValue(v) || v == SecretMask {
			out.Headers[k] = SecretMask
		}
	}
	return out
}

func normalizePreset(name string, cfg *ExternalMCPConfig) error {
	p := strings.ToLower(strings.TrimSpace(cfg.Preset))
	if p == "" {
		if IsSerenaServer(*cfg) {
			p = PresetLazyReadOnly
		} else {
			p = PresetFull
		}
	}
	switch p {
	case PresetLazyReadOnly, PresetFull, PresetCustom:
		cfg.Preset = p
		return nil
	default:
		return fmt.Errorf("invalid preset %q for %q (supported: lazy_readonly, full, custom)", cfg.Preset, name)
	}
}

func normalizeTransport(cfg *ExternalMCPConfig) error {
	t := strings.ToLower(strings.TrimSpace(cfg.Transport))
	if t == "streamable-http" || t == "streamable_http" {
		t = TransportHTTP
	}
	if t == "" {
		switch {
		case strings.TrimSpace(cfg.Command) != "":
			t = TransportStdio
		case strings.TrimSpace(cfg.URL) != "":
			lowerURL := strings.ToLower(strings.TrimSpace(cfg.URL))
			if strings.HasSuffix(lowerURL, "/sse") || strings.Contains(lowerURL, "/sse?") {
				t = TransportSSE
			} else {
				t = TransportHTTP
			}
		}
	}
	switch t {
	case TransportStdio:
		if strings.TrimSpace(cfg.Command) == "" {
			return fmt.Errorf("stdio server %q requires command", cfg.Name)
		}
		cfg.Transport = TransportStdio
	case TransportHTTP, TransportSSE:
		if strings.TrimSpace(cfg.URL) == "" {
			return fmt.Errorf("%s server %q requires url", t, cfg.Name)
		}
		if _, err := ValidateServerURL(cfg.URL); err != nil {
			return fmt.Errorf("server %q url invalid: %w", cfg.Name, err)
		}
		cfg.Transport = t
	default:
		return fmt.Errorf("invalid or missing transport %q for server %q (expected stdio, http, or sse)", cfg.Transport, cfg.Name)
	}
	return nil
}

type mcpServersFile struct {
	MCPServers map[string]ExternalMCPConfig `json:"mcpServers"`
}

func readMCPFile(path, scope string) ([]ExternalMCPConfig, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var file mcpServersFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	names := make([]string, 0, len(file.MCPServers))
	for name := range file.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)

	seenLower := make(map[string]string, len(names))
	out := make([]ExternalMCPConfig, 0, len(names))
	for _, name := range names {
		if err := ValidateServerName(name); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		low := strings.ToLower(name)
		if prev, dup := seenLower[low]; dup {
			return nil, fmt.Errorf("duplicate MCP server name %q conflicts case-insensitively with %q in %s", name, prev, path)
		}
		seenLower[low] = name

		cfg := file.MCPServers[name].Clone()
		cfg.Name = name
		cfg.Scope = scope
		out = append(out, cfg)
	}
	return out, nil
}

func writeMCPFile(path string, servers []ExternalMCPConfig, isGlobal bool) error {
	mode := os.FileMode(0644)
	dirMode := os.FileMode(0755)
	if isGlobal {
		mode = 0600
		dirMode = 0700
	}
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return err
	}
	outMap := make(map[string]ExternalMCPConfig, len(servers))
	for _, s := range servers {
		cp := s.Clone()
		name := cp.Name
		cp.Name = ""
		cp.Scope = ""
		outMap[name] = cp
	}
	payload := mcpServersFile{MCPServers: outMap}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-mcp-*")
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
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
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

// ListServers returns all configured servers across global and project scopes,
// rejecting any case-insensitive duplicate server names across files.
func (r *ClientRegistry) ListServers() ([]ExternalMCPConfig, error) {
	if r == nil {
		return nil, nil
	}
	globalList, err := readMCPFile(r.GlobalMCPPath(), ScopeGlobal)
	if err != nil {
		return nil, err
	}
	projectList, err := readMCPFile(r.ProjectMCPPath(), ScopeProject)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]ExternalMCPConfig, len(globalList)+len(projectList))
	all := make([]ExternalMCPConfig, 0, len(globalList)+len(projectList))
	for _, item := range append(globalList, projectList...) {
		low := strings.ToLower(item.Name)
		if prev, dup := seen[low]; dup {
			return nil, fmt.Errorf("duplicate MCP server name %q (%s) conflicts case-insensitively with %q (%s)", item.Name, item.Scope, prev.Name, prev.Scope)
		}
		seen[low] = item
		all = append(all, item)
	}
	sort.Slice(all, func(i, j int) bool {
		li, lj := strings.ToLower(all[i].Name), strings.ToLower(all[j].Name)
		if li != lj {
			return li < lj
		}
		return all[i].Scope < all[j].Scope
	})
	return all, nil
}

// MaskedView returns all configured servers with all secret values masked as "**********".
func (r *ClientRegistry) MaskedView() ([]ExternalMCPConfig, error) {
	servers, err := r.ListServers()
	if err != nil {
		return nil, err
	}
	out := make([]ExternalMCPConfig, len(servers))
	for i, s := range servers {
		out[i] = s.MaskedView()
	}
	return out, nil
}

// GetServer locates a configured server by name (case-insensitive).
func (r *ClientRegistry) GetServer(name string) (ExternalMCPConfig, bool, error) {
	servers, err := r.ListServers()
	if err != nil {
		return ExternalMCPConfig{}, false, err
	}
	target := strings.TrimSpace(name)
	for _, s := range servers {
		if strings.EqualFold(s.Name, target) {
			return s, true, nil
		}
	}
	return ExternalMCPConfig{}, false, nil
}

// AddServer inserts a new server into the registry, failing if any case-insensitive
// duplicate already exists in global or project scope.
func (r *ClientRegistry) AddServer(cfg ExternalMCPConfig) (ExternalMCPConfig, error) {
	if err := ValidateServerName(cfg.Name); err != nil {
		return ExternalMCPConfig{}, err
	}
	existing, err := r.ListServers()
	if err != nil {
		return ExternalMCPConfig{}, err
	}
	for _, s := range existing {
		if strings.EqualFold(s.Name, strings.TrimSpace(cfg.Name)) {
			return ExternalMCPConfig{}, fmt.Errorf("server %q already exists (conflicts case-insensitively with %q in %s scope)", cfg.Name, s.Name, s.Scope)
		}
	}
	return r.UpsertServer(cfg, SparseUpdate{ReplaceAllFields: true, ReplaceAllSecrets: true})
}

// UpsertServer creates or updates an external MCP server, enforcing case-insensitive
// uniqueness, storing secrets strictly in 0600 credentials.json ("mcp_secrets"),
// writing symbolic placeholders in mcp.json, and preserving existing secrets during
// sparse updates or when "**********" is passed.
func (r *ClientRegistry) UpsertServer(cfg ExternalMCPConfig, sparse SparseUpdate) (ExternalMCPConfig, error) {
	if r == nil {
		return ExternalMCPConfig{}, fmt.Errorf("registry is nil")
	}
	cfg.Name = strings.TrimSpace(cfg.Name)
	if err := ValidateServerName(cfg.Name); err != nil {
		return ExternalMCPConfig{}, err
	}

	scope := strings.TrimSpace(cfg.Scope)
	var existing *ExternalMCPConfig
	all, err := r.ListServers()
	if err != nil {
		return ExternalMCPConfig{}, err
	}
	for i := range all {
		if strings.EqualFold(all[i].Name, cfg.Name) {
			if all[i].Name != cfg.Name {
				return ExternalMCPConfig{}, fmt.Errorf("duplicate MCP server name %q conflicts case-insensitively with existing %q (%s scope)", cfg.Name, all[i].Name, all[i].Scope)
			}
			if scope != "" && !strings.EqualFold(scope, all[i].Scope) {
				return ExternalMCPConfig{}, fmt.Errorf("server %q already exists in %s scope (cannot duplicate into %s scope)", cfg.Name, all[i].Scope, scope)
			}
			cp := all[i].Clone()
			existing = &cp
			if scope == "" {
				scope = all[i].Scope
			}
			break
		}
	}

	normScope, targetPath, err := r.scopePath(scope)
	if err != nil {
		return ExternalMCPConfig{}, err
	}

	merged := cfg.Clone()
	merged.Scope = normScope
	if existing != nil && !sparse.ReplaceAllFields {
		if merged.Transport == "" {
			merged.Transport = existing.Transport
		}
		if merged.Command == "" && merged.URL == "" {
			merged.Command = existing.Command
			if merged.Args == nil {
				merged.Args = cloneStringSlice(existing.Args)
			}
			merged.URL = existing.URL
		} else if merged.Command != "" && merged.Args == nil && existing.Command == merged.Command {
			merged.Args = cloneStringSlice(existing.Args)
		}
		if merged.Preset == "" {
			merged.Preset = existing.Preset
		}
		if merged.EnabledTools == nil {
			merged.EnabledTools = cloneStringSlice(existing.EnabledTools)
		}
		if merged.DisabledTools == nil {
			merged.DisabledTools = cloneStringSlice(existing.DisabledTools)
		}
		if merged.Env == nil {
			merged.Env = cloneStringMap(existing.Env)
		}
		if merged.Headers == nil {
			merged.Headers = cloneStringMap(existing.Headers)
		}
	}
	if sparse.Enabled != nil {
		merged.Enabled = *sparse.Enabled
	}

	if err := normalizeTransport(&merged); err != nil {
		return ExternalMCPConfig{}, err
	}
	if err := normalizePreset(merged.Name, &merged); err != nil {
		return ExternalMCPConfig{}, err
	}

	secrets, err := r.loadMCPSecrets()
	if err != nil {
		return ExternalMCPConfig{}, err
	}
	origSecrets := cloneStringMap(secrets)

	envCleared := existing != nil && !sparse.ReplaceAllFields && cfg.Env != nil && len(cfg.Env) == 0
	headersCleared := existing != nil && !sparse.ReplaceAllFields && cfg.Headers != nil && len(cfg.Headers) == 0

	// Process Env secrets cleanly.
	cleanEnv, err := r.extractAndSanitizeKV(normScope, merged.Name, "env", merged.Env, func() map[string]string {
		if existing != nil {
			return existing.Env
		}
		return nil
	}(), !sparse.ReplaceAllSecrets && !envCleared, secrets)
	if err != nil {
		return ExternalMCPConfig{}, err
	}
	merged.Env = cleanEnv

	// Process Header secrets cleanly.
	cleanHeaders, err := r.extractAndSanitizeKV(normScope, merged.Name, "header", merged.Headers, func() map[string]string {
		if existing != nil {
			return existing.Headers
		}
		return nil
	}(), !sparse.ReplaceAllSecrets && !headersCleared, secrets)
	if err != nil {
		return ExternalMCPConfig{}, err
	}
	merged.Headers = cleanHeaders

	// Clean up removed secrets when full replace or explicit {} clear is requested.
	if sparse.ReplaceAllSecrets || envCleared || headersCleared {
		keep := make(map[string]bool)
		for k, v := range merged.Env {
			if isSecretRef(v) {
				keep[secretStoreKey(normScope, merged.Name, "env", k)] = true
			}
		}
		for k, v := range merged.Headers {
			if isSecretRef(v) {
				keep[secretStoreKey(normScope, merged.Name, "header", k)] = true
			}
		}
		basePrefix := strings.ToLower(normScope) + ":" + strings.ToLower(merged.Name) + ":"
		for sk := range secrets {
			if !strings.HasPrefix(sk, basePrefix) || keep[sk] {
				continue
			}
			rest := strings.TrimPrefix(sk, basePrefix)
			if sparse.ReplaceAllSecrets ||
				(envCleared && strings.HasPrefix(rest, "env:")) ||
				(headersCleared && strings.HasPrefix(rest, "header:")) {
				delete(secrets, sk)
			}
		}
	}

	if !mapsEqual(origSecrets, secrets) {
		if err := r.saveMCPSecrets(secrets); err != nil {
			return ExternalMCPConfig{}, err
		}
	}

	scopeServers, err := readMCPFile(targetPath, normScope)
	if err != nil {
		return ExternalMCPConfig{}, err
	}
	updated := false
	for i := range scopeServers {
		if scopeServers[i].Name == merged.Name {
			scopeServers[i] = merged
			updated = true
			break
		}
	}
	if !updated {
		scopeServers = append(scopeServers, merged)
	}
	if err := writeMCPFile(targetPath, scopeServers, normScope == ScopeGlobal); err != nil {
		return ExternalMCPConfig{}, err
	}
	return merged.Clone(), nil
}

func (r *ClientRegistry) extractAndSanitizeKV(
	scope, serverName, kind string,
	incoming map[string]string,
	existing map[string]string,
	preserveOmitted bool,
	secrets map[string]string,
) (map[string]string, error) {
	out := make(map[string]string, len(incoming))
	explicitlyCleared := make(map[string]bool)
	for k, v := range incoming {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		isSecretKey := IsSecretEnvKey(key)
		if kind == "header" {
			isSecretKey = IsSecretHeaderKey(key)
		}
		sKey := secretStoreKey(scope, serverName, kind, key)
		switch {
		case v == SecretMask:
			if _, ok := secrets[sKey]; !ok {
				if existing != nil {
					if prevVal, okPrev := existing[key]; okPrev && prevVal != "" && prevVal != SecretMask && !isSecretRef(prevVal) {
						secrets[sKey] = prevVal
					}
				}
			}
			if _, ok := secrets[sKey]; !ok {
				return nil, fmt.Errorf("cannot use masked secret %q for %s.%s when no existing secret is stored", SecretMask, serverName, key)
			}
			out[key] = formatSecretPlaceholder(kind, key)
		case isSecretRef(v):
			if _, ok := secrets[sKey]; !ok && existing != nil {
				if prevVal, okPrev := existing[key]; okPrev && prevVal != "" && prevVal != SecretMask && !isSecretRef(prevVal) {
					secrets[sKey] = prevVal
				}
			}
			out[key] = formatSecretPlaceholder(kind, key)
		case v == "" && (isSecretKey || (existing != nil && isSecretRef(existing[key]))):
			// Explicitly clearing a secret to "" removes stored secret & placeholder cleanly.
			delete(secrets, sKey)
			explicitlyCleared[key] = true
		case isSecretKey || looksLikeSecretValue(v):
			if r.CredentialsPath() == "" {
				return nil, fmt.Errorf("cannot store secret %s=%s without config directory (TETHER_CONFIG_DIR / HOME)", kind, key)
			}
			if v != "" {
				secrets[sKey] = v
			}
			out[key] = formatSecretPlaceholder(kind, key)
		default:
			out[key] = v
		}
	}
	if preserveOmitted && existing != nil {
		for k, v := range existing {
			if _, present := out[k]; present || explicitlyCleared[k] {
				continue
			}
			isSecretKey := IsSecretEnvKey(k)
			if kind == "header" {
				isSecretKey = IsSecretHeaderKey(k)
			}
			if isSecretKey || isSecretRef(v) || looksLikeSecretValue(v) {
				out[k] = formatSecretPlaceholder(kind, k)
			}
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// SetServerEnabled toggles Enabled on an existing server while preserving all secrets.
func (r *ClientRegistry) SetServerEnabled(name string, enabled bool) (ExternalMCPConfig, error) {
	srv, ok, err := r.GetServer(name)
	if err != nil {
		return ExternalMCPConfig{}, err
	}
	if !ok {
		return ExternalMCPConfig{}, fmt.Errorf("MCP server %q not found", name)
	}
	return r.UpsertServer(ExternalMCPConfig{
		Name:  srv.Name,
		Scope: srv.Scope,
	}, SparseUpdate{Enabled: &enabled})
}

// UpdateServerTools updates per-server tool enable/disable filters and optional preset
// while preserving all stored secrets.
func (r *ClientRegistry) UpdateServerTools(name string, enableTools, disableTools []string, preset string) (ExternalMCPConfig, error) {
	srv, ok, err := r.GetServer(name)
	if err != nil {
		return ExternalMCPConfig{}, err
	}
	if !ok {
		return ExternalMCPConfig{}, fmt.Errorf("MCP server %q not found", name)
	}

	enabledList := cloneStringSlice(srv.EnabledTools)
	disabledList := cloneStringSlice(srv.DisabledTools)
	for _, t := range enableTools {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		disabledList = removeString(disabledList, t)
		if !slices.Contains(enabledList, t) {
			enabledList = append(enabledList, t)
		}
	}
	for _, t := range disableTools {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		enabledList = removeString(enabledList, t)
		if !slices.Contains(disabledList, t) {
			disabledList = append(disabledList, t)
		}
	}

	next := ExternalMCPConfig{
		Name:          srv.Name,
		Scope:         srv.Scope,
		Enabled:       srv.Enabled,
		Preset:        strings.TrimSpace(preset),
		EnabledTools:  enabledList,
		DisabledTools: disabledList,
	}
	return r.UpsertServer(next, SparseUpdate{ReplaceAllFields: false})
}

func removeString(list []string, target string) []string {
	if len(list) == 0 {
		return list
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if item != target {
			out = append(out, item)
		}
	}
	return out
}

// RemoveServer deletes a server from mcp.json and removes its stored secrets from credentials.json.
func (r *ClientRegistry) RemoveServer(name, scope string) error {
	if r == nil {
		return fmt.Errorf("registry is nil")
	}
	target := strings.TrimSpace(name)
	if target == "" {
		return fmt.Errorf("server name is required")
	}

	var scopesToCheck []string
	if strings.TrimSpace(scope) != "" {
		normScope, _, err := r.scopePath(scope)
		if err != nil {
			return err
		}
		scopesToCheck = []string{normScope}
	} else {
		scopesToCheck = []string{ScopeProject, ScopeGlobal}
	}

	removed := false
	var removedScope, removedName string
	for _, sc := range scopesToCheck {
		_, path, err := r.scopePath(sc)
		if err != nil {
			continue
		}
		list, err := readMCPFile(path, sc)
		if err != nil {
			return err
		}
		kept := make([]ExternalMCPConfig, 0, len(list))
		for _, item := range list {
			if strings.EqualFold(item.Name, target) {
				removed = true
				removedScope = sc
				removedName = item.Name
				continue
			}
			kept = append(kept, item)
		}
		if removed {
			if err := writeMCPFile(path, kept, sc == ScopeGlobal); err != nil {
				return err
			}
			break
		}
	}
	if !removed {
		return fmt.Errorf("MCP server %q not found", name)
	}

	// Clean associated secrets from credentials.json if present.
	if r.CredentialsPath() != "" {
		secrets, err := r.loadMCPSecrets()
		if err != nil {
			return err
		}
		prefix := strings.ToLower(removedScope) + ":" + strings.ToLower(removedName) + ":"
		changed := false
		for k := range secrets {
			if strings.HasPrefix(k, prefix) {
				delete(secrets, k)
				changed = true
			}
		}
		if changed {
			if err := r.saveMCPSecrets(secrets); err != nil {
				return err
			}
		}
	}
	return nil
}

// ResolveRuntimeSecrets returns a copy of cfg with any ${TETHER_SECRET:...} placeholders
// substituted from 0600 credentials.json.
func (r *ClientRegistry) ResolveRuntimeSecrets(cfg ExternalMCPConfig) (ExternalMCPConfig, error) {
	out := cfg.Clone()
	secrets, err := r.loadMCPSecrets()
	if err != nil {
		return out, err
	}
	resolveMap := func(m map[string]string, defaultKind string) (map[string]string, error) {
		if len(m) == 0 {
			return nil, nil
		}
		res := make(map[string]string, len(m))
		for k, v := range m {
			if kind, refKey, ok := parseSecretRef(v); ok {
				val, found := lookupMCPSecret(secrets, cfg.Scope, cfg.Name, kind, refKey)
				if !found && defaultKind != kind {
					val, found = lookupMCPSecret(secrets, cfg.Scope, cfg.Name, defaultKind, k)
				}
				if !found {
					return nil, fmt.Errorf("missing secret %s:%s for MCP server %q in credentials.json", kind, refKey, cfg.Name)
				}
				res[k] = val
				continue
			}
			res[k] = v
		}
		return res, nil
	}

	if out.Env != nil {
		resolvedEnv, err := resolveMap(out.Env, "env")
		if err != nil {
			return out, err
		}
		out.Env = resolvedEnv
	}
	if out.Headers != nil {
		resolvedHeaders, err := resolveMap(out.Headers, "header")
		if err != nil {
			return out, err
		}
		out.Headers = resolvedHeaders
	}
	return out, nil
}

func lookupMCPSecret(secrets map[string]string, scope, serverName, kind, key string) (string, bool) {
	if len(secrets) == 0 {
		return "", false
	}
	sKey := secretStoreKey(scope, serverName, kind, key)
	if v, ok := secrets[sKey]; ok {
		return v, true
	}
	// Exact-case fallback for manually authored header keys.
	exactKey := fmt.Sprintf("%s:%s:%s:%s", strings.ToLower(scope), strings.ToLower(serverName), kind, key)
	if v, ok := secrets[exactKey]; ok {
		return v, true
	}
	// Short-form fallback "<scope>:<lower_name>:<key>".
	shortKey := fmt.Sprintf("%s:%s:%s", strings.ToLower(scope), strings.ToLower(serverName), key)
	if v, ok := secrets[shortKey]; ok {
		return v, true
	}
	return "", false
}

func (r *ClientRegistry) loadMCPSecrets() (map[string]string, error) {
	out := make(map[string]string)
	path := r.CredentialsPath()
	if path == "" {
		return out, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return out, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse credentials.json: %w", err)
	}
	if blob, ok := raw[credentialsMCPSecrt]; ok && len(strings.TrimSpace(string(blob))) > 0 && string(blob) != "null" {
		if err := json.Unmarshal(blob, &out); err != nil {
			return nil, fmt.Errorf("parse credentials.json %s: %w", credentialsMCPSecrt, err)
		}
	}
	return out, nil
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func (r *ClientRegistry) saveMCPSecrets(secrets map[string]string) error {
	path := r.CredentialsPath()
	if path == "" {
		if len(secrets) == 0 {
			return nil
		}
		return fmt.Errorf("cannot save MCP secrets without config directory")
	}
	raw := make(map[string]json.RawMessage)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read credentials.json before write: %w", err)
		}
	} else if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse credentials.json before write: %w", err)
		}
	}
	if len(secrets) == 0 {
		delete(raw, credentialsMCPSecrt)
	} else {
		enc, err := json.Marshal(secrets)
		if err != nil {
			return err
		}
		raw[credentialsMCPSecrt] = enc
	}
	return WriteSecureJSON(path, raw)
}

// WriteSecureJSON atomically writes v as indented JSON with 0700 parent dir and 0600 file permissions.
func WriteSecureJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0700)
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	tmp, err := os.CreateTemp(dir, ".tmp-cred-*")
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

// InspectRawSecretLeaks checks global and project mcp.json for raw secrets that
// were manually added instead of stored as ${TETHER_SECRET:...} placeholders.
func (r *ClientRegistry) InspectRawSecretLeaks() ([]string, error) {
	servers, err := r.ListServers()
	if err != nil {
		return nil, err
	}
	var leaks []string
	for _, s := range servers {
		for k, v := range s.Env {
			if (IsSecretEnvKey(k) || looksLikeSecretValue(v)) && v != "" && !isSecretRef(v) {
				leaks = append(leaks, fmt.Sprintf("%s/%s env.%s", s.Scope, s.Name, k))
			}
		}
		for k, v := range s.Headers {
			if (IsSecretHeaderKey(k) || looksLikeSecretValue(v)) && v != "" && !isSecretRef(v) {
				leaks = append(leaks, fmt.Sprintf("%s/%s headers.%s", s.Scope, s.Name, k))
			}
		}
	}
	return leaks, nil
}

// RepairRawSecretLeaks migrates any raw secrets found in mcp.json into 0600 credentials.json.
func (r *ClientRegistry) RepairRawSecretLeaks() (int, error) {
	leaks, err := r.InspectRawSecretLeaks()
	if err != nil || len(leaks) == 0 {
		return 0, err
	}
	servers, err := r.ListServers()
	if err != nil {
		return 0, err
	}
	for _, s := range servers {
		if _, err := r.UpsertServer(s, SparseUpdate{}); err != nil {
			return 0, err
		}
	}
	return len(leaks), nil
}

type ideRawServer struct {
	Transport string            `json:"transport"`
	Type      string            `json:"type"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	URL       string            `json:"url"`
	ServerURL string            `json:"serverUrl"`
	Headers   map[string]string `json:"headers"`
}

type ideConfigFile struct {
	MCPServers map[string]ideRawServer `json:"mcpServers"`
	Projects   map[string]struct {
		MCPServers map[string]ideRawServer `json:"mcpServers"`
	} `json:"projects"`
}

// ImportFromIDE scans local IDE MCP configs (agy, claude, cursor, or all) and
// imports discovered servers as disabled opt-in drafts (Enabled: false), extracting
// any inline secrets into 0600 credentials.json.
func (r *ClientRegistry) ImportFromIDE(source, scope string) (ImportResult, error) {
	if r == nil {
		return ImportResult{}, fmt.Errorf("registry is nil")
	}
	srcNorm := strings.ToLower(strings.TrimSpace(source))
	if srcNorm == "" {
		srcNorm = "all"
	}
	switch srcNorm {
	case "agy", "claude", "cursor", "all":
	default:
		return ImportResult{}, fmt.Errorf("unsupported import source %q (expected agy, claude, cursor, or all)", source)
	}

	if strings.TrimSpace(scope) == "" {
		scope = ScopeGlobal
	}
	normScope, targetPath, err := r.scopePath(scope)
	if err != nil {
		return ImportResult{}, err
	}

	home := r.homeDir
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ImportResult{}, fmt.Errorf("cannot resolve user home directory for IDE import")
	}

	type ideSourceFile struct {
		source string
		path   string
	}
	var sources []ideSourceFile
	if srcNorm == "agy" || srcNorm == "all" {
		sources = append(sources, ideSourceFile{"agy", filepath.Join(home, ".gemini", "config", "mcp_config.json")})
	}
	if srcNorm == "claude" || srcNorm == "all" {
		sources = append(sources,
			ideSourceFile{"claude", filepath.Join(home, ".claude.json")},
			ideSourceFile{"claude", filepath.Join(home, ".claude", "mcp_config.json")},
		)
	}
	if srcNorm == "cursor" || srcNorm == "all" {
		sources = append(sources, ideSourceFile{"cursor", filepath.Join(home, ".cursor", "mcp.json")})
	}

	existingServers, err := r.ListServers()
	if err != nil {
		return ImportResult{}, err
	}
	seenNames := make(map[string]bool, len(existingServers))
	for _, s := range existingServers {
		seenNames[strings.ToLower(s.Name)] = true
	}

	var res ImportResult
	for _, sf := range sources {
		data, rerr := os.ReadFile(sf.path)
		if rerr != nil {
			continue
		}
		var cfgFile ideConfigFile
		if uerr := json.Unmarshal(data, &cfgFile); uerr != nil {
			res.Skipped = append(res.Skipped, SkippedServerInfo{
				Name:   filepath.Base(sf.path),
				Source: sf.source,
				Reason: fmt.Sprintf("invalid JSON: %v", uerr),
			})
			continue
		}

		mergedRaw := maps.Clone(cfgFile.MCPServers)
		if mergedRaw == nil {
			mergedRaw = make(map[string]ideRawServer)
		}
		if r.workspace != "" && len(cfgFile.Projects) > 0 {
			if proj, ok := cfgFile.Projects[r.workspace]; ok {
				for k, v := range proj.MCPServers {
					if _, exists := mergedRaw[k]; !exists {
						mergedRaw[k] = v
					}
				}
			}
		}

		names := make([]string, 0, len(mergedRaw))
		for n := range mergedRaw {
			names = append(names, n)
		}
		sort.Strings(names)

		for _, name := range names {
			if strings.EqualFold(name, "tether") {
				res.Skipped = append(res.Skipped, SkippedServerInfo{Name: name, Source: sf.source, Reason: "tether self-entry skipped"})
				continue
			}
			if err := ValidateServerName(name); err != nil {
				res.Skipped = append(res.Skipped, SkippedServerInfo{Name: name, Source: sf.source, Reason: err.Error()})
				continue
			}
			low := strings.ToLower(name)
			if seenNames[low] {
				res.Skipped = append(res.Skipped, SkippedServerInfo{Name: name, Source: sf.source, Reason: "already configured"})
				continue
			}

			rawEntry := mergedRaw[name]
			urlStr := strings.TrimSpace(rawEntry.URL)
			if urlStr == "" {
				urlStr = strings.TrimSpace(rawEntry.ServerURL)
			}
			transport := strings.TrimSpace(rawEntry.Transport)
			if transport == "" {
				transport = strings.TrimSpace(rawEntry.Type)
			}

			cand := ExternalMCPConfig{
				Name:      name,
				Transport: transport,
				Command:   strings.TrimSpace(rawEntry.Command),
				Args:      cloneStringSlice(rawEntry.Args),
				Env:       cloneStringMap(rawEntry.Env),
				URL:       urlStr,
				Headers:   cloneStringMap(rawEntry.Headers),
				Enabled:   false, // Opt-in draft ONLY: never auto-enable imported servers.
				Scope:     normScope,
			}
			if IsSerenaServer(cand) {
				cand.Preset = PresetLazyReadOnly
			} else {
				cand.Preset = PresetFull
			}

			secretsBefore := countInlineSecrets(cand)
			saved, uerr := r.UpsertServer(cand, SparseUpdate{ReplaceAllFields: true, ReplaceAllSecrets: true})
			if uerr != nil {
				res.Skipped = append(res.Skipped, SkippedServerInfo{Name: name, Source: sf.source, Reason: uerr.Error()})
				continue
			}
			seenNames[low] = true
			res.Imported = append(res.Imported, ImportedServerInfo{
				Name:           saved.Name,
				Source:         sf.source,
				Scope:          saved.Scope,
				Transport:      saved.Transport,
				Preset:         saved.Preset,
				SecretsMoved:   secretsBefore,
				ConfigFilePath: targetPath,
			})
		}
	}
	return res, nil
}

func countInlineSecrets(cfg ExternalMCPConfig) int {
	n := 0
	for k, v := range cfg.Env {
		if v != "" && v != SecretMask && !isSecretRef(v) && (IsSecretEnvKey(k) || looksLikeSecretValue(v)) {
			n++
		}
	}
	for k, v := range cfg.Headers {
		if v != "" && v != SecretMask && !isSecretRef(v) && (IsSecretHeaderKey(k) || looksLikeSecretValue(v)) {
			n++
		}
	}
	return n
}

// FilterServerTools filters discovered external MCP tools by preset, EnabledTools,
// and DisabledTools. When serena uses preset lazy_readonly, ONLY the 5 semantic
// read-only tools in SerenaReadOnlySemanticTools are allowed.
func FilterServerTools(cfg ExternalMCPConfig, raw []ExternalTool) []ExternalTool {
	preset := strings.ToLower(strings.TrimSpace(cfg.Preset))
	if preset == "" {
		if IsSerenaServer(cfg) {
			preset = PresetLazyReadOnly
		} else {
			preset = PresetFull
		}
	}

	isSerena := IsSerenaServer(cfg)
	var allowSet map[string]bool
	if len(cfg.EnabledTools) > 0 {
		allowSet = make(map[string]bool, len(cfg.EnabledTools))
		for _, t := range cfg.EnabledTools {
			if trimmed := strings.TrimSpace(t); trimmed != "" {
				allowSet[trimmed] = true
			}
		}
	}
	var denySet map[string]bool
	if len(cfg.DisabledTools) > 0 {
		denySet = make(map[string]bool, len(cfg.DisabledTools))
		for _, t := range cfg.DisabledTools {
			if trimmed := strings.TrimSpace(t); trimmed != "" {
				denySet[trimmed] = true
			}
		}
	}

	out := make([]ExternalTool, 0, len(raw))
	for _, t := range raw {
		orig := strings.TrimSpace(t.OriginalName)
		if orig == "" {
			orig = strings.TrimSpace(t.Name)
		}
		if orig == "" {
			continue
		}

		// Preset gate runs first so lazy_readonly on serena cannot be widened to symbol writes.
		if preset == PresetLazyReadOnly {
			if isSerena {
				if !IsReadOnlySerenaToolName(orig) {
					continue
				}
			} else if !t.ReadOnly && !t.ReadOnlyHint && !IsReadOnlySerenaToolName(orig) {
				continue
			}
		}

		if allowSet != nil && !allowSet[orig] {
			continue
		}
		if denySet != nil && denySet[orig] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// EvaluateSerenaSuggestion evaluates whether smart serena escalation should be
// suggested based on workspace file count (>250), non-Go dominant stack, plan mode,
// or cross-file refactor tasks, along with current serena registry status.
func EvaluateSerenaSuggestion(workspace, mode string, isCrossFileRefactor bool) SerenaSuggestion {
	return EvaluateSerenaSuggestionWithRegistry(NewClientRegistry(workspace), mode, isCrossFileRefactor)
}

// EvaluateSerenaSuggestionWithRegistry runs EvaluateSerenaSuggestion using an explicit registry.
func EvaluateSerenaSuggestionWithRegistry(reg *ClientRegistry, mode string, isCrossFileRefactor bool) SerenaSuggestion {
	ws := "."
	if reg != nil && reg.workspace != "" {
		ws = reg.workspace
	}
	if abs, err := filepath.Abs(ws); err == nil {
		ws = abs
	}

	status := SerenaStatusNotConfigured
	serenaName := ""
	if reg != nil {
		if servers, err := reg.ListServers(); err == nil {
			for _, s := range servers {
				if strings.EqualFold(s.Name, "serena") || IsSerenaServer(s) {
					serenaName = s.Name
					if s.Enabled {
						status = SerenaStatusEnabled
					} else {
						status = SerenaStatusDisabledDraft
					}
					break
				}
			}
		}
	}

	fileCount, goCount, nonGoCount, topNonGoLang := countEligibleSourceFiles(ws)
	techStack, explicitDom := readProjectStackReadOnly(ws)
	dominantStack, isNonGo := detectDominantStack(techStack, explicitDom, goCount, nonGoCount, topNonGoLang)

	var reasons []string
	if fileCount > 250 {
		reasons = append(reasons, fmt.Sprintf("large workspace (%d eligible source files > 250)", fileCount))
	}
	if isNonGo {
		reasons = append(reasons, fmt.Sprintf("dominant stack is non-Go (%s)", dominantStack))
	}
	if strings.EqualFold(strings.TrimSpace(mode), "plan") {
		reasons = append(reasons, "plan/architect mode active")
	}
	if isCrossFileRefactor {
		reasons = append(reasons, "cross-file refactor task")
	}

	sugg := SerenaSuggestion{
		SerenaStatus:  status,
		SerenaName:    serenaName,
		FileCount:     fileCount,
		DominantStack: dominantStack,
		Reasons:       reasons,
	}
	if len(reasons) == 0 {
		return sugg
	}

	sugg.Recommended = true
	sugg.SuggestedPreset = PresetLazyReadOnly
	why := strings.Join(reasons, ", ")
	switch status {
	case SerenaStatusEnabled:
		sugg.Recommendation = fmt.Sprintf("serena is enabled (recommended for %s; use lazy_readonly semantic tools)", why)
	case SerenaStatusDisabledDraft:
		sugg.Recommendation = fmt.Sprintf("Enable draft serena MCP (preset: lazy_readonly) for %s: run `tether mcp enable serena`", why)
	default:
		sugg.Recommendation = fmt.Sprintf("Configure serena MCP (preset: lazy_readonly) for %s: run `tether mcp add serena ... --preset lazy_readonly --enable`", why)
	}
	return sugg
}

func countEligibleSourceFiles(workspace string) (total int, goCount int, nonGoCount int, topNonGoLang string) {
	m := mount.NewEngine(workspace)
	langCounts := make(map[string]int)

	_ = filepath.WalkDir(workspace, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path == workspace {
				return nil
			}
			name := d.Name()
			if name == ".git" || name == ".tether" || strings.HasPrefix(name, ".") || mount.IsDefaultSkipDirName(name) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(workspace, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if dec := m.AuthorizePath(rel, mount.OpRead, "code"); !dec.Allowed {
			return nil
		}

		lang := sourceLangByExt(strings.ToLower(filepath.Ext(d.Name())))
		if lang == "" {
			return nil
		}
		total++
		if lang == "go" {
			goCount++
		} else {
			nonGoCount++
			langCounts[lang]++
		}
		return nil
	})

	bestCount := 0
	for lang, c := range langCounts {
		if c > bestCount || (c == bestCount && lang < topNonGoLang) {
			bestCount = c
			topNonGoLang = lang
		}
	}
	return total, goCount, nonGoCount, topNonGoLang
}

func sourceLangByExt(ext string) string {
	switch ext {
	case ".go":
		return "go"
	case ".ts", ".tsx", ".mts", ".cts":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".svelte":
		return "svelte"
	case ".vue":
		return "vue"
	case ".py", ".pyi":
		return "python"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".kt", ".kts":
		return "kotlin"
	case ".rb":
		return "ruby"
	case ".php":
		return "php"
	case ".c", ".cc", ".cpp", ".cxx", ".h", ".hpp":
		return "c/c++"
	case ".cs":
		return "csharp"
	case ".swift":
		return "swift"
	default:
		return ""
	}
}

func readProjectStackReadOnly(workspace string) (techStack string, dominantStack string) {
	dbPath := filepath.Join(workspace, ".tether", "session.db")
	if _, err := os.Stat(dbPath); err == nil {
		if roDB, err := core.OpenReadOnlyDB(dbPath); err == nil {
			defer roDB.Close()
			rows, qerr := roDB.Query(`SELECT key, value FROM project_settings WHERE key IN ('tech_stack', 'dominant_stack')`)
			if qerr == nil {
				for rows.Next() {
					var k, v string
					if rows.Scan(&k, &v) == nil {
						switch k {
						case "tech_stack":
							techStack = strings.TrimSpace(v)
						case "dominant_stack":
							dominantStack = strings.TrimSpace(v)
						}
					}
				}
				_ = rows.Err()
				_ = rows.Close()
			}
		}
	}
	if techStack == "" {
		detected := core.DetectProjectSettings(workspace)
		techStack = strings.TrimSpace(detected["tech_stack"])
	}
	return techStack, dominantStack
}

func detectDominantStack(techStack, explicitDom string, goCount, nonGoCount int, topNonGoLang string) (dominant string, isNonGo bool) {
	if explicitDom != "" {
		low := strings.ToLower(explicitDom)
		return low, low != "go"
	}

	var hasGo bool
	var nonGoStacks []string
	for part := range strings.SplitSeq(techStack, ",") {
		p := strings.ToLower(strings.TrimSpace(part))
		if p == "" {
			continue
		}
		if p == "go" {
			hasGo = true
		} else {
			nonGoStacks = append(nonGoStacks, p)
		}
	}

	if len(nonGoStacks) > 0 && !hasGo {
		return nonGoStacks[0], true
	}
	if nonGoCount > goCount && nonGoCount > 0 {
		if topNonGoLang != "" {
			return topNonGoLang, true
		}
		if len(nonGoStacks) > 0 {
			return nonGoStacks[0], true
		}
		return "non-go", true
	}
	if hasGo || goCount > 0 {
		return "go", false
	}
	if len(nonGoStacks) > 0 {
		return nonGoStacks[0], true
	}
	return "", false
}
