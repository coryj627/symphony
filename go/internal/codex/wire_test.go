package codex

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWirePreservesNumericAndStringRequestIDsWithoutFloatConversion(t *testing.T) {
	for _, test := range []struct {
		input string
		token string
	}{
		{input: `9223372036854775807`, token: `9223372036854775807`},
		{input: `-0`, token: `0`},
		{input: `"large-9223372036854775808"`, token: `"large-9223372036854775808"`},
		{input: `"escaped\\nvalue"`, token: `"escaped\\nvalue"`},
	} {
		t.Run(test.input, func(t *testing.T) {
			id, err := ParseRequestID(json.RawMessage(test.input))
			if err != nil {
				t.Fatal(err)
			}
			if id.Token() != test.token {
				t.Fatalf("got %q want %q", id.Token(), test.token)
			}
			encoded, err := json.Marshal(id)
			if err != nil || string(encoded) != test.token {
				t.Fatalf("encoded=%q err=%v", encoded, err)
			}
		})
	}
}

func TestWireRejectsInvalidRequestIDs(t *testing.T) {
	for _, input := range []string{`null`, `{}`, `[]`, `1.5`, `1e3`, `9223372036854775808`} {
		t.Run(input, func(t *testing.T) {
			if _, err := ParseRequestID(json.RawMessage(input)); err == nil {
				t.Fatal("expected invalid request ID")
			}
		})
	}
}

func TestWireClassifiesResponseRequestAndNotification(t *testing.T) {
	response := mustDecodeEnvelope(t, `{"id":1,"result":{"ok":true}}`)
	if response.Kind != EnvelopeResponse || response.Response.ID.Token() != "1" {
		t.Fatalf("%+v", response)
	}

	request := mustDecodeEnvelope(t, `{"id":"approval-1","method":"item/tool/requestUserInput","params":{"x":1}}`)
	if request.Kind != EnvelopeServerRequest || request.ServerRequest.Method != "item/tool/requestUserInput" {
		t.Fatalf("%+v", request)
	}

	notification := mustDecodeEnvelope(t, `{"method":"future/notification","params":{"x":1}}`)
	if notification.Kind != EnvelopeNotification || notification.Notification.Method != "future/notification" {
		t.Fatalf("%+v", notification)
	}
}

func TestWireDecodesRPCErrorWithoutUsingItsMessageAsSafeSummary(t *testing.T) {
	envelope := mustDecodeEnvelope(t, `{"id":7,"error":{"code":-32000,"message":"secret-canary","data":{"token":"secret-canary"}}}`)
	if envelope.Response.Error == nil || envelope.Response.Error.Code != -32000 || envelope.Response.Error.Message != "secret-canary" {
		t.Fatalf("%+v", envelope.Response)
	}
	if strings.Contains(envelope.Response.Error.Error(), "secret-canary") {
		t.Fatalf("error string exposed remote payload: %q", envelope.Response.Error.Error())
	}
}

func TestWireRejectsAmbiguousMalformedAndUnknownMessages(t *testing.T) {
	for _, input := range []string{
		`{"id":1,"result":{},"error":{"code":1,"message":"bad"}}`,
		`{"id":1}`,
		`{"unknown":true}`,
		`[]`,
		`{"method":`,
		`{"id":null,"result":{}}`,
		`{"id":1,"id":2,"result":{}}`,
		`{"id":1,"error":{"code":1,"code":2,"message":"bad"}}`,
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := DecodeEnvelope([]byte(input)); err == nil {
				t.Fatal("expected malformed envelope")
			}
		})
	}
}

func TestWireFixturesMatchExpectedEnvelopeKinds(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "codex", "wire")
	for name, expected := range map[string][]EnvelopeKind{
		"initialize-response.jsonl": {EnvelopeResponse},
		"server-requests.jsonl":     {EnvelopeServerRequest, EnvelopeServerRequest},
		"turn-notifications.jsonl":  {EnvelopeNotification, EnvelopeNotification, EnvelopeNotification},
	} {
		t.Run(name, func(t *testing.T) {
			file, err := os.Open(filepath.Join(root, name))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			scanner := bufio.NewScanner(file)
			var got []EnvelopeKind
			for scanner.Scan() {
				got = append(got, mustDecodeEnvelope(t, scanner.Text()).Kind)
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(expected) {
				t.Fatalf("got %v want %v", got, expected)
			}
			for index := range got {
				if got[index] != expected[index] {
					t.Fatalf("got %v want %v", got, expected)
				}
			}
		})
	}
}

func mustDecodeEnvelope(t *testing.T, source string) Envelope {
	t.Helper()
	envelope, err := DecodeEnvelope([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}
