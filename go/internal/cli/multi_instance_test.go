package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/instance"
	"github.com/coryj627/symphony/go/internal/web"
	"github.com/coryj627/symphony/go/internal/workflow"
)

func TestTwoDifferentWorkflowsUseSeparateEphemeralInstancesAndRejectCanonicalDuplicate(t *testing.T) {
	root := t.TempDir()
	workflowA := filepath.Join(root, "project-a", "WORKFLOW.md")
	workflowB := filepath.Join(root, "project-b", "WORKFLOW.md")
	for _, path := range []string{workflowA, workflowB} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("workflow fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lockRoot := filepath.Join(root, "locks")
	dataRoot := filepath.Join(root, "data")
	var nextPort atomic.Int64
	nextPort.Store(45000)

	resolve := func(path, scope, _ string) (instance.Info, error) {
		info, err := instance.Resolve(path, scope, "")
		if err != nil {
			return instance.Info{}, err
		}
		info.LockPath = filepath.Join(lockRoot, info.WorkflowID+".lock")
		info.DataDir = filepath.Join(dataRoot, info.ID)
		return info, nil
	}
	type harness struct {
		cancel  context.CancelFunc
		done    chan error
		started chan int
	}
	startHarness := func(path string) harness {
		store := &cliStore{snapshots: []workflow.Snapshot{
			validCLISnapshot(path, "digest", 0),
			validCLISnapshot(path, "digest", 0),
		}}
		events := []string{}
		deps := testStartDependencies(store, &events)
		deps.resolveInstance = resolve
		deps.acquireLock = func(info instance.Info) (instanceLock, error) { return instance.Acquire(info) }
		started := make(chan int, 1)
		deps.newServer = func(options web.Options) (runtimeServer, error) {
			if options.Port != 0 {
				t.Fatalf("requested port = %d, want ephemeral 0", options.Port)
			}
			return &multiInstanceServer{port: int(nextPort.Add(1)), started: started, done: make(chan error)}, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- startWithDependencies(ctx, Options{Mode: ModeRun, WorkflowPath: path, Port: 0, PortSet: true}, io.Discard, io.Discard, deps)
		}()
		return harness{cancel: cancel, done: done, started: started}
	}

	first := startHarness(workflowA)
	second := startHarness(workflowB)
	waitStarted := func(name string, started <-chan int) int {
		t.Helper()
		select {
		case port := <-started:
			return port
		case <-time.After(3 * time.Second):
			t.Fatalf("%s instance did not start", name)
			return 0
		}
	}
	firstPort := waitStarted("first", first.started)
	secondPort := waitStarted("second", second.started)
	if firstPort == 0 || secondPort == 0 || firstPort == secondPort {
		t.Fatalf("bound ports = %d and %d, want different ephemeral ports", firstPort, secondPort)
	}

	duplicatePath := filepath.Join(root, "workflow-a-link.md")
	if err := os.Symlink(workflowA, duplicatePath); err != nil {
		t.Logf("symlink unavailable; using a second canonical spelling: %v", err)
		duplicatePath = filepath.Join(filepath.Dir(workflowA), ".", filepath.Base(workflowA))
	}
	duplicateStore := &cliStore{snapshots: []workflow.Snapshot{validCLISnapshot(duplicatePath, "digest", 0)}}
	duplicateEvents := []string{}
	duplicateDeps := testStartDependencies(duplicateStore, &duplicateEvents)
	duplicateDeps.resolveInstance = resolve
	duplicateDeps.acquireLock = func(info instance.Info) (instanceLock, error) { return instance.Acquire(info) }
	duplicateErr := startWithDependencies(context.Background(), Options{Mode: ModeRun, WorkflowPath: duplicatePath, Port: 0, PortSet: true}, io.Discard, io.Discard, duplicateDeps)
	var startup *StartupError
	if !errors.As(duplicateErr, &startup) || startup.Code != "workflow_already_running" {
		t.Fatalf("canonical duplicate error = %v", duplicateErr)
	}

	first.cancel()
	second.cancel()
	for name, done := range map[string]<-chan error{"first": first.done, "second": second.done} {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s shutdown: %v", name, err)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("%s instance did not shut down", name)
		}
	}

	for _, path := range []string{workflowA, workflowB} {
		info, err := resolve(path, "github:coryj627/symphony", "")
		if err != nil {
			t.Fatal(err)
		}
		lock, err := instance.Acquire(info)
		if err != nil {
			t.Fatalf("instance lock was not released for %s: %v", path, err)
		}
		if err := lock.Release(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestShutdownHelpersBoundServerAndRuntimeDeadlines(t *testing.T) {
	serverDeadline := make(chan time.Time, 1)
	runtimeDeadline := make(chan time.Time, 1)
	server := &deadlineServer{deadline: serverDeadline}
	runtime := &deadlineQueueRuntime{cliQueueRuntime: &cliQueueRuntime{}, deadline: runtimeDeadline}
	started := time.Now()
	if err := shutdownServer(server); err != nil {
		t.Fatal(err)
	}
	if err := shutdownQueue(runtime); err != nil {
		t.Fatal(err)
	}
	for name, deadline := range map[string]time.Time{
		"server":  <-serverDeadline,
		"runtime": <-runtimeDeadline,
	} {
		remaining := deadline.Sub(started)
		if remaining < 4500*time.Millisecond || remaining > 5500*time.Millisecond {
			t.Errorf("%s shutdown deadline = %s, want approximately 5s", name, remaining)
		}
	}
}

func TestShutdownServerReturnsWhenTheBoundedDeadlineExpires(t *testing.T) {
	server := &deadlineServer{deadline: make(chan time.Time, 1), block: true}
	started := time.Now()
	err := shutdownServer(server)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want deadline exceeded", err)
	}
	if elapsed < 4500*time.Millisecond || elapsed > 6500*time.Millisecond {
		t.Fatalf("forced shutdown elapsed = %s, want approximately 5s", elapsed)
	}
}

type multiInstanceServer struct {
	port    int
	started chan<- int
	done    chan error
}

func (server *multiInstanceServer) Start(context.Context) (web.Bound, error) {
	server.started <- server.port
	return web.Bound{URL: "http://127.0.0.1/", Port: server.port}, nil
}

func (*multiInstanceServer) Shutdown(context.Context) error { return nil }
func (server *multiInstanceServer) Done() <-chan error      { return server.done }

var _ runtimeServer = (*multiInstanceServer)(nil)

type deadlineServer struct {
	deadline chan<- time.Time
	block    bool
}

func (*deadlineServer) Start(context.Context) (web.Bound, error) { return web.Bound{}, nil }
func (server *deadlineServer) Shutdown(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("server shutdown context has no deadline")
	}
	server.deadline <- deadline
	if server.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (*deadlineServer) Done() <-chan error { return make(chan error) }

type deadlineQueueRuntime struct {
	*cliQueueRuntime
	deadline chan<- time.Time
}

func (runtime *deadlineQueueRuntime) Shutdown(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("runtime shutdown context has no deadline")
	}
	runtime.deadline <- deadline
	return nil
}

var _ runtimeServer = (*deadlineServer)(nil)
var _ queueRuntime = (*deadlineQueueRuntime)(nil)
