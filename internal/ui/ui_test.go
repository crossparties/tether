package ui

import (
	"strings"
	"testing"
)

// withStyling forces the styling state for the duration of a test.
func withStyling(t *testing.T, on bool) {
	t.Helper()
	SetEnabled(on)
	t.Cleanup(ClearOverride)
}

func TestStylesApplyWhenEnabled(t *testing.T) {
	withStyling(t, true)

	cases := map[string]string{
		"Title":   Title("x"),
		"Success": Success("x"),
		"Warn":    Warn("x"),
		"Error":   Error("x"),
		"Dim":     Dim("x"),
		"Path":    Path("x"),
		"Blocked": Blocked("x"),
	}
	for name, got := range cases {
		if !strings.Contains(got, "\033[") {
			t.Errorf("%s did not emit an escape sequence: %q", name, got)
		}
		if !strings.HasSuffix(got, reset) {
			t.Errorf("%s did not reset styling: %q", name, got)
		}
	}
}

func TestStylesPlainWhenDisabled(t *testing.T) {
	withStyling(t, false)

	if got := Title("hello"); got != "hello" {
		t.Errorf("Title = %q, want plain text", got)
	}
	if got := Error("boom"); got != "boom" {
		t.Errorf("Error = %q, want plain text", got)
	}
}

func TestNoColorEnvDisablesStyling(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("FORCE_COLOR", "")
	ClearOverride()
	ResetDetection()
	t.Cleanup(ClearOverride)

	if Enabled() {
		t.Fatal("NO_COLOR must disable styling")
	}
	if got := Success("ok"); got != "ok" {
		t.Fatalf("Success = %q, want plain text under NO_COLOR", got)
	}
}

func TestDumbTermDisablesStyling(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("FORCE_COLOR", "")
	t.Setenv("TERM", "dumb")
	ClearOverride()
	ResetDetection()
	t.Cleanup(ClearOverride)

	if Enabled() {
		t.Fatal("TERM=dumb must disable styling")
	}
}

func TestForceColorEnablesStyling(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	t.Setenv("FORCE_COLOR", "1")
	ClearOverride()
	ResetDetection()
	t.Cleanup(ClearOverride)

	if !Enabled() {
		t.Fatal("FORCE_COLOR must enable styling")
	}
}

func TestBannerContainsLabelAndBody(t *testing.T) {
	withStyling(t, false)

	out := Banner("APPROVAL REQUIRED", "handler.go needs your decision", Success)
	for _, want := range []string{"APPROVAL REQUIRED", "handler.go needs your decision", "╔", "╚"} {
		if !strings.Contains(out, want) {
			t.Errorf("banner missing %q:\n%s", want, out)
		}
	}

	// Every rendered line must be the same width so the box stays aligned.
	lines := strings.Split(out, "\n")
	width := len([]rune(lines[0]))
	for _, line := range lines {
		if got := len([]rune(line)); got != width {
			t.Errorf("misaligned banner line (want %d, got %d): %q", width, got, line)
		}
	}
}

func TestBannerWithoutBody(t *testing.T) {
	withStyling(t, false)
	out := Banner("LOCKED", "", Warn)
	if !strings.Contains(out, "LOCKED") {
		t.Errorf("banner missing label: %s", out)
	}
}

func TestSplitWidthWrapsOnWords(t *testing.T) {
	lines := splitWidth("alpha beta gamma delta", 11)
	for _, line := range lines {
		if len([]rune(line)) > 11 {
			t.Errorf("line %q exceeds width 11", line)
		}
	}
	if len(lines) < 2 {
		t.Fatalf("expected the text to wrap, got %v", lines)
	}
}

func TestSplitWidthPreservesParagraphs(t *testing.T) {
	lines := splitWidth("one\n\ntwo", 20)
	joined := strings.Join(lines, "|")
	if !strings.Contains(joined, "one") || !strings.Contains(joined, "two") {
		t.Fatalf("content lost: %v", lines)
	}
}

// TestVisibleLenIgnoresEscapeCodes pins the reason Pad exists: boxed output
// must align even when the text inside carries colour.
func TestVisibleLenIgnoresEscapeCodes(t *testing.T) {
	SetEnabled(true)
	t.Cleanup(func() { SetEnabled(false) })

	coloured := Success("OK")
	if got := VisibleLen(coloured); got != 2 {
		t.Fatalf("VisibleLen(%q) = %d, want 2", coloured, got)
	}
	if got := VisibleLen("plain"); got != 5 {
		t.Fatalf("VisibleLen(plain) = %d, want 5", got)
	}
}

// TestPadAlignsColouredText is the regression guard for status boxes: a colour
// escape must not consume padding, otherwise the right border drifts.
func TestPadAlignsColouredText(t *testing.T) {
	SetEnabled(true)
	t.Cleanup(func() { SetEnabled(false) })

	padded := Pad(Success("OK"), 10)
	if got := VisibleLen(padded); got != 10 {
		t.Fatalf("padded width = %d, want 10", got)
	}
	if !strings.HasSuffix(strings.TrimRight(padded, " "), "OK") &&
		!strings.Contains(padded, "OK") {
		t.Fatalf("content lost from padded output: %q", padded)
	}

	// Plain text behaves like %-10s.
	if got := Pad("abc", 6); got != "abc   " {
		t.Fatalf("Pad without colour = %q, want %q", got, "abc   ")
	}
	// Already-wide input is untouched.
	if got := Pad("abcdef", 3); got != "abcdef" {
		t.Fatalf("Pad must not truncate, got %q", got)
	}
}
