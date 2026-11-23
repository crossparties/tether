package tui

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	tetherctx "github.com/crossparties/tether/internal/context"
	"github.com/crossparties/tether/internal/tui/views"
)

// KeyType enumerates terminal key events processed by the TEA Update loop.
type KeyType int

const (
	KeyRune KeyType = iota
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyTab
	KeyEnter
	KeyEsc
	KeyCtrlC
	KeyBackspace
	KeyPgUp
	KeyPgDn
	KeyWheelUp
	KeyWheelDown
	KeyToggleMouse
	KeySpace
)

// KeyMsg represents a keyboard event in the Bubble-Tea-style architecture.
type KeyMsg struct {
	Type KeyType
	Rune rune
}

// WindowSizeMsg updates the terminal dimensions (e.g., on SIGWINCH).
type WindowSizeMsg struct {
	Width  int
	Height int
}

// TickMsg advances the Braille spinner and elapsed timer for active background workers.
type TickMsg struct {
	Now time.Time
}

// AgentStreamLogMsg delivers a live tool-action or auto-approval line from an active worker.
type AgentStreamLogMsg struct {
	WorkerID string
	Line     string
}

// AgentDeltaMsg delivers one live token delta from the streaming Main Agent.
// Kind is "content", "reasoning", or "endstep" (a step ended with tool calls).
type AgentDeltaMsg struct {
	Kind string
	Text string
}

// AgentTurnDoneMsg delivers the completed LLM response (or error) from an async worker.
type AgentTurnDoneMsg struct {
	WorkerID string
	Prompt   string
	Answer   string
	Err      error
	Elapsed  time.Duration
}

// SideChatDoneMsg delivers the isolated `/btw` response without touching the main stream context.
type SideChatDoneMsg struct {
	Question string
	Answer   string
	Err      error
	Elapsed  time.Duration
}

// CompactDoneMsg delivers the asynchronous `/compact` episodic condensation outcome.
type CompactDoneMsg struct {
	Result  string
	Err     error
	Elapsed time.Duration
}

// ApprovalGateMsg asks the TUI to display the Double-Bordered Approval Modal and send the user's verdict on ReplyCh.
type ApprovalGateMsg struct {
	Request ApprovalRequest
	ReplyCh chan int // receives views.ApprovalOptionIndex (0=Approve, 2=Reject, 3=Abort)
}

// PickerOption represents a selectable item for `/model` or `/provider` interactive pickers.
type PickerOption struct {
	Label  string
	Detail string
}

// AgentBridge connects the 3-Zone TUI to the live Tether Harness Runner & Substrate Engine.
type AgentBridge struct {
	Provider       func() string
	Model          func() string
	Mode           func() string
	Branch         func() string
	NeedsAPIKey    func() bool
	SaveAPIKey     func(key string) error
	ListKeys       func() string
	ProbeKeys      func(ctx context.Context) string
	ListModels     func() []PickerOption
	SwitchModel    func(name string) (string, error)
	ListProviders  func() []PickerOption
	SwitchProvider func(name string) (string, string, error)
	SwitchMode     func(mode string) error
	ClearHistory   func()
	Undo           func(turn string) string
	StatusSummary  func() string
	TokensSummary  func() string
	Compact        func(ctx context.Context) (string, error)
	ExecuteCommand func(ctx context.Context, cmdStr string) (string, error)
	RunPrompt      func(ctx context.Context, prompt string, logOut io.Writer) (string, error)
	// RunPromptStream is the streaming variant of RunPrompt; when set it is preferred so
	// tokens appear live. onDelta is called from the worker goroutine.
	RunPromptStream func(ctx context.Context, prompt string, logOut io.Writer, onDelta func(kind, text string)) (string, error)
	RunSideChat     func(ctx context.Context, question string) (string, error)
	BindApproval    func(askFn func(req ApprovalRequest) int)
}

// WorkerState represents the execution state of a sub-agent worker in Zone A.
type WorkerState string

const (
	WorkerActive    WorkerState = "ACTIVE"
	WorkerDone      WorkerState = "DONE"
	WorkerWait      WorkerState = "WAIT"
	WorkerCancelled WorkerState = "CANCELLED"
)

// WorkerStatus tracks a single worker displayed in the Zone A Worker Monitor.
type WorkerStatus struct {
	ID        string
	Name      string
	Task      string
	Detail    string
	State     WorkerState
	Spinner   string
	StartedAt time.Time
}

// StreamEntryKind distinguishes main task stream blocks from `/btw` side-chat blocks.
type StreamEntryKind int

const (
	StreamMainAgent StreamEntryKind = iota
	StreamUserPrompt
	StreamSideChat // /btw ephemeral side-channel block in #FAB387 (Peach)
	StreamSystemNotice
	StreamThought // compact "▸ Thought for Xs" + 1-line faded preview
	StreamTool    // compact 1-line "● Read (...)" / "● Ran (...)" tool row
)

// StreamEntry represents one message or collapsible block in Zone B (Middle Deck).
type StreamEntry struct {
	Kind      StreamEntryKind
	Title     string
	Body      []string
	Collapsed bool
	Summary   string
}

// SlashCommandSpec defines a `/` command entry for the Command Palette.
type SlashCommandSpec struct {
	Command     string // e.g. "/btw"
	Description string // e.g. "Diskusi sampingan tanpa ganggu task"
	NeedsArg    bool   // true if Tab completion should append a space
}

// FileCandidate represents a workspace file available in `@` autocomplete.
type FileCandidate struct {
	RelPath   string // e.g. "internal/tui/model.go"
	Extension string // e.g. "Go"
	SizeBytes int64
}

// ApprovalRequest holds the pending high-risk action requiring human sign-off.
type ApprovalRequest struct {
	ProposalID     int64
	AgentName      string
	ActionType     string
	Target         string
	ScopeHint      string // e.g. "commands starting with `rm`" or "writes under internal/core"
	MainEvaluation string
	DiffLines      []string
	SelectedOption int // 0..5
	LastDecision   string
}

// InspectorState holds the state for the scrollable diff/file inspector overlay.
type InspectorState struct {
	Active       bool
	Title        string
	Lines        []string
	ScrollOffset int
}

// DefaultSlashCommands returns the built-in `/` commands from PRD §3.2 & §3.5.
func DefaultSlashCommands() []SlashCommandSpec {
	return []SlashCommandSpec{
		{Command: "/btw", Description: "Isolated side-chat (zero context pollution)", NeedsArg: true},
		{Command: "/model", Description: "Select or switch the active LLM model", NeedsArg: true},
		{Command: "/provider", Description: "Select or switch LLM provider (deepseek/openai/gemini/etc.)", NeedsArg: true},
		{Command: "/read", Description: "Inspect file contents in scrollable viewport", NeedsArg: true},
		{Command: "/copy", Description: "Copy last answer to clipboard (/copy all, /copy code)", NeedsArg: false},
		{Command: "/select", Description: "Toggle mouse capture: ON=wheel scroll, OFF=drag-select & copy (Ctrl+Y)", NeedsArg: false},
		{Command: "/exec", Description: "Execute a shell command through the safety gate", NeedsArg: true},
		{Command: "/plan", Description: "Lock repository into read-only PLAN mode", NeedsArg: false},
		{Command: "/code", Description: "Unlock codebase mutations (CODE mode)", NeedsArg: false},
		{Command: "/status", Description: "Show workspace, session, KB, and worker metrics", NeedsArg: false},
		{Command: "/tokens", Description: "Show per-usage_id context pressure and cache hit ratios", NeedsArg: false},
		{Command: "/compact", Description: "Condense older conversation history into a 5-slot episodic summary", NeedsArg: false},
		{Command: "/undo", Description: "Restore file snapshots from the last turn", NeedsArg: false},
		{Command: "/clear", Description: "Clear the conversation viewport buffer", NeedsArg: false},
		{Command: "/help", Description: "Display keyboard shortcuts and command reference", NeedsArg: false},
		{Command: "/exit", Description: "Gracefully exit the CLI session", NeedsArg: false},
	}
}

// Model is the deterministic Bubble-Tea-style state container for the 3-Zone TUI.
type Model struct {
	Width         int
	Height        int
	Version       string
	Mode          string
	ProviderName  string
	ModelName     string
	Branch        string
	Workspace     string
	MemoryMB      uint64
	AltScreenUsed bool

	// Zone A: Active Agent Status Monitor
	Workers     []WorkerStatus
	SpinnerStep int
	cancelAllFn func() int
	CancelCount int

	// Zone B: Main Task Stream & /btw Side-Chat Stream
	Stream       []StreamEntry
	StreamScroll int // lines scrolled up from bottom (0 = pinned to bottom)

	// Zone C: Input Buffer, Cursor, and PRD §3.3 Prompt History + DraftBuffer
	InputBuffer  string
	CursorPos    int // Measured in runes; len([]rune(InputBuffer)) is EOL
	History      []string
	HistoryIndex int    // -1 when editing live DraftBuffer; 0..len(History)-1 when browsing history
	DraftBuffer  string // Preserves unsent input while navigating Up/Down through History

	// Autocomplete Overlay (/ and @) + Interactive /model & /provider sub-picker
	SlashCommands     []SlashCommandSpec
	FileIndex         []FileCandidate
	DropdownKind      views.AutocompleteKind
	DropdownQuery     string
	DropdownItems     []views.AutocompleteItem
	DropdownIndex     int
	MaxDropdownRows   int
	ActiveAtTokenByte [2]int // [startByte, endByte] of active @query token
	PickerMode        string // "" | "model" | "provider"

	// Two-Tiered Approval Modal & Scrollable Inspector
	Approval        *ApprovalRequest
	approvalReplyCh chan int
	LastApprovalRes string
	Inspector       InspectorState
	Quitting        bool

	// Live Agent Bridge & Pending Async Dispatch Queue
	Bridge         *AgentBridge
	AwaitingKey    bool
	PendingPrompt  string
	PendingBtw     string
	PendingCompact bool
	PendingExec    string
	PendingCopy    string // text queued for the system clipboard (OSC 52 + native helpers)

	// MouseCapture is true while the TUI owns the mouse wheel (scrolling). When false the
	// terminal's native text selection works, so the user can drag-select and copy.
	MouseCapture bool

	// liveStream and liveThought are 1 + the indices in Stream of the Main Agent block
	// and flushed thought block for the active attempt (0 = none).
	liveStream  int
	liveThought int

	// Live faded reasoning & activity status (agy-style live preview while working)
	ReasoningBuf     string
	ReasoningStarted time.Time
	LiveStatus       string
}

// NewModel initializes a deterministic 3-Zone TUI Model with sensible defaults.
func NewModel(workspace string) Model {
	if strings.TrimSpace(workspace) == "" {
		workspace = "."
	}
	m := Model{
		Width:           80,
		Height:          24,
		Version:         "v1.0",
		Mode:            "CODE",
		ProviderName:    "deepseek",
		ModelName:       "deepseek-chat",
		Branch:          "main",
		Workspace:       workspace,
		HistoryIndex:    -1,
		MouseCapture:    true,
		SlashCommands:   DefaultSlashCommands(),
		MaxDropdownRows: 6,
		Workers: []WorkerStatus{
			{
				ID:      "main",
				Name:    "Main Agent",
				Task:    "Ready",
				Detail:  "Idle",
				State:   WorkerDone,
				Spinner: "●",
			},
		},
		Stream: []StreamEntry{
			{
				Kind:  StreamSystemNotice,
				Title: "tether · local-first agentic coding CLI",
				Body: []string{
					"Type / for commands, @ to reference files, /btw for isolated side-chat, or Ctrl+Y to select & copy.",
				},
			},
		},
	}
	m.RefreshMemoryUsage()
	return m
}

// RefreshMemoryUsage samples runtime.MemStats so Zone A displays live memory footprint.
func (m *Model) RefreshMemoryUsage() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	mb := ms.Alloc / (1024 * 1024)
	if mb == 0 {
		mb = 1
	}
	m.MemoryMB = mb
}

// SetFiles populates the @ file picker candidates.
func (m *Model) SetFiles(files []FileCandidate) {
	m.FileIndex = make([]FileCandidate, len(files))
	copy(m.FileIndex, files)
}

// IndexWorkspaceFiles scans workspace via context.Indexer (enforcing .tether/ignore,
// built-in secrets, and symlink containment) to populate @ file candidates.
func (m *Model) IndexWorkspaceFiles(maxFiles int) {
	if maxFiles <= 0 {
		maxFiles = 200
	}
	entries, err := tetherctx.NewIndexer(m.Workspace).Scan()
	if err != nil {
		m.FileIndex = nil
		return
	}
	if len(entries) > maxFiles {
		entries = entries[:maxFiles]
	}
	out := make([]FileCandidate, len(entries))
	for i, e := range entries {
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(e.RelPath)), ".")
		out[i] = FileCandidate{
			RelPath:   e.RelPath,
			Extension: formatExtLabel(ext),
			SizeBytes: e.SizeBytes,
		}
	}
	m.FileIndex = out
}

func formatExtLabel(ext string) string {
	switch ext {
	case "go":
		return "Go"
	case "md":
		return "Markdown"
	case "json":
		return "JSON"
	case "sql":
		return "SQL"
	case "yaml", "yml":
		return "YAML"
	default:
		if ext == "" {
			return "File"
		}
		return strings.ToUpper(ext)
	}
}

func formatByteSize(b int64) string {
	if b < 1024 {
		return fmt.Sprintf("%dB", b)
	}
	return fmt.Sprintf("%.1fKB", float64(b)/1024.0)
}

// OpenApprovalModal triggers the Two-Tiered High-Risk Approval Gate (PRD §3.6).
func (m *Model) OpenApprovalModal(req ApprovalRequest) {
	req.SelectedOption = 0
	m.Approval = &req
}

// IsDropdownOpen reports whether the `/` or `@` floating autocomplete overlay is visible.
func (m Model) IsDropdownOpen() bool {
	return m.DropdownKind != views.AutocompleteNone && len(m.DropdownItems) > 0
}
