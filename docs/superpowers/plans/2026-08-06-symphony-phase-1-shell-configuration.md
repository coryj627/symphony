# Symphony Phase 1: Accessible Shell and Configuration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Produce a runnable Windows/macOS Go binary whose protected loopback UI provides an accessible dark application shell, native-vault credential setup, and lossless validation/editing of `WORKFLOW.md` without starting tracker polling or agents.

**Architecture:** Create a new `go/` implementation beside `elixir/`. The CLI composes a workflow store, provider-profile validator, native secret store, instance identity/lock, and standard-library HTTP server; server-rendered HTML works without JavaScript, while Playwright/axe and a11y-check-web establish accessibility gates before orchestration is introduced.

**Tech Stack:** Go 1.26.5; `net/http`; `html/template`; `embed`; `go.yaml.in/yaml/v3` 3.0.5; `github.com/osteele/liquid` 1.8.1; `github.com/fsnotify/fsnotify` 1.10.1; `github.com/zalando/go-keyring` 0.2.8; `github.com/gofrs/flock` 0.13.0; Node 24.18.0; Playwright 1.62.1; `@axe-core/playwright` 4.12.1; html-validate 11.6.2; a11y-check-web 0.3.1.

## Global Constraints

- Use `SPEC.md` at commit `3c372fa1f32a4d573a7bb9fa0cc101e16add63c3` and `docs/superpowers/specs/2026-08-06-symphony-accessible-cross-platform-design.md` as governing contracts.
- Support Windows 11 and macOS 14 or later; add no Linux behavior or release claim.
- Run mode fails nonzero when the selected/default workflow is missing or invalid; `configure` mode can create or repair it and never starts the scheduler.
- Bind the UI only to loopback; serve every script, style, font, icon, and image locally.
- Store tracker credentials through macOS Keychain or Windows Credential Manager; never render, log, persist in workflow text, or return an existing credential.
- Preserve YAML comments, key order, scalar style, unknown keys, and the Markdown prompt when structured fields are saved.
- Use native HTML controls and semantic landmarks; all shell/configuration states must satisfy the WCAG ledger in the approved design.
- All changes follow red-green-refactor and each task ends with the exact commit shown.

---

### Task 1: Go module, CLI contract, and repository entry points

**Files:**
- Create: `go/go.mod`
- Create: `go/mise.toml`
- Create: `go/AGENTS.md`
- Create: `go/README.md`
- Create: `go/internal/buildinfo/buildinfo.go`
- Create: `go/internal/cli/options.go`
- Create: `go/internal/cli/options_test.go`
- Create: `go/internal/cli/run.go`
- Create: `go/internal/cli/run_test.go`
- Create: `go/cmd/symphony/main.go`
- Modify: `README.md`
- Modify: `.gitignore`

**Interfaces:**
- Produces: `cli.Options`, `cli.Parse(args []string) (Options, error)`, and `cli.Run(ctx context.Context, args []string, stdout, stderr io.Writer) int`.
- Produces: `buildinfo.Version`, `buildinfo.CodexVersion`, and `buildinfo.SpecCommit` values used by status/UI and protocol preflight; only `Version` is a link-time-overridable string variable.
- Consumes: no new application interface; `Run` uses an injected package-level `start` function in tests until Task 9 supplies production composition.

- [ ] **Step 1: Add the pinned module and a failing CLI parser test**

```go
// go/internal/cli/options_test.go
func TestParseDefaultsToRunAndCurrentWorkflow(t *testing.T) {
    got, err := Parse(nil)
    if err != nil {
        t.Fatal(err)
    }
    if got.Mode != ModeRun || got.WorkflowPath != "./WORKFLOW.md" || got.Port != 0 || got.PortSet {
        t.Fatalf("unexpected defaults: %#v", got)
    }
}

func TestParseConfigureAndOverrides(t *testing.T) {
    got, err := Parse([]string{"configure", "C:/work/WORKFLOW.md", "--port", "43127", "--data-dir", "C:/state", "--open"})
    if err != nil {
        t.Fatal(err)
    }
    if got.Mode != ModeConfigure || got.WorkflowPath != "C:/work/WORKFLOW.md" || got.Port != 43127 || !got.PortSet || !got.OpenBrowser {
        t.Fatalf("unexpected options: %#v", got)
    }
}
```

```go
// go/go.mod
module github.com/coryj627/symphony/go

go 1.26.0

toolchain go1.26.5
```

- [ ] **Step 2: Run the focused test and record the expected failure**

Run from `go/`:

```bash
mise trust
mise install
mise exec -- go test ./internal/cli -run 'TestParse' -v
```

Expected: compilation fails because `Parse`, `ModeRun`, and `ModeConfigure` do not exist.

- [ ] **Step 3: Implement deterministic parsing and exit behavior**

```go
// go/internal/cli/options.go
type Mode string

const (
    ModeRun       Mode = "run"
    ModeConfigure Mode = "configure"
)

type Options struct {
    Mode          Mode
    WorkflowPath  string
    Port          int
    PortSet       bool
    DataDir       string
    OpenBrowser   bool
}

func Parse(args []string) (Options, error) {
    opts := Options{Mode: ModeRun, WorkflowPath: "./WORKFLOW.md"}
    if len(args) > 0 && args[0] == "configure" {
        opts.Mode = ModeConfigure
        args = args[1:]
    }
    // Parse one optional positional workflow path and --port/--data-dir/--open
    // with a flag.FlagSet configured for ContinueOnError. A custom port Value sets
    // PortSet even for --port 0. Reject extra positionals, ports outside 0..65535,
    // and an empty explicit workflow path.
    return parseFlagSet(opts, args)
}
```

`cli.Run` returns `2` for argument errors, `1` for startup/runtime failure, and `0` only after a clean context-driven shutdown. `cmd/symphony/main.go` calls `os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))`.

- [ ] **Step 4: Add version pins and repository documentation**

```go
// go/internal/buildinfo/buildinfo.go
var Version = "0.1.0-dev"

const (
    CodexVersion = "0.144.1"
    SpecCommit   = "3c372fa1f32a4d573a7bb9fa0cc101e16add63c3"
)
```

Set `go/mise.toml` to Go `1.26.5` and Node `24.18.0`. In root `README.md`, add “Accessible Go implementation” beside the Elixir option and link `go/README.md`. In `.gitignore`, add `go/bin/`, `go/node_modules/`, `go/playwright-report/`, `go/test-results/`, `go/coverage/`, and `go/testdata/manual/WORKFLOW.md`.

- [ ] **Step 5: Run focused and repository-safe checks**

```bash
mise exec -- gofmt -w cmd internal
mise exec -- go test ./internal/cli -v
mise exec -- go test ./...
git diff --check
```

Expected: all Go tests pass; the existing `elixir/` tree is unchanged.

- [ ] **Step 6: Commit the foundation**

```bash
git add .gitignore README.md go/go.mod go/mise.toml go/AGENTS.md go/README.md go/cmd go/internal/buildinfo go/internal/cli
git commit -m "feat(go): establish Symphony CLI foundation"
```

---

### Task 2: Workflow parsing, typed defaults, and strict prompt rendering

**Files:**
- Create: `go/internal/workflow/types.go`
- Create: `go/internal/workflow/errors.go`
- Create: `go/internal/workflow/loader.go`
- Create: `go/internal/workflow/loader_test.go`
- Create: `go/internal/workflow/config.go`
- Create: `go/internal/workflow/config_test.go`
- Create: `go/internal/workflow/template.go`
- Create: `go/internal/workflow/template_test.go`
- Create: `go/testdata/workflows/valid-linear.md`
- Create: `go/testdata/workflows/prompt-only.md`
- Modify: `go/go.mod`
- Create: `go/go.sum`

**Interfaces:**
- Produces: `workflow.Parse(path string, source []byte) (Definition, error)`.
- Produces: `workflow.Resolve(path string, Definition, LookupEnv) (EffectiveConfig, error)` where `type LookupEnv func(string) (string, bool)`.
- Produces: `workflow.Load(path string, LookupEnv) (Snapshot, error)` and the exact `Snapshot`/`EffectiveConfig` types in the roadmap.
- Produces: `workflow.Render(def Definition, issue domain.Issue, attempt *int) (string, error)`; define the minimal `workflow.TemplateIssue` inside this task so `domain.Issue` is introduced only in Phase 2.

- [ ] **Step 1: Write parsing and typed-error tests**

```go
func TestParseFrontMatterAndPrompt(t *testing.T) {
    source := []byte("---\npolling:\n  interval_ms: 15000\n---\nWork on {{ issue.identifier }}.\n")
    got, err := Parse("/repo/WORKFLOW.md", source)
    if err != nil {
        t.Fatal(err)
    }
    if got.Prompt != "Work on {{ issue.identifier }}." || got.FrontMatter.Kind != yaml.DocumentNode {
        t.Fatalf("unexpected definition: %#v", got)
    }
}

func TestParseRejectsNonMapFrontMatter(t *testing.T) {
    _, err := Parse("WORKFLOW.md", []byte("---\n- bad\n---\nprompt"))
    if !errors.Is(err, ErrFrontMatterNotMap) {
        t.Fatalf("got %v", err)
    }
}

func TestLoadMissingFileIsTyped(t *testing.T) {
    _, err := Load(filepath.Join(t.TempDir(), "WORKFLOW.md"), os.LookupEnv)
    if !errors.Is(err, ErrMissingWorkflow) {
        t.Fatalf("got %v", err)
    }
}
```

- [ ] **Step 2: Run parser tests and confirm they fail**

```bash
mise exec -- go test ./internal/workflow -run 'Test(Parse|Load)' -v
```

Expected: compilation fails because the workflow package has no implementation.

- [ ] **Step 3: Implement the lossless parser and error taxonomy**

```go
var (
    ErrMissingWorkflow    = errors.New("missing_workflow_file")
    ErrWorkflowParse      = errors.New("workflow_parse_error")
    ErrFrontMatterNotMap  = errors.New("workflow_front_matter_not_a_map")
    ErrTemplateParse      = errors.New("template_parse_error")
    ErrTemplateRender     = errors.New("template_render_error")
)

type Definition struct {
    FrontMatter *yaml.Node
    Prompt       string
}
```

Parse only a leading `---` block, require its YAML document content to be a mapping, retain the `yaml.Node`, trim only the prompt returned for rendering, and retain the full original source in `Snapshot.Source`. Wrap errors with path and line/column while keeping `errors.Is` stable.

- [ ] **Step 4: Write defaults, path, and environment-resolution tests**

```go
func TestResolveAppliesCoreDefaultsAndTargetedExpansion(t *testing.T) {
    def, err := Parse("/repo/WORKFLOW.md", []byte("---\ntracker:\n  kind: github\n  provider:\n    repo: coryj627/symphony\nworkspace:\n  root: $WORK_ROOT\ncodex:\n  command: 'codex app-server --config $UNCHANGED'\n---\nPrompt"))
    if err != nil { t.Fatal(err) }
    env := func(key string) (string, bool) {
        if key == "WORK_ROOT" { return "/safe/work", true }
        return "", false
    }
    got, err := Resolve("/repo/WORKFLOW.md", def, env)
    if err != nil { t.Fatal(err) }
    if got.Polling.Interval != 30*time.Second || got.Agent.MaxConcurrent != 10 || got.Agent.MaxTurns != 20 {
        t.Fatalf("defaults not applied: %#v", got)
    }
    if got.Workspace.Root != filepath.Clean("/safe/work") || got.Codex.Command != "codex app-server --config $UNCHANGED" {
        t.Fatalf("bad expansion: %#v", got)
    }
}
```

Also test relative workspace roots against the workflow directory, `~`, invalid numeric values, normalized per-state limits, server port `0..65535`, operator response windows of at least 30 seconds, and unknown-key preservation.

- [ ] **Step 5: Implement the exact config types and defaults**

```go
type PollingConfig struct{ Interval time.Duration }
type WorkspaceConfig struct{ Root string }
type HooksConfig struct {
    AfterCreate, BeforeRun, AfterRun, BeforeRemove string
    Timeout time.Duration
}
type AgentConfig struct {
    MaxConcurrent int
    MaxTurns int
    MaxRetryBackoff time.Duration
    MaxConcurrentByState map[string]int
}
type CodexConfig struct {
    Command string
    ApprovalPolicy any
    ThreadSandbox string
    TurnSandboxPolicy map[string]any
    TurnTimeout, ReadTimeout, StallTimeout time.Duration
}
type ServerConfig struct {
    Port int
    OperatorResponseWindow time.Duration
}
```

Use upstream defaults: polling 30 seconds, hook timeout 60 seconds, max concurrent 10, max turns 20, max retry backoff 5 minutes, command `codex app-server`, turn timeout 1 hour, read timeout 5 seconds, stall timeout 5 minutes, server port 0, and operator response window 10 minutes. Preserve Codex policy maps as pass-through JSON-safe values.

- [ ] **Step 6: Write and implement strict Liquid rendering**

```go
func TestRenderRejectsUnknownVariableAndFilter(t *testing.T) {
    def := Definition{Prompt: "{{ issue.missing | no_such_filter }}"}
    _, err := Render(def, TemplateIssue{Identifier: "GH-1", Title: "Test"}, nil)
    if !errors.Is(err, ErrTemplateRender) && !errors.Is(err, ErrTemplateParse) {
        t.Fatalf("got %v", err)
    }
}
```

```go
func Render(def Definition, issue TemplateIssue, attempt *int) (string, error) {
    engine := liquid.NewEngine()
    engine.StrictVariables()
    template, err := engine.ParseString(def.Prompt)
    if err != nil { return "", fmt.Errorf("%w: %v", ErrTemplateParse, err) }
    out, err := template.Render(liquid.Bindings{"issue": issue.Bindings(), "attempt": attempt})
    if err != nil { return "", fmt.Errorf("%w: %v", ErrTemplateRender, err) }
    return string(out), nil
}
```

Do not call `LaxFilters`; the pinned Liquid engine rejects unknown filters by default. Implement the configured-tracker fallback prompt when the Markdown body is blank.

- [ ] **Step 7: Run workflow tests and commit**

```bash
mise exec -- gofmt -w internal/workflow
mise exec -- go test ./internal/workflow -v
mise exec -- go test ./...
git diff --check
git add go/go.mod go/go.sum go/internal/workflow go/testdata/workflows
git commit -m "feat(go): parse and resolve Symphony workflows"
```

---

### Task 3: GitHub and Linear provider-profile configuration

**Files:**
- Create: `go/internal/tracker/config.go`
- Create: `go/internal/tracker/config_test.go`
- Create: `go/internal/tracker/errors.go`
- Modify: `go/internal/workflow/types.go`

**Interfaces:**
- Consumes: `workflow.TrackerConfig{Kind string, Provider map[string]any, RequiredLabels, ActiveStates, TerminalStates []string}` from Task 2.
- Produces: `tracker.DecodeConfig(workflow.TrackerConfig) (tracker.ProviderConfig, error)`.
- Produces: concrete `tracker.GitHubConfig` and `tracker.LinearConfig`, each implementing `Kind() string`, `Credential() tracker.CredentialSpec`, and `SecretEnvironmentNames() []string`.

- [ ] **Step 1: Write provider decoding tests**

```go
func TestDecodeGitHubConfig(t *testing.T) {
    raw := workflow.TrackerConfig{
        Kind: "github",
        Provider: map[string]any{
            "owner": "coryj627", "repository": "symphony",
            "endpoint": "https://api.github.com", "credential_ref": "os-vault",
            "assignee": "coryj627",
        },
        ActiveStates: []string{"open"}, TerminalStates: []string{"closed"},
    }
    cfg, err := DecodeConfig(raw)
    if err != nil { t.Fatal(err) }
    got := cfg.(GitHubConfig)
    if got.Owner != "coryj627" || got.Repository != "symphony" || got.CredentialRef != "os-vault" {
        t.Fatalf("unexpected config: %#v", got)
    }
}

func TestDecodeLinearRejectsMissingProject(t *testing.T) {
    _, err := DecodeConfig(workflow.TrackerConfig{Kind: "linear", Provider: map[string]any{}})
    if !errors.Is(err, ErrInvalidTrackerConfig) { t.Fatalf("got %v", err) }
}
```

Include table rows for unsupported kind, non-HTTPS endpoint, invalid GitHub owner/repository, GitHub states outside `open`/`closed`, missing Linear project slug, `$VAR` credential reference, blank required label, and stable field paths for UI errors.

- [ ] **Step 2: Run the focused tests and confirm failure**

```bash
mise exec -- go test ./internal/tracker -run 'TestDecode' -v
```

Expected: compilation fails because `DecodeConfig` and provider config types do not exist.

- [ ] **Step 3: Implement explicit provider config types**

```go
type CredentialSpec struct {
    Reference string
    EnvName   string
}

type ProviderConfig interface {
    Kind() string
    Credential() CredentialSpec
    SecretEnvironmentNames() []string
}

type GitHubConfig struct {
    Owner, Repository, Endpoint, CredentialRef, CredentialEnv, Assignee string
}

type LinearConfig struct {
    ProjectSlug, Endpoint, CredentialRef, CredentialEnv string
}
```

GitHub defaults endpoint to `https://api.github.com`, credential env to `GITHUB_TOKEN`, active states to `open`, and terminal states to `closed`. Linear defaults endpoint to `https://api.linear.app/graphql`, credential env to `LINEAR_API_KEY`, active states to `Todo`/`In Progress`, and terminal states to `Closed`/`Cancelled`/`Canceled`/`Duplicate`/`Done`. `os-vault` and `$NAME` are the only credential references the UI writes/recognizes.

- [ ] **Step 4: Run tests and commit**

```bash
mise exec -- gofmt -w internal/tracker internal/workflow/types.go
mise exec -- go test ./internal/tracker ./internal/workflow -v
git diff --check
git add go/internal/tracker go/internal/workflow/types.go
git commit -m "feat(go): validate GitHub and Linear profiles"
```

---

### Task 4: Native credential-vault boundary

**Files:**
- Create: `go/internal/secrets/store.go`
- Create: `go/internal/secrets/ref.go`
- Create: `go/internal/secrets/ref_test.go`
- Create: `go/internal/secrets/keyring.go`
- Create: `go/internal/secrets/keyring_test.go`
- Create: `go/internal/secrets/fake_test.go`
- Create: `go/internal/secrets/live_keyring_test.go`
- Modify: `go/go.mod`
- Modify: `go/go.sum`

**Interfaces:**
- Produces: `secrets.Store` with `Put`, `Get`, `Delete`, and `Status` signatures from the approved design.
- Produces: `secrets.Ref{WorkflowID, TrackerKind string}`, `secrets.Resolver`, and `secrets.NewKeyring(servicePrefix string) Store`.
- Consumes: canonical workflow ID as an opaque string; Task 5 creates it. Credential identity deliberately does not depend on tracker scope or the configuration-mode data directory.

- [ ] **Step 1: Write key derivation and non-disclosure tests**

```go
func TestRefUsesStableNonSecretNames(t *testing.T) {
    ref := Ref{WorkflowID: "c3a1", TrackerKind: "github"}
    if ref.Service() != "symphony/workflow/c3a1" || ref.Account() != "tracker/github" {
        t.Fatalf("unexpected keyring names: %q %q", ref.Service(), ref.Account())
    }
}

func TestStatusNeverContainsCredential(t *testing.T) {
    fake := NewMemoryStore()
    ref := Ref{WorkflowID: "w", TrackerKind: "linear"}
    if err := fake.Put(context.Background(), ref, []byte("secret-canary")); err != nil { t.Fatal(err) }
    got := fake.Status(context.Background(), ref)
    if !got.Present || strings.Contains(fmt.Sprintf("%+v", got), "secret-canary") {
        t.Fatalf("unsafe status: %#v", got)
    }
}
```

- [ ] **Step 2: Run focused tests and confirm failure**

```bash
mise exec -- go test ./internal/secrets -v
```

Expected: compilation fails because the secret-store types do not exist.

- [ ] **Step 3: Implement store and keyring adapters**

```go
type Store interface {
    Put(context.Context, Ref, []byte) error
    Get(context.Context, Ref) ([]byte, error)
    Delete(context.Context, Ref) error
    Status(context.Context, Ref) Status
}

type Resolver interface {
    Resolve(context.Context, Ref, string) ([]byte, error)
}

type Status struct {
    Present bool
    Backend string
    ErrorCode string
}
```

Wrap `github.com/zalando/go-keyring` without adding a Linux fallback. Convert strings to copied byte slices only at the `Get` boundary, overwrite temporary byte slices after client construction, map not-found separately, and never include the value in wrapped errors.

- [ ] **Step 4: Add opt-in live native-vault tests**

`live_keyring_test.go` uses build tag `keyring_live`, creates a random service name using `symphony-test/` plus 32 random hexadecimal characters, writes/reads/deletes one canary, and registers `t.Cleanup` before the write. It skips unless `SYMPHONY_RUN_KEYRING_LIVE=1` and `runtime.GOOS` is `darwin` or `windows`.

```bash
SYMPHONY_RUN_KEYRING_LIVE=1 mise exec -- go test -tags=keyring_live ./internal/secrets -run TestLiveKeyringRoundTrip -v
```

Expected on a supported interactive host: PASS with the temporary credential deleted. Default unit runs do not touch the user's vault.

- [ ] **Step 5: Run deterministic tests and commit**

```bash
mise exec -- gofmt -w internal/secrets
mise exec -- go test ./internal/secrets -v
git diff --check
git add go/go.mod go/go.sum go/internal/secrets
git commit -m "feat(go): add native tracker credential store"
```

---

### Task 5: Instance identity, data directory, and duplicate lock

**Files:**
- Create: `go/internal/instance/identity.go`
- Create: `go/internal/instance/identity_test.go`
- Create: `go/internal/instance/lock.go`
- Create: `go/internal/instance/lock_test.go`
- Modify: `go/go.mod`
- Modify: `go/go.sum`

**Interfaces:**
- Produces: `instance.Resolve(workflowPath, trackerScope, explicitDataDir string) (instance.Info, error)`.
- Produces: `instance.Acquire(info Info) (*instance.Lock, error)` and `(*Lock).Release() error`.
- Produces: `Info{ID, WorkflowID, WorkflowPath, DataDir, LockPath string}` consumed by CLI, vault refs, logs, and web bootstrap.

- [ ] **Step 1: Write canonical identity and lock contention tests**

```go
func TestResolveIsStableAcrossSymlinkedWorkflowPath(t *testing.T) {
    root := t.TempDir()
    realPath := filepath.Join(root, "repo", "WORKFLOW.md")
    mustWriteWorkflow(t, realPath)
    link := filepath.Join(root, "workflow-link.md")
    mustSymlinkOrSkip(t, realPath, link)
    a, err := Resolve(realPath, "github:coryj627/symphony", "")
    if err != nil { t.Fatal(err) }
    b, err := Resolve(link, "github:coryj627/symphony", "")
    if err != nil { t.Fatal(err) }
    if a.ID != b.ID || a.WorkflowPath != b.WorkflowPath { t.Fatalf("identity mismatch: %#v %#v", a, b) }
}

func TestAcquireRejectsSecondOwner(t *testing.T) {
    root := t.TempDir()
    info := Info{ID: "same-instance", WorkflowID: "same-workflow", DataDir: filepath.Join(root, "data"), LockPath: filepath.Join(root, "locks", "same-workflow.lock")}
    first, err := Acquire(info)
    if err != nil { t.Fatal(err) }
    defer first.Release()
    if _, err := Acquire(info); !errors.Is(err, ErrAlreadyRunning) { t.Fatalf("got %v", err) }
}
```

- [ ] **Step 2: Run the tests and confirm failure**

```bash
mise exec -- go test ./internal/instance -v
```

Expected: compilation fails because `Resolve`, `Info`, and `Acquire` do not exist.

- [ ] **Step 3: Implement canonical ID and OS data-directory behavior**

Resolve the workflow through the deepest existing parent, evaluate symlinks, and append a missing leaf only in configuration mode. `WorkflowID` is the first 16 SHA-256 bytes of only that canonical workflow path; `ID` hashes canonical path plus NUL plus normalized tracker scope. Default `DataDir` is `filepath.Join(os.UserConfigDir(), "Symphony", "instances", ID)`; an explicit path is made absolute and cleaned. `LockPath` is always `filepath.Join(os.UserConfigDir(), "Symphony", "locks", WorkflowID+".lock")`, independent of tracker scope or explicit data directory, so two processes cannot orchestrate one canonical workflow through different instance settings.

```go
type Info struct {
    ID           string
    WorkflowID   string
    WorkflowPath string
    DataDir      string
    LockPath     string
}
```

Use `gofrs/flock` non-blocking acquisition on the resolved global `LockPath`. Write non-secret PID, start time, workflow ID, and workflow path to `DataDir/instance.json` only after acquiring the lock; truncate that metadata on release while releasing only the exact workflow lock.

- [ ] **Step 4: Run tests and commit**

```bash
mise exec -- gofmt -w internal/instance
mise exec -- go test ./internal/instance -v
git diff --check
git add go/go.mod go/go.sum go/internal/instance
git commit -m "feat(go): isolate Symphony process instances"
```

---

### Task 6: Protected loopback HTTP foundation

**Files:**
- Create: `go/internal/web/server.go`
- Create: `go/internal/web/server_test.go`
- Create: `go/internal/web/security_headers.go`
- Create: `go/internal/web/security_headers_test.go`
- Create: `go/internal/web/session.go`
- Create: `go/internal/web/session_test.go`
- Create: `go/internal/web/csrf.go`
- Create: `go/internal/web/csrf_test.go`
- Create: `go/internal/web/bootstrap_common.go`
- Create: `go/internal/web/bootstrap_random.go`
- Create: `go/internal/web/bootstrap_e2e.go`
- Create: `go/internal/web/routes.go`

**Interfaces:**
- Produces: `web.NewServer(web.Options) (*web.Server, error)`, `(*Server).Start(context.Context) (web.Bound, error)`, and `(*Server).Shutdown(context.Context) error`.
- Produces: `web.Options{Port int, Bootstrap Bootstrap, Handler http.Handler, Logger *slog.Logger}`.
- Produces: `web.Bound{URL string, Port int}`; URL includes the one-time bootstrap token only in the CLI output, never in logs after exchange.

- [ ] **Step 1: Write loopback, bootstrap, and cross-site rejection tests**

```go
func TestBootstrapExchangesCapabilityAndRedirectsCleanURL(t *testing.T) {
    srv := newHTTPTestServer(t, "known-capability")
    res := mustRequest(t, srv.Client(), http.MethodGet, srv.URL+"/?access_token=known-capability", nil, nil)
    if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
        t.Fatalf("unexpected response: %d %q", res.StatusCode, res.Header.Get("Location"))
    }
    cookie := sessionCookie(res.Cookies())
    if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Domain != "" {
        t.Fatalf("unsafe cookie: %#v", cookie)
    }
}

func TestMutationRejectsCrossSiteOrigin(t *testing.T) {
    req := authenticatedRequest(t, http.MethodPost, "/api/v1/config/validate", strings.NewReader("{}"))
    req.Header.Set("Origin", "https://attacker.example")
    res := serve(t, req)
    if res.Code != http.StatusForbidden { t.Fatalf("got %d", res.Code) }
}
```

Also test invalid/expired/reused capability, missing session, missing/wrong CSRF token, wildcard Host, unsupported content type/method, IPv4 and IPv6 loopback listeners, no-store caching, and absent token/cookie values in captured logs.

- [ ] **Step 2: Run focused tests and confirm failure**

```bash
mise exec -- go test ./internal/web -run 'Test(Bootstrap|Mutation|Security|Loopback)' -v
```

Expected: compilation fails because the web server and security middleware do not exist.

- [ ] **Step 3: Implement capability exchange and session-bound CSRF**

Generate 32 random bytes for bootstrap and session values. Store only SHA-256 digests and compare with `subtle.ConstantTimeCompare`. Expire bootstrap after first exchange or five minutes. Set `symphony_session` as host-only, `HttpOnly`, `SameSite=Strict`, `Path=/`; omit `Secure` because the loopback server is intentionally HTTP. Generate a per-session CSRF value and require it in form field `csrf_token` or `X-CSRF-Token`.

- [ ] **Step 4: Implement strict listener and headers**

Bind explicit `127.0.0.1:<port>` and, when available, `[::1]:<same-or-selected-port>` without using `:port`, `0.0.0.0`, or `[::]`. Serve one selected URL and keep both listeners under one shutdown group.

Set this policy on authenticated documents and API responses:

```text
default-src 'none'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'
```

Also set `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store`, and `Cross-Origin-Resource-Policy: same-origin`.

- [ ] **Step 5: Add e2e-only deterministic bootstrap**

Use build tags so production cannot read a fixed token:

```go
//go:build e2e

func NewBootstrap() (Bootstrap, error) {
    value := os.Getenv("SYMPHONY_E2E_BOOTSTRAP_TOKEN")
    if len(value) < 32 { return Bootstrap{}, errors.New("e2e bootstrap token must be at least 32 characters") }
    return bootstrapFromValue(value), nil
}
```

`bootstrap_random.go` begins with `//go:build !e2e`, always uses `crypto/rand`, and has no environment override. Shared digest/exchange types live in untagged `bootstrap_common.go`, so exactly one `NewBootstrap` implementation compiles for either build.

- [ ] **Step 6: Run tests and commit**

```bash
mise exec -- gofmt -w internal/web
mise exec -- go test ./internal/web -v
mise exec -- go test -tags=e2e ./internal/web -v
git diff --check
git add go/internal/web
git commit -m "feat(go): protect the loopback control surface"
```

---

### Task 7: Semantic application shell and approved dark theme

**Files:**
- Create: `go/web/embed.go`
- Create: `go/internal/web/viewmodels.go`
- Create: `go/internal/web/pages.go`
- Create: `go/internal/web/pages_test.go`
- Create: `go/web/templates/base.html`
- Create: `go/web/templates/partials/nav.html`
- Create: `go/web/templates/partials/flash.html`
- Create: `go/web/templates/overview.html`
- Create: `go/web/templates/issues.html`
- Create: `go/web/templates/issue.html`
- Create: `go/web/templates/activity.html`
- Create: `go/web/templates/configuration.html`
- Create: `go/web/templates/logs.html`
- Create: `go/web/templates/unauthorized.html`
- Create: `go/web/static/app.css`
- Create: `go/web/static/app.js`
- Create: `go/package.json`
- Create: `go/package-lock.json`
- Create: `go/playwright.config.mjs`
- Create: `go/tests/accessibility/fixtures.mjs`
- Create: `go/tests/accessibility/shell.spec.mjs`
- Create: `go/tests/accessibility/html-validity.spec.mjs`

**Interfaces:**
- Consumes: authenticated route middleware and embedded handler boundary from Task 6.
- Produces: `web.Page{Title, Route, Heading, Mode, Flash, CSRFToken, Content any}` and template renderer used by every later handler.
- Produces: stable HTML routes `/`, `/issues`, `/issues/{identifier}`, `/activity`, `/configuration`, and `/logs`.

- [ ] **Step 1: Write server-rendered landmark and route tests**

```go
func TestEveryPageHasUniqueTitleMainAndH1(t *testing.T) {
    cases := []struct{ path, title, heading string }{
        {"/", "Overview — Symphony", "Overview"},
        {"/issues", "Issues — Symphony", "Issues"},
        {"/activity", "Activity — Symphony", "Activity"},
        {"/configuration", "Configuration — Symphony", "Configuration"},
        {"/logs", "Logs — Symphony", "Logs"},
    }
    for _, tc := range cases {
        html := authenticatedGET(t, tc.path)
        assertContainsOnce(t, html, "<main id=\"main-content\"")
        assertContainsOnce(t, html, "<h1>")
        assertContains(t, html, "<title>"+tc.title+"</title>")
        assertContains(t, html, ">"+tc.heading+"</h1>")
    }
}
```

Also assert the skip link is the first focusable element, navigation order is stable, active navigation uses text plus `aria-current=page`, status has visible text, no event-handler attributes exist, and all forms contain the session CSRF token.

- [ ] **Step 2: Run page tests and confirm failure**

```bash
mise exec -- go test ./internal/web -run 'TestEveryPage|TestNavigation|TestSkip' -v
```

Expected: tests fail because the assets and page renderer do not exist.

- [ ] **Step 3: Implement embedded templates and no-JavaScript navigation**

```go
// go/web/embed.go
package webassets

import "embed"

//go:embed templates/*.html templates/partials/*.html static/*
var Files embed.FS
```

`base.html` begins with `<html lang="en">`, a visible-on-focus skip link, `<header>`, labelled `<nav>`, and `<main id="main-content" tabindex="-1">`. Each route receives one `<h1>`. Links and buttons are not nested, cards are not clickable containers, and empty/loading/error states are visible text.

- [ ] **Step 4: Implement the dark, reflow-safe CSS contract**

Declare every approved token in `:root`, set `color-scheme: dark`, use a system font stack, give interactive controls a minimum block/inline size of 44 CSS pixels, and use `outline: 3px solid #ffd166; outline-offset: 3px` for `:focus-visible`. Add tested breakpoints for 320 CSS pixels, `@media (prefers-reduced-motion: reduce)`, and `@media (forced-colors: active)`. Restrict horizontal scrolling to `.code-scroll`/`.log-scroll` regions.

- [ ] **Step 5: Add Playwright and axe shell tests**

```json
{
  "private": true,
  "engines": {"node": "24.18.0"},
  "scripts": {
    "test:a11y": "playwright test",
    "html:validate": "playwright test tests/accessibility/html-validity.spec.mjs"
  },
  "devDependencies": {
    "@axe-core/playwright": "4.12.1",
    "@playwright/test": "1.62.1",
    "html-validate": "11.6.2"
  }
}
```

```js
test('configuration shell has no axe A/AA violations and works by keyboard', async ({page}) => {
  await authorize(page, '/configuration');
  const results = await new AxeBuilder({page}).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa']).analyze();
  expect(results.violations).toEqual([]);
  await page.keyboard.press('Tab');
  await expect(page.getByRole('link', {name: 'Skip to main content'})).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('main')).toBeFocused();
});
```

The Playwright `webServer.command` is `go run -tags=e2e ./cmd/symphony configure ./testdata/workflows/valid-linear.md --port 43127`, with a fixed 64-character `SYMPHONY_E2E_BOOTSTRAP_TOKEN`; production builds never accept it. `html-validity.spec.mjs` authorizes each route, passes `await page.content()` to `new HtmlValidate().validateString(...)`, and asserts `valid=true`, so validation sees rendered HTML instead of Go template directives.

- [ ] **Step 6: Run Go, HTML, Chromium, and WebKit checks**

```bash
npm install
npx playwright install chromium webkit
mise exec -- go test ./internal/web ./web -v
npm run html:validate
npm run test:a11y -- --project=chromium
npm run test:a11y -- --project=webkit
git diff --check
```

Expected: all routes pass axe, skip-link keyboard flow passes, and HTML validation is clean.

- [ ] **Step 7: Commit the shell**

```bash
git add go/internal/web go/web go/package.json go/package-lock.json go/playwright.config.mjs go/tests/accessibility
git commit -m "feat(go): add accessible dark application shell"
```

---

### Task 8: Atomic workflow editor, conflict detection, and live reload

**Files:**
- Create: `go/internal/workflow/atomic.go`
- Create: `go/internal/workflow/atomic_test.go`
- Create: `go/internal/workflow/atomic_darwin.go`
- Create: `go/internal/workflow/atomic_darwin_test.go`
- Create: `go/internal/workflow/atomic_windows.go`
- Create: `go/internal/workflow/atomic_windows_test.go`
- Create: `go/internal/workflow/editor.go`
- Create: `go/internal/workflow/editor_test.go`
- Create: `go/internal/workflow/store.go`
- Create: `go/internal/workflow/store_test.go`
- Create: `go/internal/workflow/watcher.go`
- Create: `go/internal/workflow/watcher_test.go`
- Modify: `go/go.mod`
- Modify: `go/go.sum`

**Interfaces:**
- Consumes: `workflow.Parse`, `Resolve`, and provider-validation callback `func(EffectiveConfig) []FieldError`.
- Produces: the roadmap `workflow.Store` interface.
- Produces: `SaveCommand{BaseDigest string, RawSource []byte, Patch *StructuredPatch}` and `ValidationResult{Valid bool, FieldErrors []FieldError, GlobalErrors []SafeError}`.

- [ ] **Step 1: Write comment-preservation, conflict, and atomic-failure tests**

```go
func TestStructuredSavePreservesCommentsUnknownKeysAndPrompt(t *testing.T) {
    path := copyFixture(t, "commented-workflow.md")
    store := newTestStore(t, path)
    before, err := store.Load(context.Background())
    if err != nil { t.Fatal(err) }
    got, err := store.Save(context.Background(), SaveCommand{
        BaseDigest: before.Digest,
        Patch: &StructuredPatch{PollingIntervalMS: ptr(45000)},
    })
    if err != nil { t.Fatal(err) }
    if !strings.Contains(got.Source, "# keep this comment") || !strings.Contains(got.Source, "future_extension:") || got.Definition.Prompt != before.Definition.Prompt {
        t.Fatalf("lossy save:\n%s", got.Source)
    }
}

func TestSaveRejectsExternalChange(t *testing.T) {
    store, first := loadedStore(t)
    os.WriteFile(first.Path, []byte(first.Source+"\nexternal"), 0o600)
    _, err := store.Save(context.Background(), SaveCommand{BaseDigest: first.Digest, RawSource: []byte(first.Source)})
    if !errors.Is(err, ErrSaveConflict) { t.Fatalf("got %v", err) }
}
```

Inject failures at temp create, write, file sync, close, replace, and post-replace directory sync. Every failure before the atomic replace preserves the original byte-for-byte; a directory-sync failure after replace returns `ErrDurabilityUncertain` with the complete validated new file already visible. No fault may leave a missing or partial workflow. Test invalid reload retains `Current()` and publishes a change containing the safe validation error.

- [ ] **Step 2: Run focused tests and confirm failure**

```bash
mise exec -- go test ./internal/workflow -run 'Test(StructuredSave|SaveRejects|Atomic|Watcher|InvalidReload)' -v
```

Expected: compilation fails because editor/store/watcher types do not exist.

- [ ] **Step 3: Implement digest-checked AST patching and atomic replace**

Compute digest as lowercase SHA-256 of exact source bytes. Locate mapping keys directly in `yaml.Node.Content`, mutate existing scalar values in place, append absent supported keys without rebuilding unknown siblings, encode the original front-matter node, and concatenate the untouched prompt-body bytes. Raw saves parse/resolve/validate the entire candidate.

Atomic replace uses a restrictive same-directory temp file, write loop, file `Sync`, `Chmod` preserving the existing destination's permission bits without broadening them (new files use `0600`), close, platform rename, and parent directory sync on macOS. A post-replace directory-sync error is surfaced as durability-uncertain without rolling back the complete new file. On Windows, use the tested replace path in `atomic_windows.go`; never delete the destination before replacement.

- [ ] **Step 4: Implement one owner for last-known-good state and fsnotify reload**

`Store` owns current state behind a mutex, publishes coalesced `Change` values on a buffered channel, watches the parent directory rather than only the inode, debounces replace/write bursts for 100 ms, and defensively reloads before save and before later dispatch. An invalid event publishes `Valid=false` while leaving the last good snapshot unchanged.

- [ ] **Step 5: Run workflow tests and commit**

```bash
mise exec -- gofmt -w internal/workflow
mise exec -- go test ./internal/workflow -v
mise exec -- go test -race ./internal/workflow
git diff --check
git add go/go.mod go/go.sum go/internal/workflow
git commit -m "feat(go): edit and reload workflows safely"
```

---

### Task 9: Accessible configuration and credential workflows

**Files:**
- Create: `go/internal/app/config_service.go`
- Create: `go/internal/app/config_service_test.go`
- Create: `go/internal/web/config_handlers.go`
- Create: `go/internal/web/config_handlers_test.go`
- Modify: `go/internal/web/routes.go`
- Modify: `go/internal/web/viewmodels.go`
- Modify: `go/web/templates/configuration.html`
- Modify: `go/web/static/app.css`
- Modify: `go/tests/accessibility/shell.spec.mjs`
- Create: `go/tests/accessibility/configuration.spec.mjs`
- Modify: `go/internal/cli/run.go`
- Modify: `go/internal/cli/run_test.go`
- Modify: `go/cmd/symphony/main.go`
- Create: `go/testdata/manual/.gitkeep`

**Interfaces:**
- Consumes: workflow store, `tracker.DecodeConfig`, secret store, instance info/lock, protected server, and page renderer.
- Produces: `app.ConfigService` with `View`, `Validate`, `Save`, `CredentialStatus`, `ReplaceCredential`, and `DeleteCredential` methods.
- Produces: configuration endpoints `POST /api/v1/config/validate`, `POST /api/v1/config/save`, `POST /api/v1/config/credential`, and `POST /api/v1/config/credential/delete`.

- [ ] **Step 1: Write service and HTTP behavior tests**

```go
func TestInvalidSaveFocusesLinkedErrorSummaryWithoutChangingFile(t *testing.T) {
    app := configuredTestApp(t)
    before := mustRead(t, app.WorkflowPath)
    res := app.PostForm("/api/v1/config/save", url.Values{
        "csrf_token": {app.CSRF}, "base_digest": {app.Digest}, "raw_source": {"---\ntracker: []\n---\nPrompt"},
    })
    if res.Code != http.StatusUnprocessableEntity { t.Fatalf("got %d", res.Code) }
    assertContains(t, res.Body.String(), "id=\"error-summary\"")
    assertContains(t, res.Body.String(), "href=\"#raw-source\"")
    if got := mustRead(t, app.WorkflowPath); got != before { t.Fatal("invalid save changed workflow") }
}

func TestCredentialResponseExposesPresenceNotValue(t *testing.T) {
    app := configuredTestApp(t)
    res := app.PostForm("/api/v1/config/credential", validCredentialForm(app, "canary-secret"))
    if strings.Contains(res.Body.String(), "canary-secret") { t.Fatal("credential leaked") }
    assertContains(t, res.Body.String(), "Credential stored")
}
```

Test structured save maps only known form fields into `StructuredPatch` while preserving comments/provider-owned keys/prompt, raw save uses exact textarea bytes, neither form silently applies edits from the other, save conflict `409`, credential replacement, named delete confirmation, environment-managed credential state, PRG redirect on success, focus target in flash/error model, CLI `--port` overriding `server.port` including explicit `--port 0`, workflow-only port use when CLI omits it, run-mode invalid startup exit 1, configure-mode missing file, and normal signal shutdown exit 0.

- [ ] **Step 2: Run focused tests and confirm failure**

```bash
mise exec -- go test ./internal/app ./internal/web ./internal/cli -run 'Test(InvalidSave|Credential|SaveConflict|RunMode|ConfigureMode)' -v
```

Expected: compilation fails because configuration service and handlers do not exist.

- [ ] **Step 3: Implement application composition and service transactions**

```go
type ConfigService interface {
    View(context.Context) (ConfigView, error)
    Validate(context.Context, []byte) workflow.ValidationResult
    Save(context.Context, workflow.SaveCommand) (workflow.Snapshot, error)
    CredentialStatus(context.Context) secrets.Status
    ReplaceCredential(context.Context, []byte) error
    DeleteCredential(context.Context) error
}
```

Run mode loads and fully validates before acquiring the instance lock or starting HTTP. Configure mode resolves a potentially missing workflow leaf, acquires its instance lock, and starts the UI without scheduler components. Record flag presence separately from the numeric port: an explicitly supplied CLI `--port` always wins over `server.port`, including zero; otherwise use the workflow value. Runtime workflow port changes report `restart_required` and do not rebind. Compose vault refs from canonical workflow ID plus selected tracker kind, so a credential saved before the initial workflow exists remains available after tracker scope determines a different data-directory instance ID. Zero posted credential byte buffers after store calls.

- [ ] **Step 4: Implement structured and raw form rendering**

Use one ordinary structured-settings form with `<fieldset>` groups for tracker, credential status, workspace, polling, agent, Codex, hooks, and server, plus a separate complete-workflow form. Every input has a persistent `<label>`, associated description/error IDs, units, valid range, and retained submitted value after errors. `Save structured settings` sends only known fields plus the base digest and patches the YAML AST; `Save complete workflow` sends only the labelled native `<textarea id="raw-source" spellcheck="false">` plus the same digest. No JavaScript synchronization or custom code-editor role is required, and each form explains which representation it saves.

On submitted errors, render `<div id="error-summary" role="alert" tabindex="-1">` with links to every invalid control and set the response focus target consumed by the small progressive script. Success uses one `role="status"`; existing secret values render only `Stored in macOS Keychain`, `Stored in Windows Credential Manager`, `Environment managed`, or `Not configured`.

- [ ] **Step 5: Add Playwright keyboard, error, conflict, and secret tests**

```js
test('invalid raw workflow reports linked errors and preserves input', async ({page}) => {
  await authorize(page, '/configuration');
  const raw = page.getByLabel('Complete WORKFLOW.md');
  await raw.fill('---\ntracker: []\n---\nPrompt');
  await page.getByRole('button', {name: 'Save workflow'}).click();
  await expect(page.locator('#error-summary')).toBeFocused();
  await expect(page.locator('#error-summary')).toContainText('Tracker must be a mapping');
  await expect(raw).toHaveValue('---\ntracker: []\n---\nPrompt');
});
```

Run axe on pristine, invalid, conflict, stored-credential, and delete-confirmation states. Traverse the entire form with keyboard and assert the modal closes with Escape and restores focus to `Delete credential`.

- [ ] **Step 6: Run the vertical slice and commit**

```bash
mise exec -- gofmt -w internal/app internal/web internal/cli cmd/symphony
mise exec -- go test ./internal/app ./internal/web ./internal/cli -v
npm run html:validate
npm run test:a11y -- --project=chromium configuration.spec.mjs
npm run test:a11y -- --project=webkit configuration.spec.mjs
git diff --check
git add go/internal/app go/internal/web go/internal/cli go/cmd go/web go/tests/accessibility go/testdata/manual/.gitkeep
git commit -m "feat(go): configure Symphony through accessible UI"
```

---

### Task 10: Pre-commit source scan and Windows/macOS Phase 1 CI

**Files:**
- Create: `go/scripts/a11y-precommit.mjs`
- Create: `go/scripts/a11y-precommit.test.mjs`
- Create: `go/scripts/a11y-scan-all.mjs`
- Create: `go/scripts/a11y-scan-all.test.mjs`
- Create: `go/scripts/verify.mjs`
- Create: `.githooks/pre-commit`
- Create: `go/.a11y/config.yaml`
- Create: `go/.a11y/web/baseline.json`
- Create: `go/.a11y/web/latest.md`
- Create: `go/docs/accessibility-testing.md`
- Create: `.github/workflows/go.yml`
- Modify: `go/package.json`
- Modify: `go/package-lock.json`
- Modify: `go/README.md`

**Interfaces:**
- Produces: `npm run verify`, the single deterministic Phase 1 verification entry point.
- Produces: a repository pre-commit hook that scans only staged files under `go/web/` and `go/internal/web/` and never updates baseline.
- Requires CI secret `A11Y_RELEASE_READ_TOKEN` with contents-read access only to private repository `coryj627/a11y-check-web`.

- [ ] **Step 1: Write scanner-wrapper argument/exit tests**

Create `go/scripts/a11y-precommit.test.mjs` and inject command execution so the test does not invoke the scanner:

```js
test('passes staged web paths and no-update-baseline', () => {
  const calls = [];
  const code = run({
    repoRoot: '/repo/go',
    staged: ['go/web/templates/base.html', 'README.md'],
    exec: (command, args) => { calls.push([command, args]); return 0; }
  });
  assert.equal(code, 0);
  assert.deepEqual(calls[0][1], [
    'scan', '--repo-root', '/repo/go', '--changed-files', 'web/templates/base.html',
    '--no-update-baseline', '--format', 'text'
  ]);
});
```

Add rows for no applicable files (clean exit without scanner), renamed paths, spaces, commas rejected with a clear message rather than mis-scanned, an applicable partially staged file rejected because the scanner reads the worktree rather than the Git index, scanner exit 1 propagated, and scanner exit 2 propagated.

In `a11y-scan-all.test.mjs`, inject the scanner process and assert the wrapper uses the canonical Go root, sets `A11Y_ALLOWED_ROOTS` to that same root, supplies `--no-update-baseline --format text`, propagates exits 1/2, and never changes the baseline digest.

- [ ] **Step 2: Run wrapper tests and confirm failure**

```bash
node --test scripts/a11y-precommit.test.mjs scripts/a11y-scan-all.test.mjs
```

Expected: module import fails because `a11y-precommit.mjs` does not exist.

- [ ] **Step 3: Implement local pre-commit and full-scan wrappers**

Use `spawnSync('git', ['diff', '--cached', '--name-only', '--diff-filter=ACMR', '-z'])`, split NUL bytes, keep applicable `go/` paths, strip the prefix, and invoke the installed `a11y-check-web` binary with repeated `--changed-files` arguments. Before scanning, compare applicable staged paths with `git diff --name-only -z` and reject any partially staged applicable file with remediation to stage the full file or move its unstaged edit; this prevents scanning different bytes from the commit. Set `A11Y_ALLOWED_ROOTS` to the absolute `go/` directory. `.githooks/pre-commit` contains only:

```sh
#!/usr/bin/env sh
set -eu
exec node go/scripts/a11y-precommit.mjs
```

Document one-time activation as `git config core.hooksPath .githooks` and verify executable mode is committed.

- [ ] **Step 4: Initialize a clean reviewed source-scan floor**

```bash
a11y-check-web init --repo-root "$(pwd)"
a11y-check-web scan --repo-root "$(pwd)" --no-update-baseline --format text
```

Run from `go/`. Fix every new finding in source until exit 0. Commit the generated config, empty reviewed baseline, and latest report; do not run a baseline-update command.

- [ ] **Step 5: Add exact Windows/macOS CI**

`.github/workflows/go.yml` has a `strategy.matrix.os` of `windows-latest` and `macos-latest`, and pins:

```yaml
- uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4
- uses: actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16 # v6
  with:
    go-version-file: go/go.mod
    cache-dependency-path: go/go.sum
- uses: actions/setup-node@49933ea5288caeca8642d1e84afbd3f7d6820020 # v4
  with:
    node-version: 24.18.0
    cache: npm
    cache-dependency-path: go/package-lock.json
```

The job runs `go test ./...`, `go vet ./...`, `npm ci`, `npx playwright install chromium webkit`, `npm run html:validate`, and `npm run test:a11y` from `go/`. A macOS-only step also runs `go test -race ./...`; Windows still runs deterministic concurrency tests because the race detector is not a required toolchain capability there. A separate required `source-accessibility` job on `macos-latest` downloads `v0.3.1` from `coryj627/a11y-check-web` using `A11Y_RELEASE_READ_TOKEN`, installs the tarball, and runs `node scripts/a11y-scan-all.mjs`; a missing secret produces a named setup failure rather than a pass/skip.

- [ ] **Step 6: Run Phase 1 gates and commit**

```bash
node --test scripts/a11y-precommit.test.mjs
mise exec -- go test ./...
mise exec -- go test -race ./... # required on macOS
mise exec -- go vet ./...
npm ci
npm run html:validate
npm run test:a11y
node scripts/a11y-scan-all.mjs
git diff --check
git add .github/workflows/go.yml .githooks/pre-commit go/.a11y go/scripts go/docs/accessibility-testing.md go/package.json go/package-lock.json go/README.md
git commit -m "ci(go): enforce cross-platform accessibility gates"
```

## Phase 1 Acceptance

From `go/`, verify:

```bash
go run ./cmd/symphony ./testdata/workflows/does-not-exist.md
```

Expected: nonzero exit with `missing_workflow_file` and a recommendation to run `symphony configure`; no HTTP listener remains.

```bash
go run ./cmd/symphony configure ./testdata/manual/WORKFLOW.md --port 0
```

Expected: stderr prints one protected loopback URL; the browser can create a valid workflow, store a credential in the native vault, and navigate every route without JavaScript. A second process for the same path reports `already_running`; another workflow starts independently.

Final deterministic gate:

```bash
go test ./...
go test -race ./... # required on macOS
go vet ./...
npm ci
npm run html:validate
npm run test:a11y
node scripts/a11y-scan-all.mjs
```

Expected: every command exits 0 on both Windows and macOS CI; manual keyring smoke evidence is recorded separately because it touches the host vault.
