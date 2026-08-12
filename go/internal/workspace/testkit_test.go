package workspace

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/workflow"
)

type fakeHookRunner struct {
	mu      sync.Mutex
	calls   []domain.Hook
	results map[domain.Hook]HookResult
	before  func(domain.Hook, domain.Workspace)
}

func (runner *fakeHookRunner) Run(_ context.Context, hook domain.Hook, workspace domain.Workspace, _ time.Duration) HookResult {
	runner.mu.Lock()
	runner.calls = append(runner.calls, hook)
	before := runner.before
	result, found := runner.results[hook]
	if !found {
		for configuredHook, configuredResult := range runner.results {
			if configuredHook.Name == hook.Name {
				result = configuredResult
				break
			}
		}
	}
	runner.mu.Unlock()
	if before != nil {
		before(hook, workspace)
	}
	return result
}

func (runner *fakeHookRunner) count(hook domain.Hook) int {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	count := 0
	for _, called := range runner.calls {
		if called.Name == hook.Name {
			count++
		}
	}
	return count
}

func testManager(t *testing.T, root string, hooks HookRunner) *Manager {
	t.Helper()
	manager, err := New(root, hooks, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func testIssue(id, identifier string) domain.Issue {
	return domain.Issue{ID: id, Identifier: identifier, Title: "Workspace test", State: "open", Dispatchable: true}
}

func testConfig(root string) workflow.EffectiveConfig {
	return workflow.EffectiveConfig{
		Workspace: workflow.WorkspaceConfig{Root: root},
		Hooks:     workflow.HooksConfig{AfterCreate: "prepare", BeforeRemove: "cleanup", Timeout: time.Second},
	}
}

func mustSymlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%q should exist: %v", path, err)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%q should be missing: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustRename(t *testing.T, oldPath, newPath string) {
	t.Helper()
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
}

func withoutHooks(config workflow.EffectiveConfig) workflow.EffectiveConfig {
	config.Hooks.AfterCreate = ""
	config.Hooks.BeforeRemove = ""
	return config
}

var _ HookRunner = (*fakeHookRunner)(nil)

func contextForTest(t *testing.T) context.Context {
	t.Helper()
	return context.Background()
}

func childPath(root, identifier string) string {
	key, _ := Key(identifier)
	return filepath.Join(root, key)
}
