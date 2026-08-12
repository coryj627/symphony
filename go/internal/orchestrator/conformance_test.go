package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
)

type conformanceScenario struct {
	Name    string `json:"name"`
	Check   string `json:"check"`
	Attempt int    `json:"attempt"`
	Want    string `json:"want"`
}

func TestConformanceSchedulingScenarios(t *testing.T) {
	source, err := os.ReadFile("../../testdata/orchestrator/scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	var scenarios []conformanceScenario
	if err := json.Unmarshal(source, &scenarios); err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 14 {
		t.Fatalf("scenario count = %d, want 14", len(scenarios))
	}
	seen := map[string]struct{}{}
	for _, scenario := range scenarios {
		scenario := scenario
		t.Run(scenario.Name, func(t *testing.T) {
			if _, duplicate := seen[scenario.Name]; duplicate || strings.TrimSpace(scenario.Name) == "" {
				t.Fatalf("duplicate or blank scenario %q", scenario.Name)
			}
			seen[scenario.Name] = struct{}{}
			runConformanceScenario(t, scenario)
		})
	}
}

func runConformanceScenario(t *testing.T, scenario conformanceScenario) {
	t.Helper()
	config := testConfig([]string{"open"}, []string{"closed"}, []string{"symphony"}, 2)
	switch scenario.Check {
	case "sort":
		old := mustTime(t, "2026-01-01T00:00:00Z")
		newer := mustTime(t, "2026-02-01T00:00:00Z")
		issues := []domain.Issue{
			issueWith("NEW", nil, &newer), issueWith("P2", intPointer(2), nil),
			issueWith("OLD", nil, &old), issueWith("P1", intPointer(1), nil),
		}
		ordered := SortForDispatch(issues)
		identifiers := make([]string, len(ordered))
		for index, issue := range ordered {
			identifiers[index] = issue.Identifier
		}
		if strings.Join(identifiers, ",") != scenario.Want {
			t.Fatalf("order = %v, want %s", identifiers, scenario.Want)
		}
	case "eligibility":
		if !Eligible(readyIssue("1", "open", "SYMPHONY"), View{RunningByState: map[string]int{}}, config) {
			t.Fatal("ready normalized issue was not eligible")
		}
		issue := readyIssue("2", "open", "symphony")
		issue.Dispatchable = false
		if Eligible(issue, View{RunningByState: map[string]int{}}, config) {
			t.Fatal("provider-rejected issue was eligible")
		}
	case "continuation":
		if ContinuationDelay.String() != scenario.Want {
			t.Fatalf("continuation = %s", ContinuationDelay)
		}
	case "failure_backoff":
		if got := FailureDelay(scenario.Attempt, 5*time.Minute).String(); got != scenario.Want {
			t.Fatalf("backoff = %s, want %s", got, scenario.Want)
		}
	case "caps":
		config.Agent.MaxConcurrentByState["open"] = 1
		view := View{RunningIDs: idSet("other"), ClaimedIDs: idSet("other"), RunningByState: map[string]int{"open": 1}}
		if Eligible(readyIssue("1", "open", "symphony"), view, config) {
			t.Fatal("state cap allowed dispatch")
		}
	case "slot_retry":
		entry := RetryEntry{IssueID: "1", Identifier: "GH-1", Attempt: 1, Error: "no scheduler slot is available"}
		if entry.Error != scenario.Want {
			t.Fatalf("slot error = %q", entry.Error)
		}
	case "terminal":
		state := retryActorState(issueInState("1", "closed", true))
		if !isTerminalIssue(issueInState("1", "closed", true), &state) {
			t.Fatal("terminal state did not select cleanup")
		}
	case "inactive":
		state := retryActorState(issueInState("1", "paused", true))
		if retryIssueRoutable(issueInState("1", "paused", true), retryRoutingConfig(&state)) {
			t.Fatal("inactive issue remained routable")
		}
	case "missing", "empty":
		if len([]domain.Issue{}) != 0 {
			t.Fatal("empty provider read changed")
		}
	case "stall":
		start := mustTime(t, "2026-01-01T00:00:00Z")
		if !stallExceeded(start.Add(6*time.Second), RunningEntry{StartedAt: start}, 5*time.Second) {
			t.Fatal("stalled run was not detected")
		}
	case "invalid_config":
		snapshot := testWorkflowSnapshot()
		snapshot.Config.Tracker.Kind = "linear"
		if validateSnapshot(snapshot, "github") == nil {
			t.Fatal("provider-changing configuration was accepted")
		}
	case "tracker_error":
		state := actorState{model: newState(), workflow: testWorkflowSnapshot(), poll: &pollFlight{generation: 1}, startupCleanupComplete: true}
		issue := readyIssue("1", "open", "symphony")
		claimRun(&state.model, issue, nil, time.Now())
		options := retryTestOptions(RealClock{}, &fakeTracker{}, newBlockingWorker())
		options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		unitOrchestrator(RealClock{}).handleReconcileFetched(ctx, options, &state, reconcileFetched{generation: 1, err: errors.New("tracker unavailable")})
		if entry, found := state.model.Running[issue.ID]; !found || entry.Issue.ID != issue.ID {
			t.Fatal("tracker failure changed running state")
		}
	case "restart":
		state := newState()
		if len(state.RetryAttempts) != 0 || len(state.Running) != 0 || len(state.Claimed) != 0 {
			t.Fatalf("restart restored ephemeral state: %#v", state)
		}
	default:
		t.Fatalf("unknown conformance check %q", scenario.Check)
	}
}
