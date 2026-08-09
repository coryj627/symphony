package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/secrets"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

type fakeFetch struct {
	issues             []domain.Issue
	err                error
	wait               <-chan struct{}
	called             chan<- struct{}
	ignoreCancellation bool
}

type signalingContext struct {
	context.Context
	once    sync.Once
	checked chan struct{}
}

func (ctx *signalingContext) Err() error {
	ctx.once.Do(func() { close(ctx.checked) })
	return ctx.Context.Err()
}

type fakeAdapter struct {
	mu      sync.Mutex
	kind    string
	fetches []fakeFetch
	calls   int
	states  [][]string
}

func (adapter *fakeAdapter) Kind() string {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.kind == "" {
		return "github"
	}
	return adapter.kind
}

func (adapter *fakeAdapter) FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
	adapter.mu.Lock()
	call := adapter.calls
	adapter.calls++
	adapter.states = append(adapter.states, append([]string(nil), states...))
	var fetch fakeFetch
	if call < len(adapter.fetches) {
		fetch = adapter.fetches[call]
	}
	adapter.mu.Unlock()
	if fetch.called != nil {
		select {
		case fetch.called <- struct{}{}:
		default:
		}
	}
	if fetch.wait != nil {
		if fetch.ignoreCancellation {
			<-fetch.wait
		} else {
			select {
			case <-ctx.Done():
				return []domain.Issue{}, ctx.Err()
			case <-fetch.wait:
			}
		}
	}
	return cloneTestIssues(fetch.issues), fetch.err
}

func (adapter *fakeAdapter) FetchIssuesByIDs(context.Context, []string) ([]domain.Issue, error) {
	return []domain.Issue{}, nil
}
func (adapter *fakeAdapter) AgentTools(tracker.Session) []domain.ToolSpec { return []domain.ToolSpec{} }
func (adapter *fakeAdapter) ExecuteAgentTool(context.Context, domain.ToolCall, tracker.Session) domain.ToolResult {
	return domain.ToolUnavailableResult()
}
func (adapter *fakeAdapter) SecretEnvironmentNames() []string { return []string{"FAKE_TOKEN"} }

func (adapter *fakeAdapter) callCount() int {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.calls
}

func (adapter *fakeAdapter) requestedStates() [][]string {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	clone := make([][]string, len(adapter.states))
	for index := range adapter.states {
		clone[index] = append([]string(nil), adapter.states[index]...)
	}
	return clone
}

type fakeFactory struct {
	mu       sync.Mutex
	adapters []tracker.Adapter
	errors   []error
	waits    []<-chan struct{}
	called   []chan<- struct{}
	builds   int
	raw      []workflow.TrackerConfig
}

func (factory *fakeFactory) Build(_ context.Context, raw workflow.TrackerConfig, _ secrets.Resolver) (tracker.Adapter, error) {
	factory.mu.Lock()
	call := factory.builds
	factory.builds++
	factory.raw = append(factory.raw, cloneTrackerConfigForTest(raw))
	var wait <-chan struct{}
	if call < len(factory.waits) {
		wait = factory.waits[call]
	}
	var called chan<- struct{}
	if call < len(factory.called) {
		called = factory.called[call]
	}
	var buildErr error
	if call < len(factory.errors) && factory.errors[call] != nil {
		buildErr = factory.errors[call]
	}
	var adapter tracker.Adapter
	if call < len(factory.adapters) {
		adapter = factory.adapters[call]
	} else if len(factory.adapters) > 0 {
		adapter = factory.adapters[len(factory.adapters)-1]
	}
	factory.mu.Unlock()
	if called != nil {
		select {
		case called <- struct{}{}:
		default:
		}
	}
	if wait != nil {
		<-wait
	}
	if buildErr != nil {
		return nil, buildErr
	}
	if adapter != nil {
		return adapter, nil
	}
	return nil, &tracker.Error{Category: tracker.CategoryConfig, Message: "adapter unavailable"}
}

func (factory *fakeFactory) buildCount() int {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	return factory.builds
}

type fakeResolver struct {
	mu         sync.Mutex
	value      []byte
	err        error
	refs       []secrets.Ref
	references []string
}

func (resolver *fakeResolver) Resolve(_ context.Context, ref secrets.Ref, reference string) ([]byte, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	resolver.refs = append(resolver.refs, ref)
	resolver.references = append(resolver.references, reference)
	return append([]byte(nil), resolver.value...), resolver.err
}

type fakeWorkflowStore struct {
	mu           sync.Mutex
	current      workflow.Snapshot
	hasCurrent   bool
	loadErr      error
	changes      chan workflow.Change
	currentCalls int
	loadCalls    int
	changeCalls  int
}

func (store *fakeWorkflowStore) Current() (workflow.Snapshot, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.currentCalls++
	return cloneWorkflowSnapshotForTest(store.current), store.hasCurrent
}
func (store *fakeWorkflowStore) Load(context.Context) (workflow.Snapshot, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.loadCalls++
	return cloneWorkflowSnapshotForTest(store.current), store.loadErr
}
func (*fakeWorkflowStore) Validate(context.Context, []byte) workflow.ValidationResult {
	return workflow.ValidationResult{Valid: true, FieldErrors: []workflow.FieldError{}, GlobalErrors: []workflow.SafeError{}}
}
func (*fakeWorkflowStore) Save(context.Context, workflow.SaveCommand) (workflow.Snapshot, error) {
	return workflow.Snapshot{}, errors.New("not implemented")
}
func (store *fakeWorkflowStore) Changes() <-chan workflow.Change {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.changeCalls++
	return store.changes
}

func (store *fakeWorkflowStore) setCurrent(snapshot workflow.Snapshot) {
	store.mu.Lock()
	store.current = cloneWorkflowSnapshotForTest(snapshot)
	store.hasCurrent = true
	store.mu.Unlock()
}

func (store *fakeWorkflowStore) accessCounts() (current, load, changes int) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.currentCalls, store.loadCalls, store.changeCalls
}

type fakeQueueClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []fakeQueueTimer
}

type fakeQueueTimer struct {
	due time.Time
	ch  chan time.Time
}

func newFakeQueueClock() *fakeQueueClock {
	return &fakeQueueClock{now: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)}
}

func (clock *fakeQueueClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeQueueClock) After(delay time.Duration) <-chan time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	channel := make(chan time.Time, 1)
	clock.timers = append(clock.timers, fakeQueueTimer{due: clock.now.Add(delay), ch: channel})
	return channel
}

func (clock *fakeQueueClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	remaining := clock.timers[:0]
	for _, timer := range clock.timers {
		if timer.due.After(clock.now) {
			remaining = append(remaining, timer)
			continue
		}
		timer.ch <- clock.now
		close(timer.ch)
	}
	clock.timers = remaining
	clock.mu.Unlock()
}

func (clock *fakeQueueClock) pendingTimers() int {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return len(clock.timers)
}

func newQueueRuntimeForTest(t *testing.T, adapter tracker.Adapter, snapshot workflow.Snapshot) (*QueueRuntime, *fakeFactory, *fakeWorkflowStore, *fakeQueueClock, *observability.Journal) {
	t.Helper()
	factory := &fakeFactory{adapters: []tracker.Adapter{adapter}}
	store := &fakeWorkflowStore{current: cloneWorkflowSnapshotForTest(snapshot), hasCurrent: true, changes: make(chan workflow.Change, 16)}
	clock := newFakeQueueClock()
	journal := observability.NewJournal(observability.JournalOptions{})
	runtime := newQueueRuntimeWithDependencies(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")},
		Journal: journal, Logger: slog.Default(),
	}, queueDependencies{now: clock.Now, after: clock.After, jitter: func(time.Duration) time.Duration { return 0 }})
	t.Cleanup(func() {
		_ = runtime.Shutdown(context.Background())
	})
	return runtime, factory, store, clock, journal
}

func validQueueSnapshot(kind, scope, digest string) workflow.Snapshot {
	trackerConfig := workflow.TrackerConfig{
		Kind: "github", Provider: map[string]any{"owner": "coryj627", "repository": "symphony", "credential_ref": "os-vault"},
		RequiredLabels: []string{"ready"}, ActiveStates: []string{"open"}, TerminalStates: []string{"closed"},
	}
	if kind == "linear" {
		trackerConfig = workflow.TrackerConfig{
			Kind: "linear", Provider: map[string]any{"project_slug": scope, "credential_ref": "os-vault"},
			RequiredLabels: []string{"ready"}, ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"},
		}
	}
	return workflow.Snapshot{
		Digest: digest,
		Config: workflow.EffectiveConfig{Tracker: trackerConfig, Polling: workflow.PollingConfig{Interval: time.Minute}},
	}
}

func validIssue(identifier string) domain.Issue {
	created := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	priority := 2
	return domain.Issue{
		ID: "id-" + identifier, Identifier: identifier, Title: "Issue " + identifier, State: "open",
		Priority: &priority, Labels: []string{"ready"}, BlockedBy: []domain.BlockerRef{}, Dispatchable: true,
		CreatedAt: &created, NativeRef: map[string]any{"identifier": identifier},
	}
}

func trackerErr(category tracker.Category, retryable bool, retryAfter time.Duration) error {
	return &tracker.Error{Category: category, Message: "safe tracker failure", Retryable: retryable, RetryAfter: retryAfter}
}

func cloneTestIssues(source []domain.Issue) []domain.Issue {
	result := make([]domain.Issue, len(source))
	for index, issue := range source {
		clone, err := issue.Clone()
		if err != nil {
			result[index] = issue
			continue
		}
		result[index] = clone
	}
	return result
}

func cloneTrackerConfigForTest(source workflow.TrackerConfig) workflow.TrackerConfig {
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

func cloneWorkflowSnapshotForTest(source workflow.Snapshot) workflow.Snapshot {
	clone := source
	clone.Config.Tracker = cloneTrackerConfigForTest(source.Config.Tracker)
	return clone
}

func waitFor(t *testing.T, description string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
