package orchestrator

import (
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/workflow"
)

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed
}

func intPointer(value int) *int { return &value }

func timePointer(value time.Time) *time.Time { return &value }

func issueWith(identifier string, priority *int, createdAt *time.Time) domain.Issue {
	return domain.Issue{
		ID:           "id-" + identifier,
		Identifier:   identifier,
		Title:        "Issue " + identifier,
		Priority:     priority,
		State:        "open",
		Labels:       []string{"symphony"},
		BlockedBy:    []domain.BlockerRef{},
		Dispatchable: true,
		CreatedAt:    createdAt,
	}
}

func readyIssue(id, state string, labels ...string) domain.Issue {
	return domain.Issue{
		ID:           id,
		Identifier:   "SYM-" + id,
		Title:        "Ready issue " + id,
		State:        state,
		Labels:       append([]string(nil), labels...),
		BlockedBy:    []domain.BlockerRef{},
		Dispatchable: true,
	}
}

func testConfig(activeStates, terminalStates, requiredLabels []string, maxConcurrent int) workflow.EffectiveConfig {
	return workflow.EffectiveConfig{
		Tracker: workflow.TrackerConfig{
			ActiveStates:   append([]string(nil), activeStates...),
			TerminalStates: append([]string(nil), terminalStates...),
			RequiredLabels: append([]string(nil), requiredLabels...),
		},
		Agent: workflow.AgentConfig{
			MaxConcurrent:        maxConcurrent,
			MaxConcurrentByState: map[string]int{},
		},
	}
}

func idSet(ids ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		result[id] = struct{}{}
	}
	return result
}
