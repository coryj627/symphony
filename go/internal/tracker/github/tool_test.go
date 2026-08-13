package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/tracker"
)

func TestGitHubToolAdvertisesExactOperationAllowlistForGitHubSessionsOnly(t *testing.T) {
	server := newGitHubToolServer(t)
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL()), server.Client(), nil)
	tools := adapter.AgentTools(githubToolSessionForEndpoint(t, server.URL()))
	if len(tools) != 1 || tools[0].Name != "github_api" || tools[0].Description == "" {
		t.Fatalf("tools=%+v", tools)
	}
	encoded, err := json.Marshal(tools[0].InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range githubToolOperations {
		if !strings.Contains(string(encoded), `"`+operation+`"`) {
			t.Fatalf("schema missing %q: %s", operation, encoded)
		}
	}
	linearSession, err := tracker.NewSession(githubToolSession(t).Issue, tracker.LinearConfig{ProjectSlug: "symphony", Endpoint: "https://api.linear.app/graphql"})
	if err != nil {
		t.Fatal(err)
	}
	if tools := adapter.AgentTools(linearSession); len(tools) != 0 {
		t.Fatalf("GitHub adapter advertised to Linear session: %+v", tools)
	}
}

func TestGitHubToolUsesExactMethodsPathsBodiesAndCapturedAuthorization(t *testing.T) {
	server := newGitHubToolServer(t,
		toolFixtureResponse{Method: "GET", Path: "/repos/coryj627/symphony/issues/42", Status: 200, File: "tool-issue.json", Headers: http.Header{"X-GitHub-Request-Id": {"request-get"}}},
		toolFixtureResponse{Method: "PATCH", Path: "/repos/coryj627/symphony/issues/42", Status: 200, File: "tool-issue.json"},
		toolFixtureResponse{Method: "GET", Path: "/repos/coryj627/symphony/issues/42/comments", Query: "per_page=100&page=1", Status: 200, File: "tool-comments.json"},
		toolFixtureResponse{Method: "PUT", Path: "/repos/coryj627/symphony/issues/42/labels", Status: 200, Body: `[{"name":"bug"}]`},
		toolFixtureResponse{Method: "POST", Path: "/repos/coryj627/symphony/issues/42/assignees", Status: 201, File: "tool-issue.json"},
		toolFixtureResponse{Method: "DELETE", Path: "/repos/coryj627/symphony/issues/42/assignees", Status: 200, File: "tool-issue.json"},
	)
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL()), server.Client(), nil)
	session := githubToolSessionForEndpoint(t, server.URL())
	calls := []domain.ToolCall{
		{Name: "github_api", Arguments: map[string]any{"operation": "get_issue"}},
		{Name: "github_api", Arguments: map[string]any{"operation": "update_issue", "input": map[string]any{"title": "New title", "body": nil, "state": "closed", "state_reason": "completed", "milestone": 7}}},
		{Name: "github_api", Arguments: map[string]any{"operation": "list_comments"}},
		{Name: "github_api", Arguments: map[string]any{"operation": "set_labels", "input": map[string]any{"labels": []string{"bug"}}}},
		{Name: "github_api", Arguments: map[string]any{"operation": "add_assignees", "input": map[string]any{"assignees": []string{"octocat"}}}},
		{Name: "github_api", Arguments: map[string]any{"operation": "remove_assignees", "input": map[string]any{"assignees": []string{"octocat"}}}},
	}
	for _, call := range calls {
		result := adapter.ExecuteAgentTool(t.Context(), call, session)
		if !result.Success || result.Status < 200 || result.Status >= 300 {
			t.Fatalf("call=%+v result=%+v", call, result)
		}
	}
	requests := server.Requests()
	if len(requests) != len(calls) {
		t.Fatalf("requests=%d want=%d", len(requests), len(calls))
	}
	for _, request := range requests {
		if request.Header.Get("Authorization") != "Bearer "+tokenCanary || request.Header.Get("Accept") != "application/vnd.github+json" || request.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Fatalf("headers=%v", request.Header)
		}
	}
	if requests[0].Body == nil || len(requests[0].Body) != 0 || requests[2].Body == nil || len(requests[2].Body) != 0 {
		t.Fatalf("GET bodies=%+v %+v", requests[0].Body, requests[2].Body)
	}
	if !reflect.DeepEqual(requests[1].Body, map[string]any{"body": nil, "milestone": float64(7), "state": "closed", "state_reason": "completed", "title": "New title"}) {
		t.Fatalf("update body=%+v", requests[1].Body)
	}
	if !reflect.DeepEqual(requests[3].Body, map[string]any{"labels": []any{"bug"}}) || !reflect.DeepEqual(requests[4].Body, map[string]any{"assignees": []any{"octocat"}}) || !reflect.DeepEqual(requests[5].Body, map[string]any{"assignees": []any{"octocat"}}) {
		t.Fatalf("mutation bodies=%+v", requests[3:])
	}
}

func TestGitHubToolSafeGETRetriesButMutationNeverReplays(t *testing.T) {
	transport := &sequenceGitHubToolTransport{steps: []transportStep{
		{err: errors.New("temporary GET failure")},
		{status: 200, body: fixtureBody(t, "tool-issue.json")},
		{err: errors.New("ambiguous PATCH failure")},
	}}
	adapter, err := New(defaultGitHubConfig("https://api.github.com"), []byte(tokenCanary), &http.Client{Transport: transport}, nil)
	if err != nil {
		t.Fatal(err)
	}
	session := githubToolSession(t)
	get := adapter.ExecuteAgentTool(t.Context(), domain.ToolCall{Name: "github_api", Arguments: map[string]any{"operation": "get_issue"}}, session)
	mutation := adapter.ExecuteAgentTool(t.Context(), domain.ToolCall{Name: "github_api", Arguments: map[string]any{"operation": "update_issue", "input": map[string]any{"title": "changed"}}}, session)
	if !get.Success || mutation.Success || mutation.Error == nil || mutation.Error.Retryable || transport.requests != 3 {
		t.Fatalf("get=%+v mutation=%+v requests=%d", get, mutation, transport.requests)
	}
}

func TestGitHubToolRejectsCrossOriginRedirectWithoutSendingCredential(t *testing.T) {
	var escaped int
	destination := newGitHubToolServer(t)
	destination.server.Config.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { escaped++ })
	source := newGitHubToolServer(t, toolFixtureResponse{
		Method: "GET", Path: "/repos/coryj627/symphony/issues/42", Status: http.StatusFound,
		Headers: http.Header{"Location": {destination.URL() + "/outside"}},
	})
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(source.URL()), source.Client(), nil)
	result := adapter.ExecuteAgentTool(t.Context(), domain.ToolCall{Name: "github_api", Arguments: map[string]any{"operation": "get_issue"}}, githubToolSessionForEndpoint(t, source.URL()))
	if result.Success || escaped != 0 || source.Total() != 1 {
		t.Fatalf("result=%+v escaped=%d requests=%d", result, escaped, source.Total())
	}
}

func TestGitHubToolBoundsRedactsAndCancelsWithoutReplay(t *testing.T) {
	server := newGitHubToolServer(t,
		toolFixtureResponse{Method: "GET", Path: "/repos/coryj627/symphony/issues/42", Status: 200, Body: `{"number":42,"message":"` + tokenCanary + `"}`},
		toolFixtureResponse{Method: "GET", Path: "/repos/coryj627/symphony/issues/42", Status: 200, Body: strings.Repeat("x", (1<<20)+1)},
	)
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL()), server.Client(), nil)
	session := githubToolSessionForEndpoint(t, server.URL())
	redacted := adapter.ExecuteAgentTool(t.Context(), domain.ToolCall{Name: "github_api", Arguments: map[string]any{"operation": "get_issue"}}, session)
	encoded, _ := json.Marshal(redacted)
	if !redacted.Success || strings.Contains(string(encoded), tokenCanary) || !strings.Contains(string(encoded), "[REDACTED]") {
		t.Fatalf("redacted=%s", encoded)
	}
	oversize := adapter.ExecuteAgentTool(t.Context(), domain.ToolCall{Name: "github_api", Arguments: map[string]any{"operation": "get_issue"}}, session)
	if oversize.Success || oversize.Error == nil || oversize.Error.Code != "response_too_large" {
		t.Fatalf("oversize=%+v", oversize)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	canceled := adapter.ExecuteAgentTool(ctx, domain.ToolCall{Name: "github_api", Arguments: map[string]any{"operation": "get_issue"}}, session)
	if canceled.Success || canceled.Error == nil || canceled.Error.Code != "canceled" || server.Total() != 2 {
		t.Fatalf("canceled=%+v requests=%d", canceled, server.Total())
	}
}

func TestGitHubToolRejectsReturnedIssueOutsideCapturedScope(t *testing.T) {
	server := newGitHubToolServer(t, toolFixtureResponse{
		Method: "GET", Path: "/repos/coryj627/symphony/issues/42", Status: 200,
		Body: `{"number":43,"title":"wrong issue"}`,
	})
	result := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL()), server.Client(), nil).ExecuteAgentTool(
		t.Context(), domain.ToolCall{Name: "github_api", Arguments: map[string]any{"operation": "get_issue"}}, githubToolSessionForEndpoint(t, server.URL()),
	)
	if result.Success || result.Error == nil || result.Error.Code != "response_scope_mismatch" || result.Status != 200 {
		t.Fatalf("result=%+v", result)
	}
}

type transportStep struct {
	status int
	body   string
	err    error
}

type sequenceGitHubToolTransport struct {
	steps    []transportStep
	requests int
}

func (transport *sequenceGitHubToolTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.requests++
	if request.Body != nil {
		_, _ = io.Copy(io.Discard, request.Body)
		_ = request.Body.Close()
	}
	step := transport.steps[0]
	transport.steps = transport.steps[1:]
	if step.err != nil {
		return nil, step.err
	}
	return &http.Response{
		StatusCode: step.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(step.body)), Request: request,
	}, nil
}
