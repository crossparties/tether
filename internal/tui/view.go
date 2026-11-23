package tui

import (
	"fmt"
	"strings"

	"github.com/crossparties/tether/internal/tui/views"
	"github.com/crossparties/tether/internal/ui"
)

// View renders the minimalist Antigravity-style (`agy`) 3-Zone Terminal Layout
// (Zone A: Compact Header & Agent Status, Zone B: Clean Borderless Stream,
// Zone C: Thin-Rule Input & Split Status Footer) within exact m.Width x m.Height.
func (m Model) View() string {
	width := m.Width
	if width <= 0 {
		width = 80
	} else if width < 24 {
		width = 24
	}
	height := m.Height
	if height <= 0 {
		height = 24
	} else if height < 8 {
		height = 8
	}

	zoneA := m.renderZoneA(width, height)
	maxZoneCRows := max(4, height-len(zoneA)-2)
	zoneC := m.renderZoneC(width, maxZoneCRows)

	remainingRows := height - len(zoneA) - len(zoneC)
	if remainingRows < 1 {
		remainingRows = 1
	}
	zoneB := m.renderZoneB(width, remainingRows)

	allLines := make([]string, 0, height)
	allLines = append(allLines, zoneA...)
	allLines = append(allLines, zoneB...)
	allLines = append(allLines, zoneC...)

	// Strictly enforce exact height and width so terminal resizing never overflows or scrolls the alt-screen.
	if len(allLines) > height {
		allLines = allLines[len(allLines)-height:]
	}
	for len(allLines) < height {
		allLines = append(allLines, strings.Repeat(" ", width))
	}
	for i, line := range allLines {
		allLines[i] = PadOrTruncate(line, width)
	}
	return strings.Join(allLines, "\n")
}

// renderZoneA renders the minimal top status bar and (only when active/present)
// compact 1-line live sub-agent process indicators, separated by a thin horizontal rule.
func (m Model) renderZoneA(width, totalHeight int) []string {
	provModel := m.ModelName
	if m.ProviderName != "" && m.ModelName != "" {
		provModel = m.ProviderName + "/" + m.ModelName
	} else if provModel == "" {
		provModel = "deepseek/deepseek-chat"
	}

	modeColor := CommandSuccess
	if strings.Contains(strings.ToUpper(m.Mode), "PLAN") {
		modeColor = ContextIdent
	}

	left := fmt.Sprintf("  %s %s%s",
		BoldFgHex(SubAgentAccent, "●"),
		BoldFgHex(TextPrimary, "tether"),
		FgHex(MutedMeta, " · "+shortenWorkspacePath(m.Workspace, max(10, width/3))),
	)
	right := fmt.Sprintf("%s %s ",
		FgHex(MutedMeta, provModel+" ·"),
		BoldFgHex(modeColor, strings.ToLower(m.Mode)),
	)
	headerLine := alignLeftRight(left, right, width)

	if totalHeight < 12 {
		return []string{PadOrTruncate(headerLine, width)}
	}

	out := []string{PadOrTruncate(headerLine, width)}
	out = append(out, PadOrTruncate(FgHex(MutedMeta, strings.Repeat("─", width)), width))
	return out
}

// renderZoneB renders the clean, boxless Antigravity-style conversation stream,
// faded live reasoning/activity preview, and inline permission prompts.
func (m Model) renderZoneB(width, rows int) []string {
	if m.Inspector.Active {
		vpRows := max(2, rows-2)
		lines := views.RenderInspector(views.InspectorProps{
			Title:        m.Inspector.Title,
			Lines:        m.Inspector.Lines,
			ScrollOffset: m.Inspector.ScrollOffset,
			ViewportRows: vpRows,
			Width:        width,
		})
		return fitLines(lines, width, rows, 0)
	}

	var rendered []string
	innerWidth := max(16, width-4)
	appendBlank := func() {
		if len(rendered) > 0 && strings.TrimSpace(StripANSI(rendered[len(rendered)-1])) != "" {
			rendered = append(rendered, "")
		}
	}

	for idx, entry := range m.Stream {
		switch entry.Kind {
		case StreamUserPrompt:
			if idx > 0 {
				appendBlank()
				ruleLen := min(24, max(8, width-4))
				rendered = append(rendered, PadOrTruncate("  "+FgHex(MutedMeta, strings.Repeat("─", ruleLen)), width))
			}
			firstLine := true
			for _, b := range entry.Body {
				for _, wl := range wrapLine(b, max(12, width-6)) {
					if firstLine {
						rendered = append(rendered, PadOrTruncate("  "+BoldFgHex(SubAgentAccent, "> "+wl), width))
						firstLine = false
					} else {
						rendered = append(rendered, PadOrTruncate("    "+BoldFgHex(SubAgentAccent, wl), width))
					}
				}
			}
			appendBlank()

		case StreamThought:
			rendered = append(rendered, PadOrTruncate("  "+FgHex(MutedMeta, "▸ "+entry.Title), width))
			if preview := strings.TrimSpace(entry.Summary); preview != "" {
				rendered = append(rendered, PadOrTruncate("    "+DimFgHex(MutedMeta, TruncateVisible(preview, max(12, width-6))), width))
			}

		case StreamTool:
			detail := strings.TrimSpace(entry.Summary)
			maxDetail := max(8, width-len(entry.Title)-9)
			toolLine := "  " + FgHex(CommandSuccess, "● ") + BoldFgHex(TextPrimary, entry.Title)
			if detail != "" {
				toolLine += FgHex(MutedMeta, " ("+TruncateVisible(detail, maxDetail)+")")
			}
			rendered = append(rendered, PadOrTruncate(toolLine, width))
			shownDiffs := entry.Body
			if len(shownDiffs) > 6 {
				shownDiffs = shownDiffs[:6]
			}
			for _, dl := range shownDiffs {
				lead := strings.TrimSpace(dl)
				plain := TruncateVisible(lead, max(10, width-6))
				switch {
				case strings.HasPrefix(lead, "+"):
					rendered = append(rendered, PadOrTruncate("    "+FgHex(CommandSuccess, plain), width))
				case strings.HasPrefix(lead, "-"):
					rendered = append(rendered, PadOrTruncate("    "+FgHex(DangerAlert, plain), width))
				default:
					rendered = append(rendered, PadOrTruncate("    "+DimFgHex(MutedMeta, plain), width))
				}
			}

		case StreamSideChat:
			appendBlank()
			bar := BoldFgHex(SideChatAccent, "▎ ")
			rendered = append(rendered, PadOrTruncate("  "+bar+BoldFgHex(SideChatAccent, entry.Title), width))
			for _, b := range entry.Body {
				for _, wl := range wrapLine(b, max(12, width-6)) {
					if strings.HasPrefix(strings.TrimSpace(wl), "> ") {
						rendered = append(rendered, PadOrTruncate("  "+FgHex(SideChatAccent, "▎ ")+BoldFgHex(SideChatAccent, wl), width))
					} else {
						rendered = append(rendered, PadOrTruncate("  "+FgHex(SideChatAccent, "▎ ")+FgHex(TextPrimary, wl), width))
					}
				}
			}
			appendBlank()

		case StreamSystemNotice:
			if strings.HasPrefix(entry.Title, "? ") {
				rendered = append(rendered, PadOrTruncate("  "+FgHex(MutedMeta, entry.Title), width))
				for _, b := range entry.Body {
					lower := strings.ToLower(b)
					if strings.Contains(lower, "reject") || strings.Contains(lower, "cancel") || strings.Contains(lower, "abort") {
						rendered = append(rendered, PadOrTruncate("  "+BoldFgHex(DangerAlert, b), width))
					} else {
						rendered = append(rendered, PadOrTruncate("  "+BoldFgHex(SubAgentAccent, b), width))
					}
				}
				continue
			}
			titleColor := TextPrimary
			if strings.HasPrefix(entry.Title, "✖") || strings.HasPrefix(entry.Title, "🛑") {
				titleColor = DangerAlert
			}
			if entry.Title != "" {
				rendered = append(rendered, PadOrTruncate("  "+BoldFgHex(titleColor, entry.Title), width))
			}
			for _, b := range entry.Body {
				for _, wl := range wrapLine(b, innerWidth) {
					rendered = append(rendered, PadOrTruncate("  "+FgHex(MutedMeta, wl), width))
				}
			}

		default: // StreamMainAgent — clean left-aligned prose & borderless code blocks
			appendBlank()
			inCodeFence := false
			for _, b := range entry.Body {
				trimmed := strings.TrimSpace(b)
				if strings.HasPrefix(trimmed, "```") {
					inCodeFence = !inCodeFence
					fenceLabel := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
					if inCodeFence {
						rendered = append(rendered, PadOrTruncate("  "+DimFgHex(MutedMeta, "─── "+nonEmpty(fenceLabel, "code")+" ───"), width))
					} else {
						rendered = append(rendered, PadOrTruncate("  "+DimFgHex(MutedMeta, "───────────"), width))
					}
					continue
				}
				if inCodeFence {
					rendered = append(rendered, PadOrTruncate("    "+FgHex(CommandSuccess, TruncateVisible(b, max(8, width-4))), width))
					continue
				}
				for _, wl := range wrapLine(b, innerWidth) {
					rendered = append(rendered, PadOrTruncate("  "+styleStreamLine(wl), width))
				}
			}
		}
	}

	// Inline Permission / Approval Prompt rendered directly at the bottom of the stream
	if m.Approval != nil {
		appendBlank()
		modalLines := views.RenderApprovalModal(views.ApprovalModalProps{
			ProposalID:     m.Approval.ProposalID,
			AgentName:      m.Approval.AgentName,
			ActionType:     m.Approval.ActionType,
			Target:         m.Approval.Target,
			ScopeHint:      m.Approval.ScopeHint,
			MainEvaluation: m.Approval.MainEvaluation,
			DiffLines:      m.Approval.DiffLines,
			SelectedOption: m.Approval.SelectedOption,
			Width:          width,
			MaxDiffLines:   max(2, rows-12),
		})
		rendered = append(rendered, modalLines...)
		return fitLines(rendered, width, rows, 0)
	}

	// Live faded reasoning / activity preview (`⠋ I'm checking...` + subtle tip)
	if len(m.Workers) > 0 && m.Workers[0].State == WorkerActive {
		sp := nonEmpty(m.Workers[0].Spinner, brailleFrames[m.SpinnerStep%len(brailleFrames)])
		faded := strings.TrimSpace(m.LiveStatus)
		if strings.TrimSpace(m.ReasoningBuf) != "" {
			if rp := latestReasoningPreview(m.ReasoningBuf); rp != "" {
				faded = rp
			}
		}
		if faded == "" {
			faded = m.Workers[0].Task
		}
		appendBlank()
		liveRow := fmt.Sprintf("  %s %s",
			BoldFgHex(SubAgentAccent, sp),
			DimFgHex(TextPrimary, TruncateVisible(faded, max(10, width-6))),
		)
		rendered = append(rendered, PadOrTruncate(liveRow, width))
		if m.liveStream == 0 && rows >= 8 {
			tip := "  " + DimFgHex(MutedMeta, "└ Tip: Use /btw for side questions or Ctrl+Y to drag-select text.")
			rendered = append(rendered, PadOrTruncate(tip, width))
		}
	}

	fitted := fitLines(rendered, width, rows, m.StreamScroll)
	if m.StreamScroll > 0 && len(fitted) > 0 && len(rendered) > rows {
		maxScroll := len(rendered) - rows
		shownScroll := m.StreamScroll
		if shownScroll > maxScroll {
			shownScroll = maxScroll
		}
		badge := fmt.Sprintf("  ▲ Scrolled +%d lines (PgDn / Mouse Wheel to return to bottom)", shownScroll)
		fitted[len(fitted)-1] = PadOrTruncate(BoldFgHex(ContextIdent, badge), width)
	}
	return fitted
}

// renderZoneC renders the Antigravity-style bottom input bar:
// borderless inline picker (if open), top thin rule, `> ` prompt, bottom thin rule,
// and split 1-line status footer (`esc to cancel` left, `<model> · <mode>` right).
func (m Model) renderZoneC(width, maxAllowedRows int) []string {
	var out []string

	if m.IsDropdownOpen() && maxAllowedRows >= 6 {
		maxDropdownItems := min(m.MaxDropdownRows, min(6, maxAllowedRows-5))
		if maxDropdownItems >= 1 {
			overlay := views.RenderAutocomplete(views.AutocompleteProps{
				Kind:          m.DropdownKind,
				Query:         m.DropdownQuery,
				Items:         m.DropdownItems,
				SelectedIndex: m.DropdownIndex,
				MaxVisible:    maxDropdownItems,
				Width:         width,
			})
			out = append(out, overlay...)
		}
	}

	rule := PadOrTruncate(FgHex(MutedMeta, strings.Repeat("─", width)), width)
	out = append(out, rule)

	promptPrefix := "  " + BoldFgHex(SubAgentAccent, "> ")
	if m.AwaitingKey {
		promptPrefix = "  " + BoldFgHex(ContextIdent, fmt.Sprintf("[%s key] > ", m.ProviderName))
	}
	displayInput := renderInputWithCursor(m.InputBuffer, m.CursorPos, m.AwaitingKey)
	out = append(out, PadOrTruncate(promptPrefix+displayInput, width))
	out = append(out, rule)

	// Split Footer: contextual shortcut hint on the left, model · mode on the right
	busy := false
	for _, w := range m.Workers {
		if w.State == WorkerActive {
			busy = true
			break
		}
	}

	var leftHint string
	switch {
	case m.Inspector.Active:
		leftHint = "  ↑/↓ scroll · pgup/pgdn page · esc close"
	case m.Approval != nil:
		leftHint = "  ↑/↓ navigate · a always · s session · d diff · enter confirm · esc reject"
	case m.IsDropdownOpen():
		leftHint = "  ↑/↓ navigate · tab complete · enter select · esc close"
	case busy:
		leftHint = "  esc to cancel · /btw side chat · ctrl+y select"
	case !m.MouseCapture:
		leftHint = "  SELECT MODE ON (drag to copy) · ctrl+y scroll mode"
	default:
		leftHint = "  / commands · @ files · /btw side chat · ctrl+y select"
	}

	modeColor := CommandSuccess
	if strings.Contains(strings.ToUpper(m.Mode), "PLAN") {
		modeColor = ContextIdent
	}
	modelLabel := nonEmpty(m.ModelName, "deepseek-chat")
	rightMeta := FgHex(MutedMeta, modelLabel+" · ") + FgHex(modeColor, strings.ToLower(m.Mode)) + "  "

	out = append(out, PadOrTruncate(alignLeftRight(FgHex(MutedMeta, leftHint), rightMeta, width), width))
	return out
}

func alignLeftRight(left, right string, width int) string {
	rLen := ui.VisibleLen(right)
	if rLen >= width-4 {
		return TruncateVisible(left, width)
	}
	maxLeft := max(4, width-rLen-2)
	leftTrunc := TruncateVisible(left, maxLeft)
	lLen := ui.VisibleLen(leftTrunc)
	gap := width - lLen - rLen
	if gap < 1 {
		gap = 1
	}
	return leftTrunc + strings.Repeat(" ", gap) + right
}

func renderInputWithCursor(input string, cursorPos int, masked bool) string {
	if input == "" {
		if masked {
			return DimFgHex(MutedMeta, "Paste API key and press Enter…") + BoldFgHex(SubAgentAccent, "█")
		}
		return DimFgHex(MutedMeta, "Ask anything, / for commands, @ for files…") + BoldFgHex(SubAgentAccent, "█")
	}
	if masked {
		runes := []rune(input)
		return FgHex(CommandSuccess, strings.Repeat("•", len(runes))) + BoldFgHex(SubAgentAccent, "█")
	}
	runes := []rune(input)
	if cursorPos < 0 {
		cursorPos = 0
	}
	if cursorPos > len(runes) {
		cursorPos = len(runes)
	}
	before := string(runes[:cursorPos])
	after := string(runes[cursorPos:])
	return highlightTokens(before) + BoldFgHex(SubAgentAccent, "█") + highlightTokens(after)
}

func highlightTokens(s string) string {
	if s == "" {
		return ""
	}
	parts := strings.Split(s, " ")
	for i, p := range parts {
		switch {
		case strings.HasPrefix(p, "/"):
			parts[i] = BoldFgHex(SubAgentAccent, p)
		case strings.HasPrefix(p, "@") && len(p) > 1:
			parts[i] = BoldFgHex(ContextIdent, p)
		default:
			parts[i] = FgHex(TextPrimary, p)
		}
	}
	return strings.Join(parts, " ")
}

func styleStreamLine(line string) string {
	trimmed := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(trimmed, "#"):
		return BoldFgHex(TextPrimary, strings.TrimLeft(trimmed, "# "))
	case strings.HasPrefix(trimmed, "|") && strings.HasSuffix(trimmed, "|"):
		return FgHex(SubAgentAccent, line)
	case strings.HasPrefix(trimmed, "✓") || strings.HasPrefix(trimmed, "✅"):
		return FgHex(CommandSuccess, line)
	case strings.HasPrefix(trimmed, "🛑") || strings.HasPrefix(trimmed, "✖"):
		return FgHex(DangerAlert, line)
	case strings.HasPrefix(trimmed, "🔧") || strings.HasPrefix(trimmed, "💭"):
		return DimFgHex(MutedMeta, line)
	default:
		if strings.Contains(line, "**") {
			segs := strings.Split(line, "**")
			var b strings.Builder
			for idx, seg := range segs {
				if idx%2 == 1 {
					b.WriteString(BoldFgHex(TextPrimary, seg))
				} else {
					b.WriteString(FgHex(TextPrimary, seg))
				}
			}
			return b.String()
		}
		return FgHex(TextPrimary, line)
	}
}

func wrapLine(s string, maxCols int) []string {
	s = strings.ReplaceAll(s, "\t", "  ")
	if maxCols <= 8 || ui.VisibleLen(s) <= maxCols {
		return []string{s}
	}
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, "|") || strings.HasPrefix(trimmed, "├─") || strings.HasPrefix(trimmed, "└─") {
		return []string{TruncateVisible(s, maxCols)}
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{""}
	}
	var lines []string
	cur := ""
	for _, w := range words {
		if cur == "" {
			cur = w
			continue
		}
		if ui.VisibleLen(cur)+1+ui.VisibleLen(w) <= maxCols {
			cur += " " + w
		} else {
			lines = append(lines, cur)
			cur = w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

func shortenWorkspacePath(p string, maxLen int) string {
	r := []rune(p)
	if len(r) <= maxLen {
		return p
	}
	return "…" + string(r[len(r)-(maxLen-1):])
}

func fitLines(lines []string, width, targetRows, scrollUp int) []string {
	if targetRows <= 0 {
		return nil
	}
	if len(lines) > targetRows {
		end := len(lines) - scrollUp
		if end < targetRows {
			end = targetRows
		}
		if end > len(lines) {
			end = len(lines)
		}
		start := end - targetRows
		if start < 0 {
			start = 0
		}
		lines = lines[start:end]
	}
	out := make([]string, 0, targetRows)
	for _, l := range lines {
		out = append(out, PadOrTruncate(l, width))
	}
	blank := strings.Repeat(" ", width)
	for len(out) < targetRows {
		out = append(out, blank)
	}
	return out
}
