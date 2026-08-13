package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/coryj627/symphony/go/internal/buildinfo"
	"github.com/coryj627/symphony/go/internal/codex"
	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/orchestrator"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
	"github.com/coryj627/symphony/go/internal/workspace"
)

// ProductionAgentBuilder returns the native macOS/Windows worker composition.
// It performs an app-server compatibility preflight before returning a worker
// that repeats the handshake for every real issue attempt.
func ProductionAgentBuilder(redactor *observability.Redactor, logger *slog.Logger) AgentRuntimeBuilder {
	return func(ctx context.Context, snapshot workflow.Snapshot, adapter tracker.Adapter, broker *codex.RequestBroker) (AgentRuntimeBuild, error) {
		if redactor == nil {
			redactor = observability.NewRedactor(nil, nil)
		}
		if logger == nil {
			logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		}
		if _, _, err := buildinfo.LoadCodexSchema(); err != nil {
			return AgentRuntimeBuild{}, &AgentPrerequisiteError{Code: "codex_schema_invalid", Message: "The bundled Codex app-server schema failed its integrity check."}
		}
		bashPath, err := codex.FindBash()
		if err != nil {
			return AgentRuntimeBuild{}, &AgentPrerequisiteError{Code: "bash_unavailable", Message: "A native Bash executable is required to launch the Codex app-server."}
		}
		hookRunner, err := workspace.NewHookRunner(snapshot.Config.Workspace.Root, redactor, logger)
		if err != nil {
			return AgentRuntimeBuild{}, &AgentPrerequisiteError{Code: "workspace_root_unavailable", Message: "The configured workspace root is unavailable or unsafe."}
		}
		workspaceManager, err := workspace.New(snapshot.Config.Workspace.Root, hookRunner, logger)
		if err != nil {
			return AgentRuntimeBuild{}, &AgentPrerequisiteError{Code: "workspace_root_unavailable", Message: "The configured workspace root is unavailable or unsafe."}
		}

		preflightPath, err := os.MkdirTemp(snapshot.Config.Workspace.Root, ".symphony-codex-preflight-")
		if err != nil {
			return AgentRuntimeBuild{}, &AgentPrerequisiteError{Code: "workspace_preflight_failed", Message: "Symphony could not allocate a private Codex preflight workspace."}
		}
		_ = os.Chmod(preflightPath, 0o700)
		defer func() {
			if removeErr := os.RemoveAll(preflightPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				logger.Warn("Codex preflight workspace cleanup failed", "error", redactor.Value(removeErr))
			}
		}()
		preflightPath, err = filepath.EvalSymlinks(preflightPath)
		if err != nil {
			return AgentRuntimeBuild{}, &AgentPrerequisiteError{Code: "workspace_preflight_failed", Message: "Symphony could not validate the Codex preflight workspace."}
		}
		rootPath, err := filepath.EvalSymlinks(snapshot.Config.Workspace.Root)
		if err != nil {
			return AgentRuntimeBuild{}, &AgentPrerequisiteError{Code: "workspace_root_unavailable", Message: "The configured workspace root is unavailable or unsafe."}
		}
		provider, err := tracker.DecodeConfig(snapshot.Config.Tracker)
		if err != nil {
			return AgentRuntimeBuild{}, &AgentPrerequisiteError{Code: "tracker_configuration_invalid", Message: "The tracker configuration is invalid. Review Configuration."}
		}
		state := "active"
		if len(snapshot.Config.Tracker.ActiveStates) > 0 && strings.TrimSpace(snapshot.Config.Tracker.ActiveStates[0]) != "" {
			state = snapshot.Config.Tracker.ActiveStates[0]
		}
		preflightIssue := domain.Issue{
			ID: "symphony-codex-preflight", NativeRef: map[string]any{"kind": "preflight"},
			Identifier: "SYMPHONY-PREFLIGHT", Title: "Codex compatibility preflight", State: state,
			Labels: []string{}, BlockedBy: []domain.BlockerRef{}, Dispatchable: true,
		}
		trackerSession, err := tracker.NewSession(preflightIssue, provider)
		if err != nil {
			return AgentRuntimeBuild{}, &AgentPrerequisiteError{Code: "tracker_session_invalid", Message: "The tracker session could not be captured for Codex readiness."}
		}
		runner := codex.ProcessRunner{
			Launch: codex.Launch, Broker: broker, BashPath: bashPath, Redactor: redactor, Logger: logger,
		}
		preflightRequest := codex.RunnerRequest{
			Issue:          preflightIssue,
			Workspace:      domain.Workspace{Path: preflightPath, Root: rootPath, IssueID: preflightIssue.ID, IssueIdentifier: preflightIssue.Identifier},
			TrackerSession: trackerSession, Codex: snapshot.Config.Codex, MaxTurns: 1,
			RequiredLabels: append([]string(nil), snapshot.Config.Tracker.RequiredLabels...),
			SecretNames:    append([]string(nil), adapter.SecretEnvironmentNames()...),
		}
		if err := runner.Preflight(ctx, preflightRequest); err != nil {
			return AgentRuntimeBuild{}, codexPreflightError(err)
		}
		agent := codex.AgentAttempt{Tracker: adapter, Runner: runner, Logger: logger, Redactor: redactor}
		worker := orchestrator.LifecycleWorker{Workspace: workspaceManager, Agent: agent, Logger: logger, Redactor: redactor}
		return AgentRuntimeBuild{Workspace: workspaceManager, Worker: worker}, nil
	}
}

func codexPreflightError(err error) error {
	if errors.Is(err, codex.ErrBashUnavailable) {
		return &AgentPrerequisiteError{Code: "bash_unavailable", Message: "A native Bash executable is required to launch the Codex app-server."}
	}
	var protocolErr *codex.ProtocolError
	if errors.As(err, &protocolErr) {
		switch protocolErr.Code {
		case string(codex.CompatibilityCodeSchemaIntegrity):
			return &AgentPrerequisiteError{Code: "codex_schema_invalid", Message: "The bundled Codex app-server schema failed its integrity check."}
		case string(codex.CompatibilityCodeVersionMismatch), string(codex.CompatibilityCodeUnknownUserAgent):
			return &AgentPrerequisiteError{Code: "codex_version_incompatible", Message: "The installed Codex CLI does not match the reviewed app-server version."}
		}
	}
	return &AgentPrerequisiteError{Code: "codex_preflight_failed", Message: "The Codex app-server could not complete its compatibility preflight. Review the local redacted diagnostics."}
}
