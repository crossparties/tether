package views

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/crossparties/tether/internal/ui"
)

// Catppuccin Mocha palette constants used by view components.
const (
	HexBaseSurface    = "#1E1E2E"
	HexOverlaySurface = "#313244"
	HexBorders        = "#B4BEFE"
	HexMainAccent     = "#CBA6F7"
	HexSubAgentAccent = "#89B4FA"
	HexContextIdent   = "#F9E2AF"
	HexCommandSuccess = "#A6E3A1"
	HexDangerAlert    = "#F38BA8"
	HexSideChatAccent = "#FAB387"
	HexMutedMeta      = "#6C7086"
	HexTextPrimary    = "#CDD6F4"
)

// AutocompleteKind distinguishes `/` Command Palette from `@` Context/File Picker.
type AutocompleteKind int

const (
	AutocompleteNone AutocompleteKind = iota
	AutocompleteCommand
	AutocompleteFile
)

// AutocompleteItem represents a single entry in the floating `/` or `@` dropdown.
type AutocompleteItem struct {
	Token       string // e.g. "/btw" or "@internal/tui/model.go"
	Description string // e.g. "Diskusi sampingan tanpa ganggu task" or "Go • 4.2KB"
	NeedsArg    bool   // true when Tab completion should append a trailing space for arguments
}

// AutocompleteProps holds the rendering parameters for the floating dropdown overlay.
type AutocompleteProps struct {
	Kind          AutocompleteKind
	Query         string
	Items         []AutocompleteItem
	SelectedIndex int
	MaxVisible    int
	Width         int
}

func colorOn() bool {
	if v, ok := os.LookupEnv("NO_COLOR"); ok && v != "" {
		return false
	}
	return ui.Enabled()
}

func parseRGB(hex string) (uint8, uint8, uint8, bool) {
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

func fgHex(hex, s string) string {
	if s == "" || !colorOn() {
		return s
	}
	r, g, b, ok := parseRGB(hex)
	if !ok {
		return s
	}
	return fmt.Sprintf("\033[38;2;%d;%d;%dm%s\033[0m", r, g, b, s)
}

func boldFgHex(hex, s string) string {
	if s == "" || !colorOn() {
		return s
	}
	r, g, b, ok := parseRGB(hex)
	if !ok {
		return s
	}
	return fmt.Sprintf("\033[1;38;2;%d;%d;%dm%s\033[0m", r, g, b, s)
}

func dimFgHex(hex, s string) string {
	if s == "" || !colorOn() {
		return s
	}
	r, g, b, ok := parseRGB(hex)
	if !ok {
		return s
	}
	return fmt.Sprintf("\033[2;38;2;%d;%d;%dm%s\033[0m", r, g, b, s)
}

func fgBgHex(fg, bg, s string) string {
	if s == "" || !colorOn() {
		return s
	}
	fr, fgG, fb, fok := parseRGB(fg)
	br, bgG, bb, bok := parseRGB(bg)
	if !fok || !bok {
		return s
	}
	return fmt.Sprintf("\033[1;38;2;%d;%d;%d;48;2;%d;%d;%dm%s\033[0m", fr, fgG, fb, br, bgG, bb, s)
}

func truncateCols(s string, maxCols int) string {
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
	if sawEsc && colorOn() {
		b.WriteString("\033[0m")
	}
	b.WriteString("…")
	return b.String()
}

func padCols(s string, width int) string {
	if width <= 0 {
		return ""
	}
	t := truncateCols(s, width)
	v := ui.VisibleLen(t)
	if v < width {
		return t + strings.Repeat(" ", width-v)
	}
	return t
}

// RenderAutocomplete renders a clean, borderless inline picker for `/` commands, `@` files,
// or `/model` & `/provider` options. Only the active selection changes prefix (`> `) and
// foreground color (bold accent blue), matching Antigravity CLI (`agy`) visual simplicity.
func RenderAutocomplete(props AutocompleteProps) []string {
	if props.Kind == AutocompleteNone || len(props.Items) == 0 || props.Width < 16 {
		return nil
	}

	maxVis := props.MaxVisible
	if maxVis <= 0 {
		maxVis = 6
	}
	total := len(props.Items)
	sel := props.SelectedIndex
	if sel < 0 {
		sel = 0
	}
	if sel >= total {
		sel = total - 1
	}

	start := 0
	if sel >= maxVis {
		start = sel - maxVis + 1
	}
	end := start + maxVis
	if end > total {
		end = total
	}

	titleLabel := "Commands"
	if props.Kind == AutocompleteFile {
		titleLabel = "Files"
	}
	if props.Query != "" {
		titleLabel += fmt.Sprintf(" · %s", props.Query)
	}

	width := props.Width
	innerWidth := max(8, width-4)

	headerTrunc := truncateCols(titleLabel, max(1, width-8))
	hLen := ui.VisibleLen(headerTrunc)
	rem := max(0, width-hLen-5)

	lines := make([]string, 0, (end-start)+1)
	top := "  " + boldFgHex(HexContextIdent, headerTrunc) + " " + fgHex(HexMutedMeta, strings.Repeat("─", rem))
	lines = append(lines, padCols(top, width))

	tokenColWidth := innerWidth / 3
	if tokenColWidth < 14 {
		tokenColWidth = 14
	}
	if tokenColWidth > innerWidth-6 {
		tokenColWidth = innerWidth / 2
	}
	descColWidth := innerWidth - tokenColWidth - 3
	if descColWidth < 0 {
		descColWidth = 0
	}

	for idx := start; idx < end; idx++ {
		item := props.Items[idx]
		isSelected := idx == sel

		tokPlain := padCols(item.Token, tokenColWidth)
		descPlain := ""
		if descColWidth > 0 {
			descPlain = truncateCols(item.Description, descColWidth)
			if props.Kind == AutocompleteFile {
				dLen := ui.VisibleLen(descPlain)
				if dLen < descColWidth {
					descPlain = strings.Repeat(" ", descColWidth-dLen) + descPlain
				}
			} else {
				descPlain = padCols(descPlain, descColWidth)
			}
		}

		var row string
		if isSelected {
			row = "  " + boldFgHex(HexSubAgentAccent, "> "+tokPlain)
			if descColWidth > 0 {
				row += " " + fgHex(HexTextPrimary, descPlain)
			}
		} else {
			row = "    " + fgHex(HexTextPrimary, tokPlain)
			if descColWidth > 0 {
				row += " " + dimFgHex(HexMutedMeta, descPlain)
			}
		}
		lines = append(lines, padCols(row, width))
	}

	return lines
}
