//go:build darwin

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

func configureCLIProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func interruptCLIProcess(process *os.Process) error {
	return process.Signal(os.Interrupt)
}
