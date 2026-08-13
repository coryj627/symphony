package codex

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestServerRequestRejectsMalformedAndUnsupportedMethodsFinitely(t *testing.T) {
	for _, test := range []struct {
		name   string
		method string
		params json.RawMessage
		code   int64
		want   error
	}{
		{name: "malformed", method: "item/fileChange/requestApproval", params: json.RawMessage(`{"threadId":"thread-1"}`), code: rpcInvalidParams, want: ErrMalformedServerRequest},
		{name: "trailing json", method: "item/fileChange/requestApproval", params: json.RawMessage(`{"itemId":"item-1","threadId":"thread-1","turnId":"turn-1","startedAtMs":1} {}`), code: rpcInvalidParams, want: ErrMalformedServerRequest},
		{name: "unsupported", method: "account/chatgptAuthTokens/refresh", params: json.RawMessage(`{}`), code: rpcMethodNotFound, want: ErrUnsupportedServerRequest},
		{name: "future", method: "future/request", params: json.RawMessage(`{}`), code: rpcMethodNotFound, want: ErrUnsupportedServerRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := newProtocolDecisionRecorder()
			broker := NewRequestBroker(RequestBrokerOptions{NewID: func() string { return "request-1" }})
			context := testServerRequestContext(recorder, test.method, map[string]any{})
			context.Request.Params = test.params
			if _, err := broker.Open(context); !errors.Is(err, test.want) {
				t.Fatalf("Open() = %v", err)
			}
			select {
			case code := <-recorder.rejects:
				if code != test.code {
					t.Fatalf("reject code = %d", code)
				}
			case <-time.After(time.Second):
				t.Fatal("server request was left unanswered")
			}
			if len(broker.Pending()) != 0 {
				t.Fatal("rejected request became pending")
			}
		})
	}
}

func TestServerRequestRejectsInvalidStructuredDecisionsAndQuestionIDs(t *testing.T) {
	tests := []struct {
		name    string
		context func(*protocolDecisionRecorder) ServerRequestContext
		mutate  func(*ServerRequestContext)
	}{
		{
			name:    "invalid command amendment",
			context: commandRequestContext,
			mutate: func(context *ServerRequestContext) {
				params := map[string]any{
					"itemId": "item-1", "threadId": "thread-1", "turnId": "turn-1", "startedAtMs": 1,
					"availableDecisions": []any{map[string]any{"acceptWithExecpolicyAmendment": map[string]any{"execpolicy_amendment": "not-an-array"}}},
				}
				context.Request.Params, _ = json.Marshal(params)
			},
		},
		{
			name:    "invalid permission profile",
			context: permissionRequestContext,
			mutate: func(context *ServerRequestContext) {
				params := map[string]any{
					"itemId": "item-1", "threadId": "thread-1", "turnId": "turn-1", "startedAtMs": 1, "cwd": "/workspace",
					"permissions": map[string]any{"network": "enabled"},
				}
				context.Request.Params, _ = json.Marshal(params)
			},
		},
		{
			name:    "unsafe question id",
			context: userInputRequestContext,
			mutate: func(context *ServerRequestContext) {
				params := map[string]any{
					"itemId": "item-1", "threadId": "thread-1", "turnId": "turn-1",
					"questions": []any{map[string]any{"id": "unsafe/id", "header": "Header", "question": "Question"}},
				}
				context.Request.Params, _ = json.Marshal(params)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := newProtocolDecisionRecorder()
			context := test.context(recorder)
			test.mutate(&context)
			broker := NewRequestBroker(RequestBrokerOptions{NewID: func() string { return "request-1" }})
			if _, err := broker.Open(context); !errors.Is(err, ErrMalformedServerRequest) {
				t.Fatalf("Open() = %v", err)
			}
			select {
			case code := <-recorder.rejects:
				if code != rpcInvalidParams {
					t.Fatalf("reject code = %d", code)
				}
			case <-time.After(time.Second):
				t.Fatal("malformed request was left unanswered")
			}
		})
	}
}

func TestServerRequestRejectsMismatchedSessionIdentity(t *testing.T) {
	recorder := newProtocolDecisionRecorder()
	broker := NewRequestBroker(RequestBrokerOptions{NewID: func() string { return "request-1" }})
	context := commandRequestContext(recorder)
	context.SessionID = "different-session"
	if _, err := broker.Open(context); !errors.Is(err, ErrMalformedServerRequest) {
		t.Fatalf("Open() = %v", err)
	}
	select {
	case code := <-recorder.rejects:
		if code != rpcInvalidParams {
			t.Fatalf("reject code = %d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("mismatched request was left unanswered")
	}
}
