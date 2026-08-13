package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/orchestrator"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

// AgentSession owns one live app-server process, initialized thread, and all
// continuation turns for a single scheduler attempt.
type AgentSession interface {
	Turn(context.Context, string) (TurnResult, error)
	Close() error
}

// AgentRunner starts one fully initialized app-server session.
type AgentRunner interface {
	Start(context.Context, RunnerRequest) (AgentSession, error)
}

// RunnerRequest is an immutable, credential-free snapshot for one launch.
// TrackerSession may carry provider scope but never the resolved credential.
type RunnerRequest struct {
	Issue          domain.Issue
	Workspace      domain.Workspace
	TrackerSession tracker.Session
	Codex          workflow.CodexConfig
	MaxTurns       int
	RequiredLabels []string
	SecretNames    []string
	DynamicTools   []DynamicToolSpec
	OnSessionEvent func(SessionEvent)
}

type attemptClock interface{ Now() time.Time }

// AgentAttempt runs bounded same-thread turns and rechecks the exact opaque
// tracker ID after every successful turn.
type AgentAttempt struct {
	Tracker  tracker.Adapter
	Runner   AgentRunner
	Clock    attemptClock
	Logger   *slog.Logger
	Redactor *observability.Redactor
}

func (attempt AgentAttempt) Run(
	ctx context.Context,
	request orchestrator.AgentAttemptRequest,
	emit func(domain.AgentEvent),
) (result domain.RunResult) {
	clock := attempt.Clock
	if clock == nil {
		clock = realAttemptClock{}
	}
	logger := attempt.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	redactor := attempt.Redactor
	if redactor == nil {
		redactor = observability.NewRedactor(nil, nil)
	}
	if emit == nil {
		emit = func(domain.AgentEvent) {}
	}
	emitSafe := func(event domain.AgentEvent) {
		if event.At.IsZero() {
			event.At = clock.Now().UTC()
		} else {
			event.At = event.At.UTC()
		}
		emit(cloneDomainAgentEvent(event))
	}

	var session AgentSession
	defer func() {
		if session != nil {
			if err := session.Close(); err != nil && result.Reason == domain.StopReasonNormal {
				logger.Warn("Codex session close failed", "issue_id", request.Issue.ID, "issue_identifier", request.Issue.Identifier, "error", redactor.Value(err))
				result = safeRunResult(domain.StopReasonFailed, "agent_close_failed", "The Codex process could not be stopped cleanly.")
			}
		}
		if recovered := recover(); recovered != nil {
			logger.Error("Codex attempt panic contained", "issue_id", request.Issue.ID, "issue_identifier", request.Issue.Identifier, "stack", redactor.Value(string(debug.Stack())))
			result = safeRunResult(domain.StopReasonFailed, "agent_panic", "The Codex attempt failed unexpectedly.")
		}
		if result.EndedAt.IsZero() {
			result.EndedAt = clock.Now().UTC()
		} else {
			result.EndedAt = result.EndedAt.UTC()
		}
	}()

	if err := ctx.Err(); err != nil {
		return safeRunResult(domain.StopReasonOperatorStop, "run_canceled", "The run was canceled.")
	}
	if attempt.Tracker == nil || attempt.Runner == nil {
		return safeRunResult(domain.StopReasonFailed, "agent_unavailable", "The Codex attempt is not configured.")
	}
	if strings.TrimSpace(request.Prompt) == "" || request.Workflow.Config.Agent.MaxTurns < 1 {
		return safeRunResult(domain.StopReasonFailed, "agent_configuration_invalid", "The Codex attempt configuration is invalid.")
	}
	issue, err := request.Issue.Clone()
	if err != nil {
		return safeRunResult(domain.StopReasonFailed, "issue_snapshot_invalid", "The tracker issue snapshot is invalid.")
	}
	providerConfig, err := tracker.DecodeConfig(request.Workflow.Config.Tracker)
	if err != nil {
		return safeRunResult(domain.StopReasonFailed, "tracker_session_invalid", "The tracker session configuration is invalid.")
	}
	trackerSession, err := tracker.NewSession(issue, providerConfig)
	if err != nil {
		return safeRunResult(domain.StopReasonFailed, "tracker_session_invalid", "The tracker session could not be captured.")
	}
	dynamicTools, err := dynamicToolSpecs(attempt.Tracker.AgentTools(trackerSession))
	if err != nil {
		return safeRunResult(domain.StopReasonFailed, "tool_contract_invalid", "The tracker tool contract is invalid.")
	}
	maxTurns := request.Workflow.Config.Agent.MaxTurns
	requiredLabels := append([]string(nil), request.Workflow.Config.Tracker.RequiredLabels...)
	codexConfig := cloneCodexConfig(request.Workflow.Config.Codex)
	turnCount := 0
	latest := domain.AgentEvent{}
	var eventMu sync.Mutex
	onSessionEvent := func(event SessionEvent) {
		eventMu.Lock()
		mapped := mapSessionEvent(event, turnCount)
		if mapped.Type == "" {
			eventMu.Unlock()
			return
		}
		latest = mapped
		eventMu.Unlock()
		emitSafe(mapped)
	}

	session, err = attempt.Runner.Start(ctx, RunnerRequest{
		Issue: issue, Workspace: request.Workspace, TrackerSession: trackerSession,
		Codex: codexConfig, MaxTurns: maxTurns, RequiredLabels: requiredLabels,
		SecretNames:  append([]string(nil), attempt.Tracker.SecretEnvironmentNames()...),
		DynamicTools: dynamicTools, OnSessionEvent: onSessionEvent,
	})
	if err != nil {
		logger.Warn("Codex session startup failed", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "error", redactor.Value(err))
		return safeRunResult(domain.StopReasonFailed, "agent_start_failed", "The Codex session could not be started.")
	}

	prompt := request.Prompt
	for turn := 1; turn <= maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return safeRunResult(domain.StopReasonOperatorStop, "run_canceled", "The run was canceled.")
		}
		eventMu.Lock()
		turnCount = turn
		eventMu.Unlock()
		emitSafe(domain.AgentEvent{Type: string(domain.RunStatusStreamingTurn), TurnCount: turn, Message: "Codex turn is running."})
		turnCtx := ctx
		cancel := func() {}
		if codexConfig.TurnTimeout > 0 {
			turnCtx, cancel = context.WithTimeout(ctx, codexConfig.TurnTimeout)
		}
		turnResult, turnErr := session.Turn(turnCtx, prompt)
		cancel()
		if turnErr != nil {
			return categorizeTurnError(ctx, turnErr)
		}
		if turnResult.Status != TurnCompleted {
			if turnResult.Status == TurnInterrupted && ctx.Err() != nil {
				return safeRunResult(domain.StopReasonOperatorStop, "run_canceled", "The run was canceled.")
			}
			code := strings.TrimSpace(turnResult.ErrorCode)
			if code == "" {
				code = ProtocolErrorTurnFailed
			}
			message := strings.TrimSpace(turnResult.ErrorMessage)
			if message == "" {
				message = "The Codex turn did not complete successfully."
			}
			return safeRunResult(domain.StopReasonFailed, code, message)
		}
		eventMu.Lock()
		completedEvent := latest
		completedEvent.SessionID = turnResult.SessionID
		completedEvent.ThreadID = turnResult.ThreadID
		completedEvent.TurnID = turnResult.TurnID
		completedEvent.TurnCount = turn
		completedEvent.Type = string(domain.RunStatusFinishing)
		completedEvent.Message = "Codex turn completed; refreshing the tracker issue."
		latest = completedEvent
		eventMu.Unlock()
		emitSafe(completedEvent)

		refreshed, refreshErr := attempt.Tracker.FetchIssuesByIDs(ctx, []string{issue.ID})
		if refreshErr != nil {
			logger.Warn("tracker continuation refresh failed", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "error", redactor.Value(refreshErr))
			return safeRunResult(domain.StopReasonFailed, "tracker_refresh_failed", "The tracker issue could not be refreshed after the Codex turn.")
		}
		if len(refreshed) == 0 {
			return safeRunResult(domain.StopReasonMissing, "issue_missing", "The tracker issue is no longer available.")
		}
		if len(refreshed) != 1 || refreshed[0].ID != issue.ID {
			return safeRunResult(domain.StopReasonFailed, "tracker_refresh_incomplete", "The tracker returned an incomplete issue refresh.")
		}
		issue, err = refreshed[0].Clone()
		if err != nil || issue.ValidateRequired() != nil {
			return safeRunResult(domain.StopReasonFailed, "tracker_refresh_invalid", "The refreshed tracker issue is invalid.")
		}
		state := normalizeAttemptValue(issue.State)
		if containsAttemptValue(request.Workflow.Config.Tracker.TerminalStates, state) {
			return safeRunResult(domain.StopReasonTerminal, "issue_terminal", "The tracker issue reached a terminal state.")
		}
		if !containsAttemptValue(request.Workflow.Config.Tracker.ActiveStates, state) {
			return safeRunResult(domain.StopReasonInactive, "issue_inactive", "The tracker issue is no longer active.")
		}
		if !issue.Dispatchable || !hasAttemptLabels(issue.Labels, requiredLabels) {
			return safeRunResult(domain.StopReasonUnroutable, "issue_unroutable", "The tracker issue is no longer routable.")
		}
		if turn == maxTurns {
			return safeRunResult(domain.StopReasonNormal, "max_turns_reached", "The Codex attempt reached its configured turn limit.")
		}
		prompt, err = ContinuationGuidance(turn+1, maxTurns)
		if err != nil {
			return safeRunResult(domain.StopReasonFailed, "continuation_prompt_failed", "The continuation prompt could not be built.")
		}
	}
	return safeRunResult(domain.StopReasonNormal, "max_turns_reached", "The Codex attempt reached its configured turn limit.")
}

type realAttemptClock struct{}

func (realAttemptClock) Now() time.Time { return time.Now() }

func safeRunResult(reason domain.StopReason, code, message string) domain.RunResult {
	return domain.RunResult{Reason: reason, ErrorCode: code, ErrorMessage: message}
}

func categorizeTurnError(ctx context.Context, err error) domain.RunResult {
	if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return safeRunResult(domain.StopReasonOperatorStop, "run_canceled", "The run was canceled.")
	}
	var protocolErr *ProtocolError
	if errors.As(err, &protocolErr) && protocolErr.Code == ProtocolErrorTurnSilence {
		return safeRunResult(domain.StopReasonStalled, ProtocolErrorTurnSilence, "The Codex turn stopped after producing no protocol activity.")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return safeRunResult(domain.StopReasonTimedOut, "turn_timeout", "The Codex turn exceeded its configured time limit.")
	}
	return safeRunResult(domain.StopReasonFailed, "agent_turn_failed", "The Codex turn failed. Review the local redacted diagnostics.")
}

func dynamicToolSpecs(specs []domain.ToolSpec) ([]DynamicToolSpec, error) {
	tools := make([]DynamicToolSpec, 0, len(specs))
	for _, spec := range specs {
		if err := spec.Validate(); err != nil {
			return nil, err
		}
		schema, err := json.Marshal(spec.InputSchema)
		if err != nil {
			return nil, err
		}
		tool := DynamicToolSpec{Type: "function", Name: spec.Name, Description: spec.Description, InputSchema: schema}
		if err := validateDynamicTool(tool); err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func cloneCodexConfig(config workflow.CodexConfig) workflow.CodexConfig {
	clone := config
	clone.ApprovalPolicy = cloneJSONConfigValue(config.ApprovalPolicy)
	clone.TurnSandboxPolicy = cloneJSONConfigMap(config.TurnSandboxPolicy)
	return clone
}

func cloneJSONConfigValue(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var clone any
	if json.Unmarshal(encoded, &clone) != nil {
		return nil
	}
	return clone
}

func cloneJSONConfigMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	clone, _ := cloneJSONConfigValue(value).(map[string]any)
	return clone
}

func mapSessionEvent(event SessionEvent, turnCount int) domain.AgentEvent {
	mapped := domain.AgentEvent{
		At: event.At, SessionID: event.SessionID, ThreadID: event.ThreadID, TurnID: event.TurnID,
		TurnCount: turnCount, Message: event.Summary,
		Tokens:     domain.TokenTotals{InputTokens: event.Tokens.InputTokens, OutputTokens: event.Tokens.OutputTokens, TotalTokens: event.Tokens.TotalTokens},
		RateLimits: event.RateLimits,
	}
	switch event.Type {
	case SessionEventInitialized, SessionEventThreadStarted:
		mapped.Type = string(domain.RunStatusInitializingSession)
	case SessionEventTurnStarted:
		mapped.Type = string(domain.RunStatusStreamingTurn)
	case SessionEventTelemetryUpdated:
		mapped.Type = string(domain.RunStatusStreamingTurn)
	default:
		return domain.AgentEvent{}
	}
	return mapped
}

func cloneDomainAgentEvent(event domain.AgentEvent) domain.AgentEvent {
	event.RateLimits = cloneJSONConfigMap(event.RateLimits)
	if event.Workspace != nil {
		workspace := *event.Workspace
		event.Workspace = &workspace
	}
	return event
}

func normalizeAttemptValue(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

func containsAttemptValue(values []string, target string) bool {
	if target == "" {
		return false
	}
	for _, value := range values {
		if normalizeAttemptValue(value) == target {
			return true
		}
	}
	return false
}

func hasAttemptLabels(labels, required []string) bool {
	available := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		if normalized := normalizeAttemptValue(label); normalized != "" {
			available[normalized] = struct{}{}
		}
	}
	for _, label := range required {
		normalized := normalizeAttemptValue(label)
		if normalized == "" {
			return false
		}
		if _, found := available[normalized]; !found {
			return false
		}
	}
	return true
}

var _ orchestrator.AgentAttempt = AgentAttempt{}
