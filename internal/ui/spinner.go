package ui

// A single-line spinner for work in progress.
//
// Two rules shape this file, and both come from watching what a careless
// spinner does to a terminal session:
//
//  1. It must never erase evidence. A diff, a path, an exit code and a stderr
//     tail are the record of what changed; a spinner that overwrites them turns
//     an auditable session into a scrolling animation. Only progress text is
//     allowed to be overwritten, and only ever on the line the spinner owns.
//  2. It must never assume a terminal. When output is not a TTY, or colour is
//     disabled, the spinner renders nothing: a control-character stream in a
//     log file or a piped transcript is worse than no progress at all.

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// spinnerFrames is deliberately ASCII. Braille and block glyphs look better in
// a modern terminal but render as boxes in the ones people actually paste
// build logs from.
var spinnerFrames = []string{"|", "/", "-", "\\"}

// Spinner renders one moving line while work is in flight. The zero value is
// not usable; construct it with NewSpinner.
//
// It guards its own state with a mutex because Start runs the ticking in a
// goroutine. That is the one place in this package where concurrent access is
// real, so the lock is local to the spinner rather than pushed onto the
// renderer the rest of the CLI uses.
type Spinner struct {
	out     io.Writer
	label   string
	enabled bool

	mu      sync.Mutex
	frame   int
	started bool
	stopped bool
	stop    chan struct{}
	done    chan struct{}
	drawn   bool
}

// NewSpinner returns a spinner writing to out. It is a no-op unless out is a
// terminal, because escape sequences written into a file or a pipe corrupt the
// transcript and cannot be seen anyway.
func NewSpinner(out io.Writer, label string) *Spinner {
	enabled := false
	if f, ok := out.(*os.File); ok {
		enabled = Enabled() && f == os.Stdout
	}
	return &Spinner{out: out, label: label, enabled: enabled}
}

// Start begins ticking. Calling it twice is harmless.
func (s *Spinner) Start() {
	if s == nil || !s.enabled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return
	}
	s.started = true
	s.stop = make(chan struct{})
	s.done = make(chan struct{})

	// Draw the first frame synchronously. Waiting for the first tick leaves the
	// terminal silent for a full interval, which reads as a hang at exactly the
	// moment the user is most likely to think the tool died. It also means a
	// caller that stops quickly still shows something happened.
	s.drawLocked()

	go func() {
		defer close(s.done)
		ticker := time.NewTicker(120 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				s.draw()
			}
		}
	}()
}

// draw paints the current frame over the previous one.
func (s *Spinner) draw() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drawLocked()
}

// drawLocked paints the current frame. The caller must hold s.mu.
func (s *Spinner) drawLocked() {
	if s.stopped {
		return
	}
	line := fmt.Sprintf("\r  %s %s", spinnerFrames[s.frame%len(spinnerFrames)], s.label)
	s.frame++
	// Pad to overwrite a longer previous label instead of leaving its tail
	// behind. errcheck is irrelevant here: a failed write to a terminal is not
	// recoverable and must not abort the turn.
	_, _ = io.WriteString(s.out, ClearLine()+line)
	s.drawn = true
}

// Stop clears the spinner line and waits for the ticking goroutine to exit, so
// that nothing can paint over output written immediately after.
//
// Callers must Stop before printing anything that matters. The ordering is
// enforced by the fact that Stop returns only once the goroutine is gone: a
// trailing frame cannot land after the caller's next write.
func (s *Spinner) Stop() {
	if s == nil || !s.enabled {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	drawn := s.drawn
	stop, done := s.stop, s.done
	s.mu.Unlock()

	if stop != nil {
		close(stop)
		<-done
	}
	if drawn {
		// Erase the line so the answer starts at column zero, then leave the
		// cursor there rather than on a blank line: the caller's next line is
		// the one that matters.
		_, _ = io.WriteString(s.out, "\r"+ClearLine())
	}
}

// RunWithSpinner runs fn with a spinner visible, then clears it before
// returning. If fn panics the spinner is still cleared, which matters because a
// half-drawn spinner line in a stack trace makes the real error harder to read.
func RunWithSpinner(out io.Writer, label string, fn func()) {
	s := NewSpinner(out, label)
	s.Start()
	defer s.Stop()
	fn()
}
