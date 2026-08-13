//go:build darwin

package codex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessUsesDedicatedProcessGroup(t *testing.T) {
	process, _ := launchTestProcessTree(t)
	defer process.Stop(context.Background())
	group, err := syscall.Getpgid(process.PID())
	if err != nil {
		t.Fatal(err)
	}
	if group != process.PID() {
		t.Fatalf("process group = %d, pid = %d", group, process.PID())
	}
}

func TestProcessStopTerminatesDescendantsAndNotUnrelatedProcess(t *testing.T) {
	process, pidFile := launchTestProcessTree(t)
	pids := waitForPIDFile(t, pidFile)
	unrelated := exec.Command(os.Args[0], "-test.run=^TestCodexProcessHelper$")
	unrelated.Env = replaceEnvironmentValue(os.Environ(), "SYMPHONY_CODEX_PROCESS_HELPER", "leaf")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unrelated.Process.Kill(); _, _ = unrelated.Process.Wait() })

	if err := process.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, pid := range pids {
		waitForDarwinProcessExit(t, pid)
	}
	if err := syscall.Kill(unrelated.Process.Pid, 0); err != nil {
		t.Fatalf("unrelated process was signaled: %v", err)
	}
}

func launchTestProcessTree(t *testing.T) (Process, string) {
	t.Helper()
	pidFile := t.TempDir() + "/pids"
	environment := replaceEnvironmentValue(os.Environ(), "SYMPHONY_CODEX_PROCESS_HELPER", "tree")
	environment = replaceEnvironmentValue(environment, "SYMPHONY_CODEX_PID_FILE", pidFile)
	environment = replaceEnvironmentValue(environment, "SYMPHONY_CODEX_TEST_EXE", os.Args[0])
	process, err := Launch(t.Context(), LaunchOptions{
		Cwd: canonicalTestDirectory(t), Command: `exec "$SYMPHONY_CODEX_TEST_EXE" -test.run=^TestCodexProcessHelper$`,
		BashPath: "/bin/bash", Environment: environment,
		GracePeriod: 100 * time.Millisecond, ForcePeriod: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return process, pidFile
}

func waitForPIDFile(t *testing.T, path string) []int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
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

func waitForDarwinProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d remained alive", pid)
}
