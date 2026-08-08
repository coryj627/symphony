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
	"time"

	"github.com/coryj627/symphony/go/internal/app"
	"github.com/coryj627/symphony/go/internal/instance"
	"github.com/coryj627/symphony/go/internal/secrets"
	"github.com/coryj627/symphony/go/internal/tracker"
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

type startDependencies struct {
	newStore        func(context.Context, string, workflow.LookupEnv, workflow.ProviderValidator) (workflowStore, error)
	resolveInstance func(string, string, string) (instance.Info, error)
	acquireLock     func(instance.Info) (instanceLock, error)
	newVault        func() secrets.Store
	newHandler      func(*app.ConfigService, string) (http.Handler, web.ErrorResponder, error)
	newBootstrap    func() (web.Bootstrap, error)
	newServer       func(web.Options) (runtimeServer, error)
	openBrowser     func(string) error
	lookupEnv       workflow.LookupEnv
	logger          *slog.Logger
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
		newVault: newVaultStore,
		newHandler: func(service *app.ConfigService, mode string) (http.Handler, web.ErrorResponder, error) {
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
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
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
		service := app.NewConfigService(app.ConfigServiceOptions{
			Path: info.WorkflowPath, Store: store, Vault: vault, WorkflowID: info.WorkflowID,
			RequestedPort: requestedPort, PortOverride: options.PortSet,
		})
		handler, responder, err := deps.newHandler(service, string(options.Mode))
		if err != nil {
			return joinSafe(&StartupError{Code: "web_handler_failed", Message: "Symphony could not prepare the local configuration interface."}, store.Close(), lock.Release())
		}
		bootstrap, err := deps.newBootstrap()
		if err != nil {
			return joinSafe(&StartupError{Code: "web_bootstrap_failed", Message: "Symphony could not create a protected browser launch."}, store.Close(), lock.Release())
		}
		server, err := deps.newServer(web.Options{
			Port: requestedPort, Bootstrap: bootstrap, Handler: handler, ErrorResponder: responder, Logger: deps.logger,
		})
		if err != nil {
			return joinSafe(&StartupError{Code: "web_server_failed", Message: "Symphony could not prepare the protected loopback server."}, store.Close(), lock.Release())
		}
		bound, err := server.Start(context.Background())
		if err != nil {
			return joinSafe(&StartupError{Code: "web_bind_failed", Message: "Symphony could not bind the requested loopback port."}, store.Close(), lock.Release())
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		shutdownErr := server.Shutdown(shutdownCtx)
		cancel()
		return joinSafe(runtimeErr, shutdownErr, store.Close(), lock.Release())
	}
	return &StartupError{Code: "workflow_changed_during_startup", Message: "WORKFLOW.md changed repeatedly during startup. Wait for edits to finish and try again."}
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
