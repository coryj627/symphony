package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/coryj627/symphony/go/internal/codex"
	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/orchestrator"
	"github.com/coryj627/symphony/go/internal/secrets"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

const agentRuntimeShutdownLimit = 10 * time.Second

// AgentPrerequisiteError carries a stable operator-safe readiness reason.
type AgentPrerequisiteError struct {
	Code    string
	Message string
}

func (err *AgentPrerequisiteError) Error() string {
	if err == nil {
		return ""
	}
	return err.Code
}

// AgentRuntimeBuild is one captured scheduler generation. Cleanup runs only
// after its orchestrator and workers have confirmed exit.
type AgentRuntimeBuild struct {
	Workspace orchestrator.WorkspaceManager
	Worker    orchestrator.Worker
	Cleanup   func()
}

type AgentRuntimeBuilder func(context.Context, workflow.Snapshot, tracker.Adapter, *codex.RequestBroker) (AgentRuntimeBuild, error)

type AgentRuntimeOptions struct {
	Store           workflow.Store
	Factory         tracker.Factory
	Resolver        secrets.Resolver
	Events          *observability.Journal
	Logger          *slog.Logger
	Redactor        *observability.Redactor
	InitiallyPaused bool
	OperatorWindow  time.Duration
	Build           AgentRuntimeBuilder
}

// AgentRuntime supervises the credential-bound adapter, real Phase 3
// orchestrator, and memory-only operator request broker.
type AgentRuntime struct {
	options AgentRuntimeOptions
	broker  *codex.RequestBroker

	mu               sync.RWMutex
	rebuildMu        sync.Mutex
	wg               sync.WaitGroup
	engine           *orchestrator.Orchestrator
	adapter          tracker.Adapter
	cleanup          func()
	readinessCode    string
	readinessMessage string
	runtimeCtx       context.Context
	cancel           context.CancelFunc
	started          bool
	closed           bool
}

func NewAgentRuntime(options AgentRuntimeOptions) *AgentRuntime {
	if options.Events == nil {
		options.Events = observability.NewJournal(observability.JournalOptions{})
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if options.Redactor == nil {
		options.Redactor = observability.NewRedactor(nil, nil)
	}
	runtime := &AgentRuntime{options: options, readinessCode: "starting", readinessMessage: "The Codex runtime is checking its prerequisites."}
	runtime.broker = codex.NewRequestBroker(codex.RequestBrokerOptions{
		Window: options.OperatorWindow, Redactor: options.Redactor,
		OnChange: func() { runtime.publish("runtime.changed", map[string]any{}) },
		OnWarning: func(request domain.OperatorRequest) {
			runtime.publish("operator.request_warning", map[string]any{"request_id": request.ID, "issue_id": request.IssueID, "issue_identifier": request.IssueIdentifier})
		},
	})
	return runtime
}

func (runtime *AgentRuntime) Start(ctx context.Context) error {
	if ctx == nil {
		return errors.New("agent runtime context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	runtime.mu.Lock()
	if runtime.closed {
		runtime.mu.Unlock()
		return ErrAgentRuntimeUnavailable
	}
	if runtime.started {
		runtime.mu.Unlock()
		return nil
	}
	runtime.started = true
	runtime.runtimeCtx, runtime.cancel = context.WithCancel(ctx)
	runtimeCtx := runtime.runtimeCtx
	runtime.mu.Unlock()

	if err := runtime.rebuild(runtimeCtx, false); err != nil {
		runtime.recordUnavailable(err)
	}
	return nil
}

func (runtime *AgentRuntime) rebuild(ctx context.Context, retire bool) error {
	runtime.rebuildMu.Lock()
	defer runtime.rebuildMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if retire {
		runtime.retireCurrent()
	}
	if runtime.options.Store == nil || runtime.options.Factory == nil || runtime.options.Resolver == nil || runtime.options.Build == nil {
		return &AgentPrerequisiteError{Code: "runtime_dependencies_unavailable", Message: "The Codex runtime is missing a required local component."}
	}
	snapshot, available := runtime.options.Store.Current()
	if !available {
		var err error
		snapshot, err = runtime.options.Store.Load(ctx)
		if err != nil {
			return &AgentPrerequisiteError{Code: "workflow_unavailable", Message: "The workflow is unavailable or invalid."}
		}
	}
	adapter, err := runtime.options.Factory.Build(ctx, snapshot.Config.Tracker, runtime.options.Resolver)
	if err != nil {
		return readinessErrorForTracker(err)
	}
	build, err := runtime.options.Build(ctx, snapshot, adapter, runtime.broker)
	if err != nil {
		retireTrackerAdapter(adapter)
		return err
	}
	if build.Workspace == nil || build.Worker == nil {
		retireTrackerAdapter(adapter)
		if build.Cleanup != nil {
			build.Cleanup()
		}
		return &AgentPrerequisiteError{Code: "worker_unavailable", Message: "The Codex worker could not be prepared."}
	}
	engine, err := orchestrator.Start(ctx, orchestrator.Options{
		Tracker: adapter, Workflow: runtime.options.Store, Workspace: build.Workspace, Worker: build.Worker,
		Events: runtime.options.Events, Logger: runtime.options.Logger, InitiallyPaused: runtime.options.InitiallyPaused,
	})
	if err != nil {
		retireTrackerAdapter(adapter)
		if build.Cleanup != nil {
			build.Cleanup()
		}
		return &AgentPrerequisiteError{Code: "scheduler_start_failed", Message: "The scheduler could not start with the active workflow."}
	}
	runtime.mu.Lock()
	if runtime.closed {
		runtime.mu.Unlock()
		closeCtx, cancel := context.WithTimeout(context.Background(), agentRuntimeShutdownLimit)
		_ = engine.Close(closeCtx)
		cancel()
		retireTrackerAdapter(adapter)
		if build.Cleanup != nil {
			build.Cleanup()
		}
		return context.Canceled
	}
	runtime.engine = engine
	runtime.adapter = adapter
	runtime.cleanup = build.Cleanup
	runtime.readinessCode = "ready"
	runtime.readinessMessage = "The Codex runtime is ready."
	runtime.mu.Unlock()
	runtime.publish("runtime.changed", map[string]any{})
	return nil
}

func (runtime *AgentRuntime) recordUnavailable(err error) {
	code, message := "agent_runtime_unavailable", "The Codex runtime is unavailable. Review the local redacted diagnostics."
	var prerequisite *AgentPrerequisiteError
	if errors.As(err, &prerequisite) {
		if prerequisite.Code != "" {
			code = prerequisite.Code
		}
		if prerequisite.Message != "" {
			message = prerequisite.Message
		}
	}
	runtime.mu.Lock()
	runtime.readinessCode = code
	runtime.readinessMessage = message
	runtime.mu.Unlock()
	runtime.options.Logger.Warn("Codex runtime prerequisite unavailable", "error_code", code)
	runtime.publish("runtime.changed", map[string]any{"readiness": code})
}

func readinessErrorForTracker(err error) error {
	var portable *tracker.Error
	if !errors.As(err, &portable) || portable == nil {
		return &AgentPrerequisiteError{Code: "tracker_unavailable", Message: "The tracker could not be reached during the Codex readiness check."}
	}
	switch portable.Category {
	case tracker.CategoryAuth:
		return &AgentPrerequisiteError{Code: "tracker_credential_unavailable", Message: "The tracker credential is unavailable. Add or update it in Configuration."}
	case tracker.CategoryConfig:
		return &AgentPrerequisiteError{Code: "tracker_configuration_invalid", Message: "The tracker configuration is invalid. Review Configuration."}
	default:
		return &AgentPrerequisiteError{Code: "tracker_unavailable", Message: "The tracker could not be reached during the Codex readiness check."}
	}
}

func (runtime *AgentRuntime) retireCurrent() {
	runtime.mu.Lock()
	engine, adapter, cleanup := runtime.engine, runtime.adapter, runtime.cleanup
	runtime.engine, runtime.adapter, runtime.cleanup = nil, nil, nil
	runtime.readinessCode = "rebuilding"
	runtime.readinessMessage = "The Codex runtime is rebuilding its credential-bound tracker session."
	runtime.mu.Unlock()
	if engine != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), agentRuntimeShutdownLimit)
		if err := engine.Close(closeCtx); err != nil {
			runtime.options.Logger.Warn("Codex scheduler shutdown was not confirmed", "error", runtime.options.Redactor.Value(err))
		}
		cancel()
	}
	retireTrackerAdapter(adapter)
	if cleanup != nil {
		cleanup()
	}
}

func retireTrackerAdapter(adapter tracker.Adapter) {
	if retire, ok := adapter.(interface{ Close() error }); ok && retire != nil {
		_ = retire.Close()
	}
}

func (runtime *AgentRuntime) NotifyCredentialChanged() {
	runtime.mu.RLock()
	ctx := runtime.runtimeCtx
	started, closed := runtime.started, runtime.closed
	if !started || closed || ctx == nil {
		runtime.mu.RUnlock()
		return
	}
	runtime.wg.Add(1)
	runtime.mu.RUnlock()
	go func() {
		defer runtime.wg.Done()
		if err := runtime.rebuild(ctx, true); err != nil && ctx.Err() == nil {
			runtime.recordUnavailable(err)
		}
	}()
}

func (runtime *AgentRuntime) Snapshot(ctx context.Context) (domain.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return domain.Snapshot{}, err
	}
	runtime.mu.RLock()
	engine, message := runtime.engine, runtime.readinessMessage
	runtime.mu.RUnlock()
	if engine == nil {
		snapshot := domain.EmptySnapshot()
		snapshot.GeneratedAt = time.Now().UTC()
		snapshot.EventCursor = runtime.options.Events.Cursor()
		snapshot.Scheduler = domain.SchedulerStatus{Available: false, Enabled: false, State: "unavailable", Message: message}
		snapshot.Requests = runtime.broker.Pending()
		return snapshot.Clone()
	}
	snapshot, err := engine.Snapshot(ctx)
	if err != nil {
		return domain.Snapshot{}, err
	}
	snapshot.Requests = runtime.broker.Pending()
	return snapshot.Clone()
}

func (runtime *AgentRuntime) Issue(ctx context.Context, identifier string) (domain.IssueDetail, error) {
	engine := runtime.currentEngine()
	if engine == nil {
		if err := ctx.Err(); err != nil {
			return domain.IssueDetail{}, err
		}
		return domain.IssueDetail{}, ErrIssueNotFound
	}
	return engine.Issue(ctx, identifier)
}

func (runtime *AgentRuntime) EventsAfter(ctx context.Context, cursor domain.EventCursor) (domain.EventPage, error) {
	if err := ctx.Err(); err != nil {
		return domain.EventPage{}, err
	}
	return runtime.options.Events.After(cursor), nil
}

func (runtime *AgentRuntime) RecentEvents(ctx context.Context, limit int) (domain.EventPage, error) {
	if err := ctx.Err(); err != nil {
		return domain.EventPage{}, err
	}
	return runtime.options.Events.Recent(limit), nil
}

func (runtime *AgentRuntime) SubscribeEvents(cursor domain.EventCursor) <-chan struct{} {
	return runtime.options.Events.Subscribe(cursor)
}

func (runtime *AgentRuntime) Refresh(ctx context.Context) (domain.RefreshReceipt, error) {
	engine := runtime.currentEngine()
	if engine == nil {
		return domain.RefreshReceipt{RequestedAt: time.Now().UTC(), Operations: []string{"poll"}}, ErrAgentRuntimeUnavailable
	}
	return engine.Refresh(ctx)
}

func (runtime *AgentRuntime) SetScheduler(ctx context.Context, enabled bool) error {
	engine := runtime.currentEngine()
	if engine == nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		return ErrAgentRuntimeUnavailable
	}
	return engine.SetScheduler(ctx, enabled)
}

func (runtime *AgentRuntime) Pending() []domain.OperatorRequest { return runtime.broker.Pending() }

func (runtime *AgentRuntime) Respond(ctx context.Context, response domain.OperatorResponse) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return runtime.broker.Respond(response)
}

func (runtime *AgentRuntime) ExtendOperatorRequest(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return runtime.broker.Extend(id)
}

func (runtime *AgentRuntime) Extend(id string) error { return runtime.broker.Extend(id) }

func (runtime *AgentRuntime) OperatorRequests() OperatorRequests { return runtime.broker }

func (runtime *AgentRuntime) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("agent runtime shutdown context is required")
	}
	runtime.mu.Lock()
	if runtime.closed {
		runtime.mu.Unlock()
		return nil
	}
	runtime.closed = true
	cancel := runtime.cancel
	runtime.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	runtime.retireCurrent()
	done := make(chan struct{})
	go func() { runtime.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (runtime *AgentRuntime) currentEngine() *orchestrator.Orchestrator {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.engine
}

func (runtime *AgentRuntime) publish(eventType string, data map[string]any) {
	_, _ = runtime.options.Events.Publish(domain.Event{Type: eventType, Data: data})
}

var _ RuntimeQueries = (*AgentRuntime)(nil)
var _ RuntimeCommands = (*AgentRuntime)(nil)
