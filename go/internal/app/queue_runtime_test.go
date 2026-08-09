package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

type blockingFallbackWorkflowStore struct {
	*fakeWorkflowStore
	loadStarted chan struct{}
	releaseLoad chan struct{}
	releaseOnce sync.Once
}

func (store *blockingFallbackWorkflowStore) Load(context.Context) (workflow.Snapshot, error) {
	store.mu.Lock()
	store.loadCalls++
	snapshot := cloneWorkflowSnapshotForTest(store.current)
	err := store.loadErr
	store.mu.Unlock()
	select {
	case store.loadStarted <- struct{}{}:
	default:
	}
	<-store.releaseLoad
	return snapshot, err
}

func (store *blockingFallbackWorkflowStore) makeCurrentUnavailable() {
	store.mu.Lock()
	store.hasCurrent = false
	store.mu.Unlock()
}

func (store *blockingFallbackWorkflowStore) waitForLoad(t *testing.T) {
	t.Helper()
	select {
	case <-store.loadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for fallback Load")
	}
}

func (store *blockingFallbackWorkflowStore) release() {
	store.releaseOnce.Do(func() { close(store.releaseLoad) })
}

type fallbackLoadHarness struct {
	runtime     *QueueRuntime
	store       *blockingFallbackWorkflowStore
	journal     *observability.Journal
	logs        *observability.LogStore
	cancelOwner context.CancelFunc
}

func newFallbackLoadHarness(t *testing.T, adapter tracker.Adapter, buildErr, loadErr error) *fallbackLoadHarness {
	return newFallbackLoadHarnessWithDependencies(t, adapter, buildErr, loadErr, queueDependencies{})
}

func newFallbackLoadHarnessWithDependencies(
	t *testing.T,
	adapter tracker.Adapter,
	buildErr, loadErr error,
	dependencies queueDependencies,
) *fallbackLoadHarness {
	t.Helper()
	store := &blockingFallbackWorkflowStore{
		fakeWorkflowStore: &fakeWorkflowStore{
			current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true,
			loadErr: loadErr, changes: make(chan workflow.Change, 2),
		},
		loadStarted: make(chan struct{}, 1),
		releaseLoad: make(chan struct{}),
	}
	factory := &fakeFactory{adapters: []tracker.Adapter{adapter}, errors: []error{buildErr}}
	clock := newFakeQueueClock()
	journal := observability.NewJournal(observability.JournalOptions{})
	logger, logs, err := observability.NewLogger(observability.Options{DataDir: t.TempDir(), Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	dependencies.now = clock.Now
	dependencies.after = clock.After
	dependencies.jitter = func(time.Duration) time.Duration { return 0 }
	runtime := newQueueRuntimeWithDependencies(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")},
		Journal: journal, Logger: logger,
	}, dependencies)
	harness := &fallbackLoadHarness{
		runtime: runtime, store: store, journal: journal, logs: logs, cancelOwner: cancelOwner,
	}
	t.Cleanup(func() {
		store.release()
		cancelOwner()
		_ = runtime.Shutdown(context.Background())
		_ = logs.Close()
	})
	if err := runtime.Start(ownerCtx); err != nil {
		t.Fatal(err)
	}
	store.makeCurrentUnavailable()
	return harness
}

type fallbackRuntimeEvidence struct {
	snapshot       domain.Snapshot
	events         domain.EventPage
	logs           observability.LogPage
	adapter        tracker.Adapter
	autoSuppressed bool
}

func (harness *fallbackLoadHarness) evidence(t *testing.T) fallbackRuntimeEvidence {
	t.Helper()
	snapshot, err := harness.runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	logs, err := harness.logs.Query(context.Background(), observability.LogQuery{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	harness.runtime.mu.Lock()
	adapter := harness.runtime.adapter
	autoSuppressed := harness.runtime.autoSuppressed
	harness.runtime.mu.Unlock()
	return fallbackRuntimeEvidence{
		snapshot: snapshot,
		events:   harness.journal.After(domain.EventCursor{Epoch: harness.journal.Epoch(), Sequence: 0}),
		logs:     logs, adapter: adapter, autoSuppressed: autoSuppressed,
	}
}

func (harness *fallbackLoadHarness) credentialIntent() (context.Context, credentialRebuildIntent) {
	harness.runtime.mu.Lock()
	defer harness.runtime.mu.Unlock()
	return harness.runtime.runtimeCtx, credentialRebuildIntent{
		generation: harness.runtime.generation,
		epoch:      harness.runtime.rebuildIntentEpoch,
	}
}

func assertFallbackEvidenceUnchanged(t *testing.T, before, after fallbackRuntimeEvidence) {
	t.Helper()
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("fallback failure mutated runtime evidence:\nbefore=%#v\nafter=%#v", before, after)
	}
}

func assertTrackerConfigFailure(t *testing.T, err error) {
	t.Helper()
	var failure *tracker.Error
	if !errors.As(err, &failure) || err.Error() != "tracker_config: Tracker configuration is unavailable." ||
		failure.Category != tracker.CategoryConfig || failure.Message != "Tracker configuration is unavailable." ||
		failure.Retryable || failure.RetryAfter != 0 || failure.Status != 0 || errors.Is(err, context.Canceled) {
		t.Fatalf("tracker-config fallback error = %#v", err)
	}
}

func assertCurrentFallbackFailureEvidence(t *testing.T, before, after fallbackRuntimeEvidence) {
	t.Helper()
	if !reflect.DeepEqual(after.snapshot.Config, before.snapshot.Config) ||
		after.snapshot.EventCursor.Epoch != before.snapshot.EventCursor.Epoch ||
		after.snapshot.EventCursor.Sequence != before.snapshot.EventCursor.Sequence+1 ||
		len(after.events.Events) != len(before.events.Events)+1 ||
		!reflect.DeepEqual(after.snapshot.Candidates, before.snapshot.Candidates) || len(after.snapshot.Candidates) == 0 ||
		after.snapshot.Tracker.State != "failed" || !after.snapshot.Tracker.Stale ||
		after.snapshot.Tracker.ErrorCode != string(tracker.CategoryConfig) ||
		after.snapshot.Tracker.Message != "Tracker configuration is unavailable." || after.snapshot.Tracker.Retryable ||
		after.snapshot.Tracker.RetryAt != nil ||
		!reflect.DeepEqual(after.snapshot.Tracker.LastAttemptAt, before.snapshot.Tracker.LastAttemptAt) ||
		!reflect.DeepEqual(after.snapshot.Tracker.LastSuccessAt, before.snapshot.Tracker.LastSuccessAt) ||
		after.adapter != nil || !after.autoSuppressed || !reflect.DeepEqual(after.logs, before.logs) {
		t.Fatalf("current fallback failure evidence:\nbefore=%#v\nafter=%#v", before, after)
	}
	last := after.events.Events[len(after.events.Events)-1]
	if last.Type != "queue.failed" || last.Data["error_code"] != string(tracker.CategoryConfig) ||
		last.Data["retryable"] != false {
		t.Fatalf("current fallback failure event = %#v", last)
	}
}

func TestQueueInitialPollUsesConfiguredStatesAndPublishesSortedRoutableCandidates(t *testing.T) {
	t.Parallel()
	oldest := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	first := validIssue("GH-2")
	first.Priority = pointerTo(1)
	first.CreatedAt = nil
	second := validIssue("GH-1")
	second.Priority = pointerTo(1)
	second.CreatedAt = &oldest
	third := validIssue("GH-3")
	third.Priority = nil
	third.Dispatchable = false
	third.Labels = []string{"other"}
	adapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{third, first, second}}}}
	runtime, _, _, clock, journal := newQueueRuntimeForTest(t, adapter, validQueueSnapshot("github", "", "digest-1"))

	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := adapter.requestedStates(); len(got) != 1 || strings.Join(got[0], ",") != "open" {
		t.Fatalf("configured active states = %#v", got)
	}
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.GeneratedAt.Equal(clock.Now()) || len(snapshot.Candidates) != 3 {
		t.Fatalf("initial snapshot = %#v", snapshot)
	}
	if got := []string{snapshot.Candidates[0].Issue.Identifier, snapshot.Candidates[1].Issue.Identifier, snapshot.Candidates[2].Issue.Identifier}; strings.Join(got, ",") != "GH-1,GH-2,GH-3" {
		t.Fatalf("candidate dispatch order = %v", got)
	}
	if !snapshot.Candidates[0].Routable || len(snapshot.Candidates[0].RoutingReasons) != 0 {
		t.Fatalf("routable candidate = %#v", snapshot.Candidates[0])
	}
	if got := strings.Join(snapshot.Candidates[2].RoutingReasons, ","); got != "provider_not_dispatchable,missing_required_label" {
		t.Fatalf("routing reasons = %q", got)
	}
	if snapshot.Running == nil || snapshot.Retrying == nil || snapshot.Requests == nil || snapshot.RateLimits != nil || snapshot.Scheduler.Available || snapshot.Scheduler.Enabled {
		t.Fatalf("Phase 2 empty/status fields = %#v", snapshot)
	}
	page := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 0})
	if len(page.Events) != 1 || page.Events[0].Type != "queue.refreshed" {
		t.Fatalf("initial journal page = %#v", page)
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"Issue GH-1", "identifier", "native_ref", "choice_id", "answers"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("event JSON exposed provider/operator data %q: %s", forbidden, encoded)
		}
	}
}

func TestQueueIssueLookupIsNormalizedStableAndImmutable(t *testing.T) {
	t.Parallel()
	issue := validIssue("Gh-17")
	adapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{issue}}}}
	runtime, _, _, _, _ := newQueueRuntimeForTest(t, adapter, validQueueSnapshot("github", "", "digest-1"))
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	detail, err := runtime.Issue(context.Background(), "  gh-17 ")
	if err != nil || detail.Issue.Identifier != "Gh-17" || detail.Status != "candidate" {
		t.Fatalf("normalized lookup = %#v, %v", detail, err)
	}
	detail.Issue.Labels[0] = "mutated"
	detail.Issue.NativeRef["identifier"] = "mutated"
	detail.RoutingReasons = append(detail.RoutingReasons, "mutated")
	again, err := runtime.Issue(context.Background(), "GH-17")
	if err != nil || again.Issue.Labels[0] != "ready" || again.Issue.NativeRef["identifier"] != "Gh-17" || len(again.RoutingReasons) != 0 {
		t.Fatalf("immutable lookup = %#v, %v", again, err)
	}
	if _, err := runtime.Issue(context.Background(), "unknown"); !errors.Is(err, ErrIssueNotFound) {
		t.Fatalf("unknown issue error = %v", err)
	}
}

func TestQueueNormalizesNilProviderIssueCollectionsToStableEmptyArrays(t *testing.T) {
	t.Parallel()
	issue := validIssue("GH-EMPTY")
	issue.Labels = nil
	issue.BlockedBy = nil
	adapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{issue}}}}
	runtime, _, _, _, _ := newQueueRuntimeForTest(t, adapter, validQueueSnapshot("github", "", "digest-1"))
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	detail, err := runtime.Issue(context.Background(), "GH-EMPTY")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Candidates[0].Issue.Labels == nil || snapshot.Candidates[0].Issue.BlockedBy == nil || detail.Issue.Labels == nil || detail.Issue.BlockedBy == nil {
		t.Fatalf("normalized empty issue collections were not stable: candidate=%#v detail=%#v", snapshot.Candidates[0].Issue, detail.Issue)
	}
}

func TestQueueRefreshDoesNotPublishPartialOrDuplicateProviderResults(t *testing.T) {
	t.Parallel()
	initial := validIssue("GH-1")
	duplicate := validIssue(" gh-1 ")
	duplicate.ID = "another-id"
	adapter := &fakeAdapter{fetches: []fakeFetch{
		{issues: []domain.Issue{initial}},
		{issues: []domain.Issue{validIssue("GH-2")}, err: trackerErr(tracker.CategoryPagination, false, 0)},
		{issues: []domain.Issue{initial, duplicate}},
	}}
	runtime, _, _, _, journal := newQueueRuntimeForTest(t, adapter, validQueueSnapshot("github", "", "digest-1"))
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Refresh(context.Background()); err == nil {
		t.Fatal("partial provider result did not fail")
	}
	snapshot, _ := runtime.Snapshot(context.Background())
	if len(snapshot.Candidates) != 1 || snapshot.Candidates[0].Issue.Identifier != "GH-1" || !snapshot.Tracker.Stale || snapshot.Tracker.ErrorCode != "tracker_pagination" {
		t.Fatalf("partial failure leaked or status missing: %#v", snapshot)
	}
	if _, err := runtime.Refresh(context.Background()); err == nil {
		t.Fatal("duplicate normalized identifier did not reject refresh")
	}
	again, _ := runtime.Snapshot(context.Background())
	if len(again.Candidates) != 1 || again.Candidates[0].Issue.Identifier != "GH-1" {
		t.Fatalf("duplicate refresh replaced last complete state: %#v", again.Candidates)
	}
	page := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 0})
	if len(page.Events) != 3 || page.Events[1].Type != "queue.failed" || page.Events[2].Type != "queue.failed" {
		t.Fatalf("failure events = %#v", page.Events)
	}
}

func TestUnknownTrackerCategoryFailsClosedAcrossRefreshStatusEventErrorAndLog(t *testing.T) {
	t.Parallel()
	const categoryCanary = "raw-provider-category-canary"
	const messageCanary = "raw-provider-message-canary"
	adapter := &fakeAdapter{fetches: []fakeFetch{
		{issues: []domain.Issue{validIssue("GH-1")}},
		{err: &tracker.Error{
			Category: tracker.Category(categoryCanary), Message: messageCanary,
			Retryable: true, RetryAfter: 12 * time.Hour, Status: 599,
		}},
	}}
	factory := &fakeFactory{adapters: []tracker.Adapter{adapter}}
	store := &fakeWorkflowStore{current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true, changes: make(chan workflow.Change, 1)}
	clock := newFakeQueueClock()
	journal := observability.NewJournal(observability.JournalOptions{})
	logger, logs, err := observability.NewLogger(observability.Options{DataDir: t.TempDir(), Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newQueueRuntimeWithDependencies(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")},
		Journal: journal, Logger: logger,
	}, queueDependencies{now: clock.Now, after: clock.After, jitter: func(time.Duration) time.Duration { return 0 }})
	t.Cleanup(func() {
		_ = runtime.Shutdown(context.Background())
		_ = logs.Close()
	})
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, refreshErr := runtime.Refresh(context.Background())
	if refreshErr == nil || refreshErr.Error() != "tracker_error" {
		t.Fatalf("unknown-category Refresh error = %v", refreshErr)
	}
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Tracker.ErrorCode != "tracker_error" || snapshot.Tracker.Message != "Tracker operation failed." || snapshot.Tracker.Retryable || snapshot.Tracker.RetryAt != nil {
		t.Fatalf("unknown-category tracker status = %#v", snapshot.Tracker)
	}
	events := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 0})
	if len(events.Events) != 2 || events.Events[1].Type != "queue.failed" || events.Events[1].Data["error_code"] != "tracker_error" || events.Events[1].Data["retryable"] != false {
		t.Fatalf("unknown-category event page = %#v", events)
	}
	logPage, err := logs.Query(context.Background(), observability.LogQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(struct {
		Snapshot domain.Snapshot
		Events   domain.EventPage
		Logs     observability.LogPage
		Error    string
	}{Snapshot: snapshot, Events: events, Logs: logPage, Error: refreshErr.Error()})
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{categoryCanary, messageCanary} {
		if strings.Contains(string(encoded), canary) {
			t.Fatalf("unknown tracker data reached a public surface: %s", encoded)
		}
	}
}

func TestUnknownTrackerCategoryVariantsFailClosedDuringBuild(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		category tracker.Category
	}{
		{name: "empty", category: ""},
		{name: "mixed_case", category: "TRACKER_AUTH"},
		{name: "trailing_space", category: "tracker_auth "},
		{name: "control_character", category: "tracker_auth\ncanary"},
		{name: "overlong", category: tracker.Category(strings.Repeat("x", 2048))},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			factory := &fakeFactory{errors: []error{&tracker.Error{
				Category: test.category, Message: "build-message-canary", Retryable: true,
				RetryAfter: time.Hour, Status: 599,
			}}}
			store := &fakeWorkflowStore{current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true, changes: make(chan workflow.Change, 1)}
			journal := observability.NewJournal(observability.JournalOptions{})
			runtime := NewQueueRuntime(QueueOptions{
				Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")}, Journal: journal,
			})
			t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
			if err := runtime.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			snapshot, err := runtime.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			page := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 0})
			encoded, err := json.Marshal(struct {
				Snapshot domain.Snapshot
				Events   domain.EventPage
			}{snapshot, page})
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Tracker.ErrorCode != "tracker_error" || snapshot.Tracker.Message != "Tracker operation failed." || snapshot.Tracker.Retryable || snapshot.Tracker.RetryAt != nil || len(page.Events) != 1 || page.Events[0].Data["error_code"] != "tracker_error" || page.Events[0].Data["retryable"] != false || (test.category != "" && strings.Contains(string(encoded), string(test.category))) || strings.Contains(string(encoded), "build-message-canary") {
				t.Fatalf("category %q did not fail closed: snapshot=%#v page=%#v", test.category, snapshot, page)
			}
		})
	}
}

func TestKnownTrackerCategoriesRetainOnlyStableRetryFields(t *testing.T) {
	t.Parallel()
	for _, category := range []tracker.Category{
		tracker.CategoryConfig,
		tracker.CategoryAuth,
		tracker.CategoryTransport,
		tracker.CategoryResponse,
		tracker.CategoryPayload,
		tracker.CategoryPagination,
		tracker.CategoryRateLimited,
	} {
		category := category
		t.Run(string(category), func(t *testing.T) {
			portable, returned := safeTrackerFailure(&tracker.Error{
				Category: category, Message: "raw-known-message-canary", Retryable: true,
				RetryAfter: 17 * time.Second, Status: 599,
			})
			if portable.Category != category || portable.Message != trackerFailureMessage(category) || !portable.Retryable || portable.RetryAfter != 17*time.Second || portable.Status != 0 {
				t.Fatalf("known category %q normalization = %#v", category, portable)
			}
			if returned == nil || !strings.Contains(returned.Error(), string(category)) || strings.Contains(returned.Error(), "raw-known-message-canary") || strings.Contains(returned.Error(), "599") {
				t.Fatalf("known category %q returned error = %v", category, returned)
			}
		})
	}
}

func TestRefreshCoalescesOneProviderCallAndWaitingCancellationDoesNotCancelSharedFetch(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	adapter := &fakeAdapter{fetches: []fakeFetch{
		{issues: []domain.Issue{validIssue("GH-1")}},
		{issues: []domain.Issue{validIssue("GH-2")}, wait: release, called: started},
	}}
	runtime, _, _, _, _ := newQueueRuntimeForTest(t, adapter, validQueueSnapshot("github", "", "digest-1"))
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	type result struct {
		receipt domain.RefreshReceipt
		err     error
	}
	first := make(chan result, 1)
	go func() {
		receipt, err := runtime.Refresh(context.Background())
		first <- result{receipt, err}
	}()
	<-started
	waitCtx, cancel := context.WithCancel(context.Background())
	second := make(chan result, 1)
	go func() {
		receipt, err := runtime.Refresh(waitCtx)
		second <- result{receipt, err}
	}()
	cancel()
	joined := <-second
	if !errors.Is(joined.err, context.Canceled) || !joined.receipt.Coalesced || joined.receipt.Queued || strings.Join(joined.receipt.Operations, ",") != "poll" {
		t.Fatalf("coalesced canceled receipt = %#v, %v", joined.receipt, joined.err)
	}
	close(release)
	leader := <-first
	if leader.err != nil || !leader.receipt.Queued || leader.receipt.Coalesced || strings.Join(leader.receipt.Operations, ",") != "poll" {
		t.Fatalf("leader receipt = %#v, %v", leader.receipt, leader.err)
	}
	if adapter.callCount() != 2 {
		t.Fatalf("coalesced provider calls = %d, want initial plus one", adapter.callCount())
	}
}

func TestQueueRateLimitSuppressesEarlyManualPollAndBoundsRetryAt(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{fetches: []fakeFetch{{err: trackerErr(tracker.CategoryRateLimited, true, 48*time.Hour)}}}
	factory := &fakeFactory{adapters: []tracker.Adapter{adapter}}
	store := &fakeWorkflowStore{current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true, changes: make(chan workflow.Change, 1)}
	clock := newFakeQueueClock()
	journal := observability.NewJournal(observability.JournalOptions{})
	runtime := newQueueRuntimeWithDependencies(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")}, Journal: journal,
	}, queueDependencies{
		now: clock.Now, after: clock.After,
		jitter: func(time.Duration) time.Duration { return 3 * time.Hour },
	})
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := runtime.Snapshot(context.Background())
	if snapshot.Tracker.ErrorCode != "tracker_rate_limited" || snapshot.Tracker.RetryAt == nil || !snapshot.Tracker.RetryAt.Equal(clock.Now().Add(24*time.Hour)) {
		t.Fatalf("bounded rate limit status = %#v", snapshot.Tracker)
	}
	receipt, err := runtime.Refresh(context.Background())
	if err != nil || receipt.Queued || receipt.Coalesced || strings.Join(receipt.Operations, ",") != "poll" || adapter.callCount() != 1 {
		t.Fatalf("suppressed manual refresh = %#v, %v calls=%d", receipt, err, adapter.callCount())
	}
}

func TestQueueRateLimitTimerWaitsUntilEligibilityThenPolls(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{fetches: []fakeFetch{
		{err: trackerErr(tracker.CategoryRateLimited, true, 0)},
		{issues: []domain.Issue{validIssue("GH-RECOVERED")}},
	}}
	runtime, _, _, clock, _ := newQueueRuntimeForTest(t, adapter, validQueueSnapshot("github", "", "digest-1"))
	timerArmed := make(chan time.Duration, 2)
	runtime.deps.after = func(delay time.Duration) <-chan time.Time {
		timer := clock.After(delay)
		timerArmed <- delay
		return timer
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for arm := 1; arm <= 2; arm++ {
		select {
		case delay := <-timerArmed:
			if delay != time.Minute {
				t.Fatalf("rate-limit timer arm %d delay = %s, want 1m", arm, delay)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for rate-limit timer arm %d", arm)
		}
	}
	clock.Advance(59 * time.Second)
	time.Sleep(5 * time.Millisecond)
	if adapter.callCount() != 1 {
		t.Fatalf("rate-limit timer polled early: %d calls", adapter.callCount())
	}
	clock.Advance(time.Second)
	waitFor(t, "eligible rate-limit poll", func() bool {
		snapshot, err := runtime.Snapshot(context.Background())
		return err == nil && len(snapshot.Candidates) == 1 && snapshot.Candidates[0].Issue.Identifier == "GH-RECOVERED" && snapshot.Tracker.RetryAt == nil
	})
	snapshot, _ := runtime.Snapshot(context.Background())
	if len(snapshot.Candidates) != 1 || snapshot.Candidates[0].Issue.Identifier != "GH-RECOVERED" || snapshot.Tracker.RetryAt != nil {
		t.Fatalf("rate-limit recovery snapshot = %#v", snapshot)
	}
}

func TestManualRefreshCoalescesWithWorkflowRebuildAndImmediatePoll(t *testing.T) {
	t.Parallel()
	buildStarted := make(chan struct{}, 1)
	releaseBuild := make(chan struct{})
	oldAdapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-OLD")}}}}
	newAdapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-NEW")}}}}
	runtime, factory, store, _, _ := newQueueRuntimeForTest(t, oldAdapter, validQueueSnapshot("github", "", "digest-1"))
	factory.adapters = []tracker.Adapter{oldAdapter, newAdapter, newAdapter}
	factory.waits = []<-chan struct{}{nil, releaseBuild}
	factory.called = []chan<- struct{}{nil, buildStarted}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	updated := validQueueSnapshot("github", "", "digest-2")
	store.setCurrent(updated)
	store.changes <- workflow.Change{Snapshot: updated, Digest: updated.Digest, Validation: workflow.ValidationResult{Valid: true}}
	<-buildStarted
	type result struct {
		receipt domain.RefreshReceipt
		err     error
	}
	manual := make(chan result, 1)
	checked := make(chan struct{})
	manualCtx := &signalingContext{Context: context.Background(), checked: checked}
	go func() {
		receipt, err := runtime.Refresh(manualCtx)
		manual <- result{receipt: receipt, err: err}
	}()
	<-checked
	close(releaseBuild)
	got := <-manual
	if got.err != nil || got.receipt.Queued || !got.receipt.Coalesced {
		t.Fatalf("rebuild-coalesced receipt = %#v, %v", got.receipt, got.err)
	}
	if factory.buildCount() != 2 || newAdapter.callCount() != 1 {
		t.Fatalf("manual refresh duplicated rebuild/poll: builds=%d new calls=%d", factory.buildCount(), newAdapter.callCount())
	}
}

func TestConcurrentManualRebuildRefreshesReserveOneBuildAndPoll(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{fetches: []fakeFetch{
		{issues: []domain.Issue{validIssue("GH-RECOVERED")}},
		{issues: []domain.Issue{validIssue("GH-DUPLICATE")}},
	}}
	factory := &fakeFactory{
		adapters: []tracker.Adapter{nil, adapter, adapter},
		errors:   []error{trackerErr(tracker.CategoryAuth, false, 0), nil, nil},
	}
	store := &fakeWorkflowStore{current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true, changes: make(chan workflow.Change, 1)}
	clock := newFakeQueueClock()
	journal := observability.NewJournal(observability.JournalOptions{})
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	runtime := newQueueRuntimeWithDependencies(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")}, Journal: journal,
	}, queueDependencies{
		now: clock.Now, after: clock.After, jitter: func(time.Duration) time.Duration { return 0 },
		beforeManualRebuild: func() {
			arrived <- struct{}{}
			<-release
		},
	})
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	type result struct {
		receipt domain.RefreshReceipt
		err     error
	}
	results := make(chan result, 2)
	go func() {
		receipt, err := runtime.Refresh(context.Background())
		results <- result{receipt: receipt, err: err}
	}()
	<-arrived
	go func() {
		receipt, err := runtime.Refresh(context.Background())
		results <- result{receipt: receipt, err: err}
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	first := <-results
	second := <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent rebuild errors = %v / %v", first.err, second.err)
	}
	leaders := 0
	coalesced := 0
	for _, got := range []domain.RefreshReceipt{first.receipt, second.receipt} {
		if got.Queued {
			leaders++
		}
		if got.Coalesced {
			coalesced++
		}
	}
	if leaders != 1 || coalesced != 1 || factory.buildCount() != 2 || adapter.callCount() != 1 {
		t.Fatalf("manual rebuild was not single-flight: leaders=%d coalesced=%d builds=%d polls=%d", leaders, coalesced, factory.buildCount(), adapter.callCount())
	}
}

func TestCredentialRebuildSupersedesAnOlderReservedManualRebuild(t *testing.T) {
	t.Parallel()
	recovered := &fakeAdapter{fetches: []fakeFetch{
		{issues: []domain.Issue{validIssue("GH-RECOVERED")}},
		{issues: []domain.Issue{validIssue("GH-DUPLICATE")}},
	}}
	factory := &fakeFactory{
		adapters: []tracker.Adapter{nil, recovered, recovered},
		errors:   []error{trackerErr(tracker.CategoryAuth, false, 0), nil, nil},
	}
	store := &fakeWorkflowStore{current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true, changes: make(chan workflow.Change, 1)}
	clock := newFakeQueueClock()
	journal := observability.NewJournal(observability.JournalOptions{})
	manualStarted := make(chan struct{}, 1)
	releaseManual := make(chan struct{})
	runtime := newQueueRuntimeWithDependencies(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")}, Journal: journal,
	}, queueDependencies{
		now: clock.Now, after: clock.After, jitter: func(time.Duration) time.Duration { return 0 },
		beforeManualRebuild: func() {
			manualStarted <- struct{}{}
			<-releaseManual
		},
	})
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	type result struct {
		receipt domain.RefreshReceipt
		err     error
	}
	manual := make(chan result, 1)
	go func() {
		receipt, err := runtime.Refresh(context.Background())
		manual <- result{receipt: receipt, err: err}
	}()
	<-manualStarted
	runtime.NotifyCredentialChanged()
	waitFor(t, "credential rebuild", func() bool { return recovered.callCount() == 1 })
	close(releaseManual)
	got := <-manual
	if got.err != nil || !got.receipt.Queued || got.receipt.Coalesced || factory.buildCount() != 2 || recovered.callCount() != 1 {
		t.Fatalf("newer credential rebuild did not supersede manual intent: receipt=%#v err=%v builds=%d polls=%d", got.receipt, got.err, factory.buildCount(), recovered.callCount())
	}
}

func TestValidWorkflowBuildFailureDeactivatesOldAdapterPublishesConfigurationAndRecoversExplicitly(t *testing.T) {
	t.Parallel()
	oldAdapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-OLD")}}}}
	newAdapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-NEW")}}}}
	runtime, factory, store, _, journal := newQueueRuntimeForTest(t, oldAdapter, validQueueSnapshot("github", "", "digest-1"))
	factory.adapters = []tracker.Adapter{oldAdapter, nil, newAdapter}
	factory.errors = []error{nil, trackerErr(tracker.CategoryAuth, false, 0), nil}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	updated := validQueueSnapshot("github", "", "digest-2")
	store.setCurrent(updated)
	store.changes <- workflow.Change{Snapshot: updated, Digest: updated.Digest, Validation: workflow.ValidationResult{Valid: true}}
	waitFor(t, "valid build failure", func() bool {
		snapshot, _ := runtime.Snapshot(context.Background())
		return snapshot.Tracker.ErrorCode == "tracker_auth"
	})
	failed, _ := runtime.Snapshot(context.Background())
	if failed.Config.State != "valid" || failed.Config.ActiveDigest != "" || !failed.Tracker.Stale || len(failed.Candidates) != 1 {
		t.Fatalf("valid build failure state = %#v", failed)
	}
	page := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 0})
	configurationEvents := 0
	for _, event := range page.Events {
		if event.Type == "configuration.changed" {
			configurationEvents++
		}
	}
	if configurationEvents != 1 {
		t.Fatalf("valid failed change published %d configuration events: %#v", configurationEvents, page.Events)
	}
	if _, err := runtime.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, _ := runtime.Snapshot(context.Background())
	if len(recovered.Candidates) != 1 || recovered.Candidates[0].Issue.Identifier != "GH-NEW" || factory.buildCount() != 3 {
		t.Fatalf("explicit build recovery = %#v builds=%d", recovered, factory.buildCount())
	}
}

func TestQueueDoesNotCommitProviderStateWhenJournalCannotPublishTransition(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-1")}}}}
	factory := &fakeFactory{adapters: []tracker.Adapter{adapter}}
	store := &fakeWorkflowStore{current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true, changes: make(chan workflow.Change, 1)}
	journal := observability.NewJournal(observability.JournalOptions{MaxEvents: 4, MaxBytes: 1})
	runtime := NewQueueRuntime(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")}, Journal: journal,
	})
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Candidates) != 0 || snapshot.EventCursor.Sequence != 0 || journal.Cursor().Sequence != 0 {
		t.Fatalf("state committed without journal transition: snapshot=%#v cursor=%#v", snapshot, journal.Cursor())
	}
}

func TestRefreshEventPublishFailureRestoresThePreflightTrackerStatus(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{fetches: []fakeFetch{
		{issues: []domain.Issue{validIssue("GH-OLD")}},
		{issues: []domain.Issue{validIssue("GH-NEW")}},
	}}
	runtime, _, _, _, journal := newQueueRuntimeForTest(t, adapter, validQueueSnapshot("github", "", "digest-1"))
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	journal.Close()
	if _, err := runtime.Refresh(context.Background()); !errors.Is(err, observability.ErrJournalClosed) {
		t.Fatalf("Refresh publish error = %v", err)
	}
	after, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.Tracker.State != before.Tracker.State || after.Tracker.Stale != before.Tracker.Stale || after.Tracker.ErrorCode != before.Tracker.ErrorCode || len(after.Candidates) != 1 || after.Candidates[0].Issue.Identifier != "GH-OLD" {
		t.Fatalf("failed event publication left idle runtime in transient state: before=%#v after=%#v", before, after)
	}
}

func TestStartupDoesNotExposeACompletedRebuildReservation(t *testing.T) {
	t.Parallel()
	completionReached := make(chan struct{}, 1)
	releaseCompletion := make(chan struct{})
	var releaseOnce sync.Once
	adapter := &fakeAdapter{fetches: []fakeFetch{
		{issues: []domain.Issue{validIssue("GH-INITIAL")}},
		{issues: []domain.Issue{validIssue("GH-REFRESH")}},
	}}
	factory := &fakeFactory{adapters: []tracker.Adapter{adapter}}
	store := &fakeWorkflowStore{current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true, changes: make(chan workflow.Change, 1)}
	journal := observability.NewJournal(observability.JournalOptions{})
	runtime := newQueueRuntimeWithDependencies(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")}, Journal: journal,
	}, queueDependencies{
		now: newFakeQueueClock().Now, after: time.After, jitter: func(time.Duration) time.Duration { return 0 },
		beforeRebuildCompletion: func() {
			completionReached <- struct{}{}
			<-releaseCompletion
		},
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseCompletion) })
		_ = runtime.Shutdown(context.Background())
	})
	startResult := make(chan error, 1)
	go func() { startResult <- runtime.Start(context.Background()) }()
	<-completionReached
	select {
	case err := <-startResult:
		t.Fatalf("Start returned before its rebuild reservation was retired: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(releaseCompletion) })
	if err := <-startResult; err != nil {
		t.Fatal(err)
	}
	journal.Close()
	if _, err := runtime.Refresh(context.Background()); !errors.Is(err, observability.ErrJournalClosed) {
		t.Fatalf("post-start Refresh joined stale completed reservation: %v", err)
	}
}

func TestStartupOutcomePublicationWinsOwnerCancellationAtCompletionBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		fetchErr         error
		journalMaxBytes  int
		wantRefreshError error
		wantTrackerState string
		wantAdapter      bool
		wantEvents       uint64
	}{
		{name: "success", wantTrackerState: "ready", wantAdapter: true, wantEvents: 1},
		{
			name: "provider failure", fetchErr: trackerErr(tracker.CategoryTransport, true, 0),
			wantTrackerState: "failed", wantAdapter: true, wantEvents: 1,
		},
		{
			name: "journal failure", journalMaxBytes: 1, wantRefreshError: observability.ErrEventTooLarge,
			wantTrackerState: "starting",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fetchStarted := make(chan struct{}, 1)
			releaseFetch := make(chan struct{})
			completionReached := make(chan struct{}, 1)
			releaseCompletion := make(chan struct{})
			var releaseFetchOnce sync.Once
			var releaseCompletionOnce sync.Once
			adapter := &fakeAdapter{fetches: []fakeFetch{{
				issues: []domain.Issue{validIssue("GH-COMMITTED")}, err: test.fetchErr,
				wait: releaseFetch, called: fetchStarted,
			}}}
			factory := &fakeFactory{adapters: []tracker.Adapter{adapter}}
			store := &fakeWorkflowStore{
				current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true,
				changes: make(chan workflow.Change, 1),
			}
			journalOptions := observability.JournalOptions{}
			if test.journalMaxBytes > 0 {
				journalOptions.MaxEvents = 4
				journalOptions.MaxBytes = test.journalMaxBytes
			}
			journal := observability.NewJournal(journalOptions)
			runtime := newQueueRuntimeWithDependencies(QueueOptions{
				Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")}, Journal: journal,
			}, queueDependencies{
				now: newFakeQueueClock().Now, after: time.After, jitter: func(time.Duration) time.Duration { return 0 },
				beforeRebuildCompletion: func() {
					completionReached <- struct{}{}
					<-releaseCompletion
				},
			})
			t.Cleanup(func() {
				releaseFetchOnce.Do(func() { close(releaseFetch) })
				releaseCompletionOnce.Do(func() { close(releaseCompletion) })
				_ = runtime.Shutdown(context.Background())
			})

			ownerCtx, cancelOwner := context.WithCancel(context.Background())
			ownerResult := make(chan error, 1)
			go func() { ownerResult <- runtime.Start(ownerCtx) }()
			<-fetchStarted

			joinerResult := make(chan error, 1)
			go func() { joinerResult <- runtime.Start(context.Background()) }()
			type refreshResult struct {
				receipt domain.RefreshReceipt
				err     error
			}
			refreshResultChannel := make(chan refreshResult, 1)
			go func() {
				receipt, err := runtime.Refresh(context.Background())
				refreshResultChannel <- refreshResult{receipt: receipt, err: err}
			}()
			select {
			case err := <-joinerResult:
				t.Fatalf("Start joiner returned before the startup outcome: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			select {
			case result := <-refreshResultChannel:
				t.Fatalf("Refresh joiner returned before the startup outcome: %#v", result)
			case <-time.After(20 * time.Millisecond):
			}

			releaseFetchOnce.Do(func() { close(releaseFetch) })
			<-completionReached
			cancelOwner()
			var ownerEarly error
			ownerReturnedEarly := false
			select {
			case ownerEarly = <-ownerResult:
				ownerReturnedEarly = true
			case <-time.After(20 * time.Millisecond):
			}
			releaseCompletionOnce.Do(func() { close(releaseCompletion) })
			if !ownerReturnedEarly {
				ownerEarly = <-ownerResult
			}
			if ownerEarly != nil {
				t.Fatalf("published startup outcome returned owner cancellation: %v", ownerEarly)
			}
			if err := <-joinerResult; err != nil {
				t.Fatalf("Start joiner error = %v", err)
			}
			refreshed := <-refreshResultChannel
			if !refreshed.receipt.Coalesced || refreshed.receipt.Queued {
				t.Fatalf("startup Refresh receipt = %#v", refreshed.receipt)
			}
			if test.wantRefreshError != nil {
				if !errors.Is(refreshed.err, test.wantRefreshError) {
					t.Fatalf("startup Refresh error = %v, want %v", refreshed.err, test.wantRefreshError)
				}
			} else if test.fetchErr == nil {
				if refreshed.err != nil {
					t.Fatalf("startup Refresh error = %v", refreshed.err)
				}
			} else if refreshed.err == nil || !strings.Contains(refreshed.err.Error(), "tracker_transport") {
				t.Fatalf("startup Refresh provider error = %v", refreshed.err)
			}

			snapshot, err := runtime.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			runtime.mu.Lock()
			adapterPublished := runtime.adapter != nil
			generation := runtime.generation
			flightCleared := runtime.rebuildFlight == nil
			initializing := runtime.initializing
			startErr := runtime.startErr
			runtime.mu.Unlock()
			if snapshot.Tracker.State != test.wantTrackerState || adapterPublished != test.wantAdapter ||
				snapshot.EventCursor.Sequence != test.wantEvents || generation != 1 || !flightCleared || initializing || startErr != nil {
				t.Fatalf("startup boundary state = snapshot=%#v adapter=%t generation=%d flight_cleared=%t initializing=%t start_err=%v",
					snapshot, adapterPublished, generation, flightCleared, initializing, startErr)
			}
			if test.wantAdapter {
				if snapshot.Config.ActiveDigest != "digest-1" {
					t.Fatalf("active digest = %q", snapshot.Config.ActiveDigest)
				}
				if test.fetchErr == nil && (len(snapshot.Candidates) != 1 || snapshot.Candidates[0].Issue.Identifier != "GH-COMMITTED") {
					t.Fatalf("committed candidates = %#v", snapshot.Candidates)
				}
			} else if snapshot.Config.ActiveDigest != "" || len(snapshot.Candidates) != 0 {
				t.Fatalf("unpublished startup state = %#v", snapshot)
			}
		})
	}
}

func TestStartupBuildFailurePublicationWinsOwnerCancellationAtCompletionBoundary(t *testing.T) {
	t.Parallel()
	buildStarted := make(chan struct{}, 1)
	releaseBuild := make(chan struct{})
	completionReached := make(chan struct{}, 1)
	releaseCompletion := make(chan struct{})
	var releaseBuildOnce sync.Once
	var releaseCompletionOnce sync.Once
	factory := &fakeFactory{
		adapters: []tracker.Adapter{nil}, errors: []error{trackerErr(tracker.CategoryAuth, false, 0)},
		waits: []<-chan struct{}{releaseBuild}, called: []chan<- struct{}{buildStarted},
	}
	store := &fakeWorkflowStore{
		current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true,
		changes: make(chan workflow.Change, 1),
	}
	journal := observability.NewJournal(observability.JournalOptions{})
	runtime := newQueueRuntimeWithDependencies(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")}, Journal: journal,
	}, queueDependencies{
		now: newFakeQueueClock().Now, after: time.After, jitter: func(time.Duration) time.Duration { return 0 },
		beforeRebuildCompletion: func() {
			completionReached <- struct{}{}
			<-releaseCompletion
		},
	})
	t.Cleanup(func() {
		releaseBuildOnce.Do(func() { close(releaseBuild) })
		releaseCompletionOnce.Do(func() { close(releaseCompletion) })
		_ = runtime.Shutdown(context.Background())
	})

	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	ownerResult := make(chan error, 1)
	go func() { ownerResult <- runtime.Start(ownerCtx) }()
	<-buildStarted
	joinerResult := make(chan error, 1)
	go func() { joinerResult <- runtime.Start(context.Background()) }()
	type refreshResult struct {
		receipt domain.RefreshReceipt
		err     error
	}
	refreshResultChannel := make(chan refreshResult, 1)
	go func() {
		receipt, err := runtime.Refresh(context.Background())
		refreshResultChannel <- refreshResult{receipt: receipt, err: err}
	}()
	select {
	case err := <-joinerResult:
		t.Fatalf("Start joiner returned before build outcome: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case result := <-refreshResultChannel:
		t.Fatalf("Refresh joiner returned before build outcome: %#v", result)
	case <-time.After(20 * time.Millisecond):
	}
	releaseBuildOnce.Do(func() { close(releaseBuild) })
	<-completionReached
	cancelOwner()
	releaseCompletionOnce.Do(func() { close(releaseCompletion) })
	if err := <-ownerResult; err != nil {
		t.Fatalf("published build failure returned owner cancellation: %v", err)
	}
	if err := <-joinerResult; err != nil {
		t.Fatalf("Start joiner error = %v", err)
	}
	refreshed := <-refreshResultChannel
	if !refreshed.receipt.Coalesced || refreshed.receipt.Queued || refreshed.err == nil || !strings.Contains(refreshed.err.Error(), "tracker_auth") {
		t.Fatalf("Refresh joiner result = receipt=%#v err=%v", refreshed.receipt, refreshed.err)
	}
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	adapterPublished := runtime.adapter != nil
	flightCleared := runtime.rebuildFlight == nil
	committedGeneration := runtime.generationCommitted
	runtime.mu.Unlock()
	if snapshot.Tracker.State != "failed" || snapshot.Tracker.ErrorCode != "tracker_auth" || adapterPublished ||
		snapshot.Config.ActiveDigest != "" || len(snapshot.Candidates) != 0 || snapshot.EventCursor.Sequence != 1 ||
		!flightCleared || !committedGeneration {
		t.Fatalf("published build failure state = snapshot=%#v adapter=%t flight_cleared=%t committed=%t",
			snapshot, adapterPublished, flightCleared, committedGeneration)
	}
}

func TestQueueDoesNotCommitInvalidConfigurationWhenJournalIsClosed(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-1")}}}}
	runtime, _, _, _, journal := newQueueRuntimeForTest(t, adapter, validQueueSnapshot("github", "", "digest-1"))
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	journal.Close()
	runtime.handleWorkflowChange(context.Background(), workflow.Change{
		Digest: "invalid-digest",
		Validation: workflow.ValidationResult{Valid: false, FieldErrors: []workflow.FieldError{{
			Field: "tracker.kind", Code: "invalid_tracker_config", Message: "canary detail",
		}}},
	})
	after, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.Config != before.Config || after.EventCursor != before.EventCursor {
		t.Fatalf("invalid configuration committed without journal event: before=%#v after=%#v", before.Config, after.Config)
	}
}

func TestValidWorkflowEventPublishFailureRetainsTheActiveGeneration(t *testing.T) {
	t.Parallel()
	oldAdapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-OLD")}}}}
	newAdapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-NEW")}}}}
	runtime, factory, _, _, journal := newQueueRuntimeForTest(t, oldAdapter, validQueueSnapshot("github", "", "digest-1"))
	factory.adapters = []tracker.Adapter{oldAdapter, newAdapter}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	beforeGeneration := runtime.generation
	runtime.mu.Unlock()
	journal.Close()
	updated := validQueueSnapshot("github", "", "digest-2")
	runtime.handleWorkflowChange(context.Background(), workflow.Change{
		Snapshot: updated, Digest: updated.Digest, Validation: workflow.ValidationResult{Valid: true},
	})
	runtime.mu.Lock()
	activeAdapter := runtime.adapter
	afterGeneration := runtime.generation
	runtime.mu.Unlock()
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if activeAdapter != oldAdapter || afterGeneration != beforeGeneration || snapshot.Config.Digest != "digest-1" || factory.buildCount() != 1 {
		t.Fatalf("unpublished valid change retired active generation: adapter=%T generations=%d/%d config=%#v builds=%d", activeAdapter, beforeGeneration, afterGeneration, snapshot.Config, factory.buildCount())
	}
}

func TestBuildFailureDoesNotCommitFailedStateWhenItsJournalEventCannotPublish(t *testing.T) {
	t.Parallel()
	buildStarted := make(chan struct{}, 1)
	releaseBuild := make(chan struct{})
	oldAdapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-OLD")}}}}
	runtime, factory, _, _, journal := newQueueRuntimeForTest(t, oldAdapter, validQueueSnapshot("github", "", "digest-1"))
	factory.adapters = []tracker.Adapter{oldAdapter, nil}
	factory.errors = []error{nil, trackerErr(tracker.CategoryAuth, false, 0)}
	factory.waits = []<-chan struct{}{nil, releaseBuild}
	factory.called = []chan<- struct{}{nil, buildStarted}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	updated := validQueueSnapshot("github", "", "digest-2")
	done := make(chan struct{})
	go func() {
		runtime.handleWorkflowChange(context.Background(), workflow.Change{
			Snapshot: updated, Digest: updated.Digest, Validation: workflow.ValidationResult{Valid: true},
		})
		close(done)
	}()
	<-buildStarted
	journal.Close()
	close(releaseBuild)
	<-done
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Tracker.State != "rebuilding" || snapshot.Tracker.ErrorCode != "" || snapshot.EventCursor.Sequence != 2 {
		t.Fatalf("build failure committed without queue.failed event: %#v", snapshot)
	}
}

func TestCredentialRetirementDiscardsLateOldGenerationAndRebuildsOnce(t *testing.T) {
	t.Parallel()
	oldStarted := make(chan struct{}, 1)
	releaseOld := make(chan struct{})
	oldAdapter := &fakeAdapter{fetches: []fakeFetch{
		{issues: []domain.Issue{validIssue("GH-1")}},
		{issues: []domain.Issue{validIssue("GH-OLD")}, wait: releaseOld, called: oldStarted, ignoreCancellation: true},
	}}
	newAdapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-NEW")}}}}
	runtime, factory, _, _, journal := newQueueRuntimeForTest(t, oldAdapter, validQueueSnapshot("github", "", "digest-1"))
	factory.adapters = []tracker.Adapter{oldAdapter, newAdapter}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldResult := make(chan error, 1)
	go func() {
		_, err := runtime.Refresh(context.Background())
		oldResult <- err
	}()
	<-oldStarted
	runtime.NotifyCredentialChanged()
	waitFor(t, "credential rebuild", func() bool { return newAdapter.callCount() == 1 })
	close(releaseOld)
	if err := <-oldResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("retired caller error = %v", err)
	}
	waitFor(t, "new candidate commit", func() bool {
		snapshot, _ := runtime.Snapshot(context.Background())
		return len(snapshot.Candidates) == 1 && snapshot.Candidates[0].Issue.Identifier == "GH-NEW"
	})
	time.Sleep(10 * time.Millisecond)
	snapshot, _ := runtime.Snapshot(context.Background())
	if snapshot.Candidates[0].Issue.Identifier != "GH-NEW" || oldAdapter.callCount() != 2 {
		t.Fatalf("late old generation overwrote state or made new request: %#v calls=%d", snapshot.Candidates, oldAdapter.callCount())
	}
	page := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 0})
	for _, event := range page.Events {
		encoded, _ := json.Marshal(event)
		if strings.Contains(string(encoded), "GH-OLD") {
			t.Fatalf("retired result reached journal: %s", encoded)
		}
	}
}

func TestNewestValidChangeRetiresBlockedGenerationAndDiscardsLateOutcome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		blockedErr error
	}{
		{name: "success"},
		{name: "error", blockedErr: &tracker.Error{Category: tracker.Category("raw-stale-category-canary"), Message: "raw stale message canary", Retryable: true, RetryAfter: time.Hour, Status: 599}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			blockedStarted := make(chan struct{}, 1)
			releaseBlocked := make(chan struct{})
			var releaseOnce sync.Once
			initial := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-INITIAL")}}}}
			blocked := &fakeAdapter{fetches: []fakeFetch{{
				issues: []domain.Issue{validIssue("GH-OBSOLETE")}, err: test.blockedErr,
				wait: releaseBlocked, called: blockedStarted, ignoreCancellation: true,
			}}}
			newest := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-NEWEST")}}}}
			runtime, factory, store, _, journal := newQueueRuntimeForTest(t, initial, validQueueSnapshot("github", "", "digest-1"))
			t.Cleanup(func() { releaseOnce.Do(func() { close(releaseBlocked) }) })
			factory.adapters = []tracker.Adapter{initial, blocked, newest}
			if err := runtime.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			digest2 := validQueueSnapshot("github", "", "digest-2")
			store.setCurrent(digest2)
			store.changes <- workflow.Change{Snapshot: digest2, Digest: digest2.Digest, Validation: workflow.ValidationResult{Valid: true}}
			<-blockedStarted
			blockedContext := blocked.callContext(0)
			if blockedContext == nil {
				t.Fatal("blocked generation did not retain its fetch context")
			}

			digest3 := validQueueSnapshot("github", "", "digest-3")
			store.setCurrent(digest3)
			store.changes <- workflow.Change{Snapshot: digest3, Digest: digest3.Digest, Validation: workflow.ValidationResult{Valid: true}}
			waitFor(t, "digest 3 observation", func() bool {
				snapshot, _ := runtime.Snapshot(context.Background())
				return snapshot.Config.Digest == "digest-3" && len(store.changes) == 0
			})
			select {
			case <-blockedContext.Done():
			case <-time.After(100 * time.Millisecond):
				t.Fatal("newest valid change did not synchronously cancel the blocked generation")
			}
			if factory.buildCount() != 2 {
				t.Fatalf("serialized worker started newest build before blocked provider returned: builds=%d", factory.buildCount())
			}

			releaseOnce.Do(func() { close(releaseBlocked) })
			waitFor(t, "newest generation commit", func() bool {
				snapshot, _ := runtime.Snapshot(context.Background())
				return len(snapshot.Candidates) == 1 && snapshot.Candidates[0].Issue.Identifier == "GH-NEWEST"
			})
			if factory.buildCount() != 3 || blocked.callCount() != 1 || newest.callCount() != 1 {
				t.Fatalf("newest-wins builds=%d blocked_polls=%d newest_polls=%d", factory.buildCount(), blocked.callCount(), newest.callCount())
			}
			page := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 0})
			refreshed := 0
			failed := 0
			for _, event := range page.Events {
				switch event.Type {
				case "queue.refreshed":
					refreshed++
				case "queue.failed":
					failed++
				}
			}
			encoded, _ := json.Marshal(page)
			if refreshed != 2 || failed != 0 || strings.Contains(string(encoded), "raw-stale-category-canary") || strings.Contains(string(encoded), "raw stale message canary") {
				t.Fatalf("obsolete outcome reached public journal: refreshed=%d failed=%d page=%s", refreshed, failed, encoded)
			}
		})
	}
}

func TestNewestWinsStormKeepsOnlyOnePendingRebuild(t *testing.T) {
	t.Parallel()
	blockedStarted := make(chan struct{}, 1)
	releaseBlocked := make(chan struct{})
	var releaseOnce sync.Once
	initial := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-INITIAL")}}}}
	blocked := &fakeAdapter{fetches: []fakeFetch{{
		issues: []domain.Issue{validIssue("GH-OBSOLETE")}, wait: releaseBlocked, called: blockedStarted, ignoreCancellation: true,
	}}}
	newest := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-12")}}}}
	runtime, factory, store, _, _ := newQueueRuntimeForTest(t, initial, validQueueSnapshot("github", "", "digest-1"))
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseBlocked) }) })
	factory.adapters = []tracker.Adapter{initial, blocked, newest}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	digest2 := validQueueSnapshot("github", "", "digest-2")
	store.changes <- workflow.Change{Snapshot: digest2, Digest: digest2.Digest, Validation: workflow.ValidationResult{Valid: true}}
	<-blockedStarted
	blockedContext := blocked.callContext(0)
	for digest := 3; digest <= 12; digest++ {
		snapshot := validQueueSnapshot("github", "", fmt.Sprintf("digest-%d", digest))
		store.setCurrent(snapshot)
		store.changes <- workflow.Change{Snapshot: snapshot, Digest: snapshot.Digest, Validation: workflow.ValidationResult{Valid: true}}
	}
	waitFor(t, "newest storm observation", func() bool {
		snapshot, _ := runtime.Snapshot(context.Background())
		return snapshot.Config.Digest == "digest-12" && len(store.changes) == 0
	})
	if blockedContext == nil || blockedContext.Err() == nil || factory.buildCount() != 2 {
		t.Fatalf("storm did not retire blocked work while preserving serialization: context=%v builds=%d", blockedContext, factory.buildCount())
	}
	releaseOnce.Do(func() { close(releaseBlocked) })
	waitFor(t, "storm newest commit", func() bool {
		snapshot, _ := runtime.Snapshot(context.Background())
		return len(snapshot.Candidates) == 1 && snapshot.Candidates[0].Issue.Identifier == "GH-12"
	})
	if factory.buildCount() != 3 || newest.callCount() != 1 {
		t.Fatalf("storm executed superseded pending intents: builds=%d newest_polls=%d", factory.buildCount(), newest.callCount())
	}
}

func TestInvalidChangeSupersedesBlockedValidRebuildAndDiscardsLateOutcome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		blockAt string
		lateErr error
	}{
		{name: "blocked_build_late_success", blockAt: "build"},
		{name: "blocked_build_late_error", blockAt: "build", lateErr: trackerErr(tracker.CategoryAuth, false, 0)},
		{name: "blocked_fetch_late_success", blockAt: "fetch"},
		{name: "blocked_fetch_late_error", blockAt: "fetch", lateErr: trackerErr(tracker.CategoryTransport, true, 0)},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			blockedStarted := make(chan struct{}, 1)
			releaseBlocked := make(chan struct{})
			var releaseOnce sync.Once
			rebuildCompleted := make(chan struct{}, 4)
			initial := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-INITIAL")}}}}
			obsolete := &fakeAdapter{fetches: []fakeFetch{{
				issues: []domain.Issue{validIssue("GH-OBSOLETE")}, err: test.lateErr,
			}}}
			factory := &fakeFactory{adapters: []tracker.Adapter{initial, obsolete}}
			if test.blockAt == "build" {
				factory.waits = []<-chan struct{}{nil, releaseBlocked}
				factory.called = []chan<- struct{}{nil, blockedStarted}
				factory.errors = []error{nil, test.lateErr}
			} else {
				obsolete.fetches[0].wait = releaseBlocked
				obsolete.fetches[0].called = blockedStarted
				obsolete.fetches[0].ignoreCancellation = true
			}
			store := &fakeWorkflowStore{current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true, changes: make(chan workflow.Change, 16)}
			clock := newFakeQueueClock()
			journal := observability.NewJournal(observability.JournalOptions{})
			runtime := newQueueRuntimeWithDependencies(QueueOptions{
				Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")}, Journal: journal,
			}, queueDependencies{
				now: clock.Now, after: clock.After, jitter: func(time.Duration) time.Duration { return 0 },
				beforeRebuildCompletion: func() { rebuildCompleted <- struct{}{} },
			})
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(releaseBlocked) })
				_ = runtime.Shutdown(context.Background())
			})
			if err := runtime.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			<-rebuildCompleted
			digest2 := validQueueSnapshot("github", "", "digest-2")
			store.setCurrent(digest2)
			store.changes <- workflow.Change{Snapshot: digest2, Digest: digest2.Digest, Validation: workflow.ValidationResult{Valid: true}}
			<-blockedStarted
			var blockedContext context.Context
			if test.blockAt == "build" {
				blockedContext = factory.buildContext(1)
			} else {
				blockedContext = obsolete.callContext(0)
			}
			if blockedContext == nil {
				t.Fatal("blocked valid generation did not retain its context")
			}

			clock.Advance(time.Second)
			store.changes <- workflow.Change{Digest: "invalid-digest-3", Validation: workflow.ValidationResult{
				Valid: false, FieldErrors: []workflow.FieldError{{Code: "invalid_tracker_config", Message: "unsafe detail 3"}},
			}}
			waitFor(t, "invalid digest 3 observation", func() bool {
				snapshot, _ := runtime.Snapshot(context.Background())
				return snapshot.Config.Digest == "invalid-digest-3" && len(store.changes) == 0
			})
			select {
			case <-blockedContext.Done():
			case <-time.After(100 * time.Millisecond):
				t.Fatal("invalid watcher change did not synchronously cancel the valid rebuild generation")
			}
			clock.Advance(time.Second)
			store.changes <- workflow.Change{Digest: "invalid-digest-4", Validation: workflow.ValidationResult{
				Valid: false, FieldErrors: []workflow.FieldError{{Code: "invalid_polling_interval", Message: "unsafe detail 4"}},
			}}
			waitFor(t, "repeated invalid observation", func() bool {
				snapshot, _ := runtime.Snapshot(context.Background())
				return snapshot.Config.Digest == "invalid-digest-4" && len(store.changes) == 0
			})
			invalidSnapshot, _ := runtime.Snapshot(context.Background())
			invalidEvents := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 0})
			if invalidSnapshot.Config.State != "invalid" || invalidSnapshot.Config.ActiveDigest != "" || invalidSnapshot.Config.UsingLastGood ||
				invalidSnapshot.Config.ErrorCode != "invalid_polling_interval" || len(invalidSnapshot.Candidates) != 1 || invalidSnapshot.Candidates[0].Issue.Identifier != "GH-INITIAL" {
				t.Fatalf("invalid supersession state = %#v", invalidSnapshot)
			}

			releaseOnce.Do(func() { close(releaseBlocked) })
			<-rebuildCompleted
			afterLate, _ := runtime.Snapshot(context.Background())
			afterEvents := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 0})
			if !reflect.DeepEqual(afterLate, invalidSnapshot) || !reflect.DeepEqual(afterEvents, invalidEvents) {
				t.Fatalf("late obsolete outcome changed public state: invalid=%#v after=%#v events_before=%#v events_after=%#v", invalidSnapshot, afterLate, invalidEvents, afterEvents)
			}
			if test.blockAt == "build" && obsolete.callCount() != 0 {
				t.Fatalf("obsolete adapter fetched after its build was invalidated: polls=%d", obsolete.callCount())
			}
		})
	}
}

func TestInvalidChangeFencesQueuedValidRebuildWhenStatusJournalRejects(t *testing.T) {
	t.Parallel()
	buildStarted := make(chan struct{}, 1)
	releaseBuild := make(chan struct{})
	rebuildCompleted := make(chan struct{}, 4)
	var releaseOnce sync.Once
	initial := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-INITIAL")}}}}
	obsolete := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-OBSOLETE")}}}}
	queued := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-QUEUED")}}}}
	factory := &fakeFactory{
		adapters: []tracker.Adapter{initial, obsolete, queued},
		waits:    []<-chan struct{}{nil, releaseBuild},
		called:   []chan<- struct{}{nil, buildStarted},
	}
	store := &fakeWorkflowStore{
		current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true,
		changes: make(chan workflow.Change, 4),
	}
	journal := observability.NewJournal(observability.JournalOptions{})
	runtime := newQueueRuntimeWithDependencies(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")}, Journal: journal,
	}, queueDependencies{
		now: newFakeQueueClock().Now, after: time.After, jitter: func(time.Duration) time.Duration { return 0 },
		beforeRebuildCompletion: func() { rebuildCompleted <- struct{}{} },
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseBuild) })
		_ = runtime.Shutdown(context.Background())
	})
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-rebuildCompleted

	digest2 := validQueueSnapshot("github", "", "digest-2")
	store.setCurrent(digest2)
	store.changes <- workflow.Change{Snapshot: digest2, Digest: digest2.Digest, Validation: workflow.ValidationResult{Valid: true}}
	<-buildStarted
	digest3 := validQueueSnapshot("github", "", "digest-3")
	store.setCurrent(digest3)
	store.changes <- workflow.Change{Snapshot: digest3, Digest: digest3.Digest, Validation: workflow.ValidationResult{Valid: true}}
	waitFor(t, "queued digest 3", func() bool {
		snapshot, _ := runtime.Snapshot(context.Background())
		return snapshot.Config.Digest == "digest-3" && len(store.changes) == 0
	})

	journal.Close()
	runtime.handleWorkflowChange(context.Background(), workflow.Change{
		Digest: "invalid-digest",
		Validation: workflow.ValidationResult{Valid: false, FieldErrors: []workflow.FieldError{{
			Field: "polling.interval", Code: "invalid_polling_interval", Message: "unsafe detail",
		}}},
	})
	runtime.mu.Lock()
	pending := runtime.pendingRebuild
	flight := runtime.rebuildFlight
	activeAdapter := runtime.adapter
	config := runtime.config
	runtime.mu.Unlock()
	if pending != nil || flight != nil || activeAdapter != nil || config.State != "valid" || config.Digest != "digest-3" || config.ActiveDigest != "" {
		t.Fatalf("rejected invalid status did not fence queued rebuild: pending=%#v flight=%#v adapter=%#v config=%#v", pending, flight, activeAdapter, config)
	}

	releaseOnce.Do(func() { close(releaseBuild) })
	<-rebuildCompleted
	time.Sleep(10 * time.Millisecond)
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if factory.buildCount() != 2 || len(snapshot.Candidates) != 1 || snapshot.Candidates[0].Issue.Identifier != "GH-INITIAL" ||
		snapshot.Config != config || snapshot.EventCursor.Sequence != 3 {
		t.Fatalf("fenced queued rebuild committed late: builds=%d snapshot=%#v", factory.buildCount(), snapshot)
	}
}

func TestInvalidChangePreservesActiveGenerationDuringOrdinaryRefresh(t *testing.T) {
	t.Parallel()
	refreshStarted := make(chan struct{}, 1)
	releaseRefresh := make(chan struct{})
	var releaseOnce sync.Once
	adapter := &fakeAdapter{fetches: []fakeFetch{
		{issues: []domain.Issue{validIssue("GH-INITIAL")}},
		{issues: []domain.Issue{validIssue("GH-REFRESHED")}, wait: releaseRefresh, called: refreshStarted, ignoreCancellation: true},
	}}
	runtime, _, _, _, _ := newQueueRuntimeForTest(t, adapter, validQueueSnapshot("github", "", "digest-1"))
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseRefresh) }) })
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	generation := runtime.generation
	pollInterval := runtime.pollInterval
	runtime.mu.Unlock()
	refreshResult := make(chan error, 1)
	go func() {
		_, err := runtime.Refresh(context.Background())
		refreshResult <- err
	}()
	<-refreshStarted
	refreshCtx := adapter.callContext(1)
	runtime.handleWorkflowChange(context.Background(), workflow.Change{
		Digest: "invalid-digest",
		Validation: workflow.ValidationResult{Valid: false, FieldErrors: []workflow.FieldError{{
			Field: "tracker.kind", Code: "invalid_tracker_config", Message: "unsafe detail",
		}}},
	})
	if refreshCtx == nil || refreshCtx.Err() != nil {
		t.Fatalf("ordinary refresh generation was canceled by invalid disk observation: %v", refreshCtx)
	}
	during, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	duringGeneration := runtime.generation
	duringPollInterval := runtime.pollInterval
	activeAdapter := runtime.adapter
	runtime.mu.Unlock()
	if during.Config.State != "invalid" || !during.Config.UsingLastGood || during.Config.ActiveDigest != "digest-1" ||
		len(during.Candidates) != 1 || during.Candidates[0].Issue.Identifier != "GH-INITIAL" ||
		duringGeneration != generation || duringPollInterval != pollInterval || activeAdapter != adapter {
		t.Fatalf("invalid observation retired active generation: snapshot=%#v generation=%d/%d poll=%s/%s adapter=%#v",
			during, duringGeneration, generation, duringPollInterval, pollInterval, activeAdapter)
	}
	releaseOnce.Do(func() { close(releaseRefresh) })
	if err := <-refreshResult; err != nil {
		t.Fatal(err)
	}
	after, _ := runtime.Snapshot(context.Background())
	if after.Config.State != "invalid" || !after.Config.UsingLastGood || after.Config.ActiveDigest != "digest-1" ||
		len(after.Candidates) != 1 || after.Candidates[0].Issue.Identifier != "GH-REFRESHED" {
		t.Fatalf("ordinary refresh did not remain active under invalid observation: %#v", after)
	}
}

func TestInvalidChangeImmediatelyAfterRebuildCommitPreservesCommittedAdapter(t *testing.T) {
	t.Parallel()
	commitReached := make(chan struct{}, 1)
	releaseCommit := make(chan struct{})
	var releaseOnce sync.Once
	var hookMu sync.Mutex
	hookCalls := 0
	initial := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-INITIAL")}}}}
	committed := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-COMMITTED")}}}}
	factory := &fakeFactory{adapters: []tracker.Adapter{initial, committed}}
	store := &fakeWorkflowStore{
		current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true,
		changes: make(chan workflow.Change, 2),
	}
	runtime := newQueueRuntimeWithDependencies(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")},
		Journal: observability.NewJournal(observability.JournalOptions{}),
	}, queueDependencies{
		now: newFakeQueueClock().Now, after: time.After, jitter: func(time.Duration) time.Duration { return 0 },
		beforeRebuildCompletion: func() {
			hookMu.Lock()
			hookCalls++
			call := hookCalls
			hookMu.Unlock()
			if call == 2 {
				commitReached <- struct{}{}
				<-releaseCommit
			}
		},
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseCommit) })
		_ = runtime.Shutdown(context.Background())
	})
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	digest2 := validQueueSnapshot("github", "", "digest-2")
	store.setCurrent(digest2)
	store.changes <- workflow.Change{Snapshot: digest2, Digest: digest2.Digest, Validation: workflow.ValidationResult{Valid: true}}
	<-commitReached
	runtime.handleWorkflowChange(context.Background(), workflow.Change{
		Digest: "invalid-digest",
		Validation: workflow.ValidationResult{Valid: false, FieldErrors: []workflow.FieldError{{
			Field: "polling.interval", Code: "invalid_polling_interval", Message: "unsafe detail",
		}}},
	})
	during, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	activeAdapter := runtime.adapter
	committedGeneration := runtime.generationCommitted
	runtime.mu.Unlock()
	if during.Config.State != "invalid" || !during.Config.UsingLastGood || during.Config.ActiveDigest != "digest-2" ||
		len(during.Candidates) != 1 || during.Candidates[0].Issue.Identifier != "GH-COMMITTED" || activeAdapter != committed || !committedGeneration {
		t.Fatalf("invalid observation retired a committed rebuild: snapshot=%#v adapter=%#v committed=%t", during, activeAdapter, committedGeneration)
	}
	releaseOnce.Do(func() { close(releaseCommit) })
	waitFor(t, "rebuild completion", func() bool {
		runtime.mu.Lock()
		defer runtime.mu.Unlock()
		return runtime.rebuildFlight == nil
	})
	after, _ := runtime.Snapshot(context.Background())
	if !reflect.DeepEqual(after, during) {
		t.Fatalf("external rebuild completion changed committed invalid state: before=%#v after=%#v", during, after)
	}
}

func TestInvalidChangeCancelsManualRebuildReservedBeforeObservation(t *testing.T) {
	t.Parallel()
	manualStarted := make(chan struct{}, 1)
	releaseManual := make(chan struct{})
	var releaseOnce sync.Once
	recovered := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-TOO-LATE")}}}}
	factory := &fakeFactory{
		adapters: []tracker.Adapter{nil, recovered},
		errors:   []error{trackerErr(tracker.CategoryAuth, false, 0), nil},
	}
	store := &fakeWorkflowStore{
		current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true,
		changes: make(chan workflow.Change, 1),
	}
	runtime := newQueueRuntimeWithDependencies(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")},
		Journal: observability.NewJournal(observability.JournalOptions{}),
	}, queueDependencies{
		now: newFakeQueueClock().Now, after: time.After, jitter: func(time.Duration) time.Duration { return 0 },
		beforeManualRebuild: func() {
			manualStarted <- struct{}{}
			<-releaseManual
		},
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseManual) })
		_ = runtime.Shutdown(context.Background())
	})
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	type refreshResult struct {
		receipt domain.RefreshReceipt
		err     error
	}
	refreshResultChannel := make(chan refreshResult, 1)
	go func() {
		receipt, err := runtime.Refresh(context.Background())
		refreshResultChannel <- refreshResult{receipt: receipt, err: err}
	}()
	<-manualStarted
	runtime.handleWorkflowChange(context.Background(), workflow.Change{
		Digest: "invalid-digest",
		Validation: workflow.ValidationResult{Valid: false, FieldErrors: []workflow.FieldError{{
			Field: "polling.interval", Code: "invalid_polling_interval", Message: "unsafe detail",
		}}},
	})
	var refreshed refreshResult
	returnedBeforeRelease := false
	select {
	case refreshed = <-refreshResultChannel:
		returnedBeforeRelease = true
	case <-time.After(100 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(releaseManual) })
	if !returnedBeforeRelease {
		refreshed = <-refreshResultChannel
		t.Fatalf("invalid observation did not synchronously cancel reserved manual rebuild; eventual result=%#v", refreshed)
	}
	if !errors.Is(refreshed.err, context.Canceled) || !refreshed.receipt.Queued || refreshed.receipt.Coalesced {
		t.Fatalf("superseded manual rebuild result = %#v", refreshed)
	}
	time.Sleep(10 * time.Millisecond)
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if factory.buildCount() != 1 || recovered.callCount() != 0 || snapshot.Config.State != "invalid" ||
		snapshot.Config.ActiveDigest != "" || snapshot.Config.UsingLastGood || len(snapshot.Candidates) != 0 || snapshot.EventCursor.Sequence != 2 {
		t.Fatalf("pre-invalid manual rebuild committed late: builds=%d polls=%d snapshot=%#v", factory.buildCount(), recovered.callCount(), snapshot)
	}
	currentCalls, loadCalls, _ := store.accessCounts()
	if currentCalls != 1 || loadCalls != 0 {
		t.Fatalf("pre-invalid manual intent touched store afterward: current=%d load=%d", currentCalls, loadCalls)
	}

	receipt, err := runtime.Refresh(context.Background())
	if err != nil || !receipt.Queued || receipt.Coalesced {
		t.Fatalf("post-invalid manual recovery = receipt=%#v err=%v", receipt, err)
	}
	snapshot, err = runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	currentCalls, loadCalls, _ = store.accessCounts()
	if currentCalls != 2 || loadCalls != 0 || factory.buildCount() != 2 || snapshot.Config.State != "invalid" ||
		!snapshot.Config.UsingLastGood || snapshot.Config.ActiveDigest != "digest-1" || len(snapshot.Candidates) != 1 ||
		snapshot.Candidates[0].Issue.Identifier != "GH-TOO-LATE" {
		t.Fatalf("post-invalid manual recovery = current=%d load=%d builds=%d snapshot=%#v",
			currentCalls, loadCalls, factory.buildCount(), snapshot)
	}
}

func TestCredentialNotificationBeforeInvalidObservationCannotRecoverAfterward(t *testing.T) {
	t.Parallel()
	credentialStarted := make(chan struct{}, 1)
	releaseCredential := make(chan struct{})
	credentialFinished := make(chan struct{}, 2)
	var releaseOnce sync.Once
	var hookMu sync.Mutex
	hookCalls := 0
	recovered := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-RECOVERED")}}}}
	factory := &fakeFactory{
		adapters: []tracker.Adapter{nil, recovered},
		errors:   []error{trackerErr(tracker.CategoryAuth, false, 0), nil},
	}
	store := &fakeWorkflowStore{
		current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true,
		changes: make(chan workflow.Change, 1),
	}
	runtime := newQueueRuntimeWithDependencies(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")},
		Journal: observability.NewJournal(observability.JournalOptions{}),
	}, queueDependencies{
		now: newFakeQueueClock().Now, after: time.After, jitter: func(time.Duration) time.Duration { return 0 },
		beforeCredentialRebuild: func() {
			hookMu.Lock()
			hookCalls++
			call := hookCalls
			hookMu.Unlock()
			if call == 1 {
				credentialStarted <- struct{}{}
				<-releaseCredential
			}
		},
		afterCredentialRebuild: func() { credentialFinished <- struct{}{} },
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseCredential) })
		_ = runtime.Shutdown(context.Background())
	})
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtime.NotifyCredentialChanged()
	<-credentialStarted
	runtime.handleWorkflowChange(context.Background(), workflow.Change{
		Digest: "invalid-digest",
		Validation: workflow.ValidationResult{Valid: false, FieldErrors: []workflow.FieldError{{
			Field: "tracker.kind", Code: "invalid_tracker_config", Message: "unsafe detail",
		}}},
	})
	releaseOnce.Do(func() { close(releaseCredential) })
	<-credentialFinished
	runtime.mu.Lock()
	preInvalidFlight := runtime.rebuildFlight
	preInvalidAdapter := runtime.adapter
	runtime.mu.Unlock()
	if factory.buildCount() != 1 || preInvalidFlight != nil || preInvalidAdapter != nil {
		t.Fatalf("pre-invalid credential intent rebuilt afterward: builds=%d flight=%#v adapter=%#v", factory.buildCount(), preInvalidFlight, preInvalidAdapter)
	}
	currentCalls, loadCalls, _ := store.accessCounts()
	if currentCalls != 1 || loadCalls != 0 {
		t.Fatalf("pre-invalid credential intent touched store afterward: current=%d load=%d", currentCalls, loadCalls)
	}

	runtime.NotifyCredentialChanged()
	<-credentialFinished
	waitFor(t, "post-invalid credential recovery", func() bool {
		snapshot, _ := runtime.Snapshot(context.Background())
		return snapshot.Tracker.State == "ready" && len(snapshot.Candidates) == 1
	})
	snapshot, _ := runtime.Snapshot(context.Background())
	if factory.buildCount() != 2 || snapshot.Config.State != "invalid" || !snapshot.Config.UsingLastGood ||
		snapshot.Config.ActiveDigest != "digest-1" || snapshot.Candidates[0].Issue.Identifier != "GH-RECOVERED" {
		t.Fatalf("post-invalid credential recovery = builds=%d snapshot=%#v", factory.buildCount(), snapshot)
	}
	currentCalls, loadCalls, _ = store.accessCounts()
	if currentCalls != 2 || loadCalls != 0 {
		t.Fatalf("post-invalid credential recovery store reads: current=%d load=%d", currentCalls, loadCalls)
	}
}

func TestInvalidatedManualRebuildSkipsStoreBeforeBoundedShutdown(t *testing.T) {
	t.Parallel()
	manualStarted := make(chan struct{}, 1)
	releaseManual := make(chan struct{})
	blockUnexpectedStoreRead := make(chan struct{})
	var releaseManualOnce sync.Once
	var releaseStoreOnce sync.Once
	factory := &fakeFactory{
		adapters: []tracker.Adapter{nil},
		errors:   []error{trackerErr(tracker.CategoryAuth, false, 0)},
	}
	store := &fakeWorkflowStore{
		current:      validQueueSnapshot("github", "", "digest-1"),
		hasCurrent:   true,
		changes:      make(chan workflow.Change, 1),
		currentWaits: []<-chan struct{}{nil, blockUnexpectedStoreRead},
	}
	runtime := newQueueRuntimeWithDependencies(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")},
		Journal: observability.NewJournal(observability.JournalOptions{}),
	}, queueDependencies{
		now: newFakeQueueClock().Now, after: time.After, jitter: func(time.Duration) time.Duration { return 0 },
		beforeManualRebuild: func() {
			manualStarted <- struct{}{}
			<-releaseManual
		},
	})
	t.Cleanup(func() {
		releaseManualOnce.Do(func() { close(releaseManual) })
		releaseStoreOnce.Do(func() { close(blockUnexpectedStoreRead) })
		_ = runtime.Shutdown(context.Background())
	})
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	refreshResult := make(chan error, 1)
	go func() {
		_, err := runtime.Refresh(context.Background())
		refreshResult <- err
	}()
	<-manualStarted
	runtime.handleWorkflowChange(context.Background(), workflow.Change{
		Digest: "invalid-digest",
		Validation: workflow.ValidationResult{Valid: false, FieldErrors: []workflow.FieldError{{
			Field: "polling.interval", Code: "invalid_polling_interval", Message: "unsafe detail",
		}}},
	})
	if err := <-refreshResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("invalidated manual refresh error = %v", err)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	shutdownResult := make(chan error, 1)
	go func() { shutdownResult <- runtime.Shutdown(shutdownCtx) }()
	waitFor(t, "manual shutdown admission", func() bool {
		runtime.mu.Lock()
		defer runtime.mu.Unlock()
		return runtime.closed
	})
	releaseManualOnce.Do(func() { close(releaseManual) })
	if err := <-shutdownResult; err != nil {
		currentCalls, loadCalls, _ := store.accessCounts()
		t.Fatalf("invalidated manual store read blocked shutdown: err=%v current=%d load=%d", err, currentCalls, loadCalls)
	}
	currentCalls, loadCalls, _ := store.accessCounts()
	if currentCalls != 1 || loadCalls != 0 || factory.buildCount() != 1 {
		t.Fatalf("invalidated manual intent touched dependencies: current=%d load=%d builds=%d",
			currentCalls, loadCalls, factory.buildCount())
	}
}

func TestInvalidatedCredentialRebuildSkipsStoreBeforeBoundedShutdown(t *testing.T) {
	t.Parallel()
	credentialStarted := make(chan struct{}, 1)
	releaseCredential := make(chan struct{})
	blockUnexpectedStoreRead := make(chan struct{})
	var releaseCredentialOnce sync.Once
	var releaseStoreOnce sync.Once
	adapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-INITIAL")}}}}
	factory := &fakeFactory{adapters: []tracker.Adapter{adapter}}
	store := &fakeWorkflowStore{
		current:      validQueueSnapshot("github", "", "digest-1"),
		hasCurrent:   true,
		changes:      make(chan workflow.Change, 1),
		currentWaits: []<-chan struct{}{nil, blockUnexpectedStoreRead},
	}
	runtime := newQueueRuntimeWithDependencies(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")},
		Journal: observability.NewJournal(observability.JournalOptions{}),
	}, queueDependencies{
		now: newFakeQueueClock().Now, after: time.After, jitter: func(time.Duration) time.Duration { return 0 },
		beforeCredentialRebuild: func() {
			credentialStarted <- struct{}{}
			<-releaseCredential
		},
	})
	t.Cleanup(func() {
		releaseCredentialOnce.Do(func() { close(releaseCredential) })
		releaseStoreOnce.Do(func() { close(blockUnexpectedStoreRead) })
		_ = runtime.Shutdown(context.Background())
	})
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtime.NotifyCredentialChanged()
	<-credentialStarted
	runtime.handleWorkflowChange(context.Background(), workflow.Change{
		Digest: "invalid-digest",
		Validation: workflow.ValidationResult{Valid: false, FieldErrors: []workflow.FieldError{{
			Field: "tracker.kind", Code: "invalid_tracker_config", Message: "unsafe detail",
		}}},
	})

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	shutdownResult := make(chan error, 1)
	go func() { shutdownResult <- runtime.Shutdown(shutdownCtx) }()
	waitFor(t, "credential shutdown admission", func() bool {
		runtime.mu.Lock()
		defer runtime.mu.Unlock()
		return runtime.closed
	})
	releaseCredentialOnce.Do(func() { close(releaseCredential) })
	if err := <-shutdownResult; err != nil {
		currentCalls, loadCalls, _ := store.accessCounts()
		t.Fatalf("invalidated credential store read blocked shutdown: err=%v current=%d load=%d", err, currentCalls, loadCalls)
	}
	currentCalls, loadCalls, _ := store.accessCounts()
	if currentCalls != 1 || loadCalls != 0 || factory.buildCount() != 1 {
		t.Fatalf("invalidated credential intent touched dependencies: current=%d load=%d builds=%d",
			currentCalls, loadCalls, factory.buildCount())
	}
}

func TestManualFallbackLoadFailureAfterLifecycleCancellationDoesNotPublish(t *testing.T) {
	t.Parallel()
	harness := newFallbackLoadHarness(
		t, nil, trackerErr(tracker.CategoryAuth, false, 0), errors.New("raw fallback load error canary"),
	)
	before := harness.evidence(t)
	type refreshResult struct {
		receipt domain.RefreshReceipt
		err     error
	}
	result := make(chan refreshResult, 1)
	go func() {
		receipt, err := harness.runtime.Refresh(context.Background())
		result <- refreshResult{receipt: receipt, err: err}
	}()
	harness.store.waitForLoad(t)
	harness.cancelOwner()
	harness.store.release()
	got := <-result
	if got.err != context.Canceled || !got.receipt.Queued || got.receipt.Coalesced {
		t.Fatalf("canceled manual fallback result = receipt=%#v err=%v", got.receipt, got.err)
	}
	assertFallbackEvidenceUnchanged(t, before, harness.evidence(t))
}

func TestCredentialFallbackLoadFailureAfterLifecycleCancellationDoesNotPublish(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-INITIAL")}}}}
	harness := newFallbackLoadHarness(t, adapter, nil, errors.New("raw fallback load error canary"))
	before := harness.evidence(t)
	ctx, intent := harness.credentialIntent()
	result := make(chan error, 1)
	go func() { result <- harness.runtime.rebuildCurrentForCredentialIntent(ctx, intent) }()
	harness.store.waitForLoad(t)
	harness.cancelOwner()
	harness.store.release()
	if err := <-result; err != context.Canceled {
		t.Fatalf("canceled credential fallback result = %v", err)
	}
	assertFallbackEvidenceUnchanged(t, before, harness.evidence(t))
}

func TestManualFallbackLoadFailureAfterEpochSupersessionIsNoOp(t *testing.T) {
	t.Parallel()
	harness := newFallbackLoadHarness(
		t, nil, trackerErr(tracker.CategoryAuth, false, 0), errors.New("raw fallback load error canary"),
	)
	before := harness.evidence(t)
	type refreshResult struct {
		receipt domain.RefreshReceipt
		err     error
	}
	result := make(chan refreshResult, 1)
	go func() {
		receipt, err := harness.runtime.Refresh(context.Background())
		result <- refreshResult{receipt: receipt, err: err}
	}()
	harness.store.waitForLoad(t)
	harness.runtime.mu.Lock()
	harness.runtime.rebuildIntentEpoch++
	harness.runtime.mu.Unlock()
	harness.store.release()
	got := <-result
	if got.err != nil || !got.receipt.Queued || got.receipt.Coalesced {
		t.Fatalf("stale manual fallback result = receipt=%#v err=%v", got.receipt, got.err)
	}
	assertFallbackEvidenceUnchanged(t, before, harness.evidence(t))
}

func TestCredentialFallbackLoadFailureAfterGenerationSupersessionIsNoOp(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-INITIAL")}}}}
	harness := newFallbackLoadHarness(t, adapter, nil, errors.New("raw fallback load error canary"))
	ctx, intent := harness.credentialIntent()
	result := make(chan error, 1)
	go func() { result <- harness.runtime.rebuildCurrentForCredentialIntent(ctx, intent) }()
	harness.store.waitForLoad(t)
	harness.runtime.mu.Lock()
	harness.runtime.retireGenerationLocked()
	harness.runtime.mu.Unlock()
	beforeCompletion := harness.evidence(t)
	harness.store.release()
	if err := <-result; err != nil {
		t.Fatalf("stale credential fallback result = %v", err)
	}
	assertFallbackEvidenceUnchanged(t, beforeCompletion, harness.evidence(t))
}

func TestCredentialFallbackLoadFailureAfterEpochSupersessionIsNoOp(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-INITIAL")}}}}
	harness := newFallbackLoadHarness(t, adapter, nil, errors.New("raw fallback load error canary"))
	ctx, intent := harness.credentialIntent()
	result := make(chan error, 1)
	go func() { result <- harness.runtime.rebuildCurrentForCredentialIntent(ctx, intent) }()
	harness.store.waitForLoad(t)
	harness.runtime.mu.Lock()
	harness.runtime.rebuildIntentEpoch++
	harness.runtime.mu.Unlock()
	beforeCompletion := harness.evidence(t)
	harness.store.release()
	if err := <-result; err != nil {
		t.Fatalf("epoch-stale credential fallback result = %v", err)
	}
	assertFallbackEvidenceUnchanged(t, beforeCompletion, harness.evidence(t))
}

func TestBuildFailureClassificationOrdersLifecycleBeforeStalenessAndPublication(t *testing.T) {
	t.Parallel()
	const (
		wantCanceled = "canceled"
		wantNoOp     = "no_op"
		wantJournal  = "journal_error"
	)
	for _, test := range []struct {
		name            string
		nilRuntimeCtx   bool
		closed          bool
		cancel          bool
		staleGeneration bool
		staleEpoch      bool
		closeJournal    bool
		want            string
	}{
		{name: "nil_runtime_context", nilRuntimeCtx: true, want: wantCanceled},
		{name: "closed_runtime", closed: true, want: wantCanceled},
		{name: "canceled_lifecycle", cancel: true, want: wantCanceled},
		{name: "stale_generation", staleGeneration: true, want: wantNoOp},
		{name: "stale_epoch", staleEpoch: true, want: wantNoOp},
		{name: "stale_generation_and_epoch", staleGeneration: true, staleEpoch: true, want: wantNoOp},
		{
			name: "canceled_lifecycle_beats_staleness_and_closed_journal", cancel: true,
			staleGeneration: true, staleEpoch: true, closeJournal: true, want: wantCanceled,
		},
		{name: "canceled_lifecycle_beats_closed_journal", cancel: true, closeJournal: true, want: wantCanceled},
		{name: "closed_journal_for_live_current_intent", closeJournal: true, want: wantJournal},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			adapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-INITIAL")}}}}
			harness := newFallbackLoadHarness(t, adapter, nil, errors.New("unused load failure"))
			harness.runtime.mu.Lock()
			expectedGeneration := harness.runtime.generation
			expectedEpoch := harness.runtime.rebuildIntentEpoch
			originalRuntimeCtx := harness.runtime.runtimeCtx
			harness.runtime.mu.Unlock()

			if test.cancel {
				harness.cancelOwner()
			}
			harness.runtime.mu.Lock()
			if test.nilRuntimeCtx {
				harness.runtime.runtimeCtx = nil
			}
			if test.closed {
				harness.runtime.closed = true
			}
			if test.staleGeneration {
				harness.runtime.retireGenerationLocked()
			}
			if test.staleEpoch {
				harness.runtime.rebuildIntentEpoch++
			}
			harness.runtime.mu.Unlock()
			if test.closeJournal {
				harness.journal.Close()
			}
			before := harness.evidence(t)
			err := harness.runtime.recordBuildFailureForGenerationAndIntent(
				expectedGeneration, expectedEpoch, true,
				&tracker.Error{Category: tracker.CategoryConfig, Message: "Tracker configuration is unavailable."},
			)
			harness.runtime.mu.Lock()
			if test.nilRuntimeCtx {
				harness.runtime.runtimeCtx = originalRuntimeCtx
			}
			if test.closed {
				harness.runtime.closed = false
			}
			harness.runtime.mu.Unlock()

			switch test.want {
			case wantCanceled:
				if err != context.Canceled {
					t.Fatalf("classification error = %v, want context.Canceled", err)
				}
			case wantNoOp:
				if err != nil {
					t.Fatalf("classification error = %v, want nil no-op", err)
				}
			case wantJournal:
				if !errors.Is(err, observability.ErrJournalClosed) {
					t.Fatalf("classification error = %v, want journal closed", err)
				}
			default:
				t.Fatalf("unknown expected classification %q", test.want)
			}
			assertFallbackEvidenceUnchanged(t, before, harness.evidence(t))
		})
	}
}

func TestCurrentManualFallbackLoadFailureReturnsOneSafeFailure(t *testing.T) {
	t.Parallel()
	adapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-INITIAL")}}}}
	harness := newFallbackLoadHarness(t, adapter, nil, errors.New("raw fallback load error canary"))
	harness.runtime.mu.Lock()
	harness.runtime.adapter = nil
	harness.runtime.autoSuppressed = true
	harness.runtime.mu.Unlock()
	before := harness.evidence(t)
	harness.store.release()
	receipt, err := harness.runtime.Refresh(context.Background())
	if !receipt.Queued || receipt.Coalesced {
		t.Fatalf("current manual fallback receipt = %#v", receipt)
	}
	assertTrackerConfigFailure(t, err)
	assertCurrentFallbackFailureEvidence(t, before, harness.evidence(t))
	currentCalls, loadCalls, _ := harness.store.accessCounts()
	if currentCalls != 2 || loadCalls != 1 {
		t.Fatalf("current manual fallback store calls = Current:%d Load:%d", currentCalls, loadCalls)
	}
}

func TestCurrentCredentialNotificationFallbackLoadFailureIsIgnoredAfterSafePublication(t *testing.T) {
	t.Parallel()
	credentialFinished := make(chan struct{}, 1)
	adapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-INITIAL")}}}}
	harness := newFallbackLoadHarnessWithDependencies(
		t, adapter, nil, errors.New("raw fallback load error canary"),
		queueDependencies{afterCredentialRebuild: func() { credentialFinished <- struct{}{} }},
	)
	harness.runtime.NotifyCredentialChanged()
	harness.store.waitForLoad(t)
	beforeFailure := harness.evidence(t)
	harness.store.release()
	select {
	case <-credentialFinished:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ignored credential fallback result")
	}
	assertCurrentFallbackFailureEvidence(t, beforeFailure, harness.evidence(t))
	currentCalls, loadCalls, _ := harness.store.accessCounts()
	if currentCalls != 2 || loadCalls != 1 {
		t.Fatalf("current credential fallback store calls = Current:%d Load:%d", currentCalls, loadCalls)
	}
}

func TestCurrentCredentialFallbackLoadFailuresPublishOneSafeFailure(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		loadErr error
	}{
		{name: "non_context_error", loadErr: errors.New("raw fallback load error canary")},
		{name: "context_canceled_error_with_live_lifecycle", loadErr: context.Canceled},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			adapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-INITIAL")}}}}
			harness := newFallbackLoadHarness(t, adapter, nil, test.loadErr)
			before := harness.evidence(t)
			ctx, intent := harness.credentialIntent()
			harness.store.release()
			err := harness.runtime.rebuildCurrentForCredentialIntent(ctx, intent)
			assertTrackerConfigFailure(t, err)
			after := harness.evidence(t)
			assertCurrentFallbackFailureEvidence(t, before, after)
			currentCalls, loadCalls, _ := harness.store.accessCounts()
			if currentCalls != 2 || loadCalls != 1 {
				t.Fatalf("current fallback store calls = Current:%d Load:%d", currentCalls, loadCalls)
			}
			encoded, marshalErr := json.Marshal(struct {
				Snapshot domain.Snapshot
				Events   domain.EventPage
				Error    string
			}{after.snapshot, after.events, err.Error()})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if strings.Contains(string(encoded), "raw fallback load error canary") {
				t.Fatalf("raw fallback error reached public evidence: %s", encoded)
			}
		})
	}
}

func TestRejectedRebuildEventDiscardsUncommittedAdapterBeforeFlightClears(t *testing.T) {
	t.Parallel()
	fetchStarted := make(chan struct{}, 1)
	releaseFetch := make(chan struct{})
	rebuildCompleted := make(chan struct{}, 4)
	var releaseOnce sync.Once
	initial := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-INITIAL")}}}}
	rejected := &fakeAdapter{fetches: []fakeFetch{{
		issues: []domain.Issue{validIssue("GH-REJECTED")}, wait: releaseFetch, called: fetchStarted,
	}}}
	factory := &fakeFactory{adapters: []tracker.Adapter{initial, rejected}}
	store := &fakeWorkflowStore{
		current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true,
		changes: make(chan workflow.Change, 2),
	}
	journal := observability.NewJournal(observability.JournalOptions{})
	runtime := newQueueRuntimeWithDependencies(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")}, Journal: journal,
	}, queueDependencies{
		now: newFakeQueueClock().Now, after: time.After, jitter: func(time.Duration) time.Duration { return 0 },
		beforeRebuildCompletion: func() { rebuildCompleted <- struct{}{} },
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseFetch) })
		_ = runtime.Shutdown(context.Background())
	})
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-rebuildCompleted
	digest2 := validQueueSnapshot("github", "", "digest-2")
	store.setCurrent(digest2)
	store.changes <- workflow.Change{Snapshot: digest2, Digest: digest2.Digest, Validation: workflow.ValidationResult{Valid: true}}
	<-fetchStarted
	journal.Close()
	releaseOnce.Do(func() { close(releaseFetch) })
	<-rebuildCompleted
	runtime.handleWorkflowChange(context.Background(), workflow.Change{
		Digest: "invalid-digest",
		Validation: workflow.ValidationResult{Valid: false, FieldErrors: []workflow.FieldError{{
			Field: "polling.interval", Code: "invalid_polling_interval", Message: "unsafe detail",
		}}},
	})
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	activeAdapter := runtime.adapter
	flight := runtime.rebuildFlight
	committedGeneration := runtime.generationCommitted
	runtime.mu.Unlock()
	if activeAdapter != nil || flight != nil || committedGeneration || snapshot.Config.ActiveDigest != "" || snapshot.Config.UsingLastGood ||
		len(snapshot.Candidates) != 1 || snapshot.Candidates[0].Issue.Identifier != "GH-INITIAL" || snapshot.EventCursor.Sequence != 2 {
		t.Fatalf("rejected rebuild publication left staged adapter active: snapshot=%#v adapter=%#v flight=%#v committed=%t",
			snapshot, activeAdapter, flight, committedGeneration)
	}
}

func TestWorkflowInvalidChangeRetainsLastGoodAndScopeChangeClearsBeforeNewPoll(t *testing.T) {
	t.Parallel()
	newStarted := make(chan struct{}, 1)
	releaseNew := make(chan struct{})
	oldAdapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-1")}}}}
	newAdapter := &fakeAdapter{kind: "linear", fetches: []fakeFetch{{issues: []domain.Issue{validIssue("LIN-1")}, wait: releaseNew, called: newStarted}}}
	runtime, factory, store, _, journal := newQueueRuntimeForTest(t, oldAdapter, validQueueSnapshot("github", "", "digest-1"))
	factory.adapters = []tracker.Adapter{oldAdapter, newAdapter}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.changes <- workflow.Change{Digest: "bad-digest", Validation: workflow.ValidationResult{
		Valid: false, FieldErrors: []workflow.FieldError{{Code: "invalid_tracker_config", Message: "unsafe provider text"}}, GlobalErrors: []workflow.SafeError{},
	}}
	waitFor(t, "invalid config status", func() bool {
		snapshot, _ := runtime.Snapshot(context.Background())
		return snapshot.Config.State == "invalid"
	})
	invalid, _ := runtime.Snapshot(context.Background())
	if !invalid.Config.UsingLastGood || invalid.Config.ActiveDigest != "digest-1" || len(invalid.Candidates) != 1 || factory.buildCount() != 1 {
		t.Fatalf("invalid reload did not retain last good: %#v builds=%d", invalid, factory.buildCount())
	}
	linear := validQueueSnapshot("linear", "new-project", "digest-2")
	store.setCurrent(linear)
	store.changes <- workflow.Change{Snapshot: linear, Digest: linear.Digest, Validation: workflow.ValidationResult{Valid: true, FieldErrors: []workflow.FieldError{}, GlobalErrors: []workflow.SafeError{}}}
	<-newStarted
	during, _ := runtime.Snapshot(context.Background())
	if len(during.Candidates) != 0 || during.Tracker.Scope != "linear:new-project" {
		t.Fatalf("scope change exposed old candidates during refresh: %#v", during)
	}
	close(releaseNew)
	waitFor(t, "new scope candidate", func() bool {
		snapshot, _ := runtime.Snapshot(context.Background())
		return len(snapshot.Candidates) == 1 && snapshot.Candidates[0].Issue.Identifier == "LIN-1"
	})
	page := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 0})
	configurationEvents := 0
	for _, event := range page.Events {
		if event.Type == "configuration.changed" {
			configurationEvents++
			encoded, _ := json.Marshal(event)
			if strings.Contains(string(encoded), "unsafe provider text") {
				t.Fatalf("configuration event exposed validation text: %s", encoded)
			}
		}
	}
	if configurationEvents != 2 {
		t.Fatalf("configuration event count = %d, events=%#v", configurationEvents, page.Events)
	}
}

func TestInvalidChangeDuringRebuildRequiresExplicitLastGoodRecovery(t *testing.T) {
	t.Parallel()
	buildStarted := make(chan struct{}, 1)
	releaseBuild := make(chan struct{})
	recovered := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-RECOVERED")}}}}
	runtime, factory, store, _, _ := newQueueRuntimeForTest(t, recovered, validQueueSnapshot("github", "", "digest-1"))
	factory.adapters = []tracker.Adapter{nil, recovered}
	factory.errors = []error{trackerErr(tracker.CategoryAuth, false, 0), nil}
	factory.waits = []<-chan struct{}{nil, releaseBuild}
	factory.called = []chan<- struct{}{nil, buildStarted}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	refreshed := make(chan error, 1)
	go func() {
		_, err := runtime.Refresh(context.Background())
		refreshed <- err
	}()
	<-buildStarted
	store.changes <- workflow.Change{
		Digest: "invalid-digest",
		Validation: workflow.ValidationResult{Valid: false, FieldErrors: []workflow.FieldError{{
			Field: "tracker.kind", Code: "invalid_tracker_config", Message: "unsafe detail",
		}}},
	}
	waitFor(t, "invalid configuration", func() bool {
		snapshot, _ := runtime.Snapshot(context.Background())
		return snapshot.Config.State == "invalid"
	})
	close(releaseBuild)
	if err := <-refreshed; !errors.Is(err, context.Canceled) {
		t.Fatalf("superseded rebuild error = %v, want context.Canceled", err)
	}
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Config.State != "invalid" || snapshot.Config.UsingLastGood || snapshot.Config.ActiveDigest != "" || snapshot.Tracker.State == "ready" {
		t.Fatalf("superseded adapter activated under invalid configuration: %#v", snapshot)
	}
	if _, err := runtime.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	recoveredSnapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recoveredSnapshot.Config.State != "invalid" || !recoveredSnapshot.Config.UsingLastGood ||
		recoveredSnapshot.Config.ActiveDigest != "digest-1" || recoveredSnapshot.Tracker.State != "ready" {
		t.Fatalf("explicit recovery did not activate last good under invalid observation: %#v", recoveredSnapshot)
	}
}

func TestManualRecoveryPreservesObservedInvalidConfigurationUntilAValidChange(t *testing.T) {
	t.Parallel()
	recovered := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-RECOVERED")}}}}
	runtime, factory, _, clock, journal := newQueueRuntimeForTest(t, recovered, validQueueSnapshot("github", "", "digest-1"))
	factory.adapters = []tracker.Adapter{nil, recovered}
	factory.errors = []error{trackerErr(tracker.CategoryAuth, false, 0), nil}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	runtime.handleWorkflowChange(context.Background(), workflow.Change{
		Digest: "invalid-digest",
		Validation: workflow.ValidationResult{Valid: false, FieldErrors: []workflow.FieldError{{
			Field: "tracker.kind", Code: "invalid_tracker_config", Message: "unsafe detail",
		}}},
	})
	observed, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if observed.Config.State != "invalid" || observed.Config.Digest != "invalid-digest" || observed.Config.ErrorCode != "invalid_tracker_config" {
		t.Fatalf("invalid disk observation = %#v", observed.Config)
	}

	receipt, err := runtime.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	recoveredSnapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Queued || receipt.Coalesced || recoveredSnapshot.Config.State != observed.Config.State ||
		recoveredSnapshot.Config.Digest != observed.Config.Digest || recoveredSnapshot.Config.ErrorCode != observed.Config.ErrorCode ||
		recoveredSnapshot.Config.Message != observed.Config.Message || !recoveredSnapshot.Config.ChangedAt.Equal(observed.Config.ChangedAt) ||
		!recoveredSnapshot.Config.UsingLastGood || recoveredSnapshot.Config.ActiveDigest != "digest-1" {
		t.Fatalf("manual recovery erased observed invalid status: observed=%#v recovered=%#v receipt=%#v", observed.Config, recoveredSnapshot.Config, receipt)
	}
	if recoveredSnapshot.Tracker.State != "ready" || len(recoveredSnapshot.Candidates) != 1 || recoveredSnapshot.Candidates[0].Issue.Identifier != "GH-RECOVERED" {
		t.Fatalf("last-good adapter did not activate under invalid observation: %#v", recoveredSnapshot)
	}
	page := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 0})
	configurationEvents := 0
	for _, event := range page.Events {
		if event.Type == "configuration.changed" {
			configurationEvents++
		}
	}
	if configurationEvents != 1 || recoveredSnapshot.EventCursor.Sequence != observed.EventCursor.Sequence+1 {
		t.Fatalf("manual recovery synthesized a disk observation: configuration_events=%d observed_cursor=%#v recovered_cursor=%#v events=%#v", configurationEvents, observed.EventCursor, recoveredSnapshot.EventCursor, page.Events)
	}
}

func TestCredentialRecoveryPreservesLatestInvalidObservationUntilValidScopeChange(t *testing.T) {
	t.Parallel()
	recovered := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-LAST-GOOD")}}}}
	linear := &fakeAdapter{kind: "linear", fetches: []fakeFetch{{issues: []domain.Issue{validIssue("LIN-NEW")}}}}
	runtime, factory, store, clock, journal := newQueueRuntimeForTest(t, recovered, validQueueSnapshot("github", "", "digest-1"))
	factory.adapters = []tracker.Adapter{nil, recovered, linear}
	factory.errors = []error{trackerErr(tracker.CategoryAuth, false, 0), nil, nil}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	runtime.handleWorkflowChange(context.Background(), workflow.Change{
		Digest: "invalid-digest-1",
		Validation: workflow.ValidationResult{Valid: false, FieldErrors: []workflow.FieldError{{
			Field: "tracker.kind", Code: "invalid_tracker_config", Message: "unsafe first detail",
		}}},
	})
	clock.Advance(time.Second)
	runtime.handleWorkflowChange(context.Background(), workflow.Change{
		Digest: "invalid-digest-2",
		Validation: workflow.ValidationResult{Valid: false, FieldErrors: []workflow.FieldError{{
			Field: "polling.interval", Code: "invalid_polling_interval", Message: "unsafe latest detail",
		}}},
	})
	latestInvalid, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	runtime.NotifyCredentialChanged()
	waitFor(t, "credential last-good recovery", func() bool {
		snapshot, _ := runtime.Snapshot(context.Background())
		return snapshot.Tracker.State == "ready" && len(snapshot.Candidates) == 1 && snapshot.Candidates[0].Issue.Identifier == "GH-LAST-GOOD"
	})
	afterCredential, _ := runtime.Snapshot(context.Background())
	if afterCredential.Config.State != "invalid" || afterCredential.Config.Digest != latestInvalid.Config.Digest ||
		afterCredential.Config.ErrorCode != latestInvalid.Config.ErrorCode || afterCredential.Config.Message != latestInvalid.Config.Message ||
		!afterCredential.Config.ChangedAt.Equal(latestInvalid.Config.ChangedAt) || !afterCredential.Config.UsingLastGood || afterCredential.Config.ActiveDigest != "digest-1" {
		t.Fatalf("credential recovery erased latest invalid observation: before=%#v after=%#v", latestInvalid.Config, afterCredential.Config)
	}

	validLinear := validQueueSnapshot("linear", "new-project", "digest-2")
	store.setCurrent(validLinear)
	store.changes <- workflow.Change{Snapshot: validLinear, Digest: validLinear.Digest, Validation: workflow.ValidationResult{Valid: true}}
	waitFor(t, "valid scope-changing recovery", func() bool {
		snapshot, _ := runtime.Snapshot(context.Background())
		return snapshot.Config.State == "valid" && snapshot.Config.ActiveDigest == "digest-2" &&
			len(snapshot.Candidates) == 1 && snapshot.Candidates[0].Issue.Identifier == "LIN-NEW"
	})
	valid, _ := runtime.Snapshot(context.Background())
	if valid.Config.Digest != "digest-2" || valid.Config.UsingLastGood || valid.Config.ErrorCode != "" || valid.Tracker.Scope != "linear:new-project" {
		t.Fatalf("actual valid observation did not clear invalid status and change scope: %#v", valid)
	}
	page := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 0})
	configurationEvents := 0
	for _, event := range page.Events {
		if event.Type == "configuration.changed" {
			configurationEvents++
		}
	}
	if configurationEvents != 3 || valid.EventCursor != page.LatestCursor {
		t.Fatalf("configuration/cursor history = events=%d snapshot=%#v page=%#v", configurationEvents, valid.EventCursor, page.LatestCursor)
	}
	encoded, _ := json.Marshal(struct {
		Snapshot domain.Snapshot
		Events   domain.EventPage
	}{valid, page})
	if strings.Contains(string(encoded), "unsafe first detail") || strings.Contains(string(encoded), "unsafe latest detail") {
		t.Fatalf("validation detail leaked through recovery: %s", encoded)
	}
}

func TestFailedCredentialRecoveryClearsActiveLastGoodWithoutErasingInvalidObservation(t *testing.T) {
	t.Parallel()
	initial := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-LAST-GOOD")}}}}
	runtime, factory, _, clock, journal := newQueueRuntimeForTest(t, initial, validQueueSnapshot("github", "", "digest-1"))
	factory.adapters = []tracker.Adapter{initial, nil}
	factory.errors = []error{nil, trackerErr(tracker.CategoryAuth, false, 0)}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	runtime.handleWorkflowChange(context.Background(), workflow.Change{
		Digest: "invalid-digest",
		Validation: workflow.ValidationResult{Valid: false, FieldErrors: []workflow.FieldError{{
			Field: "tracker.kind", Code: "invalid_tracker_config", Message: "unsafe detail",
		}}},
	})
	observed, _ := runtime.Snapshot(context.Background())
	if !observed.Config.UsingLastGood || observed.Config.ActiveDigest != "digest-1" {
		t.Fatalf("invalid observation did not retain the active adapter: %#v", observed.Config)
	}
	runtime.NotifyCredentialChanged()
	waitFor(t, "failed credential rebuild", func() bool {
		snapshot, _ := runtime.Snapshot(context.Background())
		return snapshot.Tracker.State == "failed" && snapshot.Tracker.ErrorCode == "tracker_auth" && factory.buildCount() == 2
	})
	failed, _ := runtime.Snapshot(context.Background())
	if failed.Config.State != observed.Config.State || failed.Config.Digest != observed.Config.Digest ||
		failed.Config.ErrorCode != observed.Config.ErrorCode || failed.Config.Message != observed.Config.Message ||
		!failed.Config.ChangedAt.Equal(observed.Config.ChangedAt) || failed.Config.UsingLastGood || failed.Config.ActiveDigest != "" {
		t.Fatalf("failed credential recovery misreported disk or active state: observed=%#v failed=%#v", observed.Config, failed.Config)
	}
	page := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 0})
	configurationEvents := 0
	for _, event := range page.Events {
		if event.Type == "configuration.changed" {
			configurationEvents++
		}
	}
	if configurationEvents != 1 || failed.EventCursor != page.LatestCursor {
		t.Fatalf("credential failure fabricated config history: configuration_events=%d snapshot=%#v page=%#v", configurationEvents, failed.EventCursor, page.LatestCursor)
	}
}

func TestQueueDisabledModeNeverTouchesProviderStoreOrResolverAndCommandsAreUnavailable(t *testing.T) {
	t.Parallel()
	store := &fakeWorkflowStore{changes: make(chan workflow.Change)}
	factory := &fakeFactory{adapters: []tracker.Adapter{&fakeAdapter{}}}
	resolver := &fakeResolver{value: []byte("must-not-resolve")}
	runtime := NewQueueRuntime(QueueOptions{Enabled: false, Store: store, Factory: factory, Resolver: resolver, Journal: observability.NewJournal(observability.JournalOptions{})})
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil || snapshot.Candidates == nil || snapshot.Running == nil || snapshot.Retrying == nil || snapshot.Requests == nil {
		t.Fatalf("disabled snapshot = %#v, %v", snapshot, err)
	}
	if _, err := runtime.Refresh(context.Background()); !errors.Is(err, ErrUnavailableInPhase) {
		t.Fatalf("disabled Refresh error = %v", err)
	}
	if err := runtime.SetScheduler(context.Background(), true); !errors.Is(err, ErrUnavailableInPhase) {
		t.Fatalf("disabled scheduler start error = %v", err)
	}
	if err := runtime.SetScheduler(context.Background(), false); err != nil {
		t.Fatalf("disabled scheduler stop error = %v", err)
	}
	if err := runtime.Respond(context.Background(), domain.OperatorResponse{ChoiceID: "answer-canary"}); !errors.Is(err, ErrUnavailableInPhase) {
		t.Fatalf("disabled response error = %v", err)
	}
	runtime.NotifyCredentialChanged()
	if current, load, changes := store.accessCounts(); current != 0 || load != 0 || changes != 0 || factory.buildCount() != 0 || len(resolver.refs) != 0 {
		t.Fatalf("disabled runtime touched dependencies: current=%d load=%d changes=%d builds=%d resolves=%d", current, load, changes, factory.buildCount(), len(resolver.refs))
	}
}

func TestRuntimeStartShutdownAreIdempotentAndNoLateCommitOrEventOccurs(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	adapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-LATE")}, wait: release, called: started, ignoreCancellation: true}}}
	runtime, factory, _, _, journal := newQueueRuntimeForTest(t, adapter, validQueueSnapshot("github", "", "digest-1"))
	startResult := make(chan error, 1)
	go func() { startResult <- runtime.Start(context.Background()) }()
	<-started
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := runtime.Shutdown(shutdownCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown of cancellation-ignoring provider = %v", err)
	}
	close(release)
	if err := <-startResult; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("retired start result = %v", err)
	}
	waitFor(t, "late provider completion", func() bool { return adapter.callCount() == 1 })
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); !errors.Is(err, ErrUnavailableInPhase) {
		t.Fatalf("restart after shutdown error = %v", err)
	}
	if factory.buildCount() != 1 || journal.Cursor().Sequence != 0 {
		t.Fatalf("late shutdown committed state/events: builds=%d cursor=%#v", factory.buildCount(), journal.Cursor())
	}
}

func TestConcurrentStartWaitsForTheSingleInitializationResult(t *testing.T) {
	t.Parallel()
	buildStarted := make(chan struct{}, 1)
	releaseBuild := make(chan struct{})
	adapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-1")}}}}
	runtime, factory, _, _, _ := newQueueRuntimeForTest(t, adapter, validQueueSnapshot("github", "", "digest-1"))
	factory.waits = []<-chan struct{}{releaseBuild}
	factory.called = []chan<- struct{}{buildStarted}
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { first <- runtime.Start(context.Background()) }()
	<-buildStarted
	go func() { second <- runtime.Start(context.Background()) }()
	var earlyErr error
	earlyReturned := false
	select {
	case earlyErr = <-second:
		earlyReturned = true
	case <-time.After(10 * time.Millisecond):
	}
	close(releaseBuild)
	if err := <-first; err != nil {
		t.Fatalf("first Start error = %v", err)
	}
	if earlyReturned {
		t.Fatalf("concurrent Start returned before initialization completed: %v", earlyErr)
	}
	if err := <-second; err != nil {
		t.Fatalf("second Start error = %v", err)
	}
	if factory.buildCount() != 1 {
		t.Fatalf("concurrent Start build count = %d", factory.buildCount())
	}
}

func TestRefreshDuringStartupCoalescesWithTheReservedInitialization(t *testing.T) {
	t.Parallel()
	releaseCurrent := make(chan struct{})
	var releaseOnce sync.Once
	currentCalled := make(chan int, 2)
	adapter := &fakeAdapter{fetches: []fakeFetch{
		{issues: []domain.Issue{validIssue("GH-INITIAL")}},
		{issues: []domain.Issue{validIssue("GH-DUPLICATE")}},
	}}
	factory := &fakeFactory{adapters: []tracker.Adapter{adapter, adapter}}
	store := &fakeWorkflowStore{
		current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true, changes: make(chan workflow.Change, 1),
		currentWaits: []<-chan struct{}{releaseCurrent}, currentCalled: currentCalled,
	}
	runtime := newQueueRuntimeWithDependencies(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")},
		Journal: observability.NewJournal(observability.JournalOptions{}),
	}, queueDependencies{now: newFakeQueueClock().Now, after: time.After, jitter: func(time.Duration) time.Duration { return 0 }})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseCurrent) })
		_ = runtime.Shutdown(context.Background())
	})

	startResult := make(chan error, 1)
	go func() { startResult <- runtime.Start(context.Background()) }()
	if call := <-currentCalled; call != 0 {
		t.Fatalf("first Current call index = %d", call)
	}
	type refreshResult struct {
		receipt domain.RefreshReceipt
		err     error
	}
	refreshResultChannel := make(chan refreshResult, 1)
	go func() {
		receipt, err := runtime.Refresh(context.Background())
		refreshResultChannel <- refreshResult{receipt: receipt, err: err}
	}()
	var early *refreshResult
	select {
	case result := <-refreshResultChannel:
		early = &result
	case <-time.After(20 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(releaseCurrent) })
	if err := <-startResult; err != nil {
		t.Fatalf("Start error = %v", err)
	}
	var refreshed refreshResult
	if early != nil {
		refreshed = *early
	} else {
		refreshed = <-refreshResultChannel
	}
	currentCalls, _, _ := store.accessCounts()
	if early != nil || refreshed.err != nil || refreshed.receipt.Queued || !refreshed.receipt.Coalesced || currentCalls != 1 || factory.buildCount() != 1 || adapter.callCount() != 1 {
		t.Fatalf("startup was not atomically reserved: early=%v receipt=%#v err=%v current=%d builds=%d polls=%d", early != nil, refreshed.receipt, refreshed.err, currentCalls, factory.buildCount(), adapter.callCount())
	}
}

func TestCanceledStartupJoinersDoNotCancelTheOwner(t *testing.T) {
	t.Parallel()
	releaseCurrent := make(chan struct{})
	var releaseOnce sync.Once
	currentCalled := make(chan int, 1)
	adapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-INITIAL")}}}}
	factory := &fakeFactory{adapters: []tracker.Adapter{adapter}}
	store := &fakeWorkflowStore{
		current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true, changes: make(chan workflow.Change, 1),
		currentWaits: []<-chan struct{}{releaseCurrent}, currentCalled: currentCalled,
	}
	runtime := NewQueueRuntime(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")},
		Journal: observability.NewJournal(observability.JournalOptions{}),
	})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseCurrent) })
		_ = runtime.Shutdown(context.Background())
	})
	ownerResult := make(chan error, 1)
	go func() { ownerResult <- runtime.Start(context.Background()) }()
	<-currentCalled

	startWaiterCtx, cancelStartWaiter := context.WithCancel(context.Background())
	startWaiterResult := make(chan error, 1)
	go func() { startWaiterResult <- runtime.Start(startWaiterCtx) }()
	refreshWaiterCtx, cancelRefreshWaiter := context.WithCancel(context.Background())
	type refreshResult struct {
		receipt domain.RefreshReceipt
		err     error
	}
	refreshWaiterResult := make(chan refreshResult, 1)
	go func() {
		receipt, err := runtime.Refresh(refreshWaiterCtx)
		refreshWaiterResult <- refreshResult{receipt: receipt, err: err}
	}()
	cancelStartWaiter()
	cancelRefreshWaiter()
	if err := <-startWaiterResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Start joiner error = %v", err)
	}
	refreshJoined := <-refreshWaiterResult
	if !errors.Is(refreshJoined.err, context.Canceled) || refreshJoined.receipt.Queued || !refreshJoined.receipt.Coalesced {
		t.Fatalf("canceled Refresh joiner = receipt=%#v err=%v", refreshJoined.receipt, refreshJoined.err)
	}
	if factory.buildCount() != 0 {
		t.Fatalf("startup joiners started independent work: builds=%d", factory.buildCount())
	}

	releaseOnce.Do(func() { close(releaseCurrent) })
	if err := <-ownerResult; err != nil {
		t.Fatalf("joiner cancellation canceled owner Start: %v", err)
	}
	if factory.buildCount() != 1 || adapter.callCount() != 1 {
		t.Fatalf("owner initialization count: builds=%d polls=%d", factory.buildCount(), adapter.callCount())
	}
}

func TestStartupCoalescedRefreshReceivesSharedProviderFailure(t *testing.T) {
	t.Parallel()
	fetchStarted := make(chan struct{}, 1)
	releaseFetch := make(chan struct{})
	var releaseOnce sync.Once
	adapter := &fakeAdapter{fetches: []fakeFetch{{
		err:  trackerErr(tracker.CategoryTransport, true, 0),
		wait: releaseFetch, called: fetchStarted,
	}}}
	runtime, factory, _, _, _ := newQueueRuntimeForTest(t, adapter, validQueueSnapshot("github", "", "digest-1"))
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseFetch) }) })
	startResult := make(chan error, 1)
	go func() { startResult <- runtime.Start(context.Background()) }()
	<-fetchStarted
	type refreshResult struct {
		receipt domain.RefreshReceipt
		err     error
	}
	refreshResultChannel := make(chan refreshResult, 1)
	go func() {
		receipt, err := runtime.Refresh(context.Background())
		refreshResultChannel <- refreshResult{receipt: receipt, err: err}
	}()
	select {
	case result := <-refreshResultChannel:
		t.Fatalf("startup-coalesced Refresh returned before shared fetch: %#v", result)
	case <-time.After(20 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(releaseFetch) })
	if err := <-startResult; err != nil {
		t.Fatalf("provider failure made read-only Start unavailable: %v", err)
	}
	result := <-refreshResultChannel
	if result.receipt.Queued || !result.receipt.Coalesced || result.err == nil || !strings.Contains(result.err.Error(), "tracker_transport") {
		t.Fatalf("startup-coalesced Refresh dropped shared provider failure: receipt=%#v err=%v", result.receipt, result.err)
	}
	if factory.buildCount() != 1 || adapter.callCount() != 1 {
		t.Fatalf("startup coalescing duplicated provider work: builds=%d polls=%d", factory.buildCount(), adapter.callCount())
	}
}

func TestStartCancellationReturnsPromptlyWhenInitializationIgnoresCancellation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		make func(started chan<- struct{}, release <-chan struct{}) (*QueueRuntime, *fakeFactory, *fakeAdapter, *observability.Journal)
	}{
		{
			name: "build",
			make: func(started chan<- struct{}, release <-chan struct{}) (*QueueRuntime, *fakeFactory, *fakeAdapter, *observability.Journal) {
				adapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-LATE")}}}}
				factory := &fakeFactory{adapters: []tracker.Adapter{adapter}, waits: []<-chan struct{}{release}, called: []chan<- struct{}{started}}
				store := &fakeWorkflowStore{current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true, changes: make(chan workflow.Change, 1)}
				journal := observability.NewJournal(observability.JournalOptions{})
				runtime := NewQueueRuntime(QueueOptions{Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")}, Journal: journal})
				return runtime, factory, adapter, journal
			},
		},
		{
			name: "initial_fetch",
			make: func(started chan<- struct{}, release <-chan struct{}) (*QueueRuntime, *fakeFactory, *fakeAdapter, *observability.Journal) {
				adapter := &fakeAdapter{fetches: []fakeFetch{{issues: []domain.Issue{validIssue("GH-LATE")}, wait: release, called: started, ignoreCancellation: true}}}
				factory := &fakeFactory{adapters: []tracker.Adapter{adapter}}
				store := &fakeWorkflowStore{current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true, changes: make(chan workflow.Change, 1)}
				journal := observability.NewJournal(observability.JournalOptions{})
				runtime := NewQueueRuntime(QueueOptions{Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")}, Journal: journal})
				return runtime, factory, adapter, journal
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			runtime, factory, adapter, journal := test.make(started, release)
			t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
			startCtx, cancelStart := context.WithCancel(context.Background())
			startResult := make(chan error, 1)
			go func() { startResult <- runtime.Start(startCtx) }()
			<-started
			cancelStart()
			var startErr error
			select {
			case startErr = <-startResult:
			case <-time.After(100 * time.Millisecond):
				close(release)
				startErr = <-startResult
				t.Fatalf("Start remained blocked until cancellation-ignoring %s completed; eventual error=%v", test.name, startErr)
			}
			if !errors.Is(startErr, context.Canceled) {
				close(release)
				t.Fatalf("Start cancellation error = %v", startErr)
			}
			close(release)
			if err := runtime.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			snapshot, err := runtime.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if factory.buildCount() != 1 || adapter.callCount() > 1 || len(snapshot.Candidates) != 0 || journal.Cursor().Sequence != 0 {
				t.Fatalf("canceled initialization committed late state: builds=%d polls=%d candidates=%#v cursor=%#v", factory.buildCount(), adapter.callCount(), snapshot.Candidates, journal.Cursor())
			}
		})
	}
}

func TestStartDeadlineDiscardsLateInitializationError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		make func(started chan<- struct{}, release <-chan struct{}) (*QueueRuntime, *fakeFactory, *fakeAdapter, *observability.Journal)
	}{
		{
			name: "build",
			make: func(started chan<- struct{}, release <-chan struct{}) (*QueueRuntime, *fakeFactory, *fakeAdapter, *observability.Journal) {
				adapter := &fakeAdapter{}
				factory := &fakeFactory{
					adapters: []tracker.Adapter{nil}, errors: []error{trackerErr(tracker.CategoryAuth, false, 0)},
					waits: []<-chan struct{}{release}, called: []chan<- struct{}{started},
				}
				store := &fakeWorkflowStore{current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true, changes: make(chan workflow.Change, 1)}
				journal := observability.NewJournal(observability.JournalOptions{})
				return NewQueueRuntime(QueueOptions{Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")}, Journal: journal}), factory, adapter, journal
			},
		},
		{
			name: "initial_fetch",
			make: func(started chan<- struct{}, release <-chan struct{}) (*QueueRuntime, *fakeFactory, *fakeAdapter, *observability.Journal) {
				adapter := &fakeAdapter{fetches: []fakeFetch{{
					err:  &tracker.Error{Category: tracker.Category("raw-late-category-canary"), Message: "raw late message canary"},
					wait: release, called: started, ignoreCancellation: true,
				}}}
				factory := &fakeFactory{adapters: []tracker.Adapter{adapter}}
				store := &fakeWorkflowStore{current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true, changes: make(chan workflow.Change, 1)}
				journal := observability.NewJournal(observability.JournalOptions{})
				return NewQueueRuntime(QueueOptions{Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")}, Journal: journal}), factory, adapter, journal
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			runtime, factory, adapter, journal := test.make(started, release)
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(release) })
				_ = runtime.Shutdown(context.Background())
			})
			startCtx, cancelStart := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancelStart()
			startResult := make(chan error, 1)
			go func() { startResult <- runtime.Start(startCtx) }()
			<-started
			select {
			case err := <-startResult:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("Start deadline error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Start did not return after its deadline")
			}
			releaseOnce.Do(func() { close(release) })
			if err := runtime.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			snapshot, err := runtime.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(snapshot)
			if factory.buildCount() != 1 || adapter.callCount() > 1 || journal.Cursor().Sequence != 0 || len(snapshot.Candidates) != 0 || strings.Contains(string(encoded), "raw-late") {
				t.Fatalf("late initialization error committed after deadline: builds=%d polls=%d cursor=%#v snapshot=%s", factory.buildCount(), adapter.callCount(), journal.Cursor(), encoded)
			}
		})
	}
}

func TestWorkflowLoadStartupFailureCancelsAndJoinsOwnedLifecycle(t *testing.T) {
	t.Parallel()
	store := &fakeWorkflowStore{
		loadErr: errors.New("raw workflow load detail canary"), changes: make(chan workflow.Change, 1),
	}
	journal := observability.NewJournal(observability.JournalOptions{})
	runtime := NewQueueRuntime(QueueOptions{
		Enabled: true, Store: store, Factory: &fakeFactory{}, Resolver: &fakeResolver{value: []byte("test-token")}, Journal: journal,
	})
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })

	err := runtime.Start(context.Background())
	if err == nil || err.Error() != "queue_workflow_unavailable" {
		t.Fatalf("Start error = %v", err)
	}
	runtime.mu.Lock()
	runtimeCtx := runtime.runtimeCtx
	adapterPublished := runtime.adapter != nil
	activeDigest := runtime.config.ActiveDigest
	startErr := runtime.startErr
	runtime.mu.Unlock()
	if runtimeCtx == nil || !errors.Is(runtimeCtx.Err(), context.Canceled) {
		t.Fatalf("failed startup left owned runtime context live: %#v", runtimeCtx)
	}
	if adapterPublished || activeDigest != "" || startErr == nil || startErr.Error() != "queue_workflow_unavailable" {
		t.Fatalf("failed startup internal state = adapter=%t active_digest=%q start_err=%v", adapterPublished, activeDigest, startErr)
	}

	workersDone := make(chan struct{})
	go func() {
		runtime.wg.Wait()
		close(workersDone)
	}()
	select {
	case <-workersDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Start returned before its owned lifecycle workers exited")
	}
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Candidates) != 0 || snapshot.Config.ActiveDigest != "" || snapshot.Tracker.State != "starting" ||
		snapshot.EventCursor.Sequence != 0 || journal.Cursor().Sequence != 0 {
		t.Fatalf("workflow load failure published queue state: %#v", snapshot)
	}
	if err := runtime.Start(context.Background()); err == nil || err.Error() != "queue_workflow_unavailable" {
		t.Fatalf("repeated Start lost original failure: %v", err)
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown error = %v", err)
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown error = %v", err)
	}
}

func TestRuntimeQueriesHonorCancellationAndSchedulerStopIsIdempotent(t *testing.T) {
	t.Parallel()
	runtime := NewQueueRuntime(QueueOptions{Enabled: false, Journal: observability.NewJournal(observability.JournalOptions{})})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runtime.Snapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Snapshot cancellation = %v", err)
	}
	if _, err := runtime.Issue(ctx, "GH-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Issue cancellation = %v", err)
	}
	if _, err := runtime.EventsAfter(ctx, domain.EventCursor{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("EventsAfter cancellation = %v", err)
	}
	if err := runtime.SetScheduler(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetScheduler(context.Background(), false); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshBeforeStartIsUnavailableWithoutTouchingDependencies(t *testing.T) {
	t.Parallel()
	store := &fakeWorkflowStore{current: validQueueSnapshot("github", "", "digest-1"), hasCurrent: true, changes: make(chan workflow.Change, 1)}
	factory := &fakeFactory{adapters: []tracker.Adapter{&fakeAdapter{}}}
	runtime := NewQueueRuntime(QueueOptions{
		Enabled: true, Store: store, Factory: factory, Resolver: &fakeResolver{value: []byte("test-token")},
	})
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	if _, err := runtime.Refresh(context.Background()); !errors.Is(err, ErrUnavailableInPhase) {
		t.Fatalf("pre-start Refresh error = %v", err)
	}
	if current, load, _ := store.accessCounts(); current != 0 || load != 0 || factory.buildCount() != 0 {
		t.Fatalf("pre-start Refresh touched dependencies: current=%d load=%d builds=%d", current, load, factory.buildCount())
	}
}

func pointerTo[T any](value T) *T { return &value }

var _ RuntimeQueries = (*QueueRuntime)(nil)
var _ RuntimeCommands = (*QueueRuntime)(nil)
