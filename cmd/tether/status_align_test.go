package main

import (
	"strings"
	"testing"

	"github.com/crossparties/tether/internal/ui"
)

// TestStatusBoxStaysAlignedWithColour is the regression guard for the status
// dashboard: colouring text inside a box must not shift the right border,
// which is what happens when escapes are counted as printable characters.
func TestStatusBoxStaysAlignedWithColour(t *testing.T) {
	ui.SetEnabled(true)
	t.Cleanup(func() { ui.SetEnabled(false) })

	badge := ui.Success("🟢 [ CODE MODE ]") + "  (mutation allowed)"
	line := "│ Substrate Mode:   " + ui.Pad(badge, 54) + "│"

	// Compare against a plain rendering: both must end with the border at the
	// same printable column.
	if got, want := ui.VisibleLen(line), ui.VisibleLen("│ Substrate Mode:   ")+54+1; got != want {
		t.Fatalf("box line width = %d, want %d (%q)", got, want, line)
	}
	if !strings.HasSuffix(line, "│") {
		t.Fatalf("box line must close with a border, got %q", line)
	}
}
