package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coryj627/symphony/go/internal/app"
	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/instance"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/secrets"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/web"
	"github.com/coryj627/symphony/go/internal/workflow"
)

func TestRunReturnsArgumentFailureForInvalidFlags(t *testing.T) {
	var stderr bytes.Buffer

	if got := Run(context.Background(), []string{"--port", "70000"}, io.Discard, &stderr); got != 2 {
		t.Fatalf("Run() = %d, want 2", got)
	}
	if stderr.Len() == 0 {
		t.Fatal("Run() did not report the argument error")
	}
}

func TestRunModeInvalidPreflightOccursBeforeLockVaultListenerOrOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	store := &cliStore{loadErrors: []error{&workflow.InvalidWorkflowError{Validation: workflow.ValidationResult{
		Valid: false, GlobalErrors: []workflow.SafeError{{Code: "workflow_parse_error", Message: "The workflow could not be parsed."}},
	}}}}
	events := []string{}
	deps := testStartDependencies(store, &events)
	deps.resolveInstance = func(string, string, string) (instance.Info, error) {
		events = append(events, "resolve")
		return instance.Info{}, nil
	}
	deps.newVault = func() secrets.Store {
		events = append(events, "vault")
		return &cliVault{}
	}
	deps.newServer = func(web.Options) (runtimeServer, error) {
		events = append(events, "server")
		return &cliServer{}, nil
	}
	var stdout, stderr bytes.Buffer

	err := startWithDependencies(context.Background(), Options{Mode: ModeRun, WorkflowPath: path}, &stdout, &stderr, deps)
	var startup *StartupError
	if !errors.As(err, &startup) || startup.Code != "workflow_parse_error" {
		t.Fatalf("run preflight error = %v", err)
	}
	if got := strings.Join(events, ","); got != "new-store,load,store-close" {
		t.Fatalf("invalid preflight side effects = %q", got)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatal("invalid preflight published output")
	}
}

func TestConfigureModeStartsWithMissingWorkflowAndCleansUpInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	store := &cliStore{loadErrors: []error{workflow.ErrMissingWorkflow, workflow.ErrMissingWorkflow}}
	events := []string{}
	deps := testStartDependencies(store, &events)
	ctx, cancel := context.WithCancel(context.Background())
	deps.newServer = func(options web.Options) (runtimeServer, error) {
		events = append(events, fmt.Sprintf("server:%d", options.Port))
		server := &cliServer{events: &events, done: make(chan error, 1), onStart: cancel}
		return server, nil
	}
	var stderr bytes.Buffer
	if err := startWithDependencies(ctx, Options{Mode: ModeConfigure, WorkflowPath: path}, io.Discard, &stderr, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "http://127.0.0.1:43210/?access_token=") || strings.Count(stderr.String(), "access_token=") != 1 {
		t.Fatalf("startup output = %q", stderr.String())
	}
	got := strings.Join(events, ",")
	if !strings.Contains(got, "new-store,load,resolve,acquire,load,vault,logger,runtime-new:false,runtime-start,handler,bootstrap,server:0,start,shutdown,runtime-shutdown,store-close,logs-close,release") {
		t.Fatalf("configure composition/cleanup order = %q", got)
	}
}

func TestConfigureModeDuplicateLockStopsBeforeVaultAndListener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	store := &cliStore{loadErrors: []error{workflow.ErrMissingWorkflow}}
	events := []string{}
	deps := testStartDependencies(store, &events)
	deps.acquireLock = func(instance.Info) (instanceLock, error) {
		events = append(events, "acquire")
		return nil, instance.ErrAlreadyRunning
	}
	err := startWithDependencies(context.Background(), Options{Mode: ModeConfigure, WorkflowPath: path}, io.Discard, io.Discard, deps)
	var startup *StartupError
	if !errors.As(err, &startup) || startup.Code != "workflow_already_running" {
		t.Fatalf("duplicate startup = %v", err)
	}
	if strings.Contains(strings.Join(events, ","), "vault") || strings.Contains(strings.Join(events, ","), "server") {
		t.Fatalf("duplicate startup reached sensitive composition: %v", events)
	}
}

func TestPortSelectionHonorsExplicitZeroFileFallbackAndCLIOverride(t *testing.T) {
	tests := []struct {
		name     string
		options  Options
		filePort int
		wantPort int
	}{
		{name: "explicit zero", options: Options{Mode: ModeRun, Port: 0, PortSet: true}, filePort: 43127, wantPort: 0},
		{name: "file fallback", options: Options{Mode: ModeRun}, filePort: 43127, wantPort: 43127},
		{name: "CLI override", options: Options{Mode: ModeRun, Port: 44000, PortSet: true}, filePort: 43127, wantPort: 44000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "WORKFLOW.md")
			test.options.WorkflowPath = path
			snapshot := validCLISnapshot(path, "digest", test.filePort)
			store := &cliStore{snapshots: []workflow.Snapshot{snapshot, snapshot}}
			events := []string{}
			deps := testStartDependencies(store, &events)
			ctx, cancel := context.WithCancel(context.Background())
			deps.newServer = func(options web.Options) (runtimeServer, error) {
				if options.Port != test.wantPort {
					t.Fatalf("server port = %d, want %d", options.Port, test.wantPort)
				}
				return &cliServer{done: make(chan error, 1), onStart: cancel}, nil
			}
			if err := startWithDependencies(ctx, test.options, io.Discard, io.Discard, deps); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRuntimeServerFailureAttemptsEveryCleanupAndJoinsErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	snapshot := validCLISnapshot(path, "digest", 0)
	storeCloseErr := errors.New("store close failed")
	lockReleaseErr := errors.New("lock release failed")
	shutdownErr := errors.New("shutdown failed")
	runtimeShutdownErr := errors.New("runtime shutdown failed")
	logsCloseErr := errors.New("logs close failed")
	runtimeErr := errors.New("serve failed")
	store := &cliStore{snapshots: []workflow.Snapshot{snapshot, snapshot}, closeErr: storeCloseErr}
	events := []string{}
	deps := testStartDependencies(store, &events)
	deps.acquireLock = func(instance.Info) (instanceLock, error) {
		events = append(events, "acquire")
		return &cliLock{events: &events, err: lockReleaseErr}, nil
	}
	deps.newServer = func(web.Options) (runtimeServer, error) {
		done := make(chan error, 1)
		done <- runtimeErr
		close(done)
		return &cliServer{events: &events, done: done, shutdownErr: shutdownErr}, nil
	}
	deps.newRuntime = func(app.QueueOptions) queueRuntime {
		return &cliQueueRuntime{events: &events, shutdownErr: runtimeShutdownErr}
	}
	originalCloseLogs := deps.closeLogs
	deps.closeLogs = func(store *observability.LogStore) error {
		_ = originalCloseLogs(store)
		return logsCloseErr
	}

	err := startWithDependencies(context.Background(), Options{Mode: ModeRun, WorkflowPath: path}, io.Discard, io.Discard, deps)
	for _, want := range []error{runtimeErr, shutdownErr, runtimeShutdownErr, storeCloseErr, logsCloseErr, lockReleaseErr} {
		if !errors.Is(err, want) {
			t.Fatalf("joined runtime error %v does not contain %v", err, want)
		}
	}
	if tail := strings.Join(events[len(events)-5:], ","); tail != "shutdown,runtime-shutdown,store-close,logs-close,release" {
		t.Fatalf("cleanup tail = %q", tail)
	}
	for _, raw := range []string{"serve failed", "shutdown failed", "runtime shutdown failed", "store close failed", "logs close failed", "lock release failed"} {
		if strings.Contains(err.Error(), raw) {
			t.Fatalf("operator-visible runtime error exposed raw cause %q: %v", raw, err)
		}
	}
	if !strings.Contains(err.Error(), "web_runtime_failed") {
		t.Fatalf("operator-visible runtime error omitted safe code: %v", err)
	}
}

func TestProductionTrackerFactoryBuildsGitHubAndLinearWithScopedOwnedCredentials(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		raw      workflow.TrackerConfig
		wantKind string
		wantRef  string
		wantEnv  string
	}{
		{
			name: "github", wantKind: "github", wantRef: "os-vault", wantEnv: "GH_TOKEN",
			raw: workflow.TrackerConfig{Kind: "github", Provider: map[string]any{
				"owner": "coryj627", "repository": "symphony", "credential_ref": "os-vault",
			}},
		},
		{
			name: "linear", wantKind: "linear", wantRef: "$SYMPHONY_LINEAR_TOKEN", wantEnv: "SYMPHONY_LINEAR_TOKEN",
			raw: workflow.TrackerConfig{Kind: "linear", Provider: map[string]any{
				"project_slug": "symphony", "credential_ref": "$SYMPHONY_LINEAR_TOKEN",
			}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			credential := []byte("factory-token-canary-123456789")
			resolver := &trackingResolver{returned: credential}
			redactor := observability.NewRedactor(nil, func(name string) (string, bool) {
				if name == test.wantEnv {
					return string(credential), true
				}
				return "", false
			})
			logger, logs, err := observability.NewLogger(observability.Options{DataDir: t.TempDir(), Redactor: redactor, Stderr: io.Discard})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = logs.Close() })
			factory := newProductionTrackerFactory("workflow-id", func(name string) (string, bool) {
				if name == test.wantEnv {
					return string(credential), true
				}
				return "", false
			}, redactor, logger)
			adapter, err := factory.Build(context.Background(), test.raw, resolver)
			if err != nil {
				t.Fatal(err)
			}
			if adapter.Kind() != test.wantKind || resolver.ref != (secrets.Ref{WorkflowID: "workflow-id", TrackerKind: test.wantKind}) || resolver.reference != test.wantRef {
				t.Fatalf("factory result kind/ref/reference = %q/%#v/%q", adapter.Kind(), resolver.ref, resolver.reference)
			}
			if strings.Trim(string(credential), "\x00") != "" {
				t.Fatalf("factory did not clear resolver-owned credential: %v", credential)
			}
			logger.Info("redaction probe", slog.String("value", "factory-token-canary-123456789"))
			page, err := logs.Query(context.Background(), observability.LogQuery{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			encoded := fmt.Sprintf("%#v", page.Records)
			if strings.Contains(encoded, "factory-token-canary-123456789") {
				t.Fatalf("factory credential was not registered with the shared redactor: %s", encoded)
			}
		})
	}
}

func TestProductionCredentialResolverUsesExactEnvironmentNameOrOwnedVaultValue(t *testing.T) {
	t.Parallel()
	lookups := []string{}
	vault := &resolverVault{result: []byte("vault-token-canary"), ref: secrets.Ref{}}
	resolver := productionCredentialResolver{vault: vault, lookupEnv: func(name string) (string, bool) {
		lookups = append(lookups, name)
		if name == "EXACT_TOKEN" {
			return "environment-token-canary", true
		}
		return "", false
	}}
	ref := secrets.Ref{WorkflowID: "workflow-id", TrackerKind: "github"}
	environment, err := resolver.Resolve(context.Background(), ref, "$EXACT_TOKEN")
	if err != nil || string(environment) != "environment-token-canary" || strings.Join(lookups, ",") != "EXACT_TOKEN" || vault.calls != 0 {
		t.Fatalf("environment resolve = %q, %v lookups=%v vault=%d", environment, err, lookups, vault.calls)
	}
	clear(environment)
	vaultValue, err := resolver.Resolve(context.Background(), ref, "os-vault")
	if err != nil || string(vaultValue) != "vault-token-canary" || vault.calls != 1 || vault.ref != ref {
		t.Fatalf("vault resolve = %q, %v calls/ref=%d/%#v", vaultValue, err, vault.calls, vault.ref)
	}
	if strings.Trim(string(vault.result), "\x00") != "" {
		t.Fatalf("resolver retained vault-returned temporary bytes: %v", vault.result)
	}
	vaultValue[0] = 'X'
	clear(vaultValue)
}

func TestRunModeSuppliesStartedLiveRuntimeAndSharedLoggerToHandler(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	snapshot := validCLISnapshot(path, "digest", 0)
	store := &cliStore{snapshots: []workflow.Snapshot{snapshot, snapshot}}
	events := []string{}
	deps := testStartDependencies(store, &events)
	var processLogger *slog.Logger
	baseNewLogger := deps.newLogger
	deps.newLogger = func(options observability.Options) (*slog.Logger, *observability.LogStore, error) {
		logger, logs, err := baseNewLogger(options)
		processLogger = logger
		return logger, logs, err
	}
	deps.newRuntime = func(options app.QueueOptions) queueRuntime {
		if !options.Enabled || options.Store == nil || options.Factory == nil || options.Resolver == nil || options.Journal == nil || options.Logger == nil || options.Logger != processLogger {
			t.Fatalf("run runtime options = %#v", options)
		}
		return &cliQueueRuntime{events: &events}
	}
	deps.newHandler = func(_ *app.ConfigService, mode string, queries app.RuntimeQueries, commands app.RuntimeCommands, logs *observability.LogStore, logger *slog.Logger) (http.Handler, web.ErrorResponder, error) {
		events = append(events, "handler")
		if mode != "run" || queries == nil || commands == nil || logs == nil || logger != processLogger {
			t.Fatalf("handler live dependencies mode=%q queries=%v commands=%v logs=%v logger=%p", mode, queries, commands, logs, logger)
		}
		return http.NotFoundHandler(), nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	deps.newServer = func(options web.Options) (runtimeServer, error) {
		if options.Logger == nil || options.Logger != processLogger {
			t.Fatal("web server did not receive the shared process logger")
		}
		return &cliServer{events: &events, done: make(chan error, 1), onStart: cancel}, nil
	}
	if err := startWithDependencies(ctx, Options{Mode: ModeRun, WorkflowPath: path}, io.Discard, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(events, ",")
	if !strings.Contains(joined, "runtime-start,handler") || !strings.Contains(joined, "shutdown,runtime-shutdown,store-close,logs-close,release") {
		t.Fatalf("runtime composition/cleanup order = %q", joined)
	}
}

func TestHandlerConstructionFailureClosesStartedRuntimeAndLogStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	snapshot := validCLISnapshot(path, "digest", 0)
	store := &cliStore{snapshots: []workflow.Snapshot{snapshot, snapshot}}
	events := []string{}
	deps := testStartDependencies(store, &events)
	deps.newHandler = func(*app.ConfigService, string, app.RuntimeQueries, app.RuntimeCommands, *observability.LogStore, *slog.Logger) (http.Handler, web.ErrorResponder, error) {
		events = append(events, "handler")
		return nil, nil, errors.New("handler-construction-canary")
	}
	err := startWithDependencies(context.Background(), Options{Mode: ModeRun, WorkflowPath: path}, io.Discard, io.Discard, deps)
	var startup *StartupError
	if !errors.As(err, &startup) || startup.Code != "web_handler_failed" || strings.Contains(err.Error(), "handler-construction-canary") {
		t.Fatalf("handler construction error = %v", err)
	}
	joined := strings.Join(events, ",")
	if !strings.Contains(joined, "runtime-start,handler,runtime-shutdown,store-close,logs-close,release") {
		t.Fatalf("handler failure cleanup order = %q", joined)
	}
}

func TestRuntimeStartAndServerStartFailuresStillCleanUpInDependencyOrder(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		runtimeErr error
		serverErr  error
		wantCode   string
		wantTail   string
	}{
		{name: "runtime start", runtimeErr: errors.New("runtime start raw"), wantCode: "queue_runtime_failed", wantTail: "runtime-start,runtime-shutdown,store-close,logs-close,release"},
		{name: "server start", serverErr: errors.New("server start raw"), wantCode: "web_bind_failed", wantTail: "start,shutdown,runtime-shutdown,store-close,logs-close,release"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "WORKFLOW.md")
			snapshot := validCLISnapshot(path, "digest", 0)
			store := &cliStore{snapshots: []workflow.Snapshot{snapshot, snapshot}}
			events := []string{}
			deps := testStartDependencies(store, &events)
			deps.newRuntime = func(app.QueueOptions) queueRuntime {
				return &cliQueueRuntime{events: &events, startErr: test.runtimeErr}
			}
			deps.newServer = func(web.Options) (runtimeServer, error) {
				return &cliServer{events: &events, done: make(chan error, 1), startErr: test.serverErr}, nil
			}
			err := startWithDependencies(context.Background(), Options{Mode: ModeRun, WorkflowPath: path}, io.Discard, io.Discard, deps)
			var startup *StartupError
			if !errors.As(err, &startup) || startup.Code != test.wantCode || strings.Contains(err.Error(), " raw") {
				t.Fatalf("startup error = %v", err)
			}
			joined := strings.Join(events, ",")
			if !strings.Contains(joined, test.wantTail) {
				t.Fatalf("failure cleanup order = %q, want tail %q", joined, test.wantTail)
			}
		})
	}
}

func TestRunReturnsStartupFailure(t *testing.T) {
	restoreStart(t, func(context.Context, Options, io.Writer, io.Writer) error {
		return errors.New("startup failed")
	})
	var stderr bytes.Buffer

	if got := Run(context.Background(), nil, io.Discard, &stderr); got != 1 {
		t.Fatalf("Run() = %d, want 1", got)
	}
	if got := stderr.String(); got != "startup failed\n" {
		t.Fatalf("stderr = %q, want startup error", got)
	}
}

func TestRunReturnsRuntimeFailureWhenStartupEndsBeforeShutdown(t *testing.T) {
	restoreStart(t, func(context.Context, Options, io.Writer, io.Writer) error {
		return nil
	})
	var stderr bytes.Buffer

	if got := Run(context.Background(), nil, io.Discard, &stderr); got != 1 {
		t.Fatalf("Run() = %d, want 1", got)
	}
	if stderr.Len() == 0 {
		t.Fatal("Run() did not report the unexpected startup completion")
	}
}

func TestRunReturnsSuccessAfterContextDrivenShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	restoreStart(t, func(gotCtx context.Context, opts Options, stdout, _ io.Writer) error {
		if gotCtx != ctx {
			return errors.New("unexpected context")
		}
		if opts.Mode != ModeConfigure || opts.WorkflowPath != "C:/work/WORKFLOW.md" {
			return fmt.Errorf("unexpected options: %#v", opts)
		}
		if _, err := fmt.Fprintln(stdout, "stopped cleanly"); err != nil {
			return err
		}
		return nil
	})
	var stdout, stderr bytes.Buffer

	if got := Run(ctx, []string{"configure", "C:/work/WORKFLOW.md"}, &stdout, &stderr); got != 0 {
		t.Fatalf("Run() = %d, want 0; stderr = %q", got, stderr.String())
	}
	if got := stdout.String(); got != "stopped cleanly\n" {
		t.Fatalf("stdout = %q, want clean shutdown output", got)
	}
}

func restoreStart(t *testing.T, replacement func(context.Context, Options, io.Writer, io.Writer) error) {
	t.Helper()
	previous := start
	start = replacement
	t.Cleanup(func() { start = previous })
}

func testStartDependencies(store *cliStore, events *[]string) startDependencies {
	return startDependencies{
		newStore: func(context.Context, string, workflow.LookupEnv, workflow.ProviderValidator) (workflowStore, error) {
			*events = append(*events, "new-store")
			store.events = events
			return store, nil
		},
		resolveInstance: func(path, _, _ string) (instance.Info, error) {
			*events = append(*events, "resolve")
			return instance.Info{WorkflowID: "workflow-id", WorkflowPath: path, DataDir: filepath.Dir(path), LockPath: path + ".lock"}, nil
		},
		acquireLock: func(instance.Info) (instanceLock, error) {
			*events = append(*events, "acquire")
			return &cliLock{events: events}, nil
		},
		newVault: func() secrets.Store {
			*events = append(*events, "vault")
			return &cliVault{}
		},
		newLogger: func(options observability.Options) (*slog.Logger, *observability.LogStore, error) {
			*events = append(*events, "logger")
			return observability.NewLogger(options)
		},
		closeLogs: func(store *observability.LogStore) error {
			*events = append(*events, "logs-close")
			return store.Close()
		},
		newRuntime: func(options app.QueueOptions) queueRuntime {
			*events = append(*events, fmt.Sprintf("runtime-new:%t", options.Enabled))
			return &cliQueueRuntime{events: events}
		},
		newHandler: func(*app.ConfigService, string, app.RuntimeQueries, app.RuntimeCommands, *observability.LogStore, *slog.Logger) (http.Handler, web.ErrorResponder, error) {
			*events = append(*events, "handler")
			handler := http.NotFoundHandler()
			return handler, nil, nil
		},
		newBootstrap: func() (web.Bootstrap, error) {
			*events = append(*events, "bootstrap")
			return web.NewBootstrap()
		},
		newServer: func(web.Options) (runtimeServer, error) {
			*events = append(*events, "server")
			return &cliServer{events: events, done: make(chan error, 1)}, nil
		},
		openBrowser: func(string) error {
			*events = append(*events, "open")
			return nil
		},
		lookupEnv: func(string) (string, bool) { return "", false },
	}
}

func validCLISnapshot(path, digest string, port int) workflow.Snapshot {
	return workflow.Snapshot{
		Path: path, Digest: digest, Source: "valid",
		Config: workflow.EffectiveConfig{
			Tracker: workflow.TrackerConfig{Kind: "github", Provider: map[string]any{"owner": "coryj627", "repository": "symphony", "credential_ref": "os-vault"}},
			Server:  workflow.ServerConfig{Port: port},
		},
	}
}

type cliStore struct {
	snapshots  []workflow.Snapshot
	loadErrors []error
	loadIndex  int
	closeErr   error
	events     *[]string
}

func (*cliStore) Current() (workflow.Snapshot, bool) { return workflow.Snapshot{}, false }
func (store *cliStore) Load(context.Context) (workflow.Snapshot, error) {
	if store.events != nil {
		*store.events = append(*store.events, "load")
	}
	index := store.loadIndex
	store.loadIndex++
	if index < len(store.loadErrors) && store.loadErrors[index] != nil {
		return workflow.Snapshot{}, store.loadErrors[index]
	}
	if index < len(store.snapshots) {
		return store.snapshots[index], nil
	}
	if len(store.snapshots) > 0 {
		return store.snapshots[len(store.snapshots)-1], nil
	}
	return workflow.Snapshot{}, workflow.ErrMissingWorkflow
}
func (*cliStore) Validate(context.Context, []byte) workflow.ValidationResult {
	return workflow.ValidationResult{Valid: true}
}
func (*cliStore) Save(context.Context, workflow.SaveCommand) (workflow.Snapshot, error) {
	return workflow.Snapshot{}, nil
}
func (*cliStore) Changes() <-chan workflow.Change { return nil }
func (store *cliStore) Close() error {
	if store.events != nil {
		*store.events = append(*store.events, "store-close")
	}
	return store.closeErr
}

type cliLock struct {
	events *[]string
	err    error
}

func (lock *cliLock) Release() error {
	if lock.events != nil {
		*lock.events = append(*lock.events, "release")
	}
	return lock.err
}

type cliServer struct {
	events      *[]string
	done        chan error
	onStart     func()
	shutdownErr error
	startErr    error
}

func (server *cliServer) Start(context.Context) (web.Bound, error) {
	if server.events != nil {
		*server.events = append(*server.events, "start")
	}
	if server.onStart != nil {
		server.onStart()
	}
	if server.startErr != nil {
		return web.Bound{}, server.startErr
	}
	return web.Bound{URL: "http://127.0.0.1:43210/?access_token=test-capability", Port: 43210}, nil
}
func (server *cliServer) Shutdown(context.Context) error {
	if server.events != nil {
		*server.events = append(*server.events, "shutdown")
	}
	return server.shutdownErr
}
func (server *cliServer) Done() <-chan error { return server.done }

type cliVault struct{}

func (*cliVault) Put(context.Context, secrets.Ref, []byte) error     { return nil }
func (*cliVault) Get(context.Context, secrets.Ref) ([]byte, error)   { return nil, secrets.ErrNotFound }
func (*cliVault) Delete(context.Context, secrets.Ref) error          { return secrets.ErrNotFound }
func (*cliVault) Status(context.Context, secrets.Ref) secrets.Status { return secrets.Status{} }

type cliQueueRuntime struct {
	events      *[]string
	startErr    error
	shutdownErr error
}

func (runtime *cliQueueRuntime) Start(context.Context) error {
	if runtime.events != nil {
		*runtime.events = append(*runtime.events, "runtime-start")
	}
	return runtime.startErr
}
func (runtime *cliQueueRuntime) Shutdown(context.Context) error {
	if runtime.events != nil {
		*runtime.events = append(*runtime.events, "runtime-shutdown")
	}
	return runtime.shutdownErr
}
func (*cliQueueRuntime) NotifyCredentialChanged() {}
func (*cliQueueRuntime) Snapshot(context.Context) (domain.Snapshot, error) {
	return domain.EmptySnapshot(), nil
}
func (*cliQueueRuntime) Issue(context.Context, string) (domain.IssueDetail, error) {
	return domain.IssueDetail{}, app.ErrIssueNotFound
}
func (*cliQueueRuntime) EventsAfter(context.Context, domain.EventCursor) (domain.EventPage, error) {
	return domain.EventPage{Events: []domain.Event{}}, nil
}
func (*cliQueueRuntime) RecentEvents(context.Context, int) (domain.EventPage, error) {
	return domain.EventPage{Events: []domain.Event{}}, nil
}
func (*cliQueueRuntime) SubscribeEvents(domain.EventCursor) <-chan struct{} {
	ready := make(chan struct{})
	close(ready)
	return ready
}
func (*cliQueueRuntime) Refresh(context.Context) (domain.RefreshReceipt, error) {
	return domain.RefreshReceipt{Operations: []string{"poll"}}, nil
}
func (*cliQueueRuntime) SetScheduler(context.Context, bool) error { return app.ErrUnavailableInPhase }
func (*cliQueueRuntime) Respond(context.Context, domain.OperatorResponse) error {
	return app.ErrUnavailableInPhase
}
func (*cliQueueRuntime) ExtendOperatorRequest(context.Context, string) error {
	return app.ErrUnavailableInPhase
}

type trackingResolver struct {
	returned  []byte
	ref       secrets.Ref
	reference string
}

func (resolver *trackingResolver) Resolve(_ context.Context, ref secrets.Ref, reference string) ([]byte, error) {
	resolver.ref = ref
	resolver.reference = reference
	return resolver.returned, nil
}

type resolverVault struct {
	result []byte
	ref    secrets.Ref
	calls  int
}

func (*resolverVault) Put(context.Context, secrets.Ref, []byte) error { return nil }
func (vault *resolverVault) Get(_ context.Context, ref secrets.Ref) ([]byte, error) {
	vault.ref = ref
	vault.calls++
	return vault.result, nil
}
func (*resolverVault) Delete(context.Context, secrets.Ref) error          { return nil }
func (*resolverVault) Status(context.Context, secrets.Ref) secrets.Status { return secrets.Status{} }

var _ tracker.Factory = (*productionTrackerFactory)(nil)
var _ secrets.Resolver = productionCredentialResolver{}
