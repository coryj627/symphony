package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/coryj627/symphony/go/internal/tracker"
)

func TestFetchIssuesByStatesPaginatesAcrossMultipleLinkLinesAndReconstructsScopedPath(t *testing.T) {
	// Break caught: following the Link target path directly lets a same-origin
	// response move a bearer request outside the configured repository; reading
	// only one Link header line can silently truncate a valid result.
	server := githubFixtureServer(t, []fixtureResponse{
		{
			Path:  "/api/v3/repos/coryj627/symphony/issues",
			Query: "state=all&per_page=100&page=1",
			File:  "issues-page-1.json",
			Links: func(serverURL string) []string {
				return []string{
					fmt.Sprintf(`<%s/canonical?page=1>; rel="prev"`, serverURL),
					fmt.Sprintf(`<%s/outside/configured/scope?state=closed&per_page=1&page=2>; rel="next"`, serverURL),
				}
			},
		},
		{
			Path:  "/api/v3/repos/coryj627/symphony/issues",
			Query: "state=all&per_page=100&page=2",
			File:  "issues-page-2.json",
		},
	})
	config := defaultGitHubConfig(server.URL + "/api/v3")
	adapter := mustNewGitHubAdapter(t, config, server.Client(), nil)
	issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"OPEN", "CLOSED"})
	if err != nil {
		t.Fatal(err)
	}
	assertIdentifiers(t, issues, []string{"#42", "#43"})
}

func TestFetchIssuesByStatesRejectsMalformedDuplicateCyclicAndUnsafeNextLinks(t *testing.T) {
	// Break caught: accepting ambiguous or unsafe rel=next metadata can repeat a
	// page, cross credential origins, bypass the page cap, or truncate results.
	for _, test := range []struct {
		name  string
		links func(serverURL string) []string
	}{
		{name: "malformed syntax", links: func(string) []string { return []string{"not-a-link"} }},
		{name: "malformed non-relation parameter", links: func(serverURL string) []string {
			return []string{fmt.Sprintf(`<%s/a?page=1>; rel="prev"; title=bad value`, serverURL)}
		}},
		{name: "duplicate next", links: func(serverURL string) []string {
			return []string{
				fmt.Sprintf(`<%s/a?page=2>; rel="next"`, serverURL),
				fmt.Sprintf(`<%s/b?page=3>; rel="next"`, serverURL),
			}
		}},
		{name: "cycle", links: func(serverURL string) []string {
			return []string{fmt.Sprintf(`<%s/a?page=1>; rel="next"`, serverURL)}
		}},
		{name: "cross origin", links: func(string) []string {
			return []string{`<https://other-origin.example/issues?page=2>; rel="next"`}
		}},
		{name: "fragment", links: func(serverURL string) []string {
			return []string{fmt.Sprintf(`<%s/a?page=2#fragment>; rel="next"`, serverURL)}
		}},
		{name: "duplicate page query", links: func(serverURL string) []string {
			return []string{fmt.Sprintf(`<%s/a?page=2&page=3>; rel="next"`, serverURL)}
		}},
		{name: "malformed query encoding", links: func(serverURL string) []string {
			return []string{fmt.Sprintf(`<%s/a?page=2&bad=%%zz>; rel="next"`, serverURL)}
		}},
		{name: "missing page", links: func(serverURL string) []string {
			return []string{fmt.Sprintf(`<%s/a?per_page=100>; rel="next"`, serverURL)}
		}},
		{name: "zero page", links: func(serverURL string) []string {
			return []string{fmt.Sprintf(`<%s/a?page=0>; rel="next"`, serverURL)}
		}},
		{name: "over cap page", links: func(serverURL string) []string {
			return []string{fmt.Sprintf(`<%s/a?page=101>; rel="next"`, serverURL)}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := githubFixtureServer(t, []fixtureResponse{{
				Path:  "/repos/coryj627/symphony/issues",
				Query: "state=all&per_page=100&page=1",
				Body:  issuePage(singleIssue(1, "open")),
				Links: test.links,
			}})
			adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)
			issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"open"})
			if issues == nil || len(issues) != 0 {
				t.Fatalf("partial issues = %#v", issues)
			}
			requireTrackerError(t, err, tracker.CategoryPagination)
		})
	}
}

func TestFetchIssuesByStatesEnforcesOneHundredPageCap(t *testing.T) {
	// Break caught: a provider-controlled unbounded Link chain can hold a poll
	// and consume memory indefinitely even when every individual page is valid.
	var (
		server   *httptest.Server
		requests atomic.Int64
	)
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		page, err := strconv.Atoi(request.URL.Query().Get("page"))
		if err != nil || page < 1 || page > 100 {
			t.Errorf("requested page = %q", request.URL.Query().Get("page"))
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Link", fmt.Sprintf(`<%s/canonical?page=%d>; rel="next"`, server.URL, page+1))
		_, _ = writer.Write([]byte(`[]`))
	}))
	defer server.Close()
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)
	issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"open"})
	if issues == nil || len(issues) != 0 {
		t.Fatalf("issues = %#v", issues)
	}
	requireTrackerError(t, err, tracker.CategoryPagination)
	if got := requests.Load(); got != 100 {
		t.Fatalf("requests = %d, want exactly 100", got)
	}
}

func TestFetchIssuesByStatesReturnsNoPartialOutputAfterSecondPageFailure(t *testing.T) {
	// Break caught: publishing page one before the complete logical fetch
	// succeeds can dispatch from a silently incomplete repository snapshot.
	for _, test := range []struct {
		name     string
		response fixtureResponse
		category tracker.Category
	}{
		{
			name: "status failure",
			response: fixtureResponse{
				Path: "/repos/coryj627/symphony/issues", Query: "state=all&per_page=100&page=2",
				Status: http.StatusServiceUnavailable, Body: `{"message":"provider failure"}`,
			},
			category: tracker.CategoryResponse,
		},
		{
			name: "JSON failure",
			response: fixtureResponse{
				Path: "/repos/coryj627/symphony/issues", Query: "state=all&per_page=100&page=2", Body: `[`,
			},
			category: tracker.CategoryPayload,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := githubFixtureServer(t, []fixtureResponse{
				{
					Path: "/repos/coryj627/symphony/issues", Query: "state=all&per_page=100&page=1",
					Body: issuePage(singleIssue(1, "open")),
					Links: func(serverURL string) []string {
						return []string{fmt.Sprintf(`<%s/canonical?page=2>; rel="next"`, serverURL)}
					},
				},
				test.response,
			})
			adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)
			issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"open"})
			if issues == nil || len(issues) != 0 {
				t.Fatalf("partial issues = %#v", issues)
			}
			requireTrackerError(t, err, test.category)
		})
	}
}

func TestFetchIssuesByStatesRevalidatesExactCachedPageWithETag(t *testing.T) {
	// Break caught: serving cache without a conditional request makes queue data
	// stale, while sending a validator for a different page can accept the wrong
	// body on a 304 response.
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		call := requests.Add(1)
		switch call {
		case 1:
			if request.Header.Get("If-None-Match") != "" {
				t.Errorf("first request validator = %q", request.Header.Get("If-None-Match"))
			}
			writer.Header().Set("ETag", `"page-v1"`)
			_, _ = writer.Write([]byte(issuePage(singleIssue(1, "open"))))
		case 2:
			if got := request.Header.Get("If-None-Match"); got != `"page-v1"` {
				t.Errorf("validator = %q, want page-v1", got)
			}
			writer.WriteHeader(http.StatusNotModified)
		default:
			t.Errorf("unexpected request %d", call)
		}
	}))
	defer server.Close()
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)
	for attempt := 0; attempt < 2; attempt++ {
		issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"open"})
		if err != nil {
			t.Fatal(err)
		}
		assertIdentifiers(t, issues, []string{"#1"})
	}
}

func TestFetchIssuesByStatesRejects304WithoutExactCachedPage(t *testing.T) {
	// Break caught: treating an unsolicited 304 as an empty page silently hides
	// every issue and violates logical-fetch completeness.
	server := githubFixtureServer(t, []fixtureResponse{{
		Path: "/repos/coryj627/symphony/issues", Query: "state=all&per_page=100&page=1", Status: http.StatusNotModified,
	}})
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)
	issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"open"})
	if issues == nil || len(issues) != 0 {
		t.Fatalf("issues = %#v", issues)
	}
	requireTrackerError(t, err, tracker.CategoryResponse)
}

func TestFetchIssuesByStatesCommitsStagedCacheOnlyAfterEveryPageSucceeds(t *testing.T) {
	// Break caught: committing page one's new validator before page two fails
	// creates a mixed-generation cache that a later 304 can publish.
	var (
		server *httptest.Server
		call   atomic.Int64
	)
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		page := request.URL.Query().Get("page")
		switch call.Add(1) {
		case 1:
			if page != "1" || request.Header.Get("If-None-Match") != "" {
				t.Errorf("initial page 1 request = page %q validator %q", page, request.Header.Get("If-None-Match"))
			}
			writer.Header().Set("ETag", `"old-one"`)
			writer.Header().Set("Link", fmt.Sprintf(`<%s/canonical?page=2>; rel="next"`, server.URL))
			_, _ = writer.Write([]byte(issuePage(singleIssue(1, "open"))))
		case 2:
			if page != "2" || request.Header.Get("If-None-Match") != "" {
				t.Errorf("initial page 2 request = page %q validator %q", page, request.Header.Get("If-None-Match"))
			}
			writer.Header().Set("ETag", `"old-two"`)
			_, _ = writer.Write([]byte(issuePage(singleIssue(2, "open"))))
		case 3:
			if page != "1" || request.Header.Get("If-None-Match") != `"old-one"` {
				t.Errorf("failed generation page 1 = page %q validator %q", page, request.Header.Get("If-None-Match"))
			}
			writer.Header().Set("ETag", `"new-one"`)
			writer.Header().Set("Link", fmt.Sprintf(`<%s/canonical?page=2>; rel="next"`, server.URL))
			_, _ = writer.Write([]byte(issuePage(singleIssue(10, "open"))))
		case 4:
			if page != "2" || request.Header.Get("If-None-Match") != `"old-two"` {
				t.Errorf("failed generation page 2 = page %q validator %q", page, request.Header.Get("If-None-Match"))
			}
			writer.WriteHeader(http.StatusInternalServerError)
		case 5:
			if page != "1" || request.Header.Get("If-None-Match") != `"old-one"` {
				t.Errorf("retry page 1 = page %q validator %q, staged cache leaked", page, request.Header.Get("If-None-Match"))
			}
			writer.WriteHeader(http.StatusNotModified)
		case 6:
			if page != "2" || request.Header.Get("If-None-Match") != `"old-two"` {
				t.Errorf("retry page 2 = page %q validator %q", page, request.Header.Get("If-None-Match"))
			}
			writer.WriteHeader(http.StatusNotModified)
		default:
			t.Errorf("unexpected request")
		}
	}))
	defer server.Close()
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)

	issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"open"})
	if err != nil {
		t.Fatal(err)
	}
	assertIdentifiers(t, issues, []string{"#1", "#2"})

	issues, err = adapter.FetchIssuesByStates(context.Background(), []string{"open"})
	if issues == nil || len(issues) != 0 {
		t.Fatalf("failed generation returned partial %#v", issues)
	}
	requireTrackerError(t, err, tracker.CategoryResponse)

	issues, err = adapter.FetchIssuesByStates(context.Background(), []string{"open"})
	if err != nil {
		t.Fatal(err)
	}
	assertIdentifiers(t, issues, []string{"#1", "#2"})
}

func TestFetchIssuesByStatesSerializesConditionalCacheAccessForRaceSafety(t *testing.T) {
	// Break caught: concurrent polls that mutate shared ETag/body metadata race
	// and can pair a validator with another goroutine's response body.
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if requests.Add(1) == 1 {
			writer.Header().Set("ETag", `"shared"`)
			_, _ = writer.Write([]byte(issuePage(singleIssue(5, "open"))))
			return
		}
		if request.Header.Get("If-None-Match") != `"shared"` {
			t.Errorf("concurrent validator = %q", request.Header.Get("If-None-Match"))
		}
		writer.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)

	const goroutines = 24
	start := make(chan struct{})
	errorsFound := make(chan error, goroutines)
	var wait sync.WaitGroup
	for range goroutines {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"open"})
			if err == nil && (len(issues) != 1 || issues[0].Identifier != "#5") {
				err = fmt.Errorf("issues = %#v", issues)
			}
			errorsFound <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPaginationLinkParserHandlesQuotedCommasWithoutSplittingRelations(t *testing.T) {
	// Break caught: splitting Link with strings.Split on commas corrupts a
	// quoted extension parameter and loses the valid next relation after it.
	server := githubFixtureServer(t, []fixtureResponse{
		{
			Path: "/repos/coryj627/symphony/issues", Query: "state=all&per_page=100&page=1", Body: `[]`,
			Links: func(serverURL string) []string {
				return []string{fmt.Sprintf(`<%s/previous?page=1>; rel="prev"; title="one,two", <%s/next?page=2>; rel="next"`, serverURL, serverURL)}
			},
		},
		{Path: "/repos/coryj627/symphony/issues", Query: "state=all&per_page=100&page=2", Body: `[]`},
	})
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)
	issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"open"})
	if err != nil || issues == nil || len(issues) != 0 {
		t.Fatalf("issues = %#v, error = %v", issues, err)
	}
}
