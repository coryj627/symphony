package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ProtocolEventActivity      = "activity"
	ProtocolEventResponse      = "response"
	ProtocolEventLateResponse  = "late_response"
	ProtocolEventServerRequest = "server_request"
	ProtocolEventNotification  = "notification"
)

const (
	defaultMaxLineBytes       = 10 << 20
	defaultRequestBuffer      = 32
	defaultNotificationBuffer = 128
	defaultEventBuffer        = 512
)

type readWriteCloser interface {
	io.Reader
	io.Writer
	io.Closer
}

// RouterOptions bounds calls and protocol delivery queues.
type RouterOptions struct {
	ReadTimeout        time.Duration
	MaxLineBytes       int
	RequestBuffer      int
	NotificationBuffer int
	EventBuffer        int
}

// ProtocolEvent is a bounded activity or routing summary with no payload body.
type ProtocolEvent struct {
	Code      string
	RequestID string
	Method    string
	Summary   string
	Retryable bool
}

// Router owns one reader, one serialized writer, and all pending request IDs.
type Router struct {
	transport readWriteCloser
	options   RouterOptions
	pending   *pendingRegistry

	writeMu sync.Mutex
	nextID  atomic.Int64

	serverRequests chan ServerRequest
	notifications  chan Notification
	events         chan ProtocolEvent
	done           chan struct{}

	stopOnce sync.Once
	errMu    sync.RWMutex
	err      error
}

// NewRouter starts a bounded JSONL reader for one app-server transport.
func NewRouter(transport readWriteCloser, options RouterOptions) *Router {
	options = normalizeRouterOptions(options)
	router := &Router{
		transport:      transport,
		options:        options,
		pending:        newPendingRegistry(),
		serverRequests: make(chan ServerRequest, options.RequestBuffer),
		notifications:  make(chan Notification, options.NotificationBuffer),
		events:         make(chan ProtocolEvent, options.EventBuffer),
		done:           make(chan struct{}),
	}
	go router.readLoop()
	return router
}

// Call sends one request and waits for its uniquely owned response.
func (router *Router) Call(ctx context.Context, method string, params any, result any) error {
	if ctx == nil {
		return newProtocolError(ProtocolErrorWriteFailed, "Codex request context is missing.", false, nil)
	}
	if router.options.ReadTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, router.options.ReadTimeout)
		defer cancel()
	}
	if method == "" || len(method) > maxMethodBytes {
		return newProtocolError(ProtocolErrorWriteFailed, "Codex request method is invalid.", false, nil)
	}

	next := router.nextID.Add(1)
	if next <= 0 {
		return newProtocolError(ProtocolErrorRequestID, "Codex request IDs are exhausted.", false, nil)
	}
	id := numericRequestID(next)
	call := newPendingCall()
	if err := router.pending.register(id, call); err != nil {
		return err
	}
	request := outboundMessage{ID: &id, Method: method, Params: params}
	if err := router.writeJSON(request); err != nil {
		router.pending.remove(id, call)
		return err
	}

	select {
	case outcome := <-call.done:
		return decodeCallOutcome(outcome, result)
	case <-ctx.Done():
		if router.pending.remove(id, call) {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return newProtocolError(
					ProtocolErrorRequestTimeout,
					"Codex app-server did not answer the request before its deadline.",
					true,
					context.DeadlineExceeded,
				)
			}
			return ctx.Err()
		}
		outcome := <-call.done
		return decodeCallOutcome(outcome, result)
	}
}

// Notify sends one notification without allocating a request ID.
func (router *Router) Notify(method string, params any) error {
	if method == "" || len(method) > maxMethodBytes {
		return newProtocolError(ProtocolErrorWriteFailed, "Codex notification method is invalid.", false, nil)
	}
	return router.writeJSON(outboundMessage{Method: method, Params: params})
}

// ServerRequests returns bounded app-server-owned requests.
func (router *Router) ServerRequests() <-chan ServerRequest { return router.serverRequests }

// Notifications returns bounded app-server notifications, including unknown methods.
func (router *Router) Notifications() <-chan Notification { return router.notifications }

// Events returns bounded payload-free protocol activity summaries.
func (router *Router) Events() <-chan ProtocolEvent { return router.events }

// Done closes when the router reaches a terminal state.
func (router *Router) Done() <-chan struct{} { return router.done }

// Err returns the router's terminal error, or nil while it is running.
func (router *Router) Err() error {
	router.errMu.RLock()
	defer router.errMu.RUnlock()
	return router.err
}

// Close terminates the router and resolves all pending calls.
func (router *Router) Close() error {
	router.shutdown(newProtocolError(ProtocolErrorRouterClosed, "Codex protocol router closed.", false, nil))
	return nil
}

type outboundMessage struct {
	ID     *RequestID `json:"id,omitempty"`
	Method string     `json:"method"`
	Params any        `json:"params,omitempty"`
}

func (router *Router) writeJSON(message outboundMessage) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return newProtocolError(ProtocolErrorWriteFailed, "Codex protocol request could not be encoded.", false, err)
	}
	encoded = append(encoded, '\n')
	router.writeMu.Lock()
	defer router.writeMu.Unlock()
	select {
	case <-router.done:
		return router.Err()
	default:
	}
	written, err := router.transport.Write(encoded)
	if err == nil && written != len(encoded) {
		err = io.ErrShortWrite
	}
	if err != nil {
		protocolError := newProtocolError(ProtocolErrorWriteFailed, "Codex app-server input could not be written.", true, err)
		router.shutdown(protocolError)
		return protocolError
	}
	return nil
}

func (router *Router) readLoop() {
	reader := bufio.NewReader(router.transport)
	for {
		line, readErr := ReadLine(reader, router.options.MaxLineBytes)
		if len(line) > 0 || readErr == nil {
			if !router.emit(ProtocolEvent{Code: ProtocolEventActivity, Summary: "Codex protocol activity received."}) {
				router.shutdown(backpressureError())
				return
			}
			envelope, err := DecodeEnvelope(line)
			if err != nil {
				router.shutdown(err)
				return
			}
			if err := router.route(envelope); err != nil {
				router.shutdown(err)
				return
			}
		}
		if readErr != nil {
			switch {
			case errors.Is(readErr, ErrMessageTooLarge):
				router.shutdown(newProtocolError(ProtocolErrorMessageTooLarge, "Codex app-server sent a protocol message larger than 10 MiB.", false, ErrMessageTooLarge))
			case errors.Is(readErr, io.EOF):
				router.shutdown(newProtocolError(ProtocolErrorTransportClosed, "Codex app-server output closed.", true, io.EOF))
			default:
				router.shutdown(newProtocolError(ProtocolErrorReadFailed, "Codex app-server output could not be read.", true, readErr))
			}
			return
		}
	}
}

func (router *Router) route(envelope Envelope) error {
	switch envelope.Kind {
	case EnvelopeResponse:
		response := *envelope.Response
		if !router.pending.complete(response.ID, response) {
			if !router.emit(ProtocolEvent{
				Code: ProtocolEventLateResponse, RequestID: boundedToken(response.ID.Token()),
				Summary: "A late or unknown Codex response was ignored.",
			}) {
				return backpressureError()
			}
			return nil
		}
		if !router.emit(ProtocolEvent{
			Code: ProtocolEventResponse, RequestID: boundedToken(response.ID.Token()),
			Summary: "A Codex response completed its request.",
		}) {
			return backpressureError()
		}
	case EnvelopeServerRequest:
		request := *envelope.ServerRequest
		select {
		case router.serverRequests <- request:
		default:
			return backpressureError()
		}
		if !router.emit(ProtocolEvent{
			Code: ProtocolEventServerRequest, RequestID: boundedToken(request.ID.Token()), Method: request.Method,
			Summary: "Codex requested a Symphony-owned operation.",
		}) {
			return backpressureError()
		}
	case EnvelopeNotification:
		notification := *envelope.Notification
		select {
		case router.notifications <- notification:
		default:
			return backpressureError()
		}
		if !router.emit(ProtocolEvent{
			Code: ProtocolEventNotification, Method: notification.Method,
			Summary: "A Codex notification was received.",
		}) {
			return backpressureError()
		}
	default:
		return malformedEnvelopeError()
	}
	return nil
}

func (router *Router) emit(event ProtocolEvent) bool {
	select {
	case router.events <- event:
		return true
	default:
		return false
	}
}

func (router *Router) shutdown(err error) {
	router.stopOnce.Do(func() {
		router.errMu.Lock()
		router.err = err
		router.errMu.Unlock()
		close(router.done)
		router.pending.close(err)
		_ = router.transport.Close()
	})
}

func normalizeRouterOptions(options RouterOptions) RouterOptions {
	if options.MaxLineBytes <= 0 {
		options.MaxLineBytes = defaultMaxLineBytes
	}
	if options.RequestBuffer <= 0 {
		options.RequestBuffer = defaultRequestBuffer
	}
	if options.NotificationBuffer <= 0 {
		options.NotificationBuffer = defaultNotificationBuffer
	}
	if options.EventBuffer <= 0 {
		options.EventBuffer = defaultEventBuffer
	}
	return options
}

func decodeCallOutcome(outcome callOutcome, result any) error {
	if outcome.err != nil {
		return outcome.err
	}
	if outcome.response.Error != nil {
		return outcome.response.Error
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(outcome.response.Result, result); err != nil {
		return newProtocolError(ProtocolErrorMalformedMessage, "Codex response result could not be decoded.", false, err)
	}
	return nil
}

func backpressureError() *ProtocolError {
	return newProtocolError(ProtocolErrorBackpressure, "Codex protocol delivery exceeded its bounded queue.", false, nil)
}

func boundedToken(value string) string {
	if len(value) > 128 {
		return value[:128]
	}
	return value
}
