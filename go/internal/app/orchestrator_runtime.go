package app

import (
	"context"
	"errors"
	"sync"

	"github.com/coryj627/symphony/go/internal/domain"
)

const phase3UnavailableMessage = "Agent runtime will be enabled in Phase 4."

type OrchestratorEngine interface {
	RuntimeQueries
	RuntimeCommands
}

type OrchestratorRuntimeOptions struct {
	Engine     OrchestratorEngine
	AgentReady bool
}

type OrchestratorRuntime struct {
	engine     OrchestratorEngine
	agentReady bool
	controlMu  sync.Mutex
}

func NewOrchestratorRuntime(options OrchestratorRuntimeOptions) (*OrchestratorRuntime, error) {
	if options.Engine == nil {
		return nil, errors.New("orchestrator runtime engine is required")
	}
	return &OrchestratorRuntime{engine: options.Engine, agentReady: options.AgentReady}, nil
}

func (runtime *OrchestratorRuntime) Snapshot(ctx context.Context) (domain.Snapshot, error) {
	snapshot, err := runtime.engine.Snapshot(ctx)
	if err != nil {
		return domain.Snapshot{}, err
	}
	if !runtime.agentReady {
		snapshot.Scheduler = domain.SchedulerStatus{
			Available: false, Enabled: false, State: "unavailable", Message: phase3UnavailableMessage,
		}
	}
	return snapshot.Clone()
}

func (runtime *OrchestratorRuntime) Issue(ctx context.Context, identifier string) (domain.IssueDetail, error) {
	return runtime.engine.Issue(ctx, identifier)
}

func (runtime *OrchestratorRuntime) EventsAfter(ctx context.Context, cursor domain.EventCursor) (domain.EventPage, error) {
	return runtime.engine.EventsAfter(ctx, cursor)
}

func (runtime *OrchestratorRuntime) RecentEvents(ctx context.Context, limit int) (domain.EventPage, error) {
	return runtime.engine.RecentEvents(ctx, limit)
}

func (runtime *OrchestratorRuntime) SubscribeEvents(cursor domain.EventCursor) <-chan struct{} {
	return runtime.engine.SubscribeEvents(cursor)
}

func (runtime *OrchestratorRuntime) Refresh(ctx context.Context) (domain.RefreshReceipt, error) {
	return runtime.engine.Refresh(ctx)
}

func (runtime *OrchestratorRuntime) SetScheduler(ctx context.Context, enabled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !runtime.agentReady {
		return ErrAgentRuntimeUnavailable
	}
	runtime.controlMu.Lock()
	defer runtime.controlMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return runtime.engine.SetScheduler(ctx, enabled)
}

func (runtime *OrchestratorRuntime) Respond(ctx context.Context, response domain.OperatorResponse) error {
	return runtime.engine.Respond(ctx, response)
}

var _ RuntimeQueries = (*OrchestratorRuntime)(nil)
var _ RuntimeCommands = (*OrchestratorRuntime)(nil)
