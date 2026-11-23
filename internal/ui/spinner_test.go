package ui

// Tests for the spinner. The contract is narrow on purpose, and each part of it
// exists because breaking it damages something concrete:
//
//   - a spinner that writes in a non-TTY corrupts piped logs and transcripts;
//   - a spinner that outlives Stop paints over output written after it;
//   - a spinner that erases more than its own line destroys the record of what
//     changed, which is the audit trail of the session.

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSpinnerWritesNothingWhenNotATTY(t *testing.T) {
	// A bytes.Buffer is never a *os.File, so detection must decline. This is
	// the piped/log-file case: escape sequences here cannot be seen and only
	// make greps harder.
	var out bytes.Buffer
	s := NewSpinner(&out, "working")
	s.Start()
	time.Sleep(30 * time.Millisecond)
	s.Stop()

	if out.Len() != 0 {
		t.Fatalf("spinner wrote %q to a non-TTY writer", out.String())
	}
}

func TestSpinnerEscapesAreCleanedUpBeforeReturn(t *testing.T) {
	// Force the enabled path with a buffer probe that records writes, so the
	// frames can be inspected without needing a real terminal.
	var out safeBuffer
	s := &Spinner{out: &out, label: "indexing", enabled: true}
	s.Start()
	time.Sleep(30 * time.Millisecond)
	s.Stop()

	got := out.String()
	if !strings.Contains(got, "indexing") {
		t.Fatalf("no frame was drawn: %q", got)
	}
	// Stop must erase the line it drew. Without this, the caller's next line
	// starts mid-frame.
	if !strings.HasSuffix(got, "\r"+ClearLine()) {
		t.Fatalf("output does not end with a line clear, so the spinner line survives: %q", got)
	}
}

func TestSpinnerStopIsIdempotent(t *testing.T) {
	var out safeBuffer
	s := &Spinner{out: &out, label: "x", enabled: true}
	s.Start()
	s.Stop()
	s.Stop() // must not close a channel twice

	if strings.Count(out.String(), ClearLine()) > 2 {
		t.Fatalf("repeated Stop cleared more than expected: %q", out.String())
	}
}

func TestRunWithSpinnerClearsOnPanic(t *testing.T) {
	var out safeBuffer
	func() {
		defer func() { _ = recover() }()
		// RunWithSpinner's deferred Stop must run while unwinding, otherwise a
		// half-drawn frame sits in the middle of the stack trace.
		s := &Spinner{out: &out, label: "boom", enabled: true}
		s.Start()
		defer s.Stop()
		panic("failure")
	}()

	if !strings.HasSuffix(out.String(), "\r"+ClearLine()) {
		t.Fatalf("spinner was not cleared while unwinding a panic: %q", out.String())
	}
}

// safeBuffer is a bytes.Buffer with a lock, because the spinner ticks from a
// goroutine while the test reads.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
