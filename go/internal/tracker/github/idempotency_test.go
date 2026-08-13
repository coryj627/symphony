package github

import (
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/tracker"
)

func TestCreateCommentRequiresAndDeduplicatesIdempotencyKey(t *testing.T) {
	server := newGitHubToolServer(t, toolFixtureResponse{Method: "POST", Path: "/repos/coryj627/symphony/issues/42/comments", Status: 201, Body: `{"id":9007199254741993,"body":"hello"}`})
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL()), server.Client(), nil)
	session := githubToolSessionForEndpoint(t, server.URL())
	call := domain.ToolCall{Name: "github_api", Arguments: map[string]any{
		"operation": "create_comment", "idempotency_key": "session-key-1", "input": map[string]any{"body": "hello"},
	}}
	results := make([]domain.ToolResult, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	for index := range results {
		go func(index int) {
			defer wait.Done()
			results[index] = adapter.ExecuteAgentTool(t.Context(), call, session)
		}(index)
	}
	wait.Wait()
	if !results[0].Success || !results[1].Success || server.Count("POST", "/repos/coryj627/symphony/issues/42/comments") != 1 {
		t.Fatalf("results=%+v requests=%d", results, server.Count("POST", "/repos/coryj627/symphony/issues/42/comments"))
	}

	changed := call
	changed.Arguments = map[string]any{"operation": "create_comment", "idempotency_key": "session-key-1", "input": map[string]any{"body": "different"}}
	result := adapter.ExecuteAgentTool(t.Context(), changed, session)
	if result.Success || result.Error == nil || result.Error.Code != "idempotency_key_reused" || server.Total() != 1 {
		t.Fatalf("changed=%+v requests=%d", result, server.Total())
	}
}

func TestCreateCommentIdempotencyIsLimitedToCapturedSession(t *testing.T) {
	server := newGitHubToolServer(t,
		toolFixtureResponse{Method: "POST", Path: "/repos/coryj627/symphony/issues/42/comments", Status: 201, Body: `{"id":9007199254741993,"body":"hello"}`},
		toolFixtureResponse{Method: "POST", Path: "/repos/coryj627/symphony/issues/42/comments", Status: 201, Body: `{"id":9007199254741994,"body":"hello"}`},
	)
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL()), server.Client(), nil)
	call := domain.ToolCall{Name: "github_api", Arguments: map[string]any{"operation": "create_comment", "idempotency_key": "same-key", "input": map[string]any{"body": "hello"}}}
	first := adapter.ExecuteAgentTool(t.Context(), call, githubToolSessionForEndpoint(t, server.URL()))
	second := adapter.ExecuteAgentTool(t.Context(), call, githubToolSessionForEndpoint(t, server.URL()))
	if !first.Success || !second.Success || server.Total() != 2 {
		t.Fatalf("first=%+v second=%+v requests=%d", first, second, server.Total())
	}
}

func TestCreateCommentCachesAmbiguousFailureAndNeverReplays(t *testing.T) {
	transport := &sequenceGitHubToolTransport{steps: []transportStep{{err: errors.New("connection dropped after comment body")}}}
	adapter, err := New(defaultGitHubConfig("https://api.github.com"), []byte(tokenCanary), &http.Client{Transport: transport}, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := domain.ToolCall{Name: "github_api", Arguments: map[string]any{"operation": "create_comment", "idempotency_key": "ambiguous-key", "input": map[string]any{"body": "hello"}}}
	session := githubToolSession(t)
	first := adapter.ExecuteAgentTool(t.Context(), call, session)
	second := adapter.ExecuteAgentTool(t.Context(), call, githubToolSessionClone(t, session))
	if first.Success || second.Success || first.Error == nil || second.Error == nil || first.Error.Retryable || second.Error.Retryable || transport.requests != 1 {
		t.Fatalf("first=%+v second=%+v requests=%d", first, second, transport.requests)
	}
}

func githubToolSessionClone(t *testing.T, session tracker.Session) tracker.Session {
	t.Helper()
	clone, err := session.Clone()
	if err != nil {
		t.Fatal(err)
	}
	return clone
}

func githubToolSessionForEndpoint(t *testing.T, endpoint string) tracker.Session {
	t.Helper()
	config := defaultGitHubConfig(endpoint)
	base := githubToolSession(t)
	base.ProviderConfig = config
	session, err := tracker.NewSession(base.Issue, config)
	if err != nil {
		t.Fatal(err)
	}
	return session
}
