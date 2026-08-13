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
			MaxRetryBackoff:      5 * time.Minute,
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
	mu             sync.Mutex
	activeCalls    int
	maxConcurrent  int
	stateCalls     int
	idCalls        int
	byStates       []domain.Issue
	byIDs          []domain.Issue
	statesErr      error
	idsErr         error
	stateResponses []fakeTrackerResponse
	idResponses    []fakeTrackerResponse
	stateStarted   chan struct{}
	stateRelease   chan struct{}
	blockStates    []string
	afterStates    func()
}

type fakeTrackerResponse struct {
	issues []domain.Issue
	err    error
}

func (tracker *fakeTracker) Kind() string { return "github" }

func (tracker *fakeTracker) FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
	tracker.beginCall()
	defer tracker.endCall()
	tracker.mu.Lock()
	tracker.stateCalls++
	started, release := tracker.stateStarted, tracker.stateRelease
	after := tracker.afterStates
	issues, err := cloneIssuesForTest(tracker.byStates), tracker.statesErr
	if len(tracker.stateResponses) > 0 {
		response := tracker.stateResponses[0]
		tracker.stateResponses = tracker.stateResponses[1:]
		issues, err = cloneIssuesForTest(response.issues), response.err
	} else if containsNormalized(states, "closed") {
		issues = []domain.Issue{}
	}
	shouldBlock := len(tracker.blockStates) == 0 || sameNormalizedStrings(states, tracker.blockStates)
	tracker.mu.Unlock()
	if started != nil && shouldBlock {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil && shouldBlock {
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

func sameNormalizedStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if normalizeComparable(left[index]) != normalizeComparable(right[index]) {
			return false
		}
	}
	return true
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
	if len(tracker.idResponses) > 0 {
		response := tracker.idResponses[0]
		tracker.idResponses = tracker.idResponses[1:]
		return cloneIssuesForTest(response.issues), response.err
	}
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

type fakeWorkspaceManager struct {
	mu          sync.Mutex
	removes     int
	removeRoots []string
	removeErr   error
}

func (*fakeWorkspaceManager) Ensure(context.Context, domain.Issue, workflow.EffectiveConfig) (domain.Workspace, error) {
	return domain.Workspace{}, nil
}

func (manager *fakeWorkspaceManager) Remove(_ context.Context, _ domain.Issue, config workflow.EffectiveConfig) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.removes++
	manager.removeRoots = append(manager.removeRoots, config.Workspace.Root)
	return manager.removeErr
}

func (manager *fakeWorkspaceManager) roots() []string {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return append([]string(nil), manager.removeRoots...)
}

func (manager *fakeWorkspaceManager) removeCount() int {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.removes
}

type fakeWorker struct {
	started chan RunRequest
	release chan domain.RunResult
}

type stubbornWorker struct {
	started  chan RunRequest
	canceled chan struct{}
	release  chan domain.RunResult
}

func newStubbornWorker() *stubbornWorker {
	return &stubbornWorker{started: make(chan RunRequest, 1), canceled: make(chan struct{}, 1), release: make(chan domain.RunResult, 1)}
}

func (worker *stubbornWorker) Run(ctx context.Context, request RunRequest, _ func(domain.AgentEvent)) domain.RunResult {
	worker.started <- request
	go func() {
		<-ctx.Done()
		select {
		case worker.canceled <- struct{}{}:
		default:
		}
	}()
	return <-worker.release
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
		Tracker: adapter, Workflow: newFakeWorkflowStore(testWorkflowSnapshot()),
		Workspace: &fakeWorkspaceManager{}, Worker: worker,
		Events: observability.NewJournal(observability.JournalOptions{}), Clock: RealClock{},
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
