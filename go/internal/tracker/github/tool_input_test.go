package github

import (
	"encoding/json"
	"testing"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/tracker"
)

func TestGitHubToolCannotEscapeCapturedIssueOrRepository(t *testing.T) {
	session := githubToolSession(t)
	for _, test := range []struct {
		name string
		args any
		code string
	}{
		{name: "other issue", args: map[string]any{"operation": "get_issue", "issue_number": 43}, code: "issue_scope_mismatch"},
		{name: "zero", args: map[string]any{"operation": "get_issue", "issue_number": 0}, code: "invalid_issue_number"},
		{name: "negative", args: map[string]any{"operation": "get_issue", "issue_number": -1}, code: "invalid_issue_number"},
		{name: "fraction", args: map[string]any{"operation": "get_issue", "issue_number": 42.5}, code: "invalid_issue_number"},
		{name: "string", args: map[string]any{"operation": "get_issue", "issue_number": "42"}, code: "invalid_issue_number"},
		{name: "owner injection", args: map[string]any{"operation": "get_issue", "owner": "other"}, code: "invalid_arguments"},
		{name: "repository injection", args: map[string]any{"operation": "get_issue", "repository": "other"}, code: "invalid_arguments"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, code := parseGitHubToolCall(test.args, session, defaultGitHubConfig("https://api.github.com"))
			if code != test.code {
				t.Fatalf("code=%q want=%q", code, test.code)
			}
		})
	}
}

func TestGitHubToolRejectsPullRequestAndMismatchedSessionScope(t *testing.T) {
	base := githubToolSession(t)
	for _, mutate := range []func(*tracker.Session){
		func(session *tracker.Session) {
			session.Issue.NativeRef["pull_request"] = map[string]any{"url": "https://api.github.com/pulls/42"}
		},
		func(session *tracker.Session) { session.Issue.NativeRef["owner"] = "other" },
		func(session *tracker.Session) { session.Issue.NativeRef["repository"] = "other" },
		func(session *tracker.Session) { session.Issue.NativeRef["number"] = json.Number("43") },
		func(session *tracker.Session) { session.Issue.ID = "github:coryj627/symphony#43" },
	} {
		session, err := base.Clone()
		if err != nil {
			t.Fatal(err)
		}
		mutate(&session)
		if _, code := parseGitHubToolCall(map[string]any{"operation": "get_issue"}, session, defaultGitHubConfig("https://api.github.com")); code != "invalid_session_scope" && code != "pull_request_unsupported" {
			t.Fatalf("code=%q session=%+v", code, session)
		}
	}
}

func TestGitHubToolStrictPerOperationInputs(t *testing.T) {
	session := githubToolSession(t)
	tests := []struct {
		name string
		args any
		code string
	}{
		{name: "unknown operation", args: map[string]any{"operation": "delete_issue"}, code: "unsupported_operation"},
		{name: "get input", args: map[string]any{"operation": "get_issue", "input": map[string]any{}}, code: "invalid_arguments"},
		{name: "update unknown field", args: map[string]any{"operation": "update_issue", "input": map[string]any{"title": "new", "labels": []string{"bug"}}}, code: "invalid_arguments"},
		{name: "update empty", args: map[string]any{"operation": "update_issue", "input": map[string]any{}}, code: "invalid_arguments"},
		{name: "invalid state", args: map[string]any{"operation": "update_issue", "input": map[string]any{"state": "merged"}}, code: "invalid_arguments"},
		{name: "invalid state reason", args: map[string]any{"operation": "update_issue", "input": map[string]any{"state_reason": "wontfix"}}, code: "invalid_arguments"},
		{name: "comment no key", args: map[string]any{"operation": "create_comment", "input": map[string]any{"body": "hello"}}, code: "idempotency_key_required"},
		{name: "comment empty body", args: map[string]any{"operation": "create_comment", "idempotency_key": "key", "input": map[string]any{"body": " "}}, code: "invalid_arguments"},
		{name: "labels wrong type", args: map[string]any{"operation": "set_labels", "input": map[string]any{"labels": "bug"}}, code: "invalid_arguments"},
		{name: "assignee extra", args: map[string]any{"operation": "add_assignees", "input": map[string]any{"assignees": []string{"octocat"}, "extra": true}}, code: "invalid_arguments"},
		{name: "remove empty", args: map[string]any{"operation": "remove_assignees", "input": map[string]any{"assignees": []string{}}}, code: "invalid_arguments"},
		{name: "invalid json", args: json.RawMessage(`{"operation":`), code: "invalid_arguments"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, code := parseGitHubToolCall(test.args, session, defaultGitHubConfig("https://api.github.com"))
			if code != test.code {
				t.Fatalf("code=%q want=%q", code, test.code)
			}
		})
	}
}

func TestGitHubToolAcceptsEveryAllowedOperation(t *testing.T) {
	session := githubToolSession(t)
	for _, args := range []any{
		map[string]any{"operation": "get_issue"},
		map[string]any{"operation": "update_issue", "issue_number": 42, "input": map[string]any{"title": "New", "body": nil, "state": "closed", "state_reason": "completed", "milestone": nil}},
		map[string]any{"operation": "list_comments"},
		map[string]any{"operation": "create_comment", "idempotency_key": "session-key-1", "input": map[string]any{"body": "hello"}},
		map[string]any{"operation": "set_labels", "input": map[string]any{"labels": []string{"bug", "ready"}}},
		map[string]any{"operation": "add_assignees", "input": map[string]any{"assignees": []string{"octocat"}}},
		map[string]any{"operation": "remove_assignees", "input": map[string]any{"assignees": []string{"octocat"}}},
	} {
		parsed, code := parseGitHubToolCall(args, session, defaultGitHubConfig("https://api.github.com"))
		if code != "" || parsed.issueNumber != "42" {
			t.Fatalf("args=%+v parsed=%+v code=%q", args, parsed, code)
		}
	}
}

func githubToolSession(t *testing.T) tracker.Session {
	t.Helper()
	config := defaultGitHubConfig("https://api.github.com")
	issue := domain.Issue{
		ID: "github:coryj627/symphony#42", Identifier: "#42", Title: "Issue 42", State: "open", Dispatchable: true,
		NativeRef: map[string]any{"owner": "coryj627", "repository": "symphony", "number": json.Number("42")},
		Labels:    []string{}, BlockedBy: []domain.BlockerRef{},
	}
	session, err := tracker.NewSession(issue, config)
	if err != nil {
		t.Fatal(err)
	}
	return session
}
