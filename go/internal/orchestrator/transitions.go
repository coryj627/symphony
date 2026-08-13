package orchestrator

import (
	"math"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
)

func newState() State {
	return State{
		Running:       make(map[string]RunningEntry),
		Claimed:       make(map[string]struct{}),
		RetryAttempts: make(map[string]RetryEntry),
		Completed:     make(map[string]struct{}),
	}
}

func claimRun(state *State, issue domain.Issue, attempt *int, now time.Time) {
	state.Claimed[issue.ID] = struct{}{}
	state.Running[issue.ID] = RunningEntry{
		Issue: issue, Attempt: cloneAttempt(attempt), Status: domain.RunStatusPreparingWorkspace,
		StartedAt: now.UTC(), LastEventAt: now.UTC(),
	}
}

func releaseRun(state *State, issueID string) {
	delete(state.Running, issueID)
	delete(state.Claimed, issueID)
}

func accumulateRunTotals(state *State, entry RunningEntry, result domain.RunResult, now time.Time) {
	state.CodexTotals.InputTokens = saturatingTokenAdd(state.CodexTotals.InputTokens, entry.Tokens.InputTokens)
	state.CodexTotals.OutputTokens = saturatingTokenAdd(state.CodexTotals.OutputTokens, entry.Tokens.OutputTokens)
	state.CodexTotals.TotalTokens = saturatingTokenAdd(state.CodexTotals.TotalTokens, entry.Tokens.TotalTokens)
	endedAt := result.EndedAt
	if endedAt.IsZero() {
		endedAt = now
	}
	seconds := endedAt.Sub(entry.StartedAt).Seconds()
	if seconds > 0 {
		state.CodexTotals.SecondsRunning += seconds
		if state.CodexTotals.SecondsRunning > math.MaxFloat64 {
			state.CodexTotals.SecondsRunning = math.MaxFloat64
		}
	}
}

func saturatingTokenAdd(left, right int64) int64 {
	if right <= 0 {
		return left
	}
	if left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

func cloneAttempt(attempt *int) *int {
	if attempt == nil {
		return nil
	}
	value := *attempt
	return &value
}

func stateView(state State) View {
	view := View{
		RunningIDs:     make(map[string]struct{}, len(state.Running)),
		ClaimedIDs:     make(map[string]struct{}, len(state.Claimed)),
		RunningByState: make(map[string]int),
	}
	for issueID, entry := range state.Running {
		view.RunningIDs[issueID] = struct{}{}
		view.RunningByState[entry.Issue.State]++
	}
	for issueID := range state.Claimed {
		view.ClaimedIDs[issueID] = struct{}{}
	}
	return view
}
