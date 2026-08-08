# Accessibility testing

Symphony's Phase 1 accessibility gates support macOS 14+ and Windows 11. The
automated checks complement one another; none is a complete WCAG conformance
assessment.

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

Then run the complete platform-aware Phase 1 verification entry point from
`go/`:

```bash
npm run verify
```

It runs wrapper tests, Go tests and vet, macOS race tests, a clean npm install,
HTML validation, Chromium and WebKit accessibility tests, and the deterministic
source scan. Windows executes the concurrency tests in `go test ./...`; the Go
race detector is required only on macOS. The launcher rejects any Node version
other than `24.18.0` and any Go version other than `1.26.5`; an executable with
the wrong or malformed version is not treated as available. No Linux support
is claimed.

The individual source scan is:

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
The Phase 1 floor is empty. Do not update it merely to make a failing scan
green: review and fix each new finding first. The generated
`.a11y/a11y-check-web.yaml` is the scanner policy, and `.a11y/web/latest.*`
records the latest tool-produced report.

The adoption scan initially reported three medium findings in 12 files: two
focusable log containers without roles and an empty-state table whose headers
were not explicitly associated with its spanning data cell. After semantic
source fixes, the review-only scan reported zero actionable findings. The
scanner also reports unresolved contrast-analysis coverage in `latest.md`; a
zero finding count does not convert those skipped use sites into verified
contrast pairs.

## What still requires review

The source scanner reports deterministic patterns in HTML, CSS, JavaScript,
and Go web sources. HTML validation checks generated markup. Playwright checks
real Chromium and WebKit pages with axe and the Phase 1 keyboard, focus,
reflow, contrast-token, and no-JavaScript assertions.

Those results do not establish complete WCAG 2.2 AA conformance. Before a
release, separately perform VoiceOver testing on macOS, NVDA and forced-colors
testing on Windows, keyboard and zoom review, and native Keychain/Credential
Manager smoke tests. Record those manual conclusions separately from scanner
counts.
