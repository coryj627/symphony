package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

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
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "rate-limit timer", func() bool { return clock.pendingTimers() > 0 })
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

func TestInvalidChangeDuringRebuildMarksTheActivatedAdapterAsLastGood(t *testing.T) {
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
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Config.State != "invalid" || !snapshot.Config.UsingLastGood || snapshot.Config.ActiveDigest != "digest-1" || snapshot.Tracker.State != "ready" {
		t.Fatalf("adapter activated under invalid configuration without last-good status: %#v", snapshot)
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
