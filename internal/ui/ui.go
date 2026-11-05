// Package ui renders terminal output for Tether.
//
// Colour is a convenience, never a requirement: every helper degrades to plain
// text when stdout is not a terminal or when the user opted out via NO_COLOR.
// Output that machines read (hook JSON, MCP payloads) must not go through this
// package at all.
package ui

import (
	"fmt"
	"os"
	"sync"

	"github.com/mattn/go-isatty"
)

// ANSI SGR sequences.
const (
	reset     = "\033[0m"
	bold      = "\033[1m"
	faint     = "\033[2m"
	red       = "\033[31m"
	green     = "\033[32m"
	yellow    = "\033[33m"
	blue      = "\033[34m"
	magenta   = "\033[35m"
	cyan      = "\033[36m"
	brightRed = "\033[91m"
)

var (
	mu       sync.RWMutex
	override *bool
	cached   *bool
)

// Enabled reports whether styled output should be emitted. Detection runs once
// per process unless SetEnabled overrides it.
func Enabled() bool {
	mu.RLock()
	if override != nil {
		v := *override
		mu.RUnlock()
		return v
	}
	if cached != nil {
		v := *cached
		mu.RUnlock()
		return v
	}
	mu.RUnlock()

	mu.Lock()
	defer mu.Unlock()
	if override != nil {
		return *override
	}
	if cached == nil {
		v := detect()
		cached = &v
	}
	return *cached
}

// ResetDetection forgets the memoised terminal probe. Tests use it to exercise
// environment-dependent detection; production code never needs it.
func ResetDetection() {
	mu.Lock()
	defer mu.Unlock()
	cached = nil
}

// detect decides once whether styling is appropriate.
func detect() bool {
	// https://no-color.org — any non-empty value disables colour.
	if v, ok := os.LookupEnv("NO_COLOR"); ok && v != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	// FORCE_COLOR lets CI capture styled output deliberately.
	if v, ok := os.LookupEnv("FORCE_COLOR"); ok && v != "" && v != "0" {
		return true
	}
	return isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
}

// SetEnabled overrides detection. Pass nil to resume automatic detection.
// Intended for tests and for callers that resolved the terminal state already.
func SetEnabled(v bool) {
	mu.Lock()
	defer mu.Unlock()
	override = &v
}

// ClearOverride drops a SetEnabled override and re-detects.
func ClearOverride() {
	mu.Lock()
	defer mu.Unlock()
	override = nil
	cached = nil
}

// style wraps s in the given SGR codes, or returns it untouched when styling is
// off.
func style(s string, codes ...string) string {
	if !Enabled() || len(codes) == 0 {
		return s
	}
	out := ""
	for _, c := range codes {
		out += c
	}
	return out + s + reset
}

// Pad right-pads s with spaces to width printable characters. Unlike the
// %-Ns verb it ignores SGR escape sequences, so coloured text still lines up in
// boxed output. Strings at or past width are returned unchanged.
func Pad(s string, width int) string {
	visible := VisibleLen(s)
	if visible >= width {
		return s
	}
	return s + repeat(" ", width-visible)
}

// VisibleLen returns the number of printable runes in s, skipping ANSI SGR
// escape sequences so callers can align coloured text.
func VisibleLen(s string) int {
	count := 0
	inEscape := false
	for _, r := range s {
		if inEscape {
			if r == 'm' {
				inEscape = false
			}
			continue
		}
		if r == '\x1b' {
			inEscape = true
			continue
		}
		count++
	}
	return count
}

// Title styles a heading.
func Title(s string) string { return style(s, bold, cyan) }

// Bold styles emphasised text.
func Bold(s string) string { return style(s, bold) }

// Dim styles low-priority log output.
func Dim(s string) string { return style(s, faint) }

// Path styles a filesystem path.
func Path(s string) string { return style(s, cyan) }

// Success styles a positive outcome.
func Success(s string) string { return style(s, green) }

// Warn styles something the user should notice.
func Warn(s string) string { return style(s, yellow) }

// Error styles a failure or a denial.
func Error(s string) string { return style(s, brightRed) }

// Blocked styles a hard refusal; distinct from a plain error.
func Blocked(s string) string { return style(s, bold, brightRed) }

// Accent styles a call to action.
func Accent(s string) string { return style(s, bold, magenta) }

// DiffAdd styles an added line.
func DiffAdd(s string) string { return style(s, green) }

// DiffDel styles a removed line.
func DiffDel(s string) string { return style(s, red) }

// Banner renders a boxed, unmissable notice. width is the inner text width.
func Banner(label, body string, colour func(string) string) string {
	const width = 56
	top := "╔" + repeat("═", width) + "╗"
	bottom := "╚" + repeat("═", width) + "╝"

	head := fmt.Sprintf(" %s", label)
	head += repeat(" ", max(0, width-len([]rune(head))))
	lines := []string{top, "║" + head + "║"}

	if body != "" {
		lines = append(lines, "║"+repeat(" ", width)+"║")
		for _, line := range splitWidth(body, width-2) {
			pad := repeat(" ", max(0, width-2-len([]rune(line))))
			lines = append(lines, "║ "+line+pad+" ║")
		}
	}
	lines = append(lines, bottom)

	out := ""
	for i, line := range lines {
		if i > 0 {
			out += "\n"
		}
		out += colour(line)
	}
	return out
}

// splitWidth breaks text into lines of at most width runes, on word
// boundaries where possible.
func splitWidth(text string, width int) []string {
	var out []string
	for _, paragraph := range splitLines(text) {
		words := splitFields(paragraph)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		current := ""
		for _, w := range words {
			switch {
			case current == "":
				current = w
			case len([]rune(current))+1+len([]rune(w)) <= width:
				current += " " + w
			default:
				out = append(out, current)
				current = w
			}
		}
		if current != "" {
			out = append(out, current)
		}
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func splitFields(s string) []string {
	var out []string
	field := ""
	for _, r := range s {
		if r == ' ' || r == '\t' {
			if field != "" {
				out = append(out, field)
				field = ""
			}
			continue
		}
		field += string(r)
	}
	if field != "" {
		out = append(out, field)
	}
	return out
}

func repeat(s string, n int) string {
	if n <= 0 {
		return ""
	}
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
