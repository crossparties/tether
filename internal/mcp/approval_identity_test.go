package mcp

import (
	"strings"
	"testing"

	"github.com/croosparties/tether/internal/preview"
)

// A run of blocked calls used to print the same opening words and the same
// boilerplate every time, so the reader could not tell one request from the
// next or whether they had already answered it. These tests pin the identity
// markers that make requests distinguishable.

func changeFor(path, after string) *mutation {
	return &mutation{
		tool:  "write_file",
		paths: []string{path},
		scope: path,
		change: preview.Change{
			Kind:   preview.KindCreate,
			Path:   path,
			After:  after,
			Before: "",
		},
	}
}

// Two different changes must be numbered differently and carry different
// fingerprints, so a reader can say which one they are looking at.
func TestApprovalRequestsAreDistinguishable(t *testing.T) {
	s, _ := newApprovalTestServer(t)

	first := s.approvalRequest(changeFor("a.txt", "one"))
	second := s.approvalRequest(changeFor("b.txt", "two"))

	if first == second {
		t.Fatal("two different requests rendered identically")
	}
	if !strings.Contains(first, "APPROVAL REQUIRED #1") {
		t.Errorf("first request should be numbered #1, got:\n%s", first)
	}
	if !strings.Contains(second, "APPROVAL REQUIRED #2") {
		t.Errorf("second request should be numbered #2, got:\n%s", second)
	}
}

// Re-asking about an identical change must be visibly a repeat, not a fresh
// request: that is how a user knows the guard is asking again rather than
// presenting something new.
func TestRepeatedApprovalIsMarkedAsRepeat(t *testing.T) {
	s, _ := newApprovalTestServer(t)

	first := s.approvalRequest(changeFor("a.txt", "same"))
	again := s.approvalRequest(changeFor("a.txt", "same"))

	if strings.Contains(first, "repeated request") {
		t.Errorf("first ask must not be labelled a repeat:\n%s", first)
	}
	if !strings.Contains(again, "repeated request") {
		t.Errorf("second identical ask should be labelled a repeat:\n%s", again)
	}
	// The fingerprint must match, proving it is the same change.
	fpFirst := approvalFingerprint(t, first)
	fpAgain := approvalFingerprint(t, again)
	if fpFirst != fpAgain {
		t.Errorf("same change produced different fingerprints: %s vs %s", fpFirst, fpAgain)
	}
}

// The block must start and end with a visible rule, so the next tool's output
// cannot be read as part of the approval text and one request cannot be
// confused with the next.
func TestApprovalBlockIsFenced(t *testing.T) {
	s, _ := newApprovalTestServer(t)

	out := s.approvalRequest(changeFor("a.txt", "x"))
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	first, last := lines[0], lines[len(lines)-1]

	if !strings.Contains(first, "approval #1") {
		t.Fatalf("opening rule should carry the request identity, got:\n%s", first)
	}
	if !strings.Contains(last, "end of approval #1") {
		t.Fatalf("closing rule should carry the request identity, got:\n%s", last)
	}
	for _, l := range []string{first, last} {
		if !strings.Contains(l, "━") {
			t.Fatalf("fence lines must contain rule characters, got:\n%s", l)
		}
	}
}

// approvalFingerprint pulls the 8-hex-char fingerprint out of a rendered block.
func approvalFingerprint(t *testing.T, rendered string) string {
	t.Helper()
	const marker = "APPROVAL REQUIRED #"
	i := strings.Index(rendered, marker)
	if i < 0 {
		t.Fatalf("no approval header in:\n%s", rendered)
	}
	rest := rendered[i+len(marker):]
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		t.Fatalf("malformed approval header in:\n%s", rendered)
	}
	return fields[1] // fields[0] is "#N", fields[1] is the fingerprint
}
