package codex

import "sync"

type callOutcome struct {
	response Response
	err      error
}

type pendingCall struct {
	done chan callOutcome
}

func newPendingCall() *pendingCall {
	return &pendingCall{done: make(chan callOutcome, 1)}
}

type pendingRegistry struct {
	mu       sync.Mutex
	byID     map[RequestID]*pendingCall
	closedBy error
}

func newPendingRegistry() *pendingRegistry {
	return &pendingRegistry{byID: make(map[RequestID]*pendingCall)}
}

func (registry *pendingRegistry) register(id RequestID, call *pendingCall) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.closedBy != nil {
		return registry.closedBy
	}
	if _, exists := registry.byID[id]; exists {
		return ErrDuplicateRequestID
	}
	registry.byID[id] = call
	return nil
}

func (registry *pendingRegistry) complete(id RequestID, response Response) bool {
	registry.mu.Lock()
	call, exists := registry.byID[id]
	if exists {
		delete(registry.byID, id)
	}
	registry.mu.Unlock()
	if !exists {
		return false
	}
	call.done <- callOutcome{response: response}
	return true
}

func (registry *pendingRegistry) remove(id RequestID, owner *pendingCall) bool {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	call, exists := registry.byID[id]
	if !exists || call != owner {
		return false
	}
	delete(registry.byID, id)
	return true
}

func (registry *pendingRegistry) close(err error) {
	registry.mu.Lock()
	if registry.closedBy != nil {
		registry.mu.Unlock()
		return
	}
	registry.closedBy = err
	calls := make([]*pendingCall, 0, len(registry.byID))
	for _, call := range registry.byID {
		calls = append(calls, call)
	}
	clear(registry.byID)
	registry.mu.Unlock()
	for _, call := range calls {
		call.done <- callOutcome{err: err}
	}
}

func (registry *pendingRegistry) len() int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return len(registry.byID)
}
