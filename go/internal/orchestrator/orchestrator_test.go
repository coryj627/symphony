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

func TestStopCancelsWorkersAndReturnsStoppingThenPausedState(t *testing.T) {
	tracker := &fakeTracker{byStates: []domain.Issue{readyIssue("1", "open", "symphony")}}
	worker := newStubbornWorker()
	orchestrator := startTestOrchestrator(t, tracker, worker)

	select {
	case request := <-worker.started:
		if request.Issue.ID != "1" {
			t.Fatalf("worker issue = %q, want 1", request.Issue.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := orchestrator.SetScheduler(ctx, false); err != nil {
		t.Fatalf("stop scheduler: %v", err)
	}
	select {
	case <-worker.canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler stop did not cancel the worker")
	}
	snapshot := mustSnapshot(t, orchestrator)
	if snapshot.Scheduler.Enabled || snapshot.Scheduler.State != "stopping" {
		t.Fatalf("scheduler = %#v, want stopping", snapshot.Scheduler)
	}
	if len(snapshot.Running) != 1 || snapshot.Running[0].LastEvent != string(domain.RunStatusStopping) {
		t.Fatalf("running rows after stop = %#v, want one stopping row", snapshot.Running)
	}

	worker.release <- domain.RunResult{Reason: domain.StopReasonOperatorStop, EndedAt: time.Now().UTC()}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		final := mustSnapshot(t, orchestrator)
		if len(final.Running) == 0 {
			if final.Scheduler.Enabled || final.Scheduler.State != "paused" {
				t.Fatalf("scheduler after drain = %#v, want paused", final.Scheduler)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("stopped worker was not drained")
}

func TestCloseCancelsAndDrainsWorkersBeforeReturning(t *testing.T) {
	tracker := &fakeTracker{byStates: []domain.Issue{readyIssue("1", "open", "symphony")}}
	worker := newStubbornWorker()
	orchestrator := startTestOrchestrator(t, tracker, worker)
	select {
	case <-worker.started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}

	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		closed <- orchestrator.Close(ctx)
	}()
	select {
	case <-worker.canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("close did not cancel the worker")
	}
	select {
	case err := <-closed:
		t.Fatalf("close returned before worker cleanup finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	worker.release <- domain.RunResult{Reason: domain.StopReasonOperatorStop, EndedAt: time.Now().UTC()}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close after drain: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("close did not return after the worker drained")
	}
}

func TestWorkerWorkspaceAndAttemptReachIssueDetail(t *testing.T) {
	issue := readyIssue("1", "open", "symphony")
	attempt := 2
	state := actorState{model: newState()}
	claimRun(&state.model, issue, &attempt, time.Now())
	workspace := domain.Workspace{Path: "/safe/workspace", Key: "SYM-1", Owned: true, IssueID: issue.ID, IssueIdentifier: issue.Identifier}
	(&Orchestrator{}).handleWorkerUpdate(Options{Events: observability.NewJournal(observability.JournalOptions{})}, &state, workerUpdate{
		issueID: issue.ID,
		event:   domain.AgentEvent{Type: string(domain.RunStatusBuildingPrompt), Workspace: &workspace, SessionID: "session-1", Message: "Safe worker progress"},
	})
	detail := detailForIssue(issue, state.model, testConfig([]string{"open"}, []string{"closed"}, []string{"symphony"}, 1))
	if detail.Workspace == nil || detail.Workspace.Path != workspace.Path || detail.Workspace.Key != workspace.Key || detail.Attempt == nil || *detail.Attempt != attempt || detail.Running == nil || detail.Running.SessionID != "session-1" || detail.Running.LastMessage != "Safe worker progress" {
		t.Fatalf("issue detail lifecycle = workspace:%#v attempt:%#v running:%#v", detail.Workspace, detail.Attempt, detail.Running)
	}
}

func TestWorkerEventUpdatesSnapshotAndPublishesSafeInvalidation(t *testing.T) {
	journal := observability.NewJournal(observability.JournalOptions{})
	emitted := make(chan struct{})
	worker := WorkerFunc(func(ctx context.Context, request RunRequest, emit func(domain.AgentEvent)) domain.RunResult {
		emit(domain.AgentEvent{
			Type: "streaming_turn", At: time.Now().UTC(), SessionID: "session-secret",
			TurnCount: 3, Message: "Safe worker progress", Tokens: domain.TokenTotals{TotalTokens: 21},
		})
		close(emitted)
		<-ctx.Done()
		return domain.RunResult{Reason: domain.StopReasonOperatorStop, EndedAt: time.Now().UTC()}
	})
	orchestrator := startTestOrchestrator(t, &fakeTracker{byStates: []domain.Issue{readyIssue("1", "open", "symphony")}}, worker, func(options *Options) {
		options.Events = journal
	})
	select {
	case <-emitted:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not emit an event")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshot := mustSnapshot(t, orchestrator)
		if len(snapshot.Running) == 1 && snapshot.Running[0].TurnCount == 3 {
			row := snapshot.Running[0]
			if row.SessionID != "session-secret" || row.LastMessage != "Safe worker progress" || row.Tokens.TotalTokens != 21 {
				t.Fatalf("worker snapshot row = %#v", row)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker event did not reach snapshot: %#v", snapshot.Running)
		}
		time.Sleep(5 * time.Millisecond)
	}

	found := false
	for _, event := range journal.Recent(20).Events {
		if event.Type != "runtime.changed" || event.Data["issue_id"] != "1" {
			continue
		}
		found = true
		if len(event.Data) != 2 || event.Data["issue_identifier"] != "SYM-1" {
			t.Fatalf("runtime invalidation data = %#v, want only safe issue identity", event.Data)
		}
	}
	if !found {
		t.Fatalf("runtime.changed was not published: %#v", journal.Recent(20).Events)
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
