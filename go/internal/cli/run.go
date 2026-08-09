package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coryj627/symphony/go/internal/app"
	"github.com/coryj627/symphony/go/internal/instance"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/secrets"
	"github.com/coryj627/symphony/go/internal/tracker"
	trackergithub "github.com/coryj627/symphony/go/internal/tracker/github"
	trackerlinear "github.com/coryj627/symphony/go/internal/tracker/linear"
	"github.com/coryj627/symphony/go/internal/web"
	"github.com/coryj627/symphony/go/internal/workflow"
)

type StartupError struct {
	Code    string
	Message string
}

type safeJoinedError struct {
	display error
	causes  []error
}

func (err *safeJoinedError) Error() string   { return err.display.Error() }
func (err *safeJoinedError) Unwrap() []error { return err.causes }

func joinSafe(primary error, causes ...error) error {
	nonNil := make([]error, 0, len(causes)+1)
	if primary != nil {
		nonNil = append(nonNil, primary)
	}
	for _, cause := range causes {
		if cause != nil {
			nonNil = append(nonNil, cause)
		}
	}
	if len(nonNil) == 0 {
		return nil
	}
	display := primary
	if display == nil {
		display = &StartupError{Code: "cleanup_failed", Message: "Symphony stopped, but one or more cleanup operations could not be confirmed."}
	}
	return &safeJoinedError{display: display, causes: nonNil}
}

func (err *StartupError) Error() string {
	if err == nil {
		return ""
	}
	return err.Code + ": " + err.Message
}

type workflowStore interface {
	workflow.Store
	Close() error
}

type instanceLock interface {
	Release() error
}

type runtimeServer interface {
	Start(context.Context) (web.Bound, error)
	Shutdown(context.Context) error
	Done() <-chan error
}

type queueRuntime interface {
	app.RuntimeQueries
	app.RuntimeCommands
	Start(context.Context) error
	Shutdown(context.Context) error
	NotifyCredentialChanged()
}

type startDependencies struct {
	newStore        func(context.Context, string, workflow.LookupEnv, workflow.ProviderValidator) (workflowStore, error)
	resolveInstance func(string, string, string) (instance.Info, error)
	acquireLock     func(instance.Info) (instanceLock, error)
	newVault        func() secrets.Store
	newLogger       func(observability.Options) (*slog.Logger, *observability.LogStore, error)
	closeLogs       func(*observability.LogStore) error
	newRuntime      func(app.QueueOptions) queueRuntime
	newHandler      func(*app.ConfigService, string, app.RuntimeQueries, app.RuntimeCommands, *observability.LogStore) (http.Handler, web.ErrorResponder, error)
	newBootstrap    func() (web.Bootstrap, error)
	newServer       func(web.Options) (runtimeServer, error)
	openBrowser     func(string) error
	lookupEnv       workflow.LookupEnv
}

var start = productionStart

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := Parse(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := start(ctx, opts, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if ctx.Err() == nil {
		fmt.Fprintln(stderr, "application stopped before context shutdown")
		return 1
	}
	return 0
}

func productionStart(ctx context.Context, options Options, stdout, stderr io.Writer) error {
	return startWithDependencies(ctx, options, stdout, stderr, defaultStartDependencies())
}

func defaultStartDependencies() startDependencies {
	return startDependencies{
		newStore: func(ctx context.Context, path string, lookup workflow.LookupEnv, validator workflow.ProviderValidator) (workflowStore, error) {
			return workflow.NewStore(ctx, path, lookup, validator)
		},
		resolveInstance: instance.Resolve,
		acquireLock: func(info instance.Info) (instanceLock, error) {
			return instance.Acquire(info)
		},
		newVault:  newVaultStore,
		newLogger: observability.NewLogger,
		closeLogs: func(store *observability.LogStore) error {
			return store.Close()
		},
		newRuntime: func(options app.QueueOptions) queueRuntime {
			return app.NewQueueRuntime(options)
		},
		newHandler: func(service *app.ConfigService, mode string, _ app.RuntimeQueries, _ app.RuntimeCommands, _ *observability.LogStore) (http.Handler, web.ErrorResponder, error) {
			handler, err := web.NewConfiguredPageHandler(service, mode)
			if err != nil {
				return nil, nil, err
			}
			return handler, handler, nil
		},
		newBootstrap: web.NewBootstrap,
		newServer: func(options web.Options) (runtimeServer, error) {
			return web.NewServer(options)
		},
		openBrowser: openProtectedURL,
		lookupEnv:   os.LookupEnv,
	}
}

type productionTrackerFactory struct {
	workflowID string
	lookupEnv  workflow.LookupEnv
	redactor   *observability.Redactor
	logger     *slog.Logger
}

func newProductionTrackerFactory(workflowID string, lookupEnv workflow.LookupEnv, redactor *observability.Redactor, logger *slog.Logger) *productionTrackerFactory {
	return &productionTrackerFactory{workflowID: workflowID, lookupEnv: lookupEnv, redactor: redactor, logger: logger}
}

func (factory *productionTrackerFactory) Build(ctx context.Context, raw workflow.TrackerConfig, resolver secrets.Resolver) (tracker.Adapter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	provider, err := tracker.DecodeConfig(raw)
	if err != nil {
		return nil, &tracker.Error{Category: tracker.CategoryConfig, Message: "Tracker configuration is invalid."}
	}
	if factory.redactor != nil {
		factory.redactor.RegisterEnvironmentNames(provider.SecretEnvironmentNames(), observability.LookupEnv(factory.lookupEnv))
	}
	if resolver == nil {
		return nil, &tracker.Error{Category: tracker.CategoryAuth, Message: "Tracker credential is unavailable."}
	}
	credential := provider.Credential()
	token, err := resolver.Resolve(ctx, secrets.Ref{WorkflowID: factory.workflowID, TrackerKind: provider.Kind()}, credential.Reference)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &tracker.Error{Category: tracker.CategoryAuth, Message: "Tracker credential is unavailable."}
	}
	defer clear(token)
	if factory.redactor != nil {
		factory.redactor.RegisterSecret(token)
	}
	client := &http.Client{}
	switch config := provider.(type) {
	case tracker.GitHubConfig:
		return trackergithub.New(config, token, client, factory.logger)
	case tracker.LinearConfig:
		return trackerlinear.New(config, token, client, factory.logger)
	default:
		return nil, &tracker.Error{Category: tracker.CategoryConfig, Message: "Tracker kind is unsupported."}
	}
}

type productionCredentialResolver struct {
	vault     secrets.Store
	lookupEnv workflow.LookupEnv
}

func (resolver productionCredentialResolver) Resolve(ctx context.Context, ref secrets.Ref, reference string) ([]byte, error) {
	if strings.HasPrefix(reference, "$") && len(reference) > 1 {
		if resolver.lookupEnv == nil {
			return nil, secrets.ErrNotFound
		}
		value, found := resolver.lookupEnv(strings.TrimPrefix(reference, "$"))
		if !found || value == "" {
			return nil, secrets.ErrNotFound
		}
		return []byte(value), nil
	}
	if reference != "" && reference != "os-vault" {
		return nil, secrets.ErrNotFound
	}
	if resolver.vault == nil {
		return nil, secrets.ErrNotFound
	}
	value, err := resolver.vault.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer clear(value)
	return append([]byte(nil), value...), nil
}

type preflightState struct {
	valid    bool
	snapshot workflow.Snapshot
	scope    string
	digest   string
}

func startWithDependencies(ctx context.Context, options Options, _, stderr io.Writer, deps startDependencies) error {
	if err := ctx.Err(); err != nil {
		return nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		store, err := deps.newStore(context.Background(), options.WorkflowPath, deps.lookupEnv, app.ValidateTracker)
		if err != nil {
			return &StartupError{Code: "workflow_store_failed", Message: "Symphony could not prepare the workflow editor. Review the workflow directory and try again."}
		}

		before, err := loadPreflight(ctx, store, options.Mode, options.WorkflowPath)
		if err != nil {
			return joinSafe(err, store.Close())
		}
		info, err := deps.resolveInstance(options.WorkflowPath, before.scope, options.DataDir)
		if err != nil {
			return joinSafe(&StartupError{Code: "instance_identity_failed", Message: "Symphony could not resolve a stable workflow identity."}, store.Close())
		}
		lock, err := deps.acquireLock(info)
		if err != nil {
			code, message := "instance_lock_failed", "Symphony could not acquire the workflow lock."
			if errors.Is(err, instance.ErrAlreadyRunning) {
				code, message = "workflow_already_running", "This workflow is already running. Use its existing Symphony window or stop it before starting another copy."
			}
			return joinSafe(&StartupError{Code: code, Message: message}, store.Close())
		}

		after, afterErr := loadPreflight(ctx, store, options.Mode, options.WorkflowPath)
		if afterErr != nil {
			return joinSafe(afterErr, store.Close(), lock.Release())
		}
		if before.digest != after.digest || before.scope != after.scope {
			cleanupErr := joinSafe(nil, store.Close(), lock.Release())
			if cleanupErr != nil {
				return cleanupErr
			}
			continue
		}

		vault := deps.newVault()
		requestedPort := selectedPort(options, after)
		redactor := observability.NewRedactor(nil, observability.LookupEnv(deps.lookupEnv))
		logger, logs, err := deps.newLogger(observability.Options{
			DataDir: info.DataDir, Redactor: redactor, Stderr: stderr,
		})
		if err != nil || logger == nil || logs == nil {
			return joinSafe(&StartupError{Code: "observability_failed", Message: "Symphony could not prepare safe local logging."}, store.Close(), closeLogStore(deps, logs), lock.Release())
		}
		factory := newProductionTrackerFactory(info.WorkflowID, deps.lookupEnv, redactor, logger)
		resolver := productionCredentialResolver{vault: vault, lookupEnv: deps.lookupEnv}
		journal := observability.NewJournal(observability.JournalOptions{})
		queue := deps.newRuntime(app.QueueOptions{
			Enabled: options.Mode == ModeRun, Store: store, Factory: factory, Resolver: resolver,
			Journal: journal, Logger: logger,
		})
		if queue == nil {
			return joinSafe(&StartupError{Code: "queue_runtime_failed", Message: "Symphony could not prepare the live work queue."}, store.Close(), closeLogStore(deps, logs), lock.Release())
		}
		service := app.NewConfigService(app.ConfigServiceOptions{
			Path: info.WorkflowPath, Store: store, Vault: vault, WorkflowID: info.WorkflowID,
			RequestedPort: requestedPort, PortOverride: options.PortSet, OnCredentialChanged: queue.NotifyCredentialChanged,
		})
		if err := queue.Start(ctx); err != nil {
			return joinSafe(&StartupError{Code: "queue_runtime_failed", Message: "Symphony could not start the live work queue."}, shutdownQueue(queue), store.Close(), closeLogStore(deps, logs), lock.Release())
		}
		handler, responder, err := deps.newHandler(service, string(options.Mode), queue, queue, logs)
		if err != nil {
			return joinSafe(&StartupError{Code: "web_handler_failed", Message: "Symphony could not prepare the local configuration interface."}, shutdownQueue(queue), store.Close(), closeLogStore(deps, logs), lock.Release())
		}
		bootstrap, err := deps.newBootstrap()
		if err != nil {
			return joinSafe(&StartupError{Code: "web_bootstrap_failed", Message: "Symphony could not create a protected browser launch."}, shutdownQueue(queue), store.Close(), closeLogStore(deps, logs), lock.Release())
		}
		server, err := deps.newServer(web.Options{
			Port: requestedPort, Bootstrap: bootstrap, Handler: handler, ErrorResponder: responder, Logger: logger,
		})
		if err != nil {
			return joinSafe(&StartupError{Code: "web_server_failed", Message: "Symphony could not prepare the protected loopback server."}, shutdownQueue(queue), store.Close(), closeLogStore(deps, logs), lock.Release())
		}
		bound, err := server.Start(context.Background())
		if err != nil {
			return joinSafe(&StartupError{Code: "web_bind_failed", Message: "Symphony could not bind the requested loopback port."}, shutdownServer(server), shutdownQueue(queue), store.Close(), closeLogStore(deps, logs), lock.Release())
		}
		fmt.Fprintln(stderr, bound.URL)
		fmt.Fprintf(stderr, "Symphony %s mode is ready on loopback port %d.\n", options.Mode, bound.Port)

		var runtimeErr error
		if options.OpenBrowser {
			if err := deps.openBrowser(bound.URL); err != nil {
				runtimeErr = &StartupError{Code: "browser_open_failed", Message: "Symphony could not open the protected URL. Open the printed URL in a supported local browser."}
			}
		}
		if runtimeErr == nil {
			select {
			case <-ctx.Done():
			case err := <-server.Done():
				if err != nil {
					runtimeErr = joinSafe(&StartupError{Code: "web_runtime_failed", Message: "The protected loopback server stopped unexpectedly."}, err)
				} else if ctx.Err() == nil {
					runtimeErr = &StartupError{Code: "web_runtime_stopped", Message: "The protected loopback server stopped before shutdown was requested."}
				}
			}
		}
		return joinSafe(runtimeErr, shutdownServer(server), shutdownQueue(queue), store.Close(), closeLogStore(deps, logs), lock.Release())
	}
	return &StartupError{Code: "workflow_changed_during_startup", Message: "WORKFLOW.md changed repeatedly during startup. Wait for edits to finish and try again."}
}

func shutdownServer(server runtimeServer) error {
	if server == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}

func shutdownQueue(runtime queueRuntime) error {
	if runtime == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return runtime.Shutdown(shutdownCtx)
}

func closeLogStore(deps startDependencies, store *observability.LogStore) error {
	if store == nil {
		return nil
	}
	return deps.closeLogs(store)
}

func loadPreflight(ctx context.Context, store workflowStore, mode Mode, path string) (preflightState, error) {
	snapshot, err := store.Load(ctx)
	if err == nil {
		provider, decodeErr := trackerConfig(snapshot)
		if decodeErr != nil {
			return preflightState{}, startupValidationError(workflow.ValidationResult{Valid: false, FieldErrors: app.ValidateTracker(snapshot.Config)})
		}
		selection, selectErr := app.SelectTracker(provider)
		if selectErr != nil {
			return preflightState{}, &StartupError{Code: "invalid_tracker_config", Message: "Tracker configuration is invalid. Run symphony configure to review it."}
		}
		return preflightState{valid: true, snapshot: snapshot, scope: selection.Scope, digest: snapshot.Digest}, nil
	}
	if mode == ModeConfigure && (errors.Is(err, workflow.ErrMissingWorkflow) || errors.Is(err, workflow.ErrInvalidWorkflow) || errors.Is(err, workflow.ErrWorkflowParse) || errors.Is(err, workflow.ErrFrontMatterNotMap) || errors.Is(err, workflow.ErrTemplateParse)) {
		return preflightState{digest: invalidFileDigest(path)}, nil
	}
	if errors.Is(err, workflow.ErrMissingWorkflow) {
		return preflightState{}, &StartupError{Code: "missing_workflow_file", Message: "WORKFLOW.md is missing. Run symphony configure to create it."}
	}
	var invalid *workflow.InvalidWorkflowError
	if errors.As(err, &invalid) {
		return preflightState{}, startupValidationError(invalid.Validation)
	}
	return preflightState{}, &StartupError{Code: "invalid_workflow", Message: "WORKFLOW.md is invalid. Run symphony configure to repair it."}
}

func trackerConfig(snapshot workflow.Snapshot) (tracker.ProviderConfig, error) {
	return tracker.DecodeConfig(snapshot.Config.Tracker)
}

func startupValidationError(validation workflow.ValidationResult) error {
	code := "invalid_workflow"
	if len(validation.GlobalErrors) > 0 && validation.GlobalErrors[0].Code != "" {
		code = validation.GlobalErrors[0].Code
	} else if len(validation.FieldErrors) > 0 && validation.FieldErrors[0].Code != "" {
		code = validation.FieldErrors[0].Code
	}
	return &StartupError{Code: code, Message: "WORKFLOW.md is invalid. Run symphony configure to review and repair it."}
}

func invalidFileDigest(path string) string {
	if path == "" {
		return ""
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(contents))
}

func selectedPort(options Options, state preflightState) int {
	if options.PortSet {
		return options.Port
	}
	if state.valid {
		return state.snapshot.Config.Server.Port
	}
	return 0
}
