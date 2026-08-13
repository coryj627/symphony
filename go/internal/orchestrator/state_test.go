package orchestrator

import (
	"strings"
	"testing"

	"github.com/coryj627/symphony/go/internal/domain"
)

func TestStateAssertAcceptsConsistentClaims(t *testing.T) {
	state := State{
		Running: map[string]RunningEntry{
			"run": {Issue: readyIssue("run", "open", "symphony")},
		},
		Claimed: idSet("run", "retry"),
		RetryAttempts: map[string]RetryEntry{
			"retry": {IssueID: "retry", Attempt: 1},
		},
		Completed:   idSet(),
		CodexTotals: domain.TokenTotals{},
	}
	if err := state.assert(); err != nil {
		t.Fatalf("consistent state rejected: %v", err)
	}
}

func TestStateAssertRejectsBrokenInvariants(t *testing.T) {
	tests := []struct {
		name  string
		state State
		want  string
	}{
		{
			name:  "running without claim",
			state: State{Running: map[string]RunningEntry{"run": {Issue: readyIssue("run", "open")}}, Claimed: idSet(), RetryAttempts: map[string]RetryEntry{}},
			want:  "running issue is not claimed",
		},
		{
			name:  "retry without claim",
			state: State{Running: map[string]RunningEntry{}, Claimed: idSet(), RetryAttempts: map[string]RetryEntry{"retry": {IssueID: "retry", Attempt: 1}}},
			want:  "retry issue is not claimed",
		},
		{
			name:  "running and retrying",
			state: State{Running: map[string]RunningEntry{"same": {Issue: readyIssue("same", "open")}}, Claimed: idSet("same"), RetryAttempts: map[string]RetryEntry{"same": {IssueID: "same", Attempt: 1}}},
			want:  "both running and retrying",
		},
		{
			name:  "running key mismatch",
			state: State{Running: map[string]RunningEntry{"key": {Issue: readyIssue("value", "open")}}, Claimed: idSet("key"), RetryAttempts: map[string]RetryEntry{}},
			want:  "running key does not match issue ID",
		},
		{
			name:  "retry key mismatch",
			state: State{Running: map[string]RunningEntry{}, Claimed: idSet("key"), RetryAttempts: map[string]RetryEntry{"key": {IssueID: "value", Attempt: 1}}},
			want:  "retry key does not match issue ID",
		},
		{
			name:  "negative token counter",
			state: State{Running: map[string]RunningEntry{}, Claimed: idSet(), RetryAttempts: map[string]RetryEntry{}, CodexTotals: domain.TokenTotals{InputTokens: -1}},
			want:  "token totals are negative",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.state.assert()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("assert() error = %v, want containing %q", err, test.want)
			}
		})
	}
}
