package harness

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/croosparties/tether/internal/mcp"
)

// These tests drive the asker through real piped input. They exist because the
// bug that prompted this work was a plumbing gap, not a logic error: the menu
// could be perfectly written and still never appear.

func ask(t *testing.T, input string) (mcp.Verdict, bool, string) {
	t.Helper()
	var out bytes.Buffer
	scanner := bufio.NewScanner(strings.NewReader(input))
	asker := newAsker(&out, scanner)
	v, ok := asker(mcp.Decision{
		Tool:    "write_file",
		Path:    "internal/a.go",
		Label:   "internal/a.go",
		Preview: "+ new line",
	})
	return v, ok, out.String()
}

func TestAskerRejectsByChoice(t *testing.T) {
	v, ok, out := ask(t, "5\n")
	if !ok || v != mcp.VerdictReject {
		t.Fatalf("choice 5 must reject, got ok=%v verdict=%v", ok, v)
	}
	if !strings.Contains(out, "Approve once") || !strings.Contains(out, "Reject") {
		t.Fatalf("menu must be shown, got:\n%s", out)
	}
	if !strings.Contains(out, "write_file wants to change something") {
		t.Fatalf("heading must name the tool, got:\n%s", out)
	}
}

func TestAskerApproveOnce(t *testing.T) {
	v, ok, _ := ask(t, "1\n")
	if !ok || v != mcp.VerdictOnce {
		t.Fatalf("choice 1 must approve once, got ok=%v verdict=%v", ok, v)
	}
}

// TestAskerEmptyLineSelectsNothing is the behaviour the user asked for: a blank
// line must not authorise anything, and the question must stay open.
func TestAskerEmptyLineSelectsNothing(t *testing.T) {
	v, ok, out := ask(t, "\n\n\n5\n")
	if !ok || v != mcp.VerdictReject {
		t.Fatalf("empty lines must be ignored and 5 must reject, got ok=%v verdict=%v", ok, v)
	}
	if strings.Count(out, "Select [1-5]") != 4 {
		t.Fatalf("each empty line should reprompt, menu prompts:\n%s", out)
	}
}

func TestAskerViewDiffThenDecide(t *testing.T) {
	v, ok, out := ask(t, "4\n1\n")
	if !ok || v != mcp.VerdictOnce {
		t.Fatalf("viewing the diff then approving must approve, got ok=%v verdict=%v", ok, v)
	}
	if strings.Count(out, "+ new line") != 2 {
		t.Fatalf("diff should render twice (menu + full view), got:\n%s", out)
	}
}

func TestAskerUnknownChoiceReprompts(t *testing.T) {
	v, ok, out := ask(t, "9\n1\n")
	if !ok || v != mcp.VerdictOnce {
		t.Fatalf("an unknown choice must reprompt, got ok=%v verdict=%v", ok, v)
	}
	if !strings.Contains(out, `unknown choice "9"`) {
		t.Fatalf("unknown input must be reported, got:\n%s", out)
	}
}

// TestAskerClosedInputRejects pins the EOF behaviour: never approve on behalf of
// a user who is no longer there.
func TestAskerClosedInputRejects(t *testing.T) {
	v, ok, _ := ask(t, "")
	if !ok || v != mcp.VerdictReject {
		t.Fatalf("closed input must reject, got ok=%v verdict=%v", ok, v)
	}
}
