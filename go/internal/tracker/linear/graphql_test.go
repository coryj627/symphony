package linear

import "testing"

func TestLinearGraphQLAcceptsExactlyOneQueryOrMutation(t *testing.T) {
	for _, test := range []struct {
		name      string
		query     string
		operation linearOperation
	}{
		{name: "anonymous query", query: `{ viewer { id } }`, operation: linearQuery},
		{name: "named query", query: `query Viewer { viewer { id } }`, operation: linearQuery},
		{name: "mutation", query: `mutation Update { issueUpdate(id: "x", input: {title: "y"}) { success } }`, operation: linearMutation},
		{name: "fragment and one operation", query: `fragment Fields on User { id } query Viewer { viewer { ...Fields } }`, operation: linearQuery},
	} {
		t.Run(test.name, func(t *testing.T) {
			operation, code := parseLinearGraphQLDocument(test.query)
			if code != "" || operation != test.operation {
				t.Fatalf("operation=%q code=%q", operation, code)
			}
		})
	}
}

func TestLinearGraphQLRejectsInvalidDocumentsWithStableCodes(t *testing.T) {
	for _, test := range []struct {
		name, query, code string
	}{
		{name: "blank", query: " ", code: "invalid_graphql"},
		{name: "syntax", query: `query {`, code: "invalid_graphql"},
		{name: "fragment only", query: `fragment Fields on User { id }`, code: "invalid_operation_count"},
		{name: "two operations", query: `query A { viewer { id } } query B { viewer { id } }`, code: "invalid_operation_count"},
		{name: "subscription", query: `subscription Events { issueUpdated { id } }`, code: "unsupported_operation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, code := parseLinearGraphQLDocument(test.query)
			if code != test.code {
				t.Fatalf("code=%q want=%q", code, test.code)
			}
		})
	}
}
