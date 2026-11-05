package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/croosparties/tether/internal/core"
)


// Server implements MCP protocol over stdio
type Server struct {
	engine *core.Engine

	// Approval request identity. A run of blocked calls otherwise prints the
	// same opening words and boilerplate every time, so the reader cannot tell
	// which request they are looking at or whether they already answered it.
	mu           sync.Mutex
	approvalSeq  int
	approvalSeen map[string]bool
}

func NewServer(engine *core.Engine) *Server {
	return &Server{engine: engine}
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
	ProtocolVersion string     `json:"protocolVersion"`
	Capabilities    Capabilities `json:"capabilities"`
	ServerInfo      ServerInfo `json:"serverInfo"`
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
			Name:        "workspace_info",
			Description: "Get current workspace path, tech stack, and project settings",
			InputSchema: InputSchema{Type: "object"},
		},
		{
			Name:        "approve_proposal",
			Description: "Mark a subagent proposal as approved by the user. Call ONLY after the user explicitly says yes in the main chat. Lifts the Tether Guard write block.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"proposal_id": {Type: "integer", Description: "Proposal to approve. Omit to approve the oldest pending one."},
				},
			},
		},
		{
			Name:        "reject_proposal",
			Description: "Mark a subagent proposal as rejected by the user. Call when the user declines. Lifts the Tether Guard write block without applying the proposal.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"proposal_id": {Type: "integer", Description: "Proposal to reject. Omit to reject the oldest pending one."},
				},
			},
		},
		{
			Name:        "proposal_status",
			Description: "Show any subagent proposal still awaiting user approval",
			InputSchema: InputSchema{Type: "object"},
		},
		{
			Name:        "session_load",
			Description: "Load the active session context (recent history, decisions, patterns)",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"limit": {Type: "integer", Description: "Max context entries to return (default 10)"},
				},
			},
		},
		{
			Name:        "session_save",
			Description: "Save a context entry to the current session",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"agent":   {Type: "string", Description: "Agent name that produced this entry (e.g., agy, claude, codex, cursor)"},
					"role":    {Type: "string", Description: "Role: user|assistant|plan|decision"},
					"content": {Type: "string", Description: "Content to save"},
				},
				Required: []string{"role", "content"},
			},
		},
		{
			Name:        "session_done",
			Description: "Mark the current session as completed",
			InputSchema: InputSchema{Type: "object"},
		},
		{
			Name:        "setting_set",
			Description: "Set a project setting (persists across sessions)",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"key":   {Type: "string", Description: "Setting key"},
					"value": {Type: "string", Description: "Setting value"},
				},
				Required: []string{"key", "value"},
			},
		},
		{
			Name:        "decision_add",
			Description: "Record an architecture/design decision",
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
		{
			Name:        "pattern_add",
			Description: "Record a learned pattern (do or avoid)",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"type":        {Type: "string", Description: "Pattern type: do|avoid"},
					"description": {Type: "string", Description: "Pattern description"},
				},
				Required: []string{"type", "description"},
			},
		},
		{
			Name:        "mode_set",
			Description: "Set current mode (plan=read-only, code=full access)",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"mode": {Type: "string", Description: "Mode: plan|code"},
				},
				Required: []string{"mode"},
			},
		},
		{
			Name:        "mode_get",
			Description: "Get current mode and whether writes are allowed",
			InputSchema: InputSchema{Type: "object"},
		},
		{
			Name:        "kb_index",
			Description: "Index a file or directory into the knowledge base for full-text search",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"path":       {Type: "string", Description: "File or directory path to index"},
					"extensions": {Type: "string", Description: "Comma-separated file extensions to include (e.g., 'go,js,py'). Empty = all files"},
				},
				Required: []string{"path"},
			},
		},
		{
			Name:        "kb_search",
			Description: "Search the knowledge base using full-text search (BM25 ranking)",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"query": {Type: "string", Description: "Search query (supports AND, OR, NOT, phrase matching with quotes)"},
					"limit": {Type: "integer", Description: "Max results (default 10)"},
				},
				Required: []string{"query"},
			},
		},
		{
			Name:        "kb_status",
			Description: "Show knowledge base stats (indexed files and chunks)",
			InputSchema: InputSchema{Type: "object"},
		},
		{
			Name:        "todo_add",
			Description: "Add a todo item to track tasks",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"description": {Type: "string", Description: "Task description"},
					"details":     {Type: "string", Description: "Optional details about the task"},
				},
				Required: []string{"description"},
			},
		},
		{
			Name:        "todo_list",
			Description: "List all todos (pending and optionally completed)",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"include_completed": {Type: "string", Description: "Include completed todos? 'true' or 'false' (default false)"},
				},
			},
		},
		{
			Name:        "todo_complete",
			Description: "Mark a todo as completed",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"id": {Type: "integer", Description: "Todo ID to mark complete"},
				},
				Required: []string{"id"},
			},
		},
		{
			Name:        "todo_remove",
			Description: "Remove a todo item",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"id": {Type: "integer", Description: "Todo ID to remove"},
				},
				Required: []string{"id"},
			},
		},
		{
			Name:        "task_classify",
			Description: "Classify a task to determine optimal execution mode: tether_direct | tether_review | tether_plan.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"prompt": {Type: "string", Description: "The user's task description or request"},
				},
				Required: []string{"prompt"},
			},
		},
		{
			Name:        "files_modified",
			Description: "List all files modified in this session (useful for cross-agent handoff context)",
			InputSchema: InputSchema{Type: "object"},
		},
		{
			Name:        "subagent_spawn_rules",
			Description: "Get mandatory rules for spawning subagents with write access. Call this before invoking any subagent that will create or modify files.",
			InputSchema: InputSchema{Type: "object"},
		},
		{
			Name:        "sql_query",
			Description: "Directly execute SQL query against session.db (full access to sessions, session_context, project_settings, decisions, learned_patterns, todos, kb_documents). Returns results as JSON.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"query": {Type: "string", Description: "SQL query to execute (e.g. 'SELECT * FROM sessions', 'SELECT * FROM decisions')"},
				},
				Required: []string{"query"},
			},
		},
		{
			Name:        "read_file",
			Description: "Read file contents in the workspace (optionally specify start_line and end_line to save context tokens).",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"path":       {Type: "string", Description: "Relative or absolute path of the file to read"},
					"start_line": {Type: "integer", Description: "Optional 1-based start line"},
					"end_line":   {Type: "integer", Description: "Optional 1-based end line"},
				},
				Required: []string{"path"},
			},
		},
		{
			Name:        "approve_change",
			Description: "Record the user's approval for a change the guard blocked. Nothing is written: repeat the original tool call afterwards. Use scope=\"all\" to approve every change under the given path until the grant expires.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"path":  {Type: "string", Description: "Path or command exactly as shown in the approval request"},
					"scope": {Type: "string", Description: "once (default) or all (every change under path, or every command with the prefix)"},
					"kind":  {Type: "string", Description: "path (default), command, or command-prefix. Use command kinds for run_command; they never authorize file writes."},
				},
				Required: []string{"path"},
			},
		},
		{
			Name:        "reject_change",
			Description: "Decline a change the guard blocked. Nothing is written; ask the user what they want instead.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"path": {Type: "string", Description: "Path exactly as shown in the approval request"},
				},
				Required: []string{"path"},
			},
		},
		{
			Name:        "list_grants",
			Description: "Show approval grants currently in effect for this session.",
			InputSchema: InputSchema{Type: "object", Properties: map[string]Property{}},
		},
		{
			Name:        "revoke_grants",
			Description: "Clear every approval grant so each change asks again.",
			InputSchema: InputSchema{Type: "object", Properties: map[string]Property{}},
		},
		{
			Name:        "write_file",
			Description: "Create or overwrite a file in the workspace. Requires user approval unless a grant covers the path; the blocked response contains the preview and the approve_change call to use.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"path":    {Type: "string", Description: "Relative or absolute path of the file to write"},
					"content": {Type: "string", Description: "Full content to write to the file"},
				},
				Required: []string{"path", "content"},
			},
		},
		{
			Name:        "edit_file",
			Description: "Replace an exact target string in an existing file with replacement text. Requires user approval unless a grant covers the path.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"path":        {Type: "string", Description: "Relative or absolute path of the file to edit"},
					"target":      {Type: "string", Description: "Exact string to find and replace"},
					"replacement": {Type: "string", Description: "Replacement string"},
				},
				Required: []string{"path", "target", "replacement"},
			},
		},
		{
			Name:        "run_command",
			Description: "Execute a shell command in the workspace. Requires user approval, and is additionally subject to Plan vs Code mode and catastrophic command guardrails.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"command": {Type: "string", Description: "Shell command to execute"},
				},
				Required: []string{"command"},
			},
		},
	}

	return &Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  map[string]interface{}{"tools": tools},
	}
}

// ListTools returns the full list of registered Tether + workspace tools
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

	// Approval gate: every mutating tool must be authorized before it can
	// touch the workspace. This is the single choke point that the MCP path
	// previously lacked, which let writes bypass internal/hooks entirely.
	if msg := s.requireApproval(name, args); msg != "" {
		return msg, true
	}

	switch name {
	case "workspace_info":
		return s.toolWorkspaceInfo(), false
	case "approve_proposal":
		return s.toolResolveProposal("approve", args), false
	case "reject_proposal":
		return s.toolResolveProposal("reject", args), false
	case "proposal_status":
		return s.toolProposalStatus(), false
	case "approve_change":
		return s.toolApproveChange("approve", args), false
	case "reject_change":
		return s.toolApproveChange("reject", args), false
	case "list_grants":
		return s.toolListGrants(), false
	case "revoke_grants":
		return s.toolRevokeGrants(), false
	case "session_load":
		return s.toolSessionLoad(args), false
	case "session_save":
		return s.toolSessionSave(args)
	case "session_done":
		return s.toolSessionDone()
	case "setting_set":
		return s.toolSettingSet(args)
	case "decision_add":
		return s.toolDecisionAdd(args)
	case "pattern_add":
		return s.toolPatternAdd(args)
	case "mode_set":
		return s.toolModeSet(args)
	case "mode_get":
		return s.toolModeGet(), false
	case "kb_index":
		return s.toolKBIndex(args)
	case "kb_search":
		return s.toolKBSearch(args)
	case "kb_status":
		return s.toolKBStatus(), false
	case "todo_add":
		return s.toolTodoAdd(args)
	case "todo_list":
		return s.toolTodoList(args), false
	case "todo_complete":
		return s.toolTodoComplete(args)
	case "todo_remove":
		return s.toolTodoRemove(args)
	case "task_classify":
		return s.toolTaskClassify(args), false
	case "files_modified":
		return s.toolFilesModified(), false
	case "subagent_spawn_rules":
		return s.toolSubagentSpawnRules(), false
	case "sql_query":
		return s.toolSQLQuery(args)
	case "read_file":
		return s.toolReadFile(args)
	case "write_file":
		return s.toolWriteFile(args)
	case "edit_file":
		return s.toolEditFile(args)
	case "run_command":
		return s.toolRunCommand(args)
	default:
		return fmt.Sprintf("unknown tool: %s", name), true
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

func (s *Server) toolWorkspaceInfo() string {
	settings, _ := s.engine.GetAllProjectSettings()
	info := map[string]interface{}{
		"workspace":  s.engine.Workspace,
		"session_id": s.engine.SessionID,
		"mode":       s.engine.GetMode(),
		"settings":   settings,
	}
	out, _ := json.MarshalIndent(info, "", "  ")
	return string(out)
}

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

// toolResolveProposal approves or rejects the pending subagent proposal. It is
// the only path that lifts the PreToolUse mutation gate.
func (s *Server) toolResolveProposal(action string, args json.RawMessage) string {
	var params struct {
		ProposalID int64  `json:"proposal_id"`
		Comment    string `json:"comment"`
		Topic      bool   `json:"topic"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &params)
	}
	if action != "approve" && action != "reject" {
		return fmt.Sprintf("unknown action %q (approve|reject)", action)
	}

	var (
		pending *core.ProposalRecord
		err     error
	)
	if params.ProposalID > 0 {
		candidate, gerr := s.engine.GetProposalByID(params.ProposalID)
		if gerr != nil {
			return fmt.Sprintf("error reading proposal #%d: %v", params.ProposalID, gerr)
		}
		if candidate == nil {
			return fmt.Sprintf("no proposal #%d", params.ProposalID)
		}
		if candidate.Status != "pending" {
			return fmt.Sprintf("proposal #%d is already %s", candidate.ID, candidate.Status)
		}
		pending = candidate
	} else {
		pending, err = s.engine.GetPendingProposal()
		if err != nil {
			return fmt.Sprintf("error reading pending proposal: %v", err)
		}
		if pending == nil {
			return "no pending proposal (the Tether Guard write block is not active)"
		}
	}

	var affected int64
	switch {
	case action == "approve":
		affected, err = s.engine.ApproveProposalByID(pending.ID)
	case params.Topic:
		affected, err = s.engine.AbandonProposal(pending.ID)
	default:
		affected, err = s.engine.RejectProposalWithComment(pending.ID, params.Comment)
	}
	if err != nil {
		return fmt.Sprintf("error resolving proposal: %v", err)
	}

	verdict := "approved"
	if action != "approve" {
		verdict = "rejected"
	}
	if params.Topic && action != "approve" {
		verdict = "abandoned"
	}
	payload := map[string]interface{}{
		"proposal_id":   pending.ID,
		"role":          pending.Role,
		"status":        verdict,
		"affected":      affected,
		"write_blocked": false,
	}
	// A rejection is not a green light to self-correct: hand the agent the
	// user's steering and tell it to wait when none was given.
	switch verdict {
	case "rejected":
		payload["write_blocked"] = true
		payload["must_revise"] = true
		payload["comment"] = params.Comment
		if params.Comment == "" {
			payload["instruction"] = "No steering given. Wait for the user's comment; do not guess at a fix."
		} else {
			payload["instruction"] = "Revise the proposal using the user's comment. Do not implement the rejected plan."
		}
	case "abandoned":
		payload["write_blocked"] = true
		payload["must_revise"] = false
		payload["instruction"] = "The user changed topic. Drop this work and do not resubmit it."
	}
	out, _ := json.MarshalIndent(payload, "", "  ")
	return string(out)
}

// toolProposalStatus reports whether a subagent proposal is blocking writes.
func (s *Server) toolProposalStatus() string {
	pending, err := s.engine.GetPendingProposal()
	if err != nil {
		return fmt.Sprintf("error reading pending proposal: %v", err)
	}
	if pending == nil {
		out, _ := json.MarshalIndent(map[string]interface{}{
			"pending":       false,
			"write_blocked": false,
		}, "", "  ")
		return string(out)
	}

	preview := pending.Proposal
	if len(preview) > 500 {
		preview = preview[:500] + "... [truncated]"
	}
	out, _ := json.MarshalIndent(map[string]interface{}{
		"pending":       true,
		"write_blocked": true,
		"proposal_id":   pending.ID,
		"role":          pending.Role,
		"created_at":    pending.CreatedAt,
		"proposal":      preview,
	}, "", "  ")
	return string(out)
}

func (s *Server) toolSessionLoad(args json.RawMessage) string {
	var params struct {
		Limit int `json:"limit"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "invalid params"
	}
	if params.Limit == 0 {
		params.Limit = 10
	}

	context, _ := s.engine.GetRecentContext(params.Limit)
	decisions, _ := s.engine.GetDecisions()
	patterns, _ := s.engine.GetPatterns()
	summary, _ := s.engine.GetSessionSummary()

	result := map[string]interface{}{
		"session_id": s.engine.SessionID,
		"summary":    summary,
		"context":    context,
		"decisions":  decisions,
		"patterns":   patterns,
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out)
}

func (s *Server) toolSessionSave(args json.RawMessage) (string, bool) {
	var params struct {
		Agent   string `json:"agent"`
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "invalid params", true
	}
	if params.Agent == "" {
		params.Agent = "unknown"
	}
	if err := s.engine.AddContext(params.Agent, params.Role, params.Content); err != nil {
		return err.Error(), true
	}
	return "saved", false
}

func (s *Server) toolSessionDone() (string, bool) {
	if err := s.engine.CompleteSession(); err != nil {
		return err.Error(), true
	}
	return "session marked completed", false
}

func (s *Server) toolSettingSet(args json.RawMessage) (string, bool) {
	var params struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "invalid params", true
	}
	if err := s.engine.SetProjectSetting(params.Key, params.Value); err != nil {
		return err.Error(), true
	}
	return fmt.Sprintf("set %s = %s", params.Key, params.Value), false
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
	if err := s.engine.AddDecision(params.Topic, params.Decision, params.Reason); err != nil {
		return err.Error(), true
	}
	return "decision recorded", false
}

func (s *Server) toolPatternAdd(args json.RawMessage) (string, bool) {
	var params struct {
		Type        string `json:"type"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "invalid params", true
	}
	if err := s.engine.AddPattern(params.Type, params.Description); err != nil {
		return err.Error(), true
	}
	return "pattern recorded", false
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
	s.engine.SetMode(params.Mode)
	return fmt.Sprintf("mode set to: %s", params.Mode), false
}

func (s *Server) toolModeGet() string {
	info := map[string]interface{}{
		"mode":          s.engine.GetMode(),
		"write_allowed": s.engine.IsWriteAllowed(),
	}
	out, _ := json.Marshal(info)
	return string(out)
}

// --- Knowledge Base Tools ---

func (s *Server) toolKBIndex(args json.RawMessage) (string, bool) {
	var params struct {
		Path       string `json:"path"`
		Extensions string `json:"extensions"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "invalid params", true
	}

	// Resolve relative path
	path := params.Path
	if !strings.HasPrefix(path, "/") {
		path = s.engine.Workspace + "/" + path
	}

	// Check if file or directory
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("path not found: %s", path), true
	}

	if info.IsDir() {
		var exts []string
		if params.Extensions != "" {
			exts = strings.Split(params.Extensions, ",")
			for i := range exts {
				exts[i] = strings.TrimSpace(exts[i])
			}
		}
		count, err := s.engine.IndexDirectory(path, exts)
		if err != nil {
			return err.Error(), true
		}
		return fmt.Sprintf("indexed %d files from %s", count, path), false
	}

	if err := s.engine.IndexFile(path); err != nil {
		return err.Error(), true
	}
	return fmt.Sprintf("indexed file: %s", path), false
}

func (s *Server) toolKBSearch(args json.RawMessage) (string, bool) {
	var params struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "invalid params", true
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

func (s *Server) toolKBStatus() string {
	stats, _ := s.engine.KBStatus()
	out, _ := json.MarshalIndent(stats, "", "  ")
	return string(out)
}

// --- Todo Tools ---

func (s *Server) toolTodoAdd(args json.RawMessage) (string, bool) {
	var params struct {
		Description string `json:"description"`
		Details     string `json:"details"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "invalid params", true
	}

	id, err := s.engine.AddTodo(params.Description, params.Details)
	if err != nil {
		return err.Error(), true
	}
	return fmt.Sprintf("todo #%d added: %s", id, params.Description), false
}

func (s *Server) toolTodoList(args json.RawMessage) string {
	var params struct {
		IncludeCompleted string `json:"include_completed"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "error: failed to parse params: " + err.Error()
	}

	includeCompleted := params.IncludeCompleted == "true"
	todos, _ := s.engine.ListTodos(includeCompleted)

	if len(todos) == 0 {
		return "no todos"
	}

	out, _ := json.MarshalIndent(todos, "", "  ")
	return string(out)
}

func (s *Server) toolTodoComplete(args json.RawMessage) (string, bool) {
	var params struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "invalid params", true
	}

	if err := s.engine.CompleteTodo(params.ID); err != nil {
		return err.Error(), true
	}
	return fmt.Sprintf("todo #%d completed", params.ID), false
}

func (s *Server) toolTodoRemove(args json.RawMessage) (string, bool) {
	var params struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "invalid params", true
	}

	if err := s.engine.RemoveTodo(params.ID); err != nil {
		return err.Error(), true
	}
	return fmt.Sprintf("todo #%d removed", params.ID), false
}

func (s *Server) toolTaskClassify(args json.RawMessage) string {
	var params struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "invalid params"
	}

	mode := core.ClassifyTask(params.Prompt)
	switch mode {
	case "tether_plan":
		return `{"mode":"tether_plan","reason":"Plan/spec trigger detected","action":"Switch to plan mode and produce design artifact in docs/"}`
	case "tether_review":
		return `{"mode":"tether_review","reason":"Audit/review task detected","action":"Read-only audit without mutations"}`
	default:
		return `{"mode":"tether_direct","reason":"Implementation task","action":"Direct coding and tool execution"}`
	}
}


// toolFilesModified returns all files modified in this session
func (s *Server) toolFilesModified() string {
	entries, err := s.engine.GetRecentContext(100)
	if err != nil {
		return "error fetching context"
	}

	seen := make(map[string]bool)
	var files []string
	for _, e := range entries {
		if e.Role == "file_modified" && !seen[e.Content] {
			seen[e.Content] = true
			files = append(files, e.Content)
		}
	}

	if len(files) == 0 {
		return "no files modified in this session"
	}

	out, _ := json.MarshalIndent(map[string]interface{}{
		"session_id":     s.engine.SessionID,
		"files_modified": files,
		"count":          len(files),
	}, "", "  ")
	return string(out)
}

// toolSubagentSpawnRules returns mandatory rules for subagents and orchestrator approval protocol
func (s *Server) toolSubagentSpawnRules() string {
	return `## Subagent Governance & Approval Protocol

### Architecture: Orchestrator-Worker Model
1. Subagents are SPECIALIST WORKERS (analysis, research, code review, diff drafting).
2. Subagents CANNOT mutate the codebase or execute mutative shell commands directly.
3. Subagents MUST send their proposed changes/diffs/commands via message back to the Main Agent.
4. The Main Agent (Orchestrator) presents the proposal to the user in the main chat session for approval.
5. Only after explicit user approval does the Main Agent execute the approved changes.

### Rules for Subagent Prompts:
1. NO DIRECT MUTATION PERMISSION: Do NOT attempt write_to_file, replace_file_content, or shell mutators. Tether Guard will strictly deny direct mutations.
2. READ & ANALYZE: Use read_file, view_file, grep, search_files, kb_search freely.
3. PROPOSE ONLY: Compose all proposed code modifications as markdown diff blocks or shell command scripts.
4. RETURN VIA MESSAGE: Return the proposal clearly formatted in the final message to the Main Agent.
5. DO NOT LOOP ON DENIAL: If any mutation is denied, immediately return your findings and proposed diff/command via message to the Main Agent.

### Named subagent types available:
- tether_architect: Architecture design and diff composition
- tether_qa: Test planning and verification script design
- tether_frontend: Frontend component design and styling diffs
- tether_product: Product specification and planning`
}


func (s *Server) toolSQLQuery(args json.RawMessage) (string, bool) {
	var params struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "invalid params", true
	}
	if strings.TrimSpace(params.Query) == "" {
		return "query cannot be empty", true
	}

	res, err := s.engine.ExecuteSQL(params.Query)
	if err != nil {
		return fmt.Sprintf("sql error: %v", err), true
	}

	out, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal error: %v", err), true
	}
	return string(out), false
}

// --- Workspace Primitives (for Micro-Harness & MCP) ---

func (s *Server) resolvePath(p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(s.engine.Workspace, p)
}

func (s *Server) toolReadFile(args json.RawMessage) (string, bool) {
	var params struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal(args, &params); err != nil || params.Path == "" {
		return "invalid params: path required", true
	}
	data, err := os.ReadFile(s.resolvePath(params.Path))
	if err != nil {
		return err.Error(), true
	}
	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	if params.StartLine > 0 || params.EndLine > 0 {
		lines := strings.Split(content, "\n")
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
			fmt.Fprintf(&b, "%d: %s\n", i+1, lines[i])
		}
		content = b.String()
	}
	if len(content) > 32768 {
		return content[:32768] + "\n... [truncated at 32KB]", false
	}
	return content, false
}

func (s *Server) toolWriteFile(args json.RawMessage) (string, bool) {
	var params struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &params); err != nil || params.Path == "" {
		return "invalid params: path and content required", true
	}
	fullPath := s.resolvePath(params.Path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return err.Error(), true
	}
	if err := os.WriteFile(fullPath, []byte(params.Content), 0644); err != nil {
		return err.Error(), true
	}
	_ = s.engine.IndexFile(fullPath)
	return fmt.Sprintf("wrote %d bytes to %s%s", len(params.Content), params.Path, s.autoVerifyGo(fullPath)), false
}

func (s *Server) toolEditFile(args json.RawMessage) (string, bool) {
	var params struct {
		Path        string `json:"path"`
		Target      string `json:"target"`
		Replacement string `json:"replacement"`
	}
	if err := json.Unmarshal(args, &params); err != nil || params.Path == "" || params.Target == "" {
		return "invalid params: path and target required", true
	}
	fullPath := s.resolvePath(params.Path)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return err.Error(), true
	}
	updated, tier, err := applyFuzzyEdit(string(data), params.Target, params.Replacement)
	if err != nil {
		return err.Error(), true
	}
	if err := os.WriteFile(fullPath, []byte(updated), 0644); err != nil {
		return err.Error(), true
	}
	_ = s.engine.IndexFile(fullPath)
	return fmt.Sprintf("edited %s (match:%s)%s", params.Path, tier, s.autoVerifyGo(fullPath)), false
}

func (s *Server) autoVerifyGo(fullPath string) string {
	if !strings.HasSuffix(fullPath, ".go") {
		return ""
	}
	if _, err := os.Stat(filepath.Join(s.engine.Workspace, "go.mod")); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "vet", "./...")
	cmd.Dir = s.engine.Workspace
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "\n⚠ go vet failed:\n" + strings.TrimSpace(string(out))
	}
	return " | ✓ go vet passed"
}

// applyFuzzyEdit implements a 3-Tier surgical matcher:
// Tier 1: Unique exact substring match (with ambiguity guard)
// Tier 2: Unique CRLF-normalized match (with ambiguity guard)
// Tier 3: Indentation & blank-line tolerant block match with proportional nested re-indentation
func applyFuzzyEdit(content, target, replacement string) (string, string, error) {
	// Tier 1: Exact match with ambiguity guard
	if cnt := strings.Count(content, target); cnt > 0 {
		if cnt > 1 {
			return "", "", fmt.Errorf("ambiguous target: matched %d locations; include more surrounding lines", cnt)
		}
		return strings.Replace(content, target, replacement, 1), "exact", nil
	}

	normContent := strings.ReplaceAll(content, "\r\n", "\n")
	normTarget := strings.ReplaceAll(target, "\r\n", "\n")
	normReplace := strings.ReplaceAll(replacement, "\r\n", "\n")

	// Tier 2: CRLF-normalized match with ambiguity guard
	if cnt := strings.Count(normContent, normTarget); cnt > 0 {
		if cnt > 1 {
			return "", "", fmt.Errorf("ambiguous target: matched %d locations; include more surrounding lines", cnt)
		}
		return strings.Replace(normContent, normTarget, normReplace, 1), "crlf-normalized", nil
	}

	// Tier 3: Indentation & blank-line tolerant block match
	contentLines := strings.Split(normContent, "\n")
	targetLines := strings.Split(strings.TrimSuffix(normTarget, "\n"), "\n")

	startIdx, endIdx, matchCount := findFuzzySpan(contentLines, targetLines)
	if matchCount == 0 {
		return "", "", fmt.Errorf("target string not found in file (even with fuzzy indentation/blank-line matching)")
	}
	if matchCount > 1 {
		return "", "", fmt.Errorf("ambiguous target: matched %d locations; include more surrounding lines", matchCount)
	}

	replaceLines := reindentReplacement(contentLines[startIdx:endIdx], targetLines, normReplace)

	var resultLines []string
	resultLines = append(resultLines, contentLines[:startIdx]...)
	resultLines = append(resultLines, replaceLines...)
	resultLines = append(resultLines, contentLines[endIdx:]...)
	return strings.Join(resultLines, "\n"), "fuzzy-indent", nil
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

func (s *Server) toolRunCommand(args json.RawMessage) (string, bool) {
	var params struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &params); err != nil || strings.TrimSpace(params.Command) == "" {
		return "invalid params: command required", true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", params.Command)
	cmd.Dir = s.engine.Workspace
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


