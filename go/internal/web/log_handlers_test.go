package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
)

func TestLogsPageBoundsCanonicalizesAndPreservesFiltersWithoutLiveSemantics(t *testing.T) {
	now := time.Date(2026, 8, 8, 14, 15, 16, 0, time.UTC)
	logs := &logQueryFake{page: observability.LogPage{
		Records: []observability.LogRecord{
			{Sequence: 10, Time: now, Level: "WARN", Message: "<script>malicious()</script> needle", Fields: map[string]any{"detail": "<img src=x onerror=bad()>"}},
			{Sequence: 9, Time: now.Add(-time.Second), Level: "WARN", Message: strings.Repeat("界", 5000), Fields: map[string]any{"nested": map[string]any{"value": strings.Repeat("z", 17000)}}},
		},
		NextBefore: 9,
		HasMore:    true,
		Degraded:   true,
	}}
	handler := newTestPageHandler(t, PageOptions{Logs: logs})
	recorder := serveDirect(t, handler, http.MethodGet, "/logs?query=needle&query=ignored&level=warn&level=ERROR&before=9&before=1", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("logs status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if logs.calls.Load() != 1 || logs.query.Search != "needle" || logs.query.Level != "WARN" || logs.query.Before != 9 || logs.query.Limit != 100 {
		t.Fatalf("log query = %#v calls=%d", logs.query, logs.calls.Load())
	}
	html := recorder.Body.String()
	for _, want := range []string{
		"Symphony logging is degraded", `<table>`, `<caption>Application log entries</caption>`, `<ul class="log-list `,
		`<time datetime="2026-08-08T14:15:16Z">`, `role="group"`, `aria-label="Structured fields for log entry 10"`, `tabindex="0"`,
		"Message truncated for display.", "Structured fields truncated for display.",
		`query=needle`, `level=WARN`, `before=9`, "Older log entries", "Newest log entries",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("logs page omitted %q", want)
		}
	}
	for _, forbidden := range []string{"<script>malicious", "<img src=x", `role="log"`, `aria-live=`} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("logs page exposed forbidden markup/semantics %q", forbidden)
		}
	}
}

func TestIssueLogsRecheckExactSanitizedIdentifierAfterSearch(t *testing.T) {
	runtime := &pageRuntimeFake{details: map[string]domain.IssueDetail{
		"TEAM-1": {Issue: domain.Issue{ID: "one", Identifier: "TEAM-1", Title: "One", State: "Open", Labels: []string{}, BlockedBy: []domain.BlockerRef{}}, Routable: true, RoutingReasons: []string{}},
	}}
	logs := &logQueryFake{page: observability.LogPage{Records: []observability.LogRecord{
		{Sequence: 3, Level: "INFO", Message: "exact", Fields: map[string]any{"issue_identifier": "TEAM-1"}},
		{Sequence: 2, Level: "INFO", Message: "substring", Fields: map[string]any{"issue_identifier": "TEAM-10"}},
		{Sequence: 1, Level: "INFO", Message: "unrelated field", Fields: map[string]any{"other": "TEAM-1"}},
	}}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime, Logs: logs})
	recorder := serveDirect(t, handler, http.MethodGet, "/issues/TEAM-1", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("issue status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if logs.query.Search != "TEAM-1" || logs.query.Limit != 100 {
		t.Fatalf("issue log query = %#v", logs.query)
	}
	html := recorder.Body.String()
	if !strings.Contains(html, "exact") || strings.Contains(html, "substring") || strings.Contains(html, "unrelated field") {
		t.Fatalf("issue log isolation failed: %s", html)
	}
}

func TestInvalidLogFiltersFallBackWithoutReflection(t *testing.T) {
	logs := &logQueryFake{page: observability.LogPage{Records: []observability.LogRecord{}}}
	handler := newTestPageHandler(t, PageOptions{Logs: logs})
	malicious := strings.Repeat("x", 257)
	target := "/logs?query=" + malicious + "&level=TRACE&before=-1"
	recorder := serveDirect(t, handler, http.MethodGet, target, "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("logs status = %d", recorder.Code)
	}
	if logs.query.Search != "" || logs.query.Level != "" || logs.query.Before != 0 || logs.query.Limit != 100 {
		t.Fatalf("invalid filters reached log store: %#v", logs.query)
	}
	if strings.Contains(recorder.Body.String(), malicious) || strings.Contains(recorder.Body.String(), "TRACE") {
		t.Fatal("invalid log filters were reflected")
	}
}

func TestLogQueryTrimsBeforeApplyingTheByteBound(t *testing.T) {
	logs := &logQueryFake{page: observability.LogPage{Records: []observability.LogRecord{}}}
	handler := newTestPageHandler(t, PageOptions{Logs: logs})
	legal := strings.Repeat("q", 250)
	target := "/logs?query=" + url.QueryEscape(strings.Repeat(" ", 7)+legal)
	recorder := serveDirect(t, handler, http.MethodGet, target, "", nil)
	if recorder.Code != http.StatusOK || logs.query.Search != legal {
		t.Fatalf("trim-before-bound log query status/search = %d/%q", recorder.Code, logs.query.Search)
	}
}
