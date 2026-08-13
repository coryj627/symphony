package linear

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

func TestLinearToolAdvertisesExactCapturedContract(t *testing.T) {
	server := linearFixtureServer(t)
	adapter := newLinearAdapter(t, server)
	tools := adapter.AgentTools(tracker.Session{})
	if len(tools) != 1 || tools[0].Name != "linear_graphql" || tools[0].Description == "" {
		t.Fatalf("tools=%+v", tools)
	}
	schema, err := json.Marshal(tools[0].InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"oneOf"`, `"query"`, `"variables"`, `"additionalProperties":false`} {
		if !strings.Contains(string(schema), want) {
			t.Fatalf("schema %s missing %s", schema, want)
		}
	}
}

func TestLinearToolAcceptsObjectVariablesAndStringShorthand(t *testing.T) {
	server := linearFixtureServer(t,
		fixtureResponse{File: "tool-success.json"},
		fixtureResponse{Body: `{"data":{"viewer":{"id":"viewer-2"}}}`},
	)
	adapter := newLinearAdapter(t, server)
	objectResult := adapter.ExecuteAgentTool(t.Context(), domain.ToolCall{
		Name: "linear_graphql",
		Arguments: map[string]any{
			"query":     `query Viewer($includeName: Boolean!) { viewer { id name @include(if: $includeName) } }`,
			"variables": map[string]any{"includeName": true},
		},
	}, tracker.Session{})
	stringResult := adapter.ExecuteAgentTool(t.Context(), domain.ToolCall{
		Name: "linear_graphql", Arguments: `{ viewer { id } }`,
	}, tracker.Session{})
	if !objectResult.Success || !stringResult.Success {
		t.Fatalf("object=%+v shorthand=%+v", objectResult, stringResult)
	}
	requests := server.Requests()
	if len(requests) != 2 || !reflect.DeepEqual(requests[0].Variables, map[string]any{"includeName": true}) || len(requests[1].Variables) != 0 {
		t.Fatalf("requests=%+v", requests)
	}
	for _, request := range requests {
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != tokenCanary || request.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("request=%+v", request)
		}
	}
}

func TestLinearMutationAmbiguousTransportFailureIsNotReplayed(t *testing.T) {
	transport := &dropAfterReadTransport{}
	adapter, err := New(defaultLinearConfig("https://api.linear.app/graphql"), []byte(tokenCanary), &http.Client{Transport: transport}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := adapter.ExecuteAgentTool(t.Context(), domain.ToolCall{
		Name: "linear_graphql", Arguments: `mutation Update { issueUpdate(id: "x", input: {title: "y"}) { success } }`,
	}, tracker.Session{})
	if result.Success || result.Error == nil || result.Error.Code != "transport_error" || result.Error.Retryable || transport.count != 1 {
		t.Fatalf("result=%+v requests=%d", result, transport.count)
	}
}

func TestLinearToolRejectsInvalidArgumentsBeforeNetwork(t *testing.T) {
	server := linearFixtureServer(t)
	adapter := newLinearAdapter(t, server)
	for _, test := range []struct {
		name string
		args any
		code string
	}{
		{name: "wrong type", args: 42, code: "invalid_arguments"},
		{name: "missing query", args: map[string]any{"variables": map[string]any{}}, code: "invalid_arguments"},
		{name: "variables array", args: map[string]any{"query": `{ viewer { id } }`, "variables": []any{}}, code: "invalid_arguments"},
		{name: "unknown field", args: map[string]any{"query": `{ viewer { id } }`, "extra": true}, code: "invalid_arguments"},
		{name: "invalid raw json", args: json.RawMessage(`{"query":`), code: "invalid_arguments"},
		{name: "two operations", args: `query A { viewer { id } } query B { viewer { id } }`, code: "invalid_operation_count"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := adapter.ExecuteAgentTool(t.Context(), domain.ToolCall{Name: "linear_graphql", Arguments: test.args}, tracker.Session{})
			if result.Success || result.Error == nil || result.Error.Code != test.code {
				t.Fatalf("result=%+v", result)
			}
		})
	}
	if result := adapter.ExecuteAgentTool(t.Context(), domain.ToolCall{Name: "future_tool", Arguments: map[string]any{}}, tracker.Session{}); result.Error == nil || result.Error.Code != domain.ToolUnavailableCode {
		t.Fatalf("unknown tool=%+v", result)
	}
}

func TestLinearToolReturnsGraphQLErrorsWithoutDiscardingSafePartialData(t *testing.T) {
	server := linearFixtureServer(t, fixtureResponse{File: "tool-errors.json"})
	result := newLinearAdapter(t, server).ExecuteAgentTool(t.Context(), domain.ToolCall{
		Name: "linear_graphql", Arguments: `query Viewer { viewer { id } }`,
	}, tracker.Session{})
	if result.Success || result.Error == nil || result.Error.Code != "graphql_errors" || len(result.Errors) != 1 || result.Data == nil {
		t.Fatalf("result=%+v", result)
	}
}

func TestLinearToolBoundsAndRedactsProviderResponses(t *testing.T) {
	server := linearFixtureServer(t,
		fixtureResponse{Body: `{"data":{"message":"` + tokenCanary + `"}}`},
		fixtureResponse{Body: `{not json`},
		fixtureResponse{Body: `{"data":{"value":"` + strings.Repeat("x", (1<<20)+1) + `"}}`},
	)
	adapter := newLinearAdapter(t, server)
	redacted := adapter.ExecuteAgentTool(t.Context(), domain.ToolCall{Name: "linear_graphql", Arguments: `{ viewer { id } }`}, tracker.Session{})
	encoded, _ := json.Marshal(redacted)
	if !redacted.Success || strings.Contains(string(encoded), tokenCanary) || !strings.Contains(string(encoded), "[REDACTED]") {
		t.Fatalf("redacted=%s", encoded)
	}
	for _, code := range []string{"malformed_response", "response_too_large"} {
		result := adapter.ExecuteAgentTool(t.Context(), domain.ToolCall{Name: "linear_graphql", Arguments: `{ viewer { id } }`}, tracker.Session{})
		if result.Success || result.Error == nil || result.Error.Code != code {
			t.Fatalf("result=%+v want=%s", result, code)
		}
	}
}

func TestLinearToolMakesExactlyOneRequestAndExposesRateLimitMetadata(t *testing.T) {
	server := linearFixtureServer(t,
		fixtureResponse{Status: http.StatusInternalServerError, Body: `{"error":"server"}`},
		fixtureResponse{Status: http.StatusTooManyRequests, Body: `{"errors":[{"message":"slow down"}]}`, Headers: http.Header{"Retry-After": {"90"}}},
	)
	adapter := newLinearAdapter(t, server)
	mutation := adapter.ExecuteAgentTool(t.Context(), domain.ToolCall{Name: "linear_graphql", Arguments: `mutation Update { issueUpdate(id: "x", input: {title: "y"}) { success } }`}, tracker.Session{})
	if mutation.Success || mutation.Error == nil || mutation.Error.Retryable {
		t.Fatalf("mutation=%+v", mutation)
	}
	limited := adapter.ExecuteAgentTool(t.Context(), domain.ToolCall{Name: "linear_graphql", Arguments: `{ viewer { id } }`}, tracker.Session{})
	if limited.Success || limited.Error == nil || limited.Error.Code != "rate_limited" || limited.Error.Status != 429 || limited.Error.RetryAfterMS != 90000 {
		t.Fatalf("limited=%+v", limited)
	}
	if requests := server.Requests(); len(requests) != 2 {
		t.Fatalf("requests=%d want=2", len(requests))
	}
}

func TestLinearToolMissingCredentialAndCancellationMakeNoRequest(t *testing.T) {
	server := linearFixtureServer(t)
	adapter := newLinearAdapter(t, server)
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
	missing := adapter.ExecuteAgentTool(t.Context(), domain.ToolCall{Name: "linear_graphql", Arguments: `{ viewer { id } }`}, tracker.Session{})
	if missing.Error == nil || missing.Error.Code != "missing_credential" {
		t.Fatalf("missing=%+v", missing)
	}

	active := newLinearAdapter(t, server)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	canceled := active.ExecuteAgentTool(ctx, domain.ToolCall{Name: "linear_graphql", Arguments: `{ viewer { id } }`}, tracker.Session{})
	if canceled.Error == nil || canceled.Error.Code != "canceled" || canceled.Error.Retryable {
		t.Fatalf("canceled=%+v", canceled)
	}
}

type dropAfterReadTransport struct{ count int }

func (transport *dropAfterReadTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.count++
	_, _ = io.Copy(io.Discard, request.Body)
	_ = request.Body.Close()
	return nil, errors.New("connection dropped after request body was read")
}
