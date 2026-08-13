//go:build e2e

package web

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coryj627/symphony/go/internal/app"
	"github.com/coryj627/symphony/go/internal/codex"
	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
)

var e2eScenarioManifest = map[string]struct{}{
	"empty": {}, "populated": {}, "stale-error": {}, "filtered-empty": {},
	"issue-not-found": {}, "malicious-text": {}, "encoded-identifier": {},
	"degraded-log": {}, "long-log": {}, "live-focus": {}, "live-structural": {},
	"live-pause": {}, "live-resume-failure": {}, "live-runtime-controls": {},
	"runtime-unavailable": {}, "runtime-retrying": {}, "runtime-stalled": {}, "runtime-stopping": {},
	"codex-incompatible": {}, "codex-tool-failure": {}, "runtime-stopping-failed": {},
	"live-operator-requests": {},
}

const maximumE2ELiveClients = 64

var e2eLiveScenarios = [...]string{
	"live-focus",
	"live-structural",
	"live-pause",
	"live-resume-failure",
	"live-runtime-controls",
	"live-operator-requests",
}

func newPageDependencyResolver() pageDependencyResolver {
	fixtures := make(map[string]pageDependencies, len(e2eScenarioManifest))
	for scenario := range e2eScenarioManifest {
		if !strings.HasPrefix(scenario, "live-") {
			fixtures[scenario] = newE2EPageDependencies(scenario)
		}
	}
	var liveMu sync.Mutex
	liveFixtures := make(map[[sha256.Size]byte]*e2eLiveFixtureSet)
	return func(request *http.Request, _ pageDependencies) (pageDependencies, string, bool) {
		values, present, ok := e2eScenarioValues(request)
		if !ok {
			return pageDependencies{}, "", false
		}
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
		if !strings.HasPrefix(scenario, "live-") {
			return fixtures[scenario], scenario, true
		}
		client := e2eClientIdentity(request)
		liveMu.Lock()
		fixtureSet := liveFixtures[client]
		if fixtureSet == nil {
			if len(liveFixtures) >= maximumE2ELiveClients {
				liveMu.Unlock()
				return pageDependencies{}, "", false
			}
			fixtureSet = newE2ELiveFixtureSet()
			liveFixtures[client] = fixtureSet
		}
		fixture := fixtureSet.fixtures[scenario]
		liveMu.Unlock()
		if fixture == nil {
			return pageDependencies{}, "", false
		}
		if request.URL.EscapedPath() == "/api/v1/state" {
			return fixture.state, scenario, true
		}
		return fixture.page, scenario, true
	}
}

type e2eLiveFixtureSet struct {
	fixtures map[string]*e2eLiveFixture
}

func newE2ELiveFixtureSet() *e2eLiveFixtureSet {
	fixtures := make(map[string]*e2eLiveFixture, len(e2eLiveScenarios))
	for _, scenario := range e2eLiveScenarios {
		fixtures[scenario] = newE2ELiveFixture(scenario)
	}
	return &e2eLiveFixtureSet{fixtures: fixtures}
}

func e2eClientIdentity(request *http.Request) [sha256.Size]byte {
	csrf, _ := CSRFToken(request.Context())
	engine := "direct"
	userAgent := strings.ToLower(request.UserAgent())
	switch {
	case strings.Contains(userAgent, "chrome"), strings.Contains(userAgent, "chromium"), strings.Contains(userAgent, "crios"), strings.Contains(userAgent, "edg/"):
		engine = "chromium"
	case strings.Contains(userAgent, "applewebkit") && strings.Contains(userAgent, "safari"):
		engine = "webkit"
	}
	return sha256.Sum256([]byte(csrf + "\x00" + engine))
}

type e2eLiveFixture struct {
	page  pageDependencies
	state pageDependencies
}

func newE2ELiveFixture(scenario string) *e2eLiveFixture {
	runtime := newLiveE2ERuntime(scenario)
	page := pageDependencies{queries: runtime, commands: runtime, logs: &e2eLogQueries{page: observability.LogPage{Records: []observability.LogRecord{}}}}
	state := page
	if scenario == "live-resume-failure" {
		state.queries = &e2eOneShotStateFailure{RuntimeQueries: runtime}
	}
	return &e2eLiveFixture{page: page, state: state}
}

type e2eOneShotStateFailure struct {
	app.RuntimeQueries
	mu       sync.Mutex
	returned bool
}

func (queries *e2eOneShotStateFailure) Snapshot(ctx context.Context) (domain.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return domain.Snapshot{}, err
	}
	queries.mu.Lock()
	defer queries.mu.Unlock()
	if !queries.returned {
		queries.returned = true
		return domain.Snapshot{}, errors.New("fixture_runtime_unavailable")
	}
	return queries.RuntimeQueries.Snapshot(ctx)
}

func e2eScenarioValues(request *http.Request) ([]string, bool, bool) {
	if request.URL.EscapedPath() != "/api/v1/events" {
		query, err := url.ParseQuery(request.URL.RawQuery)
		if err != nil {
			return nil, false, false
		}
		values, present := query["__e2e_scenario"]
		return values, present, true
	}
	var values []string
	present := false
	for _, part := range strings.Split(request.URL.RawQuery, "&") {
		if part == "" {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(part, "=")
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			return nil, false, false
		}
		if key == "after" {
			continue
		}
		if key != "__e2e_scenario" {
			if _, err := url.QueryUnescape(rawValue); err != nil {
				return nil, false, false
			}
			continue
		}
		present = true
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			return nil, true, false
		}
		values = append(values, value)
	}
	return values, present, true
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

func (runtime *e2ePageRuntime) EventsAfter(ctx context.Context, cursor domain.EventCursor) (domain.EventPage, error) {
	if err := ctx.Err(); err != nil {
		return domain.EventPage{}, err
	}
	page := domain.EventPage{LatestCursor: runtime.recent.LatestCursor, Events: []domain.Event{}}
	if cursor.Epoch == "" || cursor.Epoch != page.LatestCursor.Epoch || cursor.Sequence > page.LatestCursor.Sequence {
		page.Reset = true
		return page, nil
	}
	if len(runtime.recent.Events) > 0 && cursor.Sequence != ^uint64(0) && runtime.recent.Events[0].Sequence > cursor.Sequence+1 {
		page.Reset = true
		return page, nil
	}
	for _, event := range runtime.recent.Events {
		if event.Epoch == cursor.Epoch && event.Sequence > cursor.Sequence {
			page.Events = append(page.Events, event)
		}
	}
	return page, nil
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
func (*e2ePageRuntime) ExtendOperatorRequest(context.Context, string) error {
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

func newE2EPageDependencies(scenario string) pageDependencies {
	runtime, logs := newE2EPageFixture(scenario)
	return pageDependencies{queries: runtime, commands: runtime, logs: logs}
}

type liveE2ERuntime struct {
	mu        sync.Mutex
	scenario  string
	snapshot  domain.Snapshot
	details   map[string]domain.IssueDetail
	journal   *observability.Journal
	refreshes int
}

func newLiveE2ERuntime(scenario string) *liveE2ERuntime {
	journal := observability.NewJournal(observability.JournalOptions{})
	snapshot := domain.EmptySnapshot()
	snapshot.GeneratedAt = e2eNow
	snapshot.EventCursor = journal.Cursor()
	snapshot.Scheduler = domain.SchedulerStatus{Available: true, Enabled: false, State: "paused", Message: "Scheduler is paused."}
	snapshot.Config = domain.ConfigStatus{State: "valid", Digest: "e2e", ActiveDigest: "e2e", ChangedAt: e2eNow}
	snapshot.Tracker = domain.TrackerStatus{Kind: "fixture", Scope: "fixture/live", State: "ready", LastAttemptAt: &e2eNow, LastSuccessAt: &e2eNow}
	runtime := &liveE2ERuntime{scenario: scenario, snapshot: snapshot, details: make(map[string]domain.IssueDetail), journal: journal}
	switch scenario {
	case "live-focus":
		runtime.replaceCandidatesLocked(liveE2ECandidate("LIVE-1", "Before refresh"))
	case "live-structural":
		runtime.replaceCandidatesLocked(
			liveE2ECandidateWithID("live-one", "LIVE-1", "First issue"),
			liveE2ECandidateWithID("live-two", "LIVE-2", "Second issue"),
		)
	case "live-pause":
		runtime.replaceCandidatesLocked(liveE2ECandidate("LIVE-1", "Before pause"))
	case "live-resume-failure":
		runtime.replaceCandidatesLocked(liveE2ECandidate("LIVE-1", "Before resume"))
	case "live-runtime-controls":
		runtime.replaceCandidatesLocked(liveE2ECandidateWithID("control-one", "CTRL-1", "Control one active run"))
	case "live-operator-requests":
		runtime.replaceCandidatesLocked(liveE2ECandidateWithID("request-one", "REQ-1", "Operator request accessibility"))
		now := time.Now().UTC()
		request := func(id, kind, title string) domain.OperatorRequest {
			return domain.OperatorRequest{
				ID: id, SessionID: "thread-1-turn-1", IssueID: "request-one", IssueIdentifier: "REQ-1", Kind: kind,
				Title: title, Summary: "Codex is waiting for an operator response.", OpenedAt: now,
				WarningAt: now.Add(9*time.Minute + 40*time.Second), DeadlineAt: now.Add(10 * time.Minute), ExtensionsRemaining: 10,
				Choices: []domain.OperatorChoice{}, Questions: []domain.OperatorQuestion{}, Details: []domain.OperatorDetail{},
			}
		}
		command := request("command-request", "command_approval", "Approve command execution")
		command.Details = []domain.OperatorDetail{{Label: "Command", Value: "go test ./..."}, {Label: "Working directory", Value: "/workspace/REQ-1"}}
		command.Choices = []domain.OperatorChoice{{ID: "accept", Label: "Allow once", Description: "Run this command once."}, {ID: "decline", Label: "Deny", Description: "Do not run this command."}}
		file := request("file-request", "file_approval", "Approve file changes")
		file.Details = []domain.OperatorDetail{{Label: "Grant root", Value: "/workspace/REQ-1"}}
		file.Choices = []domain.OperatorChoice{{ID: "accept", Label: "Allow once", Description: "Apply these file changes once."}, {ID: "decline", Label: "Deny", Description: "Do not apply the file changes."}}
		permission := request("permission-request", "permission_approval", "Approve additional permissions")
		permission.Details = []domain.OperatorDetail{{Label: "Requested permission profile", Value: `{"network":{"enabled":true}}`}}
		permission.Choices = []domain.OperatorChoice{{ID: "grant_turn", Label: "Allow for this turn", Description: "Grant exactly the displayed permission profile."}, {ID: "decline", Label: "Deny", Description: "Do not grant the requested permissions."}}
		questions := request("question-request", "user_input", "Codex needs your input")
		questions.Questions = []domain.OperatorQuestion{
			{ID: "platform", Label: "Platform", Description: "Choose a platform.", Required: true, Choices: []domain.OperatorChoice{{ID: "option-1", Label: "Windows", Description: "Use Windows."}, {ID: "option-2", Label: "macOS", Description: "Use macOS."}}},
			{ID: "detail", Label: "Detail", Description: "Provide additional detail.", Required: true, AllowsOther: true, Choices: []domain.OperatorChoice{}},
			{ID: "token", Label: "Token", Description: "Enter a temporary secret.", Required: true, AllowsOther: true, IsSecret: true, Choices: []domain.OperatorChoice{}},
		}
		expiring := request("expiring-request", "file_approval", "Expiring approval")
		expiring.Choices = []domain.OperatorChoice{{ID: "decline", Label: "Deny", Description: "Do not apply the file changes."}}
		stale := request("stale-request", "command_approval", "Stale approval example")
		stale.Choices = []domain.OperatorChoice{{ID: "decline", Label: "Deny", Description: "Do not run this command."}}
		runtime.snapshot.Requests = []domain.OperatorRequest{command, file, permission, questions, expiring, stale}
	}
	return runtime
}

func liveE2ECandidate(identifier, title string) domain.CandidateRow {
	return liveE2ECandidateWithID(strings.ToLower(identifier), identifier, title)
}

func liveE2ECandidateWithID(issueID, identifier, title string) domain.CandidateRow {
	return domain.CandidateRow{Issue: domain.Issue{ID: issueID, Identifier: identifier, Title: title, State: "Open", Labels: []string{}}, Routable: true, RoutingReasons: []string{}}
}

func (runtime *liveE2ERuntime) replaceCandidatesLocked(candidates ...domain.CandidateRow) {
	runtime.snapshot.Candidates = append([]domain.CandidateRow(nil), candidates...)
	runtime.details = make(map[string]domain.IssueDetail, len(candidates))
	for _, candidate := range candidates {
		runtime.details[candidate.Issue.Identifier] = domain.IssueDetail{Issue: candidate.Issue, Status: "candidate", Routable: candidate.Routable, RoutingReasons: append([]string(nil), candidate.RoutingReasons...)}
	}
}

func (runtime *liveE2ERuntime) Snapshot(ctx context.Context) (domain.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return domain.Snapshot{}, err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.snapshot.Clone()
}

func (runtime *liveE2ERuntime) Issue(ctx context.Context, identifier string) (domain.IssueDetail, error) {
	if err := ctx.Err(); err != nil {
		return domain.IssueDetail{}, err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	detail, found := runtime.details[identifier]
	if !found {
		return domain.IssueDetail{}, app.ErrIssueNotFound
	}
	return detail.Clone()
}

func (runtime *liveE2ERuntime) EventsAfter(ctx context.Context, cursor domain.EventCursor) (domain.EventPage, error) {
	if err := ctx.Err(); err != nil {
		return domain.EventPage{}, err
	}
	return runtime.journal.After(cursor), nil
}

func (runtime *liveE2ERuntime) RecentEvents(ctx context.Context, limit int) (domain.EventPage, error) {
	if err := ctx.Err(); err != nil {
		return domain.EventPage{}, err
	}
	return runtime.journal.Recent(limit), nil
}

func (runtime *liveE2ERuntime) SubscribeEvents(cursor domain.EventCursor) <-chan struct{} {
	return runtime.journal.Subscribe(cursor)
}

func (runtime *liveE2ERuntime) Refresh(ctx context.Context) (domain.RefreshReceipt, error) {
	if err := ctx.Err(); err != nil {
		return domain.RefreshReceipt{}, err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.refreshes++
	switch runtime.scenario {
	case "live-focus":
		if runtime.refreshes == 1 {
			runtime.replaceCandidatesLocked(liveE2ECandidate("LIVE-1", "After refresh"), liveE2ECandidate("LIVE-2", "Second issue"))
		}
	case "live-structural":
		switch runtime.refreshes {
		case 1:
			runtime.replaceCandidatesLocked(liveE2ECandidateWithID("live-two", "LIVE-2", "Second issue"))
		case 2:
			runtime.replaceCandidatesLocked(liveE2ECandidateWithID("live-two", "TEAM:@&=+$!é", "Second issue updated"))
		}
	case "live-pause":
		switch runtime.refreshes {
		case 1:
			runtime.replaceCandidatesLocked(liveE2ECandidate("LIVE-2", "While paused"))
		case 2:
			runtime.replaceCandidatesLocked(liveE2ECandidate("LIVE-2", "After resume"), liveE2ECandidate("LIVE-3", "Future delivery"))
		}
	}
	published, err := runtime.journal.Publish(domain.Event{Type: "queue.refreshed", Data: map[string]any{}})
	if err != nil {
		return domain.RefreshReceipt{}, err
	}
	runtime.snapshot.EventCursor = domain.EventCursor{Epoch: published.Epoch, Sequence: published.Sequence}
	runtime.snapshot.GeneratedAt = published.At
	return domain.RefreshReceipt{Queued: true, RequestedAt: published.At, Operations: []string{"poll"}}, nil
}

func (runtime *liveE2ERuntime) SetScheduler(ctx context.Context, enabled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.scenario != "live-runtime-controls" {
		return app.ErrUnavailableInPhase
	}
	runtime.snapshot.Scheduler = domain.SchedulerStatus{Available: true, Enabled: enabled}
	issue := runtime.snapshot.Candidates[0].Issue
	detail := runtime.details[issue.Identifier]
	if enabled {
		runtime.snapshot.Scheduler.State = "running"
		runtime.snapshot.Scheduler.Message = "The scheduler is running."
		run := domain.RunningRow{
			IssueID: issue.ID, IssueIdentifier: issue.Identifier, State: issue.State,
			LastEvent: "running", LastMessage: "Worker activity is progressing.",
			StartedAt: e2eNow, LastEventAt: e2eNow, Tokens: domain.TokenTotals{InputTokens: 12, OutputTokens: 7, TotalTokens: 19},
		}
		runtime.snapshot.Running = []domain.RunningRow{run}
		detail.Status = "running"
		detail.Running = &run
	} else {
		runtime.snapshot.Scheduler.State = "paused"
		runtime.snapshot.Scheduler.Message = "The scheduler is paused."
		if len(runtime.snapshot.Running) > 0 {
			run := runtime.snapshot.Running[0]
			run.LastEvent = string(domain.RunStatusStopping)
			run.LastMessage = "The active run is stopping safely."
			runtime.snapshot.Running[0] = run
			detail.Status = "stopping"
			detail.Running = &run
		}
	}
	runtime.details[issue.Identifier] = detail
	published, err := runtime.journal.Publish(domain.Event{Type: "runtime.changed", Data: map[string]any{}})
	if err != nil {
		return err
	}
	runtime.snapshot.EventCursor = domain.EventCursor{Epoch: published.Epoch, Sequence: published.Sequence}
	runtime.snapshot.GeneratedAt = published.At
	return nil
}
func (runtime *liveE2ERuntime) Respond(ctx context.Context, response domain.OperatorResponse) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.scenario != "live-operator-requests" {
		return app.ErrUnavailableInPhase
	}
	if response.RequestID == "stale-request" {
		return codex.ErrStaleRequest
	}
	for index, request := range runtime.snapshot.Requests {
		if request.ID != response.RequestID || request.SessionID != response.SessionID {
			continue
		}
		runtime.snapshot.Requests = append(runtime.snapshot.Requests[:index:index], runtime.snapshot.Requests[index+1:]...)
		return nil
	}
	return codex.ErrStaleRequest
}
func (runtime *liveE2ERuntime) ExtendOperatorRequest(ctx context.Context, requestID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.scenario != "live-operator-requests" {
		return app.ErrUnavailableInPhase
	}
	for index := range runtime.snapshot.Requests {
		request := &runtime.snapshot.Requests[index]
		if request.ID != requestID {
			continue
		}
		if request.ExtensionsRemaining <= 0 {
			return codex.ErrExtensionLimit
		}
		request.ExtensionsUsed++
		request.ExtensionsRemaining--
		now := time.Now().UTC()
		request.WarningAt = now.Add(9*time.Minute + 40*time.Second)
		request.DeadlineAt = now.Add(10 * time.Minute)
		return nil
	}
	return codex.ErrStaleRequest
}

var _ app.RuntimeQueries = (*liveE2ERuntime)(nil)
var _ app.RuntimeCommands = (*liveE2ERuntime)(nil)

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
	case "runtime-unavailable":
		runtime.snapshot.Scheduler = domain.SchedulerStatus{Available: false, State: "unavailable", Message: "Agent runtime will be enabled in Phase 4."}
	case "runtime-retrying":
		retrying := populated
		retrying.ID, retrying.Identifier, retrying.Title = "retry-one", "RETRY-1", "Retrying issue"
		add(retrying, true, []string{})
		retry := domain.RetryRow{IssueID: retrying.ID, IssueIdentifier: retrying.Identifier, Attempt: 3, DueAt: e2eNow.Add(5 * time.Minute), Error: "safe retry detail"}
		runtime.snapshot.Retrying = []domain.RetryRow{retry}
		detail := runtime.details[retrying.Identifier]
		detail.Status = "retrying"
		detail.Retry = &retry
		runtime.details[retrying.Identifier] = detail
	case "runtime-stalled":
		stalled := populated
		stalled.ID, stalled.Identifier, stalled.Title = "stalled-one", "STALL-1", "Stalled issue"
		add(stalled, true, []string{})
		run := domain.RunningRow{IssueID: stalled.ID, IssueIdentifier: stalled.Identifier, State: stalled.State, LastEvent: string(domain.RunStatusStalled), LastMessage: "No worker event arrived before the stall deadline.", StartedAt: e2eNow.Add(-time.Hour), LastEventAt: e2eNow.Add(-time.Hour)}
		runtime.snapshot.Running = []domain.RunningRow{run}
		detail := runtime.details[stalled.Identifier]
		detail.Status = "stalled"
		detail.Running = &run
		runtime.details[stalled.Identifier] = detail
	case "runtime-stopping":
		stopping := populated
		stopping.ID, stopping.Identifier, stopping.Title = "stopping-one", "STOP-1", "Stopping issue"
		add(stopping, true, []string{})
		runtime.snapshot.Scheduler = domain.SchedulerStatus{Available: true, State: "stopping", Message: "The scheduler is stopping active work."}
		run := domain.RunningRow{IssueID: stopping.ID, IssueIdentifier: stopping.Identifier, State: stopping.State, LastEvent: string(domain.RunStatusStopping), LastMessage: "The active run is stopping safely.", StartedAt: e2eNow.Add(-time.Minute), LastEventAt: e2eNow}
		runtime.snapshot.Running = []domain.RunningRow{run}
		detail := runtime.details[stopping.Identifier]
		detail.Status = "stopping"
		detail.Running = &run
		runtime.details[stopping.Identifier] = detail
	case "codex-incompatible":
		runtime.snapshot.Scheduler = domain.SchedulerStatus{Available: false, State: "unavailable", Message: "The installed Codex CLI does not match the reviewed app-server version."}
	case "codex-tool-failure":
		failed := populated
		failed.ID, failed.Identifier, failed.Title = "codex-tool-one", "CODEX-TOOL-1", "Codex provider tool failure"
		add(failed, true, []string{})
		run := domain.RunningRow{IssueID: failed.ID, IssueIdentifier: failed.Identifier, State: failed.State, LastEvent: string(domain.RunStatusFailed), LastMessage: "The provider tool returned a safe failure result.", StartedAt: e2eNow.Add(-time.Minute), LastEventAt: e2eNow}
		runtime.snapshot.Running = []domain.RunningRow{run}
		detail := runtime.details[failed.Identifier]
		detail.Status = string(domain.RunStatusFailed)
		detail.Running = &run
		runtime.details[failed.Identifier] = detail
	case "runtime-stopping-failed":
		stopping := populated
		stopping.ID, stopping.Identifier, stopping.Title = "stopping-failed-one", "STOPFAIL-1", "Codex process cleanup needs attention"
		add(stopping, true, []string{})
		runtime.snapshot.Scheduler = domain.SchedulerStatus{Available: false, State: "stopping_failed", Message: "The Codex process tree could not be confirmed stopped."}
		run := domain.RunningRow{IssueID: stopping.ID, IssueIdentifier: stopping.Identifier, State: stopping.State, LastEvent: string(domain.RunStatusStoppingFailed), LastMessage: "Manual cleanup may be required before restarting.", StartedAt: e2eNow.Add(-time.Minute), LastEventAt: e2eNow}
		runtime.snapshot.Running = []domain.RunningRow{run}
		detail := runtime.details[stopping.Identifier]
		detail.Status = string(domain.RunStatusStoppingFailed)
		detail.Running = &run
		runtime.details[stopping.Identifier] = detail
	}
	return runtime, logs
}

func stringPointer(value string) *string { return &value }

var _ app.RuntimeQueries = (*e2ePageRuntime)(nil)
var _ app.RuntimeCommands = (*e2ePageRuntime)(nil)
var _ LogQueries = (*e2eLogQueries)(nil)
