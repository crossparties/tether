package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/croosparties/tether/internal/core"
)


// Server implements MCP protocol over stdio
type Server struct {
	engine *core.Engine
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
	}

	return &Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  map[string]interface{}{"tools": tools},
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

	var result string
	var isError bool

	switch params.Name {
	case "workspace_info":
		result = s.toolWorkspaceInfo()
	case "session_load":
		result = s.toolSessionLoad(params.Arguments)
	case "session_save":
		result, isError = s.toolSessionSave(params.Arguments)
	case "session_done":
		result, isError = s.toolSessionDone()
	case "setting_set":
		result, isError = s.toolSettingSet(params.Arguments)
	case "decision_add":
		result, isError = s.toolDecisionAdd(params.Arguments)
	case "pattern_add":
		result, isError = s.toolPatternAdd(params.Arguments)
	case "mode_set":
		result, isError = s.toolModeSet(params.Arguments)
	case "mode_get":
		result = s.toolModeGet()
	case "kb_index":
		result, isError = s.toolKBIndex(params.Arguments)
	case "kb_search":
		result, isError = s.toolKBSearch(params.Arguments)
	case "kb_status":
		result = s.toolKBStatus()
	case "todo_add":
		result, isError = s.toolTodoAdd(params.Arguments)
	case "todo_list":
		result = s.toolTodoList(params.Arguments)
	case "todo_complete":
		result, isError = s.toolTodoComplete(params.Arguments)
	case "todo_remove":
		result, isError = s.toolTodoRemove(params.Arguments)
	case "task_classify":
		result = s.toolTaskClassify(params.Arguments)
	case "files_modified":
		result = s.toolFilesModified()
	case "subagent_spawn_rules":
		result = s.toolSubagentSpawnRules()
	default:
		result = fmt.Sprintf("unknown tool: %s", params.Name)
		isError = true
	}

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

// toolSubagentSpawnRules returns mandatory rules for spawning write-capable subagents
func (s *Server) toolSubagentSpawnRules() string {
	return `## Mandatory Subagent Write Rules

PROBLEM: Subagents spawned with TypeName "self" have no system prompt guaranteeing
non-interactive terminal use. When they call mkdir or create files without the right
flags they can block on a terminal prompt → waiting_for_input → stuck forever.

### Rules (embed these in every write-capable subagent Prompt):

1. NEVER use write_to_file for project files.
   write_to_file only writes to the artifact dir (~/.gemini/brain/...), not the project.

2. NEVER use TypeName "self" for tasks that create or modify project files.
   Use a named subagent type (tether_architect, tether_qa, tether_frontend) that has
   these rules baked into its system prompt.

3. For NEW files: run_command with heredoc, WaitMsBeforeAsync >= 5000:
   cat > /absolute/path/to/file.go << 'EOF'
   ...file content...
   EOF

4. For MODIFYING existing files: replace_file_content tool.

5. For directories: run_command "mkdir -p /path", WaitMsBeforeAsync >= 3000.

6. All run_command calls MUST be non-interactive.
   Set WaitMsBeforeAsync to at least the expected completion time (min 5000ms for file writes).

7. After ALL file changes: run go build ./... 2>&1 and fix errors before reporting done.

### Named subagent types available:
- tether_architect: Go backend code, heredoc writes, build verify
- tether_qa: Test writing, go test runner
- tether_frontend: Frontend writes
- tether_product: Product specification and planning`
}
