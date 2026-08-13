package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

const maxMethodBytes = 256

var integerRequestIDPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)$`)

// RequestID is a comparable, canonical JSON string or signed 64-bit integer token.
type RequestID struct {
	token string
}

// ParseRequestID validates and canonicalizes an app-server request ID.
func ParseRequestID(raw json.RawMessage) (RequestID, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return RequestID{}, errors.New("request ID is empty")
	}
	if trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return RequestID{}, errors.New("request ID string is invalid")
		}
		canonical, err := json.Marshal(value)
		if err != nil {
			return RequestID{}, errors.New("request ID string cannot be encoded")
		}
		return RequestID{token: string(canonical)}, nil
	}
	if !integerRequestIDPattern.Match(trimmed) {
		return RequestID{}, errors.New("request ID is not a string or signed 64-bit integer")
	}
	value, err := strconv.ParseInt(string(trimmed), 10, 64)
	if err != nil {
		return RequestID{}, errors.New("request ID integer is outside the signed 64-bit range")
	}
	return numericRequestID(value), nil
}

func numericRequestID(value int64) RequestID {
	return RequestID{token: strconv.FormatInt(value, 10)}
}

// Token returns the canonical JSON token used as the correlation key.
func (id RequestID) Token() string { return id.token }

// MarshalJSON writes the canonical request ID token without float conversion.
func (id RequestID) MarshalJSON() ([]byte, error) {
	if id.token == "" {
		return nil, errors.New("request ID is unset")
	}
	return []byte(id.token), nil
}

// UnmarshalJSON validates and canonicalizes a request ID.
func (id *RequestID) UnmarshalJSON(raw []byte) error {
	parsed, err := ParseRequestID(raw)
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// EnvelopeKind identifies the safe structural classification of one JSONL message.
type EnvelopeKind string

const (
	EnvelopeResponse      EnvelopeKind = "response"
	EnvelopeServerRequest EnvelopeKind = "server_request"
	EnvelopeNotification  EnvelopeKind = "notification"
)

// Envelope is one classified app-server JSONL message.
type Envelope struct {
	Kind          EnvelopeKind
	Response      *Response
	ServerRequest *ServerRequest
	Notification  *Notification
}

// Response is an app-server response with exactly one result or error.
type Response struct {
	ID     RequestID
	Result json.RawMessage
	Error  *RPCError
}

// RPCError is the internal app-server error payload. Error returns a safe
// summary and deliberately omits Message and Data.
type RPCError struct {
	Code    int64
	Message string
	Data    json.RawMessage
}

func (err *RPCError) Error() string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("Codex app-server returned RPC error %d.", err.Code)
}

// ServerRequest is a request owned by the app-server and answered by Symphony.
type ServerRequest struct {
	ID     RequestID
	Method string
	Params json.RawMessage
}

// Notification is an app-server notification, including unknown future methods.
type Notification struct {
	Method string
	Params json.RawMessage
}

// DecodeEnvelope classifies a single complete JSON object without requiring jsonrpc.
func DecodeEnvelope(source []byte) (Envelope, error) {
	fields, err := decodeJSONObject(source)
	if err != nil {
		return Envelope{}, malformedEnvelopeError()
	}

	idRaw, hasID := fields["id"]
	resultRaw, hasResult := fields["result"]
	errorRaw, hasError := fields["error"]
	methodRaw, hasMethod := fields["method"]
	if hasID && (hasResult || hasError) {
		if hasResult == hasError {
			return Envelope{}, malformedEnvelopeError()
		}
		id, err := ParseRequestID(idRaw)
		if err != nil {
			return Envelope{}, malformedEnvelopeError()
		}
		response := &Response{ID: id}
		if hasResult {
			response.Result = cloneRaw(resultRaw)
		} else {
			response.Error, err = decodeRPCError(errorRaw)
			if err != nil {
				return Envelope{}, malformedEnvelopeError()
			}
		}
		return Envelope{Kind: EnvelopeResponse, Response: response}, nil
	}

	if hasID && hasMethod {
		id, err := ParseRequestID(idRaw)
		if err != nil {
			return Envelope{}, malformedEnvelopeError()
		}
		method, err := decodeMethod(methodRaw)
		if err != nil {
			return Envelope{}, malformedEnvelopeError()
		}
		return Envelope{
			Kind: EnvelopeServerRequest,
			ServerRequest: &ServerRequest{
				ID: id, Method: method, Params: cloneRaw(fields["params"]),
			},
		}, nil
	}

	if hasMethod && !hasID {
		method, err := decodeMethod(methodRaw)
		if err != nil {
			return Envelope{}, malformedEnvelopeError()
		}
		return Envelope{
			Kind:         EnvelopeNotification,
			Notification: &Notification{Method: method, Params: cloneRaw(fields["params"])},
		}, nil
	}
	return Envelope{}, malformedEnvelopeError()
}

func decodeMethod(raw json.RawMessage) (string, error) {
	var method string
	if err := json.Unmarshal(raw, &method); err != nil || method == "" || len(method) > maxMethodBytes {
		return "", errors.New("method is invalid")
	}
	return method, nil
}

func decodeRPCError(raw json.RawMessage) (*RPCError, error) {
	fields, err := decodeJSONObject(raw)
	if err != nil {
		return nil, errors.New("RPC error is invalid")
	}
	codeRaw, hasCode := fields["code"]
	messageRaw, hasMessage := fields["message"]
	if !hasCode || !hasMessage || !integerRequestIDPattern.Match(bytes.TrimSpace(codeRaw)) {
		return nil, errors.New("RPC error is incomplete")
	}
	code, err := strconv.ParseInt(strings.TrimSpace(string(codeRaw)), 10, 64)
	if err != nil {
		return nil, errors.New("RPC error code is invalid")
	}
	var message string
	if err := json.Unmarshal(messageRaw, &message); err != nil {
		return nil, errors.New("RPC error message is invalid")
	}
	return &RPCError{Code: code, Message: message, Data: cloneRaw(fields["data"])}, nil
}

func decodeJSONObject(source []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(source))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("JSON value is not an object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("JSON object key is invalid")
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, errors.New("JSON object contains a duplicate key")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, errors.New("JSON object is incomplete")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("JSON object has trailing content")
	}
	return fields, nil
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return bytes.Clone(raw)
}

func malformedEnvelopeError() *ProtocolError {
	return newProtocolError(
		ProtocolErrorMalformedMessage,
		"Codex app-server sent a malformed protocol message.",
		false,
		nil,
	)
}
