package observability

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type failingLineSink struct {
	writes int
	closed bool
}

func (sink *failingLineSink) WriteLine([]byte) error {
	sink.writes++
	return errors.New("sink failed")
}

func (sink *failingLineSink) Close() error {
	sink.closed = true
	return errors.New("close failed")
}

type failingWarningSink struct{}

func (failingWarningSink) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type panickingWarningSink struct{}

func (panickingWarningSink) Write([]byte) (int, error) { panic("stderr panic") }

func TestLogStoreEvictsExactlyOneAndReturnsNewestFirst(t *testing.T) {
	t.Parallel()

	store := newLogStore(nil, io.Discard)
	for sequence := 1; sequence <= logRingCapacity+1; sequence++ {
		store.append(LogRecord{
			Time:    time.Unix(int64(sequence), 0).UTC(),
			Level:   "INFO",
			Message: "record",
			Fields:  map[string]any{"number": sequence},
		}, []byte(`{"msg":"record"}`+"\n"))
	}

	page, err := store.Query(context.Background(), LogQuery{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 200 || page.Records[0].Sequence != logRingCapacity+1 || page.Records[199].Sequence != logRingCapacity-198 {
		t.Fatalf("unexpected first page boundaries: first=%d last=%d count=%d", page.Records[0].Sequence, page.Records[199].Sequence, len(page.Records))
	}
	oldest, err := store.Query(context.Background(), LogQuery{Before: 3, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	if len(oldest.Records) != 1 || oldest.Records[0].Sequence != 2 {
		t.Fatalf("eviction boundary = %#v, want sequence 2 only", oldest.Records)
	}
}

func TestLogStoreFiltersSearchesAndPaginatesWithStableExclusiveCursor(t *testing.T) {
	t.Parallel()

	store := newLogStore(nil, io.Discard)
	for _, record := range []LogRecord{
		{Level: "INFO", Message: "first", Fields: map[string]any{"issue": "ABC-1"}},
		{Level: "ERROR", Message: "second", Fields: map[string]any{"issue": "ABC-2"}},
		{Level: "ERROR", Message: "third", Fields: map[string]any{"issue": "ABC-3"}},
	} {
		store.append(record, []byte(strings.ToLower(toJSON(record))))
	}

	first, err := store.Query(context.Background(), LogQuery{Level: "error", Search: "abc", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Records) != 1 || first.Records[0].Message != "third" || !first.HasMore || first.NextBefore != first.Records[0].Sequence {
		t.Fatalf("first page = %#v", first)
	}
	store.append(LogRecord{Level: "ERROR", Message: "new", Fields: map[string]any{"issue": "ABC-4"}}, []byte(`{"issue":"abc-4"}`))
	second, err := store.Query(context.Background(), LogQuery{Level: "ERROR", Search: "ABC", Before: first.NextBefore, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Records) != 1 || second.Records[0].Message != "second" {
		t.Fatalf("stable second page = %#v", second)
	}
}

func TestLogStoreQueryCancellationAndDeepCopyIsolation(t *testing.T) {
	t.Parallel()

	store := newLogStore(nil, io.Discard)
	store.append(LogRecord{Level: "INFO", Message: "safe", Fields: map[string]any{"nested": map[string]any{"value": "original"}}}, []byte(`{"nested":{"value":"original"}}`))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Query(ctx, LogQuery{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Query error = %v, want context canceled", err)
	}
	page, err := store.Query(context.Background(), LogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	page.Records[0].Fields["nested"].(map[string]any)["value"] = "mutated"
	again, err := store.Query(context.Background(), LogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Records[0].Fields["nested"].(map[string]any)["value"] != "original" {
		t.Fatal("query result mutated retained record")
	}
}

func TestLogStoreDegradesOnceAndKeepsRingAcrossFailureAndClose(t *testing.T) {
	t.Parallel()

	sink := &failingLineSink{}
	var warning strings.Builder
	store := newLogStore(sink, &warning)
	store.append(LogRecord{Level: "ERROR", Message: "one"}, []byte("one\n"))
	store.append(LogRecord{Level: "ERROR", Message: "two"}, []byte("two\n"))
	if !store.Degraded() || sink.writes != 1 {
		t.Fatalf("degraded=%v writes=%d, want true and 1", store.Degraded(), sink.writes)
	}
	if strings.Count(warning.String(), degradationWarning) != 1 {
		t.Fatalf("warning count = %d, want 1", strings.Count(warning.String(), degradationWarning))
	}
	if err := store.Close(); err == nil {
		t.Fatal("sink close error was hidden")
	}
	if err := store.Close(); err == nil {
		t.Fatal("idempotent close did not retain the original close error")
	}
	store.append(LogRecord{Level: "INFO", Message: "after close"}, []byte("after\n"))
	page, err := store.Query(context.Background(), LogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 3 || !page.Degraded {
		t.Fatalf("post-close page = %#v", page)
	}
	if strings.Count(warning.String(), degradationWarning) != 1 {
		t.Fatal("degradation warning repeated")
	}
}

func TestLogStoreIgnoresFailingWarningSink(t *testing.T) {
	t.Parallel()

	store := newLogStore(&failingLineSink{}, failingWarningSink{})
	store.append(LogRecord{Level: "ERROR", Message: "safe"}, []byte("safe\n"))
	if !store.Degraded() {
		t.Fatal("store did not degrade when both sinks failed")
	}
}

func TestLogStoreContainsPanickingWarningSink(t *testing.T) {
	t.Parallel()

	store := newLogStore(&failingLineSink{}, panickingWarningSink{})
	store.append(LogRecord{Level: "ERROR", Message: "safe"}, []byte("safe\n"))
	if !store.Degraded() {
		t.Fatal("store did not degrade when warning sink panicked")
	}
}
