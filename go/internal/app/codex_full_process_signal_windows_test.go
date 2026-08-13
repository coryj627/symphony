//go:build windows

package app

import (
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
)

func configureFullProcessCommand(command *exec.Cmd) {
	command.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

func interruptFullProcess(process *os.Process) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(process.Pid))
}
