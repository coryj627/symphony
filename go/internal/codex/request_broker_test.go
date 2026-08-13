package codex

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
)

func TestOperatorRequestRedactsEveryUserVisibleFieldBeforeStorage(t *testing.T) {
	const secret = "operator-request-secret-canary"
	redactor := observability.NewRedactor(nil, nil)
	redactor.RegisterSecret([]byte(secret))
	recorder := newProtocolDecisionRecorder()
	context := testServerRequestContext(recorder, "item/commandExecution/requestApproval", map[string]any{
		"itemId": "item-1", "threadId": "thread-1", "turnId": "turn-1", "startedAtMs": 1,
		"command": "printf " + secret, "reason": "reason " + secret,
		"availableDecisions": []any{"accept", "decline"},
	})
	broker := NewRequestBroker(RequestBrokerOptions{
		Redactor: redactor,
		NewID:    func() string { return "request-1" },
	})
	request, err := broker.Open(context)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("operator request retained a registered secret: %s", encoded)
	}
	if !strings.Contains(string(encoded), "[REDACTED]") {
		t.Fatalf("operator request did not preserve a redaction marker: %s", encoded)
	}
	for _, pending := range broker.Pending() {
		encoded, err = json.Marshal(pending)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("pending request retained a registered secret: %s", encoded)
		}
	}
}

func TestOperatorRequestExpiresAfterAtMostElevenWindows(t *testing.T) {
	clock := newBrokerFakeClock(time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC))
	recorder := newProtocolDecisionRecorder()
	failures := make(chan string, 2)
	broker := NewRequestBroker(RequestBrokerOptions{
		Clock: clock, Window: 10 * time.Minute,
		FailSession: func(sessionID, _ string) { failures <- sessionID },
		NewID:       func() string { return "request-1" },
	})
	request, err := broker.Open(commandRequestContext(recorder))
	if err != nil {
		t.Fatal(err)
	}
	for extension := 0; extension < 10; extension++ {
		clock.Advance(9*time.Minute + 45*time.Second)
		if err := broker.Extend(request.ID); err != nil {
			t.Fatalf("extension %d: %v", extension+1, err)
		}
	}
	if err := broker.Extend(request.ID); !errors.Is(err, ErrExtensionLimit) {
		t.Fatalf("eleventh extension = %v", err)
	}
	clock.Advance(10 * time.Minute)
	recorder.awaitDecision(t, map[string]any{"decision": "cancel"})
	select {
	case got := <-failures:
		if got != "thread-1-turn-1" {
			t.Fatalf("failed session = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("expired request did not fail its session")
	}
	select {
	case duplicate := <-failures:
		t.Fatalf("session failed twice: %q", duplicate)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestOperatorResponseIsAcceptedExactlyOnce(t *testing.T) {
	recorder := newProtocolDecisionRecorder()
	broker := NewRequestBroker(RequestBrokerOptions{NewID: func() string { return "request-1" }})
	request, err := broker.Open(fileRequestContext(recorder))
	if err != nil {
		t.Fatal(err)
	}
	response := domain.OperatorResponse{RequestID: request.ID, SessionID: request.SessionID, ChoiceID: "decline"}
	if err := broker.Respond(response); err != nil {
		t.Fatal(err)
	}
	if err := broker.Respond(domain.OperatorResponse{RequestID: request.ID, SessionID: request.SessionID, ChoiceID: "accept"}); !errors.Is(err, ErrStaleRequest) {
		t.Fatalf("second response = %v", err)
	}
	recorder.awaitDecision(t, map[string]any{"decision": "decline"})
}

func TestOperatorResponseRejectsWrongSessionAndInvalidChoiceWithoutMutation(t *testing.T) {
	recorder := newProtocolDecisionRecorder()
	broker := NewRequestBroker(RequestBrokerOptions{NewID: func() string { return "request-1" }})
	request, err := broker.Open(fileRequestContext(recorder))
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Respond(domain.OperatorResponse{RequestID: request.ID, SessionID: "wrong", ChoiceID: "accept"}); !errors.Is(err, ErrStaleRequest) {
		t.Fatalf("wrong session = %v", err)
	}
	if err := broker.Respond(domain.OperatorResponse{RequestID: request.ID, SessionID: request.SessionID, ChoiceID: "invented"}); !errors.Is(err, ErrInvalidOperatorResponse) {
		t.Fatalf("invalid choice = %v", err)
	}
	if len(broker.Pending()) != 1 {
		t.Fatal("invalid response consumed the pending request")
	}
	select {
	case decision := <-recorder.responses:
		t.Fatalf("protocol response sent early: %#v", decision)
	default:
	}
}

func TestOperatorRequestWarningFiresOncePerWindow(t *testing.T) {
	clock := newBrokerFakeClock(time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC))
	changes := make(chan struct{}, 8)
	broker := NewRequestBroker(RequestBrokerOptions{
		Clock: clock, Window: time.Minute, WarningLead: 20 * time.Second,
		OnChange: func() { changes <- struct{}{} }, NewID: func() string { return "request-1" },
	})
	request, err := broker.Open(commandRequestContext(newProtocolDecisionRecorder()))
	if err != nil {
		t.Fatal(err)
	}
	<-changes // open
	clock.Advance(39 * time.Second)
	select {
	case <-changes:
		t.Fatal("warning fired early")
	default:
	}
	clock.Advance(time.Second)
	select {
	case <-changes:
	case <-time.After(time.Second):
		t.Fatal("warning did not fire")
	}
	if err := broker.Extend(request.ID); err != nil {
		t.Fatal(err)
	}
	<-changes // extension
	clock.Advance(40 * time.Second)
	select {
	case <-changes:
	case <-time.After(time.Second):
		t.Fatal("extended warning did not fire")
	}
}

func TestOperatorUserInputMapsOptionsFreeTextAndSecretAnswers(t *testing.T) {
	const secret = "answer-secret-canary"
	redactor := observability.NewRedactor(nil, nil)
	recorder := newProtocolDecisionRecorder()
	broker := NewRequestBroker(RequestBrokerOptions{Redactor: redactor, NewID: func() string { return "request-1" }})
	request, err := broker.Open(userInputRequestContext(recorder))
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Questions) != 3 || !request.Questions[1].AllowsOther || !request.Questions[2].IsSecret {
		t.Fatalf("questions = %#v", request.Questions)
	}
	response := domain.OperatorResponse{
		RequestID: request.ID, SessionID: request.SessionID,
		Answers: map[string][]string{
			"platform": {"option-2"}, "detail": {"typed detail"}, "token": {secret},
		},
	}
	if err := broker.Respond(response); err != nil {
		t.Fatal(err)
	}
	recorder.awaitDecision(t, map[string]any{"answers": map[string]any{
		"platform": map[string]any{"answers": []any{"macOS"}},
		"detail":   map[string]any{"answers": []any{"typed detail"}},
		"token":    map[string]any{"answers": []any{secret}},
	}})
	if got := redactor.Value("audit " + secret); got != "audit [REDACTED]" {
		t.Fatalf("secret answer was not registered: %q", got)
	}
}

func TestOperatorPermissionGrantReturnsOnlyDisplayedExactProfile(t *testing.T) {
	recorder := newProtocolDecisionRecorder()
	broker := NewRequestBroker(RequestBrokerOptions{NewID: func() string { return "request-1" }})
	request, err := broker.Open(permissionRequestContext(recorder))
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Details) != 2 || request.Details[0].Value != "/workspace" || request.Details[1].Value != `{"network":{"enabled":true}}` {
		t.Fatalf("displayed permission details = %#v", request.Details)
	}
	if err := broker.Respond(domain.OperatorResponse{RequestID: request.ID, SessionID: request.SessionID, ChoiceID: "grant_session"}); err != nil {
		t.Fatal(err)
	}
	recorder.awaitDecision(t, map[string]any{
		"permissions": map[string]any{"network": map[string]any{"enabled": true}}, "scope": "session",
	})
}

func TestOperatorCommandAndFileApprovalsMapEveryPinnedDecision(t *testing.T) {
	commandDecisions := []struct {
		choice   string
		decision any
	}{
		{choice: "accept", decision: "accept"},
		{choice: "accept_for_session", decision: "acceptForSession"},
		{choice: "decline", decision: "decline"},
		{choice: "cancel", decision: "cancel"},
		{choice: "accept_with_execpolicy_amendment", decision: map[string]any{"acceptWithExecpolicyAmendment": map[string]any{"execpolicy_amendment": []any{"go", "test"}}}},
		{choice: "apply_network_policy_amendment_1", decision: map[string]any{"applyNetworkPolicyAmendment": map[string]any{"network_policy_amendment": map[string]any{"action": "allow", "host": "example.invalid"}}}},
	}
	for _, test := range commandDecisions {
		t.Run("command_"+test.choice, func(t *testing.T) {
			recorder := newProtocolDecisionRecorder()
			context := testServerRequestContext(recorder, "item/commandExecution/requestApproval", map[string]any{
				"itemId": "item-1", "threadId": "thread-1", "turnId": "turn-1", "startedAtMs": 1,
				"command": "go test ./...", "availableDecisions": []any{test.decision},
			})
			broker := NewRequestBroker(RequestBrokerOptions{NewID: func() string { return "request-1" }})
			request, err := broker.Open(context)
			if err != nil {
				t.Fatal(err)
			}
			if len(request.Choices) != 1 || request.Choices[0].ID != test.choice || request.Choices[0].Description == "" {
				t.Fatalf("choice = %#v", request.Choices)
			}
			if err := broker.Respond(domain.OperatorResponse{RequestID: request.ID, SessionID: request.SessionID, ChoiceID: test.choice}); err != nil {
				t.Fatal(err)
			}
			recorder.awaitDecision(t, map[string]any{"decision": test.decision})
		})
	}

	for _, test := range []struct {
		choice   string
		decision string
	}{
		{choice: "accept", decision: "accept"},
		{choice: "accept_for_session", decision: "acceptForSession"},
		{choice: "decline", decision: "decline"},
		{choice: "cancel", decision: "cancel"},
	} {
		t.Run("file_"+test.choice, func(t *testing.T) {
			recorder := newProtocolDecisionRecorder()
			broker := NewRequestBroker(RequestBrokerOptions{NewID: func() string { return "request-1" }})
			request, err := broker.Open(fileRequestContext(recorder))
			if err != nil {
				t.Fatal(err)
			}
			if err := broker.Respond(domain.OperatorResponse{RequestID: request.ID, SessionID: request.SessionID, ChoiceID: test.choice}); err != nil {
				t.Fatal(err)
			}
			recorder.awaitDecision(t, map[string]any{"decision": test.decision})
		})
	}
}

func TestOperatorPermissionApprovalMapsTurnSessionAndDenial(t *testing.T) {
	for _, test := range []struct {
		choice      string
		permissions map[string]any
		scope       string
	}{
		{choice: "grant_turn", permissions: map[string]any{"network": map[string]any{"enabled": true}}, scope: "turn"},
		{choice: "grant_session", permissions: map[string]any{"network": map[string]any{"enabled": true}}, scope: "session"},
		{choice: "decline", permissions: map[string]any{}, scope: "turn"},
	} {
		t.Run(test.choice, func(t *testing.T) {
			recorder := newProtocolDecisionRecorder()
			broker := NewRequestBroker(RequestBrokerOptions{NewID: func() string { return "request-1" }})
			request, err := broker.Open(permissionRequestContext(recorder))
			if err != nil {
				t.Fatal(err)
			}
			if err := broker.Respond(domain.OperatorResponse{RequestID: request.ID, SessionID: request.SessionID, ChoiceID: test.choice}); err != nil {
				t.Fatal(err)
			}
			recorder.awaitDecision(t, map[string]any{"permissions": test.permissions, "scope": test.scope})
		})
	}
}

func TestOperatorUserInputRejectsInvalidAnswerSetsWithoutConsumingRequest(t *testing.T) {
	for name, answers := range map[string]map[string][]string{
		"unknown option":   {"platform": {"invented"}, "detail": {"detail"}, "token": {"secret"}},
		"missing question": {"platform": {"option-1"}, "detail": {"detail"}},
		"unknown question": {"platform": {"option-1"}, "detail": {"detail"}, "token": {"secret"}, "extra": {"value"}},
		"empty secret":     {"platform": {"option-1"}, "detail": {"detail"}, "token": {""}},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := newProtocolDecisionRecorder()
			broker := NewRequestBroker(RequestBrokerOptions{NewID: func() string { return "request-1" }})
			request, err := broker.Open(userInputRequestContext(recorder))
			if err != nil {
				t.Fatal(err)
			}
			err = broker.Respond(domain.OperatorResponse{RequestID: request.ID, SessionID: request.SessionID, Answers: answers})
			if !errors.Is(err, ErrInvalidOperatorResponse) || len(broker.Pending()) != 1 {
				t.Fatalf("Respond() = %v pending=%d", err, len(broker.Pending()))
			}
			select {
			case response := <-recorder.responses:
				t.Fatalf("invalid answers wrote protocol response %#v", response)
			default:
			}
		})
	}
}

func TestOperatorCancelSessionAnswersPendingRequestExactlyOnce(t *testing.T) {
	recorder := newProtocolDecisionRecorder()
	broker := NewRequestBroker(RequestBrokerOptions{NewID: func() string { return "request-1" }})
	request, err := broker.Open(commandRequestContext(recorder))
	if err != nil {
		t.Fatal(err)
	}
	broker.CancelSession(request.SessionID)
	broker.CancelSession(request.SessionID)
	recorder.awaitDecision(t, map[string]any{"decision": "cancel"})
	if len(broker.Pending()) != 0 {
		t.Fatal("canceled request remained pending")
	}
	select {
	case duplicate := <-recorder.responses:
		t.Fatalf("session cancellation wrote twice: %#v", duplicate)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestOperatorResponseTimeoutRaceWritesExactlyOnce(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		clock := newBrokerFakeClock(time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC))
		recorder := newProtocolDecisionRecorder()
		broker := NewRequestBroker(RequestBrokerOptions{Clock: clock, Window: time.Minute, NewID: func() string { return "request-1" }})
		request, err := broker.Open(fileRequestContext(recorder))
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			_ = broker.Respond(domain.OperatorResponse{RequestID: request.ID, SessionID: request.SessionID, ChoiceID: "decline"})
		}()
		go func() {
			defer wait.Done()
			<-start
			clock.Advance(time.Minute)
		}()
		close(start)
		wait.Wait()
		recorder.awaitAny(t)
		select {
		case duplicate := <-recorder.responses:
			t.Fatalf("iteration %d wrote twice: %#v", iteration, duplicate)
		case <-time.After(time.Millisecond):
		}
	}
}

type protocolDecisionRecorder struct {
	responses chan any
	rejects   chan int64
}

func newProtocolDecisionRecorder() *protocolDecisionRecorder {
	return &protocolDecisionRecorder{responses: make(chan any, 8), rejects: make(chan int64, 8)}
}

func (recorder *protocolDecisionRecorder) respond(_ RequestID, response any) error {
	recorder.responses <- response
	return nil
}

func (recorder *protocolDecisionRecorder) reject(_ RequestID, code int64, _ string) error {
	recorder.rejects <- code
	return nil
}

func (recorder *protocolDecisionRecorder) awaitDecision(t *testing.T, want any) {
	t.Helper()
	got := recorder.awaitAny(t)
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalized, want) {
		t.Fatalf("protocol response = %#v, want %#v", normalized, want)
	}
}

func (recorder *protocolDecisionRecorder) awaitAny(t *testing.T) any {
	t.Helper()
	select {
	case response := <-recorder.responses:
		return response
	case <-time.After(time.Second):
		t.Fatal("protocol response was not sent")
		return nil
	}
}

func commandRequestContext(recorder *protocolDecisionRecorder) ServerRequestContext {
	return testServerRequestContext(recorder, "item/commandExecution/requestApproval", map[string]any{
		"itemId": "item-1", "threadId": "thread-1", "turnId": "turn-1", "startedAtMs": 1,
		"command": "go test ./...", "reason": "Run the test suite",
		"availableDecisions": []any{"accept", "acceptForSession", "decline", "cancel"},
	})
}

func fileRequestContext(recorder *protocolDecisionRecorder) ServerRequestContext {
	return testServerRequestContext(recorder, "item/fileChange/requestApproval", map[string]any{
		"itemId": "item-1", "threadId": "thread-1", "turnId": "turn-1", "startedAtMs": 1,
		"reason": "Apply the proposed changes",
	})
}

func permissionRequestContext(recorder *protocolDecisionRecorder) ServerRequestContext {
	return testServerRequestContext(recorder, "item/permissions/requestApproval", map[string]any{
		"itemId": "item-1", "threadId": "thread-1", "turnId": "turn-1", "startedAtMs": 1,
		"cwd": "/workspace", "reason": "Connect to the requested host",
		"permissions": map[string]any{"network": map[string]any{"enabled": true}},
	})
}

func userInputRequestContext(recorder *protocolDecisionRecorder) ServerRequestContext {
	return testServerRequestContext(recorder, "item/tool/requestUserInput", map[string]any{
		"itemId": "item-1", "threadId": "thread-1", "turnId": "turn-1",
		"questions": []any{
			map[string]any{"id": "platform", "header": "Platform", "question": "Choose a platform", "options": []any{
				map[string]any{"label": "Windows", "description": "Use Windows"}, map[string]any{"label": "macOS", "description": "Use macOS"},
			}},
			map[string]any{"id": "detail", "header": "Detail", "question": "Add detail", "isOther": true, "options": []any{map[string]any{"label": "Default", "description": "Use default"}}},
			map[string]any{"id": "token", "header": "Token", "question": "Enter the secret", "isSecret": true},
		},
	})
}

func testServerRequestContext(recorder *protocolDecisionRecorder, method string, params any) ServerRequestContext {
	encoded, err := json.Marshal(params)
	if err != nil {
		panic(err)
	}
	id, err := ParseRequestID(json.RawMessage(`"protocol-1"`))
	if err != nil {
		panic(err)
	}
	return ServerRequestContext{
		Request:   ServerRequest{ID: id, Method: method, Params: encoded},
		SessionID: "thread-1-turn-1", IssueID: "issue-1", IssueIdentifier: "GH-42",
		Respond: recorder.respond, Reject: recorder.reject,
	}
}

type brokerFakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*brokerFakeTimer
}

type brokerFakeTimer struct {
	clock   *brokerFakeClock
	at      time.Time
	channel chan time.Time
	stopped bool
	fired   bool
}

func newBrokerFakeClock(now time.Time) *brokerFakeClock { return &brokerFakeClock{now: now} }

func (clock *brokerFakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *brokerFakeClock) NewTimer(delay time.Duration) BrokerTimer {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	timer := &brokerFakeTimer{clock: clock, at: clock.now.Add(delay), channel: make(chan time.Time, 1)}
	clock.timers = append(clock.timers, timer)
	return timer
}

func (clock *brokerFakeClock) Advance(delay time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(delay)
	now := clock.now
	for _, timer := range clock.timers {
		if !timer.stopped && !timer.fired && !timer.at.After(now) {
			timer.fired = true
			timer.channel <- now
		}
	}
	clock.mu.Unlock()
}

func (timer *brokerFakeTimer) C() <-chan time.Time { return timer.channel }

func (timer *brokerFakeTimer) Stop() bool {
	timer.clock.mu.Lock()
	defer timer.clock.mu.Unlock()
	wasActive := !timer.stopped && !timer.fired
	timer.stopped = true
	return wasActive
}

var _ BrokerClock = (*brokerFakeClock)(nil)
