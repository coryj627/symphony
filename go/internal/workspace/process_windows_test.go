//go:build windows

package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
)

func TestEnvironmentBlockDeduplicatesCaseInsensitiveKeysKeepingLastValue(t *testing.T) {
	block, err := environmentBlock([]string{"Path=C:\\first", "ORDINARY=first", "PATH=C:\\second", "ordinary=second"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(utf16.Decode(block)), "ordinary=second\x00PATH=C:\\second\x00\x00"; got != want {
		t.Fatalf("environment block = %q, want %q", got, want)
	}
}

func TestHookProcessUsesPowerShellStdinCWDEnvironmentAndRedactsOutput(t *testing.T) {
	const canary = "hook-secret-canary-0123456789"
	t.Setenv("SYMPHONY_HOOK_TEST_VALUE", "ordinary-value")
	root := t.TempDir()
	workspace := ownedWorkspace(t, root, "windows", "SYM-WINDOWS")
	redactor := observability.NewRedactor(nil, nil)
	redactor.RegisterSecret([]byte(canary))
	runner, err := NewHookRunner(root, redactor, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	script := `Write-Output 'quotes "double" $dollar café'
Write-Output ('cwd=' + (Get-Location).Path)
Write-Output ('env=' + $env:SYMPHONY_HOOK_TEST_VALUE)
Write-Output ('canary=' + '` + canary + `')`

	result := runner.Run(context.Background(), domain.HookBeforeRun.WithScript(script), workspace, 5*time.Second)
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

func TestHookProcessCapsPowerShellOutputAndTimesOutJob(t *testing.T) {
	root := t.TempDir()
	workspace := ownedWorkspace(t, root, "windows-timeout", "SYM-WINDOWS-TIMEOUT")
	runner, err := NewHookRunner(root, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	large := runner.Run(context.Background(), domain.HookBeforeRun.WithScript(`Write-Output ('x' * 1100000)`), workspace, 10*time.Second)
	if large.Err != nil || !large.Truncated || len(large.Output) > maxHookOutputBytes {
		t.Fatalf("large result: %#v", large)
	}
	marker := filepath.Join(workspace.Path, "descendant-survived")
	escapedMarker := strings.ReplaceAll(marker, `'`, `''`)
	descendant := `Start-Sleep -Seconds 1; [IO.File]::WriteAllText('` + escapedMarker + `','survived')`
	timeoutScript := `Start-Process powershell.exe -ArgumentList @('-NoProfile','-Command',"` + strings.ReplaceAll(descendant, `"`, `\"`) + `"); Start-Sleep 30`
	timed := runner.Run(context.Background(), domain.HookBeforeRun.WithScript(timeoutScript), workspace, 100*time.Millisecond)
	if !timed.TimedOut || timed.Err == nil {
		t.Fatalf("timeout result = %#v", timed)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("job descendant survived timeout: %v", err)
	}
}
