package linear

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
)

func TestNormalizeIssuePreservesAllProviderFieldsAndExactNativeRef(t *testing.T) {
	// Break caught: dropping a selected Linear field or widening native_ref loses
	// scheduler/tool context even though the GraphQL record itself was valid.
	node := fixtureIssue("LIN-12", " Todo ", []fixtureBlocker{{ID: "blocker-1", Identifier: "LIN-2", State: " done "}})
	node["description"] = "  keep provider spacing  "
	node["priority"] = -7
	node["branchName"] = "cory/lin-12"
	node["url"] = "https://linear.app/example/issue/LIN-12"
	node["assignee"] = map[string]any{"id": "user-12"}

	got, err := normalizeFixture(node, "Done", "Closed")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "issue-12" || got.Identifier != "LIN-12" || got.Title != "Issue LIN-12" || got.State != " Todo " {
		t.Fatalf("required/display fields = %#v", got)
	}
	if got.Description == nil || *got.Description != "  keep provider spacing  " || got.Priority == nil || *got.Priority != -7 {
		t.Fatalf("description/priority = %#v %#v", got.Description, got.Priority)
	}
	if got.BranchName == nil || *got.BranchName != "cory/lin-12" || got.URL == nil || *got.URL != "https://linear.app/example/issue/LIN-12" || got.AssigneeID == nil || *got.AssigneeID != "user-12" {
		t.Fatalf("optional provider fields = %#v", got)
	}
	if !reflect.DeepEqual(got.Labels, []string{"symphony", "bug"}) {
		t.Fatalf("labels = %#v", got.Labels)
	}
	if len(got.BlockedBy) != 1 || got.BlockedBy[0].ID == nil || *got.BlockedBy[0].ID != "blocker-1" || got.BlockedBy[0].Identifier == nil || *got.BlockedBy[0].Identifier != "LIN-2" || got.BlockedBy[0].State == nil || *got.BlockedBy[0].State != " done " {
		t.Fatalf("blocked_by = %#v", got.BlockedBy)
	}
	wantNative := map[string]any{
		"issue_id": "issue-12", "identifier": "LIN-12", "project_id": "project-1",
		"project_slug": "symphony", "team_id": "team-1",
	}
	if !reflect.DeepEqual(got.NativeRef, wantNative) {
		t.Fatalf("native_ref = %#v, want %#v", got.NativeRef, wantNative)
	}
	if !got.Dispatchable {
		t.Fatal("Todo with a complete terminal blocker set was not dispatchable")
	}
	if got.CreatedAt == nil || got.CreatedAt.Location() != time.UTC || !got.CreatedAt.Equal(time.Date(2026, 8, 8, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("created_at = %#v", got.CreatedAt)
	}
	if got.UpdatedAt == nil || got.UpdatedAt.Location() != time.UTC {
		t.Fatalf("updated_at = %#v", got.UpdatedAt)
	}
}

func TestNormalizeIssueFallsBackFromInvalidNullableProviderValues(t *testing.T) {
	// Break caught: treating optional parsing failures as whole-record failures
	// hides otherwise valid scoped work from polling and reconciliation.
	for _, test := range []struct {
		name   string
		field  string
		value  any
		assert func(*testing.T, domain.Issue)
	}{
		{name: "null priority", field: "priority", value: nil, assert: func(t *testing.T, got domain.Issue) {
			if got.Priority != nil {
				t.Fatalf("priority = %#v", got.Priority)
			}
		}},
		{name: "fractional priority", field: "priority", value: 2.5, assert: func(t *testing.T, got domain.Issue) {
			if got.Priority != nil {
				t.Fatalf("priority = %#v", got.Priority)
			}
		}},
		{name: "string priority", field: "priority", value: "2", assert: func(t *testing.T, got domain.Issue) {
			if got.Priority != nil {
				t.Fatalf("priority = %#v", got.Priority)
			}
		}},
		{name: "invalid created timestamp", field: "createdAt", value: "not-a-time", assert: func(t *testing.T, got domain.Issue) {
			if got.CreatedAt != nil {
				t.Fatalf("created_at = %#v", got.CreatedAt)
			}
		}},
		{name: "invalid updated timestamp type", field: "updatedAt", value: 7, assert: func(t *testing.T, got domain.Issue) {
			if got.UpdatedAt != nil {
				t.Fatalf("updated_at = %#v", got.UpdatedAt)
			}
		}},
		{name: "credential URL", field: "url", value: "https://user:secret@linear.app/issue/LIN-12", assert: func(t *testing.T, got domain.Issue) {
			if got.URL != nil {
				t.Fatalf("url = %#v", got.URL)
			}
		}},
		{name: "relative URL", field: "url", value: "/issue/LIN-12", assert: func(t *testing.T, got domain.Issue) {
			if got.URL != nil {
				t.Fatalf("url = %#v", got.URL)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := fixtureIssue("LIN-12", "In Progress", nil)
			node[test.field] = test.value
			got, err := normalizeFixture(node, "Done")
			if err != nil {
				t.Fatal(err)
			}
			test.assert(t, got)
		})
	}
}

func TestNormalizeIssueKeepsAnyIntegralPriority(t *testing.T) {
	// Break caught: clamping Linear priority to the scheduler's 1..4 preferred
	// bucket destroys a provider value the shared sort contract deliberately keeps.
	for _, want := range []int{-100, 0, 1, 4, 5, 999} {
		node := fixtureIssue("LIN-12", "In Progress", nil)
		node["priority"] = want
		got, err := normalizeFixture(node, "Done")
		if err != nil {
			t.Fatal(err)
		}
		if got.Priority == nil || *got.Priority != want {
			t.Fatalf("priority = %#v, want %d", got.Priority, want)
		}
	}
}

func TestNormalizeIssueFiltersInverseBlocksAndPreservesNullablePieces(t *testing.T) {
	// Break caught: treating every inverse relation as a blocker, or collapsing
	// unknown blocker fields to empty strings, invents provider semantics.
	node := fixtureIssue("LIN-12", "Todo", nil)
	node["inverseRelations"] = map[string]any{
		"nodes": []any{
			map[string]any{"type": " relates ", "issue": map[string]any{"id": "ignore", "identifier": "LIN-9", "state": map[string]any{"name": "In Progress"}}},
			map[string]any{"type": " Blocks ", "issue": map[string]any{"id": "blocker-id", "identifier": nil, "state": nil}},
			map[string]any{"type": "blocks", "issue": map[string]any{"id": nil, "identifier": "LIN-3", "state": map[string]any{"name": "Done"}}},
			map[string]any{"type": "blocks", "issue": map[string]any{"id": " ", "identifier": " ", "state": map[string]any{"name": " "}}},
		},
		"pageInfo": map[string]any{"hasNextPage": false},
	}
	got, err := normalizeFixture(node, "Done")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.BlockedBy) != 2 {
		t.Fatalf("blocked_by = %#v, want two usable blocks refs", got.BlockedBy)
	}
	if got.BlockedBy[0].ID == nil || *got.BlockedBy[0].ID != "blocker-id" || got.BlockedBy[0].Identifier != nil || got.BlockedBy[0].State != nil {
		t.Fatalf("first nullable blocker = %#v", got.BlockedBy[0])
	}
	if got.BlockedBy[1].ID != nil || got.BlockedBy[1].Identifier == nil || *got.BlockedBy[1].Identifier != "LIN-3" || got.BlockedBy[1].State == nil || *got.BlockedBy[1].State != "Done" {
		t.Fatalf("second nullable blocker = %#v", got.BlockedBy[1])
	}
	if got.Dispatchable {
		t.Fatal("Todo with a retained blocker whose state is unknown was dispatchable")
	}
}

func TestTodoDispatchabilityFailsClosedOnIncompleteBlockerData(t *testing.T) {
	// Break caught: missing, malformed, truncated, unknown, or nonterminal
	// relation state must never make Todo work appear unblocked.
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
		want   bool
	}{
		{name: "complete empty", want: true},
		{name: "terminal blocker casing", mutate: func(node map[string]any) {
			node["inverseRelations"] = relationConnection(false, []fixtureBlocker{{ID: "b1", Identifier: "LIN-2", State: " dOnE "}})
		}, want: true},
		{name: "nonterminal blocker", mutate: func(node map[string]any) {
			node["inverseRelations"] = relationConnection(false, []fixtureBlocker{{ID: "b1", Identifier: "LIN-2", State: "In Progress"}})
		}},
		{name: "unknown blocker state", mutate: func(node map[string]any) {
			node["inverseRelations"] = relationConnection(false, []fixtureBlocker{{ID: "b1", Identifier: "LIN-2", State: nil}})
		}},
		{name: "missing relation data", mutate: func(node map[string]any) { delete(node, "inverseRelations") }},
		{name: "malformed relation data", mutate: func(node map[string]any) { node["inverseRelations"] = "bad" }},
		{name: "truncated relation data", mutate: func(node map[string]any) {
			node["inverseRelations"] = relationConnection(true, []fixtureBlocker{{ID: "b1", Identifier: "LIN-2", State: "Done"}})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := fixtureIssue("LIN-12", "Todo", nil)
			if test.mutate != nil {
				test.mutate(node)
			}
			got, err := normalizeFixture(node, "Done")
			if err != nil {
				t.Fatal(err)
			}
			if got.Dispatchable != test.want {
				t.Fatalf("dispatchable = %v, want %v; issue %#v", got.Dispatchable, test.want, got)
			}
		})
	}
}

func TestNonTodoStatesRemainProviderDispatchableWithIncompleteRelations(t *testing.T) {
	// Break caught: applying Todo's blocker-routing rule to every active or
	// terminal state makes valid reconciliation snapshots unroutable.
	for _, state := range []string{"In Progress", "Done", "Triage"} {
		node := fixtureIssue("LIN-12", state, nil)
		delete(node, "inverseRelations")
		got, err := normalizeFixture(node, "Done")
		if err != nil {
			t.Fatal(err)
		}
		if !got.Dispatchable {
			t.Fatalf("state %q was not provider-dispatchable", state)
		}
	}
}

func TestNormalizeIssueRejectsTruncatedLabels(t *testing.T) {
	// Break caught: dispatching from an incomplete label set can bypass the
	// scheduler's required-label semantics.
	node := fixtureIssue("LIN-12", "In Progress", nil)
	node["labels"].(map[string]any)["pageInfo"] = map[string]any{"hasNextPage": true}
	if _, err := normalizeFixture(node, "Done"); err == nil {
		t.Fatal("truncated labels were accepted")
	}
}

func TestNormalizeIssueRequiresScopedIdentityFields(t *testing.T) {
	// Break caught: a record without complete project/team identity cannot prove
	// scope or construct tool-safe native metadata.
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "id", mutate: func(node map[string]any) { node["id"] = " " }},
		{name: "identifier", mutate: func(node map[string]any) { node["identifier"] = nil }},
		{name: "title", mutate: func(node map[string]any) { node["title"] = 7 }},
		{name: "state", mutate: func(node map[string]any) { node["state"] = map[string]any{"name": ""} }},
		{name: "project id", mutate: func(node map[string]any) { node["project"].(map[string]any)["id"] = "" }},
		{name: "project slug", mutate: func(node map[string]any) { node["project"].(map[string]any)["slugId"] = nil }},
		{name: "team id", mutate: func(node map[string]any) { node["team"] = map[string]any{"id": " "} }},
		{name: "labels missing", mutate: func(node map[string]any) { delete(node, "labels") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := fixtureIssue("LIN-12", "In Progress", nil)
			test.mutate(node)
			if _, err := normalizeFixture(node, "Done"); err == nil {
				t.Fatalf("malformed %s was accepted", test.name)
			}
		})
	}
}

func TestNormalizeIssueRejectsProjectScopeMismatch(t *testing.T) {
	// Break caught: trusting GraphQL filtering alone can publish an issue from a
	// different Linear project when provider data or a fixture is inconsistent.
	node := fixtureIssue("LIN-12", "In Progress", nil)
	node["project"].(map[string]any)["slugId"] = "other-project"
	if _, err := normalizeFixture(node, "Done"); err == nil {
		t.Fatal("out-of-project record was accepted")
	}
}

func TestNormalizeIssueNeverInfersBlockersFromDescription(t *testing.T) {
	// Break caught: parsing unstructured text invents blocker semantics that the
	// provider relation model did not supply.
	node := fixtureIssue("LIN-12", "Todo", nil)
	node["description"] = "Blocked by LIN-99"
	got, err := normalizeFixture(node, "Done")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Dispatchable || len(got.BlockedBy) != 0 {
		t.Fatalf("description invented blockers: %#v", got)
	}
}

func normalizeFixture(node map[string]any, terminalStates ...string) (domain.Issue, error) {
	raw, err := json.Marshal(node)
	if err != nil {
		return domain.Issue{}, err
	}
	return normalizeIssueRecord(raw, "symphony", normalizedStateSet(terminalStates))
}

func relationConnection(hasNext bool, blockers []fixtureBlocker) map[string]any {
	node := fixtureIssue("LIN-X", "Todo", blockers)
	connection := node["inverseRelations"].(map[string]any)
	connection["pageInfo"] = map[string]any{"hasNextPage": hasNext}
	return connection
}
