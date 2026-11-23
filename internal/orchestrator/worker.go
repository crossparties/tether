package orchestrator

import (
	"fmt"
	"strings"
	"time"
)

// WorkerStatus represents the lifecycle state of a concurrent sub-agent worker.
type WorkerStatus string

const (
	StatusIdle             WorkerStatus = "Idle"
	StatusRunning          WorkerStatus = "Running"
	StatusAwaitingApproval WorkerStatus = "Awaiting Approval"
	StatusCompleted        WorkerStatus = "Completed"
	StatusCancelled        WorkerStatus = "Cancelled"
	StatusFailed           WorkerStatus = "Failed"
)

// BrailleSpinnerFrames holds the 10-frame Braille animation sequence used to
// render non-flickering live progress for active sub-agent workers.
var BrailleSpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// WorkerSnapshot is an immutable point-in-time view of a sub-agent worker's
// state, safe for concurrent reading and UI rendering without holding locks.
type WorkerSnapshot struct {
	ID          string
	Role        string
	Description string
	Status      WorkerStatus
	StartedAt   time.Time
	Elapsed     time.Duration
	Spinner     string
	Output      string
	Err         string
}

// FormatStatusLine renders a single-line human-readable status bar entry
// suitable for live TUI/REPL status rendering.
func (w WorkerSnapshot) FormatStatusLine() string {
	icon := "•"
	switch w.Status {
	case StatusRunning:
		if strings.TrimSpace(w.Spinner) != "" {
			icon = w.Spinner
		} else {
			icon = BrailleSpinnerFrames[0]
		}
	case StatusAwaitingApproval:
		icon = "⏸"
	case StatusCompleted:
		icon = "✓"
	case StatusCancelled:
		icon = "⊘"
	case StatusFailed:
		icon = "✖"
	}

	label := w.ID
	if w.Role != "" && w.ID != "" && w.Role != w.ID {
		label = fmt.Sprintf("%s (%s)", w.Role, w.ID)
	} else if w.Role != "" {
		label = w.Role
	}

	elapsed := w.Elapsed.Round(100 * time.Millisecond)
	line := fmt.Sprintf("%s [%s] %s — %s (%s)", icon, label, w.Status, w.Description, elapsed)
	if w.Err != "" {
		line += fmt.Sprintf(" [err: %s]", w.Err)
	}
	return line
}
