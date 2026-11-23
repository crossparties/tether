package webui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/harness"
	"github.com/crossparties/tether/internal/mcp"
	"github.com/crossparties/tether/internal/safety"
	"github.com/crossparties/tether/internal/snapshot"
)

//go:embed all:dist
var distFS embed.FS

// Config configures the local Tether web server.
type Config struct {
	Engine       *core.Engine
	Runner       *harness.Runner
	RunnerConfig harness.Config
	Port         int
	Version      string
}

type approvalReply struct {
	verdict mcp.Verdict
	reason  string
	ok      bool
}

type pendingApproval struct {
	ID          string `json:"id"`
	Tool        string `json:"tool"`
	Path        string `json:"path,omitempty"`
	Command     string `json:"command,omitempty"`
	Label       string `json:"label"`
	Preview     string `json:"preview,omitempty"`
	FullPreview string `json:"full_preview,omitempty"`
	ScopeHint   string `json:"scope_hint,omitempty"`
	Risk        string `json:"risk,omitempty"`
	Reason      string `json:"reason,omitempty"`
	ForceAsk    bool   `json:"force_ask,omitempty"`
	replyCh     chan approvalReply
}

// Server hosts Tether's local HTTP REST API, multiplexed SSE stream,
// pure-Go PTY WebSocket, sandboxed preview endpoint, and embedded Svelte 5 UI.
type Server struct {
	lifecycleMu sync.Mutex

	stateMu       sync.RWMutex
	engine        *core.Engine
	runner        *harness.Runner
	registry      *mcp.ClientRegistry
	baseRunnerCfg harness.Config
	ownsWorkspace bool
	version       string

	hub *EventHub

	authMu         sync.Mutex
	bootstrapToken string
	bootstrapUsed  bool
	sessions       map[string]*sessionState
	boundPort      int

	turnMu           sync.Mutex
	activeTurnCancel context.CancelFunc
	activeTurnID     string
	activeTurnDone   chan struct{}

	approvalMu       sync.Mutex
	pendingApprovals map[string]*pendingApproval
	approvalSeq      int64

	termMu       sync.Mutex
	termCleanups map[uint64]func()
	termSeq      uint64

	mcpTestMu sync.Mutex
	testedMCP map[string]string

	mux        *http.ServeMux
	handler    http.Handler
	httpServer *http.Server

	listenMu sync.Mutex
	listener net.Listener
	addr     string

	shutdownOnce sync.Once
	shutdownCh   chan struct{}
}

// NewServer constructs a local-only Web UI Server bound to 127.0.0.1.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Engine == nil {
		return nil, fmt.Errorf("webui server requires an active core.Engine")
	}
	if cfg.Port < 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("invalid port %d", cfg.Port)
	}
	tok, err := randomHexToken(32)
	if err != nil {
		return nil, fmt.Errorf("generate bootstrap token: %w", err)
	}
	ver := strings.TrimSpace(cfg.Version)
	if ver == "" {
		ver = "0.3.0"
	}
	run := cfg.Runner
	if run == nil {
		run = harness.NewRunner(cfg.Engine, cfg.RunnerConfig)
	}

	s := &Server{
		engine:           cfg.Engine,
		runner:           run,
		registry:         mcp.NewClientRegistry(cfg.Engine.Workspace),
		baseRunnerCfg:    cfg.RunnerConfig,
		version:          ver,
		hub:              NewEventHub(sseRingCapacity),
		bootstrapToken:   tok,
		sessions:         make(map[string]*sessionState),
		boundPort:        cfg.Port,
		pendingApprovals: make(map[string]*pendingApproval),
		termCleanups:     make(map[uint64]func()),
		testedMCP:        make(map[string]string),
		shutdownCh:       make(chan struct{}),
	}

	s.bindRunner(run)
	s.mux = s.buildMux()
	s.handler = s.authMiddleware(s.mux)
	return s, nil
}

// Handler returns the complete auth-wrapped http.Handler (useful for httptest.Server).
func (s *Server) Handler() http.Handler {
	return s.handler
}

// Addr returns the bound "127.0.0.1:<port>" address once Listen/Start has run.
func (s *Server) Addr() string {
	s.listenMu.Lock()
	defer s.listenMu.Unlock()
	return s.addr
}

// Listen binds exclusively to 127.0.0.1:<port> without blocking.
func (s *Server) Listen() (string, error) {
	s.listenMu.Lock()
	defer s.listenMu.Unlock()
	if s.listener != nil {
		return s.addr, nil
	}

	s.authMu.Lock()
	port := s.boundPort
	s.authMu.Unlock()

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", err
	}
	s.listener = ln
	s.addr = ln.Addr().String()
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok {
		s.authMu.Lock()
		s.boundPort = tcpAddr.Port
		s.authMu.Unlock()
	}
	return s.addr, nil
}

// Serve accepts HTTP connections on the bound loopback listener until Shutdown.
func (s *Server) Serve() error {
	if _, err := s.Listen(); err != nil {
		return err
	}
	s.listenMu.Lock()
	ln := s.listener
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.httpServer = srv
	s.listenMu.Unlock()

	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully terminates HTTP serving, SSE clients, active turns, pending
// approvals, and active PTY WebSockets.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		close(s.shutdownCh)
	})
	s.abortPendingApprovals("server shutting down")

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	s.abortActiveChat("server shutting down")
	s.closeAllTerminals()
	s.hub.Close()

	s.listenMu.Lock()
	srv := s.httpServer
	ln := s.listener
	s.listenMu.Unlock()

	var errs []error
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	} else if ln != nil {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}

	s.stateMu.RLock()
	owns := s.ownsWorkspace
	run := s.runner
	eng := s.engine
	s.stateMu.RUnlock()
	if owns {
		if run != nil {
			_ = run.Close()
		}
		if eng != nil {
			_ = eng.Close()
		}
	}
	return errors.Join(errs...)
}

func (s *Server) activeState() (*core.Engine, *harness.Runner) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.engine, s.runner
}

func (s *Server) activeRegistry() *mcp.ClientRegistry {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.registry
}

func (s *Server) bindRunner(run *harness.Runner) {
	if run == nil {
		return
	}
	run.SetAutoApprovePolicy(run.DefaultAutoApprovePolicy())
	run.SetAskPolicyWithReason(s.webAskPolicy)
	run.SetOnEvent(s.handleRunnerEvent)
}

func (s *Server) handleRunnerEvent(kind, detail string) {
	switch kind {
	case "keys.switched", "stream.retry_reset", "condenser.ran", "crew.status", "crew.delta", "lease.updated", "stuck_detection":
		s.hub.Publish(kind, detail)
	}
}

func (s *Server) webAskPolicy(d mcp.Decision) (mcp.Verdict, string, bool) {
	target := d.Path
	if target == "" {
		target = d.Command
	}
	if target == "" {
		target = d.Label
	}

	risk, riskReason := safety.ClassifyRisk(d.Tool, target)

	var scopeHint string
	if d.Command != "" {
		if prefix := mcp.CommandPrefix(d.Command); prefix != "" {
			scopeHint = fmt.Sprintf("commands starting with %q", prefix)
		}
	} else if d.Path != "" {
		dir := filepath.ToSlash(filepath.Dir(d.Path))
		if dir == "." || dir == "/" || dir == "" {
			dir = filepath.ToSlash(d.Path)
		}
		scopeHint = fmt.Sprintf("changes under %q", dir)
	}

	s.approvalMu.Lock()
	s.approvalSeq++
	id := fmt.Sprintf("apr-%d", s.approvalSeq)
	req := &pendingApproval{
		ID:          id,
		Tool:        d.Tool,
		Path:        d.Path,
		Command:     d.Command,
		Label:       d.Label,
		Preview:     d.Preview,
		FullPreview: d.FullPreview,
		ScopeHint:   scopeHint,
		Risk:        string(risk),
		Reason:      riskReason,
		ForceAsk:    d.ForceAsk,
		replyCh:     make(chan approvalReply, 1),
	}
	s.pendingApprovals[id] = req
	s.approvalMu.Unlock()

	defer func() {
		s.approvalMu.Lock()
		delete(s.pendingApprovals, id)
		s.approvalMu.Unlock()
	}()

	s.hub.Publish("approval.required", req)

	select {
	case rep := <-req.replyCh:
		return rep.verdict, rep.reason, rep.ok
	case <-s.shutdownCh:
		return mcp.VerdictReject, "server shutting down", false
	case <-time.After(15 * time.Minute):
		return mcp.VerdictReject, "approval timed out", false
	}
}

func (s *Server) abortPendingApprovals(reason string) {
	s.approvalMu.Lock()
	list := make([]*pendingApproval, 0, len(s.pendingApprovals))
	for id, req := range s.pendingApprovals {
		list = append(list, req)
		delete(s.pendingApprovals, id)
	}
	s.approvalMu.Unlock()

	for _, req := range list {
		select {
		case req.replyCh <- approvalReply{verdict: mcp.VerdictReject, reason: reason, ok: false}:
		default:
		}
		s.hub.Publish("approval.resolved", map[string]any{
			"id":      req.ID,
			"verdict": "reject",
			"reason":  reason,
		})
	}
}

func (s *Server) abortActiveChat(reason string) bool {
	s.abortPendingApprovals(reason)

	s.turnMu.Lock()
	cancel := s.activeTurnCancel
	done := s.activeTurnDone
	s.turnMu.Unlock()

	if cancel == nil {
		return false
	}
	cancel()
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	return true
}

func (s *Server) registerTerminalCleanup(fn func()) uint64 {
	s.termMu.Lock()
	defer s.termMu.Unlock()
	s.termSeq++
	id := s.termSeq
	s.termCleanups[id] = fn
	return id
}

func (s *Server) unregisterTerminalCleanup(id uint64) {
	s.termMu.Lock()
	delete(s.termCleanups, id)
	s.termMu.Unlock()
}

func (s *Server) closeAllTerminals() {
	s.termMu.Lock()
	fns := make([]func(), 0, len(s.termCleanups))
	for id, fn := range s.termCleanups {
		fns = append(fns, fn)
		delete(s.termCleanups, id)
	}
	s.termMu.Unlock()

	for _, fn := range fns {
		if fn != nil {
			fn()
		}
	}
}

func (s *Server) buildMux() *http.ServeMux {
	mux := http.NewServeMux()

	// Auth & SPA shell
	mux.HandleFunc("GET /auth", s.redeemBootstrapToken)
	mux.HandleFunc("GET /preview/{path...}", s.handlePreview)
	mux.HandleFunc("GET /preview", s.handlePreview)
	mux.HandleFunc("GET /api/v1/preview/{path...}", s.handlePreview)
	mux.HandleFunc("GET /api/v1/preview", s.handlePreview)
	mux.HandleFunc("GET /", s.handleSPAOrBootstrap)

	// Auth API
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("GET /api/v1/auth/session", s.handleAuthSession)

	// Multiplexed SSE & Pure-Go PTY WebSocket
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)
	mux.HandleFunc("GET /api/v1/terminal/ws", s.handleTerminalWS)

	// Setup, Keys & Project
	mux.HandleFunc("GET /api/v1/setup/status", s.handleSetupStatus)
	mux.HandleFunc("POST /api/v1/keys/probe", s.handleKeysProbe)
	mux.HandleFunc("PUT /api/v1/keys", s.handleKeysPut)
	mux.HandleFunc("GET /api/v1/project", s.handleProjectGet)
	mux.HandleFunc("POST /api/v1/project/open", s.handleProjectOpen)
	mux.HandleFunc("GET /api/v1/project/browse", s.handleProjectBrowse)

	// Mounts, Files & Ignore
	mux.HandleFunc("GET /api/v1/mounts", s.handleMountsList)
	mux.HandleFunc("PATCH /api/v1/mounts/mode", s.handleMountsModePatch)
	mux.HandleFunc("GET /api/v1/files/content", s.handleFileContent)
	mux.HandleFunc("GET /api/v1/ignore", s.handleIgnoreGet)
	mux.HandleFunc("POST /api/v1/ignore", s.handleIgnorePost)
	mux.HandleFunc("POST /api/v1/ignore/test", s.handleIgnoreTest)
	mux.HandleFunc("POST /api/v1/ignore/convert-gitignore", s.handleIgnoreConvertGitignore)

	// Chat, Threads & Side-chat
	mux.HandleFunc("GET /api/v1/threads", s.handleThreadsList)
	mux.HandleFunc("POST /api/v1/threads", s.handleThreadsCreateOrSelect)
	mux.HandleFunc("GET /api/v1/threads/{id}/events", s.handleThreadEvents)
	mux.HandleFunc("POST /api/v1/chat/send", s.handleChatSend)
	mux.HandleFunc("POST /api/v1/chat/abort", s.handleChatAbort)
	mux.HandleFunc("POST /api/v1/chat/btw", s.handleChatBtw)
	mux.HandleFunc("POST /api/v1/chat/btw/promote", s.handleChatBtwPromote)
	mux.HandleFunc("POST /api/v1/chat/btw/pin", s.handleChatBtwPin)
	mux.HandleFunc("POST /api/v1/chat/approval", s.handleChatApproval)
	mux.HandleFunc("POST /api/v1/chat/compact", s.handleChatCompact)

	// Crews
	mux.HandleFunc("GET /api/v1/crews", s.handleCrewsList)
	mux.HandleFunc("GET /api/v1/crews/{id}/messages", s.handleCrewMessages)
	mux.HandleFunc("POST /api/v1/crews/{id}/abort", s.handleCrewAbort)
	mux.HandleFunc("POST /api/v1/crews/{id}/release-lock", s.handleCrewReleaseLock)

	// Diffs & Safe 1-Click Undo
	mux.HandleFunc("GET /api/v1/diffs", s.handleDiffsGet)
	mux.HandleFunc("POST /api/v1/diffs/undo-turn", s.handleDiffsUndoTurn)
	mux.HandleFunc("POST /api/v1/diffs/undo-file", s.handleDiffsUndoFile)
	mux.HandleFunc("POST /api/v1/diffs/undo-crew", s.handleDiffsUndoCrew)

	// Plan, Todos, Tokens & Grants
	mux.HandleFunc("GET /api/v1/plan", s.handlePlanGet)
	mux.HandleFunc("PUT /api/v1/plan", s.handlePlanPut)
	mux.HandleFunc("POST /api/v1/plan/start-building", s.handlePlanStartBuilding)
	mux.HandleFunc("GET /api/v1/todos", s.handleTodosList)
	mux.HandleFunc("GET /api/v1/tokens", s.handleTokensGet)
	mux.HandleFunc("GET /api/v1/grants", s.handleGrantsList)
	mux.HandleFunc("DELETE /api/v1/grants", s.handleGrantsDelete)

	// Customize (/mcp, /skills, /memory), Settings & Doctor
	mux.HandleFunc("GET /api/v1/mcp", s.handleMCPList)
	mux.HandleFunc("POST /api/v1/mcp", s.handleMCPCreate)
	mux.HandleFunc("PATCH /api/v1/mcp/{name}", s.handleMCPPatch)
	mux.HandleFunc("DELETE /api/v1/mcp/{name}", s.handleMCPDelete)
	mux.HandleFunc("POST /api/v1/mcp/test", s.handleMCPTest)
	mux.HandleFunc("POST /api/v1/mcp/import", s.handleMCPImport)
	mux.HandleFunc("POST /api/v1/mcp/{name}/toggle", s.handleMCPToggle)

	mux.HandleFunc("GET /api/v1/skills", s.handleSkillsList)
	mux.HandleFunc("PUT /api/v1/skills", s.handleSkillsPut)
	mux.HandleFunc("GET /api/v1/memory", s.handleMemoryGet)
	mux.HandleFunc("POST /api/v1/memory", s.handleMemoryPost)
	mux.HandleFunc("DELETE /api/v1/memory", s.handleMemoryDelete)
	mux.HandleFunc("GET /api/v1/settings", s.handleSettingsGet)
	mux.HandleFunc("PUT /api/v1/settings", s.handleSettingsPut)
	mux.HandleFunc("POST /api/v1/doctor", s.handleDoctorPost)

	return mux
}

func (s *Server) handleSPAOrBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" && r.URL.Query().Has("token") {
		s.redeemBootstrapToken(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusNotFound, "api route not found")
		return
	}

	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "embedded dist unavailable")
		return
	}
	cleanPath := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if cleanPath != "" {
		if info, err := fs.Stat(sub, cleanPath); err == nil && !info.IsDir() {
			if strings.HasSuffix(cleanPath, ".html") {
				w.Header().Set("Cache-Control", "no-store")
			}
			http.FileServer(http.FS(sub)).ServeHTTP(w, r)
			return
		}
	}

	indexBytes, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		writeError(w, http.StatusNotFound, "index.html not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexBytes)
}

func (s *Server) activeSnapshotStore() *snapshot.Store {
	eng, _ := s.activeState()
	if eng == nil {
		return nil
	}
	return snapshot.New(eng.Workspace)
}

func (s *Server) startChatTurn(prompt, threadID string) (string, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	select {
	case <-s.shutdownCh:
		return "", fmt.Errorf("server is shutting down")
	default:
	}

	s.turnMu.Lock()
	if s.activeTurnCancel != nil {
		s.turnMu.Unlock()
		return "", errTurnAlreadyActive
	}

	eng, run := s.activeState()
	if eng == nil || run == nil {
		s.turnMu.Unlock()
		return "", fmt.Errorf("workspace runner unavailable")
	}
	if strings.TrimSpace(threadID) != "" && strings.TrimSpace(threadID) != run.ThreadID() {
		if err := run.LoadThread(threadID); err != nil {
			s.turnMu.Unlock()
			return "", err
		}
	}

	turnCtx, cancel := context.WithCancel(context.Background())
	turnDone := make(chan struct{})
	turnID := fmt.Sprintf("web-%d", time.Now().UTC().UnixNano())
	s.activeTurnCancel = cancel
	s.activeTurnID = turnID
	s.activeTurnDone = turnDone
	activeThread := run.ThreadID()
	s.turnMu.Unlock()
	go func() {
		defer func() {
			cancel()
			s.turnMu.Lock()
			s.activeTurnCancel = nil
			s.activeTurnID = ""
			s.activeTurnDone = nil
			s.turnMu.Unlock()
			close(turnDone)
		}()

		onDelta := func(kind, text string) {
			switch kind {
			case "content", "reasoning", "endstep":
				s.hub.Publish("chat.delta", map[string]any{
					"thread_id": activeThread,
					"turn_id":   turnID,
					"kind":      kind,
					"text":      text,
				})
			}
		}

		reply, err := run.RunWithOutputStream(turnCtx, prompt, io.Discard, onDelta)
		if err != nil {
			s.hub.Publish("chat.error", map[string]any{
				"thread_id": activeThread,
				"turn_id":   turnID,
				"error":     err.Error(),
			})
		} else {
			s.hub.Publish("chat.done", map[string]any{
				"thread_id": activeThread,
				"turn_id":   turnID,
				"reply":     reply,
			})
		}
		s.hub.Publish("diff.updated", map[string]any{"thread_id": activeThread})
		s.hub.Publish("todo.updated", map[string]any{"thread_id": activeThread})
	}()

	return turnID, nil
}

var errTurnAlreadyActive = errors.New("chat turn already in progress")

// IsHeadlessEnvironment reports whether the current process is running inside SSH
// or a Linux environment without a graphical display server.
func IsHeadlessEnvironment() bool {
	for _, envName := range []string{"SSH_TTY", "SSH_CONNECTION", "SSH_CLIENT", "CI"} {
		if strings.TrimSpace(os.Getenv(envName)) != "" {
			return true
		}
	}
	if _, ok := os.LookupEnv("DISPLAY"); !ok {
		if _, wOk := os.LookupEnv("WAYLAND_DISPLAY"); !wOk {
			if os.PathSeparator == '/' && !isDarwinOS() {
				return true
			}
		}
	}
	return false
}

func isDarwinOS() bool {
	return strings.EqualFold(os.Getenv("OSTYPE"), "darwin") || fileExists("/System/Library/CoreServices/SystemVersion.plist")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func decodeJSONBody(r *http.Request, dst any) error {
	if r.Body == nil {
		return fmt.Errorf("empty request body")
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

func decodeOptionalJSONBody(r *http.Request, dst any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	err := decodeJSONBody(r, dst)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": msg,
	})
}
