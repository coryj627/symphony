package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
)

func TestNormalExitSchedulesContinuationAttemptOne(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	issue := readyIssue("1", "open", "symphony")
	adapter := &fakeTracker{byStates: []domain.Issue{issue}}
	worker := newBlockingWorker()
	orchestrator := startTestOrchestrator(t, adapter, worker, func(options *Options) { options.Clock = clock })
	worker.waitStarted(t)
	worker.release <- domain.RunResult{Reason: domain.StopReasonNormal, EndedAt: now}

	retry := waitForRetry(t, orchestrator)
	if retry.Attempt != 1 || !retry.DueAt.Equal(now.Add(ContinuationDelay)) || retry.Error != "" {
		t.Fatalf("continuation retry = %#v", retry)
	}
}

func TestRetryRowsIncludeAttemptDueIdentifierAndError(t *testing.T) {
	due := time.Date(2026, 8, 12, 12, 0, 10, 0, time.UTC)
	state := State{RetryAttempts: map[string]RetryEntry{
		"opaque-id": {
			IssueID: "opaque-id", Identifier: "SYM-42", Attempt: 3,
			DueAt: due, Error: "safe retry reason",
		},
	}}
	rows := retryRows(state)
	if len(rows) != 1 {
		t.Fatalf("retry rows = %#v", rows)
	}
	row := rows[0]
	if row.IssueID != "opaque-id" || row.IssueIdentifier != "SYM-42" || row.Attempt != 3 || !row.DueAt.Equal(due) || row.Error != "safe retry reason" {
		t.Fatalf("retry row = %#v", row)
	}
}

func TestWorkerExitAccumulatesLatestAbsoluteTokensAndRuntimeOnce(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	issue := readyIssue("1", "open", "symphony")
	adapter := &fakeTracker{byStates: []domain.Issue{issue}}
	worker := WorkerFunc(func(_ context.Context, _ RunRequest, emit func(domain.AgentEvent)) domain.RunResult {
		emit(domain.AgentEvent{At: now.Add(2 * time.Second), Tokens: domain.TokenTotals{InputTokens: 3, OutputTokens: 5, TotalTokens: 8}})
		emit(domain.AgentEvent{At: now.Add(4 * time.Second), Tokens: domain.TokenTotals{InputTokens: 7, OutputTokens: 11, TotalTokens: 18}})
		return domain.RunResult{Reason: domain.StopReasonNormal, EndedAt: now.Add(5 * time.Second)}
	})
	orchestrator := startTestOrchestrator(t, adapter, worker, func(options *Options) { options.Clock = clock })
	retry := waitForRetry(t, orchestrator)
	if retry.Attempt != 1 {
		t.Fatalf("retry = %+v", retry)
	}
	totals := mustSnapshot(t, orchestrator).CodexTotals
	if totals.InputTokens != 7 || totals.OutputTokens != 11 || totals.TotalTokens != 18 || totals.SecondsRunning != 5 {
		t.Fatalf("totals = %+v", totals)
	}
}

func TestWorkerContinuationReleaseOutcomesDoNotEnterFailureRetry(t *testing.T) {
	tests := []struct {
		name        string
		reason      domain.StopReason
		wantCleanup bool
	}{
		{name: "terminal", reason: domain.StopReasonTerminal, wantCleanup: true},
		{name: "inactive", reason: domain.StopReasonInactive},
		{name: "unroutable", reason: domain.StopReasonUnroutable},
		{name: "missing", reason: domain.StopReasonMissing},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			issue := readyIssue("1", "open", "symphony")
			adapter := &fakeTracker{byStates: []domain.Issue{issue}}
			worker := newBlockingWorker()
			workspace := &fakeWorkspaceManager{}
			orchestrator := startTestOrchestrator(t, adapter, worker, func(options *Options) { options.Workspace = workspace })
			worker.waitStarted(t)
			worker.release <- domain.RunResult{Reason: test.reason, EndedAt: time.Now().UTC()}
			waitForRunning(t, orchestrator, false)
			if snapshot := mustSnapshot(t, orchestrator); len(snapshot.Retrying) != 0 {
				t.Fatalf("release outcome scheduled retry: %#v", snapshot.Retrying)
			}
			waitForRemovalDecision(t, workspace, test.wantCleanup)
		})
	}
}

func waitForRetryError(t *testing.T, orchestrator *Orchestrator, message string) domain.RetryRow {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := mustSnapshot(t, orchestrator)
		if len(snapshot.Retrying) == 1 && snapshot.Retrying[0].Error == message {
			return snapshot.Retrying[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("retry error %q did not appear", message)
	return domain.RetryRow{}
}

func TestFailureRetryIncrementsAttemptAndUsesCurrentCap(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	issue := readyIssue("1", "open", "symphony")
	store := newFakeWorkflowStore(testWorkflowSnapshot())
	adapter := &fakeTracker{
		byStates: issueSlice(issue),
		idResponses: []fakeTrackerResponse{
			{issues: issueSlice(issue)},
			{issues: issueSlice(issue)},
		},
	}
	worker := newBlockingWorker()
	orchestrator := startTestOrchestrator(t, adapter, worker, func(options *Options) {
		options.Clock = clock
		options.Workflow = store
	})
	worker.waitStarted(t)
	worker.release <- failedRun("first")
	first := waitForRetry(t, orchestrator)
	if first.Attempt != 1 || !first.DueAt.Equal(now.Add(10*time.Second)) {
		t.Fatalf("first retry = %#v", first)
	}
	clock.lastTimer(t).forceFire(now.Add(10 * time.Second))
	secondRequest := worker.waitStarted(t)
	if secondRequest.Attempt == nil || *secondRequest.Attempt != 1 {
		t.Fatalf("retry worker attempt = %#v", secondRequest.Attempt)
	}

	snapshot := testWorkflowSnapshot()
	snapshot.Config.Agent.MaxRetryBackoff = 15 * time.Second
	store.setCurrent(snapshot)
	worker.release <- failedRun("second")
	second := waitForRetryAttempt(t, orchestrator, 2)
	if !second.DueAt.Equal(now.Add(15 * time.Second)) {
		t.Fatalf("second retry due = %s, want cap-reloaded 15s", second.DueAt)
	}
}

func TestStaleRetryTimerCannotConsumeReplacement(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	issue := readyIssue("1", "open", "symphony")
	orchestrator := unitOrchestrator(clock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state := retryActorState(issue)
	options := retryTestOptions(clock, &fakeTracker{}, newBlockingWorker())
	old := orchestrator.scheduleRetry(ctx, options, &state, issue, 1, 10*time.Second, "first")
	current := orchestrator.scheduleRetry(ctx, options, &state, issue, 2, 20*time.Second, "second")
	orchestrator.handleRetryReady(ctx, options, &state, retryReady{issueID: issue.ID, generation: old})
	entry := state.model.RetryAttempts[issue.ID]
	if entry.Generation != current || entry.Attempt != 2 || entry.Error != "second" {
		t.Fatalf("stale timer consumed replacement: %#v", entry)
	}
}

func TestRetryRefreshReleasesOrReschedulesWithoutLosingClaims(t *testing.T) {
	tests := []struct {
		name       string
		response   fakeTrackerResponse
		mutate     func(*domain.Issue)
		wantRetry  bool
		wantClaim  bool
		wantRemove bool
	}{
		{name: "fetch failure", response: fakeTrackerResponse{err: errors.New("offline")}, wantRetry: true, wantClaim: true},
		{name: "absent", response: fakeTrackerResponse{}, wantClaim: false},
		{name: "terminal", mutate: func(issue *domain.Issue) { issue.State = "closed" }, wantClaim: false, wantRemove: true},
		{name: "inactive", mutate: func(issue *domain.Issue) { issue.State = "paused" }, wantClaim: false},
		{name: "unroutable", mutate: func(issue *domain.Issue) { issue.Dispatchable = false }, wantClaim: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
			clock := newFakeClock(now)
			issue := readyIssue("1", "open", "symphony")
			refreshed := issue
			response := test.response
			if test.mutate != nil {
				test.mutate(&refreshed)
				response.issues = issueSlice(refreshed)
			}
			adapter := &fakeTracker{
				byStates:    issueSlice(issue),
				idResponses: []fakeTrackerResponse{{issues: issueSlice(issue)}, response},
			}
			worker := newBlockingWorker()
			workspace := &fakeWorkspaceManager{}
			orchestrator := startTestOrchestrator(t, adapter, worker, func(options *Options) {
				options.Clock = clock
				options.Workspace = workspace
			})
			worker.waitStarted(t)
			worker.release <- failedRun("agent failed")
			waitForRetry(t, orchestrator)
			clock.lastTimer(t).forceFire(now.Add(10 * time.Second))
			waitForRetryDecision(t, orchestrator, test.wantRetry, test.wantClaim)
			waitForRemovalDecision(t, workspace, test.wantRemove)
		})
	}
}

func TestRetrySlotExhaustionReschedulesWithExplicitReason(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	issue := readyIssue("1", "open", "symphony")
	store := newFakeWorkflowStore(testWorkflowSnapshot())
	adapter := &fakeTracker{
		byStates:    issueSlice(issue),
		idResponses: []fakeTrackerResponse{{issues: issueSlice(issue)}, {issues: issueSlice(issue)}},
	}
	worker := newBlockingWorker()
	orchestrator := startTestOrchestrator(t, adapter, worker, func(options *Options) {
		options.Clock = clock
		options.Workflow = store
	})
	worker.waitStarted(t)
	worker.release <- failedRun("agent failed")
	waitForRetry(t, orchestrator)
	snapshot := testWorkflowSnapshot()
	snapshot.Config.Agent.MaxConcurrent = 0
	store.setCurrent(snapshot)
	clock.lastTimer(t).forceFire(now.Add(10 * time.Second))
	retry := waitForRetryError(t, orchestrator, "no scheduler slot is available")
	if retry.Attempt != 1 || retry.Error != "no scheduler slot is available" {
		t.Fatalf("slot retry = %#v", retry)
	}
}

func failedRun(message string) domain.RunResult {
	return domain.RunResult{Reason: domain.StopReasonFailed, ErrorCode: "agent_failed", ErrorMessage: message}
}

func issueSlice(issues ...domain.Issue) []domain.Issue { return issues }

func waitForRetry(t *testing.T, orchestrator *Orchestrator) domain.RetryRow {
	return waitForRetryAttempt(t, orchestrator, 1)
}

func waitForRetryAttempt(t *testing.T, orchestrator *Orchestrator, attempt int) domain.RetryRow {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := mustSnapshot(t, orchestrator)
		if len(snapshot.Retrying) == 1 && snapshot.Retrying[0].Attempt == attempt {
			return snapshot.Retrying[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("retry attempt %d did not appear", attempt)
	return domain.RetryRow{}
}

func waitForRetryDecision(t *testing.T, orchestrator *Orchestrator, wantRetry, wantClaim bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := mustSnapshot(t, orchestrator)
		hasRetry := len(snapshot.Retrying) > 0
		hasClaim := len(snapshot.Candidates) > 0 && !snapshot.Candidates[0].Routable
		if hasRetry == wantRetry && hasClaim == wantClaim {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("retry decision did not settle: retry=%v claim=%v", wantRetry, wantClaim)
}

func waitForRemovalDecision(t *testing.T, workspace *fakeWorkspaceManager, want bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if (workspace.removeCount() > 0) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("workspace removal = %d, want removal %v", workspace.removeCount(), want)
}

func retryActorState(issue domain.Issue) actorState {
	state := actorState{
		workflow: testWorkflowSnapshot(), model: newState(), candidates: issueSlice(issue), scheduler: true,
		cancels: make(map[string]context.CancelFunc), retryTimers: make(map[string]retryTimerState), retryGenerations: make(map[string]uint64),
	}
	state.model.Claimed[issue.ID] = struct{}{}
	return state
}

func retryTestOptions(clock Clock, adapter *fakeTracker, worker Worker) Options {
	return Options{
		Clock: clock, Tracker: adapter, Workflow: newFakeWorkflowStore(testWorkflowSnapshot()), Worker: worker,
		Workspace: &fakeWorkspaceManager{}, Events: observability.NewJournal(observability.JournalOptions{}),
	}
}

func unitOrchestrator(clock Clock) *Orchestrator {
	return &Orchestrator{commands: make(chan any, 16), done: make(chan struct{}), clock: clock}
}
