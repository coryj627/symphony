package orchestrator

import (
	"context"
	"log/slog"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

type Worker interface {
	Run(context.Context, RunRequest, func(domain.AgentEvent)) domain.RunResult
}

type EventJournal interface {
	Cursor() domain.EventCursor
	Publish(domain.Event) (domain.Event, error)
	After(domain.EventCursor) domain.EventPage
	Recent(int) domain.EventPage
	Subscribe(domain.EventCursor) <-chan struct{}
}

type WorkspaceManager interface {
	Ensure(context.Context, domain.Issue, workflow.EffectiveConfig) (domain.Workspace, error)
	Remove(context.Context, domain.Issue, workflow.EffectiveConfig) error
}

type Options struct {
	Tracker         tracker.Adapter
	Workflow        workflow.Store
	Workspace       WorkspaceManager
	Worker          Worker
	Events          EventJournal
	Logger          *slog.Logger
	Clock           Clock
	InitiallyPaused bool
}
