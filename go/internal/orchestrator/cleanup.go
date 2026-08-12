package orchestrator

import "context"

func (orchestrator *Orchestrator) startStartupCleanup(ctx context.Context, options Options, state *actorState) {
	if state.startupCleanupStarted {
		return
	}
	state.startupCleanupStarted = true
	go func(states []string) {
		issues, err := options.Tracker.FetchIssuesByStates(ctx, states)
		orchestrator.sendInternal(ctx, startupCleanupFetched{issues: issues, err: err})
	}(append([]string(nil), state.workflow.Config.Tracker.TerminalStates...))
}

func (orchestrator *Orchestrator) handleStartupCleanupFetched(ctx context.Context, options Options, state *actorState, message startupCleanupFetched) {
	if message.err != nil {
		options.Logger.Warn("startup terminal workspace scan failed; startup will continue", "error", message.err)
		orchestrator.completeStartupCleanup(ctx, options, state)
		return
	}
	state.startupCleanupPending = len(message.issues)
	if len(message.issues) == 0 {
		state.startupCleanupPending = 0
		orchestrator.completeStartupCleanup(ctx, options, state)
		return
	}
	config := state.workflow.Config
	for _, issue := range message.issues {
		issue := issue
		go func() {
			err := options.Workspace.Remove(ctx, issue, config)
			orchestrator.sendInternal(ctx, startupCleanupRemoved{issue: issue, err: err})
		}()
	}
}

func (orchestrator *Orchestrator) handleStartupCleanupRemoved(ctx context.Context, options Options, state *actorState, message startupCleanupRemoved) {
	if message.err != nil {
		options.Logger.Warn("startup terminal workspace cleanup failed", "issue_id", message.issue.ID, "issue_identifier", message.issue.Identifier, "error", message.err)
	}
	if state.startupCleanupPending > 0 {
		state.startupCleanupPending--
	}
	if state.startupCleanupPending == 0 {
		orchestrator.completeStartupCleanup(ctx, options, state)
	}
}

func (orchestrator *Orchestrator) completeStartupCleanup(ctx context.Context, options Options, state *actorState) {
	if state.startupCleanupComplete {
		return
	}
	state.startupCleanupComplete = true
	waiters := state.startupWaiters
	state.startupWaiters = nil
	orchestrator.startPoll(ctx, options, state, nil)
	if state.poll != nil && len(waiters) > 0 {
		state.poll.waiters = append(state.poll.waiters, waiters...)
		state.poll.manualClaimed = true
	}
}
