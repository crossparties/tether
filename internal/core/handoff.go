package core

// Handoff parsing for subagent reports.
//
// The protocol is ported from gotalab/cc-sdd v3.0.2, where the controller
// parses exact fields and is told not to infer status from prose. The reason
// that discipline exists becomes obvious the first time an agent writes
// "tests look good, should be fine" and a controller treats it as success.
//
// Two rules make this parser trustworthy, and both are fail-closed:
//
//  1. A status only counts when it appears as a field line, "STATUS: X" or
//     "- STATUS: X". The word "STATUS:" inside a sentence, inside a fenced
//     code block, or as part of another word is not a field.
//  2. An unrecognised value is an error, never a default. StatusUnknown is the
//     zero value, so a report that was never parsed cannot be mistaken for a
//     success by a caller that forgot to check the error.

import (
	"fmt"
	"regexp"
	"strings"
)

// HandoffStatus is the implementer's verdict on its own work.
type HandoffStatus string

const (
	// StatusUnknown is the zero value. It is deliberately not a valid outcome:
	// a report that failed to parse must not read as success.
	StatusUnknown HandoffStatus = ""

	StatusReadyForReview HandoffStatus = "READY_FOR_REVIEW"
	StatusBlocked        HandoffStatus = "BLOCKED"
	StatusNeedsContext   HandoffStatus = "NEEDS_CONTEXT"
)

// ReviewVerdict is the reviewer's decision about a handoff.
type ReviewVerdict string

const (
	VerdictUnknown  ReviewVerdict = ""
	VerdictApproved ReviewVerdict = "APPROVED"
	VerdictRejected ReviewVerdict = "REJECTED"
)

// MaxDebugRounds caps how many fresh-context debug attempts may run before the
// task is marked blocked. cc-sdd stops at two for the same reason: a debug
// agent that sees the failed implementation history tends to repeat it, so
// rounds beyond the second mostly burn context.
const MaxDebugRounds = 2

// Handoff is a parsed subagent report.
type Handoff struct {
	Status  HandoffStatus
	Verdict ReviewVerdict
	// Boundary is the _Boundary:_ annotation: the component or path scope the
	// subagent was allowed to modify.
	Boundary string
	// Evidence is the _EVIDENCE:_ line, if present. It is kept verbatim
	// because "which command proved it" is the part reviewers ask for.
	Evidence string
	// Raw is the original report, kept so a rejection can quote what was
	// actually said instead of a summary of it.
	Raw string
}

// fieldLine matches a status or verdict field at the start of a line.
//
// It deliberately allows both "STATUS: X" and "- STATUS: X": cc-sdd's own
// templates write the dash form, but hand-written reports often omit it, and
// accepting both costs nothing. What it does NOT allow is an indented field
// inside a code block, which is why the block stripping below runs first.
var fieldLine = regexp.MustCompile(`(?m)^[ \t]*(?:[-*][ \t]+)?([A-Z_]+):[ \t]*(.+?)[ \t]*$`)

// fencedBlock matches ``` ... ``` regions, which are documentation examples
// rather than fields. A report that quotes the protocol must not be parsed as
// if it were obeying it.
var fencedBlock = regexp.MustCompile("(?s)```.*?```")

// ParseHandoff extracts the structured fields from a subagent report.
//
// It returns an error when the report carries no parseable status, when the
// status is not a known value, or when a status and a verdict contradict each
// other. The caller gets no default status to fall back on.
func ParseHandoff(report string) (Handoff, error) {
	h := Handoff{Raw: report}

	scan := fencedBlock.ReplaceAllString(report, "")
	if strings.TrimSpace(scan) == "" {
		return h, fmt.Errorf("handoff: report contains only fenced blocks, no fields")
	}

	sawField := false
	for _, m := range fieldLine.FindAllStringSubmatch(scan, -1) {
		key, val := m[1], strings.TrimSpace(m[2])
		switch key {
		case "STATUS":
			sawField = true
			switch HandoffStatus(val) {
			case StatusReadyForReview, StatusBlocked, StatusNeedsContext:
				h.Status = HandoffStatus(val)
			default:
				return h, fmt.Errorf("handoff: unrecognised STATUS %q (want READY_FOR_REVIEW, BLOCKED or NEEDS_CONTEXT)", val)
			}
		case "VERDICT":
			sawField = true
			switch ReviewVerdict(val) {
			case VerdictApproved, VerdictRejected:
				h.Verdict = ReviewVerdict(val)
			default:
				return h, fmt.Errorf("handoff: unrecognised VERDICT %q (want APPROVED or REJECTED)", val)
			}
		case "BOUNDARY":
			h.Boundary = val
		case "EVIDENCE":
			h.Evidence = val
		}
	}

	if !sawField {
		// This is the case the protocol exists to prevent: a confident report
		// with no machine-readable outcome. Painting it green is how a broken
		// change gets merged.
		return h, fmt.Errorf("handoff: no STATUS or VERDICT field found; prose is not a verdict")
	}
	if h.Status != StatusUnknown && h.Verdict != VerdictUnknown {
		return h, fmt.Errorf("handoff: report carries both STATUS and VERDICT; an implementer reports status, a reviewer reports a verdict, not both")
	}
	return h, nil
}

// IsSuccess reports whether the handoff clears the work. It is explicit so a
// caller cannot accidentally treat the zero value as success: an unparsed
// handoff is never a success.
func (h Handoff) IsSuccess() bool {
	return h.Status == StatusReadyForReview || h.Verdict == VerdictApproved
}

// BoundaryAllows reports whether p lies inside the handoff's declared
// _Boundary:_ scope. An empty boundary means the annotation was absent, which
// is treated as "no declared scope" and therefore no authorisation: a subagent
// that did not say where it may write does not get to write anywhere.
//
// The path semantics are those of scopeCoversPath, the same function the
// approval-grant gate uses. Sharing it matters: two ideas of "inside this
// directory" is how a boundary check and a grant check end up disagreeing.
func (h Handoff) BoundaryAllows(p string) bool {
	if strings.TrimSpace(h.Boundary) == "" {
		return false
	}
	return scopeCoversPath(h.Boundary, p)
}

// DebugExhausted reports whether the debug round budget is spent. Keeping the
// comparison here means the limit is enforced the same way everywhere, and a
// future call site cannot quietly pass a larger number.
func DebugExhausted(rounds int) bool {
	return rounds >= MaxDebugRounds
}
