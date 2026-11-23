package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxNamespacedToolLen    = 64
	maxExternalHTTPReply    = 16 * 1024 * 1024
	defaultExtMCPTimeout    = 30 * time.Second
	defaultStdioCloseWait   = 300 * time.Millisecond
	defaultStdioReapTimeout = 2 * time.Second
)

// ExternalTool describes a filtered, namespaced tool exposed by an external MCP server.
type ExternalTool struct {
	Name         string          `json:"name"`
	ServerName   string          `json:"server_name"`
	OriginalName string          `json:"original_name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	ReadOnly     bool            `json:"read_only,omitempty"`
	ReadOnlyHint bool            `json:"read_only_hint,omitempty"`
}

// ServerTestResult summarizes a live initialize + tools/list test against one MCP server.
type ServerTestResult struct {
	Name              string         `json:"name"`
	Scope             string         `json:"scope"`
	Transport         string         `json:"transport"`
	Preset            string         `json:"preset"`
	OK                bool           `json:"ok"`
	LatencyMs         int64          `json:"latency_ms"`
	RawToolCount      int            `json:"raw_tool_count"`
	FilteredToolCount int            `json:"filtered_tool_count"`
	Tools             []ExternalTool `json:"tools,omitempty"`
	Warnings          []string       `json:"warnings,omitempty"`
	Error             string         `json:"error,omitempty"`
}

// NamespacedToolName constructs a sanitized [a-zA-Z0-9_-] tool name <= 64 chars
// formatted as mcp_<server>__<tool>.
func NamespacedToolName(serverName, toolName string) string {
	s := sanitizeIdentPart(serverName)
	t := sanitizeIdentPart(toolName)
	if s == "" {
		s = "server"
	}
	if t == "" {
		t = "tool"
	}
	full := "mcp_" + s + "__" + t
	if len(full) <= maxNamespacedToolLen {
		return full
	}
	sum := sha256.Sum256([]byte(serverName + "\x00" + toolName))
	suffix := "_" + hex.EncodeToString(sum[:4])
	keep := maxNamespacedToolLen - len(suffix)
	return full[:keep] + suffix
}

func sanitizeIdentPart(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ValidateServerURL verifies http:// or https:// scheme, rejects embedded userinfo
// and link-local cloud metadata SSRF targets, and returns optional local-host warnings.
func ValidateServerURL(rawURL string) ([]string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return nil, fmt.Errorf("url cannot be empty")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("unsupported url scheme %q (only http:// and https:// are allowed)", u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("embedded credentials in url are not allowed; use --header instead")
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return nil, fmt.Errorf("url host cannot be empty")
	}

	lowerHost := strings.ToLower(host)
	if lowerHost == "metadata.google.internal" || lowerHost == "metadata.goog" || lowerHost == "169.254.169.254" {
		return nil, fmt.Errorf("blocked metadata host %q", host)
	}

	var warnings []string
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return nil, fmt.Errorf("blocked unsafe IP target %q", host)
		}
		if ip.IsLoopback() || ip.IsPrivate() {
			warnings = append(warnings, fmt.Sprintf("MCP URL targets local/private IP %s; ensure this server is trusted", host))
		}
	} else if lowerHost == "localhost" || strings.HasSuffix(lowerHost, ".localhost") || strings.HasSuffix(lowerHost, ".local") {
		warnings = append(warnings, fmt.Sprintf("MCP URL targets local host %s; ensure this server is trusted", host))
	}

	return warnings, nil
}

func newExternalHTTPClient() *http.Client {
	return &http.Client{
		Timeout: defaultExtMCPTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			from := ""
			if len(via) > 0 && via[0].URL != nil {
				from = via[0].URL.Host
			}
			return fmt.Errorf("external MCP HTTP redirect blocked (%s -> %s)", from, req.URL.Host)
		},
	}
}

// ClientManager manages lazy connections, tool discovery, filtering, and
// execution across enabled external MCP servers.
type ClientManager struct {
	registry *ClientRegistry
	client   *http.Client

	mu         sync.Mutex
	conns      map[string]*externalClient
	tools      map[string]ExternalTool // namespaced tool name -> descriptor
	loadErrors map[string]string       // server name -> last load error
	closed     bool
}

// NewClientManager creates a lazy ClientManager for workspace using DefaultConfigDir.
func NewClientManager(workspace string) *ClientManager {
	return NewClientManagerFromRegistry(NewClientRegistry(workspace))
}

// NewClientManagerWithDirs creates a lazy ClientManager with explicit configDir and homeDir.
func NewClientManagerWithDirs(workspace, configDir, homeDir string) *ClientManager {
	return NewClientManagerFromRegistry(NewClientRegistryWithDirs(workspace, configDir, homeDir))
}

// NewClientManagerFromRegistry creates a lazy ClientManager backed by registry.
// It performs zero subprocess spawns or network connections on construction.
func NewClientManagerFromRegistry(registry *ClientRegistry) *ClientManager {
	return &ClientManager{
		registry:   registry,
		client:     newExternalHTTPClient(),
		conns:      make(map[string]*externalClient),
		tools:      make(map[string]ExternalTool),
		loadErrors: make(map[string]string),
	}
}

func (m *ClientManager) Registry() *ClientRegistry {
	if m == nil {
		return nil
	}
	return m.registry
}

// ActiveConnections returns the sorted names of currently connected external servers.
func (m *ClientManager) ActiveConnections() []string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.conns))
	for k := range m.conns {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// LoadErrors returns any per-server errors captured during the most recent ListEnabledTools.
func (m *ClientManager) LoadErrors() map[string]string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.loadErrors)
}

// EnsureConnected lazily connects to the named server (or returns an existing live connection).
func (m *ClientManager) EnsureConnected(ctx context.Context, name string) (*externalClient, error) {
	if m == nil || m.registry == nil {
		return nil, fmt.Errorf("external MCP manager is not initialized")
	}
	srv, ok, err := m.registry.GetServer(name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("external MCP server %q not found", name)
	}

	key := strings.ToLower(srv.Name)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("external MCP manager is closed")
	}
	if existing, exists := m.conns[key]; exists && !existing.isClosed() {
		m.mu.Unlock()
		return existing, nil
	}
	m.mu.Unlock()

	resolved, err := m.registry.ResolveRuntimeSecrets(srv)
	if err != nil {
		return nil, err
	}
	conn, _, err := m.dialAndHandshake(ctx, resolved)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = conn.Close()
		return nil, fmt.Errorf("external MCP manager is closed")
	}
	if existing, exists := m.conns[key]; exists && !existing.isClosed() {
		m.mu.Unlock()
		_ = conn.Close()
		return existing, nil
	}
	m.conns[key] = conn
	m.mu.Unlock()
	return conn, nil
}

func (m *ClientManager) dialAndHandshake(ctx context.Context, cfg ExternalMCPConfig) (*externalClient, []string, error) {
	var warnings []string
	var tr mcpTransport
	var err error

	switch strings.ToLower(strings.TrimSpace(cfg.Transport)) {
	case TransportStdio:
		tr, err = dialStdioTransport(cfg, m.registry.Workspace())
	case TransportHTTP, TransportSSE:
		warnings, err = ValidateServerURL(cfg.URL)
		if err == nil {
			tr = dialHTTPTransport(cfg, m.client)
		}
	default:
		err = fmt.Errorf("unsupported MCP transport %q", cfg.Transport)
	}
	if err != nil {
		return nil, warnings, err
	}

	client := &externalClient{
		cfg:       cfg,
		transport: tr,
	}
	if err := client.initialize(ctx); err != nil {
		_ = client.Close()
		return nil, warnings, err
	}
	return client, warnings, nil
}

// ListEnabledTools lazily connects ONLY to servers with Enabled == true, discovers
// their tools, applies preset/allow/deny filters, namespaces names cleanly, and
// returns deterministic sorted tool descriptors.
func (m *ClientManager) ListEnabledTools(ctx context.Context) ([]ExternalTool, error) {
	if m == nil || m.registry == nil {
		return nil, nil
	}
	servers, err := m.registry.ListServers()
	if err != nil {
		return nil, err
	}

	var enabled []ExternalMCPConfig
	for _, s := range servers {
		if s.Enabled {
			enabled = append(enabled, s)
		}
	}
	if len(enabled) == 0 {
		m.mu.Lock()
		m.tools = make(map[string]ExternalTool)
		m.loadErrors = make(map[string]string)
		m.mu.Unlock()
		return nil, nil
	}

	var all []ExternalTool
	nextMap := make(map[string]ExternalTool)
	nextErrs := make(map[string]string)

	for _, srv := range enabled {
		conn, cerr := m.EnsureConnected(ctx, srv.Name)
		if cerr != nil {
			nextErrs[srv.Name] = cerr.Error()
			continue
		}
		rawTools, lerr := conn.listTools(ctx)
		if lerr != nil {
			nextErrs[srv.Name] = lerr.Error()
			continue
		}
		filtered := FilterServerTools(srv, rawTools)
		for _, t := range filtered {
			ns := uniqueNamespacedName(nextMap, srv.Name, t.OriginalName)
			t.Name = ns
			t.ServerName = srv.Name
			nextMap[ns] = t
			all = append(all, t)
		}
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].Name < all[j].Name
	})

	m.mu.Lock()
	m.tools = nextMap
	m.loadErrors = nextErrs
	m.mu.Unlock()

	return all, nil
}

func uniqueNamespacedName(used map[string]ExternalTool, serverName, rawToolName string) string {
	base := NamespacedToolName(serverName, rawToolName)
	if _, exists := used[base]; !exists {
		return base
	}
	for n := 2; n < 1000; n++ {
		sfx := fmt.Sprintf("_%d", n)
		prefix := base
		if len(prefix)+len(sfx) > maxNamespacedToolLen {
			prefix = prefix[:maxNamespacedToolLen-len(sfx)]
		}
		cand := prefix + sfx
		if _, exists := used[cand]; !exists {
			return cand
		}
	}
	return base
}

// LookupTool returns the cached ExternalTool metadata for a namespaced tool name.
func (m *ClientManager) LookupTool(namespacedName string) (ExternalTool, bool) {
	if m == nil {
		return ExternalTool{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tools[namespacedName]
	return t, ok
}

// CallTool invokes a filtered, namespaced external MCP tool (mcp_<server>__<tool>).
func (m *ClientManager) CallTool(ctx context.Context, namespacedName string, args map[string]any) (string, bool) {
	if m == nil || m.registry == nil {
		return "external MCP manager is not initialized", true
	}

	toolMeta, ok := m.LookupTool(namespacedName)
	if !ok {
		// Refresh enabled tools once if called without prior ListEnabledTools.
		if _, lerr := m.ListEnabledTools(ctx); lerr == nil {
			toolMeta, ok = m.LookupTool(namespacedName)
		}
	}
	if !ok {
		return fmt.Sprintf("external MCP tool %q is not enabled or permitted", namespacedName), true
	}

	srv, exists, err := m.registry.GetServer(toolMeta.ServerName)
	if err != nil {
		return fmt.Sprintf("load server %q: %v", toolMeta.ServerName, err), true
	}
	if !exists || !srv.Enabled {
		return fmt.Sprintf("external MCP server %q is disabled or missing", toolMeta.ServerName), true
	}
	if len(FilterServerTools(srv, []ExternalTool{toolMeta})) == 0 {
		return fmt.Sprintf("external MCP tool %q blocked by preset/filter policy", namespacedName), true
	}

	conn, err := m.EnsureConnected(ctx, srv.Name)
	if err != nil {
		return fmt.Sprintf("connect %q: %v", srv.Name, err), true
	}
	return conn.callTool(ctx, toolMeta.OriginalName, args)
}

// TestServer runs a live initialize + tools/list check against a configured server
// (whether enabled or disabled draft), closing temporary test connections cleanly.
func (m *ClientManager) TestServer(ctx context.Context, name string) (ServerTestResult, error) {
	if m == nil || m.registry == nil {
		return ServerTestResult{}, fmt.Errorf("external MCP manager is not initialized")
	}
	srv, ok, err := m.registry.GetServer(name)
	if err != nil {
		return ServerTestResult{}, err
	}
	if !ok {
		return ServerTestResult{}, fmt.Errorf("MCP server %q not found", name)
	}

	res := ServerTestResult{
		Name:      srv.Name,
		Scope:     srv.Scope,
		Transport: srv.Transport,
		Preset:    srv.Preset,
	}

	resolved, err := m.registry.ResolveRuntimeSecrets(srv)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}

	start := time.Now()
	conn, warnings, err := m.dialAndHandshake(ctx, resolved)
	res.Warnings = warnings
	if err != nil {
		res.LatencyMs = time.Since(start).Milliseconds()
		res.Error = err.Error()
		return res, err
	}
	defer func() {
		_ = conn.Close()
	}()

	rawTools, err := conn.listTools(ctx)
	res.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = err.Error()
		return res, err
	}

	filtered := FilterServerTools(srv, rawTools)
	for i := range filtered {
		filtered[i].Name = NamespacedToolName(srv.Name, filtered[i].OriginalName)
		filtered[i].ServerName = srv.Name
	}

	res.OK = true
	res.RawToolCount = len(rawTools)
	res.FilteredToolCount = len(filtered)
	res.Tools = filtered
	return res, nil
}

// Close shuts down all active external MCP connections cleanly (zero zombie processes).
func (m *ClientManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.closed = true
	conns := make([]*externalClient, 0, len(m.conns))
	for k, c := range m.conns {
		conns = append(conns, c)
		delete(m.conns, k)
	}
	m.mu.Unlock()

	var errs []error
	for _, c := range conns {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// --- Protocol & Transport Layer ---

type rpcWireRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcWireResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type mcpTransport interface {
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	notify(ctx context.Context, method string, params any) error
	isClosed() bool
	Close() error
}

type externalClient struct {
	cfg       ExternalMCPConfig
	transport mcpTransport
}

func (c *externalClient) isClosed() bool {
	return c == nil || c.transport == nil || c.transport.isClosed()
}

func (c *externalClient) Close() error {
	if c == nil || c.transport == nil {
		return nil
	}
	return c.transport.Close()
}

func (c *externalClient) initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "tether",
			"version": "0.3.0",
		},
	}
	if _, err := c.transport.call(ctx, "initialize", params); err != nil {
		return fmt.Errorf("mcp initialize (%s): %w", c.cfg.Name, err)
	}
	if err := c.transport.notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return fmt.Errorf("mcp notifications/initialized (%s): %w", c.cfg.Name, err)
	}
	return nil
}

func (c *externalClient) listTools(ctx context.Context) ([]ExternalTool, error) {
	var all []ExternalTool
	cursor := ""
	const maxPages = 20

	for range maxPages {
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		rawResult, err := c.transport.call(ctx, "tools/list", params)
		if err != nil {
			return nil, fmt.Errorf("tools/list (%s): %w", c.cfg.Name, err)
		}

		var decoded struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
				Annotations *struct {
					ReadOnlyHint bool `json:"readOnlyHint"`
				} `json:"annotations,omitempty"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(rawResult, &decoded); err != nil {
			return nil, fmt.Errorf("decode tools/list (%s): %w", c.cfg.Name, err)
		}

		for _, t := range decoded.Tools {
			orig := strings.TrimSpace(t.Name)
			if orig == "" {
				continue
			}
			roHint := t.Annotations != nil && t.Annotations.ReadOnlyHint
			all = append(all, ExternalTool{
				ServerName:   c.cfg.Name,
				OriginalName: orig,
				Description:  t.Description,
				InputSchema:  normalizeToolSchema(t.InputSchema),
				ReadOnly:     IsSerenaServer(c.cfg) && IsReadOnlySerenaToolName(orig),
				ReadOnlyHint: roHint,
			})
		}

		if strings.TrimSpace(decoded.NextCursor) == "" || decoded.NextCursor == cursor {
			break
		}
		cursor = decoded.NextCursor
	}
	return all, nil
}

func normalizeToolSchema(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	var obj map[string]any
	if err := json.Unmarshal(trimmed, &obj); err != nil || obj == nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	if _, hasType := obj["type"]; !hasType {
		obj["type"] = "object"
	}
	if _, hasProps := obj["properties"]; !hasProps {
		obj["properties"] = map[string]any{}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return json.RawMessage(out)
}

func (c *externalClient) callTool(ctx context.Context, rawToolName string, args map[string]any) (string, bool) {
	if args == nil {
		args = map[string]any{}
	}
	rawResult, err := c.transport.call(ctx, "tools/call", map[string]any{
		"name":      rawToolName,
		"arguments": args,
	})
	if err != nil {
		return err.Error(), true
	}

	var tr struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Data string `json:"data,omitempty"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(rawResult, &tr); err != nil {
		return string(rawResult), false
	}

	var parts []string
	for _, blk := range tr.Content {
		switch {
		case blk.Text != "":
			parts = append(parts, blk.Text)
		case blk.Data != "":
			parts = append(parts, blk.Data)
		default:
			if raw, merr := json.Marshal(blk); merr == nil {
				parts = append(parts, string(raw))
			}
		}
	}
	text := strings.Join(parts, "\n")
	if text == "" && len(rawResult) > 0 {
		text = string(rawResult)
	}
	return text, tr.IsError
}

// --- Stdio Subprocess Transport ---

type limitedBuffer struct {
	mu  sync.Mutex
	max int
	buf bytes.Buffer
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.max <= 0 {
		return len(p), nil
	}
	if l.buf.Len() < l.max {
		rem := l.max - l.buf.Len()
		if len(p) > rem {
			l.buf.Write(p[:rem])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.TrimSpace(l.buf.String())
}

type stdioTransport struct {
	cmd        *exec.Cmd
	procCancel context.CancelFunc
	stdin      io.WriteCloser
	stderr     *limitedBuffer

	writeMu sync.Mutex
	mu      sync.Mutex
	closed  bool
	nextID  atomic.Int64
	pending map[string]chan rpcWireResponse

	readDone chan struct{}
	waitDone chan struct{}
	readErr  error
}

func dialStdioTransport(cfg ExternalMCPConfig, workspace string) (*stdioTransport, error) {
	procCtx, procCancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, cfg.Command, cfg.Args...)
	configureProcessGroup(cmd)
	if workspace != "" {
		cmd.Dir = workspace
	}
	if len(cfg.Env) > 0 {
		cmd.Env = mergeProcessEnv(os.Environ(), cfg.Env)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		procCancel()
		return nil, fmt.Errorf("stdin pipe (%s): %w", cfg.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		procCancel()
		_ = stdin.Close()
		return nil, fmt.Errorf("stdout pipe (%s): %w", cfg.Name, err)
	}
	stderrBuf := &limitedBuffer{max: 8192}
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		procCancel()
		_ = stdin.Close()
		return nil, fmt.Errorf("start MCP process %q: %w", cfg.Command, err)
	}

	st := &stdioTransport{
		cmd:        cmd,
		procCancel: procCancel,
		stdin:      stdin,
		stderr:     stderrBuf,
		pending:    make(map[string]chan rpcWireResponse),
		readDone:   make(chan struct{}),
		waitDone:   make(chan struct{}),
	}

	go st.readLoop(stdout)
	go st.reapLoop()
	return st, nil
}

func mergeProcessEnv(base []string, overrides map[string]string) []string {
	merged := make(map[string]string, len(base)+len(overrides))
	for _, entry := range base {
		k, v, ok := strings.Cut(entry, "=")
		if ok {
			merged[k] = v
		}
	}
	maps.Copy(merged, overrides)
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+merged[k])
	}
	return out
}

func readLineBounded(br *bufio.Reader, maxBytes int) (string, error) {
	var full []byte
	for {
		frag, err := br.ReadSlice('\n')
		if len(frag) > 0 {
			if len(full)+len(frag) > maxBytes {
				return "", fmt.Errorf("stdio frame exceeds max size (%d bytes)", maxBytes)
			}
			full = append(full, frag...)
		}
		if err == nil {
			return string(full), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if len(full) > 0 {
			return string(full), err
		}
		return "", err
	}
}

func (t *stdioTransport) readLoop(r io.Reader) {
	defer close(t.readDone)
	br := bufio.NewReaderSize(r, 64*1024)

	for {
		line, err := readLineBounded(br, maxExternalHTTPReply+1)
		if err != nil && len(line) == 0 {
			t.failPending(err)
			if !errors.Is(err, io.EOF) {
				t.abortFromReadError()
			}
			return
		}
		trimmed := strings.TrimSpace(line)
		var payload []byte
		if lenVal, hasCL := parseContentLengthHeader(trimmed); hasCL {
			for {
				hdr, herr := readLineBounded(br, 64*1024)
				if herr != nil {
					t.failPending(herr)
					t.abortFromReadError()
					return
				}
				if strings.TrimSpace(hdr) == "" {
					break
				}
			}
			if lenVal <= 0 || lenVal > maxExternalHTTPReply {
				t.failPending(fmt.Errorf("invalid or oversize stdio Content-Length: %d", lenVal))
				t.abortFromReadError()
				return
			}
			payload = make([]byte, lenVal)
			if _, rerr := io.ReadFull(br, payload); rerr != nil {
				t.failPending(rerr)
				t.abortFromReadError()
				return
			}
		} else if trimmed != "" {
			if len(trimmed) > maxExternalHTTPReply {
				t.failPending(fmt.Errorf("stdio JSON-RPC frame exceeds max size (%d bytes)", maxExternalHTTPReply))
				t.abortFromReadError()
				return
			}
			payload = []byte(trimmed)
		}

		if len(payload) > 0 {
			var resp rpcWireResponse
			if uerr := json.Unmarshal(payload, &resp); uerr == nil {
				idKey := normalizeRPCID(resp.ID)
				if idKey != "" {
					t.mu.Lock()
					ch, ok := t.pending[idKey]
					if ok {
						delete(t.pending, idKey)
					}
					t.mu.Unlock()
					if ok {
						ch <- resp
						close(ch)
					}
				}
			}
		}

		if err != nil {
			t.failPending(err)
			return
		}
	}
}

func parseContentLengthHeader(line string) (int, bool) {
	k, v, ok := strings.Cut(line, ":")
	if !ok || !strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return -1, true
	}
	return n, true
}

func (t *stdioTransport) abortFromReadError() {
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		stdin := t.stdin
		t.mu.Unlock()
		if stdin != nil {
			_ = stdin.Close()
		}
		t.procCancel()
		killProcessGroup(t.cmd)
		return
	}
	t.mu.Unlock()
}

func (t *stdioTransport) reapLoop() {
	<-t.readDone
	_ = t.cmd.Wait()
	close(t.waitDone)
}

func (t *stdioTransport) failPending(err error) {
	t.mu.Lock()
	if t.readErr == nil && err != nil {
		t.readErr = err
	}
	for k, ch := range t.pending {
		delete(t.pending, k)
		close(ch)
	}
	t.mu.Unlock()
}

func normalizeRPCID(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return "s:" + s
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return "n:" + strconv.FormatInt(n, 10)
	}
	return trimmed
}

func (t *stdioTransport) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

func (t *stdioTransport) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := t.nextID.Add(1)
	idKey := "n:" + strconv.FormatInt(id, 10)
	ch := make(chan rpcWireResponse, 1)

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, fmt.Errorf("stdio MCP transport closed")
	}
	t.pending[idKey] = ch
	t.mu.Unlock()

	req := rpcWireRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.removePending(idKey)
		return nil, err
	}

	t.writeMu.Lock()
	_, werr := t.stdin.Write(append(body, '\n'))
	t.writeMu.Unlock()
	if werr != nil {
		t.removePending(idKey)
		return nil, fmt.Errorf("write stdio request (%s): %w", method, werr)
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			stderrMsg := t.stderr.String()
			if stderrMsg != "" {
				return nil, fmt.Errorf("stdio MCP process closed during %s (stderr: %s)", method, stderrMsg)
			}
			return nil, fmt.Errorf("stdio MCP process closed during %s", method)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("MCP error (%d): %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	case <-ctx.Done():
		t.removePending(idKey)
		return nil, ctx.Err()
	}
}

func (t *stdioTransport) notify(_ context.Context, method string, params any) error {
	req := rpcWireRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if t.isClosed() {
		return fmt.Errorf("stdio MCP transport closed")
	}
	_, err = t.stdin.Write(append(body, '\n'))
	return err
}

func (t *stdioTransport) removePending(idKey string) {
	t.mu.Lock()
	delete(t.pending, idKey)
	t.mu.Unlock()
}

func (t *stdioTransport) Close() error {
	t.mu.Lock()
	alreadyClosed := t.closed
	t.closed = true
	for k, ch := range t.pending {
		delete(t.pending, k)
		close(ch)
	}
	stdin := t.stdin
	t.mu.Unlock()

	if !alreadyClosed && stdin != nil {
		_ = stdin.Close()
	}

	select {
	case <-t.waitDone:
	case <-time.After(defaultStdioCloseWait):
		t.procCancel()
		killProcessGroup(t.cmd)
		select {
		case <-t.waitDone:
		case <-time.After(defaultStdioReapTimeout):
		}
	}
	t.procCancel()
	return nil
}

// --- HTTP / SSE Transport ---

type httpTransport struct {
	url       string
	headers   map[string]string
	client    *http.Client
	mu        sync.Mutex
	sessionID string
	closed    bool
	nextID    atomic.Int64
}

func dialHTTPTransport(cfg ExternalMCPConfig, client *http.Client) *httpTransport {
	if client == nil {
		client = newExternalHTTPClient()
	}
	return &httpTransport{
		url:     strings.TrimSpace(cfg.URL),
		headers: cloneStringMap(cfg.Headers),
		client:  client,
	}
}

func (t *httpTransport) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

func (t *httpTransport) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	return nil
}

func (t *httpTransport) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := t.nextID.Add(1)
	return t.post(ctx, rpcWireRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}, id, false)
}

func (t *httpTransport) notify(ctx context.Context, method string, params any) error {
	_, err := t.post(ctx, rpcWireRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}, 0, true)
	return err
}

func (t *httpTransport) post(ctx context.Context, reqPayload rpcWireRequest, reqID int64, isNotification bool) (json.RawMessage, error) {
	if t.isClosed() {
		return nil, fmt.Errorf("http MCP transport closed")
	}
	bodyBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	t.mu.Lock()
	sid := t.sessionID
	t.mu.Unlock()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if newSID := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id")); newSID != "" {
		t.mu.Lock()
		t.sessionID = newSID
		t.mu.Unlock()
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxExternalHTTPReply+1))
	if err != nil {
		return nil, fmt.Errorf("read MCP HTTP response: %w", err)
	}
	if int64(len(respBody)) > maxExternalHTTPReply {
		return nil, fmt.Errorf("MCP HTTP response exceeds max size (%d bytes)", maxExternalHTTPReply)
	}
	if resp.StatusCode >= 400 {
		snippet := strings.TrimSpace(string(respBody))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "..."
		}
		return nil, fmt.Errorf("MCP HTTP %d: %s", resp.StatusCode, snippet)
	}
	if isNotification || resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		if len(bytes.TrimSpace(respBody)) == 0 {
			return nil, nil
		}
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	trimmed := bytes.TrimSpace(respBody)
	if strings.Contains(ct, "text/event-stream") || bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte("event:")) {
		return parseSSEResponse(trimmed, reqID)
	}

	var rpcResp rpcWireResponse
	if err := json.Unmarshal(trimmed, &rpcResp); err != nil {
		return nil, fmt.Errorf("decode MCP JSON response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("MCP error (%d): %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

func parseSSEResponse(raw []byte, wantID int64) (json.RawMessage, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), maxExternalHTTPReply)

	wantKey := "n:" + strconv.FormatInt(wantID, 10)
	var dataLines []string
	var fallback *rpcWireResponse

	flushEvent := func() (*rpcWireResponse, bool) {
		if len(dataLines) == 0 {
			return nil, false
		}
		payload := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = nil
		if payload == "" || payload == "[DONE]" {
			return nil, false
		}
		var msg rpcWireResponse
		if err := json.Unmarshal([]byte(payload), &msg); err != nil {
			return nil, false
		}
		if normalizeRPCID(msg.ID) == wantKey {
			return &msg, true
		}
		if fallback == nil && (len(msg.Result) > 0 || msg.Error != nil) {
			cp := msg
			fallback = &cp
		}
		return nil, false
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if match, ok := flushEvent(); ok {
				if match.Error != nil {
					return nil, fmt.Errorf("MCP error (%d): %s", match.Error.Code, match.Error.Message)
				}
				return match.Result, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			dataLines = append(dataLines, strings.TrimSpace(rest))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan SSE MCP response: %w", err)
	}
	if match, ok := flushEvent(); ok {
		if match.Error != nil {
			return nil, fmt.Errorf("MCP error (%d): %s", match.Error.Code, match.Error.Message)
		}
		return match.Result, nil
	}
	if fallback != nil {
		if fallback.Error != nil {
			return nil, fmt.Errorf("MCP error (%d): %s", fallback.Error.Code, fallback.Error.Message)
		}
		return fallback.Result, nil
	}
	return nil, fmt.Errorf("no JSON-RPC reply found in text/event-stream response")
}
