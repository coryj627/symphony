package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/workflow"
)

func TestLifecycleWorkerOrdersHooksAndAlwaysRunsAfterRun(t *testing.T) {
	trace := &safeTrace{}
	workspace := &traceWorkspace{trace: trace, workspace: domain.Workspace{IssueID: "1", IssueIdentifier: "GH-1"}}
	agent := AgentAttemptFunc(func(context.Context, AgentAttemptRequest, func(domain.AgentEvent)) domain.RunResult {
		trace.add("agent")
		return domain.RunResult{Reason: domain.StopReasonFailed, ErrorCode: "agent_failed", ErrorMessage: "safe failure"}
	})
	worker := lifecycleWorkerForTest(workspace, agent)
	result := worker.Run(context.Background(), lifecycleRunRequest(), func(domain.AgentEvent) {})
	if result.ErrorCode != "agent_failed" {
		t.Fatalf("result = %#v", result)
	}
	if got, want := trace.values(), []string{"ensure", "before_run", "agent", "after_run"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("trace = %#v, want %#v", got, want)
	}
}

func TestLifecycleWorkerFailureBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		workspace *traceWorkspace
		workflow  workflow.Snapshot
		wantCode  string
		wantTrace []string
	}{
		{
			name:      "ensure failure has no after run",
			workspace: &traceWorkspace{ensureErr: errors.New("ensure canary")},
			workflow:  lifecycleWorkflow(), wantCode: "workspace_failed", wantTrace: []string{"ensure"},
		},
		{
			name:      "prompt failure still runs after run",
			workspace: &traceWorkspace{workspace: domain.Workspace{IssueID: "1", IssueIdentifier: "GH-1"}},
			workflow: func() workflow.Snapshot {
				snapshot := lifecycleWorkflow()
				snapshot.Definition.Prompt = "{{ missing.value }}"
				return snapshot
			}(),
			wantCode: "prompt_failed", wantTrace: []string{"ensure", "after_run"},
		},
		{
			name:      "before run failure skips agent but runs after run",
			workspace: &traceWorkspace{workspace: domain.Workspace{IssueID: "1", IssueIdentifier: "GH-1"}, hookErrors: map[domain.HookName]error{domain.HookNameBeforeRun: errors.New("hook canary")}},
			workflow:  lifecycleWorkflow(), wantCode: "before_run_failed", wantTrace: []string{"ensure", "before_run", "after_run"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			trace := &safeTrace{}
			test.workspace.trace = trace
			agent := AgentAttemptFunc(func(context.Context, AgentAttemptRequest, func(domain.AgentEvent)) domain.RunResult {
				trace.add("agent")
				return domain.RunResult{Reason: domain.StopReasonNormal}
			})
			worker := lifecycleWorkerForTest(test.workspace, agent)
			request := lifecycleRunRequest()
			request.Workflow = test.workflow
			result := worker.Run(context.Background(), request, func(domain.AgentEvent) {})
			if result.ErrorCode != test.wantCode {
				t.Fatalf("result = %#v, want code %q", result, test.wantCode)
			}
			if got := trace.values(); !reflect.DeepEqual(got, test.wantTrace) {
				t.Fatalf("trace = %#v, want %#v", got, test.wantTrace)
			}
		})
	}
}

func TestLifecycleWorkerTimestampsEventsAndContainsPanics(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 123, time.FixedZone("test", -4*60*60))
	clock := newFakeClock(now)
	trace := &safeTrace{}
	workspace := &traceWorkspace{trace: trace, workspace: domain.Workspace{IssueID: "1", IssueIdentifier: "GH-1"}}
	agent := AgentAttemptFunc(func(_ context.Context, _ AgentAttemptRequest, emit func(domain.AgentEvent)) domain.RunResult {
		emit(domain.AgentEvent{Type: "streaming_turn"})
		panic("panic-canary-secret")
	})
	worker := lifecycleWorkerForTest(workspace, agent)
	worker.Clock = clock
	events := []domain.AgentEvent{}
	result := worker.Run(context.Background(), lifecycleRunRequest(), func(event domain.AgentEvent) { events = append(events, event) })
	if result.ErrorCode != "worker_panic" || result.ErrorMessage != "The worker failed unexpectedly." || !result.EndedAt.Equal(now.UTC()) {
		t.Fatalf("panic result = %#v", result)
	}
	if len(events) == 0 {
		t.Fatal("no events emitted")
	}
	for _, event := range events {
		if event.At.Location() != time.UTC || event.At.IsZero() {
			t.Fatalf("event timestamp = %v", event.At)
		}
	}
	if _, err := json.Marshal(events); err != nil {
		t.Fatalf("events did not serialize: %v", err)
	}
	if got := trace.values(); !reflect.DeepEqual(got, []string{"ensure", "before_run", "after_run"}) {
		t.Fatalf("panic trace = %#v", got)
	}
}

func TestLifecycleWorkerHonorsCancellationAndStillRunsAfterRun(t *testing.T) {
	trace := &safeTrace{}
	workspace := &traceWorkspace{trace: trace, workspace: domain.Workspace{IssueID: "1", IssueIdentifier: "GH-1"}}
	agent := AgentAttemptFunc(func(ctx context.Context, _ AgentAttemptRequest, _ func(domain.AgentEvent)) domain.RunResult {
		trace.add("agent")
		<-ctx.Done()
		return domain.RunResult{Reason: domain.StopReasonOperatorStop, ErrorCode: "run_canceled"}
	})
	worker := lifecycleWorkerForTest(workspace, agent)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := worker.Run(ctx, lifecycleRunRequest(), func(domain.AgentEvent) {})
	if result.Reason != domain.StopReasonOperatorStop || result.ErrorCode != "run_canceled" {
		t.Fatalf("canceled result = %#v", result)
	}
	if got := trace.values(); !reflect.DeepEqual(got, []string{"ensure", "before_run", "after_run"}) {
		t.Fatalf("canceled trace = %#v", got)
	}
}

func TestLifecycleWorkerAfterRunFailureDoesNotReplacePrimaryOutcome(t *testing.T) {
	trace := &safeTrace{}
	workspace := &traceWorkspace{
		trace: trace, workspace: domain.Workspace{IssueID: "1", IssueIdentifier: "GH-1"},
		hookErrors: map[domain.HookName]error{domain.HookNameAfterRun: errors.New("after canary")},
	}
	worker := lifecycleWorkerForTest(workspace, AgentAttemptFunc(func(context.Context, AgentAttemptRequest, func(domain.AgentEvent)) domain.RunResult {
		trace.add("agent")
		return domain.RunResult{Reason: domain.StopReasonNormal}
	}))
	result := worker.Run(context.Background(), lifecycleRunRequest(), func(domain.AgentEvent) {})
	if result.Reason != domain.StopReasonNormal || result.ErrorCode != "" {
		t.Fatalf("after_run replaced result: %#v", result)
	}
}

type safeTrace struct {
	mu    sync.Mutex
	items []string
}

func (trace *safeTrace) add(item string) {
	trace.mu.Lock()
	trace.items = append(trace.items, item)
	trace.mu.Unlock()
}

func (trace *safeTrace) values() []string {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return append([]string(nil), trace.items...)
}

type traceWorkspace struct {
	trace      *safeTrace
	workspace  domain.Workspace
	ensureErr  error
	hookErrors map[domain.HookName]error
}

func (workspace *traceWorkspace) Ensure(context.Context, domain.Issue, workflow.EffectiveConfig) (domain.Workspace, error) {
	workspace.trace.add("ensure")
	return workspace.workspace, workspace.ensureErr
}

func (*traceWorkspace) Remove(context.Context, domain.Issue, workflow.EffectiveConfig) error {
	return nil
}

func (workspace *traceWorkspace) RunHook(_ context.Context, hook domain.Hook, _ domain.Workspace, _ time.Duration) error {
	workspace.trace.add(string(hook.Name))
	return workspace.hookErrors[hook.Name]
}

func lifecycleWorkerForTest(workspace LifecycleWorkspace, agent AgentAttempt) LifecycleWorker {
	return LifecycleWorker{
		Workspace: workspace, Agent: agent, Clock: RealClock{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Redactor: observability.NewRedactor(nil, nil),
	}
}

func lifecycleRunRequest() RunRequest {
	return RunRequest{Issue: readyIssue("1", "open", "symphony"), Workflow: lifecycleWorkflow()}
}

func lifecycleWorkflow() workflow.Snapshot {
	snapshot := testWorkflowSnapshot()
	snapshot.Definition.Prompt = "Work on {{ issue.identifier }}."
	snapshot.Config.Hooks.BeforeRun = "before"
	snapshot.Config.Hooks.AfterRun = "after"
	snapshot.Config.Hooks.Timeout = time.Second
	return snapshot
}
