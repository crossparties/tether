package ui

// Tests for the arrow-key picker. The interesting cases are the negative ones:
// a picker that takes over a pipe, or one that leaves the terminal in raw mode
// after an error, does more damage than a menu that simply refuses.

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestPickRefusesNonTerminal is the load-bearing test. Piped input is how the
// e2e approval tests and `printf '1\n' | tether` work; if Pick did not refuse
// here it would consume the pipe and the approval gate would hang.
func TestPickRefusesNonTerminal(t *testing.T) {
	choices := []Choice{{Label: "approve"}, {Label: "reject"}}

	// A buffered writer is not an *os.File, so IsTerminalFile reports false.
	var out bytes.Buffer
	if idx, ok := Pick(os.Stdin, &out, choices, 0); ok {
		t.Fatalf("Pick on non-terminal = (%d, true), want refusal", idx)
	}
	if out.Len() != 0 {
		t.Errorf("Pick wrote to a non-terminal: %q", out.String())
	}
}

func TestPickRefusesEmptyList(t *testing.T) {
	var out bytes.Buffer
	if idx, ok := Pick(os.Stdin, &out, nil, 0); ok {
		t.Fatalf("Pick with no choices = (%d, true), want refusal", idx)
	}
}

// TestPickClampsStartIndex guards the wrap arithmetic: an out-of-range start
// would index past the slice on the first Up press.
func TestPickClampsStartIndex(t *testing.T) {
	choices := []Choice{{Label: "a"}, {Label: "b"}, {Label: "c"}}
	// No terminal, so Pick returns before drawing; this only asserts it does not
	// panic while normalising start.
	for _, start := range []int{-5, 0, 2, 99} {
		var out bytes.Buffer
		_, _ = Pick(os.Stdin, &out, choices, start)
	}
}

// TestIsTerminalFileRejectsBuffers pins the distinction the caller relies on.
func TestIsTerminalFileRejectsBuffers(t *testing.T) {
	var buf bytes.Buffer
	if IsTerminalFile(&buf) {
		t.Error("bytes.Buffer reported as terminal")
	}
	if IsTerminalFile(nil) {
		t.Error("nil writer reported as terminal")
	}
}

// TestStripANSIWidth keeps the helper honest: the picker's own cursor codes
// must not count toward the label width used to align rows.
func TestStripANSIWidth(t *testing.T) {
	plain := "1) approve once"
	styled := "\x1b[1m1) approve once\x1b[0m"
	if got := stripANSI(styled); got != plain {
		t.Errorf("stripANSI = %q, want %q", got, plain)
	}
	if VisibleLen(stripANSI(styled)) != VisibleLen(plain) {
		t.Error("styled and plain labels must measure the same width")
	}
}

// TestDrawChoicesMarksSelection checks the marker and that no row is silently
// dropped -- a picker that hides options is worse than one without arrows.
func TestDrawChoicesMarksSelection(t *testing.T) {
	choices := []Choice{
		{Label: "Approve once", Detail: "apply this change only"},
		{Label: "Reject", Detail: "discard it"},
	}
	var out bytes.Buffer
	drawChoices(&out, choices, 1, 0)
	got := stripANSI(out.String())

	for _, want := range []string{"Approve once", "Reject", "apply this change only", "discard it"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	// The selected row must be the one marked, not the first row by default.
	if !strings.Contains(got, "> 2)") {
		t.Errorf("selected row not marked:\n%s", got)
	}
	if strings.Contains(got, "> 1)") {
		t.Errorf("unselected row marked:\n%s", got)
	}
}
