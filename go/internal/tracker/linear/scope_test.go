package linear

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/coryj627/symphony/go/internal/tracker"
)

const expectedProjectScopeQuery = `query SymphonyProjectScope(
  $projectSlug: String!
  $first: Int!
) {
  projects(filter: { slugId: { eq: $projectSlug } }, first: $first) {
    nodes { id slugId }
    pageInfo { hasNextPage }
  }
}`

func TestProjectScopeQueryDocumentVariablesAndOrderAreExact(t *testing.T) {
	// Break caught: an unbounded or differently filtered probe cannot prove the
	// one configured project before the issue query starts.
	if SymphonyProjectScope != expectedProjectScopeQuery {
		t.Fatalf("scope query changed:\n%s", SymphonyProjectScope)
	}
	server := linearScopedFixtureServer(t,
		fixtureResponse{Body: graphQLPage(nil, false, nil)},
		fixtureResponse{Body: projectScopeBody([]projectScopeFixture{{ID: "project-1", Slug: "symphony"}}, false)},
		fixtureResponse{Body: graphQLPage(nil, false, nil)},
	)
	adapter := newLinearAdapter(t, server)
	if issues, err := adapter.FetchIssuesByStates(context.Background(), []string{" Todo "}); err != nil || issues == nil || len(issues) != 0 {
		t.Fatalf("state result = %#v, %v", issues, err)
	}
	if issues, err := adapter.FetchIssuesByIDs(context.Background(), []string{"issue-1"}); err != nil || issues == nil || len(issues) != 0 {
		t.Fatalf("ID result = %#v, %v", issues, err)
	}

	requests := server.Requests()
	wantQueries := []string{SymphonyProjectScope, SymphonyIssuesByStates, SymphonyProjectScope, SymphonyIssuesByIDs}
	if len(requests) != len(wantQueries) {
		t.Fatalf("requests = %d, want %d", len(requests), len(wantQueries))
	}
	for index, want := range wantQueries {
		if requests[index].Query != want {
			t.Fatalf("request %d query was not %q", index, operationName(want))
		}
	}
	for _, index := range []int{0, 2} {
		variables := requests[index].Variables
		if !reflect.DeepEqual(variables, map[string]any{"first": json.Number("2"), "projectSlug": "symphony"}) {
			t.Fatalf("scope variables %d = %#v", index, variables)
		}
	}
}

func TestProjectScopeResultClassificationStopsBeforeIssueRequest(t *testing.T) {
	// Break caught: an empty issue connection is ambiguous until project scope
	// is proved, while duplicate or malformed scope must fail closed as payload.
	for _, test := range []struct {
		name     string
		body     string
		category tracker.Category
	}{
		{name: "missing", body: projectScopeBody(nil, false), category: tracker.CategoryScope},
		{name: "duplicate", body: projectScopeBody([]projectScopeFixture{{ID: "project-1", Slug: "symphony"}, {ID: "project-2", Slug: "symphony"}}, false), category: tracker.CategoryPayload},
		{name: "mismatched slug", body: projectScopeBody([]projectScopeFixture{{ID: "project-1", Slug: "Symphony"}}, false), category: tracker.CategoryPayload},
		{name: "blank ID", body: projectScopeBody([]projectScopeFixture{{ID: " ", Slug: "symphony"}}, false), category: tracker.CategoryPayload},
		{name: "paginated", body: projectScopeBody([]projectScopeFixture{{ID: "project-1", Slug: "symphony"}}, true), category: tracker.CategoryPayload},
		{name: "missing projects", body: `{"data":{}}`, category: tracker.CategoryPayload},
		{name: "malformed connection", body: `{"data":{"projects":{"nodes":null,"pageInfo":{"hasNextPage":false}}}}`, category: tracker.CategoryPayload},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := linearFixtureServer(t, fixtureResponse{Body: test.body})
			got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
			if got == nil || len(got) != 0 {
				t.Fatalf("partial result = %#v", got)
			}
			portable := requireTrackerError(t, err, test.category)
			if portable.Retryable {
				t.Fatalf("scope classification was retryable: %#v", portable)
			}
			if test.category == tracker.CategoryScope && portable.Message != "Linear project is missing or inaccessible" {
				t.Fatalf("scope message = %q", portable.Message)
			}
			if len(server.Requests()) != 1 {
				t.Fatalf("requests = %d, issue request started after failed probe", len(server.Requests()))
			}
		})
	}
}

func TestValidProjectWithNoIssuesSucceedsEmpty(t *testing.T) {
	server := linearScopedFixtureServer(t, fixtureResponse{Body: graphQLPage(nil, false, nil)})
	got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("valid empty scope = %#v, %v", got, err)
	}
}

func TestProjectScopeUsesSharedHTTPAndGraphQLErrorBoundary(t *testing.T) {
	for _, test := range []struct {
		name     string
		response fixtureResponse
		category tracker.Category
	}{
		{name: "unauthorized", response: fixtureResponse{Status: http.StatusUnauthorized, Body: "credential-body-canary"}, category: tracker.CategoryAuth},
		{name: "rate limited", response: fixtureResponse{Status: http.StatusTooManyRequests, Body: `{}`}, category: tracker.CategoryRateLimited},
		{name: "GraphQL payload", response: fixtureResponse{Body: `{"data":{"projects":{"nodes":[],"pageInfo":{"hasNextPage":false}}},"errors":[{"message":"provider-canary","extensions":{"code":"INTERNAL_ERROR"}}]}`}, category: tracker.CategoryPayload},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := linearFixtureServer(t, test.response)
			got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
			if got == nil || len(got) != 0 {
				t.Fatalf("partial result = %#v", got)
			}
			requireTrackerError(t, err, test.category)
			for _, canary := range []string{"credential-body-canary", "provider-canary", tokenCanary, server.URL()} {
				if strings.Contains(err.Error(), canary) {
					t.Fatalf("error leaked %q: %v", canary, err)
				}
			}
		})
	}
}

func TestFetchIssuesByIDsValidatesEveryIDBeforeScopeRequest(t *testing.T) {
	invalidUTF8 := string([]byte{0xff})
	if utf8.ValidString(invalidUTF8) {
		t.Fatal("invalid UTF-8 fixture became valid")
	}
	for _, id := range []string{
		"", " issue-1", "issue-1 ", "issue\n1", "issue\u00851", invalidUTF8,
		strings.Repeat("x", 257),
	} {
		name := "invalid"
		if utf8.ValidString(id) {
			name = strings.ReplaceAll(id, "\n", "newline")
		}
		t.Run(name, func(t *testing.T) {
			server := linearFixtureServer(t)
			got, err := newLinearAdapter(t, server).FetchIssuesByIDs(context.Background(), []string{"issue-1", id})
			if got == nil || len(got) != 0 {
				t.Fatalf("partial result = %#v", got)
			}
			requireTrackerError(t, err, tracker.CategoryConfig)
			if len(server.Requests()) != 0 {
				t.Fatalf("invalid ID made %d requests", len(server.Requests()))
			}
		})
	}
}

func TestFetchIssuesByIDsAcceptsExact256ByteBoundaryThenProbesBeforeIssues(t *testing.T) {
	identifier := strings.Repeat("x", 256)
	server := linearScopedFixtureServer(t, fixtureResponse{Body: graphQLPage(nil, false, nil)})
	got, err := newLinearAdapter(t, server).FetchIssuesByIDs(context.Background(), []string{identifier})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("boundary result = %#v, %v", got, err)
	}
	requests := server.Requests()
	if len(requests) != 2 {
		t.Fatalf("boundary requests = %d, want 2", len(requests))
	}
	if requests[0].Query != SymphonyProjectScope || requests[1].Query != SymphonyIssuesByIDs {
		t.Fatalf("boundary request order = %#v", []string{operationName(requests[0].Query), operationName(requests[1].Query)})
	}
	if !reflect.DeepEqual(requests[1].Variables["ids"], []any{identifier}) {
		t.Fatal("256-byte ID was not preserved as one opaque issue filter")
	}
}

func linearScopedFixtureServer(t *testing.T, responses ...fixtureResponse) *fixtureServer {
	t.Helper()
	planned := make([]fixtureResponse, 0, len(responses)+1)
	planned = append(planned, fixtureResponse{Body: projectScopeBody([]projectScopeFixture{{ID: "project-1", Slug: "symphony"}}, false)})
	planned = append(planned, responses...)
	return linearFixtureServer(t, planned...)
}

type projectScopeFixture struct {
	ID   string
	Slug string
}

func projectScopeBody(nodes []projectScopeFixture, hasNext bool) string {
	items := make([]map[string]any, len(nodes))
	for index, node := range nodes {
		items[index] = map[string]any{"id": node.ID, "slugId": node.Slug}
	}
	payload := map[string]any{"data": map[string]any{"projects": map[string]any{
		"nodes": items, "pageInfo": map[string]any{"hasNextPage": hasNext},
	}}}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func issueRequests(requests []recordedRequest) []recordedRequest {
	result := make([]recordedRequest, 0, len(requests))
	for _, request := range requests {
		if request.Query != SymphonyProjectScope {
			result = append(result, request)
		}
	}
	return result
}

func operationName(query string) string {
	fields := strings.Fields(query)
	if len(fields) < 2 {
		return "unknown"
	}
	return strings.TrimSuffix(fields[1], "(")
}

func successfulBodyForLinearRequest(request *http.Request, issueBody string) (string, error) {
	var operation graphQLRequest
	if err := json.NewDecoder(request.Body).Decode(&operation); err != nil {
		return "", err
	}
	if operation.Query == SymphonyProjectScope {
		return projectScopeBody([]projectScopeFixture{{ID: "project-1", Slug: "symphony"}}, false), nil
	}
	return issueBody, nil
}
