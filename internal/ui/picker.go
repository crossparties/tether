package ui

// An arrow-key picker for a short list of choices.
//
// The reference is kirocrew's approval prompt: a highlighted row the user moves
// with Up/Down and confirms with Enter. cc-sdd, which we also looked at, has no
// arrow handling at all -- its promptSelect is a readline.question that only
// accepts a typed number -- so there was nothing there to copy.
//
// Three constraints shaped this file.
//
//  1. It must degrade, not crash. stdin is a pipe whenever the caller does
//     `printf '1\n' | tether` or runs the e2e approval tests, and raw mode is
//     meaningless there. Pick refuses to take over the terminal unless both
//     ends are a TTY, and the caller falls back to the line prompt it already
//     had. A picker that throws on a pipe would break the very tests that
//     guard the approval gate.
//
//  2. The terminal must always come back. Raw mode disables echo and line
//     editing; leaving it on after an error turns the user's shell into a
//     silent, unusable terminal. Every exit path -- including panic -- restores
//     the saved termios.
//
//  3. Redrawing must not erase evidence. Only the rows this picker owns are
//     rewritten, using the cursor-up sequence, never ClearScreen. A diff or an
//     error above the menu stays on screen.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// Choice is one row of the picker. Detail is rendered dimmed after the label,
// matching the approval prompt's existing layout.
type Choice struct {
	Label  string
	Detail string
}

// Key codes the picker understands. The escape sequences are the ANSI CSI
// forms sent by every terminal in common use; Enter is carriage return because
// raw mode reports it that way.
const (
	keyUp    = '\x1b'
	keyEnter = '\r'
	keyLF    = '\n'
	keyCtrlC = '\x03'
)

// Pick shows choices and returns the index chosen. start is the initially
// highlighted row.
//
// ok is false when the input is not an interactive terminal, when the user
// interrupts with Ctrl-C, or when input ends before a choice is made. Callers
// must treat !ok as "ask some other way" or "cancel" -- never as "the default".
func Pick(in *os.File, out io.Writer, choices []Choice, start int) (index int, ok bool) {
	if len(choices) == 0 {
		return 0, false
	}
	if start < 0 || start >= len(choices) {
		start = 0
	}

	// Without a real terminal on both ends there is no arrow key to receive and
	// no cursor to move. Refusing here is what keeps piped input working: the
	// caller keeps its line-based prompt.
	if !IsTerminal(in) || !IsTerminalFile(out) {
		return 0, false
	}

	restore, err := makeRaw(in)
	if err != nil {
		return 0, false
	}
	// Restore on every path, panic included, so a crash cannot leave the user
	// with a terminal that no longer echoes.
	defer restore()

	selected := start
	drawChoices(out, choices, selected, 0)

	reader := bufio.NewReader(in)
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return 0, false
		}

		switch b {
		case keyEnter, keyLF:
			// Redraw once with the cursor parked so the final frame matches the
			// selection the caller is about to act on.
			drawChoices(out, choices, selected, len(choices))
			return selected, true

		case keyCtrlC:
			// Leave the last frame on screen; the caller prints why it stopped.
			fmt.Fprintln(out)
			return 0, false

		case keyUp:
			// ESC begins an escape sequence. Consume the rest so a stray arrow
			// does not get interpreted as a typed character later.
			seq, _ := readEscape(reader)
			switch seq {
			case "A": // Up
				selected = (selected - 1 + len(choices)) % len(choices)
			case "B": // Down
				selected = (selected + 1) % len(choices)
			default:
				// Left/Right/Home/End and unknown sequences are ignored rather
				// than treated as a choice.
				continue
			}
			drawChoices(out, choices, selected, len(choices))

		case 'k', 'K':
			selected = (selected - 1 + len(choices)) % len(choices)
			drawChoices(out, choices, selected, len(choices))

		case 'j', 'J':
			selected = (selected + 1) % len(choices)
			drawChoices(out, choices, selected, len(choices))

		default:
			// Digit shortcuts keep the muscle memory from the numeric prompt.
			if b >= '1' && b <= '9' {
				idx := int(b - '1')
				if idx < len(choices) {
					selected = idx
					drawChoices(out, choices, selected, len(choices))
				}
			}
		}
	}
}

// drawChoices rewrites the menu in place. clearLines is how many rows from the
// previous frame are already on screen and must be overwritten; 0 means this is
// the first frame.
func drawChoices(out io.Writer, choices []Choice, selected, clearLines int) {
	// Move to the start of the block this picker owns and wipe it row by row.
	if clearLines > 0 {
		fmt.Fprintf(out, "\033[%dA", clearLines)
		for i := 0; i < clearLines; i++ {
			fmt.Fprint(out, ClearLine())
			if i < clearLines-1 {
				fmt.Fprint(out, "\033[B")
			}
		}
		fmt.Fprintf(out, "\033[%dA", clearLines-1)
	}

	for i, c := range choices {
		marker := "  "
		label := c.Label
		if i == selected {
			marker = "> "
			label = Bold(c.Label)
		}
		line := fmt.Sprintf("%s%d) %-20s", marker, i+1, label)
		if c.Detail != "" {
			line += " " + Dim(c.Detail)
		}
		fmt.Fprint(out, ClearLine())
		fmt.Fprintln(out, line)
	}
	fmt.Fprint(out, ClearLine())
	fmt.Fprint(out, Dim("  ↑/↓ move · Enter select · Ctrl-C cancel"))
}

// readEscape consumes the remainder of a CSI sequence after ESC and returns
// its final byte (for example "A" for Up).
func readEscape(r *bufio.Reader) (string, bool) {
	next, err := r.ReadByte()
	if err != nil {
		return "", false
	}
	if next != '[' && next != 'O' {
		return "", false
	}
	// Parameters (digits, ';') may precede the final byte; the sequences the
	// picker cares about have none, but consuming them keeps the stream aligned.
	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", false
		}
		if b >= 0x40 && b <= 0x7e {
			return string(b), true
		}
	}
}

// IsTerminal reports whether f is a character device, which is the cheapest
// reliable check that raw mode and arrow keys make sense. It avoids pulling in
// golang.org/x/term for one ioctl.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// IsTerminalFile is IsTerminal for a writer that may not be an *os.File (tests
// pass buffers here, which must not be mistaken for terminals).
func IsTerminalFile(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && IsTerminal(f)
}

// makeRaw puts f into raw mode and returns a function that restores the
// previous settings. It delegates to the platform-specific MakeRaw and prints
// a trailing newline to stderr on restore.
func makeRaw(f *os.File) (func(), error) {
	restore, err := MakeRaw(f)
	if err != nil {
		return nil, err
	}
	return func() {
		restore()
		// The prompt line above the menu was left open; end it so the next
		// output starts on its own line.
		fmt.Fprint(os.Stderr, "\n")
	}, nil
}

// stripANSI removes the escape sequences the picker adds, so widths stay
// correct when the prompt is measured in tests.
func stripANSI(s string) string {
	var b strings.Builder
	inEscape := false
	justSawESC := false
	for _, r := range s {
		switch {
		case inEscape:
			if justSawESC {
				justSawESC = false
				if r == '[' {
					continue
				}
				inEscape = false
				continue
			}
			if r >= 0x40 && r <= 0x7e {
				inEscape = false
			}
		case r == '\x1b':
			inEscape = true
			justSawESC = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
