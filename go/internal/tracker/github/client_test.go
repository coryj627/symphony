package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/tracker"
)

func TestNewRejectsUnsafeEndpointFormsAndMissingToken(t *testing.T) {
	// Break caught: accepting an endpoint with userinfo/query/fragment or a
	// non-HTTPS scheme can disclose bearer credentials outside the configured
	// API boundary; accepting no token defers a deterministic auth failure.
	for _, test := range []struct {
		name     string
		endpoint string
		category tracker.Category
		token    []byte
	}{
		{name: "HTTP", endpoint: "http://api.github.test", category: tracker.CategoryConfig, token: []byte("token")},
		{name: "missing host", endpoint: "https:///api/v3", category: tracker.CategoryConfig, token: []byte("token")},
		{name: "userinfo", endpoint: "https://user@example.test/api/v3", category: tracker.CategoryConfig, token: []byte("token")},
		{name: "query", endpoint: "https://example.test/api/v3?token=value", category: tracker.CategoryConfig, token: []byte("token")},
		{name: "fragment", endpoint: "https://example.test/api/v3#scope", category: tracker.CategoryConfig, token: []byte("token")},
		{name: "missing token", endpoint: "https://example.test/api/v3", category: tracker.CategoryAuth, token: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter, err := New(defaultGitHubConfig(test.endpoint), test.token, nil, nil)
			if adapter != nil {
				t.Fatalf("adapter = %#v, want nil", adapter)
			}
			requireTrackerError(t, err, test.category)
			if strings.Contains(err.Error(), "token=value") || (len(test.token) > 0 && strings.Contains(err.Error(), string(test.token))) {
				t.Fatalf("unsafe constructor error = %q", err)
			}
		})
	}
}

func TestNewClonesTokenAndHTTPClientAndSendsExactScopedHeaders(t *testing.T) {
	// Break caught: retaining the caller's token slice makes secure zeroing
	// disable auth, while mutating/retaining the caller's client lets unrelated
	// timeout or redirect changes alter adapter behavior.
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if got := request.URL.EscapedPath(); got != "/api/v3/repos/Owner%20Name/Repo%2FName/issues" {
			t.Errorf("escaped path = %q", got)
		}
		if got := request.URL.RawQuery; got != "state=all&per_page=100&page=1" {
			t.Errorf("query = %q", got)
		}
		if got := request.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q", got)
		}
		if got := request.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Errorf("API version = %q", got)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer "+tokenCanary {
			t.Errorf("Authorization did not retain cloned token")
		}
		if got := request.Header.Get("User-Agent"); strings.TrimSpace(got) == "" || strings.Contains(got, tokenCanary) {
			t.Errorf("User-Agent = %q, want stable non-secret value", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(issuePage(singleIssue(42, "open"))))
	}))
	defer server.Close()

	originalRedirect := func(*http.Request, []*http.Request) error { return errors.New("caller redirect policy") }
	caller := server.Client()
	caller.Timeout = 7 * time.Second
	caller.CheckRedirect = originalRedirect
	config := defaultGitHubConfig(server.URL + "/api/v3/")
	config.Owner = "Owner Name"
	config.Repository = "Repo/Name"
	token := []byte(tokenCanary)
	adapter, err := New(config, token, caller, nil)
	if err != nil {
		t.Fatal(err)
	}
	if caller.Timeout != 7*time.Second || reflect.ValueOf(caller.CheckRedirect).Pointer() != reflect.ValueOf(originalRedirect).Pointer() {
		t.Fatal("New mutated the caller-owned HTTP client")
	}
	for index := range token {
		token[index] = 0
	}
	caller.Timeout = time.Nanosecond
	caller.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("changed after construction") }
	issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"open"})
	if err != nil {
		t.Fatal(err)
	}
	assertIdentifiers(t, issues, []string{"#42"})
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}
}

func TestNewDoesNotShareCallerCookieJar(t *testing.T) {
	// Break caught: retaining the caller's CookieJar sends unrelated cookies to
	// GitHub and lets provider Set-Cookie responses mutate caller-owned state.
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Cookie"); got != "" {
			t.Errorf("adapter sent caller cookie %q", got)
		}
		writer.Header().Set("Set-Cookie", "github_session=provider-value; Path=/")
		_, _ = writer.Write([]byte(issuePage(singleIssue(42, "open"))))
	}))
	defer server.Close()

	jar := &recordingCookieJar{}
	caller := server.Client()
	caller.Jar = jar
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), caller, nil)
	if caller.Jar != jar {
		t.Fatal("New mutated the caller-owned CookieJar")
	}

	issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"open"})
	if err != nil {
		t.Fatal(err)
	}
	assertIdentifiers(t, issues, []string{"#42"})
	reads, writes := jar.calls()
	if reads != 0 || writes != 0 {
		t.Fatalf("caller CookieJar calls = reads %d, writes %d; want zero", reads, writes)
	}
	if caller.Jar != jar {
		t.Fatal("adapter request mutated the original HTTP client")
	}
}

func TestNewWithNilClientStillEnforcesThirtySecondTimeout(t *testing.T) {
	// Break caught: a nil client path that uses an unbounded default client can
	// leave polling permanently stalled on a provider request.
	adapter, err := New(defaultGitHubConfig("https://api.github.test"), []byte(tokenCanary), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.client == nil || adapter.client.Timeout != 30*time.Second {
		t.Fatalf("client timeout = %#v", adapter.client)
	}
}

func TestClientRejectsCrossOriginRedirectBeforeTargetRequest(t *testing.T) {
	// Break caught: following a cross-origin redirect forwards a scoped bearer
	// request beyond the configured API origin.
	var targetRequests atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetRequests.Add(1)
	}))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/capture", http.StatusFound)
	}))
	defer source.Close()
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(source.URL), source.Client(), nil)
	issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"open"})
	if issues == nil || len(issues) != 0 {
		t.Fatalf("issues = %#v", issues)
	}
	requireTrackerError(t, err, tracker.CategoryTransport)
	if got := targetRequests.Load(); got != 0 {
		t.Fatalf("cross-origin target requests = %d, want 0", got)
	}
	if strings.Contains(err.Error(), tokenCanary) || strings.Contains(err.Error(), target.URL) {
		t.Fatalf("redirect error leaked request detail: %q", err)
	}
}

func TestClientAppliesFiniteSameOriginRedirectLimit(t *testing.T) {
	// Break caught: an unrestricted same-origin redirect cycle can hold the
	// state-fetch lock forever and continuously replay authorization headers.
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		http.Redirect(writer, request, request.URL.Path+"?hop=again", http.StatusFound)
	}))
	defer server.Close()
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)
	_, err := adapter.FetchIssuesByStates(context.Background(), []string{"open"})
	requireTrackerError(t, err, tracker.CategoryTransport)
	if got := requests.Load(); got < 2 || got > 10 {
		t.Fatalf("same-origin redirect requests = %d, want a finite limit <= 10", got)
	}
}

func TestClientMapsHTTPFailuresToPortableSafeCategoriesAndRetryMetadata(t *testing.T) {
	// Break caught: collapsing auth, authorization, rate-limit, and retryable
	// provider statuses removes the runtime's recovery signal; rendering the
	// provider body creates a secret-bearing diagnostic channel.
	now := time.Now()
	for _, test := range []struct {
		name              string
		status            int
		headers           http.Header
		category          tracker.Category
		retryable         bool
		messageContains   string
		wantRetryAfter    time.Duration
		retryAfterAtLeast time.Duration
		retryAfterAtMost  time.Duration
	}{
		{name: "authentication", status: http.StatusUnauthorized, category: tracker.CategoryAuth, messageContains: "authentication"},
		{name: "authorization", status: http.StatusForbidden, category: tracker.CategoryAuth, messageContains: "authorization"},
		{
			name: "primary rate limit", status: http.StatusForbidden, category: tracker.CategoryRateLimited, retryable: true,
			headers:           http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(now.Add(90*time.Second).Unix(), 10)}},
			retryAfterAtLeast: 80 * time.Second, retryAfterAtMost: 24 * time.Hour,
		},
		{
			name: "retry after seconds", status: http.StatusTooManyRequests, category: tracker.CategoryRateLimited, retryable: true,
			headers: http.Header{"Retry-After": {"120"}}, wantRetryAfter: 120 * time.Second,
		},
		{
			name: "bounded retry after", status: http.StatusTooManyRequests, category: tracker.CategoryRateLimited, retryable: true,
			headers: http.Header{"Retry-After": {"999999999999"}}, retryAfterAtMost: 24 * time.Hour,
		},
		{name: "request timeout", status: http.StatusRequestTimeout, category: tracker.CategoryResponse, retryable: true},
		{name: "server failure", status: http.StatusServiceUnavailable, category: tracker.CategoryResponse, retryable: true},
		{name: "ordinary response", status: http.StatusUnprocessableEntity, category: tracker.CategoryResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := githubFixtureServer(t, []fixtureResponse{{
				Path:    "/repos/coryj627/symphony/issues",
				Query:   "state=all&per_page=100&page=1",
				Status:  test.status,
				Headers: test.headers,
				File:    "rate-limited.json",
			}})
			adapter := mustNewGitHubAdapter(t, defaultGitHubConfig(server.URL), server.Client(), nil)
			issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"open"})
			if issues == nil || len(issues) != 0 {
				t.Fatalf("issues = %#v", issues)
			}
			portable := requireTrackerError(t, err, test.category)
			if portable.Status != test.status || portable.Retryable != test.retryable {
				t.Fatalf("portable metadata = %#v", portable)
			}
			if test.messageContains != "" && !strings.Contains(strings.ToLower(portable.Message), test.messageContains) {
				t.Fatalf("message = %q, want %q", portable.Message, test.messageContains)
			}
			if test.wantRetryAfter != 0 && portable.RetryAfter != test.wantRetryAfter {
				t.Fatalf("retry after = %v, want %v", portable.RetryAfter, test.wantRetryAfter)
			}
			if portable.RetryAfter < 0 || portable.RetryAfter < test.retryAfterAtLeast || (test.retryAfterAtMost != 0 && portable.RetryAfter > test.retryAfterAtMost) {
				t.Fatalf("retry after = %v, want %v <= value <= %v", portable.RetryAfter, test.retryAfterAtLeast, test.retryAfterAtMost)
			}
			for _, forbidden := range []string{tokenCanary, "provider-owned diagnostic identity", "documentation_url"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("error leaked %q: %q", forbidden, err)
				}
			}
		})
	}
}

func TestClientMapsCancellationAndRawTransportFailureWithoutLeakingCause(t *testing.T) {
	// Break caught: returning net/http's raw error can expose URLs or transport
	// diagnostics, while treating caller cancellation as retryable causes an
	// unwanted request loop.
	for _, test := range []struct {
		name      string
		context   func() (context.Context, context.CancelFunc)
		transport roundTripperFunc
		retryable bool
	}{
		{
			name: "cancellation",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			transport: func(request *http.Request) (*http.Response, error) { return nil, request.Context().Err() },
		},
		{
			name: "transport",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			transport: func(*http.Request) (*http.Response, error) {
				return nil, errors.New("raw-transport-cause " + tokenCanary)
			},
			retryable: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := test.context()
			defer cancel()
			adapter := mustNewGitHubAdapter(t, defaultGitHubConfig("https://api.github.test"), &http.Client{Transport: test.transport}, nil)
			_, err := adapter.FetchIssuesByStates(ctx, []string{"open"})
			portable := requireTrackerError(t, err, tracker.CategoryTransport)
			if portable.Retryable != test.retryable {
				t.Fatalf("retryable = %v, want %v", portable.Retryable, test.retryable)
			}
			if strings.Contains(err.Error(), tokenCanary) || strings.Contains(err.Error(), "raw-transport-cause") || strings.Contains(err.Error(), "api.github.test") {
				t.Fatalf("transport error leaked cause/request: %q", err)
			}
		})
	}
}

func TestClientRejectsSuccessBodyLargerThanSixteenMiB(t *testing.T) {
	// Break caught: an unbounded success response lets a provider or proxy
	// exhaust process memory before JSON validation.
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        make(http.Header),
			Body:          io.NopCloser(&repeatedByteReader{remaining: maxResponseBodyBytes + 1}),
			ContentLength: -1,
			Request:       request,
		}, nil
	})}
	adapter := mustNewGitHubAdapter(t, defaultGitHubConfig("https://api.github.test"), client, nil)
	issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"open"})
	if issues == nil || len(issues) != 0 {
		t.Fatalf("issues = %#v", issues)
	}
	requireTrackerError(t, err, tracker.CategoryPayload)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type recordingCookieJar struct {
	mu     sync.Mutex
	reads  int
	writes int
}

func (jar *recordingCookieJar) Cookies(*url.URL) []*http.Cookie {
	jar.mu.Lock()
	defer jar.mu.Unlock()
	jar.reads++
	return []*http.Cookie{{Name: "caller_session", Value: "caller-cookie-canary"}}
}

func (jar *recordingCookieJar) SetCookies(*url.URL, []*http.Cookie) {
	jar.mu.Lock()
	defer jar.mu.Unlock()
	jar.writes++
}

func (jar *recordingCookieJar) calls() (int, int) {
	jar.mu.Lock()
	defer jar.mu.Unlock()
	return jar.reads, jar.writes
}

type repeatedByteReader struct {
	remaining int64
}

func (reader *repeatedByteReader) Read(payload []byte) (int, error) {
	if reader.remaining == 0 {
		return 0, io.EOF
	}
	count := int64(len(payload))
	if count > reader.remaining {
		count = reader.remaining
	}
	for index := int64(0); index < count; index++ {
		payload[index] = 'x'
	}
	reader.remaining -= count
	return int(count), nil
}
