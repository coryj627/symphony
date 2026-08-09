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
	events := strings.Index(issue, "Issue-specific activity is not available in this phase.")
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
