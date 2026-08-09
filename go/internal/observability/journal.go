package observability

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coryj627/symphony/go/internal/domain"
)

const (
	defaultJournalMaxEvents = 4096
	defaultJournalMaxBytes  = 8 << 20
	maximumEventBytes       = 64 << 10
	maximumEventTypeBytes   = 128
	maximumEventDataDepth   = 16
	maximumEventDataItems   = 1024
	maximumEventStringBytes = 8192
)

var (
	ErrJournalClosed = errors.New("event_journal_closed")
	ErrInvalidEvent  = errors.New("invalid_event")
	ErrEventTooLarge = errors.New("event_too_large")
	readyJournalWait = func() <-chan struct{} {
		ready := make(chan struct{})
		close(ready)
		return ready
	}()
)

type JournalOptions struct {
	MaxEvents int
	MaxBytes  int
}

type journalDependencies struct {
	now    func() time.Time
	random func([]byte) (int, error)
}

type retainedEvent struct {
	event domain.Event
	bytes int
}

type Journal struct {
	mu         sync.RWMutex
	epoch      string
	sequence   uint64
	events     []retainedEvent
	totalBytes int
	maxEvents  int
	maxBytes   int
	now        func() time.Time
	notify     chan struct{}
	closed     bool
}

func NewJournal(options JournalOptions) *Journal {
	return newJournalWithDependencies(options, journalDependencies{now: time.Now, random: rand.Read})
}

func newJournalWithDependencies(options JournalOptions, dependencies journalDependencies) *Journal {
	if options.MaxEvents <= 0 {
		options.MaxEvents = defaultJournalMaxEvents
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = defaultJournalMaxBytes
	}
	if dependencies.now == nil {
		dependencies.now = time.Now
	}
	if dependencies.random == nil {
		dependencies.random = rand.Read
	}
	random := make([]byte, 16)
	count, err := dependencies.random(random)
	if err != nil || count != len(random) {
		panic("event journal could not create a restart epoch")
	}
	return &Journal{
		epoch: hex.EncodeToString(random), maxEvents: options.MaxEvents, maxBytes: options.MaxBytes,
		now: dependencies.now, notify: make(chan struct{}), events: []retainedEvent{},
	}
}

func (journal *Journal) Epoch() string {
	journal.mu.RLock()
	defer journal.mu.RUnlock()
	return journal.epoch
}

func (journal *Journal) Cursor() domain.EventCursor {
	journal.mu.RLock()
	defer journal.mu.RUnlock()
	return journal.cursorLocked()
}

func (journal *Journal) Publish(candidate domain.Event) (domain.Event, error) {
	if strings.TrimSpace(candidate.Type) == "" || !utf8.ValidString(candidate.Type) || len(candidate.Type) > maximumEventTypeBytes {
		return domain.Event{}, ErrInvalidEvent
	}
	data := sanitizeJournalData(candidate.Data)

	journal.mu.Lock()
	defer journal.mu.Unlock()
	if journal.closed {
		return domain.Event{}, ErrJournalClosed
	}
	published := domain.Event{
		Epoch: journal.epoch, Sequence: journal.sequence + 1, Type: candidate.Type,
		At: journal.now().UTC(), Data: data,
	}
	encoded, err := json.Marshal(published)
	if err != nil {
		return domain.Event{}, ErrInvalidEvent
	}
	if len(encoded) > maximumEventBytes || len(encoded) > journal.maxBytes {
		return domain.Event{}, ErrEventTooLarge
	}

	journal.sequence = published.Sequence
	journal.events = append(journal.events, retainedEvent{event: published, bytes: len(encoded)})
	journal.totalBytes += len(encoded)
	for len(journal.events) > journal.maxEvents || journal.totalBytes > journal.maxBytes {
		journal.totalBytes -= journal.events[0].bytes
		journal.events[0] = retainedEvent{}
		journal.events = journal.events[1:]
	}
	close(journal.notify)
	journal.notify = make(chan struct{})
	return cloneJournalEvent(published), nil
}

func (journal *Journal) After(cursor domain.EventCursor) domain.EventPage {
	journal.mu.RLock()
	defer journal.mu.RUnlock()
	latest := journal.cursorLocked()
	page := domain.EventPage{Events: []domain.Event{}, LatestCursor: latest}
	if cursor.Epoch == "" || cursor.Epoch != journal.epoch || cursor.Sequence > journal.sequence {
		page.Reset = true
		return page
	}
	if len(journal.events) > 0 && journal.events[0].event.Sequence > 1 && cursor.Sequence < journal.events[0].event.Sequence {
		page.Reset = true
		return page
	}
	for _, retained := range journal.events {
		if retained.event.Sequence > cursor.Sequence {
			page.Events = append(page.Events, cloneJournalEvent(retained.event))
		}
	}
	return page
}

func (journal *Journal) Subscribe(cursor domain.EventCursor) <-chan struct{} {
	journal.mu.RLock()
	defer journal.mu.RUnlock()
	if journal.closed || cursor.Epoch != journal.epoch || cursor.Sequence != journal.sequence {
		return readyJournalWait
	}
	return journal.notify
}

func (journal *Journal) Close() {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if journal.closed {
		return
	}
	journal.closed = true
	close(journal.notify)
}

func (journal *Journal) cursorLocked() domain.EventCursor {
	return domain.EventCursor{Epoch: journal.epoch, Sequence: journal.sequence}
}

type journalVisit struct {
	typeOf  reflect.Type
	pointer uintptr
}

type journalDataCloner struct {
	stack map[journalVisit]struct{}
	items int
}

func sanitizeJournalData(data map[string]any) map[string]any {
	if data == nil {
		return map[string]any{}
	}
	cloner := journalDataCloner{stack: make(map[journalVisit]struct{})}
	clone, ok := cloner.cloneMap(data, 0)
	if !ok {
		return map[string]any{"status": "invalid_event_data"}
	}
	return clone
}

func (cloner *journalDataCloner) cloneMap(source map[string]any, depth int) (map[string]any, bool) {
	if depth > maximumEventDataDepth || cloner.items+len(source) > maximumEventDataItems {
		return nil, false
	}
	visit := journalVisit{typeOf: reflect.TypeOf(source), pointer: reflect.ValueOf(source).Pointer()}
	if visit.pointer != 0 {
		if _, found := cloner.stack[visit]; found {
			return nil, false
		}
		cloner.stack[visit] = struct{}{}
		defer delete(cloner.stack, visit)
	}
	cloner.items += len(source)
	clone := make(map[string]any, len(source))
	for key, value := range source {
		if !validJournalString(key) {
			return nil, false
		}
		item, ok := cloner.cloneValue(value, depth+1)
		if !ok {
			return nil, false
		}
		clone[key] = item
	}
	return clone, true
}

func (cloner *journalDataCloner) cloneSlice(source []any, depth int) ([]any, bool) {
	if depth > maximumEventDataDepth || cloner.items+len(source) > maximumEventDataItems {
		return nil, false
	}
	visit := journalVisit{typeOf: reflect.TypeOf(source), pointer: reflect.ValueOf(source).Pointer()}
	if visit.pointer != 0 {
		if _, found := cloner.stack[visit]; found {
			return nil, false
		}
		cloner.stack[visit] = struct{}{}
		defer delete(cloner.stack, visit)
	}
	cloner.items += len(source)
	clone := make([]any, len(source))
	for index, value := range source {
		item, ok := cloner.cloneValue(value, depth+1)
		if !ok {
			return nil, false
		}
		clone[index] = item
	}
	return clone, true
}

func (cloner *journalDataCloner) cloneValue(value any, depth int) (any, bool) {
	if depth > maximumEventDataDepth {
		return nil, false
	}
	switch typed := value.(type) {
	case nil:
		return nil, true
	case bool:
		return typed, true
	case string:
		return typed, validJournalString(typed)
	case float32:
		return typed, !math.IsNaN(float64(typed)) && !math.IsInf(float64(typed), 0)
	case float64:
		return typed, !math.IsNaN(typed) && !math.IsInf(typed, 0)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, uintptr:
		return typed, true
	case json.Number:
		if _, err := json.Marshal(typed); err != nil {
			return nil, false
		}
		parsed, err := strconv.ParseFloat(string(typed), 64)
		return typed, err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
	case map[string]any:
		return cloner.cloneMap(typed, depth)
	case []any:
		return cloner.cloneSlice(typed, depth)
	default:
		return nil, false
	}
}

func validJournalString(value string) bool {
	return utf8.ValidString(value) && len(value) <= maximumEventStringBytes
}

func cloneJournalEvent(event domain.Event) domain.Event {
	clone := event
	data, ok := (&journalDataCloner{stack: make(map[journalVisit]struct{})}).cloneMap(event.Data, 0)
	if !ok {
		return domain.Event{Epoch: event.Epoch, Sequence: event.Sequence, Type: event.Type, At: event.At, Data: map[string]any{"status": "invalid_event_data"}}
	}
	clone.Data = data
	return clone
}
