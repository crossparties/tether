package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerSnapshotFormatStatusLine(t *testing.T) {
	snap := WorkerSnapshot{
		ID:          "w-1",
		Role:        "tether_architect",
		Description: "analyzing package boundaries",
		Status:      StatusRunning,
		StartedAt:   time.Now().Add(-1500 * time.Millisecond),
		Elapsed:     1500 * time.Millisecond,
		Spinner:     BrailleSpinnerFrames[2],
	}
	line := snap.FormatStatusLine()
	if !strings.Contains(line, BrailleSpinnerFrames[2]) {
		t.Fatalf("expected spinner frame in status line, got %q", line)
	}
	if !strings.Contains(line, "tether_architect") || !strings.Contains(line, "Running") {
		t.Fatalf("expected role and status in line, got %q", line)
	}

	failed := WorkerSnapshot{
		ID:          "w-2",
		Role:        "tether_qa",
		Description: "running race suite",
		Status:      StatusFailed,
		Err:         "exit status 1",
	}
	fLine := failed.FormatStatusLine()
	if !strings.Contains(fLine, "✖") || !strings.Contains(fLine, "exit status 1") {
		t.Fatalf("expected failure icon and error text, got %q", fLine)
	}
}

func TestRegistryFilterAndSlashTrigger(t *testing.T) {
	all := DefaultCommandSpecs()
	if len(all) < 10 {
		t.Fatalf("expected default command specs to be populated, got %d", len(all))
	}

	btw := FilterCommands("/b")
	if len(btw) != 1 || btw[0].Name != "/btw" || !btw[0].NeedsArg {
		t.Fatalf("expected /btw command spec, got %+v", btw)
	}

	slashAll := FilterCommands("/")
	if len(slashAll) != len(all) {
		t.Fatalf("expected '/' filter to return all %d commands, got %d", len(all), len(slashAll))
	}

	cases := []struct {
		input      string
		cursor     int
		wantFilter string
		wantActive bool
	}{
		{input: "/", cursor: 1, wantFilter: "/", wantActive: true},
		{input: "/bt", cursor: 3, wantFilter: "/bt", wantActive: true},
		{input: "/model", cursor: 6, wantFilter: "/model", wantActive: true},
		{input: "/btw kenapa", cursor: 11, wantFilter: "", wantActive: false},
		{input: "/btw ", cursor: 5, wantFilter: "", wantActive: false},
		{input: "halo /btw", cursor: 9, wantFilter: "", wantActive: false},
		{input: "", cursor: 0, wantFilter: "", wantActive: false},
	}

	for _, tc := range cases {
		gotFilter, gotActive := IsSlashTriggerActive(tc.input, tc.cursor)
		if gotActive != tc.wantActive || gotFilter != tc.wantFilter {
			t.Errorf("IsSlashTriggerActive(%q, %d) = (%q, %v), want (%q, %v)",
				tc.input, tc.cursor, gotFilter, gotActive, tc.wantFilter, tc.wantActive)
		}
	}
}

func TestEngineSpawnAndCompleteConcurrent(t *testing.T) {
	eng := NewEngine()
	defer eng.Close()

	const numWorkers = 8
	var completed int32

	for i := 0; i < numWorkers; i++ {
		eng.SpawnWorker("", "tether_qa", "concurrent unit check", func(ctx context.Context) (string, error) {
			select {
			case <-time.After(30 * time.Millisecond):
				atomic.AddInt32(&completed, 1)
				return "PASS", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})
	}

	eng.Wait()

	if got := atomic.LoadInt32(&completed); got != numWorkers {
		t.Fatalf("expected %d completed workers, got %d", numWorkers, got)
	}

	snaps := eng.Snapshots()
	if len(snaps) != numWorkers {
		t.Fatalf("expected %d snapshots, got %d", numWorkers, len(snaps))
	}
	for _, s := range snaps {
		if s.Status != StatusCompleted || s.Output != "PASS" {
			t.Fatalf("unexpected worker snapshot: %+v", s)
		}
	}
}

func TestEngineCancelWorkerAndCancelAll(t *testing.T) {
	eng := NewEngine()
	defer eng.Close()

	started := make(chan struct{})
	id1 := eng.SpawnWorker("w-cancel-1", "tether_architect", "long analysis", func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})
	<-started

	if !eng.CancelWorker(id1) {
		t.Fatalf("expected CancelWorker(%q) to return true", id1)
	}

	// Spawn 3 more workers and cancel all at once
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		eng.SpawnWorker("", "tether_frontend", "ui task", func(ctx context.Context) (string, error) {
			wg.Done()
			<-ctx.Done()
			return "", ctx.Err()
		})
	}
	wg.Wait()

	cancelledCount := eng.CancelAll()
	if cancelledCount != 3 {
		t.Fatalf("expected CancelAll to cancel 3 workers, got %d", cancelledCount)
	}

	eng.Wait()
	for _, s := range eng.Snapshots() {
		if s.Status != StatusCancelled {
			t.Fatalf("expected worker %s to be StatusCancelled, got %s", s.ID, s.Status)
		}
	}
}

func TestEngineMainAgentReviewerAutoApproval(t *testing.T) {
	eng := NewEngine()
	defer eng.Close()

	// Configure Tier-2 Main Agent Reviewer so safe commands auto-approve without human blocking
	eng.SetMainAgentReviewer(func(req ApprovalRequest) (bool, string) {
		if strings.HasPrefix(req.Command, "go test") {
			return true, "safe test execution approved by Main Agent"
		}
		return false, "destructive command rejected by Main Agent"
	})

	var approvedAction, rejectedAction ApprovalAction
	eng.SpawnWorker("w-auto", "tether_qa", "running guarded tools", func(ctx context.Context) (string, error) {
		approvedAction = eng.RequestApproval(ctx, ApprovalRequest{
			WorkerID:    "w-auto",
			ToolName:    "run_command",
			Description: "run go test",
			Command:     "go test ./...",
		})
		rejectedAction = eng.RequestApproval(ctx, ApprovalRequest{
			WorkerID:    "w-auto",
			ToolName:    "run_command",
			Description: "remove workspace",
			Command:     "rm -rf /",
		})
		return "done", nil
	})

	eng.Wait()

	if approvedAction != ApproveAndRun {
		t.Fatalf("expected ApproveAndRun from MainAgentReviewer, got %s", approvedAction)
	}
	if rejectedAction != RejectAction {
		t.Fatalf("expected RejectAction from MainAgentReviewer, got %s", rejectedAction)
	}

	// Also verify Tier-1 SetAutoApproveAll
	eng.SetMainAgentReviewer(nil)
	eng.SetAutoApproveAll(true)
	if !eng.AutoApproveAll() {
		t.Fatalf("expected AutoApproveAll() to be true")
	}
	act := eng.RequestApproval(context.Background(), ApprovalRequest{
		ToolName:    "write_file",
		Description: "auto-approved write",
	})
	if act != ApproveAndRun {
		t.Fatalf("expected ApproveAndRun when SetAutoApproveAll(true), got %s", act)
	}
}

func TestEngineInteractiveApprovalAndFailure(t *testing.T) {
	eng := NewEngine()
	defer eng.Close()

	reqCh := make(chan ApprovalRequest, 1)
	go func() {
		for ev := range eng.Events() {
			if ev.Type == EventApprovalRequested && ev.Approval != nil {
				select {
				case reqCh <- *ev.Approval:
				default:
				}
			}
		}
	}()

	eng.SpawnWorker("w-human", "tether_product", "propose file edit", func(ctx context.Context) (string, error) {
		act := eng.RequestApproval(ctx, ApprovalRequest{
			ID:          "req-42",
			WorkerID:    "w-human",
			ToolName:    "edit_file",
			Description: "update README",
		})
		if act != ApproveAndRun {
			return "", errors.New("user rejected action")
		}
		return "edited", nil
	})

	select {
	case req := <-reqCh:
		if !eng.SubmitApproval(req.ID, ApproveAndRun, "looks good") {
			t.Fatalf("SubmitApproval failed for %s", req.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for EventApprovalRequested")
	}

	eng.Wait()
	snaps := eng.Snapshots()
	if len(snaps) != 1 || snaps[0].Status != StatusCompleted || snaps[0].Output != "edited" {
		t.Fatalf("unexpected worker state after human approval: %+v", snaps)
	}
}
