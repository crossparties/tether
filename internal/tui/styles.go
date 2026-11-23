package tui

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/crossparties/tether/internal/ui"
)

// Catppuccin Mocha TrueColor Hex Tokens (PRD §4.1).
const (
	BaseSurface    = "#1E1E2E" // Primary background surface (Crust/Mantle)
	OverlaySurface = "#313244" // Floating dropdowns, input bar, side-chat block
	Borders        = "#B4BEFE" // Active panel borders & dividers (Lavender)
	MainAccent     = "#CBA6F7" // Main Agent identity & header branding (Mauve)
	SubAgentAccent = "#89B4FA" // Sub-Agent workers & read-only tools (Blue)
	ContextIdent   = "#F9E2AF" // @file context tokens & file picker (Yellow)
	CommandSuccess = "#A6E3A1" // /command palette & successful state (Green)
	DangerAlert    = "#F38BA8" // High-risk approval modal & errors (Red/Maroon)
	SideChatAccent = "#FAB387" // /btw ephemeral side-channel accent (Peach)
	MutedMeta      = "#6C7086" // Line numbers, timestamps, shortcuts (Overlay0)
	TextPrimary    = "#CDD6F4" // Primary readable text (Text)
)

// Alternate Screen Buffer, Mouse Wheel & Cursor ANSI Escape Sequences (PRD §2.2).
const (
	EnterAltScreenSeq = "\033[?1049h\033[H"
	ExitAltScreenSeq  = "\033[?1049l"
	EnableMouseSeq    = "\033[?1007l\033[?1000h\033[?1006h"
	DisableMouseSeq   = "\033[?1006l\033[?1000l"
	HideCursorSeq     = "\033[?25l"
	ShowCursorSeq     = "\033[?25h"
	ClearScreenSeq    = "\033[2J\033[H"
	ResetSeq          = "\033[0m"
)

// ColorEnabled reports whether TrueColor ANSI output should be emitted.
// It honors both NO_COLOR environment variable and ui.Enabled().
func ColorEnabled() bool {
	if v, ok := os.LookupEnv("NO_COLOR"); ok && v != "" {
		return false
	}
	return ui.Enabled()
}

// ParseHexRGB converts a "#RRGGBB" or "RRGGBB" string into 24-bit R, G, B values.
func ParseHexRGB(hex string) (uint8, uint8, uint8, bool) {
	clean := strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(clean) != 6 {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(clean, 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return uint8((v >> 16) & 0xFF), uint8((v >> 8) & 0xFF), uint8(v & 0xFF), true
}

// FgHex wraps text in a 24-bit TrueColor foreground ANSI SGR sequence.
func FgHex(hex, text string) string {
	if text == "" || !ColorEnabled() {
		return text
	}
	r, g, b, ok := ParseHexRGB(hex)
	if !ok {
		return text
	}
	return fmt.Sprintf("\033[38;2;%d;%d;%dm%s%s", r, g, b, text, ResetSeq)
}

// BgHex wraps text in a 24-bit TrueColor background ANSI SGR sequence.
func BgHex(hex, text string) string {
	if text == "" || !ColorEnabled() {
		return text
	}
	r, g, b, ok := ParseHexRGB(hex)
	if !ok {
		return text
	}
	return fmt.Sprintf("\033[48;2;%d;%d;%dm%s%s", r, g, b, text, ResetSeq)
}

// BoldFgHex wraps text in bold + 24-bit TrueColor foreground ANSI SGR sequence.
func BoldFgHex(hex, text string) string {
	if text == "" || !ColorEnabled() {
		return text
	}
	r, g, b, ok := ParseHexRGB(hex)
	if !ok {
		return text
	}
	return fmt.Sprintf("\033[1;38;2;%d;%d;%dm%s%s", r, g, b, text, ResetSeq)
}

// DimFgHex wraps text in faint/dimmed + 24-bit TrueColor foreground ANSI SGR sequence,
// used for live faded reasoning previews and secondary hints.
func DimFgHex(hex, text string) string {
	if text == "" || !ColorEnabled() {
		return text
	}
	r, g, b, ok := ParseHexRGB(hex)
	if !ok {
		return text
	}
	return fmt.Sprintf("\033[2;38;2;%d;%d;%dm%s%s", r, g, b, text, ResetSeq)
}

// StripANSI removes CSI ANSI escape sequences from s so raw log lines can be parsed cleanly.
func StripANSI(s string) string {
	if !strings.ContainsRune(s, '\x1b') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	inEsc := false
	for _, r := range s {
		if inEsc {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			inEsc = true
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// FgBgHex wraps text in both foreground and background 24-bit TrueColor SGR sequences.
func FgBgHex(fgHex, bgHex, text string) string {
	if text == "" || !ColorEnabled() {
		return text
	}
	fr, fg, fb, fok := ParseHexRGB(fgHex)
	br, bg, bb, bok := ParseHexRGB(bgHex)
	if !fok || !bok {
		return text
	}
	return fmt.Sprintf("\033[38;2;%d;%d;%d;48;2;%d;%d;%dm%s%s", fr, fg, fb, br, bg, bb, text, ResetSeq)
}

// BoldFgBgHex wraps text in bold + foreground + background 24-bit TrueColor SGR sequences.
func BoldFgBgHex(fgHex, bgHex, text string) string {
	if text == "" || !ColorEnabled() {
		return text
	}
	fr, fg, fb, fok := ParseHexRGB(fgHex)
	br, bg, bb, bok := ParseHexRGB(bgHex)
	if !fok || !bok {
		return text
	}
	return fmt.Sprintf("\033[1;38;2;%d;%d;%d;48;2;%d;%d;%dm%s%s", fr, fg, fb, br, bg, bb, text, ResetSeq)
}

// TruncateVisible truncates s so that its visible terminal column width does not
// exceed maxCols. ANSI SGR sequences are preserved and closed cleanly.
func TruncateVisible(s string, maxCols int) string {
	if maxCols <= 0 {
		return ""
	}
	if ui.VisibleLen(s) <= maxCols {
		return s
	}
	if maxCols <= 1 {
		return "…"
	}
	target := maxCols - 1
	var b strings.Builder
	cols := 0
	inEsc := false
	sawEsc := false
	for _, r := range s {
		if inEsc {
			b.WriteRune(r)
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			inEsc = true
			sawEsc = true
			b.WriteRune(r)
			continue
		}
		rw := ui.VisibleLen(string(r))
		if cols+rw > target {
			break
		}
		b.WriteRune(r)
		cols += rw
	}
	if sawEsc && ColorEnabled() {
		b.WriteString(ResetSeq)
	}
	b.WriteString("…")
	return b.String()
}

// PadOrTruncate ensures s occupies at most width visible columns and pads with
// trailing spaces if shorter.
func PadOrTruncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	truncated := TruncateVisible(s, width)
	vlen := ui.VisibleLen(truncated)
	if vlen < width {
		return truncated + strings.Repeat(" ", width-vlen)
	}
	return truncated
}

// RoundedBox renders a rounded-corner box (`╭─╮`, `│ │`, `╰─╯`) of exact total
// terminal width around the provided content lines.
func RoundedBox(title string, lines []string, width int, borderHex string) []string {
	if width < 4 {
		width = 4
	}
	innerWidth := width - 4 // "│ " + content + " │"
	if innerWidth < 1 {
		innerWidth = 1
	}

	var top string
	if strings.TrimSpace(title) == "" {
		top = FgHex(borderHex, "╭"+strings.Repeat("─", width-2)+"╮")
	} else {
		titleTrimmed := TruncateVisible(title, max(1, width-6))
		tLen := ui.VisibleLen(titleTrimmed)
		rem := (width - 2) - (tLen + 4) // "─ " + title + " ─"
		if rem < 0 {
			rem = 0
		}
		top = FgHex(borderHex, "╭─ ") + titleTrimmed + FgHex(borderHex, " "+strings.Repeat("─", rem)+"╮")
	}

	out := make([]string, 0, len(lines)+2)
	out = append(out, top)
	leftBar := FgHex(borderHex, "│ ")
	rightBar := FgHex(borderHex, " │")
	for _, l := range lines {
		out = append(out, leftBar+PadOrTruncate(l, innerWidth)+rightBar)
	}
	bottom := FgHex(borderHex, "╰"+strings.Repeat("─", width-2)+"╯")
	out = append(out, bottom)
	return out
}
