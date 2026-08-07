# Symphony Phase 3: Orchestrator and Workspaces Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement Symphony’s single-authority scheduler, deterministic safe workspaces, lifecycle hooks, retries, reconciliation, and accessible runtime controls using deterministic fake workers before the real Codex client is connected.

**Architecture:** A single orchestrator goroutine receives timers, provider results, worker events, configuration changes, and UI commands over typed channels and is the only owner of claims/running/retry state. Workspace and hook operations sit behind narrow interfaces with OS-specific process code; production remains visibly paused until Phase 4 supplies a real worker, while conformance tests drive the complete lifecycle through deterministic fakes.

**Tech Stack:** Go 1.26.5; standard filesystem/process/context primitives; `golang.org/x/sys` 0.47.0 for Windows path/process facilities; Phase 1 workflow/instance/web foundations; Phase 2 tracker/domain/observability runtime.

## Global Constraints

- Match root `SPEC.md` Sections 7–9 and 16–18 exactly for scheduling, workspace, retry, reconciliation, and restart behavior.
- Only the orchestrator goroutine mutates `running`, `claimed`, `retry_attempts`, token totals, and latest rate limits.
- Sort priorities `1..4` first ascending; other/null priorities next; oldest non-null `created_at` first; null timestamps last; identifier lexicographic tie-breaker.
- Clean continuation retry delay is exactly 1 second. Failure attempt `n` is `min(10 seconds * 2^(n-1), max_retry_backoff)` with overflow-safe saturation.
- Workspace keys permit only `[A-Za-z0-9._-]`; changed identifiers gain a lowercase SHA-256 suffix exposing at least 64 bits.
- Every launch/hook/cleanup revalidates canonical containment. Existing non-directories fail and are preserved; ambiguous paths are never removed.
- macOS hooks use `/bin/sh -lc`. Windows hooks use PowerShell script content through standard input, never command-line interpolation.
- `after_create`/`before_run` failure is fatal to that operation; `after_run`/`before_remove` failure is logged and ignored according to the upstream contract.
- Process restart restores no retry timer or live session; fresh tracker reads and startup terminal cleanup recover useful state.

---

### Task 1: Orchestration domain, eligibility, and exact dispatch ordering

**Files:**
- Create: `go/internal/domain/run.go`
- Create: `go/internal/domain/workspace.go`
- Create: `go/internal/domain/agent_event.go`
- Create: `go/internal/orchestrator/run.go`
- Create: `go/internal/orchestrator/eligibility.go`
- Create: `go/internal/orchestrator/eligibility_test.go`
- Create: `go/internal/orchestrator/sort.go`
- Create: `go/internal/orchestrator/sort_test.go`
- Create: `go/internal/orchestrator/state.go`
- Create: `go/internal/orchestrator/state_test.go`
- Create: `go/internal/orchestrator/testkit_test.go`

**Interfaces:**
- Produces: `orchestrator.RunRequest`, plus domain `RunResult`, `AgentEvent`, `Workspace`, `Hook`, `RunStatus`, and `StopReason`.
- Produces: `orchestrator.Eligible(issue domain.Issue, state View, cfg workflow.EffectiveConfig) bool`.
- Produces: `orchestrator.SortForDispatch([]domain.Issue) []domain.Issue` without mutating input.
- Produces: internal `orchestrator.State` containing maps keyed only by opaque issue ID.

- [ ] **Step 1: Write exhaustive sort and eligibility tables**

```go
func TestSortForDispatchExactBuckets(t *testing.T) {
    p1, p4, p9 := 1, 4, 9
    old, newer := mustTime("2026-01-01T00:00:00Z"), mustTime("2026-02-01T00:00:00Z")
    input := []domain.Issue{
        issueWith("Z", &p9, &old), issueWith("N", nil, nil), issueWith("B", &p1, &newer),
        issueWith("A", &p1, &newer), issueWith("C", &p4, &old),
    }
    got := SortForDispatch(input)
    assertIdentifiers(t, got, []string{"A", "B", "C", "Z", "N"})
    assertIdentifiers(t, input, []string{"Z", "N", "B", "A", "C"})
}

func TestEligibleAppliesProviderNeutralRulesOnly(t *testing.T) {
    cfg := testConfig("open", []string{"symphony"}, 2)
    state := View{RunningIDs: set("1"), ClaimedIDs: set("2"), RunningByState: map[string]int{"open": 1}}
    cases := []struct{name string; issue domain.Issue; want bool}{
        {"ready", readyIssue("3", "open", "symphony"), true},
        {"provider rejected", nondispatchableIssue("3", "open"), false},
        {"missing label", readyIssue("3", "open"), false},
        {"running", readyIssue("1", "open", "symphony"), false},
        {"claimed", readyIssue("2", "open", "symphony"), false},
        {"inactive", readyIssue("3", "closed", "symphony"), false},
    }
    for _, tc := range cases { t.Run(tc.name, func(t *testing.T) { if got := Eligible(tc.issue, state, cfg); got != tc.want { t.Fatalf("got %v", got) } }) }
}
```

Add per-state cap fallback, global cap floor at zero, blank configured label matches nothing, case-insensitive state/label matching, terminal-over-active precedence, and no interpretation of `blocked_by`/`native_ref`.

- [ ] **Step 2: Run tests and confirm failure**

```bash
go test ./internal/orchestrator -run 'Test(Sort|Eligible|State)' -v
```

Expected: compilation fails because orchestration types/functions do not exist.

- [ ] **Step 3: Implement immutable sorting and eligibility helpers**

Copy the input slice before `sort.SliceStable`. Priority rank is `(0, value)` only for `1..4` and `(1, 0)` otherwise; created rank uses `(0, timestamp)` for non-null and `(1, zero)` for null. Final comparison is `Identifier` bytewise lexicographic.

```go
type RunRequest struct {
    Issue domain.Issue
    Attempt *int
    Workflow workflow.Snapshot
}

type RunResult struct {
    Reason StopReason
    ErrorCode string
    ErrorMessage string
    EndedAt time.Time
}
```

- [ ] **Step 4: Implement state invariants and tests**

`State.assert()` verifies every running/retry ID is claimed, no ID is both running and retrying, counters are nonnegative, and map entries carry matching issue IDs. Call it after every transition in tests and debug builds.

`testkit_test.go` initially defines `mustTime`, issue constructors, `assertIdentifiers`, label/ID set builders, and test configuration used by this task. Later orchestrator tasks modify this same file to add their clock/tracker/worker harnesses before first use.

- [ ] **Step 5: Run tests and commit**

```bash
gofmt -w internal/domain internal/orchestrator
go test ./internal/orchestrator ./internal/domain -v
git diff --check
git add go/internal/domain go/internal/orchestrator
git commit -m "feat(go): define Symphony scheduling invariants"
```

---

### Task 2: Collision-resistant workspace paths and conservative cleanup

**Files:**
- Create: `go/internal/workspace/key.go`
- Create: `go/internal/workspace/key_test.go`
- Create: `go/internal/workspace/path.go`
- Create: `go/internal/workspace/path_test.go`
- Create: `go/internal/workspace/manager.go`
- Create: `go/internal/workspace/manager_test.go`
- Create: `go/internal/workspace/ownership.go`
- Create: `go/internal/workspace/ownership_test.go`
- Create: `go/internal/workspace/path_windows_test.go`
- Create: `go/internal/workspace/path_darwin_test.go`
- Create: `go/internal/workspace/testkit_test.go`

**Interfaces:**
- Produces: `workspace.Key(identifier string) (string, error)`.
- Produces: `workspace.New(root string, hooks HookRunner, logger *slog.Logger) (*Manager, error)`.
- Produces: roadmap `workspace.Manager`; `Ensure` in this task runs `after_create` through the injected hook runner.
- Produces: `workspace.ErrOutsideRoot`, `ErrRootIdentity`, `ErrExistingNonDirectory`, `ErrWorkspaceKeyCollision`, and `ErrAmbiguousPath`.

- [ ] **Step 1: Write key collision and containment adversaries**

```go
func TestChangedIdentifiersUseDistinctStableHashSuffixes(t *testing.T) {
    a, err := Key("A/B")
    if err != nil { t.Fatal(err) }
    b, err := Key("A?B")
    if err != nil { t.Fatal(err) }
    if a == b || !regexp.MustCompile(`^A_B-[0-9a-f]{16,}$`).MatchString(a) { t.Fatalf("unsafe keys: %q %q", a, b) }
    same, _ := Key("ABC-123")
    if same != "ABC-123" { t.Fatalf("unchanged identifier changed: %q", same) }
}

func TestEnsureRejectsSymlinkEscapeAndPreservesTarget(t *testing.T) {
    root, outside := t.TempDir(), t.TempDir()
    mustSymlinkOrSkip(t, outside, filepath.Join(root, "GH-42"))
    manager := newManager(t, root)
    _, err := manager.Ensure(context.Background(), issue("GH-42"), config(root))
    if !errors.Is(err, ErrOutsideRoot) { t.Fatalf("got %v", err) }
    mustStillExist(t, outside)
}
```

Test `..`, absolute identifiers, separator variants, prefix confusion (`root` vs `root-other`), symlinked root canonicalization, reparse/junction escape on Windows, case-insensitive key collision with a different ownership marker, existing file preserved, marked existing directory reused, unmarked existing directory reused as unowned, new directory `created_now=true`, root itself never removed, unowned workspace never removed, missing cleanup idempotence, and a path swapped to symlink immediately before remove.

- [ ] **Step 2: Run focused tests and confirm failure**

```bash
go test ./internal/workspace -run 'Test(Key|Ensure|Remove|Path)' -v
```

Expected: compilation fails because the workspace package does not exist.

- [ ] **Step 3: Implement workspace key and canonical containment**

Keep allowed bytes; replace each other Unicode rune with one underscore; if any replacement occurred append `-` plus the first 16 lowercase hex characters of SHA-256 over the original UTF-8 identifier. Reject blank, `.` and `..` keys.

Canonicalize the root's deepest existing parent, create the root with `0700`, evaluate symlinks/reparse points, then compute candidate via `filepath.Join`. Use `filepath.Rel` and reject `..`, absolute rel paths, or rel components that escape. Re-evaluate root and candidate immediately after `Mkdir` and before hook/launch/remove.

- [ ] **Step 4: Implement conservative manager semantics**

An existing non-directory returns `ErrExistingNonDirectory` without replacing it. On creation, atomically write `.symphony-workspace.json` containing format version, opaque issue ID, original identifier, workspace key, and canonical root identity. A matching marker permits restart reuse/cleanup; a different issue/identifier at the same filesystem key returns `ErrWorkspaceKeyCollision`; an unmarked pre-existing directory may be reused as `Owned=false` but is never recursively removed. A newly created directory is removed only when `after_create` fails and a final marker/identity/containment check proves it is still the same empty/new workspace; otherwise preserve and warn. Successful workspaces persist. `Remove` calls `before_remove`, logs its failure, revalidates marker and filesystem identity, then removes only a validated owned child.

`workspace/testkit_test.go` defines symlink-or-skip, manager construction, path identity, and preservation assertions used by both native contract files.

- [ ] **Step 5: Run platform tests and commit**

```bash
gofmt -w internal/workspace
go test ./internal/workspace -v
go test -race ./internal/workspace
git diff --check
git add go/internal/workspace
git commit -m "feat(go): enforce safe issue workspaces"
```

---

### Task 3: Cross-platform hook runner and bounded child output

**Files:**
- Create: `go/internal/workspace/hook.go`
- Create: `go/internal/workspace/hook_test.go`
- Create: `go/internal/workspace/process.go`
- Create: `go/internal/workspace/process_darwin.go`
- Create: `go/internal/workspace/process_windows.go`
- Create: `go/internal/workspace/process_darwin_test.go`
- Create: `go/internal/workspace/process_windows_test.go`
- Modify: `go/internal/workspace/manager.go`
- Modify: `go/internal/workspace/testkit_test.go`
- Modify: `go/go.mod`
- Modify: `go/go.sum`

**Interfaces:**
- Produces: `workspace.HookRunner.Run(context.Context, domain.Hook, domain.Workspace, time.Duration) workspace.HookResult`.
- Produces: `HookResult{ExitCode int, TimedOut bool, Output string, Truncated bool, Err error}`.
- Consumes: validated `domain.Workspace.Path`; refuses root/out-of-root paths passed by callers.

- [ ] **Step 1: Write lifecycle severity, timeout, and quoting tests**

```go
func TestBeforeRunFailureIsFatalAndAfterRunFailureIsWarning(t *testing.T) {
    runner := fakeHookRunner{results: map[domain.Hook]HookResult{
        domain.HookBeforeRun: {ExitCode: 2, Err: errors.New("exit 2")},
        domain.HookAfterRun: {ExitCode: 3, Err: errors.New("exit 3")},
    }}
    if err := enforceHookResult(domain.HookBeforeRun, runner.results[domain.HookBeforeRun]); err == nil { t.Fatal("before_run was not fatal") }
    if err := enforceHookResult(domain.HookAfterRun, runner.results[domain.HookAfterRun]); err != nil { t.Fatalf("after_run became fatal: %v", err) }
}
```

Platform tests pass a script containing quotes, dollar signs, Unicode, and newlines; assert exact output, workspace cwd, timeout kills descendants, environment inherits non-secret values, output caps at 1 MiB, and redactor removes canaries.

- [ ] **Step 2: Run focused tests and confirm failure**

```bash
go test ./internal/workspace -run 'Test(BeforeRun|Hook|Process)' -v
```

Expected: compilation fails because hook/process types do not exist.

- [ ] **Step 3: Implement host shell launch**

macOS uses `exec.CommandContext(ctx, "/bin/sh", "-lc", script)` with validated cwd. Windows uses `exec.CommandContext(ctx, powershellPath, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "-")` and writes the script bytes to stdin, with validated cwd. Locate `pwsh.exe` first and `powershell.exe` second; absence is a typed hook-shell error.

Use process groups on macOS and a Job Object with kill-on-close on Windows. The Windows launcher uses `CreateProcess` suspended with explicit inheritable stdin/stdout/stderr handles, assigns the process to the job, then resumes the primary thread so a script cannot spawn an uncontained descendant first. On timeout/cancel, terminate the entire tree, wait, and return `TimedOut=true`. Capture combined stdout/stderr through a concurrency-safe writer capped at 1 MiB and mark truncation.

- [ ] **Step 4: Wire all lifecycle hooks**

`Ensure` runs `after_create` only when it created the directory. The Phase 3 lifecycle worker runs `before_run` before agent start and registers best-effort `after_run` immediately after `Ensure` succeeds, so it also runs when prompt construction, `before_run`, launch, timeout, cancellation, or the agent fails. `Remove` runs `before_remove` before deletion. Every start/failure/timeout log includes hook name, issue ID/identifier when known, and no script body. Extend `workspace/testkit_test.go` with fake hook results, captured invocations, and descendant-process helpers used here.

- [ ] **Step 5: Run tests and commit**

```bash
gofmt -w internal/workspace
go test ./internal/workspace -v
go test -race ./internal/workspace
git diff --check
git add go/go.mod go/go.sum go/internal/workspace
git commit -m "feat(go): run bounded cross-platform workspace hooks"
```

---

### Task 4: Single-owner orchestrator, immediate poll, and dispatch claims

**Files:**
- Create: `go/internal/orchestrator/interfaces.go`
- Create: `go/internal/orchestrator/clock.go`
- Create: `go/internal/orchestrator/fake_clock_test.go`
- Create: `go/internal/orchestrator/orchestrator.go`
- Create: `go/internal/orchestrator/orchestrator_test.go`
- Create: `go/internal/orchestrator/messages.go`
- Create: `go/internal/orchestrator/transitions.go`
- Create: `go/internal/orchestrator/transitions_test.go`
- Modify: `go/internal/orchestrator/testkit_test.go`

**Interfaces:**
- Consumes: tracker adapter, workflow store, workspace manager, `orchestrator.Worker`, event publisher, logger, and roadmap `Clock`.
- Produces: `orchestrator.Start(ctx context.Context, Options) (*Orchestrator, error)`.
- Produces: `Snapshot`, `Issue`, `EventsAfter`, `Refresh`, `SetScheduler`, and `Respond` methods satisfying application interfaces; `Respond` remains unavailable until Phase 4.

- [ ] **Step 1: Write immediate tick, claim-before-start, and single-owner tests**

```go
func TestStartPerformsImmediatePollAndClaimsBeforeWorkerStart(t *testing.T) {
    tracker := &fakeTracker{byStates: []domain.Issue{readyIssue("1")}}
    worker := newBlockingWorker()
    orch := startTestOrchestrator(t, tracker, worker)
    worker.WaitStarted(t)
    snap := mustSnapshot(t, orch)
    if len(snap.Running) != 1 || snap.Running[0].IssueID != "1" { t.Fatalf("unexpected snapshot: %#v", snap) }
    if worker.ClaimObserved("1") != true { t.Fatal("worker started before claim was visible") }
}

func TestConcurrentRefreshesCoalesceToOnePoll(t *testing.T) {
    tracker := newBlockingTracker()
    orch := startTestOrchestrator(t, tracker, newBlockingWorker())
    waitForFirstPoll(t, tracker)
    receipts := callRefreshConcurrently(t, orch, 20)
    tracker.Release()
    if tracker.MaxConcurrentCalls() != 1 || countNotCoalesced(receipts) != 1 { t.Fatalf("bad coalescing") }
}
```

Run tests under `-race`; fake dependencies panic if called concurrently outside their contract. Add no-slot, per-state slot, pre-dispatch ID refresh, provider-routing change, workflow validation failure, and adapter failure rows.

- [ ] **Step 2: Run focused tests and confirm failure**

```bash
go test -race ./internal/orchestrator -run 'Test(Start|ConcurrentRefresh|Dispatch|Claim)' -v
```

Expected: compilation fails because the orchestrator actor does not exist.

- [ ] **Step 3: Implement typed actor messages and lifecycle**

```go
type Orchestrator struct {
    commands chan any
    done chan struct{}
}

type snapshotRequest struct { ctx context.Context; reply chan snapshotResult }
type refreshRequest struct { requestedAt time.Time; reply chan refreshResult }
type workerUpdate struct { issueID string; event domain.AgentEvent }
type workerExit struct { issueID string; result domain.RunResult }
type configChanged struct { change workflow.Change }
```

`Start` validates dependencies/config, performs startup cleanup through a message-owned startup sequence, schedules poll delay zero, then returns only after the loop is ready. Public methods send one request and select on reply/context/done; timeout/unavailable map to typed status errors.

Before compiling this task, extend `testkit_test.go` with mutex-protected fake tracker/worker types, blocking gates, `startTestOrchestrator`, and snapshot helpers used by the actor tests.

- [ ] **Step 4: Implement poll/revalidate/claim/dispatch**

Each tick reconciles first, validates current config/adapter, fetches active candidates, sorts, checks slots/eligibility, refreshes that ID immediately before dispatch, rechecks eligibility, creates the claim/running entry, then starts one worker goroutine. A failed goroutine start converts the running entry to a retry. Worker goroutines send events/results only; they never edit state.

- [ ] **Step 5: Run actor tests and commit**

```bash
gofmt -w internal/orchestrator
go test ./internal/orchestrator -v
go test -race ./internal/orchestrator
git diff --check
git add go/internal/orchestrator
git commit -m "feat(go): add the single-owner poll and dispatch loop"
```

---

### Task 5: Overflow-safe retries and continuation scheduling

**Files:**
- Create: `go/internal/orchestrator/backoff.go`
- Create: `go/internal/orchestrator/backoff_test.go`
- Create: `go/internal/orchestrator/retry.go`
- Create: `go/internal/orchestrator/retry_test.go`
- Modify: `go/internal/orchestrator/messages.go`
- Modify: `go/internal/orchestrator/transitions.go`
- Modify: `go/internal/domain/snapshot.go`
- Modify: `go/internal/orchestrator/testkit_test.go`

**Interfaces:**
- Produces: `orchestrator.FailureDelay(attempt int, cap time.Duration) time.Duration` and constant `ContinuationDelay = time.Second`.
- Produces: `domain.RetryRow{IssueID, IssueIdentifier, IssueURL string, Attempt int, DueAt time.Time, Error string}`.
- Consumes: generation-tagged `Timer` so stale timer messages cannot consume newer retries.

- [ ] **Step 1: Write formula, cap, overflow, and stale-timer tests**

```go
func TestFailureDelayExactAndSaturating(t *testing.T) {
    cap := 5 * time.Minute
    cases := []struct{attempt int; want time.Duration}{
        {1, 10*time.Second}, {2, 20*time.Second}, {3, 40*time.Second},
        {5, 160*time.Second}, {6, cap}, {63, cap}, {math.MaxInt, cap},
    }
    for _, tc := range cases { if got := FailureDelay(tc.attempt, cap); got != tc.want { t.Fatalf("attempt %d: %s", tc.attempt, got) } }
}

func TestStaleRetryTimerCannotConsumeReplacement(t *testing.T) {
    h := newRetryHarness(t)
    old := h.Schedule("1", 1, errors.New("first"))
    current := h.Schedule("1", 2, errors.New("second"))
    h.Fire(old)
    if got := h.Entry("1"); got.Generation != current.Generation || got.Attempt != 2 { t.Fatalf("stale timer consumed current: %#v", got) }
}
```

Add normal exit attempt 1 at one second, failure attempt increment, fetch failure reschedule, absent issue release, terminal cleanup, inactive/unroutable release, slot exhaustion explicit error/reschedule, and cap reload applies only to future scheduling.

- [ ] **Step 2: Run focused tests and confirm failure**

```bash
go test ./internal/orchestrator -run 'Test(FailureDelay|StaleRetry|NormalExit|Retry)' -v
```

Expected: tests fail because retry helpers/transitions do not exist.

- [ ] **Step 3: Implement saturated arithmetic and generation-tagged timers**

Return cap immediately when attempt is less than 1 or multiplication/shift would meet/exceed cap. Every scheduled retry cancels the prior timer, increments a per-entry generation, stores monotonic due basis plus wall-clock `DueAt`, and captures only `{issueID, generation}` in the timer message.

- [ ] **Step 4: Implement retry refresh decisions**

On fire, discard missing/stale generation. Remove the entry, fetch exactly that issue ID, then: release absent; cleanup/release terminal; release inactive/unroutable; reschedule on fetch failure or no slots; otherwise dispatch with the stored attempt. Publish a state event after every transition.

Extend `testkit_test.go` with the fake-timer retry harness before writing the stale-generation tests.

- [ ] **Step 5: Run tests and commit**

```bash
gofmt -w internal/orchestrator internal/domain/snapshot.go
go test ./internal/orchestrator -v
go test -race ./internal/orchestrator
git diff --check
git add go/internal/orchestrator go/internal/domain/snapshot.go
git commit -m "feat(go): schedule deterministic Symphony retries"
```

---

### Task 6: Reconciliation, stall handling, and startup terminal cleanup

**Files:**
- Create: `go/internal/orchestrator/reconcile.go`
- Create: `go/internal/orchestrator/reconcile_test.go`
- Create: `go/internal/orchestrator/cleanup.go`
- Create: `go/internal/orchestrator/cleanup_test.go`
- Modify: `go/internal/orchestrator/orchestrator.go`
- Modify: `go/internal/orchestrator/transitions.go`
- Modify: `go/internal/orchestrator/testkit_test.go`

**Interfaces:**
- Consumes: running worker cancel functions, last agent event times, tracker ID/state reads, and workspace `Remove`.
- Produces: transition reasons `terminal`, `inactive`, `unroutable`, `missing`, `stalled`, and `operator_stop` in logs/events/snapshots.

- [ ] **Step 1: Write the complete reconciliation matrix**

```go
func TestReconcileTransitions(t *testing.T) {
    cases := []struct{name string; refreshed []domain.Issue; wantStop bool; wantCleanup bool}{
        {"active routable", []domain.Issue{readyIssueInState("1", "open")}, false, false},
        {"terminal", []domain.Issue{readyIssueInState("1", "closed")}, true, true},
        {"inactive", []domain.Issue{readyIssueInState("1", "paused")}, true, false},
        {"unroutable", []domain.Issue{nondispatchableIssue("1", "open")}, true, false},
        {"missing", nil, true, false},
    }
    for _, tc := range cases { t.Run(tc.name, func(t *testing.T) { assertReconcile(t, tc.refreshed, tc.wantStop, tc.wantCleanup) }) }
}
```

Test no-running no provider call; refresh error keeps workers; cancellation finishes before cleanup; a worker that has not exited at the stop-status deadline keeps its claim/workspace and becomes `stopping_failed`; cleanup uses workspace recorded at dispatch even if config root reloads; stall bases time on last event else start; stall disabled at `<=0`; startup terminal fetch error warns and continues; startup cleanup processes every terminal identifier conservatively.

- [ ] **Step 2: Run tests and confirm failure**

```bash
go test ./internal/orchestrator -run 'Test(Reconcile|Stall|StartupCleanup)' -v
```

Expected: tests fail because reconciliation/cleanup transitions do not exist.

- [ ] **Step 3: Implement cancel-then-clean reconciliation**

Fetch full issue snapshots for all running IDs once. On terminal: signal worker cancellation and enter `stopping`; remove the recorded workspace and release only after the matching worker-exit message proves the attempt ended. A 10-second status deadline does not imply process death: it changes the row to `stopping_failed`, preserves claim/workspace, blocks redispatch, and surfaces remediation while continuing to wait for a real exit. On non-active/unroutable/missing: cancel, then release only after confirmed exit, without cleanup. On active/routable: replace the full in-memory issue snapshot. A fetch error changes none of these entries.

- [ ] **Step 4: Implement stall and startup cleanup**

Before tracker reconciliation, compare `clock.Now()` with `LastEventAt` or `StartedAt`; a value strictly greater than `stall_timeout_ms` cancels, then schedules failure retry only after confirmed worker exit. Startup calls `FetchIssuesByStates(terminalStates)`, removes each workspace, logs per-issue cleanup failure, and continues. Provider failure logs one warning and does not fail process startup. Extend `testkit_test.go` with reconciliation outcomes and startup-cleanup harnesses.

- [ ] **Step 5: Run tests and commit**

```bash
gofmt -w internal/orchestrator
go test ./internal/orchestrator -v
go test -race ./internal/orchestrator
git diff --check
git add go/internal/orchestrator
git commit -m "feat(go): reconcile runs and terminal workspaces"
```

---

### Task 7: Worker lifecycle wrapper and deterministic conformance harness

**Files:**
- Create: `go/internal/orchestrator/worker.go`
- Create: `go/internal/orchestrator/worker_test.go`
- Create: `go/internal/orchestrator/conformance_test.go`
- Modify: `go/internal/orchestrator/testkit_test.go`
- Create: `go/testdata/orchestrator/scenarios.json`
- Modify: `go/internal/workspace/manager.go`

**Interfaces:**
- Produces: roadmap `orchestrator.Worker`, `AgentAttempt`, `AgentAttemptRequest`, and `WorkerFunc` adapter.
- Produces: `orchestrator.LifecycleWorker{Workspace workspace.Manager, Agent AgentAttempt}` where `AgentAttempt.Run` is injected; Phase 4 supplies the real agent.
- Guarantees: `before_run` occurs after `Ensure`, `after_run` occurs once after any attempt whose workspace exists, and emitted events carry UTC timestamps.

- [ ] **Step 1: Write workspace/hook/agent ordering tests**

```go
func TestLifecycleWorkerOrdersHooksAndAlwaysRunsAfterRun(t *testing.T) {
    trace := &safeTrace{}
    worker := lifecycleWorkerWithTrace(trace, agentResult(errors.New("agent failed")))
    result := worker.Run(context.Background(), runRequest("GH-1"), func(domain.AgentEvent){})
    if result.ErrorCode != "agent_failed" { t.Fatalf("unexpected result: %#v", result) }
    if diff := trace.Diff([]string{"ensure", "before_run", "agent", "after_run"}); diff != "" { t.Fatal(diff) }
}
```

Test ensure/after_create failure stops before an owned workspace is returned; prompt or `before_run` failure skips the agent but still runs `after_run` once because the attempt owns a workspace; also test cancellation, timestamp injection, panic recovery to typed failure, and emitted event serialization.

- [ ] **Step 2: Run focused tests and confirm failure**

```bash
go test ./internal/orchestrator -run 'TestLifecycleWorker|TestConformance' -v
```

Expected: tests fail because lifecycle worker/harness do not exist.

- [ ] **Step 3: Implement lifecycle worker and panic containment**

Run `Ensure`; immediately install best-effort `after_run`; then build the prompt, run `before_run`, and invoke the agent under the run context. Convert workspace/prompt/hook/agent/panic outcomes to stable redacted `RunResult` codes. A deferred recover logs stack locally after redaction and never crashes the orchestrator process. Extend `testkit_test.go` with the ordered trace and fake `AgentAttempt` used by these tests.

- [ ] **Step 4: Encode Section 17 scheduling scenarios**

`scenarios.json` contains named inputs/expected transitions for exact ordering, dispatchable/labels, normal/failure retry, caps, slot exhaustion, terminal/inactive/missing reconciliation, stalled run, invalid config, tracker errors, empty reads, and restart with no restored timers. The table test executes each with fake clock/tracker/workspace/worker and checks state plus ordered side effects.

- [ ] **Step 5: Run tests and commit**

```bash
gofmt -w internal/orchestrator internal/workspace
go test ./internal/orchestrator ./internal/workspace -v
go test -race ./internal/orchestrator
git diff --check
git add go/internal/orchestrator go/internal/workspace go/testdata/orchestrator
git commit -m "test(go): prove orchestrator lifecycle conformance"
```

---

### Task 8: Accessible scheduler controls, snapshots, shutdown, and multi-instance test

**Files:**
- Create: `go/internal/app/orchestrator_runtime.go`
- Create: `go/internal/app/orchestrator_runtime_test.go`
- Modify: `go/internal/app/runtime.go`
- Modify: `go/internal/web/api.go`
- Modify: `go/internal/web/api_test.go`
- Modify: `go/internal/web/queue_handlers.go`
- Modify: `go/internal/web/viewmodels.go`
- Modify: `go/web/templates/overview.html`
- Modify: `go/web/templates/issue.html`
- Modify: `go/web/static/app.js`
- Modify: `go/tests/accessibility/queue.spec.mjs`
- Create: `go/tests/accessibility/runtime-controls.spec.mjs`
- Modify: `go/internal/cli/run.go`
- Create: `go/internal/cli/multi_instance_test.go`

**Interfaces:**
- Consumes: orchestrator public methods and instance lock.
- Produces: `POST /api/v1/runtime/start`, `POST /api/v1/runtime/stop`, and enriched upstream state/detail snapshots.
- Production behavior in this phase: scheduler starts paused with `Agent runtime will be enabled in Phase 4`; start returns `409 agent_runtime_unavailable`. Tests inject a ready fake and exercise full controls.

- [ ] **Step 1: Write control, focus, and shutdown tests**

```go
func TestStopCancelsWorkersAndReturnsAcceptedState(t *testing.T) {
    runtime, worker := runtimeWithReadyFake(t)
    worker.StartOne(t)
    res := postRuntime(t, runtime, "/api/v1/runtime/stop")
    if res.Code != http.StatusAccepted { t.Fatalf("got %d", res.Code) }
    worker.WaitCanceled(t)
    if snap := mustSnapshot(t, runtime); snap.Scheduler.Running { t.Fatal("scheduler still running") }
}

func TestTwoDifferentWorkflowsRunWhileDuplicateFails(t *testing.T) {
    first := startCLIHarness(t, workflowA)
    second := startCLIHarness(t, workflowB)
    defer first.Stop(); defer second.Stop()
    duplicate := runCLIOnce(t, workflowA)
    if duplicate.ExitCode != 1 || !strings.Contains(duplicate.Stderr, "already_running") { t.Fatalf("unexpected duplicate: %#v", duplicate) }
}
```

Test `202` start/stop, repeated idempotent calls, unavailable worker `409`, snapshot timeout `503`, graceful signal shutdown prevents dispatch then cancels/drains/releases lock, forced deadline, two ephemeral ports, same symlinked workflow duplicate, and no orphan hook process.

- [ ] **Step 2: Run focused tests and confirm failure**

```bash
go test ./internal/app ./internal/web ./internal/cli -run 'Test(Stop|Start|TwoDifferent|Shutdown|SnapshotTimeout)' -v
```

Expected: tests fail because orchestrator runtime/control handlers are not wired.

- [ ] **Step 3: Wire runtime and exact HTTP semantics**

Use the orchestrator as `RuntimeQueries/RuntimeCommands`. Start/stop commands are CSRF/origin protected, return `202` with requested/effective state and correlation ID, and are serialized by the actor. Upstream state includes running/retry rows, token totals/rate limits, scheduler/config status; issue detail includes recorded workspace, attempts, latest events, and bounded logs.

- [ ] **Step 4: Render accessible runtime status and controls**

Overview displays visible `Running`, `Paused`, `Stopping`, or `Unavailable` text plus an icon hidden when decorative. Start/Stop buttons have stable names, disabled reason text, 44 px targets, and a concise polite result. Automatic snapshot changes do not move focus or announce timers. Issue detail presents lifecycle state as a definition list and ordered event list.

- [ ] **Step 5: Add runtime-control browser tests**

Keyboard-start/stop with fake-ready e2e mode, hold focus while a worker event arrives, check retry countdown has an absolute `<time>`, pause live presentation, and run axe on running/retrying/stalled/stopping/unavailable states in Chromium and WebKit.

- [ ] **Step 6: Run Phase 3 gates and commit**

```bash
gofmt -w internal/app internal/web internal/cli
go test ./...
go test -race ./...
go vet ./...
npm ci
npm run html:validate
npm run test:a11y
node scripts/a11y-scan-all.mjs
git diff --check
git add go/internal/app go/internal/web go/internal/cli go/web go/tests/accessibility
git commit -m "feat(go): control and observe the Symphony scheduler"
```

## Phase 3 Acceptance

Run the complete scenario harness and browser states with fake workers. Verify exact state transitions and side-effect order, then start two CLI harness processes for different workflows and reject a duplicate canonical path. On both supported CI hosts, verify workspace/hook path adversaries and shutdown leave no process or lock behind.

Deterministic gate from `go/`:

```bash
go test ./...
go test -race ./... # required on macOS
go vet ./...
npm ci
npm run html:validate
npm run test:a11y
node scripts/a11y-scan-all.mjs
```

Expected: exit 0. The production UI truthfully remains paused until Phase 4 wires the compatible Codex worker.
