package codex

import (
	"errors"
	"strings"
)

const (
	ProtocolErrorMalformedMessage = "malformed_message"
	ProtocolErrorMessageTooLarge  = "message_too_large"
	ProtocolErrorTransportClosed  = "transport_closed"
	ProtocolErrorReadFailed       = "read_failed"
	ProtocolErrorWriteFailed      = "write_failed"
	ProtocolErrorRequestTimeout   = "request_timeout"
	ProtocolErrorBackpressure     = "protocol_backpressure"
	ProtocolErrorRouterClosed     = "router_closed"
	ProtocolErrorRequestID        = "request_id_exhausted"
)

var (
	ErrMessageTooLarge    = errors.New("Codex protocol message exceeds the configured limit")
	ErrDuplicateRequestID = errors.New("Codex protocol request ID already has an owner")
)

// ProtocolError is a bounded, display-safe protocol failure.
type ProtocolError struct {
	Code      string
	Summary   string
	Retryable bool
	cause     error
}

func (err *ProtocolError) Error() string {
	if err == nil {
		return ""
	}
	if err.Summary != "" {
		return err.Summary
	}
	return err.Code
}

func (err *ProtocolError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

func newProtocolError(code, summary string, retryable bool, cause error) *ProtocolError {
	summary = strings.TrimSpace(summary)
	if len(summary) > 512 {
		summary = summary[:512]
	}
	return &ProtocolError{Code: code, Summary: summary, Retryable: retryable, cause: cause}
}
