package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coryj627/symphony/go/internal/observability"
)

const (
	defaultProcessGracePeriod = 5 * time.Second
	defaultProcessForcePeriod = 2 * time.Second
	maxWorkflowCommandBytes   = 64 << 10
)

var (
	ErrInvalidLaunch          = errors.New("codex_invalid_launch")
	ErrUnsafeWorkingDirectory = errors.New("codex_unsafe_working_directory")
	ErrProcessStopTimeout     = errors.New("codex_process_stop_timeout")
)

// LaunchOptions is an immutable child-process launch snapshot.
type LaunchOptions struct {
	Cwd           string
	WorkspaceRoot string
	Command       string
	BashPath      string
	Environment   []string
	SecretNames   []string
	GracePeriod   time.Duration
	ForcePeriod   time.Duration
	Redactor      *observability.Redactor
	Logger        *slog.Logger
}

// Process is the contained app-server transport and lifecycle handle.
type Process interface {
	io.Reader
	io.Writer
	io.Closer
	PID() int
	Done() <-chan struct{}
	Wait(context.Context) error
	Stop(context.Context) error
	Diagnostic() string
}

type processBackend interface {
	pid() int
	waitForExit() error
	graceful() error
	force() error
	close() error
}

type nativeLaunch struct {
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	backend processBackend
}

type childProcess struct {
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	backend processBackend
	stderr  *StderrCapture
	grace   time.Duration
	force   time.Duration

	waitDone chan struct{}
	waitMu   sync.RWMutex
	waitErr  error

	stdinOnce  sync.Once
	stdinErr   error
	stdoutOnce sync.Once
	stdoutErr  error
	stopOnce   sync.Once
	stopDone   chan struct{}
	stopMu     sync.RWMutex
	stopErr    error
}

type validatedLaunchOptions struct {
	LaunchOptions
	Spec CommandSpec
}

// Launch validates the workspace again, strips credentials, and returns only
// after the native containment boundary owns the child.
func Launch(ctx context.Context, options LaunchOptions) (Process, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is missing", ErrInvalidLaunch)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	validated, err := validateLaunchOptions(options)
	if err != nil {
		return nil, err
	}
	capture := NewStderrCapture(validated.Redactor, validated.Logger)
	native, err := launchNative(validated, capture)
	if err != nil {
		return nil, err
	}
	process := newProcess(native, capture, validated.GracePeriod, validated.ForcePeriod)
	go func() {
		select {
		case <-ctx.Done():
			_ = process.Stop(context.Background())
		case <-process.Done():
		}
	}()
	return process, nil
}

func validateLaunchOptions(options LaunchOptions) (validatedLaunchOptions, error) {
	options.Cwd = filepath.Clean(options.Cwd)
	options.WorkspaceRoot = filepath.Clean(options.WorkspaceRoot)
	if strings.TrimSpace(options.Command) == "" || len(options.Command) > maxWorkflowCommandBytes || strings.IndexByte(options.Command, 0) >= 0 {
		return validatedLaunchOptions{}, fmt.Errorf("%w: workflow command is invalid", ErrInvalidLaunch)
	}
	for _, supplied := range options.SecretNames {
		if strings.TrimSpace(supplied) == "" {
			continue
		}
		if _, ok := normalizeSecretEnvironmentName(supplied); !ok {
			return validatedLaunchOptions{}, fmt.Errorf("%w: declared secret environment name is invalid", ErrInvalidLaunch)
		}
	}
	for _, entry := range options.Environment {
		if strings.IndexByte(entry, 0) >= 0 {
			return validatedLaunchOptions{}, fmt.Errorf("%w: child environment contains NUL", ErrInvalidLaunch)
		}
	}
	if err := validateWorkingDirectory(options.WorkspaceRoot, options.Cwd); err != nil {
		return validatedLaunchOptions{}, err
	}
	if options.BashPath == "" {
		bashPath, err := FindBash()
		if err != nil {
			return validatedLaunchOptions{}, err
		}
		options.BashPath = bashPath
	}
	if !filepath.IsAbs(options.BashPath) || !executableFile(options.BashPath) {
		return validatedLaunchOptions{}, fmt.Errorf("%w: configured Bash executable is unavailable", ErrBashUnavailable)
	}
	if options.Environment == nil {
		options.Environment = os.Environ()
	}
	options.Environment = SanitizeEnvironment(options.Environment, options.SecretNames)
	options.SecretNames = append([]string(nil), options.SecretNames...)
	if options.GracePeriod <= 0 {
		options.GracePeriod = defaultProcessGracePeriod
	}
	if options.ForcePeriod <= 0 {
		options.ForcePeriod = defaultProcessForcePeriod
	}
	return validatedLaunchOptions{LaunchOptions: options, Spec: BashCommand(options.BashPath, options.Command)}, nil
}

func validateWorkingDirectory(root, cwd string) error {
	if cwd == "." || !filepath.IsAbs(cwd) {
		return fmt.Errorf("%w: working directory must be absolute", ErrUnsafeWorkingDirectory)
	}
	resolvedCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return fmt.Errorf("%w: resolve working directory", ErrUnsafeWorkingDirectory)
	}
	info, err := os.Stat(resolvedCwd)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: working directory is not a directory", ErrUnsafeWorkingDirectory)
	}
	if !samePlatformPath(resolvedCwd, cwd) {
		return fmt.Errorf("%w: working directory is no longer canonical", ErrUnsafeWorkingDirectory)
	}
	if root == "." || root == "" {
		return nil
	}
	if !filepath.IsAbs(root) {
		return fmt.Errorf("%w: workspace root must be absolute", ErrUnsafeWorkingDirectory)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil || !samePlatformPath(resolvedRoot, root) {
		return fmt.Errorf("%w: workspace root is no longer canonical", ErrUnsafeWorkingDirectory)
	}
	relative, err := filepath.Rel(resolvedRoot, resolvedCwd)
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: working directory is outside its workspace root", ErrUnsafeWorkingDirectory)
	}
	return nil
}

func samePlatformPath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func newProcess(native nativeLaunch, capture *StderrCapture, grace, force time.Duration) *childProcess {
	process := &childProcess{
		stdin: native.stdin, stdout: native.stdout, backend: native.backend, stderr: capture,
		grace: grace, force: force, waitDone: make(chan struct{}), stopDone: make(chan struct{}),
	}
	go func() {
		err := process.backend.waitForExit()
		process.waitMu.Lock()
		process.waitErr = err
		process.waitMu.Unlock()
		capture.LogSummary()
		close(process.waitDone)
	}()
	return process
}

func (process *childProcess) PID() int { return process.backend.pid() }

func (process *childProcess) Done() <-chan struct{} { return process.waitDone }

func (process *childProcess) Read(target []byte) (int, error) { return process.stdout.Read(target) }

func (process *childProcess) Write(source []byte) (int, error) { return process.stdin.Write(source) }

func (process *childProcess) Close() error {
	inputErr := process.closeInput()
	process.stdoutOnce.Do(func() { process.stdoutErr = ignoreAlreadyClosed(process.stdout.Close()) })
	return errors.Join(inputErr, process.stdoutErr)
}

func (process *childProcess) Wait(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: wait context is missing", ErrInvalidLaunch)
	}
	select {
	case <-process.waitDone:
		process.waitMu.RLock()
		defer process.waitMu.RUnlock()
		return process.waitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (process *childProcess) Stop(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: stop context is missing", ErrInvalidLaunch)
	}
	process.stopOnce.Do(func() { go process.stop() })
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-process.stopDone:
		process.stopMu.RLock()
		defer process.stopMu.RUnlock()
		return process.stopErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (process *childProcess) Diagnostic() string { return process.stderr.Diagnostic() }

func (process *childProcess) stop() {
	defer close(process.stopDone)
	inputErr := process.closeInput()
	if process.exited() {
		process.finishStop(errors.Join(inputErr, process.backend.close()))
		return
	}
	gracefulErr := process.backend.graceful()
	if waitForClosed(process.waitDone, process.grace) {
		process.finishStop(errors.Join(inputErr, gracefulErr, process.backend.close()))
		return
	}
	forceErr := process.backend.force()
	if waitForClosed(process.waitDone, process.force) {
		process.finishStop(errors.Join(inputErr, gracefulErr, forceErr, process.backend.close()))
		return
	}
	process.finishStop(errors.Join(inputErr, gracefulErr, forceErr, process.backend.close(), ErrProcessStopTimeout))
}

func (process *childProcess) closeInput() error {
	process.stdinOnce.Do(func() { process.stdinErr = ignoreAlreadyClosed(process.stdin.Close()) })
	return process.stdinErr
}

func ignoreAlreadyClosed(err error) error {
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

func (process *childProcess) exited() bool {
	select {
	case <-process.waitDone:
		return true
	default:
		return false
	}
}

func (process *childProcess) finishStop(err error) {
	process.stopMu.Lock()
	process.stopErr = err
	process.stopMu.Unlock()
}

func waitForClosed(done <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
