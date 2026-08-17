# Windows Chrome/NVDA configuration accessibility slice — 2026-08-16

This report records a focused Windows configuration and native-credential
validation slice. It is not a complete Windows acceptance report and does not
establish WCAG 2.2 conformance for Symphony as a whole.

## Evidence boundary

The session used stable Chrome with NVDA for keyboard interaction. Actual NVDA
speech evidence came from NVDA's `speech.speech.speak` debug stream: these are
the utterance commands NVDA sent to its synthesizer, not text inferred from the
DOM or accessibility tree. No audio recording was retained. Debug logging was
disabled before either disposable credential value was entered and re-enabled
only after the protected field had been cleared.

Chrome DOM snapshots and Windows UI Automation inspection were used separately
to confirm roles, names, current focus, and selected state. Visual inspection
was used for focus visibility, 400% zoom, and the Windows contrast theme. Those
inspection results are labelled separately below and are not represented as
NVDA speech.

No bootstrap URL, session cookie, credential value, or Windows Credential
Manager secret is included in this report, its git history, or retained test
artifacts.

## Environment

| Item | Tested value |
| --- | --- |
| Date | 2026-08-16 |
| Source commit | `692bcddcf556bab546a708bcfc7594a161fee336` |
| Windows | Windows 11 Pro 25H2, build `26200.9168` |
| Chrome | Stable `151.0.7922.138` |
| NVDA | `2026.1.1`, file version `2026.1.1.55980` |
| NVDA synthesizer | Windows OneCore |
| Symphony mode | `configure` on an ephemeral loopback port |
| Credential backend | Native Windows Credential Manager |
| Repository toolchain | Go `1.26.5`; Node.js `24.18.0` |

Windows' compatibility registry identified the product as `Windows 10 Pro`,
while the 25H2 release and build identify this as the tested Windows 11 system.
The workflow, server data, module cache, build cache, and downloaded toolchains
were isolated below a disposable repository-local directory.

## Results

| Area | Relevant WCAG 2.2 A/AA criteria | Result and observed evidence |
| --- | --- | --- |
| Page structure | 1.3.1, 2.4.2, 2.4.6, 4.1.2 | Pass for this route. DOM and UI Automation inspection exposed the unique `Configuration — Symphony` title, primary navigation, one level-one Configuration heading, labelled groups, provider subsections, descriptions, named controls, a protected credential field, and a named credential region. |
| Skip navigation | 2.1.1, 2.4.1, 2.4.3 | Pass. In NVDA focus mode, Shift-Tab from Overview reached `Skip to main content`. NVDA spoke `Skip to main content, link, same page`; Enter moved DOM focus to the main landmark, and the next Tab reached the Provider combo box. |
| NVDA browse and focus interaction | 2.1.1, 2.1.2, 2.4.3, 3.3.2, 4.1.2 | Pass for exercised controls. NVDA spoke control names, roles, values, and associated descriptions. Examples include `Provider, combo box, GitHub, collapsed`, `Project slug, edit`, `New github credential, edit, protected`, and `Replace credential, button`. All product interactions in this session used the keyboard. |
| Validation summary and focus | 3.3.1, 3.3.2, 3.3.3, 4.1.3 | **Fail.** Saving an intentionally incomplete Linear scope focused the `There is a problem` alert, retained entered values, and exposed a link to `#linear-project-slug`. However, both the DOM name and NVDA speech were only `is required`, without the affected field name. Activating the link did not move focus from the alert to Project slug in stable Chrome. |
| Successful-save status | 3.3.1, 4.1.3 | **Fail.** `Configuration saved.` was present as a DOM `status`, and focus returned to `Save structured settings`, but the status was not automatically sent through NVDA's speech pipeline after the redirect. NVDA announced the returned button and surrounding content instead. |
| Provider switch | 1.3.1, 3.2.2, 4.1.2 | Pass. An unsaved Linear selection left the saved GitHub scope, `New github credential` label, and stored GitHub state unchanged. After Save, the selected scope and credential controls changed together to Linear and reported `Not configured`. Saving GitHub again restored the GitHub label and `Stored in Windows Credential Manager` state. DOM inspection supplied the state comparison; NVDA supplied the control announcements. |
| Credential replacement | 1.3.1, 2.1.1, 3.3.2, 4.1.2, 4.1.3 | Pass except for the status-announcement defect above. Two generated disposable values exercised initial storage and replacement. They were pasted only into the protected field while NVDA debug logging was off; the clipboard was cleared immediately after each paste. The field cleared, focus returned to `Replace credential`, the DOM exposed `Credential stored.`, and a reload reported `Stored in Windows Credential Manager`. NVDA browse navigation spoke `Current state: Stored in Windows Credential Manager`. A metadata-only `cmdkey` query confirmed the exact generic target, account `tracker/github`, and local-machine persistence without reading the value. |
| Delete dialog | 1.3.1, 2.1.1, 2.1.2, 2.4.3, 2.4.7, 2.4.11, 4.1.2 | Pass. The named `Delete credential?` dialog exposed its description, Cancel link, and Delete button. NVDA spoke the dialog name and description. DOM focus began on Cancel; Shift-Tab and Tab wrapped between Delete and Cancel. Escape closed the dialog and restored focus to the invoking Delete button. |
| Deletion and cleanup | 2.4.3, 4.1.3 | Cleanup pass; confirmation-protocol evidence invalid. During the intended Cancel check, NVDA's continuous-reading virtual cursor advanced from the DOM-focused Cancel link to the destructive button before Enter. The disposable canary was therefore deleted before the planned separate action-time confirmation. Symphony returned focus to `Delete credential`, exposed `Credential deleted.`, and changed the state to `Not configured`, but the status was not automatically spoken. A metadata-only elevated `cmdkey` query returned `* NONE *`. No canary was recreated. |
| Windows forced colors | 1.4.3, 1.4.11, 2.4.7, 2.4.11 | Pass for the exercised configuration controls. Windows' Aquatic contrast theme was applied through Settings. Chrome adopted system colors; field and group boundaries remained distinct; and the focused Replace button retained a prominent system focus indicator. UI Automation retained the main landmark and Credential management region. NVDA spoke the protected field and Replace button while the contrast theme was active. Contrast mode was then disabled and the prior Windows dark mode was restored; registry metadata confirmed high contrast off and both system and app light-theme flags returned to `0`. |
| Zoom and reflow | 1.4.4, 1.4.10 | Pass for literal 400% Chrome page zoom. The single-column configuration remained readable and keyboard-operable. Visual inspection at the document end showed only the vertical page scrollbar and no horizontal page scrollbar. Zoom was restored to 100%. |
| Text spacing | 1.4.12 | Automated evidence only. The repository Playwright accessibility suite applies the documented text-spacing overrides. No separate manual user-style injection was performed in stable Chrome, so this row does not claim manual coverage. |
| Visible focus | 2.4.7, 2.4.11 | Pass for the exercised controls in normal and forced-colors modes. Skip navigation, Provider, described fields, save controls, credential controls, and dialog controls retained visible focus. |

## Automated verification

Commands ran from `go/` unless a different directory is shown. Exit codes and
counts below are the observed Windows results; unrelated main-branch failures
are not represented as passing evidence for this slice.

| Command | Exit | Result |
| --- | --- | --- |
| `go build -o <temporary-path>/symphony.exe ./cmd/symphony` | 0 | Built with Go 1.26.5. |
| `go vet ./...` | 0 | Passed. |
| `go test ./...` | 1 | All packages passed except `internal/codex`: `TestProcessStopTerminatesWindowsDescendantsAndNotUnrelatedProcess` reported that its unrelated helper process had terminated. An isolated rerun failed the same way in this Codex Windows host process-containment environment. |
| `go test -v -tags=integration_live -count=1 -run '^TestGitHubLiveProfile$' ./internal/tracker/github` | 0 | Passed with the exact disabled-profile sentinel `SKIPPED: GitHub live profile not enabled`; no provider credential was supplied. |
| `go test -v -tags=integration_live -count=1 -run '^TestLinearLiveProfile$' ./internal/tracker/linear` | 0 | Passed with the exact disabled-profile sentinel `SKIPPED: Linear live profile not enabled`; no provider credential was supplied. |
| `npm run test:wrappers` | 1 | 74 passed and 1 failed because this Windows account lacks the privilege to create the test's symbolic link (`EPERM`). The canonical `npm run verify` stopped at the same first failing gate after 67 of its then-selected 68 wrapper tests passed. |
| `npm run html:validate` | 0 | 24 passed across Chromium and WebKit. |
| `npx playwright test tests/accessibility/configuration.spec.mjs` | 0 | 20 passed across Chromium and WebKit, including axe, validation retention/focus, credential lifecycle, dialog focus containment/restoration, and no-JavaScript coverage. |
| `npm run test:a11y` | 1 | 217 passed, 2 skipped, and 3 failed. Two Chromium/WebKit failures hard-code UTC while the Windows workstation correctly rendered local EDT; the WebKit no-JavaScript log-filter test did not retain the INFO row. These base-commit defects are tracked separately below. |
| `npm run a11y:source` | 0 | Pinned `a11y-check-web` 0.3.1 scanned 15 files with 0 skipped and 0 actionable findings; the reviewed baseline was not updated. |
| `git diff --check` (repository root) | 0 | Passed; line-ending warnings for generated scanner output were removed during cleanup. |
| `make -C elixir all` (repository root) | 1 | Not runnable because GNU Make is not installed on this Windows workstation. No Elixir result is claimed. |

The Go race detector is intentionally not an applicable Windows gate in the
repository verifier. The focused configuration suite is the relevant automated
gate for this report and passed completely. The full-suite failures remain
visible here so this evidence does not hide base-commit or host limitations.

## Confirmed defects and follow-up

This evidence PR intentionally contains no product fix. The focused slice found
two configuration accessibility defects that require separate one-feature
work:

- [Validation-summary links need the invalid field's name and must move Chrome
  focus to that field when activated](https://github.com/coryj627/symphony/issues/8); and
- [redirect-based save, credential-store, and credential-delete statuses need a
  delivery mechanism that NVDA announces automatically](https://github.com/coryj627/symphony/issues/9)
  without forcing the operator to browse backward for the message.

The full automated run also reproduced two unrelated base-commit failures,
which are excluded from this evidence-only PR and tracked independently:

- [the Windows WebKit no-JavaScript log-filter test drops the INFO
  row](https://github.com/coryj627/symphony/issues/10); and
- [the queue local-time accessibility test hard-codes UTC on a non-UTC Windows
  workstation](https://github.com/coryj627/symphony/issues/11).

The accidental canary deletion described above is an operator-procedure failure,
not evidence that the dialog's initial DOM focus was incorrect. A second,
secret-free dialog pass confirmed initial Cancel focus, two-control focus
wrapping, Escape cancellation, and focus restoration.

The following work remains before changing the Windows matrix to complete:

- fixes and Windows retests for the two defects above;
- remaining routes and run-mode workflows in the Phase 5 manual matrix;
- a manual text-spacing override session in stable Chrome; and
- reconciliation against the complete WCAG 2.2 A/AA ledger.

The Windows Credential Manager canary is permanently absent. The clipboard is
empty, NVDA is back to normal logging, Windows contrast mode is off, the prior
dark mode is restored, and no disposable test process or directory is intended
to remain after repository verification. This report makes no claim about
macOS, provider-network access, complete WCAG 2.2 conformance, or release
readiness.
