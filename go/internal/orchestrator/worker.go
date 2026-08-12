package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/workflow"
)

type LifecycleWorkspace interface {
	Ensure(context.Context, domain.Issue, workflow.EffectiveConfig) (domain.Workspace, error)
	RunHook(context.Context, domain.Hook, domain.Workspace, time.Duration) error
}

type AgentAttempt interface {
	Run(context.Context, AgentAttemptRequest, func(domain.AgentEvent)) domain.RunResult
}

type AgentAttemptFunc func(context.Context, AgentAttemptRequest, func(domain.AgentEvent)) domain.RunResult

func (run AgentAttemptFunc) Run(ctx context.Context, request AgentAttemptRequest, emit func(domain.AgentEvent)) domain.RunResult {
	return run(ctx, request, emit)
}

type AgentAttemptRequest struct {
	Issue     domain.Issue
	Attempt   *int
	Workspace domain.Workspace
	Workflow  workflow.Snapshot
	Prompt    string
}

type WorkerFunc func(context.Context, RunRequest, func(domain.AgentEvent)) domain.RunResult

func (run WorkerFunc) Run(ctx context.Context, request RunRequest, emit func(domain.AgentEvent)) domain.RunResult {
	return run(ctx, request, emit)
}

type LifecycleWorker struct {
	Workspace LifecycleWorkspace
	Agent     AgentAttempt
	Clock     Clock
	Logger    *slog.Logger
	Redactor  *observability.Redactor
}

func (worker LifecycleWorker) Run(ctx context.Context, request RunRequest, emit func(domain.AgentEvent)) (result domain.RunResult) {
	clock := worker.Clock
	if clock == nil {
		clock = RealClock{}
	}
	logger := worker.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	redactor := worker.Redactor
	if redactor == nil {
		redactor = observability.NewRedactor(nil, nil)
	}
	if emit == nil {
		emit = func(domain.AgentEvent) {}
	}
	emitSafe := func(event domain.AgentEvent) {
		if event.At.IsZero() {
			event.At = clock.Now().UTC()
		} else {
			event.At = event.At.UTC()
		}
		emit(cloneAgentEvent(event))
	}
	finish := func(candidate domain.RunResult) domain.RunResult {
		if candidate.EndedAt.IsZero() {
			candidate.EndedAt = clock.Now().UTC()
		} else {
			candidate.EndedAt = candidate.EndedAt.UTC()
		}
		return candidate
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			stack := redactor.Value(string(debug.Stack()))
			logger.Error("worker panic contained", "issue_id", request.Issue.ID, "issue_identifier", request.Issue.Identifier, "stack", stack)
			result = finish(domain.RunResult{
				Reason: domain.StopReasonFailed, ErrorCode: "worker_panic", ErrorMessage: "The worker failed unexpectedly.",
			})
		}
	}()
	if worker.Workspace == nil || worker.Agent == nil {
		return finish(domain.RunResult{Reason: domain.StopReasonFailed, ErrorCode: "worker_unavailable", ErrorMessage: "The worker is not configured."})
	}

	emitSafe(domain.AgentEvent{Type: string(domain.RunStatusPreparingWorkspace)})
	workspace, err := worker.Workspace.Ensure(ctx, request.Issue, request.Workflow.Config)
	if err != nil {
		logger.Warn("workspace preparation failed", "issue_id", request.Issue.ID, "issue_identifier", request.Issue.Identifier, "error", redactor.Value(err))
		return finish(domain.RunResult{Reason: domain.StopReasonFailed, ErrorCode: "workspace_failed", ErrorMessage: "The issue workspace could not be prepared."})
	}
	defer func() {
		hook := domain.HookAfterRun.WithScript(request.Workflow.Config.Hooks.AfterRun)
		if err := worker.Workspace.RunHook(context.WithoutCancel(ctx), hook, workspace, request.Workflow.Config.Hooks.Timeout); err != nil {
			logger.Warn("workspace after-run hook failed", "hook", hook.Name, "issue_id", request.Issue.ID, "issue_identifier", request.Issue.Identifier, "error", redactor.Value(err))
		}
	}()

	emitSafe(domain.AgentEvent{Type: string(domain.RunStatusBuildingPrompt)})
	prompt, err := workflow.Render(request.Workflow.Definition, templateIssue(request.Issue), request.Attempt)
	if err != nil {
		logger.Warn("prompt construction failed", "issue_id", request.Issue.ID, "issue_identifier", request.Issue.Identifier, "error", redactor.Value(err))
		return finish(domain.RunResult{Reason: domain.StopReasonFailed, ErrorCode: "prompt_failed", ErrorMessage: "The issue prompt could not be built."})
	}
	beforeRun := domain.HookBeforeRun.WithScript(request.Workflow.Config.Hooks.BeforeRun)
	if err := worker.Workspace.RunHook(ctx, beforeRun, workspace, request.Workflow.Config.Hooks.Timeout); err != nil {
		logger.Warn("workspace before-run hook failed", "hook", beforeRun.Name, "issue_id", request.Issue.ID, "issue_identifier", request.Issue.Identifier, "error", redactor.Value(err))
		return finish(domain.RunResult{Reason: domain.StopReasonFailed, ErrorCode: "before_run_failed", ErrorMessage: "The before-run hook failed."})
	}
	if err := ctx.Err(); err != nil {
		return finish(domain.RunResult{Reason: domain.StopReasonOperatorStop, ErrorCode: "run_canceled", ErrorMessage: "The run was canceled."})
	}
	emitSafe(domain.AgentEvent{Type: string(domain.RunStatusLaunchingAgent)})
	result = worker.Agent.Run(ctx, AgentAttemptRequest{
		Issue: request.Issue, Attempt: cloneAttempt(request.Attempt), Workspace: workspace,
		Workflow: request.Workflow, Prompt: prompt,
	}, emitSafe)
	return finish(result)
}

func templateIssue(issue domain.Issue) workflow.TemplateIssue {
	blockers := make([]map[string]any, len(issue.BlockedBy))
	for index, blocker := range issue.BlockedBy {
		blockers[index] = map[string]any{"id": blocker.ID, "identifier": blocker.Identifier, "state": blocker.State}
	}
	return workflow.TemplateIssue{
		ID: issue.ID, NativeRef: cloneMap(issue.NativeRef), Identifier: issue.Identifier, Title: issue.Title,
		Description: cloneStringPointer(issue.Description), Priority: cloneIntPointer(issue.Priority), State: issue.State,
		BranchName: cloneStringPointer(issue.BranchName), URL: cloneStringPointer(issue.URL), AssigneeID: cloneStringPointer(issue.AssigneeID),
		Labels: append([]string(nil), issue.Labels...), BlockedBy: blockers, Dispatchable: issue.Dispatchable,
		CreatedAt: cloneTimePointer(issue.CreatedAt), UpdatedAt: cloneTimePointer(issue.UpdatedAt),
	}
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
