package views

import (
	"fmt"
	"strings"

	"github.com/crossparties/tether/internal/ui"
)

// ApprovalOptionIndex enumerates the 6 interactive choices in the Two-Tiered Approval Prompt.
type ApprovalOptionIndex int

const (
	ApprovalApproveAndRun ApprovalOptionIndex = iota
	ApprovalApproveFolder
	ApprovalApproveSession
	ApprovalViewFullDiff
	ApprovalRejectAction
	ApprovalAbortTask
)

// ApprovalOptionLabels holds the 6 interactive choices in the inline permission prompt.
var ApprovalOptionLabels = [6]string{
	"Yes, approve once",
	"Yes, always allow in this session",
	"Yes, allow all in session",
	"View full diff",
	"No, reject action",
	"Cancel task",
}

// ApprovalOptionLabel returns the display label for choice, specializing the
// standing-grant option with scopeHint when present.
func ApprovalOptionLabel(choice ApprovalOptionIndex, scopeHint string) string {
	if choice < 0 || int(choice) >= len(ApprovalOptionLabels) {
		return ""
	}
	if choice == ApprovalApproveFolder && strings.TrimSpace(scopeHint) != "" {
		return "Yes, always allow " + strings.TrimSpace(scopeHint)
	}
	return ApprovalOptionLabels[choice]
}

// ApprovalModalProps holds the state required to render the inline permission prompt.
type ApprovalModalProps struct {
	ProposalID     int64
	AgentName      string   // e.g. "Worker-2 (Refactor)"
	ActionType     string   // e.g. "edit_file" or "run_command"
	Target         string   // e.g. "src/auth/jwt.go" or "git push"
	ScopeHint      string   // Optional standing grant description
	MainEvaluation string   // Optional safety/risk evaluation reason
	DiffLines      []string // Unified +/- diff or command preview lines
	SelectedOption int      // 0..5 corresponding to ApprovalOptionLabels
	Width          int
	MaxDiffLines   int
}

// RenderApprovalModal renders an Antigravity-style (`agy`) minimal, boxless inline permission
// prompt. Only the active option receives `> ` and bold accent blue (`#89B4FA`) foreground
// highlighting, with zero side-wall borders (`║`/`│`) so copy-paste and resizing stay clean.
func RenderApprovalModal(props ApprovalModalProps) []string {
	width := props.Width
	if width < 24 {
		width = 24
	}
	contentWidth := max(12, width-4)

	maxDiff := props.MaxDiffLines
	if maxDiff <= 0 {
		maxDiff = 5
	}

	actionLower := strings.ToLower(strings.TrimSpace(props.ActionType))
	category := "Permission"
	question := "Allow this action?"
	switch {
	case strings.Contains(actionLower, "command") || strings.Contains(actionLower, "exec") || strings.Contains(actionLower, "shell"):
		category = "Command"
		question = "Run this command?"
	case strings.Contains(actionLower, "edit") || strings.Contains(actionLower, "write"):
		category = "Edit File"
		question = "Apply this change?"
	}
	if props.ProposalID > 0 {
		category = fmt.Sprintf("Proposal #%d", props.ProposalID)
	}

	target := strings.TrimSpace(props.Target)
	if target == "" {
		target = "(workspace)"
	}

	var out []string
	addLine := func(s string) {
		out = append(out, padCols(s, width))
	}

	// 1. Minimal header label + thin horizontal rule
	catTrunc := truncateCols(category, max(4, width/3))
	rem := max(0, width-ui.VisibleLen(catTrunc)-4)
	addLine("  " + boldFgHex(HexContextIdent, catTrunc) + " " + fgHex(HexMutedMeta, strings.Repeat("─", rem)))

	// 2. Request target & subtle evaluation context
	addLine("  " + fgHex(HexMutedMeta, "Requesting permission for: ") + boldFgHex(HexTextPrimary, truncateCols(target, max(8, contentWidth-27))))
	if eval := strings.TrimSpace(props.MainEvaluation); eval != "" {
		addLine("  " + dimFgHex(HexMutedMeta, truncateCols(eval, contentWidth)))
	}

	// 3. Clean indented diff / command preview (zero box walls)
	if len(props.DiffLines) > 0 {
		shown := props.DiffLines
		extra := 0
		if len(shown) > maxDiff {
			extra = len(shown) - maxDiff
			shown = shown[:maxDiff]
		}
		for _, dl := range shown {
			trimmed := strings.TrimRight(dl, "\r\n")
			plain := truncateCols(trimmed, max(8, contentWidth-2))
			lead := strings.TrimSpace(trimmed)
			switch {
			case strings.HasPrefix(lead, "+"):
				addLine("    " + fgHex(HexCommandSuccess, plain))
			case strings.HasPrefix(lead, "-"):
				addLine("    " + fgHex(HexDangerAlert, plain))
			default:
				addLine("    " + fgHex(HexMutedMeta, plain))
			}
		}
		if extra > 0 {
			addLine("    " + dimFgHex(HexMutedMeta, fmt.Sprintf("… (+%d lines hidden — press d to view full diff)", extra)))
		}
	}

	// 4. Question + inline color-highlighted numbered options (only active selection turns blue)
	addLine("  " + boldFgHex(HexTextPrimary, question))

	sel := props.SelectedOption
	if sel < 0 || sel >= len(ApprovalOptionLabels) {
		sel = 0
	}
	for idx := range ApprovalOptionLabels {
		label := ApprovalOptionLabel(ApprovalOptionIndex(idx), props.ScopeHint)
		numbered := truncateCols(fmt.Sprintf("%d. %s", idx+1, label), max(12, contentWidth-2))
		if idx == sel {
			addLine("  " + boldFgHex(HexSubAgentAccent, "> "+numbered))
		} else {
			addLine("    " + fgHex(HexMutedMeta, numbered))
		}
	}

	return out
}
