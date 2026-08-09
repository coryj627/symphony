package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
)

func TestIssuesHTMLUsesOneFilteredRowSliceForEquivalentTableAndList(t *testing.T) {
	now := time.Date(2026, 8, 8, 15, 0, 0, 0, time.UTC)
	unsafeURL := "javascript:alert(1)"
	runtime := &pageRuntimeFake{snapshot: domain.Snapshot{
		GeneratedAt: now,
		Candidates: []domain.CandidateRow{
			{Issue: domain.Issue{ID: "2", Identifier: "TEAM-2", Title: "<script>bad()</script>", State: "Open", URL: &unsafeURL, Labels: []string{"backend"}}, Routable: false, RoutingReasons: []string{"provider_not_dispatchable"}},
			{Issue: domain.Issue{ID: "1", Identifier: "TEAM-1", Title: "Ready", State: "Open", Labels: []string{"frontend"}}, Routable: true, RoutingReasons: []string{}},
		},
		Tracker: domain.TrackerStatus{Stale: true},
	}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	recorder := serveDirect(t, handler, http.MethodGet, "/issues?state=open&eligibility=all&sort=identifier", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("issues status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	html := recorder.Body.String()
	for _, want := range []string{
		`<caption>Tracker work candidates</caption>`, `<th scope="col">Title</th>`, `<th scope="row"><a href="/issues/TEAM-1?sort=identifier&amp;state=Open">TEAM-1</a></th>`,
		`<ul class="issue-list responsive-narrow"`, "Needs attention", "Routable", "last known", "Tracker marked this issue unavailable for dispatch.",
		`value="Open" selected`, `value="all" selected`, `value="identifier" selected`, `&lt;script&gt;bad()&lt;/script&gt;`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("issues page omitted %q", want)
		}
	}
	for _, identifier := range []string{"TEAM-1", "TEAM-2"} {
		if count := strings.Count(html, ">"+identifier+"</a>"); count != 2 {
			t.Fatalf("%s table/list link count = %d, want 2", identifier, count)
		}
	}
	if strings.Index(html, ">TEAM-1</a>") > strings.Index(html, ">TEAM-2</a>") {
		t.Fatal("identifier display sort was not deterministic")
	}
	if strings.Contains(html, `name="csrf_token"`) || strings.Contains(html, "javascript:alert") || strings.Contains(html, "<script>bad()") {
		t.Fatal("issues GET form or provider content was unsafe")
	}
}

func TestIssuesAndStateAPIShareFirstValueFilterOrderWithoutMutatingScheduling(t *testing.T) {
	candidates := []domain.CandidateRow{
		{Issue: domain.Issue{ID: "b", Identifier: "B-2", Title: "Second open", State: "Open", Labels: []string{}}, Routable: true, RoutingReasons: []string{}},
		{Issue: domain.Issue{ID: "a", Identifier: "A-1", Title: "First open", State: "Open", Labels: []string{}}, Routable: false, RoutingReasons: []string{"missing_required_label"}},
		{Issue: domain.Issue{ID: "c", Identifier: "C-3", Title: "Closed", State: "Closed", Labels: []string{}}, Routable: true, RoutingReasons: []string{}},
	}
	runtime := &pageRuntimeFake{snapshot: domain.Snapshot{GeneratedAt: time.Now().UTC(), Candidates: candidates, EventCursor: domain.EventCursor{Epoch: "epoch-a"}}, recent: domain.EventPage{Events: []domain.Event{}, LatestCursor: domain.EventCursor{Epoch: "epoch-a"}}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	target := "?state=open&state=closed&eligibility=all&sort=identifier&query=open"
	issues := serveDirect(t, handler, http.MethodGet, "/issues"+target, "", nil).Body.String()
	stateRecorder := serveDirect(t, handler, http.MethodGet, "/api/v1/state"+target, "", nil)
	var state struct {
		Candidates []struct {
			Identifier string `json:"identifier"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(stateRecorder.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Candidates) != 2 || state.Candidates[0].Identifier != "A-1" || state.Candidates[1].Identifier != "B-2" {
		t.Fatalf("state filtered identifiers = %#v", state.Candidates)
	}
	first, second := strings.Index(issues, ">A-1</a>"), strings.Index(issues, ">B-2</a>")
	if first == -1 || second == -1 || first > second || strings.Contains(issues, ">C-3</a>") {
		t.Fatal("HTML/API filter sequence diverged")
	}
	scheduling := serveDirect(t, handler, http.MethodGet, "/issues", "", nil).Body.String()
	if strings.Index(scheduling, ">B-2</a>") > strings.Index(scheduling, ">A-1</a>") {
		t.Fatal("identifier sort mutated the backing scheduling order")
	}
}

func TestOverviewShowsSafeScopeCountsSchedulerAndPersistentFailures(t *testing.T) {
	now := time.Date(2026, 8, 8, 15, 30, 0, 0, time.UTC)
	runtime := &pageRuntimeFake{snapshot: domain.Snapshot{
		GeneratedAt: now,
		Candidates: []domain.CandidateRow{
			{Issue: domain.Issue{ID: "one", Identifier: "ONE", Title: "One", State: "Open", Labels: []string{}}, Routable: true, RoutingReasons: []string{}},
			{Issue: domain.Issue{ID: "two", Identifier: "TWO", Title: "Two", State: "Open", Labels: []string{}}, Routable: false, RoutingReasons: []string{"missing_required_label"}},
		},
		Running: []domain.RunningRow{{IssueIdentifier: "ONE"}}, Retrying: []domain.RetryRow{{IssueIdentifier: "TWO"}}, Requests: []domain.OperatorRequest{{ID: "request"}},
		Scheduler: domain.SchedulerStatus{Available: false, State: "unavailable", Message: "Scheduler unavailable"},
		Config:    domain.ConfigStatus{State: "invalid", ErrorCode: "invalid_workflow", Message: "Configuration needs attention.", ChangedAt: now},
		Tracker:   domain.TrackerStatus{Kind: "github", Scope: "safe/example", State: "failed", Stale: true, ErrorCode: "tracker_transport", Message: "Tracker is unavailable.", LastAttemptAt: &now},
	}}
	handler := newTestPageHandler(t, PageOptions{Mode: "run", Queries: runtime, Commands: runtime})
	recorder := serveDirect(t, handler, http.MethodGet, "/", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("overview status = %d", recorder.Code)
	}
	html := recorder.Body.String()
	for _, want := range []string{"Tracker scope", "safe/example", "Unavailable", "2", "1", "Configuration needs attention.", "Tracker is unavailable.", "last known", "Refresh tracker work"} {
		if !strings.Contains(html, want) {
			t.Fatalf("overview omitted %q", want)
		}
	}
	assertRenderedTime(t, html, now)
	if strings.Contains(html, "Start scheduler") || strings.Contains(html, "Stop scheduler") {
		t.Fatal("overview exposed Phase 3 scheduler controls")
	}
}

func TestOverviewLabelsGitHubAndLinearScopesProviderNeutrally(t *testing.T) {
	for _, tracker := range []domain.TrackerStatus{
		{Kind: "github", Scope: "github:owner/repository"},
		{Kind: "linear", Scope: "linear:project-slug"},
	} {
		runtime := &pageRuntimeFake{snapshot: domain.Snapshot{Tracker: tracker}}
		handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
		html := serveDirect(t, handler, http.MethodGet, "/", "", nil).Body.String()
		want := `<dt>Tracker scope</dt><dd>` + tracker.Scope + `</dd>`
		if !strings.Contains(html, want) || strings.Contains(html, `<dt>Repository</dt>`) {
			t.Fatalf("%s overview scope label was provider-specific: %s", tracker.Kind, html)
		}
	}
}

func TestSafeStatusMessageSeparatesHealthyTextFromUnknownErrors(t *testing.T) {
	const fallback = "Tracker status needs attention."
	if got := safeStatusMessage("Tracker is ready.", false, "", fallback); got != "Tracker is ready." {
		t.Fatalf("healthy informational status = %q", got)
	}
	if got := safeStatusMessage("", true, "", fallback); got != fallback {
		t.Fatalf("unknown source error status = %q", got)
	}
}

func TestActivityUsesBoundedTailResetTextAndSafeAllowlistedSummaries(t *testing.T) {
	now := time.Date(2026, 8, 8, 16, 0, 0, 0, time.UTC)
	runtime := &pageRuntimeFake{recent: domain.EventPage{Reset: true, LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 3}, Events: []domain.Event{
		{Epoch: "epoch-a", Sequence: 1, Type: "queue.refreshed", At: now.Add(-time.Minute), Data: map[string]any{"secret": "event-secret-canary"}},
		{Epoch: "epoch-a", Sequence: 2, Type: "unknown-type-canary", At: now, Data: map[string]any{"message": "event-message-canary"}},
	}}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	recorder := serveDirect(t, handler, http.MethodGet, "/activity", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("activity status = %d", recorder.Code)
	}
	if runtime.callOrderString() != "recent:100" {
		t.Fatalf("activity call order = %q", runtime.callOrderString())
	}
	html := recorder.Body.String()
	reset := "Earlier activity is unavailable because the in-memory history was restarted or trimmed."
	if strings.Count(html, reset) != 1 || !strings.Contains(html, "Tracker work refreshed.") || !strings.Contains(html, "Activity occurred.") || !strings.Contains(html, `<ol class="timeline">`) || !strings.Contains(html, `<time datetime="`) {
		t.Fatal("activity semantics or summaries are incomplete")
	}
	for _, forbidden := range []string{"event-secret-canary", "event-message-canary", "unknown-type-canary", `role="log"`, `aria-live=`} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("activity exposed %q", forbidden)
		}
	}
	assertRenderedTime(t, html, now.Add(-time.Minute))
	assertRenderedTime(t, html, now)
}

func TestIssueHTMLRendersEscapedMetadataUnsafeURLAsTextAndNeverGlobalActivity(t *testing.T) {
	unsafeURL := "https://user:password@example.invalid/issue?secret=1#fragment"
	description := "<img src=x onerror=bad()>"
	label := "<script>label()</script>"
	runtime := &pageRuntimeFake{details: map[string]domain.IssueDetail{
		"TEAM-9": {Issue: domain.Issue{ID: "nine", Identifier: "TEAM-9", Title: "<script>title()</script>", Description: &description, State: "Open", URL: &unsafeURL, Labels: []string{label}, BlockedBy: []domain.BlockerRef{}}, Routable: false, RoutingReasons: []string{"unknown-reason-canary"}},
	}}
	logs := &logQueryFake{page: observability.LogPage{Records: []observability.LogRecord{}}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime, Logs: logs})
	recorder := serveDirect(t, handler, http.MethodGet, "/issues/TEAM-9", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("issue status = %d", recorder.Code)
	}
	html := recorder.Body.String()
	for _, want := range []string{"&lt;script&gt;title()&lt;/script&gt;", "&lt;img src=x onerror=bad()&gt;", "&lt;script&gt;label()&lt;/script&gt;", "Routing details are unavailable.", "Issue-specific activity is not available in this phase."} {
		if !strings.Contains(html, want) {
			t.Fatalf("issue page omitted %q", want)
		}
	}
	if strings.Contains(html, "user:password") || strings.Contains(html, "secret=1") || strings.Contains(html, "unknown-reason-canary") || strings.Contains(runtime.callOrderString(), "recent:") {
		t.Fatal("issue page linked unsafe URL, reflected raw reason, or queried global events")
	}
}

func TestIssueHTMLUsesAllowlistedRunRetryAndMatchingOperatorRequests(t *testing.T) {
	now := time.Date(2026, 8, 8, 17, 0, 0, 0, time.UTC)
	runtime := &pageRuntimeFake{
		details: map[string]domain.IssueDetail{
			"TEAM-7": {
				Issue: domain.Issue{ID: "seven", Identifier: "TEAM-7", Title: "Active issue", State: "Open", Labels: []string{}, BlockedBy: []domain.BlockerRef{}}, Routable: true, RoutingReasons: []string{},
				Running: &domain.RunningRow{IssueID: "seven", IssueIdentifier: "TEAM-7", SessionID: "workspace-session-canary", State: "running", TurnCount: 3, LastEvent: "turn_completed", LastMessage: "Safe progress", StartedAt: now, LastEventAt: now, Tokens: domain.TokenTotals{InputTokens: 5, OutputTokens: 6, TotalTokens: 11, SecondsRunning: 99}},
				Retry:   &domain.RetryRow{IssueID: "seven", IssueIdentifier: "TEAM-7", Attempt: 2, DueAt: now, Error: "credential-retry-detail-canary"},
			},
		},
		snapshot: domain.Snapshot{Requests: []domain.OperatorRequest{
			{ID: "matching", IssueIdentifier: "TEAM-7", Kind: "choice", Title: "Approve next step", Summary: "Choose how to proceed.", Choices: []domain.OperatorChoice{}, Questions: []domain.OperatorQuestion{}},
			{ID: "other", IssueIdentifier: "TEAM-8", Kind: "choice", Title: "other-request-canary", Choices: []domain.OperatorChoice{}, Questions: []domain.OperatorQuestion{}},
		}},
	}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime, Logs: &logQueryFake{page: observability.LogPage{Records: []observability.LogRecord{}}}})
	recorder := serveDirect(t, handler, http.MethodGet, "/issues/TEAM-7", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("issue status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	html := recorder.Body.String()
	for _, want := range []string{"Approve next step", "Choose how to proceed.", "running", "3", "turn_completed", "Safe progress", "A retry is scheduled."} {
		if !strings.Contains(html, want) {
			t.Fatalf("issue live detail omitted %q", want)
		}
	}
	if count := strings.Count(html, `<time datetime="`+now.UTC().Format(time.RFC3339)+`">`+now.Local().Format("Jan 2, 2006 3:04:05 PM MST")+`</time>`); count != 2 {
		t.Fatalf("issue live detail rendered valid local run/retry time %d times, want 2", count)
	}
	for _, forbidden := range []string{"workspace-session-canary", "credential-retry-detail-canary", "other-request-canary", "seconds_running"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("issue live detail exposed %q", forbidden)
		}
	}
}

func assertRenderedTime(t *testing.T, html string, value time.Time) {
	t.Helper()
	want := `<time datetime="` + value.UTC().Format(time.RFC3339) + `">` + value.Local().Format("Jan 2, 2006 3:04:05 PM MST") + `</time>`
	if !strings.Contains(html, want) {
		t.Fatalf("rendered page omitted semantic local time %q", want)
	}
	if strings.Contains(html, value.String()) {
		t.Fatalf("rendered page exposed Go time formatting %q", value.String())
	}
}

func TestCandidateFilteringAppliesBeforeCapAndInvalidControlsFallBack(t *testing.T) {
	source := make([]domain.CandidateRow, 0, 205)
	for index := range 205 {
		identifier := "ITEM-" + leftPad(index, 3)
		title := "ordinary"
		if index >= 200 {
			title = "late needle"
		}
		source = append(source, domain.CandidateRow{Issue: domain.Issue{ID: identifier, Identifier: identifier, Title: title, State: "Open", Labels: []string{}}, Routable: true, RoutingReasons: []string{}})
	}
	filtered, filters := filteredCandidateResponses(source, false, url.Values{"query": {"needle"}})
	if len(filtered) != 5 || filters.Query != "needle" || filtered[0].Identifier != "ITEM-200" {
		t.Fatalf("filter-before-cap result = %d/%#v/%#v", len(filtered), filters, filtered)
	}
	invalid, controls := filteredCandidateResponses(source, false, url.Values{
		"query": {strings.Repeat("x", 257)}, "state": {"Open\ncanary"}, "eligibility": {"unknown"}, "sort": {"priority"},
	})
	if len(invalid) != 200 || controls != (issueFilters{Eligibility: "all", Sort: "scheduling"}) {
		t.Fatalf("invalid filter fallback = %d/%#v", len(invalid), controls)
	}
}

func TestIssueFiltersTrimBeforeApplyingTheByteBound(t *testing.T) {
	legal := strings.Repeat("q", 250)
	source := []domain.CandidateRow{
		{Issue: domain.Issue{ID: "match", Identifier: "MATCH", Title: legal, State: "Open", Labels: []string{}}, Routable: true, RoutingReasons: []string{}},
		{Issue: domain.Issue{ID: "other", Identifier: "OTHER", Title: "other", State: "Open", Labels: []string{}}, Routable: true, RoutingReasons: []string{}},
	}
	filtered, filters := filteredCandidateResponses(source, false, url.Values{"query": {strings.Repeat(" ", 7) + legal}})
	if filters.Query != legal || len(filtered) != 1 || filtered[0].Identifier != "MATCH" {
		t.Fatalf("trim-before-bound filter = %#v/%#v", filters, filtered)
	}
}

func TestProductionPageDependenciesIgnoreAndNeverReflectE2EScenario(t *testing.T) {
	runtime := &pageRuntimeFake{snapshot: domain.Snapshot{Candidates: []domain.CandidateRow{{
		Issue: domain.Issue{ID: "base", Identifier: "BASE-1", Title: "Base runtime candidate", State: "Open", Labels: []string{}}, Routable: true, RoutingReasons: []string{},
	}}}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	recorder := serveDirect(t, handler, http.MethodGet, "/issues?__e2e_scenario=malicious-text", "", nil)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "BASE-1") || runtime.callOrderString() != "snapshot" {
		t.Fatalf("production dependency selection = %d/%q body=%s", recorder.Code, runtime.callOrderString(), recorder.Body.String())
	}
	for _, forbidden := range []string{"__e2e_scenario", "malicious-text"} {
		if strings.Contains(recorder.Body.String(), forbidden) {
			t.Fatalf("production page reflected fixture selector %q", forbidden)
		}
	}
}

func leftPad(value, width int) string {
	result := string(rune('0' + value%10))
	for value /= 10; value > 0; value /= 10 {
		result = string(rune('0'+value%10)) + result
	}
	for len(result) < width {
		result = "0" + result
	}
	return result
}
