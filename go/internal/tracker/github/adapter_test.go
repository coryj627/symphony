package github

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/tracker"
)

var _ tracker.Adapter = (*Adapter)(nil)

func TestAdapterImplementsProviderNeutralReadAndUnavailableToolSurface(t *testing.T) {
	// Break caught: a provider adapter that returns nil collections or a Go
	// error-shaped tool response violates the live Task 1 boundary.
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("empty adapter calls must not make a request")
	}))
	defer server.Close()
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)

	if adapter.Kind() != "github" {
		t.Fatalf("kind = %q", adapter.Kind())
	}
	if tools := adapter.AgentTools(tracker.Session{}); tools == nil || len(tools) != 0 {
		t.Fatalf("tools = %#v, want non-nil empty", tools)
	}
	result := adapter.ExecuteAgentTool(context.Background(), domain.ToolCall{Name: "github_api"}, tracker.Session{})
	if result.Success || result.Error == nil || result.Error.Code != domain.ToolUnavailableCode {
		t.Fatalf("tool result = %#v", result)
	}
	if names := adapter.SecretEnvironmentNames(); !slices.Equal(names, []string{"GH_TOKEN", "GITHUB_TOKEN"}) {
		t.Fatalf("secret names = %#v", names)
	}
	for _, states := range [][]string{nil, {}, {"unsupported"}, {" ", "IN PROGRESS"}} {
		issues, err := adapter.FetchIssuesByStates(context.Background(), states)
		if err != nil || issues == nil || len(issues) != 0 {
			t.Fatalf("states %#v = %#v, %v; want non-nil empty", states, issues, err)
		}
	}
	issues, err := adapter.FetchIssuesByIDs(context.Background(), nil)
	if err != nil || issues == nil || len(issues) != 0 {
		t.Fatalf("empty IDs = %#v, %v; want non-nil empty", issues, err)
	}
}

func TestFetchIssuesByStatesFiltersCaseInsensitivelyAndSafelyLogsMalformedPeers(t *testing.T) {
	// Break caught: decoding a whole page into typed structs lets one malformed
	// issue hide valid peers or leaks provider content when that peer is logged.
	var logs lockedBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	server := githubFixtureServer(t, []fixtureResponse{{
		Path:  "/repos/coryj627/symphony/issues",
		Query: "state=all&per_page=100&page=1",
		File:  "issues-page-1.json",
	}})
	config := defaultGitHubConfig(server.URL)
	config.Assignee = "coryj627"
	adapter := mustNewGitHubAdapter(t, config, server.Client(), logger)

	issues, err := adapter.FetchIssuesByStates(context.Background(), []string{" OPEN ", "open"})
	if err != nil {
		t.Fatal(err)
	}
	assertIdentifiers(t, issues, []string{"#42"})
	if !issues[0].Dispatchable {
		t.Fatal("matching issue was not explicitly dispatchable")
	}
	output := logs.String()
	for _, expected := range []string{"github_issue_omitted", "page=1", "index=2", "reason=malformed_required_record"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("log = %q, want static field %q", output, expected)
		}
	}
	for _, forbidden := range []string{tokenCanary, "This malformed record", "missing its required title"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("log leaked %q: %s", forbidden, output)
		}
	}
}

func TestFetchIssuesByStatesReturnsAssigneeMismatchAsNondispatchable(t *testing.T) {
	// Break caught: omitting an assignee mismatch moves provider routing policy
	// into visibility semantics and prevents the scheduler/UI seeing the issue.
	server := githubFixtureServer(t, []fixtureResponse{{
		Path:  "/repos/coryj627/symphony/issues",
		Query: "state=all&per_page=100&page=1",
		Body: issuePage(`{
			"number":8,"title":"Visible mismatch","state":"open",
			"assignees":[{"login":"different-user"}]
		}`),
	}})
	config := defaultGitHubConfig(server.URL)
	config.Assignee = "required-user"
	adapter := mustNewGitHubAdapter(t, config, server.Client(), nil)
	issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"open"})
	if err != nil {
		t.Fatal(err)
	}
	assertIdentifiers(t, issues, []string{"#8"})
	if issues[0].Dispatchable {
		t.Fatal("assignee mismatch was dispatchable")
	}
}

func TestFetchIssuesByIDsRejectsEveryNoncanonicalOrOutOfScopeIDBeforeRequest(t *testing.T) {
	// Break caught: silently omitting an invalid refresh ID tells orchestration
	// that a running issue disappeared, and validating lazily can send earlier
	// requests before discovering an out-of-scope ID.
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)
	for _, ids := range [][]string{
		{"42"},
		{"github:CoryJ627/symphony#42"},
		{"github:coryj627/other#42"},
		{"github:coryj627/symphony#0"},
		{"github:coryj627/symphony#-1"},
		{"github:coryj627/symphony#042"},
		{"github:coryj627/symphony#42 "},
		{"github:coryj627/symphony#42/extra"},
		{"github:coryj627/symphony#42", "github:coryj627/other#7"},
	} {
		issues, err := adapter.FetchIssuesByIDs(context.Background(), ids)
		if issues == nil || len(issues) != 0 {
			t.Fatalf("IDs %#v returned partial %#v", ids, issues)
		}
		requireTrackerError(t, err, tracker.CategoryConfig)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("requests = %d, want zero", got)
	}
}

func TestFetchIssuesByIDsDeduplicatesInFirstSeenOrderAndOmits404(t *testing.T) {
	// Break caught: issuing duplicate refreshes wastes rate limit, reordering
	// results makes deterministic consumers unstable, and a 404 is visibility
	// loss rather than a failure for a valid in-scope dispatch ID.
	server := githubFixtureServer(t, []fixtureResponse{
		{Path: "/repos/coryj627/symphony/issues/43", Body: singleIssue(43, "closed")},
		{Path: "/repos/coryj627/symphony/issues/42", File: "issue-42.json"},
		{Path: "/repos/coryj627/symphony/issues/44", Status: http.StatusNotFound, Body: `{"message":"Not Found"}`},
	})
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)
	issues, err := adapter.FetchIssuesByIDs(context.Background(), []string{
		"github:coryj627/symphony#43",
		"github:coryj627/symphony#42",
		"github:coryj627/symphony#43",
		"github:coryj627/symphony#44",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertIdentifiers(t, issues, []string{"#43", "#42"})
}

func TestFetchIssuesByIDsRejectsReturnedNumberMismatchWithoutPartialOutput(t *testing.T) {
	// Break caught: trusting the request path instead of the response number can
	// bind a running orchestration identity to the wrong provider record.
	server := githubFixtureServer(t, []fixtureResponse{
		{Path: "/repos/coryj627/symphony/issues/41", Body: singleIssue(41, "open")},
		{Path: "/repos/coryj627/symphony/issues/42", Body: singleIssue(43, "open")},
	})
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)
	issues, err := adapter.FetchIssuesByIDs(context.Background(), []string{
		"github:coryj627/symphony#41", "github:coryj627/symphony#42",
	})
	if issues == nil || len(issues) != 0 {
		t.Fatalf("partial issues = %#v", issues)
	}
	requireTrackerError(t, err, tracker.CategoryPayload)
}

func TestFetchIssuesByIDsOmitsPullRequestsButFailsMalformedVisibleIssues(t *testing.T) {
	// Break caught: PR visibility must not masquerade as an issue refresh, while
	// omitting a malformed ordinary record has orchestration disappearance
	// semantics and therefore must fail the complete refresh.
	t.Run("pull request omitted", func(t *testing.T) {
		server := githubFixtureServer(t, []fixtureResponse{{
			Path: "/repos/coryj627/symphony/issues/9",
			Body: `{"number":9,"title":"PR","state":"open","pull_request":null}`,
		}})
		adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)
		issues, err := adapter.FetchIssuesByIDs(context.Background(), []string{"github:coryj627/symphony#9"})
		if err != nil || issues == nil || len(issues) != 0 {
			t.Fatalf("PR refresh = %#v, %v", issues, err)
		}
	})

	t.Run("pull request number mismatch still fails", func(t *testing.T) {
		server := githubFixtureServer(t, []fixtureResponse{{
			Path: "/repos/coryj627/symphony/issues/9",
			Body: `{"number":10,"title":"Wrong PR","state":"open","pull_request":{}}`,
		}})
		adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)
		issues, err := adapter.FetchIssuesByIDs(context.Background(), []string{"github:coryj627/symphony#9"})
		if issues == nil || len(issues) != 0 {
			t.Fatalf("partial issues = %#v", issues)
		}
		requireTrackerError(t, err, tracker.CategoryPayload)
	})

	t.Run("malformed pull request mismatch fails before omission", func(t *testing.T) {
		// Break caught: checking required display fields before raw identity lets a
		// malformed PR for another number masquerade as an omitted requested issue.
		server := githubFixtureServer(t, []fixtureResponse{{
			Path: "/repos/coryj627/symphony/issues/9",
			Body: `{"number":10,"state":"open","pull_request":{}}`,
		}})
		adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)
		issues, err := adapter.FetchIssuesByIDs(context.Background(), []string{"github:coryj627/symphony#9"})
		if issues == nil || len(issues) != 0 {
			t.Fatalf("partial issues = %#v", issues)
		}
		requireTrackerError(t, err, tracker.CategoryPayload)
	})

	t.Run("malformed visible record fails atomically", func(t *testing.T) {
		server := githubFixtureServer(t, []fixtureResponse{
			{Path: "/repos/coryj627/symphony/issues/8", Body: singleIssue(8, "open")},
			{Path: "/repos/coryj627/symphony/issues/9", Body: `{"number":9,"state":"open"}`},
		})
		adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)
		issues, err := adapter.FetchIssuesByIDs(context.Background(), []string{
			"github:coryj627/symphony#8", "github:coryj627/symphony#9",
		})
		if issues == nil || len(issues) != 0 {
			t.Fatalf("partial issues = %#v", issues)
		}
		requireTrackerError(t, err, tracker.CategoryPayload)
	})
}

func TestFetchIssuesByStatesRejectsInvalidTopLevelShapesAndTrailingData(t *testing.T) {
	// Break caught: treating null/object/trailing JSON as an empty or successful
	// page can silently produce an incomplete scheduler snapshot.
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "null", body: `null`},
		{name: "object", body: `{"number":1}`},
		{name: "trailing", body: `[] {}`},
		{name: "malformed", body: `[`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := githubFixtureServer(t, []fixtureResponse{{
				Path:  "/repos/coryj627/symphony/issues",
				Query: "state=all&per_page=100&page=1",
				Body:  test.body,
			}})
			adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)
			issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"open"})
			if issues == nil || len(issues) != 0 {
				t.Fatalf("issues = %#v", issues)
			}
			requireTrackerError(t, err, tracker.CategoryPayload)
		})
	}
}
