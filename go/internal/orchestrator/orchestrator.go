package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/workflow"
)

var (
	ErrClosed        = errors.New("orchestrator_closed")
	ErrIssueNotFound = errors.New("issue_not_found")
	ErrUnavailable   = errors.New("unavailable_in_phase")
)

const commandBuffer = 256

type Orchestrator struct {
	commands  chan any
	done      chan struct{}
	cancel    context.CancelFunc
	clock     Clock
	events    EventJournal
	closeOnce sync.Once
	workers   sync.WaitGroup
}

type refreshWaiter struct {
	ctx     context.Context
	reply   chan<- refreshResult
	receipt domain.RefreshReceipt
}

type pollFlight struct {
	generation    uint64
	workflow      workflow.Snapshot
	candidates    []domain.Issue
	next          int
	waiters       []refreshWaiter
	manualClaimed bool
	err           error
}

type actorState struct {
	workflow               workflow.Snapshot
	model                  State
	candidates             []domain.Issue
	scheduler              bool
	config                 domain.ConfigStatus
	tracker                domain.TrackerStatus
	poll                   *pollFlight
	pollGeneration         uint64
	timerGeneration        uint64
	cancels                map[string]context.CancelFunc
	retryTimers            map[string]retryTimerState
	retryGenerations       map[string]uint64
	startupCleanupStarted  bool
	startupCleanupComplete bool
	startupCleanupPending  int
	startupWaiters         []refreshWaiter
}

func Start(ctx context.Context, options Options) (*Orchestrator, error) {
	if ctx == nil {
		return nil, errors.New("orchestrator context is required")
	}
	if options.Tracker == nil || options.Workflow == nil || options.Workspace == nil || options.Worker == nil || options.Events == nil {
		return nil, errors.New("tracker, workflow, workspace, worker, and event journal are required")
	}
	if options.Clock == nil {
		options.Clock = RealClock{}
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	snapshot, ok := options.Workflow.Current()
	if !ok {
		var err error
		snapshot, err = options.Workflow.Load(ctx)
		if err != nil {
			return nil, err
		}
	}
	if err := validateSnapshot(snapshot, options.Tracker.Kind()); err != nil {
		return nil, err
	}

	runContext, cancel := context.WithCancel(ctx)
	orchestrator := &Orchestrator{
		commands: make(chan any, commandBuffer), done: make(chan struct{}), cancel: cancel,
		clock: options.Clock, events: options.Events,
	}
	ready := make(chan struct{})
	go orchestrator.loop(runContext, options, snapshot, ready)
	go orchestrator.forwardConfigChanges(runContext, options.Workflow.Changes())
	select {
	case <-ready:
		return orchestrator, nil
	case <-runContext.Done():
		cancel()
		return nil, runContext.Err()
	}
}

func validateSnapshot(snapshot workflow.Snapshot, adapterKind string) error {
	kind := strings.TrimSpace(snapshot.Config.Tracker.Kind)
	if kind == "" {
		return fmt.Errorf("invalid workflow: tracker kind is required")
	}
	if !strings.EqualFold(kind, strings.TrimSpace(adapterKind)) {
		return fmt.Errorf("workflow tracker %q does not match adapter %q", kind, adapterKind)
	}
	return nil
}

func (orchestrator *Orchestrator) forwardConfigChanges(ctx context.Context, changes <-chan workflow.Change) {
	for changes != nil {
		select {
		case <-ctx.Done():
			return
		case change, open := <-changes:
			if !open {
				return
			}
			orchestrator.sendInternal(ctx, configChanged{change: change})
		}
	}
}

func (orchestrator *Orchestrator) loop(ctx context.Context, options Options, snapshot workflow.Snapshot, ready chan<- struct{}) {
	defer close(orchestrator.done)
	now := options.Clock.Now().UTC()
	state := actorState{
		workflow: snapshot, model: newState(), candidates: []domain.Issue{},
		scheduler:        !options.InitiallyPaused,
		config:           domain.ConfigStatus{State: "ready", Digest: snapshot.Digest, ActiveDigest: snapshot.Digest, ChangedAt: now},
		tracker:          domain.TrackerStatus{Kind: options.Tracker.Kind(), State: "loading"},
		cancels:          make(map[string]context.CancelFunc),
		retryTimers:      make(map[string]retryTimerState),
		retryGenerations: make(map[string]uint64),
	}
	orchestrator.startStartupCleanup(ctx, options, &state)
	close(ready)

	for {
		select {
		case <-ctx.Done():
			for _, cancel := range state.cancels {
				cancel()
			}
			for _, retry := range state.retryTimers {
				retry.cancel()
				retry.timer.Stop()
			}
			orchestrator.finishPoll(&state, ctx.Err())
			orchestrator.workers.Wait()
			return
		case message := <-orchestrator.commands:
			switch message := message.(type) {
			case snapshotRequest:
				orchestrator.handleSnapshot(options, &state, message)
			case issueRequest:
				orchestrator.handleIssue(&state, message)
			case eventsAfterRequest:
				orchestrator.handleEventsAfter(message)
			case recentEventsRequest:
				orchestrator.handleRecentEvents(message)
			case refreshRequest:
				orchestrator.handleRefresh(ctx, options, &state, message)
			case schedulerRequest:
				orchestrator.handleScheduler(ctx, options, &state, message)
			case pollTick:
				if message.generation == state.timerGeneration && state.poll == nil {
					orchestrator.startPoll(ctx, options, &state, nil)
				}
			case pollCandidates:
				orchestrator.handleCandidates(ctx, options, &state, message)
			case pollIssue:
				orchestrator.handlePollIssue(ctx, options, &state, message)
			case workerUpdate:
				orchestrator.handleWorkerUpdate(options, &state, message)
			case workerExit:
				orchestrator.handleWorkerExit(ctx, options, &state, message)
			case retryReady:
				orchestrator.handleRetryReady(ctx, options, &state, message)
			case retryFetchCompleted:
				orchestrator.handleRetryFetchCompleted(ctx, options, &state, message)
			case retryCleanupDone:
				orchestrator.handleRetryCleanupDone(options, &state, message)
			case configChanged:
				orchestrator.handleConfigChanged(ctx, options, &state, message.change)
			case startupCleanupFetched:
				orchestrator.handleStartupCleanupFetched(ctx, options, &state, message)
			case startupCleanupRemoved:
				orchestrator.handleStartupCleanupRemoved(ctx, options, &state, message)
			case reconcileFetched:
				orchestrator.handleReconcileFetched(ctx, options, &state, message)
			case stopDeadline:
				orchestrator.handleStopDeadline(options, &state, message)
			}
		}
	}
}

func (orchestrator *Orchestrator) startPoll(ctx context.Context, options Options, state *actorState, first *refreshWaiter) {
	if !state.startupCleanupComplete {
		return
	}
	if state.poll != nil {
		if first != nil {
			state.poll.waiters = append(state.poll.waiters, *first)
		}
		return
	}
	snapshot, ok := options.Workflow.Current()
	if !ok {
		snapshot = state.workflow
	}
	state.pollGeneration++
	flight := &pollFlight{generation: state.pollGeneration, workflow: snapshot, candidates: []domain.Issue{}}
	if first != nil {
		flight.waiters = append(flight.waiters, *first)
		flight.manualClaimed = true
	}
	state.poll = flight
	now := options.Clock.Now().UTC()
	state.tracker.State = "loading"
	state.tracker.LastAttemptAt = &now

	if err := validateSnapshot(snapshot, options.Tracker.Kind()); err != nil {
		state.tracker.State = "error"
		state.tracker.ErrorCode = "tracker_configuration_changed"
		state.tracker.Message = "The workflow tracker no longer matches the active adapter."
		orchestrator.finishPoll(state, err)
		orchestrator.scheduleNextPoll(ctx, options, state)
		return
	}
	if orchestrator.startReconcile(ctx, options, state, flight.generation) {
		return
	}
	orchestrator.fetchPollCandidates(ctx, options, state)
}

func (orchestrator *Orchestrator) fetchPollCandidates(ctx context.Context, options Options, state *actorState) {
	if state.poll == nil {
		return
	}
	generation := state.poll.generation
	snapshot := state.poll.workflow
	go func(generation uint64, workflowSnapshot workflow.Snapshot) {
		issues, err := options.Tracker.FetchIssuesByStates(ctx, append([]string(nil), workflowSnapshot.Config.Tracker.ActiveStates...))
		orchestrator.sendInternal(ctx, pollCandidates{generation: generation, workflow: workflowSnapshot, issues: issues, err: err})
	}(generation, snapshot)
}

func (orchestrator *Orchestrator) handleCandidates(ctx context.Context, options Options, state *actorState, message pollCandidates) {
	if state.poll == nil || state.poll.generation != message.generation {
		return
	}
	if message.err != nil {
		state.tracker.State = "error"
		state.tracker.ErrorCode = "candidate_fetch_failed"
		state.tracker.Message = "Tracker candidates could not be refreshed."
		state.tracker.Stale = len(state.candidates) > 0
		options.Logger.Error("tracker candidate refresh failed", "error", message.err)
		orchestrator.publish(options, "orchestrator.poll_failed", map[string]any{"stage": "candidates"})
		orchestrator.finishPoll(state, message.err)
		orchestrator.scheduleNextPoll(ctx, options, state)
		return
	}
	state.workflow = message.workflow
	state.config.State = "ready"
	state.config.Digest = message.workflow.Digest
	state.config.ActiveDigest = message.workflow.Digest
	state.config.UsingLastGood = false
	state.config.ErrorCode = ""
	state.config.Message = ""
	state.candidates = SortForDispatch(message.issues)
	state.poll.candidates = append([]domain.Issue(nil), state.candidates...)
	state.poll.next = 0
	state.tracker.State = "ready"
	state.tracker.Stale = false
	state.tracker.ErrorCode = ""
	state.tracker.Message = ""
	now := options.Clock.Now().UTC()
	state.tracker.LastSuccessAt = &now
	orchestrator.dispatchNext(ctx, options, state)
}

func (orchestrator *Orchestrator) dispatchNext(ctx context.Context, options Options, state *actorState) {
	if state.poll == nil {
		return
	}
	current, ok := options.Workflow.Current()
	if ok {
		if err := validateSnapshot(current, options.Tracker.Kind()); err != nil {
			state.tracker.State = "error"
			state.tracker.ErrorCode = "tracker_configuration_changed"
			state.tracker.Message = "The workflow tracker no longer matches the active adapter."
			orchestrator.finishPoll(state, err)
			orchestrator.scheduleNextPoll(ctx, options, state)
			return
		}
		state.workflow = current
	}
	if !state.scheduler {
		orchestrator.finishPoll(state, state.poll.err)
		orchestrator.scheduleNextPoll(ctx, options, state)
		return
	}
	for state.poll.next < len(state.poll.candidates) {
		candidate := state.poll.candidates[state.poll.next]
		state.poll.next++
		if !Eligible(candidate, stateView(state.model), state.workflow.Config) {
			continue
		}
		generation := state.poll.generation
		go func() {
			issues, err := options.Tracker.FetchIssuesByIDs(ctx, []string{candidate.ID})
			orchestrator.sendInternal(ctx, pollIssue{generation: generation, candidate: candidate, issues: issues, err: err})
		}()
		return
	}
	orchestrator.publish(options, "orchestrator.poll_completed", map[string]any{"candidates": len(state.candidates)})
	orchestrator.finishPoll(state, state.poll.err)
	orchestrator.scheduleNextPoll(ctx, options, state)
}

func (orchestrator *Orchestrator) handlePollIssue(ctx context.Context, options Options, state *actorState, message pollIssue) {
	if state.poll == nil || state.poll.generation != message.generation {
		return
	}
	if message.err != nil {
		state.poll.err = message.err
		options.Logger.Warn("tracker issue revalidation failed", "issue_id", message.candidate.ID, "issue_identifier", message.candidate.Identifier, "error", message.err)
		orchestrator.dispatchNext(ctx, options, state)
		return
	}
	if len(message.issues) == 1 && message.issues[0].ID == message.candidate.ID {
		issue := message.issues[0]
		current, ok := options.Workflow.Current()
		if ok {
			if err := validateSnapshot(current, options.Tracker.Kind()); err != nil {
				state.tracker.State = "error"
				state.tracker.ErrorCode = "tracker_configuration_changed"
				state.tracker.Message = "The workflow tracker no longer matches the active adapter."
				orchestrator.finishPoll(state, err)
				orchestrator.scheduleNextPoll(ctx, options, state)
				return
			}
			state.workflow = current
		}
		if state.scheduler && Eligible(issue, stateView(state.model), state.workflow.Config) {
			orchestrator.startWorker(ctx, options, state, issue, nil)
		}
	}
	orchestrator.dispatchNext(ctx, options, state)
}

func (orchestrator *Orchestrator) startWorker(ctx context.Context, options Options, state *actorState, issue domain.Issue, attempt *int) {
	now := options.Clock.Now().UTC()
	claimRun(&state.model, issue, attempt, now)
	workerContext, cancel := context.WithCancel(ctx)
	state.cancels[issue.ID] = cancel
	entry := state.model.Running[issue.ID]
	entry.CleanupConfig = state.workflow.Config
	state.model.Running[issue.ID] = entry
	request := RunRequest{Issue: issue, Attempt: cloneAttempt(attempt), Workflow: state.workflow}
	orchestrator.publish(options, "orchestrator.run_claimed", map[string]any{"issue_id": issue.ID, "issue_identifier": issue.Identifier})
	orchestrator.publish(options, "runtime.changed", map[string]any{"issue_id": issue.ID, "issue_identifier": issue.Identifier})
	orchestrator.workers.Add(1)
	go func() {
		defer orchestrator.workers.Done()
		result := options.Worker.Run(workerContext, request, func(event domain.AgentEvent) {
			orchestrator.sendInternal(workerContext, workerUpdate{issueID: issue.ID, event: cloneAgentEvent(event)})
		})
		orchestrator.sendInternal(ctx, workerExit{issueID: issue.ID, result: result})
	}()
}

func (orchestrator *Orchestrator) handleWorkerUpdate(options Options, state *actorState, message workerUpdate) {
	entry, found := state.model.Running[message.issueID]
	if !found {
		return
	}
	entry.LastEventAt = message.event.At.UTC()
	entry.TurnCount = message.event.TurnCount
	entry.Tokens = message.event.Tokens
	if message.event.SessionID != "" {
		entry.SessionID = message.event.SessionID
	}
	if message.event.Message != "" {
		entry.LastMessage = message.event.Message
	}
	if workspace := message.event.Workspace; workspace != nil && workspace.IssueID == entry.Issue.ID && workspace.IssueIdentifier == entry.Issue.Identifier {
		entry.Workspace = *workspace
	}
	if message.event.Type != "" {
		entry.Status = domain.RunStatus(message.event.Type)
	}
	state.model.Running[message.issueID] = entry
	state.model.RateLimits = cloneMap(message.event.RateLimits)
	orchestrator.publish(options, "runtime.changed", map[string]any{"issue_id": entry.Issue.ID, "issue_identifier": entry.Issue.Identifier})
}

func (orchestrator *Orchestrator) handleWorkerExit(ctx context.Context, options Options, state *actorState, message workerExit) {
	entry, found := state.model.Running[message.issueID]
	if !found {
		return
	}
	defer orchestrator.publish(options, "runtime.changed", map[string]any{"issue_id": entry.Issue.ID, "issue_identifier": entry.Issue.Identifier})
	cancel := state.cancels[message.issueID]
	delete(state.cancels, message.issueID)
	if cancel != nil {
		cancel()
	}
	if current, ok := options.Workflow.Current(); ok {
		if validateSnapshot(current, options.Tracker.Kind()) == nil {
			state.workflow = current
		}
	}
	delete(state.model.Running, message.issueID)
	if entry.StopReason != "" {
		if entry.StopReason == domain.StopReasonTerminal && options.Workspace != nil {
			go func(issue domain.Issue, config workflow.EffectiveConfig) {
				if err := options.Workspace.Remove(ctx, issue, config); err != nil {
					options.Logger.Warn("terminal workspace cleanup failed", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "error", err)
				}
			}(entry.Issue, entry.CleanupConfig)
		}
		if entry.StopReason == domain.StopReasonStalled {
			orchestrator.scheduleRetry(ctx, options, state, entry.Issue, nextFailureAttempt(entry.Attempt), FailureDelay(nextFailureAttempt(entry.Attempt), state.workflow.Config.Agent.MaxRetryBackoff), "worker stalled")
		} else {
			delete(state.model.Claimed, message.issueID)
		}
		return
	}
	switch message.result.Reason {
	case domain.StopReasonNormal:
		orchestrator.scheduleRetry(ctx, options, state, entry.Issue, 1, ContinuationDelay, "")
	case domain.StopReasonTerminal, domain.StopReasonOperatorStop:
		delete(state.model.Claimed, message.issueID)
	default:
		attempt := nextFailureAttempt(entry.Attempt)
		delay := FailureDelay(attempt, state.workflow.Config.Agent.MaxRetryBackoff)
		orchestrator.scheduleRetry(ctx, options, state, entry.Issue, attempt, delay, retryFailureMessage(message.result))
	}
}

func (orchestrator *Orchestrator) handleRetryReady(ctx context.Context, options Options, state *actorState, message retryReady) {
	entry, found := state.model.RetryAttempts[message.issueID]
	if !found || entry.Generation != message.generation || state.retryGenerations[message.issueID] != message.generation {
		return
	}
	if timer, found := state.retryTimers[message.issueID]; found && timer.generation == message.generation {
		timer.cancel()
		timer.timer.Stop()
		delete(state.retryTimers, message.issueID)
	}
	delete(state.model.RetryAttempts, message.issueID)
	orchestrator.publish(options, "runtime.changed", map[string]any{"issue_id": entry.IssueID, "issue_identifier": entry.Identifier})
	go func() {
		issues, err := options.Tracker.FetchIssuesByIDs(ctx, []string{entry.IssueID})
		orchestrator.sendInternal(ctx, retryFetchCompleted{entry: entry, generation: message.generation, issues: issues, err: err})
	}()
}

func (orchestrator *Orchestrator) handleRetryFetchCompleted(ctx context.Context, options Options, state *actorState, message retryFetchCompleted) {
	issueID := message.entry.IssueID
	if state.retryGenerations[issueID] != message.generation {
		return
	}
	if _, claimed := state.model.Claimed[issueID]; !claimed {
		return
	}
	if current, ok := options.Workflow.Current(); ok {
		if validateSnapshot(current, options.Tracker.Kind()) == nil {
			state.workflow = current
		}
	}
	if message.err != nil {
		orchestrator.rescheduleRetry(ctx, options, state, message.entry, "tracker issue refresh failed")
		return
	}
	if len(message.issues) == 0 {
		orchestrator.releaseRetry(state, issueID)
		orchestrator.publish(options, "orchestrator.retry_released", map[string]any{"issue_id": issueID, "reason": "missing"})
		return
	}
	if len(message.issues) != 1 || message.issues[0].ID != issueID {
		orchestrator.rescheduleRetry(ctx, options, state, message.entry, "tracker issue refresh was incomplete")
		return
	}
	issue := message.issues[0]
	if isTerminalIssue(issue, state) {
		if options.Workspace == nil {
			orchestrator.releaseRetry(state, issueID)
			return
		}
		go func(generation uint64, config workflow.EffectiveConfig) {
			err := options.Workspace.Remove(ctx, issue, config)
			orchestrator.sendInternal(ctx, retryCleanupDone{issueID: issueID, generation: generation, err: err})
		}(message.generation, state.workflow.Config)
		return
	}
	if !retryIssueRoutable(issue, retryRoutingConfig(state)) {
		orchestrator.releaseRetry(state, issueID)
		orchestrator.publish(options, "orchestrator.retry_released", map[string]any{"issue_id": issueID, "reason": "inactive_or_unroutable"})
		return
	}
	if !state.scheduler || !Eligible(issue, retryView(state.model, issueID), state.workflow.Config) {
		orchestrator.rescheduleRetry(ctx, options, state, message.entry, "no scheduler slot is available")
		return
	}
	orchestrator.startWorker(ctx, options, state, issue, &message.entry.Attempt)
}

func (orchestrator *Orchestrator) rescheduleRetry(ctx context.Context, options Options, state *actorState, entry RetryEntry, reason string) {
	issue := domain.Issue{ID: entry.IssueID, Identifier: entry.Identifier, Title: entry.Identifier, State: "retrying", URL: cloneStringPointer(entry.IssueURL)}
	delay := FailureDelay(entry.Attempt, state.workflow.Config.Agent.MaxRetryBackoff)
	orchestrator.scheduleRetry(ctx, options, state, issue, entry.Attempt, delay, reason)
}

func (orchestrator *Orchestrator) handleRetryCleanupDone(options Options, state *actorState, message retryCleanupDone) {
	if state.retryGenerations[message.issueID] != message.generation {
		return
	}
	if message.err != nil {
		options.Logger.Warn("terminal retry workspace cleanup failed", "issue_id", message.issueID, "error", message.err)
	}
	orchestrator.releaseRetry(state, message.issueID)
	orchestrator.publish(options, "orchestrator.retry_released", map[string]any{"issue_id": message.issueID, "reason": "terminal"})
}

func (orchestrator *Orchestrator) releaseRetry(state *actorState, issueID string) {
	if timer, found := state.retryTimers[issueID]; found {
		timer.cancel()
		timer.timer.Stop()
		delete(state.retryTimers, issueID)
	}
	delete(state.model.RetryAttempts, issueID)
	delete(state.model.Claimed, issueID)
}

func (orchestrator *Orchestrator) handleRefresh(ctx context.Context, options Options, state *actorState, request refreshRequest) {
	receipt := domain.RefreshReceipt{RequestedAt: request.requestedAt.UTC(), Operations: []string{"poll"}}
	waiter := refreshWaiter{ctx: request.ctx, reply: request.reply, receipt: receipt}
	if !state.startupCleanupComplete {
		if len(state.startupWaiters) == 0 {
			waiter.receipt.Queued = true
		} else {
			waiter.receipt.Coalesced = true
		}
		state.startupWaiters = append(state.startupWaiters, waiter)
		return
	}
	if state.poll == nil {
		waiter.receipt.Queued = true
		orchestrator.startPoll(ctx, options, state, &waiter)
		return
	}
	if !state.poll.manualClaimed {
		waiter.receipt.Queued = true
		state.poll.manualClaimed = true
	} else {
		waiter.receipt.Coalesced = true
	}
	state.poll.waiters = append(state.poll.waiters, waiter)
}

func (orchestrator *Orchestrator) finishPoll(state *actorState, err error) {
	if state.poll == nil {
		return
	}
	waiters := state.poll.waiters
	state.poll = nil
	for _, waiter := range waiters {
		select {
		case <-waiter.ctx.Done():
		case waiter.reply <- refreshResult{receipt: waiter.receipt, err: err}:
		default:
		}
	}
}

func (orchestrator *Orchestrator) scheduleNextPoll(ctx context.Context, options Options, state *actorState) {
	interval := state.workflow.Config.Polling.Interval
	if interval <= 0 {
		return
	}
	state.timerGeneration++
	generation := state.timerGeneration
	go func() {
		select {
		case <-ctx.Done():
		case <-options.Clock.After(interval):
			orchestrator.sendInternal(ctx, pollTick{generation: generation})
		}
	}()
}

func (orchestrator *Orchestrator) handleScheduler(ctx context.Context, options Options, state *actorState, request schedulerRequest) {
	if err := request.ctx.Err(); err != nil {
		request.reply <- err
		return
	}
	state.scheduler = request.enabled
	if !request.enabled {
		issueIDs := make([]string, 0, len(state.model.Running))
		for issueID := range state.model.Running {
			issueIDs = append(issueIDs, issueID)
		}
		sort.Strings(issueIDs)
		for _, issueID := range issueIDs {
			entry := state.model.Running[issueID]
			if entry.Status == domain.RunStatusStopping || entry.Status == domain.RunStatusStoppingFailed {
				continue
			}
			orchestrator.requestStop(ctx, options, state, issueID, domain.StopReasonOperatorStop, false)
		}
	}
	if request.enabled && state.poll == nil {
		orchestrator.startPoll(ctx, options, state, nil)
	}
	orchestrator.publish(options, "runtime.changed", map[string]any{})
	request.reply <- nil
}

func (orchestrator *Orchestrator) handleConfigChanged(ctx context.Context, options Options, state *actorState, change workflow.Change) {
	now := options.Clock.Now().UTC()
	state.config.ChangedAt = now
	if !change.Validation.Valid {
		state.config.State = "error"
		state.config.UsingLastGood = true
		state.config.ErrorCode = "invalid_workflow"
		state.config.Message = "The workflow change is invalid; the last known good configuration remains active."
		orchestrator.publish(options, "configuration.invalid", map[string]any{"active_digest": state.config.ActiveDigest})
		return
	}
	if err := validateSnapshot(change.Snapshot, options.Tracker.Kind()); err != nil {
		state.config.State = "error"
		state.config.UsingLastGood = true
		state.config.ErrorCode = "tracker_configuration_changed"
		state.config.Message = "The selected tracker requires an adapter rebuild."
		return
	}
	state.workflow = change.Snapshot
	state.config = domain.ConfigStatus{State: "ready", Digest: change.Digest, ActiveDigest: change.Digest, ChangedAt: now}
	if state.poll == nil {
		orchestrator.startPoll(ctx, options, state, nil)
	}
}

func (orchestrator *Orchestrator) handleSnapshot(options Options, state *actorState, request snapshotRequest) {
	if err := request.ctx.Err(); err != nil {
		request.reply <- snapshotResult{err: err}
		return
	}
	snapshot := domain.EmptySnapshot()
	snapshot.GeneratedAt = options.Clock.Now().UTC()
	snapshot.EventCursor = options.Events.Cursor()
	snapshot.Scheduler = schedulerStatus(state.scheduler, state.model)
	snapshot.Candidates = candidateRows(state.candidates, state.model, state.workflow.Config)
	snapshot.Running = runningRows(state.model)
	snapshot.Retrying = retryRows(state.model)
	snapshot.CodexTotals = state.model.CodexTotals
	snapshot.RateLimits = cloneMap(state.model.RateLimits)
	snapshot.Config = state.config
	snapshot.Tracker = state.tracker
	clone, err := snapshot.Clone()
	request.reply <- snapshotResult{snapshot: clone, err: err}
}

func schedulerStatus(enabled bool, state State) domain.SchedulerStatus {
	if enabled {
		return domain.SchedulerStatus{Available: true, Enabled: true, State: "running", Message: "The scheduler is running."}
	}
	for _, entry := range state.Running {
		if entry.Status == domain.RunStatusStopping || entry.Status == domain.RunStatusStoppingFailed {
			return domain.SchedulerStatus{Available: true, Enabled: false, State: "stopping", Message: "The scheduler is stopping active work."}
		}
	}
	return domain.SchedulerStatus{Available: true, Enabled: false, State: "paused", Message: "The scheduler is paused."}
}

func candidateRows(issues []domain.Issue, state State, config workflow.EffectiveConfig) []domain.CandidateRow {
	rows := make([]domain.CandidateRow, 0, len(issues))
	for _, issue := range issues {
		routable := Eligible(issue, stateView(state), config)
		rows = append(rows, domain.CandidateRow{Issue: issue, Routable: routable, RoutingReasons: []string{}})
	}
	return rows
}

func runningRows(state State) []domain.RunningRow {
	rows := make([]domain.RunningRow, 0, len(state.Running))
	for _, entry := range state.Running {
		rows = append(rows, domain.RunningRow{
			IssueID: entry.Issue.ID, IssueIdentifier: entry.Issue.Identifier, IssueURL: cloneStringPointer(entry.Issue.URL),
			State: entry.Issue.State, SessionID: entry.SessionID, TurnCount: entry.TurnCount, LastEvent: string(entry.Status), LastMessage: entry.LastMessage,
			StartedAt: entry.StartedAt, LastEventAt: entry.LastEventAt, Tokens: entry.Tokens,
		})
	}
	sort.Slice(rows, func(left, right int) bool { return rows[left].IssueIdentifier < rows[right].IssueIdentifier })
	return rows
}

func retryRows(state State) []domain.RetryRow {
	rows := make([]domain.RetryRow, 0, len(state.RetryAttempts))
	for _, entry := range state.RetryAttempts {
		rows = append(rows, domain.RetryRow{IssueID: entry.IssueID, IssueIdentifier: entry.Identifier, IssueURL: cloneStringPointer(entry.IssueURL), Attempt: entry.Attempt, DueAt: entry.DueAt, Error: entry.Error})
	}
	sort.Slice(rows, func(left, right int) bool { return rows[left].IssueIdentifier < rows[right].IssueIdentifier })
	return rows
}

func (orchestrator *Orchestrator) handleIssue(state *actorState, request issueRequest) {
	if err := request.ctx.Err(); err != nil {
		request.reply <- issueResult{err: err}
		return
	}
	identifier := strings.TrimSpace(request.identifier)
	for _, issue := range state.candidates {
		if strings.EqualFold(issue.Identifier, identifier) {
			detail := detailForIssue(issue, state.model, state.workflow.Config)
			clone, err := detail.Clone()
			request.reply <- issueResult{detail: clone, err: err}
			return
		}
	}
	for _, entry := range state.model.Running {
		if strings.EqualFold(entry.Issue.Identifier, identifier) {
			detail := detailForIssue(entry.Issue, state.model, state.workflow.Config)
			clone, err := detail.Clone()
			request.reply <- issueResult{detail: clone, err: err}
			return
		}
	}
	request.reply <- issueResult{err: ErrIssueNotFound}
}

func detailForIssue(issue domain.Issue, state State, config workflow.EffectiveConfig) domain.IssueDetail {
	detail := domain.IssueDetail{Issue: issue, Status: "candidate", Routable: Eligible(issue, stateView(state), config), RoutingReasons: []string{}}
	if entry, found := state.Running[issue.ID]; found {
		rows := runningRows(State{Running: map[string]RunningEntry{issue.ID: entry}})
		detail.Status = string(entry.Status)
		if detail.Status == "" {
			detail.Status = "running"
		}
		detail.Running = &rows[0]
		if entry.Workspace.Path != "" {
			workspace := entry.Workspace
			detail.Workspace = &workspace
		}
		detail.Attempt = cloneAttempt(entry.Attempt)
	}
	if retry, found := state.RetryAttempts[issue.ID]; found {
		row := domain.RetryRow{IssueID: retry.IssueID, IssueIdentifier: retry.Identifier, IssueURL: cloneStringPointer(retry.IssueURL), Attempt: retry.Attempt, DueAt: retry.DueAt, Error: retry.Error}
		detail.Status = "retrying"
		detail.Retry = &row
	}
	return detail
}

func (orchestrator *Orchestrator) handleEventsAfter(request eventsAfterRequest) {
	if err := request.ctx.Err(); err != nil {
		request.reply <- eventPageResult{err: err}
		return
	}
	request.reply <- eventPageResult{page: orchestrator.events.After(request.cursor)}
}

func (orchestrator *Orchestrator) handleRecentEvents(request recentEventsRequest) {
	if err := request.ctx.Err(); err != nil {
		request.reply <- eventPageResult{err: err}
		return
	}
	limit := request.limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 100 {
		limit = 100
	}
	request.reply <- eventPageResult{page: orchestrator.events.Recent(limit)}
}

func (orchestrator *Orchestrator) publish(options Options, eventType string, data map[string]any) {
	if _, err := options.Events.Publish(domain.Event{Type: eventType, Data: data}); err != nil {
		options.Logger.Warn("orchestrator event publish failed", "event_type", eventType, "error", err)
	}
}

func cloneAgentEvent(event domain.AgentEvent) domain.AgentEvent {
	event.RateLimits = cloneMap(event.RateLimits)
	if event.Workspace != nil {
		workspace := *event.Workspace
		event.Workspace = &workspace
	}
	return event
}

func cloneMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	encoded, err := json.Marshal(source)
	if err != nil {
		return map[string]any{}
	}
	var clone map[string]any
	if json.Unmarshal(encoded, &clone) != nil {
		return map[string]any{}
	}
	return clone
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (orchestrator *Orchestrator) sendInternal(ctx context.Context, message any) {
	select {
	case orchestrator.commands <- message:
	case <-ctx.Done():
	case <-orchestrator.done:
	}
}

func (orchestrator *Orchestrator) send(ctx context.Context, message any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case orchestrator.commands <- message:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-orchestrator.done:
		return ErrClosed
	}
}

func (orchestrator *Orchestrator) Snapshot(ctx context.Context) (domain.Snapshot, error) {
	reply := make(chan snapshotResult, 1)
	if err := orchestrator.send(ctx, snapshotRequest{ctx: ctx, reply: reply}); err != nil {
		return domain.Snapshot{}, err
	}
	select {
	case result := <-reply:
		return result.snapshot, result.err
	case <-ctx.Done():
		return domain.Snapshot{}, ctx.Err()
	case <-orchestrator.done:
		return domain.Snapshot{}, ErrClosed
	}
}

func (orchestrator *Orchestrator) Issue(ctx context.Context, identifier string) (domain.IssueDetail, error) {
	reply := make(chan issueResult, 1)
	if err := orchestrator.send(ctx, issueRequest{ctx: ctx, identifier: identifier, reply: reply}); err != nil {
		return domain.IssueDetail{}, err
	}
	select {
	case result := <-reply:
		return result.detail, result.err
	case <-ctx.Done():
		return domain.IssueDetail{}, ctx.Err()
	case <-orchestrator.done:
		return domain.IssueDetail{}, ErrClosed
	}
}

func (orchestrator *Orchestrator) EventsAfter(ctx context.Context, cursor domain.EventCursor) (domain.EventPage, error) {
	reply := make(chan eventPageResult, 1)
	if err := orchestrator.send(ctx, eventsAfterRequest{ctx: ctx, cursor: cursor, reply: reply}); err != nil {
		return domain.EventPage{}, err
	}
	return orchestrator.awaitEventPage(ctx, reply)
}

func (orchestrator *Orchestrator) RecentEvents(ctx context.Context, limit int) (domain.EventPage, error) {
	reply := make(chan eventPageResult, 1)
	if err := orchestrator.send(ctx, recentEventsRequest{ctx: ctx, limit: limit, reply: reply}); err != nil {
		return domain.EventPage{}, err
	}
	return orchestrator.awaitEventPage(ctx, reply)
}

func (orchestrator *Orchestrator) awaitEventPage(ctx context.Context, reply <-chan eventPageResult) (domain.EventPage, error) {
	select {
	case result := <-reply:
		return result.page, result.err
	case <-ctx.Done():
		return domain.EventPage{}, ctx.Err()
	case <-orchestrator.done:
		return domain.EventPage{}, ErrClosed
	}
}

func (orchestrator *Orchestrator) SubscribeEvents(cursor domain.EventCursor) <-chan struct{} {
	return orchestrator.events.Subscribe(cursor)
}

func (orchestrator *Orchestrator) Refresh(ctx context.Context) (domain.RefreshReceipt, error) {
	reply := make(chan refreshResult, 1)
	request := refreshRequest{ctx: ctx, requestedAt: orchestrator.clock.Now().UTC(), reply: reply}
	if err := orchestrator.send(ctx, request); err != nil {
		return domain.RefreshReceipt{RequestedAt: request.requestedAt, Operations: []string{"poll"}}, err
	}
	select {
	case result := <-reply:
		return result.receipt, result.err
	case <-ctx.Done():
		return domain.RefreshReceipt{RequestedAt: request.requestedAt, Operations: []string{"poll"}}, ctx.Err()
	case <-orchestrator.done:
		return domain.RefreshReceipt{RequestedAt: request.requestedAt, Operations: []string{"poll"}}, ErrClosed
	}
}

func (orchestrator *Orchestrator) SetScheduler(ctx context.Context, enabled bool) error {
	reply := make(chan error, 1)
	if err := orchestrator.send(ctx, schedulerRequest{ctx: ctx, enabled: enabled, reply: reply}); err != nil {
		return err
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-orchestrator.done:
		return ErrClosed
	}
}

func (*Orchestrator) Respond(ctx context.Context, _ domain.OperatorResponse) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrUnavailable
}

func (orchestrator *Orchestrator) Close(ctx context.Context) error {
	orchestrator.closeOnce.Do(orchestrator.cancel)
	select {
	case <-orchestrator.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
