package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ApprovalAction represents the decision taken on a guarded tool or diff proposal.
type ApprovalAction string

const (
	ApproveAndRun  ApprovalAction = "ApproveAndRun"
	ViewFullDiff   ApprovalAction = "ViewFullDiff"
	RejectAction   ApprovalAction = "RejectAction"
	CancelAllTasks ApprovalAction = "CancelAllTasks"
)

// ApprovalRequest encapsulates a permission request raised by a worker before
// executing a destructive or mutative operation.
type ApprovalRequest struct {
	ID           string
	WorkerID     string
	Role         string
	ToolName     string
	Description  string
	Command      string
	FilePath     string
	DiffPreview  string
	FullDiff     string
	AutoApproved bool
	AutoReason   string
	CreatedAt    time.Time
	ResponseCh   chan ApprovalResponse
}

// ApprovalResponse carries the resolution for an ApprovalRequest.
type ApprovalResponse struct {
	RequestID string
	Action    ApprovalAction
	Reason    string
}

// EventType categorizes orchestrator lifecycle notifications.
type EventType string

const (
	EventWorkerSpawned     EventType = "WorkerSpawned"
	EventWorkerUpdated     EventType = "WorkerUpdated"
	EventWorkerCompleted   EventType = "WorkerCompleted"
	EventWorkerCancelled   EventType = "WorkerCancelled"
	EventWorkerFailed      EventType = "WorkerFailed"
	EventApprovalRequested EventType = "ApprovalRequested"
	EventApprovalResolved  EventType = "ApprovalResolved"
)

// Event is emitted on Engine.Events() whenever a worker changes state or an
// approval gate is requested/resolved.
type Event struct {
	Type      EventType
	Worker    WorkerSnapshot
	Approval  *ApprovalRequest
	Message   string
	Timestamp time.Time
}

// TaskFunc is the unit of work executed by a spawned sub-agent worker.
type TaskFunc func(ctx context.Context) (string, error)

// MainAgentReviewerFunc evaluates an ApprovalRequest on behalf of the Main Agent.
// When approved is true, the Engine automatically resolves the request with
// ApproveAndRun without blocking on the human user.
type MainAgentReviewerFunc func(req ApprovalRequest) (approved bool, reason string)

type workerEntry struct {
	snapshot   WorkerSnapshot
	cancel     context.CancelFunc
	spinnerIdx int
	order      uint64
}

// Engine coordinates concurrent sub-agent workers, non-blocking Braille spinner
// updates, cancellation propagation, and two-tiered Main Agent auto-approval.
type Engine struct {
	mu                sync.RWMutex
	workers           map[string]*workerEntry
	pendingApprovals  map[string]chan ApprovalResponse
	autoApproveAll    bool
	mainAgentReviewer MainAgentReviewerFunc
	events            chan Event
	closed            bool
	closedCh          chan struct{}
	closeOnce         sync.Once
	wg                sync.WaitGroup
	seq               uint64
	spinnerInterval   time.Duration
}

// NewEngine creates and initializes a ready-to-use concurrent orchestrator Engine.
func NewEngine() *Engine {
	return &Engine{
		workers:          make(map[string]*workerEntry),
		pendingApprovals: make(map[string]chan ApprovalResponse),
		events:           make(chan Event, 256),
		closedCh:         make(chan struct{}),
		spinnerInterval:  80 * time.Millisecond,
	}
}

// Events returns the read-only channel of orchestrator lifecycle events.
func (e *Engine) Events() <-chan Event {
	return e.events
}

// SetAutoApproveAll enables or disables unconditional Main Agent auto-approval
// for all worker approval requests so workers never block waiting for a human.
func (e *Engine) SetAutoApproveAll(enabled bool) {
	e.mu.Lock()
	e.autoApproveAll = enabled
	e.mu.Unlock()
}

// AutoApproveAll reports whether unconditional auto-approval is currently active.
func (e *Engine) AutoApproveAll() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.autoApproveAll
}

// SetMainAgentReviewer installs a callback allowing the Main Agent to inspect
// and auto-approve (or reject) sub-agent actions without blocking on human input.
func (e *Engine) SetMainAgentReviewer(fn MainAgentReviewerFunc) {
	e.mu.Lock()
	e.mainAgentReviewer = fn
	e.mu.Unlock()
}

// SpawnWorker launches a concurrent worker goroutine tracked by the Engine and
// returns its assigned worker ID.
func (e *Engine) SpawnWorker(id, role, description string, fn TaskFunc) string {
	seq := atomic.AddUint64(&e.seq, 1)
	if id == "" {
		id = fmt.Sprintf("worker-%d", seq)
	}
	if role == "" {
		role = "subagent"
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return id
	}

	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()
	snap := WorkerSnapshot{
		ID:          id,
		Role:        role,
		Description: description,
		Status:      StatusRunning,
		StartedAt:   now,
		Elapsed:     0,
		Spinner:     BrailleSpinnerFrames[0],
	}
	e.workers[id] = &workerEntry{
		snapshot:   snap,
		cancel:     cancel,
		spinnerIdx: 0,
		order:      seq,
	}
	e.wg.Add(1)
	e.mu.Unlock()

	e.emit(Event{
		Type:      EventWorkerSpawned,
		Worker:    snap,
		Message:   fmt.Sprintf("spawned worker %s (%s)", id, role),
		Timestamp: now,
	})

	go e.runWorker(ctx, id, fn)
	return id
}

func (e *Engine) runWorker(ctx context.Context, id string, fn TaskFunc) {
	defer e.wg.Done()

	doneCh := make(chan struct{})
	var (
		output string
		runErr error
	)

	go func() {
		defer close(doneCh)
		if fn != nil {
			output, runErr = fn(ctx)
		}
	}()

	ticker := time.NewTicker(e.spinnerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-doneCh:
			e.finishWorker(id, ctx.Err(), output, runErr)
			return
		case <-ticker.C:
			e.advanceSpinner(id)
		}
	}
}

func (e *Engine) advanceSpinner(id string) {
	e.mu.Lock()
	entry, ok := e.workers[id]
	if !ok || (entry.snapshot.Status != StatusRunning && entry.snapshot.Status != StatusAwaitingApproval) {
		e.mu.Unlock()
		return
	}
	entry.spinnerIdx = (entry.spinnerIdx + 1) % len(BrailleSpinnerFrames)
	entry.snapshot.Spinner = BrailleSpinnerFrames[entry.spinnerIdx]
	entry.snapshot.Elapsed = time.Since(entry.snapshot.StartedAt)
	snap := entry.snapshot
	e.mu.Unlock()

	e.emit(Event{
		Type:      EventWorkerUpdated,
		Worker:    snap,
		Timestamp: time.Now(),
	})
}

func (e *Engine) finishWorker(id string, ctxErr error, output string, runErr error) {
	e.mu.Lock()
	entry, ok := e.workers[id]
	if !ok {
		e.mu.Unlock()
		return
	}

	entry.snapshot.Elapsed = time.Since(entry.snapshot.StartedAt)
	entry.snapshot.Output = output

	var evType EventType
	switch {
	case entry.snapshot.Status == StatusCancelled || errors.Is(ctxErr, context.Canceled) || errors.Is(runErr, context.Canceled):
		entry.snapshot.Status = StatusCancelled
		entry.snapshot.Spinner = "⊘"
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			entry.snapshot.Err = runErr.Error()
		}
		evType = EventWorkerCancelled
	case runErr != nil:
		entry.snapshot.Status = StatusFailed
		entry.snapshot.Spinner = "✖"
		entry.snapshot.Err = runErr.Error()
		evType = EventWorkerFailed
	default:
		entry.snapshot.Status = StatusCompleted
		entry.snapshot.Spinner = "✓"
		evType = EventWorkerCompleted
	}

	snap := entry.snapshot
	e.mu.Unlock()

	e.emit(Event{
		Type:      evType,
		Worker:    snap,
		Message:   snap.FormatStatusLine(),
		Timestamp: time.Now(),
	})
}

// RequestApproval transitions the worker into StatusAwaitingApproval and
// evaluates the request through the Two-Tiered Main Agent Auto-Approval Mode
// before falling back to interactive human approval.
func (e *Engine) RequestApproval(ctx context.Context, req ApprovalRequest) ApprovalAction {
	if req.ID == "" {
		seq := atomic.AddUint64(&e.seq, 1)
		req.ID = fmt.Sprintf("approval-%d", seq)
	}
	if req.CreatedAt.IsZero() {
		req.CreatedAt = time.Now()
	}
	if req.ResponseCh == nil {
		req.ResponseCh = make(chan ApprovalResponse, 1)
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return CancelAllTasks
	}

	autoAll := e.autoApproveAll
	reviewer := e.mainAgentReviewer

	var workerSnap WorkerSnapshot
	if entry, ok := e.workers[req.WorkerID]; ok {
		if req.Role == "" {
			req.Role = entry.snapshot.Role
		}
		if entry.snapshot.Status == StatusRunning {
			entry.snapshot.Status = StatusAwaitingApproval
			entry.snapshot.Elapsed = time.Since(entry.snapshot.StartedAt)
		}
		workerSnap = entry.snapshot
	}
	e.mu.Unlock()

	// Tier 1: Unconditional Main Agent Auto-Approval Mode
	if autoAll {
		req.AutoApproved = true
		req.AutoReason = "auto-approved by Main Agent policy (AutoApproveAll)"
		e.restoreWorkerRunning(req.WorkerID)
		reqCopy := req
		e.emit(Event{
			Type:      EventApprovalResolved,
			Worker:    workerSnap,
			Approval:  &reqCopy,
			Message:   req.AutoReason,
			Timestamp: time.Now(),
		})
		return ApproveAndRun
	}

	// Tier 2: Main Agent Evaluator Callback
	if reviewer != nil {
		approved, reason := reviewer(req)
		req.AutoApproved = approved
		req.AutoReason = reason
		e.restoreWorkerRunning(req.WorkerID)
		reqCopy := req
		e.emit(Event{
			Type:      EventApprovalResolved,
			Worker:    workerSnap,
			Approval:  &reqCopy,
			Message:   reason,
			Timestamp: time.Now(),
		})
		if approved {
			return ApproveAndRun
		}
		return RejectAction
	}

	// Tier 3: Interactive Human Approval Gate
	e.mu.Lock()
	e.pendingApprovals[req.ID] = req.ResponseCh
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		delete(e.pendingApprovals, req.ID)
		e.mu.Unlock()
	}()

	reqCopy := req
	e.emit(Event{
		Type:      EventApprovalRequested,
		Worker:    workerSnap,
		Approval:  &reqCopy,
		Message:   fmt.Sprintf("approval requested for %s (%s)", req.ToolName, req.Description),
		Timestamp: time.Now(),
	})

	for {
		select {
		case resp := <-req.ResponseCh:
			if resp.Action == ViewFullDiff {
				// Re-emit with full diff visibility while keeping worker awaiting final decision
				e.emit(Event{
					Type:      EventApprovalRequested,
					Worker:    workerSnap,
					Approval:  &reqCopy,
					Message:   "full diff requested",
					Timestamp: time.Now(),
				})
				continue
			}
			if resp.Action == CancelAllTasks {
				e.CancelAll()
			} else {
				e.restoreWorkerRunning(req.WorkerID)
			}
			e.emit(Event{
				Type:      EventApprovalResolved,
				Worker:    workerSnap,
				Approval:  &reqCopy,
				Message:   string(resp.Action),
				Timestamp: time.Now(),
			})
			return resp.Action

		case <-ctx.Done():
			e.restoreWorkerRunning(req.WorkerID)
			return RejectAction

		case <-e.closedCh:
			return CancelAllTasks
		}
	}
}

// SubmitApproval delivers a user or orchestrator decision to a pending
// ApprovalRequest by its ID.
func (e *Engine) SubmitApproval(requestID string, action ApprovalAction, reason string) bool {
	e.mu.RLock()
	ch, ok := e.pendingApprovals[requestID]
	e.mu.RUnlock()
	if !ok {
		return false
	}
	select {
	case ch <- ApprovalResponse{RequestID: requestID, Action: action, Reason: reason}:
		return true
	default:
		return false
	}
}

func (e *Engine) restoreWorkerRunning(workerID string) {
	if workerID == "" {
		return
	}
	e.mu.Lock()
	if entry, ok := e.workers[workerID]; ok && entry.snapshot.Status == StatusAwaitingApproval {
		entry.snapshot.Status = StatusRunning
		entry.snapshot.Elapsed = time.Since(entry.snapshot.StartedAt)
	}
	e.mu.Unlock()
}

// Snapshots returns a point-in-time slice of all worker snapshots in spawn order.
func (e *Engine) Snapshots() []WorkerSnapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()

	type orderedSnap struct {
		order uint64
		snap  WorkerSnapshot
	}
	list := make([]orderedSnap, 0, len(e.workers))
	for _, entry := range e.workers {
		snap := entry.snapshot
		if snap.Status == StatusRunning || snap.Status == StatusAwaitingApproval {
			snap.Elapsed = time.Since(snap.StartedAt)
		}
		list = append(list, orderedSnap{order: entry.order, snap: snap})
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].order < list[j].order
	})

	out := make([]WorkerSnapshot, len(list))
	for i, item := range list {
		out[i] = item.snap
	}
	return out
}

// CancelWorker cancels a single running or awaiting-approval worker by ID.
// Returns true if the worker was active and cancelled.
func (e *Engine) CancelWorker(id string) bool {
	e.mu.Lock()
	entry, ok := e.workers[id]
	if !ok {
		e.mu.Unlock()
		return false
	}
	if entry.snapshot.Status != StatusRunning && entry.snapshot.Status != StatusAwaitingApproval {
		e.mu.Unlock()
		return false
	}
	entry.snapshot.Status = StatusCancelled
	entry.snapshot.Spinner = "⊘"
	entry.snapshot.Elapsed = time.Since(entry.snapshot.StartedAt)
	cancel := entry.cancel
	e.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return true
}

// CancelAll cancels all currently running or awaiting-approval workers and
// returns the count of workers cancelled.
func (e *Engine) CancelAll() int {
	e.mu.Lock()
	var cancels []context.CancelFunc
	for _, entry := range e.workers {
		if entry.snapshot.Status == StatusRunning || entry.snapshot.Status == StatusAwaitingApproval {
			entry.snapshot.Status = StatusCancelled
			entry.snapshot.Spinner = "⊘"
			entry.snapshot.Elapsed = time.Since(entry.snapshot.StartedAt)
			if entry.cancel != nil {
				cancels = append(cancels, entry.cancel)
			}
		}
	}
	e.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	return len(cancels)
}

// Wait blocks until all spawned workers have exited.
func (e *Engine) Wait() {
	e.wg.Wait()
}

// Close cancels all active workers, waits for their goroutines to terminate,
// and closes the event stream cleanly without races.
func (e *Engine) Close() {
	e.closeOnce.Do(func() {
		close(e.closedCh)
		e.CancelAll()
		e.wg.Wait()

		e.mu.Lock()
		e.closed = true
		close(e.events)
		e.mu.Unlock()
	})
}

func (e *Engine) emit(ev Event) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed {
		return
	}
	select {
	case e.events <- ev:
	default:
	}
}
