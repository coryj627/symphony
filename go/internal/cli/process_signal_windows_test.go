//go:build windows

package cli

import (
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
)

func configureCLIProcess(command *exec.Cmd) {
	command.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

func interruptCLIProcess(process *os.Process) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(process.Pid))
}
