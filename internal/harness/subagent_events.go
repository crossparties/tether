package harness

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SubagentEvent is one lifecycle update of a spawned worker. It exists so a UI can show
// each worker's process live (which tool it is on, how long it ran, what it concluded)
// instead of the single opaque "working" spinner the buffered output allowed.
type SubagentEvent struct {
	ID       int
	Role     string
	Task     string
	Kind     string // "start" | "tool" | "done"
	Line     string // tool line, for Kind == "tool"
	Steps    int
	Elapsed  time.Duration
	Status   string // READY_FOR_REVIEW | BLOCKED | NEEDS_CONTEXT | ERROR | ...
	Summary  []string
	Err      string
	Proposal int64
	Analysis bool
}

var subagentSeq int64

// SetSubagentObserver registers the consumer of worker lifecycle events. The callback
// may be invoked concurrently from several worker goroutines and must be safe for that.
func (r *Runner) SetSubagentObserver(fn func(SubagentEvent)) { r.onSubagent = fn }

func (r *Runner) emitSubagentStart(role, task string) int {
	id := int(atomic.AddInt64(&subagentSeq, 1))
	if r.onSubagent != nil {
		r.onSubagent(SubagentEvent{ID: id, Role: role, Task: task, Kind: "start"})
	}
	return id
}

func (r *Runner) emitSubagentDone(id int, role string, start time.Time, steps int, run *subagentRun, report string) {
	if r.onSubagent == nil {
		return
	}
	ev := SubagentEvent{
		ID: id, Role: role, Kind: "done", Steps: steps, Elapsed: time.Since(start),
		Status: string(run.Status), Proposal: run.Proposal, Analysis: run.Analysis,
		Summary: reportDigest(report, 4, 110),
	}
	if run.Err != nil {
		ev.Err = run.Err.Error()
		ev.Status = "ERROR"
	}
	if ev.Status == "" {
		ev.Status = "DONE"
	}
	r.onSubagent(ev)
}

// printRunSummary prints the plain-text run block. With a live observer attached the UI
// already shows that information, so printing it again would only duplicate it.
func (r *Runner) printRunSummary(run subagentRun) {
	if r.onSubagent != nil {
		return
	}
	_, _ = r.cfg.Out.Write([]byte(run.Summary()))
}

// subagentTee keeps the worker log buffer (so nothing about its contract changes) and
// forwards each tool line to the observer as it happens.
type subagentTee struct {
	mu   sync.Mutex
	buf  *bytes.Buffer
	emit func(line string)
}

func (r *Runner) newSubagentTee(buf *bytes.Buffer, id int, role string) *subagentTee {
	t := &subagentTee{buf: buf}
	if r.onSubagent != nil {
		obs := r.onSubagent
		t.emit = func(line string) { obs(SubagentEvent{ID: id, Role: role, Kind: "tool", Line: line}) }
	}
	return t
}

func (t *subagentTee) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf.Write(p)
	t.mu.Unlock()
	if t.emit != nil {
		for _, line := range strings.Split(string(p), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				t.emit(line)
			}
		}
	}
	return len(p), nil
}

// reportDigest returns up to n meaningful lines of a worker report, each cut to maxCols
// runes, skipping the STATUS trailer (the status is reported as its own field).
func reportDigest(text string, n, maxCols int) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(strings.ToUpper(line), "STATUS:") || strings.HasPrefix(line, "```") {
			continue
		}
		if r := []rune(line); len(r) > maxCols {
			line = string(r[:maxCols-1]) + "…"
		}
		out = append(out, line)
		if len(out) == n {
			break
		}
	}
	return out
}
