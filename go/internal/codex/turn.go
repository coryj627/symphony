package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const (
	ProtocolErrorTurnSilence  = "turn_silence_timeout"
	ProtocolErrorTurnFailed   = "turn_failed"
	ProtocolErrorTurnCanceled = "turn_cancelled"
)

// TurnStatus is Symphony's stable terminal turn status.
type TurnStatus string

const (
	TurnCompleted   TurnStatus = "completed"
	TurnFailed      TurnStatus = "failed"
	TurnInterrupted TurnStatus = "interrupted"
)

// TurnResult is the safe terminal result for one app-server turn.
type TurnResult struct {
	SessionID    string
	ThreadID     string
	TurnID       string
	Status       TurnStatus
	ErrorCode    string
	ErrorMessage string
}

type turnOutcome struct {
	result TurnResult
	err    error
}

type turnSignal uint8

const (
	turnSignalActivity turnSignal = iota + 1
	turnSignalPause
	turnSignalResume
)

type activeTurn struct {
	threadID        string
	turnID          string
	earlyCompletion json.RawMessage
	earlyError      error
	done            chan turnOutcome
	signals         chan turnSignal
	once            sync.Once
}

// StartTurn starts one text-input turn on the existing live thread.
func (session *Session) StartTurn(ctx context.Context, text string) (TurnResult, error) {
	if ctx == nil {
		return TurnResult{}, errors.New("turn context is missing")
	}
	if text == "" {
		return TurnResult{}, newProtocolError("empty_turn_input", "Codex turn input is empty.", false, nil)
	}
	session.mu.Lock()
	if session.threadID == "" {
		session.mu.Unlock()
		return TurnResult{}, newProtocolError("thread_not_started", "Codex thread is not started.", false, nil)
	}
	if session.active != nil {
		session.mu.Unlock()
		return TurnResult{}, newProtocolError("turn_already_active", "A Codex turn is already active.", false, nil)
	}
	active := &activeTurn{
		threadID: session.threadID,
		done:     make(chan turnOutcome, 1),
		signals:  make(chan turnSignal, 64),
	}
	session.active = active
	threadID := session.threadID
	session.mu.Unlock()

	params := TurnStartParams{
		ThreadID:              threadID,
		Input:                 []UserInput{{Type: "text", Text: text}},
		Cwd:                   session.options.Workspace,
		RuntimeWorkspaceRoots: []string{session.options.Workspace},
		SandboxPolicy: WorkspaceWriteSandboxPolicy{
			Type: "workspaceWrite", WritableRoots: []string{session.options.Workspace}, NetworkAccess: false,
		},
	}
	callCtx, cancel := session.requestContext(ctx)
	var response TurnStartResponse
	err := session.router.Call(callCtx, "turn/start", params, &response)
	cancel()
	if err != nil {
		session.clearActive(active)
		return TurnResult{}, err
	}
	if response.Turn.ID == "" || !validTurnItems(response.Turn.Items) {
		session.clearActive(active)
		return TurnResult{}, newProtocolError(ProtocolErrorMalformedMessage, "Codex turn/start response is incomplete.", false, nil)
	}
	if response.Turn.Status != "inProgress" {
		session.clearActive(active)
		return TurnResult{}, newProtocolError(ProtocolErrorMalformedMessage, "Codex turn/start response has a non-starting status.", false, nil)
	}
	session.mu.Lock()
	if active.turnID != "" && active.turnID != response.Turn.ID {
		session.mu.Unlock()
		session.clearActive(active)
		return TurnResult{}, newProtocolError(ProtocolErrorMalformedMessage, "Codex turn/start response conflicts with the active turn.", false, nil)
	}
	active.turnID = response.Turn.ID
	earlyCompletion := cloneRaw(active.earlyCompletion)
	earlyError := active.earlyError
	active.earlyCompletion = nil
	active.earlyError = nil
	session.mu.Unlock()
	session.emit(SessionEvent{
		Type: SessionEventTurnStarted, ThreadID: threadID, TurnID: response.Turn.ID,
		Summary: "Codex turn started.",
	})
	if len(earlyCompletion) > 0 {
		session.handleTurnCompleted(earlyCompletion)
	}
	if earlyError != nil {
		session.failActive(earlyError)
	}

	result, err := session.waitTurn(ctx, active)
	session.clearActive(active)
	return result, err
}

// InterruptTurn requests interruption of the active turn.
func (session *Session) InterruptTurn(ctx context.Context) error {
	if ctx == nil {
		return newProtocolError("missing_context", "Codex turn context is missing.", false, nil)
	}
	session.mu.Lock()
	active := session.active
	if active == nil || active.turnID == "" {
		session.mu.Unlock()
		return newProtocolError("turn_not_active", "No Codex turn is available to interrupt.", false, nil)
	}
	params := TurnInterruptParams{ThreadID: active.threadID, TurnID: active.turnID}
	session.mu.Unlock()
	var response map[string]any
	callCtx, cancel := session.requestContext(ctx)
	defer cancel()
	return session.router.Call(callCtx, "turn/interrupt", params, &response)
}

func (session *Session) waitTurn(ctx context.Context, active *activeTurn) (TurnResult, error) {
	remaining := session.options.SilenceTimeout
	startedAt := time.Now()
	timer := time.NewTimer(remaining)
	defer stopTimer(timer)
	paused := 0
	for {
		select {
		case outcome := <-active.done:
			return outcome.result, outcome.err
		case signal := <-active.signals:
			switch signal {
			case turnSignalActivity:
				remaining = session.options.SilenceTimeout
				if paused == 0 {
					startedAt = time.Now()
					resetTimer(timer, remaining)
				}
			case turnSignalPause:
				if paused == 0 {
					elapsed := time.Since(startedAt)
					remaining = max(time.Nanosecond, remaining-elapsed)
					stopTimer(timer)
				}
				paused++
			case turnSignalResume:
				if paused > 0 {
					paused--
					if paused == 0 {
						startedAt = time.Now()
						resetTimer(timer, remaining)
					}
				}
			}
		case <-timer.C:
			interruptCtx, cancel := context.WithTimeout(context.Background(), session.options.RequestTimeout)
			_ = session.InterruptTurn(interruptCtx)
			cancel()
			return TurnResult{}, newProtocolError(
				ProtocolErrorTurnSilence,
				"Codex turn stopped after the app-server produced no protocol activity.",
				true,
				nil,
			)
		case <-ctx.Done():
			interruptCtx, cancel := context.WithTimeout(context.Background(), session.options.RequestTimeout)
			_ = session.InterruptTurn(interruptCtx)
			cancel()
			return TurnResult{}, ctx.Err()
		}
	}
}

func (session *Session) handleTurnCompleted(raw json.RawMessage) {
	var notification TurnCompletedNotification
	if err := json.Unmarshal(raw, &notification); err != nil || notification.ThreadID == "" || notification.Turn.ID == "" || !validTurnItems(notification.Turn.Items) {
		session.failActive(newProtocolError(ProtocolErrorMalformedMessage, "Codex turn completion notification is malformed.", false, err))
		return
	}

	session.mu.Lock()
	active := session.active
	if active == nil {
		duplicate := notification.ThreadID == session.lastThread && notification.Turn.ID == session.lastTurn
		session.mu.Unlock()
		if duplicate {
			session.emit(SessionEvent{
				Type: SessionEventTurnNotificationIgnored, ThreadID: notification.ThreadID, TurnID: notification.Turn.ID,
				Summary: "A duplicate Codex turn completion was ignored.",
			})
		}
		return
	}
	if active.threadID != notification.ThreadID || (active.turnID != "" && active.turnID != notification.Turn.ID) {
		session.mu.Unlock()
		session.emit(SessionEvent{
			Type: SessionEventTurnNotificationIgnored, ThreadID: notification.ThreadID, TurnID: notification.Turn.ID,
			Summary: "A Codex completion for a different turn was ignored.",
		})
		return
	}
	if active.turnID == "" {
		if active.earlyError != nil {
			session.mu.Unlock()
			return
		}
		if len(active.earlyCompletion) == 0 {
			active.earlyCompletion = cloneRaw(raw)
			session.mu.Unlock()
			return
		}
		session.mu.Unlock()
		session.emit(SessionEvent{
			Type: SessionEventTurnNotificationIgnored, ThreadID: notification.ThreadID, TurnID: notification.Turn.ID,
			Summary: "An additional early Codex turn completion was ignored.",
		})
		return
	}
	session.lastThread = notification.ThreadID
	session.lastTurn = notification.Turn.ID
	session.mu.Unlock()

	result := TurnResult{
		SessionID: notification.ThreadID + "-" + notification.Turn.ID,
		ThreadID:  notification.ThreadID,
		TurnID:    notification.Turn.ID,
	}
	switch notification.Turn.Status {
	case "completed":
		result.Status = TurnCompleted
	case "failed":
		result.Status = TurnFailed
		result.ErrorCode = ProtocolErrorTurnFailed
		result.ErrorMessage = "Codex turn failed. Review the local redacted diagnostics."
	case "interrupted":
		result.Status = TurnInterrupted
		result.ErrorCode = ProtocolErrorTurnCanceled
		result.ErrorMessage = "Codex turn was interrupted."
	default:
		active.once.Do(func() {
			active.done <- turnOutcome{err: newProtocolError(
				ProtocolErrorMalformedMessage,
				"Codex turn completion contains a nonterminal status.",
				false,
				nil,
			)}
		})
		return
	}
	completed := false
	active.once.Do(func() {
		completed = true
		active.done <- turnOutcome{result: result}
	})
	if !completed {
		session.emit(SessionEvent{
			Type: SessionEventTurnNotificationIgnored, ThreadID: result.ThreadID, TurnID: result.TurnID,
			Summary: "A duplicate Codex turn completion was ignored.",
		})
		return
	}
	session.emit(SessionEvent{
		Type: SessionEventTurnCompleted, ThreadID: result.ThreadID, TurnID: result.TurnID, SessionID: result.SessionID,
		Summary: "Codex turn reached a terminal state.",
	})
}

func validTurnItems(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '[' && json.Valid(trimmed)
}

func (session *Session) signalActive(signal turnSignal) {
	session.mu.Lock()
	active := session.active
	session.mu.Unlock()
	if active == nil {
		return
	}
	select {
	case active.signals <- signal:
	default:
		if signal != turnSignalActivity {
			active.once.Do(func() {
				active.done <- turnOutcome{err: newProtocolError(ProtocolErrorBackpressure, "Codex turn control delivery exceeded its bounded queue.", false, nil)}
			})
		}
	}
}

func (session *Session) failActive(err error) {
	if err == nil {
		err = newProtocolError(ProtocolErrorTransportClosed, "Codex app-server output closed.", true, nil)
	}
	session.mu.Lock()
	active := session.active
	if active != nil && active.turnID == "" {
		if len(active.earlyCompletion) == 0 && active.earlyError == nil {
			active.earlyError = err
		}
		session.mu.Unlock()
		return
	}
	session.mu.Unlock()
	if active != nil {
		active.once.Do(func() { active.done <- turnOutcome{err: err} })
	}
}

func (session *Session) clearActive(active *activeTurn) {
	session.mu.Lock()
	if session.active == active {
		session.active = nil
	}
	session.mu.Unlock()
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	stopTimer(timer)
	timer.Reset(duration)
}
