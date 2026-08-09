# Symphony Accessible Cross-Platform Implementation Roadmap

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver the approved local, accessible Symphony application as a Go implementation in the fork, with GitHub Issues and Linear tracking on Windows and macOS.

**Architecture:** Add a new `go/` implementation beside the existing `elixir/` reference implementation. A single Go process owns workflow configuration, one provider-neutral orchestrator, local workspaces and Codex child processes, and a loopback-only server-rendered web UI; the five phase plans each leave a working vertical slice and share the interfaces recorded below.

**Tech Stack:** Go 1.26.5; standard-library `net/http`, `html/template`, `log/slog`, `embed`, and `crypto`; `go.yaml.in/yaml/v3` 3.0.5; `github.com/osteele/liquid` 1.8.1; `github.com/fsnotify/fsnotify` 1.10.1; `github.com/zalando/go-keyring` 0.2.8; `github.com/gofrs/flock` 0.13.0; `github.com/vektah/gqlparser/v2` 2.5.36; `github.com/santhosh-tekuri/jsonschema/v6` 6.0.2; `golang.org/x/sys` 0.47.0; `golang.org/x/vuln/cmd/govulncheck` 1.6.0; Node 24.18.0; Playwright 1.62.1; axe-core Playwright 4.12.1; html-validate 11.6.2; a11y-check-web CLI 0.3.1.

## Global Constraints

- Governing service contract: repository-root `SPEC.md` whose last modifying commit is `3c372fa1f32a4d573a7bb9fa0cc101e16add63c3`.
- Governing product design: `docs/superpowers/specs/2026-08-06-symphony-accessible-cross-platform-design.md`.
- Supported hosts: Windows 11 and macOS 14 or later; Linux support is not implemented or claimed.
- Primary assistive-technology matrix: current stable NVDA with current stable Chrome on Windows; VoiceOver with current stable Safari on macOS.
- Accessibility target: every rendered route and state conforms to all applicable WCAG 2.2 Level A and AA success criteria.
- Runtime accessibility automation: Playwright plus axe; A11yNow is an optional independent runtime scanner.
- Pre-commit accessibility gate: `a11y-check-web scan --no-update-baseline`; the hook never changes the committed baseline.
- UI transport: loopback HTTP only, packaged local assets only, no wildcard/LAN bind option.
- Multiple instances are supported: one workflow and one GitHub repository or Linear project per process; different workflows can run concurrently; a canonical workflow path cannot be orchestrated twice.
- Secrets: macOS Keychain and Windows Credential Manager; no literal credential written by the UI, logged, returned over HTTP, or inherited by Codex.
- Codex target: CLI/app-server 0.144.1 with a committed generated schema manifest; protocol schema wins over hand-written assumptions.
- Codex launch: `bash -lc <codex.command>` from the validated issue workspace on both platforms; Windows requires Git for Windows Bash.
- UI implementation: server-rendered semantic HTML, ordinary forms and links without JavaScript, then minimal JavaScript for SSE and focus-safe enhancements.
- Visual tokens: `#090e15`, `#0d1420`, `#121b29`, `#182436`, `#304159`, `#47607f`, `#f4f7fb`, `#b1bfd2`, `#65dcc7`, `#4fc6b1`, `#ffd166`, `#83e7a3`, and `#ff929a` as specified in the approved design; rendered contrast is tested rather than inferred from tokens.
- Delivery discipline: red-green-refactor, one independently reviewable commit per task, no changes to `elixir/` unless a root-level integration test proves the Go work broke an existing repository contract.

---

## Plan Set and Order

Execute these plans in order. A phase begins only after the preceding phase's acceptance commands pass on its current commit.

1. `2026-08-06-symphony-phase-1-shell-configuration.md`
   - Creates the Go module, CLI/configuration mode, workflow parser/editor/watcher, OS-vault boundary, local browser authorization, accessible dark shell, runtime axe tests, and source-scan hook.
2. `2026-08-06-symphony-phase-2-trackers-observability.md`
   - Adds the provider-neutral issue model, GitHub and Linear read adapters, logs/events/snapshots, queue routes, and SSE updates.
3. `2026-08-06-symphony-phase-3-orchestrator-workspaces.md`
   - Adds deterministic safe workspaces, host hooks, the single-owner scheduler, retries, reconciliation, lifecycle controls, and fake-worker conformance.
4. `2026-08-06-symphony-phase-4-codex-tools.md`
   - Adds the pinned Codex JSONL client, worker turn loop, process-tree cancellation, operator requests, and scoped provider-native tools.
5. `2026-08-06-symphony-phase-5-conformance-release.md`
   - Closes upstream traceability, security/failure-injection coverage, full WCAG automation and manual scripts, real-integration profiles, cross-platform CI, and operator documentation.

## Repository Placement

Existing root files remain authoritative:

```text
SPEC.md
README.md
LICENSE
NOTICE
elixir/                         existing reference implementation; do not refactor
docs/superpowers/specs/         approved product design
docs/superpowers/plans/         this roadmap and phase plans
```

The new implementation has this stable top-level structure:

```text
go/
  AGENTS.md                     Go-specific contribution and verification rules
  README.md                     operator/developer entry point
  go.mod, go.sum                pinned Go module and dependencies
  mise.toml                     local Go/Node toolchain pins
  package.json, package-lock.json
  cmd/symphony/                 production CLI
  internal/app/                 process composition and UI-facing services
  internal/buildinfo/           version and schema compatibility metadata
  internal/cli/                 argument parsing and exit behavior
  internal/domain/              provider/protocol-neutral state types
  internal/workflow/            WORKFLOW.md load, resolve, render, edit, watch
  internal/secrets/             native credential-vault boundary
  internal/instance/            stable identity, data directory, and lock
  internal/tracker/             adapter contract and provider config decoding
  internal/tracker/github/      GitHub Issues implementation
  internal/tracker/linear/      Linear implementation
  internal/observability/       redacted logs, events, snapshots, and accounting
  internal/workspace/           safe directories and lifecycle hooks
  internal/orchestrator/        single-authority scheduling state machine
  internal/codex/               app-server protocol and child lifecycle
  internal/web/                 authorization, API, HTML handlers, and view models
  schema/codex/embed.go         package-local embed owner for protocol schemas
  schema/codex/0.144.1/         generated protocol schema snapshot and manifest
  web/embed.go                  embedded template/static filesystem
  web/templates/                semantic server-rendered documents and partials
  web/static/                   dark CSS and minimal progressive enhancement
  tests/accessibility/          Playwright/axe route and interaction suites
  tests/conformance/            traceability manifest and release evidence checks
  testdata/                     workflows and protocol/provider fixtures
  scripts/                      cross-platform verification and pre-commit helpers
  docs/                         Go operator, security, and accessibility guides
  .a11y/                        a11y-check-web config, findings, and reviewed baseline
.githooks/pre-commit            repository hook entry point for changed Go web files
.github/workflows/go.yml        Windows/macOS deterministic Go and browser checks
.github/workflows/go-integrations.yml
                                explicitly enabled credentialed smoke profiles
```

## Cross-Phase Interface Registry

These names prevent phase plans from inventing incompatible boundaries. A phase may add fields required by its own plan, but it must not rename these exported contracts without updating every later plan before implementation.

### Workflow and provider configuration

```go
package workflow

type Snapshot struct {
    Path       string
    Source     string
    Digest     string
    Definition Definition
    Config     EffectiveConfig
    LoadedAt   time.Time
}

type Definition struct {
    FrontMatter *yaml.Node
    Prompt       string
}

type EffectiveConfig struct {
    Tracker   TrackerConfig
    Polling   PollingConfig
    Workspace WorkspaceConfig
    Hooks     HooksConfig
    Agent     AgentConfig
    Codex     CodexConfig
    Server    ServerConfig
}

type Store interface {
    Current() (Snapshot, bool)
    Load(context.Context) (Snapshot, error)
    Validate(context.Context, []byte) ValidationResult
    Save(context.Context, SaveCommand) (Snapshot, error)
    Changes() <-chan Change
}
```

```go
package tracker

type Adapter interface {
    Kind() string
    FetchIssuesByStates(context.Context, []string) ([]domain.Issue, error)
    FetchIssuesByIDs(context.Context, []string) ([]domain.Issue, error)
    AgentTools(Session) []domain.ToolSpec
    ExecuteAgentTool(context.Context, domain.ToolCall, Session) domain.ToolResult
    SecretEnvironmentNames() []string
}

type Factory interface {
    Build(context.Context, workflow.TrackerConfig, secrets.Resolver) (Adapter, error)
}
```

### Runtime domain

```go
package domain

type Issue struct {
    ID           string
    NativeRef    map[string]any
    Identifier   string
    Title        string
    Description  *string
    Priority     *int
    State        string
    BranchName   *string
    URL          *string
    AssigneeID   *string
    Labels       []string
    BlockedBy    []BlockerRef
    Dispatchable bool
    CreatedAt    *time.Time
    UpdatedAt    *time.Time
}

type Snapshot struct {
    GeneratedAt time.Time
    EventCursor EventCursor
    Scheduler   SchedulerStatus
    Running     []RunningRow
    Retrying    []RetryRow
    Requests    []OperatorRequest
    CodexTotals TokenTotals
    RateLimits  map[string]any
    Config      ConfigStatus
}

type Event struct {
    Epoch    string
    Sequence uint64
    Type     string
    At       time.Time
    Data     map[string]any
}
```

### UI-facing runtime boundary

```go
package app

type RuntimeQueries interface {
    Snapshot(context.Context) (domain.Snapshot, error)
    Issue(context.Context, string) (domain.IssueDetail, error)
    EventsAfter(context.Context, domain.EventCursor) (domain.EventPage, error)
}

type RuntimeCommands interface {
    Refresh(context.Context) (domain.RefreshReceipt, error)
    SetScheduler(context.Context, bool) error
    Respond(context.Context, domain.OperatorResponse) error
}
```

Phase 2 supplies a read-only queue runtime; Phase 3 replaces it with the orchestrator without changing web handlers; Phase 4 wires `Respond` to the Codex request broker.

### Workspace and agent execution

```go
package workspace

type Manager interface {
    Ensure(context.Context, domain.Issue, workflow.EffectiveConfig) (domain.Workspace, error)
    RunHook(context.Context, domain.Hook, domain.Workspace) error
    Remove(context.Context, domain.Issue, workflow.EffectiveConfig) error
}
```

```go
package orchestrator

type Worker interface {
    Run(context.Context, RunRequest, func(domain.AgentEvent)) domain.RunResult
}

type RunRequest struct {
    Issue    domain.Issue
    Attempt  *int
    Workflow workflow.Snapshot
}

type AgentAttempt interface {
    Run(context.Context, AgentAttemptRequest, func(domain.AgentEvent)) domain.RunResult
}

type AgentAttemptRequest struct {
    Issue     domain.Issue
    Attempt   *int
    Workspace domain.Workspace
    Workflow  workflow.Snapshot
    Prompt    string
}

type Clock interface {
    Now() time.Time
    After(time.Duration) <-chan time.Time
    NewTimer(time.Duration) Timer
}
```

## Cross-Phase Verification Commands

Every task runs its focused command. Every phase ends with these deterministic gates from `go/`:

```bash
go test ./...
go test -race ./...             macOS and other supported race-detector hosts
go vet ./...
npm ci
npm run html:validate
npm run test:a11y
node scripts/a11y-scan-all.mjs
```

`node scripts/a11y-scan-all.mjs` invokes `a11y-check-web scan --repo-root <absolute-go-directory> --no-update-baseline --format text` with `A11Y_ALLOWED_ROOTS` set to that same directory. Exit 1 and exit 2 both fail.

The Windows and macOS CI jobs execute the same commands. A real tracker/Codex profile is a separate explicit job and reports `SKIPPED` when its enable flag or credential set is absent.

## Commit and Review Discipline

- Start each task by recording the focused failing test output.
- Define every helper used by a shown test in that task's named `_test.go` or `testkit_test.go` before running the red test; helper names in snippets are not assumed framework functions.
- Keep generated files in the same commit as the source/configuration that produces them.
- Run `git diff --check` before every commit.
- Stage only paths named by the task.
- Use the exact task commit subject so later review can map commits back to the plan.
- Review the task's diff and focused result before starting the next task.
- At each phase boundary, compare the current implementation against the approved design and the corresponding `SPEC.md` Section 17 rows; green broad tests do not replace that trace.
