package orchestrator

import (
	"context"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/workflow"
)

type snapshotResult struct {
	snapshot domain.Snapshot
	err      error
}

type snapshotRequest struct {
	ctx   context.Context
	reply chan snapshotResult
}

type issueResult struct {
	detail domain.IssueDetail
	err    error
}

type issueRequest struct {
	ctx        context.Context
	identifier string
	reply      chan issueResult
}

type eventPageResult struct {
	page domain.EventPage
	err  error
}

type eventsAfterRequest struct {
	ctx    context.Context
	cursor domain.EventCursor
	reply  chan eventPageResult
}

type recentEventsRequest struct {
	ctx   context.Context
	limit int
	reply chan eventPageResult
}

type refreshResult struct {
	receipt domain.RefreshReceipt
	err     error
}

type refreshRequest struct {
	ctx         context.Context
	requestedAt time.Time
	reply       chan refreshResult
}

type schedulerRequest struct {
	ctx     context.Context
	enabled bool
	reply   chan error
}

type pollTick struct{ generation uint64 }

type pollCandidates struct {
	generation uint64
	workflow   workflow.Snapshot
	issues     []domain.Issue
	err        error
}

type pollIssue struct {
	generation uint64
	candidate  domain.Issue
	issues     []domain.Issue
	err        error
}

type workerUpdate struct {
	issueID string
	event   domain.AgentEvent
}

type workerExit struct {
	issueID string
	result  domain.RunResult
}

type configChanged struct{ change workflow.Change }
