package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/workflow"
)

func TestStartPerformsImmediatePollAndClaimsBeforeWorkerStart(t *testing.T) {
	tracker := &fakeTracker{byStates: []domain.Issue{readyIssue("1", "open", "symphony")}}
	worker := newBlockingWorker()
	orchestrator := startTestOrchestrator(t, tracker, worker)

	request := worker.waitStarted(t)
	if request.Issue.ID != "1" {
		t.Fatalf("worker issue = %q, want 1", request.Issue.ID)
	}
	snapshot := mustSnapshot(t, orchestrator)
	if len(snapshot.Running) != 1 || snapshot.Running[0].IssueID != "1" {
		t.Fatalf("claim was not visible after worker start: %#v", snapshot.Running)
	}
	states, ids, maximum := tracker.counts()
	if states != 2 || ids != 1 || maximum != 1 {
		t.Fatalf("tracker calls = states:%d ids:%d concurrent:%d", states, ids, maximum)
	}
}

func TestConcurrentRefreshesCoalesceToOnePoll(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	tracker := &fakeTracker{stateStarted: started, stateRelease: release, blockStates: []string{"open"}}
	orchestrator := startTestOrchestrator(t, tracker, newBlockingWorker())
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("initial poll did not start")
	}

	const callers = 20
	receipts := make(chan domain.RefreshReceipt, callers)
	errorsSeen := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			receipt, err := orchestrator.Refresh(ctx)
			receipts <- receipt
			errorsSeen <- err
		}()
	}
	time.Sleep(25 * time.Millisecond)
	close(release)
	group.Wait()
	close(receipts)
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
	}
	leaders := 0
	for receipt := range receipts {
		if receipt.Queued && !receipt.Coalesced {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("non-coalesced receipts = %d, want 1", leaders)
	}
	states, _, maximum := tracker.counts()
	if states != 2 || maximum != 1 {
		t.Fatalf("poll calls = %d, maximum concurrency = %d", states, maximum)
	}
}

func TestDispatchRevalidatesAndAppliesCurrentRoutingAndSlots(t *testing.T) {
	tests := []struct {
		name        string
		configure   func(*fakeTracker, *fakeWorkflowStore)
		wantIDCalls int
	}{
		{
			name: "no global slot",
			configure: func(adapter *fakeTracker, store *fakeWorkflowStore) {
				snapshot := testWorkflowSnapshot()
				snapshot.Config.Agent.MaxConcurrent = 0
				store.setCurrent(snapshot)
			},
			wantIDCalls: 0,
		},
		{
			name: "no per-state slot",
			configure: func(adapter *fakeTracker, store *fakeWorkflowStore) {
				snapshot := testWorkflowSnapshot()
				snapshot.Config.Agent.MaxConcurrentByState["open"] = 0
				store.setCurrent(snapshot)
			},
			wantIDCalls: 0,
		},
		{
			name: "provider revalidation rejects issue",
			configure: func(adapter *fakeTracker, _ *fakeWorkflowStore) {
				issue := readyIssue("1", "open", "symphony")
				issue.Dispatchable = false
				adapter.byIDs = []domain.Issue{issue}
			},
			wantIDCalls: 1,
		},
		{
			name: "workflow provider routing changed",
			configure: func(adapter *fakeTracker, store *fakeWorkflowStore) {
				adapter.afterStates = func() {
					snapshot := testWorkflowSnapshot()
					snapshot.Config.Tracker.Kind = "linear"
					store.setCurrent(snapshot)
				}
			},
			wantIDCalls: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter := &fakeTracker{byStates: []domain.Issue{readyIssue("1", "open", "symphony")}}
			worker := newBlockingWorker()
			store := newFakeWorkflowStore(testWorkflowSnapshot())
			test.configure(adapter, store)
			orchestrator := startTestOrchestrator(t, adapter, worker, func(options *Options) { options.Workflow = store })
			waitForPollToSettle(t, orchestrator)
			worker.assertNotStarted(t)
			_, ids, maximum := adapter.counts()
			if ids != test.wantIDCalls || maximum > 1 {
				t.Fatalf("ID calls = %d, want %d; maximum concurrency = %d", ids, test.wantIDCalls, maximum)
			}
		})
	}
}

func TestStartRejectsInvalidWorkflowAndAdapterFailureSkipsDispatch(t *testing.T) {
	t.Run("workflow load failure", func(t *testing.T) {
		store := newFakeWorkflowStore(workflow.Snapshot{})
		store.hasValue = false
		store.loadErr = workflow.ErrInvalidWorkflow
		_, err := Start(context.Background(), Options{
			Tracker: &fakeTracker{}, Workflow: store, Worker: newBlockingWorker(),
			Workspace: &fakeWorkspaceManager{}, Events: observability.NewJournal(observability.JournalOptions{}), Clock: RealClock{},
		})
		if !errors.Is(err, workflow.ErrInvalidWorkflow) {
			t.Fatalf("Start() error = %v, want invalid workflow", err)
		}
	})

	t.Run("candidate adapter failure", func(t *testing.T) {
		adapter := &fakeTracker{statesErr: errors.New("provider unavailable")}
		worker := newBlockingWorker()
		orchestrator := startTestOrchestrator(t, adapter, worker)
		waitForPollToSettle(t, orchestrator)
		worker.assertNotStarted(t)
		if snapshot := mustSnapshot(t, orchestrator); snapshot.Tracker.State != "error" {
			t.Fatalf("tracker state = %q, want error", snapshot.Tracker.State)
		}
	})
}

func waitForPollToSettle(t *testing.T, orchestrator *Orchestrator) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := mustSnapshot(t, orchestrator)
		if snapshot.Tracker.State != "loading" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("poll did not settle")
}
