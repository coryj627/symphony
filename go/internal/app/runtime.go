package app

import (
	"context"
	"errors"

	"github.com/coryj627/symphony/go/internal/domain"
)

var (
	ErrUnavailableInPhase      = errors.New("unavailable_in_phase")
	ErrAgentRuntimeUnavailable = errors.New("agent_runtime_unavailable")
	ErrIssueNotFound           = errors.New("issue_not_found")
)

type RuntimeQueries interface {
	Snapshot(context.Context) (domain.Snapshot, error)
	Issue(context.Context, string) (domain.IssueDetail, error)
	EventsAfter(context.Context, domain.EventCursor) (domain.EventPage, error)
	RecentEvents(context.Context, int) (domain.EventPage, error)
	SubscribeEvents(domain.EventCursor) <-chan struct{}
}

type RuntimeCommands interface {
	Refresh(context.Context) (domain.RefreshReceipt, error)
	SetScheduler(context.Context, bool) error
	Respond(context.Context, domain.OperatorResponse) error
}
