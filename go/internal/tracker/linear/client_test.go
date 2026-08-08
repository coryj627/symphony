package linear

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/tracker"
)

func TestNewValidatesExactHTTPSScopeAndCredential(t *testing.T) {
	// Break caught: accepting endpoint adornments can send authorization to an
	// ambiguous request target or leak it through userinfo/query diagnostics.
	for _, endpoint := range []string{
		"", "http://linear.example/graphql", "https://", "https:opaque",
		"https://user:secret@linear.example/graphql", "https://linear.example/graphql?token=x",
		"https://linear.example/graphql#fragment",
	} {
		t.Run(endpoint, func(t *testing.T) {
			config := defaultLinearConfig(endpoint)
			_, err := New(config, []byte("token"), &http.Client{}, nil)
			requireTrackerError(t, err, tracker.CategoryConfig)
		})
	}
	config := defaultLinearConfig("https://linear.example/graphql")
	config.ProjectSlug = " "
	_, err := New(config, []byte("token"), &http.Client{}, nil)
	requireTrackerError(t, err, tracker.CategoryConfig)
	config.ProjectSlug = "symphony"
	_, err = New(config, nil, &http.Client{}, nil)
	requireTrackerError(t, err, tracker.CategoryAuth)
}

func TestNewClonesTokenConfigAndCallerHTTPClient(t *testing.T) {
	// Break caught: aliasing caller-owned auth/config/client state allows a
	// resolver cleanup or later mutation to change an in-flight adapter.
	jar := &recordingJar{}
	callerRedirect := func(*http.Request, []*http.Request) error { return errors.New("caller redirect") }
	caller := &http.Client{Timeout: 7 * time.Second, CheckRedirect: callerRedirect, Jar: jar}
	config := defaultLinearConfig("https://linear.example/graphql")
	token := []byte(tokenCanary)
	adapter, err := New(config, token, caller, nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := range token {
		token[index] = 'x'
	}
	config.ProjectSlug = "changed"
	config.TerminalStates[0] = "changed"
	if string(adapter.token) != tokenCanary || adapter.config.ProjectSlug != "symphony" || adapter.config.TerminalStates[0] != "Closed" {
		t.Fatalf("adapter aliases caller: token=%q config=%#v", adapter.token, adapter.config)
	}
	if adapter.client == caller || adapter.client.Timeout != 30*time.Second || adapter.client.CheckRedirect == nil || adapter.client.Jar != nil {
		t.Fatalf("cloned client = %#v", adapter.client)
	}
	if caller.Timeout != 7*time.Second || caller.CheckRedirect == nil || caller.Jar != jar {
		t.Fatalf("caller client mutated = %#v", caller)
	}
}

func TestRequestUsesExactHeadersAndJSONEnvelopeWithClonedToken(t *testing.T) {
	// Break caught: a Bearer prefix, missing content negotiation, or extra JSON
	// field violates Linear's captured request/auth contract.
	server := linearFixtureServer(t, fixtureResponse{Body: graphQLPage(nil, false, nil)})
	token := []byte(tokenCanary)
	adapter, err := New(defaultLinearConfig(server.URL()), token, server.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := range token {
		token[index] = 'x'
	}
	if _, err := adapter.FetchIssuesByStates(context.Background(), []string{"Todo"}); err != nil {
		t.Fatal(err)
	}
	request := server.Requests()[0]
	if request.Header.Get("Authorization") != tokenCanary {
		t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
	}
	if strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ") {
		t.Fatal("Linear authorization incorrectly used Bearer prefix")
	}
	if request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Accept") != "application/json" {
		t.Fatalf("content headers = %#v", request.Header)
	}
	if request.Header.Get("User-Agent") != "symphony-go/linear" {
		t.Fatalf("user agent = %q", request.Header.Get("User-Agent"))
	}
}

func TestHTTPStatusesMapToPortableSafeErrors(t *testing.T) {
	// Break caught: collapsing auth and transient response failures prevents the
	// runtime from presenting remediation or choosing bounded retry policy.
	for _, test := range []struct {
		name      string
		status    int
		category  tracker.Category
		retryable bool
		message   string
	}{
		{name: "authentication", status: http.StatusUnauthorized, category: tracker.CategoryAuth, message: "authentication"},
		{name: "authorization", status: http.StatusForbidden, category: tracker.CategoryAuth, message: "authorization"},
		{name: "bad request", status: http.StatusBadRequest, category: tracker.CategoryResponse},
		{name: "request timeout", status: http.StatusRequestTimeout, category: tracker.CategoryResponse, retryable: true},
		{name: "server error", status: http.StatusBadGateway, category: tracker.CategoryResponse, retryable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := linearFixtureServer(t, fixtureResponse{Status: test.status, Body: tokenCanary})
			_, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
			portable := requireTrackerError(t, err, test.category)
			if portable.Status != test.status || portable.Retryable != test.retryable {
				t.Fatalf("metadata = %#v", portable)
			}
			if test.message != "" && !strings.Contains(strings.ToLower(portable.Message), test.message) {
				t.Fatalf("message = %q, want %q", portable.Message, test.message)
			}
			if strings.Contains(err.Error(), tokenCanary) {
				t.Fatalf("token leaked in %v", err)
			}
		})
	}
}

func TestNonGraphQLHTTPStatusClassificationPrecedesUntrustedBody(t *testing.T) {
	// Break caught: an unreadable or oversized provider error body must not
	// replace the actionable auth, rate-limit, or retryable response category.
	for _, test := range []struct {
		name          string
		status        int
		contentLength int64
		category      tracker.Category
		retryable     bool
	}{
		{name: "unreadable authentication", status: http.StatusUnauthorized, category: tracker.CategoryAuth},
		{name: "oversized rate limit", status: http.StatusTooManyRequests, contentLength: maxResponseBodyBytes + 1, category: tracker.CategoryRateLimited, retryable: true},
		{name: "unreadable server failure", status: http.StatusBadGateway, category: tracker.CategoryResponse, retryable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &trackingReadCloser{readErr: errors.New("provider body was truncated")}
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: test.status, ContentLength: test.contentLength,
					Header: make(http.Header), Body: body, Request: request,
				}, nil
			})}
			adapter := mustNewLinearAdapter(t, defaultLinearConfig("https://linear.example/graphql"), client, nil)
			_, err := adapter.FetchIssuesByStates(context.Background(), []string{"Todo"})
			portable := requireTrackerError(t, err, test.category)
			if portable.Status != test.status || portable.Retryable != test.retryable {
				t.Fatalf("metadata = %#v", portable)
			}
			if !body.closed {
				t.Fatal("response body was not closed")
			}
		})
	}
}

func TestGraphQLErrorsRejectPartialDataAndUseOnlyStableExtensionCodes(t *testing.T) {
	// Break caught: consuming partial GraphQL data or provider-controlled error
	// messages can publish an incomplete queue and leak response details.
	for _, test := range []struct {
		name     string
		status   int
		code     string
		category tracker.Category
	}{
		{name: "HTTP 200 rate limited", status: http.StatusOK, code: "RATELIMITED", category: tracker.CategoryRateLimited},
		{name: "HTTP 400 rate limited", status: http.StatusBadRequest, code: "RATELIMITED", category: tracker.CategoryRateLimited},
		{name: "authentication code", status: http.StatusOK, code: "AUTHENTICATION_ERROR", category: tracker.CategoryAuth},
		{name: "forbidden code", status: http.StatusOK, code: "FORBIDDEN", category: tracker.CategoryAuth},
		{name: "other code", status: http.StatusOK, code: "INTERNAL_ERROR", category: tracker.CategoryPayload},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `{"data":{"issues":{"nodes":[` + rawFixtureIssue(t, fixtureIssue("LIN-12", "Todo", nil)) + `],"pageInfo":{"hasNextPage":false,"endCursor":null}}},"errors":[{"message":"` + tokenCanary + ` provider detail","extensions":{"code":"` + test.code + `"}}]}`
			server := linearFixtureServer(t, fixtureResponse{Status: test.status, Body: body})
			got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
			if got == nil || len(got) != 0 {
				t.Fatalf("partial data returned: %#v", got)
			}
			portable := requireTrackerError(t, err, test.category)
			if strings.Contains(err.Error(), tokenCanary) || strings.Contains(portable.Message, "provider detail") {
				t.Fatalf("provider error detail leaked: %v", err)
			}
			if test.category == tracker.CategoryRateLimited && (!portable.Retryable || portable.RetryAfter <= 0) {
				t.Fatalf("rate metadata = %#v", portable)
			}
		})
	}
}

func TestGraphQLErrorFixtureFailsWithoutPartialData(t *testing.T) {
	// Break caught: the recorded provider contract fixture must exercise the
	// same top-level error path as synthetic adversarial cases.
	server := linearFixtureServer(t, fixtureResponse{File: "graphql-errors.json"})
	got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
	if got == nil || len(got) != 0 {
		t.Fatalf("partial result = %#v", got)
	}
	requireTrackerError(t, err, tracker.CategoryPayload)
}

func TestRateLimitResetHeadersChooseLatestFutureAndCapDelay(t *testing.T) {
	// Break caught: Linear exposes three independent reset instants; choosing an
	// earlier or unbounded value resumes too soon or stalls the queue indefinitely.
	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	header := http.Header{}
	header.Set("X-RateLimit-Requests-Reset", epochMilliseconds(now.Add(2*time.Minute)))
	header.Set("X-RateLimit-Endpoint-Requests-Reset", epochMilliseconds(now.Add(5*time.Minute)))
	header.Set("X-RateLimit-Complexity-Reset", epochMilliseconds(now.Add(3*time.Minute)))
	if got := linearRetryAfter(header, now); got != 5*time.Minute {
		t.Fatalf("retry after = %s, want 5m", got)
	}
	header.Set("X-RateLimit-Complexity-Reset", epochMilliseconds(now.Add(72*time.Hour)))
	if got := linearRetryAfter(header, now); got != 24*time.Hour {
		t.Fatalf("capped retry after = %s", got)
	}
	header = http.Header{
		"X-RateLimit-Requests-Reset":   []string{"bad"},
		"X-RateLimit-Complexity-Reset": []string{epochMilliseconds(now.Add(-time.Minute))},
	}
	if got := linearRetryAfter(header, now); got != time.Minute {
		t.Fatalf("fallback retry after = %s, want 1m", got)
	}
}

func TestHTTP429UsesBoundedResetMetadata(t *testing.T) {
	// Break caught: a 429 without a usable provider reset must still back off for
	// a bounded nonzero interval rather than immediately hot-looping.
	server := linearFixtureServer(t, fixtureResponse{Status: http.StatusTooManyRequests, Body: `{}`})
	_, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
	portable := requireTrackerError(t, err, tracker.CategoryRateLimited)
	if portable.Status != 429 || !portable.Retryable || portable.RetryAfter != time.Minute {
		t.Fatalf("rate metadata = %#v", portable)
	}
}

func TestResponseRequiresBoundedSingleCompleteSchemaEnvelope(t *testing.T) {
	// Break caught: accepting oversized, trailing, or structurally incomplete
	// JSON can conceal truncation or a second provider-controlled document.
	valid := graphQLPage(nil, false, nil)
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "malformed", body: `{"data":`},
		{name: "trailing", body: valid + `{}`},
		{name: "missing data", body: `{}`},
		{name: "missing issues", body: `{"data":{}}`},
		{name: "missing nodes", body: `{"data":{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`},
		{name: "oversized", body: strings.Repeat("x", (4<<20)+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := linearFixtureServer(t, fixtureResponse{Body: test.body})
			got, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
			if got == nil || len(got) != 0 {
				t.Fatalf("partial result = %#v", got)
			}
			requireTrackerError(t, err, tracker.CategoryPayload)
		})
	}
}

func TestTransportCancellationAndReadFailuresAreSafeAndCloseBodies(t *testing.T) {
	// Break caught: treating cancellation as retryable or leaving a body open
	// leaks resources and causes hot retries after an operator stop.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, request.Context().Err()
	})}
	adapter := mustNewLinearAdapter(t, defaultLinearConfig("https://linear.example/graphql"), cancelClient, nil)
	_, err := adapter.FetchIssuesByStates(ctx, []string{"Todo"})
	portable := requireTrackerError(t, err, tracker.CategoryTransport)
	if portable.Retryable {
		t.Fatalf("cancellation was retryable: %#v", portable)
	}

	body := &trackingReadCloser{readErr: errors.New("sensitive transport detail")}
	readClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})}
	adapter = mustNewLinearAdapter(t, defaultLinearConfig("https://linear.example/graphql"), readClient, nil)
	_, err = adapter.FetchIssuesByStates(context.Background(), []string{"Todo"})
	requireTrackerError(t, err, tracker.CategoryTransport)
	if !body.closed {
		t.Fatal("response body was not closed")
	}
	if strings.Contains(err.Error(), "sensitive transport detail") {
		t.Fatalf("raw transport error leaked: %v", err)
	}
}

func TestRedirectIsNotFollowedAndAuthorizationNeverMoves(t *testing.T) {
	// Break caught: even a same-origin redirect can move the captured
	// authorization to an unapproved path; Task 3 disables all redirects.
	server := linearFixtureServer(t, fixtureResponse{
		Status: http.StatusFound, Body: "redirect",
		Headers: http.Header{"Location": []string{"/other"}},
	})
	_, err := newLinearAdapter(t, server).FetchIssuesByStates(context.Background(), []string{"Todo"})
	portable := requireTrackerError(t, err, tracker.CategoryResponse)
	if portable.Status != http.StatusFound {
		t.Fatalf("status = %d", portable.Status)
	}
}

func TestAdapterNeverReturnsOrLogsTokenCanary(t *testing.T) {
	// Break caught: raw request/header/body/error logging creates a credential
	// path before the centralized redactor is available.
	buffer := &lockedBuffer{}
	server := linearFixtureServer(t, fixtureResponse{Status: http.StatusInternalServerError, Body: tokenCanary})
	adapter := mustNewLinearAdapter(t, defaultLinearConfig(server.URL()), server.Client(), slog.New(slog.NewTextHandler(buffer, nil)))
	_, err := adapter.FetchIssuesByStates(context.Background(), []string{"Todo"})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), tokenCanary) || strings.Contains(buffer.String(), tokenCanary) {
		t.Fatalf("token leaked: error=%v log=%s", err, buffer.String())
	}
	for _, name := range []string{"candidates-page-1.json", "candidates-page-2.json", "id-refresh.json", "graphql-errors.json"} {
		if strings.Contains(fixtureBody(t, name), tokenCanary) {
			t.Fatalf("token canary present in fixture %s", name)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type trackingReadCloser struct {
	readErr error
	closed  bool
}

func (body *trackingReadCloser) Read([]byte) (int, error) { return 0, body.readErr }
func (body *trackingReadCloser) Close() error             { body.closed = true; return nil }

type recordingJar struct {
	mu     sync.Mutex
	reads  int
	writes int
}

func (jar *recordingJar) SetCookies(*url.URL, []*http.Cookie) {
	jar.mu.Lock()
	defer jar.mu.Unlock()
	jar.writes++
}
func (jar *recordingJar) Cookies(*url.URL) []*http.Cookie {
	jar.mu.Lock()
	defer jar.mu.Unlock()
	jar.reads++
	return nil
}

func rawFixtureIssue(t *testing.T, node map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func epochMilliseconds(value time.Time) string {
	return strconv.FormatInt(value.UnixMilli(), 10)
}

func TestCallerClientCookieJarIsNeverReadOrWritten(t *testing.T) {
	// Break caught: sharing a caller-owned jar can attach unrelated cookies to
	// Linear or let provider responses mutate caller state across adapters.
	jar := &recordingJar{}
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Set-Cookie": []string{"session=provider"}},
			Body: io.NopCloser(strings.NewReader(graphQLPage(nil, false, nil))),
		}, nil
	})
	caller := &http.Client{Transport: transport, Jar: jar}
	adapter := mustNewLinearAdapter(t, defaultLinearConfig("https://linear.example/graphql"), caller, nil)
	if _, err := adapter.FetchIssuesByStates(context.Background(), []string{"Todo"}); err != nil {
		t.Fatal(err)
	}
	jar.mu.Lock()
	defer jar.mu.Unlock()
	if jar.reads != 0 || jar.writes != 0 {
		t.Fatalf("caller jar reads=%d writes=%d", jar.reads, jar.writes)
	}
	if caller.Jar != jar {
		t.Fatal("caller jar mutated")
	}
}

func TestClientCloneRetainsCallerTransport(t *testing.T) {
	// Break caught: rebuilding instead of cloning the supplied client discards
	// TLS/test transports and makes endpoint verification unusable.
	called := false
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(graphQLPage(nil, false, nil)))}, nil
	})
	caller := &http.Client{Transport: transport}
	adapter := mustNewLinearAdapter(t, defaultLinearConfig("https://linear.example/graphql"), caller, nil)
	if _, err := adapter.FetchIssuesByStates(context.Background(), []string{"Todo"}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("caller transport was not used")
	}
}
