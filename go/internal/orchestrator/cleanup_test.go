package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
)

func TestRefreshDuringStartupCleanupWaitsAndCoalesces(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	adapter := &fakeTracker{
		stateResponses: []fakeTrackerResponse{{issues: []domain.Issue{}}, {issues: []domain.Issue{}}},
		stateStarted:   started, stateRelease: release, blockStates: []string{"closed"},
	}
	orchestrator := startTestOrchestrator(t, adapter, newBlockingWorker(), func(options *Options) { options.InitiallyPaused = true })
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("startup cleanup did not begin")
	}

	receipts := make(chan domain.RefreshReceipt, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			receipt, err := orchestrator.Refresh(ctx)
			if err != nil {
				t.Errorf("refresh: %v", err)
			}
			receipts <- receipt
		}()
	}
	time.Sleep(25 * time.Millisecond)
	states, _, maximum := adapter.counts()
	if states != 1 || maximum != 1 {
		t.Fatalf("startup refresh bypassed cleanup: states=%d concurrent=%d", states, maximum)
	}
	close(release)
	group.Wait()
	close(receipts)
	leaders, coalesced := 0, 0
	for receipt := range receipts {
		if receipt.Queued {
			leaders++
		}
		if receipt.Coalesced {
			coalesced++
		}
	}
	if leaders != 1 || coalesced != 1 {
		t.Fatalf("receipts = leaders:%d coalesced:%d", leaders, coalesced)
	}
}

func TestStartupCleanupProcessesEveryTerminalIssueBeforeActivePoll(t *testing.T) {
	first := issueInState("1", "closed", true)
	second := issueInState("2", "closed", true)
	adapter := &fakeTracker{stateResponses: []fakeTrackerResponse{{issues: issueSlice(first, second)}, {issues: []domain.Issue{}}}}
	workspace := &fakeWorkspaceManager{}
	orchestrator := startTestOrchestrator(t, adapter, newBlockingWorker(), func(options *Options) {
		options.Workspace = workspace
		options.InitiallyPaused = true
	})
	waitForPollToSettle(t, orchestrator)
	waitForRemovalCount(t, workspace, 2)
	states, ids, maximum := adapter.counts()
	if states != 2 || ids != 0 || maximum != 1 {
		t.Fatalf("startup calls = states:%d ids:%d concurrent:%d", states, ids, maximum)
	}
}

func TestStartupTerminalFetchFailureWarnsAndContinues(t *testing.T) {
	adapter := &fakeTracker{stateResponses: []fakeTrackerResponse{{err: errors.New("terminal read failed")}, {issues: []domain.Issue{}}}}
	orchestrator := startTestOrchestrator(t, adapter, newBlockingWorker(), func(options *Options) { options.InitiallyPaused = true })
	waitForPollToSettle(t, orchestrator)
	if snapshot := mustSnapshot(t, orchestrator); snapshot.Tracker.State != "ready" {
		t.Fatalf("tracker state = %q, want ready after active poll", snapshot.Tracker.State)
	}
	states, _, maximum := adapter.counts()
	if states != 2 || maximum != 1 {
		t.Fatalf("startup failure calls = %d, concurrency = %d", states, maximum)
	}
}

func waitForRemovalCount(t *testing.T, workspace *fakeWorkspaceManager, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if workspace.removeCount() == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("workspace removals = %d, want %d", workspace.removeCount(), count)
}
