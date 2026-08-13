//go:build windows

package codex

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsProcess struct {
	process   windows.Handle
	job       windows.Handle
	processID int
	assigned  bool
	stderr    <-chan error
	closeOnce sync.Once
	closeErr  error
}

func launchNative(options validatedLaunchOptions, stderr io.Writer) (result nativeLaunch, resultErr error) {
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		return nativeLaunch{}, fmt.Errorf("create Codex stdin: %w", err)
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		_ = stdinRead.Close()
		_ = stdinWrite.Close()
		return nativeLaunch{}, fmt.Errorf("create Codex stdout: %w", err)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		_ = stdinRead.Close()
		_ = stdinWrite.Close()
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		return nativeLaunch{}, fmt.Errorf("create Codex stderr: %w", err)
	}
	parentFiles := []*os.File{stdinWrite, stdoutRead, stderrRead}
	childFiles := []*os.File{stdinRead, stdoutWrite, stderrWrite}
	cleanupFiles := func() {
		for _, file := range append(parentFiles, childFiles...) {
			_ = file.Close()
		}
	}

	childHandles := []windows.Handle{
		windows.Handle(stdinRead.Fd()), windows.Handle(stdoutWrite.Fd()), windows.Handle(stderrWrite.Fd()),
	}
	for _, handle := range childHandles {
		if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			cleanupFiles()
			return nativeLaunch{}, fmt.Errorf("configure Codex standard handles: %w", err)
		}
	}
	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		cleanupFiles()
		return nativeLaunch{}, fmt.Errorf("create Codex handle list: %w", err)
	}
	defer attributes.Delete()
	if err := attributes.Update(
		windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
		unsafe.Pointer(&childHandles[0]),
		uintptr(len(childHandles))*unsafe.Sizeof(childHandles[0]),
	); err != nil {
		cleanupFiles()
		return nativeLaunch{}, fmt.Errorf("set Codex handle list: %w", err)
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		cleanupFiles()
		return nativeLaunch{}, fmt.Errorf("create Codex job object: %w", err)
	}
	jobOwned := true
	defer func() {
		if resultErr != nil && jobOwned {
			_ = windows.CloseHandle(job)
		}
	}()
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	); err != nil {
		cleanupFiles()
		return nativeLaunch{}, fmt.Errorf("configure Codex job object: %w", err)
	}

	executable, err := windows.UTF16PtrFromString(options.Spec.Path)
	if err != nil {
		cleanupFiles()
		return nativeLaunch{}, fmt.Errorf("encode Codex Bash path: %w", err)
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{options.Spec.Path}, options.Spec.Args...)))
	if err != nil {
		cleanupFiles()
		return nativeLaunch{}, fmt.Errorf("encode Codex command: %w", err)
	}
	workingDirectory, err := windows.UTF16PtrFromString(options.Cwd)
	if err != nil {
		cleanupFiles()
		return nativeLaunch{}, fmt.Errorf("encode Codex working directory: %w", err)
	}
	environment, err := windowsEnvironmentBlock(options.Environment)
	if err != nil {
		cleanupFiles()
		return nativeLaunch{}, fmt.Errorf("encode Codex environment: %w", err)
	}
	startup := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:       uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Flags:    windows.STARTF_USESTDHANDLES,
			StdInput: childHandles[0], StdOutput: childHandles[1], StdErr: childHandles[2],
		},
		ProcThreadAttributeList: attributes.List(),
	}
	information := windows.ProcessInformation{}
	flags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_NO_WINDOW)
	if err := windows.CreateProcess(
		executable, commandLine, nil, nil, true, flags, &environment[0], workingDirectory,
		&startup.StartupInfo, &information,
	); err != nil {
		cleanupFiles()
		return nativeLaunch{}, fmt.Errorf("start Codex app-server: %w", err)
	}
	processOwned := true
	threadOwned := true
	defer func() {
		if threadOwned {
			_ = windows.CloseHandle(information.Thread)
		}
		if resultErr != nil && processOwned {
			_ = windows.TerminateProcess(information.Process, 1)
			_ = windows.CloseHandle(information.Process)
		}
	}()
	if err := windows.AssignProcessToJobObject(job, information.Process); err != nil {
		cleanupFiles()
		return nativeLaunch{}, fmt.Errorf("contain Codex process in job object: %w", err)
	}
	if _, err := windows.ResumeThread(information.Thread); err != nil {
		_ = windows.TerminateJobObject(job, 1)
		cleanupFiles()
		return nativeLaunch{}, fmt.Errorf("resume contained Codex process: %w", err)
	}
	if err := windows.CloseHandle(information.Thread); err != nil {
		_ = windows.TerminateJobObject(job, 1)
		cleanupFiles()
		return nativeLaunch{}, fmt.Errorf("close Codex primary thread handle: %w", err)
	}
	threadOwned = false
	_ = stdinRead.Close()
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()

	stderrDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(stderr, stderrRead)
		stderrDone <- errors.Join(copyErr, ignoreAlreadyClosed(stderrRead.Close()))
	}()
	backend := &windowsProcess{
		process: information.Process, job: job, processID: int(information.ProcessId),
		assigned: true, stderr: stderrDone,
	}
	processOwned = false
	jobOwned = false
	return nativeLaunch{stdin: stdinWrite, stdout: stdoutRead, backend: backend}, nil
}

func (process *windowsProcess) pid() int { return process.processID }

func (process *windowsProcess) waitForExit() error {
	status, waitErr := windows.WaitForSingleObject(process.process, windows.INFINITE)
	if waitErr == nil && status != windows.WAIT_OBJECT_0 {
		waitErr = fmt.Errorf("unexpected Codex process wait status %d", status)
	}
	var exitCode uint32
	exitErr := windows.GetExitCodeProcess(process.process, &exitCode)
	closeErr := windows.CloseHandle(process.process)
	stderrErr := <-process.stderr
	if waitErr == nil && exitErr == nil && exitCode != 0 {
		waitErr = fmt.Errorf("Codex app-server exited with code %d", exitCode)
	}
	return errors.Join(waitErr, exitErr, closeErr, stderrErr)
}

// Closing stdin is the graceful Windows signal. The force path terminates the
// kill-on-close Job Object, which contains every descendant.
func (*windowsProcess) graceful() error { return nil }

func (process *windowsProcess) force() error {
	err := windows.TerminateJobObject(process.job, 1)
	if errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		return nil
	}
	return err
}

func (process *windowsProcess) close() error {
	process.closeOnce.Do(func() {
		process.closeErr = windows.CloseHandle(process.job)
		if errors.Is(process.closeErr, windows.ERROR_INVALID_HANDLE) {
			process.closeErr = nil
		}
	})
	return process.closeErr
}

func windowsEnvironmentBlock(environment []string) ([]uint16, error) {
	values := append([]string(nil), environment...)
	for _, value := range values {
		if strings.IndexByte(value, 0) >= 0 {
			return nil, errors.New("environment contains NUL")
		}
	}
	sort.SliceStable(values, func(left, right int) bool {
		return strings.ToLower(values[left]) < strings.ToLower(values[right])
	})
	joined := strings.Join(values, "\x00") + "\x00\x00"
	return utf16.Encode([]rune(joined)), nil
}
