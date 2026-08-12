package orchestrator

import (
	"context"
	"sort"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
)

const stopStatusDeadline = 10 * time.Second

func (orchestrator *Orchestrator) startReconcile(ctx context.Context, options Options, state *actorState, generation uint64) bool {
	if len(state.model.Running) == 0 {
		return false
	}
	ids := make([]string, 0, len(state.model.Running))
	for issueID := range state.model.Running {
		ids = append(ids, issueID)
	}
	sort.Strings(ids)
	go func() {
		issues, err := options.Tracker.FetchIssuesByIDs(ctx, ids)
		orchestrator.sendInternal(ctx, reconcileFetched{generation: generation, issues: issues, err: err})
	}()
	return true
}

func (orchestrator *Orchestrator) handleReconcileFetched(ctx context.Context, options Options, state *actorState, message reconcileFetched) {
	if state.poll == nil || state.poll.generation != message.generation {
		return
	}
	if message.err != nil {
		options.Logger.Warn("running issue reconciliation failed; workers remain active", "error", message.err)
		orchestrator.fetchPollCandidates(ctx, options, state)
		return
	}
	byID := make(map[string]domain.Issue, len(message.issues))
	for _, issue := range message.issues {
		byID[issue.ID] = issue
	}
	for issueID, entry := range state.model.Running {
		if entry.Status == domain.RunStatusStopping || entry.Status == domain.RunStatusStoppingFailed {
			continue
		}
		if stallExceeded(options.Clock.Now(), entry, state.workflow.Config.Codex.StallTimeout) {
			orchestrator.requestStop(ctx, options, state, issueID, domain.StopReasonStalled, false)
			continue
		}
		issue, found := byID[issueID]
		switch {
		case !found:
			orchestrator.requestStop(ctx, options, state, issueID, domain.StopReasonMissing, false)
		case isTerminalIssue(issue, state):
			entry.Issue = issue
			state.model.Running[issueID] = entry
			orchestrator.requestStop(ctx, options, state, issueID, domain.StopReasonTerminal, true)
		case !retryIssueRoutable(issue, retryRoutingConfig(state)):
			entry.Issue = issue
			state.model.Running[issueID] = entry
			reason := domain.StopReasonInactive
			if containsNormalized(state.workflow.Config.Tracker.ActiveStates, normalizeComparable(issue.State)) {
				reason = domain.StopReasonUnroutable
			}
			orchestrator.requestStop(ctx, options, state, issueID, reason, false)
		default:
			entry.Issue = issue
			state.model.Running[issueID] = entry
		}
	}
	orchestrator.fetchPollCandidates(ctx, options, state)
}

func stallExceeded(now time.Time, entry RunningEntry, timeout time.Duration) bool {
	if timeout <= 0 {
		return false
	}
	basis := entry.StartedAt
	if !entry.LastEventAt.IsZero() {
		basis = entry.LastEventAt
	}
	return now.Sub(basis) > timeout
}

func (orchestrator *Orchestrator) requestStop(ctx context.Context, options Options, state *actorState, issueID string, reason domain.StopReason, cleanup bool) {
	entry, found := state.model.Running[issueID]
	if !found {
		return
	}
	entry.Status = domain.RunStatusStopping
	entry.StopReason = reason
	entry.StopGeneration++
	if cleanup {
		entry.CleanupConfig = state.workflow.Config
	}
	state.model.Running[issueID] = entry
	if cancel := state.cancels[issueID]; cancel != nil {
		cancel()
	}
	timer := options.Clock.NewTimer(stopStatusDeadline)
	generation := entry.StopGeneration
	go func() {
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C():
			orchestrator.sendInternal(ctx, stopDeadline{issueID: issueID, generation: generation})
		}
	}()
}

func (orchestrator *Orchestrator) handleStopDeadline(state *actorState, message stopDeadline) {
	entry, found := state.model.Running[message.issueID]
	if !found || entry.StopGeneration != message.generation || entry.Status != domain.RunStatusStopping {
		return
	}
	entry.Status = domain.RunStatusStoppingFailed
	state.model.Running[message.issueID] = entry
}
