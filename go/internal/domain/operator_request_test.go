package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestOperatorAndSnapshotClonesDoNotAliasOwnedCollections(t *testing.T) {
	t.Parallel()
	opened := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	native := map[string]any{"nested": map[string]any{"value": "original"}}
	request := OperatorRequest{
		ID: "request-1", SessionID: "session-1", IssueID: "issue-1", IssueIdentifier: "GH-1",
		Kind: "approval", Title: "Choose", Summary: "A safe summary", OpenedAt: opened,
		WarningAt: opened.Add(time.Minute), DeadlineAt: opened.Add(2 * time.Minute),
		ExtensionsUsed: 1, ExtensionsRemaining: 2,
		Choices: []OperatorChoice{{ID: "approve", Label: "Approve", Description: "Continue"}},
		Questions: []OperatorQuestion{{
			ID: "reason", Label: "Reason", Description: "Explain", Required: true,
			AllowsMultiple: true, Choices: []OperatorChoice{{ID: "safe", Label: "Safe", Description: "No risk"}},
		}},
	}
	snapshot := Snapshot{
		Candidates: []CandidateRow{{
			Issue:    Issue{ID: "issue-1", Identifier: "GH-1", Title: "Title", State: "open", NativeRef: native, Labels: []string{"ready"}, BlockedBy: []BlockerRef{}},
			Routable: true, RoutingReasons: []string{},
		}},
		Running: []RunningRow{}, Retrying: []RetryRow{}, Requests: []OperatorRequest{request},
		RateLimits: map[string]any{"future": []any{"original"}},
	}

	clone, err := snapshot.Clone()
	if err != nil {
		t.Fatal(err)
	}
	clone.Candidates[0].Issue.NativeRef["nested"].(map[string]any)["value"] = "changed"
	clone.Candidates[0].Issue.Labels[0] = "changed"
	clone.Candidates[0].RoutingReasons = append(clone.Candidates[0].RoutingReasons, "changed")
	clone.Requests[0].Choices[0].Label = "changed"
	clone.Requests[0].Questions[0].Choices[0].Label = "changed"
	clone.RateLimits["future"].([]any)[0] = "changed"

	if got := native["nested"].(map[string]any)["value"]; got != "original" {
		t.Fatalf("snapshot clone aliased native ref: %v", got)
	}
	if got := snapshot.Candidates[0].Issue.Labels[0]; got != "ready" {
		t.Fatalf("snapshot clone aliased labels: %q", got)
	}
	if got := snapshot.Requests[0].Choices[0].Label; got != "Approve" {
		t.Fatalf("snapshot clone aliased request choices: %q", got)
	}
	if got := snapshot.Requests[0].Questions[0].Choices[0].Label; got != "Safe" {
		t.Fatalf("snapshot clone aliased question choices: %q", got)
	}
	if got := snapshot.RateLimits["future"].([]any)[0]; got != "original" {
		t.Fatalf("snapshot clone aliased rate limits: %v", got)
	}
}

func TestPhaseTwoSnapshotJSONUsesEmptyCollectionsAndNeverContainsOperatorResponses(t *testing.T) {
	t.Parallel()
	snapshot := EmptySnapshot()
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, want := range []string{`"candidates":[]`, `"running":[]`, `"retrying":[]`, `"requests":[]`, `"rate_limits":null`} {
		if !strings.Contains(text, want) {
			t.Fatalf("snapshot JSON %s does not contain %s", text, want)
		}
	}
	for _, forbidden := range []string{"choice_id", "answers", "operator_response", "answer-canary"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("snapshot JSON exposed response field/value %q: %s", forbidden, text)
		}
	}
}

func TestSnapshotClonePreservesIntentionalNonNilEmptyNestedCollections(t *testing.T) {
	t.Parallel()
	snapshot := EmptySnapshot()
	snapshot.Candidates = []CandidateRow{{
		Issue: Issue{
			ID: "issue-1", Identifier: "GH-1", Title: "Title", State: "open",
			Labels: []string{}, BlockedBy: []BlockerRef{},
		},
		RoutingReasons: []string{},
	}}
	clone, err := snapshot.Clone()
	if err != nil {
		t.Fatal(err)
	}
	if clone.Candidates[0].Issue.Labels == nil || clone.Candidates[0].Issue.BlockedBy == nil || clone.Candidates[0].RoutingReasons == nil || clone.Running == nil || clone.Retrying == nil || clone.Requests == nil {
		t.Fatalf("clone collapsed intentional empty collections: %#v", clone)
	}
}

func TestOperatorResponseCloneOwnsNestedAnswers(t *testing.T) {
	t.Parallel()
	response := OperatorResponse{
		RequestID: "request-1", SessionID: "session-1", ChoiceID: "approve",
		Answers: map[string][]string{"reason": {"answer-canary"}, "empty": {}},
	}
	clone := response.Clone()
	if clone.Answers["empty"] == nil {
		t.Fatal("response clone collapsed an intentional empty answer collection")
	}
	clone.Answers["reason"][0] = "changed"
	clone.Answers["extra"] = []string{"new"}
	if got := response.Answers["reason"][0]; got != "answer-canary" || len(response.Answers) != 2 {
		t.Fatalf("response clone aliased answers: %#v", response.Answers)
	}
}
