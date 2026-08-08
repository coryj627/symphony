package linear

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/coryj627/symphony/go/internal/tracker"
)

func TestStatePaginationRejectsMissingCursorWithoutPartialOutput(t *testing.T) {
	// Break caught: hasNextPage without a usable cursor otherwise replays the
	// first page or silently reports an incomplete candidate list.
	server := linearFixtureServer(t, fixtureResponse{Body: graphQLPage([]map[string]any{fixtureIssue("LIN-1", "Todo", nil)}, true, nil)})
	got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
	if got == nil || len(got) != 0 {
		t.Fatalf("partial result = %#v", got)
	}
	requireTrackerError(t, err, tracker.CategoryPagination)
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
	// Break caught: a corrupt provider cursor chain must terminate finitely at
	// the documented page cap without issuing request 101.
	responses := make([]fixtureResponse, 100)
	for page := 1; page <= 100; page++ {
		responses[page-1] = fixtureResponse{Body: graphQLPage(nil, true, fmt.Sprintf("cursor-%d", page))}
	}
	server := linearFixtureServer(t, responses...)
	got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
	if got == nil || len(got) != 0 {
		t.Fatalf("partial result = %#v", got)
	}
	requireTrackerError(t, err, tracker.CategoryPagination)
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
