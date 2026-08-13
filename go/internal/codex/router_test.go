package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRouterDoesNotLetLateResponseCompleteReusedCall(t *testing.T) {
	router, transport := newPipeTransport(t, RouterOptions{MaxLineBytes: 10 << 20})
	firstContext, cancelFirst := context.WithCancel(t.Context())
	first := startCall(firstContext, router, "initialize")
	firstRequest := transport.readRequest(t)
	firstID := string(firstRequest["id"])
	cancelFirst()
	if got := <-first; !errors.Is(got.err, context.Canceled) {
		t.Fatalf("first call: %v", got.err)
	}

	second := startCall(t.Context(), router, "thread/start")
	secondRequest := transport.readRequest(t)
	secondID := string(secondRequest["id"])
	if firstID == secondID {
		t.Fatalf("request ID was reused: %s", firstID)
	}
	transport.sendRaw(t, []byte(fmt.Sprintf(`{"id":%s,"result":{}}`+"\n", firstID)))
	select {
	case got := <-second:
		t.Fatalf("late response completed second call: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}
	transport.sendRaw(t, []byte(fmt.Sprintf(`{"id":%s,"result":{"thread":{"id":"t2"}}}`+"\n", secondID)))
	got := <-second
	if got.err != nil || got.value["thread"].(map[string]any)["id"] != "t2" {
		t.Fatalf("%+v", got)
	}
	awaitEventCode(t, router.Events(), ProtocolEventLateResponse)
}

func TestRouterHandlesNumericAndStringResponseIDsAndReportsDuplicates(t *testing.T) {
	router, transport := newPipeTransport(t, RouterOptions{})
	call := startCall(t.Context(), router, "initialize")
	request := transport.readRequest(t)
	id := string(request["id"])
	transport.sendRaw(t, []byte(fmt.Sprintf(`{"id":%s,"result":{"ok":true}}`+"\n", id)))
	if got := <-call; got.err != nil || got.value["ok"] != true {
		t.Fatalf("%+v", got)
	}
	transport.sendRaw(t, []byte(fmt.Sprintf(`{"id":%s,"result":{"duplicate":true}}`+"\n", id)))
	awaitEventCode(t, router.Events(), ProtocolEventLateResponse)

	transport.sendJSON(t, map[string]any{"id": "server-owned", "result": map[string]any{}})
	awaitEventCode(t, router.Events(), ProtocolEventLateResponse)
}

func TestRouterDeliversServerRequestsAndUnknownNotificationsWithoutFailing(t *testing.T) {
	router, transport := newPipeTransport(t, RouterOptions{})
	transport.sendJSON(t, map[string]any{
		"id": "approval-1", "method": "item/tool/requestUserInput", "params": map[string]any{"question": "safe"},
	})
	select {
	case request := <-router.ServerRequests():
		if request.ID.Token() != `"approval-1"` || request.Method != "item/tool/requestUserInput" {
			t.Fatalf("%+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("server request was not delivered")
	}

	transport.sendJSON(t, map[string]any{"method": "future/notification", "params": map[string]any{"x": 1}})
	select {
	case notification := <-router.Notifications():
		if notification.Method != "future/notification" {
			t.Fatalf("%+v", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("notification was not delivered")
	}
	if err := router.Err(); err != nil {
		t.Fatalf("unknown notification stopped router: %v", err)
	}
}

func TestRouterRespondsToStringAndNumericServerRequestsExactlyOnce(t *testing.T) {
	for _, rawID := range []json.RawMessage{json.RawMessage(`"approval-1"`), json.RawMessage(`17`)} {
		t.Run(string(rawID), func(t *testing.T) {
			router, transport := newPipeTransport(t, RouterOptions{})
			transport.sendRaw(t, []byte(fmt.Sprintf(
				`{"id":%s,"method":"item/tool/requestUserInput","params":{}}`+"\n",
				rawID,
			)))
			var request ServerRequest
			select {
			case request = <-router.ServerRequests():
			case <-time.After(time.Second):
				t.Fatal("server request was not delivered")
			}

			responseDone := make(chan error, 1)
			go func() {
				responseDone <- router.Respond(request.ID, map[string]any{"accepted": true})
			}()
			response := transport.readRequest(t)
			if err := <-responseDone; err != nil {
				t.Fatal(err)
			}
			if string(response["id"]) != request.ID.Token() || string(response["result"]) != `{"accepted":true}` {
				t.Fatalf("response = %v", response)
			}
			if err := router.Respond(request.ID, map[string]any{}); err == nil {
				t.Fatal("server request was answered twice")
			}
			awaitEventCode(t, router.Events(), ProtocolEventServerRequestResolved)
		})
	}
}

func TestRouterRejectsReusedPendingServerRequestID(t *testing.T) {
	router, transport := newPipeTransport(t, RouterOptions{})
	request := []byte(`{"id":"approval-1","method":"item/tool/requestUserInput","params":{}}` + "\n")
	transport.sendRaw(t, request)
	select {
	case <-router.ServerRequests():
	case <-time.After(time.Second):
		t.Fatal("first server request was not delivered")
	}
	transport.sendRaw(t, request)
	select {
	case <-router.Done():
		var protocolError *ProtocolError
		if !errors.As(router.Err(), &protocolError) || protocolError.Code != ProtocolErrorMalformedMessage {
			t.Fatalf("%v", router.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("reused pending server request ID did not stop router")
	}
}

func TestRouterEveryPhysicalLineEmitsActivityBeforeMalformedShutdown(t *testing.T) {
	router, transport := newPipeTransport(t, RouterOptions{})
	transport.sendRaw(t, []byte(`{"unknown":true}`+"\n"))
	awaitEventCode(t, router.Events(), ProtocolEventActivity)
	select {
	case <-router.Done():
		var protocolError *ProtocolError
		if !errors.As(router.Err(), &protocolError) || protocolError.Code != ProtocolErrorMalformedMessage {
			t.Fatalf("%v", router.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("malformed message did not stop router")
	}
}

func TestRouterIncompleteFinalLineFailsAsMalformedWithoutLeakingPayload(t *testing.T) {
	router, transport := newPipeTransport(t, RouterOptions{})
	writeDone := make(chan error, 1)
	go func() {
		_, err := transport.serverOutput.Write([]byte(`{"method":"secret-canary"`))
		if closeErr := transport.serverOutput.Close(); err == nil {
			err = closeErr
		}
		writeDone <- err
	}()
	awaitEventCode(t, router.Events(), ProtocolEventActivity)
	<-router.Done()
	var protocolError *ProtocolError
	if !errors.As(router.Err(), &protocolError) || protocolError.Code != ProtocolErrorMalformedMessage {
		t.Fatalf("%v", router.Err())
	}
	if bytes.Contains([]byte(protocolError.Error()), []byte("secret-canary")) {
		t.Fatalf("malformed payload leaked: %q", protocolError.Error())
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
}

func TestRouterRejectsOversizeLine(t *testing.T) {
	router, transport := newPipeTransport(t, RouterOptions{MaxLineBytes: 64})
	transport.sendRaw(t, append(bytes.Repeat([]byte("x"), 65), '\n'))
	select {
	case <-router.Done():
		if !errors.Is(router.Err(), ErrMessageTooLarge) {
			t.Fatalf("%v", router.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("oversize line did not stop router")
	}
}

func TestRouterAcceptsValidMessageAtExactLineLimit(t *testing.T) {
	const maxBytes = 64
	prefix := []byte(`{"method":"future/notification","params":"`)
	suffix := []byte(`"}`)
	padding := bytes.Repeat([]byte("x"), maxBytes-len(prefix)-len(suffix))
	line := append(append(prefix, padding...), suffix...)
	if len(line) != maxBytes {
		t.Fatalf("fixture has %d bytes", len(line))
	}
	router, transport := newPipeTransport(t, RouterOptions{MaxLineBytes: maxBytes})
	transport.sendRaw(t, append(line, '\n'))
	select {
	case notification := <-router.Notifications():
		if notification.Method != "future/notification" {
			t.Fatalf("%+v", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("max-sized valid message was not delivered")
	}
	if err := router.Err(); err != nil {
		t.Fatalf("max-sized valid message stopped router: %v", err)
	}
}

func TestRouterRequestTimeoutAndCancellationRemovePendingOwnership(t *testing.T) {
	for _, test := range []struct {
		name    string
		context func() (context.Context, context.CancelFunc)
		want    error
	}{
		{
			name: "timeout",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(t.Context(), 20*time.Millisecond)
			},
			want: context.DeadlineExceeded,
		},
		{
			name: "cancellation",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(t.Context())
				return ctx, cancel
			},
			want: context.Canceled,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			router, transport := newPipeTransport(t, RouterOptions{})
			ctx, cancel := test.context()
			call := startCall(ctx, router, "initialize")
			_ = transport.readRequest(t)
			if test.name == "cancellation" {
				cancel()
			} else {
				defer cancel()
			}
			if got := <-call; !errors.Is(got.err, test.want) {
				t.Fatalf("got %v want %v", got.err, test.want)
			}
			if router.pending.len() != 0 {
				t.Fatalf("pending calls remain: %d", router.pending.len())
			}
		})
	}
}

func TestRouterTimeoutHasStableRetryableCode(t *testing.T) {
	router, transport := newPipeTransport(t, RouterOptions{})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	call := startCall(ctx, router, "initialize")
	_ = transport.readRequest(t)
	got := <-call
	var protocolError *ProtocolError
	if !errors.As(got.err, &protocolError) || protocolError.Code != ProtocolErrorRequestTimeout || !protocolError.Retryable {
		t.Fatalf("%v", got.err)
	}
	if !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("timeout cause missing: %v", got.err)
	}
}

func TestRouterFailsClosedWhenBoundedDeliveryQueueIsFull(t *testing.T) {
	router, transport := newPipeTransport(t, RouterOptions{RequestBuffer: 1})
	transport.sendJSON(t, map[string]any{"id": "one", "method": "item/tool/call", "params": map[string]any{}})
	transport.sendJSON(t, map[string]any{"id": "two", "method": "item/tool/call", "params": map[string]any{}})
	select {
	case <-router.Done():
		var protocolError *ProtocolError
		if !errors.As(router.Err(), &protocolError) || protocolError.Code != ProtocolErrorBackpressure {
			t.Fatalf("%v", router.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("full delivery queue did not stop router")
	}
}

func TestRouterNeverWrapsRequestIDsAfterInt64Exhaustion(t *testing.T) {
	router, _ := newPipeTransport(t, RouterOptions{})
	router.nextID.Store(int64(^uint64(0) >> 1))
	err := router.Call(t.Context(), "initialize", nil, nil)
	var protocolError *ProtocolError
	if !errors.As(err, &protocolError) || protocolError.Code != ProtocolErrorRequestID {
		t.Fatalf("%v", err)
	}
}

func TestRouterSubprocessEOFResolvesPendingCall(t *testing.T) {
	router, transport := newPipeTransport(t, RouterOptions{})
	call := startCall(t.Context(), router, "initialize")
	_ = transport.readRequest(t)
	if err := transport.serverOutput.Close(); err != nil {
		t.Fatal(err)
	}
	got := <-call
	var protocolError *ProtocolError
	if !errors.As(got.err, &protocolError) || protocolError.Code != ProtocolErrorTransportClosed || !protocolError.Retryable {
		t.Fatalf("%v", got.err)
	}
}

func TestRouterProcessesFinalCompleteResponseBeforeEOF(t *testing.T) {
	router, transport := newPipeTransport(t, RouterOptions{})
	call := startCall(t.Context(), router, "initialize")
	request := transport.readRequest(t)
	writeDone := make(chan error, 1)
	go func() {
		_, err := transport.serverOutput.Write([]byte(fmt.Sprintf(`{"id":%s,"result":{"ok":true}}`, request["id"])))
		if closeErr := transport.serverOutput.Close(); err == nil {
			err = closeErr
		}
		writeDone <- err
	}()
	if got := <-call; got.err != nil || got.value["ok"] != true {
		t.Fatalf("%+v", got)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
}

func TestRouterSerializesConcurrentWrites(t *testing.T) {
	transport := &concurrencyDetectingTransport{readDone: make(chan struct{})}
	router := NewRouter(transport, RouterOptions{})
	defer router.Close()

	const count = 100
	var wait sync.WaitGroup
	wait.Add(count)
	for index := 0; index < count; index++ {
		go func(index int) {
			defer wait.Done()
			if err := router.Notify("test/event", map[string]any{"index": index}); err != nil {
				t.Errorf("notify: %v", err)
			}
		}(index)
	}
	wait.Wait()
	if transport.concurrent.Load() {
		t.Fatal("transport Write was entered concurrently")
	}
	lines := bytes.Split(bytes.TrimSpace(transport.bytes()), []byte("\n"))
	if len(lines) != count {
		t.Fatalf("got %d lines want %d", len(lines), count)
	}
	for _, line := range lines {
		if !json.Valid(line) {
			t.Fatalf("interleaved JSON: %q", line)
		}
	}
}

func TestRouterCorrelatesOneHundredConcurrentCalls(t *testing.T) {
	router, transport := newPipeTransport(t, RouterOptions{EventBuffer: 512})
	const count = 100
	results := make([]<-chan callResult, count)
	for index := range count {
		results[index] = startCall(t.Context(), router, fmt.Sprintf("test/%d", index))
	}
	requests := make([]map[string]json.RawMessage, count)
	for index := range count {
		requests[index] = transport.readRequest(t)
	}
	for index := count - 1; index >= 0; index-- {
		transport.sendRaw(t, []byte(fmt.Sprintf(`{"id":%s,"result":{"id":%s}}`+"\n", requests[index]["id"], requests[index]["id"])))
	}
	for index, result := range results {
		got := <-result
		if got.err != nil {
			t.Fatalf("call %d: %v", index, got.err)
		}
		if got.value["id"].(float64) <= 0 {
			t.Fatalf("call %d: %+v", index, got.value)
		}
	}
}

func awaitEventCode(t *testing.T, events <-chan ProtocolEvent, code string) ProtocolEvent {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event.Code == code {
				return event
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for event %q", code)
		}
	}
}

type concurrencyDetectingTransport struct {
	readDone   chan struct{}
	closeOnce  sync.Once
	active     atomic.Int32
	concurrent atomic.Bool
	mu         sync.Mutex
	buffer     bytes.Buffer
	closed     atomic.Bool
}

func (transport *concurrencyDetectingTransport) Read(target []byte) (int, error) {
	<-transport.readDone
	return 0, io.EOF
}

func (transport *concurrencyDetectingTransport) Write(source []byte) (int, error) {
	if transport.active.Add(1) != 1 {
		transport.concurrent.Store(true)
	}
	defer transport.active.Add(-1)
	time.Sleep(time.Microsecond)
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.buffer.Write(source)
}

func (transport *concurrencyDetectingTransport) Close() error {
	transport.closeOnce.Do(func() {
		transport.closed.Store(true)
		close(transport.readDone)
	})
	return nil
}

func (transport *concurrencyDetectingTransport) bytes() []byte {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return bytes.Clone(transport.buffer.Bytes())
}
