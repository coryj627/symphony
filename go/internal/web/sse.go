package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coryj627/symphony/go/internal/domain"
)

const (
	maximumEventCursorBytes  = 149
	maximumEventPayloadBytes = 64 << 10
	eventWriteTimeout        = 2 * time.Second
	eventHeartbeatInterval   = 20 * time.Second
	maximumEventClients      = 32
)

type eventTicker interface {
	C() <-chan time.Time
	Stop()
}

type realEventTicker struct{ ticker *time.Ticker }

func (ticker realEventTicker) C() <-chan time.Time { return ticker.ticker.C }
func (ticker realEventTicker) Stop()               { ticker.ticker.Stop() }

type eventStreamConfig struct {
	clients           chan struct{}
	now               func() time.Time
	writeTimeout      time.Duration
	heartbeatInterval time.Duration
	newTicker         func(time.Duration) eventTicker
	setDeadline       func(http.ResponseWriter, time.Time) error
}

func newEventStreamConfig() eventStreamConfig {
	return eventStreamConfig{
		clients: make(chan struct{}, maximumEventClients), now: time.Now,
		writeTimeout: eventWriteTimeout, heartbeatInterval: eventHeartbeatInterval,
		newTicker: func(interval time.Duration) eventTicker { return realEventTicker{ticker: time.NewTicker(interval)} },
		setDeadline: func(w http.ResponseWriter, deadline time.Time) error {
			return http.NewResponseController(w).SetWriteDeadline(deadline)
		},
	}
}

type eventResetData struct {
	Cursor string `json:"cursor"`
	Reason string `json:"reason"`
}

type preparedEventRecord struct {
	cursor   string
	typeName string
	data     []byte
}

func (handler *PageHandler) eventsAPI(w http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodHead {
		if started, err := handler.beginEventStream(w); err != nil {
			if !started {
				handler.writeAPIError(w, "internal_error")
			} else {
				handler.logEventStreamFailure()
			}
		}
		return
	}

	cursor, ok := requestEventCursor(request)
	if !ok {
		handler.writeAPIError(w, "invalid_event_cursor")
		return
	}
	select {
	case handler.events.clients <- struct{}{}:
		defer func() { <-handler.events.clients }()
	default:
		handler.writeAPIError(w, "event_stream_unavailable")
		return
	}
	page, err := handler.dependencies(request).queries.EventsAfter(request.Context(), cursor)
	if err != nil {
		handler.writeAPIError(w, "runtime_unavailable")
		return
	}
	records, next, reset, ok := prepareEventPage(cursor, page)
	if !ok {
		handler.writeAPIError(w, "internal_error")
		return
	}
	if started, err := handler.beginEventStream(w); err != nil {
		if !started {
			handler.writeAPIError(w, "internal_error")
		} else {
			handler.logEventStreamFailure()
		}
		return
	}
	if reset != nil {
		if err := handler.writeEventRecord(w, *reset); err != nil {
			handler.logEventStreamFailure()
		}
		return
	}
	if err := handler.writePreparedEvents(w, records); err != nil {
		handler.logEventStreamFailure()
		return
	}
	cursor = next
	heartbeats := handler.events.newTicker(handler.events.heartbeatInterval)
	defer heartbeats.Stop()
	ready := handler.dependencies(request).queries.SubscribeEvents(cursor)

	for {
		select {
		case <-request.Context().Done():
			return
		case <-heartbeats.C():
			if err := handler.writeHeartbeat(w); err != nil {
				handler.logEventStreamFailure()
				return
			}
			continue
		case <-ready:
		}
		page, err = handler.dependencies(request).queries.EventsAfter(request.Context(), cursor)
		if err != nil {
			if !errorsIsContext(err, request.Context()) {
				handler.logEventStreamFailure()
			}
			return
		}
		records, next, reset, ok = prepareEventPage(cursor, page)
		if !ok {
			handler.logEventStreamFailure()
			return
		}
		if reset != nil {
			if err := handler.writeEventRecord(w, *reset); err != nil {
				handler.logEventStreamFailure()
			}
			return
		}
		if len(records) == 0 && next == cursor {
			return
		}
		if err := handler.writePreparedEvents(w, records); err != nil {
			handler.logEventStreamFailure()
			return
		}
		cursor = next
		ready = handler.dependencies(request).queries.SubscribeEvents(cursor)
	}
}

func prepareEventPage(after domain.EventCursor, page domain.EventPage) ([]preparedEventRecord, domain.EventCursor, *preparedEventRecord, bool) {
	current, currentOK := formatValidEventCursor(page.LatestCursor)
	if !currentOK {
		return nil, after, nil, false
	}
	resetRecord := func() ([]preparedEventRecord, domain.EventCursor, *preparedEventRecord, bool) {
		record, ok := prepareResetRecord(current)
		return nil, page.LatestCursor, &record, ok
	}
	if page.Reset {
		return resetRecord()
	}
	if _, ok := formatValidEventCursor(after); !ok {
		return resetRecord()
	}
	next := after
	records := make([]preparedEventRecord, 0, len(page.Events))
	for _, event := range page.Events {
		if next.Sequence == ^uint64(0) || event.Epoch != next.Epoch || event.Sequence != next.Sequence+1 || !validEventType(event.Type) {
			return resetRecord()
		}
		cursor, ok := formatValidEventCursor(domain.EventCursor{Epoch: event.Epoch, Sequence: event.Sequence})
		if !ok {
			return resetRecord()
		}
		encoded, err := json.Marshal(event)
		if err != nil || len(encoded) > maximumEventPayloadBytes {
			return resetRecord()
		}
		records = append(records, preparedEventRecord{cursor: cursor, typeName: event.Type, data: encoded})
		next = domain.EventCursor{Epoch: event.Epoch, Sequence: event.Sequence}
	}
	if next != page.LatestCursor {
		return resetRecord()
	}
	return records, next, nil, true
}

func prepareResetRecord(cursor string) (preparedEventRecord, bool) {
	encoded, err := json.Marshal(eventResetData{Cursor: cursor, Reason: "snapshot_required"})
	if err != nil {
		return preparedEventRecord{}, false
	}
	return preparedEventRecord{cursor: cursor, typeName: "reset", data: encoded}, true
}

func validEventType(value string) bool {
	switch value {
	case "queue.refreshed", "queue.failed", "configuration.changed", "runtime.changed":
		return true
	default:
		return false
	}
}

func (handler *PageHandler) beginEventStream(w http.ResponseWriter) (bool, error) {
	if err := handler.events.setDeadline(w, handler.events.now().Add(handler.events.writeTimeout)); err != nil {
		return false, err
	}
	setEventStreamHeaders(w.Header())
	w.WriteHeader(http.StatusOK)
	if err := http.NewResponseController(w).Flush(); err != nil {
		return true, err
	}
	if err := handler.events.setDeadline(w, time.Time{}); err != nil {
		return true, err
	}
	return true, nil
}

func (handler *PageHandler) writePreparedEvents(w http.ResponseWriter, records []preparedEventRecord) error {
	for _, record := range records {
		if err := handler.writeEventRecord(w, record); err != nil {
			return err
		}
	}
	return nil
}

func (handler *PageHandler) writeEventRecord(w http.ResponseWriter, record preparedEventRecord) error {
	framing := "id: " + record.cursor + "\nevent: " + record.typeName + "\ndata: "
	complete := make([]byte, 0, len(framing)+len(record.data)+2)
	complete = append(complete, framing...)
	complete = append(complete, record.data...)
	complete = append(complete, '\n', '\n')
	return handler.writeCompleteEventBytes(w, complete)
}

func (handler *PageHandler) writeHeartbeat(w http.ResponseWriter) error {
	return handler.writeCompleteEventBytes(w, []byte(": keep-alive\n\n"))
}

func (handler *PageHandler) writeCompleteEventBytes(w http.ResponseWriter, complete []byte) error {
	return handler.writeEventOperation(w, func() error {
		count, err := w.Write(complete)
		if err != nil {
			return err
		}
		if count != len(complete) {
			return io.ErrShortWrite
		}
		return nil
	})
}

func (handler *PageHandler) writeEventOperation(w http.ResponseWriter, write func() error) error {
	if err := handler.events.setDeadline(w, handler.events.now().Add(handler.events.writeTimeout)); err != nil {
		return err
	}
	if err := write(); err != nil {
		return err
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		return err
	}
	return handler.events.setDeadline(w, time.Time{})
}

func (handler *PageHandler) logEventStreamFailure() {
	handler.logger.Error("event stream closed", "code", "event_stream_failed", "correlation_id", handler.nextCorrelationID())
}

func errorsIsContext(err error, ctx context.Context) bool {
	return ctx.Err() != nil && errors.Is(err, ctx.Err())
}

func setEventStreamHeaders(header http.Header) {
	setSecurityHeaders(header)
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Accel-Buffering", "no")
	header.Del("Connection")
}

func requestEventCursor(request *http.Request) (domain.EventCursor, bool) {
	headerValues, present := rawHeaderValues(request.Header, "Last-Event-ID")
	if present {
		if len(headerValues) != 1 || headerValues[0] == "" {
			return domain.EventCursor{}, false
		}
		return parseEventCursor(headerValues[0])
	}

	values, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return domain.EventCursor{}, false
	}
	after, present := values["after"]
	if !present {
		return domain.EventCursor{}, true
	}
	if len(after) != 1 {
		return domain.EventCursor{}, false
	}
	if after[0] == "" {
		return domain.EventCursor{}, true
	}
	return parseEventCursor(after[0])
}

func rawHeaderValues(header http.Header, name string) ([]string, bool) {
	var values []string
	present := false
	for key, candidates := range header {
		if !strings.EqualFold(key, name) {
			continue
		}
		present = true
		values = append(values, candidates...)
	}
	return values, present
}

func parseEventCursor(value string) (domain.EventCursor, bool) {
	if value == "" || len(value) > maximumEventCursorBytes || !utf8.ValidString(value) {
		return domain.EventCursor{}, false
	}
	separator := strings.LastIndexByte(value, ':')
	if separator < 1 || separator > 128 || separator == len(value)-1 {
		return domain.EventCursor{}, false
	}
	epoch, sequenceText := value[:separator], value[separator+1:]
	for index := range len(epoch) {
		current := epoch[index]
		if (current < 'a' || current > 'z') && (current < 'A' || current > 'Z') && (current < '0' || current > '9') && current != '.' && current != '_' && current != '~' && current != '-' {
			return domain.EventCursor{}, false
		}
	}
	if len(sequenceText) > 20 {
		return domain.EventCursor{}, false
	}
	for index := range len(sequenceText) {
		if sequenceText[index] < '0' || sequenceText[index] > '9' {
			return domain.EventCursor{}, false
		}
	}
	sequence, err := strconv.ParseUint(sequenceText, 10, 64)
	if err != nil || strconv.FormatUint(sequence, 10) != sequenceText {
		return domain.EventCursor{}, false
	}
	return domain.EventCursor{Epoch: epoch, Sequence: sequence}, true
}

func formatValidEventCursor(cursor domain.EventCursor) (string, bool) {
	formatted := cursor.Epoch + ":" + strconv.FormatUint(cursor.Sequence, 10)
	parsed, ok := parseEventCursor(formatted)
	return formatted, ok && parsed == cursor
}
