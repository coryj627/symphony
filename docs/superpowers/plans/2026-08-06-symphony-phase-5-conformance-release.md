# Symphony Phase 5: Cross-Platform, WCAG, and Release Conformance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the working Symphony application into an evidence-backed Windows/macOS release candidate with complete upstream traceability, all WCAG 2.2 A/AA criteria accounted for, hardened failure/security behavior, and recorded NVDA/Chrome plus VoiceOver/Safari verification.

**Architecture:** Machine-readable conformance ledgers bind every upstream and WCAG requirement to exact tests or manual evidence. Deterministic CI runs on native Windows and macOS, browser automation drives the compiled loopback application through every meaningful state, opt-in profiles distinguish real integration evidence from skips, and a final verifier refuses to report release readiness while any required evidence is absent, stale, failed, or synthetic.

**Tech Stack:** Go 1.26.5; Node 24.18.0; Playwright 1.62.1; `@axe-core/playwright` 4.12.1; html-validate 11.6.2; a11y-check-web 0.3.1; `golang.org/x/vuln/cmd/govulncheck` 1.6.0; GitHub Actions on `windows-2025` and `macos-15`; current stable Chrome/NVDA on Windows and current stable Safari/VoiceOver on macOS for the recorded manual gate.

## Global Constraints

- Root `SPEC.md` SHA-256 is `29d6b45a85453e045883c064c0e08595f9d4a33f9a2527f649bc1363b74e0176`; its last modifying commit remains `3c372fa1f32a4d573a7bb9fa0cc101e16add63c3`.
- Approved design SHA-256 is `c566bfb531bdd94a2be961748f652bfd143e97af7856e6029022623843da7267`.
- Every required Section 17/18 bullet receives its own stable ledger row and exact evidence. A broad integration test cannot stand in for unlisted rows.
- The WCAG ledger has exactly 55 WCAG 2.2 A/AA success criteria. Each row is `PASS` or a still-valid product-constrained `NOT_APPLICABLE` with current evidence; `NOT_TESTED`, `SKIPPED`, missing, or stale fails release verification.
- Axe, a11y-check-web, HTML validation, and optional A11yNow results remain separate tool outputs. Findings are triaged as confirmed, advisory, false positive, or tool gap; zero scanner findings alone never establishes conformance.
- Required deterministic CI never needs a tracker/Codex credential. Credentialed GitHub, Linear, and real Codex profiles are opt-in and say `SKIPPED` when disabled; once enabled, any missing prerequisite or failure fails that profile.
- The pre-commit source scan always uses `--no-update-baseline`. Baseline updates are a separate reviewed commit with linked runtime/manual triage.
- Native tests, not cross-compilation, prove Keychain/Credential Manager, process tree, Bash, file identity, locking, hook, and browser behavior.
- A release cannot be accepted without two actual human reports: Windows 11 + Chrome + NVDA and macOS 14+ + Safari + VoiceOver. Automation cannot manufacture or waive these reports.
- No critical/high security defect, accessibility blocker, conformance failure, flaky required test, unexplained skip, or untriaged scanner delta may remain.

---

### Task 1: Make the pinned Symphony requirements executable and traceable

**Files:**
- Create: `go/tests/conformance/upstream-requirements.json`
- Create: `go/tests/conformance/upstream_schema.go`
- Create: `go/tests/conformance/upstream_manifest_test.go`
- Create: `go/tests/conformance/spec_pin_test.go`
- Create: `go/tests/conformance/evidence_names_test.go`
- Create: `go/scripts/check-upstream-trace.mjs`
- Create: `go/scripts/check-upstream-trace.test.mjs`
- Modify: `go/package.json`
- Modify: `go/package-lock.json`
- Create: `go/docs/conformance.md`

**Interfaces:**
- Produces: a row for every bullet in `SPEC.md` Sections 17.1 through 17.8 and 18.1 through 18.3.
- Each row contains `id`, `section`, `profile`, `source_text_sha256`, `evidence`, and `status`; evidence entries are exact Go test names, Playwright test titles, commands, or report IDs.
- Produces: `npm run conformance:upstream`, which fails missing, duplicate, stale, synthetic, unresolved, or nonexistent evidence.

- [ ] **Step 1: Write pin and manifest-completeness tests**

```go
func TestPinnedSpecificationsHaveReviewedDigests(t *testing.T) {
    assertFileSHA256(t, "../../../SPEC.md", "29d6b45a85453e045883c064c0e08595f9d4a33f9a2527f649bc1363b74e0176")
    assertFileSHA256(t, "../../../docs/superpowers/specs/2026-08-06-symphony-accessible-cross-platform-design.md", "c566bfb531bdd94a2be961748f652bfd143e97af7856e6029022623843da7267")
}

func TestEveryUpstreamRequirementHasExactEvidence(t *testing.T) {
    manifest := loadUpstreamManifest(t)
    for _, row := range manifest.Rows {
        if row.Status != "pass" && row.Status != "not_implemented_optional" && row.Status != "skipped_real_profile" { t.Errorf("%s status %q", row.ID, row.Status) }
        if len(row.Evidence) == 0 { t.Errorf("%s has no evidence", row.ID) }
        for _, evidence := range row.Evidence { assertEvidenceExists(t, evidence) }
    }
    assertExpectedSectionCounts(t, manifest)
}
```

`not_implemented_optional` is valid only for an explicitly optional/conditional capability that this approved design does not ship, including workspace population/synchronization and the three explicitly deferred Section 18.2 extensions; it requires a design-scope citation and a test proving the capability is not claimed. `skipped_real_profile` is valid only for Sections 17.8/18.3 and is displayed as not-yet-production-ready; it never satisfies the final production-readiness command.

- [ ] **Step 2: Run tests and confirm failure**

```bash
go test ./tests/conformance -run 'Test(Pinned|EveryUpstream|Evidence)' -v
node --test scripts/check-upstream-trace.test.mjs
```

Expected: manifest, schema, and trace checker do not exist.

- [ ] **Step 3: Transcribe each requirement as one row**

Use stable IDs such as `S17.2-workspace-containment` and `S18.1-dynamic-workflow-reload`. Store a SHA-256 of the exact normalized bullet text so changed wording forces deliberate review. Profiles are `core`, `extension`, or `real_integration`, matching Sections 17 and 18. Evidence points to exact current tests; if a requirement is not actually tested, add its missing focused test before setting `pass`. Preserve repeated source occurrences explicitly rather than silently collapsing them. Mark the three explicitly deferred Section 18.2 extensions `not_implemented_optional` with evidence that Symphony claims no durable retry/session restore, workflow-configurable observability settings, or generic semantic CRUD tool layer.

`check-upstream-trace.mjs` independently extracts the governed section bullets, recomputes row hashes, enumerates Go tests with `go test -list`, enumerates Playwright titles with `--list`, and confirms referenced documents/report IDs exist. It emits a readable table grouped by section and exits nonzero on any mismatch.

- [ ] **Step 4: Fill real test gaps row-by-row**

Run the trace checker, take its first missing row, write a focused red test in the owning package, implement only if behavior is missing, then update that one evidence row. Repeat until the deterministic core and extension rows are all `pass`. Never batch-mark rows because a package suite is green.

- [ ] **Step 5: Run and commit**

```bash
gofmt -w tests/conformance
go test ./tests/conformance -v
node --test scripts/check-upstream-trace.test.mjs
node scripts/check-upstream-trace.mjs --profile deterministic
go test ./...
git diff --check
git add go/tests/conformance go/scripts/check-upstream-trace.mjs go/scripts/check-upstream-trace.test.mjs go/package.json go/package-lock.json go/docs/conformance.md
git commit -m "test(go): trace every Symphony conformance row"
```

---

### Task 2: Red-team the loopback, secret, and provider security boundaries

**Files:**
- Modify: `go/go.mod`
- Modify: `go/go.sum`
- Create: `go/internal/security/canary.go`
- Create: `go/internal/security/canary_test.go`
- Create: `go/internal/security/artifact_scan.go`
- Create: `go/internal/security/artifact_scan_test.go`
- Create: `go/internal/web/security_integration_test.go`
- Create: `go/internal/app/secret_boundary_integration_test.go`
- Create: `go/internal/tracker/redirect_contract_test.go`
- Create: `go/internal/workspace/security_contract_test.go`
- Create: `go/scripts/check-local-assets.mjs`
- Create: `go/scripts/check-local-assets.test.mjs`
- Create: `go/scripts/check-secret-patterns.mjs`
- Create: `go/scripts/check-secret-patterns.test.mjs`
- Modify: `go/package.json`
- Modify: `go/package-lock.json`
- Modify: `go/docs/security.md`

**Interfaces:**
- Produces: `npm run security:assets`, `npm run security:secrets`, and pinned `go tool govulncheck ./...` gates.
- Produces: a canary scanner shared by tests that inspects HTTP bodies/headers, SSE, snapshots, logs, data directories, child environments, and captured test artifacts.
- Guarantees: browser authority remains loopback capability plus session/CSRF checks, not a network-accessible authentication service.

- [ ] **Step 1: Write cross-site and capability adversaries**

```go
func TestUntrustedLoopbackRequestCannotBootstrapOrMutate(t *testing.T) {
    app := newSecurityApp(t)
    for _, tc := range []requestCase{
        getWithHost("/", "evil.example"),
        getWithOrigin("/bootstrap/capability", "https://evil.example"),
        postWithoutCSRF("/api/v1/scheduler/start"),
        postWithForeignOrigin("/api/v1/refresh", "http://localhost.attacker.invalid"),
    } {
        response := app.Do(tc)
        if response.StatusCode < 400 { t.Errorf("%s unexpectedly allowed: %d", tc.Name, response.StatusCode) }
    }
    app.AssertNoRuntimeCommand(t)
}
```

Test IPv4/IPv6 loopback binds, wildcard/LAN bind refusal, Host allowlist with port, DNS rebinding-style Host, foreign/null Origin on unsafe methods, absent/wrong CSRF, cookie flags, bootstrap one-time use/expiry, URL query capability redaction, cross-origin redirect rejection, CSP nonces, frame denial, MIME sniffing denial, referrer policy, no wildcard CORS, HTML/script/style injection, open redirect, path traversal, CRLF/header injection, oversized form/JSON bodies, slow body cancellation, and graceful unauthorized error pages.

- [ ] **Step 2: Seed a unique canary through every secret path**

```go
func TestCanaryNeverCrossesSecretBoundary(t *testing.T) {
    canary := security.NewCanary(t)
    result := runSecretBoundaryScenario(t, canary.Value)
    canary.AssertAbsent(t, result.HTTP, result.SSE, result.Snapshot, result.LogFiles,
        result.DataDirectory, result.ChildEnvironment, result.PlaywrightArtifacts)
}
```

Cover Keychain/Credential Manager errors, GitHub/Linear auth headers, URL query strings, provider error bodies, Codex stderr/stdout, tool arguments/results, operator secret input, panic stacks, structured logs, SSE reset, save conflicts, and CI artifact collection. Seed common fake token shapes too, while proving safe non-secret issue content remains observable.

- [ ] **Step 3: Run tests and confirm failure**

```bash
go test ./internal/security ./internal/web ./internal/app ./internal/tracker/... ./internal/workspace -run 'Test.*(Security|Secret|Canary|Untrusted|Redirect)' -v
node --test scripts/check-local-assets.test.mjs scripts/check-secret-patterns.test.mjs
```

Expected: cross-cutting canary/artifact gates and several adversarial cases do not exist.

- [ ] **Step 4: Implement the minimal hardening and local-asset gates**

Keep CSP `default-src 'none'`; permit only same-origin packaged styles/scripts with per-response nonces where inline bootstrap data is unavoidable, `connect-src 'self'`, `img-src 'self' data:`, `form-action 'self'`, `base-uri 'none'`, and `frame-ancestors 'none'`. Reject any configured tracker redirect whose origin differs after normalized scheme/host/port comparison.

`check-local-assets.mjs` parses rendered HTML plus CSS/JS and rejects remote URL schemes, protocol-relative URLs, external source maps, analytics/telemetry markers, inline event handlers, and resources missing from the embed manifest. `check-secret-patterns.mjs` scans tracked non-fixture text for private-key blocks, common live token prefixes, authorization values, and unapproved credential literals; its explicit fixture allowlist is path-and-line scoped and tested.

- [ ] **Step 5: Add the pinned vulnerability check**

Add Go's tool directive/dependency for `golang.org/x/vuln/cmd/govulncheck` 1.6.0. CI runs `go tool govulncheck ./...`; inability to reach the official vulnerability database is an infrastructure error, not a clean scan. Record the command/version in CI output.

- [ ] **Step 6: Run and commit**

```bash
gofmt -w internal/security internal/web internal/app internal/tracker internal/workspace
go test ./internal/security ./internal/web ./internal/app ./internal/tracker/... ./internal/workspace -run 'Test.*(Security|Secret|Canary|Untrusted|Redirect)' -count=5 -v
node --test scripts/check-local-assets.test.mjs scripts/check-secret-patterns.test.mjs
npm run security:assets
npm run security:secrets
go tool govulncheck ./...
go test ./...
git diff --check
git add go/go.mod go/go.sum go/internal/security go/internal/web go/internal/app go/internal/tracker go/internal/workspace go/scripts/check-local-assets.mjs go/scripts/check-local-assets.test.mjs go/scripts/check-secret-patterns.mjs go/scripts/check-secret-patterns.test.mjs go/package.json go/package-lock.json go/docs/security.md
git commit -m "test(go): harden Symphony security boundaries"
```

---

### Task 3: Inject host failures and prove native Windows/macOS behavior

**Files:**
- Create: `go/internal/testfault/fs.go`
- Create: `go/internal/testfault/fs_test.go`
- Create: `go/internal/testfault/process.go`
- Create: `go/internal/testfault/process_test.go`
- Modify: `go/internal/workflow/atomic.go`
- Create: `go/internal/workflow/save_failure_test.go`
- Modify: `go/internal/workspace/manager.go`
- Create: `go/internal/workspace/manager_failure_test.go`
- Modify: `go/internal/observability/rotating_writer.go`
- Create: `go/internal/observability/file_sink_failure_test.go`
- Create: `go/internal/instance/native_contract_test.go`
- Create: `go/internal/secrets/native_contract_test.go`
- Create: `go/internal/workspace/native_contract_test.go`
- Create: `go/internal/codex/native_contract_test.go`
- Create: `go/scripts/run-native-contracts.mjs`
- Create: `go/scripts/run-native-contracts.test.mjs`
- Modify: `.github/workflows/go.yml`

**Interfaces:**
- Produces: deterministic fault points for create/write/fsync/rename/stat/remove, hook start/wait, log rotation/write, child start/assign/interrupt/kill, and shutdown drain.
- Produces: `node scripts/run-native-contracts.mjs`, selecting only assertions meaningful on the current supported host and emitting JUnit evidence.
- Guarantees: fault injection is constructor-injected and unavailable from the production CLI.

- [ ] **Step 1: Write red failure-path tests before adapters**

```go
func TestAtomicSaveNeverLeavesMissingOrPartialWorkflow(t *testing.T) {
    for _, point := range testfault.AtomicSavePoints() {
        t.Run(point.Name, func(t *testing.T) {
            fixture := newSaveFixture(t, "old valid workflow")
            fixture.FS.FailOnce(point.Name)
            if err := fixture.Save("new valid workflow"); err == nil { t.Fatal("expected failure") }
            got := fixture.ReadWorkflow()
            want := "old valid workflow"
            if point.AfterAtomicReplace { want = "new valid workflow" }
            if got != want { t.Fatalf("workflow became %q, want complete %q", got, want) }
            fixture.AssertNoSecretOrOrphanTemp(t)
        })
    }
}
```

Add matrix tests for log sink failure without scheduler crash, journal truncation, lock contention/stale metadata, keychain/credential-manager denied/not-found/corrupt values, workspace root replaced by symlink/reparse point between validation and launch/remove, hook timeout with descendants, Bash missing, app-server job/group setup failure, browser port collision, tracker partial pagination, workflow reload during run, shutdown during retry/request/tool call, and startup sweep partial cleanup.

- [ ] **Step 2: Run tests and confirm failure**

```bash
go test ./internal/... -run 'Test.*(Failure|Fault|NativeContract)' -v
node --test scripts/run-native-contracts.test.mjs
```

Expected: injectable fault seams and native contract runner do not exist.

- [ ] **Step 3: Implement narrow fault seams and recovery invariants**

Define package-local filesystem/process interfaces implemented by standard library adapters; production constructors always use real adapters. Preserve original data on ambiguous failures, never broaden permissions, never hot-loop permanent config/auth/safety errors, and retain a visible degraded status. A child that cannot be proven stopped prevents workspace cleanup/re-dispatch for its ID.

- [ ] **Step 4: Run native contracts on both platforms**

macOS contract: Keychain round trip with random service/account, canonical/symlink containment, `/bin/bash`, process-group descendant termination, lock collision, fsync/rename behavior, Chrome and WebKit browser suite. Windows contract: Credential Manager round trip with random target, case-insensitive/reparse containment, Git for Windows Bash, Job Object descendant termination, lock collision, replace/rename behavior, and stable Chrome browser suite. Every live credential test deletes only the exact random target it created in a test cleanup.

- [ ] **Step 5: Harden CI matrix**

Pin actions by commit SHA and use `windows-2025` plus `macos-15`. The native job runs unit/integration tests, `run-native-contracts.mjs`, browser tests, native binary smoke, and architecture cross-builds. Run `go test -race ./...` on macOS; on Windows run deterministic concurrency stress without claiming race-detector coverage. Upload JUnit/Playwright logs only after the canary artifact scanner passes.

- [ ] **Step 6: Run and commit**

```bash
gofmt -w internal/testfault internal/workflow internal/workspace internal/observability internal/instance internal/secrets internal/codex
go test ./internal/... -run 'Test.*(Failure|Fault|NativeContract)' -count=5 -v
node --test scripts/run-native-contracts.test.mjs
node scripts/run-native-contracts.mjs
go test ./...
git diff --check
git add go/internal/testfault go/internal/workflow go/internal/workspace go/internal/observability go/internal/instance go/internal/secrets go/internal/codex go/scripts/run-native-contracts.mjs go/scripts/run-native-contracts.test.mjs .github/workflows/go.yml
git commit -m "test(go): prove native host failure recovery"
```

---

### Task 4: Encode and automate the complete 55-row WCAG ledger

**Files:**
- Create: `go/tests/accessibility/wcag-22-aa.json`
- Create: `go/tests/accessibility/wcag-ledger.test.mjs`
- Modify: `go/playwright.config.mjs`
- Modify: `go/tests/accessibility/fixtures.mjs`
- Create: `go/tests/accessibility/axe-all-states.spec.mjs`
- Create: `go/tests/accessibility/structure-sequence.spec.mjs`
- Create: `go/tests/accessibility/keyboard-focus.spec.mjs`
- Create: `go/tests/accessibility/timing-motion.spec.mjs`
- Create: `go/tests/accessibility/visual-geometry.spec.mjs`
- Create: `go/tests/accessibility/text-resize-spacing.spec.mjs`
- Create: `go/tests/accessibility/forms-errors-auth.spec.mjs`
- Create: `go/tests/accessibility/status-updates.spec.mjs`
- Create: `go/tests/accessibility/content-constraints.spec.mjs`
- Create: `go/tests/accessibility/helpers/accessibility-tree.mjs`
- Create: `go/tests/accessibility/helpers/color.mjs`
- Create: `go/tests/accessibility/helpers/focus.mjs`
- Create: `go/tests/accessibility/helpers/geometry.mjs`
- Create: `go/tests/accessibility/helpers/text-spacing.mjs`
- Create: `go/tests/accessibility/helpers/wcag-ledger.mjs`
- Modify: `go/package.json`
- Modify: `go/package-lock.json`
- Create: `go/docs/accessibility/automated-testing.md`

**Interfaces:**
- Produces: exactly 55 rows matching the approved design ledger, each with `criterion`, `level`, `disposition`, `evidence`, and `rationale`.
- Produces: Playwright projects `chrome-windows`, `chrome-macos`, and `webkit-macos`; unsupported host/project combinations are excluded explicitly rather than reported as passes.
- Produces: machine-readable per-state axe and functional results without combining their counts.

- [ ] **Step 1: Write ledger-integrity tests**

```js
test('ledger contains every WCAG 2.2 A and AA criterion exactly once', () => {
  const ledger = loadLedger();
  assert.equal(ledger.rows.length, 55);
  assert.deepEqual(ledger.rows.map(row => row.criterion).sort(), expectedWCAG22AAndAA);
  for (const row of ledger.rows) {
    assert.match(row.disposition, /^(pass|not_applicable)$/);
    assert.ok(row.evidence.length > 0, `${row.criterion} lacks evidence`);
  }
});
```

The expected list is literal and includes WCAG 2.2 additions 2.4.11, 2.5.7, 2.5.8, 3.2.6, 3.3.7, and 3.3.8. It excludes removed 4.1.1 while HTML validity remains a separate required gate. `not_applicable` rows have executable product-constraint checks proving no audio/video/flashing/motion/image-of-text feature was introduced.

- [ ] **Step 2: Expand the exact route/state manifest**

For every route cover applicable loading, empty, populated, running, retrying, needs-attention, validation error, server error, modal open, operator request, stale request, incompatible Codex, and SSE reconnect/reset states. Each state fixture has a stable URL/data seed and one expected `<title>`, `<h1>`, focus target, live-region behavior, and screenshot/DOM artifact prefix.

- [ ] **Step 3: Write functional accessibility tests before fixes**

```js
for (const state of routeStates) {
  test(`${state.id} has no WCAG 2.2 A/AA axe violations`, async ({ page }, testInfo) => {
    await state.open(page);
    const result = await new AxeBuilder({ page })
      .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa'])
      .analyze();
    await writeToolResult(testInfo, 'axe', result);
    expect(result.violations).toEqual([]);
  });
}
```

Add independent assertions for skip/landmark/headings/table associations/DOM order; full keyboard journeys and no traps; native label/name/role/value; focus visible and not obscured geometry; no focus theft; dialog containment/restoration; 44x44 product targets with documented inline-link exceptions; color/text/non-text/focus contrast; 320 CSS px reflow; 200% text and 400% zoom equivalent; WCAG text-spacing overrides; forced colors; reduced motion; orientation; hover/focus content; deadline warning and ten extensions; presentation pause; error summary/association/suggestion/value preservation; accessible authentication; concise status messages; and silent logs/timers/SSE.

- [ ] **Step 4: Run tests and record genuine failures**

```bash
npm run test:a11y -- --project chrome-macos
npm run test:a11y -- --project webkit-macos
node --test tests/accessibility/wcag-ledger.test.mjs
```

Expected: new functional tests reveal any remaining product defects or missing evidence; do not edit expected results to accept them.

- [ ] **Step 5: Fix each confirmed defect with red-green evidence**

For each failure, keep the focused test red, make the smallest semantic/template/CSS/JS change, rerun that test in both applicable engines, then rerun the route's axe state. Scanner suggestions remain advisory until reproduced against rendered behavior and the criterion.

- [ ] **Step 6: Bind every ledger row to exact passing evidence**

Evidence names include exact Playwright titles, axe state IDs, HTML validator command, source constraint tests, or later manual report step IDs. `wcag-ledger.test.mjs` enumerates Playwright tests with `--list` and fails nonexistent references. No row may cite only `axe-all-states` when its requirement needs keyboard, timing, content, or manual behavior.

- [ ] **Step 7: Run and commit**

```bash
node --test tests/accessibility/wcag-ledger.test.mjs
npm run html:validate
npm run test:a11y
go test ./...
git diff --check
git add go/tests/accessibility go/package.json go/package-lock.json go/web go/internal/web go/docs/accessibility/automated-testing.md
git commit -m "test(go): automate the WCAG 2.2 AA ledger"
```

---

### Task 5: Enforce a11y-check-web pre-commit and reconcile scanner evidence

**Files:**
- Modify: `go/scripts/a11y-precommit.mjs`
- Modify: `go/scripts/a11y-precommit.test.mjs`
- Modify: `go/scripts/a11y-scan-all.mjs`
- Modify: `go/scripts/a11y-scan-all.test.mjs`
- Create: `go/scripts/reconcile-accessibility-results.mjs`
- Create: `go/scripts/reconcile-accessibility-results.test.mjs`
- Modify: `go/.a11y/config.yaml`
- Modify: `go/.a11y/web/baseline.json`
- Create: `go/docs/accessibility/scanner-triage.md`
- Modify: `.githooks/pre-commit`
- Modify: `.github/workflows/go.yml`

**Interfaces:**
- Produces: exact changed-file invocation `a11y-check-web scan --repo-root <absolute-go-root> --changed-files <comma-separated-paths> --no-update-baseline --format text`.
- Produces: full-tree CI invocation with the same root and `--no-update-baseline --format json`.
- Produces: a reconciliation report retaining separate axe, a11y-check-web, optional A11yNow, manual, and confirmed-defect counts.

- [ ] **Step 1: Write hostile staged-path and exit-code tests**

Test no applicable files, one file, spaces, comma-path rejection, Unicode, rename, deletion, partially staged applicable-file rejection, root-relative normalization, outside-root rejection, duplicate paths, scanner missing, exit 0/1/2, signal termination, invalid JSON, and baseline mtime/digest unchanged. Use NUL-delimited `git diff --cached --name-status -z`; never split filenames on whitespace or interpolate them into a shell command.

- [ ] **Step 2: Run tests and confirm failure**

```bash
node --test scripts/a11y-precommit.test.mjs scripts/a11y-scan-all.test.mjs scripts/reconcile-accessibility-results.test.mjs
```

Expected: edge-case coverage and reconciliation script are absent or incomplete.

- [ ] **Step 3: Implement scanner execution without baseline mutation**

Spawn the CLI directly. Reject an applicable file present in both staged and unstaged diffs because the CLI scans worktree bytes; the message gives the two safe remedies instead of claiming the staged content passed. Set `A11Y_ALLOWED_ROOTS` to the canonical Go root. Exit 1 blocks new findings; exit 2 or launch failure blocks as scanner/configuration error. Hash `.a11y/web/baseline.json` before/after and fail if it changes even when the CLI exits zero. Full-tree CI supplies all applicable source paths if the CLI requires changed-file scope.

Required CI downloads private release `v0.3.1` from `coryj627/a11y-check-web` with a fine-grained contents-read `A11Y_RELEASE_READ_TOKEN`, installs the `.tgz`, verifies `a11y-check-web --version`, then scans. Missing/invalid token fails the named setup step; it is not reported as a clean accessibility result.

- [ ] **Step 4: Implement evidence reconciliation**

Input files are tool-native JSON plus manual/ledger JSON. Normalize IDs only for comparison while preserving original tool text. Each potential issue is classified `confirmed`, `advisory`, `false_positive`, or `tool_gap` with criterion, route/source, reviewer rationale, and evidence link. Reports display each tool's raw count separately and never subtract false positives to rewrite scanner output.

Optional A11yNow uses the same route/state manifest and writes a separate input file. Its absence is `not_run_optional`, not a failure and not an axe substitute.

- [ ] **Step 5: Run the actual source gate and review any delta**

```bash
node --test scripts/a11y-precommit.test.mjs scripts/a11y-scan-all.test.mjs scripts/reconcile-accessibility-results.test.mjs
node scripts/a11y-scan-all.mjs
npm run test:a11y
node scripts/reconcile-accessibility-results.mjs --axe test-results/axe --source test-results/a11y-check-web.json --out test-results/accessibility-reconciliation.json
```

If the source scanner reports a lead, reproduce it in rendered behavior, add a focused test for a confirmed defect, fix it, and rescan. Baseline changes require a separate commit and documented review; this task expects no unreviewed baseline delta.

- [ ] **Step 6: Commit**

```bash
git diff --check
git add go/scripts/a11y-precommit.mjs go/scripts/a11y-precommit.test.mjs go/scripts/a11y-scan-all.mjs go/scripts/a11y-scan-all.test.mjs go/scripts/reconcile-accessibility-results.mjs go/scripts/reconcile-accessibility-results.test.mjs go/.a11y go/docs/accessibility/scanner-triage.md .githooks/pre-commit .github/workflows/go.yml
git commit -m "test(go): enforce and reconcile accessibility scans"
```

---

### Task 6: Record real NVDA/Chrome and VoiceOver/Safari acceptance

**Files:**
- Create: `go/docs/accessibility/manual-nvda-chrome.md`
- Create: `go/docs/accessibility/manual-voiceover-safari.md`
- Create: `go/docs/accessibility/manual-result-schema.json`
- Create: `go/scripts/record-manual-accessibility.mjs`
- Create: `go/scripts/record-manual-accessibility.test.mjs`
- Create: `go/scripts/runtime-source-digest.mjs`
- Create: `go/scripts/runtime-source-digest.test.mjs`
- Create: `go/scripts/build-supported.mjs`
- Create: `go/scripts/build-supported.test.mjs`
- Create: `go/internal/buildinfo/version.go`
- Create: `go/internal/buildinfo/version_test.go`
- Create: `go/internal/web/version.go`
- Create: `go/internal/web/version_test.go`
- Modify: `go/internal/web/routes.go`
- Create: `go/tests/conformance/manual_accessibility_test.go`
- Create: `go/testdata/conformance/manual-result-valid.json`
- Create after actual execution: `go/docs/releases/accessibility/windows-nvda-chrome.json`
- Create after actual execution: `go/docs/releases/accessibility/macos-voiceover-safari.json`

**Interfaces:**
- Produces: two versioned, machine-validated manual scripts with identical stable step IDs where tasks overlap.
- Produces: an interactive recorder that writes a report only after every required step has an explicit result, tester note, exact environment version, date, served binary SHA-256, runtime-source SHA-256, and defect/retest disposition.
- Guarantees: fixture evidence under `testdata/` is marked synthetic and rejected by the release verifier.

- [ ] **Step 1: Write schema/recorder tests**

```go
func TestReleaseNeedsBothActualScreenReaderReportsForCurrentRuntimeSource(t *testing.T) {
    reports := loadManualReports(t)
    digest := currentRuntimeSourceDigest(t)
    requireActualPassingReport(t, reports, "windows", "nvda", "chrome", digest)
    requireActualPassingReport(t, reports, "macos", "voiceover", "safari", digest)
}
```

Test missing platform, synthetic flag, wrong AT/browser pair, blank versions, stale runtime-source digest, binary hash mismatch, unexecuted/skipped step, unresolved defect, failed retest, duplicate step, missing note, impossible date, and valid reports. Recorder tests use scripted stdin and prove it cannot label a failed/incomplete run as pass. The digest script hashes only runtime-affecting `cmd/`, `internal/`, `web/`, `schema/`, `go.mod`, and `go.sum` content in canonical path order, so committing reports cannot invalidate its own evidence.

- [ ] **Step 2: Run tests and confirm the release gate is red**

```bash
node --test scripts/record-manual-accessibility.test.mjs scripts/runtime-source-digest.test.mjs scripts/build-supported.test.mjs
go test ./tests/conformance -run TestReleaseNeedsBothActualScreenReaderReportsForCurrentRuntimeSource -v
```

Expected: recorder/schema/candidate builder do not exist initially; after implementation the conformance test still fails until both human runs are recorded.

- [ ] **Step 3: Write exact task scripts**

Both scripts cover: launch authorization, unique title/landmarks/headings, skip link, navigation, queue discovery/filtering, issue inspection, scheduler start/stop/refresh, an approval, multi-question user input, warning and Extend, configuration validation/save/conflict/value retention, credential replace/delete confirmation, log navigation without announcement flooding, error remediation, dialog focus/restoration, SSE reconnect/reset, narrow/zoom/reflow, text spacing, forced colors/high contrast where supported, and shutdown recovery.

NVDA steps specify Browse/Focus mode, H/D/T/F/B element navigation, Elements List, speech output, Chrome zoom, and focus announcement expectations. VoiceOver steps specify Quick Nav, rotor headings/landmarks/links/form controls, Control-Option navigation, Safari zoom, and announcement expectations. Scripts instruct the tester to record observed speech/focus, not just visual success.

- [ ] **Step 4: Implement the recorder and red release verifier**

Implement the initial native-only `build-supported.mjs`: compute the canonical runtime-source digest, inject it plus application/Codex-schema versions through constrained `-ldflags -X` variables, build the current host binary with `-trimpath`, and write a manifest containing its absolute path and SHA-256. Add authenticated `GET /api/v1/version` returning only those non-secret build values. Build the native candidate, launch that exact manifest path, and let the recorder authorize then query the endpoint; it rejects a source digest or executable hash that differs from the build manifest before walking any step. The recorder obtains OS/browser/AT versions through explicit commands or tester confirmation. It writes JSON atomically and never preselects pass. Any defect requires an ID and later passing retest entry. The verifier accepts only `synthetic:false` reports whose runtime-source digest matches the current runtime tree and whose binary hash matches the recorded tested artifact.

- [ ] **Step 5: Human checkpoint — execute both matrices**

On Windows 11 in stable Chrome with stable NVDA:

```powershell
$env:SYMPHONY_TEST_URL = Read-Host "Paste the complete loopback URL printed by Symphony"
node scripts/record-manual-accessibility.mjs --matrix windows-nvda-chrome --url $env:SYMPHONY_TEST_URL
```

On macOS 14 or later in stable Safari with VoiceOver:

```bash
read -r "SYMPHONY_TEST_URL?Paste the complete loopback URL printed by Symphony: "
node scripts/record-manual-accessibility.mjs --matrix macos-voiceover-safari --url "$SYMPHONY_TEST_URL"
```

The printed ephemeral port is copied from the running local process; it is not a fixed release value. Stop here for any failed step, preserve its report, add a focused regression where automatable, fix, and repeat the failed matrix. Do not create a passing report from recollection or another browser/screen reader.

- [ ] **Step 6: Validate actual evidence and commit**

```bash
node --test scripts/record-manual-accessibility.test.mjs scripts/runtime-source-digest.test.mjs scripts/build-supported.test.mjs
go test ./tests/conformance -run TestReleaseNeedsBothActualScreenReaderReportsForCurrentRuntimeSource -v
node scripts/reconcile-accessibility-results.mjs --manual docs/releases/accessibility --ledger tests/accessibility/wcag-22-aa.json --out test-results/manual-reconciliation.json
git diff --check
git add go/docs/accessibility/manual-nvda-chrome.md go/docs/accessibility/manual-voiceover-safari.md go/docs/accessibility/manual-result-schema.json go/scripts/record-manual-accessibility.mjs go/scripts/record-manual-accessibility.test.mjs go/scripts/runtime-source-digest.mjs go/scripts/runtime-source-digest.test.mjs go/scripts/build-supported.mjs go/scripts/build-supported.test.mjs go/internal/buildinfo/version.go go/internal/buildinfo/version_test.go go/internal/web/version.go go/internal/web/version_test.go go/internal/web/routes.go go/tests/conformance/manual_accessibility_test.go go/testdata/conformance/manual-result-valid.json go/docs/releases/accessibility
git commit -m "test(go): record NVDA and VoiceOver acceptance"
```

---

### Task 7: Run explicit GitHub, Linear, and real Codex production profiles

**Files:**
- Create: `go/tests/integration/profile.go`
- Create: `go/tests/integration/profile_test.go`
- Create: `go/tests/integration/github_live_test.go`
- Create: `go/tests/integration/linear_live_test.go`
- Create: `go/tests/integration/codex_live_test.go`
- Create: `go/tests/integration/end_to_end_live_test.go`
- Create: `go/scripts/run-real-integrations.mjs`
- Create: `go/scripts/run-real-integrations.test.mjs`
- Create: `go/docs/integration-testing.md`
- Create after actual execution: `go/docs/releases/integrations/real-integration-report.json`
- Modify: `.github/workflows/go-integrations.yml`

**Interfaces:**
- Produces: independent profiles `github`, `linear`, `codex`, and `end_to_end`, each with explicit enable flag, prerequisite list, isolated scope, cleanup policy, and JSON result.
- Enable flags: `SYMPHONY_ENABLE_GITHUB_LIVE`, `SYMPHONY_ENABLE_LINEAR_LIVE`, `SYMPHONY_ENABLE_CODEX_LIVE`, and `SYMPHONY_ENABLE_END_TO_END_LIVE`.
- Produces: `node scripts/run-real-integrations.mjs`; disabled is `SKIPPED`, enabled prerequisite failure/test failure is `FAILED`.

- [ ] **Step 1: Write profile truthfulness tests**

Test disabled profile, enabled with missing credential/workflow/Bash/Codex, enabled test failure, cleanup failure, timeout, report redaction, stale report, and all-pass. Assert a disabled profile cannot emit `PASS`, and an enabled profile cannot turn missing auth/network into `SKIPPED`.

- [ ] **Step 2: Run tests and confirm failure**

```bash
go test ./tests/integration -run TestProfile -v
node --test scripts/run-real-integrations.test.mjs
```

Expected: profile runner/report schema do not exist.

- [ ] **Step 3: Implement isolated provider profiles**

GitHub live reads a designated repository Issue, verifies pagination/normalization, runs get/list plus one idempotent comment against a designated disposable issue, and deletes/marks its artifact when the API supports safe cleanup. Linear live reads the designated project and executes one query plus one mutation only against a designated disposable issue. Profiles use test-specific vault targets or protected CI variables, never a developer's default credential implicitly.

Real Codex launches target 0.144.1, checks the manifest/user-agent preflight, runs a harmless turn in a temporary isolated repository, and confirms bounded shutdown. End-to-end uses an explicitly designated disposable tracker issue/workspace, scoped provider tool, and validates state/observability without touching the Symphony development issue.

- [ ] **Step 4: Implement report/redaction and CI controls**

Each profile records start/end, platform, runtime-source digest, external versions, isolated identifiers, assertions, cleanup outcome, and `PASS|FAIL|SKIPPED`. Strip credentials, authorization headers, query capabilities, prompt/model output, and full provider bodies. CI environments must approve the private/protected secret context; untrusted pull requests never receive credentials.

- [ ] **Step 5: Execute authorized real profiles**

This is an external-mutation checkpoint. Before running, show the operator the exact GitHub repository/issue, Linear project/issue, intended comment/field mutation, cleanup action, and Codex test repository, and obtain confirmation for those resolved targets.

```bash
node scripts/run-real-integrations.mjs --profiles github,linear,codex,end_to_end --out docs/releases/integrations/real-integration-report.json
```

Run only with the explicit enable flags, designated test workflow/scopes, and credentials already authorized for this verification. If those prerequisites are unavailable, retain truthful `SKIPPED` entries and do not mark production readiness complete.

- [ ] **Step 6: Validate and commit actual evidence**

```bash
go test ./tests/integration -v
node --test scripts/run-real-integrations.test.mjs
node scripts/run-real-integrations.mjs --verify docs/releases/integrations/real-integration-report.json
git diff --check
git add go/tests/integration go/scripts/run-real-integrations.mjs go/scripts/run-real-integrations.test.mjs go/docs/integration-testing.md go/docs/releases/integrations/real-integration-report.json .github/workflows/go-integrations.yml
git commit -m "test(go): record real Symphony integration profiles"
```

---

### Task 8: Finish operator documentation, CI, and the no-excuses release verifier

**Files:**
- Modify: `README.md`
- Modify: `go/README.md`
- Create: `go/docs/getting-started.md`
- Create: `go/docs/configuration.md`
- Modify: `go/docs/github.md`
- Modify: `go/docs/linear.md`
- Create: `go/docs/multiple-instances.md`
- Create: `go/docs/recovery.md`
- Create: `go/docs/logging-privacy.md`
- Create: `go/docs/accessibility/operator-guide.md`
- Create: `go/docs/release.md`
- Create: `go/scripts/verify-release.mjs`
- Create: `go/scripts/verify-release.test.mjs`
- Modify: `go/scripts/build-supported.mjs`
- Modify: `go/scripts/build-supported.test.mjs`
- Create: `go/tests/conformance/release_readiness_test.go`
- Modify: `go/package.json`
- Modify: `go/package-lock.json`
- Modify: `.github/workflows/go.yml`
- Modify: `.github/workflows/go-integrations.yml`

**Interfaces:**
- Produces: `npm run verify:release`, the single deterministic entry point for all code, conformance, security, browser, scanner, evidence-freshness, and artifact checks.
- Produces: supported binaries for `darwin/amd64`, `darwin/arm64`, `windows/amd64`, and `windows/arm64`; no installer and no Linux support claim.
- Guarantees: a successful verifier names the candidate Git commit, runtime-source digest, tested binary hashes, platform/tool versions, and every skip disposition.

- [ ] **Step 1: Write release-verifier falsification tests**

Feed fixture reports with one missing upstream row, one missing WCAG row, one stale manual report, one unresolved defect, scanner exit 2, mutated baseline, skipped enabled integration, canary in artifact, wrong Codex digest, failed native contract, remote asset, missing architecture, and flaky-test marker. Each must produce a named failure and nonzero exit. A complete synthetic fixture may exercise verifier mechanics but must never satisfy production mode.

- [ ] **Step 2: Run tests and confirm failure**

```bash
node --test scripts/verify-release.test.mjs scripts/build-supported.test.mjs
go test ./tests/conformance -run TestReleaseReadiness -v
```

Expected: the release verifier/final readiness test do not exist, and the existing native-only candidate builder fails the new four-target matrix assertions.

- [ ] **Step 3: Write operator documentation against the live CLI/UI**

Document prerequisites, `go run ./cmd/symphony [path-to-WORKFLOW.md]`, configuration mode, printed local URL/bootstrap authorization, GitHub repository scope, Linear project scope, native vault replacement/deletion, Git for Windows Bash, default Codex sandbox/network/approval behavior, running several processes with distinct workflows/ports/data directories, duplicate-workflow lock errors, start/stop/refresh, request deadlines/extensions, logs and redaction, crash/restart recovery, accessible navigation and shortcuts, NVDA/VoiceOver browser behaviors, privacy boundary, and the fact that trusted hooks/permissive agent settings can access host-account data.

Do not claim installability, Linux support, restored live sessions after restart, scanner-proven WCAG conformance, or remote browser access.

- [ ] **Step 4: Implement supported builds and CI workflows**

`build-supported.mjs` invokes `go build -trimpath` with embedded application version, runtime-source digest, and Codex schema digest for exactly four supported targets and writes SHA-256 sums. It performs native smoke on the current platform and cross-compiles the others. The Git commit remains release-report metadata rather than part of binary identity, so evidence commits cannot change the tested runtime artifact. CI uses these pinned action commits:

```text
actions/checkout@11d5960a326750d5838078e36cf38b85af677262
actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16
actions/setup-node@49933ea5288caeca8642d1e84afbd3f7d6820020
actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02
```

Required jobs are native macOS and Windows code/conformance/security/browser/source-scan gates plus supported builds. Integration jobs remain protected/explicit. Artifact upload runs only after secret/canary scanning. No Ubuntu/Linux job is added for the product.

- [ ] **Step 5: Implement the release verifier**

Run, in order: pin/schema integrity; format/vet/unit/race where supported; upstream trace; native contracts; fake integrations; HTML validity; axe/Playwright functional accessibility; a11y-check-web full source scan; scanner reconciliation; local asset/secret/vulnerability checks; WCAG ledger; actual manual-report freshness; real-integration production profile; supported builds; artifact canary scan. Capture each command/exit code and fail closed. `--deterministic` permits truthful disabled real profiles but reports `NOT PRODUCTION READY`; `--production` requires them all pass.

- [ ] **Step 6: Run the complete release candidate gate on both native hosts**

```bash
npm run verify:release -- --deterministic
npm run verify:release -- --production
git diff --check
git status --short
```

The deterministic command must pass on both Windows and macOS. The production command passes only after current actual manual and real-integration evidence matches the candidate's runtime-source digest and tested artifact hashes. Investigate any flaky retry; do not add automatic retries to turn an unstable required test green.

- [ ] **Step 7: Commit documentation and release gates**

```bash
git add README.md go/README.md go/docs go/scripts/verify-release.mjs go/scripts/verify-release.test.mjs go/scripts/build-supported.mjs go/scripts/build-supported.test.mjs go/tests/conformance/release_readiness_test.go go/package.json go/package-lock.json .github/workflows/go.yml .github/workflows/go-integrations.yml
git commit -m "docs(go): complete Symphony release operations"
```

## Phase 5 Acceptance

At the candidate commit and its verified runtime-source digest, deterministic Windows and macOS jobs pass with no unexplained skip; every pinned Symphony core/extension row has exact passing evidence; every one of the 55 WCAG 2.2 A/AA rows has current pass or still-valid not-applicable evidence; rendered states pass Playwright plus axe, HTML validity, keyboard/focus/geometry/reflow/text-spacing/contrast/status assertions, and the unchanged a11y-check-web baseline gate; scanner outputs are reconciled without rewriting their raw counts; native process/vault/workspace/lock/failure contracts pass; actual NVDA/Chrome and VoiceOver/Safari reports contain no unresolved defect; real GitHub, Linear, Codex, and end-to-end profiles pass or the verifier truthfully reports that production readiness is incomplete; four supported binaries build without an installer or Linux claim; and `npm run verify:release -- --production` exits zero.
