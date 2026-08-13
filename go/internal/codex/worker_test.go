package codex

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/orchestrator"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

func TestWorkerContinuesSameThreadWithoutRepeatingOriginalPrompt(t *testing.T) {
	session := &fakeAgentSession{turns: []TurnResult{
		{SessionID: "thread-1-turn-1", ThreadID: "thread-1", TurnID: "turn-1", Status: TurnCompleted},
		{SessionID: "thread-1-turn-2", ThreadID: "thread-1", TurnID: "turn-2", Status: TurnCompleted},
		{SessionID: "thread-1-turn-3", ThreadID: "thread-1", TurnID: "turn-3", Status: TurnCompleted},
	}}
	adapter := &fakeAttemptTracker{issues: [][]domain.Issue{
		{attemptIssue("open", true, "ready")},
		{attemptIssue("open", true, "ready")},
		{attemptIssue("closed", true, "ready")},
	}}
	events := []domain.AgentEvent{}
	result := newAttemptForTest(adapter, &fakeAgentRunner{session: session}).Run(
		t.Context(), attemptRequest(20), func(event domain.AgentEvent) { events = append(events, event) },
	)
	if result.Reason != domain.StopReasonTerminal {
		t.Fatalf("result = %+v", result)
	}
	if !session.closed || len(session.prompts) != 3 || session.prompts[0] != "Original task for GH-42" {
		t.Fatalf("session = %+v", session)
	}
	if !slices.ContainsFunc(session.prompts[1:2], func(prompt string) bool {
		return containsAll(prompt, "continuation turn #2 of 20", "do not restate them before acting") && prompt != session.prompts[0]
	}) {
		t.Fatalf("second prompt = %q", session.prompts[1])
	}
	for _, prompt := range session.prompts[1:] {
		if containsAll(prompt, "Original task for GH-42") {
			t.Fatalf("continuation repeated original prompt: %q", prompt)
		}
	}
	if adapter.fetches != 3 || len(events) == 0 || events[len(events)-1].TurnCount != 3 {
		t.Fatalf("fetches=%d events=%+v", adapter.fetches, events)
	}
}

func TestWorkerComposedLifecycleOrderClosesProcessBeforeAfterRun(t *testing.T) {
	trace := &attemptTrace{}
	request := attemptRequest(2)
	request.Workflow.Definition.Prompt = "Original task for {{ issue.identifier }}"
	workspace := &tracingAttemptWorkspace{trace: trace, workspace: request.Workspace}
	session := &tracingAgentSession{trace: trace, result: TurnResult{Status: TurnCompleted}}
	runner := &tracingAgentRunner{trace: trace, session: session}
	worker := orchestrator.LifecycleWorker{
		Workspace: workspace,
		Agent:     newAttemptForTest(&fakeAttemptTracker{issues: [][]domain.Issue{{}}}, runner),
	}
	result := worker.Run(t.Context(), orchestrator.RunRequest{Issue: request.Issue, Workflow: request.Workflow}, nil)
	if result.Reason != domain.StopReasonMissing {
		t.Fatalf("result = %+v", result)
	}
	if got, want := trace.values(), []string{"ensure", "before_run", "launch", "initialize", "thread", "turn", "close", "after_run"}; !slices.Equal(got, want) {
		t.Fatalf("trace = %v, want %v", got, want)
	}
}

func TestWorkerReturnsExactContinuationOutcomes(t *testing.T) {
	tests := []struct {
		name   string
		issues [][]domain.Issue
		err    error
		want   domain.StopReason
	}{
		{name: "missing", issues: [][]domain.Issue{{}}, want: domain.StopReasonMissing},
		{name: "inactive", issues: [][]domain.Issue{{attemptIssue("paused", true, "ready")}}, want: domain.StopReasonInactive},
		{name: "unroutable dispatch", issues: [][]domain.Issue{{attemptIssue("open", false, "ready")}}, want: domain.StopReasonUnroutable},
		{name: "unroutable labels", issues: [][]domain.Issue{{attemptIssue("open", true)}}, want: domain.StopReasonUnroutable},
		{name: "refresh failure", err: errors.New("provider canary"), want: domain.StopReasonFailed},
		{name: "incomplete refresh", issues: [][]domain.Issue{{attemptIssueWithID("other", "open", true, "ready")}}, want: domain.StopReasonFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter := &fakeAttemptTracker{issues: test.issues, err: test.err}
			session := &fakeAgentSession{turns: []TurnResult{{SessionID: "thread-turn", ThreadID: "thread", TurnID: "turn", Status: TurnCompleted}}}
			result := newAttemptForTest(adapter, &fakeAgentRunner{session: session}).Run(t.Context(), attemptRequest(20), nil)
			if result.Reason != test.want || !session.closed {
				t.Fatalf("result=%+v closed=%v", result, session.closed)
			}
			if test.err != nil && containsAll(result.ErrorMessage, "canary") {
				t.Fatalf("unsafe result = %+v", result)
			}
		})
	}
}

func TestWorkerStopsAtMaximumTurnsAndClosesBeforeReturning(t *testing.T) {
	adapter := &fakeAttemptTracker{issues: [][]domain.Issue{
		{attemptIssue("open", true, "ready")},
		{attemptIssue("open", true, "ready")},
	}}
	session := &fakeAgentSession{turns: []TurnResult{
		{SessionID: "thread-1-turn-1", ThreadID: "thread-1", TurnID: "turn-1", Status: TurnCompleted},
		{SessionID: "thread-1-turn-2", ThreadID: "thread-1", TurnID: "turn-2", Status: TurnCompleted},
	}}
	result := newAttemptForTest(adapter, &fakeAgentRunner{session: session}).Run(t.Context(), attemptRequest(2), nil)
	if result.Reason != domain.StopReasonNormal || !session.closed || len(session.prompts) != 2 || adapter.fetches != 2 {
		t.Fatalf("result=%+v session=%+v fetches=%d", result, session, adapter.fetches)
	}
}

func TestWorkerConvertsNormalSessionCloseFailureToRetryableFailure(t *testing.T) {
	adapter := &fakeAttemptTracker{issues: [][]domain.Issue{{attemptIssue("open", true, "ready")}}}
	session := &fakeAgentSession{
		turns:    []TurnResult{{Status: TurnCompleted}},
		closeErr: errors.New("close canary"),
	}
	result := newAttemptForTest(adapter, &fakeAgentRunner{session: session}).Run(t.Context(), attemptRequest(1), nil)
	if result.Reason != domain.StopReasonFailed || result.ErrorCode != "agent_close_failed" || containsAll(result.ErrorMessage, "canary") {
		t.Fatalf("result = %+v", result)
	}
}

func TestWorkerCategorizesStartupTurnTimeoutStallAndCancellation(t *testing.T) {
	tests := []struct {
		name      string
		runnerErr error
		turnErr   error
		status    TurnStatus
		cancel    bool
		want      domain.StopReason
		code      string
	}{
		{name: "startup", runnerErr: errors.New("startup canary"), want: domain.StopReasonFailed, code: "agent_start_failed"},
		{name: "turn", turnErr: errors.New("turn canary"), want: domain.StopReasonFailed, code: "agent_turn_failed"},
		{name: "timeout", turnErr: context.DeadlineExceeded, want: domain.StopReasonTimedOut, code: "turn_timeout"},
		{name: "stall", turnErr: newProtocolError(ProtocolErrorTurnSilence, "stalled canary", true, nil), want: domain.StopReasonStalled, code: ProtocolErrorTurnSilence},
		{name: "failed result", status: TurnFailed, want: domain.StopReasonFailed, code: ProtocolErrorTurnFailed},
		{name: "cancel", cancel: true, want: domain.StopReasonOperatorStop, code: "run_canceled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			if test.cancel {
				cancel()
			} else {
				defer cancel()
			}
			status := test.status
			if status == "" {
				status = TurnCompleted
			}
			session := &fakeAgentSession{turns: []TurnResult{{Status: status, ErrorCode: ProtocolErrorTurnFailed, ErrorMessage: "safe turn failure"}}, turnErr: test.turnErr}
			result := newAttemptForTest(&fakeAttemptTracker{}, &fakeAgentRunner{session: session, err: test.runnerErr}).Run(ctx, attemptRequest(2), nil)
			if result.Reason != test.want || result.ErrorCode != test.code {
				t.Fatalf("result = %+v", result)
			}
			if containsAll(result.ErrorMessage, "canary") {
				t.Fatalf("unsafe result = %+v", result)
			}
		})
	}
}

func TestWorkerCapturesTrackerSessionAndWorkflowBeforeRunnerStarts(t *testing.T) {
	request := attemptRequest(3)
	started := make(chan struct{})
	release := make(chan struct{})
	runner := &fakeAgentRunner{session: &fakeAgentSession{turns: []TurnResult{{Status: TurnCompleted}}}, started: started, release: release}
	adapter := &fakeAttemptTracker{issues: [][]domain.Issue{{}}}
	result := make(chan domain.RunResult, 1)
	go func() { result <- newAttemptForTest(adapter, runner).Run(t.Context(), request, nil) }()
	<-started
	request.Workflow.Config.Agent.MaxTurns = 99
	request.Workflow.Config.Tracker.RequiredLabels[0] = "mutated"
	request.Issue.Labels[0] = "mutated"
	close(release)
	got := <-result
	if got.Reason != domain.StopReasonMissing || runner.request.MaxTurns != 3 || runner.request.TrackerSession.Issue.Labels[0] != "ready" || runner.request.RequiredLabels[0] != "ready" {
		t.Fatalf("result=%+v request=%+v", got, runner.request)
	}
}

func TestWorkerContainsRunnerPanicAndReturnsUTCOnce(t *testing.T) {
	clock := fixedAttemptClock{now: time.Date(2026, 8, 7, 12, 30, 0, 0, time.FixedZone("offset", -4*60*60))}
	attempt := newAttemptForTest(&fakeAttemptTracker{}, panicAgentRunner{})
	attempt.Clock = clock
	result := attempt.Run(t.Context(), attemptRequest(2), nil)
	if result.Reason != domain.StopReasonFailed || result.ErrorCode != "agent_panic" || result.EndedAt.Location() != time.UTC || !result.EndedAt.Equal(clock.now) {
		t.Fatalf("result = %+v", result)
	}
}

type fakeAgentRunner struct {
	mu      sync.Mutex
	session AgentSession
	err     error
	request RunnerRequest
	started chan struct{}
	release chan struct{}
}

type attemptTrace struct {
	mu    sync.Mutex
	items []string
}

func (trace *attemptTrace) add(value string) {
	trace.mu.Lock()
	trace.items = append(trace.items, value)
	trace.mu.Unlock()
}

func (trace *attemptTrace) values() []string {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return append([]string(nil), trace.items...)
}

type tracingAttemptWorkspace struct {
	trace     *attemptTrace
	workspace domain.Workspace
}

func (workspace *tracingAttemptWorkspace) Ensure(context.Context, domain.Issue, workflow.EffectiveConfig) (domain.Workspace, error) {
	workspace.trace.add("ensure")
	return workspace.workspace, nil
}

func (workspace *tracingAttemptWorkspace) RunHook(_ context.Context, hook domain.Hook, _ domain.Workspace, _ time.Duration) error {
	workspace.trace.add(string(hook.Name))
	return nil
}

type tracingAgentRunner struct {
	trace   *attemptTrace
	session AgentSession
}

func (runner *tracingAgentRunner) Start(context.Context, RunnerRequest) (AgentSession, error) {
	runner.trace.add("launch")
	runner.trace.add("initialize")
	runner.trace.add("thread")
	return runner.session, nil
}

type tracingAgentSession struct {
	trace  *attemptTrace
	result TurnResult
}

func (session *tracingAgentSession) Turn(context.Context, string) (TurnResult, error) {
	session.trace.add("turn")
	return session.result, nil
}

func (session *tracingAgentSession) Close() error {
	session.trace.add("close")
	return nil
}

func (runner *fakeAgentRunner) Start(_ context.Context, request RunnerRequest) (AgentSession, error) {
	runner.mu.Lock()
	runner.request = request
	runner.mu.Unlock()
	if runner.started != nil {
		close(runner.started)
	}
	if runner.release != nil {
		<-runner.release
	}
	return runner.session, runner.err
}

type panicAgentRunner struct{}

func (panicAgentRunner) Start(context.Context, RunnerRequest) (AgentSession, error) {
	panic("secret canary")
}

type fakeAgentSession struct {
	turns    []TurnResult
	turnErr  error
	closeErr error
	prompts  []string
	closed   bool
}

func (session *fakeAgentSession) Turn(_ context.Context, prompt string) (TurnResult, error) {
	session.prompts = append(session.prompts, prompt)
	if session.turnErr != nil {
		return TurnResult{}, session.turnErr
	}
	if len(session.turns) == 0 {
		return TurnResult{}, errors.New("unexpected turn")
	}
	result := session.turns[0]
	session.turns = session.turns[1:]
	return result, nil
}

func (session *fakeAgentSession) Close() error { session.closed = true; return session.closeErr }

type fakeAttemptTracker struct {
	issues  [][]domain.Issue
	err     error
	fetches int
}

func (*fakeAttemptTracker) Kind() string { return "github" }
func (*fakeAttemptTracker) FetchIssuesByStates(context.Context, []string) ([]domain.Issue, error) {
	return []domain.Issue{}, nil
}
func (adapter *fakeAttemptTracker) FetchIssuesByIDs(_ context.Context, ids []string) ([]domain.Issue, error) {
	adapter.fetches++
	if len(ids) != 1 || ids[0] != "issue-42" {
		return nil, errors.New("wrong issue id")
	}
	if adapter.err != nil {
		return nil, adapter.err
	}
	if len(adapter.issues) == 0 {
		return nil, errors.New("unexpected refresh")
	}
	issues := adapter.issues[0]
	adapter.issues = adapter.issues[1:]
	return issues, nil
}
func (*fakeAttemptTracker) AgentTools(tracker.Session) []domain.ToolSpec { return []domain.ToolSpec{} }
func (*fakeAttemptTracker) ExecuteAgentTool(context.Context, domain.ToolCall, tracker.Session) domain.ToolResult {
	return domain.ToolUnavailableResult()
}
func (*fakeAttemptTracker) SecretEnvironmentNames() []string { return []string{"GITHUB_TOKEN"} }

type fixedAttemptClock struct{ now time.Time }

func (clock fixedAttemptClock) Now() time.Time { return clock.now }

func newAttemptForTest(adapter tracker.Adapter, runner AgentRunner) AgentAttempt {
	return AgentAttempt{
		Tracker: adapter, Runner: runner, Clock: fixedAttemptClock{now: time.Date(2026, 8, 7, 16, 30, 0, 0, time.UTC)},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func attemptRequest(maxTurns int) orchestrator.AgentAttemptRequest {
	issue := attemptIssue("open", true, "ready")
	return orchestrator.AgentAttemptRequest{
		Issue: issue, Workspace: domain.Workspace{Path: "/workspaces/GH-42", Root: "/workspaces", IssueID: issue.ID, IssueIdentifier: issue.Identifier},
		Workflow: workflow.Snapshot{Config: workflow.EffectiveConfig{
			Tracker: workflow.TrackerConfig{Kind: "github", Provider: map[string]any{"owner": "coryj627", "repository": "symphony"}, ActiveStates: []string{"open"}, TerminalStates: []string{"closed"}, RequiredLabels: []string{"ready"}},
			Agent:   workflow.AgentConfig{MaxTurns: maxTurns},
			Codex:   workflow.CodexConfig{Command: "codex app-server", ApprovalPolicy: "on-request", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute, ReadTimeout: time.Second, StallTimeout: time.Minute},
		}},
		Prompt: "Original task for GH-42",
	}
}

func attemptIssue(state string, dispatchable bool, labels ...string) domain.Issue {
	return attemptIssueWithID("issue-42", state, dispatchable, labels...)
}

func attemptIssueWithID(id, state string, dispatchable bool, labels ...string) domain.Issue {
	return domain.Issue{ID: id, NativeRef: map[string]any{"number": 42}, Identifier: "GH-42", Title: "Task", State: state, Dispatchable: dispatchable, Labels: append([]string{}, labels...), BlockedBy: []domain.BlockerRef{}}
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}
