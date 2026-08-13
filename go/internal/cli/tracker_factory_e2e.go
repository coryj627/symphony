//go:build codex_e2e

package cli

import (
	"context"
	"log/slog"
	"os"
	"sync"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/secrets"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

const e2eCodexTrackerEnabled = "SYMPHONY_E2E_CODEX_TRACKER"

var e2eCodexSecretNames = []string{
	"SYMPHONY_E2E_SECRET_CANARY",
	"SYMPHONY_GITHUB_TEST_TOKEN",
	"SYMPHONY_LINEAR_TEST_TOKEN",
}

func newTrackerFactory(workflowID string, lookup workflow.LookupEnv, redactor *observability.Redactor, logger *slog.Logger) tracker.Factory {
	if os.Getenv(e2eCodexTrackerEnabled) != "1" {
		return newProductionTrackerFactory(workflowID, lookup, redactor, logger)
	}
	if redactor != nil {
		redactor.RegisterEnvironmentNames(e2eCodexSecretNames, observability.LookupEnv(lookup))
	}
	return &e2eCodexTrackerFactory{}
}

type e2eCodexTrackerFactory struct{}

func (*e2eCodexTrackerFactory) Build(ctx context.Context, _ workflow.TrackerConfig, _ secrets.Resolver) (tracker.Adapter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &e2eCodexTrackerAdapter{}, nil
}

type e2eCodexTrackerAdapter struct {
	mu            sync.Mutex
	toolExecuted  bool
	postToolReads int
}

func (*e2eCodexTrackerAdapter) Kind() string { return "github" }

func (adapter *e2eCodexTrackerAdapter) FetchIssuesByStates(ctx context.Context, _ []string) ([]domain.Issue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	adapter.mu.Lock()
	terminal := adapter.toolExecuted && adapter.postToolReads >= 2
	adapter.mu.Unlock()
	if terminal {
		return []domain.Issue{}, nil
	}
	return []domain.Issue{e2eCodexIssue("open")}, nil
}

func (adapter *e2eCodexTrackerAdapter) FetchIssuesByIDs(ctx context.Context, ids []string) ([]domain.Issue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ids) != 1 || ids[0] != "fake-gh-42" {
		return []domain.Issue{}, nil
	}
	adapter.mu.Lock()
	if adapter.toolExecuted {
		adapter.postToolReads++
	}
	terminal := adapter.toolExecuted && adapter.postToolReads >= 2
	adapter.mu.Unlock()
	state := "open"
	if terminal {
		state = "closed"
	}
	return []domain.Issue{e2eCodexIssue(state)}, nil
}

func (*e2eCodexTrackerAdapter) AgentTools(session tracker.Session) []domain.ToolSpec {
	if session.Issue.ID != "fake-gh-42" {
		return []domain.ToolSpec{}
	}
	return []domain.ToolSpec{{
		Name: "github_api", Description: "Deterministic issue-scoped e2e tool.",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{"operation": map[string]any{"const": "get_issue"}},
			"required":   []any{"operation"},
		},
	}}
}

func (adapter *e2eCodexTrackerAdapter) ExecuteAgentTool(ctx context.Context, call domain.ToolCall, session tracker.Session) domain.ToolResult {
	if err := ctx.Err(); err != nil {
		return domain.ToolFailure("canceled", "The deterministic provider tool was canceled.")
	}
	arguments, ok := call.Arguments.(map[string]any)
	if !ok || call.Name != "github_api" || session.Issue.ID != "fake-gh-42" || arguments["operation"] != "get_issue" {
		return domain.ToolUnavailableResult()
	}
	adapter.mu.Lock()
	adapter.toolExecuted = true
	adapter.mu.Unlock()
	result, err := domain.ToolSuccess(map[string]any{"fake_tool": "executed", "issue_id": session.Issue.ID})
	if err != nil {
		return domain.ToolFailure("fixture_failure", "The deterministic provider tool failed.")
	}
	return result
}

func (*e2eCodexTrackerAdapter) SecretEnvironmentNames() []string {
	return append([]string(nil), e2eCodexSecretNames...)
}

func e2eCodexIssue(state string) domain.Issue {
	return domain.Issue{
		ID: "fake-gh-42", Identifier: "GH-42", Title: "Full Codex process integration", State: state,
		Labels: []string{"ready"}, BlockedBy: []domain.BlockerRef{}, Dispatchable: true,
		NativeRef: map[string]any{"owner": "fixture", "repository": "fixture", "number": 42},
	}
}

var _ tracker.Factory = (*e2eCodexTrackerFactory)(nil)
var _ tracker.Adapter = (*e2eCodexTrackerAdapter)(nil)
