package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/app"
	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/tracker"
)

func TestPageHandlerReadsEntropyOnceAndBuildsStableUniqueAPIErrorEnvelopes(t *testing.T) {
	reader := &exactEntropyReader{remaining: bytes.Repeat([]byte{0x5a}, 32), chunk: 3}
	handler, err := newPageHandlerWithEntropy(PageOptions{}, reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.remaining) != 0 {
		t.Fatalf("constructor left %d seed bytes unread", len(reader.remaining))
	}
	first := httptest.NewRecorder()
	handler.RespondRequestError(first, httptest.NewRequest(http.MethodGet, "/api/v1/unknown", nil), http.StatusNotFound)
	var firstEnvelope apiErrorResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstEnvelope); err != nil {
		t.Fatal(err)
	}
	if firstEnvelope.Error.CorrelationID != "ed6985a1a7556a53009cc29217bc26fd" {
		t.Fatalf("fixed-seed first correlation ID = %q", firstEnvelope.Error.CorrelationID)
	}

	const requests = 64
	identifiers := make(chan string, requests)
	var group sync.WaitGroup
	for range requests {
		group.Add(1)
		go func() {
			defer group.Done()
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/api/v1/unknown?credential=credential-canary", nil)
			handler.RespondRequestError(recorder, request, http.StatusNotFound)
			response := recorder.Result()
			var envelope struct {
				Error struct {
					Code          string `json:"code"`
					Message       string `json:"message"`
					CorrelationID string `json:"correlation_id"`
					Retryable     bool   `json:"retryable"`
				} `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Errorf("decode error envelope: %v", err)
				return
			}
			id := envelope.Error.CorrelationID
			if response.StatusCode != http.StatusNotFound || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("X-Correlation-ID") != id {
				t.Errorf("error response status/headers = %d/%q/%q/%q", response.StatusCode, response.Header.Get("Cache-Control"), response.Header.Get("Content-Type"), response.Header.Get("X-Correlation-ID"))
				return
			}
			if envelope.Error.Code != "not_found" || envelope.Error.Message != "The requested API route was not found." || envelope.Error.Retryable || len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
				t.Errorf("error envelope = %#v", envelope.Error)
				return
			}
			identifiers <- id
		}()
	}
	group.Wait()
	close(identifiers)
	seen := make(map[string]struct{}, requests)
	for id := range identifiers {
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate correlation ID %q", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != requests {
		t.Fatalf("unique correlation IDs = %d, want %d", len(seen), requests)
	}

	if _, err := newPageHandlerWithEntropy(PageOptions{}, io.LimitReader(strings.NewReader(strings.Repeat("x", 31)), 31)); err == nil {
		t.Fatal("short entropy reader did not fail handler construction")
	}
	entropyErr := errors.New("entropy-read-canary")
	if _, err := newPageHandlerWithEntropy(PageOptions{}, &failingEntropyReader{remaining: 7, err: entropyErr}); !errors.Is(err, entropyErr) {
		t.Fatalf("non-EOF entropy error = %v", err)
	}
}

func TestCompatibilityHandlerStateUsesCanonicalConsistentEmptyCursor(t *testing.T) {
	handler, err := NewPageHandler()
	if err != nil {
		t.Fatal(err)
	}
	recorder := serveDirect(t, handler, http.MethodGet, "/api/v1/state", "", nil)
	var response struct {
		EventCursor       string         `json:"event_cursor"`
		RecentEvents      []domain.Event `json:"recent_events"`
		RecentEventsReset bool           `json:"recent_events_reset"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(response.EventCursor, ":")
	if recorder.Code != http.StatusOK || len(parts) != 2 || len(parts[0]) != 32 || strings.Trim(parts[0], "0123456789abcdef") != "" || parts[1] != "0" || response.RecentEvents == nil || len(response.RecentEvents) != 0 || response.RecentEventsReset {
		t.Fatalf("compatibility state cursor/events = %d/%q/%#v/%t", recorder.Code, response.EventCursor, response.RecentEvents, response.RecentEventsReset)
	}
}

func TestStateAPIUsesExactAllowlistedNonNullShapeAndCoherentEventSuffix(t *testing.T) {
	now := time.Date(2026, 8, 8, 13, 14, 15, 0, time.UTC)
	unsafeURL := "https://user:secret@example.invalid/issues/1?credential=credential-canary#fragment"
	safeURL := "https://tracker.example/issues/TEAM-2"
	priority := 2
	description := "description-canary"
	branch := "branch-canary"
	assignee := "assignee-canary"
	runtime := &pageRuntimeFake{
		snapshot: domain.Snapshot{
			GeneratedAt: now,
			EventCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 2},
			Scheduler:   domain.SchedulerStatus{Available: false, Enabled: false, State: "unavailable", Message: "Scheduler is unavailable."},
			Candidates: []domain.CandidateRow{
				{Issue: domain.Issue{ID: "internal-1", Identifier: "TEAM-1", Title: "Needs attention", Description: &description, Priority: &priority, State: "Open", URL: &unsafeURL, BranchName: &branch, AssigneeID: &assignee, NativeRef: map[string]any{"credential": "native-ref-canary"}, Labels: []string{"backend"}}, Routable: false, RoutingReasons: []string{"unknown-reason-canary"}},
				{Issue: domain.Issue{ID: "internal-2", Identifier: "TEAM-2", Title: "Ready", Priority: &priority, State: "Open", URL: &safeURL, Labels: []string{"frontend"}}, Routable: true, RoutingReasons: []string{}},
			},
			Running:     []domain.RunningRow{{IssueID: "internal-2", IssueIdentifier: "TEAM-2", SessionID: "session-path-canary", State: "running", StartedAt: now, LastEventAt: now, Tokens: domain.TokenTotals{InputTokens: 1, OutputTokens: 2, TotalTokens: 3, SecondsRunning: 99}}},
			Retrying:    []domain.RetryRow{{IssueID: "internal-3", IssueIdentifier: "TEAM-3", Attempt: 2, DueAt: now, Error: "credential-retry-canary"}},
			Requests:    []domain.OperatorRequest{{ID: "request-1", SessionID: "request-session-canary", IssueID: "provider-id-canary", IssueIdentifier: "TEAM-2", Kind: "choice", Title: "Choose", Summary: "A safe summary", OpenedAt: now, WarningAt: now.Add(time.Minute), DeadlineAt: now.Add(2 * time.Minute), Choices: []domain.OperatorChoice{{ID: "yes", Label: "Yes", Description: "Proceed"}}, Questions: []domain.OperatorQuestion{}}},
			CodexTotals: domain.TokenTotals{InputTokens: 4, OutputTokens: 5, TotalTokens: 9, SecondsRunning: 1.5},
			RateLimits:  map[string]any{"credential": "rate-limit-canary"},
			Config:      domain.ConfigStatus{State: "valid", Digest: "digest-a", ActiveDigest: "digest-a", ChangedAt: now},
			Tracker:     domain.TrackerStatus{Kind: "github", Scope: "example/project", State: "ready", LastAttemptAt: &now, LastSuccessAt: &now},
		},
		recent: domain.EventPage{Events: []domain.Event{
			{Epoch: "epoch-a", Sequence: 1, Type: "queue.refreshed", At: now.Add(-time.Minute), Data: map[string]any{"credential": "event-data-canary"}},
			{Epoch: "epoch-a", Sequence: 2, Type: "unknown-event-canary", At: now, Data: map[string]any{"native": "event-native-canary"}},
		}, LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 2}},
	}
	handler := newTestPageHandler(t, PageOptions{Mode: "run", Queries: runtime, Commands: runtime})
	recorder := serveDirect(t, handler, http.MethodGet, "/api/v1/state", "", nil)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("state status/headers = %d/%q/%q", recorder.Code, recorder.Header().Get("Cache-Control"), recorder.Header().Get("Content-Type"))
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &top); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, top, "activity_events", "activity_events_reset", "candidates", "codex_totals", "config", "counts", "event_cursor", "generated_at", "rate_limits", "recent_events", "recent_events_reset", "requests", "retrying", "running", "scheduler", "tracker")
	for _, collection := range []string{"activity_events", "candidates", "recent_events", "requests", "retrying", "running"} {
		if bytes.Equal(top[collection], []byte("null")) {
			t.Fatalf("%s was null", collection)
		}
	}
	if string(top["rate_limits"]) != "null" {
		t.Fatalf("rate_limits = %s, want null", top["rate_limits"])
	}

	var candidates []map[string]json.RawMessage
	if err := json.Unmarshal(top["candidates"], &candidates); err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidate count = %d", len(candidates))
	}
	assertExactJSONKeys(t, candidates[0], "created_at", "identifier", "issue_id", "labels", "needs_attention", "priority", "routable", "routing_reasons", "stale", "state", "title", "updated_at", "url")
	var reasons []map[string]json.RawMessage
	if err := json.Unmarshal(candidates[0]["routing_reasons"], &reasons); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, reasons[0], "code", "message")

	var counts map[string]json.RawMessage
	if err := json.Unmarshal(top["counts"], &counts); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, counts, "candidates", "errors", "needs_attention", "requests", "retrying", "routable", "running")
	var running []map[string]json.RawMessage
	if err := json.Unmarshal(top["running"], &running); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, running[0], "issue_id", "issue_identifier", "issue_url", "last_event", "last_event_at", "last_message", "started_at", "state", "tokens", "turn_count")
	var runningTokens map[string]json.RawMessage
	if err := json.Unmarshal(running[0]["tokens"], &runningTokens); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, runningTokens, "input_tokens", "output_tokens", "total_tokens")
	var retrying []map[string]json.RawMessage
	if err := json.Unmarshal(top["retrying"], &retrying); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, retrying[0], "attempt", "due_at", "error", "issue_id", "issue_identifier", "issue_url")
	if string(retrying[0]["error"]) != `"A retry is scheduled."` {
		t.Fatalf("retry error = %s", retrying[0]["error"])
	}
	var totals map[string]json.RawMessage
	if err := json.Unmarshal(top["codex_totals"], &totals); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, totals, "input_tokens", "output_tokens", "seconds_running", "total_tokens")
	var requests []map[string]json.RawMessage
	if err := json.Unmarshal(top["requests"], &requests); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, requests[0], "choices", "deadline_at", "extensions_remaining", "extensions_used", "issue_identifier", "kind", "opened_at", "questions", "request_id", "summary", "title", "warning_at")
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(requests[0]["choices"], &choices); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, choices[0], "description", "id", "label")
	var scheduler map[string]json.RawMessage
	if err := json.Unmarshal(top["scheduler"], &scheduler); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, scheduler, "available", "enabled", "message", "state")
	var config map[string]json.RawMessage
	if err := json.Unmarshal(top["config"], &config); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, config, "active_digest", "changed_at", "digest", "error_code", "has_error", "message", "state", "using_last_good")
	var trackerStatus map[string]json.RawMessage
	if err := json.Unmarshal(top["tracker"], &trackerStatus); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, trackerStatus, "error_code", "has_error", "kind", "last_attempt_at", "last_success_at", "message", "retry_at", "retryable", "scope", "stale", "state")

	var events []map[string]json.RawMessage
	if err := json.Unmarshal(top["recent_events"], &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("event count = %d", len(events))
	}
	for _, event := range events {
		assertExactJSONKeys(t, event, "at", "code", "event_cursor", "summary", "type")
	}
	var activityEvents []map[string]json.RawMessage
	if err := json.Unmarshal(top["activity_events"], &activityEvents); err != nil {
		t.Fatal(err)
	}
	if len(activityEvents) != 2 || string(top["activity_events_reset"]) != "false" {
		t.Fatalf("activity event view = %d/%s", len(activityEvents), top["activity_events_reset"])
	}
	if runtime.callOrderString() != "snapshot,recent:100" {
		t.Fatalf("runtime call order = %q", runtime.callOrderString())
	}
	body := recorder.Body.String()
	for _, canary := range []string{"credential-canary", "credential-retry-canary", "native-ref-canary", "branch-canary", "assignee-canary", "session-path-canary", "request-session-canary", "provider-id-canary", "rate-limit-canary", "event-data-canary", "event-native-canary", "unknown-reason-canary", "unknown-event-canary", "description-canary"} {
		if strings.Contains(body, canary) {
			t.Fatalf("state response exposed %q", canary)
		}
	}
}

func TestStateAPICoherentEventTailResetEpochAndDisplacementRules(t *testing.T) {
	now := time.Date(2026, 8, 8, 13, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		cursor    domain.EventCursor
		tail      domain.EventPage
		wantCount int
		wantReset bool
	}{
		{
			name: "retained reset suffix", cursor: domain.EventCursor{Epoch: "same", Sequence: 2}, wantCount: 2, wantReset: true,
			tail: domain.EventPage{Reset: true, LatestCursor: domain.EventCursor{Epoch: "same", Sequence: 2}, Events: []domain.Event{
				{Epoch: "same", Sequence: 1, Type: "queue.refreshed", At: now, Data: map[string]any{}},
				{Epoch: "same", Sequence: 2, Type: "queue.failed", At: now, Data: map[string]any{"error_code": "refresh_\x00failed-canary", "secret": "credential-event-code-canary"}},
			}},
		},
		{
			name: "displaced snapshot cursor", cursor: domain.EventCursor{Epoch: "same", Sequence: 2}, wantReset: true,
			tail: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "same", Sequence: 4}, Events: []domain.Event{
				{Epoch: "same", Sequence: 3, At: now, Data: map[string]any{}},
				{Epoch: "same", Sequence: 4, At: now, Data: map[string]any{}},
			}},
		},
		{
			name: "epoch mismatch", cursor: domain.EventCursor{Epoch: "old", Sequence: 2}, wantReset: true,
			tail: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "new", Sequence: 2}, Events: []domain.Event{
				{Epoch: "new", Sequence: 2, At: now, Data: map[string]any{}},
			}},
		},
		{
			name: "future burst does not advance cursor", cursor: domain.EventCursor{Epoch: "same", Sequence: 2}, wantCount: 2,
			tail: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "same", Sequence: 3}, Events: []domain.Event{
				{Epoch: "same", Sequence: 1, At: now, Data: map[string]any{}},
				{Epoch: "same", Sequence: 2, At: now, Data: map[string]any{}},
				{Epoch: "same", Sequence: 3, At: now, Data: map[string]any{}},
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := &pageRuntimeFake{snapshot: domain.Snapshot{EventCursor: test.cursor}, recent: test.tail}
			handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
			recorder := serveDirect(t, handler, http.MethodGet, "/api/v1/state", "", nil)
			var response struct {
				EventCursor         string                 `json:"event_cursor"`
				RecentEvents        []eventSummaryResponse `json:"recent_events"`
				RecentEventsReset   bool                   `json:"recent_events_reset"`
				ActivityEvents      []eventSummaryResponse `json:"activity_events"`
				ActivityEventsReset bool                   `json:"activity_events_reset"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.EventCursor != eventCursorString(test.cursor) || len(response.RecentEvents) != test.wantCount || len(response.ActivityEvents) != test.wantCount || response.RecentEventsReset != test.wantReset || response.ActivityEventsReset != test.wantReset || strings.Contains(recorder.Body.String(), "canary") {
				t.Fatalf("coherent response = %#v body=%s", response, recorder.Body.String())
			}
		})
	}
}

func TestStateAPIMarshalFailureReturnsOnlyCleanInternalError(t *testing.T) {
	runtime := &pageRuntimeFake{snapshot: domain.Snapshot{GeneratedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), EventCursor: domain.EventCursor{Epoch: "epoch"}}, recent: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch"}, Events: []domain.Event{}}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	recorder := serveDirect(t, handler, http.MethodGet, "/api/v1/state", "", nil)
	if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), "generated_at") || strings.Count(recorder.Body.String(), `"error"`) != 1 || !strings.Contains(recorder.Body.String(), `"code":"internal_error"`) {
		t.Fatalf("marshal failure response = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestStateAPIKeepsWholeSnapshotRequestsAndCountsUnknownSourceErrors(t *testing.T) {
	requests := make([]domain.OperatorRequest, 101)
	for index := range requests {
		requests[index] = domain.OperatorRequest{ID: "request-" + jsonNumber(index), Choices: []domain.OperatorChoice{}, Questions: []domain.OperatorQuestion{}}
	}
	requests[0].IssueIdentifier = strings.Repeat("x", 200)
	runtime := &pageRuntimeFake{snapshot: domain.Snapshot{
		EventCursor: domain.EventCursor{Epoch: "epoch-a"},
		Requests:    requests,
		Config:      domain.ConfigStatus{ErrorCode: "future-code-canary", Message: "provider-secret-canary"},
	}, recent: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a"}, Events: []domain.Event{}}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	recorder := serveDirect(t, handler, http.MethodGet, "/api/v1/state", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("state status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Counts struct {
			Requests int `json:"requests"`
			Errors   int `json:"errors"`
		} `json:"counts"`
		Requests []map[string]json.RawMessage `json:"requests"`
		Config   struct {
			ErrorCode string `json:"error_code"`
			Message   string `json:"message"`
		} `json:"config"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Counts.Requests != 101 || len(response.Requests) != 101 {
		t.Fatalf("request count/summaries = %d/%d, want 101/101", response.Counts.Requests, len(response.Requests))
	}
	var firstIdentifier string
	if err := json.Unmarshal(response.Requests[0]["issue_identifier"], &firstIdentifier); err != nil {
		t.Fatal(err)
	}
	if len(firstIdentifier) != maximumShortTextBytes {
		t.Fatalf("request issue identifier length = %d, want %d", len(firstIdentifier), maximumShortTextBytes)
	}
	if response.Counts.Errors != 1 {
		t.Fatalf("error count = %d, want 1 for a nonempty source error code", response.Counts.Errors)
	}
	if response.Config.ErrorCode != "" || strings.Contains(response.Config.Message, "provider-secret-canary") {
		t.Fatalf("unknown provider error was reflected: %#v", response.Config)
	}
}

func TestStateAPIEmitsCommittedTrackerErrorCodeAndCountsIt(t *testing.T) {
	for _, test := range []struct {
		code    string
		message string
	}{
		{code: "tracker_error", message: "Tracker operation failed."},
		{code: "tracker_scope", message: "Tracker scope is unavailable."},
	} {
		t.Run(test.code, func(t *testing.T) {
			runtime := &pageRuntimeFake{snapshot: domain.Snapshot{
				EventCursor: domain.EventCursor{Epoch: "epoch-a"},
				Tracker:     domain.TrackerStatus{ErrorCode: test.code, Message: test.message},
			}, recent: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a"}, Events: []domain.Event{}}}
			handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
			recorder := serveDirect(t, handler, http.MethodGet, "/api/v1/state", "", nil)
			var response struct {
				Counts struct {
					Errors int `json:"errors"`
				} `json:"counts"`
				Tracker struct {
					ErrorCode string `json:"error_code"`
					Message   string `json:"message"`
				} `json:"tracker"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Counts.Errors != 1 || response.Tracker.ErrorCode != test.code || response.Tracker.Message != test.message {
				t.Fatalf("tracker failure = %d/%#v", response.Counts.Errors, response.Tracker)
			}
		})
	}
}

func TestStateAPIRejectsMalformedStatusCodesBeforeAllowlisting(t *testing.T) {
	tests := []string{
		"tracker_\x00auth",
		"tracker_\u0085auth",
		" tracker_auth",
		"tracker_auth ",
		strings.Repeat(" ", 60) + "tracker_auth",
	}
	for _, target := range []string{"config", "tracker"} {
		for index, malformed := range tests {
			t.Run(target+"-"+jsonNumber(index), func(t *testing.T) {
				snapshot := domain.Snapshot{EventCursor: domain.EventCursor{Epoch: "epoch-a"}}
				if target == "config" {
					snapshot.Config = domain.ConfigStatus{ErrorCode: malformed, Message: "credential-status-message-canary"}
				} else {
					snapshot.Tracker = domain.TrackerStatus{ErrorCode: malformed, Message: "credential-status-message-canary"}
				}
				runtime := &pageRuntimeFake{snapshot: snapshot, recent: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a"}, Events: []domain.Event{}}}
				handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
				recorder := serveDirect(t, handler, http.MethodGet, "/api/v1/state", "", nil)
				var response struct {
					Counts struct {
						Errors int `json:"errors"`
					} `json:"counts"`
					Config struct {
						ErrorCode string `json:"error_code"`
						Message   string `json:"message"`
					} `json:"config"`
					Tracker struct {
						ErrorCode string `json:"error_code"`
						Message   string `json:"message"`
					} `json:"tracker"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				status := response.Tracker
				fallback := "Tracker status needs attention."
				if target == "config" {
					status = response.Config
					fallback = "Configuration status needs attention."
				}
				if response.Counts.Errors != 1 || status.ErrorCode != "" || status.Message != fallback || strings.Contains(recorder.Body.String(), "credential-status-message-canary") {
					t.Fatalf("malformed status code was normalized into trust: %#v body=%s", response, recorder.Body.String())
				}
			})
		}
	}
}

func TestIssueAPIRoundTripsOneEncodedSegmentAndReturnsExactPhaseTwoShape(t *testing.T) {
	identifier := "TEAM/#42"
	description := "Issue description"
	priority := 1
	safeURL := "https://tracker.example/issues/TEAM-42"
	blockerIdentifier := "TEAM-1"
	blockerState := "Open"
	blockerID := "raw-blocker-id-canary"
	runtime := &pageRuntimeFake{details: map[string]domain.IssueDetail{
		identifier: {
			Issue:  domain.Issue{ID: "issue-42", Identifier: identifier, Title: "Encoded issue", Description: &description, Priority: &priority, State: "Open", URL: &safeURL, Labels: []string{"one"}, BlockedBy: []domain.BlockerRef{{ID: &blockerID, Identifier: &blockerIdentifier, State: &blockerState}}, NativeRef: map[string]any{"secret": "native-canary"}},
			Status: "candidate", Routable: false, RoutingReasons: []string{"missing_required_label"},
		},
	}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	recorder := serveDirect(t, handler, http.MethodGet, "/api/v1/TEAM%2F%2342", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("issue status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if runtime.lastIssue != identifier {
		t.Fatalf("runtime issue identifier = %q", runtime.lastIssue)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &top); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, top, "attempts", "eligibility", "issue", "issue_id", "issue_identifier", "last_error", "logs", "recent_events", "retry", "running", "status", "tracked", "workspace")
	if string(top["workspace"]) != "null" || string(top["running"]) != "null" || string(top["retry"]) != "null" || string(top["last_error"]) != "null" || string(top["tracked"]) != "{}" || string(top["recent_events"]) != "[]" {
		t.Fatalf("phase-two constants = workspace:%s running:%s retry:%s last_error:%s tracked:%s recent:%s", top["workspace"], top["running"], top["retry"], top["last_error"], top["tracked"], top["recent_events"])
	}
	body := recorder.Body.String()
	for _, canary := range []string{"native-canary", "raw-blocker-id-canary"} {
		if strings.Contains(body, canary) {
			t.Fatalf("issue response exposed %q", canary)
		}
	}
}

func TestValidatedTrackerURLRequiresANonemptyHostname(t *testing.T) {
	for _, value := range []string{"https://:443/path", "https://:/path"} {
		if got := validatedTrackerURL(&value); got != nil {
			t.Fatalf("hostname-free tracker URL %q was accepted as %q", value, *got)
		}
	}
	valid := "https://tracker.example:443/path"
	if got := validatedTrackerURL(&valid); got == nil || *got != valid {
		t.Fatalf("valid tracker URL = %#v", got)
	}
}

func TestProtectedAPIBoundaryAuthenticatesBeforeDependenciesAndOrdersMutationChecks(t *testing.T) {
	runtime := &pageRuntimeFake{snapshot: domain.Snapshot{EventCursor: domain.EventCursor{Epoch: "epoch-a"}}, recent: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a"}, Events: []domain.Event{}}, receipt: domain.RefreshReceipt{Queued: true, Operations: []string{"poll"}}}
	logs := &logQueryFake{page: observability.LogPage{Records: []observability.LogRecord{}}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime, Logs: logs})
	server := startTestServerWithErrorResponder(t, bootstrapFromValue("protected-api-capability"), handler, handler)
	plainURL := strings.Split(server.bound.URL, "?")[0]

	unauthenticated := request(t, server.client, http.MethodGet, plainURL+"api/v1/state", nil, nil)
	assertStableAPIError(t, unauthenticated, http.StatusUnauthorized, "unauthorized", "Authentication is required.", false)
	if runtime.callOrderString() != "" || runtime.refreshCalls.Load() != 0 || logs.calls.Load() != 0 {
		t.Fatal("unauthenticated API request invoked application dependencies")
	}

	cookie := exchange(t, server)
	wrongState := authenticatedRequest(t, server, cookie, http.MethodPost, "/api/v1/state", nil, nil)
	assertStableAPIError(t, wrongState, http.StatusMethodNotAllowed, "method_not_allowed", "The method is not allowed for this route.", false)
	if wrongState.Header.Get("Allow") != "GET, HEAD" {
		t.Fatalf("state Allow = %q", wrongState.Header.Get("Allow"))
	}
	wrongRefresh := authenticatedRequest(t, server, cookie, http.MethodGet, "/api/v1/refresh", nil, nil)
	assertStableAPIError(t, wrongRefresh, http.StatusMethodNotAllowed, "method_not_allowed", "The method is not allowed for this route.", false)
	if wrongRefresh.Header.Get("Allow") != "POST" || runtime.refreshCalls.Load() != 0 {
		t.Fatalf("refresh wrong-method policy = %q/calls=%d", wrongRefresh.Header.Get("Allow"), runtime.refreshCalls.Load())
	}

	for _, target := range []string{"api/v1/%73tate", "api/v1/%72efresh", "api/v1/state/", "api/v1/unknown/extra"} {
		response := requestWithCookie(t, server, cookie, http.MethodGet, plainURL+target, nil, nil)
		assertStableAPIError(t, response, http.StatusNotFound, "not_found", "The requested API route was not found.", false)
	}
	if runtime.callOrderString() != "" || runtime.refreshCalls.Load() != 0 || logs.calls.Load() != 0 {
		t.Fatal("method/noncanonical API requests invoked application dependencies")
	}

	unsupported := authenticatedRequest(t, server, cookie, http.MethodPost, "/api/v1/refresh", strings.NewReader("{}"), http.Header{"Content-Type": {"text/plain"}})
	assertStableAPIError(t, unsupported, http.StatusUnsupportedMediaType, "unsupported_media_type", "Use JSON or form data for this request.", false)
	missingCSRF := authenticatedRequest(t, server, cookie, http.MethodPost, "/api/v1/refresh", strings.NewReader("{}"), http.Header{"Content-Type": {"application/json"}})
	assertStableAPIError(t, missingCSRF, http.StatusForbidden, "forbidden", "The request was not allowed.", false)
	csrf := csrfForCookie(t, server, cookie)
	badOrigin := authenticatedRequest(t, server, cookie, http.MethodPost, "/api/v1/refresh", strings.NewReader("{}"), http.Header{"Content-Type": {"application/json"}, "X-CSRF-Token": {csrf}, "Origin": {"https://attacker.invalid"}})
	assertStableAPIError(t, badOrigin, http.StatusForbidden, "forbidden", "The request was not allowed.", false)
	if runtime.refreshCalls.Load() != 0 {
		t.Fatal("rejected refresh invoked command")
	}

	malformed := authenticatedRequest(t, server, cookie, http.MethodPost, "/api/v1/refresh", strings.NewReader(`{"member":true}`), http.Header{"Content-Type": {"application/json"}, "X-CSRF-Token": {csrf}})
	assertStableAPIError(t, malformed, http.StatusBadRequest, "invalid_request", "The request body is invalid.", false)
	if runtime.refreshCalls.Load() != 0 {
		t.Fatal("malformed refresh invoked command")
	}
	accepted := authenticatedRequest(t, server, cookie, http.MethodPost, "/api/v1/refresh", strings.NewReader("{}"), http.Header{"Content-Type": {"application/json"}, "X-CSRF-Token": {csrf}})
	if accepted.StatusCode != http.StatusAccepted || runtime.refreshCalls.Load() != 1 {
		t.Fatalf("accepted refresh = %d/calls=%d body=%s", accepted.StatusCode, runtime.refreshCalls.Load(), readResponse(t, accepted))
	}
	disabledConfiguration := authenticatedRequest(t, server, cookie, http.MethodPost, "/api/v1/config/save", strings.NewReader("{}"), http.Header{"Content-Type": {"application/json"}, "X-CSRF-Token": {csrf}})
	assertStableAPIError(t, disabledConfiguration, http.StatusServiceUnavailable, "runtime_unavailable", "Runtime state is temporarily unavailable.", true)

	invalidHostRequest, err := http.NewRequest(http.MethodGet, plainURL+"api/v1/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	invalidHostRequest.Host = "example.invalid:1"
	invalidHost := doRequest(t, server, invalidHostRequest)
	assertStableAPIError(t, invalidHost, http.StatusBadRequest, "invalid_request", "The request is invalid.", false)
}

func TestIssueAPIRejectsNoncanonicalStaticEscapesAndAcceptsLiteralPercentIdentifier(t *testing.T) {
	runtime := &pageRuntimeFake{details: map[string]domain.IssueDetail{
		"%2F": {Issue: domain.Issue{ID: "percent", Identifier: "%2F", Labels: []string{}, BlockedBy: []domain.BlockerRef{}}, RoutingReasons: []string{}},
	}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	for _, target := range []string{"/api/v1/%73tate", "/api/v1/%72efresh", "/api/v1/%72efresh", "/api/v1/TEAM%2f%2342"} {
		recorder := serveDirect(t, handler, http.MethodGet, target, "", nil)
		if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), `"code":"not_found"`) {
			t.Fatalf("noncanonical target %q = %d %s", target, recorder.Code, recorder.Body.String())
		}
	}
	if runtime.callOrderString() != "" || runtime.lastIssue != "" {
		t.Fatalf("noncanonical paths invoked runtime: order=%q issue=%q", runtime.callOrderString(), runtime.lastIssue)
	}
	recorder := serveDirect(t, handler, http.MethodGet, "/api/v1/%252F", "", nil)
	if recorder.Code != http.StatusOK || runtime.lastIssue != "%2F" {
		t.Fatalf("literal percent identifier = %d/%q body=%s", recorder.Code, runtime.lastIssue, recorder.Body.String())
	}
}

func TestIssueAPIRejectsMalformedIdentifiersBeforeRuntimeAccess(t *testing.T) {
	runtime := &pageRuntimeFake{details: map[string]domain.IssueDetail{}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	invalid := []string{"", " leading", "trailing ", "TEAM\x00ONE", "TEAM\u0085ONE", string([]byte{0xff}), strings.Repeat("x", 257)}
	for index, identifier := range invalid {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/placeholder", nil)
		request.SetPathValue("issue_identifier", identifier)
		recorder := httptest.NewRecorder()
		handler.issueAPI(recorder, request)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"invalid_identifier"`) {
			t.Fatalf("invalid identifier %d = %d %s", index, recorder.Code, recorder.Body.String())
		}
	}
	if runtime.issueCalls.Load() != 0 {
		t.Fatalf("invalid identifiers invoked Issue %d times", runtime.issueCalls.Load())
	}
}

func TestRefreshAPIAcceptsOnlyWhitespaceOrOneEmptyJSONObject(t *testing.T) {
	runtime := &pageRuntimeFake{receipt: domain.RefreshReceipt{Queued: true, RequestedAt: time.Date(2026, 8, 8, 13, 0, 0, 0, time.UTC), Operations: []string{"poll"}}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	for _, body := range []string{"", " \t\r\n", "{}", " { \n } \r\n"} {
		recorder := serveDirect(t, handler, http.MethodPost, "/api/v1/refresh", body, map[string]string{"Content-Type": "application/json"})
		if recorder.Code != http.StatusAccepted {
			t.Fatalf("accepted body %q status=%d response=%s", body, recorder.Code, recorder.Body.String())
		}
	}
	acceptedCalls := runtime.refreshCalls.Load()
	invalid := []string{"{\"member\":1}", "{}{}", "{} trailing", "[ ]", "null", "{", strings.Repeat(" ", 1025)}
	for _, body := range invalid {
		recorder := serveDirect(t, handler, http.MethodPost, "/api/v1/refresh", body, map[string]string{"Content-Type": "application/json"})
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("invalid body %q status=%d response=%s", body, recorder.Code, recorder.Body.String())
		}
	}
	if runtime.refreshCalls.Load() != acceptedCalls {
		t.Fatalf("invalid refresh bodies invoked command %d extra times", runtime.refreshCalls.Load()-acceptedCalls)
	}
}

func TestRefreshAPIMapsOnlyTrackerRetryability(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		status    int
		code      string
		retryable bool
	}{
		{name: "unavailable", err: app.ErrUnavailableInPhase, status: http.StatusConflict, code: "refresh_unavailable"},
		{name: "ordinary", err: errors.New("credential-refresh-canary"), status: http.StatusServiceUnavailable, code: "refresh_failed"},
		{name: "tracker value", err: tracker.Error{Category: tracker.CategoryTransport, Retryable: true}, status: http.StatusServiceUnavailable, code: "refresh_failed", retryable: true},
		{name: "tracker pointer", err: &tracker.Error{Category: tracker.CategoryResponse, Retryable: false}, status: http.StatusServiceUnavailable, code: "refresh_failed"},
		{name: "wrapped tracker", err: errors.Join(errors.New("outer"), &tracker.Error{Category: tracker.CategoryRateLimited, Retryable: true}), status: http.StatusServiceUnavailable, code: "refresh_failed", retryable: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := &pageRuntimeFake{refreshErr: test.err}
			handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
			recorder := serveDirect(t, handler, http.MethodPost, "/api/v1/refresh", "{}", map[string]string{"Content-Type": "application/json"})
			var envelope apiErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != test.status || envelope.Error.Code != test.code || envelope.Error.Retryable != test.retryable || strings.Contains(recorder.Body.String(), "credential-refresh-canary") {
				t.Fatalf("refresh error = %d/%#v", recorder.Code, envelope)
			}
		})
	}
}

func TestPageHandlerMethodPolicyUsesExactOrderedManifest(t *testing.T) {
	handler := newTestPageHandler(t, PageOptions{})
	cases := []struct {
		method  string
		target  string
		defined bool
		want    []string
	}{
		{http.MethodPost, "/api/v1/state", true, []string{http.MethodGet, http.MethodHead}},
		{http.MethodGet, "/api/v1/refresh", true, []string{http.MethodPost}},
		{http.MethodPost, "/api/v1/TEAM%2F%2342", true, []string{http.MethodGet, http.MethodHead}},
		{http.MethodPost, "/api/v1/config/save", true, []string{http.MethodPost}},
		{http.MethodGet, "/issues/TEAM%2F%2342", true, []string{http.MethodGet, http.MethodHead}},
		{http.MethodGet, "/api/v1/unknown/extra", false, nil},
		{http.MethodGet, "/api/v1/state/", false, nil},
		{http.MethodGet, "/api/v1/%73tate", false, nil},
		{http.MethodPost, "/api/v1/%72efresh", false, nil},
		{http.MethodGet, "/api/v1/TEAM%2f%2342", false, nil},
	}
	for _, test := range cases {
		request := httptest.NewRequest(test.method, test.target, nil)
		got, defined := handler.AllowedMethods(request)
		if defined != test.defined || !reflect.DeepEqual(got, test.want) {
			t.Errorf("%s %s policy = %#v/%t, want %#v/%t", test.method, test.target, got, defined, test.want, test.defined)
		}
	}
}

type exactEntropyReader struct {
	remaining []byte
	chunk     int
}

type failingEntropyReader struct {
	remaining int
	err       error
}

func (reader *failingEntropyReader) Read(destination []byte) (int, error) {
	if reader.remaining == 0 {
		return 0, reader.err
	}
	count := min(len(destination), reader.remaining)
	for index := range count {
		destination[index] = 0x42
	}
	reader.remaining -= count
	return count, nil
}

func (reader *exactEntropyReader) Read(destination []byte) (int, error) {
	if len(reader.remaining) == 0 {
		panic("entropy reader called after the process seed was filled")
	}
	limit := min(len(destination), len(reader.remaining))
	if reader.chunk > 0 {
		limit = min(limit, reader.chunk)
	}
	copy(destination, reader.remaining[:limit])
	reader.remaining = reader.remaining[limit:]
	return limit, nil
}

type pageRuntimeFake struct {
	mu           sync.Mutex
	snapshot     domain.Snapshot
	snapshotErr  error
	details      map[string]domain.IssueDetail
	issueErr     error
	recent       domain.EventPage
	recentErr    error
	receipt      domain.RefreshReceipt
	refreshErr   error
	callOrder    []string
	lastIssue    string
	refreshCalls atomic.Int64
	issueCalls   atomic.Int64
}

func (runtime *pageRuntimeFake) Snapshot(context.Context) (domain.Snapshot, error) {
	runtime.mu.Lock()
	runtime.callOrder = append(runtime.callOrder, "snapshot")
	runtime.mu.Unlock()
	if runtime.snapshotErr != nil {
		return domain.Snapshot{}, runtime.snapshotErr
	}
	return runtime.snapshot.Clone()
}

func (runtime *pageRuntimeFake) Issue(_ context.Context, identifier string) (domain.IssueDetail, error) {
	runtime.issueCalls.Add(1)
	runtime.mu.Lock()
	runtime.lastIssue = identifier
	runtime.mu.Unlock()
	if runtime.issueErr != nil {
		return domain.IssueDetail{}, runtime.issueErr
	}
	detail, found := runtime.details[identifier]
	if !found {
		return domain.IssueDetail{}, app.ErrIssueNotFound
	}
	return detail.Clone()
}

func (*pageRuntimeFake) EventsAfter(context.Context, domain.EventCursor) (domain.EventPage, error) {
	return domain.EventPage{Events: []domain.Event{}}, nil
}

func (runtime *pageRuntimeFake) RecentEvents(_ context.Context, limit int) (domain.EventPage, error) {
	runtime.mu.Lock()
	runtime.callOrder = append(runtime.callOrder, "recent:"+jsonNumber(limit))
	runtime.mu.Unlock()
	if runtime.recentErr != nil {
		return domain.EventPage{}, runtime.recentErr
	}
	page := runtime.recent
	page.Events = append([]domain.Event{}, page.Events...)
	return page, nil
}

func (*pageRuntimeFake) SubscribeEvents(domain.EventCursor) <-chan struct{} {
	ready := make(chan struct{})
	close(ready)
	return ready
}

func (runtime *pageRuntimeFake) Refresh(context.Context) (domain.RefreshReceipt, error) {
	runtime.refreshCalls.Add(1)
	return runtime.receipt, runtime.refreshErr
}

func (*pageRuntimeFake) SetScheduler(context.Context, bool) error { return app.ErrUnavailableInPhase }
func (*pageRuntimeFake) Respond(context.Context, domain.OperatorResponse) error {
	return app.ErrUnavailableInPhase
}

func (runtime *pageRuntimeFake) callOrderString() string {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return strings.Join(runtime.callOrder, ",")
}

type logQueryFake struct {
	page  observability.LogPage
	err   error
	query observability.LogQuery
	calls atomic.Int64
}

func (logs *logQueryFake) Query(_ context.Context, query observability.LogQuery) (observability.LogPage, error) {
	logs.calls.Add(1)
	logs.query = query
	return logs.page, logs.err
}

func newTestPageHandler(t *testing.T, options PageOptions) *PageHandler {
	t.Helper()
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	handler, err := newPageHandlerWithEntropy(options, bytes.NewReader(bytes.Repeat([]byte{0x42}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func serveDirect(t *testing.T, handler http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	request = request.WithContext(context.WithValue(request.Context(), csrfContextKey{}, testCSRFToken))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func requestWithCookie(t *testing.T, server *runningTestServer, cookie *http.Cookie, method, rawURL string, body io.Reader, headers http.Header) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(cookie)
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	return doRequest(t, server, request)
}

func doRequest(t *testing.T, server *runningTestServer, request *http.Request) *http.Response {
	t.Helper()
	response, err := server.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func assertStableAPIError(t *testing.T, response *http.Response, status int, code, message string, retryable bool) {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode API error: %v", err)
	}
	assertExactJSONKeys(t, envelope, "error")
	var body map[string]json.RawMessage
	if err := json.Unmarshal(envelope["error"], &body); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, body, "code", "correlation_id", "message", "retryable")
	var got apiErrorBody
	if err := json.Unmarshal(envelope["error"], &got); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status || got.Code != code || got.Message != message || got.Retryable != retryable || len(got.CorrelationID) != 32 || response.Header.Get("X-Correlation-ID") != got.CorrelationID || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("API error = status:%d headers:%v body:%#v", response.StatusCode, response.Header, got)
	}
}

func assertExactJSONKeys(t *testing.T, object map[string]json.RawMessage, keys ...string) {
	t.Helper()
	want := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		want[key] = struct{}{}
	}
	if len(object) != len(want) {
		t.Fatalf("JSON keys = %#v, want %#v", reflect.ValueOf(object).MapKeys(), keys)
	}
	for key := range want {
		if _, found := object[key]; !found {
			t.Fatalf("JSON object missing key %q: %#v", key, reflect.ValueOf(object).MapKeys())
		}
	}
}

func jsonNumber(value int) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

var _ app.RuntimeQueries = (*pageRuntimeFake)(nil)
var _ app.RuntimeCommands = (*pageRuntimeFake)(nil)
var _ LogQueries = (*logQueryFake)(nil)
