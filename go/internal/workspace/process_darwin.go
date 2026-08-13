package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"
)

const hookTerminationGrace = 250 * time.Millisecond

func runHookProcess(ctx context.Context, spec hookProcessSpec, output io.Writer) hookProcessResult {
	if err := ctx.Err(); err != nil {
		return hookProcessResult{ExitCode: -1, TimedOut: true, Err: err}
	}
	command := exec.Command("/bin/sh", "-lc", spec.Script)
	command.Dir = spec.WorkingDir
	command.Env = append([]string(nil), spec.Environment...)
	command.Stdout = output
	command.Stderr = output
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return hookProcessResult{ExitCode: -1, Err: fmt.Errorf("start workspace hook shell: %w", err)}
	}

	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	select {
	case err := <-wait:
		return completedProcessResult(err, false)
	case <-ctx.Done():
		pid := command.Process.Pid
		terminateErr := signalProcessGroup(pid, syscall.SIGTERM)
		select {
		case waitErr := <-wait:
			return hookProcessResult{ExitCode: processExitCode(waitErr), TimedOut: true, Err: errors.Join(ctx.Err(), terminateErr)}
		case <-time.After(hookTerminationGrace):
			killErr := signalProcessGroup(pid, syscall.SIGKILL)
			waitErr := <-wait
			return hookProcessResult{ExitCode: processExitCode(waitErr), TimedOut: true, Err: errors.Join(ctx.Err(), terminateErr, killErr)}
		}
	}
}

func completedProcessResult(err error, timedOut bool) hookProcessResult {
	if err == nil {
		return hookProcessResult{ExitCode: 0, TimedOut: timedOut}
	}
	return hookProcessResult{ExitCode: processExitCode(err), TimedOut: timedOut, Err: fmt.Errorf("workspace hook process: %w", err)}
}

func processExitCode(err error) int {
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return -1
}

func signalProcessGroup(pid int, signal syscall.Signal) error {
	err := syscall.Kill(-pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
