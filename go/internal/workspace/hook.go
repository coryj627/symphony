package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
)

var ErrHookShellUnavailable = errors.New("workspace_hook_shell_unavailable")

type ProcessHookRunner struct {
	root         string
	rootIdentity string
	redactor     *observability.Redactor
	logger       *slog.Logger
}

func NewHookRunner(root string, redactor *observability.Redactor, logger *slog.Logger) (*ProcessHookRunner, error) {
	canonicalRoot, rootIdentity, err := prepareRoot(root)
	if err != nil {
		return nil, err
	}
	if redactor == nil {
		redactor = observability.NewRedactor(nil, nil)
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &ProcessHookRunner{root: canonicalRoot, rootIdentity: rootIdentity, redactor: redactor, logger: logger}, nil
}

func (runner *ProcessHookRunner) Run(ctx context.Context, hook domain.Hook, workspace domain.Workspace, timeout time.Duration) HookResult {
	result := HookResult{ExitCode: -1}
	if ctx == nil {
		ctx = context.Background()
	}
	if !knownHook(hook.Name) {
		result.Err = fmt.Errorf("workspace hook name is invalid")
		return result
	}
	if timeout <= 0 {
		result.Err = fmt.Errorf("workspace hook timeout must be positive")
		return result
	}
	if strings.TrimSpace(hook.Script) == "" {
		result.ExitCode = 0
		return result
	}
	if err := runner.validateWorkspace(workspace); err != nil {
		result.Err = err
		return result
	}

	runner.logger.Info("workspace hook starting", "hook", hook.Name, "issue_id", workspace.IssueID, "issue_identifier", workspace.IssueIdentifier)
	output := newBoundedOutput(maxHookOutputBytes)
	runContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	process := runHookProcess(runContext, hookProcessSpec{
		Script: hook.Script, WorkingDir: workspace.Path, Environment: append([]string(nil), os.Environ()...),
	}, output)
	rawOutput, truncated := output.snapshot()
	result.ExitCode = process.ExitCode
	result.TimedOut = process.TimedOut
	result.Output = runner.redactOutput(rawOutput)
	result.Truncated = truncated
	result.Err = process.Err

	attributes := []any{"hook", hook.Name, "issue_id", workspace.IssueID, "issue_identifier", workspace.IssueIdentifier, "exit_code", result.ExitCode, "timed_out", result.TimedOut, "truncated", result.Truncated}
	if result.Err != nil {
		runner.logger.Warn("workspace hook failed", append(attributes, "error", result.Err)...)
	} else {
		runner.logger.Info("workspace hook completed", attributes...)
	}
	return result
}

func (runner *ProcessHookRunner) validateWorkspace(workspace domain.Workspace) error {
	rootInfo, err := os.Lstat(runner.root)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRootIdentity, err)
	}
	reparse, err := pathIsReparse(runner.root, rootInfo)
	if err != nil || reparse || !rootInfo.IsDir() {
		return fmt.Errorf("%w: hook root changed", ErrRootIdentity)
	}
	rootIdentity, err := fileIdentity(runner.root)
	if err != nil || rootIdentity != runner.rootIdentity || workspace.RootIdentity != runner.rootIdentity {
		return fmt.Errorf("%w: hook root identity differs", ErrRootIdentity)
	}
	if workspace.Root != "" && filepath.Clean(workspace.Root) != runner.root {
		return fmt.Errorf("%w: hook workspace belongs to another root", ErrRootIdentity)
	}
	if !pathWithin(runner.root, workspace.Path) {
		return fmt.Errorf("%w: hook working directory is not a child", ErrOutsideRoot)
	}
	if _, err := inspectChildPath(runner.root, workspace.Path); err != nil {
		return err
	}
	identity, err := fileIdentity(workspace.Path)
	if err != nil {
		return fmt.Errorf("identify hook workspace: %w", err)
	}
	if workspace.PathIdentity == "" || identity != workspace.PathIdentity {
		return fmt.Errorf("%w: hook workspace identity differs", ErrAmbiguousPath)
	}
	return nil
}

func (runner *ProcessHookRunner) redactOutput(raw []byte) string {
	text := strings.ToValidUTF8(string(raw), "")
	redacted := runner.redactor.Value(text)
	if output, ok := redacted.(string); ok {
		return output
	}
	return "[UNSAFE VALUE]"
}

func knownHook(name domain.HookName) bool {
	switch name {
	case domain.HookNameAfterCreate, domain.HookNameBeforeRun, domain.HookNameAfterRun, domain.HookNameBeforeRemove:
		return true
	default:
		return false
	}
}

func enforceHookResult(hook domain.Hook, result HookResult) error {
	err := hookResultError(hook, result)
	if err == nil {
		return nil
	}
	switch hook.Name {
	case domain.HookNameAfterRun, domain.HookNameBeforeRemove:
		return nil
	default:
		return err
	}
}

var _ HookRunner = (*ProcessHookRunner)(nil)
