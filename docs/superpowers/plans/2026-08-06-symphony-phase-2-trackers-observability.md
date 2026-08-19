# Symphony Phase 2: Trackers and Observable Queue Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show an accurate, accessible, live read-only work queue for one configured GitHub repository or Linear project, with provider-normalized issues, structured logs, snapshots, and reconnect-safe events.

**Architecture:** Add provider adapters behind the shared tracker interface, build them from the last-known-good workflow plus host-side credential resolver, and feed a read-only queue runtime. The existing protected server consumes only application query/command interfaces, so the Phase 3 orchestrator can replace the queue runtime without changing route semantics.

**Tech Stack:** Go 1.26.5 standard HTTP/JSON, GraphQL request documents, `log/slog`, server-rendered templates, SSE, Playwright 1.62.1, axe-core Playwright 4.12.1, and the Phase 1 workflow/vault/web foundations.

## Global Constraints

- Implement the adapter contract and Section 17.3 semantics in root `SPEC.md`; provider reads are atomic from the scheduler/UI perspective.
- Only GitHub repository Issues and one Linear project are supported; no Asana, Jira, GitLab, multi-repository, or multi-project adapter is added.
- GitHub pull requests returned by the Issues API are normalized with `dispatchable=false` and never presented as eligible issues.
- Tracker credentials resolve host-side and do not enter browser state, logs, fixtures, query strings, or serialized events.
- State-list reads may omit malformed individual records with a visible redacted warning; ID refresh fails if any requested visible record is malformed.
- Empty state and ID lists return empty without issuing a provider request.
- Every HTML/API/event representation is derived from provider-neutral domain types; core UI code does not inspect `native_ref`.
- All new route states pass Playwright plus axe, keyboard flows, 320 CSS-pixel reflow, 400% zoom, forced colors, reduced motion, and a11y-check-web pre-commit scanning.

---

### Task 1: Provider-neutral issue, tracker, and portable error contracts

**Files:**
- Create: `go/internal/domain/issue.go`
- Create: `go/internal/domain/issue_test.go`
- Create: `go/internal/domain/tool.go`
- Create: `go/internal/tracker/adapter.go`
- Create: `go/internal/tracker/normalize.go`
- Create: `go/internal/tracker/normalize_test.go`
- Create: `go/internal/tracker/http_error.go`
- Create: `go/internal/tracker/http_error_test.go`
- Create: `go/internal/tracker/testkit_test.go`

**Interfaces:**
- Produces: roadmap `domain.Issue`, plus `BlockerRef`, `NormalizeState`, `NormalizeLabels`, `Issue.ValidateRequired() error`, tool request/result types, and `domain.ErrToolUnavailable`.
- Produces: roadmap `tracker.Adapter`/`Factory`, `tracker.Session{Issue domain.Issue, ProviderConfig ProviderConfig}`, and `tracker.Error{Category, Message string, Retryable bool, RetryAfter time.Duration, Status int}`.
- Produces: `tracker.Category` constants `tracker_config`, `tracker_auth`, `tracker_transport`, `tracker_response`, `tracker_payload`, `tracker_pagination`, and `tracker_rate_limited`.

- [ ] **Step 1: Write normalization and required-field tests**

```go
func TestNormalizeLabelsTrimsLowercasesDeduplicatesAndDropsBlank(t *testing.T) {
    got := NormalizeLabels([]string{" Symphony ", "BUG", "bug", " "})
    if !slices.Equal(got, []string{"symphony", "bug"}) { t.Fatalf("got %q", got) }
}

func TestIssueValidateRequiredAllowsFalseDispatchableAndNilMetadata(t *testing.T) {
    issue := domain.Issue{ID: "42", Identifier: "GH-42", Title: "Title", State: "open", Dispatchable: false}
    if err := issue.ValidateRequired(); err != nil { t.Fatal(err) }
    issue.Title = ""
    if err := issue.ValidateRequired(); !errors.Is(err, domain.ErrInvalidIssue) { t.Fatalf("got %v", err) }
}
```

Test normalized state trim/lowercase comparison, deep copied JSON-safe `native_ref`, label immutability, blocker fallback, UTC timestamps, provider-safe identifier requirements, and structured unavailable tool results. Provider wire decoders—not the normalized boolean field—verify that required dispatchability inputs were actually present.

- [ ] **Step 2: Run focused tests and confirm failure**

```bash
go test ./internal/domain ./internal/tracker -run 'Test(Normalize|IssueValidate|TrackerError)' -v
```

Expected: compilation fails because the domain and adapter contracts do not exist.

- [ ] **Step 3: Implement exact domain fields and validation**

```go
type Issue struct {
    ID           string         `json:"id"`
    NativeRef    map[string]any `json:"native_ref,omitempty"`
    Identifier   string         `json:"identifier"`
    Title        string         `json:"title"`
    Description  *string        `json:"description"`
    Priority     *int           `json:"priority"`
    State        string         `json:"state"`
    BranchName   *string        `json:"branch_name"`
    URL          *string        `json:"url"`
    AssigneeID   *string        `json:"assignee_id"`
    Labels       []string       `json:"labels"`
    BlockedBy    []BlockerRef   `json:"blocked_by"`
    Dispatchable bool           `json:"dispatchable"`
    CreatedAt    *time.Time     `json:"created_at"`
    UpdatedAt    *time.Time     `json:"updated_at"`
}
```

Validation rejects blank ID/identifier/title/state and a non-JSON-safe `native_ref`; optional invalid provider values normalize to nil/empty before validation.

- [ ] **Step 4: Implement adapter and portable errors**

```go
type Adapter interface {
    Kind() string
    FetchIssuesByStates(context.Context, []string) ([]domain.Issue, error)
    FetchIssuesByIDs(context.Context, []string) ([]domain.Issue, error)
    AgentTools(Session) []domain.ToolSpec
    ExecuteAgentTool(context.Context, domain.ToolCall, Session) domain.ToolResult
    SecretEnvironmentNames() []string
}

type Error struct {
    Category Category
    Message string
    Retryable bool
    RetryAfter time.Duration
    Status int
}
```

`Error.Error()` excludes response bodies, request headers, and credentials. A separate bounded diagnostic value goes only through the centralized redactor introduced in Task 4.

`domain/tool.go` defines JSON-safe `ToolSpec`, `ToolCall`, and `ToolResult`, plus `ErrToolUnavailable`. `tracker/testkit_test.go` defines provider-neutral adapter contract helpers; the normalization test uses `slices.Equal` rather than an undeclared comparison helper.

- [ ] **Step 5: Run tests and commit**

```bash
gofmt -w internal/domain internal/tracker
go test ./internal/domain ./internal/tracker -v
git diff --check
git add go/internal/domain go/internal/tracker
git commit -m "feat(go): define tracker-neutral issue contracts"
```

---

### Task 2: GitHub repository Issues read adapter

**Files:**
- Create: `go/internal/tracker/github/client.go`
- Create: `go/internal/tracker/github/client_test.go`
- Create: `go/internal/tracker/github/adapter.go`
- Create: `go/internal/tracker/github/adapter_test.go`
- Create: `go/internal/tracker/github/testkit_test.go`
- Create: `go/internal/tracker/github/normalize.go`
- Create: `go/internal/tracker/github/normalize_test.go`
- Create: `go/internal/tracker/github/pagination.go`
- Create: `go/internal/tracker/github/pagination_test.go`
- Create: `go/testdata/github/issues-page-1.json`
- Create: `go/testdata/github/issues-page-2.json`
- Create: `go/testdata/github/issue-42.json`
- Create: `go/testdata/github/rate-limited.json`

**Interfaces:**
- Consumes: `tracker.GitHubConfig`, `secrets.Resolver`, `tracker.Adapter`, and `domain.Issue`.
- Produces: `github.New(config tracker.GitHubConfig, token []byte, client *http.Client, logger *slog.Logger) (*github.Adapter, error)`.
- Produces: `github.Adapter.FetchIssuesByStates` and `FetchIssuesByIDs`; tool methods return `domain.ErrToolUnavailable` until Phase 4.

- [ ] **Step 1: Write repository-scope, pagination, and normalization tests**

```go
func TestFetchIssuesByStatesPaginatesAndExcludesPullRequests(t *testing.T) {
    server := githubFixtureServer(t, []fixtureResponse{
        {Path: "/repos/coryj627/symphony/issues", Query: "state=all&per_page=100&page=1", File: "issues-page-1.json", LinkNext: 2},
        {Path: "/repos/coryj627/symphony/issues", Query: "state=all&per_page=100&page=2", File: "issues-page-2.json"},
    })
    adapter := newGitHubAdapter(t, server.URL)
    got, err := adapter.FetchIssuesByStates(context.Background(), []string{"open"})
    if err != nil { t.Fatal(err) }
    assertIdentifiers(t, got, []string{"GH-42"})
}
```

Add tests for exact owner/repository path escaping, no call on empty states, case-insensitive state filtering, optional assignee mismatch, required-field omission with warning, label normalization, nullable timestamps/body, no token in recorded request diagnostics, 429 `Retry-After`, malformed Link header, and a redirect to another host rejected.

- [ ] **Step 2: Run GitHub tests and confirm failure**

```bash
go test ./internal/tracker/github -v
```

Expected: compilation fails because the GitHub package does not exist.

- [ ] **Step 3: Implement bounded authenticated HTTP and pagination**

Construct paths from separately escaped owner/repository segments, set `Accept: application/vnd.github+json`, `X-GitHub-Api-Version: 2022-11-28`, and `Authorization: Bearer <token>` only in memory. Use an `http.Client` with 30-second timeout and `CheckRedirect` that permits only the configured API origin. Parse RFC 5988 `Link` values, cap pages at 100, reject repeated URLs, and return no partial list after any page failure.

- [ ] **Step 4: Implement normalization and ID refresh atomicity**

Map issue number string to `ID`, `GH-<number>` to `Identifier`, preserve owner/repository/database ID/number in `NativeRef`, and expose provider URL. Exclude every pull-request record from state-list and ID-refresh results; an assignee mismatch sets `Dispatchable=false`. State-list reads log and omit a malformed record; ID refresh fetches each unique positive numeric ID in stable input order, omits 404, but fails the whole call for a malformed visible response or other status. `testkit_test.go` owns the HTTP fixture server, adapter constructor, and identifier assertion used in the snippets.

- [ ] **Step 5: Run tests and commit**

```bash
gofmt -w internal/tracker/github
go test ./internal/tracker/github -v
go test ./internal/tracker -v
git diff --check
git add go/internal/tracker/github go/testdata/github
git commit -m "feat(go): read scoped GitHub repository issues"
```

---

### Task 3: Linear project read adapter and blocker routing

**Files:**
- Create: `go/internal/tracker/linear/client.go`
- Create: `go/internal/tracker/linear/client_test.go`
- Create: `go/internal/tracker/linear/adapter.go`
- Create: `go/internal/tracker/linear/adapter_test.go`
- Create: `go/internal/tracker/linear/testkit_test.go`
- Create: `go/internal/tracker/linear/queries.go`
- Create: `go/internal/tracker/linear/normalize.go`
- Create: `go/internal/tracker/linear/normalize_test.go`
- Create: `go/internal/tracker/linear/pagination.go`
- Create: `go/internal/tracker/linear/pagination_test.go`
- Create: `go/testdata/linear/candidates-page-1.json`
- Create: `go/testdata/linear/candidates-page-2.json`
- Create: `go/testdata/linear/id-refresh.json`
- Create: `go/testdata/linear/graphql-errors.json`

**Interfaces:**
- Consumes: `tracker.LinearConfig`, `secrets.Resolver`, `tracker.Adapter`, and `domain.Issue`.
- Produces: `linear.New(config tracker.LinearConfig, token []byte, client *http.Client, logger *slog.Logger) (*linear.Adapter, error)`.
- Produces: full normalized Linear issues with non-secret `native_ref`; tool methods remain unavailable until Phase 4.

- [ ] **Step 1: Write project scope, blockers, and paging tests**

```go
func TestTodoWithNonterminalBlockerIsNotDispatchable(t *testing.T) {
    raw := fixtureIssue("LIN-12", "Todo", []fixtureBlocker{{ID: "b1", Identifier: "LIN-2", State: "In Progress"}})
    got, err := normalizeIssue(raw, terminalSet("Done", "Closed"))
    if err != nil { t.Fatal(err) }
    if got.Dispatchable { t.Fatal("blocked Todo issue was dispatchable") }
    if got.BlockedBy[0].Identifier != "LIN-2" { t.Fatalf("lost blocker: %#v", got.BlockedBy) }
}

func TestFetchByStatesFollowsCursorAndKeepsProjectScope(t *testing.T) {
    server := linearFixtureServer(t, "candidates-page-1.json", "candidates-page-2.json")
    got, err := newLinearAdapter(t, server.URL).FetchIssuesByStates(context.Background(), []string{"Todo", "In Progress"})
    if err != nil { t.Fatal(err) }
    assertIdentifiers(t, got, []string{"LIN-12", "LIN-13"})
    server.AssertEveryVariable(t, "projectSlug", "symphony")
}
```

Test empty inputs, 50-item pages/batches, missing cursor, duplicate ID, GraphQL errors on HTTP 200, non-200 response, invalid timestamp/priority fallback, labels, branch/URL/assignee/native refs, malformed state-list omission, malformed ID-refresh failure, and token redaction.

- [ ] **Step 2: Run Linear tests and confirm failure**

```bash
go test ./internal/tracker/linear -v
```

Expected: compilation fails because the Linear package does not exist.

- [ ] **Step 3: Implement fixed GraphQL documents and paging**

Define named `SymphonyIssuesByStates` and `SymphonyIssuesByIDs` documents as constants. Send JSON `{query, variables}` to the configured HTTPS endpoint with `Authorization: <token>`, cap response bodies at 4 MiB, use pages/batches of 50, require `endCursor` when `hasNextPage=true`, reject repeated cursors, and cap 100 pages. Top-level `errors` returns `tracker_payload` and a bounded redacted diagnostic; no partial data is returned.

- [ ] **Step 4: Implement normalization and provider routing**

Preserve Linear issue ID as `ID` and identifier as `Identifier`; `NativeRef` contains issue ID, project ID/slug, team ID, and identifier only. Normalize inverse `blocks` relations to `BlockedBy`. A `Todo` issue is dispatchable only when every known blocker state is terminal; other in-scope active issues are dispatchable. Never infer blockers from description text.

`testkit_test.go` defines raw fixture constructors, the GraphQL fixture server, adapter construction, variable assertions, and identifier assertions used by these tests.

- [ ] **Step 5: Run tests and commit**

```bash
gofmt -w internal/tracker/linear
go test ./internal/tracker/linear -v
go test ./internal/tracker/... -v
git diff --check
git add go/internal/tracker/linear go/testdata/linear
git commit -m "feat(go): read scoped Linear project issues"
```

---

### Task 4: Redacted structured logging and bounded JSONL files

**Files:**
- Create: `go/internal/observability/redactor.go`
- Create: `go/internal/observability/redactor_test.go`
- Create: `go/internal/observability/handler.go`
- Create: `go/internal/observability/handler_test.go`
- Create: `go/internal/observability/rotating_writer.go`
- Create: `go/internal/observability/rotating_writer_test.go`
- Create: `go/internal/observability/log_store.go`
- Create: `go/internal/observability/log_store_test.go`

**Interfaces:**
- Consumes: data directory from `instance.Info`, adapter-declared secret environment names, and secret canary values during tests.
- Produces: `observability.NewLogger(Options) (*slog.Logger, *LogStore, error)`.
- Produces: `LogStore.Query(context.Context, LogQuery) (LogPage, error)` for the Logs page.
- Produces: `Redactor.RegisterSecret([]byte)` and recursive `Redactor.Value(any) any`.

- [ ] **Step 1: Write canary, control-byte, and rotation tests**

```go
func TestLoggerRedactsSecretsInMessagesAttributesErrorsAndURLs(t *testing.T) {
    sink := &bytes.Buffer{}
    logger, redactor := testLogger(sink)
    canary := "test-only-canary"
    redactor.RegisterSecret([]byte(canary))
    logger.Error("failed "+canary, "authorization", "Bearer "+canary, "url", "http://127.0.0.1/?access_token="+canary)
    if strings.Contains(sink.String(), canary) { t.Fatalf("secret leaked: %s", sink.String()) }
    for _, want := range []string{"[REDACTED]", "failed", "authorization"} {
        if !strings.Contains(sink.String(), want) { t.Fatalf("missing %q", want) }
    }
}
```

Test nested maps/slices, error chains, cookie headers, ANSI/control characters, 64 KiB field truncation, sink failure fallback, rotation at exactly 10 MiB, retention of five archives, and newest-first bounded queries.

- [ ] **Step 2: Run focused tests and confirm failure**

```bash
go test ./internal/observability -run 'Test(Logger|Redact|Rotat|LogStore)' -v
```

Expected: compilation fails because the observability package does not exist.

- [ ] **Step 3: Implement one mandatory redaction path**

Wrap a JSON `slog.Handler`; sanitize message and every attribute recursively before forwarding. Redact registered exact values, `Authorization`, `Cookie`, `Set-Cookie`, `access_token` query values, and declared secret environment names. Remove ANSI/control bytes except tab/newline in bounded log text. Provider clients log through this logger only.

- [ ] **Step 4: Implement rotation and safe degradation**

Write `DataDir/logs/symphony.jsonl`, rotate before a write would exceed 10 MiB, keep `.1` through `.5`, and serialize rotation/writes under one mutex. If the file sink fails, mark `LogStore.Degraded()`, send one rate-limited redacted warning to stderr, and continue returning records retained in the 2,000-entry in-memory ring.

- [ ] **Step 5: Run tests and commit**

```bash
gofmt -w internal/observability
go test ./internal/observability -v
go test -race ./internal/observability
git diff --check
git add go/internal/observability
git commit -m "feat(go): add redacted structured observability"
```

---

### Task 5: Event journal, snapshots, and read-only queue runtime

**Files:**
- Create: `go/internal/domain/snapshot.go`
- Create: `go/internal/domain/event.go`
- Create: `go/internal/domain/operator_request.go`
- Create: `go/internal/domain/operator_request_test.go`
- Create: `go/internal/observability/journal.go`
- Create: `go/internal/observability/journal_test.go`
- Create: `go/internal/app/runtime.go`
- Create: `go/internal/app/queue_runtime.go`
- Create: `go/internal/app/queue_runtime_test.go`
- Create: `go/internal/app/testkit_test.go`
- Modify: `go/internal/app/config_service.go`
- Modify: `go/internal/cli/run.go`

**Interfaces:**
- Produces: roadmap `domain.Snapshot`/`Event`, plus `EventCursor`, `IssueDetail`, `EventPage`, `RefreshReceipt`, `SchedulerStatus`, `ConfigStatus`, `RunningRow`, `RetryRow`, display-safe `OperatorRequest`, and `OperatorResponse`.
- Produces: roadmap `app.RuntimeQueries` and `RuntimeCommands`.
- Produces: `app.NewQueueRuntime(QueueOptions) *QueueRuntime`; defines `app.ErrUnavailableInPhase`; `Respond` and scheduler start return that error while `Refresh` performs a read-only provider refresh.

- [ ] **Step 1: Write journal replay/reset and queue atomicity tests**

```go
func TestJournalReturnsResetWhenSequenceFellOutOfWindow(t *testing.T) {
    journal := NewJournal(JournalOptions{MaxEvents: 2, MaxBytes: 1024})
    journal.Publish(event("one")); journal.Publish(event("two")); journal.Publish(event("three"))
    page := journal.After(domain.EventCursor{Epoch: journal.Epoch(), Sequence: 0})
    if !page.Reset || page.LatestCursor.Epoch != journal.Epoch() || page.LatestCursor.Sequence != 3 { t.Fatalf("unexpected page: %#v", page) }
}

func TestQueueRefreshDoesNotPublishPartialProviderResults(t *testing.T) {
    adapter := &fakeAdapter{statesResult: []domain.Issue{validIssue("GH-1")}, statesErr: trackerErr("page 2 failed")}
    runtime := newQueueRuntime(t, adapter)
    _, err := runtime.Refresh(context.Background())
    if err == nil { t.Fatal("expected refresh error") }
    snap, _ := runtime.Snapshot(context.Background())
    if len(snap.Running) != 0 || runtime.issueCount() != 0 { t.Fatalf("partial state leaked: %#v", snap) }
}
```

Test monotonic sequence, previous-process ID reset, 4,096-event/8 MiB limits, idempotent refresh coalescing, active-state fetch, stable issue lookup, adapter rebuild on valid workflow change, last-good adapter retained on invalid reload, rate-limit status, and safe auth errors.

- [ ] **Step 2: Run focused tests and confirm failure**

```bash
go test ./internal/observability ./internal/app -run 'Test(Journal|Queue|Refresh)' -v
```

Expected: compilation fails because journal and queue runtime do not exist.

- [ ] **Step 3: Implement bounded events and immutable snapshots**

Assign each process a random non-secret epoch and each published event the next sequence. Deep-copy event maps and snapshot slices on write/read. `After(domain.EventCursor)` returns events only when both epoch and sequence are available; a missing/mismatched epoch or an evicted sequence returns `Reset=true` plus the latest full cursor without fabricating history.

- [ ] **Step 4: Implement the read-only poller**

Build exactly one adapter from the current workflow and vault resolver. Poll configured active states immediately then at `polling.interval_ms`; coalesce manual refresh with an in-flight poll. Keep the last complete issue map and latest provider status. Publish `queue.refreshed`, `queue.failed`, and `configuration.changed` events. Configuration mode keeps a disabled runtime and never polls.

`app/testkit_test.go` defines the mutex-protected fake adapter, valid issue, tracker error, and runtime constructors used by these tests; every fake deep-copies recorded inputs and returned outputs for the race suite.

Define the Phase 4-ready operator boundary now so `domain.Snapshot.Requests` and `RuntimeCommands.Respond` compile: request identity/session/issue/kind/title/summary, opened/warning/deadline times, extension counts, choices/questions, and a response carrying request/session identity, one choice, and answer lists. Phase 2 always returns an empty request slice and `ErrUnavailableInPhase`; tests prove deep-copy behavior and that snapshot/event JSON contains no operator response or answer values.

- [ ] **Step 5: Run race tests and commit**

```bash
gofmt -w internal/domain internal/observability internal/app internal/cli
go test ./internal/observability ./internal/app ./internal/cli -v
go test -race ./internal/app ./internal/observability
git diff --check
git add go/internal/domain go/internal/observability go/internal/app go/internal/cli
git commit -m "feat(go): expose a live read-only work queue"
```

---

### Task 6: Accessible queue, issue, activity, logs, and JSON APIs

**Files:**
- Create: `go/internal/web/api.go`
- Create: `go/internal/web/api_test.go`
- Create: `go/internal/web/queue_handlers.go`
- Create: `go/internal/web/queue_handlers_test.go`
- Create: `go/internal/web/log_handlers.go`
- Create: `go/internal/web/log_handlers_test.go`
- Modify: `go/internal/web/routes.go`
- Modify: `go/internal/web/viewmodels.go`
- Modify: `go/web/templates/overview.html`
- Modify: `go/web/templates/issues.html`
- Modify: `go/web/templates/issue.html`
- Modify: `go/web/templates/activity.html`
- Modify: `go/web/templates/logs.html`
- Modify: `go/web/static/app.css`
- Create: `go/tests/accessibility/queue.spec.mjs`
- Create: `go/tests/accessibility/logs.spec.mjs`

**Interfaces:**
- Consumes: `app.RuntimeQueries`, `app.RuntimeCommands`, and `observability.LogStore`.
- Produces: upstream-compatible `GET /api/v1/state`, `GET /api/v1/{issue_identifier}`, and `POST /api/v1/refresh`.
- Produces: HTML Overview, Issues, Issue detail, Activity, and Logs views without provider-specific branching.

- [ ] **Step 1: Write API contract and escaped-rendering tests**

```go
func TestStateAPIUsesBaselineShapeAndNoStore(t *testing.T) {
    res := authenticatedJSON(t, http.MethodGet, "/api/v1/state", nil)
    if res.Code != http.StatusOK || res.Header().Get("Cache-Control") != "no-store" { t.Fatalf("bad response") }
    var body struct {
        GeneratedAt time.Time `json:"generated_at"`
        Running []domain.RunningRow `json:"running"`
        Retrying []domain.RetryRow `json:"retrying"`
        CodexTotals domain.TokenTotals `json:"codex_totals"`
    }
    json.Unmarshal(res.Body.Bytes(), &body)
    if body.GeneratedAt.IsZero() { t.Fatal("missing generated_at") }
}

func TestIssueHTMLTreatsProviderContentAsText(t *testing.T) {
    runtime := runtimeWithIssue(domain.Issue{Identifier: "GH-42", Title: "<script>bad()</script>", State: "open"})
    html := renderIssue(t, runtime, "GH-42")
    if strings.Contains(html, "<script>bad()") || !strings.Contains(html, "&lt;script&gt;bad()") { t.Fatal("unsafe issue title") }
}
```

Test 404 `issue_not_found`, 405, `202` refresh receipt, invalid identifier decoding, query/filter retention, error envelope/correlation ID, no workspace path to unauthenticated caller, log escaping, and no secret values in all bodies.

- [ ] **Step 2: Run focused tests and confirm failure**

```bash
go test ./internal/web -run 'Test(StateAPI|IssueHTML|Refresh|Logs|Activity)' -v
```

Expected: tests fail because queue/API handlers do not exist.

- [ ] **Step 3: Implement API and HTML view models**

Sort displayed issues by orchestration order but do not claim them. Render a captioned table at wide widths and the same semantic row content as lists at reflow widths using CSS, without duplicating both representations in the accessibility tree. The issue identifier/title is a normal link; status includes visible text. Filters use a GET form with labelled state, text, and eligibility controls.

The issue route URL-escapes one identifier segment and looks it up by exact normalized identifier. Activity renders bounded event summaries; Logs uses a labelled search form, level selector, pagination links, `<time datetime>`, and a horizontally scrollable labelled `<pre>` region whose updates are never live-announced. At wide widths render a real captioned table; at narrow widths render an equivalent labelled list. Both branches receive identical view-model data, CSS exposes exactly one branch per breakpoint with `display:none` on the other, and browser accessibility-tree tests assert only the visible representation exists for assistive technology.

- [ ] **Step 4: Add route-state axe and keyboard tests**

Cover empty, populated, provider error, filtered, issue-not-found, malicious provider text, long log line, and narrow viewport states. Assert table caption/header relationships, named filters, no row-click handlers, unique titles/headings, 44 px controls, focus visibility, 320 px reflow, text-spacing overrides, and no axe A/AA violations in Chromium/WebKit.

- [ ] **Step 5: Run tests and commit**

```bash
gofmt -w internal/web
go test ./internal/web -v
npm run html:validate
npm run test:a11y -- --project=chromium queue.spec.mjs logs.spec.mjs
npm run test:a11y -- --project=webkit queue.spec.mjs logs.spec.mjs
node scripts/a11y-precommit.mjs
git diff --check
git add go/internal/web go/web go/tests/accessibility
git commit -m "feat(go): present tracker work through accessible views"
```

---

### Task 7: Reconnect-safe SSE without focus or announcement churn

**Files:**
- Create: `go/internal/web/sse.go`
- Create: `go/internal/web/sse_test.go`
- Modify: `go/internal/web/routes.go`
- Modify: `go/web/static/app.js`
- Modify: `go/web/templates/base.html`
- Modify: `go/web/templates/overview.html`
- Modify: `go/web/templates/issues.html`
- Modify: `go/web/templates/activity.html`
- Create: `go/tests/accessibility/live-updates.spec.mjs`

**Interfaces:**
- Consumes: `RuntimeQueries.EventsAfter`, process epoch, and snapshot endpoint.
- Produces: `GET /api/v1/events`, SSE records with `id`, `event`, and JSON `data`.
- Produces: client controls `Pause live updates`/`Resume live updates`; orchestration/provider polling continues while presentation is paused.

- [ ] **Step 1: Write replay, reset, disconnect, and focus tests**

```go
func TestSSEReplaysAfterLastEventIDAndEmitsResetForGap(t *testing.T) {
    runtime := runtimeWithEvents(11, 12, 13)
    req := authenticatedRequest(t, http.MethodGet, "/api/v1/events", nil)
    req.Header.Set("Last-Event-ID", "epoch-a:12")
    body := streamUntil(t, serveStreaming(t, runtime, req), "id: epoch-a:13")
    if strings.Contains(body, "id: epoch-a:11") || !strings.Contains(body, "event: queue.updated") { t.Fatalf("bad replay: %s", body) }
}
```

Playwright holds focus on a filter/button while events arrive, asserts the same element remains focused, checks no dialog opens, checks routine updates do not mutate the live status region, pauses presentation, publishes an event, and verifies DOM rows remain stable until Resume.

- [ ] **Step 2: Run focused tests and confirm failure**

```bash
go test ./internal/web -run TestSSE -v
npm run test:a11y -- --project=chromium live-updates.spec.mjs
```

Expected: Go test fails because `/api/v1/events` is absent; Playwright receives 404.

- [ ] **Step 3: Implement bounded SSE**

Authenticate before streaming, set `text/event-stream`, `Cache-Control: no-store`, and `X-Accel-Buffering: no`, flush after each record, send a comment heartbeat every 20 seconds, stop immediately on request context cancellation, and cap each JSON event at 64 KiB. Encode IDs as `<process-epoch>:<unsigned-sequence>`. Accept either the browser-managed `Last-Event-ID` header or one initial `after` query cursor, rejecting disagreement; malformed IDs, epoch mismatch after restart, or an evicted sequence receive one `reset` event rather than 500.

- [ ] **Step 4: Implement idempotent progressive updates**

`app.js` is an ES module served from self. Each state snapshot includes its latest full event cursor; the client opens one `EventSource` with that cursor as the initial `after` query, then lets native reconnects send `Last-Event-ID`. It tracks the full cursor in memory, requests `/api/v1/state` after `reset`, and updates only elements with stable `data-issue-id`/`data-field` targets. Before replacement it records `document.activeElement`; it never replaces an ancestor of that element. The only polite status message is a concise user-triggered refresh result. Pause closes the source; Resume fetches a snapshot and reconnects from that snapshot cursor, preventing a snapshot/stream gap.

- [ ] **Step 5: Run tests and commit**

```bash
gofmt -w internal/web
go test ./internal/web -run TestSSE -v
npm run test:a11y -- --project=chromium live-updates.spec.mjs
npm run test:a11y -- --project=webkit live-updates.spec.mjs
git diff --check
git add go/internal/web go/web go/tests/accessibility/live-updates.spec.mjs
git commit -m "feat(go): stream queue updates without disrupting focus"
```

---

### Task 8: Tracker smoke profiles, CI, and operator documentation

**Files:**
- Create: `go/internal/tracker/github/live_test.go`
- Create: `go/internal/tracker/linear/live_test.go`
- Create: `go/docs/github.md`
- Create: `go/docs/linear.md`
- Create: `go/docs/observability.md`
- Create: `.github/workflows/go-integrations.yml`
- Modify: `go/README.md`
- Modify: `go/tests/accessibility/fixtures.mjs`
- Modify: `.github/workflows/go.yml`

**Interfaces:**
- Produces: opt-in `integration_live` tests that print explicit `SKIP` messages when disabled and fail when enabled credentials/scopes are invalid.
- Requires: `SYMPHONY_GITHUB_TEST_REPO`, `SYMPHONY_GITHUB_TEST_TOKEN`, `SYMPHONY_LINEAR_TEST_PROJECT`, and `SYMPHONY_LINEAR_TEST_TOKEN` only inside the credentialed workflow.

- [ ] **Step 1: Write live-profile enablement tests without network**

```go
func TestLiveProfileRequiresCompleteEnablement(t *testing.T) {
    env := map[string]string{"SYMPHONY_RUN_GITHUB_LIVE": "1", "SYMPHONY_GITHUB_TEST_REPO": "coryj627/symphony"}
    _, err := githubLiveConfig(mapLookup(env))
    if !errors.Is(err, errMissingLiveCredential) { t.Fatalf("got %v", err) }
}
```

Test disabled returns a named skip reason; enabled incomplete config is a test failure; repository/project test scope is parsed explicitly; tokens never appear in test names/output.

- [ ] **Step 2: Implement read-only live tests**

GitHub live test fetches `open` from the configured repository and validates every returned issue. Linear live test fetches `Todo`/`In Progress` from the configured project and validates records. Neither test creates, edits, or deletes tracker data. Both use an isolated temporary data directory and the environment credential resolver, then overwrite local token byte slices.

- [ ] **Step 3: Add manually dispatched credentialed CI**

`.github/workflows/go-integrations.yml` runs only on `workflow_dispatch`, has separate GitHub and Linear boolean inputs, and sets the matching `SYMPHONY_RUN_*_LIVE=1`. A selected profile with missing repository secret fails its setup step. An unselected profile prints `SKIPPED: profile not selected` and does not report a passing test.

- [ ] **Step 4: Document exact provider profiles and diagnostics**

`github.md` records config fields/defaults, one-repository scope, `GH-<number>` identity, PR behavior, pagination, optional assignee, secret env stripping, portable error categories, and read-only Phase 2 status. `linear.md` records project scope, blockers, pagination/batching, normalization, credential behavior, and errors. `observability.md` records JSONL path/rotation, required context fields, event retention/reset, redaction, and degraded logging.

- [ ] **Step 5: Run Phase 2 gates and commit**

```bash
go test ./...
go test -race ./...
go vet ./...
npm ci
npm run html:validate
npm run test:a11y
node scripts/a11y-scan-all.mjs
git diff --check
git add go/internal/tracker/github/live_test.go go/internal/tracker/linear/live_test.go go/docs go/README.md go/tests/accessibility/fixtures.mjs .github/workflows/go.yml .github/workflows/go-integrations.yml
git commit -m "test(go): verify tracker queue integrations"
```

## Phase 2 Acceptance

Run one instance in read-only run mode with a valid GitHub workflow and one with a valid Linear workflow, using different workflow files and ephemeral ports. Confirm each shows only its configured scope, provider failures are visible without partial rows, manual Refresh coalesces, SSE reconnect restores current state, logs never announce continuously, and no tracker mutation endpoint/tool is available.

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

Expected: exit 0. Run each selected credentialed profile separately and record its real `PASS` or explicit `SKIPPED` status.
