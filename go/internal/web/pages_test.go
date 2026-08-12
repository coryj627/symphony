package web

import (
	"bufio"
	"context"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
)

const testCSRFToken = "test-csrf-value-never-log"

func TestEveryPageHasUniqueTitleMainH1AndRoutineStatusIsNotLive(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path    string
		title   string
		heading string
		status  string
	}{
		{"/", "Overview — Symphony", "Overview", "No scheduler is running. Current tracker work is shown."},
		{"/issues", "Issues — Symphony", "Issues", "No tracker work candidates match these filters."},
		{"/issues/SYM-123", "SYM-123 — Symphony", "Issue SYM-123", "Issue details are shown."},
		{"/activity", "Activity — Symphony", "Activity", "No activity has been recorded."},
		{"/configuration", "Configuration — Symphony", "Configuration", "Configuration has not been loaded."},
		{"/logs", "Logs — Symphony", "Logs", "No log entries are available."},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			html := renderedGET(t, tc.path)
			assertContainsOnce(t, html, `<main id="main-content" tabindex="-1">`)
			assertContainsOnce(t, html, "<h1>")
			assertContains(t, html, "<title>"+tc.title+"</title>")
			assertContains(t, html, ">"+tc.heading+"</h1>")
			assertContains(t, html, tc.status)
			if strings.Contains(html, `role="status"`) || strings.Contains(html, `aria-live=`) {
				t.Fatal("routine page status was exposed as a live region")
			}
			assertContainsOnce(t, html, `<header class="site-header">`)
			assertContainsOnce(t, html, `<nav aria-label="Primary">`)
		})
	}
}

func TestLivePagesRenderCanonicalProgressiveControlsAndServerGeneratedURLs(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	runtime := &pageRuntimeFake{
		snapshot: domain.Snapshot{
			GeneratedAt: now,
			EventCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 7},
			Candidates:  []domain.CandidateRow{{Issue: domain.Issue{ID: "one", Identifier: "ONE-1", Title: "Alpha", State: "Open", Labels: []string{}}, Routable: true, RoutingReasons: []string{}}},
		},
		recent: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 7}, Events: []domain.Event{{Epoch: "epoch-a", Sequence: 7, Type: "queue.refreshed", At: now, Data: map[string]any{}}}},
	}
	handler := newTestPageHandler(t, PageOptions{Mode: "run", Queries: runtime, Commands: runtime})
	handler.resolveDependencies = func(_ *http.Request, base pageDependencies) (pageDependencies, string, bool) {
		return base, "", true
	}
	tests := []struct {
		path      string
		route     string
		stateURL  string
		eventsURL string
	}{
		{path: "/?credential=overview-canary", route: "overview", stateURL: "/api/v1/state", eventsURL: "/api/v1/events?after=epoch-a%3A7"},
		{path: "/issues?query=Alpha&query=ignored-canary&state=open&eligibility=routable&sort=identifier&credential=issues-canary", route: "issues", stateURL: "/api/v1/state?eligibility=routable&amp;query=Alpha&amp;sort=identifier&amp;state=Open", eventsURL: "/api/v1/events?after=epoch-a%3A7"},
		{path: "/activity?credential=activity-canary", route: "activity", stateURL: "/api/v1/state", eventsURL: "/api/v1/events?after=epoch-a%3A7"},
	}
	for _, test := range tests {
		t.Run(test.route, func(t *testing.T) {
			recorder := serveDirect(t, handler, http.MethodGet, test.path, "", nil)
			html := recorder.Body.String()
			for _, want := range []string{
				`data-live-root data-live-route="` + test.route + `"`,
				`data-event-cursor-id="epoch-a:7"`,
				`data-state-url="` + test.stateURL + `"`,
				`data-events-url="` + test.eventsURL + `"`,
				`data-live-controls hidden`,
				`data-live-toggle>Pause live updates</button>`,
				`data-live-connection`,
				`data-live-apply hidden>Apply pending updates</button>`,
				`data-live-feedback`,
				`<script type="module" src="/static/app.js"></script>`,
			} {
				if !strings.Contains(html, want) {
					t.Fatalf("%s omitted %q", test.path, want)
				}
			}
			if test.route == "overview" && !strings.Contains(html, `data-live-refresh-form`) {
				t.Fatalf("%s omitted the existing manual Refresh form enhancement hook", test.path)
			}
			if strings.Contains(html, "credential=") || strings.Contains(html, "ignored-canary") || strings.Contains(html, `role="status"`) || strings.Contains(html, `aria-live=`) || strings.Contains(html, `aria-atomic=`) {
				t.Fatalf("%s copied unsafe query state or rendered a routine live region", test.path)
			}
		})
	}
}

func TestEventsValidReconnectHeaderBeatsMalformedAfterThroughDependencyResolution(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	runtime := &sseRuntimeFake{eventPages: []sseEventResult{
		{page: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "e2e", Sequence: 2}, Events: []domain.Event{{Epoch: "e2e", Sequence: 2, Type: "queue.refreshed", At: now, Data: map[string]any{}}}}},
		{page: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "e2e", Sequence: 2}, Events: []domain.Event{}}},
	}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/events?after=%ZZ&after=stale%3A1&__e2e_scenario=empty", nil)
	request.Header.Set("Last-Event-ID", "e2e:1")
	request = request.WithContext(context.WithValue(request.Context(), csrfContextKey{}, testCSRFToken))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(&deadlineRecorder{ResponseRecorder: recorder}, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" || !strings.Contains(recorder.Body.String(), "id: e2e:2\n") {
		t.Fatalf("resolved reconnect = %d/%q body=%s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
}

func TestEventsMalformedEncodedScenarioKeyFailsClosedInE2EResolver(t *testing.T) {
	runtime := &sseRuntimeFake{events: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "production-base", Sequence: 1}, Reset: true, Events: []domain.Event{}}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/events?__e2e_scenario%ZZ=populated&after=e2e%3A1", nil)
	request.Header.Set("Last-Event-ID", "e2e:1")
	request = request.WithContext(context.WithValue(request.Context(), csrfContextKey{}, testCSRFToken))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(&deadlineRecorder{ResponseRecorder: recorder}, request)
	// The production resolver is deliberately scenario-blind and therefore
	// reaches the injected base runtime. The e2e resolver must reject this
	// malformed selector before selecting any fixture.
	if strings.Contains(recorder.Body.String(), "production-base:1") {
		return
	}
	if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), `"code":"not_found"`) || runtime.eventsCalls != 0 {
		t.Fatalf("malformed scenario selector = %d calls=%d body=%s", recorder.Code, runtime.eventsCalls, recorder.Body.String())
	}
}

func TestLiveResumeFailureFixtureFailsOnlyTheFirstStateRequest(t *testing.T) {
	handler := newTestPageHandler(t, PageOptions{})
	const scenario = "?__e2e_scenario=live-resume-failure"

	overview := serveDirect(t, handler, http.MethodGet, "/"+scenario, "", nil)
	if !strings.Contains(overview.Body.String(), "__e2e_scenario=live-resume-failure") {
		t.Skip("e2e fixture resolver is not enabled")
	}
	issues := serveDirect(t, handler, http.MethodGet, "/issues"+scenario, "", nil)
	if overview.Code != http.StatusOK || issues.Code != http.StatusOK || !strings.Contains(issues.Body.String(), "LIVE-1") {
		t.Fatalf("server-rendered fixture pages = overview %d, issues %d body=%s", overview.Code, issues.Code, issues.Body.String())
	}

	firstState := serveDirect(t, handler, http.MethodGet, "/api/v1/state"+scenario, "", nil)
	secondState := serveDirect(t, handler, http.MethodGet, "/api/v1/state"+scenario, "", nil)
	if firstState.Code != http.StatusServiceUnavailable || !strings.Contains(firstState.Body.String(), `"code":"runtime_unavailable"`) {
		t.Fatalf("first state request = %d body=%s", firstState.Code, firstState.Body.String())
	}
	if secondState.Code != http.StatusOK || !strings.Contains(secondState.Body.String(), `"identifier":"LIVE-1"`) {
		t.Fatalf("second state request = %d body=%s", secondState.Code, secondState.Body.String())
	}
}

func TestLiveFixturesArePristineAndIndependentForEachBrowserEngine(t *testing.T) {
	handler := newTestPageHandler(t, PageOptions{})
	const (
		scenario   = "?__e2e_scenario=live-focus"
		chromiumUA = "Mozilla/5.0 AppleWebKit/537.36 Chrome/140.0 Safari/537.36"
		webkitUA   = "Mozilla/5.0 AppleWebKit/605.1.15 Version/18.0 Safari/605.1.15"
	)
	getIssues := func(userAgent string) *httptest.ResponseRecorder {
		return serveDirect(t, handler, http.MethodGet, "/issues"+scenario, "", map[string]string{"User-Agent": userAgent})
	}
	refresh := func(userAgent string) *httptest.ResponseRecorder {
		return serveDirect(t, handler, http.MethodPost, "/api/v1/refresh"+scenario, `{}`, map[string]string{"Content-Type": "application/json", "User-Agent": userAgent})
	}

	chromiumInitial := getIssues(chromiumUA)
	if !strings.Contains(chromiumInitial.Body.String(), "__e2e_scenario=live-focus") {
		t.Skip("e2e fixture resolver is not enabled")
	}
	webkitInitial := getIssues(webkitUA)
	for name, recorder := range map[string]*httptest.ResponseRecorder{"chromium": chromiumInitial, "webkit": webkitInitial} {
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Before refresh") || strings.Contains(recorder.Body.String(), "After refresh") {
			t.Fatalf("%s initial live fixture = %d body=%s", name, recorder.Code, recorder.Body.String())
		}
	}

	if recorder := refresh(chromiumUA); recorder.Code != http.StatusAccepted {
		t.Fatalf("chromium refresh = %d body=%s", recorder.Code, recorder.Body.String())
	}
	chromiumAfter, webkitStillInitial := getIssues(chromiumUA), getIssues(webkitUA)
	if !strings.Contains(chromiumAfter.Body.String(), "After refresh") {
		t.Fatalf("chromium mutation did not persist: %s", chromiumAfter.Body.String())
	}
	if !strings.Contains(webkitStillInitial.Body.String(), "Before refresh") || strings.Contains(webkitStillInitial.Body.String(), "After refresh") {
		t.Fatalf("webkit fixture was mutated by chromium: %s", webkitStillInitial.Body.String())
	}
	if recorder := refresh(webkitUA); recorder.Code != http.StatusAccepted {
		t.Fatalf("webkit refresh = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if webkitAfter := getIssues(webkitUA); !strings.Contains(webkitAfter.Body.String(), "After refresh") {
		t.Fatalf("webkit mutation did not persist: %s", webkitAfter.Body.String())
	}
}

func TestSkipLinkIsFirstFocusableElementAndTargetsMain(t *testing.T) {
	t.Parallel()
	html := renderedGET(t, "/")
	body := strings.Index(html, "<body>")
	skip := strings.Index(html, `<a class="skip-link" href="#main-content" tabindex="0">Skip to main content</a>`)
	if body == -1 || skip == -1 || skip < body {
		t.Fatalf("rendered page does not begin its body with the skip link")
	}
	prefix := html[body+len("<body>") : skip]
	if focusableTagPattern.MatchString(prefix) {
		t.Fatalf("focusable markup precedes skip link: %q", prefix)
	}
}

func TestNavigationOrderAndCurrentPageAreStable(t *testing.T) {
	t.Parallel()
	routes := []string{"/", "/issues", "/activity", "/configuration", "/logs"}
	wants := []string{"Overview", "Issues", "Activity", "Configuration", "Logs"}
	for routeIndex, route := range routes {
		html := renderedGET(t, route)
		positions := make([]int, len(wants))
		for index, label := range wants {
			needle := ">" + label + "</a>"
			positions[index] = strings.Index(html, needle)
			if positions[index] == -1 {
				t.Fatalf("%s: missing navigation item %q", route, label)
			}
			if index > 0 && positions[index] <= positions[index-1] {
				t.Fatalf("%s: navigation item %q is out of order", route, label)
			}
		}
		current := regexp.MustCompile(`<a href="[^"]+" aria-current="page">` + regexp.QuoteMeta(wants[routeIndex]) + `</a>`)
		if matches := current.FindAllString(html, -1); len(matches) != 1 {
			t.Fatalf("%s: current navigation match count = %d", route, len(matches))
		}
		if strings.Count(html, `aria-current="page"`) != 1 {
			t.Fatalf("%s: expected one current navigation item", route)
		}
	}
}

func TestOnlyRenderedMutationFormsContainCurrentSessionCSRFToken(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/", "/issues", "/configuration"} {
		html := renderedGET(t, path)
		forms := formPattern.FindAllString(html, -1)
		if len(forms) == 0 {
			t.Fatalf("%s: expected a rendered form", path)
		}
		for index, form := range forms {
			want := `<input type="hidden" name="csrf_token" value="` + testCSRFToken + `">`
			csrfCount := strings.Count(form, want)
			if strings.Contains(form, `method="post"`) && csrfCount != 1 {
				t.Fatalf("%s mutation form %d: CSRF field count = %d", path, index, csrfCount)
			}
			if strings.Contains(form, `method="get"`) && csrfCount != 0 {
				t.Fatalf("%s GET form %d leaked a CSRF field", path, index)
			}
		}
	}
}

func TestRenderedShellUsesTextualEmptyStatesAndIssueSectionOrder(t *testing.T) {
	t.Parallel()
	issues := renderedGET(t, "/issues")
	assertContains(t, issues, "No tracker work candidates match these filters.")
	if strings.Contains(issues, "<table") || strings.Contains(issues, `<ul class="issue-list`) {
		t.Fatal("empty issues page rendered empty duplicate collections")
	}
	issue := renderedGET(t, "/issues/SYM-123")
	metadata := strings.Index(issue, "Issue metadata")
	eligibility := strings.Index(issue, "Eligibility")
	operator := strings.Index(issue, "Operator requests")
	run := strings.Index(issue, "Current run")
	retry := strings.Index(issue, "Retry history")
	events := strings.Index(issue, "No issue-specific activity has been recorded.")
	logs := strings.Index(issue, `<h2 id="issue-logs-heading">Logs</h2>`)
	if metadata == -1 || eligibility == -1 || operator == -1 || run == -1 || retry == -1 || events == -1 || logs == -1 || !(metadata < eligibility && eligibility < operator && operator < run && run < retry && retry < events && events < logs) {
		t.Fatalf("issue detail sections are missing or out of contract order")
	}
	assertContains(t, renderedGET(t, "/activity"), "No activity has been recorded.")
	assertContains(t, renderedGET(t, "/logs"), "No log entries are available for these filters.")
}

func TestRenderedPagesHaveNoInlineEventHandlersOrNestedInteractiveControls(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/", "/issues", "/issues/SYM-123", "/activity", "/configuration", "/logs"} {
		html := renderedGET(t, path)
		if inlineHandlerPattern.MatchString(html) {
			t.Fatalf("%s: rendered inline event handler", path)
		}
		if hasNestedInteractiveControl(html) {
			t.Fatalf("%s: rendered nested interactive controls", path)
		}
	}
}

func TestProtectedPageErrorsRenderSafeSemanticDocuments(t *testing.T) {
	handler, err := NewPageHandler()
	if err != nil {
		t.Fatal(err)
	}
	server := startTestServerWithErrorResponder(t, bootstrapFromValue("rendered-error-capability"), handler, handler)
	plainURL := strings.Split(server.bound.URL, "?")[0]

	missing := request(t, server.client, http.MethodGet, plainURL, nil, nil)
	assertRenderedError(t, missing, http.StatusUnauthorized, "Authorization required — Symphony", "Authorization required", "Return to the terminal and open the newest Symphony launch URL.")

	validCookie := exchange(t, server)
	invalidCookie := &http.Cookie{Name: sessionCookieName, Value: "invalid-session-canary", Path: "/"}
	invalid := authenticatedRequest(t, server, invalidCookie, http.MethodGet, "/", nil, nil)
	assertRenderedError(t, invalid, http.StatusUnauthorized, "Authorization required — Symphony", "Authorization required", "Return to the terminal and open the newest Symphony launch URL.")

	missingPage := authenticatedRequest(t, server, validCookie, http.MethodGet, "/not-a-page", nil, nil)
	assertRenderedError(t, missingPage, http.StatusNotFound, "Page not found — Symphony", "Page not found", "Use the primary navigation to choose an available page.")
}

func TestPageHandlerRendersMethodErrorsSemantically(t *testing.T) {
	handler, err := NewPageHandler()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/configuration", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	assertRenderedError(t, recorder.Result(), http.StatusMethodNotAllowed, "Method not allowed — Symphony", "Method not allowed", "Return to the previous page and use the available controls.")
}

func TestInvalidHostRemainsSecurityFirstWithRenderedErrorResponder(t *testing.T) {
	handler, err := NewPageHandler()
	if err != nil {
		t.Fatal(err)
	}
	server := startTestServerWithErrorResponder(t, bootstrapFromValue("invalid-host-renderer-capability"), handler, handler)
	requestURL := strings.Split(server.bound.URL, "?")[0]
	req, err := http.NewRequest(http.MethodGet, requestURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.invalid:" + strconv.Itoa(server.bound.Port)
	response, err := server.client.Do(req)
	if err != nil {
		t.Fatal("perform invalid-host request")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("invalid Host response status/content type = %d/%q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || !strings.Contains(string(body), "Request could not be completed") {
		t.Fatal("invalid Host did not render the safe semantic HTML error")
	}
}

func TestUnauthenticatedStaticStylesRemainNarrowAndProtected(t *testing.T) {
	handler, err := NewPageHandler()
	if err != nil {
		t.Fatal(err)
	}
	server := startTestServerWithErrorResponder(t, bootstrapFromValue("static-style-capability"), handler, handler)
	plainURL := strings.Split(server.bound.URL, "?")[0]

	stylesheet := request(t, server.client, http.MethodGet, plainURL+"static/app.css", nil, nil)
	defer stylesheet.Body.Close()
	if stylesheet.StatusCode != http.StatusOK || !strings.HasPrefix(stylesheet.Header.Get("Content-Type"), "text/css") {
		t.Fatalf("unauthenticated stylesheet status/content type = %d/%q", stylesheet.StatusCode, stylesheet.Header.Get("Content-Type"))
	}
	if stylesheet.Header.Get("Content-Security-Policy") != contentSecurityPolicy {
		t.Fatal("unauthenticated stylesheet omitted the standard security policy")
	}

	mutation := request(t, server.client, http.MethodPost, plainURL+"static/app.css", nil, nil)
	defer mutation.Body.Close()
	if mutation.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(mutation.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("unauthenticated static mutation status/content type = %d/%q", mutation.StatusCode, mutation.Header.Get("Content-Type"))
	}
}

func TestUnauthenticatedStaticRequestsRequireCanonicalRawTargets(t *testing.T) {
	pageHandler, err := NewPageHandler()
	if err != nil {
		t.Fatal(err)
	}
	recorder := &handlerCallRecorder{handler: pageHandler}
	server := startTestServerWithErrorResponder(t, bootstrapFromValue("canonical-static-capability"), recorder, pageHandler)

	rejected := []struct {
		name   string
		target string
	}{
		{name: "encoded parent segment", target: "/static/%2e%2e/templates/base.html"},
		{name: "encoded slash after parent", target: "/static/..%2ftemplates/base.html"},
		{name: "uppercase encoded slash after parent", target: "/static/..%2Ftemplates/base.html"},
		{name: "encoded parent and slash", target: "/static/%2e%2e%2ftemplates/base.html"},
		{name: "encoded backslash after parent", target: "/static/%2e%2e%5ctemplates/base.html"},
		{name: "uppercase encoded backslash after parent", target: "/static/%2E%2E%5Ctemplates/base.html"},
		{name: "encoded leading backslash", target: "/static/%5capp.css"},
		{name: "encoded subtree slash", target: "/static%2fapp.css"},
		{name: "uppercase encoded subtree slash", target: "/static%2Fapp.css"},
		{name: "encoded asset dot", target: "/static/app%2ecss"},
		{name: "double encoded slash", target: "/static/app%252f.css"},
		{name: "literal parent segment", target: "/static/../templates/base.html"},
		{name: "literal current segment", target: "/static/./app.css"},
		{name: "cleaning changes path", target: "/static/assets/../app.css"},
		{name: "repeated slash", target: "/static//app.css"},
		{name: "static root", target: "/static/"},
		{name: "encoded current directory", target: "/static/%2e/"},
		{name: "encoded parent directory", target: "/static/%2e%2e/"},
	}
	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			response, body := rawLoopbackRequest(t, server.bound.Port, http.MethodGet, tt.target)
			if response.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") {
				t.Fatalf("raw target status/content type = %d/%q, body category = %s; want semantic 401", response.StatusCode, response.Header.Get("Content-Type"), staticResponseBodyCategory(body))
			}
			if response.Header.Get("Content-Security-Policy") != contentSecurityPolicy {
				t.Fatal("rejected raw target omitted the standard security policy")
			}
			if !strings.Contains(string(body), "Authorization required") {
				t.Fatalf("rejected raw target body category = %s, want semantic authorization document", staticResponseBodyCategory(body))
			}
		})
	}
	if calls := recorder.calls.Load(); calls != 0 {
		t.Fatalf("noncanonical static targets dispatched the application handler %d times", calls)
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run("canonical "+method, func(t *testing.T) {
			response, body := rawLoopbackRequest(t, server.bound.Port, method, "/static/app.css")
			if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/css") {
				t.Fatalf("canonical stylesheet status/content type = %d/%q", response.StatusCode, response.Header.Get("Content-Type"))
			}
			if response.Header.Get("Content-Security-Policy") != contentSecurityPolicy {
				t.Fatal("canonical stylesheet omitted the standard security policy")
			}
			if method == http.MethodGet && !strings.Contains(string(body), ":root") {
				t.Fatal("canonical stylesheet GET omitted CSS content")
			}
			if method == http.MethodHead && len(body) != 0 {
				t.Fatal("canonical stylesheet HEAD returned a response body")
			}
		})
	}
	if calls := recorder.calls.Load(); calls != 2 {
		t.Fatalf("canonical static requests dispatched application handler %d times, want 2", calls)
	}
}

func TestStaticFileSystemIsConfinedToTheEmbeddedStaticSubtree(t *testing.T) {
	staticFiles, err := newStaticFileSystem()
	if err != nil {
		t.Fatal(err)
	}
	stylesheet, err := fs.ReadFile(staticFiles, "app.css")
	if err != nil || !strings.Contains(string(stylesheet), ":root") {
		t.Fatal("confined static file system omitted app.css")
	}
	for _, name := range []string{"../templates/base.html", "templates/base.html", "../embed.go"} {
		if contents, err := fs.ReadFile(staticFiles, name); err == nil {
			t.Fatalf("confined static file system exposed %q with %d bytes", name, len(contents))
		}
	}
}

type handlerCallRecorder struct {
	handler http.Handler
	calls   atomic.Int64
}

func (r *handlerCallRecorder) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	r.calls.Add(1)
	r.handler.ServeHTTP(w, request)
}

func rawLoopbackRequest(t *testing.T, port int, method, target string) (*http.Response, []byte) {
	t.Helper()
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	connection, err := net.Dial("tcp4", address)
	if err != nil {
		t.Fatal("dial raw loopback request")
	}
	defer connection.Close()
	if _, err := io.WriteString(connection, method+" "+target+" HTTP/1.1\r\nHost: "+address+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal("write raw loopback request")
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: method})
	if err != nil {
		t.Fatal("read raw loopback response")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal("read raw loopback response body")
	}
	return response, body
}

func staticResponseBodyCategory(body []byte) string {
	text := string(body)
	switch {
	case strings.Contains(text, "{{define"):
		return "embedded template source"
	case strings.Contains(text, "Directory listing"):
		return "embedded directory listing"
	case strings.Contains(text, "<pre>") && strings.Contains(text, "href="):
		return "embedded directory listing"
	case strings.Contains(text, "Authorization required"):
		return "semantic authorization document"
	default:
		return "other response"
	}
}

func TestNonemptyFlashUsesTheSinglePersistentStatusRegion(t *testing.T) {
	renderer, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	err = renderer.Render(recorder, "overview", Page{
		Title:   "Overview — Symphony",
		Route:   "/",
		Heading: "Overview",
		Flash:   "Configuration saved.",
		Content: overviewContent{TrackerScope: "Tracker scope not selected"},
	})
	if err != nil {
		t.Fatal(err)
	}
	html := recorder.Body.String()
	if count := strings.Count(html, `role="status"`); count != 1 {
		t.Fatalf("rendered flash status region count = %d, want 1", count)
	}
	if !strings.Contains(html, `role="status" aria-live="polite" aria-atomic="true">Configuration saved.`) {
		t.Fatal("single status region did not contain the flash message")
	}
	if strings.Contains(html, "Scheduler configuration is ready.") {
		t.Fatal("fallback route status remained alongside the flash message")
	}
}

func assertRenderedError(t *testing.T, response *http.Response, status int, title, heading, instruction string) {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal("read rendered error")
	}
	html := string(body)
	if response.StatusCode != status {
		t.Fatalf("rendered error status = %d, want %d", response.StatusCode, status)
	}
	if response.Header.Get("Content-Security-Policy") != contentSecurityPolicy || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") {
		t.Fatal("rendered error omitted standard security headers or HTML content type")
	}
	for _, want := range []string{"<title>" + title + "</title>", `<main id="main-content"`, "<h1>" + heading + "</h1>", instruction} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered error omitted required semantic content category")
		}
	}
	for _, forbidden := range []string{"rendered-error-capability", "invalid-session-canary", "access_token", "symphony_session", "csrf_token"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("rendered error exposed forbidden %s category", forbidden)
		}
	}
}

var (
	focusableTagPattern  = regexp.MustCompile(`(?i)<(?:a\s[^>]*href|button\b|input\b|select\b|textarea\b)`)
	formPattern          = regexp.MustCompile(`(?s)<form\b.*?</form>`)
	inlineHandlerPattern = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
)

func hasNestedInteractiveControl(html string) bool {
	lower := strings.ToLower(html)
	for _, element := range []string{"a", "button"} {
		opening := "<" + element + " "
		closing := "</" + element + ">"
		for remainder := lower; ; {
			start := strings.Index(remainder, opening)
			if start == -1 {
				break
			}
			end := strings.Index(remainder[start:], closing)
			if end == -1 {
				return true
			}
			content := remainder[start : start+end]
			if focusableTagPattern.MatchString(content[len(opening):]) {
				return true
			}
			remainder = remainder[start+end+len(closing):]
		}
	}
	return false
}

func renderedGET(t *testing.T, path string) string {
	t.Helper()
	runtime := &pageRuntimeFake{details: map[string]domain.IssueDetail{
		"SYM-123": {
			Issue:    domain.Issue{ID: "sym-123", Identifier: "SYM-123", Title: "Example issue", State: "Open", Labels: []string{}, BlockedBy: []domain.BlockerRef{}},
			Routable: true, RoutingReasons: []string{},
		},
	}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request = request.WithContext(context.WithValue(request.Context(), csrfContextKey{}, testCSRFToken))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d", path, recorder.Code)
	}
	return recorder.Body.String()
}

func assertContains(t *testing.T, value, want string) {
	t.Helper()
	if !strings.Contains(value, want) {
		t.Fatalf("rendered page does not contain %q", want)
	}
}

func assertContainsOnce(t *testing.T, value, want string) {
	t.Helper()
	if count := strings.Count(value, want); count != 1 {
		t.Fatalf("rendered page contains %q %d times", want, count)
	}
}
