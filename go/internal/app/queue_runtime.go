package app

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/secrets"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

const defaultQueuePollInterval = 30 * time.Second

type QueueOptions struct {
	Enabled  bool
	Store    workflow.Store
	Factory  tracker.Factory
	Resolver secrets.Resolver
	Journal  *observability.Journal
	Logger   *slog.Logger
}

type queueDependencies struct {
	now                 func() time.Time
	after               func(time.Duration) <-chan time.Time
	jitter              func(time.Duration) time.Duration
	beforeManualRebuild func()
}

type refreshFlight struct {
	done            chan struct{}
	once            sync.Once
	err             error
	previousTracker domain.TrackerStatus
}

func newRefreshFlight() *refreshFlight {
	return &refreshFlight{done: make(chan struct{})}
}

func (flight *refreshFlight) complete(err error) {
	flight.once.Do(func() {
		flight.err = err
		close(flight.done)
	})
}

type refreshGeneration struct {
	number         uint64
	ctx            context.Context
	adapter        tracker.Adapter
	activeStates   []string
	requiredLabels []string
}

type QueueRuntime struct {
	options QueueOptions
	deps    queueDependencies

	mu          sync.Mutex
	operationMu sync.Mutex
	wg          sync.WaitGroup

	started     bool
	startErr    error
	closed      bool
	startupDone chan struct{}
	startupOnce sync.Once
	done        chan struct{}
	runtimeCtx  context.Context
	cancel      context.CancelFunc

	generation       uint64
	generationCtx    context.Context
	generationCancel context.CancelFunc
	adapter          tracker.Adapter
	activeStates     []string
	requiredLabels   []string
	pollInterval     time.Duration
	activeScope      string
	currentSnapshot  workflow.Snapshot
	hasSnapshot      bool
	inFlight         *refreshFlight
	rebuildFlight    *refreshFlight
	manualRebuild    *refreshFlight

	candidates []domain.CandidateRow
	issues     map[string]domain.IssueDetail
	config     domain.ConfigStatus
	tracker    domain.TrackerStatus

	autoSuppressed bool
	retryAt        *time.Time
	wake           chan struct{}
	credentials    chan struct{}
}

func NewQueueRuntime(options QueueOptions) *QueueRuntime {
	return newQueueRuntimeWithDependencies(options, queueDependencies{
		now: time.Now, after: time.After, jitter: randomQueueJitter,
	})
}

func newQueueRuntimeWithDependencies(options QueueOptions, dependencies queueDependencies) *QueueRuntime {
	if dependencies.now == nil {
		dependencies.now = time.Now
	}
	if dependencies.after == nil {
		dependencies.after = time.After
	}
	if dependencies.jitter == nil {
		dependencies.jitter = randomQueueJitter
	}
	if options.Journal == nil {
		options.Journal = observability.NewJournal(observability.JournalOptions{})
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	now := dependencies.now().UTC()
	configState := "starting"
	trackerState := "starting"
	if !options.Enabled {
		configState = "disabled"
		trackerState = "disabled"
	}
	return &QueueRuntime{
		options: options, deps: dependencies,
		startupDone: make(chan struct{}), wake: make(chan struct{}, 1), credentials: make(chan struct{}, 1),
		pollInterval: defaultQueuePollInterval,
		candidates:   []domain.CandidateRow{}, issues: make(map[string]domain.IssueDetail),
		config:  domain.ConfigStatus{State: configState, ChangedAt: now},
		tracker: domain.TrackerStatus{State: trackerState},
	}
}

func (runtime *QueueRuntime) Start(ctx context.Context) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	runtime.mu.Lock()
	if runtime.closed {
		runtime.mu.Unlock()
		return ErrUnavailableInPhase
	}
	if runtime.started {
		startupDone := runtime.startupDone
		runtime.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-startupDone:
			runtime.mu.Lock()
			defer runtime.mu.Unlock()
			return runtime.startErr
		}
	}
	runtime.started = true
	runtime.runtimeCtx, runtime.cancel = context.WithCancel(context.Background())
	enabled := runtime.options.Enabled
	runtime.mu.Unlock()
	defer func() {
		runtime.mu.Lock()
		runtime.startErr = result
		runtime.mu.Unlock()
		runtime.finishStartup()
	}()
	if !enabled {
		return nil
	}
	if runtime.options.Store == nil || runtime.options.Factory == nil || runtime.options.Resolver == nil {
		return errors.New("queue_runtime_dependencies_unavailable")
	}

	snapshot, available := runtime.options.Store.Current()
	if !available {
		var err error
		snapshot, err = runtime.options.Store.Load(ctx)
		if err != nil {
			return errors.New("queue_workflow_unavailable")
		}
	}
	if err := runtime.rebuildSnapshot(ctx, snapshot, false, 0, false); err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		// Provider failures are observable runtime state; the local read-only UI
		// remains available so a workflow or credential change can recover it.
	}

	runtime.mu.Lock()
	if runtime.closed {
		runtime.mu.Unlock()
		return context.Canceled
	}
	runtimeCtx := runtime.runtimeCtx
	runtime.wg.Add(2)
	runtime.mu.Unlock()
	changes := runtime.options.Store.Changes()
	go runtime.controlLoop(runtimeCtx, changes)
	go runtime.pollLoop(runtimeCtx)
	return nil
}

func (runtime *QueueRuntime) Shutdown(ctx context.Context) error {
	runtime.mu.Lock()
	if !runtime.closed {
		runtime.closed = true
		if !runtime.started {
			runtime.finishStartup()
		}
		if runtime.cancel != nil {
			runtime.cancel()
		}
		runtime.retireGenerationLocked()
		runtime.options.Journal.Close()
		runtime.done = make(chan struct{})
		startupDone := runtime.startupDone
		done := runtime.done
		go func() {
			<-startupDone
			runtime.wg.Wait()
			close(done)
		}()
	}
	done := runtime.done
	runtime.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (runtime *QueueRuntime) Snapshot(ctx context.Context) (domain.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return domain.Snapshot{}, err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	snapshot := domain.EmptySnapshot()
	snapshot.GeneratedAt = runtime.deps.now().UTC()
	snapshot.EventCursor = runtime.options.Journal.Cursor()
	snapshot.Scheduler = domain.SchedulerStatus{
		Available: false, Enabled: false, State: "unavailable",
		Message: "The scheduler is unavailable in Phase 2.",
	}
	snapshot.Candidates = cloneCandidateRows(runtime.candidates)
	snapshot.Config = runtime.config
	snapshot.Tracker = cloneTrackerStatus(runtime.tracker)
	return snapshot.Clone()
}

func (runtime *QueueRuntime) Issue(ctx context.Context, identifier string) (domain.IssueDetail, error) {
	if err := ctx.Err(); err != nil {
		return domain.IssueDetail{}, err
	}
	key := normalizedIssueIdentifier(identifier)
	runtime.mu.Lock()
	detail, found := runtime.issues[key]
	runtime.mu.Unlock()
	if !found {
		return domain.IssueDetail{}, ErrIssueNotFound
	}
	return detail.Clone()
}

func (runtime *QueueRuntime) EventsAfter(ctx context.Context, cursor domain.EventCursor) (domain.EventPage, error) {
	if err := ctx.Err(); err != nil {
		return domain.EventPage{}, err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.options.Journal.After(cursor), nil
}

func (runtime *QueueRuntime) SubscribeEvents(cursor domain.EventCursor) <-chan struct{} {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.options.Journal.Subscribe(cursor)
}

func (runtime *QueueRuntime) Refresh(ctx context.Context) (domain.RefreshReceipt, error) {
	receipt := domain.RefreshReceipt{
		RequestedAt: runtime.deps.now().UTC(), Operations: []string{"poll"},
	}
	runtime.mu.Lock()
	if !runtime.options.Enabled || !runtime.started || runtime.closed {
		runtime.mu.Unlock()
		return receipt, ErrUnavailableInPhase
	}
	if err := ctx.Err(); err != nil && runtime.inFlight == nil && runtime.rebuildFlight == nil && runtime.manualRebuild == nil {
		runtime.mu.Unlock()
		return receipt, err
	}
	if runtime.rebuildFlight != nil {
		flight := runtime.rebuildFlight
		runtime.mu.Unlock()
		receipt.Coalesced = true
		select {
		case <-ctx.Done():
			return receipt, ctx.Err()
		case <-flight.done:
			return receipt, flight.err
		}
	}
	if runtime.manualRebuild != nil {
		flight := runtime.manualRebuild
		runtime.mu.Unlock()
		receipt.Coalesced = true
		select {
		case <-ctx.Done():
			return receipt, ctx.Err()
		case <-flight.done:
			return receipt, flight.err
		}
	}
	if runtime.retryAt != nil && runtime.deps.now().UTC().Before(*runtime.retryAt) {
		runtime.mu.Unlock()
		return receipt, nil
	}
	needsRebuild := runtime.adapter == nil || runtime.autoSuppressed
	if needsRebuild {
		flight := newRefreshFlight()
		runtime.manualRebuild = flight
		rebuildCtx := runtime.runtimeCtx
		expectedGeneration := runtime.generation
		runtime.wg.Add(1)
		runtime.mu.Unlock()
		receipt.Queued = true
		go runtime.runManualRebuild(rebuildCtx, flight, expectedGeneration)
		select {
		case <-ctx.Done():
			return receipt, ctx.Err()
		case <-flight.done:
			return receipt, flight.err
		}
	}
	runtime.mu.Unlock()

	flight, generation, leader, err := runtime.beginRefresh()
	if err != nil {
		return receipt, err
	}
	receipt.Queued = leader
	receipt.Coalesced = !leader
	if leader {
		go runtime.runFetch(generation, flight)
	}
	select {
	case <-ctx.Done():
		return receipt, ctx.Err()
	case <-flight.done:
		return receipt, flight.err
	}
}

func (runtime *QueueRuntime) runManualRebuild(ctx context.Context, flight *refreshFlight, expectedGeneration uint64) {
	defer runtime.wg.Done()
	if runtime.deps.beforeManualRebuild != nil {
		runtime.deps.beforeManualRebuild()
	}
	err := runtime.rebuildCurrentForGeneration(ctx, expectedGeneration)
	runtime.mu.Lock()
	if runtime.manualRebuild == flight {
		runtime.manualRebuild = nil
	}
	runtime.mu.Unlock()
	flight.complete(err)
	runtime.signalWake()
}

func (runtime *QueueRuntime) SetScheduler(ctx context.Context, enabled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if enabled {
		return ErrUnavailableInPhase
	}
	return nil
}

func (runtime *QueueRuntime) Respond(ctx context.Context, _ domain.OperatorResponse) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrUnavailableInPhase
}

func (runtime *QueueRuntime) NotifyCredentialChanged() {
	if !runtime.options.Enabled {
		return
	}
	runtime.mu.Lock()
	if runtime.closed || !runtime.started {
		runtime.mu.Unlock()
		return
	}
	runtime.retireGenerationLocked()
	runtime.adapter = nil
	runtime.autoSuppressed = false
	runtime.retryAt = nil
	runtime.tracker.Stale = len(runtime.candidates) > 0
	runtime.tracker.State = "rebuilding"
	runtime.mu.Unlock()
	select {
	case runtime.credentials <- struct{}{}:
	default:
	}
	runtime.signalWake()
}

func (runtime *QueueRuntime) controlLoop(ctx context.Context, changes <-chan workflow.Change) {
	defer runtime.wg.Done()
	for changes != nil || runtime.credentials != nil {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-runtime.credentials:
			if !ok {
				return
			}
			_ = runtime.rebuildCurrent(ctx)
		case change, ok := <-changes:
			if !ok {
				changes = nil
				continue
			}
			runtime.handleWorkflowChange(ctx, change)
		}
	}
}

func (runtime *QueueRuntime) pollLoop(ctx context.Context) {
	defer runtime.wg.Done()
	for {
		delay, scheduled := runtime.nextAutomaticDelay()
		var timer <-chan time.Time
		if scheduled {
			timer = runtime.deps.after(delay)
		}
		select {
		case <-ctx.Done():
			return
		case <-runtime.wake:
			continue
		case <-timer:
			runtime.startAutomaticRefresh()
		}
	}
}

func (runtime *QueueRuntime) handleWorkflowChange(ctx context.Context, change workflow.Change) {
	if !change.Validation.Valid || change.Snapshot.Digest == "" {
		runtime.mu.Lock()
		next := runtime.config
		next.State = "invalid"
		next.Digest = change.Digest
		next.UsingLastGood = runtime.adapter != nil
		next.ErrorCode = stableValidationCode(change.Validation)
		next.Message = "Workflow configuration is invalid."
		next.ChangedAt = runtime.deps.now().UTC()
		if _, err := runtime.options.Journal.Publish(domain.Event{Type: "configuration.changed", Data: map[string]any{
			"status": "invalid", "error_code": next.ErrorCode,
		}}); err != nil {
			runtime.mu.Unlock()
			return
		}
		runtime.config = next
		runtime.mu.Unlock()
		runtime.signalWake()
		return
	}
	_ = runtime.rebuildSnapshot(ctx, change.Snapshot, true, 0, false)
}

func (runtime *QueueRuntime) rebuildCurrent(ctx context.Context) error {
	snapshot, available := runtime.options.Store.Current()
	if !available {
		var err error
		snapshot, err = runtime.options.Store.Load(ctx)
		if err != nil {
			return runtime.recordBuildFailure(err, "tracker_config")
		}
	}
	return runtime.rebuildSnapshot(ctx, snapshot, false, 0, false)
}

func (runtime *QueueRuntime) rebuildCurrentForGeneration(ctx context.Context, expectedGeneration uint64) error {
	snapshot, available := runtime.options.Store.Current()
	if !available {
		var err error
		snapshot, err = runtime.options.Store.Load(ctx)
		if err != nil {
			if !runtime.generationIsCurrent(expectedGeneration) {
				return nil
			}
			return runtime.recordBuildFailure(err, "tracker_config")
		}
	}
	return runtime.rebuildSnapshot(ctx, snapshot, false, expectedGeneration, true)
}

func (runtime *QueueRuntime) generationIsCurrent(expectedGeneration uint64) bool {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return !runtime.closed && runtime.generation == expectedGeneration
}

func (runtime *QueueRuntime) rebuildSnapshot(ctx context.Context, snapshot workflow.Snapshot, publishConfiguration bool, expectedGeneration uint64, requireCurrentGeneration bool) (result error) {
	runtime.operationMu.Lock()
	if err := ctx.Err(); err != nil {
		runtime.operationMu.Unlock()
		return err
	}
	if requireCurrentGeneration && !runtime.generationIsCurrent(expectedGeneration) {
		runtime.operationMu.Unlock()
		return nil
	}
	rebuild := newRefreshFlight()

	runtime.mu.Lock()
	if runtime.closed {
		runtime.mu.Unlock()
		runtime.operationMu.Unlock()
		return context.Canceled
	}
	runtime.rebuildFlight = rebuild
	runtime.mu.Unlock()
	defer func() {
		runtime.mu.Lock()
		if runtime.rebuildFlight == rebuild {
			runtime.rebuildFlight = nil
		}
		runtime.mu.Unlock()
		rebuild.complete(result)
		runtime.signalWake()
		runtime.operationMu.Unlock()
	}()

	runtime.mu.Lock()
	if runtime.closed {
		runtime.mu.Unlock()
		return context.Canceled
	}
	runtime.mu.Unlock()

	provider, err := tracker.DecodeConfig(snapshot.Config.Tracker)
	if err != nil {
		return runtime.deactivateForBuildFailure(&tracker.Error{Category: tracker.CategoryConfig, Message: "Tracker configuration is unavailable."})
	}
	selection, err := SelectTracker(provider)
	if err != nil {
		return runtime.deactivateForBuildFailure(&tracker.Error{Category: tracker.CategoryConfig, Message: "Tracker configuration is unavailable."})
	}

	runtime.mu.Lock()
	if runtime.closed {
		runtime.mu.Unlock()
		return context.Canceled
	}
	previousScope := runtime.activeScope
	previousConfig := runtime.config
	previousTracker := runtime.tracker
	previousCandidates := runtime.candidates
	previousIssues := runtime.issues
	runtime.config.State = "valid"
	runtime.config.Digest = snapshot.Digest
	runtime.config.ActiveDigest = ""
	runtime.config.UsingLastGood = false
	runtime.config.ErrorCode = ""
	runtime.config.Message = ""
	runtime.config.ChangedAt = runtime.deps.now().UTC()
	runtime.activeScope = selection.Scope
	runtime.tracker.Kind = selection.Kind
	runtime.tracker.Scope = selection.Scope
	runtime.tracker.State = "rebuilding"
	runtime.tracker.Stale = len(runtime.candidates) > 0
	if previousScope != "" && previousScope != selection.Scope {
		runtime.candidates = []domain.CandidateRow{}
		runtime.issues = make(map[string]domain.IssueDetail)
		runtime.tracker.Stale = false
	}
	if publishConfiguration {
		if _, err := runtime.options.Journal.Publish(domain.Event{Type: "configuration.changed", Data: map[string]any{
			"status": "valid",
		}}); err != nil {
			runtime.config = previousConfig
			runtime.tracker = previousTracker
			runtime.activeScope = previousScope
			runtime.candidates = previousCandidates
			runtime.issues = previousIssues
			runtime.mu.Unlock()
			return err
		}
	}
	runtime.retireGenerationLocked()
	runtime.adapter = nil
	runtime.autoSuppressed = false
	runtime.retryAt = nil
	runtime.generation++
	generation := runtime.generation
	generationCtx, cancel := context.WithCancel(runtime.runtimeCtx)
	runtime.generationCancel = cancel
	runtime.generationCtx = generationCtx
	runtime.mu.Unlock()

	adapter, err := runtime.options.Factory.Build(generationCtx, cloneRuntimeTrackerConfig(snapshot.Config.Tracker), runtime.options.Resolver)
	if err != nil || adapter == nil {
		cancel()
		if err == nil {
			err = errors.New("tracker_adapter_unavailable")
		}
		return runtime.recordBuildFailureForGeneration(generation, err)
	}

	runtime.mu.Lock()
	if runtime.closed || runtime.generation != generation {
		runtime.mu.Unlock()
		cancel()
		return context.Canceled
	}
	runtime.adapter = adapter
	runtime.activeStates = append([]string(nil), providerActiveStates(provider)...)
	runtime.requiredLabels = append([]string(nil), snapshot.Config.Tracker.RequiredLabels...)
	runtime.pollInterval = snapshot.Config.Polling.Interval
	if runtime.pollInterval <= 0 {
		runtime.pollInterval = defaultQueuePollInterval
	}
	runtime.currentSnapshot = cloneRuntimeSnapshot(snapshot)
	runtime.hasSnapshot = true
	flight := newRefreshFlight()
	flight.previousTracker = cloneTrackerStatus(runtime.tracker)
	runtime.config.ActiveDigest = snapshot.Digest
	if runtime.config.State == "invalid" {
		runtime.config.UsingLastGood = true
	}
	runtime.tracker.State = "refreshing"
	runtime.inFlight = flight
	refresh := refreshGeneration{
		number: generation, ctx: generationCtx, adapter: adapter,
		activeStates:   append([]string(nil), runtime.activeStates...),
		requiredLabels: append([]string(nil), runtime.requiredLabels...),
	}
	runtime.mu.Unlock()
	runtime.runFetchSync(refresh, flight)
	return flight.err
}

func (runtime *QueueRuntime) beginRefresh() (*refreshFlight, refreshGeneration, bool, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.closed || runtime.adapter == nil {
		return nil, refreshGeneration{}, false, ErrUnavailableInPhase
	}
	if runtime.inFlight != nil {
		return runtime.inFlight, refreshGeneration{}, false, nil
	}
	flight := newRefreshFlight()
	flight.previousTracker = cloneTrackerStatus(runtime.tracker)
	runtime.inFlight = flight
	runtime.tracker.State = "refreshing"
	runtime.wg.Add(1)
	return flight, refreshGeneration{
		number: runtime.generation, ctx: runtime.runtimeCtxForGeneration(), adapter: runtime.adapter,
		activeStates:   append([]string(nil), runtime.activeStates...),
		requiredLabels: append([]string(nil), runtime.requiredLabels...),
	}, true, nil
}

func (runtime *QueueRuntime) runtimeCtxForGeneration() context.Context {
	if runtime.generationCtx == nil {
		return runtime.runtimeCtx
	}
	return runtime.generationCtx
}

func (runtime *QueueRuntime) runFetch(generation refreshGeneration, flight *refreshFlight) {
	defer runtime.wg.Done()
	runtime.runFetchSync(generation, flight)
}

func (runtime *QueueRuntime) runFetchSync(generation refreshGeneration, flight *refreshFlight) {
	if err := generation.ctx.Err(); err != nil {
		runtime.finishRefresh(generation.number, flight, nil, nil, err)
		return
	}
	issues, err := generation.adapter.FetchIssuesByStates(generation.ctx, append([]string(nil), generation.activeStates...))
	var candidates []domain.CandidateRow
	var details map[string]domain.IssueDetail
	if err == nil {
		candidates, details, err = normalizeProviderIssues(issues, generation.requiredLabels)
	}
	runtime.finishRefresh(generation.number, flight, candidates, details, err)
}

func (runtime *QueueRuntime) finishRefresh(generation uint64, flight *refreshFlight, candidates []domain.CandidateRow, details map[string]domain.IssueDetail, providerErr error) {
	runtime.mu.Lock()
	if runtime.closed || runtime.generation != generation {
		if runtime.inFlight == flight {
			runtime.inFlight = nil
		}
		runtime.mu.Unlock()
		flight.complete(context.Canceled)
		return
	}
	if errors.Is(providerErr, context.Canceled) {
		if runtime.inFlight == flight {
			runtime.inFlight = nil
		}
		runtime.mu.Unlock()
		flight.complete(context.Canceled)
		return
	}
	now := runtime.deps.now().UTC()
	if providerErr == nil {
		if _, err := runtime.options.Journal.Publish(domain.Event{Type: "queue.refreshed", Data: map[string]any{
			"status": "ready", "candidate_count": len(candidates),
		}}); err != nil {
			if runtime.inFlight == flight {
				runtime.inFlight = nil
			}
			runtime.tracker = cloneTrackerStatus(flight.previousTracker)
			runtime.mu.Unlock()
			flight.complete(err)
			runtime.signalWake()
			return
		}
		runtime.candidates = cloneCandidateRows(candidates)
		runtime.issues = cloneIssueDetails(details)
		runtime.tracker.LastAttemptAt = timePointer(now)
		runtime.tracker.State = "ready"
		runtime.tracker.Stale = false
		runtime.tracker.Retryable = false
		runtime.tracker.ErrorCode = ""
		runtime.tracker.Message = ""
		runtime.tracker.RetryAt = nil
		runtime.tracker.LastSuccessAt = timePointer(now)
		runtime.retryAt = nil
		runtime.autoSuppressed = false
		if runtime.inFlight == flight {
			runtime.inFlight = nil
		}
		runtime.mu.Unlock()
		flight.complete(nil)
		runtime.signalWake()
		return
	}

	portable, returned := safeTrackerFailure(providerErr)
	errorCode := string(portable.Category)
	if errorCode == "" {
		errorCode = "tracker_error"
	}
	retryable := portable.Retryable
	var retryAt *time.Time
	if portable.Category == tracker.CategoryRateLimited {
		delay := boundedRateLimitDelay(portable.RetryAfter, runtime.deps.jitter)
		retryAt = timePointer(now.Add(delay))
		retryable = true
	} else if portable.Retryable {
		delay := runtime.pollInterval
		if delay <= 0 {
			delay = defaultQueuePollInterval
		}
		retryAt = timePointer(now.Add(delay))
	}
	if _, err := runtime.options.Journal.Publish(domain.Event{Type: "queue.failed", Data: map[string]any{
		"status": "failed", "error_code": errorCode, "retryable": retryable,
	}}); err != nil {
		if runtime.inFlight == flight {
			runtime.inFlight = nil
		}
		runtime.tracker = cloneTrackerStatus(flight.previousTracker)
		runtime.mu.Unlock()
		flight.complete(err)
		runtime.signalWake()
		return
	}
	runtime.tracker.LastAttemptAt = timePointer(now)
	runtime.tracker.State = "failed"
	runtime.tracker.Stale = len(runtime.candidates) > 0
	runtime.tracker.Retryable = retryable
	runtime.tracker.ErrorCode = errorCode
	runtime.tracker.Message = portable.Message
	runtime.tracker.RetryAt = nil
	runtime.retryAt = nil
	if retryAt != nil {
		runtime.tracker.RetryAt = timePointer(*retryAt)
		runtime.retryAt = timePointer(*retryAt)
	}
	runtime.autoSuppressed = portable.Category == tracker.CategoryAuth || portable.Category == tracker.CategoryConfig
	runtime.options.Logger.Warn("queue_refresh_failed", slog.String("error_code", runtime.tracker.ErrorCode))
	if runtime.inFlight == flight {
		runtime.inFlight = nil
	}
	runtime.mu.Unlock()
	flight.complete(returned)
	runtime.signalWake()
}

func (runtime *QueueRuntime) startAutomaticRefresh() {
	runtime.mu.Lock()
	if runtime.closed || runtime.adapter == nil || runtime.autoSuppressed || runtime.inFlight != nil {
		runtime.mu.Unlock()
		return
	}
	now := runtime.deps.now().UTC()
	if runtime.retryAt != nil && now.Before(*runtime.retryAt) {
		runtime.mu.Unlock()
		runtime.signalWake()
		return
	}
	flight := newRefreshFlight()
	flight.previousTracker = cloneTrackerStatus(runtime.tracker)
	runtime.inFlight = flight
	runtime.tracker.State = "refreshing"
	generation := refreshGeneration{
		number: runtime.generation, ctx: runtime.runtimeCtxForGeneration(), adapter: runtime.adapter,
		activeStates:   append([]string(nil), runtime.activeStates...),
		requiredLabels: append([]string(nil), runtime.requiredLabels...),
	}
	runtime.wg.Add(1)
	runtime.mu.Unlock()
	go runtime.runFetch(generation, flight)
}

func (runtime *QueueRuntime) nextAutomaticDelay() (time.Duration, bool) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.closed || runtime.adapter == nil || runtime.autoSuppressed || runtime.inFlight != nil {
		return 0, false
	}
	now := runtime.deps.now().UTC()
	if runtime.retryAt != nil && now.Before(*runtime.retryAt) {
		return runtime.retryAt.Sub(now), true
	}
	delay := runtime.pollInterval
	if delay <= 0 {
		delay = defaultQueuePollInterval
	}
	return delay, true
}

func (runtime *QueueRuntime) retireGenerationLocked() {
	if runtime.generationCancel != nil {
		runtime.generationCancel()
		runtime.generationCancel = nil
		runtime.generationCtx = nil
	}
	runtime.generation++
	if runtime.inFlight != nil {
		runtime.inFlight.complete(context.Canceled)
		runtime.inFlight = nil
	}
}

func (runtime *QueueRuntime) recordBuildFailure(err error, fallback tracker.Category) error {
	runtime.mu.Lock()
	generation := runtime.generation
	runtime.mu.Unlock()
	if fallback == "tracker_config" {
		return runtime.recordBuildFailureForGeneration(generation, &tracker.Error{Category: tracker.CategoryConfig, Message: "Tracker configuration is unavailable."})
	}
	return runtime.recordBuildFailureForGeneration(generation, err)
}

func (runtime *QueueRuntime) deactivateForBuildFailure(err error) error {
	runtime.mu.Lock()
	if runtime.closed {
		runtime.mu.Unlock()
		return context.Canceled
	}
	runtime.retireGenerationLocked()
	runtime.adapter = nil
	runtime.autoSuppressed = false
	runtime.retryAt = nil
	generation := runtime.generation
	runtime.mu.Unlock()
	return runtime.recordBuildFailureForGeneration(generation, err)
}

func (runtime *QueueRuntime) recordBuildFailureForGeneration(generation uint64, err error) error {
	portable, returned := safeTrackerFailure(err)
	errorCode := string(portable.Category)
	if errorCode == "" {
		errorCode = "tracker_error"
	}
	runtime.mu.Lock()
	if runtime.closed || runtime.generation != generation {
		runtime.mu.Unlock()
		return context.Canceled
	}
	if _, err := runtime.options.Journal.Publish(domain.Event{Type: "queue.failed", Data: map[string]any{
		"status": "failed", "error_code": errorCode, "retryable": false,
	}}); err != nil {
		runtime.mu.Unlock()
		runtime.signalWake()
		return err
	}
	runtime.adapter = nil
	runtime.tracker.State = "failed"
	runtime.tracker.Stale = len(runtime.candidates) > 0
	runtime.tracker.Retryable = false
	runtime.tracker.ErrorCode = errorCode
	runtime.tracker.Message = portable.Message
	runtime.tracker.RetryAt = nil
	runtime.autoSuppressed = true
	runtime.retryAt = nil
	runtime.mu.Unlock()
	runtime.signalWake()
	return returned
}

func (runtime *QueueRuntime) signalWake() {
	select {
	case runtime.wake <- struct{}{}:
	default:
	}
}

func (runtime *QueueRuntime) finishStartup() {
	runtime.startupOnce.Do(func() { close(runtime.startupDone) })
}

func providerActiveStates(provider tracker.ProviderConfig) []string {
	switch config := provider.(type) {
	case tracker.GitHubConfig:
		return append([]string(nil), config.ActiveStates...)
	case tracker.LinearConfig:
		return append([]string(nil), config.ActiveStates...)
	default:
		return []string{}
	}
}

func normalizeProviderIssues(issues []domain.Issue, requiredLabels []string) ([]domain.CandidateRow, map[string]domain.IssueDetail, error) {
	candidates := make([]domain.CandidateRow, 0, len(issues))
	seenIDs := make(map[string]struct{}, len(issues))
	details := make(map[string]domain.IssueDetail, len(issues))
	for _, source := range issues {
		issue := source
		clone, err := issue.Clone()
		if err != nil && issue.NativeRef != nil {
			issue.NativeRef = nil
			clone, err = issue.Clone()
		}
		if err != nil || clone.ValidateRequired() != nil {
			return nil, nil, &tracker.Error{Category: tracker.CategoryPayload, Message: "Tracker returned an invalid issue payload."}
		}
		if clone.Labels == nil {
			clone.Labels = []string{}
		}
		if clone.BlockedBy == nil {
			clone.BlockedBy = []domain.BlockerRef{}
		}
		if _, duplicate := seenIDs[clone.ID]; duplicate {
			return nil, nil, &tracker.Error{Category: tracker.CategoryPayload, Message: "Tracker returned duplicate issue identities."}
		}
		key := normalizedIssueIdentifier(clone.Identifier)
		if key == "" {
			return nil, nil, &tracker.Error{Category: tracker.CategoryPayload, Message: "Tracker returned an invalid issue identifier."}
		}
		if _, duplicate := details[key]; duplicate {
			return nil, nil, &tracker.Error{Category: tracker.CategoryPayload, Message: "Tracker returned duplicate issue identifiers."}
		}
		seenIDs[clone.ID] = struct{}{}
		routable, reasons := routeIssue(clone, requiredLabels)
		candidate := domain.CandidateRow{Issue: clone, Routable: routable, RoutingReasons: reasons}
		candidates = append(candidates, candidate)
		details[key] = domain.IssueDetail{
			Issue: clone, Status: "candidate", Routable: routable, RoutingReasons: append([]string(nil), reasons...),
		}
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		return candidateBefore(candidates[left].Issue, candidates[right].Issue)
	})
	return candidates, details, nil
}

func routeIssue(issue domain.Issue, requiredLabels []string) (bool, []string) {
	reasons := []string{}
	if !issue.Dispatchable {
		reasons = append(reasons, "provider_not_dispatchable")
	}
	labels := make(map[string]struct{}, len(issue.Labels))
	for _, label := range issue.Labels {
		labels[strings.ToLower(strings.TrimSpace(label))] = struct{}{}
	}
	missing := false
	for _, required := range requiredLabels {
		key := strings.ToLower(strings.TrimSpace(required))
		if key == "" {
			continue
		}
		if _, found := labels[key]; !found {
			missing = true
			break
		}
	}
	if missing {
		reasons = append(reasons, "missing_required_label")
	}
	return len(reasons) == 0, reasons
}

func candidateBefore(left, right domain.Issue) bool {
	leftPriority := candidatePriority(left.Priority)
	rightPriority := candidatePriority(right.Priority)
	if leftPriority != rightPriority {
		return leftPriority < rightPriority
	}
	if left.CreatedAt == nil && right.CreatedAt != nil {
		return false
	}
	if left.CreatedAt != nil && right.CreatedAt == nil {
		return true
	}
	if left.CreatedAt != nil && right.CreatedAt != nil && !left.CreatedAt.Equal(*right.CreatedAt) {
		return left.CreatedAt.Before(*right.CreatedAt)
	}
	return left.Identifier < right.Identifier
}

func candidatePriority(priority *int) int {
	if priority != nil && *priority >= 1 && *priority <= 4 {
		return *priority
	}
	return 5
}

func normalizedIssueIdentifier(identifier string) string {
	return strings.ToLower(strings.TrimSpace(identifier))
}

func safeTrackerFailure(err error) (tracker.Error, error) {
	var portable *tracker.Error
	if errors.As(err, &portable) && portable != nil {
		clone := tracker.Error{
			Category: portable.Category, Retryable: portable.Retryable,
			RetryAfter: portable.RetryAfter, Status: portable.Status,
			Message: trackerFailureMessage(portable.Category),
		}
		return clone, &clone
	}
	fallback := tracker.Error{Message: "Tracker operation failed."}
	return fallback, errors.New("tracker_error")
}

func trackerFailureMessage(category tracker.Category) string {
	switch category {
	case tracker.CategoryConfig:
		return "Tracker configuration is unavailable."
	case tracker.CategoryAuth:
		return "Tracker authentication is unavailable."
	case tracker.CategoryTransport:
		return "Tracker transport is unavailable."
	case tracker.CategoryResponse:
		return "Tracker returned an unsuccessful response."
	case tracker.CategoryPayload:
		return "Tracker returned an invalid payload."
	case tracker.CategoryPagination:
		return "Tracker pagination could not be completed."
	case tracker.CategoryRateLimited:
		return "Tracker rate limit is active."
	default:
		return "Tracker operation failed."
	}
}

func boundedRateLimitDelay(providerDelay time.Duration, jitter func(time.Duration) time.Duration) time.Duration {
	base := providerDelay
	if base <= 0 {
		base = time.Minute
	}
	if base > 24*time.Hour {
		base = 24 * time.Hour
	}
	offset := jitter(base)
	if offset < 0 {
		offset = 0
	}
	maximumOffset := base / 10
	if offset > maximumOffset {
		offset = maximumOffset
	}
	delay := base + offset
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	return delay
}

func randomQueueJitter(base time.Duration) time.Duration {
	maximum := base / 10
	if maximum <= 0 {
		return 0
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(maximum)+1))
	if err != nil {
		return 0
	}
	return time.Duration(value.Int64())
}

func stableValidationCode(validation workflow.ValidationResult) string {
	code := "invalid_workflow"
	if len(validation.FieldErrors) > 0 && validation.FieldErrors[0].Code != "" {
		code = validation.FieldErrors[0].Code
	} else if len(validation.GlobalErrors) > 0 && validation.GlobalErrors[0].Code != "" {
		code = validation.GlobalErrors[0].Code
	}
	if len(code) > 64 {
		return "invalid_workflow"
	}
	for _, character := range code {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return "invalid_workflow"
		}
	}
	return code
}

func cloneRuntimeTrackerConfig(source workflow.TrackerConfig) workflow.TrackerConfig {
	clone := source
	clone.Provider = make(map[string]any, len(source.Provider))
	for key, value := range source.Provider {
		clone.Provider[key] = value
	}
	clone.RequiredLabels = append([]string(nil), source.RequiredLabels...)
	clone.ActiveStates = append([]string(nil), source.ActiveStates...)
	clone.TerminalStates = append([]string(nil), source.TerminalStates...)
	return clone
}

func cloneRuntimeSnapshot(source workflow.Snapshot) workflow.Snapshot {
	clone := source
	clone.Config.Tracker = cloneRuntimeTrackerConfig(source.Config.Tracker)
	return clone
}

func cloneCandidateRows(source []domain.CandidateRow) []domain.CandidateRow {
	result := make([]domain.CandidateRow, len(source))
	for index, candidate := range source {
		issue, err := candidate.Issue.Clone()
		if err != nil {
			continue
		}
		if candidate.Issue.Labels != nil && issue.Labels == nil {
			issue.Labels = []string{}
		}
		if candidate.Issue.BlockedBy != nil && issue.BlockedBy == nil {
			issue.BlockedBy = []domain.BlockerRef{}
		}
		result[index] = domain.CandidateRow{
			Issue: issue, Routable: candidate.Routable,
			RoutingReasons: append([]string{}, candidate.RoutingReasons...),
		}
	}
	return result
}

func cloneIssueDetails(source map[string]domain.IssueDetail) map[string]domain.IssueDetail {
	result := make(map[string]domain.IssueDetail, len(source))
	for key, detail := range source {
		clone, err := detail.Clone()
		if err == nil {
			result[key] = clone
		}
	}
	return result
}

func cloneTrackerStatus(source domain.TrackerStatus) domain.TrackerStatus {
	clone := source
	clone.LastAttemptAt = cloneRuntimeTime(source.LastAttemptAt)
	clone.LastSuccessAt = cloneRuntimeTime(source.LastSuccessAt)
	clone.RetryAt = cloneRuntimeTime(source.RetryAt)
	return clone
}

func cloneRuntimeTime(source *time.Time) *time.Time {
	if source == nil {
		return nil
	}
	clone := *source
	return &clone
}

func timePointer(value time.Time) *time.Time {
	clone := value
	return &clone
}
