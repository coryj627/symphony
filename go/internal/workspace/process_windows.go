package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

func runHookProcess(ctx context.Context, spec hookProcessSpec, output io.Writer) (result hookProcessResult) {
	result.ExitCode = -1
	if err := ctx.Err(); err != nil {
		result.TimedOut = true
		result.Err = err
		return result
	}
	shell, err := powerShellPath()
	if err != nil {
		result.Err = err
		return result
	}
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		result.Err = fmt.Errorf("create hook stdin pipe: %w", err)
		return result
	}
	defer stdinRead.Close()
	defer stdinWrite.Close()
	outputRead, outputWrite, err := os.Pipe()
	if err != nil {
		result.Err = fmt.Errorf("create hook output pipe: %w", err)
		return result
	}
	defer outputRead.Close()
	defer outputWrite.Close()

	childHandles := []windows.Handle{windows.Handle(stdinRead.Fd()), windows.Handle(outputWrite.Fd())}
	for _, handle := range childHandles {
		if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			result.Err = fmt.Errorf("make hook pipe inheritable: %w", err)
			return result
		}
	}
	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		result.Err = fmt.Errorf("create hook handle list: %w", err)
		return result
	}
	defer attributes.Delete()
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&childHandles[0]), uintptr(len(childHandles))*unsafe.Sizeof(childHandles[0])); err != nil {
		result.Err = fmt.Errorf("set hook handle list: %w", err)
		return result
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		result.Err = fmt.Errorf("create hook job object: %w", err)
		return result
	}
	jobClosed := false
	defer func() {
		if !jobClosed {
			_ = windows.CloseHandle(job)
		}
	}()
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		result.Err = fmt.Errorf("configure hook job object: %w", err)
		return result
	}

	executable, err := windows.UTF16PtrFromString(shell)
	if err != nil {
		result.Err = fmt.Errorf("encode hook shell path: %w", err)
		return result
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "-"}))
	if err != nil {
		result.Err = fmt.Errorf("encode hook command line: %w", err)
		return result
	}
	workingDirectory, err := windows.UTF16PtrFromString(spec.WorkingDir)
	if err != nil {
		result.Err = fmt.Errorf("encode hook working directory: %w", err)
		return result
	}
	environment, err := environmentBlock(spec.Environment)
	if err != nil {
		result.Err = fmt.Errorf("encode hook environment: %w", err)
		return result
	}
	startup := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})), Flags: windows.STARTF_USESTDHANDLES,
			StdInput: childHandles[0], StdOutput: childHandles[1], StdErr: childHandles[1],
		},
		ProcThreadAttributeList: attributes.List(),
	}
	process := windows.ProcessInformation{}
	flags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_NO_WINDOW)
	if err := windows.CreateProcess(executable, commandLine, nil, nil, true, flags, &environment[0], workingDirectory, &startup.StartupInfo, &process); err != nil {
		result.Err = fmt.Errorf("start workspace hook shell: %w", err)
		return result
	}
	defer windows.CloseHandle(process.Process)
	defer windows.CloseHandle(process.Thread)
	if err := windows.AssignProcessToJobObject(job, process.Process); err != nil {
		_ = windows.TerminateProcess(process.Process, 1)
		result.Err = fmt.Errorf("assign hook process to job object: %w", err)
		return result
	}
	if _, err := windows.ResumeThread(process.Thread); err != nil {
		_ = windows.TerminateJobObject(job, 1)
		result.Err = fmt.Errorf("resume hook process: %w", err)
		return result
	}
	_ = stdinRead.Close()
	_ = outputWrite.Close()

	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := io.WriteString(stdinWrite, spec.Script)
		writeDone <- errors.Join(writeErr, stdinWrite.Close())
	}()
	outputDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(output, outputRead)
		outputDone <- copyErr
	}()
	waitDone := make(chan error, 1)
	go func() {
		_, waitErr := windows.WaitForSingleObject(process.Process, windows.INFINITE)
		waitDone <- waitErr
	}()

	select {
	case waitErr := <-waitDone:
		result.Err = waitErr
	case <-ctx.Done():
		result.TimedOut = true
		result.Err = errors.Join(ctx.Err(), windows.TerminateJobObject(job, 1), <-waitDone)
	}
	if closeErr := windows.CloseHandle(job); closeErr != nil {
		result.Err = errors.Join(result.Err, closeErr)
	} else {
		jobClosed = true
	}
	_ = <-writeDone
	if copyErr := <-outputDone; result.Err == nil && copyErr != nil {
		result.Err = copyErr
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(process.Process, &exitCode); err != nil {
		result.Err = errors.Join(result.Err, err)
		return result
	}
	result.ExitCode = int(exitCode)
	if result.Err == nil && exitCode != 0 {
		result.Err = fmt.Errorf("workspace hook process exited with code %d", exitCode)
	}
	return result
}

func powerShellPath() (string, error) {
	for _, name := range []string{"pwsh.exe", "powershell.exe"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("%w: install PowerShell or add it to PATH", ErrHookShellUnavailable)
}

func environmentBlock(environment []string) ([]uint16, error) {
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
