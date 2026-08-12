package orchestrator

import (
	"fmt"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
)

type RunningEntry struct {
	Issue       domain.Issue
	Attempt     *int
	Workspace   domain.Workspace
	Status      domain.RunStatus
	StartedAt   time.Time
	LastEventAt time.Time
	TurnCount   int
	Tokens      domain.TokenTotals
}

type RetryEntry struct {
	IssueID    string
	Identifier string
	IssueURL   *string
	Attempt    int
	DueAt      time.Time
	Error      string
	Generation uint64
}

type State struct {
	Running       map[string]RunningEntry
	Claimed       map[string]struct{}
	RetryAttempts map[string]RetryEntry
	Completed     map[string]struct{}
	CodexTotals   domain.TokenTotals
	RateLimits    map[string]any
}

func (state State) assert() error {
	for issueID, entry := range state.Running {
		if entry.Issue.ID != issueID {
			return fmt.Errorf("running key does not match issue ID: key=%q issue_id=%q", issueID, entry.Issue.ID)
		}
		if _, claimed := state.Claimed[issueID]; !claimed {
			return fmt.Errorf("running issue is not claimed: issue_id=%q", issueID)
		}
		if _, retrying := state.RetryAttempts[issueID]; retrying {
			return fmt.Errorf("issue is both running and retrying: issue_id=%q", issueID)
		}
		if entry.TurnCount < 0 || tokenTotalsNegative(entry.Tokens) {
			return fmt.Errorf("running counters are negative: issue_id=%q", issueID)
		}
		if entry.Attempt != nil && *entry.Attempt < 0 {
			return fmt.Errorf("running attempt is negative: issue_id=%q", issueID)
		}
	}
	for issueID, entry := range state.RetryAttempts {
		if entry.IssueID != issueID {
			return fmt.Errorf("retry key does not match issue ID: key=%q issue_id=%q", issueID, entry.IssueID)
		}
		if _, claimed := state.Claimed[issueID]; !claimed {
			return fmt.Errorf("retry issue is not claimed: issue_id=%q", issueID)
		}
		if entry.Attempt < 1 {
			return fmt.Errorf("retry attempt is not positive: issue_id=%q", issueID)
		}
	}
	if tokenTotalsNegative(state.CodexTotals) {
		return fmt.Errorf("token totals are negative")
	}
	return nil
}

func tokenTotalsNegative(totals domain.TokenTotals) bool {
	return totals.InputTokens < 0 || totals.OutputTokens < 0 || totals.TotalTokens < 0 || totals.SecondsRunning < 0
}
