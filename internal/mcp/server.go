package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/mount"
	"github.com/crossparties/tether/internal/safety"
	"github.com/crossparties/tether/internal/snapshot"
)

const maxEditReflections = 3

// Server implements MCP protocol over stdio
type Server struct {
	engine *core.Engine
	mount  *mount.Engine

	// Approval request identity. A run of blocked calls otherwise prints the
	// same opening words and boilerplate every time, so the reader cannot tell
	// which request they are looking at or whether they already answered it.
	mu           sync.Mutex
	approvalSeq  int
	approvalSeen map[string]bool
	editMisses   map[string]int

	// ask, when set, connects the approval gate to a human. The gate itself
	// cannot ask: it only knows how to refuse. The interactive REPL installs an
	// asker that reads the same input stream as the chat; hook and JSON-RPC
	// callers leave it nil and keep the refuse-with-instructions behaviour. ok
	// is false when the user cancelled, which is treated as a rejection.
	askMu         sync.Mutex
	ask           func(Decision) (Verdict, bool)
	askWithReason func(Decision) (Verdict, string, bool)

	// autoApprove, when set, permits matching non-critical mutations (e.g.
	// read-only shell commands, test/build validation, or undoable routine
	// workspace edits) to execute in a single step without gating or retry.
	autoApprove func(Decision) bool

	// approveAll is set when the user chose to approve everything for the rest
	// of the session. It is separate from ask because it must survive while the
	// asker stays installed: replacing the asker would discard the real one.
	approveAll     bool
	forceAskGrants map[string]bool
	extToolGrants  map[string]bool

	snapMu         sync.RWMutex
	snapshotStore  *snapshot.Store
	snapshotTurn   string
	snapshotCrew   string
	actorID        string
	readOnly       bool
	forceAskWrites bool
}

// Verdict is the user's answer to an approval request.
type Verdict int

const (
	// VerdictOnce authorises this one change, recorded as an exact-scope grant.
	VerdictOnce Verdict = iota
	// VerdictFolder authorises everything in the containing directory (or the
	// command prefix) for the rest of the session.
	VerdictFolder
	// VerdictSession authorises every further change without asking again.
	VerdictSession
	// VerdictReject discards the change.
	VerdictReject
)

// Decision is what the interactive asker is shown. It carries no behaviour so
// that rendering stays with the caller.
type Decision struct {
	Tool        string // tool requesting the change, e.g. "write_file"
	Command     string // shell command, empty for file changes
	Path        string // path the change targets, empty for commands
	Label       string // one-line subject used in the heading
	Preview     string // rendered diff (collapsed after maxDiffLines), or a description for commands
	FullPreview string // untruncated diff for "View full diff"
	ForceAsk    bool   // when true, callers must not auto-approve via non-critical/low-risk heuristics
}

// SetAskPolicy installs the interactive asker. Passing nil restores the
// non-interactive behaviour. Safe to call while tool calls are in flight.
func (s *Server) SetAskPolicy(fn func(Decision) (Verdict, bool)) {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	s.ask = fn
	s.askWithReason = nil
}

// SetAskPolicyWithReason installs an interactive asker that can also return an
// explicit user rejection reason when rejecting a proposed mutation.
func (s *Server) SetAskPolicyWithReason(fn func(Decision) (Verdict, string, bool)) {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	s.askWithReason = fn
	if fn == nil {
		s.ask = nil
		return
	}
	s.ask = func(d Decision) (Verdict, bool) {
		v, _, ok := fn(d)
		return v, ok
	}
}

// AskPolicy returns the installed asker, or nil.
func (s *Server) AskPolicy() func(Decision) (Verdict, bool) {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	return s.ask
}

// AskPolicyWithReason returns the installed reason-aware asker, or nil.
func (s *Server) AskPolicyWithReason() func(Decision) (Verdict, string, bool) {
	return s.askPolicyWithReason()
}

func (s *Server) askPolicyWithReason() func(Decision) (Verdict, string, bool) {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	if s.askWithReason != nil {
		return s.askWithReason
	}
	if s.ask != nil {
		fn := s.ask
		return func(d Decision) (Verdict, string, bool) {
			v, ok := fn(d)
			return v, "", ok
		}
	}
	return nil
}

// SetActorID configures the actor ("main" or Crew ID) enforced on lease-locked writes.
func (s *Server) SetActorID(actorID string) {
	s.snapMu.Lock()
	s.actorID = strings.TrimSpace(actorID)
	s.snapMu.Unlock()
	if s.mount != nil {
		s.mount.SetActorID(actorID)
	}
}

// SetReadOnly locks the MCP server into read-only review mode regardless of DB grants.
func (s *Server) SetReadOnly(ro bool) {
	s.snapMu.Lock()
	s.readOnly = ro
	s.snapMu.Unlock()
}

// IsReadOnly reports whether the server is locked into read-only review mode.
func (s *Server) IsReadOnly() bool {
	s.snapMu.RLock()
	ro := s.readOnly
	s.snapMu.RUnlock()
	return ro
}

// SetForceAskWrites ensures file writes and mutating shell commands bypass auto-approval heuristics.
func (s *Server) SetForceAskWrites(force bool) {
	s.snapMu.Lock()
	s.forceAskWrites = force
	s.snapMu.Unlock()
}

func (s *Server) forcesAskWrites() bool {
	s.snapMu.RLock()
	f := s.forceAskWrites
	s.snapMu.RUnlock()
	return f
}

// SetSnapshotContext binds the per-execution snapshot turn_id and optional crew_id
// so write_file and edit_file capture pre-write state and PostHash automatically.
func (s *Server) SetSnapshotContext(store *snapshot.Store, turnID, crewID string) {
	s.snapMu.Lock()
	s.snapshotStore = store
	s.snapshotTurn = strings.TrimSpace(turnID)
	s.snapshotCrew = strings.TrimSpace(crewID)
	s.snapMu.Unlock()
}

func (s *Server) captureSnapshotBefore(canonPath string) {
	s.snapMu.RLock()
	store := s.snapshotStore
	turn := s.snapshotTurn
	crew := s.snapshotCrew
	s.snapMu.RUnlock()
	if store == nil || turn == "" || canonPath == "" {
		return
	}
	_ = store.CaptureForCrew(turn, crew, canonPath)
}

func (s *Server) recordSnapshotAfter(canonPath string) {
	s.snapMu.RLock()
	store := s.snapshotStore
	turn := s.snapshotTurn
	s.snapMu.RUnlock()
	if store == nil || turn == "" || canonPath == "" {
		return
	}
	_ = store.RecordPostHash(turn, canonPath)
}

// SetAutoApprovePolicy installs a predicate that lets non-critical mutations
// execute immediately in one step without prompting or requiring a retry.
// Passing nil restores strict gate-every-mutation behaviour.
func (s *Server) SetAutoApprovePolicy(fn func(Decision) bool) {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	s.autoApprove = fn
}

// AutoApprovePolicy returns the installed auto-approval predicate, or nil.
func (s *Server) AutoApprovePolicy() func(Decision) bool {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	return s.autoApprove
}

func NewServer(engine *core.Engine) *Server {
	var m *mount.Engine
	if engine != nil {
		m = engine.Mount()
	}
	return &Server{engine: engine, mount: m}
}

// Mount returns the Server's attached Mount Engine.
func (s *Server) Mount() *mount.Engine {
	return s.mountEngine()
}

func (s *Server) mountEngine() *mount.Engine {
	var m *mount.Engine
	if s.mount != nil {
		m = s.mount
	} else if s.engine != nil {
		m = s.engine.Mount()
	} else {
		m = mount.NewEngine(".")
	}
	s.snapMu.RLock()
	actorID := s.actorID
	s.snapMu.RUnlock()
	if actorID != "" {
		m.SetActorID(actorID)
	}
	return m
}

func (s *Server) workspace() string {
	if s.engine != nil && s.engine.Workspace != "" {
		return s.engine.Workspace
	}
	return "."
}

func (s *Server) mode() string {
	if s.IsReadOnly() {
		return "review"
	}
	if s.engine != nil {
		return s.engine.GetMode()
	}
	return "code"
}

func (s *Server) shellAuthorizer() func(rawPath string, write bool) (bool, string) {
	m := s.mountEngine()
	mode := s.mode()
	return func(rawPath string, write bool) (bool, string) {
		op := mount.OpRead
		if write {
			op = mount.OpWrite
		}
		d := m.AuthorizePath(rawPath, op, mode)
		return d.Allowed, d.Reason
	}
}

// JSON-RPC types
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *RPCError   `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCP Protocol types
type InitializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    Capabilities `json:"capabilities"`
	ServerInfo      ServerInfo   `json:"serverInfo"`
}

type Capabilities struct {
	Tools *ToolsCapability `json:"tools,omitempty"`
}

type ToolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema InputSchema `json:"inputSchema"`
}

type InputSchema struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties,omitempty"`
	Required   []string            `json:"required,omitempty"`
}

type Property struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type ToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ToolResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Serve starts the MCP server listening on stdio
func (s *Server) Serve() error {
	return s.ServeIO(os.Stdin, os.Stdout)
}

// ServeIO starts the MCP server listening on the provided reader and writer
func (s *Server) ServeIO(r io.Reader, w io.Writer) error {
	reader := bufio.NewReader(r)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return nil // clean shutdown
			}
			return err
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var body []byte
		if strings.HasPrefix(line, "Content-Length:") {
			var contentLength int
			fmt.Sscanf(line, "Content-Length: %d", &contentLength)
			// Read until empty line
			for {
				headerLine, err := reader.ReadString('\n')
				if err != nil {
					return err
				}
				if strings.TrimSpace(headerLine) == "" {
					break
				}
			}
			body = make([]byte, contentLength)
			if _, err := io.ReadFull(reader, body); err != nil {
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					return nil
				}
				return err
			}
		} else {
			body = []byte(line)
		}

		var req Request
		if err := json.Unmarshal(body, &req); err != nil {
			continue
		}

		resp := s.handleRequest(&req)
		if resp != nil {
			out, err := json.Marshal(resp)
			if err != nil {
				continue
			}
			out = append(out, '\n')
			if _, err := w.Write(out); err != nil {
				return err
			}
		}
	}
}

func (s *Server) handleRequest(req *Request) *Response {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "tools/list":
		return s.handleToolsList(req)
	case "tools/call":
		return s.handleToolsCall(req)
	case "notifications/initialized", "initialized":
		return nil // notifications don't need response
	default:
		if req.ID != nil {
			return &Response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &RPCError{
					Code:    -32601,
					Message: fmt.Sprintf("method not found: %s", req.Method),
				},
			}
		}
		return nil
	}
}

func (s *Server) handleInitialize(req *Request) *Response {
	return &Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: InitializeResult{
			ProtocolVersion: "2024-11-05",
			Capabilities: Capabilities{
				Tools: &ToolsCapability{ListChanged: false},
			},
			ServerInfo: ServerInfo{
				Name:    "tether",
				Version: "0.2.0",
			},
		},
	}
}

func (s *Server) handleToolsList(req *Request) *Response {
	tools := []Tool{
		{
			Name:        "read_file",
			Description: "Read workspace file contents with 1-based line numbers (optionally specify start_line and end_line).",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"path":       {Type: "string", Description: "Relative or workspace-contained path of the file to read"},
					"start_line": {Type: "integer", Description: "Optional 1-based start line"},
					"end_line":   {Type: "integer", Description: "Optional 1-based end line"},
				},
				Required: []string{"path"},
			},
		},
		{
			Name:        "write_file",
			Description: "Create or overwrite a file inside the workspace (enforces .tether/ignore, secret protection, and approval policy).",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"path":    {Type: "string", Description: "Relative or workspace-contained path of the file to write"},
					"content": {Type: "string", Description: "Full content to write to the file"},
				},
				Required: []string{"path", "content"},
			},
		},
		{
			Name:        "edit_file",
			Description: "Replace an exact target block in an existing workspace file with replacement text.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"path":        {Type: "string", Description: "Relative or workspace-contained path of the file to edit"},
					"target":      {Type: "string", Description: "Exact string to find and replace"},
					"replacement": {Type: "string", Description: "Replacement string"},
				},
				Required: []string{"path", "target", "replacement"},
			},
		},
		{
			Name:        "glob",
			Description: "Find workspace files matching a glob pattern (e.g. '**/*.go', 'internal/*/*.go'), obeying .tether/ignore and secret boundaries.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"pattern": {Type: "string", Description: "Glob pattern supporting *, **, and ?"},
					"path":    {Type: "string", Description: "Optional subdirectory relative to workspace root"},
				},
				Required: []string{"pattern"},
			},
		},
		{
			Name:        "grep",
			Description: "Search workspace file contents with Go regex or literal matching, returning path:line: content and obeying .tether/ignore.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"pattern": {Type: "string", Description: "Go regular expression or literal text to search"},
					"path":    {Type: "string", Description: "Optional file or subdirectory relative to workspace root"},
					"glob":    {Type: "string", Description: "Optional filename/path filter (e.g. '*.go')"},
					"literal": {Type: "boolean", Description: "Treat pattern as literal text instead of regex (default false)"},
				},
				Required: []string{"pattern"},
			},
		},
		{
			Name:        "run_command",
			Description: "Execute a shell command in the workspace subject to shell safety guardrails, mode boundaries, and approval policy.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"command": {Type: "string", Description: "Shell command to execute"},
				},
				Required: []string{"command"},
			},
		},
		{
			Name:        "kb_search",
			Description: "Search the repository documentation knowledge base (.tether/cache.db FTS5 with BM25 ranking).",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"query": {Type: "string", Description: "Full-text search query"},
					"limit": {Type: "integer", Description: "Max results (default 10)"},
				},
				Required: []string{"query"},
			},
		},
		{
			Name:        "todo_manage",
			Description: "Manage session todos (add, list, complete, remove).",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"action":            {Type: "string", Description: "Action: add | list | complete | remove"},
					"description":       {Type: "string", Description: "Task description (for action=add)"},
					"details":           {Type: "string", Description: "Optional details (for action=add)"},
					"id":                {Type: "integer", Description: "Todo ID (for action=complete|remove)"},
					"include_completed": {Type: "boolean", Description: "Include completed todos (for action=list)"},
				},
				Required: []string{"action"},
			},
		},
		{
			Name:        "mode_set",
			Description: "Set active governance mode (plan=read-only except plan artifacts, code=full workspace access).",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"mode": {Type: "string", Description: "Mode: plan | code"},
				},
				Required: []string{"mode"},
			},
		},
		{
			Name:        "decision_add",
			Description: "Record an architecture or design decision into .tether/session.db.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"topic":    {Type: "string", Description: "Decision topic"},
					"decision": {Type: "string", Description: "What was decided"},
					"reason":   {Type: "string", Description: "Why"},
				},
				Required: []string{"topic", "decision", "reason"},
			},
		},
	}

	return &Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  map[string]interface{}{"tools": tools},
	}
}

// ListTools returns the focused core workspace and substrate tools.
func (s *Server) ListTools() []Tool {
	resp := s.handleToolsList(&Request{JSONRPC: "2.0", ID: 1})
	if resp == nil {
		return nil
	}
	resMap, ok := resp.Result.(map[string]interface{})
	if !ok {
		return nil
	}
	tools, _ := resMap["tools"].([]Tool)
	return tools
}

// CallTool executes a named tool with JSON arguments and returns (resultText, isError)
func (s *Server) CallTool(name string, args json.RawMessage) (string, bool) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if s.IsReadOnly() && isMutatingMCPTool(name, args) {
		return fmt.Sprintf("Tether Guard: read-only crew cannot invoke %s.", name), true
	}

	// Hard security boundaries & approval gate share one unified validation/preview pass
	// so neither session-wide approvals nor grants can bypass mount/secret/shell rules.
	if mut, secErr, paramErr := s.prepareMutation(name, args); secErr != "" {
		return secErr, true
	} else if paramErr != "" {
		return paramErr, true
	} else if mut != nil {
		if msg := s.authorizeMutation(mut); msg != "" {
			return msg, true
		}
		switch name {
		case "write_file":
			return s.execWriteFile(mut)
		case "edit_file":
			return s.execEditFile(mut)
		case "run_command":
			return s.execRunCommand(mut)
		}
	}

	switch name {
	case "read_file":
		return s.toolReadFile(args)
	case "glob":
		return s.toolGlob(args)
	case "grep":
		return s.toolGrep(args)
	case "kb_search":
		return s.toolKBSearch(args)
	case "todo_manage":
		return s.toolTodoManage(args)
	case "mode_set":
		return s.toolModeSet(args)
	case "decision_add":
		return s.toolDecisionAdd(args)
	// Internal stdio approval protocol tools (used by `tether mcp` external clients & tests).
	case "approve_change":
		return s.toolApproveChange("approve", args), false
	case "reject_change":
		return s.toolApproveChange("reject", args), false
	case "list_grants":
		return s.toolListGrants(), false
	case "revoke_grants":
		return s.toolRevokeGrants(), false
	default:
		return fmt.Sprintf("unknown tool: %s", name), true
	}
}

func isMutatingMCPTool(name string, args json.RawMessage) bool {
	switch name {
	case "write_file", "edit_file", "mode_set", "decision_add":
		return true
	case "todo_manage":
		var payload struct {
			Action string `json:"action"`
		}
		if len(args) > 0 && json.Unmarshal(args, &payload) == nil &&
			strings.EqualFold(strings.TrimSpace(payload.Action), "list") {
			return false
		}
		return true
	case "run_command":
		var payload struct {
			Command string `json:"command"`
		}
		if len(args) > 0 && json.Unmarshal(args, &payload) == nil {
			if ok, _ := safety.IsReadOnlyCommand(payload.Command); ok {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (s *Server) handleToolsCall(req *Request) *Response {
	var params ToolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  ToolResult{Content: []ContentBlock{{Type: "text", Text: "invalid params"}}, IsError: true},
		}
	}

	result, isError := s.CallTool(params.Name, params.Arguments)

	return &Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  ToolResult{Content: []ContentBlock{{Type: "text", Text: result}}, IsError: isError},
	}
}

// --- Tool Implementations ---

// toolApproveChange records the user's decision for a blocked mutation.
//
// scope="once" grants a single immediate retry; scope="all" stores a standing
// grant covering every file under the given path until it expires. Nothing is
// written by this call: it only records permission, so the agent must repeat the
// original tool call and the gate will let it through.
func (s *Server) toolApproveChange(action string, args json.RawMessage) string {
	var params struct {
		Path  string `json:"path"`
		Scope string `json:"scope"`
		Kind  string `json:"kind"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &params)
	}
	path := strings.TrimSpace(params.Path)
	if path == "" {
		return "invalid params: path required (the path or command shown in the approval request)"
	}

	if action == "reject" {
		return fmt.Sprintf("Change to %q rejected. Nothing was written. Ask the user what they would prefer instead.", path)
	}

	// Command grants are a separate namespace. "all" against a shell command
	// means "every command starting with this prefix", which is the only
	// reading that makes "stop asking about go test" possible.
	if params.Kind == core.GrantKindCommand || params.Kind == core.GrantKindCommandPrefix {
		prefix := params.Kind == core.GrantKindCommandPrefix
		if err := s.engine.AddCommandGrant(path, prefix); err != nil {
			return fmt.Sprintf("error recording approval: %v", err)
		}
		if prefix {
			return fmt.Sprintf("Approved: every command starting with %q is allowed until %s.\nRepeat the original tool call to run it.",
				path, core.GrantExpiryHint())
		}
		return fmt.Sprintf("Approved: %q may run once (grant expires %s).\nRepeat the original tool call to run it.",
			path, core.GrantExpiryHint())
	}

	switch params.Scope {
	case "all":
		if err := s.engine.AddApprovalGrant(path, "path"); err != nil {
			return fmt.Sprintf("error recording approval: %v", err)
		}
		return fmt.Sprintf("Approved: all changes under %q are allowed until %s (session-scoped).\nRepeat the original tool call to apply this change.",
			path, core.GrantExpiryHint())
	case "", "once":
		// A one-shot approval is stored as a grant scoped to the exact path and
		// revoked concept-free: the gate re-checks it, so re-running the same
		// call proceeds. Path-wide wording is avoided so the user is not misled
		// into thinking siblings are covered.
		if err := s.engine.AddApprovalGrant(path, "path"); err != nil {
			return fmt.Sprintf("error recording approval: %v", err)
		}
		return fmt.Sprintf("Approved: %q for this path until %s.\nRepeat the original tool call to apply the change.",
			path, core.GrantExpiryHint())
	default:
		return fmt.Sprintf("invalid scope %q (once|all)", params.Scope)
	}
}

// toolListGrants shows what the user has already authorized this session.
func (s *Server) toolListGrants() string {
	grants, err := s.engine.ListApprovalGrants()
	if err != nil {
		return fmt.Sprintf("error listing grants: %v", err)
	}
	if len(grants) == 0 {
		return "No approval grants: every change will ask first."
	}
	var b strings.Builder
	b.WriteString("Active approval grants:\n")
	for _, g := range grants {
		// Name the namespace so a command grant can never be mistaken for a
		// file grant when reading this list.
		label := "file"
		switch g.Kind {
		case core.GrantKindCommand:
			label = "command"
		case core.GrantKindCommandPrefix:
			label = "command-prefix"
		}
		fmt.Fprintf(&b, "  [%-14s] %-40s until %s\n", label, g.PathScope, g.ExpiresAt)
	}
	b.WriteString("\nUse revoke_grants to clear them.")
	return b.String()
}

// toolRevokeGrants clears every grant so the safe default returns.
func (s *Server) toolRevokeGrants() string {
	n, err := s.engine.RevokeApprovalGrants()
	if err != nil {
		return fmt.Sprintf("error revoking grants: %v", err)
	}
	return fmt.Sprintf("Revoked %d approval grant(s). Every change will ask again.", n)
}

func (s *Server) toolDecisionAdd(args json.RawMessage) (string, bool) {
	var params struct {
		Topic    string `json:"topic"`
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "invalid params", true
	}
	if strings.TrimSpace(params.Topic) == "" || strings.TrimSpace(params.Decision) == "" {
		return "invalid params: topic and decision required", true
	}
	if err := s.engine.AddDecision(params.Topic, params.Decision, params.Reason); err != nil {
		return err.Error(), true
	}
	return "decision recorded", false
}

func (s *Server) toolModeSet(args json.RawMessage) (string, bool) {
	var params struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "invalid params", true
	}
	if params.Mode != "plan" && params.Mode != "code" {
		return "mode must be 'plan' or 'code'", true
	}
	if err := s.engine.SetMode(params.Mode); err != nil {
		return err.Error(), true
	}
	return fmt.Sprintf("mode set to: %s", params.Mode), false
}

// --- Knowledge Base Search ---

func (s *Server) toolKBSearch(args json.RawMessage) (string, bool) {
	var params struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &params); err != nil || strings.TrimSpace(params.Query) == "" {
		return "invalid params: query required", true
	}

	results, err := s.engine.SearchKB(params.Query, params.Limit)
	if err != nil {
		return fmt.Sprintf("search error: %v", err), true
	}

	if len(results) == 0 {
		return "no results found", false
	}

	out, _ := json.MarshalIndent(results, "", "  ")
	return string(out), false
}

// --- Consolidated Todo Management ---

func (s *Server) toolTodoManage(args json.RawMessage) (string, bool) {
	var params struct {
		Action           string          `json:"action"`
		Description      string          `json:"description"`
		Details          string          `json:"details"`
		ID               int             `json:"id"`
		IncludeCompleted json.RawMessage `json:"include_completed"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "invalid params", true
	}
	switch strings.ToLower(strings.TrimSpace(params.Action)) {
	case "add":
		if strings.TrimSpace(params.Description) == "" {
			return "invalid params: description required for action=add", true
		}
		id, err := s.engine.AddTodo(params.Description, params.Details)
		if err != nil {
			return err.Error(), true
		}
		return fmt.Sprintf("todo #%d added: %s", id, params.Description), false
	case "list":
		includeCompleted := false
		if len(params.IncludeCompleted) > 0 {
			var b bool
			if err := json.Unmarshal(params.IncludeCompleted, &b); err == nil {
				includeCompleted = b
			} else {
				var str string
				if err := json.Unmarshal(params.IncludeCompleted, &str); err == nil {
					includeCompleted = strings.EqualFold(strings.TrimSpace(str), "true")
				}
			}
		}
		todos, err := s.engine.ListTodos(includeCompleted)
		if err != nil {
			return err.Error(), true
		}
		if len(todos) == 0 {
			return "no todos", false
		}
		out, _ := json.MarshalIndent(todos, "", "  ")
		return string(out), false
	case "complete":
		if params.ID <= 0 {
			return "invalid params: positive id required for action=complete", true
		}
		if err := s.engine.CompleteTodo(params.ID); err != nil {
			return err.Error(), true
		}
		return fmt.Sprintf("todo #%d completed", params.ID), false
	case "remove":
		if params.ID <= 0 {
			return "invalid params: positive id required for action=remove", true
		}
		if err := s.engine.RemoveTodo(params.ID); err != nil {
			return err.Error(), true
		}
		return fmt.Sprintf("todo #%d removed", params.ID), false
	default:
		return fmt.Sprintf("invalid action %q (expected add | list | complete | remove)", params.Action), true
	}
}

// --- Workspace Primitives (for Micro-Harness & MCP) ---

const (
	maxReadFileBytes = 1024 * 1024
	maxReadOutputLen = 32768
)

var errStopWalk = errors.New("stop walk")

func (s *Server) toolReadFile(args json.RawMessage) (string, bool) {
	var params struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal(args, &params); err != nil || strings.TrimSpace(params.Path) == "" {
		return "invalid params: path required", true
	}

	d := s.mountEngine().AuthorizePath(params.Path, mount.OpRead, s.mode())
	if !d.Allowed {
		return d.Reason, true
	}
	if d.IsDir {
		return fmt.Sprintf("path %q is a directory, not a regular file", d.RelPath), true
	}

	info, err := os.Stat(d.CanonicalPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("file not found: %s", d.RelPath), true
		}
		return err.Error(), true
	}
	if !info.Mode().IsRegular() {
		return fmt.Sprintf("path %q is not a regular file", d.RelPath), true
	}
	if info.Size() > maxReadFileBytes {
		return fmt.Sprintf("file %s is too large (%d bytes; max %d bytes) — use start_line/end_line or grep", d.RelPath, info.Size(), maxReadFileBytes), true
	}

	data, err := os.ReadFile(d.CanonicalPath)
	if err != nil {
		return err.Error(), true
	}
	sampleLen := len(data)
	if sampleLen > 8192 {
		sampleLen = 8192
	}
	if bytes.IndexByte(data[:sampleLen], 0) >= 0 {
		return fmt.Sprintf("cannot read binary file: %s", d.RelPath), true
	}

	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	if content == "" {
		s.resetEditMisses(d.CanonicalPath)
		return "", false
	}

	trimmed := strings.TrimSuffix(content, "\n")
	lines := strings.Split(trimmed, "\n")

	start := params.StartLine
	if start < 1 {
		start = 1
	}
	end := params.EndLine
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if start > len(lines) || start > end {
		return fmt.Sprintf("invalid line range %d-%d (file has %d lines)", start, end, len(lines)), true
	}

	var b strings.Builder
	for i := start - 1; i < end; i++ {
		line := fmt.Sprintf("%d: %s\n", i+1, lines[i])
		if b.Len()+len(line) > maxReadOutputLen {
			if b.Len() == 0 {
				limit := maxReadOutputLen - len("\n... [line truncated at 32KB]\n")
				prefix := truncateUTF8(line, limit)
				b.WriteString(prefix)
				b.WriteString("\n... [line truncated at 32KB]\n")
			} else {
				b.WriteString("... [truncated at 32KB]\n")
			}
			break
		}
		b.WriteString(line)
	}
	s.resetEditMisses(d.CanonicalPath)
	return b.String(), false
}

func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) == 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func (s *Server) resetEditMisses(canonPath string) {
	if canonPath == "" {
		return
	}
	s.mu.Lock()
	delete(s.editMisses, canonPath)
	s.mu.Unlock()
}

func (s *Server) recordEditMiss(canonPath string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.editMisses == nil {
		s.editMisses = make(map[string]int)
	}
	s.editMisses[canonPath]++
	return s.editMisses[canonPath]
}

func (s *Server) execWriteFile(m *mutation) (string, bool) {
	if err := os.MkdirAll(filepath.Dir(m.canonPath), 0755); err != nil {
		return err.Error(), true
	}
	s.captureSnapshotBefore(m.canonPath)
	if err := os.WriteFile(m.canonPath, []byte(m.content), 0644); err != nil {
		return err.Error(), true
	}
	s.recordSnapshotAfter(m.canonPath)
	s.resetEditMisses(m.canonPath)
	if s.engine != nil {
		_ = s.engine.IndexFile(m.canonPath)
	}
	return fmt.Sprintf("wrote %d bytes to %s%s", len(m.content), m.relPath, s.autoVerify(m.canonPath)), false
}

func (s *Server) execEditFile(m *mutation) (string, bool) {
	data, err := os.ReadFile(m.canonPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("file not found: %s", m.relPath), true
		}
		return err.Error(), true
	}
	updated, tier, suggestStart, suggestEnd, err := applyFuzzyEditWithHint(string(data), m.target, m.replace)
	if err != nil {
		misses := s.recordEditMiss(m.canonPath)
		if misses >= maxEditReflections {
			startLine, endLine := suggestStart, suggestEnd
			if startLine <= 0 {
				startLine = 1
				endLine = 40
			} else {
				if startLine > 5 {
					startLine -= 5
				} else {
					startLine = 1
				}
				endLine += 5
			}
			return fmt.Sprintf("max_reflections=%d reached for %s. Stop retrying blind edits and call read_file(path=%q, start_line=%d, end_line=%d) before retrying.\n%v",
				maxEditReflections, m.relPath, m.relPath, startLine, endLine, err), true
		}
		return err.Error(), true
	}
	s.captureSnapshotBefore(m.canonPath)
	if err := os.WriteFile(m.canonPath, []byte(updated), 0644); err != nil {
		return err.Error(), true
	}
	s.recordSnapshotAfter(m.canonPath)
	s.resetEditMisses(m.canonPath)
	if s.engine != nil {
		_ = s.engine.IndexFile(m.canonPath)
	}
	return fmt.Sprintf("edited %s (match:%s)%s", m.relPath, tier, s.autoVerify(m.canonPath)), false
}

func (s *Server) walkWorkspaceFiles(mountEng *mount.Engine, searchRoot, mode string, visit func(dec mount.Decision) bool) error {
	ws := s.workspace()
	return filepath.WalkDir(searchRoot, func(path string, d fs.DirEntry, werr error) error {
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
			if path == searchRoot {
				return nil
			}
			if d.Name() == ".git" || d.Name() == ".tether" {
				return filepath.SkipDir
			}
			if relDir, _, _, err := mount.ResolveWithinWorkspace(path, ws); err == nil && relDir != "." {
				if mountEng.ShouldSkipDir(relDir) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		dec := mountEng.AuthorizePath(path, mount.OpRead, mode)
		if !dec.Allowed || dec.IsDir {
			return nil
		}
		if !visit(dec) {
			return errStopWalk
		}
		return nil
	})
}

func (s *Server) toolGlob(args json.RawMessage) (string, bool) {
	var params struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(args, &params); err != nil || strings.TrimSpace(params.Pattern) == "" {
		return "invalid params: pattern required", true
	}
	rawPattern := strings.TrimSpace(filepath.ToSlash(params.Pattern))
	if strings.Contains(rawPattern, "..") || strings.HasPrefix(rawPattern, "/") {
		return fmt.Sprintf("access denied: glob pattern %q escapes workspace root", params.Pattern), true
	}
	re, err := mount.CompileWorkspaceGlob(rawPattern)
	if err != nil {
		return fmt.Sprintf("invalid glob pattern: %v", err), true
	}

	mountEng := s.mountEngine()
	searchRoot, baseRel, reason := s.resolveSearchRoot(params.Path, mountEng)
	if reason != "" {
		return reason, true
	}

	const maxMatches = 200
	var matches []string
	total := 0

	walkErr := s.walkWorkspaceFiles(mountEng, searchRoot, s.mode(), func(dec mount.Decision) bool {
		targetForMatch := dec.RelPath
		if baseRel != "" {
			targetForMatch = strings.TrimPrefix(dec.RelPath, baseRel+"/")
		}
		if re.MatchString(targetForMatch) || re.MatchString(dec.RelPath) {
			total++
			if len(matches) < maxMatches {
				matches = append(matches, dec.RelPath)
			}
		}
		return true
	})
	if walkErr != nil && !errors.Is(walkErr, errStopWalk) {
		return walkErr.Error(), true
	}
	if len(matches) == 0 {
		return "no files matched", false
	}
	sort.Strings(matches)
	out := strings.Join(matches, "\n")
	if total > maxMatches {
		out += fmt.Sprintf("\n... (%d more matches omitted)", total-maxMatches)
	}
	return out, false
}

func (s *Server) toolGrep(args json.RawMessage) (string, bool) {
	var params struct {
		Pattern string `json:"pattern"`
		Query   string `json:"query"`
		Path    string `json:"path"`
		Glob    string `json:"glob"`
		Literal bool   `json:"literal"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "invalid params", true
	}
	pat := params.Pattern
	if pat == "" {
		pat = params.Query
	}
	if pat == "" {
		return "invalid params: pattern required", true
	}

	var matcher *regexp.Regexp
	if !params.Literal {
		var rerr error
		matcher, rerr = regexp.Compile(pat)
		if rerr != nil {
			return fmt.Sprintf("invalid regex: %v", rerr), true
		}
	}

	var fileFilter *regexp.Regexp
	if g := strings.TrimSpace(filepath.ToSlash(params.Glob)); g != "" {
		if !strings.Contains(g, "/") {
			g = "**/" + g
		}
		var gerr error
		fileFilter, gerr = mount.CompileWorkspaceGlob(g)
		if gerr != nil {
			return fmt.Sprintf("invalid glob filter: %v", gerr), true
		}
	}

	mountEng := s.mountEngine()
	mode := s.mode()
	searchRoot, _, reason := s.resolveSearchRoot(params.Path, mountEng)
	if reason != "" {
		return reason, true
	}

	const maxLines = 100
	var outLines []string
	truncated := false

	scanFile := func(canonPath, relPath string) {
		if truncated {
			return
		}
		if fileFilter != nil && !fileFilter.MatchString(relPath) {
			return
		}
		info, err := os.Stat(canonPath)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 2*1024*1024 {
			return
		}
		f, err := os.Open(canonPath)
		if err != nil {
			return
		}
		defer f.Close()

		var sample [512]byte
		n, _ := f.Read(sample[:])
		if bytes.IndexByte(sample[:n], 0) >= 0 {
			return // skip binary files
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return
		}

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			text := scanner.Text()
			matched := false
			if params.Literal {
				matched = strings.Contains(text, pat)
			} else {
				matched = matcher.MatchString(text)
			}
			if matched {
				outLines = append(outLines, fmt.Sprintf("%s:%d: %s", relPath, lineNo, text))
				if len(outLines) >= maxLines {
					truncated = true
					return
				}
			}
		}
		_ = scanner.Err()
	}

	walkErr := s.walkWorkspaceFiles(mountEng, searchRoot, mode, func(dec mount.Decision) bool {
		scanFile(dec.CanonicalPath, dec.RelPath)
		return !truncated
	})
	if walkErr != nil && !errors.Is(walkErr, errStopWalk) {
		return walkErr.Error(), true
	}

	if len(outLines) == 0 {
		return "no matches found", false
	}
	res := strings.Join(outLines, "\n")
	if truncated {
		res += fmt.Sprintf("\n... (truncated at %d matches)", maxLines)
	}
	return res, false
}

func (s *Server) resolveSearchRoot(rawPath string, mountEng *mount.Engine) (canonRoot, relPrefix, errReason string) {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" || trimmed == "." {
		abs, err := filepath.Abs(s.workspace())
		if err != nil {
			return "", "", err.Error()
		}
		realRoot, err := mount.EvalSymlinksWithAncestor(abs)
		if err != nil {
			return "", "", err.Error()
		}
		return realRoot, "", ""
	}
	d := mountEng.AuthorizePath(trimmed, mount.OpRead, s.mode())
	if !d.Allowed {
		return "", "", d.Reason
	}
	return d.CanonicalPath, d.RelPath, ""
}

func (s *Server) autoVerify(fullPath string) string {
	ws := s.workspace()
	ext := strings.ToLower(filepath.Ext(fullPath))

	var settings map[string]string
	if s.engine != nil {
		settings, _ = s.engine.GetAllProjectSettings()
	}

	switch ext {
	case ".go":
		if _, err := os.Stat(filepath.Join(ws, "go.mod")); err != nil {
			return ""
		}
		if _, err := exec.LookPath("go"); err != nil {
			return ""
		}
		return runAutoVerifyCmd(ws, "go vet", "go", "vet", "./...")

	case ".ts", ".tsx", ".svelte", ".js", ".jsx":
		return s.autoVerifyFrontend(ws, fullPath, settings)

	case ".py":
		if _, err := exec.LookPath("python3"); err != nil {
			return ""
		}
		tmpOut := filepath.Join(os.TempDir(), fmt.Sprintf("tether-pyc-%d.pyc", time.Now().UnixNano()))
		defer os.Remove(tmpOut)
		return runAutoVerifyCmd(ws, "py_compile", "python3", "-B", "-c",
			"import py_compile, sys; py_compile.compile(sys.argv[1], cfile=sys.argv[2], doraise=True)",
			fullPath, tmpOut)

	default:
		return ""
	}
}

func runAutoVerifyCmd(dir, label, bin string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Sprintf("\n⚠ %s failed:\n%s", label, detail)
	}
	return fmt.Sprintf(" | ✓ %s passed", label)
}

func (s *Server) autoVerifyFrontend(ws, fullPath string, settings map[string]string) string {
	checkCmd := strings.TrimSpace(settings["check_cmd"])
	frontendDir := strings.TrimSpace(settings["frontend_dir"])

	// Walk && segments, tracking safe relative `cd <dir>` prefixes.
	if checkCmd != "" {
		curDir := ws
		for _, rawSeg := range strings.Split(checkCmd, "&&") {
			seg := strings.TrimSpace(rawSeg)
			if seg == "" {
				continue
			}
			fields := strings.Fields(seg)
			if len(fields) == 2 && fields[0] == "cd" {
				_, canon, _, err := mount.ResolveWithinWorkspace(fields[1], ws)
				if err != nil {
					return ""
				}
				curDir = canon
				continue
			}
			if res, handled := tryRunFrontendCheck(curDir, fields); handled {
				return res
			}
		}
	}

	// Fallback if check_cmd wasn't populated or didn't match a frontend dir directly.
	candidateDir := ws
	if frontendDir != "" {
		if _, canon, _, err := mount.ResolveWithinWorkspace(frontendDir, ws); err == nil {
			candidateDir = canon
		}
	} else if dir := filepath.Dir(fullPath); dir != "" {
		if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
			candidateDir = dir
		}
	}
	if pkg, err := os.ReadFile(filepath.Join(candidateDir, "package.json")); err == nil && strings.Contains(string(pkg), `"check"`) {
		if res, handled := tryRunFrontendCheck(candidateDir, []string{"npm", "run", "check"}); handled {
			return res
		}
	}
	if _, err := os.Stat(filepath.Join(candidateDir, "tsconfig.json")); err == nil {
		if res, handled := tryRunFrontendCheck(candidateDir, []string{"tsc", "--noEmit"}); handled {
			return res
		}
	}
	return ""
}

func tryRunFrontendCheck(dir string, fields []string) (string, bool) {
	if len(fields) == 0 {
		return "", false
	}
	cmdStr := strings.Join(fields, " ")
	hasNodeModules := func() bool {
		fi, err := os.Stat(filepath.Join(dir, "node_modules"))
		return err == nil && fi.IsDir()
	}
	hasAnyTool := func(names ...string) bool {
		for _, n := range names {
			if _, err := os.Stat(filepath.Join(dir, "node_modules", ".bin", n)); err == nil {
				return true
			}
			if _, err := exec.LookPath(n); err == nil {
				return true
			}
		}
		return false
	}

	switch cmdStr {
	case "npm run check":
		if _, err := exec.LookPath("npm"); err != nil {
			return "", false
		}
		pkg, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if err != nil || !strings.Contains(string(pkg), `"check"`) {
			return "", false
		}
		if !hasNodeModules() && !hasAnyTool("svelte-check", "tsc") {
			return "", false
		}
		return runAutoVerifyCmd(dir, "npm run check", "npm", "run", "check"), true

	case "pnpm check", "pnpm run check":
		if _, err := exec.LookPath("pnpm"); err != nil || (!hasNodeModules() && !hasAnyTool("svelte-check", "tsc")) {
			return "", false
		}
		return runAutoVerifyCmd(dir, cmdStr, fields[0], fields[1:]...), true

	case "bun run check":
		if _, err := exec.LookPath("bun"); err != nil || (!hasNodeModules() && !hasAnyTool("svelte-check", "tsc")) {
			return "", false
		}
		return runAutoVerifyCmd(dir, "bun run check", "bun", "run", "check"), true

	case "npx --no-install tsc --noEmit", "tsc --noEmit":
		localTsc := filepath.Join(dir, "node_modules", ".bin", "tsc")
		if _, err := os.Stat(localTsc); err == nil {
			return runAutoVerifyCmd(dir, "tsc --noEmit", localTsc, "--noEmit"), true
		}
		if _, err := exec.LookPath("tsc"); err == nil {
			return runAutoVerifyCmd(dir, "tsc --noEmit", "tsc", "--noEmit"), true
		}
		return "", false
	}
	return "", false
}

// applyFuzzyEditWithHint implements a 3-Tier surgical matcher and returns a
// 1-based line-range suggestion ("Did you mean (around lines X-Y)?") when unmatched:
// Tier 1: Unique exact substring match (with ambiguity guard)
// Tier 2: Unique CRLF-normalized match (with ambiguity guard)
// Tier 3: Indentation & blank-line tolerant block match with proportional nested re-indentation
func applyFuzzyEditWithHint(content, target, replacement string) (string, string, int, int, error) {
	// Tier 1: Exact match with ambiguity guard
	if cnt := strings.Count(content, target); cnt > 0 {
		if cnt > 1 {
			return "", "", 0, 0, fmt.Errorf("ambiguous target: matched %d locations; include more surrounding lines", cnt)
		}
		return strings.Replace(content, target, replacement, 1), "exact", 0, 0, nil
	}

	normContent := strings.ReplaceAll(content, "\r\n", "\n")
	normTarget := strings.ReplaceAll(target, "\r\n", "\n")
	normReplace := strings.ReplaceAll(replacement, "\r\n", "\n")

	// Tier 2: CRLF-normalized match with ambiguity guard
	if cnt := strings.Count(normContent, normTarget); cnt > 0 {
		if cnt > 1 {
			return "", "", 0, 0, fmt.Errorf("ambiguous target: matched %d locations; include more surrounding lines", cnt)
		}
		return strings.Replace(normContent, normTarget, normReplace, 1), "crlf-normalized", 0, 0, nil
	}

	// Tier 3: Indentation & blank-line tolerant block match
	contentLines := strings.Split(normContent, "\n")
	targetLines := strings.Split(strings.TrimSuffix(normTarget, "\n"), "\n")

	startIdx, endIdx, matchCount := findFuzzySpan(contentLines, targetLines)
	if matchCount == 0 {
		sLine, eLine, didYouMean := buildDidYouMeanHint(contentLines, targetLines)
		if didYouMean != "" {
			return "", "", sLine, eLine, fmt.Errorf("target string not found in file (even with fuzzy indentation/blank-line matching)\n%s", didYouMean)
		}
		return "", "", 0, 0, fmt.Errorf("target string not found in file (even with fuzzy indentation/blank-line matching)")
	}
	if matchCount > 1 {
		return "", "", 0, 0, fmt.Errorf("ambiguous target: matched %d locations; include more surrounding lines", matchCount)
	}

	replaceLines := reindentReplacement(contentLines[startIdx:endIdx], targetLines, normReplace)

	var resultLines []string
	resultLines = append(resultLines, contentLines[:startIdx]...)
	resultLines = append(resultLines, replaceLines...)
	resultLines = append(resultLines, contentLines[endIdx:]...)
	return strings.Join(resultLines, "\n"), "fuzzy-indent", 0, 0, nil
}

func buildDidYouMeanHint(contentLines, targetLines []string) (int, int, string) {
	if len(contentLines) == 0 || len(targetLines) == 0 {
		return 0, 0, ""
	}
	var trimmedTarget []string
	for _, tl := range targetLines {
		if t := strings.TrimSpace(tl); t != "" {
			trimmedTarget = append(trimmedTarget, t)
		}
	}
	if len(trimmedTarget) == 0 {
		return 0, 0, ""
	}

	targetBlock := strings.Join(trimmedTarget, "\n")
	targetTok := tokenizeAlphaNum(targetBlock)
	targetBg := charBigrams(targetBlock)

	winLen := len(targetLines)
	if winLen < 1 {
		winLen = 1
	}
	if winLen > len(contentLines) {
		winLen = len(contentLines)
	}

	bestScore := 0.0
	bestStart, bestEnd := -1, -1

	for i := 0; i <= len(contentLines)-winLen; i++ {
		end := i + winLen
		candBlock := strings.TrimSpace(strings.Join(contentLines[i:end], "\n"))
		if candBlock == "" {
			continue
		}
		score := similarityScorePrecomputed(targetTok, targetBg, candBlock)
		if score > bestScore {
			bestScore = score
			bestStart = i
			bestEnd = end
		}
	}

	// Also check single-line match against the first non-empty target line in case target length shifted.
	if winLen > 1 {
		anchor := trimmedTarget[0]
		anchorTok := tokenizeAlphaNum(anchor)
		anchorBg := charBigrams(anchor)
		for i, line := range contentLines {
			t := strings.TrimSpace(line)
			if t == "" {
				continue
			}
			score := similarityScorePrecomputed(anchorTok, anchorBg, t) * 0.90
			if score > bestScore {
				bestScore = score
				bestStart = i
				bestEnd = i + winLen
				if bestEnd > len(contentLines) {
					bestEnd = len(contentLines)
				}
			}
		}
	}

	if bestStart < 0 || bestScore < 0.20 {
		return 0, 0, ""
	}

	// Cap snippet display to 12 lines so errors stay compact.
	displayEnd := bestEnd
	if displayEnd-bestStart > 12 {
		displayEnd = bestStart + 12
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Did you mean (around lines %d-%d)?", bestStart+1, displayEnd)
	for i := bestStart; i < displayEnd; i++ {
		fmt.Fprintf(&b, "\n%d: %s", i+1, contentLines[i])
	}
	return bestStart + 1, displayEnd, b.String()
}

func similarityScorePrecomputed(tokA, bgA map[string]bool, b string) float64 {
	tokB := tokenizeAlphaNum(b)
	tScore := diceSetScore(tokA, tokB)
	bgScore := diceSetScore(bgA, charBigrams(b))
	if len(tokA) == 0 && len(tokB) == 0 {
		return bgScore
	}
	return 0.55*tScore + 0.45*bgScore
}

func tokenizeAlphaNum(s string) map[string]bool {
	out := make(map[string]bool)
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out[cur.String()] = true
			cur.Reset()
		}
	}
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

func charBigrams(s string) map[string]bool {
	compact := strings.Join(strings.Fields(strings.ToLower(s)), " ")
	runes := []rune(compact)
	out := make(map[string]bool)
	if len(runes) < 2 {
		if len(runes) == 1 {
			out[string(runes)] = true
		}
		return out
	}
	for i := 0; i < len(runes)-1; i++ {
		out[string(runes[i:i+2])] = true
	}
	return out
}

func diceSetScore(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	return float64(2*inter) / float64(len(a)+len(b))
}

// findFuzzySpan locates [startIdx, endIdx) in contentLines matching non-empty trimmed targetLines,
// tolerating added or omitted blank lines inside the block.
func findFuzzySpan(contentLines, targetLines []string) (int, int, int) {
	var nonEmptyTarget []string
	for _, tl := range targetLines {
		if t := strings.TrimSpace(tl); t != "" {
			nonEmptyTarget = append(nonEmptyTarget, t)
		}
	}
	if len(nonEmptyTarget) == 0 {
		return -1, -1, 0
	}

	bestStart, bestEnd, matchCount := -1, -1, 0
	for i := 0; i < len(contentLines); i++ {
		if strings.TrimSpace(contentLines[i]) != nonEmptyTarget[0] {
			continue
		}
		cIdx := i
		tIdx := 0
		for cIdx < len(contentLines) && tIdx < len(nonEmptyTarget) {
			trimmed := strings.TrimSpace(contentLines[cIdx])
			if trimmed == "" {
				cIdx++
				continue
			}
			if trimmed != nonEmptyTarget[tIdx] {
				break
			}
			tIdx++
			cIdx++
		}
		if tIdx == len(nonEmptyTarget) {
			bestStart = i
			bestEnd = cIdx
			matchCount++
		}
	}
	return bestStart, bestEnd, matchCount
}

// reindentReplacement aligns base indentation and proportionally converts nested spaces<->tabs
func reindentReplacement(matchedFileLines, targetLines []string, normReplace string) []string {
	var fileIndent, targetIndent string
	fileUsesTabs := false
	for _, fl := range matchedFileLines {
		if strings.TrimSpace(fl) != "" {
			ind := leadingWhitespace(fl)
			if fileIndent == "" {
				fileIndent = ind
			}
			if strings.Contains(ind, "\t") {
				fileUsesTabs = true
			}
		}
	}
	for _, tl := range targetLines {
		if strings.TrimSpace(tl) != "" {
			targetIndent = leadingWhitespace(tl)
			break
		}
	}

	replaceLines := strings.Split(strings.TrimSuffix(normReplace, "\n"), "\n")
	for j, rl := range replaceLines {
		if strings.TrimSpace(rl) == "" {
			replaceLines[j] = ""
			continue
		}
		relLine := strings.TrimPrefix(rl, targetIndent)
		relIndent := leadingWhitespace(relLine)
		codePart := strings.TrimLeft(relLine, " \t")

		if fileUsesTabs && strings.Contains(relIndent, "    ") {
			relIndent = strings.ReplaceAll(relIndent, "    ", "\t")
		} else if !fileUsesTabs && strings.Contains(fileIndent, " ") && strings.Contains(relIndent, "\t") {
			relIndent = strings.ReplaceAll(relIndent, "\t", "    ")
		}
		replaceLines[j] = fileIndent + relIndent + codePart
	}
	return replaceLines
}

func leadingWhitespace(s string) string {
	trimmed := strings.TrimLeft(s, " \t")
	return s[:len(s)-len(trimmed)]
}

func (s *Server) execRunCommand(m *mutation) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", m.command)
	cmd.Dir = s.workspace()
	out, err := cmd.CombinedOutput()
	output := string(out)
	if len(output) > 16384 {
		output = output[:16384] + "\n... [truncated at 16KB]"
	}
	if err != nil {
		return fmt.Sprintf("exit error: %v\n%s", err, output), true
	}
	if output == "" {
		return "(command completed with no output)", false
	}
	return output, false
}
