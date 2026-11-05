package preview

import (
	"strings"
	"testing"
)

// TestDiffShowsRemovedAndAdded is the core promise: the user sees what changes.
func TestDiffShowsRemovedAndAdded(t *testing.T) {
	got := Diff("alpha\nbeta\ngamma\n", "alpha\nBETA\ngamma\n")

	if !strings.Contains(got, "- beta") {
		t.Errorf("diff must show the removed line, got:\n%s", got)
	}
	if !strings.Contains(got, "+ BETA") {
		t.Errorf("diff must show the added line, got:\n%s", got)
	}
}

// TestDiffKeepsContextNearby proves the reader can locate the change.
func TestDiffKeepsContextNearby(t *testing.T) {
	before := "line1\nline2\nline3\ntarget\nline5\nline6\nline7\n"
	after := "line1\nline2\nline3\nTARGET\nline5\nline6\nline7\n"
	got := Diff(before, after)

	if !strings.Contains(got, "line3") {
		t.Errorf("expected context above the change, got:\n%s", got)
	}
	if !strings.Contains(got, "line5") {
		t.Errorf("expected context below the change, got:\n%s", got)
	}
}

// TestDiffIsEmptyForIdenticalInput keeps previews free of noise.
func TestDiffIsEmptyForIdenticalInput(t *testing.T) {
	if got := Diff("same\n", "same\n"); got != "" {
		t.Fatalf("identical content must produce no diff, got %q", got)
	}
}

// TestDiffCollapsesHugeChanges prevents one enormous rewrite from flooding the
// transcript and hiding how big it is.
func TestDiffCollapsesHugeChanges(t *testing.T) {
	var before, after strings.Builder
	for i := 0; i < 200; i++ {
		before.WriteString("old line\n")
		after.WriteString("new line\n")
	}
	got := Diff(before.String(), after.String())

	if lines := strings.Count(got, "\n") + 1; lines > maxDiffLines+10 {
		t.Errorf("diff not capped: %d lines", lines)
	}
	if !strings.Contains(got, "more changed lines") {
		t.Errorf("capped diff should report the remainder, got:\n%s", got)
	}
}

// TestRenderCreateAnswersWhatAndWhere pins the two questions the approval
// prompt must answer.
func TestRenderCreateAnswersWhatAndWhere(t *testing.T) {
	c := Change{
		Kind:  KindCreate,
		Path:  "docs/design.md",
		After: "hello\nworld\n",
	}
	out := c.Render()

	for _, want := range []string{"docs/design.md", "new file", "+ hello", "+ world"} {
		if !strings.Contains(out, want) {
			t.Errorf("preview missing %q, got:\n%s", want, out)
		}
	}
}

// TestRenderUpdateReportsLineCounts gives a sense of scale before deciding.
func TestRenderUpdateReportsLineCounts(t *testing.T) {
	c := Change{
		Kind:   KindUpdate,
		Path:   "main.go",
		Before: "a\nb\n",
		After:  "a\nc\nd\n",
	}
	out := c.Render()

	if !strings.Contains(out, "+2") || !strings.Contains(out, "-1") {
		t.Errorf("expected +2/-1 summary, got:\n%s", out)
	}
}

// TestRenderCommandShowsCommandAndDir answers what runs and where.
func TestRenderCommandShowsCommandAndDir(t *testing.T) {
	c := Change{Kind: KindCommand, Before: "go   test ./...", Detail: "workspace"}
	out := c.Render()

	if !strings.Contains(out, "go test ./...") {
		t.Errorf("command should be whitespace-normalized, got:\n%s", out)
	}
	if !strings.Contains(out, "workspace") {
		t.Errorf("preview should state the working directory, got:\n%s", out)
	}
}

// TestRelativeToKeepsWorkspacePathsShort avoids leaking absolute layouts.
func TestRelativeToKeepsWorkspacePathsShort(t *testing.T) {
	if got := RelativeTo("/home/u/proj", "/home/u/proj/src/a.go"); got != "src/a.go" {
		t.Errorf("RelativeTo = %q, want src/a.go", got)
	}
	// Outside the workspace the absolute path is kept, since a relative path
	// starting with .. would be ambiguous.
	if got := RelativeTo("/home/u/proj", "/etc/passwd"); got != "/etc/passwd" {
		t.Errorf("outside path should stay absolute, got %q", got)
	}
}
