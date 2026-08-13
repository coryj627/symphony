package orchestrator

import (
	"strings"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/workflow"
)

type View struct {
	RunningIDs     map[string]struct{}
	ClaimedIDs     map[string]struct{}
	RunningByState map[string]int
}

func Eligible(issue domain.Issue, view View, config workflow.EffectiveConfig) bool {
	if issue.ValidateRequired() != nil || !issue.Dispatchable {
		return false
	}
	state := normalizeComparable(issue.State)
	if state == "" || containsNormalized(config.Tracker.TerminalStates, state) || !containsNormalized(config.Tracker.ActiveStates, state) {
		return false
	}
	if !hasRequiredLabels(issue.Labels, config.Tracker.RequiredLabels) {
		return false
	}
	if _, running := view.RunningIDs[issue.ID]; running {
		return false
	}
	if _, claimed := view.ClaimedIDs[issue.ID]; claimed {
		return false
	}

	globalLimit := config.Agent.MaxConcurrent
	if globalLimit <= 0 || runningCount(view) >= globalLimit {
		return false
	}
	stateLimit := globalLimit
	for configuredState, configuredLimit := range config.Agent.MaxConcurrentByState {
		if normalizeComparable(configuredState) == state {
			stateLimit = configuredLimit
			break
		}
	}
	return stateLimit > 0 && runningInState(view.RunningByState, state) < stateLimit
}

func normalizeComparable(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func containsNormalized(values []string, target string) bool {
	for _, value := range values {
		if normalizeComparable(value) == target {
			return true
		}
	}
	return false
}

func hasRequiredLabels(labels, required []string) bool {
	available := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		if normalized := normalizeComparable(label); normalized != "" {
			available[normalized] = struct{}{}
		}
	}
	for _, label := range required {
		normalized := normalizeComparable(label)
		if normalized == "" {
			return false
		}
		if _, found := available[normalized]; !found {
			return false
		}
	}
	return true
}

func runningCount(view View) int {
	count := len(view.RunningIDs)
	byState := 0
	for _, stateCount := range view.RunningByState {
		if stateCount > 0 {
			byState += stateCount
		}
	}
	if byState > count {
		return byState
	}
	return count
}

func runningInState(counts map[string]int, state string) int {
	count := 0
	for configuredState, configuredCount := range counts {
		if normalizeComparable(configuredState) == state && configuredCount > 0 {
			count += configuredCount
		}
	}
	return count
}
