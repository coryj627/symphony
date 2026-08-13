package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/app"
	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
)

func TestEventsRoutePrecedesTheIssueWildcardAndStreamsOneReset(t *testing.T) {
	runtime := &sseRuntimeFake{
		events: domain.EventPage{
			LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 4},
			Reset:        true,
			Events:       []domain.Event{},
		},
	}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})

	recorder := serveSSEDirect(t, handler, "/api/v1/events?after=old:0", nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("events status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "text/event-stream; charset=utf-8" {
		t.Fatalf("events content type = %q", contentType)
	}
	want := "id: epoch-a:4\nevent: reset\ndata: {\"cursor\":\"epoch-a:4\",\"reason\":\"snapshot_required\"}\n\n"
	if recorder.Body.String() != want {
		t.Fatalf("reset stream = %q, want %q", recorder.Body.String(), want)
	}
	if runtime.issueCalls != 0 || runtime.eventsCalls != 1 {
		t.Fatalf("issue/events calls = %d/%d", runtime.issueCalls, runtime.eventsCalls)
	}
}

func TestSSEResetForFutureEvictedAndRestartedCursorsIsOneRecordThenEOF(t *testing.T) {
	for _, name := range []string{"future sequence", "evicted sequence", "previous process epoch"} {
		t.Run(name, func(t *testing.T) {
			journal := observability.NewJournal(observability.JournalOptions{MaxEvents: 2, MaxBytes: 1 << 20})
			for sequence := range 3 {
				if _, err := journal.Publish(domain.Event{Type: "queue.refreshed", Data: map[string]any{"sequence": sequence + 1}}); err != nil {
					t.Fatal(err)
				}
			}
			latest := journal.Cursor()
			var after domain.EventCursor
			switch name {
			case "future sequence":
				after = domain.EventCursor{Epoch: latest.Epoch, Sequence: latest.Sequence + 1}
			case "evicted sequence":
				after = domain.EventCursor{Epoch: latest.Epoch, Sequence: 1}
			case "previous process epoch":
				after = domain.EventCursor{Epoch: "epoch-old", Sequence: latest.Sequence}
			}
			runtime := &sseJournalRuntime{journal: journal}
			handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
			target := "/api/v1/events?after=" + url.QueryEscape(after.Epoch+":"+strconv.FormatUint(after.Sequence, 10))

			recorder := serveSSEDirect(t, handler, target, nil)

			cursor := latest.Epoch + ":" + strconv.FormatUint(latest.Sequence, 10)
			want := "id: " + cursor + "\nevent: reset\ndata: {\"cursor\":\"" + cursor + "\",\"reason\":\"snapshot_required\"}\n\n"
			if recorder.Code != http.StatusOK || recorder.Body.String() != want || len(handler.events.clients) != 0 {
				t.Fatalf("reset status/body/slots = %d/%q/%d, want 200/%q/0", recorder.Code, recorder.Body.String(), len(handler.events.clients), want)
			}
		})
	}
}

func TestEventsCursorSourcePrecedenceUsesHeaderWithoutParsingStaleQuery(t *testing.T) {
	tests := []struct {
		name       string
		target     string
		headers    []string
		wantCursor domain.EventCursor
	}{
		{name: "query only", target: "/api/v1/events?after=epoch-a%3A10", wantCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 10}},
		{name: "equal header and query", target: "/api/v1/events?after=epoch-a%3A10", headers: []string{"epoch-a:10"}, wantCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 10}},
		{name: "newer native reconnect header", target: "/api/v1/events?after=epoch-a%3A10", headers: []string{"epoch-a:12"}, wantCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 12}},
		{name: "valid header ignores malformed duplicate query", target: "/api/v1/events?after=%ZZ&after=old%3A4", headers: []string{"epoch-a:12"}, wantCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 12}},
		{name: "missing query starts from empty cursor", target: "/api/v1/events", wantCursor: domain.EventCursor{}},
		{name: "one empty query starts from empty cursor", target: "/api/v1/events?after=", wantCursor: domain.EventCursor{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := &sseRuntimeFake{events: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 13}, Reset: true, Events: []domain.Event{}}}
			handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
			recorder := serveSSEDirect(t, handler, test.target, test.headers)
			if recorder.Code != http.StatusOK || runtime.eventsCalls != 1 || !reflect.DeepEqual(runtime.lastCursor, test.wantCursor) {
				t.Fatalf("status/calls/cursor = %d/%d/%#v, want 200/1/%#v; body=%s", recorder.Code, runtime.eventsCalls, runtime.lastCursor, test.wantCursor, recorder.Body.String())
			}
		})
	}
}

func TestSSELastEventIDReplaysOnlyTheRecordsAfterTheNativeReconnectCursor(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	runtime := &sseRuntimeFake{eventPages: []sseEventResult{
		{page: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 13}, Events: []domain.Event{
			{Epoch: "epoch-a", Sequence: 13, Type: "queue.refreshed", At: now, Data: map[string]any{"count": 3}},
		}}},
		{page: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 13}, Events: []domain.Event{}}},
	}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})

	recorder := serveSSEDirect(t, handler, "/api/v1/events?after=epoch-a%3A10", []string{"epoch-a:12"})

	want := "id: epoch-a:13\nevent: queue.refreshed\ndata: {\"epoch\":\"epoch-a\",\"sequence\":13,\"type\":\"queue.refreshed\",\"at\":\"2026-08-09T12:00:00Z\",\"data\":{\"count\":3}}\n\n"
	if recorder.Code != http.StatusOK || recorder.Body.String() != want {
		t.Fatalf("native replay status/body = %d/%q, want 200/%q", recorder.Code, recorder.Body.String(), want)
	}
	if got := runtime.eventCursorCalls(); !reflect.DeepEqual(got, []domain.EventCursor{{Epoch: "epoch-a", Sequence: 12}, {Epoch: "epoch-a", Sequence: 13}}) {
		t.Fatalf("native replay cursors = %#v", got)
	}
}

func TestSSECursorAcceptsEveryCanonicalBoundary(t *testing.T) {
	longEpoch := strings.Repeat("a", 128)
	tests := []struct {
		name string
		raw  string
		want domain.EventCursor
	}{
		{name: "zero sequence", raw: "a:0", want: domain.EventCursor{Epoch: "a", Sequence: 0}},
		{name: "every epoch character", raw: "Az09._~-:1", want: domain.EventCursor{Epoch: "Az09._~-", Sequence: 1}},
		{name: "maximum uint64", raw: "a:18446744073709551615", want: domain.EventCursor{Epoch: "a", Sequence: ^uint64(0)}},
		{name: "maximum 149 byte cursor", raw: longEpoch + ":18446744073709551615", want: domain.EventCursor{Epoch: longEpoch, Sequence: ^uint64(0)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := parseEventCursor(test.raw)
			if !ok || got != test.want {
				t.Fatalf("parseEventCursor(%q) = %#v/%t, want %#v/true", test.raw, got, ok, test.want)
			}
			formatted, ok := formatValidEventCursor(test.want)
			if !ok || formatted != test.raw {
				t.Fatalf("formatValidEventCursor(%#v) = %q/%t, want %q/true", test.want, formatted, ok, test.raw)
			}
		})
	}
}

func TestEventsRejectsEveryInvalidCursorSourceBeforeRuntimeAccess(t *testing.T) {
	oversized := strings.Repeat("e", 129) + ":1"
	invalidUTF8 := string([]byte{'e', ':', 0xff})
	tests := []struct {
		name    string
		target  string
		headers []string
	}{
		{name: "duplicate query", target: "/api/v1/events?after=epoch-a%3A1&after=epoch-a%3A2"},
		{name: "malformed query encoding", target: "/api/v1/events?after=%ZZ"},
		{name: "query epoch character", target: "/api/v1/events?after=epoch%2Fa%3A1"},
		{name: "query leading zero", target: "/api/v1/events?after=epoch-a%3A01"},
		{name: "query overflow", target: "/api/v1/events?after=epoch-a%3A18446744073709551616"},
		{name: "query oversized", target: "/api/v1/events?after=" + oversized},
		{name: "query invalid UTF-8", target: "/api/v1/events?after=e%3A%FF"},
		{name: "query NUL control", target: "/api/v1/events?after=epoch-a%3A1%00"},
		{name: "query DEL control", target: "/api/v1/events?after=epoch-a%3A1%7F"},
		{name: "present empty header cannot fall back", target: "/api/v1/events?after=epoch-a%3A1", headers: []string{""}},
		{name: "duplicate header cannot fall back", target: "/api/v1/events?after=epoch-a%3A1", headers: []string{"epoch-a:1", "epoch-a:2"}},
		{name: "malformed header cannot fall back", target: "/api/v1/events?after=epoch-a%3A1", headers: []string{"epoch-a:+1"}},
		{name: "control header cannot fall back", target: "/api/v1/events?after=epoch-a%3A1", headers: []string{"epoch-a:\x7f"}},
		{name: "invalid UTF-8 header cannot fall back", target: "/api/v1/events?after=epoch-a%3A1", headers: []string{invalidUTF8}},
		{name: "oversized header cannot fall back", target: "/api/v1/events?after=epoch-a%3A1", headers: []string{oversized}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := &sseRuntimeFake{events: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a"}, Reset: true, Events: []domain.Event{}}}
			handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
			recorder := serveSSEDirect(t, handler, test.target, test.headers)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"invalid_event_cursor"`) || runtime.eventsCalls != 0 || runtime.issueCalls != 0 {
				t.Fatalf("invalid cursor response = %d calls=%d/%d body=%s", recorder.Code, runtime.eventsCalls, runtime.issueCalls, recorder.Body.String())
			}
		})
	}
}

func TestEventsHEADReturnsStreamHeadersWithoutRuntimeOrBody(t *testing.T) {
	runtime := &sseRuntimeFake{events: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a"}, Reset: true, Events: []domain.Event{}}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	handler.events.clients = make(chan struct{}, 1)
	handler.events.clients <- struct{}{}
	request := httptest.NewRequest(http.MethodHead, "/api/v1/events?after=epoch-a%3A0", nil)
	request = request.WithContext(context.WithValue(request.Context(), csrfContextKey{}, testCSRFToken))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(&deadlineRecorder{ResponseRecorder: recorder}, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" || recorder.Body.Len() != 0 || runtime.eventsCalls != 0 || runtime.issueCalls != 0 || len(handler.events.clients) != 1 || len(runtime.subscribeCursorCalls()) != 0 {
		t.Fatalf("HEAD status/content/body/calls/slots/subscribes = %d/%q/%q/%d/%d/%d/%#v", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String(), runtime.eventsCalls, runtime.issueCalls, len(handler.events.clients), runtime.subscribeCursorCalls())
	}
}

func TestEventsHEADFlushesHeadersInsideOneDeadlineAndClearsIt(t *testing.T) {
	runtime := &sseRuntimeFake{}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	handler.events.now = func() time.Time { return now }
	request := httptest.NewRequest(http.MethodHead, "/api/v1/events?after=epoch-a%3A0", nil)
	request = request.WithContext(context.WithValue(request.Context(), csrfContextKey{}, testCSRFToken))
	recorder := &countingDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.flushes != 1 || len(recorder.deadlines) != 2 || recorder.deadlines[0] != now.Add(2*time.Second) || !recorder.deadlines[1].IsZero() || runtime.eventsCalls != 0 {
		t.Fatalf("HEAD status/flush/deadlines/calls = %d/%d/%#v/%d", recorder.Code, recorder.flushes, recorder.deadlines, runtime.eventsCalls)
	}
}

func TestSSEProtectedRouteAuthenticatesAndValidatesHostAndMethodBeforeDependencyResolution(t *testing.T) {
	runtime := &sseRuntimeFake{events: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a"}, Reset: true, Events: []domain.Event{}}}
	pageHandler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	var resolverCalls int
	pageHandler.resolveDependencies = func(_ *http.Request, base pageDependencies) (pageDependencies, string, bool) {
		resolverCalls++
		return base, "", true
	}
	server, err := NewServer(Options{
		Bootstrap:      bootstrapFromValue("protected-sse-capability"),
		Handler:        pageHandler,
		ErrorResponder: pageHandler,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.boundPort.Store(43127)
	rawSession, err := server.sessions.issue()
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: rawSession}
	protected := server.protectedHandler()

	tests := []struct {
		name       string
		method     string
		host       string
		authorized bool
		status     int
	}{
		{name: "unauthenticated", method: http.MethodGet, host: "127.0.0.1:43127", status: http.StatusUnauthorized},
		{name: "wrong host", method: http.MethodGet, host: "attacker.example:43127", authorized: true, status: http.StatusBadRequest},
		{name: "disallowed method", method: http.MethodPost, host: "127.0.0.1:43127", authorized: true, status: http.StatusMethodNotAllowed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "http://127.0.0.1:43127/api/v1/events?after=epoch-a%3A0", nil)
			request.Host = test.host
			if test.authorized {
				request.AddCookie(cookie)
			}
			recorder := httptest.NewRecorder()
			protected.ServeHTTP(&deadlineRecorder{ResponseRecorder: recorder}, request)
			if recorder.Code != test.status {
				t.Fatalf("protected status = %d, want %d; body=%s", recorder.Code, test.status, recorder.Body.String())
			}
			assertNoCORSResponseHeaders(t, recorder.Header())
			if test.status == http.StatusMethodNotAllowed {
				if recorder.Header().Get("Allow") != "GET, HEAD" || recorder.Header().Get("Content-Type") != "application/json; charset=utf-8" || recorder.Header().Get("X-Accel-Buffering") != "" {
					t.Fatalf("405 headers = %#v", recorder.Header())
				}
			}
		})
	}
	if resolverCalls != 0 || runtime.eventsCalls != 0 || runtime.issueCalls != 0 {
		t.Fatalf("rejected protected requests reached resolver/events/issues = %d/%d/%d", resolverCalls, runtime.eventsCalls, runtime.issueCalls)
	}

	wantStreamHeaders := http.Header{
		"Cache-Control":                {"no-store"},
		"Content-Security-Policy":      {contentSecurityPolicy},
		"Content-Type":                 {"text/event-stream; charset=utf-8"},
		"Cross-Origin-Resource-Policy": {"same-origin"},
		"Referrer-Policy":              {"no-referrer"},
		"X-Accel-Buffering":            {"no"},
		"X-Content-Type-Options":       {"nosniff"},
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method+" exact stream headers", func(t *testing.T) {
			request := httptest.NewRequest(method, "http://127.0.0.1:43127/api/v1/events?after=epoch-a%3A0", nil)
			request.Host = "127.0.0.1:43127"
			request.AddCookie(cookie)
			recorder := httptest.NewRecorder()
			protected.ServeHTTP(&deadlineRecorder{ResponseRecorder: recorder}, request)
			if recorder.Code != http.StatusOK || !reflect.DeepEqual(recorder.Header(), wantStreamHeaders) {
				t.Fatalf("%s stream status/headers = %d/%#v, want 200/%#v", method, recorder.Code, recorder.Header(), wantStreamHeaders)
			}
			assertNoCORSResponseHeaders(t, recorder.Header())
			if method == http.MethodHead && recorder.Body.Len() != 0 {
				t.Fatalf("HEAD stream body = %q", recorder.Body.String())
			}
		})
	}
	if resolverCalls != 2 || runtime.eventsCalls != 1 || runtime.issueCalls != 0 {
		t.Fatalf("accepted protected requests resolver/events/issues = %d/%d/%d, want 2/1/0", resolverCalls, runtime.eventsCalls, runtime.issueCalls)
	}
}

func TestEventsLogsOneSafeCorrelationForEveryPostStreamDeadlineFailure(t *testing.T) {
	for _, failAt := range []int{2, 3, 4} {
		t.Run("deadline call "+strconv.Itoa(failAt), func(t *testing.T) {
			var logs bytes.Buffer
			runtime := &sseRuntimeFake{events: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 1}, Reset: true, Events: []domain.Event{}}}
			handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
			calls := 0
			handler.events.setDeadline = func(http.ResponseWriter, time.Time) error {
				calls++
				if calls == failAt {
					return io.ErrClosedPipe
				}
				return nil
			}
			recorder := serveSSEDirect(t, handler, "/api/v1/events?after=old%3A0", nil)
			output := logs.String()
			if recorder.Code != http.StatusOK || strings.Count(output, "event_stream_failed") != 1 || strings.Count(output, "correlation_id=") != 1 || strings.Contains(output, "closed pipe") {
				t.Fatalf("post-stream failure status/log = %d/%q", recorder.Code, output)
			}
		})
	}
}

func TestSSEPreHeaderDeadlineFailureReturnsOneSafeJSONErrorAndReleasesItsSlot(t *testing.T) {
	runtime := &sseRuntimeFake{events: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 1}, Reset: true, Events: []domain.Event{}}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	handler.events.setDeadline = func(http.ResponseWriter, time.Time) error { return io.ErrClosedPipe }
	recorder := serveSSEDirect(t, handler, "/api/v1/events?after=epoch-a%3A0", nil)
	var response apiErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode pre-header deadline error: %v; body=%q", err, recorder.Body.String())
	}
	if recorder.Code != http.StatusInternalServerError || response.Error.Code != "internal_error" || response.Error.Message != "The request could not be completed." || response.Error.Retryable || response.Error.CorrelationID == "" || strings.Contains(recorder.Body.String(), "closed pipe") || len(handler.events.clients) != 0 {
		t.Fatalf("pre-header deadline status/error/slots = %d/%#v/%d; body=%q", recorder.Code, response.Error, len(handler.events.clients), recorder.Body.String())
	}
}

func TestSSEInitialHeaderFlushFailureLogsOneSafeFailureAndReleasesItsSlot(t *testing.T) {
	var logs bytes.Buffer
	runtime := &sseRuntimeFake{events: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 1}, Reset: true, Events: []domain.Event{}}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	writer := &flushFailureEventWriter{ResponseRecorder: httptest.NewRecorder()}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/events?after=epoch-a%3A0", nil)
	request = request.WithContext(context.WithValue(request.Context(), csrfContextKey{}, testCSRFToken))
	handler.ServeHTTP(writer, request)
	if writer.Code != http.StatusOK || writer.Body.Len() != 0 || writer.flushes != 1 || len(handler.events.clients) != 0 {
		t.Fatalf("header flush failure status/body/flushes/slots = %d/%q/%d/%d", writer.Code, writer.Body.String(), writer.flushes, len(handler.events.clients))
	}
	if strings.Count(logs.String(), "event_stream_failed") != 1 || strings.Count(logs.String(), "correlation_id=") != 1 || strings.Contains(logs.String(), "closed pipe") {
		t.Fatalf("header flush failure log = %q", logs.String())
	}
}

func TestSSEPostFlushRuntimeFailureClosesTheStreamAndReleasesTheClientSlot(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	var logs bytes.Buffer
	runtime := &sseRuntimeFake{
		eventPages: []sseEventResult{
			{page: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a"}, Events: []domain.Event{}}},
			{err: io.ErrUnexpectedEOF},
		},
		subscribeChannels: []<-chan struct{}{ready},
	}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime, Logger: slog.New(slog.NewTextHandler(&logs, nil))})

	recorder := serveSSEDirect(t, handler, "/api/v1/events?after=epoch-a%3A0", nil)

	if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 || runtime.eventsCalls != 2 || len(handler.events.clients) != 0 {
		t.Fatalf("post-flush runtime failure status/body/calls/slots = %d/%q/%d/%d", recorder.Code, recorder.Body.String(), runtime.eventsCalls, len(handler.events.clients))
	}
	logged := logs.String()
	if strings.Count(logged, "event_stream_failed") != 1 || strings.Count(logged, "correlation_id=") != 1 || strings.Contains(logged, "unexpected EOF") {
		t.Fatalf("post-flush runtime failure log = %q", logged)
	}
}

func TestSSEWriteAndFlushDeadlinesBoundBlockedTransportsAndReleaseSlots(t *testing.T) {
	tests := []struct {
		name       string
		blockWrite bool
		blockFlush bool
	}{
		{name: "record write", blockWrite: true},
		{name: "record flush", blockFlush: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			runtime := &sseRuntimeFake{events: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 1}, Reset: true, Events: []domain.Event{}}}
			handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
			handler.events.writeTimeout = 20 * time.Millisecond
			writer := &deadlineBlockingEventWriter{header: make(http.Header), blockWrite: test.blockWrite, blockRecordFlush: test.blockFlush}
			request := httptest.NewRequest(http.MethodGet, "/api/v1/events?after=old%3A0", nil)
			request = request.WithContext(context.WithValue(request.Context(), csrfContextKey{}, testCSRFToken))

			started := time.Now()
			handler.ServeHTTP(writer, request)
			elapsed := time.Since(started)

			if writer.status != http.StatusOK || elapsed < 15*time.Millisecond || elapsed > time.Second || len(handler.events.clients) != 0 {
				t.Fatalf("blocked %s status/elapsed/slots = %d/%s/%d", test.name, writer.status, elapsed, len(handler.events.clients))
			}
			if strings.Count(logs.String(), "event_stream_failed") != 1 || strings.Count(logs.String(), "correlation_id=") != 1 {
				t.Fatalf("blocked %s log = %q", test.name, logs.String())
			}
		})
	}
}

func TestSSEBlockedWriteRemainsBoundedWhenTheRequestIsCanceledInProgress(t *testing.T) {
	var logs bytes.Buffer
	runtime := &sseRuntimeFake{events: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 1}, Reset: true, Events: []domain.Event{}}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	handler.events.writeTimeout = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/api/v1/events?after=old%3A0", nil).WithContext(ctx)
	request = request.WithContext(context.WithValue(request.Context(), csrfContextKey{}, testCSRFToken))
	writer := &deadlineBlockingEventWriter{header: make(http.Header), blockWrite: true, writeStarted: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(writer, request)
		close(done)
	}()
	waitForSignal(t, writer.writeStarted, "blocked event write")

	started := time.Now()
	cancel()
	select {
	case <-done:
		t.Fatal("blocked transport returned synchronously without honoring its write deadline")
	case <-time.After(20 * time.Millisecond):
	}
	waitForSignal(t, done, "blocked write deadline after cancellation")
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("blocked canceled write took %s", elapsed)
	}
	if len(handler.events.clients) != 0 || strings.Count(logs.String(), "event_stream_failed") != 1 {
		t.Fatalf("blocked canceled write slots/log = %d/%q", len(handler.events.clients), logs.String())
	}
}

func TestEventsReplaysCanonicalRecordsInStrictOrderAndClosesOnReadyWithoutProgress(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	runtime := &sseRuntimeFake{eventPages: []sseEventResult{
		{page: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 12}, Events: []domain.Event{
			{Epoch: "epoch-a", Sequence: 11, Type: "queue.refreshed", At: now, Data: map[string]any{"count": 2}},
			{Epoch: "epoch-a", Sequence: 12, Type: "queue.failed", At: now.Add(time.Second), Data: map[string]any{}},
		}}},
		{page: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 12}, Events: []domain.Event{}}},
	}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	recorder := serveSSEDirect(t, handler, "/api/v1/events?after=epoch-a%3A10", nil)
	want := "id: epoch-a:11\nevent: queue.refreshed\ndata: {\"epoch\":\"epoch-a\",\"sequence\":11,\"type\":\"queue.refreshed\",\"at\":\"2026-08-09T12:00:00Z\",\"data\":{\"count\":2}}\n\n" +
		"id: epoch-a:12\nevent: queue.failed\ndata: {\"epoch\":\"epoch-a\",\"sequence\":12,\"type\":\"queue.failed\",\"at\":\"2026-08-09T12:00:01Z\",\"data\":{}}\n\n"
	if recorder.Code != http.StatusOK || recorder.Body.String() != want {
		t.Fatalf("event stream status/body = %d/%q, want 200/%q", recorder.Code, recorder.Body.String(), want)
	}
	if got := runtime.eventCursorCalls(); !reflect.DeepEqual(got, []domain.EventCursor{{Epoch: "epoch-a", Sequence: 10}, {Epoch: "epoch-a", Sequence: 12}}) {
		t.Fatalf("EventsAfter cursors = %#v", got)
	}
	if got := runtime.subscribeCursorCalls(); !reflect.DeepEqual(got, []domain.EventCursor{{Epoch: "epoch-a", Sequence: 12}}) {
		t.Fatalf("SubscribeEvents cursors = %#v", got)
	}
	if strings.Contains(recorder.Body.String(), "queue.updated") {
		t.Fatal("stream exposed the noncanonical queue.updated event")
	}
}

func TestRuntimeChangedIsACanonicalSnapshotInvalidationEvent(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	page := domain.EventPage{
		LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 1},
		Events: []domain.Event{{
			Epoch: "epoch-a", Sequence: 1, Type: "runtime.changed", At: now,
			Data: map[string]any{"issue_id": "issue-1", "issue_identifier": "SYM-1"},
		}},
	}
	records, next, reset, ok := prepareEventPage(domain.EventCursor{Epoch: "epoch-a", Sequence: 0}, page)
	if !ok || reset != nil || len(records) != 1 || records[0].typeName != "runtime.changed" || next != page.LatestCursor {
		t.Fatalf("runtime event = records:%#v next:%#v reset:%#v ok:%v", records, next, reset, ok)
	}
}

func TestEventsInvalidOrOversizedRecordEmitsOnlyOneSafeReset(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	oversized := eventWithEncodedSize(t, 64<<10+1)
	tests := []struct {
		name   string
		event  domain.Event
		latest domain.EventCursor
		canary string
	}{
		{name: "wrong epoch", event: domain.Event{Epoch: "epoch-b", Sequence: 1, Type: "queue.refreshed", At: now, Data: map[string]any{}}, latest: domain.EventCursor{Epoch: "epoch-a", Sequence: 1}},
		{name: "sequence gap", event: domain.Event{Epoch: "epoch-a", Sequence: 2, Type: "queue.refreshed", At: now, Data: map[string]any{}}, latest: domain.EventCursor{Epoch: "epoch-a", Sequence: 2}},
		{name: "noncanonical type", event: domain.Event{Epoch: "epoch-a", Sequence: 1, Type: "queue.updated", At: now, Data: map[string]any{"unsafe": "type-canary"}}, latest: domain.EventCursor{Epoch: "epoch-a", Sequence: 1}, canary: "type-canary"},
		{name: "unsafe identity", event: domain.Event{Epoch: "bad/epoch", Sequence: 1, Type: "queue.refreshed", At: now, Data: map[string]any{"unsafe": "identity-canary"}}, latest: domain.EventCursor{Epoch: "epoch-a", Sequence: 1}, canary: "identity-canary"},
		{name: "unencodable data", event: domain.Event{Epoch: "epoch-a", Sequence: 1, Type: "configuration.changed", At: now, Data: map[string]any{"unsafe": make(chan int)}}, latest: domain.EventCursor{Epoch: "epoch-a", Sequence: 1}, canary: "unsafe"},
		{name: "oversized data", event: oversized, latest: domain.EventCursor{Epoch: "epoch-a", Sequence: 1}, canary: strings.Repeat("x", 32)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := &sseRuntimeFake{events: domain.EventPage{LatestCursor: test.latest, Events: []domain.Event{test.event}}}
			handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
			recorder := serveSSEDirect(t, handler, "/api/v1/events?after=epoch-a%3A0", nil)
			current := test.latest.Epoch + ":" + strconv.FormatUint(test.latest.Sequence, 10)
			want := "id: " + current + "\nevent: reset\ndata: {\"cursor\":\"" + current + "\",\"reason\":\"snapshot_required\"}\n\n"
			if recorder.Code != http.StatusOK || recorder.Body.String() != want || (test.canary != "" && strings.Contains(recorder.Body.String(), test.canary)) {
				t.Fatalf("invalid event stream = %d/%q, want %q", recorder.Code, recorder.Body.String(), want)
			}
		})
	}
}

func TestSSEPrevalidatesTheWholePageBeforeWritingAnyValidPrefix(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	valid := domain.Event{Epoch: "epoch-a", Sequence: 1, Type: "queue.refreshed", At: now, Data: map[string]any{"prefix_canary": "must-not-stream"}}
	oversized := eventWithEncodedSize(t, maximumEventPayloadBytes+1)
	oversized.Sequence = 2
	tests := []struct {
		name   string
		latest domain.EventCursor
		tail   domain.Event
	}{
		{name: "invalid second record", latest: domain.EventCursor{Epoch: "epoch-a", Sequence: 3}, tail: domain.Event{Epoch: "epoch-a", Sequence: 3, Type: "queue.failed", At: now, Data: map[string]any{"tail_canary": "invalid-gap"}}},
		{name: "oversized second record", latest: domain.EventCursor{Epoch: "epoch-a", Sequence: 2}, tail: oversized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := &sseRuntimeFake{events: domain.EventPage{LatestCursor: test.latest, Events: []domain.Event{valid, test.tail}}}
			handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
			recorder := serveSSEDirect(t, handler, "/api/v1/events?after=epoch-a%3A0", nil)
			cursor := test.latest.Epoch + ":" + strconv.FormatUint(test.latest.Sequence, 10)
			want := "id: " + cursor + "\nevent: reset\ndata: {\"cursor\":\"" + cursor + "\",\"reason\":\"snapshot_required\"}\n\n"
			if recorder.Code != http.StatusOK || recorder.Body.String() != want || strings.Contains(recorder.Body.String(), "must-not-stream") || strings.Contains(recorder.Body.String(), "event: queue.refreshed") {
				t.Fatalf("prevalidated page status/body = %d/%q, want reset only %q", recorder.Code, recorder.Body.String(), want)
			}
		})
	}
}

func TestEventsAcceptsAnExact64KiBCompactJSONPayloadWithoutTruncation(t *testing.T) {
	event := eventWithEncodedSize(t, 64<<10)
	runtime := &sseRuntimeFake{eventPages: []sseEventResult{
		{page: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 1}, Events: []domain.Event{event}}},
		{page: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 1}, Events: []domain.Event{}}},
	}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	recorder := serveSSEDirect(t, handler, "/api/v1/events?after=epoch-a%3A0", nil)
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Body.String(), "id: epoch-a:1\nevent: queue.refreshed\ndata: {") || !strings.HasSuffix(recorder.Body.String(), "}\n\n") || strings.Contains(recorder.Body.String(), "event: reset") {
		t.Fatalf("exact-boundary stream status/length/prefix = %d/%d/%q", recorder.Code, recorder.Body.Len(), recorder.Body.String()[:min(recorder.Body.Len(), 80)])
	}
}

func TestEventRecordUsesOneCompleteTransportWriteAndRejectsShortWrites(t *testing.T) {
	handler := newTestPageHandler(t, PageOptions{})
	handler.events.setDeadline = func(http.ResponseWriter, time.Time) error { return nil }
	record := preparedEventRecord{cursor: "epoch-a:1", typeName: "queue.refreshed", data: []byte(`{"epoch":"epoch-a","sequence":1}`)}
	want := "id: epoch-a:1\nevent: queue.refreshed\ndata: {\"epoch\":\"epoch-a\",\"sequence\":1}\n\n"

	complete := &writeBehaviorRecorder{ResponseRecorder: httptest.NewRecorder(), failAfter: 1}
	if err := handler.writeEventRecord(complete, record); err != nil || complete.calls != 1 || complete.Body.String() != want {
		t.Fatalf("complete record err/calls/body = %v/%d/%q", err, complete.calls, complete.Body.String())
	}

	short := &writeBehaviorRecorder{ResponseRecorder: httptest.NewRecorder(), short: true}
	if err := handler.writeEventRecord(short, record); !errors.Is(err, io.ErrShortWrite) || short.calls != 1 {
		t.Fatalf("short record err/calls = %v/%d", err, short.calls)
	}
}

func TestSSEHeaderEventAndResetUseTheExactInjectedWriteDeadline(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		runtime *sseRuntimeFake
	}{
		{name: "event", runtime: &sseRuntimeFake{eventPages: []sseEventResult{
			{page: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 1}, Events: []domain.Event{{Epoch: "epoch-a", Sequence: 1, Type: "queue.refreshed", At: now, Data: map[string]any{}}}}},
			{page: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 1}, Events: []domain.Event{}}},
		}}},
		{name: "reset", runtime: &sseRuntimeFake{events: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 1}, Reset: true, Events: []domain.Event{}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestPageHandler(t, PageOptions{Queries: test.runtime, Commands: test.runtime})
			handler.events.now = func() time.Time { return now }
			request := httptest.NewRequest(http.MethodGet, "/api/v1/events?after=epoch-a%3A0", nil)
			request = request.WithContext(context.WithValue(request.Context(), csrfContextKey{}, testCSRFToken))
			recorder := &countingDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			handler.ServeHTTP(recorder, request)
			want := []time.Time{now.Add(eventWriteTimeout), {}, now.Add(eventWriteTimeout), {}}
			if recorder.Code != http.StatusOK || recorder.flushes != 2 || !reflect.DeepEqual(recorder.deadlines, want) {
				t.Fatalf("%s status/flush/deadlines = %d/%d/%#v, want 200/2/%#v", test.name, recorder.Code, recorder.flushes, recorder.deadlines, want)
			}
		})
	}
}

func TestEventsHeartbeatIsExactDoesNotAdvanceCursorAndStopsOnCancel(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	hold := make(chan struct{})
	ticker := newManualEventTicker()
	runtime := &sseRuntimeFake{
		events:            domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 0}, Events: []domain.Event{}},
		subscribeChannels: []<-chan struct{}{hold},
	}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	handler.events.now = func() time.Time { return now }
	handler.events.newTicker = func(interval time.Duration) eventTicker {
		if interval != 20*time.Second {
			t.Fatalf("heartbeat interval = %s", interval)
		}
		return ticker
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/events?after=epoch-a%3A0", nil).WithContext(ctx)
	request = request.WithContext(context.WithValue(request.Context(), csrfContextKey{}, testCSRFToken))
	writer := newAsyncEventWriter()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(writer, request)
		close(done)
	}()
	waitForSignal(t, writer.flushed, "initial SSE header flush")
	ticker.ticks <- time.Date(2026, 8, 9, 12, 1, 0, 0, time.UTC)
	waitForSignal(t, writer.flushed, "heartbeat flush")
	cancel()
	waitForSignal(t, done, "request cancellation")
	waitForSignal(t, ticker.stopped, "ticker stop")
	body, deadlines := writer.snapshot()
	wantDeadlines := []time.Time{now.Add(eventWriteTimeout), {}, now.Add(eventWriteTimeout), {}}
	if body != ": keep-alive\n\n" || !reflect.DeepEqual(deadlines, wantDeadlines) {
		t.Fatalf("heartbeat body/deadlines = %q/%#v", body, deadlines)
	}
	if got := runtime.subscribeCursorCalls(); !reflect.DeepEqual(got, []domain.EventCursor{{Epoch: "epoch-a", Sequence: 0}}) {
		t.Fatalf("heartbeat advanced subscription cursor: %#v", got)
	}
}

func TestSSERequestCancellationDuringIdleWaitStopsImmediatelyAndReleasesItsSlot(t *testing.T) {
	hold := make(chan struct{})
	ticker := newManualEventTicker()
	runtime := &sseRuntimeFake{
		events:            domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a"}, Events: []domain.Event{}},
		subscribeChannels: []<-chan struct{}{hold},
	}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	handler.events.newTicker = func(time.Duration) eventTicker { return ticker }
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/api/v1/events?after=epoch-a%3A0", nil).WithContext(ctx)
	request = request.WithContext(context.WithValue(request.Context(), csrfContextKey{}, testCSRFToken))
	writer := newAsyncEventWriter()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(writer, request)
		close(done)
	}()
	waitForSignal(t, writer.flushed, "idle stream header flush")
	if len(handler.events.clients) != 1 {
		t.Fatalf("idle stream slots = %d, want 1", len(handler.events.clients))
	}

	started := time.Now()
	cancel()
	waitForSignal(t, done, "idle stream cancellation")
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("idle stream cancellation took %s", elapsed)
	}
	if len(handler.events.clients) != 0 {
		t.Fatalf("idle stream retained %d client slots", len(handler.events.clients))
	}
	waitForSignal(t, ticker.stopped, "idle stream ticker stop")
}

func TestEventsClientCapRejectsBeforeRuntimeAndReleasesEveryAcquiredSlot(t *testing.T) {
	runtime := &sseRuntimeFake{events: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 0}, Reset: true, Events: []domain.Event{}}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	handler.events.clients = make(chan struct{}, 1)
	handler.events.clients <- struct{}{}
	rejected := serveSSEDirect(t, handler, "/api/v1/events?after=epoch-a%3A0", nil)
	if rejected.Code != http.StatusServiceUnavailable || !strings.Contains(rejected.Body.String(), `"code":"event_stream_unavailable"`) || !strings.Contains(rejected.Body.String(), `"retryable":true`) || runtime.eventsCalls != 0 {
		t.Fatalf("capped response = %d calls=%d body=%s", rejected.Code, runtime.eventsCalls, rejected.Body.String())
	}
	<-handler.events.clients

	accepted := serveSSEDirect(t, handler, "/api/v1/events?after=epoch-a%3A0", nil)
	if accepted.Code != http.StatusOK || len(handler.events.clients) != 0 || runtime.eventsCalls != 1 {
		t.Fatalf("released slot response/cap/calls = %d/%d/%d", accepted.Code, len(handler.events.clients), runtime.eventsCalls)
	}

	runtime.eventsErr = io.ErrUnexpectedEOF
	runtime.events = domain.EventPage{}
	failed := serveSSEDirect(t, handler, "/api/v1/events?after=epoch-a%3A0", nil)
	var failedResponse apiErrorResponse
	if err := json.Unmarshal(failed.Body.Bytes(), &failedResponse); err != nil {
		t.Fatalf("decode pre-stream error: %v; body=%q", err, failed.Body.String())
	}
	if failed.Code != http.StatusServiceUnavailable || len(handler.events.clients) != 0 || failedResponse.Error.Code != "runtime_unavailable" || failedResponse.Error.Message != "Runtime state is temporarily unavailable." || !failedResponse.Error.Retryable || failedResponse.Error.CorrelationID == "" || strings.Contains(failed.Body.String(), "unexpected EOF") {
		t.Fatalf("pre-stream failure status/cap/error = %d/%d/%#v; body=%q", failed.Code, len(handler.events.clients), failedResponse.Error, failed.Body.String())
	}
}

func TestSSEClientCapIsSharedAcrossIPv4AndIPv6HostsAndDisconnectReleasesIt(t *testing.T) {
	hold := make(chan struct{})
	runtime := &sseRuntimeFake{
		events:            domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a"}, Events: []domain.Event{}},
		subscribeChannels: []<-chan struct{}{hold},
	}
	pageHandler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	pageHandler.events.clients = make(chan struct{}, 1)
	server, err := NewServer(Options{Bootstrap: bootstrapFromValue("shared-cap-capability"), Handler: pageHandler, ErrorResponder: pageHandler})
	if err != nil {
		t.Fatal(err)
	}
	server.boundPort.Store(43127)
	rawSession, err := server.sessions.issue()
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: rawSession}
	protected := server.protectedHandler()
	ctx, cancel := context.WithCancel(context.Background())
	firstRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:43127/api/v1/events?after=epoch-a%3A0", nil).WithContext(ctx)
	firstRequest.Host = "127.0.0.1:43127"
	firstRequest.AddCookie(cookie)
	firstWriter := newAsyncEventWriter()
	firstDone := make(chan struct{})
	go func() {
		protected.ServeHTTP(firstWriter, firstRequest)
		close(firstDone)
	}()
	waitForSignal(t, firstWriter.flushed, "IPv4 stream header flush")
	if len(pageHandler.events.clients) != 1 {
		t.Fatalf("IPv4 stream slots = %d, want 1", len(pageHandler.events.clients))
	}

	secondRequest := httptest.NewRequest(http.MethodGet, "http://[::1]:43127/api/v1/events?after=epoch-a%3A0", nil)
	secondRequest.Host = "[::1]:43127"
	secondRequest.AddCookie(cookie)
	secondRecorder := httptest.NewRecorder()
	protected.ServeHTTP(&deadlineRecorder{ResponseRecorder: secondRecorder}, secondRequest)
	if secondRecorder.Code != http.StatusServiceUnavailable || !strings.Contains(secondRecorder.Body.String(), `"code":"event_stream_unavailable"`) || runtime.eventsCalls != 1 {
		t.Fatalf("IPv6 capped response status/calls/body = %d/%d/%s", secondRecorder.Code, runtime.eventsCalls, secondRecorder.Body.String())
	}

	cancel()
	waitForSignal(t, firstDone, "IPv4 stream disconnect")
	if len(pageHandler.events.clients) != 0 {
		t.Fatalf("IPv4 disconnect retained %d stream slots", len(pageHandler.events.clients))
	}
	runtime.mu.Lock()
	runtime.events = domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a"}, Reset: true, Events: []domain.Event{}}
	runtime.mu.Unlock()
	thirdRequest := httptest.NewRequest(http.MethodGet, "http://[::1]:43127/api/v1/events?after=epoch-a%3A0", nil)
	thirdRequest.Host = "[::1]:43127"
	thirdRequest.AddCookie(cookie)
	thirdRecorder := httptest.NewRecorder()
	protected.ServeHTTP(&deadlineRecorder{ResponseRecorder: thirdRecorder}, thirdRequest)
	if thirdRecorder.Code != http.StatusOK || len(pageHandler.events.clients) != 0 || runtime.eventsCalls != 2 {
		t.Fatalf("IPv6 post-disconnect status/slots/calls = %d/%d/%d", thirdRecorder.Code, len(pageHandler.events.clients), runtime.eventsCalls)
	}
}

func TestEventsPublicationBetweenReadAndSubscribeReplaysWithoutMissingOrDuplicating(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	runtime := &sseRuntimeFake{
		eventPages: []sseEventResult{
			{page: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 0}, Events: []domain.Event{}}},
			{page: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 1}, Events: []domain.Event{{Epoch: "epoch-a", Sequence: 1, Type: "configuration.changed", At: now, Data: map[string]any{}}}}},
			{page: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 1}, Events: []domain.Event{}}},
		},
		subscribeChannels: []<-chan struct{}{ready, ready},
	}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	recorder := serveSSEDirect(t, handler, "/api/v1/events?after=epoch-a%3A0", nil)
	if recorder.Code != http.StatusOK || strings.Count(recorder.Body.String(), "event: configuration.changed") != 1 || runtime.eventsCalls != 3 {
		t.Fatalf("between-read replay = %d calls=%d body=%q", recorder.Code, runtime.eventsCalls, recorder.Body.String())
	}
}

func TestSSEMultipleWakeupsPreserveCanonicalPublicationOrder(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	runtime := &sseRuntimeFake{
		eventPages: []sseEventResult{
			{page: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a"}, Events: []domain.Event{}}},
			{page: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 1}, Events: []domain.Event{{Epoch: "epoch-a", Sequence: 1, Type: "queue.refreshed", At: now, Data: map[string]any{"publication": 1}}}}},
			{page: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 2}, Events: []domain.Event{{Epoch: "epoch-a", Sequence: 2, Type: "configuration.changed", At: now.Add(time.Second), Data: map[string]any{"publication": 2}}}}},
			{page: domain.EventPage{LatestCursor: domain.EventCursor{Epoch: "epoch-a", Sequence: 2}, Events: []domain.Event{}}},
		},
		subscribeChannels: []<-chan struct{}{ready, ready, ready},
	}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	recorder := serveSSEDirect(t, handler, "/api/v1/events?after=epoch-a%3A0", nil)
	want := "id: epoch-a:1\nevent: queue.refreshed\ndata: {\"epoch\":\"epoch-a\",\"sequence\":1,\"type\":\"queue.refreshed\",\"at\":\"2026-08-09T12:00:00Z\",\"data\":{\"publication\":1}}\n\n" +
		"id: epoch-a:2\nevent: configuration.changed\ndata: {\"epoch\":\"epoch-a\",\"sequence\":2,\"type\":\"configuration.changed\",\"at\":\"2026-08-09T12:00:01Z\",\"data\":{\"publication\":2}}\n\n"
	if recorder.Code != http.StatusOK || recorder.Body.String() != want {
		t.Fatalf("multi-publication stream status/body = %d/%q, want 200/%q", recorder.Code, recorder.Body.String(), want)
	}
	if got := runtime.eventCursorCalls(); !reflect.DeepEqual(got, []domain.EventCursor{{Epoch: "epoch-a"}, {Epoch: "epoch-a"}, {Epoch: "epoch-a", Sequence: 1}, {Epoch: "epoch-a", Sequence: 2}}) {
		t.Fatalf("multi-publication cursors = %#v", got)
	}
}

func TestSSEPublishReadDisconnectAndRuntimeShutdownAreRaceSafe(t *testing.T) {
	journal := observability.NewJournal(observability.JournalOptions{MaxEvents: 256, MaxBytes: 1 << 20})
	runtime := &sseJournalRuntime{journal: journal}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	baseContext, cancelAll := context.WithCancel(context.Background())
	defer cancelAll()
	const streams = 8
	done := make([]chan struct{}, streams)
	cancels := make([]context.CancelFunc, streams)
	writers := make([]*asyncEventWriter, streams)
	for index := range streams {
		ctx, cancel := context.WithCancel(baseContext)
		cancels[index] = cancel
		request := httptest.NewRequest(http.MethodGet, "/api/v1/events?after="+url.QueryEscape(journal.Epoch()+":0"), nil).WithContext(ctx)
		request = request.WithContext(context.WithValue(request.Context(), csrfContextKey{}, testCSRFToken))
		writer := newAsyncEventWriter()
		writers[index] = writer
		done[index] = make(chan struct{})
		go func(done chan struct{}) {
			handler.ServeHTTP(writer, request)
			close(done)
		}(done[index])
		waitForSignal(t, writer.flushed, "race stream header flush")
	}

	atBarrier := make(chan struct{})
	raceStart := make(chan struct{})
	publisherResult := make(chan error, 1)
	go func() {
		for sequence := range 100 {
			if sequence == 11 {
				close(atBarrier)
				<-raceStart
			}
			_, err := journal.Publish(domain.Event{Type: "queue.refreshed", Data: map[string]any{"sequence": sequence}})
			if errors.Is(err, observability.ErrJournalClosed) {
				publisherResult <- nil
				return
			}
			if err != nil {
				publisherResult <- err
				return
			}
		}
		publisherResult <- nil
	}()
	waitForSignal(t, atBarrier, "concurrent publication barrier")
	closerDone := make(chan struct{})
	go func() {
		<-raceStart
		for index := 0; index < streams; index += 2 {
			cancels[index]()
		}
		journal.Close()
		close(closerDone)
	}()
	close(raceStart)
	waitForSignal(t, closerDone, "concurrent disconnect and runtime shutdown")
	select {
	case err := <-publisherResult:
		if err != nil {
			t.Fatalf("concurrent publish failed unexpectedly: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for concurrent publisher")
	}
	for index := range streams {
		waitForSignal(t, done[index], "race stream shutdown")
	}
	cancelAll()
	if len(handler.events.clients) != 0 {
		t.Fatalf("race shutdown retained %d stream slots", len(handler.events.clients))
	}
	latest := journal.Cursor()
	body, _ := writers[1].snapshot()
	identifiers := make([]string, 0, latest.Sequence)
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "id: ") {
			identifiers = append(identifiers, strings.TrimPrefix(line, "id: "))
		}
	}
	if uint64(len(identifiers)) != latest.Sequence {
		t.Fatalf("complete observer delivered %d IDs, want %d; body length=%d", len(identifiers), latest.Sequence, len(body))
	}
	for sequence, identifier := range identifiers {
		want := journal.Epoch() + ":" + strconv.Itoa(sequence+1)
		if identifier != want {
			t.Fatalf("complete observer ID[%d] = %q, want %q", sequence, identifier, want)
		}
	}
}

func TestDefaultHandlerGeneratedCursorStreamsSafelyWithoutReconnectChurn(t *testing.T) {
	handler := newTestPageHandler(t, PageOptions{})
	handler.resolveDependencies = func(_ *http.Request, base pageDependencies) (pageDependencies, string, bool) {
		return base, "", true
	}
	overview := serveDirect(t, handler, http.MethodGet, "/", "", nil).Body.String()
	cursorMarker := `data-event-cursor-id="`
	cursorStart := strings.Index(overview, cursorMarker)
	if cursorStart == -1 {
		t.Fatal("default overview omitted its server-generated event cursor")
	}
	cursorStart += len(cursorMarker)
	cursorEnd := strings.IndexByte(overview[cursorStart:], '"')
	if cursorEnd == -1 {
		t.Fatal("default overview event cursor was not terminated")
	}
	generatedCursor := overview[cursorStart : cursorStart+cursorEnd]
	marker := `data-events-url="`
	start := strings.Index(overview, marker)
	if start == -1 {
		t.Fatal("default overview omitted its server-generated events URL")
	}
	start += len(marker)
	end := strings.IndexByte(overview[start:], '"')
	if end == -1 {
		t.Fatal("default overview events URL was not terminated")
	}
	eventsURL := html.UnescapeString(overview[start : start+end])

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, eventsURL, nil).WithContext(ctx)
	request = request.WithContext(context.WithValue(request.Context(), csrfContextKey{}, testCSRFToken))
	writer := newAsyncEventWriter()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(writer, request)
		close(done)
	}()
	waitForSignal(t, writer.flushed, "default stream header flush")
	cancel()
	waitForSignal(t, done, "default stream cancellation")
	body, _ := writer.snapshot()
	if writer.statusCode() != http.StatusOK || body != "" {
		t.Fatalf("default generated stream = %d body=%q", writer.statusCode(), body)
	}

	missing := serveSSEDirect(t, handler, "/api/v1/events", nil)
	wantReset := "id: " + generatedCursor + "\nevent: reset\ndata: {\"cursor\":\"" + generatedCursor + "\",\"reason\":\"snapshot_required\"}\n\n"
	if missing.Code != http.StatusOK || missing.Body.String() != wantReset {
		t.Fatalf("default missing-cursor reset = %d/%q, want %q", missing.Code, missing.Body.String(), wantReset)
	}
}

func eventWithEncodedSize(t *testing.T, size int) domain.Event {
	t.Helper()
	event := domain.Event{Epoch: "epoch-a", Sequence: 1, Type: "queue.refreshed", At: time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC), Data: map[string]any{"padding": ""}}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	padding := size - len(encoded)
	if padding < 0 {
		t.Fatalf("requested encoded size %d is below base %d", size, len(encoded))
	}
	event.Data["padding"] = strings.Repeat("x", padding)
	encoded, err = json.Marshal(event)
	if err != nil || len(encoded) != size {
		t.Fatalf("encoded event size = %d/%v, want %d", len(encoded), err, size)
	}
	return event
}

func serveSSEDirect(t *testing.T, handler http.Handler, target string, lastEventIDs []string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	for _, value := range lastEventIDs {
		request.Header.Add("Last-Event-ID", value)
	}
	request = request.WithContext(context.WithValue(request.Context(), csrfContextKey{}, testCSRFToken))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(&deadlineRecorder{ResponseRecorder: recorder}, request)
	return recorder
}

func assertNoCORSResponseHeaders(t *testing.T, header http.Header) {
	t.Helper()
	for name := range header {
		if strings.HasPrefix(strings.ToLower(name), "access-control-") {
			t.Fatalf("response exposed CORS header %q", name)
		}
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
}

func (*deadlineRecorder) SetWriteDeadline(time.Time) error { return nil }

type countingDeadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	flushes   int
}

func (recorder *countingDeadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	recorder.deadlines = append(recorder.deadlines, deadline)
	return nil
}

func (recorder *countingDeadlineRecorder) Flush() {
	recorder.flushes++
	recorder.ResponseRecorder.Flush()
}

type writeBehaviorRecorder struct {
	*httptest.ResponseRecorder
	calls     int
	failAfter int
	short     bool
}

func (recorder *writeBehaviorRecorder) Write(value []byte) (int, error) {
	recorder.calls++
	if recorder.failAfter > 0 && recorder.calls > recorder.failAfter {
		return 0, io.ErrClosedPipe
	}
	if recorder.short {
		count := max(0, len(value)-1)
		_, _ = recorder.ResponseRecorder.Write(value[:count])
		return count, nil
	}
	return recorder.ResponseRecorder.Write(value)
}

func (recorder *writeBehaviorRecorder) WriteString(value string) (int, error) {
	return recorder.Write([]byte(value))
}

type flushFailureEventWriter struct {
	*httptest.ResponseRecorder
	flushes int
}

func (*flushFailureEventWriter) SetWriteDeadline(time.Time) error { return nil }

func (writer *flushFailureEventWriter) FlushError() error {
	writer.flushes++
	return io.ErrClosedPipe
}

type deadlineBlockingEventWriter struct {
	header           http.Header
	status           int
	body             bytes.Buffer
	deadline         time.Time
	flushes          int
	blockWrite       bool
	blockRecordFlush bool
	writeStarted     chan struct{}
	writeStartOnce   sync.Once
}

func (writer *deadlineBlockingEventWriter) Header() http.Header { return writer.header }

func (writer *deadlineBlockingEventWriter) WriteHeader(status int) {
	if writer.status == 0 {
		writer.status = status
	}
}

func (writer *deadlineBlockingEventWriter) Write(value []byte) (int, error) {
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	if writer.blockWrite {
		if writer.writeStarted != nil {
			writer.writeStartOnce.Do(func() { close(writer.writeStarted) })
		}
		writer.waitForDeadline()
		return 0, os.ErrDeadlineExceeded
	}
	return writer.body.Write(value)
}

func (writer *deadlineBlockingEventWriter) FlushError() error {
	writer.flushes++
	if writer.blockRecordFlush && writer.flushes == 2 {
		writer.waitForDeadline()
		return os.ErrDeadlineExceeded
	}
	return nil
}

func (writer *deadlineBlockingEventWriter) SetWriteDeadline(deadline time.Time) error {
	writer.deadline = deadline
	return nil
}

func (writer *deadlineBlockingEventWriter) waitForDeadline() {
	delay := time.Until(writer.deadline)
	if delay > 0 {
		timer := time.NewTimer(delay)
		<-timer.C
	}
}

type manualEventTicker struct {
	ticks    chan time.Time
	stopped  chan struct{}
	stopOnce sync.Once
}

func newManualEventTicker() *manualEventTicker {
	return &manualEventTicker{ticks: make(chan time.Time, 1), stopped: make(chan struct{})}
}

func (ticker *manualEventTicker) C() <-chan time.Time { return ticker.ticks }
func (ticker *manualEventTicker) Stop() {
	ticker.stopOnce.Do(func() { close(ticker.stopped) })
}

type asyncEventWriter struct {
	mu        sync.Mutex
	header    http.Header
	status    int
	body      bytes.Buffer
	deadlines []time.Time
	flushed   chan struct{}
}

func newAsyncEventWriter() *asyncEventWriter {
	return &asyncEventWriter{header: make(http.Header), flushed: make(chan struct{}, 8)}
}

func (writer *asyncEventWriter) Header() http.Header { return writer.header }

func (writer *asyncEventWriter) WriteHeader(status int) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.status == 0 {
		writer.status = status
	}
}

func (writer *asyncEventWriter) Write(value []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	return writer.body.Write(value)
}

func (writer *asyncEventWriter) Flush() {
	select {
	case writer.flushed <- struct{}{}:
	default:
	}
}

func (writer *asyncEventWriter) SetWriteDeadline(deadline time.Time) error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.deadlines = append(writer.deadlines, deadline)
	return nil
}

func (writer *asyncEventWriter) snapshot() (string, []time.Time) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.body.String(), append([]time.Time(nil), writer.deadlines...)
}

func (writer *asyncEventWriter) statusCode() int {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.status
}

func waitForSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

type sseRuntimeFake struct {
	mu                sync.Mutex
	events            domain.EventPage
	eventsErr         error
	eventPages        []sseEventResult
	eventsCalls       int
	issueCalls        int
	lastCursor        domain.EventCursor
	cursors           []domain.EventCursor
	subscribes        []domain.EventCursor
	subscribeChannels []<-chan struct{}
}

type sseJournalRuntime struct {
	journal *observability.Journal
}

func (runtime *sseJournalRuntime) Snapshot(ctx context.Context) (domain.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return domain.Snapshot{}, err
	}
	return domain.Snapshot{EventCursor: runtime.journal.Cursor()}, nil
}

func (runtime *sseJournalRuntime) Issue(context.Context, string) (domain.IssueDetail, error) {
	return domain.IssueDetail{}, app.ErrIssueNotFound
}

func (runtime *sseJournalRuntime) EventsAfter(ctx context.Context, cursor domain.EventCursor) (domain.EventPage, error) {
	if err := ctx.Err(); err != nil {
		return domain.EventPage{}, err
	}
	return runtime.journal.After(cursor), nil
}

func (runtime *sseJournalRuntime) RecentEvents(ctx context.Context, limit int) (domain.EventPage, error) {
	if err := ctx.Err(); err != nil {
		return domain.EventPage{}, err
	}
	return runtime.journal.Recent(limit), nil
}

func (runtime *sseJournalRuntime) SubscribeEvents(cursor domain.EventCursor) <-chan struct{} {
	return runtime.journal.Subscribe(cursor)
}

func (*sseJournalRuntime) Refresh(context.Context) (domain.RefreshReceipt, error) {
	return domain.RefreshReceipt{Operations: []string{}}, nil
}

func (*sseJournalRuntime) SetScheduler(context.Context, bool) error {
	return app.ErrUnavailableInPhase
}

func (*sseJournalRuntime) Respond(context.Context, domain.OperatorResponse) error {
	return app.ErrUnavailableInPhase
}
func (*sseJournalRuntime) ExtendOperatorRequest(context.Context, string) error {
	return app.ErrUnavailableInPhase
}

type sseEventResult struct {
	page domain.EventPage
	err  error
}

func (runtime *sseRuntimeFake) Snapshot(context.Context) (domain.Snapshot, error) {
	return domain.EmptySnapshot(), nil
}

func (runtime *sseRuntimeFake) Issue(context.Context, string) (domain.IssueDetail, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.issueCalls++
	return domain.IssueDetail{}, app.ErrIssueNotFound
}

func (runtime *sseRuntimeFake) EventsAfter(_ context.Context, cursor domain.EventCursor) (domain.EventPage, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.eventsCalls++
	runtime.lastCursor = cursor
	runtime.cursors = append(runtime.cursors, cursor)
	if len(runtime.eventPages) > 0 {
		result := runtime.eventPages[0]
		runtime.eventPages = runtime.eventPages[1:]
		return result.page, result.err
	}
	return runtime.events, runtime.eventsErr
}

func (runtime *sseRuntimeFake) RecentEvents(context.Context, int) (domain.EventPage, error) {
	return domain.EventPage{Events: []domain.Event{}}, nil
}

func (runtime *sseRuntimeFake) SubscribeEvents(cursor domain.EventCursor) <-chan struct{} {
	runtime.mu.Lock()
	runtime.subscribes = append(runtime.subscribes, cursor)
	if len(runtime.subscribeChannels) > 0 {
		ready := runtime.subscribeChannels[0]
		runtime.subscribeChannels = runtime.subscribeChannels[1:]
		runtime.mu.Unlock()
		return ready
	}
	runtime.mu.Unlock()
	ready := make(chan struct{})
	close(ready)
	return ready
}

func (runtime *sseRuntimeFake) eventCursorCalls() []domain.EventCursor {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return append([]domain.EventCursor(nil), runtime.cursors...)
}

func (runtime *sseRuntimeFake) subscribeCursorCalls() []domain.EventCursor {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return append([]domain.EventCursor(nil), runtime.subscribes...)
}

func (*sseRuntimeFake) Refresh(context.Context) (domain.RefreshReceipt, error) {
	return domain.RefreshReceipt{Operations: []string{}}, nil
}

func (*sseRuntimeFake) SetScheduler(context.Context, bool) error { return app.ErrUnavailableInPhase }

func (*sseRuntimeFake) Respond(context.Context, domain.OperatorResponse) error {
	return app.ErrUnavailableInPhase
}
func (*sseRuntimeFake) ExtendOperatorRequest(context.Context, string) error {
	return app.ErrUnavailableInPhase
}

var _ app.RuntimeQueries = (*sseRuntimeFake)(nil)
var _ app.RuntimeCommands = (*sseRuntimeFake)(nil)
var _ app.RuntimeQueries = (*sseJournalRuntime)(nil)
var _ app.RuntimeCommands = (*sseJournalRuntime)(nil)
