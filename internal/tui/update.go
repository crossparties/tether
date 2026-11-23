package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crossparties/tether/internal/tui/views"
)

var brailleFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Update advances the deterministic TUI state machine in response to an event message.
// It implements the exact key resolution priority matrix specified in PRD §3.3.
func (m Model) Update(msg any) Model {
	switch ev := msg.(type) {
	case WindowSizeMsg:
		if ev.Width > 0 {
			m.Width = ev.Width
		}
		if ev.Height > 0 {
			m.Height = ev.Height
		}
		return m

	case TickMsg:
		m.SpinnerStep = (m.SpinnerStep + 1) % len(brailleFrames)
		now := ev.Now
		if now.IsZero() {
			now = time.Now()
		}
		m = m.tickSubagents(now)
		for i := range m.Workers {
			if m.Workers[i].State == WorkerActive {
				m.Workers[i].Spinner = brailleFrames[m.SpinnerStep]
				if !m.Workers[i].StartedAt.IsZero() {
					elapsed := now.Sub(m.Workers[i].StartedAt)
					m.Workers[i].Detail = fmt.Sprintf("%.1fs", elapsed.Seconds())
				}
			}
		}
		return m

	case AgentStreamLogMsg:
		if strings.TrimSpace(ev.Line) == "" {
			return m
		}
		if ev.WorkerID == "Main Agent" && m.ReasoningBuf != "" {
			m.flushReasoning(time.Now())
		}
		m.ingestStreamLog(ev.WorkerID, ev.Line)
		m.StreamScroll = 0
		return m

	case SubagentEventMsg:
		return m.handleSubagentEvent(ev)

	case AgentDeltaMsg:
		// Ignore late deltas that arrive after Esc/Ctrl+C or completion.
		if len(m.Workers) == 0 || (m.Workers[0].State != WorkerActive && m.Workers[0].State != WorkerWait) {
			return m
		}
		switch ev.Kind {
		case "reasoning":
			if m.ReasoningBuf == "" {
				m.ReasoningStarted = time.Now()
			}
			m.ReasoningBuf += ev.Text
			if preview := latestReasoningPreview(m.ReasoningBuf); preview != "" {
				m.LiveStatus = preview
			} else {
				m.LiveStatus = "Thinking…"
			}
			m.Workers[0].Task = "Thinking…"
		case "endstep":
			// Reasoning & intermediate prose end when tool calls begin; flush thought block.
			m.flushReasoning(time.Now())
			m.liveStream = 0
			m.LiveStatus = "Running tools…"
			m.Workers[0].Task = "Running tools…"
		case "content":
			m.flushReasoning(time.Now())
			if m.liveStream == 0 {
				m.Stream = append(m.Stream, StreamEntry{
					Kind:  StreamMainAgent,
					Title: "streaming…",
					Body:  []string{""},
				})
				m.liveStream = len(m.Stream)
				m.Workers[0].Task = "Writing answer…"
			}
			m.LiveStatus = "Writing response…"
			e := &m.Stream[m.liveStream-1]
			parts := strings.Split(ev.Text, "\n")
			e.Body[len(e.Body)-1] += parts[0]
			e.Body = append(e.Body, parts[1:]...)
		}
		return m

	case AgentTurnDoneMsg:
		if ev.WorkerID == "main" {
			m.flushReasoning(time.Now())
			m.LiveStatus = ""
		}
		// Finalize the live streamed block instead of appending a duplicate answer.
		if ev.WorkerID == "main" && m.liveStream > 0 {
			idx := m.liveStream - 1
			m.liveStream = 0
			if idx < len(m.Stream) {
				e := &m.Stream[idx]
				e.Title = fmt.Sprintf("%s · %.1fs", m.ModelName, ev.Elapsed.Seconds())
				if ans := strings.TrimSpace(ev.Answer); ev.Err == nil && ans != "" {
					e.Body = strings.Split(ans, "\n")
				}
				for i := range m.Workers {
					if m.Workers[i].ID == "main" || m.Workers[i].State == WorkerActive {
						m.Workers[i].State = WorkerDone
						m.Workers[i].Spinner = "✓"
						m.Workers[i].Task = "Ready"
						m.Workers[i].Detail = fmt.Sprintf("%.1fs", ev.Elapsed.Seconds())
					}
				}
				if ev.Err != nil {
					m.Stream = append(m.Stream, StreamEntry{
						Kind:  StreamSystemNotice,
						Title: fmt.Sprintf("✖ Error · %.1fs", ev.Elapsed.Seconds()),
						Body:  strings.Split(ev.Err.Error(), "\n"),
					})
				}
				m.StreamScroll = 0
				return m
			}
		}
		for i := range m.Workers {
			if m.Workers[i].ID == ev.WorkerID || (ev.WorkerID == "main" && m.Workers[i].State == WorkerActive) {
				m.Workers[i].State = WorkerDone
				m.Workers[i].Spinner = "✓"
				m.Workers[i].Task = "Ready"
				m.Workers[i].Detail = fmt.Sprintf("%.1fs", ev.Elapsed.Seconds())
			}
		}
		if ev.Err != nil {
			m.Stream = append(m.Stream, StreamEntry{
				Kind:  StreamSystemNotice,
				Title: fmt.Sprintf("✖ Error · %.1fs", ev.Elapsed.Seconds()),
				Body:  strings.Split(ev.Err.Error(), "\n"),
			})
		} else if ev.WorkerID == "subagent" {
			ans := strings.TrimSpace(ev.Answer)
			if ans == "" {
				ans = "(sub-agent finished)"
			}
			m.Stream = append(m.Stream, StreamEntry{
				Kind:  StreamSubAgent,
				Title: fmt.Sprintf("Subagent report · %.1fs", ev.Elapsed.Seconds()),
				Body:  strings.Split(ans, "\n"),
			})
		} else {
			ans := strings.TrimSpace(ev.Answer)
			if ans == "" {
				ans = "(completed)"
			}
			m.Stream = append(m.Stream, StreamEntry{
				Kind:  StreamMainAgent,
				Title: fmt.Sprintf("%s · %.1fs", m.ModelName, ev.Elapsed.Seconds()),
				Body:  strings.Split(ans, "\n"),
			})
		}
		m.StreamScroll = 0
		return m

	case SideChatDoneMsg:
		if ev.Err != nil {
			m.Stream = append(m.Stream, StreamEntry{
				Kind:  StreamSideChat,
				Title: "btw · side chat error",
				Body: []string{
					"> " + ev.Question,
					ev.Err.Error(),
				},
			})
		} else {
			body := []string{"> " + ev.Question}
			body = append(body, strings.Split(strings.TrimSpace(ev.Answer), "\n")...)
			m.Stream = append(m.Stream, StreamEntry{
				Kind:  StreamSideChat,
				Title: fmt.Sprintf("btw · isolated side chat · %.1fs", ev.Elapsed.Seconds()),
				Body:  body,
			})
		}
		m.StreamScroll = 0
		return m

	case ApprovalGateMsg:
		req := ev.Request
		req.SelectedOption = 0
		m.Approval = &req
		m.approvalReplyCh = ev.ReplyCh
		for i := range m.Workers {
			if m.Workers[i].State == WorkerActive {
				m.Workers[i].State = WorkerWait
				m.Workers[i].Spinner = "⏸"
				m.Workers[i].Detail = "Awaiting Approval"
			}
		}
		return m

	case KeyMsg:
		return m.handleKey(ev)
	}
	return m
}

func (m Model) handleKey(k KeyMsg) Model {
	// 0. Ctrl+Y: toggle between wheel-scroll (mouse capture) and native drag-select/copy.
	if k.Type == KeyToggleMouse {
		return m.toggleMouseCapture()
	}

	// 1. Highest Priority: Scrollable Inspector Viewport ([View Full Diff] / /read)
	if m.Inspector.Active {
		switch k.Type {
		case KeyEsc:
			m.Inspector.Active = false
			return m
		case KeyRune:
			if k.Rune == 'q' || k.Rune == 'Q' {
				m.Inspector.Active = false
				return m
			}
		case KeyUp, KeyWheelUp:
			step := 1
			if k.Type == KeyWheelUp {
				step = 3
			}
			m.Inspector.ScrollOffset -= step
			if m.Inspector.ScrollOffset < 0 {
				m.Inspector.ScrollOffset = 0
			}
			return m
		case KeyDown, KeyWheelDown:
			step := 1
			if k.Type == KeyWheelDown {
				step = 3
			}
			m.Inspector.ScrollOffset += step
			if m.Inspector.ScrollOffset >= len(m.Inspector.Lines) {
				m.Inspector.ScrollOffset = max(0, len(m.Inspector.Lines)-1)
			}
			return m
		case KeyPgUp:
			m.Inspector.ScrollOffset -= 5
			if m.Inspector.ScrollOffset < 0 {
				m.Inspector.ScrollOffset = 0
			}
			return m
		case KeyPgDn:
			m.Inspector.ScrollOffset += 5
			if m.Inspector.ScrollOffset >= len(m.Inspector.Lines) {
				m.Inspector.ScrollOffset = max(0, len(m.Inspector.Lines)-1)
			}
			return m
		}
		return m
	}

	// 2. Second Priority: Inline Permission / Approval Prompt
	if m.Approval != nil {
		switch k.Type {
		case KeyUp, KeyLeft:
			m.Approval.SelectedOption--
			if m.Approval.SelectedOption < 0 {
				m.Approval.SelectedOption = len(views.ApprovalOptionLabels) - 1
			}
			return m
		case KeyDown, KeyRight, KeyTab:
			m.Approval.SelectedOption = (m.Approval.SelectedOption + 1) % len(views.ApprovalOptionLabels)
			return m
		case KeyEsc:
			return m.confirmApprovalOption(views.ApprovalRejectAction)
		case KeyCtrlC:
			return m.confirmApprovalOption(views.ApprovalAbortTask)
		case KeyRune:
			switch k.Rune {
			case '1', 'y', 'Y':
				return m.confirmApprovalOption(views.ApprovalApproveAndRun)
			case '2', 'a', 'A':
				return m.confirmApprovalOption(views.ApprovalApproveFolder)
			case '3', 's', 'S':
				return m.confirmApprovalOption(views.ApprovalApproveSession)
			case '4', 'd', 'D':
				return m.confirmApprovalOption(views.ApprovalViewFullDiff)
			case '5', 'n', 'N':
				return m.confirmApprovalOption(views.ApprovalRejectAction)
			case '6':
				return m.confirmApprovalOption(views.ApprovalAbortTask)
			}
			return m
		case KeyEnter:
			return m.confirmApprovalOption(views.ApprovalOptionIndex(m.Approval.SelectedOption))
		}
		return m
	}

	// 3. Global Cancellation & Dropdown Dismissal (Esc / Ctrl+C)
	if k.Type == KeyEsc || k.Type == KeyCtrlC {
		if m.IsDropdownOpen() && k.Type == KeyEsc {
			m.closeDropdown()
			return m
		}
		m.cancelActiveWorkers()
		return m
	}

	// 4. Stream Viewport Scrolling via Mouse Wheel, Shift/Ctrl+Arrow, or PgUp / PgDn
	if k.Type == KeyWheelUp {
		m.StreamScroll += 3
		return m
	}
	if k.Type == KeyWheelDown {
		m.StreamScroll -= 3
		if m.StreamScroll < 0 {
			m.StreamScroll = 0
		}
		return m
	}
	if k.Type == KeyPgUp {
		m.StreamScroll += 5
		return m
	}
	if k.Type == KeyPgDn {
		m.StreamScroll -= 5
		if m.StreamScroll < 0 {
			m.StreamScroll = 0
		}
		return m
	}

	// 5. Autocomplete vs Prompt History Navigation (PRD §3.3 Exact Resolution)
	switch k.Type {
	case KeyUp:
		if m.IsDropdownOpen() {
			m.DropdownIndex--
			if m.DropdownIndex < 0 {
				m.DropdownIndex = len(m.DropdownItems) - 1
			}
			return m
		}
		return m.navigateHistoryUp()

	case KeyDown:
		if m.IsDropdownOpen() {
			m.DropdownIndex = (m.DropdownIndex + 1) % len(m.DropdownItems)
			return m
		}
		return m.navigateHistoryDown()

	case KeyTab:
		if m.IsDropdownOpen() {
			return m.applyAutocompleteSelection()
		}
		return m

	case KeyEnter:
		if m.IsDropdownOpen() {
			selected := m.DropdownItems[m.DropdownIndex]
			// If PickerMode ("model" or "provider") is active, Enter immediately applies the picked model/provider!
			if m.PickerMode == "model" {
				modelName := strings.TrimSpace(strings.TrimPrefix(selected.Token, "/model "))
				m.closeDropdown()
				m.InputBuffer = "/model " + modelName
				m.CursorPos = len([]rune(m.InputBuffer))
				return m.submitInput()
			}
			if m.PickerMode == "provider" {
				provName := strings.TrimSpace(strings.TrimPrefix(selected.Token, "/provider "))
				m.closeDropdown()
				m.InputBuffer = "/provider " + provName
				m.CursorPos = len([]rune(m.InputBuffer))
				return m.submitInput()
			}
			if m.DropdownKind == views.AutocompleteFile {
				return m.applyAutocompleteSelection()
			}
			// If input is a slash command prefix without spaces, complete it first
			if m.DropdownKind == views.AutocompleteCommand && m.InputBuffer != selected.Token && !strings.Contains(m.InputBuffer, " ") {
				// Allow `/model` and `/provider` with no args to open their interactive TUI sub-picker on Enter!
				if selected.Token == "/model" || selected.Token == "/provider" {
					m.InputBuffer = selected.Token
					m.CursorPos = len([]rune(m.InputBuffer))
					m.closeDropdown()
					return m.submitInput()
				}
				m = m.applyAutocompleteSelection()
				if selected.NeedsArg {
					return m
				}
			}
		}
		return m.submitInput()

	case KeyBackspace:
		runes := []rune(m.InputBuffer)
		if len(runes) > 0 && m.CursorPos > 0 {
			idx := min(m.CursorPos, len(runes))
			runes = append(runes[:idx-1], runes[idx:]...)
			m.InputBuffer = string(runes)
			m.CursorPos = idx - 1
		}
		if m.HistoryIndex == -1 {
			m.DraftBuffer = m.InputBuffer
		}
		m.recomputeAutocomplete()
		return m

	case KeyLeft:
		if m.CursorPos > 0 {
			m.CursorPos--
		}
		return m

	case KeyRight:
		runes := []rune(m.InputBuffer)
		if m.CursorPos < len(runes) {
			m.CursorPos++
		}
		return m

	case KeySpace:
		return m.insertRune(' ')

	case KeyRune:
		return m.insertRune(k.Rune)
	}

	return m
}

func (m *Model) resolveApprovalGate(optionIdx int) {
	if m.approvalReplyCh != nil {
		select {
		case m.approvalReplyCh <- optionIdx:
		default:
		}
		m.approvalReplyCh = nil
	}
	for i := range m.Workers {
		if m.Workers[i].State == WorkerWait {
			m.Workers[i].State = WorkerActive
			m.Workers[i].Spinner = brailleFrames[m.SpinnerStep%len(brailleFrames)]
		}
	}
}

func (m Model) insertRune(r rune) Model {
	runes := []rune(m.InputBuffer)
	pos := m.CursorPos
	if pos < 0 {
		pos = 0
	}
	if pos > len(runes) {
		pos = len(runes)
	}
	updated := make([]rune, 0, len(runes)+1)
	updated = append(updated, runes[:pos]...)
	updated = append(updated, r)
	updated = append(updated, runes[pos:]...)
	m.InputBuffer = string(updated)
	m.CursorPos = pos + 1

	// When typing while not in history mode (or modifying a recalled item), update DraftBuffer
	if m.HistoryIndex == -1 {
		m.DraftBuffer = m.InputBuffer
	}
	m.recomputeAutocomplete()
	return m
}

// navigateHistoryUp implements PRD §3.3 State 2: recalls older prompts while
// preserving the unsent DraftBuffer and placing the cursor at EOL.
func (m Model) navigateHistoryUp() Model {
	if len(m.History) == 0 {
		return m
	}
	if m.HistoryIndex == -1 {
		m.DraftBuffer = m.InputBuffer
		m.HistoryIndex = len(m.History) - 1
	} else if m.HistoryIndex > 0 {
		m.HistoryIndex--
	}
	m.InputBuffer = m.History[m.HistoryIndex]
	m.CursorPos = len([]rune(m.InputBuffer))
	m.closeDropdown()
	return m
}

// navigateHistoryDown implements PRD §3.3 State 2: moves forward in history or
// restores the unsent DraftBuffer when passing the newest item, with cursor at EOL.
func (m Model) navigateHistoryDown() Model {
	if m.HistoryIndex == -1 {
		return m
	}
	if m.HistoryIndex < len(m.History)-1 {
		m.HistoryIndex++
		m.InputBuffer = m.History[m.HistoryIndex]
	} else {
		m.HistoryIndex = -1
		m.InputBuffer = m.DraftBuffer
	}
	m.CursorPos = len([]rune(m.InputBuffer))
	m.closeDropdown()
	return m
}

func (m *Model) closeDropdown() {
	m.DropdownKind = views.AutocompleteNone
	m.DropdownQuery = ""
	m.DropdownItems = nil
	m.DropdownIndex = 0
	m.PickerMode = ""
}

// recomputeAutocomplete evaluates InputBuffer to trigger `/` Command Palette or `@` File Picker.
func (m *Model) recomputeAutocomplete() {
	m.closeDropdown()
	if m.InputBuffer == "" || m.AwaitingKey {
		return
	}

	// 1. Check for `@` file token at the end or around cursor
	if atStart, atEnd, query, ok := detectAtToken(m.InputBuffer); ok {
		qLower := strings.ToLower(query)
		var items []views.AutocompleteItem
		for _, fc := range m.FileIndex {
			if qLower == "" ||
				strings.Contains(strings.ToLower(fc.RelPath), qLower) ||
				fuzzySubsequence(strings.ToLower(fc.RelPath), qLower) {
				desc := fmt.Sprintf("%s • %s", fc.Extension, formatByteSize(fc.SizeBytes))
				items = append(items, views.AutocompleteItem{
					Token:       "@" + fc.RelPath,
					Description: desc,
					NeedsArg:    true,
				})
			}
		}
		if len(items) > 0 {
			m.DropdownKind = views.AutocompleteFile
			m.DropdownQuery = query
			m.DropdownItems = items
			m.DropdownIndex = 0
			m.ActiveAtTokenByte = [2]int{atStart, atEnd}
			return
		}
	}

	// 2. Interactive `/model ` or `/provider ` argument picker inside the TUI overlay
	if strings.HasPrefix(strings.ToLower(m.InputBuffer), "/model ") && m.Bridge != nil && m.Bridge.ListModels != nil {
		arg := strings.ToLower(strings.TrimSpace(m.InputBuffer[len("/model "):]))
		var items []views.AutocompleteItem
		for _, opt := range m.Bridge.ListModels() {
			if arg == "" || strings.Contains(strings.ToLower(opt.Label), arg) {
				items = append(items, views.AutocompleteItem{
					Token:       "/model " + opt.Label,
					Description: opt.Detail,
					NeedsArg:    false,
				})
			}
		}
		if len(items) > 0 {
			m.DropdownKind = views.AutocompleteCommand
			m.DropdownQuery = arg
			m.DropdownItems = items
			m.DropdownIndex = 0
			m.PickerMode = "model"
			return
		}
	}

	if strings.HasPrefix(strings.ToLower(m.InputBuffer), "/provider ") && m.Bridge != nil && m.Bridge.ListProviders != nil {
		arg := strings.ToLower(strings.TrimSpace(m.InputBuffer[len("/provider "):]))
		var items []views.AutocompleteItem
		for _, opt := range m.Bridge.ListProviders() {
			if arg == "" || strings.Contains(strings.ToLower(opt.Label), arg) {
				items = append(items, views.AutocompleteItem{
					Token:       "/provider " + opt.Label,
					Description: opt.Detail,
					NeedsArg:    false,
				})
			}
		}
		if len(items) > 0 {
			m.DropdownKind = views.AutocompleteCommand
			m.DropdownQuery = arg
			m.DropdownItems = items
			m.DropdownIndex = 0
			m.PickerMode = "provider"
			return
		}
	}

	// 3. Check for `/` command prefix at the start of the input buffer (when no space has been typed yet)
	if strings.HasPrefix(m.InputBuffer, "/") && !strings.Contains(m.InputBuffer, " ") {
		qLower := strings.ToLower(m.InputBuffer)
		var items []views.AutocompleteItem
		for _, sc := range m.SlashCommands {
			if strings.HasPrefix(strings.ToLower(sc.Command), qLower) {
				items = append(items, views.AutocompleteItem{
					Token:       sc.Command,
					Description: sc.Description,
					NeedsArg:    sc.NeedsArg,
				})
			}
		}
		if len(items) > 0 {
			m.DropdownKind = views.AutocompleteCommand
			m.DropdownQuery = strings.TrimPrefix(m.InputBuffer, "/")
			m.DropdownItems = items
			m.DropdownIndex = 0
		}
	}
}

func detectAtToken(input string) (startByte, endByte int, query string, ok bool) {
	lastAt := strings.LastIndexByte(input, '@')
	if lastAt < 0 {
		return 0, 0, "", false
	}
	if lastAt > 0 && input[lastAt-1] != ' ' && input[lastAt-1] != '\t' {
		return 0, 0, "", false
	}
	tail := input[lastAt+1:]
	if strings.ContainsAny(tail, " \t") {
		return 0, 0, "", false
	}
	return lastAt, len(input), tail, true
}

func fuzzySubsequence(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	hr := []rune(haystack)
	nr := []rune(needle)
	ni := 0
	for _, r := range hr {
		if r == nr[ni] {
			ni++
			if ni == len(nr) {
				return true
			}
		}
	}
	return false
}

func (m Model) applyAutocompleteSelection() Model {
	if !m.IsDropdownOpen() {
		return m
	}
	sel := m.DropdownItems[m.DropdownIndex]
	switch m.DropdownKind {
	case views.AutocompleteCommand:
		if sel.NeedsArg {
			m.InputBuffer = sel.Token + " "
		} else {
			m.InputBuffer = sel.Token
		}
	case views.AutocompleteFile:
		start, end := m.ActiveAtTokenByte[0], m.ActiveAtTokenByte[1]
		if start >= 0 && end <= len(m.InputBuffer) && start < end {
			m.InputBuffer = m.InputBuffer[:start] + sel.Token + " " + m.InputBuffer[end:]
		} else {
			m.InputBuffer = sel.Token + " "
		}
	}
	m.CursorPos = len([]rune(m.InputBuffer))
	if m.HistoryIndex == -1 {
		m.DraftBuffer = m.InputBuffer
	}
	m.closeDropdown()
	m.recomputeAutocomplete()
	return m
}

func (m Model) openModelSubPicker() Model {
	var choices []PickerOption
	if m.Bridge != nil && m.Bridge.ListModels != nil {
		choices = m.Bridge.ListModels()
	}
	if len(choices) == 0 {
		choices = []PickerOption{
			{Label: "deepseek-chat", Detail: "fast, general-purpose coding & chat (default)"},
			{Label: "deepseek-reasoner", Detail: "reasoning model, thinks step by step (R1)"},
		}
	}
	items := make([]views.AutocompleteItem, len(choices))
	selIdx := 0
	for i, c := range choices {
		if strings.EqualFold(c.Label, m.ModelName) {
			selIdx = i
		}
		items[i] = views.AutocompleteItem{
			Token:       "/model " + c.Label,
			Description: c.Detail,
			NeedsArg:    false,
		}
	}
	m.DropdownKind = views.AutocompleteCommand
	m.DropdownQuery = "select model ↑/↓ then press Enter"
	m.DropdownItems = items
	m.DropdownIndex = selIdx
	m.PickerMode = "model"
	return m
}

func (m Model) openProviderSubPicker() Model {
	var choices []PickerOption
	if m.Bridge != nil && m.Bridge.ListProviders != nil {
		choices = m.Bridge.ListProviders()
	}
	if len(choices) == 0 {
		choices = []PickerOption{
			{Label: "deepseek", Detail: "default: deepseek-chat"},
			{Label: "openai", Detail: "default: gpt-4o"},
			{Label: "gemini", Detail: "default: gemini-2.5-pro"},
		}
	}
	items := make([]views.AutocompleteItem, len(choices))
	selIdx := 0
	for i, c := range choices {
		if strings.EqualFold(c.Label, m.ProviderName) {
			selIdx = i
		}
		items[i] = views.AutocompleteItem{
			Token:       "/provider " + c.Label,
			Description: c.Detail,
			NeedsArg:    false,
		}
	}
	m.DropdownKind = views.AutocompleteCommand
	m.DropdownQuery = "select provider ↑/↓ then press Enter"
	m.DropdownItems = items
	m.DropdownIndex = selIdx
	m.PickerMode = "provider"
	return m
}

func (m Model) submitInput() Model {
	raw := strings.TrimSpace(m.InputBuffer)
	m.closeDropdown()
	if raw == "" {
		return m
	}

	// If awaiting API key entry on first launch
	if m.AwaitingKey && m.Bridge != nil && m.Bridge.SaveAPIKey != nil {
		m.InputBuffer = ""
		m.CursorPos = 0
		if err := m.Bridge.SaveAPIKey(raw); err != nil {
			m.Stream = append(m.Stream, StreamEntry{
				Kind:  StreamSystemNotice,
				Title: "✖ [API Key Error]",
				Body:  []string{err.Error()},
			})
			return m
		}
		m.AwaitingKey = false
		m.Stream = append(m.Stream, StreamEntry{
			Kind:  StreamSystemNotice,
			Title: "🔑 [API Key Saved]",
			Body:  []string{fmt.Sprintf("API key for %s saved to ~/.config/tether/credentials.json.", m.ProviderName)},
		})
		return m
	}

	// Append to Prompt History and reset DraftBuffer + HistoryIndex
	m.History = append(m.History, raw)
	m.HistoryIndex = -1
	m.DraftBuffer = ""
	m.InputBuffer = ""
	m.CursorPos = 0
	m.StreamScroll = 0

	// Handle /btw Ephemeral Side-Channel (PRD §3.5)
	if raw == "/btw" || strings.HasPrefix(raw, "/btw ") {
		question := strings.TrimSpace(strings.TrimPrefix(raw, "/btw"))
		if question == "" {
			question = "(quick question)"
		}
		if m.Bridge != nil && m.Bridge.RunSideChat != nil {
			m.PendingBtw = question
			m.Stream = append(m.Stream, StreamEntry{
				Kind:  StreamSideChat,
				Title: "btw · asking in background…",
				Body:  []string{"> " + question},
			})
			return m
		}
		m.Stream = append(m.Stream, StreamEntry{
			Kind:  StreamSideChat,
			Title: "btw · isolated side chat",
			Body: []string{
				"> " + question,
				"Side-channel recorded in isolation without interrupting active workers.",
			},
		})
		return m
	}

	// Handle /model [name]
	if raw == "/model" || strings.HasPrefix(raw, "/model ") {
		arg := strings.TrimSpace(strings.TrimPrefix(raw, "/model"))
		if arg == "" {
			return m.openModelSubPicker()
		}
		if m.Bridge != nil && m.Bridge.SwitchModel != nil {
			canonical, err := m.Bridge.SwitchModel(arg)
			if err != nil {
				m.Stream = append(m.Stream, StreamEntry{
					Kind:  StreamSystemNotice,
					Title: "✖ Model error",
					Body:  []string{err.Error()},
				})
				return m
			}
			m.ModelName = canonical
		} else {
			m.ModelName = arg
		}
		m.Stream = append(m.Stream, StreamEntry{
			Kind:  StreamSystemNotice,
			Title: "? Select model",
			Body:  []string{fmt.Sprintf("> %s (%s)", m.ModelName, m.ProviderName)},
		})
		return m
	}

	// Handle /provider [name]
	if raw == "/provider" || strings.HasPrefix(raw, "/provider ") {
		arg := strings.TrimSpace(strings.TrimPrefix(raw, "/provider"))
		if arg == "" {
			return m.openProviderSubPicker()
		}
		if m.Bridge != nil && m.Bridge.SwitchProvider != nil {
			prov, mod, err := m.Bridge.SwitchProvider(arg)
			if err != nil {
				m.Stream = append(m.Stream, StreamEntry{
					Kind:  StreamSystemNotice,
					Title: "✖ Provider error",
					Body:  []string{err.Error()},
				})
				return m
			}
			m.ProviderName = prov
			m.ModelName = mod
		} else {
			m.ProviderName = arg
		}
		m.Stream = append(m.Stream, StreamEntry{
			Kind:  StreamSystemNotice,
			Title: "? Select provider",
			Body:  []string{fmt.Sprintf("> %s (%s)", m.ProviderName, m.ModelName)},
		})
		return m
	}

	// Handle /key <api-key>
	if strings.HasPrefix(raw, "/key") {
		arg := strings.TrimSpace(strings.TrimPrefix(raw, "/key"))
		if arg == "" {
			m.Stream = append(m.Stream, StreamEntry{
				Kind:  StreamSystemNotice,
				Title: "API key usage",
				Body:  []string{"Usage: /key <api-key> to save credentials for the active provider."},
			})
			return m
		}
		if m.Bridge != nil && m.Bridge.SaveAPIKey != nil {
			_ = m.Bridge.SaveAPIKey(arg)
		}
		m.AwaitingKey = false
		m.Stream = append(m.Stream, StreamEntry{
			Kind:  StreamSystemNotice,
			Title: "? API key",
			Body:  []string{fmt.Sprintf("> Saved for %s", m.ProviderName)},
		})
		return m
	}

	// Handle /read <path> Inspector
	if raw == "/read" || strings.HasPrefix(raw, "/read ") {
		relPath := strings.TrimSpace(strings.TrimPrefix(raw, "/read"))
		relPath = strings.TrimPrefix(relPath, "@")
		if relPath == "" {
			m.Stream = append(m.Stream, StreamEntry{
				Kind:  StreamSystemNotice,
				Title: "Usage /read",
				Body:  []string{"Usage: /read <path> or /read @filename to open the scrollable Inspector."},
			})
			return m
		}
		fullPath := relPath
		if !filepath.IsAbs(fullPath) && m.Workspace != "" {
			fullPath = filepath.Join(m.Workspace, relPath)
		}
		data, err := os.ReadFile(fullPath)
		var lines []string
		if err != nil {
			lines = []string{fmt.Sprintf("Failed to read file %s: %v", relPath, err)}
		} else {
			lines = strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		}
		m.Inspector = InspectorState{
			Active:       true,
			Title:        "READ · " + relPath,
			Lines:        lines,
			ScrollOffset: 0,
		}
		return m
	}

	// Handle /exec <cmd>
	if raw == "/exec" || strings.HasPrefix(raw, "/exec ") {
		cmdStr := strings.TrimSpace(strings.TrimPrefix(raw, "/exec"))
		if cmdStr == "" {
			m.Stream = append(m.Stream, StreamEntry{
				Kind:  StreamSystemNotice,
				Title: "Usage /exec",
				Body:  []string{"Usage: /exec <shell-command> to execute a command through the safety gate."},
			})
			return m
		}
		m.PendingExec = cmdStr
		m.Stream = append(m.Stream, StreamEntry{
			Kind:  StreamUserPrompt,
			Title: ">",
			Body:  []string{"$ " + cmdStr},
		})
		return m
	}

	// Handle /spawn [role] <task> (Background Sub-Agent Worker Delegation)
	if raw == "/spawn" || strings.HasPrefix(raw, "/spawn ") {
		rest := strings.TrimSpace(strings.TrimPrefix(raw, "/spawn"))
		if rest == "" {
			m.Stream = append(m.Stream, StreamEntry{
				Kind:  StreamSystemNotice,
				Title: "Usage /spawn",
				Body: []string{
					"Usage  : /spawn [tether_architect|tether_qa|tether_frontend|tether_product] <task>",
					"Example: /spawn tether_architect analyze package internal/tui",
				},
			})
			return m
		}
		role := "tether_architect"
		task := rest
		fields := strings.Fields(rest)
		if len(fields) >= 2 {
			candidate := strings.ToLower(fields[0])
			switch candidate {
			case "tether_architect", "architect", "tether_qa", "qa", "tether_frontend", "frontend", "tether_product", "product":
				if !strings.HasPrefix(candidate, "tether_") {
					role = "tether_" + candidate
				} else {
					role = candidate
				}
				task = strings.TrimSpace(rest[len(fields[0]):])
			}
		}
		if len(m.Workers) > 1 {
			m.Workers[1].Name = "Sub-Agent (" + role + ")"
			m.Workers[1].State = WorkerActive
			m.Workers[1].Spinner = brailleFrames[0]
			m.Workers[1].Task = truncateTask(task, 42)
			m.Workers[1].Detail = "0.0s"
			m.Workers[1].StartedAt = time.Now()
		}
		m.PendingSpawn = [2]string{role, task}
		m.Stream = append(m.Stream, StreamEntry{
			Kind:  StreamSubAgent,
			Title: fmt.Sprintf("Spawned %s · running", role),
			Body:  []string{"Task: " + task},
		})
		return m
	}

	// Handle /select (toggle mouse capture) and /copy [all|code]
	if raw == "/select" {
		return m.toggleMouseCapture()
	}
	if raw == "/copy" || strings.HasPrefix(raw, "/copy ") {
		mode := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(raw, "/copy")))
		text := m.collectCopyText(mode)
		if strings.TrimSpace(text) == "" {
			m.Stream = append(m.Stream, StreamEntry{
				Kind:  StreamSystemNotice,
				Title: "Copy",
				Body:  []string{"Nothing to copy yet."},
			})
			return m
		}
		m.PendingCopy = text
		m.Stream = append(m.Stream, StreamEntry{
			Kind:  StreamSystemNotice,
			Title: "? Clipboard",
			Body:  []string{fmt.Sprintf("> Copied %d characters", len([]rune(text)))},
		})
		return m
	}

	// Handle /clear
	if raw == "/clear" {
		if m.Bridge != nil && m.Bridge.ClearHistory != nil {
			m.Bridge.ClearHistory()
		}
		m.Stream = []StreamEntry{
			{
				Kind:  StreamSystemNotice,
				Title: "Context cleared",
				Body:  []string{"Viewport cleared; substrate memory remains persisted in session.db."},
			},
		}
		return m
	}

	// Handle /plan and /code
	if raw == "/plan" {
		m.Mode = "PLAN"
		if m.Bridge != nil && m.Bridge.SwitchMode != nil {
			_ = m.Bridge.SwitchMode("plan")
		}
		m.Stream = append(m.Stream, StreamEntry{
			Kind:  StreamSystemNotice,
			Title: "? Mode",
			Body:  []string{"> PLAN (read-only governance)"},
		})
		return m
	}
	if raw == "/code" {
		m.Mode = "CODE"
		if m.Bridge != nil && m.Bridge.SwitchMode != nil {
			_ = m.Bridge.SwitchMode("code")
		}
		m.Stream = append(m.Stream, StreamEntry{
			Kind:  StreamSystemNotice,
			Title: "? Mode",
			Body:  []string{"> CODE (mutations enabled with safety gate)"},
		})
		return m
	}

	// Handle /status
	if raw == "/status" || raw == "/workers" {
		summary := fmt.Sprintf("Workspace: %s | Mode: %s | Provider: %s (%s) | Memory: %dMB",
			m.Workspace, m.Mode, m.ProviderName, m.ModelName, m.MemoryMB)
		if m.Bridge != nil && m.Bridge.StatusSummary != nil {
			summary = m.Bridge.StatusSummary()
		}
		m.Stream = append(m.Stream, StreamEntry{
			Kind:  StreamSystemNotice,
			Title: "Workspace & session status",
			Body:  strings.Split(summary, "\n"),
		})
		return m
	}

	// Handle /undo
	if raw == "/undo" || strings.HasPrefix(raw, "/undo ") {
		turn := strings.TrimSpace(strings.TrimPrefix(raw, "/undo"))
		msg := "No turn snapshot available to restore."
		if m.Bridge != nil && m.Bridge.Undo != nil {
			msg = m.Bridge.Undo(turn)
		}
		m.Stream = append(m.Stream, StreamEntry{
			Kind:  StreamSystemNotice,
			Title: "Undo snapshot",
			Body:  strings.Split(msg, "\n"),
		})
		return m
	}

	// Handle /help
	if raw == "/help" || raw == "/?" {
		var helpLines []string
		for _, sc := range m.SlashCommands {
			helpLines = append(helpLines, fmt.Sprintf("  %-12s  %s", sc.Command, sc.Description))
		}
		m.Stream = append(m.Stream, StreamEntry{
			Kind:  StreamSystemNotice,
			Title: "Commands",
			Body:  helpLines,
		},
		)
		return m
	}

	if raw == "/exit" || raw == "/quit" {
		m.Quitting = true
		return m
	}

	// Standard user prompt submission into Zone B + mark Main Orchestrator worker active
	m.Stream = append(m.Stream, StreamEntry{
		Kind:  StreamUserPrompt,
		Title: ">",
		Body:  []string{raw},
	})
	m.ReasoningBuf = ""
	m.ReasoningStarted = time.Time{}
	m.LiveStatus = "Thinking about your request…"
	if len(m.Workers) > 0 {
		m.Workers[0].State = WorkerActive
		m.Workers[0].Spinner = brailleFrames[0]
		m.Workers[0].Task = "Processing: " + truncateTask(raw, 42)
		m.Workers[0].Detail = "0.0s"
		m.Workers[0].StartedAt = time.Now()
	}
	m.PendingPrompt = raw
	return m
}

func (m Model) confirmApprovalOption(choice views.ApprovalOptionIndex) Model {
	if m.Approval == nil {
		return m
	}
	if choice < 0 || int(choice) >= len(views.ApprovalOptionLabels) {
		choice = views.ApprovalApproveAndRun
	}
	label := views.ApprovalOptionLabel(choice, m.Approval.ScopeHint)
	m.LastApprovalRes = label
	target := nonEmpty(m.Approval.Target, "(workspace)")
	action := nonEmpty(m.Approval.ActionType, "action")

	switch choice {
	case views.ApprovalApproveAndRun, views.ApprovalApproveFolder, views.ApprovalApproveSession:
		m.Stream = append(m.Stream, StreamEntry{
			Kind:    StreamSystemNotice,
			Title:   fmt.Sprintf("? %s · %s", action, target),
			Summary: "> " + label,
			Body:    []string{"> " + label},
		})
		m.resolveApprovalGate(int(choice))
		m.Approval = nil
	case views.ApprovalViewFullDiff:
		m.Inspector = InspectorState{
			Active:       true,
			Title:        "DIFF · " + target,
			Lines:        append([]string(nil), m.Approval.DiffLines...),
			ScrollOffset: 0,
		}
	case views.ApprovalRejectAction:
		m.Stream = append(m.Stream, StreamEntry{
			Kind:    StreamSystemNotice,
			Title:   fmt.Sprintf("? %s · %s", action, target),
			Summary: "> " + label,
			Body:    []string{"> " + label},
		})
		m.resolveApprovalGate(int(views.ApprovalRejectAction))
		m.Approval = nil
	case views.ApprovalAbortTask:
		m.Stream = append(m.Stream, StreamEntry{
			Kind:    StreamSystemNotice,
			Title:   fmt.Sprintf("? %s · %s", action, target),
			Summary: "> " + label,
			Body:    []string{"> " + label},
		})
		m.resolveApprovalGate(int(views.ApprovalAbortTask))
		m.Approval = nil
		m.cancelActiveWorkers()
	}
	return m
}

// flushReasoning converts any accumulated live reasoning text into a compact,
// muted "▸ Thought for Xs" transcript entry with a 1-line faded preview.
func (m *Model) flushReasoning(now time.Time) {
	text := strings.TrimSpace(m.ReasoningBuf)
	if text == "" {
		m.ReasoningBuf = ""
		m.ReasoningStarted = time.Time{}
		return
	}
	var dur time.Duration
	if !m.ReasoningStarted.IsZero() && !now.IsZero() {
		dur = now.Sub(m.ReasoningStarted)
		if dur < 0 {
			dur = 0
		}
	}
	label := "Thought"
	if dur >= time.Second {
		label = fmt.Sprintf("Thought for %ds", int(dur.Round(time.Second).Seconds()))
	} else if dur > 0 {
		label = fmt.Sprintf("Thought for %.1fs", dur.Seconds())
	}
	preview := firstMeaningfulLine(text)
	entry := StreamEntry{
		Kind:      StreamThought,
		Title:     label,
		Summary:   preview,
		Body:      []string{preview},
		Collapsed: true,
	}
	if m.liveStream > 0 && m.liveStream-1 <= len(m.Stream) {
		pos := m.liveStream - 1
		m.Stream = append(m.Stream, StreamEntry{})
		copy(m.Stream[pos+1:], m.Stream[pos:])
		m.Stream[pos] = entry
		m.liveStream++
	} else {
		m.Stream = append(m.Stream, entry)
	}
	m.ReasoningBuf = ""
	m.ReasoningStarted = time.Time{}
}

func firstMeaningfulLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		clean := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-*#•> "))
		if clean != "" {
			return truncateTask(clean, 100)
		}
	}
	return ""
}

// latestReasoningPreview extracts the active trailing thought sentence/line from
// a live streaming reasoning buffer so the user sees what the model is currently doing.
func latestReasoningPreview(buf string) string {
	lines := strings.Split(strings.TrimSpace(buf), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		clean := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(lines[i]), "-*#•> "))
		if clean == "" {
			continue
		}
		r := []rune(clean)
		if len(r) > 96 {
			return "…" + string(r[len(r)-95:])
		}
		return clean
	}
	return ""
}

// ingestStreamLog parses raw harness tool/reasoning lines into clean 1-line `agy` entries
// (`● Read (...)`, `● Ran (...)`, `● Searched (...)`, `● Edited (...)`, `▸ Thought`).
func (m *Model) ingestStreamLog(workerID, raw string) {
	cleaned := StripANSI(raw)
	if strings.Contains(cleaned, "[Sub-Agent ") && len(m.Workers) > 1 {
		m.Workers[1].State = WorkerActive
		m.Workers[1].Spinner = brailleFrames[m.SpinnerStep%len(brailleFrames)]
		m.Workers[1].Task = truncateTask(cleaned, 44)
		m.Workers[1].Detail = "0.0s"
		m.Workers[1].StartedAt = time.Now()
	}

	rawLines := strings.Split(cleaned, "\n")
	first := strings.TrimSpace(rawLines[0])
	if first == "" {
		return
	}

	// 1. Non-streamed reasoning dump -> compact Thought block
	if strings.HasPrefix(first, "💭 [reasoning]") {
		rest := strings.TrimSpace(strings.TrimPrefix(cleaned, "💭 [reasoning]"))
		preview := firstMeaningfulLine(rest)
		m.Stream = append(m.Stream, StreamEntry{
			Kind:      StreamThought,
			Title:     "Thought",
			Summary:   preview,
			Body:      []string{preview},
			Collapsed: true,
		})
		return
	}

	// 2. WRITE <path> (+ optional diff preview lines)
	if strings.HasPrefix(first, "WRITE ") {
		target := strings.TrimSpace(strings.TrimPrefix(first, "WRITE "))
		var diffs []string
		for _, dl := range rawLines[1:] {
			t := strings.TrimSpace(dl)
			if t == "" || strings.HasPrefix(t, "↳") {
				continue
			}
			diffs = append(diffs, t)
		}
		m.LiveStatus = "Editing " + truncateTask(target, 56) + "…"
		m.Stream = append(m.Stream, StreamEntry{
			Kind:    StreamTool,
			Title:   "Edited",
			Summary: target,
			Body:    diffs,
		})
		return
	}

	// 3. Post-write status line ("↳ ..." or "✖ ...")
	if strings.HasPrefix(first, "↳ ") {
		return
	}
	if strings.HasPrefix(first, "✖ ") || strings.HasPrefix(first, "🛑 ") {
		m.Stream = append(m.Stream, StreamEntry{
			Kind:  StreamSystemNotice,
			Title: first,
		})
		return
	}

	// 4. Standard MCP tool call lines ("🔧 [tool_name] details")
	if strings.HasPrefix(first, "🔧 [") {
		rest := strings.TrimPrefix(first, "🔧 [")
		closeIdx := strings.IndexByte(rest, ']')
		if closeIdx > 0 {
			toolName := rest[:closeIdx]
			detail := strings.TrimSpace(rest[closeIdx+1:])
			verb := toolName
			switch toolName {
			case "read_file":
				verb = "Read"
				m.LiveStatus = "Reading " + truncateTask(detail, 56) + "…"
			case "run_command":
				verb = "Ran"
				detail = strings.TrimSpace(strings.TrimPrefix(detail, "$"))
				m.LiveStatus = "Running " + truncateTask(detail, 56) + "…"
			case "kb_search":
				verb = "Searched"
				m.LiveStatus = "Searching " + truncateTask(detail, 56) + "…"
			case "kb_index":
				verb = "Indexed"
				m.LiveStatus = "Indexing " + truncateTask(detail, 56) + "…"
			default:
				m.LiveStatus = "Running " + toolName + "…"
			}
			m.Stream = append(m.Stream, StreamEntry{
				Kind:    StreamTool,
				Title:   verb,
				Summary: detail,
			})
			return
		}
	}

	// 5. Fallback for any other worker log line
	if workerID != "" && workerID != "Main Agent" {
		// When live SubagentEvent observer is active, skip duplicate raw subagent logs
		if len(m.Subagents) > 0 {
			return
		}
		m.Stream = append(m.Stream, StreamEntry{
			Kind:  StreamSubAgent,
			Title: fmt.Sprintf("Subagent %s", workerID),
			Body:  rawLines,
		})
		return
	}
	m.Stream = append(m.Stream, StreamEntry{
		Kind:    StreamTool,
		Title:   "Ran",
		Summary: first,
	})
}

func truncateTask(s string, maxRunes int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= maxRunes {
		return string(r)
	}
	return string(r[:maxRunes-1]) + "…"
}

func nonEmpty(a, fallback string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return fallback
}

func (m *Model) cancelActiveWorkers() {
	m.flushReasoning(time.Now())
	m.ReasoningBuf = ""
	m.LiveStatus = ""
	if m.liveStream > 0 && m.liveStream-1 < len(m.Stream) {
		m.Stream[m.liveStream-1].Title = "interrupted"
	}
	m.liveStream = 0
	for i := range m.Subagents {
		if m.Subagents[i].State == WorkerActive {
			m.Subagents[i].State, m.Subagents[i].Spinner, m.Subagents[i].Detail = WorkerCancelled, "⊘", "Cancelled"
		}
	}
	cancelled := 0
	if m.cancelAllFn != nil {
		cancelled = m.cancelAllFn()
	}
	for i := range m.Workers {
		if m.Workers[i].State == WorkerActive || m.Workers[i].State == WorkerWait {
			m.Workers[i].State = WorkerCancelled
			m.Workers[i].Spinner = "⊘"
			m.Workers[i].Detail = "Cancelled"
			cancelled++
		}
	}
	m.CancelCount += max(1, cancelled)
	m.Stream = append(m.Stream, StreamEntry{
		Kind:  StreamSystemNotice,
		Title: "Interrupted",
		Body:  []string{"Stopped active tasks (Esc)."},
	})
}
