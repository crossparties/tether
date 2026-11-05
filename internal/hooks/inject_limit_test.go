package hooks

import (
	"fmt"
	"strings"
	"testing"
)

// The injection budget exists because everything injected is re-sent on every
// turn of every session. These tests pin the limit at the source so a future
// refactor cannot quietly reintroduce an unbounded query.

func TestInjectDecisionsRespectsLimit(t *testing.T) {
	e := newTestEngine(t)
	for i := 0; i < 20; i++ {
		if err := e.AddDecision(fmt.Sprintf("topic-%02d", i), "d", "r"); err != nil {
			t.Fatalf("AddDecision: %v", err)
		}
	}

	got, err := e.InjectDecisions(injectDecisionLimit)
	if err != nil {
		t.Fatalf("InjectDecisions: %v", err)
	}
	if len(got) != injectDecisionLimit {
		t.Fatalf("got %d decisions, want %d", len(got), injectDecisionLimit)
	}
	if got[0].Topic != "topic-19" {
		t.Errorf("newest decision = %q, want topic-19", got[0].Topic)
	}
}

func TestInjectPatternsRespectsLimit(t *testing.T) {
	e := newTestEngine(t)
	for i := 0; i < 20; i++ {
		if err := e.AddPattern("do", fmt.Sprintf("pattern-%02d", i)); err != nil {
			t.Fatalf("AddPattern: %v", err)
		}
	}

	got, err := e.InjectPatterns(injectPatternLimit)
	if err != nil {
		t.Fatalf("InjectPatterns: %v", err)
	}
	if len(got) != injectPatternLimit {
		t.Fatalf("got %d patterns, want %d", len(got), injectPatternLimit)
	}
	if got[0].Description != "pattern-19" {
		t.Errorf("newest pattern = %q, want pattern-19", got[0].Description)
	}
}

// The archive must stay whole even though injection is capped: capping the
// prompt must never cost the agent access to history.
func TestFullArchiveStillReachable(t *testing.T) {
	e := newTestEngine(t)
	for i := 0; i < 12; i++ {
		if err := e.AddDecision(fmt.Sprintf("topic-%02d", i), "d", "r"); err != nil {
			t.Fatalf("AddDecision: %v", err)
		}
		if err := e.AddPattern("do", fmt.Sprintf("pattern-%02d", i)); err != nil {
			t.Fatalf("AddPattern: %v", err)
		}
	}

	if ds, err := e.GetDecisions(); err != nil || len(ds) != 12 {
		t.Fatalf("GetDecisions = %d entries, %v; want 12, nil", len(ds), err)
	}
	if ps, err := e.GetPatterns(); err != nil || len(ps) != 12 {
		t.Fatalf("GetPatterns = %d entries, %v; want 12, nil", len(ps), err)
	}
}

func TestTruncate(t *testing.T) {
	short := "hello"
	if got := truncate(short, 300); got != short {
		t.Errorf("truncate(%q, 300) = %q, want unchanged", short, got)
	}

	got := truncate(strings.Repeat("x", 500), 300)
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated text should end with an ellipsis, got %q", got)
	}
	if runes := []rune(got); len(runes) != 303 {
		t.Errorf("truncated length = %d runes, want 303 (300 + ellipsis)", len(runes))
	}

	// Multi-byte input must not be split mid-character.
	if runes := []rune(truncate(strings.Repeat("é", 400), 300)); len(runes) != 303 {
		t.Errorf("multi-byte truncation produced %d runes, want 303", len(runes))
	}
}

// TestPreInvocationContextIsBounded locks down the whole injected block, not
// just one field. This is the regression guard for "the prompt grows without
// bound": every source is stuffed with oversized entries, and the rendered
// context still has to fit in a fixed budget.
func TestPreInvocationContextIsBounded(t *testing.T) {
	e := newTestEngine(t)

	for i := 0; i < 40; i++ {
		if err := e.AddDecision(fmt.Sprintf("topic-%02d", i),
			strings.Repeat("d", 2000), strings.Repeat("r", 2000)); err != nil {
			t.Fatalf("AddDecision: %v", err)
		}
		if err := e.AddPattern("avoid", strings.Repeat("p", 2000)); err != nil {
			t.Fatalf("AddPattern: %v", err)
		}
		if err := e.AddContext("main", "assistant", strings.Repeat("c", 2000)); err != nil {
			t.Fatalf("AddContext: %v", err)
		}
	}

	h := NewHandler(e)
	out := h.handlePreInvocation(HookInput{})
	size := len(out.InjectSteps)
	for _, s := range out.InjectSteps {
		size += len(s.EphemeralMessage)
	}
	if size > 12*1024 {
		t.Fatalf("injected context = %d bytes, want <= 12KB; the budget leaked", size)
	}
}
