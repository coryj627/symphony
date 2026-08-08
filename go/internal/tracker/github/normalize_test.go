package github

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/tracker"
)

func TestNormalizeIssueRecordUsesRepositoryQualifiedIdentityAndLosslessNativeRef(t *testing.T) {
	// Break caught: a bare provider number collides across configured
	// repositories, while float decoding corrupts database IDs above 2^53.
	config := defaultGitHubConfig("https://api.github.com")
	config.Owner = "CoryJ627"
	config.Repository = "SymPhony"
	config.Assignee = "coryj627"
	issue, pullRequest, err := normalizeIssueRecord(json.RawMessage(fixtureBody(t, "issue-42.json")), config)
	if err != nil {
		t.Fatal(err)
	}
	if pullRequest {
		t.Fatal("ordinary issue was classified as a pull request")
	}
	if issue.ID != "github:coryj627/symphony#42" || issue.Identifier != "#42" {
		t.Fatalf("identity = %q / %q", issue.ID, issue.Identifier)
	}
	if issue.NativeRef["owner"] != "CoryJ627" || issue.NativeRef["repository"] != "SymPhony" {
		t.Fatalf("scope native_ref = %#v", issue.NativeRef)
	}
	if number, ok := issue.NativeRef["number"].(json.Number); !ok || number.String() != "42" {
		t.Fatalf("number = %#v (%T), want json.Number(42)", issue.NativeRef["number"], issue.NativeRef["number"])
	}
	if databaseID, ok := issue.NativeRef["database_id"].(json.Number); !ok || databaseID.String() != "9007199254740993" {
		t.Fatalf("database_id = %#v (%T), want lossless json.Number", issue.NativeRef["database_id"], issue.NativeRef["database_id"])
	}
	if issue.NativeRef["node_id"] != "I_kwDOExample42" || issue.NativeRef["state_reason"] != "reopened" {
		t.Fatalf("native_ref metadata = %#v", issue.NativeRef)
	}
	if issue.Priority != nil || issue.BranchName != nil || issue.BlockedBy == nil || len(issue.BlockedBy) != 0 {
		t.Fatalf("unsupported normalized fields = %#v", issue)
	}
	if issue.Description == nil || *issue.Description != "Keep tracker reads scoped and atomic." {
		t.Fatalf("description = %#v", issue.Description)
	}
	if issue.URL == nil || *issue.URL != "https://github.com/coryj627/symphony/issues/42" {
		t.Fatalf("URL = %#v", issue.URL)
	}
	if issue.AssigneeID == nil || *issue.AssigneeID != "PrimaryUser" || !issue.Dispatchable {
		t.Fatalf("assignee/dispatchable = %#v / %v", issue.AssigneeID, issue.Dispatchable)
	}
	if !slices.Equal(issue.Labels, []string{"symphony", "bug"}) {
		t.Fatalf("labels = %#v", issue.Labels)
	}
	if issue.CreatedAt == nil || issue.CreatedAt.Location() != time.UTC || !issue.CreatedAt.Equal(time.Date(2026, 8, 8, 12, 30, 0, 0, time.UTC)) {
		t.Fatalf("created_at = %#v", issue.CreatedAt)
	}
	if issue.UpdatedAt == nil || issue.UpdatedAt.Location() != time.UTC || !issue.UpdatedAt.Equal(time.Date(2026, 8, 8, 13, 45, 0, 0, time.UTC)) {
		t.Fatalf("updated_at = %#v", issue.UpdatedAt)
	}
}

func TestNormalizeIssueRecordOptionalValuesFallBackWithoutPoisoningRequiredFields(t *testing.T) {
	// Break caught: strongly typing best-effort fields makes one malformed URL,
	// timestamp, label, assignee, or native ID hide an otherwise valid issue.
	record := json.RawMessage(`{
		"id":"not-a-number",
		"node_id":"  ",
		"number":7,
		"title":"Usable issue",
		"body":17,
		"state":" Open ",
		"state_reason":false,
		"html_url":"not a usable URL",
		"labels":{"name":"bug"},
		"assignees":[{"login":5},null,{}],
		"created_at":"not-a-time",
		"updated_at":42
	}`)
	issue, pullRequest, err := normalizeIssueRecord(record, defaultGitHubConfig("https://api.github.com"))
	if err != nil {
		t.Fatal(err)
	}
	if pullRequest || issue.ID != "github:coryj627/symphony#7" || issue.State != " Open " || !issue.Dispatchable {
		t.Fatalf("required normalization = %#v, pull request %v", issue, pullRequest)
	}
	if issue.Description != nil || issue.URL != nil || issue.AssigneeID != nil || issue.CreatedAt != nil || issue.UpdatedAt != nil {
		t.Fatalf("optional fallbacks = %#v", issue)
	}
	if issue.Labels == nil || len(issue.Labels) != 0 || issue.BlockedBy == nil {
		t.Fatalf("collection fallbacks = labels %#v blockers %#v", issue.Labels, issue.BlockedBy)
	}
	if len(issue.NativeRef) != 3 || issue.NativeRef["owner"] != "coryj627" || issue.NativeRef["repository"] != "symphony" {
		t.Fatalf("native_ref fallback = %#v", issue.NativeRef)
	}
}

func TestNormalizeIssueRecordRequiresPositiveIntegralNumberTitleAndState(t *testing.T) {
	// Break caught: optional fallback on a required field creates an issue the
	// orchestrator cannot safely identify, display, or refresh.
	for _, test := range []struct {
		name   string
		record string
	}{
		{name: "missing number", record: `{"title":"Title","state":"open"}`},
		{name: "zero number", record: `{"number":0,"title":"Title","state":"open"}`},
		{name: "negative number", record: `{"number":-1,"title":"Title","state":"open"}`},
		{name: "fractional number", record: `{"number":1.5,"title":"Title","state":"open"}`},
		{name: "string number", record: `{"number":"1","title":"Title","state":"open"}`},
		{name: "blank title", record: `{"number":1,"title":"  ","state":"open"}`},
		{name: "wrong title", record: `{"number":1,"title":false,"state":"open"}`},
		{name: "blank state", record: `{"number":1,"title":"Title","state":"\t"}`},
		{name: "wrong state", record: `{"number":1,"title":"Title","state":[]}`},
		{name: "non-object", record: `[]`},
		{name: "trailing data", record: `{"number":1,"title":"Title","state":"open"}{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := normalizeIssueRecord(json.RawMessage(test.record), defaultGitHubConfig("https://api.github.com")); err == nil {
				t.Fatal("error = nil, want malformed required record")
			}
		})
	}
}

func TestNormalizeIssueRecordDetectsEveryPullRequestKeyAndMakesItNondispatchable(t *testing.T) {
	// Break caught: checking only for a non-null pull_request object lets a PR
	// represented by a null or malformed metadata value enter the issue queue.
	for _, value := range []string{"null", `{}`, `"unexpected"`} {
		record := json.RawMessage(`{"number":9,"title":"PR","state":"open","pull_request":` + value + `}`)
		issue, pullRequest, err := normalizeIssueRecord(record, defaultGitHubConfig("https://api.github.com"))
		if err != nil {
			t.Fatal(err)
		}
		if !pullRequest || issue.Dispatchable {
			t.Fatalf("pull_request=%s normalized to pullRequest=%v dispatchable=%v", value, pullRequest, issue.Dispatchable)
		}
	}
}

func TestNormalizeIssueRecordAssigneeFilterMatchesAnyUsableLoginCaseInsensitively(t *testing.T) {
	// Break caught: inspecting only the primary assignee incorrectly marks an
	// issue ineligible when a later assignee satisfies the configured filter.
	record := json.RawMessage(`{
		"number":12,"title":"Assigned issue","state":"open",
		"assignees":[{"login":"FirstUser"},{"login":" TARGET-user "},{"login":9}]
	}`)
	config := defaultGitHubConfig("https://api.github.com")
	config.Assignee = "target-USER"
	issue, _, err := normalizeIssueRecord(record, config)
	if err != nil {
		t.Fatal(err)
	}
	if issue.AssigneeID == nil || *issue.AssigneeID != "FirstUser" || !issue.Dispatchable {
		t.Fatalf("matching assignees normalized to %#v / %v", issue.AssigneeID, issue.Dispatchable)
	}

	config.Assignee = "someone-else"
	issue, _, err = normalizeIssueRecord(record, config)
	if err != nil {
		t.Fatal(err)
	}
	if issue.AssigneeID == nil || *issue.AssigneeID != "FirstUser" || issue.Dispatchable {
		t.Fatalf("mismatching assignees normalized to %#v / %v", issue.AssigneeID, issue.Dispatchable)
	}
}

func TestNormalizeIssueRecordUsesSharedStateAndLabelNormalizationContracts(t *testing.T) {
	// Break caught: provider normalization that bypasses the shared helpers can
	// drift from scheduler comparison and required-label behavior.
	record := json.RawMessage(`{
		"number":21,"title":"Provider spelling","state":" Open ",
		"labels":[{"name":" Bug "},{"name":"BUG"},{"name":""}]
	}`)
	issue, _, err := normalizeIssueRecord(record, tracker.GitHubConfig{Owner: "Owner", Repository: "Repo"})
	if err != nil {
		t.Fatal(err)
	}
	if issue.State != " Open " || tracker.NormalizeState(issue.State) != "open" || !slices.Equal(issue.Labels, []string{"bug"}) {
		t.Fatalf("state/labels = %q / %#v", issue.State, issue.Labels)
	}
}
