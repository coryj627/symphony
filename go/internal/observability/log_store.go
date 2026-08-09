package observability

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	logRingCapacity    = 2000
	defaultQueryLimit  = 100
	maximumQueryLimit  = 200
	degradationWarning = "Symphony logging degraded; recent logs remain available in memory."
)

// LogRecord is an immutable query result describing one sanitized log entry.
type LogRecord struct {
	Sequence uint64         `json:"sequence"`
	Time     time.Time      `json:"time"`
	Level    string         `json:"level"`
	Message  string         `json:"message"`
	Fields   map[string]any `json:"fields,omitempty"`
}

// LogQuery selects restart-scoped in-memory records.
type LogQuery struct {
	Search string
	Level  string
	Before uint64
	Limit  int
}

// LogPage is a newest-first page of immutable records.
type LogPage struct {
	Records    []LogRecord
	NextBefore uint64
	HasMore    bool
	Degraded   bool
}

type retainedLogRecord struct {
	record    LogRecord
	canonical string
}

// LogStore retains a bounded restart-scoped ring and mirrors records to a
// rotating JSONL sink while that sink remains healthy.
type LogStore struct {
	mu       sync.Mutex
	sink     lineSink
	stderr   io.Writer
	ring     []retainedLogRecord
	next     uint64
	degraded bool
	warned   bool
	closed   bool
	closeErr error
}

func newLogStore(sink lineSink, stderr io.Writer) *LogStore {
	if stderr == nil {
		stderr = os.Stderr
	}
	return &LogStore{
		sink:   sink,
		stderr: stderr,
		ring:   make([]retainedLogRecord, 0, logRingCapacity),
	}
}

func (s *LogStore) append(record LogRecord, jsonLine []byte) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.next++
	record.Sequence = s.next
	record = cloneLogRecord(record)
	canonical := strings.TrimSpace(string(append([]byte(nil), jsonLine...)))
	if canonical == "" {
		canonical = toJSON(record)
	}
	entry := retainedLogRecord{record: record, canonical: canonical}
	if len(s.ring) == logRingCapacity {
		copy(s.ring, s.ring[1:])
		s.ring[len(s.ring)-1] = entry
	} else {
		s.ring = append(s.ring, entry)
	}

	if s.closed {
		s.markDegradedLocked()
		return
	}
	if s.degraded || s.sink == nil {
		return
	}
	if err := s.sink.WriteLine(jsonLine); err != nil {
		s.markDegradedLocked()
	}
}

// Query returns newest-first records from the restart-scoped ring.
func (s *LogStore) Query(ctx context.Context, query LogQuery) (LogPage, error) {
	if err := ctx.Err(); err != nil {
		return LogPage{}, err
	}
	if s == nil {
		return LogPage{}, nil
	}
	limit := query.Limit
	if limit <= 0 {
		limit = defaultQueryLimit
	}
	if limit > maximumQueryLimit {
		limit = maximumQueryLimit
	}
	level := strings.TrimSpace(query.Level)
	search := strings.ToLower(strings.TrimSpace(query.Search))

	s.mu.Lock()
	defer s.mu.Unlock()
	page := LogPage{
		Records:  make([]LogRecord, 0, min(limit, len(s.ring))),
		Degraded: s.degraded,
	}
	for index := len(s.ring) - 1; index >= 0; index-- {
		if err := ctx.Err(); err != nil {
			return LogPage{}, err
		}
		entry := s.ring[index]
		if query.Before != 0 && entry.record.Sequence >= query.Before {
			continue
		}
		if level != "" && !strings.EqualFold(entry.record.Level, level) {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(entry.canonical), search) {
			continue
		}
		if len(page.Records) == limit {
			page.HasMore = true
			break
		}
		page.Records = append(page.Records, cloneLogRecord(entry.record))
	}
	if page.HasMore && len(page.Records) > 0 {
		page.NextBefore = page.Records[len(page.Records)-1].Sequence
	}
	return page, nil
}

// Degraded reports whether the file sink has failed or the store was logged
// to after close. Degradation is sticky.
func (s *LogStore) Degraded() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.degraded
}

// Close closes the active file once. Ring queries remain available.
func (s *LogStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	if s.sink != nil {
		s.closeErr = s.sink.Close()
		if s.closeErr != nil {
			s.markDegradedLocked()
		}
	}
	return s.closeErr
}

func (s *LogStore) markDegraded() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.markDegradedLocked()
	s.mu.Unlock()
}

func (s *LogStore) markDegradedLocked() {
	s.degraded = true
	if s.warned {
		return
	}
	s.warned = true
	writeDegradationWarning(s.stderr)
}

func writeDegradationWarning(destination io.Writer) {
	defer func() { _ = recover() }()
	_, _ = io.WriteString(destination, degradationWarning+"\n")
}

func cloneLogRecord(source LogRecord) LogRecord {
	return LogRecord{
		Sequence: source.Sequence,
		Time:     source.Time,
		Level:    source.Level,
		Message:  source.Message,
		Fields:   cloneStringMap(source.Fields),
	}
}

func cloneStringMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = cloneSanitizedValue(value)
	}
	return result
}

func cloneSanitizedValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneStringMap(typed)
	case []any:
		copyValues := make([]any, len(typed))
		for index, item := range typed {
			copyValues[index] = cloneSanitizedValue(item)
		}
		return copyValues
	case []string:
		return append([]string(nil), typed...)
	case []byte:
		return append([]byte(nil), typed...)
	default:
		return typed
	}
}
