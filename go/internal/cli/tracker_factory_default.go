//go:build !codex_e2e

package cli

import (
	"log/slog"

	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

func newTrackerFactory(workflowID string, lookup workflow.LookupEnv, redactor *observability.Redactor, logger *slog.Logger) tracker.Factory {
	return newProductionTrackerFactory(workflowID, lookup, redactor, logger)
}
