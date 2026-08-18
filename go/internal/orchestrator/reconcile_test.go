package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/workflow"
)

func TestReconcileTransitions(t *testing.T) {
	tests := []struct {
		name        string
		refreshed   []domain.Issue
		candidates  []domain.Issue
		wantRunning bool
		wantCleanup bool
	}{
		{name: "active routable", refreshed: issueSlice(readyIssue("1", "open", "symphony")), candidates: issueSlice(readyIssue("1", "open", "symphony")), wantRunning: true},
		{name: "terminal", refreshed: issueSlice(issueInState("1", "closed", true)), wantCleanup: true},
		{name: "inactive", refreshed: issueSlice(issueInState("1", "paused", true))},
		{name: "unroutable", refreshed: issueSlice(issueInState("1", "open", false)), candidates: issueSlice(issueInState("1", "open", false))},
		{name: "missing", refreshed: []domain.Issue{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			issue := readyIssue("1", "open", "symphony")
			adapter := &fakeTracker{
				stateResponses: []fakeTrackerResponse{{issues: []domain.Issue{}}, {issues: issueSlice(issue)}, {issues: test.candidates}},
				idResponses:    []fakeTrackerResponse{{issues: issueSlice(issue)}, {issues: test.refreshed}},
			}
			worker := newBlockingWorker()
			workspace := &fakeWorkspaceManager{}
			orchestrator := startTestOrchestrator(t, adapter, worker, func(options *Options) { options.Workspace = workspace })
			worker.waitStarted(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err := orchestrator.Refresh(ctx); err != nil {
				t.Fatalf("reconcile refresh: %v", err)
			}
			waitForRunning(t, orchestrator, test.wantRunning)
			waitForRemovalDecision(t, workspace, test.wantCleanup)
		})
	}
}

func TestActiveRefreshUpdatesRunningIssueState(t *testing.T) {
	issue := readyIssue("1", "open", "symphony")
	refreshed := issue
	refreshed.State = "in progress"
	snapshot := testWorkflowSnapshot()
	snapshot.Config.Tracker.ActiveStates = []string{"open", "in progress"}
	adapter := &fakeTracker{
		stateResponses: []fakeTrackerResponse{
			{issues: []domain.Issue{}},
			{issues: issueSlice(issue)},
			{issues: issueSlice(refreshed)},
		},
		idResponses: []fakeTrackerResponse{
			{issues: issueSlice(issue)},
			{issues: issueSlice(refreshed)},
		},
	}
	worker := newBlockingWorker()
	orchestrator := startTestOrchestrator(t, adapter, worker, func(options *Options) {
		options.Workflow = newFakeWorkflowStore(snapshot)
	})
	worker.waitStarted(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := orchestrator.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if rows := mustSnapshot(t, orchestrator).Running; len(rows) != 1 || rows[0].State != refreshed.State {
		t.Fatalf("running rows = %#v, want refreshed state %q", rows, refreshed.State)
	}
}

func TestReconcileWithoutRunningIssuesSkipsIDRead(t *testing.T) {
	adapter := &fakeTracker{stateResponses: []fakeTrackerResponse{{issues: []domain.Issue{}}, {issues: []domain.Issue{}}}}
	orchestrator := startTestOrchestrator(t, adapter, newBlockingWorker(), func(options *Options) { options.InitiallyPaused = true })
	waitForPollToSettle(t, orchestrator)
	states, ids, maximum := adapter.counts()
	if states != 2 || ids != 0 || maximum != 1 {
		t.Fatalf("provider calls = states:%d ids:%d concurrent:%d", states, ids, maximum)
	}
}

func TestReconcileRefreshErrorKeepsWorkerRunning(t *testing.T) {
	issue := readyIssue("1", "open", "symphony")
	adapter := &fakeTracker{
		stateResponses: []fakeTrackerResponse{{issues: []domain.Issue{}}, {issues: issueSlice(issue)}, {issues: issueSlice(issue)}},
		idResponses:    []fakeTrackerResponse{{issues: issueSlice(issue)}, {err: errors.New("reconcile failed")}},
	}
	worker := newBlockingWorker()
	orchestrator := startTestOrchestrator(t, adapter, worker)
	worker.waitStarted(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := orchestrator.Refresh(ctx); err != nil {
		t.Fatalf("candidate poll should continue after reconcile error: %v", err)
	}
	if snapshot := mustSnapshot(t, orchestrator); len(snapshot.Running) != 1 {
		t.Fatalf("worker changed after reconcile error: %#v", snapshot.Running)
	}
}

func TestReconcileWaitsForExitBeforeCleanupAndPreservesRecordedRoot(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	issue := readyIssue("1", "open", "symphony")
	terminal := issueInState("1", "closed", true)
	snapshot := testWorkflowSnapshot()
	snapshot.Config.Workspace.Root = "old-root"
	store := newFakeWorkflowStore(snapshot)
	adapter := &fakeTracker{
		stateResponses: []fakeTrackerResponse{{issues: []domain.Issue{}}, {issues: issueSlice(issue)}, {issues: []domain.Issue{}}},
		idResponses:    []fakeTrackerResponse{{issues: issueSlice(issue)}, {issues: issueSlice(terminal)}},
	}
	worker := newStubbornWorker()
	workspace := &fakeWorkspaceManager{}
	orchestrator := startTestOrchestrator(t, adapter, worker, func(options *Options) {
		options.Clock, options.Workflow, options.Workspace = clock, store, workspace
	})
	select {
	case <-worker.started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	changed := snapshot
	changed.Digest = "changed-digest"
	changed.Config.Workspace.Root = "new-root"
	store.setCurrent(changed)
	store.changes <- workflow.Change{
		Snapshot: changed,
		Digest:   changed.Digest,
		Validation: workflow.ValidationResult{
			Valid:        true,
			FieldErrors:  []workflow.FieldError{},
			GlobalErrors: []workflow.SafeError{},
		},
	}
	waitForActiveConfig(t, orchestrator, changed.Digest)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := orchestrator.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	select {
	case <-worker.canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("worker was not canceled")
	}
	if workspace.removeCount() != 0 {
		t.Fatal("workspace cleanup began before the worker exited")
	}
	if got := mustSnapshot(t, orchestrator).Running[0].LastEvent; got != string(domain.RunStatusStopping) {
		t.Fatalf("run status = %q, want stopping", got)
	}
	clock.lastTimer(t).forceFire(now.Add(10 * time.Second))
	waitForRunEvent(t, orchestrator, string(domain.RunStatusStoppingFailed))
	if workspace.removeCount() != 0 {
		t.Fatal("deadline incorrectly implied process exit")
	}
	worker.release <- domain.RunResult{Reason: domain.StopReasonOperatorStop}
	waitForRunning(t, orchestrator, false)
	waitForRemovalDecision(t, workspace, true)
	if roots := workspace.roots(); len(roots) != 1 || roots[0] != "old-root" {
		t.Fatalf("cleanup roots = %#v, want recorded old-root", roots)
	}
}

func waitForActiveConfig(t *testing.T, orchestrator *Orchestrator, digest string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if mustSnapshot(t, orchestrator).Config.ActiveDigest == digest {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("active config digest did not become %q", digest)
}

func TestStallUsesLastEventAndSchedulesFailureOnlyAfterExit(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	issue := readyIssue("1", "open", "symphony")
	snapshot := testWorkflowSnapshot()
	snapshot.Config.Codex.StallTimeout = 5 * time.Second
	store := newFakeWorkflowStore(snapshot)
	adapter := &fakeTracker{
		stateResponses: []fakeTrackerResponse{{issues: []domain.Issue{}}, {issues: issueSlice(issue)}, {issues: []domain.Issue{}}},
		idResponses:    []fakeTrackerResponse{{issues: issueSlice(issue)}, {issues: issueSlice(issue)}},
	}
	worker := newBlockingWorker()
	orchestrator := startTestOrchestrator(t, adapter, worker, func(options *Options) { options.Clock, options.Workflow = clock, store })
	worker.waitStarted(t)
	clock.setNow(now.Add(6 * time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := orchestrator.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	retry := waitForRetryError(t, orchestrator, "worker stalled")
	if retry.Attempt != 1 {
		t.Fatalf("stall retry = %#v", retry)
	}
}

func TestStallBasisUsesLastEventOtherwiseStartAndDisablesAtNonPositiveTimeout(t *testing.T) {
	start := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	lastEvent := start.Add(4 * time.Second)
	entry := RunningEntry{StartedAt: start, LastEventAt: lastEvent}
	if stallExceeded(start.Add(8*time.Second), entry, 5*time.Second) {
		t.Fatal("last event was ignored")
	}
	if !stallExceeded(start.Add(10*time.Second), entry, 5*time.Second) {
		t.Fatal("elapsed time strictly beyond last event did not stall")
	}
	entry.LastEventAt = time.Time{}
	if !stallExceeded(start.Add(6*time.Second), entry, 5*time.Second) {
		t.Fatal("start time fallback did not stall")
	}
	if stallExceeded(start.Add(time.Hour), entry, 0) || stallExceeded(start.Add(time.Hour), entry, -time.Second) {
		t.Fatal("nonpositive stall timeout was not disabled")
	}
}

func issueInState(id, state string, dispatchable bool) domain.Issue {
	issue := readyIssue(id, state, "symphony")
	issue.Dispatchable = dispatchable
	return issue
}

func waitForRunning(t *testing.T, orchestrator *Orchestrator, want bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		running := len(mustSnapshot(t, orchestrator).Running) > 0
		if running == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("running state did not become %v", want)
}

func waitForRunEvent(t *testing.T, orchestrator *Orchestrator, event string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := mustSnapshot(t, orchestrator)
		if len(snapshot.Running) == 1 && snapshot.Running[0].LastEvent == event {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("run event %q did not appear", event)
}
