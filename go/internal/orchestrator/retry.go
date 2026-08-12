package orchestrator

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coryj627/symphony/go/internal/domain"
)

const maximumRetryErrorBytes = 512

type retryTimerState struct {
	generation uint64
	timer      Timer
	cancel     context.CancelFunc
}

func (orchestrator *Orchestrator) scheduleRetry(
	ctx context.Context,
	options Options,
	state *actorState,
	issue domain.Issue,
	attempt int,
	delay time.Duration,
	retryError string,
) uint64 {
	if delay < 0 {
		delay = 0
	}
	if existing, found := state.retryTimers[issue.ID]; found {
		existing.cancel()
		existing.timer.Stop()
	}
	state.retryGenerations[issue.ID]++
	generation := state.retryGenerations[issue.ID]
	timer := options.Clock.NewTimer(delay)
	timerContext, cancel := context.WithCancel(ctx)
	state.retryTimers[issue.ID] = retryTimerState{generation: generation, timer: timer, cancel: cancel}
	state.model.Claimed[issue.ID] = struct{}{}
	state.model.RetryAttempts[issue.ID] = RetryEntry{
		IssueID: issue.ID, Identifier: issue.Identifier, IssueURL: cloneStringPointer(issue.URL),
		Attempt: attempt, DueAt: options.Clock.Now().Add(delay).UTC(), Error: conciseRetryError(retryError), Generation: generation,
	}
	orchestrator.publish(options, "orchestrator.retry_scheduled", map[string]any{
		"issue_id": issue.ID, "issue_identifier": issue.Identifier, "attempt": attempt,
		"due_at": state.model.RetryAttempts[issue.ID].DueAt.Format(time.RFC3339Nano),
	})
	orchestrator.publish(options, "runtime.changed", map[string]any{"issue_id": issue.ID, "issue_identifier": issue.Identifier})
	go func() {
		select {
		case <-timerContext.Done():
		case <-timer.C():
			orchestrator.sendInternal(ctx, retryReady{issueID: issue.ID, generation: generation})
		}
	}()
	return generation
}

func conciseRetryError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= maximumRetryErrorBytes {
		return message
	}
	message = message[:maximumRetryErrorBytes]
	for !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}
	return strings.TrimSpace(message)
}

func nextFailureAttempt(attempt *int) int {
	if attempt == nil || *attempt < 1 {
		return 1
	}
	if *attempt == int(^uint(0)>>1) {
		return *attempt
	}
	return *attempt + 1
}

func retryFailureMessage(result domain.RunResult) string {
	if message := conciseRetryError(result.ErrorMessage); message != "" {
		return message
	}
	if code := conciseRetryError(result.ErrorCode); code != "" {
		return code
	}
	return conciseRetryError(string(result.Reason))
}

func retryIssueRoutable(issue domain.Issue, config workflowConfig) bool {
	return issue.ValidateRequired() == nil && issue.Dispatchable &&
		containsNormalized(config.activeStates, normalizeComparable(issue.State)) &&
		!containsNormalized(config.terminalStates, normalizeComparable(issue.State)) &&
		hasRequiredLabels(issue.Labels, config.requiredLabels)
}

type workflowConfig struct {
	activeStates   []string
	terminalStates []string
	requiredLabels []string
}

func retryRoutingConfig(state *actorState) workflowConfig {
	return workflowConfig{
		activeStates:   state.workflow.Config.Tracker.ActiveStates,
		terminalStates: state.workflow.Config.Tracker.TerminalStates,
		requiredLabels: state.workflow.Config.Tracker.RequiredLabels,
	}
}

func isTerminalIssue(issue domain.Issue, state *actorState) bool {
	return containsNormalized(state.workflow.Config.Tracker.TerminalStates, normalizeComparable(issue.State))
}

func retryView(state State, issueID string) View {
	view := stateView(state)
	delete(view.ClaimedIDs, issueID)
	delete(view.RunningIDs, issueID)
	return view
}
