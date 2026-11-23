package tui

import (
	"fmt"
	"strings"
	"time"
)

const maxSubagentToolLines = 8

func (m Model) hasRunningSubagent() bool {
	for _, s := range m.Subagents {
		if s.State == WorkerActive {
			return true
		}
	}
	return false
}

func (m Model) tickSubagents(now time.Time) Model {
	for i := range m.Subagents {
		if m.Subagents[i].State == WorkerActive {
			m.Subagents[i].Spinner = brailleFrames[m.SpinnerStep%len(brailleFrames)]
			m.Subagents[i].Detail = fmt.Sprintf("%.1fs", now.Sub(m.Subagents[i].StartedAt).Seconds())
		}
	}
	return m
}

// displayWorkers is the Zone A row list: the Main Orchestrator, then one row per spawned
// sub-agent (running first, then the most recent finished). With no sub-agent ever spawned
// it falls back to the static pool row so the layout is unchanged.
func (m Model) displayWorkers() []WorkerStatus {
	if len(m.Subagents) == 0 {
		return m.Workers
	}
	var out []WorkerStatus
	if len(m.Workers) > 0 {
		out = append(out, m.Workers[0])
	}
	var running, finished []WorkerStatus
	for i := len(m.Subagents) - 1; i >= 0; i-- {
		s := m.Subagents[i]
		task := s.Task
		if s.State == WorkerActive && s.LastTool != "" {
			task = s.LastTool
		}
		w := WorkerStatus{
			ID: fmt.Sprintf("sub-%d", s.ID), Name: s.Role, Task: truncateTask(task, 40),
			State: s.State, Spinner: s.Spinner, Detail: s.Detail, StartedAt: s.StartedAt,
		}
		if s.State == WorkerActive {
			running = append(running, w)
		} else {
			finished = append(finished, w)
		}
	}
	out = append(out, running...)
	return append(out, finished...)
}

// handleSubagentEvent keeps one Zone A record and one transcript block per sub-agent so
// parallel workers each show their own live process and final verdict.
func (m Model) handleSubagentEvent(ev SubagentEventMsg) Model {
	if m.subStream == nil {
		m.subStream = map[int]int{}
	}
	idx := -1
	for i := range m.Subagents {
		if m.Subagents[i].ID == ev.ID {
			idx = i
			break
		}
	}

	switch ev.Kind {
	case "start":
		now := time.Now()
		m.Subagents = append(m.Subagents, SubagentView{
			ID: ev.ID, Role: ev.Role, Task: ev.Task, State: WorkerActive,
			Spinner: brailleFrames[m.SpinnerStep%len(brailleFrames)], StartedAt: now, Detail: "0.0s",
		})
		m.Stream = append(m.Stream, StreamEntry{
			Kind:  StreamSubAgent,
			Title: fmt.Sprintf("Subagent %s · running", ev.Role),
			Body:  []string{"Task: " + ev.Task},
		})
		m.subStream[ev.ID] = len(m.Stream) - 1
		m.markSubagentPool(true, now)

	case "tool":
		if idx < 0 {
			return m
		}
		s := &m.Subagents[idx]
		s.LastTool = strings.TrimPrefix(ev.Line, "🔧 ")
		s.Tools++
		if bi, ok := m.subStream[ev.ID]; ok && bi < len(m.Stream) {
			e := &m.Stream[bi]
			e.Title = fmt.Sprintf("Subagent %s · running · %d tools", s.Role, s.Tools)
			e.Body = append(e.Body, "▸ "+s.LastTool)
			// Keep the live block compact: Task line + the most recent tool lines.
			if len(e.Body) > maxSubagentToolLines+1 {
				e.Body = append([]string{e.Body[0]}, e.Body[len(e.Body)-maxSubagentToolLines:]...)
			}
		}

	case "done":
		if idx >= 0 {
			s := &m.Subagents[idx]
			ok := ev.Err == "" && ev.Status != "BLOCKED" && ev.Status != "NEEDS_CONTEXT"
			s.State, s.Spinner = WorkerDone, "✓"
			if !ok {
				s.State, s.Spinner = WorkerCancelled, "✖"
			}
			s.Detail = fmt.Sprintf("%s · %.1fs", ev.Status, ev.Elapsed.Seconds())
			if bi, found := m.subStream[ev.ID]; found && bi < len(m.Stream) {
				e := &m.Stream[bi]
				e.Title = fmt.Sprintf("Subagent %s · %s · %.1fs · %d steps", s.Role, ev.Status, ev.Elapsed.Seconds(), ev.Steps)
				body := []string{"Task: " + s.Task}
				if ev.Err != "" {
					body = append(body, "Error: "+ev.Err)
				}
				switch {
				case ev.Proposal > 0:
					body = append(body, fmt.Sprintf("Proposal #%d is awaiting your approval.", ev.Proposal))
				case ev.Analysis:
					body = append(body, "Read-only analysis — nothing to approve.")
				}
				body = append(body, ev.Summary...)
				e.Body = body
			}
		}
		m.StreamScroll = 0
		if !m.hasRunningSubagent() {
			m.markSubagentPool(false, time.Now())
		}
	}
	return m
}

// markSubagentPool reflects "any sub-agent running" on the aggregate pool worker so the
// spinner ticker keeps running and the idle state returns when the last one finishes.
func (m *Model) markSubagentPool(running bool, now time.Time) {
	if len(m.Workers) < 2 {
		return
	}
	w := &m.Workers[1]
	if running {
		if w.State != WorkerActive {
			w.State, w.StartedAt = WorkerActive, now
		}
		w.Spinner = brailleFrames[m.SpinnerStep%len(brailleFrames)]
		w.Task = fmt.Sprintf("%d sub-agent(s) running", m.runningSubagentCount())
		return
	}
	w.State, w.Spinner = WorkerDone, "✓"
	w.Task = fmt.Sprintf("%d sub-agent(s) finished", len(m.Subagents))
	w.Detail = "Completed"
}

func (m Model) runningSubagentCount() int {
	n := 0
	for _, s := range m.Subagents {
		if s.State == WorkerActive {
			n++
		}
	}
	return n
}
