package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTurnUsesTextInputWorkspaceSandboxAndSessionIdentity(t *testing.T) {
	session, transport := startTestSession(t, nil)
	result := startTurn(t.Context(), session, "Do the work")
	turnStart := transport.readRequest(t)
	if methodOf(t, turnStart) != "turn/start" {
		t.Fatalf("%s", turnStart["method"])
	}
	var params TurnStartParams
	mustUnmarshalRaw(t, turnStart["params"], &params)
	if params.ThreadID != "thread-1" || len(params.Input) != 1 || params.Input[0].Type != "text" || params.Input[0].Text != "Do the work" {
		t.Fatalf("%+v", params)
	}
	if params.Cwd == "" || params.SandboxPolicy.Type != "workspaceWrite" || params.SandboxPolicy.NetworkAccess || len(params.SandboxPolicy.WritableRoots) != 1 {
		t.Fatalf("%+v", params)
	}
	respondTurnStarted(t, transport, turnStart, "turn-7")
	transport.sendJSON(t, turnCompletedMessage("thread-1", "turn-7", "completed"))
	got := <-result
	if got.err != nil || got.result.SessionID != "thread-1-turn-7" || got.result.Status != TurnCompleted {
		t.Fatalf("%+v err=%v", got.result, got.err)
	}
}

func TestDynamicToolWaitsUntilTurnStartResponseConfirmsItsIdentity(t *testing.T) {
	active := &activeTurn{threadID: "thread-1", turnID: "", ready: make(chan struct{})}
	session := &Session{threadID: "thread-1", active: active}
	matched := make(chan bool, 1)
	go func() { matched <- session.waitForActiveTurn(t.Context(), "thread-1", "turn-1") }()
	select {
	case <-matched:
		t.Fatal("dynamic tool matched before turn/start was accepted")
	case <-time.After(20 * time.Millisecond):
	}
	session.mu.Lock()
	active.turnID = "turn-1"
	active.readyOnce.Do(func() { close(active.ready) })
	session.mu.Unlock()
	select {
	case ok := <-matched:
		if !ok {
			t.Fatal("dynamic tool did not match the accepted turn")
		}
	case <-time.After(time.Second):
		t.Fatal("dynamic tool remained blocked after turn/start was accepted")
	}
}

func TestTurnMapsFailedInterruptedAndInvalidTerminalStatuses(t *testing.T) {
	for _, test := range []struct {
		status   string
		want     TurnStatus
		wantCode string
		wantErr  bool
	}{
		{status: "failed", want: TurnFailed, wantCode: "turn_failed"},
		{status: "interrupted", want: TurnInterrupted, wantCode: "turn_cancelled"},
		{status: "inProgress", wantErr: true},
	} {
		t.Run(test.status, func(t *testing.T) {
			session, transport := startTestSession(t, nil)
			result := startTurn(t.Context(), session, "work")
			turnStart := transport.readRequest(t)
			respondTurnStarted(t, transport, turnStart, "turn-1")
			message := turnCompletedMessage("thread-1", "turn-1", test.status)
			if test.status == "failed" {
				message["params"].(map[string]any)["turn"].(map[string]any)["error"] = map[string]any{"message": "secret-canary"}
			}
			transport.sendJSON(t, message)
			got := <-result
			if test.wantErr {
				if got.err == nil {
					t.Fatal("invalid terminal status succeeded")
				}
				return
			}
			if got.err != nil || got.result.Status != test.want || got.result.ErrorCode != test.wantCode || got.result.ErrorMessage == "secret-canary" {
				t.Fatalf("%+v err=%v", got.result, got.err)
			}
		})
	}
}

func TestTurnIgnoresMismatchedAndDuplicateTerminalNotifications(t *testing.T) {
	events := make(chan SessionEvent, 16)
	session, transport := startTestSession(t, func(event SessionEvent) { events <- event })
	result := startTurn(t.Context(), session, "work")
	turnStart := transport.readRequest(t)
	respondTurnStarted(t, transport, turnStart, "turn-1")
	transport.sendJSON(t, turnCompletedMessage("thread-other", "turn-1", "completed"))
	select {
	case got := <-result:
		t.Fatalf("mismatched notification completed turn: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}
	transport.sendJSON(t, turnCompletedMessage("thread-1", "turn-1", "completed"))
	transport.sendJSON(t, turnCompletedMessage("thread-1", "turn-1", "completed"))
	if got := <-result; got.err != nil {
		t.Fatal(got.err)
	}
	awaitSessionEvent(t, events, SessionEventTurnNotificationIgnored)
}

func TestTurnStartedEventPrecedesEarlyCompletion(t *testing.T) {
	events := make(chan SessionEvent, 16)
	session, transport := startTestSession(t, func(event SessionEvent) { events <- event })
	result := startTurn(t.Context(), session, "work")
	turnStart := transport.readRequest(t)
	completion := turnCompletedMessage("thread-1", "turn-1", "completed")["params"]
	encoded, err := json.Marshal(completion)
	if err != nil {
		t.Fatal(err)
	}
	session.handleTurnCompleted(encoded)
	respondTurnStarted(t, transport, turnStart, "turn-1")

	first := awaitTurnLifecycleEvent(t, events)
	second := awaitTurnLifecycleEvent(t, events)
	if first.Type != SessionEventTurnStarted || second.Type != SessionEventTurnCompleted {
		t.Fatalf("events = %s then %s", first.Type, second.Type)
	}
	if got := <-result; got.err != nil || got.result.Status != TurnCompleted {
		t.Fatalf("%+v", got)
	}
}

func TestTurnPreservesFirstProvisionalTerminalOutcome(t *testing.T) {
	session, transport := startTestSession(t, nil)
	result := startTurn(t.Context(), session, "work")
	turnStart := transport.readRequest(t)
	session.handleTurnCompleted(json.RawMessage(`{}`))
	validCompletion, err := json.Marshal(turnCompletedMessage("thread-1", "turn-1", "completed")["params"])
	if err != nil {
		t.Fatal(err)
	}
	session.handleTurnCompleted(validCompletion)
	respondTurnStarted(t, transport, turnStart, "turn-1")
	if got := <-result; got.err == nil || got.result.Status == TurnCompleted {
		t.Fatalf("later completion replaced earlier protocol failure: %+v", got)
	}
}

func TestTurnRejectsNonInProgressStartResponse(t *testing.T) {
	session, transport := startTestSession(t, nil)
	result := startTurn(t.Context(), session, "work")
	turnStart := transport.readRequest(t)
	respondResult(t, transport, turnStart, map[string]any{"turn": map[string]any{"id": "turn-1", "status": "completed", "items": []any{}}})
	if got := <-result; got.err == nil {
		t.Fatalf("non-inProgress start response succeeded: %+v", got.result)
	}
}

func TestTurnRejectsStartAndCompletionWithoutRequiredItems(t *testing.T) {
	t.Run("start response", func(t *testing.T) {
		session, transport := startTestSession(t, nil)
		result := startTurn(t.Context(), session, "work")
		turnStart := transport.readRequest(t)
		respondResult(t, transport, turnStart, map[string]any{
			"turn": map[string]any{"id": "turn-1", "status": "inProgress"},
		})
		transport.sendJSON(t, turnCompletedMessage("thread-1", "turn-1", "completed"))
		if got := <-result; got.err == nil {
			t.Fatalf("start response without items succeeded: %+v", got.result)
		}
	})

	t.Run("completion", func(t *testing.T) {
		session, transport := startTestSession(t, nil)
		result := startTurn(t.Context(), session, "work")
		turnStart := transport.readRequest(t)
		respondTurnStarted(t, transport, turnStart, "turn-1")
		transport.sendJSON(t, map[string]any{
			"method": "turn/completed",
			"params": map[string]any{
				"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "completed"},
			},
		})
		if got := <-result; got.err == nil {
			t.Fatalf("completion without items succeeded: %+v", got.result)
		}
	})
}

func TestTurnStartResponseErrorIsSafeAndClearsActiveTurn(t *testing.T) {
	session, transport := startTestSession(t, nil)
	result := startTurn(t.Context(), session, "work")
	turnStart := transport.readRequest(t)
	var id any
	mustUnmarshalRaw(t, turnStart["id"], &id)
	transport.sendJSON(t, map[string]any{
		"id": id,
		"error": map[string]any{
			"code": -32602, "message": "secret-canary", "data": map[string]any{"detail": "secret-canary"},
		},
	})
	got := <-result
	var rpcError *RPCError
	if !errors.As(got.err, &rpcError) || rpcError.Code != -32602 {
		t.Fatalf("%v", got.err)
	}
	if strings.Contains(got.err.Error(), "secret-canary") {
		t.Fatalf("RPC error leaked its raw message: %q", got.err)
	}
	if session.active != nil {
		t.Fatal("failed turn/start retained active ownership")
	}
}

func TestSessionSynchronousCallUsesBoundedRequestTimeout(t *testing.T) {
	options := testSessionOptions(t)
	options.RequestTimeout = 20 * time.Millisecond
	router, transport := newPipeTransport(t, RouterOptions{})
	session := NewSession(router, options)
	result := make(chan error, 1)
	go func() { result <- session.Initialize(t.Context()) }()
	_ = transport.readRequest(t)
	err := <-result
	var protocolError *ProtocolError
	if !errors.As(err, &protocolError) || protocolError.Code != ProtocolErrorRequestTimeout {
		t.Fatalf("%v", err)
	}
}

func TestInterruptTurnUsesBoundedRequestTimeout(t *testing.T) {
	options := testSessionOptions(t)
	options.RequestTimeout = 20 * time.Millisecond
	events := make(chan SessionEvent, 8)
	session, transport := startTestSessionWithOptions(t, options, func(event SessionEvent) { events <- event })
	turnResult := startTurn(t.Context(), session, "work")
	turnStart := transport.readRequest(t)
	respondTurnStarted(t, transport, turnStart, "turn-1")
	awaitSessionEvent(t, events, SessionEventTurnStarted)

	interruptResult := make(chan error, 1)
	go func() { interruptResult <- session.InterruptTurn(t.Context()) }()
	if interrupt := transport.readRequest(t); methodOf(t, interrupt) != "turn/interrupt" {
		t.Fatalf("%s", interrupt["method"])
	}
	err := <-interruptResult
	var protocolError *ProtocolError
	if !errors.As(err, &protocolError) || protocolError.Code != ProtocolErrorRequestTimeout {
		t.Fatalf("%v", err)
	}
	_ = transport.serverOutput.Close()
	<-turnResult
}

func TestTurnCancellationCallsInterrupt(t *testing.T) {
	events := make(chan SessionEvent, 8)
	session, transport := startTestSession(t, func(event SessionEvent) { events <- event })
	ctx, cancel := context.WithCancel(t.Context())
	result := startTurn(ctx, session, "work")
	turnStart := transport.readRequest(t)
	respondTurnStarted(t, transport, turnStart, "turn-1")
	awaitSessionEvent(t, events, SessionEventTurnStarted)
	cancel()
	interrupt := transport.readRequest(t)
	if methodOf(t, interrupt) != "turn/interrupt" {
		t.Fatalf("%s", interrupt["method"])
	}
	respondResult(t, transport, interrupt, map[string]any{})
	if got := <-result; !errors.Is(got.err, context.Canceled) {
		t.Fatalf("%v", got.err)
	}
}

func TestTurnSilenceResetsOnActivityAndPausesForServerRequest(t *testing.T) {
	options := testSessionOptions(t)
	options.SilenceTimeout = 30 * time.Millisecond
	session, transport := startTestSessionWithOptions(t, options, nil)
	result := startTurn(t.Context(), session, "work")
	turnStart := transport.readRequest(t)
	respondTurnStarted(t, transport, turnStart, "turn-1")

	transport.sendJSON(t, map[string]any{"method": "future/activity", "params": map[string]any{}})
	time.Sleep(15 * time.Millisecond)
	transport.sendJSON(t, map[string]any{"id": "operator-1", "method": "item/tool/requestUserInput", "params": map[string]any{}})
	select {
	case <-session.ServerRequests():
	case <-time.After(time.Second):
		t.Fatal("server request missing")
	}
	time.Sleep(60 * time.Millisecond)
	select {
	case got := <-result:
		t.Fatalf("turn timed out while operator request was pending: %+v", got)
	default:
	}
	responseDone := make(chan error, 1)
	go func() { responseDone <- session.Respond("operator-1", map[string]any{"answers": map[string]any{}}) }()
	response := transport.readRequest(t)
	if _, hasMethod := response["method"]; hasMethod {
		t.Fatalf("server response has method: %+v", response)
	}
	if err := <-responseDone; err != nil {
		t.Fatal(err)
	}
	transport.sendJSON(t, turnCompletedMessage("thread-1", "turn-1", "completed"))
	if got := <-result; got.err != nil {
		t.Fatal(got.err)
	}
}

func TestTurnSilenceTimeoutResetsForEveryPhysicalLineAndInterrupts(t *testing.T) {
	options := testSessionOptions(t)
	options.SilenceTimeout = 35 * time.Millisecond
	session, transport := startTestSessionWithOptions(t, options, nil)
	result := startTurn(t.Context(), session, "work")
	turnStart := transport.readRequest(t)
	respondTurnStarted(t, transport, turnStart, "turn-1")

	for index := 0; index < 3; index++ {
		time.Sleep(20 * time.Millisecond)
		transport.sendJSON(t, map[string]any{
			"method": "future/activity", "params": map[string]any{"index": index},
		})
	}
	lastActivity := time.Now()
	interrupt := transport.readRequest(t)
	if methodOf(t, interrupt) != "turn/interrupt" {
		t.Fatalf("%s", interrupt["method"])
	}
	if elapsed := time.Since(lastActivity); elapsed < 25*time.Millisecond {
		t.Fatalf("silence timer was not reset by the final line: %v", elapsed)
	}
	respondResult(t, transport, interrupt, map[string]any{})

	got := <-result
	var protocolError *ProtocolError
	if !errors.As(got.err, &protocolError) || protocolError.Code != ProtocolErrorTurnSilence || !protocolError.Retryable {
		t.Fatalf("%v", got.err)
	}
}

func TestTurnSubprocessExitResolvesWaiter(t *testing.T) {
	session, transport := startTestSession(t, nil)
	result := startTurn(t.Context(), session, "work")
	turnStart := transport.readRequest(t)
	respondTurnStarted(t, transport, turnStart, "turn-1")
	if err := transport.serverOutput.Close(); err != nil {
		t.Fatal(err)
	}
	got := <-result
	var protocolError *ProtocolError
	if !errors.As(got.err, &protocolError) || protocolError.Code != ProtocolErrorTransportClosed {
		t.Fatalf("%v", got.err)
	}
}

func TestTurnFinalCompletionBeforeEOFWinsOverTransportExit(t *testing.T) {
	for iteration := 0; iteration < 20; iteration++ {
		session, transport := startTestSession(t, nil)
		result := startTurn(t.Context(), session, "work")
		turnStart := transport.readRequest(t)
		respondTurnStarted(t, transport, turnStart, "turn-1")
		message, err := json.Marshal(turnCompletedMessage("thread-1", "turn-1", "completed"))
		if err != nil {
			t.Fatal(err)
		}
		writeDone := make(chan error, 1)
		go func() {
			_, writeErr := transport.serverOutput.Write(message)
			if closeErr := transport.serverOutput.Close(); writeErr == nil {
				writeErr = closeErr
			}
			writeDone <- writeErr
		}()
		got := <-result
		if got.err != nil || got.result.Status != TurnCompleted {
			t.Fatalf("iteration %d: result=%+v err=%v", iteration, got.result, got.err)
		}
		if err := <-writeDone; err != nil {
			t.Fatal(err)
		}
	}
}

func TestTurnEarlyCompletionHandoffWinsOverTransportExit(t *testing.T) {
	for _, test := range []struct {
		name             string
		completionTurnID string
		wantErrorCode    string
	}{
		{name: "matching completion", completionTurnID: "turn-1"},
		{name: "mismatched completion", completionTurnID: "turn-other", wantErrorCode: ProtocolErrorTransportClosed},
	} {
		t.Run(test.name, func(t *testing.T) {
			turnStarted := make(chan struct{})
			releaseTurnStarted := make(chan struct{})
			released := false
			t.Cleanup(func() {
				if !released {
					close(releaseTurnStarted)
				}
			})
			session, transport := startTestSession(t, func(event SessionEvent) {
				if event.Type == SessionEventTurnStarted {
					close(turnStarted)
					<-releaseTurnStarted
				}
			})
			result := startTurn(t.Context(), session, "work")
			turnStart := transport.readRequest(t)
			completion, err := json.Marshal(turnCompletedMessage("thread-1", test.completionTurnID, "completed")["params"])
			if err != nil {
				t.Fatal(err)
			}
			session.handleTurnCompleted(completion)
			respondTurnStarted(t, transport, turnStart, "turn-1")
			select {
			case <-turnStarted:
			case <-time.After(time.Second):
				t.Fatal("turn/start did not reach the provisional outcome handoff")
			}
			if err := transport.serverOutput.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-session.pumpDone:
			case <-time.After(time.Second):
				t.Fatal("transport exit did not reach the session pump")
			}
			close(releaseTurnStarted)
			released = true
			got := <-result
			if test.wantErrorCode != "" {
				var protocolError *ProtocolError
				if !errors.As(got.err, &protocolError) || protocolError.Code != test.wantErrorCode {
					t.Fatalf("result=%+v err=%v", got.result, got.err)
				}
				return
			}
			if got.err != nil || got.result.Status != TurnCompleted {
				t.Fatalf("result=%+v err=%v", got.result, got.err)
			}
		})
	}
}

type turnCallResult struct {
	result TurnResult
	err    error
}

func startTurn(ctx context.Context, session *Session, text string) <-chan turnCallResult {
	result := make(chan turnCallResult, 1)
	go func() {
		turn, err := session.StartTurn(ctx, text)
		result <- turnCallResult{result: turn, err: err}
	}()
	return result
}

func startTestSession(t *testing.T, callback func(SessionEvent)) (*Session, *pipeTransport) {
	t.Helper()
	return startTestSessionWithOptions(t, testSessionOptions(t), callback)
}

func startTestSessionWithOptions(t *testing.T, options SessionOptions, callback func(SessionEvent)) (*Session, *pipeTransport) {
	t.Helper()
	options.OnEvent = callback
	router, transport := newPipeTransport(t, RouterOptions{})
	session := NewSession(router, options)
	result := make(chan error, 1)
	go func() {
		_, err := session.Start(t.Context())
		result <- err
	}()
	initialize := transport.readRequest(t)
	respondResult(t, transport, initialize, map[string]any{
		"userAgent": "codex_cli_rs/0.144.1", "codexHome": options.Workspace, "platformFamily": "unix", "platformOs": "macos",
	})
	if initialized := transport.readRequest(t); methodOf(t, initialized) != "initialized" {
		t.Fatalf("%s", initialized["method"])
	}
	threadStart := transport.readRequest(t)
	respondThreadStarted(t, transport, threadStart, options.Workspace, "thread-1")
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	return session, transport
}

func respondTurnStarted(t *testing.T, transport *pipeTransport, request map[string]json.RawMessage, turnID string) {
	t.Helper()
	respondResult(t, transport, request, map[string]any{"turn": map[string]any{"id": turnID, "status": "inProgress", "items": []any{}}})
}

func turnCompletedMessage(threadID, turnID, status string) map[string]any {
	return map[string]any{
		"method": "turn/completed",
		"params": map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": turnID, "status": status, "items": []any{}, "error": nil},
		},
	}
}

func awaitSessionEvent(t *testing.T, events <-chan SessionEvent, kind string) SessionEvent {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event.Type == kind {
				return event
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for session event %s", kind)
		}
	}
}

func awaitTurnLifecycleEvent(t *testing.T, events <-chan SessionEvent) SessionEvent {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event.Type == SessionEventTurnStarted || event.Type == SessionEventTurnCompleted {
				return event
			}
		case <-timer.C:
			t.Fatal("timed out waiting for turn lifecycle event")
		}
	}
}
