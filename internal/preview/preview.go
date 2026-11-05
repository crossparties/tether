// Package preview builds a human-readable summary of a proposed change so the
// user can decide whether to approve it.
//
// The point is to answer three questions before anything is written: what file,
// what changes, and how much of it. A preview is deliberately textual: it must
// survive being shown in a terminal, a chat transcript, or a diff viewer without
// losing meaning.
package preview

import (
	"fmt"
	"path/filepath"
	"strings"
)

// maxDiffLines caps how many changed lines are shown. Beyond this the preview
// reports a count instead, so a huge rewrite cannot flood the transcript and
// hide the fact that something enormous is about to happen.
const maxDiffLines = 40

// maxContextLines is how many unchanged lines surround each hunk, enough to
// orient the reader without turning the preview into the whole file.
const maxContextLines = 2

// Kind identifies the sort of operation being previewed.
type Kind string

const (
	// KindCreate means the file does not exist yet.
	KindCreate Kind = "create"
	// KindUpdate means an existing file will be modified in place.
	KindUpdate Kind = "update"
	// KindCommand means a shell command will run.
	KindCommand Kind = "command"
)

// Change is one proposed filesystem edit.
type Change struct {
	Kind      Kind
	Path      string
	Before    string
	After     string
	HasBefore bool
	// Detail carries extra context, for example the exact target string of an
	// edit or the working directory of a command.
	Detail string
}

// Render produces the user-facing preview. It never fails: an unreadable input
// yields a short descriptive line rather than an error, because a preview must
// always be shown.
func (c Change) Render() string {
	var b strings.Builder

	switch c.Kind {
	case KindCreate:
		fmt.Fprintf(&b, "  File:    %s (new file)\n", c.Path)
		fmt.Fprintf(&b, "  Size:    %s\n", describeSize(len(c.After)))
		fmt.Fprintf(&b, "  Lines:   +%d\n", countLines(c.After))
	case KindUpdate:
		fmt.Fprintf(&b, "  File:    %s (modify)\n", c.Path)
		added, removed := countDiff(c.Before, c.After)
		fmt.Fprintf(&b, "  Changes: +%d / -%d lines\n", added, removed)
	case KindCommand:
		fmt.Fprintf(&b, "  Command: %s\n", oneline(c.Before))
		if c.Detail != "" {
			fmt.Fprintf(&b, "  In:      %s\n", c.Detail)
		}
		return strings.TrimRight(b.String(), "\n")
	}

	if c.Detail != "" {
		fmt.Fprintf(&b, "  Target:  %s\n", c.Detail)
	}

	diff := Diff(c.Before, c.After)
	if diff != "" {
		b.WriteString("\n")
		b.WriteString(indent(diff, "    "))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// Summary is a single line suitable for a list of many changes.
func (c Change) Summary() string {
	switch c.Kind {
	case KindCreate:
		return fmt.Sprintf("+ %s (new, %d lines)", c.Path, countLines(c.After))
	case KindUpdate:
		added, removed := countDiff(c.Before, c.After)
		return fmt.Sprintf("~ %s (+%d/-%d)", c.Path, added, removed)
	case KindCommand:
		return fmt.Sprintf("$ %s", oneline(c.Before))
	}
	return c.Path
}

// Diff renders a unified-ish line diff between before and after. It is
// intentionally simple: unchanged stretches collapse to a marker, and context
// around each change is kept short. Good enough to judge a change, cheap enough
// to run on every write.
func Diff(before, after string) string {
	if before == after {
		return ""
	}

	beforeLines := splitLines(before)
	afterLines := splitLines(after)

	// Trim common prefix and suffix so the interesting region is centred.
	start := 0
	for start < len(beforeLines) && start < len(afterLines) && beforeLines[start] == afterLines[start] {
		start++
	}
	endBefore, endAfter := len(beforeLines), len(afterLines)
	for endBefore > start && endAfter > start && beforeLines[endBefore-1] == afterLines[endAfter-1] {
		endBefore--
		endAfter--
	}

	var out []string
	emitted := 0

	// Leading context.
	ctxStart := start - maxContextLines
	if ctxStart < 0 {
		ctxStart = 0
	}
	if ctxStart < start {
		if ctxStart > 0 {
			out = append(out, "  ...")
		}
		for _, line := range beforeLines[ctxStart:start] {
			out = append(out, "  "+line)
		}
	}

	// Removed lines.
	for _, line := range beforeLines[start:endBefore] {
		if emitted >= maxDiffLines {
			out = append(out, fmt.Sprintf("  ... (%d more changed lines)", (endBefore-start)+(endAfter-start)-emitted))
			emitted = -1
			break
		}
		out = append(out, "- "+line)
		emitted++
	}
	// Added lines.
	if emitted >= 0 {
		for _, line := range afterLines[start:endAfter] {
			if emitted >= maxDiffLines {
				out = append(out, fmt.Sprintf("  ... (%d more changed lines)", (endBefore-start)+(endAfter-start)-emitted))
				emitted = -1
				break
			}
			out = append(out, "+ "+line)
			emitted++
		}
	}

	// Trailing context.
	if emitted >= 0 {
		ctxEnd := endAfter + maxContextLines
		if ctxEnd > len(afterLines) {
			ctxEnd = len(afterLines)
		}
		for _, line := range afterLines[endAfter:ctxEnd] {
			out = append(out, "  "+line)
		}
		if ctxEnd < len(afterLines) {
			out = append(out, "  ...")
		}
	}

	return strings.Join(out, "\n")
}

// countDiff returns how many lines were added and removed.
func countDiff(before, after string) (added, removed int) {
	b := splitLines(before)
	a := splitLines(after)

	start := 0
	for start < len(b) && start < len(a) && b[start] == a[start] {
		start++
	}
	endB, endA := len(b), len(a)
	for endB > start && endA > start && b[endB-1] == a[endA-1] {
		endB--
		endA--
	}
	return endA - start, endB - start
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	// A trailing newline yields a final empty element that is not a real line.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func countLines(s string) int {
	return len(splitLines(s))
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

func oneline(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// describeSize renders a byte count in a compact human form.
func describeSize(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

// RelativeTo renders path relative to workspace when possible, so previews do
// not leak the absolute layout of the user's machine.
func RelativeTo(workspace, path string) string {
	rel, err := filepath.Rel(workspace, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return filepath.ToSlash(rel)
}
