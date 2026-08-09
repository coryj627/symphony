//go:build e2e

package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coryj627/symphony/go/internal/app"
	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
)

var e2eScenarioManifest = map[string]struct{}{
	"empty": {}, "populated": {}, "stale-error": {}, "filtered-empty": {},
	"issue-not-found": {}, "malicious-text": {}, "encoded-identifier": {},
	"degraded-log": {}, "long-log": {},
}

func resolvePageDependencies(request *http.Request, _ pageDependencies) (pageDependencies, string, bool) {
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return pageDependencies{}, "", false
	}
	values, present := query["__e2e_scenario"]
	scenario := "empty"
	if present {
		if len(values) != 1 || !validE2EScenarioValue(values[0]) {
			return pageDependencies{}, "", false
		}
		scenario = values[0]
	}
	if _, known := e2eScenarioManifest[scenario]; !known {
		return pageDependencies{}, "", false
	}
	runtime, logs := newE2EPageFixture(scenario)
	return pageDependencies{queries: runtime, commands: runtime, logs: logs}, scenario, true
}

func validE2EScenarioValue(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for index := range len(value) {
		if value[index] > 0x7f {
			return false
		}
	}
	return true
}

type e2ePageRuntime struct {
	snapshot domain.Snapshot
	details  map[string]domain.IssueDetail
	recent   domain.EventPage
}

func (runtime *e2ePageRuntime) Snapshot(ctx context.Context) (domain.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return domain.Snapshot{}, err
	}
	return runtime.snapshot.Clone()
}

func (runtime *e2ePageRuntime) Issue(ctx context.Context, identifier string) (domain.IssueDetail, error) {
	if err := ctx.Err(); err != nil {
		return domain.IssueDetail{}, err
	}
	detail, found := runtime.details[identifier]
	if !found {
		return domain.IssueDetail{}, app.ErrIssueNotFound
	}
	return detail.Clone()
}

func (runtime *e2ePageRuntime) EventsAfter(context.Context, domain.EventCursor) (domain.EventPage, error) {
	return domain.EventPage{Events: []domain.Event{}}, nil
}

func (runtime *e2ePageRuntime) RecentEvents(ctx context.Context, limit int) (domain.EventPage, error) {
	if err := ctx.Err(); err != nil {
		return domain.EventPage{}, err
	}
	events := runtime.recent.Events
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	page := runtime.recent
	page.Events = make([]domain.Event, len(events))
	for index, event := range events {
		page.Events[index] = event
		page.Events[index].Data = make(map[string]any, len(event.Data))
		for key, value := range event.Data {
			page.Events[index].Data[key] = value
		}
	}
	return page, nil
}

func (*e2ePageRuntime) SubscribeEvents(domain.EventCursor) <-chan struct{} {
	ready := make(chan struct{})
	close(ready)
	return ready
}

func (*e2ePageRuntime) Refresh(ctx context.Context) (domain.RefreshReceipt, error) {
	if err := ctx.Err(); err != nil {
		return domain.RefreshReceipt{}, err
	}
	return domain.RefreshReceipt{Queued: true, RequestedAt: e2eNow, Operations: []string{"poll"}}, nil
}

func (*e2ePageRuntime) SetScheduler(context.Context, bool) error { return app.ErrUnavailableInPhase }
func (*e2ePageRuntime) Respond(context.Context, domain.OperatorResponse) error {
	return app.ErrUnavailableInPhase
}

type e2eLogQueries struct {
	page observability.LogPage
}

func (logs *e2eLogQueries) Query(ctx context.Context, query observability.LogQuery) (observability.LogPage, error) {
	if err := ctx.Err(); err != nil {
		return observability.LogPage{}, err
	}
	limit := query.Limit
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	page := observability.LogPage{Degraded: logs.page.Degraded, Records: []observability.LogRecord{}}
	for _, record := range logs.page.Records {
		if query.Before != 0 && record.Sequence >= query.Before {
			continue
		}
		if query.Level != "" && !strings.EqualFold(record.Level, query.Level) {
			continue
		}
		if query.Search != "" && !strings.Contains(strings.ToLower(record.Message+" "+formatE2EFields(record.Fields)), strings.ToLower(query.Search)) {
			continue
		}
		if len(page.Records) == limit {
			page.HasMore = true
			page.NextBefore = page.Records[len(page.Records)-1].Sequence
			break
		}
		page.Records = append(page.Records, cloneE2ELogRecord(record))
	}
	return page, nil
}

func formatE2EFields(fields map[string]any) string {
	var result strings.Builder
	for key, value := range fields {
		result.WriteString(key)
		result.WriteByte(' ')
		if text, ok := value.(string); ok {
			result.WriteString(text)
		}
		result.WriteByte(' ')
	}
	return result.String()
}

func cloneE2ELogRecord(record observability.LogRecord) observability.LogRecord {
	clone := record
	clone.Fields = make(map[string]any, len(record.Fields))
	for key, value := range record.Fields {
		clone.Fields[key] = value
	}
	return clone
}

var e2eNow = time.Date(2026, 8, 8, 16, 0, 0, 0, time.UTC)

func newE2EPageFixture(scenario string) (*e2ePageRuntime, *e2eLogQueries) {
	snapshot := domain.EmptySnapshot()
	snapshot.GeneratedAt = e2eNow
	snapshot.EventCursor = domain.EventCursor{Epoch: "e2e", Sequence: 2}
	snapshot.Scheduler = domain.SchedulerStatus{Available: true, Enabled: false, State: "paused", Message: "Scheduler is paused."}
	snapshot.Config = domain.ConfigStatus{State: "valid", Digest: "e2e", ActiveDigest: "e2e", ChangedAt: e2eNow}
	snapshot.Tracker = domain.TrackerStatus{Kind: "fixture", Scope: "fixture/example", State: "ready", LastAttemptAt: &e2eNow, LastSuccessAt: &e2eNow}
	recent := domain.EventPage{LatestCursor: snapshot.EventCursor, Events: []domain.Event{
		{Epoch: "e2e", Sequence: 1, Type: "configuration.changed", At: e2eNow.Add(-time.Minute), Data: map[string]any{}},
		{Epoch: "e2e", Sequence: 2, Type: "queue.refreshed", At: e2eNow, Data: map[string]any{}},
	}}
	runtime := &e2ePageRuntime{snapshot: snapshot, details: map[string]domain.IssueDetail{}, recent: recent}
	logs := &e2eLogQueries{page: observability.LogPage{Records: []observability.LogRecord{}}}

	safeURL := "https://tracker.example/issues/SYM-123"
	populated := domain.Issue{ID: "sym-123", Identifier: "SYM-123", Title: "Improve keyboard navigation", Description: stringPointer("Verify every workflow without a mouse."), State: "Open", URL: &safeURL, Labels: []string{"accessibility", "symphony"}, BlockedBy: []domain.BlockerRef{}, CreatedAt: &e2eNow, UpdatedAt: &e2eNow}
	add := func(issue domain.Issue, routable bool, reasons []string) {
		runtime.snapshot.Candidates = append(runtime.snapshot.Candidates, domain.CandidateRow{Issue: issue, Routable: routable, RoutingReasons: reasons})
		runtime.details[issue.Identifier] = domain.IssueDetail{Issue: issue, Status: "candidate", Routable: routable, RoutingReasons: reasons}
	}

	switch scenario {
	case "populated":
		add(populated, true, []string{})
		second := domain.Issue{ID: "sym-124", Identifier: "SYM-124", Title: "Waiting for required label", State: "Open", Labels: []string{"backend"}, BlockedBy: []domain.BlockerRef{}}
		add(second, false, []string{"missing_required_label"})
		runtime.snapshot.Running = []domain.RunningRow{{IssueID: "sym-123", IssueIdentifier: "SYM-123", State: "running", StartedAt: e2eNow, LastEventAt: e2eNow, Tokens: domain.TokenTotals{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}}}
		runtime.snapshot.Requests = []domain.OperatorRequest{{ID: "request-1", IssueIdentifier: "SYM-123", Kind: "choice", Title: "Choose next step", Summary: "Operator input is needed.", Choices: []domain.OperatorChoice{}, Questions: []domain.OperatorQuestion{}}}
		logs.page.Records = []observability.LogRecord{{Sequence: 2, Time: e2eNow, Level: "INFO", Message: "Issue run started", Fields: map[string]any{"issue_identifier": "SYM-123"}}}
	case "stale-error":
		add(populated, false, []string{"provider_not_dispatchable"})
		runtime.snapshot.Tracker = domain.TrackerStatus{Kind: "fixture", Scope: "fixture/example", State: "failed", Stale: true, ErrorCode: "tracker_transport", Message: strings.Repeat("W", maximumDisplayBytes), LastAttemptAt: &e2eNow}
		runtime.snapshot.Config = domain.ConfigStatus{State: "invalid", ErrorCode: "invalid_workflow", Message: "Configuration needs attention.", ChangedAt: e2eNow}
	case "filtered-empty":
		closed := populated
		closed.State = "Closed"
		add(closed, true, []string{})
	case "malicious-text":
		unsafeURL := "javascript:alert(1)"
		malicious := domain.Issue{ID: "mal-1", Identifier: "MAL-1", Title: "<script>fixture-title-canary</script>", Description: stringPointer("<img src=x onerror=fixture-description-canary>"), State: "Open<script>", URL: &unsafeURL, Labels: []string{"<b>fixture-label-canary</b>", strings.Repeat("W", maximumShortTextBytes)}, BlockedBy: []domain.BlockerRef{}}
		add(malicious, false, []string{"untrusted-reason-canary"})
		runtime.snapshot.Requests = []domain.OperatorRequest{{
			ID: "long-request", IssueIdentifier: "MAL-1", Kind: "choice", Title: strings.Repeat("W", maximumDisplayBytes), Summary: "Long unbroken operator text.",
			Choices: []domain.OperatorChoice{{ID: "long-choice", Label: strings.Repeat("W", maximumDisplayBytes)}}, Questions: []domain.OperatorQuestion{{ID: "long-question", Label: strings.Repeat("W", maximumDisplayBytes), Choices: []domain.OperatorChoice{}}},
		}}
		logs.page.Records = []observability.LogRecord{{Sequence: 1, Time: e2eNow, Level: "WARN", Message: "<script>fixture-log-canary</script>", Fields: map[string]any{"issue_identifier": "MAL-1"}}}
	case "encoded-identifier":
		encoded := populated
		encoded.ID = "encoded"
		encoded.Identifier = "TEAM/#42"
		encoded.Title = "Encoded identifier"
		add(encoded, true, []string{})
	case "degraded-log":
		add(populated, true, []string{})
		logs.page.Degraded = true
		logs.page.Records = []observability.LogRecord{{Sequence: 1, Time: e2eNow, Level: "ERROR", Message: "Recent in-memory log", Fields: map[string]any{"issue_identifier": "SYM-123"}}}
	case "long-log":
		add(populated, true, []string{})
		logs.page.Records = []observability.LogRecord{{Sequence: 101, Time: e2eNow, Level: "INFO", Message: strings.Repeat("long message ", 1000), Fields: map[string]any{"issue_identifier": "SYM-123", "detail": strings.Repeat("long field ", 2200)}}}
		for sequence := 100; sequence >= 1; sequence-- {
			logs.page.Records = append(logs.page.Records, observability.LogRecord{Sequence: uint64(sequence), Time: e2eNow.Add(-time.Duration(101-sequence) * time.Second), Level: "DEBUG", Message: "Pagination fixture entry", Fields: map[string]any{"issue_identifier": "SYM-123"}})
		}
	}
	return runtime, logs
}

func stringPointer(value string) *string { return &value }

var _ app.RuntimeQueries = (*e2ePageRuntime)(nil)
var _ app.RuntimeCommands = (*e2ePageRuntime)(nil)
var _ LogQueries = (*e2eLogQueries)(nil)
