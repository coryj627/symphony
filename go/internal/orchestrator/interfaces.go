package orchestrator

import (
	"context"
	"log/slog"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

// Worker executes one claimed issue and emits lifecycle updates until it
// returns a terminal result or the supplied context is canceled.
type Worker interface {
	Run(context.Context, RunRequest, func(domain.AgentEvent)) domain.RunResult
}

// EventJournal retains sanitized runtime events. Subscribe returns a signal
// channel; callers fetch event payloads separately with After.
type EventJournal interface {
	Cursor() domain.EventCursor
	Publish(domain.Event) (domain.Event, error)
	After(domain.EventCursor) domain.EventPage
	Recent(int) domain.EventPage
	Subscribe(domain.EventCursor) <-chan struct{}
}

// WorkspaceManager creates and removes only workspaces it can prove it owns.
type WorkspaceManager interface {
	Ensure(context.Context, domain.Issue, workflow.EffectiveConfig) (domain.Workspace, error)
	Remove(context.Context, domain.Issue, workflow.EffectiveConfig) error
}

// Options supplies the single-owner orchestrator's required collaborators.
// Start validates required fields and installs safe Clock and Logger defaults.
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
