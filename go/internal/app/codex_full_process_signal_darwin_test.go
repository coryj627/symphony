//go:build darwin

package app

import (
	"os"
	"os/exec"
	"syscall"
)

func configureFullProcessCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func interruptFullProcess(process *os.Process) error { return process.Signal(os.Interrupt) }
