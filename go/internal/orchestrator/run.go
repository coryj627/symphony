package orchestrator

import (
	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/workflow"
)

type RunRequest struct {
	Issue    domain.Issue
	Attempt  *int
	Workflow workflow.Snapshot
}
