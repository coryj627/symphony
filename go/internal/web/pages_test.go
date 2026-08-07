package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const testCSRFToken = "test-csrf-value-never-log"

func TestEveryPageHasUniqueTitleMainH1AndStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path    string
		title   string
		heading string
		status  string
	}{
		{"/", "Overview — Symphony", "Overview", "Scheduler configuration is ready."},
		{"/issues", "Issues — Symphony", "Issues", "No issues are available."},
		{"/issues/SYM-123", "SYM-123 — Symphony", "Issue SYM-123", "Issue details are not available yet."},
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
			assertContains(t, html, `role="status"`)
			assertContains(t, html, tc.status)
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

func TestRenderedFormsContainCurrentSessionCSRFToken(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/", "/issues", "/configuration"} {
		html := renderedGET(t, path)
		forms := formPattern.FindAllString(html, -1)
		if len(forms) == 0 {
			t.Fatalf("%s: expected a rendered form", path)
		}
		for index, form := range forms {
			want := `<input type="hidden" name="csrf_token" value="` + testCSRFToken + `">`
			if strings.Count(form, want) != 1 {
				t.Fatalf("%s form %d: CSRF field count = %d", path, index, strings.Count(form, want))
			}
		}
	}
}

func TestRenderedShellUsesSemanticListsTableAndTextualEmptyStates(t *testing.T) {
	t.Parallel()
	issues := renderedGET(t, "/issues")
	for _, want := range []string{
		`<table>`, `<caption>Issues available to Symphony</caption>`, `<th scope="col">Identifier</th>`,
		`<ul class="issue-list" aria-label="Issues available to Symphony">`, "No issues are available.",
	} {
		assertContains(t, issues, want)
	}
	issue := renderedGET(t, "/issues/SYM-123")
	operator := strings.Index(issue, "Operator requests")
	events := strings.Index(issue, "Event and log stream")
	metadata := strings.Index(issue, "Issue metadata")
	if operator == -1 || events == -1 || metadata == -1 || !(operator < events && events < metadata) {
		t.Fatalf("issue detail sections are missing or out of contract order")
	}
	for _, path := range []string{"/activity", "/logs"} {
		assertContains(t, renderedGET(t, path), "<ol")
	}
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
	if response.StatusCode != http.StatusBadRequest || strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("invalid Host response status/content type = %d/%q", response.StatusCode, response.Header.Get("Content-Type"))
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
		Content: overviewContent{Repository: "Repository not selected"},
	})
	if err != nil {
		t.Fatal(err)
	}
	html := recorder.Body.String()
	if count := strings.Count(html, `role="status"`); count != 1 {
		t.Fatalf("rendered flash status region count = %d, want 1", count)
	}
	if !strings.Contains(html, `role="status" aria-live="polite">Configuration saved.`) {
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
	handler, err := NewPageHandler()
	if err != nil {
		t.Fatal(err)
	}
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
