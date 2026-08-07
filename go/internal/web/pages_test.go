package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
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
