package orchestrator

import (
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
