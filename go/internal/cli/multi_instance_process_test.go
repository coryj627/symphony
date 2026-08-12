//go:build darwin || windows

package cli

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/instance"
)

func TestBuiltCLIProcessesUseRealEphemeralPortsRejectDuplicateAndReleaseLocks(t *testing.T) {
	root := t.TempDir()
	binary := buildProcessTestBinary(t, root)
	configureProcessTestHome(t, root)
	workflowA := writeProcessWorkflow(t, filepath.Join(root, "project-a", "WORKFLOW.md"))
	workflowB := writeProcessWorkflow(t, filepath.Join(root, "project-b", "WORKFLOW.md"))

	first := launchCLIChild(t, binary, "configure", workflowA, "--port", "0", "--data-dir", filepath.Join(root, "data-a"))
	second := launchCLIChild(t, binary, "configure", workflowB, "--port", "0", "--data-dir", filepath.Join(root, "data-b"))
	firstPort := waitForCLIReady(t, first)
	secondPort := waitForCLIReady(t, second)
	if firstPort == 0 || secondPort == 0 || firstPort == secondPort {
		t.Fatalf("bound ports = %d and %d, want distinct ephemeral ports", firstPort, secondPort)
	}
	for _, port := range []int{firstPort, secondPort} {
		connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
		if err != nil {
			t.Fatalf("connect to bound port %d: %v", port, err)
		}
		_ = connection.Close()
	}

	duplicatePath := filepath.Join(root, "workflow-a-link.md")
	if err := os.Symlink(workflowA, duplicatePath); err != nil {
		t.Logf("symlink unavailable; using an equivalent canonical spelling: %v", err)
		duplicatePath = filepath.Join(filepath.Dir(workflowA), ".", filepath.Base(workflowA))
	}
	duplicate := launchCLIChild(t, binary, "configure", duplicatePath, "--port", "0", "--data-dir", filepath.Join(root, "data-duplicate"))
	duplicateErr := waitForCLIExit(t, duplicate, 8*time.Second)
	if duplicateErr == nil || !strings.Contains(duplicate.output.String(), "workflow_already_running") {
		t.Fatalf("duplicate exit/output = %v/%q, want workflow_already_running", duplicateErr, duplicate.output.String())
	}

	for name, child := range map[string]*cliChild{"first": first, "second": second} {
		if err := interruptCLIProcess(child.cmd.Process); err != nil {
			t.Fatalf("interrupt %s CLI: %v", name, err)
		}
		if err := waitForCLIExit(t, child, 8*time.Second); err != nil {
			t.Fatalf("%s CLI shutdown: %v; output=%q", name, err, child.output.String())
		}
	}

	for _, path := range []string{workflowA, workflowB} {
		info, err := instance.Resolve(path, "", filepath.Join(root, "lock-check"))
		if err != nil {
			t.Fatal(err)
		}
		lock, err := instance.Acquire(info)
		if err != nil {
			t.Fatalf("instance lock was not released for %s: %v", path, err)
		}
		if err := lock.Release(); err != nil {
			t.Fatalf("release reacquired lock for %s: %v", path, err)
		}
	}
}

type cliChild struct {
	cmd     *exec.Cmd
	lines   chan string
	exited  chan struct{}
	waitErr error
	output  safeOutput
}

type safeOutput struct {
	mu      sync.Mutex
	builder strings.Builder
}

func (output *safeOutput) WriteLine(line string) {
	output.mu.Lock()
	defer output.mu.Unlock()
	output.builder.WriteString(line)
	output.builder.WriteByte('\n')
}

func (output *safeOutput) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.builder.String()
}

func buildProcessTestBinary(t *testing.T, root string) string {
	t.Helper()
	name := "symphony-process-test"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(root, name)
	command := exec.Command("go", "build", "-o", binary, "./cmd/symphony")
	command.Dir = filepath.Join("..", "..")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build CLI process fixture: %v\n%s", err, output)
	}
	return binary
}

func writeProcessWorkflow(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// Configure mode intentionally accepts this incomplete document while still
	// exercising the production identity, lock, observability, and HTTP server.
	if err := os.WriteFile(path, []byte("Symphony process fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func launchCLIChild(t *testing.T, binary string, args ...string) *cliChild {
	t.Helper()
	child := &cliChild{cmd: exec.Command(binary, args...), lines: make(chan string, 32), exited: make(chan struct{})}
	configureCLIProcess(child.cmd)
	pipe, err := child.cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.cmd.Start(); err != nil {
		t.Fatalf("start CLI child: %v", err)
	}
	go func() {
		scanner := bufio.NewScanner(pipe)
		for scanner.Scan() {
			line := scanner.Text()
			child.output.WriteLine(line)
			child.lines <- line
		}
		close(child.lines)
		child.waitErr = child.cmd.Wait()
		close(child.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-child.exited:
			return
		default:
		}
		_ = child.cmd.Process.Kill()
		select {
		case <-child.exited:
		case <-time.After(3 * time.Second):
		}
	})
	return child
}

func waitForCLIReady(t *testing.T, child *cliChild) int {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-child.lines:
			if !ok {
				<-child.exited
				t.Fatalf("CLI exited before ready: %v; output=%q", child.waitErr, child.output.String())
			}
			const prefix = "Symphony configure mode is ready on loopback port "
			if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, ".") {
				continue
			}
			port, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(line, prefix), "."))
			if err != nil {
				t.Fatalf("parse ready line %q: %v", line, err)
			}
			return port
		case <-timer.C:
			t.Fatalf("CLI did not become ready; output=%q", child.output.String())
		}
	}
}

func waitForCLIExit(t *testing.T, child *cliChild, timeout time.Duration) error {
	t.Helper()
	select {
	case <-child.exited:
		return child.waitErr
	case <-time.After(timeout):
		return fmt.Errorf("CLI process did not exit within %s", timeout)
	}
}

func configureProcessTestHome(t *testing.T, root string) {
	t.Helper()
	switch runtime.GOOS {
	case "darwin":
		t.Setenv("HOME", root)
	case "windows":
		t.Setenv("APPDATA", filepath.Join(root, "AppData", "Roaming"))
	default:
		t.Fatal(errors.New("unsupported process-test platform"))
	}
}
