package codex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
)

const (
	defaultOperatorWindow      = 10 * time.Minute
	defaultOperatorWarningLead = 20 * time.Second
	maximumOperatorExtensions  = 10
)

var (
	ErrUnknownRequest = errors.New("operator_request_unknown")
	ErrStaleRequest   = errors.New("operator_request_stale")
	ErrExtensionLimit = errors.New("operator_request_extension_limit")
)

type BrokerTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type BrokerClock interface {
	Now() time.Time
	NewTimer(time.Duration) BrokerTimer
}

type realBrokerClock struct{}

func (realBrokerClock) Now() time.Time { return time.Now() }
func (realBrokerClock) NewTimer(delay time.Duration) BrokerTimer {
	return realBrokerTimer{timer: time.NewTimer(delay)}
}

type realBrokerTimer struct{ timer *time.Timer }

func (timer realBrokerTimer) C() <-chan time.Time { return timer.timer.C }
func (timer realBrokerTimer) Stop() bool          { return timer.timer.Stop() }

type RequestBrokerOptions struct {
	Clock       BrokerClock
	Window      time.Duration
	WarningLead time.Duration
	Redactor    *observability.Redactor
	OnChange    func()
	OnWarning   func(domain.OperatorRequest)
	FailSession func(sessionID, code string)
	NewID       func() string
}

type RequestBroker struct {
	mu      sync.Mutex
	options RequestBrokerOptions
	pending map[string]*pendingOperatorRequest
}

type pendingOperatorRequest struct {
	request         domain.OperatorRequest
	protocolID      RequestID
	respond         protocolResponder
	reject          protocolRejecter
	encodeResponse  func(domain.OperatorResponse) (any, [][]byte, error)
	cancelResponse  any
	warningTimer    BrokerTimer
	expirationTimer BrokerTimer
	generation      uint64
	windowCancel    chan struct{}
}

func NewRequestBroker(options RequestBrokerOptions) *RequestBroker {
	if options.Clock == nil {
		options.Clock = realBrokerClock{}
	}
	if options.Window <= 0 {
		options.Window = defaultOperatorWindow
	}
	if options.WarningLead <= 0 || options.WarningLead >= options.Window {
		options.WarningLead = min(defaultOperatorWarningLead, options.Window/2)
	}
	if options.Redactor == nil {
		options.Redactor = observability.NewRedactor(nil, nil)
	}
	if options.NewID == nil {
		options.NewID = randomOperatorRequestID
	}
	return &RequestBroker{options: options, pending: make(map[string]*pendingOperatorRequest)}
}

func (broker *RequestBroker) Open(context ServerRequestContext) (domain.OperatorRequest, error) {
	mapped, err := mapServerRequest(context)
	if err != nil {
		code := int64(rpcInvalidParams)
		message := "The Codex request parameters are invalid."
		if errors.Is(err, ErrUnsupportedServerRequest) {
			code = rpcMethodNotFound
			message = "The Codex request method is not supported."
		}
		if context.Reject != nil && context.Request.ID.Token() != "" {
			_ = context.Reject(context.Request.ID, code, message)
		}
		return domain.OperatorRequest{}, err
	}

	broker.mu.Lock()
	id := broker.options.NewID()
	if !validOperatorToken(id) {
		broker.mu.Unlock()
		_ = context.Reject(context.Request.ID, rpcInvalidParams, "Symphony could not allocate the operator request.")
		return domain.OperatorRequest{}, ErrMalformedServerRequest
	}
	if _, duplicate := broker.pending[id]; duplicate {
		broker.mu.Unlock()
		_ = context.Reject(context.Request.ID, rpcInvalidParams, "Symphony could not allocate the operator request.")
		return domain.OperatorRequest{}, ErrMalformedServerRequest
	}
	now := broker.options.Clock.Now().UTC()
	mapped.request.ID = id
	mapped.request.OpenedAt = now
	mapped.request.ExtensionsRemaining = maximumOperatorExtensions
	pending := &pendingOperatorRequest{
		request: mapped.request, protocolID: context.Request.ID, respond: context.Respond, reject: context.Reject,
		encodeResponse: mapped.response, cancelResponse: mapped.cancelResponse,
	}
	broker.pending[id] = pending
	broker.startWindowLocked(id, pending, now)
	request := pending.request.Clone()
	broker.mu.Unlock()
	broker.changed()
	return request, nil
}

func (broker *RequestBroker) Respond(response domain.OperatorResponse) error {
	response = response.Clone()
	broker.mu.Lock()
	pending, exists := broker.pending[response.RequestID]
	if !exists {
		broker.mu.Unlock()
		return ErrStaleRequest
	}
	if response.SessionID != pending.request.SessionID {
		broker.mu.Unlock()
		return ErrStaleRequest
	}
	protocolResponse, secrets, err := pending.encodeResponse(response)
	if err != nil {
		broker.mu.Unlock()
		return err
	}
	broker.removeLocked(response.RequestID, pending)
	broker.mu.Unlock()
	for _, secret := range secrets {
		broker.options.Redactor.RegisterSecret(secret)
	}
	err = pending.respond(pending.protocolID, protocolResponse)
	broker.changed()
	return err
}

func (broker *RequestBroker) Extend(id string) error {
	broker.mu.Lock()
	pending, exists := broker.pending[id]
	if !exists {
		broker.mu.Unlock()
		return ErrStaleRequest
	}
	if pending.request.ExtensionsUsed >= maximumOperatorExtensions {
		broker.mu.Unlock()
		return ErrExtensionLimit
	}
	pending.request.ExtensionsUsed++
	pending.request.ExtensionsRemaining = maximumOperatorExtensions - pending.request.ExtensionsUsed
	broker.stopTimersLocked(pending)
	broker.startWindowLocked(id, pending, broker.options.Clock.Now().UTC())
	broker.mu.Unlock()
	broker.changed()
	return nil
}

func (broker *RequestBroker) CancelSession(sessionID string) {
	type cancellation struct {
		pending *pendingOperatorRequest
	}
	cancellations := make([]cancellation, 0)
	broker.mu.Lock()
	for id, pending := range broker.pending {
		if pending.request.SessionID != sessionID {
			continue
		}
		broker.removeLocked(id, pending)
		cancellations = append(cancellations, cancellation{pending: pending})
	}
	broker.mu.Unlock()
	for _, cancellation := range cancellations {
		_ = cancellation.pending.respond(cancellation.pending.protocolID, cancellation.pending.cancelResponse)
	}
	if len(cancellations) > 0 {
		broker.changed()
	}
}

func (broker *RequestBroker) Pending() []domain.OperatorRequest {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	requests := make([]domain.OperatorRequest, 0, len(broker.pending))
	for _, pending := range broker.pending {
		requests = append(requests, pending.request.Clone())
	}
	sort.Slice(requests, func(left, right int) bool {
		if requests[left].OpenedAt.Equal(requests[right].OpenedAt) {
			return requests[left].ID < requests[right].ID
		}
		return requests[left].OpenedAt.Before(requests[right].OpenedAt)
	})
	return requests
}

func (broker *RequestBroker) startWindowLocked(id string, pending *pendingOperatorRequest, now time.Time) {
	pending.generation++
	generation := pending.generation
	pending.request.WarningAt = now.Add(broker.options.Window - broker.options.WarningLead)
	pending.request.DeadlineAt = now.Add(broker.options.Window)
	pending.warningTimer = broker.options.Clock.NewTimer(broker.options.Window - broker.options.WarningLead)
	pending.expirationTimer = broker.options.Clock.NewTimer(broker.options.Window)
	pending.windowCancel = make(chan struct{})
	go broker.awaitWarning(id, generation, pending.warningTimer, pending.windowCancel)
	go broker.awaitExpiration(id, generation, pending.expirationTimer, pending.windowCancel)
}

func (broker *RequestBroker) awaitWarning(id string, generation uint64, timer BrokerTimer, cancel <-chan struct{}) {
	select {
	case <-timer.C():
	case <-cancel:
		return
	}
	broker.mu.Lock()
	pending, exists := broker.pending[id]
	if !exists || pending.generation != generation || pending.warningTimer != timer {
		broker.mu.Unlock()
		return
	}
	request := pending.request.Clone()
	broker.mu.Unlock()
	if broker.options.OnWarning != nil {
		broker.options.OnWarning(request)
	}
	broker.changed()
}

func (broker *RequestBroker) awaitExpiration(id string, generation uint64, timer BrokerTimer, cancel <-chan struct{}) {
	select {
	case <-timer.C():
	case <-cancel:
		return
	}
	broker.mu.Lock()
	pending, exists := broker.pending[id]
	if !exists || pending.generation != generation || pending.expirationTimer != timer {
		broker.mu.Unlock()
		return
	}
	broker.removeLocked(id, pending)
	broker.mu.Unlock()
	_ = pending.respond(pending.protocolID, pending.cancelResponse)
	if broker.options.FailSession != nil {
		broker.options.FailSession(pending.request.SessionID, "operator_request_timeout")
	}
	broker.changed()
}

func (broker *RequestBroker) removeLocked(id string, pending *pendingOperatorRequest) {
	if current, exists := broker.pending[id]; !exists || current != pending {
		return
	}
	delete(broker.pending, id)
	broker.stopTimersLocked(pending)
}

func (*RequestBroker) stopTimersLocked(pending *pendingOperatorRequest) {
	pending.generation++
	if pending.windowCancel != nil {
		close(pending.windowCancel)
		pending.windowCancel = nil
	}
	if pending.warningTimer != nil {
		pending.warningTimer.Stop()
	}
	if pending.expirationTimer != nil {
		pending.expirationTimer.Stop()
	}
}

func (broker *RequestBroker) changed() {
	if broker.options.OnChange != nil {
		broker.options.OnChange()
	}
}

func randomOperatorRequestID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return ""
	}
	return "request-" + hex.EncodeToString(buffer)
}

// Run consumes the session's bounded server-request stream until either side
// closes. Unsupported requests are rejected by Open and never block the turn.
func (broker *RequestBroker) Run(ctx context.Context, session *Session, requestContext func(ServerRequest) ServerRequestContext) error {
	if ctx == nil || session == nil || requestContext == nil {
		return fmt.Errorf("%w: request broker run configuration is incomplete", ErrMalformedServerRequest)
	}
	for {
		select {
		case request := <-session.ServerRequests():
			context := requestContext(request)
			context.Request = request
			if _, err := broker.Open(context); err != nil {
				if errors.Is(err, ErrMalformedServerRequest) || errors.Is(err, ErrUnsupportedServerRequest) {
					continue
				}
				return err
			}
		case <-session.router.Done():
			return session.router.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
