# Symphony Phase 4: Codex and Provider-Native Tools Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Connect the Phase 3 scheduler to a bounded Codex app-server 0.144.1 client, expose accessible operator approvals and user input, and execute Linear and GitHub tools host-side without exposing tracker credentials to the Codex child.

**Architecture:** Each worker captures one immutable workflow/adapter/tool snapshot, launches one Codex app-server child in the validated workspace, and owns its request router and live thread across bounded continuation turns. A broker translates only the server-request variants supported by the pinned schema into finite operator requests, while adapter-owned tool executors perform provider calls with host-side credentials and return bounded JSON-safe results.

**Tech Stack:** Go 1.26.5; Codex CLI/app-server 0.144.1 generated JSON schema; standard-library JSON, process, HTTP, context, and hashing packages; `github.com/santhosh-tekuri/jsonschema/v6` 6.0.2 for schema conformance tests; `github.com/vektah/gqlparser/v2` 2.5.36; `golang.org/x/sys` 0.47.0; Phase 1 secure web/configuration shell; Phase 2 tracker adapters and observability; Phase 3 worker/orchestrator contracts.

## Global Constraints

- The generated Codex 0.144.1 schema is the source of truth for method names and payload shapes. Symphony wire envelopes omit a JSON-RPC `jsonrpc` member because the targeted JSONL app-server transport does.
- Startup ordering is `initialize` response, `initialized` notification, `thread/start` response, then `turn/start`. No later method may overtake an earlier step.
- Protocol stdout uses newline-delimited JSON with a 10 MiB maximum line; stderr is a separate bounded diagnostic stream and is never decoded as protocol.
- Request IDs are correlation keys. A timed-out, duplicate, or late response cannot satisfy another request.
- Defaults are `approval_policy: on-request`, `thread_sandbox: workspace-write`, and turn sandbox `{type: workspaceWrite, writableRoots: [workspace], networkAccess: false}`. Values are validated against the vendored compatible schema.
- Operator requests are single-use and finite: one 10-minute default window, a warning at least 20 seconds before expiry, and no more than ten same-duration extensions.
- Cancellation sends `turn/interrupt` where a turn exists, waits a bounded grace period, then terminates the entire child process tree.
- Every child environment removes `GH_TOKEN`, `GITHUB_TOKEN`, `LINEAR_API_KEY`, the selected adapter's declared secret names, and any configured `$VAR_NAME` credential source.
- Only the selected adapter's captured tool specifications are advertised. Reload never changes an in-flight adapter, scope, endpoint, credential reference, issue context, or tool set.
- `linear_graphql` performs exactly one HTTP request per call. GitHub mutations are never automatically replayed after an ambiguous transport failure.
- Browser-visible request/tool/protocol data is escaped, redacted, and size-bounded. Raw secrets and authorization headers never enter the event journal, logs, snapshots, HTTP responses, or test artifacts.

---

### Task 1: Vendor the exact Codex schema and enforce compatibility preflight

**Files:**
- Create: `go/scripts/update-codex-schema.mjs`
- Create: `go/scripts/update-codex-schema.test.mjs`
- Create: `go/schema/codex/0.144.1/manifest.json`
- Generate: `go/schema/codex/0.144.1/codex_app_server_protocol.schemas.json`
- Generate: `go/schema/codex/0.144.1/codex_app_server_protocol.v2.schemas.json`
- Generate: `go/schema/codex/0.144.1/v1/InitializeParams.json`
- Generate: `go/schema/codex/0.144.1/v1/InitializeResponse.json`
- Generate: `go/schema/codex/0.144.1/v2/ThreadStartParams.json`
- Generate: `go/schema/codex/0.144.1/v2/ThreadStartResponse.json`
- Generate: `go/schema/codex/0.144.1/v2/TurnStartParams.json`
- Generate: `go/schema/codex/0.144.1/v2/TurnStartResponse.json`
- Generate: `go/schema/codex/0.144.1/v2/TurnCompletedNotification.json`
- Generate: every other file emitted by `codex app-server generate-json-schema --experimental`
- Create: `go/schema/codex/embed.go`
- Create: `go/internal/buildinfo/codex_schema.go`
- Create: `go/internal/buildinfo/codex_schema_test.go`
- Create: `go/internal/codex/compatibility.go`
- Create: `go/internal/codex/compatibility_test.go`

**Interfaces:**
- Produces: `buildinfo.CodexSchemaManifest`, loaded from package `schema/codex` through `//go:embed 0.144.1` with target version, aggregate SHA-256 digest, generation command, and reviewed compatible version/digest entries.
- Produces: `codex.CheckCompatibility(initializeResponse, manifest) Compatibility` with stable `compatible`, `version_mismatch`, `unknown_user_agent`, and `schema_integrity` codes.
- Consumes: `InitializeResponse.userAgent` and the checked-in manifest; it does not trust a browser-supplied version.

- [ ] **Step 1: Write manifest-integrity and compatibility tests**

```go
func TestVendoredCodexSchemaMatchesManifest(t *testing.T) {
    manifest, files := buildinfo.MustCodexSchema(t)
    if manifest.TargetVersion != "0.144.1" { t.Fatalf("target %q", manifest.TargetVersion) }
    got := buildinfo.AggregateSchemaDigest(files)
    if got != manifest.SchemaSHA256 { t.Fatalf("schema digest %s, manifest %s", got, manifest.SchemaSHA256) }
    if !manifest.Supports("0.144.1", got) { t.Fatal("target schema is not in reviewed compatibility set") }
}

func TestCompatibilityRejectsUnreviewedVersion(t *testing.T) {
    manifest := buildinfo.TestManifest("0.144.1", "sha256:known")
    got := CheckCompatibility(InitializeResponse{UserAgent: "codex_cli_rs/0.145.0"}, manifest)
    if got.DispatchAllowed || got.Code != "version_mismatch" { t.Fatalf("%+v", got) }
}
```

Also test malformed/missing user agent, an exact reviewed version, manifest digest tampering, duplicate compatibility entries, deterministic path ordering, CRLF-independent hashing, and a reviewed additional version/digest fixture.

- [ ] **Step 2: Run tests and confirm failure**

```bash
go test ./internal/buildinfo ./internal/codex -run 'Test(Vendored|Compatibility)' -v
node --test scripts/update-codex-schema.test.mjs
```

Expected: packages, manifest, and generation script do not exist.

- [ ] **Step 3: Implement the deterministic schema generator**

`update-codex-schema.mjs` allocates a directory with `fs.mkdtemp` and runs these commands without a shell:

```js
spawnSync('codex', ['--version'], {encoding: 'utf8'});
spawnSync('codex', ['app-server', 'generate-json-schema', '--out', schemaTempDirectory, '--experimental'], {encoding: 'utf8'});
```

Require the first command to report exactly `codex-cli 0.144.1`. Normalize only final newlines, sort relative filenames bytewise, hash `relative-path + NUL + file-bytes + NUL` for every Codex-generated JSON file, then write the separate manifest; `manifest.json` is deliberately excluded from its own aggregate digest. Replace only `schema/codex/0.144.1/` after every command and validation succeeds. The manifest records the literal generation command and every hashed relative path. The test injects a fake executable and proves failed generation leaves the prior directory byte-for-byte intact.

- [ ] **Step 4: Generate and review the snapshot**

```bash
node scripts/update-codex-schema.mjs
node --test scripts/update-codex-schema.test.mjs
git diff --stat -- schema/codex/0.144.1
git diff -- schema/codex/0.144.1/manifest.json
```

Expected: the manifest target is 0.144.1, its digest is computed rather than hand-entered, and the generated combined schema contains `initialize`, `thread/start`, `turn/start`, `turn/completed`, `item/tool/call`, command/file/permission approval requests, and `item/tool/requestUserInput`.

- [ ] **Step 5: Implement embedded integrity and runtime compatibility checks**

Parse the version only from the initialized app-server's `userAgent`, then match a reviewed manifest entry. A mismatch keeps the web UI and configuration available but returns a permanent worker failure and gates new dispatch. Expose the safe expected/observed version and remediation in `domain.ConfigStatus`; never include the full initialization payload.

`schema/codex/embed.go` owns `var Files embed.FS`; no `go:embed` pattern reaches through `..`, and the generator replaces only the `0.144.1` child so it cannot delete the package source.

- [ ] **Step 6: Run tests and commit**

```bash
gofmt -w internal/buildinfo internal/codex
go test ./internal/buildinfo ./internal/codex -run 'Test(Vendored|Compatibility|Schema)' -v
go test ./...
git diff --check
git add go/scripts/update-codex-schema.mjs go/scripts/update-codex-schema.test.mjs go/schema/codex go/internal/buildinfo go/internal/codex/compatibility.go go/internal/codex/compatibility_test.go
git commit -m "feat(go): pin the Codex app-server schema"
```

---

### Task 2: Build a bounded JSONL router with explicit request ownership

**Files:**
- Create: `go/internal/codex/errors.go`
- Create: `go/internal/codex/wire.go`
- Create: `go/internal/codex/wire_test.go`
- Create: `go/internal/codex/reader.go`
- Create: `go/internal/codex/reader_test.go`
- Create: `go/internal/codex/router.go`
- Create: `go/internal/codex/router_test.go`
- Create: `go/internal/codex/pending.go`
- Create: `go/internal/codex/pending_test.go`
- Create: `go/internal/codex/testkit_test.go`
- Create: `go/testdata/codex/wire/initialize-response.jsonl`
- Create: `go/testdata/codex/wire/server-requests.jsonl`
- Create: `go/testdata/codex/wire/turn-notifications.jsonl`

**Interfaces:**
- Produces: `codex.Envelope`, `RequestID`, `Response`, `ServerRequest`, `Notification`, and `ProtocolEvent` derived from the vendored schema.
- Produces: `codex.Router.Call(ctx, method, params, result)`, `Notify(method, params)`, `ServerRequests()`, `Notifications()`, and `Events()`.
- Guarantees: at most one pending call owns an ID; terminal router shutdown resolves every pending waiter with one typed error.

- [ ] **Step 1: Write framing, correlation, and adversarial routing tests**

```go
func TestRouterDoesNotLetLateResponseCompleteReusedCall(t *testing.T) {
    transport := newPipeTransport(t)
    router := NewRouter(transport, RouterOptions{ReadTimeout: time.Second, MaxLineBytes: 10 << 20})
    first := transport.StartCall(t, router, "initialize")
    first.CancelAndWait(t)
    second := transport.StartCall(t, router, "thread/start")
    transport.SendJSON(t, map[string]any{"id": first.ID, "result": map[string]any{}})
    second.AssertPending(t)
    transport.SendJSON(t, map[string]any{"id": second.ID, "result": map[string]any{"thread": map[string]any{"id": "t2"}}})
    second.AssertCompleted(t)
}

func TestReaderRejectsLineLargerThanTenMiB(t *testing.T) {
    _, err := ReadLine(bufio.NewReader(strings.NewReader(strings.Repeat("x", (10<<20)+1)+"\n")), 10<<20)
    if !errors.Is(err, ErrMessageTooLarge) { t.Fatalf("%v", err) }
}
```

Test numeric and string IDs without lossy float conversion, split reads, CRLF, EOF after a final complete line, missing newline on an incomplete object, malformed JSON, a response carrying both result/error, unknown response ID, duplicate response, notification, server request, unknown message, writer serialization under concurrency, request timeout removal, cancellation, subprocess EOF, and 100 concurrent calls under `-race`.

- [ ] **Step 2: Run tests and confirm failure**

```bash
go test ./internal/codex -run 'Test(Router|Reader|Wire|Pending)' -v
```

Expected: JSONL transport and correlation types do not exist.

- [ ] **Step 3: Implement bounded wire decoding**

Decode the envelope first into `json.RawMessage` fields. Classify in this order: response (`id` plus exactly one of `result`/`error`), server request (`id` plus `method`), notification (`method` without `id`), else `malformed`. Preserve IDs as their canonical JSON token and reject object/array/null IDs. Do not add or require `jsonrpc`.

Every physical stdout line, including an unknown or malformed JSON line, emits an activity pulse used by the turn silence timer. A line over 10 MiB is fatal to that client. Unknown notifications are observable and nonfatal; out-of-sequence required startup messages fail the owning session.

`testkit_test.go` defines the in-memory pipe transport, controlled call handle, JSON sender, and pending/completion assertions used by this task.

- [ ] **Step 4: Implement one reader, one writer lock, and pending ownership**

Use an atomic monotonically increasing integer request ID. Register before writing; remove on response, timeout, cancellation, write error, or router shutdown. Never reuse an ID during one process. A late response emits `late_response` with the ID but no payload. Protocol errors expose a stable code, bounded safe summary, and retryability.

- [ ] **Step 5: Run race tests and commit**

```bash
gofmt -w internal/codex
go test ./internal/codex -run 'Test(Router|Reader|Wire|Pending)' -count=10 -v
go test -race ./internal/codex -run 'TestRouter' -count=10
go test ./...
git diff --check
git add go/internal/codex go/testdata/codex/wire
git commit -m "feat(go): add bounded Codex JSONL routing"
```

---

### Task 3: Implement initialize, thread, turn, and telemetry semantics

**Files:**
- Create: `go/internal/codex/protocol_types.go`
- Create: `go/internal/codex/protocol_types_test.go`
- Create: `go/internal/codex/session.go`
- Create: `go/internal/codex/session_test.go`
- Create: `go/internal/codex/turn.go`
- Create: `go/internal/codex/turn_test.go`
- Create: `go/internal/codex/telemetry.go`
- Create: `go/internal/codex/telemetry_test.go`
- Modify: `go/internal/codex/testkit_test.go`
- Create: `go/testdata/codex/session/happy.jsonl`
- Create: `go/testdata/codex/session/failed-turn.jsonl`
- Create: `go/testdata/codex/session/usage-rate-limit.jsonl`
- Modify: `go/go.mod`
- Modify: `go/go.sum`

**Interfaces:**
- Produces: `codex.NewSession(router, SessionOptions)`, `Initialize`, `StartThread`, `StartTurn`, `InterruptTurn`, and `Close`.
- Produces: stable `SessionEvent` values carrying UTC time, process ID when known, thread/turn/session IDs, absolute usage totals, rate limits, and safe summaries.
- Consumes: immutable workspace path, policy values, dynamic tool specs, clock, and event callback.

- [ ] **Step 1: Write an exact startup transcript test**

```go
func TestSessionStartupUsesPinnedProtocolOrderAndWorkspace(t *testing.T) {
    fake := NewTranscriptServer(t)
    session := NewSession(fake.Router(), testSessionOptions(t))
    threadID, err := session.Start(t.Context())
    if err != nil { t.Fatal(err) }
    if threadID != "thread-1" { t.Fatalf("thread %q", threadID) }
    fake.AssertMethods(t, "initialize", "initialized", "thread/start")
    fake.AssertThreadStart(t, func(p ThreadStartParams) {
        if p.Cwd != fake.Workspace || p.ThreadSandbox != "workspace-write" { t.Fatalf("%+v", p) }
        if !slices.Equal(p.RuntimeWorkspaceRoots, []string{fake.Workspace}) { t.Fatalf("%+v", p.RuntimeWorkspaceRoots) }
    })
}
```

Assert initialize `clientInfo.name == "symphony-go"`, semantic client version, required experimental capability for dynamic tools/user input, and `initialized` has no params. Assert thread/start includes captured approval policy, cwd, runtime workspace roots, sandbox, and only captured tool specs. Compile the vendored draft-7 schemas with `github.com/santhosh-tekuri/jsonschema/v6` 6.0.2 and validate every serialized request/response/notification fixture against its exact schema in tests.

- [ ] **Step 2: Write turn completion, timeout, and telemetry tests**

```go
func TestTurnUsesTextInputAndSessionIdentity(t *testing.T) {
    result, err := testSession(t).StartTurn(t.Context(), "Do the work")
    if err != nil { t.Fatal(err) }
    if result.SessionID != "thread-1-turn-7" || result.Status != TurnCompleted { t.Fatalf("%+v", result) }
}
```

Test `turn/start` input `[{'type':'text','text':'...'}]`, absolute cwd and workspace-write turn sandbox, `turn/completed` statuses `completed`, `failed`, and `interrupted`, response error, subprocess exit, silence timeout reset by each stdout line, silence accounting paused while an operator-backed server request is pending and resumed after its reply, read timeout during synchronous calls, cancellation calling `turn/interrupt`, mismatched thread/turn notification ignored with event, duplicate terminal notification, and max-sized valid payload. Feed repeated absolute token/rate-limit notifications and prove totals are replaced/idempotent rather than added twice.

- [ ] **Step 3: Run tests and confirm failure**

```bash
go test ./internal/codex -run 'Test(Session|Turn|Telemetry|ProtocolTypes)' -v
```

Expected: lifecycle and schema-derived types do not exist.

- [ ] **Step 4: Implement schema-derived request/response structs**

Keep the structs narrow but match the pinned schema exactly. Use custom JSON for pass-through approval/sandbox values so unsupported shapes fail schema-derived local validation before child launch. The first thread uses `thread/start`; this phase does not claim cross-process thread resumption. A live session reuses the returned thread ID for every `turn/start`.

Extend `testkit_test.go` with the transcript server, session options, workspace fixture, and method/parameter assertions before compiling the session tests.

- [ ] **Step 5: Implement terminal and telemetry state machines**

Only the active turn ID may complete the waiter. `completed` succeeds; `failed` maps the schema error to `turn_failed`; `interrupted` maps to `turn_cancelled`; `inProgress` in a completion notification is malformed. Build `session_id` as `<thread_id>-<turn_id>`. Emit notification summaries without raw model content. Record only absolute usage values exposed by the targeted protocol; unknown delta-only shapes remain observable but do not alter totals.

- [ ] **Step 6: Run tests and commit**

```bash
gofmt -w internal/codex
go test ./internal/codex -run 'Test(Session|Turn|Telemetry|ProtocolTypes)' -count=5 -v
go test -race ./internal/codex -run 'Test(Session|Turn)' -count=5
go test ./...
git diff --check
git add go/go.mod go/go.sum go/internal/codex go/testdata/codex/session
git commit -m "feat(go): implement Codex session and turn lifecycle"
```

---

### Task 4: Launch Bash safely and terminate complete process trees

**Files:**
- Create: `go/internal/codex/environment.go`
- Create: `go/internal/codex/environment_test.go`
- Create: `go/internal/codex/bash.go`
- Create: `go/internal/codex/bash_test.go`
- Create: `go/internal/codex/process.go`
- Create: `go/internal/codex/process_test.go`
- Create: `go/internal/codex/process_darwin.go`
- Create: `go/internal/codex/process_darwin_test.go`
- Create: `go/internal/codex/process_windows.go`
- Create: `go/internal/codex/process_windows_test.go`
- Create: `go/internal/codex/stderr.go`
- Create: `go/internal/codex/stderr_test.go`
- Create: `go/internal/codex/testhelper/main.go`
- Modify: `go/internal/codex/testkit_test.go`

**Interfaces:**
- Produces: `codex.Launch(ctx, LaunchOptions) (Process, error)` where `LaunchOptions` contains validated cwd, literal workflow command, environment, declared secret names, and graceful/force deadlines.
- Produces: `codex.FindBash()` with `/bin/bash` on macOS and Git for Windows Bash discovery on Windows.
- Guarantees: a macOS process group or Windows Job Object contains the app-server and descendants before protocol traffic is accepted.

- [ ] **Step 1: Write environment and invocation tests**

```go
func TestChildEnvironmentRemovesEveryTrackerSecretName(t *testing.T) {
    env := []string{"PATH=/bin", "GH_TOKEN=one", "GITHUB_TOKEN=two", "LINEAR_API_KEY=three", "CUSTOM_TRACKER=four", "SAFE=yes"}
    got := SanitizeEnvironment(env, []string{"CUSTOM_TRACKER"})
    assertEnv(t, got, map[string]string{"PATH": "/bin", "SAFE": "yes"})
}

func TestBashReceivesCommandAsOneArgument(t *testing.T) {
    spec := BashCommand("/safe/bash", "codex app-server --config 'x y'")
    if diff := slices.Compare(spec.Args, []string{"-lc", "codex app-server --config 'x y'"}); diff != 0 { t.Fatalf("%q", spec.Args) }
}
```

Test case-insensitive environment keys on Windows, duplicate keys, credential `$VAR_NAME`, empty/invalid secret names, cwd canonical containment recheck, missing Bash remediation, stderr truncation/redaction, and that the workflow command is never concatenated with a path or browser value.

- [ ] **Step 2: Write OS process-tree contract tests**

The test helper starts a child and grandchild, writes their PIDs, and waits. Cancel through `Process.Stop`; assert both descendants exit, the method is idempotent, graceful completion avoids force kill, force deadline is bounded, and no unrelated process is signaled. The Windows test also asserts the process is assigned to a kill-on-close Job Object before returning. The macOS test asserts a dedicated process group.

- [ ] **Step 3: Run tests and confirm failure**

```bash
go test ./internal/codex -run 'Test(ChildEnvironment|Bash|Process|Stderr)' -v
```

Expected: host launch and containment implementations do not exist.

- [ ] **Step 4: Implement launch and graceful-to-force shutdown**

On macOS set a new process group before start; send interrupt/termination to the group, wait five seconds, then kill the group. On Windows create a Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, start suspended when required to avoid an uncontained child race, assign it, resume it, and terminate/close the job after the same grace interval. `Session.Close` first sends `turn/interrupt` when applicable, closes stdin after its bounded reply window, then delegates to `Process.Stop`.

Capture at most 256 KiB of redacted stderr diagnostic tail and log summarized lines as `codex_stderr`; protocol stdout remains untouched.

Extend `testkit_test.go` with environment assertions and native helper-process launch/wait utilities. The Windows implementation uses `windows.CreateProcess` with `CREATE_SUSPENDED|CREATE_UNICODE_ENVIRONMENT` and explicit inheritable stdio handles, assigns the returned process handle to the configured Job Object, calls `ResumeThread` on the returned primary thread handle, then closes that handle; protocol handling cannot start before containment succeeds.

- [ ] **Step 5: Compile and run per-platform tests**

```bash
gofmt -w internal/codex
go test ./internal/codex -run 'Test(ChildEnvironment|Bash|Process|Stderr)' -v
GOOS=darwin GOARCH=arm64 go test -c ./internal/codex -o "$TMPDIR/codex-darwin.test"
GOOS=windows GOARCH=amd64 go test -c ./internal/codex -o "$TMPDIR/codex-windows.test.exe"
go test ./...
git diff --check
```

Run the OS-specific process-tree tests on their native CI runners; cross-compilation is not a substitute for those assertions.

- [ ] **Step 6: Commit**

```bash
git add go/internal/codex
git commit -m "feat(go): contain Codex child process trees"
```

---

### Task 5: Broker finite operator requests through an accessible UI

**Files:**
- Modify: `go/internal/domain/operator_request.go`
- Create: `go/internal/codex/request_broker.go`
- Create: `go/internal/codex/request_broker_test.go`
- Create: `go/internal/codex/server_request.go`
- Create: `go/internal/codex/server_request_test.go`
- Modify: `go/internal/app/runtime.go`
- Create: `go/internal/app/runtime_test.go`
- Modify: `go/internal/web/routes.go`
- Create: `go/internal/web/requests.go`
- Create: `go/internal/web/requests_test.go`
- Modify: `go/internal/web/viewmodels.go`
- Create: `go/web/templates/partials/operator_requests.html`
- Modify: `go/web/templates/overview.html`
- Modify: `go/web/templates/issue.html`
- Modify: `go/web/static/app.css`
- Create: `go/web/static/requests.js`
- Modify: `go/tests/accessibility/fixtures.mjs`
- Create: `go/tests/accessibility/operator-requests.spec.mjs`
- Modify: `go/internal/codex/testkit_test.go`

**Interfaces:**
- Produces: `codex.RequestBroker.Open(ServerRequestContext)`, `Respond(domain.OperatorResponse)`, `Extend(id)`, `CancelSession(sessionID)`, and `Pending()`.
- Implements: roadmap `app.RuntimeCommands.Respond`; adds `ExtendOperatorRequest` without weakening the existing interface.
- Handles pinned methods `item/commandExecution/requestApproval`, `item/fileChange/requestApproval`, `item/permissions/requestApproval`, and `item/tool/requestUserInput`.

- [ ] **Step 1: Write fake-clock deadline and exactly-once tests**

```go
func TestOperatorRequestExpiresAfterAtMostElevenWindows(t *testing.T) {
    clock := newFakeClock()
    broker := newTestBroker(t, clock, 10*time.Minute)
    request := broker.Open(testCommandApproval())
    for i := 0; i < 10; i++ {
        clock.Advance(9*time.Minute + 45*time.Second)
        if err := broker.Extend(request.ID); err != nil { t.Fatalf("extension %d: %v", i+1, err) }
    }
    if err := broker.Extend(request.ID); !errors.Is(err, ErrExtensionLimit) { t.Fatalf("%v", err) }
    clock.Advance(10 * time.Minute)
    broker.AssertProtocolDecision(t, request.ID, "cancel")
    broker.AssertFailedOnce(t, request.SessionID)
}

func TestOperatorResponseIsAcceptedExactlyOnce(t *testing.T) {
    broker := newTestBroker(t, newFakeClock(), 10*time.Minute)
    req := broker.Open(testFileApproval())
    if err := broker.Respond(domain.OperatorResponse{RequestID: req.ID, SessionID: req.SessionID, Choice: "decline"}); err != nil { t.Fatal(err) }
    if err := broker.Respond(domain.OperatorResponse{RequestID: req.ID, SessionID: req.SessionID, Choice: "accept"}); !errors.Is(err, ErrStaleRequest) { t.Fatalf("%v", err) }
}
```

Test the warning event at 20 seconds, timeout, ten extensions reaching the full eleven-window limit without the turn silence timer preempting it, process exit, shutdown, stale/wrong session, unknown ID, response-vs-timeout race under `-race`, all legal command/file decisions, denied permission mapping, multi-question answers, free text, option validation, secret answer redaction, malformed server params, and unsupported current/future server requests returning a bounded protocol error rather than hanging.

- [ ] **Step 2: Run broker tests and confirm failure**

```bash
go test ./internal/codex ./internal/domain -run 'Test(Operator|ServerRequest)' -v
```

Expected: broker and request domain types do not exist.

- [ ] **Step 3: Implement schema-exact server-request mapping**

Map UI choices only to values allowed by the pinned response schemas. Permission approval may grant only the exact displayed requested profile for turn/session; denial sends an empty permission profile. `requestUserInput` response keys must exactly match question IDs, answers remain memory-only, and secret answers are replaced by `[REDACTED]` before any audit event. Maintain a per-turn pending-elicitation count: while it is nonzero, pause Symphony's own turn-silence timer and let only each finite broker deadline run; resume silence accounting when the count returns to zero after responses/errors are written. Do not support account token refresh or legacy request variants silently: return an error response and fail safely when the protocol cannot continue.

Extend `testkit_test.go` with the fake clock, broker constructor, protocol-decision recorder, and request fixtures used by the deadline/race tests.

- [ ] **Step 4: Write HTTP and accessibility tests**

```go
func TestRespondRejectsStaleRequestWithoutMutatingBroker(t *testing.T) {
    app := newWebTestApp(t)
    response := app.PostForm("/api/v1/requests/stale/respond", validCSRF(map[string]string{
        "session_id": "thread-1-turn-1", "choice": "decline",
    }))
    if response.Code != http.StatusConflict { t.Fatalf("status %d", response.Code) }
    app.AssertNoRuntimeCommand(t)
}
```

Playwright opens command approval, file approval, permission approval, option questions, free-text questions, and the expiring warning. Assert named `<fieldset>`/`<legend>`, persistent labels and descriptions, issue/session context, visible deadline text, a 44px Extend button, keyboard-only response, no focus movement from countdown updates, focus restoration after submit, error-summary focus on invalid submit, and concise status announcement. Secret fields use `type=password` but permit paste.

- [ ] **Step 5: Implement server-rendered request surfaces**

Render one global Requests region on Overview and the active issue's requests before its event/log stream. Forms post normally and work without JavaScript. `requests.js` only reduces countdown display frequency, emits the 20-second warning once, and preserves focus; the server owns expiry. Routine countdown ticks and SSE refreshes are `aria-live=off`.

- [ ] **Step 6: Run tests and commit**

```bash
gofmt -w internal/domain internal/codex internal/app internal/web
go test ./internal/codex ./internal/app ./internal/web -run 'Test(Operator|ServerRequest|Respond|Request)' -v
npm run test:a11y -- --grep "operator request"
npm run html:validate
go test ./...
git diff --check
git add go/internal/domain/operator_request.go go/internal/codex go/internal/app go/internal/web go/web/templates go/web/static go/tests/accessibility
git commit -m "feat(go): add accessible Codex operator requests"
```

---

### Task 6: Run bounded continuation turns and wire the real worker

**Files:**
- Create: `go/internal/codex/runner.go`
- Create: `go/internal/codex/runner_test.go`
- Create: `go/internal/codex/continuation.go`
- Create: `go/internal/codex/continuation_test.go`
- Create: `go/internal/codex/worker.go`
- Create: `go/internal/codex/worker_test.go`
- Modify: `go/internal/app/runtime.go`
- Modify: `go/internal/app/runtime_test.go`
- Modify: `go/internal/orchestrator/worker.go`
- Modify: `go/internal/orchestrator/worker_test.go`
- Modify: `go/internal/orchestrator/reconcile.go`
- Modify: `go/internal/orchestrator/reconcile_test.go`
- Modify: `go/cmd/symphony/main.go`
- Modify: `go/internal/codex/testkit_test.go`

**Interfaces:**
- Produces: `codex.AgentAttempt`, implementing the `orchestrator.AgentAttempt` used by Phase 3 `LifecycleWorker`; the composed lifecycle worker remains the roadmap `orchestrator.Worker`.
- Consumes: one captured `workflow.Snapshot`, tracker adapter/session, workspace manager, request broker, process launcher, and event sink per run.
- Guarantees: first turn gets the full strict-rendered prompt; later turns use continuation guidance on the same live thread and never exceed `agent.max_turns`.

- [ ] **Step 1: Write lifecycle trace tests with a fake app-server**

```go
func TestWorkerContinuesSameThreadWithoutRepeatingOriginalPrompt(t *testing.T) {
    fake := newWorkerCodex(t, completedTurns(3))
    tracker := fakeTrackerStates("open", "open", "closed")
    result := newComposedLifecycleWorker(fake, tracker).Run(t.Context(), testRunRequest(), fake.Events)
    if result.Reason != domain.StopCompleted { t.Fatalf("%+v", result) }
    fake.AssertOneThread(t)
    fake.AssertTurnText(t, 1, "Original task for GH-42")
    fake.AssertTurnTextContains(t, 2, "continuation turn #2 of 20")
    fake.AssertTurnTextDoesNotContain(t, 2, "Original task for GH-42")
}
```

Test full order `ensure -> before_run -> launch -> initialize -> thread -> turn(s) -> after_run -> close`; prompt failure before launch; compatibility gate; startup failure; active/routable continuation; terminal after turn; non-active release; missing issue; refresh failure; max turns; cancellation; timeout; panic recovery; process tree exit; after_run on every workspace-backed attempt including before_run failure; immutable session snapshot after workflow reload; UTC events; and exactly one final `RunResult`.

- [ ] **Step 2: Run tests and confirm failure**

```bash
go test ./internal/codex ./internal/orchestrator ./internal/app -run 'Test(Worker|Continuation|RuntimeCodex)' -v
```

Expected: real worker composition and continuation loop do not exist.

- [ ] **Step 3: Implement the agent runner contract**

`LifecycleWorker` retains ownership of `Ensure`, prompt rendering, `before_run`, and best-effort `after_run`. `codex.AgentAttempt` receives the validated workspace, rendered first prompt, captured issue/workflow/adapter session, and event callback; it owns launch, initialize, live thread, turns, tracker rechecks, and close. Use this fixed continuation guidance, filling only turn number and maximum:

Extend `testkit_test.go` with completed-turn scenarios, tracker state sequences, composed lifecycle construction, and turn-text assertions before running the red worker tests.

```text
Continuation guidance:

- The previous Codex turn completed normally, but the tracker work item is still in an active state.
- This is continuation turn #N of M for the current agent run.
- Resume from the current workspace and workpad state instead of restarting from scratch.
- The original task instructions and prior turn context are already present in this thread, so do not restate them before acting.
- Focus on the remaining ticket work and do not end the turn while the issue stays active unless you are truly blocked.
```

After each successful turn fetch exactly the current opaque issue ID. Continue only when the full normalized issue is present, active, and adapter-routable. Close the process before returning. Preserve successful workspaces. Convert every failure to the `AgentAttempt` result consumed by Phase 3 retry semantics, with its stable category and safe message.

- [ ] **Step 4: Wire production without a fake/unavailable state**

At startup build the selected adapter, schema compatibility checker, broker, process launcher, and `codex.AgentAttempt`; compose it with Phase 3 `orchestrator.LifecycleWorker` and inject that worker into the orchestrator. Before enabling dispatch, launch one short-lived app-server preflight with the configured command, complete initialize/compatibility validation, and shut it down cleanly; each real attempt repeats the handshake rather than trusting stale preflight state. Scheduler readiness requires a valid workflow, tracker adapter, vault credential, safe workspace root, native Bash, intact schema, and compatible app-server handshake. The Overview states the exact unmet prerequisite rather than displaying a generic paused label.

Reconciliation waits for confirmed worker exit before terminal workspace removal. If bounded stop fails, keep the workspace, mark the row `stopping_failed`, gate redispatch of that ID, and surface remediation; never delete underneath a live/unconfirmed child.

- [ ] **Step 5: Run integration and race tests, then commit**

```bash
gofmt -w internal/codex internal/orchestrator internal/app cmd/symphony
go test ./internal/codex ./internal/orchestrator ./internal/app -run 'Test(Worker|Continuation|RuntimeCodex)' -count=5 -v
go test -race ./internal/codex ./internal/orchestrator -run 'Test(Worker|Reconcile)' -count=5
go test ./...
git diff --check
git add go/internal/codex go/internal/orchestrator go/internal/app go/cmd/symphony
git commit -m "feat(go): run Codex workers through the scheduler"
```

---

### Task 7: Add the captured Linear `linear_graphql` tool

**Files:**
- Modify: `go/go.mod`
- Modify: `go/go.sum`
- Create: `go/internal/tracker/linear/tool.go`
- Create: `go/internal/tracker/linear/tool_test.go`
- Create: `go/internal/tracker/linear/graphql.go`
- Create: `go/internal/tracker/linear/graphql_test.go`
- Create: `go/internal/tracker/linear/tool_result.go`
- Create: `go/internal/tracker/linear/tool_result_test.go`
- Modify: `go/internal/tracker/linear/adapter.go`
- Modify: `go/internal/tracker/linear/adapter_test.go`
- Modify: `go/internal/tracker/linear/testkit_test.go`
- Create: `go/testdata/linear/tool-success.json`
- Create: `go/testdata/linear/tool-errors.json`

**Interfaces:**
- Implements: tracker `AgentTools` and `ExecuteAgentTool` for `linear_graphql`.
- Advertises input `{query: string, variables?: object}` and also accepts a top-level JSON string as query shorthand.
- Returns: `{success:boolean,data?:object,errors?:array,error?:object}` translated by the Codex router to one `inputText` content item.

- [ ] **Step 1: Write GraphQL parsing and one-request transport tests**

```go
func TestLinearToolRequiresExactlyOneOperation(t *testing.T) {
    for _, query := range []string{"fragment F on Issue { id }", "query A { viewer { id } } query B { viewer { id } }"} {
        result := executeLinearTool(t, query)
        if result.Success || result.Error.Code != "invalid_operation_count" { t.Fatalf("%+v", result) }
    }
}

func TestLinearMutationAmbiguousFailureIsNotReplayed(t *testing.T) {
    server := newDropAfterReadServer(t)
    result := newLinearTool(server.URL).Execute(t.Context(), json.RawMessage(`{"query":"mutation { issueUpdate(id:\"x\", input:{title:\"y\"}) { success } }"}`))
    if result.Success || server.RequestCount() != 1 { t.Fatalf("result=%+v requests=%d", result, server.RequestCount()) }
}
```

Test query/mutation, named/anonymous operation, fragments plus one operation, no operation, two operations, syntax error, variables object, shorthand string, invalid JSON/type, captured endpoint/auth, missing credential, HTTP failure, GraphQL errors on HTTP 200, malformed/oversize response, rate-limit metadata, secret redaction, cancellation, and no automatic retry for any call.

- [ ] **Step 2: Run tests and confirm failure**

```bash
go test ./internal/tracker/linear -run 'TestLinear.*(Tool|GraphQL|Mutation)' -v
```

Expected: tool executor and GraphQL parser dependency do not exist.

- [ ] **Step 3: Implement one-operation validation and captured execution**

Use `github.com/vektah/gqlparser/v2/parser` 2.5.36 to parse a query document without requiring a provider schema; require `len(document.Operations) == 1`. Do not use substring or regex operation counting. POST once to the captured Linear endpoint with the captured credential resolver, a bounded 1 MiB response reader, and redirects disabled outside the configured origin.

HTTP 2xx with nonempty top-level `errors` returns `success:false` while retaining only bounded/redacted `data` and `errors`. Transport ambiguity returns `retryable:false` for mutations and never triggers an internal replay. The orchestrator remains unaware of GraphQL semantics.

- [ ] **Step 4: Advertise schema and translate results**

Return one function dynamic-tool spec named `linear_graphql`. Marshal the result to JSON and then to `DynamicToolCallResponse{success, contentItems:[{type:"inputText",text:...}]}`. Invalid arguments are a normal tool failure and do not fail the whole transport/session.

- [ ] **Step 5: Run tests and commit**

```bash
gofmt -w internal/tracker/linear
go test ./internal/tracker/linear -run 'TestLinear.*(Tool|GraphQL|Mutation)' -count=5 -v
go test ./internal/codex ./internal/tracker/linear
go test ./...
git diff --check
git add go/go.mod go/go.sum go/internal/tracker/linear go/testdata/linear
git commit -m "feat(go): expose the scoped Linear GraphQL tool"
```

---

### Task 8: Add the repository/current-issue-scoped GitHub tool

**Files:**
- Create: `go/internal/tracker/github/tool.go`
- Create: `go/internal/tracker/github/tool_test.go`
- Create: `go/internal/tracker/github/tool_input.go`
- Create: `go/internal/tracker/github/tool_input_test.go`
- Create: `go/internal/tracker/github/tool_result.go`
- Create: `go/internal/tracker/github/tool_result_test.go`
- Create: `go/internal/tracker/github/idempotency.go`
- Create: `go/internal/tracker/github/idempotency_test.go`
- Modify: `go/internal/tracker/github/client.go`
- Modify: `go/internal/tracker/github/client_test.go`
- Modify: `go/internal/tracker/github/adapter.go`
- Modify: `go/internal/tracker/github/adapter_test.go`
- Modify: `go/internal/tracker/github/testkit_test.go`
- Create: `go/testdata/github/tool-issue.json`
- Create: `go/testdata/github/tool-comments.json`

**Interfaces:**
- Implements: tracker `AgentTools` and `ExecuteAgentTool` for `github_api`.
- Operations: `get_issue`, `update_issue`, `list_comments`, `create_comment`, `set_labels`, `add_assignees`, and `remove_assignees` only.
- Returns: `{success:boolean,status:integer,request_id?:string,data?:object,error?:object}`.

- [ ] **Step 1: Write scope and allowlist adversaries**

```go
func TestGitHubToolCannotEscapeCapturedIssue(t *testing.T) {
    session := githubSession("coryj627", "symphony", 42)
    result := executeGitHubTool(t, session, `{"operation":"get_issue","issue_number":43}`)
    if result.Success || result.Error.Code != "issue_scope_mismatch" { t.Fatalf("%+v", result) }
}

func TestCreateCommentRequiresAndDeduplicatesIdempotencyKey(t *testing.T) {
    server := newGitHubToolServer(t)
    call := `{"operation":"create_comment","idempotency_key":"session-key-1","input":{"body":"hello"}}`
    first := server.Tool.Execute(t.Context(), call)
    second := server.Tool.Execute(t.Context(), call)
    if !first.Success || !second.Success || server.Count("POST", "/repos/coryj627/symphony/issues/42/comments") != 1 { t.Fatalf("%+v %+v", first, second) }
}
```

Test omitted issue number defaults to current, zero/negative/fraction/string rejected, owner/repository fields rejected, pull-request context rejected, unknown operation, extra top-level/input keys, strict per-operation fields, update allowlist, labels/assignee types, mutation method/path/body, safe GET retry only, redirect to other origin rejected, rate-limit/request-ID propagation, 401/403/404/422/429/5xx mapping, body bounds, credential redaction, idempotency-key reuse with different body rejected, session-local dedupe, and ambiguous mutation failure never replayed.

- [ ] **Step 2: Run tests and confirm failure**

```bash
go test ./internal/tracker/github -run 'TestGitHub.*(Tool|Idempotency|Scope|Operation)' -v
```

Expected: scoped tool implementation does not exist.

- [ ] **Step 3: Implement strict input decoders and provider calls**

Use `json.Decoder.DisallowUnknownFields` at every operation-specific shape. Symphony supplies the URL owner/repository and issue number from the immutable session snapshot. `update_issue` accepts only `title`, `body`, `state`, `state_reason`, and `milestone`; each other mutation accepts only its documented body/list fields. Refuse execution when the captured issue `native_ref` identifies a pull request.

GET operations may retry only failures proven to have sent no mutation. Set-style calls use one provider request. `create_comment` requires a nonempty idempotency key, hashes the normalized input, and caches its bounded result for the captured session; the same key with a different hash fails. Never follow a redirect outside the configured API origin.

- [ ] **Step 4: Advertise and route the tool**

Advertise one function spec named `github_api` with the exact operation enum and conditional fields described above. Verify a GitHub session advertises no Linear tool, a Linear session advertises no GitHub tool, and a workflow reload changes only later sessions.

- [ ] **Step 5: Run tests and commit**

```bash
gofmt -w internal/tracker/github
go test ./internal/tracker/github -run 'TestGitHub.*(Tool|Idempotency|Scope|Operation)' -count=5 -v
go test ./internal/codex ./internal/tracker/github
go test ./...
git diff --check
git add go/internal/tracker/github go/testdata/github
git commit -m "feat(go): expose the scoped GitHub issue tool"
```

---

### Task 9: Prove the complete Codex slice and document its safety boundary

**Files:**
- Create: `go/internal/codex/fakeappserver/main.go`
- Create: `go/internal/codex/fakeappserver/scenario.go`
- Create: `go/internal/codex/fakeappserver/scenario_test.go`
- Create: `go/internal/app/codex_integration_test.go`
- Create: `go/internal/app/codex_failure_integration_test.go`
- Modify: `go/tests/accessibility/fixtures.mjs`
- Create: `go/tests/accessibility/codex-runtime.spec.mjs`
- Create: `go/tests/accessibility/codex-runtime.axe.spec.mjs`
- Create: `go/tests/conformance/codex_phase4_test.go`
- Create: `go/docs/codex.md`
- Create: `go/docs/provider-tools.md`
- Create: `go/docs/security.md`
- Modify: `go/README.md`
- Modify: `.github/workflows/go.yml`
- Modify: `.github/workflows/go-integrations.yml`

**Interfaces:**
- Produces: a deterministic fake app-server executable selected only by test workflow fixtures.
- Produces: `SYMPHONY_REAL_CODEX_SMOKE=1` opt-in profile; absence reports `SKIPPED` and never `PASSED`.
- Guarantees: production composition has no Phase 3 fake worker path.

- [ ] **Step 1: Write full-process happy and failure integration tests**

Build the fake app-server, launch the real Symphony binary with a temporary workflow/workspace/data directory, inject a fake tracker/vault through the existing test bootstrap build tag, authorize the browser session, start GH-42, respond to one approval and one user-input request over HTTP, execute the selected fake provider tool, finish two turns, and assert the orchestrator snapshot and event journal.

Separate scenarios cover incompatible user agent, malformed/oversize stdout, stderr noise, request timeout, turn silence, unsupported tool, approval expiry, stale response, child exit, failed/interrupted turn, cancellation with descendant, canary secrets in every fake payload, and shutdown during a request. Assert no canary reaches files, stderr capture, HTTP, SSE, snapshots, or Playwright artifacts.

- [ ] **Step 2: Run integration tests and confirm failure**

```bash
go test ./internal/app ./internal/codex -run 'TestCodex.*Integration|TestFakeAppServer' -v
```

Expected: fake executable, scenarios, and process-level tests do not exist.

- [ ] **Step 3: Implement the fake app-server and complete composition**

The fake accepts newline-delimited requests on stdin and emits only pinned-schema-valid messages except in explicitly named malformed scenarios. Scenario selection comes from a test-only environment variable unavailable in the production build. Production always uses the workflow command and native process containment.

- [ ] **Step 4: Add runtime accessibility states**

Exercise Overview and issue detail with pending/expiring/stale approval, multiple user-input questions, secret input, tool failure, incompatible Codex, and stopping failure. Run axe with WCAG 2.2 A/AA tags, keyboard journeys, name/role/state assertions, focus restoration, 320px reflow, 400% zoom equivalent, text spacing, reduced motion, and forced colors. Assert protocol/log updates never flood a live region.

- [ ] **Step 5: Document exact policies and smoke behavior**

`codex.md` records target 0.144.1, schema update/review procedure, Bash requirement, default approval/sandbox/network posture, timeouts, continuation behavior, version mismatch recovery, and real smoke command. `provider-tools.md` records exact schemas, mutation/scope rules, idempotency, error/result formats, and credential isolation for both tools. `security.md` states that trusted hooks/permissive Codex policy can access host-account data and that Symphony is not a VM boundary.

- [ ] **Step 6: Add deterministic CI and opt-in real smoke**

The required Windows/macOS workflow builds/runs the fake profiles. The integration workflow runs real Codex only when `SYMPHONY_REAL_CODEX_SMOKE` and its isolated workflow path are supplied; missing enablement or auth emits one explicit `SKIPPED: real Codex smoke` annotation and exits success. Once enabled, any failure exits nonzero.

- [ ] **Step 7: Run the phase acceptance gate**

```bash
gofmt -w internal/codex internal/app tests/conformance
go test ./...
go test -race ./internal/codex ./internal/orchestrator ./internal/app
go vet ./...
npm ci
npm run html:validate
npm run test:a11y
node scripts/a11y-scan-all.mjs
git diff --check
```

On Windows CI also run native Job Object/Bash tests; on macOS CI run native process-group tests and the race suite.

- [ ] **Step 8: Commit Phase 4 completion**

```bash
git add go/internal/codex go/internal/app go/tests go/docs go/README.md .github/workflows/go.yml .github/workflows/go-integrations.yml
git commit -m "test(go): prove the Codex and provider tool slice"
```

## Phase 4 Acceptance

From `go/`, every deterministic command above passes. A compatible fake app-server drives an issue through safe workspace launch, operator requests, provider tool execution, continuation, tracker recheck, and normal release; every timeout/error/cancellation path terminates finitely and is visible. Native CI proves full process-tree termination on Windows and macOS. A GitHub child receives no GitHub/Linear secret and can mutate only its captured current Issue through `github_api`; a Linear child receives no secret and can call only its captured `linear_graphql` endpoint. The real Codex profile reports either an actual result or explicit `SKIPPED`, never a synthetic pass.
