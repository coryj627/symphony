package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/coryj627/symphony/go/internal/tracker"
)

func TestStatePaginationRejectsContinuingMissingNullOrBlankCursorAsPagination(t *testing.T) {
	// Break caught: decoding an omitted cursor as a schema failure bypasses the
	// pagination category, while accepting null/blank could replay the page.
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "missing", body: `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":true}}}}`},
		{name: "null", body: graphQLPage(nil, true, nil)},
		{name: "blank", body: graphQLPage(nil, true, " ")},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := linearFixtureServer(t, fixtureResponse{Body: test.body})
			got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
			if got == nil || len(got) != 0 {
				t.Fatalf("partial result = %#v", got)
			}
			requireTrackerError(t, err, tracker.CategoryPagination)
		})
	}
}

func TestIssuePageRejectsNumericCursorAsPayload(t *testing.T) {
	// Break caught: coercing a numeric cursor to nil accepts malformed final data
	// and misclassifies malformed continuing data as a pagination omission.
	for _, hasNext := range []bool{false, true} {
		name := "final"
		if hasNext {
			name = "continuing"
		}
		t.Run(name, func(t *testing.T) {
			server := linearFixtureServer(t, fixtureResponse{Body: graphQLPage(nil, hasNext, 123)})
			got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
			if got == nil || len(got) != 0 {
				t.Fatalf("partial result = %#v", got)
			}
			requireTrackerError(t, err, tracker.CategoryPayload)
		})
	}
}

func TestStatePaginationRejectsRepeatedCursorWithoutPartialOutput(t *testing.T) {
	// Break caught: accepting a repeated cursor loops until resource exhaustion
	// and may duplicate issues in the visible queue.
	server := linearFixtureServer(t,
		fixtureResponse{Body: graphQLPage([]map[string]any{fixtureIssue("LIN-1", "Todo", nil)}, true, "cursor-a")},
		fixtureResponse{Body: graphQLPage([]map[string]any{fixtureIssue("LIN-2", "Todo", nil)}, true, "cursor-a")},
	)
	got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
	if got == nil || len(got) != 0 {
		t.Fatalf("partial result = %#v", got)
	}
	requireTrackerError(t, err, tracker.CategoryPagination)
}

func TestStatePaginationCapsOneHundredPages(t *testing.T) {
	// Break caught: internal complexity-safe requests must not accidentally
	// halve the documented 100 logical pages or permit logical page 101.
	responses := make([]fixtureResponse, 200)
	for request := 1; request <= 200; request++ {
		responses[request-1] = fixtureResponse{Body: graphQLPage(nil, true, fmt.Sprintf("cursor-%d", request))}
	}
	server := linearFixtureServer(t, responses...)
	got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
	if got == nil || len(got) != 0 {
		t.Fatalf("partial result = %#v", got)
	}
	requireTrackerError(t, err, tracker.CategoryPagination)
}

func TestEveryLinearGraphQLRequestStaysBelowComplexityCeiling(t *testing.T) {
	// Break caught: restoring a root first=50 makes both fixed query documents
	// exceed Linear's hard 10,000-point per-query complexity limit.
	t.Run("state logical page", func(t *testing.T) {
		server := linearFixtureServer(t,
			fixtureResponse{Body: graphQLPage(nil, true, "cursor-40")},
			fixtureResponse{Body: graphQLPage(nil, false, nil)},
		)
		got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("result = %#v, %v", got, err)
		}
		assertComplexitySafeRequests(t, server.Requests(), SymphonyIssuesByStates, []int{40, 10})
	})

	t.Run("ID logical batch", func(t *testing.T) {
		ids := make([]string, 50)
		for index := range ids {
			ids[index] = "issue-" + strconv.Itoa(index+1)
		}
		server := linearFixtureServer(t,
			fixtureResponse{Body: graphQLPage(nil, false, nil)},
			fixtureResponse{Body: graphQLPage(nil, false, nil)},
		)
		got, err := newLinearAdapter(t, server).FetchIssuesByIDs(context.Background(), ids)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("result = %#v, %v", got, err)
		}
		assertComplexitySafeRequests(t, server.Requests(), SymphonyIssuesByIDs, []int{40, 10})
	})
}

func TestStatePaginationLateFailureReturnsNoEarlierPages(t *testing.T) {
	// Break caught: state polling is one logical atomic read; page one must not
	// reach the scheduler when page two fails.
	server := linearFixtureServer(t,
		fixtureResponse{Body: graphQLPage([]map[string]any{fixtureIssue("LIN-1", "Todo", nil)}, true, "cursor-a")},
		fixtureResponse{Status: 502, Body: "provider detail"},
	)
	got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
	if got == nil || len(got) != 0 {
		t.Fatalf("partial result = %#v", got)
	}
	requireTrackerError(t, err, tracker.CategoryResponse)
}

func TestStatePaginationPreservesProviderPageOrder(t *testing.T) {
	// Break caught: sorting or set-based accumulation inside the adapter changes
	// provider page order before orchestration applies its own stable sort.
	server := linearFixtureServer(t,
		fixtureResponse{Body: graphQLPage([]map[string]any{fixtureIssue("LIN-3", "Todo", nil), fixtureIssue("LIN-1", "Todo", nil)}, true, "cursor-a")},
		fixtureResponse{Body: graphQLPage([]map[string]any{fixtureIssue("LIN-2", "Todo", nil)}, false, nil)},
	)
	got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatal(err)
	}
	assertIdentifiers(t, got, []string{"LIN-3", "LIN-1", "LIN-2"})
}

func TestStatePaginationRejectsDuplicateDispatchIdentityOrIdentifier(t *testing.T) {
	// Break caught: duplicate provider identities make issue maps/routes
	// ambiguous and cannot be repaired by silently keeping one page's record.
	for _, test := range []struct {
		name  string
		nodes []map[string]any
	}{
		{name: "duplicate ID", nodes: []map[string]any{fixtureIssue("LIN-1", "Todo", nil), fixtureIssue("LIN-1", "Todo", nil)}},
		{name: "duplicate identifier", nodes: func() []map[string]any {
			first := fixtureIssue("LIN-1", "Todo", nil)
			second := fixtureIssue("LIN-1", "Todo", nil)
			second["id"] = "issue-2"
			return []map[string]any{first, second}
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := linearFixtureServer(t, fixtureResponse{Body: graphQLPage(test.nodes, false, nil)})
			got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
			if got == nil || len(got) != 0 {
				t.Fatalf("partial result = %#v", got)
			}
			requireTrackerError(t, err, tracker.CategoryPayload)
		})
	}
}

func TestQueryDocumentsLockOperationFiltersArgumentsAndNormalizedSelections(t *testing.T) {
	// Break caught: these fixed documents are the adapter's provider contract;
	// omitting any selected field makes normalization silently incomplete.
	sharedSelections := []string{
		"id", "identifier", "title", "description", "priority", "state {", "name",
		"branchName", "url", "assignee {", "labels(first: 50)",
		"inverseRelations(first: $relationFirst)", "type", "issue {", "project {", "slugId",
		"team {", "createdAt", "updatedAt", "pageInfo {", "hasNextPage", "endCursor",
	}
	for name, document := range map[string]string{
		"states": SymphonyIssuesByStates,
		"ids":    SymphonyIssuesByIDs,
	} {
		if document == "" {
			t.Fatalf("%s document is blank", name)
		}
		for _, selection := range sharedSelections {
			if !strings.Contains(document, selection) {
				t.Fatalf("%s document missing %q:\n%s", name, selection, document)
			}
		}
	}
	for _, want := range []string{
		"query SymphonyIssuesByStates", "$projectSlug: String!", "$stateNames: [String!]!",
		"$first: Int!", "$relationFirst: Int!", "$after: String",
		"project: { slugId: { eq: $projectSlug } }", "state: { name: { in: $stateNames } }",
		"first: $first", "after: $after", "orderBy: createdAt",
	} {
		if !strings.Contains(SymphonyIssuesByStates, want) {
			t.Fatalf("state document missing %q", want)
		}
	}
	for _, want := range []string{
		"query SymphonyIssuesByIDs", "$ids: [ID!]!", "$projectSlug: String!",
		"id: { in: $ids }", "project: { slugId: { eq: $projectSlug } }", "first: $first",
	} {
		if !strings.Contains(SymphonyIssuesByIDs, want) {
			t.Fatalf("ID document missing %q", want)
		}
	}
}

func TestNestedQueryConnectionsSelectTruncationMetadata(t *testing.T) {
	// Break caught: without nested hasNextPage the adapter cannot fail closed on
	// incomplete labels or blocker relations.
	for name, document := range map[string]string{"states": SymphonyIssuesByStates, "ids": SymphonyIssuesByIDs} {
		if strings.Count(document, "hasNextPage") < 3 {
			t.Fatalf("%s document does not select all three hasNextPage values", name)
		}
		if !strings.Contains(document, "labels(first: 50)") {
			t.Fatalf("%s labels are not capped at 50", name)
		}
	}
}

func assertComplexitySafeRequests(t *testing.T, requests []recordedRequest, query string, wantFirst []int) {
	t.Helper()
	if len(requests) != len(wantFirst) {
		t.Fatalf("requests = %d, want %d", len(requests), len(wantFirst))
	}
	for index, request := range requests {
		if request.Query != query {
			t.Fatalf("request %d used unexpected query", index)
		}
		first := testJSONInt(t, request.Variables["first"])
		relationFirst := testJSONInt(t, request.Variables["relationFirst"])
		if complexity := documentedLinearIssueQueryComplexity(first, relationFirst); complexity >= 10_000 {
			t.Fatalf("request %d complexity = %d, must be below 10000", index, complexity)
		}
		if first != wantFirst[index] {
			t.Fatalf("request %d first = %d, want %d", index, first, wantFirst[index])
		}
	}
}

func testJSONInt(t *testing.T, value any) int {
	t.Helper()
	number, ok := value.(json.Number)
	if !ok {
		t.Fatalf("number = %#v (%T)", value, value)
	}
	integer, err := strconv.Atoi(number.String())
	if err != nil {
		t.Fatalf("number = %q: %v", number, err)
	}
	return integer
}

// documentedLinearIssueQueryComplexity applies Linear's published weights to
// the selections locked above. Values are kept in tenths until final rounding:
// properties cost 0.1, objects cost 1, and connections multiply node cost by
// their requested first value.
func documentedLinearIssueQueryComplexity(first, relationFirst int) int {
	const (
		labelsFirst             = 50
		outerPageInfoTenths     = 12
		issueConnectionTenths   = 10
		directIssueFieldsTenths = 54
		labelConnectionNode     = 11
		relationConnectionNode  = 34
		nestedPageInfoTenths    = 11
	)
	perIssueTenths := issueConnectionTenths + directIssueFieldsTenths +
		labelsFirst*labelConnectionNode + nestedPageInfoTenths +
		relationFirst*relationConnectionNode + nestedPageInfoTenths
	totalTenths := outerPageInfoTenths + first*perIssueTenths
	return (totalTenths + 9) / 10
}
