package github

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/tracker"
)

const tokenCanary = "github-token-canary-should-never-leak"

type fixtureResponse struct {
	Path    string
	Query   string
	File    string
	Body    string
	Status  int
	Headers http.Header
	Links   func(serverURL string) []string
}

func githubFixtureServer(t *testing.T, responses []fixtureResponse) *httptest.Server {
	t.Helper()
	var (
		server *httptest.Server
		mu     sync.Mutex
		next   int
	)
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if next >= len(responses) {
			t.Errorf("unexpected request %s %s", request.Method, request.URL.String())
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		response := responses[next]
		next++
		if request.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", request.Method)
		}
		if request.URL.EscapedPath() != response.Path {
			t.Errorf("path = %q, want %q", request.URL.EscapedPath(), response.Path)
		}
		if request.URL.RawQuery != response.Query {
			t.Errorf("query = %q, want %q", request.URL.RawQuery, response.Query)
		}
		for name, values := range response.Headers {
			for _, value := range values {
				writer.Header().Add(name, value)
			}
		}
		if response.Links != nil {
			for _, value := range response.Links(server.URL) {
				writer.Header().Add("Link", value)
			}
		}
		status := response.Status
		if status == 0 {
			status = http.StatusOK
		}
		writer.WriteHeader(status)
		body := response.Body
		if response.File != "" {
			contents, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "github", response.File))
			if err != nil {
				t.Errorf("read fixture %s: %v", response.File, err)
				return
			}
			body = string(contents)
		}
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(func() {
		server.Close()
		mu.Lock()
		defer mu.Unlock()
		if next != len(responses) {
			t.Errorf("server received %d requests, want %d", next, len(responses))
		}
	})
	return server
}

func defaultGitHubConfig(endpoint string) tracker.GitHubConfig {
	return tracker.GitHubConfig{
		Owner:          "coryj627",
		Repository:     "symphony",
		Endpoint:       endpoint,
		CredentialEnv:  "GITHUB_TOKEN",
		ActiveStates:   []string{"open"},
		TerminalStates: []string{"closed"},
	}
}

func mustNewGitHubAdapter(t *testing.T, config tracker.GitHubConfig, client *http.Client, logger *slog.Logger) *Adapter {
	t.Helper()
	adapter, err := New(config, []byte(tokenCanary), client, logger)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
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

func fixtureBody(t *testing.T, name string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "github", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func singleIssue(number uint64, state string) string {
	return fmt.Sprintf(`{"id":%d,"node_id":"node-%d","number":%d,"title":"Issue %d","body":null,"state":%q,"html_url":"https://github.com/coryj627/symphony/issues/%d","labels":[],"assignees":[],"created_at":null,"updated_at":null}`,
		number+9007199254740000, number, number, number, state, number)
}

func issuePage(records ...string) string {
	var body bytes.Buffer
	body.WriteByte('[')
	for index, record := range records {
		if index > 0 {
			body.WriteByte(',')
		}
		body.WriteString(record)
	}
	body.WriteByte(']')
	return body.String()
}
