package linear

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/tracker"
)

var _ tracker.Adapter = (*Adapter)(nil)

func TestAdapterCloseRetiresCapturedCredentialBytes(t *testing.T) {
	adapter := &Adapter{token: []byte("credential-canary")}
	captured := adapter.token
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
	if len(adapter.token) != 0 {
		t.Fatalf("retired token length = %d", len(adapter.token))
	}
	for _, value := range captured {
		if value != 0 {
			t.Fatal("retired Linear credential bytes were retained")
		}
	}
}

func TestAdapterImplementsLiveTrackerAndToolContract(t *testing.T) {
	// Break caught: the live adapter must retain issue reads while exposing only
	// the captured Linear GraphQL tool at the shared boundary.
	server := linearFixtureServer(t)
	adapter := newLinearAdapter(t, server)
	if adapter.Kind() != "linear" {
		t.Fatalf("kind = %q", adapter.Kind())
	}
	states, err := adapter.FetchIssuesByStates(context.Background(), nil)
	if err != nil || states == nil || len(states) != 0 {
		t.Fatalf("empty states = %#v, %v", states, err)
	}
	ids, err := adapter.FetchIssuesByIDs(context.Background(), nil)
	if err != nil || ids == nil || len(ids) != 0 {
		t.Fatalf("empty IDs = %#v, %v", ids, err)
	}
	if tools := adapter.AgentTools(tracker.Session{}); len(tools) != 1 || tools[0].Name != linearGraphQLToolName {
		t.Fatalf("tools = %#v, want linear_graphql", tools)
	}
	result := adapter.ExecuteAgentTool(context.Background(), domain.ToolCall{Name: "future_tool"}, tracker.Session{})
	if result.Success || result.Error == nil || result.Error.Code != domain.ToolUnavailableCode {
		t.Fatalf("tool result = %#v", result)
	}
}

func TestFetchIssuesByStatesFollowsCursorAndLocksEveryScopeVariable(t *testing.T) {
	// Break caught: omitting a cursor or scope/state variable can return only the
	// first page or mix another Linear project into the candidate set.
	server := linearScopedFixtureServer(t,
		fixtureResponse{File: "candidates-page-1.json"},
		fixtureResponse{File: "candidates-page-2.json"},
	)
	got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{" Todo ", "todo", "In Progress", " "})
	if err != nil {
		t.Fatal(err)
	}
	assertIdentifiers(t, got, []string{"LIN-12", "LIN-13"})
	if !got[0].Dispatchable || !got[1].Dispatchable {
		t.Fatalf("valid page results lost dispatchability: %#v", got)
	}

	requests := issueRequests(server.Requests())
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	wantFirst := []string{"40", "10"}
	for index, request := range requests {
		if request.Method != http.MethodPost {
			t.Fatalf("request %d method = %s", index, request.Method)
		}
		if request.Query != SymphonyIssuesByStates {
			t.Fatalf("request %d used unexpected document", index)
		}
		keys := append([]string(nil), request.RawKeys...)
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, []string{"query", "variables"}) {
			t.Fatalf("request keys = %#v", keys)
		}
		assertJSONNumber(t, request.Variables["first"], wantFirst[index])
		assertJSONNumber(t, request.Variables["relationFirst"], "50")
		if request.Variables["projectSlug"] != "symphony" {
			t.Fatalf("request %d projectSlug = %#v", index, request.Variables["projectSlug"])
		}
		if !reflect.DeepEqual(request.Variables["stateNames"], []any{"Todo", "In Progress"}) {
			t.Fatalf("request %d stateNames = %#v", index, request.Variables["stateNames"])
		}
	}
	if requests[0].Variables["after"] != nil || requests[1].Variables["after"] != "cursor-1" {
		t.Fatalf("after cursors = %#v, %#v", requests[0].Variables["after"], requests[1].Variables["after"])
	}
}

func TestFetchIssuesByStatesSupportsArbitraryRequestedProviderStates(t *testing.T) {
	// Break caught: hardcoding the default active states prevents the same read
	// operation from serving terminal startup cleanup and authored workflows.
	node := fixtureIssue("LIN-40", "Archived", nil)
	server := linearScopedFixtureServer(t, fixtureResponse{Body: graphQLPage([]map[string]any{node}, false, nil)})
	got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{" Archived "})
	if err != nil {
		t.Fatal(err)
	}
	assertIdentifiers(t, got, []string{"LIN-40"})
	if values := issueRequests(server.Requests())[0].Variables["stateNames"]; !reflect.DeepEqual(values, []any{"Archived"}) {
		t.Fatalf("stateNames = %#v", values)
	}
}

func TestFetchIssuesByStatesBlankOnlyInputMakesNoRequest(t *testing.T) {
	// Break caught: a blank filter can accidentally broaden the provider query
	// from one requested state set to every issue in the project.
	server := linearFixtureServer(t)
	got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{" ", "\t"})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("result = %#v, %v", got, err)
	}
}

func TestFetchIssuesByStatesOmitsMalformedRecordsWithStaticSafeWarning(t *testing.T) {
	// Break caught: returning or logging a malformed raw record can dispatch an
	// unsafe issue or leak provider/token content before the redactor exists.
	valid := fixtureIssue("LIN-12", "Todo", nil)
	malformed := fixtureIssue("LIN-99", "Todo", nil)
	malformed["title"] = " "
	malformed["description"] = tokenCanary
	buffer := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(buffer, nil))
	server := linearScopedFixtureServer(t, fixtureResponse{Body: graphQLPage([]map[string]any{valid, malformed}, false, nil)})
	adapter := mustNewLinearAdapter(t, defaultLinearConfig(server.URL()), server.Client(), logger)
	got, err := adapter.FetchIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatal(err)
	}
	assertIdentifiers(t, got, []string{"LIN-12"})
	logged := buffer.String()
	for _, want := range []string{
		"linear_issue_omitted", "operation=fetch_issues_by_states", "page=1", "index=1", "reason=malformed_required_record",
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log missing %q: %s", want, logged)
		}
	}
	for _, forbidden := range []string{tokenCanary, "LIN-99", "description", "Issue LIN-99"} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("unsafe %q in log: %s", forbidden, logged)
		}
	}
}

func TestFetchIssuesByStatesOmitsOutOfScopeUnexpectedStateAndTruncatedLabels(t *testing.T) {
	// Break caught: trusting provider filters or incomplete labels can publish a
	// record outside the logical request or required-label boundary.
	valid := fixtureIssue("LIN-12", "Todo", nil)
	outOfScope := fixtureIssue("LIN-13", "Todo", nil)
	outOfScope["project"].(map[string]any)["slugId"] = "other"
	wrongState := fixtureIssue("LIN-14", "Done", nil)
	truncated := fixtureIssue("LIN-15", "Todo", nil)
	truncated["labels"].(map[string]any)["pageInfo"] = map[string]any{"hasNextPage": true}
	server := linearScopedFixtureServer(t, fixtureResponse{Body: graphQLPage([]map[string]any{valid, outOfScope, wrongState, truncated}, false, nil)})
	got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatal(err)
	}
	assertIdentifiers(t, got, []string{"LIN-12"})
}

func TestFetchIssuesByIDsBatchesFiftyDeduplicatesAndPreservesFirstSeenInputs(t *testing.T) {
	// Break caught: changing the logical 50-ID contract while splitting provider
	// requests can lose, reorder, or duplicate active issue refreshes.
	ids := make([]string, 0, 53)
	logicalNodes := make([]map[string]any, 0, 50)
	for number := 1; number <= 50; number++ {
		identifier := "LIN-" + jsonNumber(number)
		ids = append(ids, "issue-"+jsonNumber(number))
		logicalNodes = append(logicalNodes, fixtureIssue(identifier, "In Progress", nil))
	}
	ids = append(ids, "issue-1", "issue-51")
	lastNode := fixtureIssue("LIN-51", "Done", nil)
	server := linearScopedFixtureServer(t,
		fixtureResponse{Body: graphQLPage(logicalNodes[:40], false, nil)},
		fixtureResponse{Body: graphQLPage(logicalNodes[40:], false, nil)},
		fixtureResponse{Body: graphQLPage([]map[string]any{lastNode}, false, nil)},
	)
	got, err := newLinearAdapter(t, server).FetchIssuesByIDs(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 51 {
		t.Fatalf("issues = %d, want 51", len(got))
	}
	requests := issueRequests(server.Requests())
	if len(requests) != 3 {
		t.Fatalf("requests = %d", len(requests))
	}
	for index, request := range requests {
		if request.Query != SymphonyIssuesByIDs {
			t.Fatalf("batch %d used unexpected query", index)
		}
		if request.Variables["projectSlug"] != "symphony" {
			t.Fatalf("batch %d scope = %#v", index, request.Variables["projectSlug"])
		}
		assertJSONNumber(t, request.Variables["relationFirst"], "50")
	}
	assertJSONNumber(t, requests[0].Variables["first"], "40")
	assertJSONNumber(t, requests[1].Variables["first"], "10")
	assertJSONNumber(t, requests[2].Variables["first"], "1")
	firstIDs := requests[0].Variables["ids"].([]any)
	if len(firstIDs) != 40 || firstIDs[0] != "issue-1" || firstIDs[39] != "issue-40" {
		t.Fatalf("first batch IDs = %#v", firstIDs)
	}
	secondIDs := requests[1].Variables["ids"].([]any)
	if len(secondIDs) != 10 || secondIDs[0] != "issue-41" || secondIDs[9] != "issue-50" {
		t.Fatalf("second batch IDs = %#v", requests[1].Variables["ids"])
	}
	if !reflect.DeepEqual(requests[2].Variables["ids"], []any{"issue-51"}) {
		t.Fatalf("third batch IDs = %#v", requests[2].Variables["ids"])
	}
	for _, boundary := range []struct {
		index int
		want  string
	}{
		{index: 0, want: "LIN-1"},
		{index: 39, want: "LIN-40"},
		{index: 40, want: "LIN-41"},
		{index: 49, want: "LIN-50"},
		{index: 50, want: "LIN-51"},
	} {
		if got[boundary.index].Identifier != boundary.want {
			t.Fatalf("issue %d = %q, want %q", boundary.index, got[boundary.index].Identifier, boundary.want)
		}
	}
}

func TestFetchIssuesByIDsRejectsIssueFromDifferentInternalRequest(t *testing.T) {
	// Break caught: accepting a response ID from another subrequest weakens the
	// exact ID filter after a logical 50-ID batch is split for complexity.
	ids := make([]string, 50)
	for index := range ids {
		ids[index] = "issue-" + jsonNumber(index+1)
	}
	server := linearScopedFixtureServer(t, fixtureResponse{Body: graphQLPage([]map[string]any{
		fixtureIssue("LIN-41", "In Progress", nil),
	}, false, nil)})
	got, err := newLinearAdapter(t, server).FetchIssuesByIDs(context.Background(), ids)
	if got == nil || len(got) != 0 {
		t.Fatalf("partial result = %#v", got)
	}
	requireTrackerError(t, err, tracker.CategoryPayload)
}

func TestFetchIssuesByIDsOmitsMissingAndOutOfProjectRequestedIDs(t *testing.T) {
	// Break caught: fabricating missing records or accepting an out-of-project
	// match changes reconciliation meaning and crosses configured scope.
	outOfProject := fixtureIssue("LIN-2", "In Progress", nil)
	outOfProject["project"].(map[string]any)["slugId"] = "other"
	server := linearScopedFixtureServer(t, fixtureResponse{Body: graphQLPage([]map[string]any{outOfProject}, false, nil)})
	got, err := newLinearAdapter(t, server).FetchIssuesByIDs(context.Background(), []string{"issue-1", "issue-2"})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("issues = %#v", got)
	}
}

func TestFetchIssuesByIDsRejectsUnexpectedDuplicateAndIdentifierCollision(t *testing.T) {
	// Break caught: a response that cannot map one-to-one onto requested opaque
	// IDs is not a complete atomic reconciliation snapshot.
	for _, test := range []struct {
		name  string
		ids   []string
		nodes []map[string]any
	}{
		{name: "unexpected ID", ids: []string{"issue-1"}, nodes: []map[string]any{fixtureIssue("LIN-2", "Done", nil)}},
		{name: "duplicate ID", ids: []string{"issue-1"}, nodes: []map[string]any{fixtureIssue("LIN-1", "Done", nil), fixtureIssue("LIN-1", "Done", nil)}},
		{name: "duplicate identifier", ids: []string{"issue-1", "issue-2"}, nodes: func() []map[string]any {
			first := fixtureIssue("LIN-1", "Done", nil)
			second := fixtureIssue("LIN-1", "Done", nil)
			second["id"] = "issue-2"
			return []map[string]any{first, second}
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := linearScopedFixtureServer(t, fixtureResponse{Body: graphQLPage(test.nodes, false, nil)})
			got, err := newLinearAdapter(t, server).FetchIssuesByIDs(context.Background(), test.ids)
			if got == nil || len(got) != 0 {
				t.Fatalf("partial result = %#v", got)
			}
			requireTrackerError(t, err, tracker.CategoryPayload)
		})
	}
}

func TestFetchIssuesByIDsRejectsBlankOpaqueIDWithoutRequest(t *testing.T) {
	// Break caught: trimming or sending a blank dispatch ID changes its opaque
	// identity and can accidentally broaden a provider filter.
	server := linearFixtureServer(t)
	got, err := newLinearAdapter(t, server).FetchIssuesByIDs(context.Background(), []string{"issue-1", " "})
	if got == nil || len(got) != 0 {
		t.Fatalf("partial result = %#v", got)
	}
	requireTrackerError(t, err, tracker.CategoryConfig)
}

func TestFetchIssuesByIDsFailsAtomicallyOnLateMalformedBatch(t *testing.T) {
	// Break caught: publishing the first successful batch after a later batch
	// fails makes missing active issues look invisible to reconciliation.
	logicalNodes := make([]map[string]any, 50)
	ids := make([]string, 51)
	for number := 1; number <= 50; number++ {
		logicalNodes[number-1] = fixtureIssue("LIN-"+jsonNumber(number), "In Progress", nil)
		ids[number-1] = "issue-" + jsonNumber(number)
	}
	ids[50] = "issue-51"
	bad := fixtureIssue("LIN-51", "In Progress", nil)
	bad["title"] = nil
	server := linearScopedFixtureServer(t,
		fixtureResponse{Body: graphQLPage(logicalNodes[:40], false, nil)},
		fixtureResponse{Body: graphQLPage(logicalNodes[40:], false, nil)},
		fixtureResponse{Body: graphQLPage([]map[string]any{bad}, false, nil)},
	)
	got, err := newLinearAdapter(t, server).FetchIssuesByIDs(context.Background(), ids)
	if got == nil || len(got) != 0 {
		t.Fatalf("partial result = %#v", got)
	}
	requireTrackerError(t, err, tracker.CategoryPayload)
}

func TestFetchIssuesByIDsRejectsBatchThatClaimsAnotherPage(t *testing.T) {
	// Break caught: silently ignoring hasNextPage loses requested refresh rows
	// while reporting a complete ID batch.
	node := fixtureIssue("LIN-1", "In Progress", nil)
	server := linearScopedFixtureServer(t, fixtureResponse{Body: graphQLPage([]map[string]any{node}, true, "next")})
	got, err := newLinearAdapter(t, server).FetchIssuesByIDs(context.Background(), []string{"issue-1"})
	if got == nil || len(got) != 0 {
		t.Fatalf("partial result = %#v", got)
	}
	requireTrackerError(t, err, tracker.CategoryPagination)
}

func TestSecretEnvironmentNamesReturnsOwnedNonNilCopy(t *testing.T) {
	// Break caught: an aliased secret-name slice lets a caller mutate the child
	// environment stripping contract captured by the adapter.
	server := linearFixtureServer(t)
	config := defaultLinearConfig(server.URL())
	config.CredentialEnv = "SYMPHONY_LINEAR_TOKEN"
	adapter := mustNewLinearAdapter(t, config, server.Client(), nil)
	first := adapter.SecretEnvironmentNames()
	if !reflect.DeepEqual(first, []string{"LINEAR_API_KEY", "SYMPHONY_LINEAR_TOKEN"}) {
		t.Fatalf("secret names = %#v", first)
	}
	first[0] = "changed"
	if second := adapter.SecretEnvironmentNames(); second == nil || second[0] != "LINEAR_API_KEY" {
		t.Fatalf("secret names aliased caller: %#v", second)
	}
}

func TestAdapterConcurrentReadsAreRaceSafe(t *testing.T) {
	// Break caught: request-local pagination and normalization state must not be
	// retained on the adapter or shared across concurrent scheduler reads.
	const readers = 24
	responseBody := graphQLPage([]map[string]any{fixtureIssue("LIN-1", "Todo", nil)}, false, nil)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := responseBody
		var operation graphQLRequest
		if err := json.NewDecoder(request.Body).Decode(&operation); err != nil {
			return nil, err
		}
		if operation.Query == SymphonyProjectScope {
			body = projectScopeBody([]projectScopeFixture{{ID: "project-1", Slug: "symphony"}}, false)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	adapter := mustNewLinearAdapter(t, defaultLinearConfig("https://linear.example/graphql"), client, nil)

	var wait sync.WaitGroup
	failures := make(chan error, readers)
	for range readers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"Todo"})
			if err != nil {
				failures <- err
				return
			}
			if len(issues) != 1 || issues[0].Identifier != "LIN-1" {
				failures <- errors.New("concurrent read returned the wrong issue snapshot")
			}
		}()
	}
	wait.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
}

func assertJSONNumber(t *testing.T, value any, want string) {
	t.Helper()
	number, ok := value.(json.Number)
	if !ok || number.String() != want {
		t.Fatalf("number = %#v (%T), want json.Number(%s)", value, value, want)
	}
}

func jsonNumber(number int) string {
	return strconv.Itoa(number)
}
