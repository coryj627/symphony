//go:build darwin

package codex

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"syscall"
)

type darwinProcess struct {
	command   *exec.Cmd
	processID int
}

func launchNative(options validatedLaunchOptions, stderr io.Writer) (nativeLaunch, error) {
	command := exec.Command(options.Spec.Path, options.Spec.Args...)
	command.Dir = options.Cwd
	command.Env = append([]string(nil), options.Environment...)
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nativeLaunch{}, fmt.Errorf("create Codex stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nativeLaunch{}, fmt.Errorf("create Codex stdout: %w", err)
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nativeLaunch{}, fmt.Errorf("start Codex app-server: %w", err)
	}
	pid := command.Process.Pid
	group, err := syscall.Getpgid(pid)
	if err != nil || group != pid {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = command.Process.Kill()
		_ = command.Wait()
		return nativeLaunch{}, fmt.Errorf("contain Codex process group: %w", errors.Join(err, ErrInvalidLaunch))
	}
	return nativeLaunch{
		stdin: stdin, stdout: stdout,
		backend: &darwinProcess{command: command, processID: pid},
	}, nil
}

func (process *darwinProcess) pid() int { return process.processID }

func (process *darwinProcess) waitForExit() error { return process.command.Wait() }

func (process *darwinProcess) graceful() error {
	return signalDarwinProcessGroup(process.processID, syscall.SIGTERM)
}

func (process *darwinProcess) force() error {
	return signalDarwinProcessGroup(process.processID, syscall.SIGKILL)
}

func (*darwinProcess) close() error { return nil }

func signalDarwinProcessGroup(pid int, signal syscall.Signal) error {
	err := syscall.Kill(-pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
