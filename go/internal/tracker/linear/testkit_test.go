package linear

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/tracker"
)

const tokenCanary = "linear-token-canary-should-never-leak"

type fixtureResponse struct {
	File    string
	Body    string
	Status  int
	Headers http.Header
}

type recordedRequest struct {
	Method    string
	Header    http.Header
	Query     string
	Variables map[string]any
	RawKeys   []string
}

type fixtureServer struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	next    int
	planned []fixtureResponse
	seen    []recordedRequest
}

func linearFixtureServer(t *testing.T, responses ...fixtureResponse) *fixtureServer {
	t.Helper()
	fixture := &fixtureServer{t: t, planned: append([]fixtureResponse(nil), responses...)}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(fixture.serveHTTP))
	t.Cleanup(func() {
		fixture.server.Close()
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		if fixture.next != len(fixture.planned) {
			t.Errorf("server received %d requests, want %d", fixture.next, len(fixture.planned))
		}
	})
	return fixture
}

func (server *fixtureServer) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.next >= len(server.planned) {
		server.t.Errorf("unexpected request %s %s", request.Method, request.URL.String())
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	response := server.planned[server.next]
	server.next++

	var raw map[string]json.RawMessage
	decoder := json.NewDecoder(io.LimitReader(request.Body, 2<<20))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		server.t.Errorf("decode request: %v", err)
	}
	var query string
	if err := json.Unmarshal(raw["query"], &query); err != nil {
		server.t.Errorf("decode query: %v", err)
	}
	variables := map[string]any{}
	if err := decodeTestJSON(raw["variables"], &variables); err != nil {
		server.t.Errorf("decode variables: %v", err)
	}
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	server.seen = append(server.seen, recordedRequest{
		Method: request.Method, Header: request.Header.Clone(), Query: query,
		Variables: cloneTestMap(variables), RawKeys: keys,
	})

	for name, values := range response.Headers {
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	status := response.Status
	if status == 0 {
		status = http.StatusOK
	}
	writer.WriteHeader(status)
	body := response.Body
	if response.File != "" {
		body = fixtureBody(server.t, response.File)
	}
	_, _ = writer.Write([]byte(body))
}

func (server *fixtureServer) URL() string { return server.server.URL }

func (server *fixtureServer) Client() *http.Client { return server.server.Client() }

func (server *fixtureServer) Requests() []recordedRequest {
	server.mu.Lock()
	defer server.mu.Unlock()
	requests := make([]recordedRequest, len(server.seen))
	for index, request := range server.seen {
		requests[index] = request
		requests[index].Header = request.Header.Clone()
		requests[index].Variables = cloneTestMap(request.Variables)
		requests[index].RawKeys = append([]string(nil), request.RawKeys...)
	}
	return requests
}

func (server *fixtureServer) AssertEveryVariable(t *testing.T, name string, want any) {
	t.Helper()
	requests := server.Requests()
	if len(requests) == 0 {
		t.Fatal("no recorded requests")
	}
	for index, request := range requests {
		if got := request.Variables[name]; !reflect.DeepEqual(got, want) {
			t.Fatalf("request %d variable %q = %#v, want %#v", index, name, got, want)
		}
	}
}

func defaultLinearConfig(endpoint string) tracker.LinearConfig {
	return tracker.LinearConfig{
		ProjectSlug: "symphony", Endpoint: endpoint, CredentialEnv: "LINEAR_API_KEY",
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"},
	}
}

func newLinearAdapter(t *testing.T, server *fixtureServer) *Adapter {
	t.Helper()
	return mustNewLinearAdapter(t, defaultLinearConfig(server.URL()), server.Client(), nil)
}

func mustNewLinearAdapter(t *testing.T, config tracker.LinearConfig, client *http.Client, logger *slog.Logger) *Adapter {
	t.Helper()
	adapter, err := New(config, []byte(tokenCanary), client, logger)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func requireTrackerError(t *testing.T, err error, category tracker.Category) *tracker.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want category %q", category)
	}
	var target *tracker.Error
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T (%v), want *tracker.Error", err, err)
	}
	if target.Category != category {
		t.Fatalf("category = %q, want %q (error %v)", target.Category, category, err)
	}
	return target
}

func assertIdentifiers(t *testing.T, issues []domain.Issue, want []string) {
	t.Helper()
	got := make([]string, len(issues))
	for index, issue := range issues {
		got[index] = issue.Identifier
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("identifiers = %#v, want %#v", got, want)
	}
}

type fixtureBlocker struct {
	Type                  string
	ID, Identifier, State any
}

func fixtureIssue(identifier, state string, blockers []fixtureBlocker) map[string]any {
	id := "issue-" + strings.TrimPrefix(strings.ToLower(identifier), "lin-")
	relations := make([]any, 0, len(blockers))
	for _, blocker := range blockers {
		relationType := blocker.Type
		if relationType == "" {
			relationType = "blocks"
		}
		stateValue := any(nil)
		if blocker.State != nil {
			stateValue = map[string]any{"name": blocker.State}
		}
		relations = append(relations, map[string]any{
			"type": relationType,
			"issue": map[string]any{
				"id": blocker.ID, "identifier": blocker.Identifier, "state": stateValue,
			},
		})
	}
	return map[string]any{
		"id": id, "identifier": identifier, "title": "Issue " + identifier,
		"description": nil, "priority": 2, "state": map[string]any{"name": state},
		"branchName": nil, "url": "https://linear.app/example/issue/" + identifier,
		"assignee": nil,
		"labels": map[string]any{
			"nodes":    []any{map[string]any{"name": " Symphony "}, map[string]any{"name": "BUG"}, map[string]any{"name": "bug"}},
			"pageInfo": map[string]any{"hasNextPage": false},
		},
		"inverseRelations": map[string]any{
			"nodes": relations, "pageInfo": map[string]any{"hasNextPage": false},
		},
		"project":   map[string]any{"id": "project-1", "slugId": "symphony"},
		"team":      map[string]any{"id": "team-1"},
		"createdAt": "2026-08-08T12:00:00-04:00", "updatedAt": "2026-08-08T17:00:00Z",
	}
}

func graphQLPage(nodes []map[string]any, hasNext bool, endCursor any) string {
	items := make([]any, len(nodes))
	for index, node := range nodes {
		items[index] = node
	}
	payload := map[string]any{"data": map[string]any{"issues": map[string]any{
		"nodes":    items,
		"pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": endCursor},
	}}}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func fixtureBody(t *testing.T, name string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "linear", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func decodeTestJSON(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}

func cloneTestMap(value map[string]any) map[string]any {
	encoded, _ := json.Marshal(value)
	clone := map[string]any{}
	_ = decodeTestJSON(encoded, &clone)
	return clone
}

type lockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (buffer *lockedBuffer) Write(payload []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.Buffer.Write(payload)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.Buffer.String()
}
