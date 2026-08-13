package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/codex"
	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/orchestrator"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

func TestRuntimeCodexStartsRealSchedulerOnlyAfterPrerequisitesPass(t *testing.T) {
	snapshot := validQueueSnapshot("github", "", "digest-1")
	snapshot.Config.Agent.MaxConcurrent = 1
	store := &fakeWorkflowStore{current: snapshot, hasCurrent: true, changes: make(chan workflow.Change, 8)}
	adapter := &fakeAdapter{kind: "github", fetches: []fakeFetch{{issues: []domain.Issue{}}}}
	trace := []string{}
	var traceMu sync.Mutex
	runtime := NewAgentRuntime(AgentRuntimeOptions{
		Store: store, Factory: &fakeFactory{adapters: []tracker.Adapter{adapter}}, Resolver: &fakeResolver{value: []byte("test")},
		Events: observability.NewJournal(observability.JournalOptions{}), InitiallyPaused: true,
		Build: func(context.Context, workflow.Snapshot, tracker.Adapter, *codex.RequestBroker) (AgentRuntimeBuild, error) {
			traceMu.Lock()
			trace = append(trace, "preflight")
			traceMu.Unlock()
			return AgentRuntimeBuild{Workspace: noOpAgentWorkspace{}, Worker: orchestrator.WorkerFunc(func(context.Context, orchestrator.RunRequest, func(domain.AgentEvent)) domain.RunResult {
				return domain.RunResult{Reason: domain.StopReasonNormal}
			})}, nil
		},
	})
	if err := runtime.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	snapshotView, err := runtime.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	traceMu.Lock()
	gotTrace := append([]string(nil), trace...)
	traceMu.Unlock()
	if len(gotTrace) != 1 || gotTrace[0] != "preflight" || !snapshotView.Scheduler.Available || snapshotView.Scheduler.State != "paused" {
		t.Fatalf("trace=%v scheduler=%+v", gotTrace, snapshotView.Scheduler)
	}
	if err := runtime.SetScheduler(t.Context(), true); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCodexSurfacesExactUnavailablePrerequisiteWithoutAbortingUI(t *testing.T) {
	snapshot := validQueueSnapshot("github", "", "digest-1")
	store := &fakeWorkflowStore{current: snapshot, hasCurrent: true, changes: make(chan workflow.Change, 8)}
	runtime := NewAgentRuntime(AgentRuntimeOptions{
		Store: store, Factory: &fakeFactory{errors: []error{&tracker.Error{Category: tracker.CategoryAuth, Message: "canary"}}}, Resolver: &fakeResolver{},
		Events: observability.NewJournal(observability.JournalOptions{}),
		Build: func(context.Context, workflow.Snapshot, tracker.Adapter, *codex.RequestBroker) (AgentRuntimeBuild, error) {
			return AgentRuntimeBuild{}, errors.New("unexpected build")
		},
	})
	if err := runtime.Start(t.Context()); err != nil {
		t.Fatalf("readiness failure aborted UI startup: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	view, err := runtime.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if view.Scheduler.Available || view.Scheduler.Enabled || view.Scheduler.State != "unavailable" || !strings.Contains(view.Scheduler.Message, "credential") || strings.Contains(view.Scheduler.Message, "canary") {
		t.Fatalf("scheduler = %+v", view.Scheduler)
	}
	if err := runtime.SetScheduler(t.Context(), true); !errors.Is(err, ErrAgentRuntimeUnavailable) {
		t.Fatalf("SetScheduler = %v", err)
	}
}

func TestRuntimeCodexCredentialNotificationRebuildsAndRetiresActiveGeneration(t *testing.T) {
	snapshot := validQueueSnapshot("github", "", "digest-1")
	snapshot.Config.Agent.MaxConcurrent = 1
	store := &fakeWorkflowStore{current: snapshot, hasCurrent: true, changes: make(chan workflow.Change, 8)}
	first := &retiringAdapter{Adapter: &fakeAdapter{kind: "github", fetches: []fakeFetch{{issues: []domain.Issue{}}}}}
	factory := &fakeFactory{adapters: []tracker.Adapter{
		first,
		&fakeAdapter{kind: "github", fetches: []fakeFetch{{issues: []domain.Issue{}}}},
	}}
	builds := make(chan struct{}, 2)
	runtime := NewAgentRuntime(AgentRuntimeOptions{
		Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test")}, Events: observability.NewJournal(observability.JournalOptions{}), InitiallyPaused: true,
		Build: func(context.Context, workflow.Snapshot, tracker.Adapter, *codex.RequestBroker) (AgentRuntimeBuild, error) {
			builds <- struct{}{}
			return AgentRuntimeBuild{Workspace: noOpAgentWorkspace{}, Worker: orchestrator.WorkerFunc(func(context.Context, orchestrator.RunRequest, func(domain.AgentEvent)) domain.RunResult {
				return domain.RunResult{Reason: domain.StopReasonNormal}
			})}, nil
		},
	})
	if err := runtime.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	<-builds
	runtime.NotifyCredentialChanged()
	select {
	case <-builds:
	case <-time.After(3 * time.Second):
		t.Fatal("credential notification did not rebuild the active runtime")
	}
	if !first.wasClosed() {
		t.Fatal("credential rebuild retained the previous adapter generation")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, err := runtime.Snapshot(t.Context())
		if err == nil && view.Scheduler.Available {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("rebuilt runtime did not become ready")
}

func TestRuntimeCodexPreflightErrorsExposeStablePrerequisiteMessages(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		code    string
		message string
	}{
		{name: "bash", err: codex.ErrBashUnavailable, code: "bash_unavailable", message: "Bash"},
		{name: "version", err: &codex.ProtocolError{Code: string(codex.CompatibilityCodeVersionMismatch)}, code: "codex_version_incompatible", message: "reviewed"},
		{name: "schema", err: &codex.ProtocolError{Code: string(codex.CompatibilityCodeSchemaIntegrity)}, code: "codex_schema_invalid", message: "schema"},
		{name: "startup", err: errors.New("process canary"), code: "codex_preflight_failed", message: "diagnostics"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var prerequisite *AgentPrerequisiteError
			if err := codexPreflightError(test.err); !errors.As(err, &prerequisite) || prerequisite.Code != test.code || !strings.Contains(prerequisite.Message, test.message) || strings.Contains(prerequisite.Message, "canary") {
				t.Fatalf("prerequisite = %+v", prerequisite)
			}
		})
	}
}

type noOpAgentWorkspace struct{}

type retiringAdapter struct {
	tracker.Adapter
	mu     sync.Mutex
	closed bool
}

func (adapter *retiringAdapter) Close() error {
	adapter.mu.Lock()
	adapter.closed = true
	adapter.mu.Unlock()
	return nil
}

func (adapter *retiringAdapter) wasClosed() bool {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.closed
}

func (noOpAgentWorkspace) Ensure(context.Context, domain.Issue, workflow.EffectiveConfig) (domain.Workspace, error) {
	return domain.Workspace{}, nil
}
func (noOpAgentWorkspace) Remove(context.Context, domain.Issue, workflow.EffectiveConfig) error {
	return nil
}
func (noOpAgentWorkspace) RunHook(context.Context, domain.Hook, domain.Workspace, time.Duration) error {
	return nil
}

var _ orchestrator.WorkspaceManager = noOpAgentWorkspace{}
var _ orchestrator.LifecycleWorkspace = noOpAgentWorkspace{}
