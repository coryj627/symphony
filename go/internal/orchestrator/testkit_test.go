package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed
}

func intPointer(value int) *int { return &value }

func timePointer(value time.Time) *time.Time { return &value }

func issueWith(identifier string, priority *int, createdAt *time.Time) domain.Issue {
	return domain.Issue{
		ID:           "id-" + identifier,
		Identifier:   identifier,
		Title:        "Issue " + identifier,
		Priority:     priority,
		State:        "open",
		Labels:       []string{"symphony"},
		BlockedBy:    []domain.BlockerRef{},
		Dispatchable: true,
		CreatedAt:    createdAt,
	}
}

func readyIssue(id, state string, labels ...string) domain.Issue {
	return domain.Issue{
		ID:           id,
		Identifier:   "SYM-" + id,
		Title:        "Ready issue " + id,
		State:        state,
		Labels:       append([]string(nil), labels...),
		BlockedBy:    []domain.BlockerRef{},
		Dispatchable: true,
	}
}

func testConfig(activeStates, terminalStates, requiredLabels []string, maxConcurrent int) workflow.EffectiveConfig {
	return workflow.EffectiveConfig{
		Tracker: workflow.TrackerConfig{
			ActiveStates:   append([]string(nil), activeStates...),
			TerminalStates: append([]string(nil), terminalStates...),
			RequiredLabels: append([]string(nil), requiredLabels...),
		},
		Agent: workflow.AgentConfig{
			MaxConcurrent:        maxConcurrent,
			MaxConcurrentByState: map[string]int{},
		},
	}
}

func idSet(ids ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		result[id] = struct{}{}
	}
	return result
}

type fakeTracker struct {
	mu            sync.Mutex
	activeCalls   int
	maxConcurrent int
	stateCalls    int
	idCalls       int
	byStates      []domain.Issue
	byIDs         []domain.Issue
	statesErr     error
	idsErr        error
	stateStarted  chan struct{}
	stateRelease  chan struct{}
	afterStates   func()
}

func (tracker *fakeTracker) Kind() string { return "github" }

func (tracker *fakeTracker) FetchIssuesByStates(ctx context.Context, _ []string) ([]domain.Issue, error) {
	tracker.beginCall()
	defer tracker.endCall()
	tracker.mu.Lock()
	tracker.stateCalls++
	started, release := tracker.stateStarted, tracker.stateRelease
	after := tracker.afterStates
	issues, err := cloneIssuesForTest(tracker.byStates), tracker.statesErr
	tracker.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
		}
	}
	if after != nil {
		after()
	}
	return issues, err
}

func (tracker *fakeTracker) FetchIssuesByIDs(ctx context.Context, _ []string) ([]domain.Issue, error) {
	tracker.beginCall()
	defer tracker.endCall()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.idCalls++
	issues := tracker.byIDs
	if issues == nil {
		issues = tracker.byStates
	}
	return cloneIssuesForTest(issues), tracker.idsErr
}

func (*fakeTracker) AgentTools(tracker.Session) []domain.ToolSpec { return []domain.ToolSpec{} }
func (*fakeTracker) ExecuteAgentTool(context.Context, domain.ToolCall, tracker.Session) domain.ToolResult {
	return domain.ToolUnavailableResult()
}
func (*fakeTracker) SecretEnvironmentNames() []string { return []string{} }

func (tracker *fakeTracker) beginCall() {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.activeCalls++
	if tracker.activeCalls > tracker.maxConcurrent {
		tracker.maxConcurrent = tracker.activeCalls
	}
}

func (tracker *fakeTracker) endCall() {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.activeCalls--
}

func (tracker *fakeTracker) counts() (states, ids, maximum int) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.stateCalls, tracker.idCalls, tracker.maxConcurrent
}

func cloneIssuesForTest(issues []domain.Issue) []domain.Issue {
	result := make([]domain.Issue, len(issues))
	for index, issue := range issues {
		clone, err := issue.Clone()
		if err != nil {
			panic(err)
		}
		result[index] = clone
	}
	return result
}

type fakeWorker struct {
	started chan RunRequest
	release chan domain.RunResult
}

func newBlockingWorker() *fakeWorker {
	return &fakeWorker{started: make(chan RunRequest, 16), release: make(chan domain.RunResult, 16)}
}

func (worker *fakeWorker) Run(ctx context.Context, request RunRequest, _ func(domain.AgentEvent)) domain.RunResult {
	select {
	case worker.started <- request:
	case <-ctx.Done():
		return domain.RunResult{Reason: domain.StopReasonOperatorStop, EndedAt: time.Now().UTC()}
	}
	select {
	case result := <-worker.release:
		return result
	case <-ctx.Done():
		return domain.RunResult{Reason: domain.StopReasonOperatorStop, EndedAt: time.Now().UTC()}
	}
}

func (worker *fakeWorker) waitStarted(t *testing.T) RunRequest {
	t.Helper()
	select {
	case request := <-worker.started:
		return request
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
		return RunRequest{}
	}
}

func (worker *fakeWorker) assertNotStarted(t *testing.T) {
	t.Helper()
	select {
	case request := <-worker.started:
		t.Fatalf("worker unexpectedly started for %q", request.Issue.ID)
	case <-time.After(75 * time.Millisecond):
	}
}

type fakeWorkflowStore struct {
	mu       sync.RWMutex
	current  workflow.Snapshot
	hasValue bool
	loadErr  error
	changes  chan workflow.Change
}

func newFakeWorkflowStore(snapshot workflow.Snapshot) *fakeWorkflowStore {
	return &fakeWorkflowStore{current: snapshot, hasValue: true, changes: make(chan workflow.Change, 8)}
}

func (store *fakeWorkflowStore) Current() (workflow.Snapshot, bool) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.current, store.hasValue
}

func (store *fakeWorkflowStore) Load(context.Context) (workflow.Snapshot, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.loadErr != nil {
		return workflow.Snapshot{}, store.loadErr
	}
	if !store.hasValue {
		return workflow.Snapshot{}, errors.New("no workflow")
	}
	return store.current, nil
}

func (*fakeWorkflowStore) Validate(context.Context, []byte) workflow.ValidationResult {
	return workflow.ValidationResult{Valid: true, FieldErrors: []workflow.FieldError{}, GlobalErrors: []workflow.SafeError{}}
}
func (*fakeWorkflowStore) Save(context.Context, workflow.SaveCommand) (workflow.Snapshot, error) {
	return workflow.Snapshot{}, errors.New("not implemented by fake")
}
func (store *fakeWorkflowStore) Changes() <-chan workflow.Change { return store.changes }

func (store *fakeWorkflowStore) setCurrent(snapshot workflow.Snapshot) {
	store.mu.Lock()
	store.current, store.hasValue = snapshot, true
	store.mu.Unlock()
}

func testWorkflowSnapshot() workflow.Snapshot {
	snapshot := workflow.Snapshot{
		Digest: "test-digest",
		Config: testConfig([]string{"open"}, []string{"closed"}, []string{"symphony"}, 2),
	}
	snapshot.Config.Tracker.Kind = "github"
	return snapshot
}

func startTestOrchestrator(t *testing.T, adapter *fakeTracker, worker Worker, mutate ...func(*Options)) *Orchestrator {
	t.Helper()
	options := Options{
		Tracker:  adapter,
		Workflow: newFakeWorkflowStore(testWorkflowSnapshot()),
		Worker:   worker,
		Events:   observability.NewJournal(observability.JournalOptions{}),
		Clock:    RealClock{},
	}
	for _, apply := range mutate {
		apply(&options)
	}
	orchestrator, err := Start(context.Background(), options)
	if err != nil {
		t.Fatalf("start orchestrator: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := orchestrator.Close(ctx); err != nil {
			t.Errorf("close orchestrator: %v", err)
		}
	})
	return orchestrator
}

func mustSnapshot(t *testing.T, orchestrator *Orchestrator) domain.Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	snapshot, err := orchestrator.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return snapshot
}
