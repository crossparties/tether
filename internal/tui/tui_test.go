package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/crossparties/tether/internal/tui/views"
	"github.com/crossparties/tether/internal/ui"
)

func TestLayoutGeometry80x24And120x40(t *testing.T) {
	ui.SetEnabled(true)
	defer ui.ClearOverride()

	geometries := []struct {
		name   string
		width  int
		height int
	}{
		{name: "Compact_36x10", width: 36, height: 10},
		{name: "Small_50x15", width: 50, height: 15},
		{name: "Standard_80x24", width: 80, height: 24},
		{name: "Wide_120x40", width: 120, height: 40},
	}

	for _, g := range geometries {
		t.Run(g.name, func(t *testing.T) {
			m := NewModel(".")
			m = m.Update(KeyMsg{Type: KeyRune, Rune: '/'}) // open dropdown during resize!
			m = m.Update(WindowSizeMsg{Width: g.width, Height: g.height})

			frame := m.View()
			lines := strings.Split(frame, "\n")
			if len(lines) != g.height {
				t.Fatalf("expected %d lines for %s, got %d", g.height, g.name, len(lines))
			}
			for i, line := range lines {
				vlen := ui.VisibleLen(line)
				if vlen > g.width {
					t.Errorf("line %d overflowed width %d (VisibleLen=%d): %q", i+1, g.width, vlen, line)
				}
			}
		})
	}
}

func TestSlashAndAtAutocompleteAndTabCompletion(t *testing.T) {
	m := NewModel(".")
	m.SetFiles([]FileCandidate{
		{RelPath: "internal/tui/model.go", Extension: "Go", SizeBytes: 4200},
		{RelPath: "internal/core/engine.go", Extension: "Go", SizeBytes: 18400},
		{RelPath: "README.md", Extension: "Markdown", SizeBytes: 2048},
	})

	// 1. Type "/" -> opens Command Palette
	m = m.Update(KeyMsg{Type: KeyRune, Rune: '/'})
	if !m.IsDropdownOpen() || m.DropdownKind != views.AutocompleteCommand {
		t.Fatalf("expected '/' to open Command Palette dropdown")
	}

	// Type "b" -> filters to "/btw"
	m = m.Update(KeyMsg{Type: KeyRune, Rune: 'b'})
	if len(m.DropdownItems) != 1 || m.DropdownItems[0].Token != "/btw" {
		t.Fatalf("expected '/b' to filter to '/btw', got %+v", m.DropdownItems)
	}

	// Press Tab -> auto-completes to "/btw " (with trailing space because NeedsArg=true)
	m = m.Update(KeyMsg{Type: KeyTab})
	if m.InputBuffer != "/btw " {
		t.Fatalf("expected Tab to complete '/btw ', got %q", m.InputBuffer)
	}
	if m.IsDropdownOpen() {
		t.Fatalf("expected dropdown to close after Tab completion")
	}

	// 2. Type "@mod" -> opens File Picker filtered to "internal/tui/model.go"
	for _, r := range "@mod" {
		m = m.Update(KeyMsg{Type: KeyRune, Rune: r})
	}
	if !m.IsDropdownOpen() || m.DropdownKind != views.AutocompleteFile {
		t.Fatalf("expected '@mod' to open File Picker dropdown")
	}
	if len(m.DropdownItems) != 1 || m.DropdownItems[0].Token != "@internal/tui/model.go" {
		t.Fatalf("expected '@internal/tui/model.go', got %+v", m.DropdownItems)
	}

	// Press Tab -> replaces "@mod" with "@internal/tui/model.go "
	m = m.Update(KeyMsg{Type: KeyTab})
	expected := "/btw @internal/tui/model.go "
	if m.InputBuffer != expected {
		t.Fatalf("expected %q after Tab completion, got %q", expected, m.InputBuffer)
	}
}

func TestArrowKeyResolutionAndDraftBufferPreservation(t *testing.T) {
	m := NewModel(".")

	// Submit two prompts into History
	for _, prompt := range []string{"first prompt", "second prompt"} {
		m.InputBuffer = prompt
		m.CursorPos = len([]rune(prompt))
		m = m.Update(KeyMsg{Type: KeyEnter})
	}

	// Case A: When '/' dropdown is open, Up/Down navigates dropdown items, NOT prompt history
	m = m.Update(KeyMsg{Type: KeyRune, Rune: '/'})
	if !m.IsDropdownOpen() {
		t.Fatalf("expected dropdown open")
	}
	m = m.Update(KeyMsg{Type: KeyDown})
	if m.DropdownIndex != 1 {
		t.Fatalf("expected Down arrow to move DropdownIndex to 1, got %d", m.DropdownIndex)
	}
	if m.InputBuffer != "/" {
		t.Fatalf("expected InputBuffer to remain '/', got %q", m.InputBuffer)
	}

	// Close dropdown and type an unsent draft
	m.InputBuffer = ""
	m.CursorPos = 0
	m.closeDropdown()
	for _, r := range "unsent draft text" {
		m = m.Update(KeyMsg{Type: KeyRune, Rune: r})
	}

	// Case B: Dropdown is closed -> Up arrow recalls "second prompt" while preserving DraftBuffer
	m = m.Update(KeyMsg{Type: KeyUp})
	if m.InputBuffer != "second prompt" {
		t.Fatalf("expected 'second prompt', got %q", m.InputBuffer)
	}
	if m.DraftBuffer != "unsent draft text" {
		t.Fatalf("expected DraftBuffer 'unsent draft text' to be preserved, got %q", m.DraftBuffer)
	}
	if m.CursorPos != len([]rune("second prompt")) {
		t.Fatalf("expected cursor at EOL (%d), got %d", len([]rune("second prompt")), m.CursorPos)
	}

	// Up arrow again -> recalls "first prompt"
	m = m.Update(KeyMsg{Type: KeyUp})
	if m.InputBuffer != "first prompt" {
		t.Fatalf("expected 'first prompt', got %q", m.InputBuffer)
	}

	// Down arrow twice -> restores "unsent draft text" with cursor at EOL
	m = m.Update(KeyMsg{Type: KeyDown})
	m = m.Update(KeyMsg{Type: KeyDown})
	if m.InputBuffer != "unsent draft text" {
		t.Fatalf("expected DraftBuffer 'unsent draft text' restored, got %q", m.InputBuffer)
	}
	if m.CursorPos != len([]rune("unsent draft text")) {
		t.Fatalf("expected cursor at EOL (%d), got %d", len([]rune("unsent draft text")), m.CursorPos)
	}
}

func TestTwoTieredApprovalModalNavigationAndCancel(t *testing.T) {
	m := NewModel(".")
	cancelledCalls := 0
	m.cancelAllFn = func() int {
		cancelledCalls++
		return 2
	}

	m.OpenApprovalModal(ApprovalRequest{
		ProposalID:     42,
		AgentName:      "Worker-2 (Refactor)",
		ActionType:     "Modifikasi File Existing (WRITE)",
		Target:         "src/auth/jwt.go",
		MainEvaluation: "Perubahan aman dan sesuai kontrak.",
		DiffLines: []string{
			"- token.Expire = 3600",
			"+ token.Expire = cfg.TokenTTL",
		},
	})

	// Navigate across all 6 options using Down arrow
	for expectedIdx := 1; expectedIdx < len(views.ApprovalOptionLabels); expectedIdx++ {
		m = m.Update(KeyMsg{Type: KeyDown})
		if m.Approval.SelectedOption != expectedIdx {
			t.Fatalf("expected option %d, got %d", expectedIdx, m.Approval.SelectedOption)
		}
	}

	// Select ApprovalViewFullDiff and press Enter -> opens Inspector
	m.Approval.SelectedOption = int(views.ApprovalViewFullDiff)
	m = m.Update(KeyMsg{Type: KeyEnter})
	if !m.Inspector.Active {
		t.Fatalf("expected [Lihat Diff Lengkap] to open scrollable Inspector")
	}
	// Close Inspector with Esc -> returns to Approval Modal
	m = m.Update(KeyMsg{Type: KeyEsc})
	if m.Inspector.Active || m.Approval == nil {
		t.Fatalf("expected Esc in Inspector to return to Approval Modal")
	}

	// Select ApprovalAbortTask and press Enter -> cancels workers
	m.Approval.SelectedOption = int(views.ApprovalAbortTask)
	m = m.Update(KeyMsg{Type: KeyEnter})
	if m.Approval != nil {
		t.Fatalf("expected Approval Modal to close after aborting task")
	}
	if cancelledCalls != 1 {
		t.Fatalf("expected CancelAll callback to be invoked once, got %d", cancelledCalls)
	}
}

func TestMemoryBudgetUnder40MB(t *testing.T) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	m := NewModel(".")
	_ = m.View()

	runtime.ReadMemStats(&after)
	allocMB := float64(after.Alloc) / (1024.0 * 1024.0)
	if allocMB >= 40.0 {
		t.Fatalf("idle memory budget exceeded: %.2f MB >= 40 MB", allocMB)
	}
}

func TestEndToEndEditDiscussReadScrollAndEsc(t *testing.T) {
	ui.SetEnabled(true)
	defer ui.ClearOverride()

	tmpDir := t.TempDir()
	m := NewModel(tmpDir)
	m = m.Update(WindowSizeMsg{Width: 100, Height: 30})

	// 1. Submit a Main Agent prompt
	for _, r := range "analisa struktur kode" {
		m = m.Update(KeyMsg{Type: KeyRune, Rune: r})
	}
	m = m.Update(KeyMsg{Type: KeyEnter})
	if m.PendingPrompt != "analisa struktur kode" {
		t.Fatalf("expected PendingPrompt, got %q", m.PendingPrompt)
	}
	if m.Workers[0].State != WorkerActive {
		t.Fatalf("expected Main Agent worker to be WorkerActive, got %v", m.Workers[0].State)
	}

	// Verify rotating Braille spinner advances on TickMsg and renders in Zone B
	m = m.Update(TickMsg{Now: time.Now().Add(400 * time.Millisecond)})
	viewWithSpinner := m.View()
	if !strings.Contains(viewWithSpinner, "⠙") {
		t.Fatalf("expected rotating spinner '⠙' in view:\n%s", viewWithSpinner)
	}

	// 2. Non-blocking discussion via `/btw` while Main Agent is still running in background
	for _, r := range "/btw apakah side chat memblokir input?" {
		m = m.Update(KeyMsg{Type: KeyRune, Rune: r})
	}
	m = m.Update(KeyMsg{Type: KeyEnter})
	m = m.Update(SideChatDoneMsg{
		Question: "apakah side chat memblokir input?",
		Answer:   "Tidak, /btw berjalan terisolasi tanpa mengganggu task utama.",
		Elapsed:  250 * time.Millisecond,
	})
	if m.Workers[0].State != WorkerActive {
		t.Fatalf("expected Main Agent to remain WorkerActive during /btw discussion")
	}

	// Deliver Main Agent completion report
	m = m.Update(AgentTurnDoneMsg{
		WorkerID: "main",
		Prompt:   "analisa struktur kode",
		Answer:   "Analisa selesai: arsitektur 3-Zona valid.",
		Elapsed:  1200 * time.Millisecond,
	})
	if m.Workers[0].State != WorkerDone {
		t.Fatalf("expected Main Agent to transition to WorkerDone, got %v", m.Workers[0].State)
	}

	// 3. Edit File -> High-Risk Approval Gate Modal -> View Full Diff -> Esc -> Approve
	replyCh := make(chan int, 1)
	m = m.Update(ApprovalGateMsg{
		Request: ApprovalRequest{
			ProposalID:     7,
			AgentName:      "Main Agent",
			ActionType:     "edit_file",
			Target:         "internal/tui/view.go",
			MainEvaluation: "Perubahan UI aman untuk diterapkan.",
			DiffLines:      []string{"- oldSpinner", "+ newRotatingSpinner"},
		},
		ReplyCh: replyCh,
	})
	if m.Approval == nil {
		t.Fatalf("expected Approval Modal to open for edit_file")
	}
	// Navigate down to View full diff (ApprovalViewFullDiff) and press Enter -> opens Inspector
	for i := 0; i < int(views.ApprovalViewFullDiff); i++ {
		m = m.Update(KeyMsg{Type: KeyDown})
	}
	m = m.Update(KeyMsg{Type: KeyEnter})
	if !m.Inspector.Active {
		t.Fatalf("expected Inspector to open when viewing full diff")
	}
	// Press Esc to close Inspector and return to Approval Modal
	m = m.Update(KeyMsg{Type: KeyEsc})
	if m.Inspector.Active || m.Approval == nil {
		t.Fatalf("expected Esc to return from Inspector to Approval Modal")
	}
	// Move back up to ApprovalApproveAndRun (index 0) and press Enter -> resolves channel
	for i := 0; i < int(views.ApprovalViewFullDiff); i++ {
		m = m.Update(KeyMsg{Type: KeyUp})
	}
	m = m.Update(KeyMsg{Type: KeyEnter})
	select {
	case verdict := <-replyCh:
		if verdict != int(views.ApprovalApproveAndRun) {
			t.Fatalf("expected ApprovalApproveAndRun (0), got %d", verdict)
		}
	default:
		t.Fatalf("expected approval verdict on replyCh")
	}

	// 4. Test `/read` File Inspector + Mouse Wheel Parsing (`\x1b[<64;...M` & `\x1b[<65;...M`) + Esc
	upWheel, okUp := ParseKeyBytes([]byte("\x1b[<64;20;10M"))
	downWheel, okDown := ParseKeyBytes([]byte("\x1b[<65;20;10M"))
	if !okUp || upWheel.Type != KeyWheelUp || !okDown || downWheel.Type != KeyWheelDown {
		t.Fatalf("expected SGR mouse wheel sequences to parse into KeyWheelUp/KeyWheelDown, got %+v / %+v", upWheel, downWheel)
	}
	m = m.Update(upWheel)
	if m.StreamScroll != 3 {
		t.Fatalf("expected mouse wheel up to scroll stream by 3 without touching history, got %d", m.StreamScroll)
	}
	m = m.Update(downWheel)
	if m.StreamScroll != 0 {
		t.Fatalf("expected mouse wheel down to return StreamScroll to 0, got %d", m.StreamScroll)
	}

	// 5. Test `Esc` (KeyEsc) cancellation of active workers
	cancelled := 0
	m.cancelAllFn = func() int {
		cancelled++
		return 1
	}
	m.Workers[0].State = WorkerActive
	m = m.Update(KeyMsg{Type: KeyEsc})
	if cancelled != 1 || m.Workers[0].State != WorkerCancelled {
		t.Fatalf("expected Esc to cancel active worker (cancelled=%d, state=%v)", cancelled, m.Workers[0].State)
	}
	if m.Quitting {
		t.Fatalf("expected Esc to cancel workers without quitting the TUI")
	}
}

func BenchmarkTUIRender(b *testing.B) {
	ui.SetEnabled(true)
	defer ui.ClearOverride()

	m := NewModel(".")
	m = m.Update(WindowSizeMsg{Width: 120, Height: 40})
	m.SetFiles([]FileCandidate{
		{RelPath: "internal/tui/model.go", Extension: "Go", SizeBytes: 4200},
		{RelPath: "internal/tui/update.go", Extension: "Go", SizeBytes: 6100},
		{RelPath: "internal/tui/view.go", Extension: "Go", SizeBytes: 5400},
	})
	m = m.Update(KeyMsg{Type: KeyRune, Rune: '/'})

	start := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
	b.StopTimer()

	avg := time.Since(start) / time.Duration(b.N)
	if avg >= 16*time.Millisecond {
		b.Fatalf("frame render time %v exceeded 16ms (<60 FPS) budget", avg)
	}
}

func TestCopySelectModeAndSeparators(t *testing.T) {
	ui.SetEnabled(true)
	defer ui.ClearOverride()

	if km, ok := ParseKeyBytes([]byte{0x19}); !ok || km.Type != KeyToggleMouse {
		t.Fatalf("Ctrl+Y must parse to KeyToggleMouse, got %+v", km)
	}

	m := NewModel(".")
	if !m.MouseCapture {
		t.Fatalf("mouse capture should default to ON (wheel scroll)")
	}
	m = m.Update(KeyMsg{Type: KeyToggleMouse})
	if m.MouseCapture {
		t.Fatalf("Ctrl+Y should switch to native select mode")
	}

	m = m.Update(AgentTurnDoneMsg{WorkerID: "main", Answer: "hello\n```go\nfmt.Println(1)\n```", Elapsed: time.Second})
	for _, r := range "/copy code" {
		m = m.Update(KeyMsg{Type: KeyRune, Rune: r})
	}
	m = m.Update(KeyMsg{Type: KeyEnter})
	if m.PendingCopy != "fmt.Println(1)" {
		t.Fatalf("expected /copy code to queue the fenced code, got %q", m.PendingCopy)
	}

	// Dividers adapt to every width and never overflow.
	for _, w := range []int{30, 60, 100, 160} {
		m = m.Update(WindowSizeMsg{Width: w, Height: 30})
		frame := m.View()
		if !strings.Contains(frame, strings.Repeat("─", w)) {
			t.Fatalf("expected full-width divider at width %d", w)
		}
		for i, line := range strings.Split(frame, "\n") {
			if ui.VisibleLen(line) > w {
				t.Fatalf("width %d line %d overflow", w, i)
			}
		}
	}
}

func TestStreamingDeltasBuildLiveBlockAndFinalize(t *testing.T) {
	m := NewModel(".")
	for _, r := range "hello" {
		m = m.Update(KeyMsg{Type: KeyRune, Rune: r})
	}
	m = m.Update(KeyMsg{Type: KeyEnter})
	if m.Workers[0].State != WorkerActive {
		t.Fatalf("worker should be active")
	}
	before := len(m.Stream)
	m = m.Update(AgentDeltaMsg{Kind: "content", Text: "Hel"})
	m = m.Update(AgentDeltaMsg{Kind: "content", Text: "lo\nwor"})
	m = m.Update(AgentDeltaMsg{Kind: "content", Text: "ld"})
	if len(m.Stream) != before+1 {
		t.Fatalf("expected a single live block, got %d new entries", len(m.Stream)-before)
	}
	live := m.Stream[len(m.Stream)-1]
	if strings.Join(live.Body, "|") != "Hello|world" || !strings.Contains(live.Title, "streaming") {
		t.Fatalf("bad live block: %+v", live)
	}
	// A tool step closes the block; the next text opens a fresh one.
	m = m.Update(AgentDeltaMsg{Kind: "endstep"})
	m = m.Update(AgentDeltaMsg{Kind: "content", Text: "final"})
	if len(m.Stream) != before+2 {
		t.Fatalf("expected a second block after endstep")
	}
	m = m.Update(AgentTurnDoneMsg{WorkerID: "main", Answer: "final answer", Elapsed: time.Second})
	if len(m.Stream) != before+2 {
		t.Fatalf("done must finalize the live block, not append a duplicate (got %d)", len(m.Stream)-before)
	}
	last := m.Stream[len(m.Stream)-1]
	if strings.Contains(last.Title, "streaming") || strings.Join(last.Body, "") != "final answer" {
		t.Fatalf("live block not finalized: %+v", last)
	}
	// Late deltas after completion are ignored.
	m = m.Update(AgentDeltaMsg{Kind: "content", Text: "ghost"})
	if len(m.Stream) != before+2 {
		t.Fatalf("late delta must be ignored")
	}
}

func TestLiveFadedReasoningAndThoughtCollapse(t *testing.T) {
	ui.SetEnabled(true)
	defer ui.ClearOverride()

	m := NewModel(".")
	m = m.Update(WindowSizeMsg{Width: 100, Height: 28})

	for _, r := range "refactor selection UI" {
		m = m.Update(KeyMsg{Type: KeyRune, Rune: r})
	}
	m = m.Update(KeyMsg{Type: KeyEnter})

	// Initial faded status appears immediately before first token arrives
	initialView := m.View()
	if !strings.Contains(StripANSI(initialView), "Thinking about your request") {
		t.Fatalf("expected initial faded activity text in view:\n%s", StripANSI(initialView))
	}

	// Stream live reasoning deltas -> faded text updates live with ANSI faint SGR (\033[2;38;2;)
	m = m.Update(AgentDeltaMsg{Kind: "reasoning", Text: "First checking current approval layout.\n"})
	m.ReasoningStarted = time.Now().Add(-3 * time.Second)
	m = m.Update(AgentDeltaMsg{Kind: "reasoning", Text: "I'm inspecting how selection colors update inline"})

	thinkingView := m.View()
	if !strings.Contains(StripANSI(thinkingView), "I'm inspecting how selection colors update inline") {
		t.Fatalf("expected live reasoning preview in view:\n%s", StripANSI(thinkingView))
	}
	if !strings.Contains(thinkingView, "\033[2;38;2;") {
		t.Fatalf("expected ANSI faint/dimmed SGR sequence for live reasoning preview")
	}

	// Transition to content -> flushes reasoning into "▸ Thought for 3s" before the answer
	m = m.Update(AgentDeltaMsg{Kind: "content", Text: "Updated inline selection."})
	m = m.Update(AgentTurnDoneMsg{WorkerID: "main", Answer: "Updated inline selection.", Elapsed: 3200 * time.Millisecond})

	donePlain := StripANSI(m.View())
	if !strings.Contains(donePlain, "▸ Thought for ") {
		t.Fatalf("expected collapsed '▸ Thought for Xs' line after reasoning finishes:\n%s", donePlain)
	}
	if !strings.Contains(donePlain, "First checking current approval layout.") {
		t.Fatalf("expected 1-line faded summary under Thought header:\n%s", donePlain)
	}
	if !strings.Contains(donePlain, "Updated inline selection.") {
		t.Fatalf("expected final answer in view:\n%s", donePlain)
	}
}

func TestBoxlessInlineSelectionHighlightAndCollapse(t *testing.T) {
	ui.SetEnabled(true)
	defer ui.ClearOverride()

	m := NewModel(".")
	m = m.Update(WindowSizeMsg{Width: 100, Height: 28})

	// 1. Check borderless `/` command picker highlights active option with `> ` + accent blue
	m = m.Update(KeyMsg{Type: KeyRune, Rune: '/'})
	pickerView := m.View()
	for _, boxChar := range []string{"╭", "╮", "╰", "╯", "╔", "╗", "╚", "╝", "║"} {
		if strings.Contains(StripANSI(pickerView), boxChar) {
			t.Fatalf("expected simple boxless layout without %q, got:\n%s", boxChar, StripANSI(pickerView))
		}
	}
	if !strings.Contains(StripANSI(pickerView), "> /btw") {
		t.Fatalf("expected active selection '> /btw' in command picker:\n%s", StripANSI(pickerView))
	}
	m = m.Update(KeyMsg{Type: KeyEsc})

	// 2. Check inline Approval prompt changes selection highlight via `> ` + color and collapses on confirm
	replyCh := make(chan int, 1)
	m = m.Update(ApprovalGateMsg{
		Request: ApprovalRequest{
			ActionType:     "edit_file",
			Target:         "internal/tui/view.go",
			MainEvaluation: "Safe UI update",
			DiffLines:      []string{"- oldBox", "+ inlineClean"},
		},
		ReplyCh: replyCh,
	})

	viewOpt0 := m.View()
	plainOpt0 := StripANSI(viewOpt0)
	if !strings.Contains(plainOpt0, "> 1. Yes, approve once") || !strings.Contains(plainOpt0, "  2. Yes, always allow in this session") {
		t.Fatalf("expected option 1 selected with '> ' and option 2 unselected:\n%s", plainOpt0)
	}
	// Verify bold blue (#89B4FA -> 137;180;250) on active selection
	if !strings.Contains(viewOpt0, "\033[1;38;2;137;180;250m> 1. Yes, approve once") {
		t.Fatalf("expected active option to highlight in bold accent blue (#89B4FA)")
	}

	// Move down -> selection moves to option 2
	m = m.Update(KeyMsg{Type: KeyDown})
	plainOpt1 := StripANSI(m.View())
	if !strings.Contains(plainOpt1, "  1. Yes, approve once") || !strings.Contains(plainOpt1, "> 2. Yes, always allow in this session") {
		t.Fatalf("expected selection to move to option 2:\n%s", plainOpt1)
	}

	// Press 'y' shortcut -> approves and collapses into inline `? edit_file · internal/tui/view.go` + `> Yes, approve once`
	m = m.Update(KeyMsg{Type: KeyRune, Rune: 'y'})
	if verdict := <-replyCh; verdict != int(views.ApprovalApproveAndRun) {
		t.Fatalf("expected 'y' to approve (0), got %d", verdict)
	}
	collapsed := StripANSI(m.View())
	if !strings.Contains(collapsed, "? edit_file · internal/tui/view.go") || !strings.Contains(collapsed, "> Yes, approve once") {
		t.Fatalf("expected inline collapsed confirmation in stream:\n%s", collapsed)
	}
}

func TestApprovalScopeHintAndStandingShortcuts(t *testing.T) {
	ui.SetEnabled(false)
	defer ui.ClearOverride()

	m := NewModel(".")
	m = m.Update(WindowSizeMsg{Width: 100, Height: 28})

	replyCh := make(chan int, 1)
	m = m.Update(ApprovalGateMsg{
		Request: ApprovalRequest{
			ActionType:     "run_command",
			Target:         "rm -rf build/",
			ScopeHint:      `commands starting with "rm"`,
			MainEvaluation: "Eksekusi perintah shell memerlukan persetujuan",
		},
		ReplyCh: replyCh,
	})

	plain := m.View()
	if !strings.Contains(plain, `2. Yes, always allow commands starting with "rm"`) {
		t.Fatalf("expected dynamic ScopeHint on option 2:\n%s", plain)
	}
	// Press 'a' -> standing folder/prefix approval
	m = m.Update(KeyMsg{Type: KeyRune, Rune: 'a'})
	if verdict := <-replyCh; verdict != int(views.ApprovalApproveFolder) {
		t.Fatalf("expected 'a' to trigger ApprovalApproveFolder (%d), got %d", views.ApprovalApproveFolder, verdict)
	}

	// Another modal: press 's' -> session-wide approval
	replyCh2 := make(chan int, 1)
	m = m.Update(ApprovalGateMsg{
		Request: ApprovalRequest{
			ActionType: "write_file",
			Target:     "go.mod",
		},
		ReplyCh: replyCh2,
	})
	m = m.Update(KeyMsg{Type: KeyRune, Rune: 's'})
	if verdict := <-replyCh2; verdict != int(views.ApprovalApproveSession) {
		t.Fatalf("expected 's' to trigger ApprovalApproveSession (%d), got %d", views.ApprovalApproveSession, verdict)
	}
}

func TestCompactToolLogsInStream(t *testing.T) {
	ui.SetEnabled(false)
	defer ui.ClearOverride()

	m := NewModel(".")
	m = m.Update(WindowSizeMsg{Width: 100, Height: 26})
	m = m.Update(AgentStreamLogMsg{WorkerID: "Main Agent", Line: "🔧 [read_file] internal/tui/view.go (lines 1-60)"})
	m = m.Update(AgentStreamLogMsg{WorkerID: "Main Agent", Line: "🔧 [run_command] $ go test ./..."})

	plain := m.View()
	if !strings.Contains(plain, "● Read (internal/tui/view.go (lines 1-60))") {
		t.Fatalf("expected compact '● Read (...)' row:\n%s", plain)
	}
	if !strings.Contains(plain, "● Ran (go test ./...)") {
		t.Fatalf("expected compact '● Ran (...)' row:\n%s", plain)
	}
}

func TestIndexWorkspaceFilesExcludesSubstrateDBs(t *testing.T) {
	ws := t.TempDir()
	for _, rel := range []string{
		"cmd/tether/main.go",
		".tether/session.db",
		".tether/cache.db",
		".tether/PLAN.md",
		".tether/ignore",
		".env",
		"secret.ignored",
		"session.db",
		"session.db-wal",
		"cache.db",
		"cache.db-wal",
	} {
		full := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		body := "x"
		if rel == ".tether/ignore" {
			body = "*.ignored\n"
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	_ = os.Symlink(filepath.Join(t.TempDir(), "escape.go"), filepath.Join(ws, "escape_link.go"))

	m := NewModel(ws)
	m.IndexWorkspaceFiles(50)

	got := make(map[string]bool, len(m.FileIndex))
	for _, c := range m.FileIndex {
		got[c.RelPath] = true
	}
	if !got["cmd/tether/main.go"] {
		t.Fatalf("expected cmd/tether/main.go to be indexed, got %v", m.FileIndex)
	}
	for _, excluded := range []string{
		".tether/session.db",
		".tether/cache.db",
		".tether/PLAN.md",
		".tether/ignore",
		".env",
		"secret.ignored",
		"escape_link.go",
		"session.db",
		"session.db-wal",
		"cache.db",
		"cache.db-wal",
	} {
		if got[excluded] {
			t.Errorf("expected %q to be excluded from @ file index, got %v", excluded, m.FileIndex)
		}
	}
}

func TestStreamRetryResetDiscardsPartialBufferAndKeysSwitchedUpdatesModel(t *testing.T) {
	prov := "deepseek"
	mod := "deepseek-chat"
	m := NewModel(".")
	m.ProviderName = prov
	m.ModelName = mod
	m.Bridge = &AgentBridge{
		Provider: func() string { return prov },
		Model:    func() string { return mod },
	}
	m = m.Update(WindowSizeMsg{Width: 100, Height: 28})
	for _, r := range "check failover" {
		m = m.Update(KeyMsg{Type: KeyRune, Rune: r})
	}
	m = m.Update(KeyMsg{Type: KeyEnter})

	// Simulate reasoning -> content -> stream.retry_reset (must roll back BOTH flushed Thought and partial content).
	m = m.Update(AgentDeltaMsg{Kind: "reasoning", Text: "aborted thought"})
	if m.ReasoningBuf == "" {
		t.Fatalf("expected active reasoning buffer")
	}
	m = m.Update(AgentDeltaMsg{Kind: "content", Text: "stale partial"})
	if m.liveStream == 0 || m.liveThought == 0 {
		t.Fatalf("expected both active liveStream and flushed liveThought, got stream=%d thought=%d", m.liveStream, m.liveThought)
	}

	// stream.retry_reset must discard BOTH the flushed Thought entry and partial content block.
	m = m.Update(AgentDeltaMsg{Kind: "stream.retry_reset", Text: "idle timeout"})
	if m.liveStream != 0 || m.liveThought != 0 || m.ReasoningBuf != "" {
		t.Fatalf("expected liveStream=0, liveThought=0, empty ReasoningBuf after stream.retry_reset, got stream=%d thought=%d buf=%q", m.liveStream, m.liveThought, m.ReasoningBuf)
	}
	for _, entry := range m.Stream {
		joined := strings.Join(entry.Body, "\n")
		if strings.Contains(joined, "stale partial") || strings.Contains(joined, "aborted thought") {
			t.Fatalf("aborted stream thought/content should have been rolled back on retry reset: %+v", entry)
		}
	}

	// keys.switched updates active provider/model and appends failover notice.
	prov = "openai"
	mod = "gpt-4o"
	m = m.Update(AgentDeltaMsg{Kind: "keys.switched", Text: "deepseek:ds-1 -> openai:oai-1"})
	if m.ProviderName != "openai" || m.ModelName != "gpt-4o" {
		t.Fatalf("expected provider/model refreshed to openai/gpt-4o, got %s/%s", m.ProviderName, m.ModelName)
	}
	m = m.Update(AgentDeltaMsg{Kind: "content", Text: "clean retry answer"})
	m = m.Update(AgentTurnDoneMsg{WorkerID: "main", Answer: "clean retry answer", Elapsed: time.Second})
	last := m.Stream[len(m.Stream)-1]
	if strings.Join(last.Body, "") != "clean retry answer" {
		t.Fatalf("expected clean retry answer in final block, got %+v", last)
	}
}

func TestTUIKeyListProbeAndErrorSurfacingDoNotSaveLiterals(t *testing.T) {
	var savedKeys []string
	var saveErr error

	m := NewModel(".")
	m.AwaitingKey = true
	m.Bridge = &AgentBridge{
		SaveAPIKey: func(k string) error {
			savedKeys = append(savedKeys, k)
			return saveErr
		},
		ListKeys:  func() string { return " * ds-1 provider=deepseek prio=1 status=ok" },
		ProbeKeys: func(_ context.Context) string { return " ✔ ds-1 (deepseek) status=200" },
	}

	// 1. /key list must NOT call SaveAPIKey("list").
	m.InputBuffer = "/key list"
	m = m.submitInput()
	if len(savedKeys) != 0 {
		t.Fatalf("/key list erroneously saved key: %v", savedKeys)
	}
	if !strings.Contains(strings.Join(m.Stream[len(m.Stream)-1].Body, "\n"), "ds-1") {
		t.Fatalf("expected /key list output in stream, got: %+v", m.Stream[len(m.Stream)-1])
	}

	// 2. /key probe must NOT call SaveAPIKey("probe").
	m.InputBuffer = "/key probe"
	m = m.submitInput()
	if len(savedKeys) != 0 {
		t.Fatalf("/key probe erroneously saved key: %v", savedKeys)
	}
	if !strings.Contains(strings.Join(m.Stream[len(m.Stream)-1].Body, "\n"), "status=200") {
		t.Fatalf("expected /key probe output in stream, got: %+v", m.Stream[len(m.Stream)-1])
	}

	// 3. SaveAPIKey error must surface visibly and preserve AwaitingKey=true.
	saveErr = fmt.Errorf("disk permission denied")
	m.InputBuffer = "/key sk-bad"
	m = m.submitInput()
	if !m.AwaitingKey || !strings.Contains(m.Stream[len(m.Stream)-1].Title, "API Key Error") {
		t.Fatalf("expected visible SaveAPIKey error and AwaitingKey preserved, got awaiting=%v entry=%+v", m.AwaitingKey, m.Stream[len(m.Stream)-1])
	}
}

func TestAsyncCompactQueuesWithoutBlockingUIThread(t *testing.T) {
	syncCalls := 0
	m := NewModel(".")
	m.Bridge = &AgentBridge{
		Compact: func(_ context.Context) (string, error) {
			syncCalls++
			return "Condensed 12 older events.", nil
		},
	}

	m.InputBuffer = "/compact"
	m = m.submitInput()
	if !m.PendingCompact {
		t.Fatalf("expected /compact to set PendingCompact=true")
	}
	if syncCalls != 0 {
		t.Fatalf("expected /compact Update to NOT block synchronously calling Bridge.Compact, calls=%d", syncCalls)
	}

	m = m.Update(CompactDoneMsg{
		Result:  "Condensed 12 older events.",
		Elapsed: 250 * time.Millisecond,
	})
	last := m.Stream[len(m.Stream)-1]
	if !strings.Contains(last.Title, "Episodic condensation") || !strings.Contains(strings.Join(last.Body, "\n"), "Condensed 12 older events") {
		t.Fatalf("unexpected CompactDoneMsg stream entry: %+v", last)
	}
}
