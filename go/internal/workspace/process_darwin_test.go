//go:build darwin

package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
)

func TestHookProcessPreservesScriptCWDEnvironmentAndRedactsOutput(t *testing.T) {
	const canary = "hook-secret-canary-0123456789"
	t.Setenv("SYMPHONY_HOOK_TEST_VALUE", "ordinary-value")
	root := t.TempDir()
	workspace := ownedWorkspace(t, root, "darwin", "SYM-DARWIN")
	redactor := observability.NewRedactor(nil, nil)
	redactor.RegisterSecret([]byte(canary))
	runner, err := NewHookRunner(root, redactor, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	script := `printf '%s\n' 'quotes "double" $dollar café'
printf 'cwd=%s\n' "$PWD"
printf 'env=%s\n' "$SYMPHONY_HOOK_TEST_VALUE"
printf 'canary=%s\n' '` + canary + `'`

	result := runner.Run(context.Background(), domain.HookBeforeRun.WithScript(script), workspace, time.Second)
	if result.Err != nil || result.ExitCode != 0 || result.TimedOut || result.Truncated {
		t.Fatalf("Run result = %#v", result)
	}
	want := []string{
		`quotes "double" $dollar café`,
		"cwd=" + workspace.Path,
		"env=ordinary-value",
		"canary=[REDACTED]",
	}
	if got := outputLines(result.Output); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("output = %#v, want %#v", got, want)
	}
	if strings.Contains(result.Output, canary) {
		t.Fatal("hook output retained secret canary")
	}
}

func TestHookProcessReturnsExitCodeAndCapsCombinedOutput(t *testing.T) {
	root := t.TempDir()
	workspace := ownedWorkspace(t, root, "output", "SYM-OUTPUT")
	runner, err := NewHookRunner(root, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}

	failure := runner.Run(context.Background(), domain.HookBeforeRun.WithScript("printf 'before-failure\\n'; exit 7"), workspace, time.Second)
	if failure.ExitCode != 7 || failure.Err == nil || failure.Output != "before-failure\n" {
		t.Fatalf("failure result = %#v", failure)
	}

	large := runner.Run(context.Background(), domain.HookBeforeRun.WithScript("head -c 1100000 /dev/zero | tr '\\000' x"), workspace, 5*time.Second)
	if large.Err != nil || !large.Truncated || len(large.Output) > maxHookOutputBytes || !strings.HasPrefix(large.Output, "xxxx") {
		t.Fatalf("large result: exit=%d timeout=%t truncated=%t len=%d err=%v", large.ExitCode, large.TimedOut, large.Truncated, len(large.Output), large.Err)
	}
}

func TestHookTimeoutKillsDescendantProcessGroup(t *testing.T) {
	root := t.TempDir()
	workspace := ownedWorkspace(t, root, "timeout", "SYM-TIMEOUT")
	runner, err := NewHookRunner(root, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	script := "sleep 30 & child=$!; printf 'child=%s\\n' \"$child\"; wait"
	result := runner.Run(context.Background(), domain.HookBeforeRun.WithScript(script), workspace, 100*time.Millisecond)
	if !result.TimedOut || result.Err == nil {
		t.Fatalf("timeout result = %#v", result)
	}
	lines := outputLines(result.Output)
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "child=") {
		t.Fatalf("missing descendant PID: %q", result.Output)
	}
	pid, err := strconv.Atoi(strings.TrimPrefix(lines[0], "child="))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant process %d survived timeout: %v", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHookCancellationIsReportedAsTimedOut(t *testing.T) {
	root := t.TempDir()
	workspace := ownedWorkspace(t, root, "cancel", "SYM-CANCEL")
	runner, err := NewHookRunner(root, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := runner.Run(ctx, domain.HookBeforeRun.WithScript("sleep 30"), workspace, time.Second)
	if !result.TimedOut || !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("canceled result = %#v", result)
	}
	if result.ExitCode == 0 {
		t.Fatal("canceled process reported success")
	}
}

func TestManagerRunsRealAfterCreateAndBeforeRemoveHooks(t *testing.T) {
	root := t.TempDir()
	runner, err := NewHookRunner(root, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(root, runner, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	issue := testIssue("lifecycle", "SYM-LIFECYCLE")
	config := testConfig(root)
	config.Hooks.AfterCreate = "printf created > hook-created"
	config.Hooks.BeforeRemove = "printf removed > ../before-remove-ran"
	workspace, err := manager.Ensure(context.Background(), issue, config)
	if err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(filepath.Join(workspace.Path, "hook-created")); err != nil || string(contents) != "created" {
		t.Fatalf("after_create output = %q, %v", contents, err)
	}
	if err := manager.Remove(context.Background(), issue, config); err != nil {
		t.Fatal(err)
	}
	assertMissing(t, workspace.Path)
	if contents, err := os.ReadFile(filepath.Join(root, "before-remove-ran")); err != nil || string(contents) != "removed" {
		t.Fatalf("before_remove output = %q, %v", contents, err)
	}
}
