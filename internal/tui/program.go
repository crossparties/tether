package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/croosparties/tether/internal/tui/views"
	"github.com/croosparties/tether/internal/ui"
)

// ParseKeyBytes translates raw terminal bytes read in raw mode into a KeyMsg.
func ParseKeyBytes(buf []byte) (KeyMsg, bool) {
	if len(buf) == 0 {
		return KeyMsg{}, false
	}
	// CSI escape sequences & Mouse Wheel sequences
	if buf[0] == 0x1b {
		if len(buf) == 1 {
			return KeyMsg{Type: KeyEsc}, true
		}
		// SGR Mouse Mode: \x1b[<btn;col;rowM or m
		if len(buf) >= 6 && buf[1] == '[' && buf[2] == '<' {
			s := string(buf[3:])
			semi := strings.IndexByte(s, ';')
			if semi > 0 {
				btnStr := s[:semi]
				switch btnStr {
				case "64":
					return KeyMsg{Type: KeyWheelUp}, true
				case "65":
					return KeyMsg{Type: KeyWheelDown}, true
				default:
					// Ignore other mouse button press/release events so they never pollute input
					return KeyMsg{}, false
				}
			}
			return KeyMsg{}, false
		}
		// X10 / Normal Mouse Mode: \x1b[M + 3 bytes (button, x, y)
		if len(buf) >= 6 && buf[1] == '[' && buf[2] == 'M' {
			switch buf[3] {
			case 96: // 0x60 = Wheel Up
				return KeyMsg{Type: KeyWheelUp}, true
			case 97: // 0x61 = Wheel Down
				return KeyMsg{Type: KeyWheelDown}, true
			default:
				return KeyMsg{}, false
			}
		}
		// Modified Arrow Keys (Shift/Alt/Ctrl + Up/Down -> \x1b[1;2A, \x1b[1;3A, \x1b[1;5A)
		if len(buf) >= 6 && buf[1] == '[' && buf[2] == '1' && buf[3] == ';' {
			switch buf[5] {
			case 'A':
				return KeyMsg{Type: KeyWheelUp}, true
			case 'B':
				return KeyMsg{Type: KeyWheelDown}, true
			}
		}
		if len(buf) >= 3 && (buf[1] == '[' || buf[1] == 'O') {
			switch buf[2] {
			case 'A':
				return KeyMsg{Type: KeyUp}, true
			case 'B':
				return KeyMsg{Type: KeyDown}, true
			case 'C':
				return KeyMsg{Type: KeyRight}, true
			case 'D':
				return KeyMsg{Type: KeyLeft}, true
			case '5':
				return KeyMsg{Type: KeyPgUp}, true
			case '6':
				return KeyMsg{Type: KeyPgDn}, true
			}
		}
		return KeyMsg{Type: KeyEsc}, true
	}

	switch buf[0] {
	case 3: // Ctrl+C
		return KeyMsg{Type: KeyCtrlC}, true
	case 0x19: // Ctrl+Y toggles mouse capture (scroll <-> native select/copy)
		return KeyMsg{Type: KeyToggleMouse}, true
	case '\r', '\n':
		return KeyMsg{Type: KeyEnter}, true
	case '\t':
		return KeyMsg{Type: KeyTab}, true
	case 0x7f, 0x08:
		return KeyMsg{Type: KeyBackspace}, true
	case ' ':
		return KeyMsg{Type: KeySpace, Rune: ' '}, true
	}

	r, _ := utf8.DecodeRune(buf)
	if r == utf8.RuneError || r < 32 {
		return KeyMsg{}, false
	}
	return KeyMsg{Type: KeyRune, Rune: r}, true
}

type tuiLogWriter struct {
	workerID string
	msgCh    chan<- any
}

func (w *tuiLogWriter) Write(p []byte) (int, error) {
	text := strings.TrimSpace(string(p))
	if text != "" {
		select {
		case w.msgCh <- AgentStreamLogMsg{WorkerID: w.workerID, Line: text}:
		default:
		}
	}
	return len(p), nil
}

func queryTerminalSize(fds ...uintptr) (int, int) {
	type winsize struct {
		Row    uint16
		Col    uint16
		Xpixel uint16
		Ypixel uint16
	}
	for _, fd := range fds {
		var ws winsize
		_, _, errno := syscall.Syscall(
			syscall.SYS_IOCTL,
			fd,
			uintptr(syscall.TIOCGWINSZ),
			uintptr(unsafe.Pointer(&ws)),
		)
		if errno == 0 && ws.Col > 0 && ws.Row > 0 {
			return int(ws.Col), int(ws.Row)
		}
	}
	return 100, 30
}

// renderFrame writes a double-buffered, synchronized TUI frame to out.
// Using DEC Synchronized Output (\033[?2026h ... \033[?2026l) and per-line
// Erase-in-Line (\033[K) prevents any tearing or leftover ghost characters when resizing.
func renderFrame(out io.Writer, m Model, clearScreen bool) {
	view := m.View()
	lines := strings.Split(view, "\n")
	var b strings.Builder
	b.Grow(len(view) + len(lines)*8 + 32)
	b.WriteString("\033[?2026h")
	if clearScreen {
		b.WriteString("\033[2J\033[H")
	} else {
		b.WriteString("\033[H")
	}
	for i, line := range lines {
		b.WriteString(line)
		b.WriteString("\033[K")
		if i < len(lines)-1 {
			b.WriteByte('\n')
		}
	}
	b.WriteString("\033[J\033[?2026l")
	_, _ = io.WriteString(out, b.String())
}

// RunTUI launches the interactive 3-Zone Alt-Screen TUI loop on the given input/output streams.
// It guarantees terminal restoration (ExitAltScreenSeq, ShowCursorSeq, and termios restore)
// on normal exit, context cancellation, SIGINT/SIGTERM, or panic.
func RunTUI(ctx context.Context, in *os.File, out io.Writer, workspace, mode, branch string, orch any, bridge ...*AgentBridge) (err error) {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}

	m := NewModel(workspace)
	if mode != "" {
		m.Mode = mode
	}
	if branch != "" {
		m.Branch = branch
	}
	if len(bridge) > 0 && bridge[0] != nil {
		m.Bridge = bridge[0]
		if m.Bridge.Provider != nil {
			m.ProviderName = m.Bridge.Provider()
		}
		if m.Bridge.Model != nil {
			m.ModelName = m.Bridge.Model()
		}
		if m.Bridge.Mode != nil {
			m.Mode = strings.ToUpper(m.Bridge.Mode())
		}
		if m.Bridge.NeedsAPIKey != nil && m.Bridge.NeedsAPIKey() {
			m.AwaitingKey = true
			m.Stream = append(m.Stream, StreamEntry{
				Kind:  StreamSystemNotice,
				Title: fmt.Sprintf("🔑 [API Key Diperlukan untuk %s]", m.ProviderName),
				Body:  []string{"Ketik API Key Anda di kotak input bawah lalu tekan Enter (atau ketik /provider untuk ganti provider)."},
			})
		}
	}
	m.AltScreenUsed = true
	m.BindOrchestrator(orch)
	m.IndexWorkspaceFiles(200)

	if outFile, ok := out.(*os.File); ok && (ui.IsTerminal(outFile) || ui.IsTerminal(in)) {
		w, h := queryTerminalSize(outFile.Fd(), in.Fd())
		m = m.Update(WindowSizeMsg{Width: w, Height: h})
	}

	var restoreTerm func()
	if ui.IsTerminal(in) {
		if r, rawErr := ui.MakeRaw(in); rawErr == nil {
			restoreTerm = r
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	defer signal.Stop(winchCh)

	var activeMu sync.Mutex
	var activeCancels []context.CancelFunc
	cancelActive := func() int {
		activeMu.Lock()
		cancels := activeCancels
		activeCancels = nil
		activeMu.Unlock()
		for _, c := range cancels {
			c()
		}
		if orchCancel, ok := orch.(interface{ CancelAll() int }); ok {
			return len(cancels) + orchCancel.CancelAll()
		}
		return len(cancels)
	}
	m.SetCancelAllFunc(cancelActive)

	cleanedUp := false
	cleanup := func() {
		if cleanedUp {
			return
		}
		cleanedUp = true
		cancelActive()
		if restoreTerm != nil {
			restoreTerm()
		}
		fmt.Fprint(out, DisableMouseSeq+ShowCursorSeq+ExitAltScreenSeq)
	}
	defer func() {
		cleanup()
		if rec := recover(); rec != nil {
			err = fmt.Errorf("tui recovered from panic: %v", rec)
		}
	}()

	fmt.Fprint(out, EnterAltScreenSeq+HideCursorSeq+EnableMouseSeq)
	renderFrame(out, m, true)

	msgCh := make(chan any, 128)
	eofCh := make(chan struct{})

	if m.Bridge != nil && m.Bridge.BindApproval != nil {
		m.Bridge.BindApproval(func(req ApprovalRequest) int {
			return int(SendApprovalGate(msgCh, req))
		})
	}
	if m.Bridge != nil && m.Bridge.BindSubagents != nil {
		m.Bridge.BindSubagents(func(ev SubagentEventMsg) {
			select {
			case msgCh <- ev:
			case <-ctx.Done():
			}
		})
	}
	go func() {
		defer close(eofCh)
		var buf [64]byte
		for {
			n, rerr := in.Read(buf[:])
			if n > 0 {
				if buf[0] == 4 { // Ctrl+D exits cleanly
					return
				}
				if km, ok := ParseKeyBytes(buf[:n]); ok {
					select {
					case msgCh <- km:
					case <-ctx.Done():
						return
					}
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	// applyEffects performs terminal side effects queued by the pure Update function:
	// clipboard writes and mouse-capture toggling (scroll <-> native select/copy).
	lastMouse := true
	applyEffects := func() {
		if m.PendingCopy != "" {
			writeClipboard(out, m.PendingCopy)
			m.PendingCopy = ""
		}
		if m.MouseCapture != lastMouse {
			lastMouse = m.MouseCapture
			if lastMouse {
				fmt.Fprint(out, EnableMouseSeq)
			} else {
				fmt.Fprint(out, DisableMouseSeq)
			}
		}
	}

	// resyncSize re-queries the terminal geometry. It is called on SIGWINCH and on every
	// tick, so a missed/coalesced signal (tmux, some emulators) can never leave the layout
	// stuck at a stale size. A real change forces a full clear to erase ghost borders.
	resyncSize := func() bool {
		outFile, ok := out.(*os.File)
		if !ok {
			return false
		}
		w, h := queryTerminalSize(outFile.Fd(), in.Fd())
		if w == m.Width && h == m.Height {
			return false
		}
		m = m.Update(WindowSizeMsg{Width: w, Height: h})
		renderFrame(out, m, true)
		return true
	}

	for !m.Quitting {
		select {
		case <-ctx.Done():
			return nil
		case <-sigCh:
			return nil
		case <-eofCh:
			return nil
		case <-winchCh:
			resyncSize()
		case now := <-ticker.C:
			if resyncSize() {
				continue
			}
			hasActive := m.hasRunningSubagent()
			for _, w := range m.Workers {
				if w.State == WorkerActive {
					hasActive = true
					break
				}
			}
			if hasActive {
				m = m.Update(TickMsg{Now: now})
				renderFrame(out, m, false)
			}
		case msg := <-msgCh:
			m = m.Update(msg)
			applyEffects()
			// Dispatch asynchronous Main Agent turn if PendingPrompt was queued
			if m.PendingPrompt != "" && m.Bridge != nil && (m.Bridge.RunPrompt != nil || m.Bridge.RunPromptStream != nil) {
				prompt := m.PendingPrompt
				m.PendingPrompt = ""
				turnCtx, turnCancel := context.WithCancel(ctx)
				activeMu.Lock()
				activeCancels = append(activeCancels, turnCancel)
				activeMu.Unlock()

				go func(p string) {
					defer turnCancel()
					start := time.Now()
					lw := &tuiLogWriter{workerID: "Main Agent", msgCh: msgCh}
					var ans string
					var runErr error
					if m.Bridge.RunPromptStream != nil {
						onDelta := func(kind, text string) {
							select {
							case msgCh <- AgentDeltaMsg{Kind: kind, Text: text}:
							case <-turnCtx.Done():
							}
						}
						ans, runErr = m.Bridge.RunPromptStream(turnCtx, p, lw, onDelta)
					} else {
						ans, runErr = m.Bridge.RunPrompt(turnCtx, p, lw)
					}
					msgCh <- AgentTurnDoneMsg{
						WorkerID: "main",
						Prompt:   p,
						Answer:   ans,
						Err:      runErr,
						Elapsed:  time.Since(start),
					}
				}(prompt)
			}

			// Dispatch asynchronous /btw Side-Chat turn if PendingBtw was queued
			if m.PendingBtw != "" && m.Bridge != nil && m.Bridge.RunSideChat != nil {
				question := m.PendingBtw
				m.PendingBtw = ""
				btwCtx, btwCancel := context.WithCancel(ctx)
				activeMu.Lock()
				activeCancels = append(activeCancels, btwCancel)
				activeMu.Unlock()

				go func(q string) {
					defer btwCancel()
					start := time.Now()
					ans, btwErr := m.Bridge.RunSideChat(btwCtx, q)
					msgCh <- SideChatDoneMsg{
						Question: q,
						Answer:   ans,
						Err:      btwErr,
						Elapsed:  time.Since(start),
					}
				}(question)
			}

			// Dispatch asynchronous /exec command if PendingExec was queued
			if m.PendingExec != "" && m.Bridge != nil && m.Bridge.ExecuteCommand != nil {
				cmdStr := m.PendingExec
				m.PendingExec = ""
				execCtx, execCancel := context.WithCancel(ctx)
				activeMu.Lock()
				activeCancels = append(activeCancels, execCancel)
				activeMu.Unlock()

				go func(c string) {
					defer execCancel()
					start := time.Now()
					res, execErr := m.Bridge.ExecuteCommand(execCtx, c)
					msgCh <- AgentTurnDoneMsg{
						WorkerID: "exec",
						Prompt:   "/exec " + c,
						Answer:   res,
						Err:      execErr,
						Elapsed:  time.Since(start),
					}
				}(cmdStr)
			}

			// Dispatch asynchronous /spawn Sub-Agent worker if PendingSpawn was queued
			if m.PendingSpawn[1] != "" && m.Bridge != nil && m.Bridge.SpawnSubagent != nil {
				role, task := m.PendingSpawn[0], m.PendingSpawn[1]
				m.PendingSpawn = [2]string{}
				subCtx, subCancel := context.WithCancel(ctx)
				activeMu.Lock()
				activeCancels = append(activeCancels, subCancel)
				activeMu.Unlock()

				go func(r, t string) {
					defer subCancel()
					start := time.Now()
					lw := &tuiLogWriter{workerID: r, msgCh: msgCh}
					res, subErr := m.Bridge.SpawnSubagent(subCtx, r, t, lw)
					msgCh <- AgentTurnDoneMsg{
						WorkerID: "subagent",
						Prompt:   "/spawn " + r + " " + t,
						Answer:   res,
						Err:      subErr,
						Elapsed:  time.Since(start),
					}
				}(role, task)
			}

			renderFrame(out, m, false)
		}
	}
	return nil
}

// SendApprovalGate routes a high-risk tool/command decision into the running TUI loop
// and blocks the worker goroutine until the user chooses one of the 4 arrow options.
func SendApprovalGate(msgCh chan<- any, req ApprovalRequest) views.ApprovalOptionIndex {
	replyCh := make(chan int, 1)
	msgCh <- ApprovalGateMsg{
		Request: req,
		ReplyCh: replyCh,
	}
	choice := <-replyCh
	return views.ApprovalOptionIndex(choice)
}
