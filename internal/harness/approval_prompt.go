package harness

// This file connects the approval gate to the person at the keyboard.
//
// The gate in internal/mcp can only refuse: it renders an instruction and
// returns it as a tool result, which reaches the model and never the user.
// That is why blocking a write used to leave no question on screen. The asker
// below closes the loop by reading the answer from the same input stream as the
// chat, so a blocked mutation becomes a real prompt.
//
// Only the interactive REPL installs an asker. Hook and JSON-RPC callers leave
// it unset and keep the refuse-with-instructions behaviour, because there is no
// human attached to those streams and a prompt would corrupt the protocol.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/crossparties/tether/internal/mcp"
	"github.com/crossparties/tether/internal/safety"
	"github.com/crossparties/tether/internal/ui"
)

// approvalOption is one row of the approval menu.
type approvalOption struct {
	key     string
	aliases []string
	label   string
	detail  string
	verdict mcp.Verdict
	viewAll bool // show the full diff instead of deciding
}

// approvalOptions is the menu shown for a blocked change. Nothing is
// preselected: an empty line selects nothing, so a stray Enter cannot authorise
// a write. The options deliberately span the safe end of the range, and
// "Approve everything" is third rather than first so the cheapest answer to
// reach is "once".
var approvalOptions = []approvalOption{
	{key: "1", aliases: []string{"y", "yes"}, label: "Approve once", detail: "apply this change only [y]", verdict: mcp.VerdictOnce},
	{key: "2", aliases: []string{"a", "folder"}, label: "Approve this folder", detail: "stop asking for this folder [a]", verdict: mcp.VerdictFolder},
	{key: "3", aliases: []string{"s", "all"}, label: "Approve everything", detail: "apply every change without asking [s]", verdict: mcp.VerdictSession},
	{key: "4", aliases: []string{"d", "v", "diff"}, label: "View full diff", detail: "show every changed line, then ask again [d]", viewAll: true},
	{key: "5", aliases: []string{"n", "no", "r"}, label: "Reject", detail: "discard it and tell the model why [n]", verdict: mcp.VerdictReject},
}

// newAsker returns a function that renders a blocked change and reads a choice.
//
// There are two ways to answer. On a terminal, the options are an arrow-key
// picker (up/down to move, Enter to confirm) modelled on kirocrew's approval
// prompt. When stdin or stdout is not a TTY -- a pipe, a test harness, a CI job
// -- the picker declines and the original numbered line prompt runs instead, so
// `printf 'fix it\n1\n' | tether` keeps working.
//
// Both paths return the same verdict and both share scanner with the chat loop,
// so a piped answer still lands in the order the user typed it.
func newAsker(out io.Writer, scanner *bufio.Scanner) func(mcp.Decision) (mcp.Verdict, bool) {
	return func(d mcp.Decision) (mcp.Verdict, bool) {
		// PRD §3.6 Tier-1 Low-Risk Auto-Approval: read-only inspection commands
		// (date, ls, pwd, git status, git diff, etc.) are auto-approved by Main Agent
		// with a non-blocking green log line.
		if d.Tool == "run_command" {
			if ok, reason := safety.IsReadOnlyCommand(d.Command); ok {
				fmt.Fprintf(out, "  %s %s\n", ui.Success("✓ [Auto-Approved]"), ui.Dim(reason))
				return mcp.VerdictOnce, true
			}
			if ok, reason := safety.IsValidationCommand(d.Command); ok {
				fmt.Fprintf(out, "  %s %s\n", ui.Success("✓ [Auto-Approved]"), ui.Dim(reason))
				return mcp.VerdictOnce, true
			}
		}

		if ui.IsTerminal(os.Stdin) && ui.IsTerminalFile(out) {
			renderDecisionPreview(out, d)
			if v, ok := pickVerdict(out, d); ok {
				return v, true
			}
			// Ctrl-C in the interactive picker cancels/rejects the mutation.
			return mcp.VerdictReject, true
		}

		renderDecision(out, d)

		for {
			fmt.Fprintf(out, "\n  %s  ", ui.Dim("Select [1-5] (y/a/d/n, Enter keeps waiting):"))
			if !scanner.Scan() {
				// Input closed while asking. Reject rather than hang, and reject
				// rather than approve silently.
				return mcp.VerdictReject, true
			}

			choice := strings.TrimSpace(scanner.Text())
			if choice == "" {
				// An empty line selects nothing. The question stays open.
				continue
			}

			opt, ok := lookupOption(choice)
			if !ok {
				fmt.Fprintf(out, "  %s unknown choice %q\n", ui.Warn("✖"), choice)
				continue
			}
			if opt.viewAll {
				fmt.Fprintln(out)
				fmt.Fprintln(out, fullPreview(d))
				continue
			}
			return opt.verdict, true
		}
	}
}

// pickVerdict offers the decision options as an arrow-key menu.
//
// Selecting "View full diff" prints the complete untruncated diff and re-opens
// the picker below it so the user never has to approve a change blind.
//
// ok is false when there is no terminal to draw on or the user cancelled.
func pickVerdict(out io.Writer, d mcp.Decision) (mcp.Verdict, bool) {
	choices := make([]ui.Choice, len(approvalOptions))
	for i, o := range approvalOptions {
		choices[i] = ui.Choice{Label: o.label, Detail: o.detail}
	}

	for {
		// Reading the stdin as an *os.File is what the picker needs to switch the
		// terminal mode; a scanner in front of it would already have buffered bytes
		// and swallow the first keypress.
		idx, ok := ui.Pick(os.Stdin, out, choices, 0)
		if !ok {
			// Verdict is an int whose zero value is VerdictOnce, so the caller must
			// pair this with the false return. Returning a zero Verdict alone would
			// read as "approve once" -- the worst possible default for a failure.
			return 0, false
		}
		chosen := approvalOptions[idx]
		if chosen.viewAll {
			fmt.Fprintln(out)
			fmt.Fprintln(out, ui.Dim(fullPreview(d)))
			fmt.Fprintln(out)
			continue
		}
		return chosen.verdict, true
	}
}

// fullPreview returns the untruncated diff when present, or the standard
// preview otherwise.
func fullPreview(d mcp.Decision) string {
	if d.FullPreview != "" {
		return d.FullPreview
	}
	return d.Preview
}

// lookupOption resolves a typed key or letter alias to a menu row.
func lookupOption(choice string) (approvalOption, bool) {
	norm := strings.ToLower(strings.TrimSpace(choice))
	for _, o := range approvalOptions {
		if o.key == norm {
			return o, true
		}
		for _, a := range o.aliases {
			if a == norm {
				return o, true
			}
		}
	}
	return approvalOption{}, false
}

// renderDecisionPreview prints the fenced header and collapsed preview without
// the static numbered rows (used when the interactive picker draws the rows).
func renderDecisionPreview(out io.Writer, d mcp.Decision) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.Warn(ui.Rule(fmt.Sprintf("%s wants to change something", d.Tool))))
	if d.Label != "" {
		fmt.Fprintf(out, "  %s\n", ui.Path(d.Label))
	}
	if d.Preview != "" {
		fmt.Fprintln(out, ui.Dim(d.Preview))
	}
	fmt.Fprintln(out)
}

// renderDecision prints what is about to change, fenced so it cannot be
// mistaken for ordinary output.
func renderDecision(out io.Writer, d mcp.Decision) {
	renderDecisionPreview(out, d)
	for _, o := range approvalOptions {
		fmt.Fprintf(out, "    %s) %-20s %s\n", o.key, o.label, ui.Dim(o.detail))
	}
	fmt.Fprintln(out, ui.Warn(ui.Rule(fmt.Sprintf("end of %s request", d.Tool))))
}
