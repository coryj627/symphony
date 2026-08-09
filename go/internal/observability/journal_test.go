package observability

import (
	"encoding/json"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
)

func TestJournalAssignsMonotonicSequenceProcessTimeAndDistinctRestartEpochs(t *testing.T) {
	t.Parallel()
	first := NewJournal(JournalOptions{})
	second := NewJournal(JournalOptions{})
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)
	if first.Epoch() == "" || first.Epoch() == second.Epoch() {
		t.Fatalf("restart epochs are not distinct: %q / %q", first.Epoch(), second.Epoch())
	}
	one, err := first.Publish(domain.Event{Epoch: "caller", Sequence: 99, Type: "one", At: time.Unix(1, 0), Data: map[string]any{"n": 1}})
	if err != nil {
		t.Fatal(err)
	}
	two, err := first.Publish(domain.Event{Type: "two", Data: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if one.Epoch != first.Epoch() || one.Sequence != 1 || two.Sequence != 2 || one.At.Equal(time.Unix(1, 0)) || two.At.Before(one.At) {
		t.Fatalf("journal did not own event identity/time: %#v %#v", one, two)
	}
	page := first.After(domain.EventCursor{Epoch: first.Epoch(), Sequence: 0})
	if page.Reset || len(page.Events) != 2 || page.LatestCursor.Sequence != 2 {
		t.Fatalf("initial replay = %#v", page)
	}
	previous := first.After(domain.EventCursor{Epoch: second.Epoch(), Sequence: 0})
	if !previous.Reset || len(previous.Events) != 0 || previous.LatestCursor != first.Cursor() {
		t.Fatalf("previous-process cursor = %#v", previous)
	}
}

func TestJournalReturnsResetWhenSequenceFellOutOfWindow(t *testing.T) {
	t.Parallel()
	journal := NewJournal(JournalOptions{MaxEvents: 2, MaxBytes: 1024})
	t.Cleanup(journal.Close)
	publishEvent(t, journal, "one", map[string]any{})
	publishEvent(t, journal, "two", map[string]any{})
	publishEvent(t, journal, "three", map[string]any{})
	for _, sequence := range []uint64{0, 1} {
		page := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: sequence})
		if !page.Reset || len(page.Events) != 0 || page.LatestCursor.Epoch != journal.Epoch() || page.LatestCursor.Sequence != 3 {
			t.Fatalf("evicted cursor %d returned %#v", sequence, page)
		}
	}
	page := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 2})
	if page.Reset || len(page.Events) != 1 || page.Events[0].Sequence != 3 {
		t.Fatalf("retained cursor replay = %#v", page)
	}
}

func TestJournalResetsMissingMismatchFutureAndGapCursorsWithoutFabricatingHistory(t *testing.T) {
	t.Parallel()
	journal := NewJournal(JournalOptions{})
	t.Cleanup(journal.Close)
	publishEvent(t, journal, "one", map[string]any{})
	for _, cursor := range []domain.EventCursor{
		{},
		{Epoch: "other", Sequence: 1},
		{Epoch: journal.Epoch(), Sequence: 2},
	} {
		page := journal.After(cursor)
		if !page.Reset || len(page.Events) != 0 || page.LatestCursor != journal.Cursor() {
			t.Fatalf("cursor %#v returned %#v", cursor, page)
		}
	}
}

func TestJournalEnforcesExactCountAndEncodedByteWindows(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 8, 8, 12, 0, 0, 123456789, time.UTC)
	measure := newJournalWithDependencies(JournalOptions{MaxEvents: 8, MaxBytes: 4096}, journalDependencies{
		now: func() time.Time { return clock }, random: fixedJournalRandom(1),
	})
	first := publishEvent(t, measure, "one", map[string]any{"value": "a"})
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	measure.Close()

	journal := newJournalWithDependencies(JournalOptions{MaxEvents: 8, MaxBytes: len(encoded)}, journalDependencies{
		now: func() time.Time { return clock }, random: fixedJournalRandom(2),
	})
	t.Cleanup(journal.Close)
	publishEvent(t, journal, "one", map[string]any{"value": "a"})
	publishEvent(t, journal, "two", map[string]any{"value": "b"})
	page := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 1})
	if !page.Reset || page.LatestCursor.Sequence != 2 {
		t.Fatalf("byte eviction did not evict the first complete event: %#v", page)
	}
	latest := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 2})
	if latest.Reset || len(latest.Events) != 0 {
		t.Fatalf("latest byte-bounded cursor = %#v", latest)
	}
}

func TestJournalDeepCopiesIngressAndEgress(t *testing.T) {
	t.Parallel()
	journal := NewJournal(JournalOptions{})
	t.Cleanup(journal.Close)
	data := map[string]any{"nested": map[string]any{"values": []any{"original"}}}
	published := publishEvent(t, journal, "copy", data)
	data["nested"].(map[string]any)["values"].([]any)[0] = "ingress mutation"
	published.Data["nested"].(map[string]any)["values"].([]any)[0] = "return mutation"
	page := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 0})
	page.Events[0].Data["nested"].(map[string]any)["values"].([]any)[0] = "egress mutation"
	again := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 0})
	if got := again.Events[0].Data["nested"].(map[string]any)["values"].([]any)[0]; got != "original" {
		t.Fatalf("journal retained aliased data: %v", got)
	}
}

func TestJournalSanitizesInvalidCyclicCallbackAndBudgetedDataWithoutInvokingCallbacks(t *testing.T) {
	t.Parallel()
	journal := NewJournal(JournalOptions{})
	t.Cleanup(journal.Close)
	cycle := map[string]any{}
	cycle["self"] = cycle
	tooDeep := any("leaf")
	for range 17 {
		tooDeep = []any{tooDeep}
	}
	tooMany := make([]any, 1025)
	tooLong := string(make([]byte, 8193))
	cases := []map[string]any{
		{"bad": math.NaN()},
		{"bad_number": json.Number("+1")},
		cycle,
		{"callback": panicJSONMarshaler{}},
		{"deep": tooDeep},
		{"many": tooMany},
		{"long": tooLong},
	}
	for index, data := range cases {
		published, err := journal.Publish(domain.Event{Type: "invalid", Data: data})
		if err != nil {
			t.Fatalf("case %d: %v", index, err)
		}
		if len(published.Data) != 1 || published.Data["status"] != "invalid_event_data" {
			t.Fatalf("case %d marker = %#v", index, published.Data)
		}
	}
	if journal.Cursor().Sequence != uint64(len(cases)) {
		t.Fatalf("marker publications did not consume exactly one sequence each: %#v", journal.Cursor())
	}
}

func TestJournalRejectsInvalidTypeAndOversizedCompleteEventWithoutConsumingSequence(t *testing.T) {
	t.Parallel()
	journal := NewJournal(JournalOptions{MaxEvents: 4, MaxBytes: 128})
	t.Cleanup(journal.Close)
	for _, event := range []domain.Event{
		{Type: "", Data: map[string]any{}},
		{Type: string(make([]byte, 129)), Data: map[string]any{}},
	} {
		if _, err := journal.Publish(event); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("invalid type error = %v", err)
		}
	}
	if _, err := journal.Publish(domain.Event{Type: "oversized", Data: map[string]any{"value": string(make([]byte, 80))}}); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("oversized event error = %v", err)
	}
	if journal.Cursor().Sequence != 0 {
		t.Fatalf("rejected publications consumed sequence: %#v", journal.Cursor())
	}
}

func TestJournalSubscribeCannotMissWakeupAndCloseWakesWaiters(t *testing.T) {
	t.Parallel()
	journal := NewJournal(JournalOptions{})
	cursor := journal.Cursor()
	wait := journal.Subscribe(cursor)
	select {
	case <-wait:
		t.Fatal("current cursor subscription was ready before publication")
	default:
	}
	publishEvent(t, journal, "one", map[string]any{})
	select {
	case <-wait:
	case <-time.After(time.Second):
		t.Fatal("publication did not wake current subscription")
	}
	if page := journal.After(cursor); page.Reset || len(page.Events) != 1 {
		t.Fatalf("post-wakeup replay = %#v", page)
	}
	ready := journal.Subscribe(cursor)
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("already-available event did not return ready subscription")
	}
	current := journal.Subscribe(journal.Cursor())
	journal.Close()
	journal.Close()
	select {
	case <-current:
	case <-time.After(time.Second):
		t.Fatal("close did not wake waiter")
	}
	select {
	case <-journal.Subscribe(journal.Cursor()):
	default:
		t.Fatal("closed journal subscription was not ready")
	}
	if _, err := journal.Publish(domain.Event{Type: "late"}); !errors.Is(err, ErrJournalClosed) {
		t.Fatalf("post-close publish error = %v", err)
	}
	if journal.Cursor().Sequence != 1 || len(journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 0}).Events) != 1 {
		t.Fatal("close discarded retained reads or consumed sequence")
	}
}

func TestJournalConcurrentPublishReadAndSubscribe(t *testing.T) {
	journal := NewJournal(JournalOptions{MaxEvents: 4096, MaxBytes: 8 << 20})
	t.Cleanup(journal.Close)
	const publishers = 8
	const perPublisher = 50
	var failures atomic.Int64
	var group sync.WaitGroup
	for publisher := range publishers {
		group.Add(1)
		go func(publisher int) {
			defer group.Done()
			for sequence := range perPublisher {
				cursor := journal.Cursor()
				wait := journal.Subscribe(cursor)
				if _, err := journal.Publish(domain.Event{Type: "concurrent", Data: map[string]any{"publisher": publisher, "sequence": sequence}}); err != nil {
					failures.Add(1)
					return
				}
				<-wait
				_ = journal.After(cursor)
			}
		}(publisher)
	}
	group.Wait()
	if failures.Load() != 0 || journal.Cursor().Sequence != publishers*perPublisher {
		t.Fatalf("concurrent journal failures=%d cursor=%#v", failures.Load(), journal.Cursor())
	}
}

func TestJournalRecentReturnsBoundedImmutableTailAndExactResetState(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		journal := NewJournal(JournalOptions{})
		t.Cleanup(journal.Close)

		page := journal.Recent(20)
		if page.Events == nil || len(page.Events) != 0 || page.Reset || page.LatestCursor != journal.Cursor() {
			t.Fatalf("empty recent page = %#v", page)
		}
	})

	t.Run("partial and capped", func(t *testing.T) {
		journal := NewJournal(JournalOptions{})
		t.Cleanup(journal.Close)
		for _, kind := range []string{"one", "two", "three"} {
			publishEvent(t, journal, kind, map[string]any{"kind": kind})
		}

		page := journal.Recent(2)
		if page.Reset || page.LatestCursor != journal.Cursor() || len(page.Events) != 2 || page.Events[0].Sequence != 2 || page.Events[1].Sequence != 3 {
			t.Fatalf("bounded recent page = %#v", page)
		}
		all := journal.Recent(100)
		if all.Reset || len(all.Events) != 3 || all.Events[0].Sequence != 1 || all.Events[2].Sequence != 3 {
			t.Fatalf("uncapped recent page = %#v", all)
		}
	})

	t.Run("count eviction", func(t *testing.T) {
		journal := NewJournal(JournalOptions{MaxEvents: 2, MaxBytes: 4096})
		t.Cleanup(journal.Close)
		for _, kind := range []string{"one", "two", "three"} {
			publishEvent(t, journal, kind, map[string]any{})
		}

		page := journal.Recent(100)
		if !page.Reset || len(page.Events) != 2 || page.Events[0].Sequence != 2 || page.Events[1].Sequence != 3 || page.LatestCursor.Sequence != 3 {
			t.Fatalf("count-evicted recent page = %#v", page)
		}
	})

	t.Run("byte eviction", func(t *testing.T) {
		clock := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
		measure := newJournalWithDependencies(JournalOptions{MaxEvents: 8, MaxBytes: 4096}, journalDependencies{
			now: func() time.Time { return clock }, random: fixedJournalRandom(20),
		})
		encoded, err := json.Marshal(publishEvent(t, measure, "one", map[string]any{"value": "a"}))
		if err != nil {
			t.Fatal(err)
		}
		measure.Close()

		journal := newJournalWithDependencies(JournalOptions{MaxEvents: 8, MaxBytes: len(encoded)}, journalDependencies{
			now: func() time.Time { return clock }, random: fixedJournalRandom(21),
		})
		t.Cleanup(journal.Close)
		publishEvent(t, journal, "one", map[string]any{"value": "a"})
		publishEvent(t, journal, "two", map[string]any{"value": "b"})

		page := journal.Recent(100)
		if !page.Reset || len(page.Events) != 1 || page.Events[0].Sequence != 2 || page.LatestCursor.Sequence != 2 {
			t.Fatalf("byte-evicted recent page = %#v", page)
		}
	})

	t.Run("immutable and readable after close", func(t *testing.T) {
		journal := NewJournal(JournalOptions{})
		publishEvent(t, journal, "copy", map[string]any{"nested": map[string]any{"value": "original"}})
		journal.Close()

		page := journal.Recent(1)
		page.Events[0].Data["nested"].(map[string]any)["value"] = "mutated"
		again := journal.Recent(1)
		if got := again.Events[0].Data["nested"].(map[string]any)["value"]; got != "original" {
			t.Fatalf("recent page retained an aliased value: %v", got)
		}
	})
}

func TestJournalRecentIsConcurrentAndInvokesNoEventCallbacks(t *testing.T) {
	journal := NewJournal(JournalOptions{MaxEvents: 512, MaxBytes: 8 << 20})
	t.Cleanup(journal.Close)

	const publishers = 4
	const perPublisher = 50
	var group sync.WaitGroup
	for publisher := range publishers {
		group.Add(1)
		go func(publisher int) {
			defer group.Done()
			for sequence := range perPublisher {
				if _, err := journal.Publish(domain.Event{Type: "concurrent", Data: map[string]any{"publisher": publisher, "sequence": sequence}}); err != nil {
					t.Errorf("publish: %v", err)
					return
				}
				_ = journal.Recent(20)
			}
		}(publisher)
	}
	group.Wait()

	page := journal.Recent(20)
	if len(page.Events) != 20 || page.LatestCursor.Sequence != publishers*perPublisher || page.Events[0].Sequence != publishers*perPublisher-19 {
		t.Fatalf("concurrent recent page = %#v", page)
	}

	journal.mu.Lock()
	journal.events[len(journal.events)-1].event.Data = map[string]any{"callback": panicJSONMarshaler{}}
	journal.mu.Unlock()
	callbackSafe := journal.Recent(1)
	if got := callbackSafe.Events[0].Data["status"]; got != "invalid_event_data" {
		t.Fatalf("callback data marker = %#v", callbackSafe.Events[0].Data)
	}
}

type panicJSONMarshaler struct{}

func (panicJSONMarshaler) MarshalJSON() ([]byte, error) { panic("must not be invoked") }

func publishEvent(t *testing.T, journal *Journal, kind string, data map[string]any) domain.Event {
	t.Helper()
	published, err := journal.Publish(domain.Event{Type: kind, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	return published
}

func fixedJournalRandom(fill byte) func([]byte) (int, error) {
	return func(destination []byte) (int, error) {
		for index := range destination {
			destination[index] = fill
		}
		return len(destination), nil
	}
}
