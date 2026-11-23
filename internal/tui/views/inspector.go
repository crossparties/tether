package views

import (
	"fmt"
	"strings"

	"github.com/croosparties/tether/internal/ui"
)

// InspectorProps defines the state for the scrollable viewport inspector
// used by `View full diff` and `/read <path>`.
type InspectorProps struct {
	Title        string   // e.g. "DIFF • src/auth/jwt.go" or "READ • internal/tui/model.go"
	Lines        []string // Full document or unified diff lines
	ScrollOffset int      // Top visible line (0-indexed)
	ViewportRows int      // Number of content rows to display
	Width        int      // Total terminal width
}

// RenderInspector renders a clean, side-borderless scrollable file/diff viewport
// with line numbers so multi-line code selection never captures right-edge walls.
func RenderInspector(props InspectorProps) []string {
	width := props.Width
	if width < 20 {
		width = 20
	}
	innerWidth := max(8, width-2)

	vpRows := props.ViewportRows
	if vpRows <= 0 {
		vpRows = 8
	}

	total := len(props.Lines)
	offset := props.ScrollOffset
	if offset > total-vpRows {
		offset = total - vpRows
	}
	if offset < 0 {
		offset = 0
	}

	end := offset + vpRows
	if end > total {
		end = total
	}

	title := props.Title
	if strings.TrimSpace(title) == "" {
		title = "Inspector"
	}
	meta := fmt.Sprintf("lines %d-%d of %d", minInt(total, offset+1), end, total)
	headerPlain := fmt.Sprintf("%s (%s)", title, meta)
	headerTrunc := truncateCols(headerPlain, max(4, width-8))
	rem := max(0, width-ui.VisibleLen(headerTrunc)-5)

	out := make([]string, 0, vpRows+2)
	top := "  " + boldFgHex(HexContextIdent, headerTrunc) + " " + fgHex(HexMutedMeta, strings.Repeat("─", rem))
	out = append(out, padCols(top, width))

	for i := 0; i < vpRows; i++ {
		lineIdx := offset + i
		if lineIdx >= total {
			out = append(out, strings.Repeat(" ", width))
			continue
		}
		raw := strings.TrimRight(props.Lines[lineIdx], "\r\n")
		raw = strings.ReplaceAll(raw, "\t", "  ")
		numPrefix := fmt.Sprintf("  %4d │ ", lineIdx+1)
		contentWidth := max(1, innerWidth-len(numPrefix)+2)
		contentTrunc := truncateCols(raw, contentWidth)

		var styledContent string
		trimmed := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(trimmed, "+"):
			styledContent = fgHex(HexCommandSuccess, contentTrunc)
		case strings.HasPrefix(trimmed, "-"):
			styledContent = fgHex(HexDangerAlert, contentTrunc)
		case strings.HasPrefix(trimmed, "@@"):
			styledContent = fgHex(HexSubAgentAccent, contentTrunc)
		default:
			styledContent = fgHex(HexTextPrimary, contentTrunc)
		}

		row := fgHex(HexMutedMeta, numPrefix) + styledContent
		out = append(out, padCols(row, width))
	}

	bottom := fgHex(HexMutedMeta, strings.Repeat("─", width))
	out = append(out, padCols(bottom, width))
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
