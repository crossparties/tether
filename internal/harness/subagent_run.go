package harness

// subagentRun is the record of one worker dispatch.
//
// It exists because a subagent used to be visible only as two lines on the
// terminal -- a header and a status word -- while everything that decided the
// outcome (how many steps, how long, whether a proposal was recorded) was
// discarded. The Summary method is the single place that renders those facts,
// so success and failure are reported the same way and a caller cannot print
// half of them.

import (
	"fmt"
	"strings"
	"time"

	"github.com/croosparties/tether/internal/core"
	"github.com/croosparties/tether/internal/ui"
)

type subagentRun struct {
	Role     string
	Status   core.HandoffStatus
	Steps    int
	Elapsed  time.Duration
	Proposal int64
	Analysis bool
	Err      error
}

// Summary renders the run as a single block.
//
// The rule is one block per dispatch, not one line per event: the worker's own
// output went to a buffer precisely so it cannot flood the main session, and a
// summary that dribbled out line by line would give that noise back.
func (s subagentRun) Summary() string {
	var b strings.Builder

	head := fmt.Sprintf("🤖 %s", s.Role)
	if s.Steps > 0 {
		head += fmt.Sprintf(" · %d steps", s.Steps)
	}
	if s.Elapsed > 0 {
		head += " · " + formatElapsed(s.Elapsed)
	}
	b.WriteString("\n")
	b.WriteString(ui.Rule(head))
	b.WriteString("\n")

	mark, detail := s.verdict()
	b.WriteString(fmt.Sprintf("   %s %s\n", mark, detail))
	b.WriteString("\n")
	return b.String()
}

// verdict turns the run's state into a mark and a one-line explanation. Failure
// outranks analysis: a run that errored is never reported as a clean audit.
func (s subagentRun) verdict() (string, string) {
	switch {
	case s.Err != nil:
		return ui.Error("✖"), ui.Error(s.Err.Error())

	case s.Status == core.StatusUnknown:
		return ui.Warn("?"), ui.Warn("no parseable STATUS in the report")

	case s.Status == core.StatusBlocked:
		return ui.Warn("⊘"), ui.Warn("BLOCKED — the worker could not proceed")

	case s.Status == core.StatusNeedsContext:
		return ui.Warn("?"), ui.Warn("NEEDS_CONTEXT — the worker needs more information")

	case s.Proposal > 0:
		return ui.Success("✓"), fmt.Sprintf("%s · proposal #%d awaiting approval",
			ui.Success(string(s.Status)), s.Proposal)

	case s.Analysis:
		return ui.Success("✓"), fmt.Sprintf("%s · read-only, nothing to approve",
			ui.Success(string(s.Status)))

	default:
		return ui.Success("✓"), ui.Success(string(s.Status))
	}
}

// statusFromReport extracts the status field from a worker's report.
//
// The whole point is that this is strict. cc-sdd's controller is told not to
// infer status from prose, and the reason is visible in the failure it avoids:
// a report that says "tests look good" has no STATUS field, so treating it as
// done would mark a task complete on the strength of an adjective. An error
// here means "not finished", never "finished by default".
func statusFromReport(report string) (core.HandoffStatus, error) {
	h, err := core.ParseHandoff(report)
	if err != nil {
		return core.StatusUnknown, err
	}
	if h.Status == core.StatusUnknown {
		return core.StatusUnknown, fmt.Errorf("report has no STATUS field")
	}
	return h.Status, nil
}
