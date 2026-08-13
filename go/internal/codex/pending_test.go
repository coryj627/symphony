package codex

import (
	"errors"
	"testing"
)

func TestPendingRejectsDuplicateOwnershipAndRemovesCanceledCall(t *testing.T) {
	registry := newPendingRegistry()
	id := mustRequestID(t, `1`)
	first := newPendingCall()
	if err := registry.register(id, first); err != nil {
		t.Fatal(err)
	}
	if err := registry.register(id, newPendingCall()); !errors.Is(err, ErrDuplicateRequestID) {
		t.Fatalf("got %v", err)
	}
	if !registry.remove(id, first) || registry.len() != 0 {
		t.Fatalf("pending call was not removed: %d", registry.len())
	}
	if registry.complete(id, Response{}) {
		t.Fatal("late response completed a removed call")
	}
}

func TestPendingShutdownResolvesEveryWaiterExactlyOnce(t *testing.T) {
	registry := newPendingRegistry()
	want := newProtocolError("transport_closed", "Codex app-server output closed.", true, nil)
	var calls []*pendingCall
	for index := int64(1); index <= 20; index++ {
		call := newPendingCall()
		calls = append(calls, call)
		if err := registry.register(numericRequestID(index), call); err != nil {
			t.Fatal(err)
		}
	}
	registry.close(want)
	registry.close(errors.New("second close must not replace terminal error"))
	for _, call := range calls {
		outcome := <-call.done
		if !errors.Is(outcome.err, want) {
			t.Fatalf("got %v want %v", outcome.err, want)
		}
		select {
		case <-call.done:
			t.Fatal("waiter resolved more than once")
		default:
		}
	}
	if registry.len() != 0 {
		t.Fatalf("pending calls remain: %d", registry.len())
	}
	if err := registry.register(numericRequestID(21), newPendingCall()); !errors.Is(err, want) {
		t.Fatalf("closed registry returned %v", err)
	}
}

func mustRequestID(t *testing.T, source string) RequestID {
	t.Helper()
	id, err := ParseRequestID([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	return id
}
