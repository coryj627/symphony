package domain

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestIssueValidateRequiredAllowsFalseDispatchableAndNilMetadata(t *testing.T) {
	// Break caught: treating false or absent optional metadata as missing makes
	// valid provider records fail before the scheduler can filter them.
	issue := Issue{
		ID:           "42",
		Identifier:   "GH-42",
		Title:        "Title",
		State:        "open",
		Dispatchable: false,
	}
	if err := issue.ValidateRequired(); err != nil {
		t.Fatal(err)
	}
}

func TestIssueValidateRequiredRejectsBlankProviderNeutralFields(t *testing.T) {
	// Break caught: accepting an empty dispatch identity, route identifier,
	// title, or display state creates an Issue the downstream contracts cannot
	// safely identify or present.
	valid := Issue{ID: "opaque-42", Identifier: "GH-42", Title: "Title", State: "Open"}
	for _, test := range []struct {
		name   string
		mutate func(*Issue)
	}{
		{name: "ID", mutate: func(issue *Issue) { issue.ID = "\t" }},
		{name: "identifier", mutate: func(issue *Issue) { issue.Identifier = " " }},
		{name: "title", mutate: func(issue *Issue) { issue.Title = "\n" }},
		{name: "state", mutate: func(issue *Issue) { issue.State = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := valid
			test.mutate(&issue)
			if err := issue.ValidateRequired(); !errors.Is(err, ErrInvalidIssue) {
				t.Fatalf("error = %v, want ErrInvalidIssue", err)
			}
		})
	}
}

func TestIssueValidateRequiredRejectsUnsafeNativeRefWithoutPanicking(t *testing.T) {
	// Break caught: recursively accepting a cycle, non-finite number, or Go-only
	// value can hang, panic, or leak a value that cannot cross the JSON boundary.
	cycle := map[string]any{}
	cycle["self"] = cycle
	for _, test := range []struct {
		name      string
		nativeRef map[string]any
	}{
		{name: "cycle", nativeRef: cycle},
		{name: "function", nativeRef: map[string]any{"value": func() {}}},
		{name: "non-finite number", nativeRef: map[string]any{"value": math.Inf(1)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := Issue{ID: "42", Identifier: "GH-42", Title: "Title", State: "open", NativeRef: test.nativeRef}
			if err := issue.ValidateRequired(); !errors.Is(err, ErrInvalidIssue) {
				t.Fatalf("error = %v, want ErrInvalidIssue", err)
			}
		})
	}
}

func TestIssueCloneDeepCopiesJSONDataCollectionsAndPointers(t *testing.T) {
	// Break caught: a shallow snapshot lets provider refreshes mutate an active
	// session, while JSON round-tripping corrupts integral provider IDs above
	// JavaScript's exact-integer range.
	description := "description"
	priority := 2
	blockerID := "blocker-1"
	blockerIdentifier := "LIN-1"
	blockerState := "In Progress"
	createdAt := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.FixedZone("provider", -4*60*60))
	wantCreatedAt := createdAt
	updatedAt := createdAt.Add(time.Minute)
	nested := map[string]any{"database_id": uint64(9007199254740993)}
	issue := Issue{
		ID: "opaque-42", NativeRef: map[string]any{"items": []any{nested}}, Identifier: "GH-42",
		Title: "Title", Description: &description, Priority: &priority, State: "Open",
		Labels: []string{"bug"}, BlockedBy: []BlockerRef{{ID: &blockerID, Identifier: &blockerIdentifier, State: &blockerState}},
		CreatedAt: &createdAt, UpdatedAt: &updatedAt,
	}

	clone, err := issue.Clone()
	if err != nil {
		t.Fatal(err)
	}

	nested["database_id"] = uint64(1)
	issue.Labels[0] = "changed"
	*issue.BlockedBy[0].Identifier = "changed"
	*issue.Description = "changed"
	*issue.Priority = 4
	*issue.CreatedAt = time.Time{}

	clonedNested := clone.NativeRef["items"].([]any)[0].(map[string]any)
	if got := clonedNested["database_id"]; got != uint64(9007199254740993) {
		t.Fatalf("database ID = %#v (%T), want lossless uint64", got, got)
	}
	if !reflect.DeepEqual(clone.Labels, []string{"bug"}) || *clone.BlockedBy[0].Identifier != "LIN-1" {
		t.Fatalf("collections aliased source: %#v", clone)
	}
	if *clone.Description != "description" || *clone.Priority != 2 || !clone.CreatedAt.Equal(wantCreatedAt) {
		t.Fatalf("optional pointers aliased source: %#v", clone)
	}
}

func TestIssueJSONIncludesNullNativeRefAndNullableBlockerFields(t *testing.T) {
	// Break caught: omitting native_ref or collapsing an unknown blocker field
	// to an empty string violates the normalized wire contract.
	identifier := "LIN-2"
	payload, err := json.Marshal(Issue{
		ID: "1", Identifier: "LIN-1", Title: "Title", State: "Todo",
		BlockedBy: []BlockerRef{{Identifier: &identifier}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if value, present := decoded["native_ref"]; !present || value != nil {
		t.Fatalf("native_ref = %#v, present = %v, want explicit null", value, present)
	}
	blockedBy := decoded["blocked_by"].([]any)
	blocker := blockedBy[0].(map[string]any)
	if blocker["id"] != nil || blocker["identifier"] != "LIN-2" || blocker["state"] != nil {
		t.Fatalf("blocker = %#v, want nullable fields preserved", blocker)
	}
}

func TestToolUnavailableResultIsStructuredAndStable(t *testing.T) {
	// Break caught: returning a Go error or an unstructured string leaves the
	// app-server unable to answer an unsupported dynamic tool call and continue.
	result := ToolUnavailableResult()
	if result.Success || result.Error == nil {
		t.Fatalf("result = %#v, want structured failure", result)
	}
	if result.Error.Code != ToolUnavailableCode || result.Error.Message == "" {
		t.Fatalf("tool error = %#v, want stable code and safe message", result.Error)
	}
	if ErrToolUnavailable.Error() != ToolUnavailableCode {
		t.Fatalf("sentinel = %q, want code %q", ErrToolUnavailable, ToolUnavailableCode)
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"success":false`) || !strings.Contains(string(payload), `"code":"tool_unavailable"`) {
		t.Fatalf("payload = %s, want structured JSON failure", payload)
	}
}

func TestToolContractsMarshalStructuredSuccessAndFailureValues(t *testing.T) {
	// Break caught: adding a Go error or opaque provider-specific payload field
	// makes the portable tool boundary impossible to translate to app-server
	// JSON without leaking implementation detail.
	values := []any{
		ToolSpec{
			Name: "github_api", Description: "Operate on the captured issue.",
			InputSchema: map[string]any{"type": "object", "required": []any{"operation"}},
		},
		ToolCall{Name: "linear_graphql", Arguments: "query { viewer { id } }"},
		ToolResult{Success: true, Data: map[string]any{"status": 200, "data": map[string]any{"id": "42"}}},
		ToolFailure("invalid_arguments", "The tool arguments are invalid."),
	}
	for _, value := range values {
		if _, err := json.Marshal(value); err != nil {
			t.Fatalf("%T did not marshal as structured JSON: %v", value, err)
		}
	}
	invalid := values[3].(ToolResult)
	if invalid.Success || invalid.Error == nil || invalid.Error.Code != "invalid_arguments" {
		t.Fatalf("invalid-argument failure = %#v", invalid)
	}
}

func TestToolContractsRejectUnsafeAndContradictoryValues(t *testing.T) {
	// Break caught: an adapter-provided function, cycle, or contradictory result
	// must be rejected before app-server translation rather than failing the
	// session's JSON response path.
	cycle := map[string]any{}
	cycle["self"] = cycle
	for _, test := range []struct {
		name     string
		validate func() error
	}{
		{
			name: "unsafe tool schema",
			validate: func() error {
				return (ToolSpec{Name: "tool", InputSchema: map[string]any{"bad": func() {}}}).Validate()
			},
		},
		{
			name: "cyclic arguments",
			validate: func() error {
				return (ToolCall{Name: "tool", Arguments: cycle}).Validate()
			},
		},
		{
			name: "successful result with error",
			validate: func() error {
				return (ToolResult{Success: true, Error: &ToolError{Code: "failed", Message: "failed"}}).Validate()
			},
		},
		{
			name: "failed result without structured error",
			validate: func() error {
				return (ToolResult{Success: false}).Validate()
			},
		},
		{
			name: "unsafe result data",
			validate: func() error {
				return (ToolResult{Success: true, Data: func() {}}).Validate()
			},
		},
		{
			name: "unsafe result errors",
			validate: func() error {
				return (ToolResult{Success: false, Errors: []any{func() {}}, Error: &ToolError{Code: "failed", Message: "failed"}}).Validate()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.validate(); !errors.Is(err, ErrInvalidToolContract) {
				t.Fatalf("error = %v, want ErrInvalidToolContract", err)
			}
		})
	}
}

func TestToolSuccessOwnsLosslessJSONSafeData(t *testing.T) {
	// Break caught: accepting a caller-owned result map lets later mutation
	// change the captured tool response, while JSON round-tripping loses exact
	// large provider IDs.
	nested := map[string]any{"id": uint64(9007199254740993)}
	result, err := ToolSuccess(map[string]any{"nested": nested})
	if err != nil {
		t.Fatal(err)
	}
	nested["id"] = uint64(1)
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	got := result.Data.(map[string]any)["nested"].(map[string]any)["id"]
	if got != uint64(9007199254740993) {
		t.Fatalf("result data = %#v (%T), want copied lossless uint64", got, got)
	}
}
