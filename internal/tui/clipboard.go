package tui

import (
	"encoding/base64"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// toggleMouseCapture flips between wheel-scroll mode (the TUI owns the mouse) and
// native selection mode (the terminal owns the mouse so drag-select + copy work).
func (m Model) toggleMouseCapture() Model {
	m.MouseCapture = !m.MouseCapture
	title, body := "🖱  [Scroll Mode]", "Mouse wheel scrolls the stream. Hold Shift while dragging to select, or press Ctrl+Y for select mode."
	if !m.MouseCapture {
		title, body = "✂  [Select Mode]", "Drag with the mouse to select and copy text natively. Press Ctrl+Y again to restore wheel scrolling (PgUp/PgDn always work)."
	}
	m.Stream = append(m.Stream, StreamEntry{Kind: StreamSystemNotice, Title: title, Body: []string{body}})
	m.StreamScroll = 0
	return m
}

// collectCopyText returns the text for /copy. "" = last agent answer, "all" = whole
// transcript, "code" = contents of the last fenced code block.
func (m Model) collectCopyText(mode string) string {
	switch mode {
	case "all":
		var b strings.Builder
		for _, e := range m.Stream {
			b.WriteString(e.Title)
			b.WriteByte('\n')
			b.WriteString(strings.Join(e.Body, "\n"))
			b.WriteString("\n\n")
		}
		return strings.TrimSpace(b.String())
	case "code":
		for i := len(m.Stream) - 1; i >= 0; i-- {
			if code := lastCodeFence(m.Stream[i].Body); code != "" {
				return code
			}
		}
		return ""
	default:
		for i := len(m.Stream) - 1; i >= 0; i-- {
			e := m.Stream[i]
			if e.Kind == StreamMainAgent || e.Kind == StreamSideChat {
				return strings.Join(e.Body, "\n")
			}
		}
		return ""
	}
}

func lastCodeFence(body []string) string {
	var cur, last []string
	in := false
	for _, l := range body {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			if in {
				last = cur
				cur = nil
			}
			in = !in
			continue
		}
		if in {
			cur = append(cur, l)
		}
	}
	return strings.Join(last, "\n")
}

// writeClipboard sends text to the system clipboard. OSC 52 works across ssh/tmux in
// most modern terminals; wl-copy/xclip/xsel/pbcopy cover terminals that block OSC 52.
func writeClipboard(out io.Writer, text string) {
	if text == "" {
		return
	}
	enc := base64.StdEncoding.EncodeToString([]byte(text))
	// Limit OSC 52 payload; many terminals reject very large sequences.
	if len(enc) <= 100000 {
		fmt.Fprintf(out, "\033]52;c;%s\a", enc)
	}
	for _, c := range [][]string{
		{"wl-copy"},
		{"xclip", "-selection", "clipboard"},
		{"xsel", "--clipboard", "--input"},
		{"pbcopy"},
	} {
		path, err := exec.LookPath(c[0])
		if err != nil {
			continue
		}
		cmd := exec.Command(path, c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if cmd.Run() == nil {
			return
		}
	}
}
