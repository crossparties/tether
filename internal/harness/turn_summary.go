package harness

// This file makes every turn announce how it ended.
//
// Before, the REPL printed the answer on success and a bare "✖" on failure, and
// printed nothing at all when the process was interrupted. Those three cases
// looked identical at the bottom of the screen -- and identical again to "the
// model is still thinking" -- so the user could not tell whether the chat had
// finished.
//
// runTurn therefore always reports an outcome, including on a recovered panic
// and on Ctrl-C. It deliberately does NOT print the mark: the answer is printed
// by the caller after runTurn returns, so a mark printed inside the deferred
// block would appear above the text it is supposed to close.

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/crossparties/tether/internal/ui"
)

// turnResult is everything the caller needs to close a turn on screen.
type turnResult struct {
	answer   string
	err      error
	outcome  string // done | failed | interrupted | crashed
	elapsed  time.Duration
	panicMsg any
}

// runTurn executes one prompt and always reports an outcome, whatever happens.
//
// The deferred block is what guarantees the outcome: it runs on success, on
// error, and on a recovered panic, and the SIGINT handler is observed before
// returning so a forced stop is reported as interrupted rather than done.
func runTurn(r *Runner, ctx context.Context, line string) (res turnResult) {
	start := time.Now()
	res.outcome = "done"

	// Catch a forced stop. Without this, Ctrl-C kills the process mid-turn and
	// nothing is reported.
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	defer func() {
		if rec := recover(); rec != nil {
			// Record the panic and let the caller print both it and the mark, so
			// the two stay in order. Re-panicking would hide the mark entirely.
			res.panicMsg = rec
			res.outcome = "crashed"
			res.err = fmt.Errorf("panic: %v", rec)
		}
		res.elapsed = time.Since(start)
	}()

	res.answer, res.err = r.Run(sigCtx, line)
	switch {
	case res.err != nil:
		res.outcome = "failed"
	case sigCtx.Err() != nil:
		// Run returned after an interrupt was requested: report the
		// interruption even if it reported success.
		res.outcome = "interrupted"
	}
	return res
}

// DoneRule builds the closing line of a turn, e.g.
//
//	━━━━━━━━━━━━━━━━━━ done · 12.4s ━━━━━━━━━━━━━━━━━━
//
// It is the only output guaranteed to appear after a turn, so it must never be
// conditional on styling: ui.Rule degrades to plain text when colour is off.
// Exported because the one-shot CLI path renders the same mark without going
// through runTurn.
func DoneRule(outcome string, d time.Duration) string {
	return ui.Rule(fmt.Sprintf("%s · %s", outcome, formatElapsed(d)))
}

// formatElapsed keeps the summary short: sub-second turns read "0.4s", long
// ones "2m18s" rather than an unreadable decimal.
func formatElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	m := int(d / time.Minute)
	s := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dm%ds", m, s)
}
