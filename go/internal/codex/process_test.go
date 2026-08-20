package codex

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionCloseClosesTransportBeforeStoppingProcessOnce(t *testing.T) {
	process := newSessionLifecycleProcess()
	router := NewRouter(process, RouterOptions{})
	options := testSessionOptions(t)
	options.Process = process
	session := NewSession(router, options)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	process.mu.Lock()
	defer process.mu.Unlock()
	if strings.Join(process.operations, ",") != "close,stop" {
		t.Fatalf("operations = %q", process.operations)
	}
}

func TestSessionCloseInterruptsActiveTurnBeforeTransportAndProcessStop(t *testing.T) {
	stdoutReader, serverOutput := io.Pipe()
	serverInputRaw, stdinWriter := io.Pipe()
	transport := &pipeTransport{
		stdoutReader: stdoutReader,
		stdinWriter:  stdinWriter,
		serverInput:  bufio.NewReader(serverInputRaw),
		serverOutput: serverOutput,
	}
	process := &trackingPipeProcess{pipeTransport: transport, done: make(chan struct{})}
	t.Cleanup(func() { _ = transport.Close() })
	router := NewRouter(process, RouterOptions{})
	options := testSessionOptions(t)
	options.Process = process
	events := make(chan SessionEvent, 8)
	options.OnEvent = func(event SessionEvent) { events <- event }
	session := NewSession(router, options)
	startup := make(chan error, 1)
	go func() {
		_, err := session.Start(t.Context())
		startup <- err
	}()
	initialize := transport.readRequest(t)
	respondResult(t, transport, initialize, map[string]any{
		"userAgent": "codex_cli_rs/0.144.1", "codexHome": options.Workspace,
		"platformFamily": "unix", "platformOs": "macos",
	})
	_ = transport.readRequest(t) // initialized
	threadStart := transport.readRequest(t)
	respondThreadStarted(t, transport, threadStart, options.Workspace, "thread-1")
	if err := <-startup; err != nil {
		t.Fatal(err)
	}
	turnResult := startTurn(t.Context(), session, "work")
	turnStart := transport.readRequest(t)
	respondTurnStarted(t, transport, turnStart, "turn-1")
	awaitSessionEvent(t, events, SessionEventTurnStarted)

	closeResult := make(chan error, 1)
	go func() { closeResult <- session.Close() }()
	interrupt := transport.readRequest(t)
	if methodOf(t, interrupt) != "turn/interrupt" {
		t.Fatalf("first shutdown request = %s", interrupt["method"])
	}
	process.mu.Lock()
	if len(process.operations) != 0 {
		t.Fatalf("process stopped before interrupt reply: %q", process.operations)
	}
	process.mu.Unlock()
	respondResult(t, transport, interrupt, map[string]any{})
	if err := <-closeResult; err != nil {
		t.Fatal(err)
	}
	<-turnResult
	process.mu.Lock()
	defer process.mu.Unlock()
	if strings.Join(process.operations, ",") != "close,stop" {
		t.Fatalf("operations = %q", process.operations)
	}
}

func TestSessionClosedStateRejectsNewTurnBeforeProtocolWrite(t *testing.T) {
	router, _ := newPipeTransport(t, RouterOptions{})
	session := NewSession(router, testSessionOptions(t))
	session.mu.Lock()
	session.threadID = "thread-1"
	session.closed = true
	session.mu.Unlock()
	result := make(chan error, 1)
	go func() {
		_, err := session.StartTurn(t.Context(), "work")
		result <- err
	}()
	select {
	case err := <-result:
		var protocolError *ProtocolError
		if !errors.As(err, &protocolError) || protocolError.Code != ProtocolErrorRouterClosed {
			t.Fatalf("StartTurn() = %v", err)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("closed session attempted a protocol write")
	}
}

func TestChildEnvironmentIsSanitizedInSpawnedProcess(t *testing.T) {
	bash, err := FindBash()
	if err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(canonicalTestDirectory(t), "environment.txt")
	environment := os.Environ()
	for name, value := range map[string]string{
		"SYMPHONY_CODEX_PROCESS_HELPER": "environment",
		"SYMPHONY_CODEX_ENV_REPORT":     report,
		"SYMPHONY_CODEX_ENV_KEYS":       "GH_TOKEN,GITHUB_TOKEN,LINEAR_API_KEY,CUSTOM_TRACKER,SAFE_MARKER",
		"SYMPHONY_CODEX_TEST_EXE":       os.Args[0],
		"GH_TOKEN":                      "github-one",
		"GITHUB_TOKEN":                  "github-two",
		"LINEAR_API_KEY":                "linear-three",
		"CUSTOM_TRACKER":                "custom-four",
		"SAFE_MARKER":                   "yes",
	} {
		environment = replaceEnvironmentValue(environment, name, value)
	}
	process, err := Launch(t.Context(), LaunchOptions{
		Cwd:      canonicalTestDirectory(t),
		Command:  `exec "$SYMPHONY_CODEX_TEST_EXE" -test.run=^TestCodexProcessHelper$`,
		BashPath: bash, Environment: environment, SecretNames: []string{"CUSTOM_TRACKER"},
		GracePeriod: 100 * time.Millisecond, ForcePeriod: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Stop(context.Background()) })
	waitContext, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := process.Wait(waitContext); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != "SAFE_MARKER=yes\n" {
		t.Fatalf("child environment report = %q", got)
	}
}

func TestProcessStopIsIdempotentAndGracefulExitAvoidsForce(t *testing.T) {
	backend := newFakeProcessBackend(713)
	backend.onGraceful = func() { backend.complete(nil) }
	stdin := &recordingWriteCloser{}
	process := newProcess(nativeLaunch{
		stdin: stdin, stdout: io.NopCloser(strings.NewReader("")), backend: backend,
	}, NewStderrCapture(nil, nil), 100*time.Millisecond, 100*time.Millisecond)

	var wait sync.WaitGroup
	errorsSeen := make(chan error, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsSeen <- process.Stop(t.Context())
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("Stop() = %v", err)
		}
	}
	if got := backend.gracefulCalls.Load(); got != 1 {
		t.Fatalf("graceful calls = %d", got)
	}
	if got := backend.forceCalls.Load(); got != 0 {
		t.Fatalf("force calls = %d", got)
	}
	if got := stdin.closes.Load(); got != 1 {
		t.Fatalf("stdin closes = %d", got)
	}
}

func TestProcessStopUsesBoundedForceDeadline(t *testing.T) {
	backend := newFakeProcessBackend(714)
	process := newProcess(nativeLaunch{
		stdin: &recordingWriteCloser{}, stdout: io.NopCloser(strings.NewReader("")), backend: backend,
	}, NewStderrCapture(nil, nil), 15*time.Millisecond, 20*time.Millisecond)
	started := time.Now()
	err := process.Stop(t.Context())
	elapsed := time.Since(started)
	if !errors.Is(err, ErrProcessStopTimeout) {
		t.Fatalf("Stop() error = %v", err)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("Stop() took %v", elapsed)
	}
	if backend.gracefulCalls.Load() != 1 || backend.forceCalls.Load() != 1 || backend.closeCalls.Load() != 1 {
		t.Fatalf("backend calls graceful=%d force=%d close=%d", backend.gracefulCalls.Load(), backend.forceCalls.Load(), backend.closeCalls.Load())
	}
	backend.complete(nil)
}

func TestProcessStopContinuesCleanupAfterCallerCancellation(t *testing.T) {
	backend := newFakeProcessBackend(715)
	backend.onForce = func() { backend.complete(nil) }
	process := newProcess(nativeLaunch{
		stdin: &recordingWriteCloser{}, stdout: io.NopCloser(strings.NewReader("")), backend: backend,
	}, NewStderrCapture(nil, nil), 20*time.Millisecond, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := process.Stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop() = %v", err)
	}
	select {
	case <-process.stopDone:
	case <-time.After(time.Second):
		t.Fatal("cleanup stopped with its caller")
	}
	if backend.forceCalls.Load() != 1 {
		t.Fatalf("force calls = %d", backend.forceCalls.Load())
	}
}

func TestLaunchRechecksCanonicalWorkspaceContainment(t *testing.T) {
	bash, err := FindBash()
	if err != nil {
		t.Fatal(err)
	}
	root := canonicalTestDirectory(t)
	workspace := filepath.Join(root, "GH-42")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	options := LaunchOptions{Cwd: workspace, WorkspaceRoot: root, Command: "codex app-server", BashPath: bash}
	if _, err := validateLaunchOptions(options); err != nil {
		t.Fatalf("valid options: %v", err)
	}

	outside := t.TempDir()
	original := workspace + "-original"
	if err := os.Rename(workspace, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, workspace); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := validateLaunchOptions(options); !errors.Is(err, ErrUnsafeWorkingDirectory) {
		t.Fatalf("changed workspace error = %v", err)
	}
}

func TestLaunchRejectsInvalidSecretNamesAndEnvironment(t *testing.T) {
	bash, err := FindBash()
	if err != nil {
		t.Fatal(err)
	}
	root := canonicalTestDirectory(t)
	workspace := filepath.Join(root, "GH-42")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	base := LaunchOptions{Cwd: workspace, WorkspaceRoot: root, Command: "codex app-server", BashPath: bash}
	invalidSecret := base
	invalidSecret.SecretNames = []string{"TOKEN=VALUE"}
	if _, err := validateLaunchOptions(invalidSecret); !errors.Is(err, ErrInvalidLaunch) {
		t.Fatalf("invalid secret error = %v", err)
	}
	invalidEnvironment := base
	invalidEnvironment.Environment = []string{"SAFE=yes\x00BAD=secret"}
	if _, err := validateLaunchOptions(invalidEnvironment); !errors.Is(err, ErrInvalidLaunch) {
		t.Fatalf("invalid environment error = %v", err)
	}
}

func TestCodexProcessHelper(t *testing.T) {
	role := os.Getenv("SYMPHONY_CODEX_PROCESS_HELPER")
	if role == "" {
		return
	}
	if role == "leaf" {
		waitForCodexProcessHelperTermination()
		return
	}
	if role == "environment" {
		var report strings.Builder
		for _, name := range strings.Split(os.Getenv("SYMPHONY_CODEX_ENV_KEYS"), ",") {
			if value, ok := os.LookupEnv(name); ok {
				report.WriteString(name)
				report.WriteByte('=')
				report.WriteString(value)
				report.WriteByte('\n')
			}
		}
		if err := os.WriteFile(os.Getenv("SYMPHONY_CODEX_ENV_REPORT"), []byte(report.String()), 0o600); err != nil {
			os.Exit(5)
		}
		return
	}
	if role != "tree" {
		os.Exit(2)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestCodexProcessHelper$")
	child.Env = replaceEnvironmentValue(os.Environ(), "SYMPHONY_CODEX_PROCESS_HELPER", "leaf")
	if err := child.Start(); err != nil {
		os.Exit(3)
	}
	pidFile := os.Getenv("SYMPHONY_CODEX_PID_FILE")
	value := strconv.Itoa(os.Getpid()) + "\n" + strconv.Itoa(child.Process.Pid) + "\n"
	if err := os.WriteFile(pidFile, []byte(value), 0o600); err != nil {
		os.Exit(4)
	}
	waitForCodexProcessHelperTermination()
}

// A case-free select lets the Go runtime's deadlock detector terminate the
// helper. Keeping a timer pending blocks until the process test stops it.
func waitForCodexProcessHelperTermination() {
	for {
		time.Sleep(time.Hour)
	}
}

type recordingWriteCloser struct{ closes atomic.Int32 }

func (*recordingWriteCloser) Write(source []byte) (int, error) { return len(source), nil }
func (writer *recordingWriteCloser) Close() error {
	writer.closes.Add(1)
	return nil
}

type fakeProcessBackend struct {
	processID     int
	wait          chan error
	completeOnce  sync.Once
	gracefulCalls atomic.Int32
	forceCalls    atomic.Int32
	closeCalls    atomic.Int32
	onGraceful    func()
	onForce       func()
}

type sessionLifecycleProcess struct {
	reader     *io.PipeReader
	writer     *io.PipeWriter
	output     bytes.Buffer
	done       chan struct{}
	doneOnce   sync.Once
	mu         sync.Mutex
	operations []string
}

func newSessionLifecycleProcess() *sessionLifecycleProcess {
	reader, writer := io.Pipe()
	return &sessionLifecycleProcess{reader: reader, writer: writer, done: make(chan struct{})}
}

func (process *sessionLifecycleProcess) Read(target []byte) (int, error) {
	return process.reader.Read(target)
}
func (process *sessionLifecycleProcess) Write(source []byte) (int, error) {
	return process.output.Write(source)
}
func (process *sessionLifecycleProcess) Close() error {
	process.mu.Lock()
	process.operations = append(process.operations, "close")
	process.mu.Unlock()
	process.doneOnce.Do(func() { close(process.done) })
	return errors.Join(process.reader.Close(), process.writer.Close())
}
func (*sessionLifecycleProcess) PID() int                      { return 716 }
func (process *sessionLifecycleProcess) Done() <-chan struct{} { return process.done }
func (process *sessionLifecycleProcess) Wait(ctx context.Context) error {
	select {
	case <-process.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (process *sessionLifecycleProcess) Stop(context.Context) error {
	process.mu.Lock()
	process.operations = append(process.operations, "stop")
	process.mu.Unlock()
	return nil
}
func (*sessionLifecycleProcess) Diagnostic() string { return "" }

type trackingPipeProcess struct {
	*pipeTransport
	mu         sync.Mutex
	operations []string
	done       chan struct{}
	stopOnce   sync.Once
}

func (process *trackingPipeProcess) Close() error {
	process.mu.Lock()
	process.operations = append(process.operations, "close")
	process.mu.Unlock()
	return process.pipeTransport.Close()
}

func (*trackingPipeProcess) PID() int { return 717 }

func (process *trackingPipeProcess) Done() <-chan struct{} { return process.done }

func (process *trackingPipeProcess) Wait(ctx context.Context) error {
	select {
	case <-process.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (process *trackingPipeProcess) Stop(context.Context) error {
	process.stopOnce.Do(func() {
		process.mu.Lock()
		process.operations = append(process.operations, "stop")
		process.mu.Unlock()
		close(process.done)
	})
	return nil
}

func (*trackingPipeProcess) Diagnostic() string { return "" }

func newFakeProcessBackend(processID int) *fakeProcessBackend {
	return &fakeProcessBackend{processID: processID, wait: make(chan error, 1)}
}

func (backend *fakeProcessBackend) pid() int           { return backend.processID }
func (backend *fakeProcessBackend) waitForExit() error { return <-backend.wait }
func (backend *fakeProcessBackend) graceful() error {
	backend.gracefulCalls.Add(1)
	if backend.onGraceful != nil {
		backend.onGraceful()
	}
	return nil
}
func (backend *fakeProcessBackend) force() error {
	backend.forceCalls.Add(1)
	if backend.onForce != nil {
		backend.onForce()
	}
	return nil
}
func (backend *fakeProcessBackend) close() error {
	backend.closeCalls.Add(1)
	return nil
}
func (backend *fakeProcessBackend) complete(err error) {
	backend.completeOnce.Do(func() { backend.wait <- err })
}
