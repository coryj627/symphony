package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/workflow"
)

type HookResult struct {
	ExitCode  int
	TimedOut  bool
	Output    string
	Truncated bool
	Err       error
}

type HookRunner interface {
	Run(context.Context, domain.Hook, domain.Workspace, time.Duration) HookResult
}

type Manager struct {
	mu           sync.Mutex
	root         string
	rootIdentity string
	hooks        HookRunner
	logger       *slog.Logger
}

func New(root string, hooks HookRunner, logger *slog.Logger) (*Manager, error) {
	canonicalRoot, identity, err := prepareRoot(root)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Manager{root: canonicalRoot, rootIdentity: identity, hooks: hooks, logger: logger}, nil
}

func (manager *Manager) Ensure(ctx context.Context, issue domain.Issue, config workflow.EffectiveConfig) (domain.Workspace, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()

	if err := issue.ValidateRequired(); err != nil {
		return domain.Workspace{}, err
	}
	if err := manager.validateConfiguredRoot(config.Workspace.Root); err != nil {
		return domain.Workspace{}, err
	}
	if err := manager.validateRoot(); err != nil {
		return domain.Workspace{}, err
	}
	key, err := Key(issue.Identifier)
	if err != nil {
		return domain.Workspace{}, err
	}
	path := filepath.Join(manager.root, key)
	if !pathWithin(manager.root, path) {
		return domain.Workspace{}, fmt.Errorf("%w: %s", ErrOutsideRoot, path)
	}

	createdNow := false
	info, err := inspectChildPath(manager.root, path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(path, 0o700); err != nil {
			return domain.Workspace{}, fmt.Errorf("create issue workspace: %w", err)
		}
		createdNow = true
		if err := os.Chmod(path, 0o700); err != nil {
			_ = os.Remove(path)
			return domain.Workspace{}, fmt.Errorf("secure issue workspace: %w", err)
		}
		info, err = inspectChildPath(manager.root, path)
	}
	if err != nil {
		return domain.Workspace{}, err
	}
	if err := manager.validateRoot(); err != nil {
		return domain.Workspace{}, err
	}
	pathIdentity, err := fileIdentity(path)
	if err != nil {
		return domain.Workspace{}, fmt.Errorf("identify issue workspace: %w", err)
	}
	_ = info

	workspace := domain.Workspace{
		Path: path, Key: key, CreatedNow: createdNow, Root: manager.root,
		RootIdentity: manager.rootIdentity, PathIdentity: pathIdentity,
		IssueID: issue.ID, IssueIdentifier: issue.Identifier,
	}
	if createdNow {
		marker := markerFor(workspace)
		if err := writeOwnershipMarker(path, marker); err != nil {
			cleanupErr := removeUnmarkedEmptyDirectory(path)
			return domain.Workspace{}, errors.Join(err, cleanupErr)
		}
		workspace.Owned = true
	} else {
		marker, markerErr := readOwnershipMarker(path)
		if errors.Is(markerErr, fs.ErrNotExist) {
			workspace.Owned = false
		} else if markerErr != nil {
			return domain.Workspace{}, markerErr
		} else if err := validateMarker(marker, workspace); err != nil {
			return domain.Workspace{}, err
		} else {
			workspace.Owned = true
		}
	}

	if createdNow && strings.TrimSpace(config.Hooks.AfterCreate) != "" {
		result := manager.runHook(ctx, domain.HookAfterCreate.WithScript(config.Hooks.AfterCreate), workspace, config.Hooks.Timeout)
		if err := hookResultError(domain.HookAfterCreate, result); err != nil {
			cleanupErr := manager.removeFailedCreation(workspace)
			return domain.Workspace{}, errors.Join(err, cleanupErr)
		}
	}
	return workspace, nil
}

func (manager *Manager) Remove(ctx context.Context, issue domain.Issue, config workflow.EffectiveConfig) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()

	if err := issue.ValidateRequired(); err != nil {
		return err
	}
	if err := manager.validateConfiguredRoot(config.Workspace.Root); err != nil {
		return err
	}
	workspace, owned, err := manager.inspectOwnedWorkspace(issue)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !owned {
		return err
	}
	if strings.TrimSpace(config.Hooks.BeforeRemove) != "" {
		result := manager.runHook(ctx, domain.HookBeforeRemove.WithScript(config.Hooks.BeforeRemove), workspace, config.Hooks.Timeout)
		if hookErr := hookResultError(domain.HookBeforeRemove, result); hookErr != nil {
			manager.logger.Warn("workspace hook failed; cleanup will continue", "hook", domain.HookNameBeforeRemove, "issue_id", issue.ID, "issue_identifier", issue.Identifier, "error", hookErr)
		}
	}

	workspace, owned, err = manager.inspectOwnedWorkspace(issue)
	if err != nil || !owned {
		return err
	}
	if err := os.RemoveAll(workspace.Path); err != nil {
		return fmt.Errorf("remove issue workspace: %w", err)
	}
	return nil
}

func (manager *Manager) inspectOwnedWorkspace(issue domain.Issue) (domain.Workspace, bool, error) {
	if err := manager.validateRoot(); err != nil {
		return domain.Workspace{}, false, err
	}
	key, err := Key(issue.Identifier)
	if err != nil {
		return domain.Workspace{}, false, err
	}
	path := filepath.Join(manager.root, key)
	if _, err := inspectChildPath(manager.root, path); err != nil {
		return domain.Workspace{}, false, err
	}
	identity, err := fileIdentity(path)
	if err != nil {
		return domain.Workspace{}, false, err
	}
	workspace := domain.Workspace{
		Path: path, Key: key, Owned: true, Root: manager.root,
		RootIdentity: manager.rootIdentity, PathIdentity: identity,
		IssueID: issue.ID, IssueIdentifier: issue.Identifier,
	}
	marker, err := readOwnershipMarker(path)
	if errors.Is(err, fs.ErrNotExist) {
		workspace.Owned = false
		return workspace, false, nil
	}
	if err != nil {
		return domain.Workspace{}, false, err
	}
	if err := validateMarker(marker, workspace); err != nil {
		return domain.Workspace{}, false, err
	}
	return workspace, true, nil
}

func (manager *Manager) validateConfiguredRoot(configured string) error {
	if strings.TrimSpace(configured) == "" {
		return nil
	}
	resolved, err := filepath.Abs(configured)
	if err != nil {
		return fmt.Errorf("resolve configured workspace root: %w", err)
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return fmt.Errorf("%w: resolve configured root: %v", ErrRootIdentity, err)
	}
	resolved, err = canonicalExistingPath(resolved)
	if err != nil {
		return fmt.Errorf("%w: resolve configured root: %v", ErrRootIdentity, err)
	}
	if filepath.Clean(resolved) != manager.root {
		return fmt.Errorf("%w: configured root changed", ErrRootIdentity)
	}
	return nil
}

func (manager *Manager) validateRoot() error {
	info, err := os.Lstat(manager.root)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRootIdentity, err)
	}
	reparse, err := pathIsReparse(manager.root, info)
	if err != nil || reparse || !info.IsDir() {
		return fmt.Errorf("%w: root path is no longer the original directory", ErrRootIdentity)
	}
	identity, err := fileIdentity(manager.root)
	if err != nil || identity != manager.rootIdentity {
		return fmt.Errorf("%w: root identity no longer matches", ErrRootIdentity)
	}
	return nil
}

func (manager *Manager) runHook(ctx context.Context, hook domain.Hook, workspace domain.Workspace, timeout time.Duration) HookResult {
	if manager.hooks == nil {
		return HookResult{ExitCode: -1, Err: errors.New("workspace hook runner is unavailable")}
	}
	return manager.hooks.Run(ctx, hook, workspace, timeout)
}

func (manager *Manager) RunHook(ctx context.Context, hook domain.Hook, workspace domain.Workspace, timeout time.Duration) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()

	if !knownHook(hook.Name) {
		return errors.New("workspace hook name is invalid")
	}
	if err := manager.validateRoot(); err != nil {
		return err
	}
	if workspace.RootIdentity != manager.rootIdentity || filepath.Clean(workspace.Root) != manager.root {
		return fmt.Errorf("%w: hook workspace belongs to another root", ErrRootIdentity)
	}
	if !pathWithin(manager.root, workspace.Path) {
		return fmt.Errorf("%w: hook workspace is not a child", ErrOutsideRoot)
	}
	if _, err := inspectChildPath(manager.root, workspace.Path); err != nil {
		return err
	}
	identity, err := fileIdentity(workspace.Path)
	if err != nil {
		return fmt.Errorf("identify hook workspace: %w", err)
	}
	if workspace.PathIdentity == "" || identity != workspace.PathIdentity {
		return fmt.Errorf("%w: hook workspace identity differs", ErrAmbiguousPath)
	}
	if strings.TrimSpace(hook.Script) == "" {
		return nil
	}
	return hookResultError(hook, manager.runHook(ctx, hook, workspace, timeout))
}

func (manager *Manager) removeFailedCreation(workspace domain.Workspace) error {
	current, owned, err := manager.inspectOwnedWorkspace(issueForWorkspace(workspace))
	if err != nil || !owned || current.PathIdentity != workspace.PathIdentity {
		if err == nil {
			err = fmt.Errorf("%w: failed workspace identity changed", ErrAmbiguousPath)
		}
		return err
	}
	entries, err := os.ReadDir(current.Path)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != markerFilename {
		return fmt.Errorf("%w: failed workspace is not empty", ErrAmbiguousPath)
	}
	if err := os.Remove(filepath.Join(current.Path, markerFilename)); err != nil {
		return err
	}
	if err := os.Remove(current.Path); err != nil {
		return err
	}
	return nil
}

func markerFor(workspace domain.Workspace) ownershipMarker {
	return ownershipMarker{
		Version: markerVersion, IssueID: workspace.IssueID, Identifier: workspace.IssueIdentifier,
		Key: workspace.Key, RootIdentity: workspace.RootIdentity, WorkspaceIdentity: workspace.PathIdentity,
	}
}

func validateMarker(marker ownershipMarker, workspace domain.Workspace) error {
	if marker.RootIdentity != workspace.RootIdentity {
		return fmt.Errorf("%w: marker root identity differs", ErrRootIdentity)
	}
	if marker.IssueID != workspace.IssueID || marker.Identifier != workspace.IssueIdentifier || marker.Key != workspace.Key {
		return fmt.Errorf("%w: workspace is owned by another issue", ErrWorkspaceKeyCollision)
	}
	if marker.WorkspaceIdentity != workspace.PathIdentity {
		return fmt.Errorf("%w: workspace identity differs", ErrAmbiguousPath)
	}
	return nil
}

func hookResultError(hook domain.Hook, result HookResult) error {
	if result.Err == nil && !result.TimedOut && result.ExitCode == 0 {
		return nil
	}
	cause := result.Err
	if cause == nil {
		cause = errors.New("nonzero exit or timeout")
	}
	return fmt.Errorf("workspace hook %s failed: exit_code=%d timed_out=%t: %w", hook.Name, result.ExitCode, result.TimedOut, cause)
}

func removeUnmarkedEmptyDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("%w: unmarked workspace is not empty", ErrAmbiguousPath)
	}
	return os.Remove(path)
}

func issueForWorkspace(workspace domain.Workspace) domain.Issue {
	return domain.Issue{ID: workspace.IssueID, Identifier: workspace.IssueIdentifier, Title: "workspace cleanup", State: "cleanup"}
}
