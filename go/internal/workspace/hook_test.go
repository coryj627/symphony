package workspace

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
)

func TestBeforeRunFailureIsFatalAndAfterRunFailureIsWarning(t *testing.T) {
	failed := HookResult{ExitCode: 2, Err: errors.New("exit 2")}
	for _, hook := range []domain.Hook{domain.HookAfterCreate, domain.HookBeforeRun} {
		if err := enforceHookResult(hook, failed); err == nil {
			t.Fatalf("%s failure was not fatal", hook.Name)
		}
	}
	for _, hook := range []domain.Hook{domain.HookAfterRun, domain.HookBeforeRemove} {
		if err := enforceHookResult(hook, failed); err != nil {
			t.Fatalf("%s failure became fatal: %v", hook.Name, err)
		}
	}
}

func TestHookRunnerRejectsRootOutsideAndChangedWorkspaceBeforeLaunch(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, nil)
	workspace, err := manager.Ensure(context.Background(), testIssue("hook", "SYM-HOOK"), withoutHooks(testConfig(root)))
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewHookRunner(root, observability.NewRedactor(nil, nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		workspace domain.Workspace
		want      error
	}{
		{name: "root", workspace: func() domain.Workspace {
			changed := workspace
			changed.Path = runner.root
			changed.PathIdentity = runner.rootIdentity
			return changed
		}(), want: ErrOutsideRoot},
		{name: "outside", workspace: func() domain.Workspace {
			changed := workspace
			changed.Path = t.TempDir()
			return changed
		}(), want: ErrOutsideRoot},
		{name: "changed identity", workspace: func() domain.Workspace {
			moved := workspace.Path + "-original"
			mustRename(t, workspace.Path, moved)
			if err := os.Mkdir(workspace.Path, 0o700); err != nil {
				t.Fatal(err)
			}
			return workspace
		}(), want: ErrAmbiguousPath},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := runner.Run(context.Background(), domain.HookBeforeRun.WithScript("must not execute"), test.workspace, time.Second)
			if !errors.Is(result.Err, test.want) {
				t.Fatalf("Run error = %v, want %v", result.Err, test.want)
			}
		})
	}
}

func TestBoundedOutputAcceptsChildWritesAndMarksTruncation(t *testing.T) {
	writer := newBoundedOutput(8)
	for _, value := range []string{"12345", "67890"} {
		written, err := writer.Write([]byte(value))
		if err != nil || written != len(value) {
			t.Fatalf("Write(%q) = %d, %v", value, written, err)
		}
	}
	contents, truncated := writer.snapshot()
	if string(contents) != "12345678" || !truncated {
		t.Fatalf("snapshot = %q, %t", contents, truncated)
	}
}

func TestHookRunnerRejectsUnknownHookAndNonpositiveTimeout(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, nil)
	workspace, err := manager.Ensure(context.Background(), testIssue("validation", "SYM-VALID"), withoutHooks(testConfig(root)))
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewHookRunner(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		hook    domain.Hook
		timeout time.Duration
	}{
		{name: "unknown", hook: domain.Hook{Script: "ignored"}, timeout: time.Second},
		{name: "zero timeout", hook: domain.HookBeforeRun.WithScript("ignored"), timeout: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := runner.Run(context.Background(), test.hook, workspace, test.timeout)
			if result.Err == nil || !strings.Contains(result.Err.Error(), "hook") {
				t.Fatalf("Run result = %#v", result)
			}
		})
	}
}

func ownedWorkspace(t *testing.T, root, id, identifier string) domain.Workspace {
	t.Helper()
	manager := testManager(t, root, nil)
	workspace, err := manager.Ensure(context.Background(), testIssue(id, identifier), withoutHooks(testConfig(root)))
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func outputLines(output string) []string {
	output = strings.ReplaceAll(output, "\r\n", "\n")
	return strings.Split(strings.TrimSuffix(output, "\n"), "\n")
}
