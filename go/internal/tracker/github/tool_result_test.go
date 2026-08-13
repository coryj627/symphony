package github

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/coryj627/symphony/go/internal/domain"
)

func TestGitHubToolMapsHTTPFailuresAndPropagatesSafeMetadata(t *testing.T) {
	for _, test := range []struct {
		status int
		code   string
	}{
		{status: 401, code: "authentication_failed"},
		{status: 403, code: "authorization_failed"},
		{status: 404, code: "not_found"},
		{status: 422, code: "validation_failed"},
		{status: 429, code: "rate_limited"},
		{status: 500, code: "http_error"},
	} {
		t.Run(test.code, func(t *testing.T) {
			headers := http.Header{"X-GitHub-Request-Id": {"request-42"}}
			if test.status == http.StatusTooManyRequests {
				headers.Set("Retry-After", "60")
			}
			server := newGitHubToolServer(t, toolFixtureResponse{
				Method: "PATCH", Path: "/repos/coryj627/symphony/issues/42", Status: test.status,
				Body: `{"message":"provider detail"}`, Headers: headers,
			})
			adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL()), server.Client(), nil)
			result := adapter.ExecuteAgentTool(t.Context(), domain.ToolCall{Name: "github_api", Arguments: map[string]any{
				"operation": "update_issue", "input": map[string]any{"title": "changed"},
			}}, githubToolSessionForEndpoint(t, server.URL()))
			if result.Success || result.Status != test.status || result.RequestID != "request-42" || result.Error == nil || result.Error.Code != test.code || result.Error.Status != test.status {
				t.Fatalf("result=%+v", result)
			}
			if result.Error.Retryable {
				t.Fatalf("mutation failure was retryable: %+v", result)
			}
		})
	}
}

func TestGitHubToolNeverLeaksCredentialThroughRequestIDOrBody(t *testing.T) {
	server := newGitHubToolServer(t, toolFixtureResponse{
		Method: "GET", Path: "/repos/coryj627/symphony/issues/42", Status: 200,
		Body: `{"number":42,"title":"` + tokenCanary + `"}`, Headers: http.Header{"X-GitHub-Request-Id": {tokenCanary}},
	})
	result := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL()), server.Client(), nil).ExecuteAgentTool(
		t.Context(), domain.ToolCall{Name: "github_api", Arguments: map[string]any{"operation": "get_issue"}}, githubToolSessionForEndpoint(t, server.URL()),
	)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), tokenCanary) || !strings.Contains(string(encoded), "[REDACTED]") {
		t.Fatalf("unsafe result=%s", encoded)
	}
}

func TestGitHubToolResultContractCarriesTopLevelStatusAndRequestID(t *testing.T) {
	result := domain.ToolResult{Success: true, Status: 200, RequestID: "request-42", Data: map[string]any{"id": 42}}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	for _, want := range []string{`"success":true`, `"status":200`, `"request_id":"request-42"`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("result=%s missing %s", encoded, want)
		}
	}
}
