package web

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/coryj627/symphony/go/internal/app"
	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/tracker"
)

const (
	maximumCandidateResponses = 200
	maximumRequestItems       = 100
	maximumRefreshBody        = 1024
)

type issueFilters struct {
	Query       string
	State       string
	Eligibility string
	Sort        string
}

func (handler *PageHandler) overviewHTML(w http.ResponseWriter, request *http.Request) {
	dependencies := handler.dependencies(request)
	snapshot, err := dependencies.queries.Snapshot(request.Context())
	if err != nil {
		handler.respondHTMLRequestError(w, request, http.StatusServiceUnavailable)
		return
	}
	routable := 0
	for _, candidate := range snapshot.Candidates {
		if candidate.Routable {
			routable++
		}
	}
	configCode := cleanCode(snapshot.Config.ErrorCode)
	trackerCode := cleanCode(snapshot.Tracker.ErrorCode)
	trackerScope := handler.presentationTrackerScope(request, snapshot)
	content := overviewContent{
		TrackerScope: trackerScope, Mode: handler.mode,
		Scheduler:      schedulerResponse{Available: snapshot.Scheduler.Available, Enabled: snapshot.Scheduler.Enabled, State: cleanDisplayValue(snapshot.Scheduler.State, maximumShortTextBytes), Message: cleanDisplayValue(snapshot.Scheduler.Message, maximumDisplayBytes)},
		CandidateCount: len(snapshot.Candidates), RoutableCount: routable, RunningCount: len(snapshot.Running), RetryingCount: len(snapshot.Retrying), RequestCount: len(snapshot.Requests),
		Tracker: trackerStatusResponse{
			Kind: cleanDisplayValue(snapshot.Tracker.Kind, maximumShortTextBytes), Scope: trackerScope, State: cleanDisplayValue(snapshot.Tracker.State, maximumShortTextBytes), Stale: snapshot.Tracker.Stale,
			Retryable: snapshot.Tracker.Retryable, HasError: snapshot.Tracker.ErrorCode != "", LastAttemptAt: cloneTimePointer(snapshot.Tracker.LastAttemptAt), LastSuccessAt: cloneTimePointer(snapshot.Tracker.LastSuccessAt), RetryAt: cloneTimePointer(snapshot.Tracker.RetryAt),
			ErrorCode: trackerCode, Message: safeStatusMessage(snapshot.Tracker.Message, snapshot.Tracker.ErrorCode != "", trackerCode, "Tracker status needs attention."),
		},
		Config: configStatusResponse{
			State: cleanDisplayValue(snapshot.Config.State, maximumShortTextBytes), Digest: cleanMachine(snapshot.Config.Digest, maximumDisplayBytes), ActiveDigest: cleanMachine(snapshot.Config.ActiveDigest, maximumDisplayBytes),
			UsingLastGood: snapshot.Config.UsingLastGood, HasError: snapshot.Config.ErrorCode != "", ErrorCode: configCode, Message: safeStatusMessage(snapshot.Config.Message, snapshot.Config.ErrorCode != "", configCode, "Configuration status needs attention."), ChangedAt: snapshot.Config.ChangedAt,
		},
		ConfigError: snapshot.Config.ErrorCode != "", TrackerError: snapshot.Tracker.ErrorCode != "",
		Requests: operatorRequestResponses(snapshot.Requests),
	}
	content.StartDisabled, content.StartReason, content.StopDisabled, content.StopReason = schedulerControlState(snapshot.Scheduler)
	populateTrackerTimeViews(&content.Tracker)
	if content.ConfigError {
		content.ErrorCount++
	}
	if content.TrackerError {
		content.ErrorCount++
	}
	csrf, _ := CSRFToken(request.Context())
	status := "Current tracker work is shown."
	if handler.mode == "configure" {
		status = "No scheduler is running. Current tracker work is shown."
	}
	page := Page{Title: "Overview — Symphony", Route: "/", Heading: "Overview", Mode: handler.mode, Status: status, CSRFToken: csrf, Scenario: dependencies.scenario, OperatorReturnURL: "/", Content: content}
	configureLivePage(&page, "overview", snapshot.EventCursor, nil)
	if firstQueryValue(request.URL.Query(), "result") == "refresh-requested" {
		page.Flash = "Refresh requested."
		if firstQueryValue(request.URL.Query(), "focus") == "refresh" {
			page.FocusTarget = "refresh"
		}
	}
	switch firstQueryValue(request.URL.Query(), "result") {
	case "runtime-started":
		page.Flash = "Scheduler start requested."
	case "runtime-stopped":
		page.Flash = "Scheduler stop requested."
	case "request-responded":
		page.Flash = "Operator response submitted."
	case "request-extended":
		page.Flash = "Operator request extended."
	}
	if target := firstQueryValue(request.URL.Query(), "focus"); target == "start-runtime" || target == "stop-runtime" || target == "requests-heading" {
		page.FocusTarget = target
	}
	if err := handler.renderHTML(w, "overview", page); err != nil {
		handler.respondHTMLRequestError(w, request, http.StatusInternalServerError)
	}
}

func schedulerControlState(status domain.SchedulerStatus) (startDisabled bool, startReason string, stopDisabled bool, stopReason string) {
	if !status.Available {
		reason := cleanDisplayValue(status.Message, maximumDisplayBytes)
		if reason == "" {
			reason = "Agent runtime will be enabled in Phase 4."
		}
		return true, reason, true, reason
	}
	if status.State == "stopping" {
		return true, "Wait for active work to stop before restarting the scheduler.", true, "The scheduler is already stopping."
	}
	if status.Enabled {
		return true, "The scheduler is already running.", false, "Stop the scheduler after active work is canceled safely."
	}
	return false, "Start polling and dispatch for this project.", true, "The scheduler is already paused."
}

func (handler *PageHandler) presentationTrackerScope(request *http.Request, snapshot domain.Snapshot) string {
	trackerScope := cleanDisplayValue(snapshot.Tracker.Scope, maximumDisplayBytes)
	if trackerScope == "" && handler.configService != nil {
		if view, err := handler.configService.View(request.Context()); err == nil && view.StructuredAvailable {
			trackerScope = cleanDisplayValue(view.Tracker.Scope, maximumDisplayBytes)
		}
	}
	if trackerScope == "" {
		return "Tracker scope not selected"
	}
	return trackerScope
}

func (handler *PageHandler) issuesHTML(w http.ResponseWriter, request *http.Request) {
	dependencies := handler.dependencies(request)
	snapshot, err := dependencies.queries.Snapshot(request.Context())
	if err != nil {
		handler.respondHTMLRequestError(w, request, http.StatusServiceUnavailable)
		return
	}
	rows, filters := filteredCandidateResponses(snapshot.Candidates, snapshot.Tracker.Stale, request.URL.Query())
	for index := range rows {
		rows[index].DetailURL = internalURL(rows[index].DetailURL, dependencies.scenario)
	}
	states := candidateStates(snapshot.Candidates)
	status := strconv.Itoa(len(rows)) + " tracker work candidates are shown."
	if len(rows) == 0 {
		status = "No tracker work candidates match these filters."
	} else if len(rows) == 1 {
		status = "1 tracker work candidate is shown."
	}
	csrf, _ := CSRFToken(request.Context())
	page := Page{Title: "Issues — Symphony", Route: "/issues", Heading: "Issues", Mode: handler.mode, Status: status, CSRFToken: csrf, Scenario: dependencies.scenario, Content: issuesContent{Filters: filters, Rows: rows, States: states}}
	configureLivePage(&page, "issues", snapshot.EventCursor, issueFilterValues(filters))
	if err := handler.renderHTML(w, "issues", page); err != nil {
		handler.respondHTMLRequestError(w, request, http.StatusInternalServerError)
	}
}

func (handler *PageHandler) activityHTML(w http.ResponseWriter, request *http.Request) {
	dependencies := handler.dependencies(request)
	snapshot, err := dependencies.queries.Snapshot(request.Context())
	if err != nil {
		handler.respondHTMLRequestError(w, request, http.StatusServiceUnavailable)
		return
	}
	tail, err := dependencies.queries.RecentEvents(request.Context(), 100)
	if err != nil {
		handler.respondHTMLRequestError(w, request, http.StatusServiceUnavailable)
		return
	}
	events, _, reset, _ := coherentEventViews(snapshot.EventCursor, tail)
	status := strconv.Itoa(len(events)) + " recent activity items are shown."
	if len(events) == 0 {
		status = "No activity has been recorded."
	} else if len(events) == 1 {
		status = "1 recent activity item is shown."
	}
	csrf, _ := CSRFToken(request.Context())
	page := Page{Title: "Activity — Symphony", Route: "/activity", Heading: "Activity", Mode: handler.mode, Status: status, CSRFToken: csrf, Scenario: dependencies.scenario, Content: activityContent{Events: events, Reset: reset}}
	configureLivePage(&page, "activity", snapshot.EventCursor, nil)
	if err := handler.renderHTML(w, "activity", page); err != nil {
		handler.respondHTMLRequestError(w, request, http.StatusInternalServerError)
	}
}

func safeStatusMessage(value string, sourceHasError bool, safeCode, fallback string) string {
	if sourceHasError && safeCode == "" {
		return fallback
	}
	return cleanDisplayValue(value, maximumDisplayBytes)
}

func candidateStates(candidates []domain.CandidateRow) []string {
	seen := make(map[string]struct{})
	states := make([]string, 0)
	for _, candidate := range candidates {
		state := cleanDisplayValue(candidate.Issue.State, maximumShortTextBytes)
		if state == "" {
			continue
		}
		key := foldText(state)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		states = append(states, state)
	}
	sort.SliceStable(states, func(left, right int) bool { return foldText(states[left]) < foldText(states[right]) })
	return states
}

func (handler *PageHandler) stateAPI(w http.ResponseWriter, request *http.Request) {
	dependencies := handler.dependencies(request)
	snapshot, err := dependencies.queries.Snapshot(request.Context())
	if err != nil {
		handler.writeAPIError(w, "runtime_unavailable")
		return
	}
	tail, err := dependencies.queries.RecentEvents(request.Context(), 100)
	if err != nil {
		handler.writeAPIError(w, "runtime_unavailable")
		return
	}
	candidates, _ := filteredCandidateResponses(snapshot.Candidates, snapshot.Tracker.Stale, request.URL.Query())
	activityEvents, recentEvents, activityReset, recentReset := coherentEventViews(snapshot.EventCursor, tail)

	running := make([]runningResponse, 0, len(snapshot.Running))
	for _, row := range snapshot.Running {
		running = append(running, runningResponseFrom(row))
	}
	retrying := make([]retryResponse, 0, len(snapshot.Retrying))
	for _, row := range snapshot.Retrying {
		retrying = append(retrying, retryResponseFrom(row))
	}
	requests := operatorRequestResponses(snapshot.Requests)
	routableTotal := 0
	for _, candidate := range snapshot.Candidates {
		if candidate.Routable {
			routableTotal++
		}
	}
	errorsCount := 0
	if snapshot.Config.ErrorCode != "" {
		errorsCount++
	}
	if snapshot.Tracker.ErrorCode != "" {
		errorsCount++
	}
	configCode := cleanCode(snapshot.Config.ErrorCode)
	trackerCode := cleanCode(snapshot.Tracker.ErrorCode)
	trackerScope := handler.presentationTrackerScope(request, snapshot)
	response := stateResponse{
		GeneratedAt: snapshot.GeneratedAt,
		Counts: stateCountsResponse{
			Running: len(running), Retrying: len(retrying), Candidates: len(snapshot.Candidates), Routable: routableTotal,
			NeedsAttention: len(snapshot.Candidates) - routableTotal, Requests: len(requests), Errors: errorsCount,
		},
		Running: running, Retrying: retrying, CodexTotals: tokenTotalsResponseFrom(snapshot.CodexTotals), RateLimits: nil,
		EventCursor: eventCursorString(snapshot.EventCursor), Candidates: candidates,
		RecentEvents: recentEvents, RecentEventsReset: recentReset, ActivityEvents: activityEvents, ActivityEventsReset: activityReset,
		Scheduler: schedulerResponse{
			Available: snapshot.Scheduler.Available, Enabled: snapshot.Scheduler.Enabled, State: cleanDisplayValue(snapshot.Scheduler.State, maximumShortTextBytes), Message: cleanDisplayValue(snapshot.Scheduler.Message, maximumDisplayBytes),
		},
		Config: configStatusResponse{
			State: cleanDisplayValue(snapshot.Config.State, maximumShortTextBytes), Digest: cleanMachine(snapshot.Config.Digest, maximumDisplayBytes), ActiveDigest: cleanMachine(snapshot.Config.ActiveDigest, maximumDisplayBytes),
			UsingLastGood: snapshot.Config.UsingLastGood, HasError: snapshot.Config.ErrorCode != "", ErrorCode: configCode, Message: safeAPIStatusMessage(snapshot.Config.Message, snapshot.Config.ErrorCode != "", configCode, "Configuration status needs attention."), ChangedAt: snapshot.Config.ChangedAt,
		},
		Tracker: trackerStatusResponse{
			Kind: cleanDisplayValue(snapshot.Tracker.Kind, maximumShortTextBytes), Scope: trackerScope, State: cleanDisplayValue(snapshot.Tracker.State, maximumShortTextBytes),
			Stale: snapshot.Tracker.Stale, HasError: snapshot.Tracker.ErrorCode != "", Retryable: snapshot.Tracker.Retryable, LastAttemptAt: cloneTimePointer(snapshot.Tracker.LastAttemptAt), LastSuccessAt: cloneTimePointer(snapshot.Tracker.LastSuccessAt), RetryAt: cloneTimePointer(snapshot.Tracker.RetryAt),
			ErrorCode: trackerCode, Message: safeAPIStatusMessage(snapshot.Tracker.Message, snapshot.Tracker.ErrorCode != "", trackerCode, "Tracker status needs attention."),
		},
		Requests: requests,
	}
	if err := writeJSON(w, http.StatusOK, response); err != nil {
		handler.writeAPIError(w, "internal_error")
	}
}

func (handler *PageHandler) issueAPI(w http.ResponseWriter, request *http.Request) {
	dependencies := handler.dependencies(request)
	identifier := request.PathValue("issue_identifier")
	if !validIssueIdentifier(identifier) {
		handler.writeAPIError(w, "invalid_identifier")
		return
	}
	detail, err := dependencies.queries.Issue(request.Context(), identifier)
	if err != nil {
		if errors.Is(err, app.ErrIssueNotFound) {
			handler.writeAPIError(w, "issue_not_found")
		} else {
			handler.writeAPIError(w, "runtime_unavailable")
		}
		return
	}
	response := issueResponseFrom(detail)
	if tail, tailErr := dependencies.queries.RecentEvents(request.Context(), 100); tailErr == nil {
		response.RecentEvents = issueEventResponses(tail.Events, detail.Issue.ID, detail.Issue.Identifier)
	}
	if logPage, logErr := dependencies.logs.Query(request.Context(), observability.LogQuery{Search: response.IssueIdentifier, Limit: 100}); logErr == nil {
		response.Logs.CodexSessionLogs = boundedIssueLogRecordViews(response.IssueIdentifier, logPage.Records, 100)
		response.Logs.Degraded = logPage.Degraded
	}
	if err := writeJSON(w, http.StatusOK, response); err != nil {
		handler.writeAPIError(w, "internal_error")
	}
}

func (handler *PageHandler) issueHTML(w http.ResponseWriter, request *http.Request) {
	dependencies := handler.dependencies(request)
	identifier := request.PathValue("identifier")
	if !validIssueIdentifier(identifier) {
		handler.respondHTMLRequestError(w, request, http.StatusBadRequest)
		return
	}
	detail, err := dependencies.queries.Issue(request.Context(), identifier)
	if err != nil {
		if errors.Is(err, app.ErrIssueNotFound) {
			handler.respondHTMLRequestError(w, request, http.StatusNotFound)
		} else {
			handler.respondHTMLRequestError(w, request, http.StatusServiceUnavailable)
		}
		return
	}
	response := issueResponseFrom(detail)
	canonicalIdentifier := response.Issue.Identifier
	snapshot, err := dependencies.queries.Snapshot(request.Context())
	if err != nil {
		handler.respondHTMLRequestError(w, request, http.StatusServiceUnavailable)
		return
	}
	filters := parseIssueFilters(request.URL.Query(), snapshot.Candidates)
	logPage, logErr := dependencies.logs.Query(request.Context(), observability.LogQuery{Search: canonicalIdentifier, Limit: 100})
	logs := []logRecordView{}
	degraded := false
	if logErr == nil {
		logs = issueLogRecordViews(canonicalIdentifier, logPage.Records)
		degraded = logPage.Degraded
	}
	requests := make([]domain.OperatorRequest, 0)
	for _, candidate := range snapshot.Requests {
		if cleanMachine(candidate.IssueIdentifier, maximumIdentifierBytes) == canonicalIdentifier {
			requests = append(requests, candidate)
		}
	}
	content := issueContent{
		Identifier: response.IssueIdentifier, Issue: response.Issue, Eligibility: response.Eligibility,
		Requests: operatorRequestResponses(requests), Logs: logs, LogDegraded: degraded,
	}
	if tail, tailErr := dependencies.queries.RecentEvents(request.Context(), 100); tailErr == nil {
		content.Activity = issueEventResponses(tail.Events, detail.Issue.ID, canonicalIdentifier)
	}
	if detail.Running != nil {
		running := runningResponseFrom(*detail.Running)
		content.Running = &running
	}
	if detail.Retry != nil {
		retry := retryResponseFrom(*detail.Retry)
		content.Retry = &retry
	}
	csrf, _ := CSRFToken(request.Context())
	page := Page{
		Title: response.Issue.Identifier + " — Symphony", Route: "/issues", Heading: "Issue " + response.Issue.Identifier, Mode: handler.mode,
		Status: "Issue details are shown.", CSRFToken: csrf, Scenario: dependencies.scenario, OperatorReturnURL: "/issues/" + url.PathEscape(response.IssueIdentifier), Content: content,
	}
	switch firstQueryValue(request.URL.Query(), "result") {
	case "request-responded":
		page.Flash = "Operator response submitted."
	case "request-extended":
		page.Flash = "Operator request extended."
	}
	if firstQueryValue(request.URL.Query(), "focus") == "requests-heading" {
		page.FocusTarget = "requests-heading"
	}
	if returnURL := issueListURL(filters); returnURL != "/issues" {
		page.IssueListURL = returnURL
	}
	if err := handler.renderHTML(w, "issue", page); err != nil {
		handler.respondHTMLRequestError(w, request, http.StatusInternalServerError)
	}
}

func runningResponseFrom(row domain.RunningRow) runningResponse {
	response := runningResponse{
		IssueID: cleanMachine(row.IssueID, maximumIdentifierBytes), IssueIdentifier: cleanMachine(row.IssueIdentifier, maximumIdentifierBytes),
		IssueURL: validatedTrackerURL(row.IssueURL), State: cleanDisplayValue(row.State, maximumShortTextBytes), TurnCount: row.TurnCount,
		LastEvent: cleanDisplayValue(row.LastEvent, maximumShortTextBytes), LastMessage: cleanDisplayValue(row.LastMessage, maximumDisplayBytes),
		StartedAt: row.StartedAt, LastEventAt: row.LastEventAt, Tokens: runningTokenResponseFrom(row.Tokens),
	}
	response.StartedDateTime, response.StartedDisplayTime = semanticTimeStrings(row.StartedAt)
	return response
}

func retryResponseFrom(row domain.RetryRow) retryResponse {
	safeError := ""
	if row.Error != "" {
		safeError = "A retry is scheduled."
	}
	response := retryResponse{
		IssueID: cleanMachine(row.IssueID, maximumIdentifierBytes), IssueIdentifier: cleanMachine(row.IssueIdentifier, maximumIdentifierBytes),
		IssueURL: validatedTrackerURL(row.IssueURL), Attempt: row.Attempt, DueAt: row.DueAt, Error: safeError,
	}
	response.DueDateTime, response.DueDisplayTime = semanticTimeStrings(row.DueAt)
	return response
}

func (handler *PageHandler) refreshAPI(w http.ResponseWriter, request *http.Request) {
	dependencies := handler.dependencies(request)
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil {
		handler.writeAPIError(w, "unsupported_media_type")
		return
	}
	switch mediaType {
	case "application/json":
		if !validEmptyRefreshJSON(request.Body) {
			handler.writeAPIError(w, "invalid_body")
			return
		}
	case "application/x-www-form-urlencoded":
		if err := request.ParseForm(); err != nil {
			handler.writeAPIError(w, "invalid_body")
			return
		}
	default:
		handler.writeAPIError(w, "unsupported_media_type")
		return
	}

	receipt, err := dependencies.commands.Refresh(request.Context())
	if err != nil {
		if errors.Is(err, app.ErrUnavailableInPhase) {
			handler.writeAPIError(w, "refresh_unavailable")
			return
		}
		spec := apiErrorSpecs["refresh_failed"]
		spec.retryable = trackerRetryable(err)
		handler.writeAPIErrorSpec(w, spec)
		return
	}
	if mediaType == "application/x-www-form-urlencoded" {
		location := internalURL("/?result=refresh-requested&focus=refresh", dependencies.scenario)
		setSecurityHeaders(w.Header())
		http.Redirect(w, request, location, http.StatusSeeOther)
		return
	}
	operations := make([]string, 0, len(receipt.Operations))
	for _, operation := range receipt.Operations {
		if operation == "poll" {
			operations = append(operations, operation)
		}
	}
	if operations == nil {
		operations = []string{}
	}
	response := refreshReceiptResponse{Queued: receipt.Queued, Coalesced: receipt.Coalesced, RequestedAt: receipt.RequestedAt, Operations: operations}
	if err := writeJSON(w, http.StatusAccepted, response); err != nil {
		handler.writeAPIError(w, "internal_error")
	}
}

func (handler *PageHandler) startRuntimeAPI(w http.ResponseWriter, request *http.Request) {
	handler.schedulerAPI(w, request, true)
}

func (handler *PageHandler) stopRuntimeAPI(w http.ResponseWriter, request *http.Request) {
	handler.schedulerAPI(w, request, false)
}

func (handler *PageHandler) schedulerAPI(w http.ResponseWriter, request *http.Request, enabled bool) {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil {
		handler.writeAPIError(w, "unsupported_media_type")
		return
	}
	switch mediaType {
	case "application/json":
		if !validEmptyRefreshJSON(request.Body) {
			handler.writeAPIError(w, "invalid_body")
			return
		}
	case "application/x-www-form-urlencoded":
		if err := request.ParseForm(); err != nil {
			handler.writeAPIError(w, "invalid_body")
			return
		}
	default:
		handler.writeAPIError(w, "unsupported_media_type")
		return
	}
	dependencies := handler.dependencies(request)
	if err := dependencies.commands.SetScheduler(request.Context(), enabled); err != nil {
		if errors.Is(err, app.ErrAgentRuntimeUnavailable) || errors.Is(err, app.ErrUnavailableInPhase) {
			handler.writeAPIError(w, "agent_runtime_unavailable")
			return
		}
		handler.writeAPIError(w, "scheduler_failed")
		return
	}
	if mediaType == "application/x-www-form-urlencoded" {
		result, focus := "runtime-stopped", "start-runtime"
		if enabled {
			result, focus = "runtime-started", "stop-runtime"
		}
		setSecurityHeaders(w.Header())
		http.Redirect(w, request, internalURL("/?result="+result+"&focus="+focus, dependencies.scenario), http.StatusSeeOther)
		return
	}
	snapshot, err := dependencies.queries.Snapshot(request.Context())
	if err != nil {
		handler.writeAPIError(w, "runtime_unavailable")
		return
	}
	requested := "paused"
	if enabled {
		requested = "running"
	}
	correlationID := handler.nextCorrelationID()
	w.Header().Set("X-Correlation-ID", correlationID)
	response := schedulerCommandResponse{
		Requested:     requested,
		Effective:     schedulerResponse{Available: snapshot.Scheduler.Available, Enabled: snapshot.Scheduler.Enabled, State: cleanDisplayValue(snapshot.Scheduler.State, maximumShortTextBytes), Message: cleanDisplayValue(snapshot.Scheduler.Message, maximumDisplayBytes)},
		CorrelationID: correlationID,
	}
	if err := writeJSON(w, http.StatusAccepted, response); err != nil {
		handler.writeAPIError(w, "internal_error")
	}
}

func validEmptyRefreshJSON(source io.Reader) bool {
	contents, err := io.ReadAll(io.LimitReader(source, maximumRefreshBody+1))
	if err != nil || len(contents) > maximumRefreshBody {
		return false
	}
	trimmed := trimJSONWhitespace(contents)
	if len(trimmed) == 0 {
		return true
	}
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return false
	}
	return len(trimJSONWhitespace(trimmed[1:len(trimmed)-1])) == 0
}

func trimJSONWhitespace(value []byte) []byte {
	start, end := 0, len(value)
	for start < end && isJSONWhitespace(value[start]) {
		start++
	}
	for end > start && isJSONWhitespace(value[end-1]) {
		end--
	}
	return value[start:end]
}

func isJSONWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n' || value == '\r'
}

func trackerRetryable(err error) bool {
	var pointer *tracker.Error
	if errors.As(err, &pointer) && pointer != nil {
		return pointer.Retryable
	}
	var value tracker.Error
	return errors.As(err, &value) && value.Retryable
}

func filteredCandidateResponses(source []domain.CandidateRow, stale bool, values url.Values) ([]candidateResponse, issueFilters) {
	filters := parseIssueFilters(values, source)
	candidates := make([]candidateResponse, 0, len(source))
	foldedQuery := foldText(filters.Query)
	for _, row := range source {
		candidate := candidateResponseFrom(row, stale, filters)
		if foldedQuery != "" && !strings.Contains(foldText(candidate.Identifier), foldedQuery) && !strings.Contains(foldText(candidate.Title), foldedQuery) {
			continue
		}
		if filters.State != "" && !strings.EqualFold(candidate.State, filters.State) {
			continue
		}
		if filters.Eligibility == "routable" && !candidate.Routable {
			continue
		}
		if filters.Eligibility == "needs_attention" && candidate.Routable {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if filters.Sort == "identifier" {
		sort.SliceStable(candidates, func(left, right int) bool {
			leftFolded, rightFolded := foldText(candidates[left].Identifier), foldText(candidates[right].Identifier)
			if leftFolded == rightFolded {
				return candidates[left].Identifier < candidates[right].Identifier
			}
			return leftFolded < rightFolded
		})
	}
	if len(candidates) > maximumCandidateResponses {
		candidates = candidates[:maximumCandidateResponses]
	}
	return candidates, filters
}

func parseIssueFilters(values url.Values, candidates []domain.CandidateRow) issueFilters {
	filters := issueFilters{Eligibility: "all", Sort: "scheduling"}
	if query, ok := normalizedFilterText(firstQueryValue(values, "query"), 256); ok {
		filters.Query = query
	}
	if state, ok := normalizedFilterText(firstQueryValue(values, "state"), maximumShortTextBytes); ok {
		for _, candidate := range candidates {
			canonical := cleanDisplayValue(candidate.Issue.State, maximumShortTextBytes)
			if state != "" && strings.EqualFold(state, canonical) {
				filters.State = canonical
				break
			}
		}
	}
	switch firstQueryValue(values, "eligibility") {
	case "routable", "needs_attention":
		filters.Eligibility = firstQueryValue(values, "eligibility")
	}
	if firstQueryValue(values, "sort") == "identifier" {
		filters.Sort = "identifier"
	}
	return filters
}

func firstQueryValue(values url.Values, key string) string {
	items := values[key]
	if len(items) == 0 {
		return ""
	}
	return items[0]
}

func normalizedFilterText(value string, maximum int) (string, bool) {
	if !utf8.ValidString(value) {
		return "", false
	}
	value = strings.TrimSpace(value)
	if len(value) > maximum {
		return "", false
	}
	for _, current := range value {
		if unicode.IsControl(current) {
			return "", false
		}
	}
	return value, true
}

func foldText(value string) string {
	var folded strings.Builder
	for _, current := range value {
		minimum := current
		for next := unicode.SimpleFold(current); next != current; next = unicode.SimpleFold(next) {
			if next < minimum {
				minimum = next
			}
		}
		folded.WriteRune(minimum)
	}
	return folded.String()
}

func candidateResponseFrom(row domain.CandidateRow, stale bool, filters issueFilters) candidateResponse {
	labels := make([]string, 0, min(len(row.Issue.Labels), 50))
	for _, label := range row.Issue.Labels {
		if len(labels) == 50 {
			break
		}
		labels = append(labels, cleanDisplayValue(label, maximumShortTextBytes))
	}
	return candidateResponse{
		IssueID: cleanMachine(row.Issue.ID, maximumIdentifierBytes), Identifier: cleanMachine(row.Issue.Identifier, maximumIdentifierBytes),
		Title: cleanDisplayValue(row.Issue.Title, maximumDisplayBytes), State: cleanDisplayValue(row.Issue.State, maximumShortTextBytes), Priority: cloneIntPointer(row.Issue.Priority),
		URL: validatedTrackerURL(row.Issue.URL), Labels: labels, CreatedAt: cloneTimePointer(row.Issue.CreatedAt), UpdatedAt: cloneTimePointer(row.Issue.UpdatedAt),
		Routable: row.Routable, RoutingReasons: routingReasonResponses(row.RoutingReasons), NeedsAttention: !row.Routable, Stale: stale,
		DetailURL: issueDetailURL(cleanMachine(row.Issue.Identifier, maximumIdentifierBytes), filters),
	}
}

func issueFilterValues(filters issueFilters) url.Values {
	values := make(url.Values)
	if filters.Query != "" {
		values.Set("query", filters.Query)
	}
	if filters.State != "" {
		values.Set("state", filters.State)
	}
	if filters.Eligibility != "" && filters.Eligibility != "all" {
		values.Set("eligibility", filters.Eligibility)
	}
	if filters.Sort != "" && filters.Sort != "scheduling" {
		values.Set("sort", filters.Sort)
	}
	return values
}

func issueListURL(filters issueFilters) string {
	if encoded := issueFilterValues(filters).Encode(); encoded != "" {
		return "/issues?" + encoded
	}
	return "/issues"
}

func issueDetailURL(identifier string, filters issueFilters) string {
	target := "/issues/" + url.PathEscape(identifier)
	if encoded := issueFilterValues(filters).Encode(); encoded != "" {
		return target + "?" + encoded
	}
	return target
}

func coherentEventViews(cursor domain.EventCursor, tail domain.EventPage) ([]eventSummaryResponse, []eventSummaryResponse, bool, bool) {
	activity := []eventSummaryResponse{}
	if cursor.Epoch == "" || tail.LatestCursor.Epoch != cursor.Epoch {
		return activity, []eventSummaryResponse{}, true, true
	}
	for _, event := range tail.Events {
		if event.Epoch == cursor.Epoch && event.Sequence <= cursor.Sequence {
			activity = append(activity, eventSummary(event))
		}
	}
	if cursor.Sequence != 0 {
		if len(activity) == 0 || activity[len(activity)-1].EventCursor != eventCursorString(cursor) {
			return []eventSummaryResponse{}, []eventSummaryResponse{}, true, true
		}
	}
	if len(activity) > 100 {
		activity = activity[len(activity)-100:]
	}
	recentStart := max(0, len(activity)-20)
	recent := make([]eventSummaryResponse, len(activity)-recentStart)
	copy(recent, activity[recentStart:])
	return activity, recent, tail.Reset, tail.Reset
}

func operatorRequestResponses(source []domain.OperatorRequest) []operatorRequestResponse {
	result := make([]operatorRequestResponse, 0, len(source))
	for requestIndex, request := range source {
		choices := operatorChoiceResponses(request.Choices)
		details := make([]operatorDetailResponse, 0, min(len(request.Details), maximumRequestItems))
		for _, detail := range request.Details[:min(len(request.Details), maximumRequestItems)] {
			details = append(details, operatorDetailResponse{Label: cleanDisplayValue(detail.Label, maximumShortTextBytes), Value: cleanDisplayValue(detail.Value, 16<<10)})
		}
		questions := make([]operatorQuestionResponse, 0, min(len(request.Questions), maximumRequestItems))
		for questionIndex, question := range request.Questions[:min(len(request.Questions), maximumRequestItems)] {
			questions = append(questions, operatorQuestionResponse{
				ID: cleanMachine(question.ID, maximumShortTextBytes), Label: cleanDisplayValue(question.Label, maximumDisplayBytes), Description: cleanDisplayValue(question.Description, 2<<10),
				Required: question.Required, AllowsMultiple: question.AllowsMultiple, AllowsOther: question.AllowsOther, IsSecret: question.IsSecret,
				Choices: operatorChoiceResponses(question.Choices), DOMID: "operator-question-" + strconv.Itoa(requestIndex+1) + "-" + strconv.Itoa(questionIndex+1),
			})
		}
		response := operatorRequestResponse{
			RequestID: cleanMachine(request.ID, maximumShortTextBytes), IssueIdentifier: cleanMachine(request.IssueIdentifier, maximumShortTextBytes), Kind: cleanMachine(request.Kind, maximumShortTextBytes),
			Title: cleanDisplayValue(request.Title, maximumDisplayBytes), Summary: cleanDisplayValue(request.Summary, 2<<10), OpenedAt: request.OpenedAt, WarningAt: request.WarningAt, DeadlineAt: request.DeadlineAt,
			Details: details, ExtensionsUsed: request.ExtensionsUsed, ExtensionsRemaining: request.ExtensionsRemaining, Choices: choices, Questions: questions,
			SessionID: cleanMachine(request.SessionID, maximumDisplayBytes), DOMID: "operator-request-" + strconv.Itoa(requestIndex+1),
		}
		response.RespondPath = "/api/v1/requests/" + url.PathEscape(response.RequestID) + "/respond"
		response.ExtendPath = "/api/v1/requests/" + url.PathEscape(response.RequestID) + "/extend"
		response.DeadlineDateTime, response.DeadlineDisplayTime = semanticTimeStrings(response.DeadlineAt)
		result = append(result, response)
	}
	return result
}

func safeAPIStatusMessage(value string, sourceHasError bool, safeCode, fallback string) string {
	if sourceHasError && safeCode == "" {
		return fallback
	}
	return cleanDisplayValue(value, maximumDisplayBytes)
}

func operatorChoiceResponses(source []domain.OperatorChoice) []operatorChoiceResponse {
	limit := min(len(source), maximumRequestItems)
	result := make([]operatorChoiceResponse, 0, limit)
	for _, choice := range source[:limit] {
		result = append(result, operatorChoiceResponse{ID: cleanMachine(choice.ID, maximumShortTextBytes), Label: cleanDisplayValue(choice.Label, maximumDisplayBytes), Description: cleanDisplayValue(choice.Description, 2<<10)})
	}
	return result
}

func issueResponseFrom(detail domain.IssueDetail) issueResponse {
	description, truncated := "", false
	var descriptionPointer *string
	if detail.Issue.Description != nil {
		description, truncated = cleanDisplay(*detail.Issue.Description, maximumDescription)
		descriptionPointer = &description
	}
	labels := make([]string, 0, min(len(detail.Issue.Labels), 50))
	for _, label := range detail.Issue.Labels[:min(len(detail.Issue.Labels), 50)] {
		labels = append(labels, cleanDisplayValue(label, maximumShortTextBytes))
	}
	blockers := make([]issueBlockerResponse, 0, min(len(detail.Issue.BlockedBy), 100))
	for _, blocker := range detail.Issue.BlockedBy[:min(len(detail.Issue.BlockedBy), 100)] {
		blockers = append(blockers, issueBlockerResponse{Identifier: cleanOptional(blocker.Identifier, maximumIdentifierBytes), State: cleanOptional(blocker.State, maximumShortTextBytes)})
	}
	response := issueResponse{
		IssueIdentifier: cleanMachine(detail.Issue.Identifier, maximumIdentifierBytes), IssueID: cleanMachine(detail.Issue.ID, maximumIdentifierBytes), Status: issueDetailStatus(detail),
		Workspace: nil, Attempts: issueAttemptsResponse{}, Running: nil, Retry: nil, Logs: issueLogsResponse{CodexSessionLogs: []logRecordView{}}, RecentEvents: []eventSummaryResponse{}, LastError: nil, Tracked: emptyResponse{},
		Issue: issueSummaryResponse{
			Identifier: cleanMachine(detail.Issue.Identifier, maximumIdentifierBytes), Title: cleanDisplayValue(detail.Issue.Title, maximumDisplayBytes), Description: descriptionPointer, DescriptionTruncated: truncated,
			Priority: cloneIntPointer(detail.Issue.Priority), State: cleanDisplayValue(detail.Issue.State, maximumShortTextBytes), URL: validatedTrackerURL(detail.Issue.URL), Labels: labels, Blockers: blockers,
			CreatedAt: cloneTimePointer(detail.Issue.CreatedAt), UpdatedAt: cloneTimePointer(detail.Issue.UpdatedAt),
		},
		Eligibility: issueEligibilityResponse{Routable: detail.Routable, Reasons: routingReasonResponses(detail.RoutingReasons)},
	}
	if detail.Running != nil {
		running := runningResponseFrom(*detail.Running)
		response.Running = &running
	}
	if detail.Workspace != nil {
		response.Workspace = &workspaceResponse{
			Path: cleanMachine(detail.Workspace.Path, maximumWorkspacePath), Key: cleanMachine(detail.Workspace.Key, maximumIdentifierBytes),
			CreatedNow: detail.Workspace.CreatedNow, Owned: detail.Workspace.Owned,
		}
	}
	if detail.Attempt != nil && *detail.Attempt >= 0 {
		response.Attempts.RestartCount = *detail.Attempt
		response.Attempts.CurrentRetryAttempt = *detail.Attempt
	}
	if detail.Retry != nil {
		retry := retryResponseFrom(*detail.Retry)
		response.Retry = &retry
		response.Attempts.CurrentRetryAttempt = retry.Attempt
		response.Attempts.RestartCount = retry.Attempt
	}
	populateIssueTimeViews(&response.Issue)
	return response
}

func issueDetailStatus(detail domain.IssueDetail) string {
	switch detail.Status {
	case "candidate", "running", "retrying", "stalled", "stopping", "stopping_failed", "preparing_workspace", "building_prompt", "launching_agent_process", "initializing_session", "streaming_turn", "finishing":
		return detail.Status
	}
	if detail.Running != nil {
		return "running"
	}
	if detail.Retry != nil {
		return "retrying"
	}
	return "candidate"
}

func boundedIssueLogRecordViews(identifier string, records []observability.LogRecord, limit int) []logRecordView {
	views := issueLogRecordViews(identifier, records)
	if limit >= 0 && len(views) > limit {
		views = views[:limit]
	}
	return views
}

func issueEventResponses(events []domain.Event, issueID, identifier string) []eventSummaryResponse {
	result := make([]eventSummaryResponse, 0, min(len(events), 20))
	for index := len(events) - 1; index >= 0 && len(result) < 20; index-- {
		data := events[index].Data
		matches := false
		if value, ok := data["issue_id"].(string); ok && value == issueID {
			matches = true
		}
		if value, ok := data["issue_identifier"].(string); ok && value == identifier {
			matches = true
		}
		if matches {
			result = append(result, eventSummary(events[index]))
		}
	}
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result
}

func cleanOptional(value *string, maximum int) *string {
	if value == nil {
		return nil
	}
	cleaned := cleanDisplayValue(*value, maximum)
	return &cleaned
}

func cleanDisplayValue(value string, maximum int) string {
	cleaned, _ := cleanDisplay(value, maximum)
	return cleaned
}

func parseUnsignedCursor(value string) uint64 {
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}
