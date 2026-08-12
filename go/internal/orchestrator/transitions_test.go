package orchestrator

import (
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
)

func TestClaimPrecedesRunningTransitionAndReleaseIsAtomic(t *testing.T) {
	state := newState()
	issue := readyIssue("1", "open", "symphony")
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	claimRun(&state, issue, nil, now)
	if _, claimed := state.Claimed[issue.ID]; !claimed {
		t.Fatal("issue was not claimed")
	}
	if entry, running := state.Running[issue.ID]; !running || entry.Status != domain.RunStatusPreparingWorkspace {
		t.Fatalf("running entry = %#v", entry)
	}
	if err := state.assert(); err != nil {
		t.Fatalf("claim transition violated invariant: %v", err)
	}
	releaseRun(&state, issue.ID)
	if _, claimed := state.Claimed[issue.ID]; claimed {
		t.Fatal("release retained claim")
	}
	if _, running := state.Running[issue.ID]; running {
		t.Fatal("release retained running entry")
	}
}
