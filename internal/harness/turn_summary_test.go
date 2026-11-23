package harness

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// The closing mark is the only output guaranteed to appear after a turn, so its
// shape is pinned here.

// TestRunTurnReportsWithoutPrinting pins the ordering contract that a real run
// exposed: the mark must be printed by the caller AFTER the answer, so runTurn
// itself must not write it. When the mark was printed inside runTurn's deferred
// block it appeared above the reply it was supposed to close.
func TestRunTurnReportsWithoutPrinting(t *testing.T) {
	var buf bytes.Buffer
	r := &Runner{cfg: Config{Out: &buf}}

	res := runTurn(r, context.Background(), "irrelevant")
	if res.elapsed <= 0 {
		t.Errorf("runTurn must always measure elapsed time, got %v", res.elapsed)
	}
	if res.outcome == "" {
		t.Errorf("runTurn must always report an outcome")
	}
	if buf.Len() != 0 {
		t.Errorf("runTurn must not print; the caller orders the mark after the answer, got:\n%s", buf.String())
	}
}

func TestDoneRuleNamesTheOutcome(t *testing.T) {
	for _, outcome := range []string{"done", "failed", "interrupted", "crashed"} {
		got := DoneRule(outcome, 1500*time.Millisecond)
		if !strings.Contains(got, outcome) {
			t.Errorf("rule for %q must name the outcome, got %q", outcome, got)
		}
		if !strings.Contains(got, "1.5s") {
			t.Errorf("rule for %q must carry elapsed time, got %q", outcome, got)
		}
		if !strings.HasPrefix(got, "━") {
			t.Errorf("rule for %q must be a visible rule, got %q", outcome, got)
		}
	}
}

func TestFormatElapsed(t *testing.T) {
	cases := map[time.Duration]string{
		400 * time.Millisecond:           "0.4s",
		2 * time.Second:                  "2.0s",
		59 * time.Second:                 "59.0s",
		61 * time.Second:                 "1m1s",
		(2*time.Minute + 18*time.Second): "2m18s",
	}
	for d, want := range cases {
		if got := formatElapsed(d); got != want {
			t.Errorf("formatElapsed(%v) = %q, want %q", d, got, want)
		}
	}
}
