//go:build windows

package codex

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestProcessIsAssignedToKillOnCloseJobBeforeLaunchReturns(t *testing.T) {
	process, _ := launchWindowsTestProcessTree(t)
	defer process.Stop(context.Background())
	child, ok := process.(*childProcess)
	if !ok {
		t.Fatalf("process type = %T", process)
	}
	backend, ok := child.backend.(*windowsProcess)
	if !ok || !backend.assigned || backend.job == 0 {
		t.Fatalf("Windows containment was not established: %#v", child.backend)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	if err := windows.QueryInformationJobObject(
		backend.job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if limits.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 {
		t.Fatalf("job limit flags = %#x", limits.BasicLimitInformation.LimitFlags)
	}
}

func TestProcessStopTerminatesWindowsDescendantsAndNotUnrelatedProcess(t *testing.T) {
	process, pidFile := launchWindowsTestProcessTree(t)
	pids := waitForWindowsPIDFile(t, pidFile)
	unrelated := exec.Command(os.Args[0], "-test.run=^TestCodexProcessHelper$")
	unrelated.Env = replaceEnvironmentValue(os.Environ(), "SYMPHONY_CODEX_PROCESS_HELPER", "leaf")
	var unrelatedStderr strings.Builder
	unrelated.Stderr = &unrelatedStderr
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	var unrelatedWaitErr error
	unrelatedDone := make(chan struct{})
	go func() {
		unrelatedWaitErr = unrelated.Wait()
		close(unrelatedDone)
	}()
	t.Cleanup(func() {
		_ = unrelated.Process.Kill()
		<-unrelatedDone
	})
	if err := process.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, pid := range pids {
		waitForWindowsProcessExit(t, pid)
	}
	select {
	case <-unrelatedDone:
		t.Fatalf("unrelated process exited: %v; stderr=%q", unrelatedWaitErr, unrelatedStderr.String())
	default:
	}
}

func TestWindowsLaunchPreservesQuoteOnlyWorkflowCommand(t *testing.T) {
	bash, err := FindBash()
	if err != nil {
		t.Fatal(err)
	}
	process, err := Launch(t.Context(), LaunchOptions{
		Cwd:      canonicalTestDirectory(t),
		Command:  `"true"`,
		BashPath: bash,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Stop(context.Background()) })
	waitContext, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := process.Wait(waitContext); err != nil {
		t.Fatalf("quote-only workflow command failed: %v; diagnostic=%q", err, process.Diagnostic())
	}
}

func TestWindowsBashCommandLineAlwaysQuotesWorkflowCommand(t *testing.T) {
	spec := BashCommand(`C:\Program Files\Git\bin\bash.exe`, `"C:/fixture/fake.exe"`)
	got := composeWindowsBashCommandLine(spec)
	want := `"C:\Program Files\Git\bin\bash.exe" -lc "\"C:/fixture/fake.exe\""`
	if got != want {
		t.Fatalf("composeWindowsBashCommandLine() = %q, want %q", got, want)
	}
}

func launchWindowsTestProcessTree(t *testing.T) (Process, string) {
	t.Helper()
	bash, err := FindBash()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := canonicalTestDirectory(t) + `\pids`
	environment := replaceEnvironmentValue(os.Environ(), "SYMPHONY_CODEX_PROCESS_HELPER", "tree")
	environment = replaceEnvironmentValue(environment, "SYMPHONY_CODEX_PID_FILE", pidFile)
	environment = replaceEnvironmentValue(environment, "SYMPHONY_CODEX_TEST_EXE", os.Args[0])
	process, err := Launch(t.Context(), LaunchOptions{
		Cwd:      canonicalTestDirectory(t),
		Command:  `exec "$SYMPHONY_CODEX_TEST_EXE" -test.run=^TestCodexProcessHelper$`,
		BashPath: bash, Environment: environment,
		GracePeriod: 100 * time.Millisecond, ForcePeriod: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return process, pidFile
}

func waitForWindowsPIDFile(t *testing.T, path string) []int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Fields(string(raw))
			if len(lines) == 2 {
				pids := make([]int, 0, 2)
				for _, line := range lines {
					pid, conversionErr := strconv.Atoi(line)
					if conversionErr != nil {
						t.Fatal(conversionErr)
					}
					pids = append(pids, pid)
				}
				return pids
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("process helper did not write its PIDs")
	return nil
}

func waitForWindowsProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !windowsProcessAlive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d remained alive", pid)
}

func windowsProcessAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	status, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && status == uint32(windows.WAIT_TIMEOUT)
}
