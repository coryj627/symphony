# Accessibility testing

Symphony targets every WCAG 2.2 Level A and AA success criterion on macOS 14+
and Windows 11. Its automated checks complement one another; none is a complete
WCAG conformance assessment or a substitute for testing with the supported
browser and screen reader. Linux is not supported.

Keep tool-produced results separate from manual conclusions. A zero-finding
scanner run means only that the scanner found no violations within its own
rules and coverage.

## Install the source scanner

The source gate uses private release `v0.3.1` of
`coryj627/a11y-check-web`. Download
`a11y-check-web-mcp-server-0.3.1.tgz` from that release and install it with
Node.js 24.18.0:

```bash
npm install -g ./a11y-check-web-mcp-server-0.3.1.tgz
```

The GitHub Actions secret `A11Y_RELEASE_READ_TOKEN` must have contents-read
access to that repository only. The source-accessibility job fails during its
named download step when the secret is absent. That token exists only while
`gh release download` runs. Installation and npm lifecycle scripts run in a
separate step without the token. CI requests exactly
`a11y-check-web-mcp-server-0.3.1.tgz`, rejects missing or extra directory
entries and non-regular/symlink artifacts, and verifies that the installed CLI
reports version `0.3.1` before scanning.

## Run the gates

Install Chromium and WebKit once after `npm ci`:

```bash
npx playwright install chromium webkit
```

Then run the complete platform-aware verification entry point from `go/`:

```bash
npm run verify
```

It runs wrapper tests, the Go build, Go tests and vet, macOS race tests,
disabled live-provider profile checks, a clean npm install, HTML validation,
Chromium and WebKit accessibility tests, and the deterministic source scan.
Windows executes the concurrency tests in `go test ./...`; the Go race detector
is required only on macOS. The launcher rejects any Node version other than
`24.18.0` and any Go version other than `1.26.5`; an executable with the wrong
or malformed version is not treated as available.

The runtime-focused commands are:

```bash
npm run html:validate
npm run test:a11y
```

`html-validate` checks generated documents. Playwright runs the rendered
application in both Chromium and WebKit, and `@axe-core/playwright` evaluates
the covered states. The browser tests also exercise keyboard and focus
behavior, 320 CSS-pixel reflow, text spacing, local contrast contracts,
no-JavaScript behavior, and live-update focus safety. Forced-colors emulation
is Chromium-only. These engines are useful runtime coverage, but Playwright
Chromium is not stable Chrome with NVDA, and Playwright WebKit is not Safari
with VoiceOver.

Phase 4 adds dedicated Codex runtime coverage in
`tests/accessibility/codex-runtime.spec.mjs` and
`tests/accessibility/codex-runtime.axe.spec.mjs`. Those tests exercise named
operator approval and user-input groups, password treatment for secret
answers, deadline warnings, stale-response recovery focus, incompatible Codex
readiness, provider-tool failure text, and process-cleanup failure text. Each
fixed state is also included in the generated-HTML manifest.

The individual full-tree source scan is:

```bash
npm run a11y:source
```

The wrapper resolves the absolute `go/` directory, sets `A11Y_ALLOWED_ROOTS`
to exactly that path, and passes `--no-update-baseline`. It fails with the
scanner's exit `1` for new findings and exit `2` for scanner or setup errors.
It also detects and restores any unexpected baseline mutation before failing
closed. Baseline snapshot and comparison I/O errors are classified as exit `2`.
The wrapper rejects a pre-scan baseline that is a symlink or non-regular file,
and restoration uses an exclusive temporary regular file plus atomic rename so
it replaces a hostile symlink itself rather than writing through to its target.

## Pre-commit source scan

Activate the repository hook once from the repository root:

```bash
git config core.hooksPath .githooks
```

The hook scans only staged files below `go/web/` and `go/internal/web/`. It
passes each path as a separate changed-file argument and never updates the
baseline. Because the scanner reads worktree bytes, the hook rejects an
applicable partially staged file; stage the full file or move its unstaged edit
before committing. Applicable filenames containing commas or whitespace are
also rejected because scanner argument parsing cannot represent them safely.

## Reviewed source-scan floor

`.a11y/web/baseline.json` is the reviewed floor, not a suppression mechanism.
The current floor is empty. Do not update it merely to make a failing scan
green: review and fix each new finding first. The generated
`.a11y/a11y-check-web.yaml` is the scanner policy, and `.a11y/web/latest.*`
records the latest tool-produced report.

The recorded adoption scan initially reported three medium findings in 12
files: two focusable log containers without roles and an empty-state table
whose headers were not explicitly associated with its spanning data cell.
After semantic source fixes, the review-only scan reported zero actionable
findings. The scanner also reports unresolved contrast-analysis coverage in
`latest.md`; a zero finding count does not convert those skipped use sites into
verified contrast pairs.

## Optional supplementary runtime scan

A11yNow may be used as an additional runtime scan when it is available. Record
its tool-produced counts separately from axe, HTML validation, and
`a11y-check-web`, then triage each result against the rendered application.
A supplementary scanner does not replace either the deterministic gates above
or manual assistive-technology testing.

## What still requires review

The source scanner reports deterministic patterns in HTML, CSS, JavaScript,
and Go web sources. HTML validation checks generated markup. Playwright checks
rendered Chromium and WebKit pages with axe and focused interaction assertions.

Those results do not establish complete WCAG 2.2 A or AA conformance. The
following stable-browser sessions remain a Phase 5 manual requirement:

| Platform | Browser and assistive technology | Status |
| --- | --- | --- |
| Windows 11 | Stable Chrome with NVDA, including Windows forced-colors behavior | [Configuration and Windows Credential Manager slice recorded](accessibility/windows-chrome-nvda-2026-08-16.md); validation-summary and status-announcement fixes, manual text spacing, remaining routes, and complete-ledger reconciliation pending |
| macOS 14+ | Safari with VoiceOver | [Configuration and Keychain slice recorded](accessibility/macos-safari-configuration-2026-08-14.md); full speech, rotor, and cross-route validation pending |

Also perform keyboard-only and zoom review and native Keychain/Credential
Manager smoke tests. For every manual session, record the operating-system,
browser, and assistive-technology versions; tested routes and workflows;
results by WCAG criterion; and linked defects. Keep those conclusions separate
from scanner counts and automated engine results.
