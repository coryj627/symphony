# Windows Chrome/NVDA Phase 5 accessibility validation — 2026-08-17

This report records Symphony's Phase 5 Windows 11 validation with stable Chrome,
NVDA, Windows forced colors, and native Windows Credential Manager. It covers the
configuration workflows requested in issue 7 and the wider operator routes used
to reconcile the WCAG 2.2 Level A/AA ledger.

This is test evidence, not a claim of complete WCAG 2.2 conformance or release
readiness. Criteria marked not applicable or not assessed below remain explicit
so that automated results and a focused manual session are not mistaken for a
formal conformance evaluation.

## Evidence boundary and method

All product interaction in the manual session was keyboard-only. Stable Chrome
was driven with ordinary keyboard commands while NVDA was running. Actual NVDA
speech evidence came from NVDA's `speech.speech.speak` debug stream: these are
utterance commands NVDA sent to its synthesizer, not text inferred from the DOM,
Chrome accessibility tree, or Windows UI Automation. No audio recording was
retained.

DOM, Chrome accessibility-tree, and UI Automation inspection were used
separately to confirm roles, names, states, relationships, and focus. Visual
inspection was used for focus visibility, reflow, text spacing, and the Windows
contrast theme. The results below label those sources separately.

Only generated fixture data and one disposable credential canary were used. No
provider request was sent. NVDA debug logging was disabled before either canary
value was entered and re-enabled only after the protected field was empty. No
bootstrap URL, session cookie, credential value, token, or Windows Credential
Manager secret is included in this report, git history, screenshots, or retained
artifacts.

## Environment

| Item | Tested value |
| --- | --- |
| Date | 2026-08-17 |
| Discovery source | `2d9babe693e7c67d09f12738e30d01cf97a55d42` |
| Status-announcement fix | `783ea1f` |
| Final merged-main regression source | `c1d67e6` |
| Windows | Windows 11 Pro 25H2, build `26200.9168` |
| Chrome | Stable `151.0.7922.138` |
| NVDA | `2026.1.1`, file version `2026.1.1.55980` |
| NVDA synthesizer | Windows OneCore |
| Credential backend | Native Windows Credential Manager |
| Repository toolchain | Go `1.26.5`; Node.js `24.18.0` |

The application, workflow, browser profile, downloaded toolchains, caches, and
NVDA diagnostic output were isolated in a disposable local environment. The
temporary server was loopback-only.

## Manual results

| Area | Actual NVDA speech evidence | Separate DOM, accessibility-tree, or visual evidence |
| --- | --- | --- |
| Overview and navigation | NVDA spoke the page title, skip link, banner, primary navigation, current page, main landmark, level-one heading, controls, and status content. | One page title, banner, navigation, main landmark, level-one heading, and named regions were present. Skip-link activation moved focus to main content. |
| Scheduler controls | On merged main, keyboard activation produced exactly one `Scheduler start requested.` utterance and exactly one `Scheduler stop requested.` utterance. | Each redirect exposed the matching status, updated Running/Paused state, enabled the appropriate control, and restored visible focus to the corresponding Stop/Start button. |
| Issue list and detail | NVDA spoke the Issues title, current navigation item, heading, search field, State combo box, table caption/rows/columns, issue-detail heading, named sections, metadata, and issue link. | Table semantics, filtering controls, issue metadata, named regions, and contextual links were present. |
| Activity and logs | NVDA spoke the page titles, current navigation items, headings, recent-activity list, local-time text, log count, search field, Level combo box, Apply button, and the long log table's 101 rows, three columns, and caption. | Activity timestamps retained exact RFC 3339 values while rendering local EDT. The long-log fixture remained structurally navigable. |
| Operator request | NVDA spoke the named request, grouped radio controls, required description, selected state, protected answer field, and Submit button. On the fix commit later merged unchanged, it spoke `Operator response submitted.` after keyboard submission. | Six request headings, grouped choices, the protected password input, action controls, restored focus, and a status region were present. Merged-main automated coverage exercises the same redirect result. |
| Refresh | On the fix commit later merged unchanged, NVDA spoke `Refresh requested.` after keyboard activation. | The result redirect exposed the status and restored action focus. Merged-main automated coverage exercises this redirect result. |
| Error document | NVDA spoke the error-page title, skip link, banner, navigation, main landmark, level-one heading, and message. | The generated not-found document retained the standard landmark and bypass structure. |
| Configuration validation | NVDA spoke `There is a problem` and `Project slug: is required`; activating the error link announced Project slug as invalid, required, and blank. | The alert received focus, linked to the invalid field, preserved entered values, and activation moved DOM focus to that field. |
| Provider switching and save | NVDA announced the selected provider and the corresponding provider-specific controls. It spoke `Configuration saved.` after a valid keyboard save. | Unsaved values survived switching away and back. A saved provider change updated its scope and credential controls together. Save restored visible focus to its trigger. |
| Credential replacement | NVDA identified the new-credential field as protected and spoke the stored state. It spoke `Credential stored.` after the safe replay described below. | The field used password semantics and `autocomplete="new-password"`, cleared after submission, and never exposed a stored value. Replacement preserved the same metadata-only target and restored trigger focus. |
| Deletion dialog and cleanup | NVDA spoke the `Delete credential?` dialog name and description, then spoke `Credential deleted.` after the approved deletion. | Focus began on Cancel, Tab and Shift-Tab wrapped between the two dialog actions, and Escape and Cancel restored focus to the invoking Delete button. Confirmed deletion restored trigger focus and changed the state to Not configured. |

The merged-main scheduler regression was repeated after pull request 17 merged.
The refresh and operator-response utterances were captured on its fix commit,
which is the same product commit contained in merged main; merged-main browser
tests cover all four redirect messages. This distinction avoids representing a
DOM status assertion as fresh NVDA speech.

## Visual adaptation results

| Area | Result and evidence |
| --- | --- |
| Windows forced colors | Pass for the exercised configuration workflow. The Windows Aquatic contrast theme was enabled at the operating-system level. Chrome adopted system colors; text and controls remained readable; field, group, and dialog boundaries remained distinguishable; the protected field remained recognizable; and the keyboard focus indicator remained visible. The theme was disabled afterward and the prior Windows dark mode was restored. |
| Text resize and reflow | Pass for the exercised routes at 200% stable-Chrome zoom. No content, action, or status was clipped or overlapped, and no horizontal page scrolling was introduced. Zoom was restored to 100%. |
| Text spacing | Pass for the exercised visible paragraphs and controls after a keyboard-applied user-style override of 1.5 line height, 0.12em letter spacing, 0.16em word spacing, and 2em paragraph spacing. Content and controls remained visible and no horizontal page overflow was introduced. |
| Visible and unobscured focus | Pass for exercised skip navigation, navigation links, filters, table links, provider and configuration fields, scheduler/actions, credential controls, and dialog actions in normal and forced-colors modes. |

## Native credential lifecycle and cleanup

The Windows Credential Manager exercise used one randomly generated disposable
canary and the exact Symphony generic-credential target derived for the
ephemeral workflow. No credential value was displayed or queried.

1. A metadata-only `cmdkey` query confirmed the exact target was initially
   absent.
2. The first disposable value was entered only into the protected field while
   NVDA debug logging was off. The field cleared after storage.
3. A metadata-only query confirmed one generic credential for account
   `tracker/github` with local-machine persistence.
4. A second generated disposable value replaced the first through the same
   protected field. The first value was not reused.
5. After the protected field was empty and logging was safe again, replaying the
   result document produced the actual `Credential stored.` NVDA utterance.
   This replay validates result delivery, not secret entry or the original
   redirect timing.
6. After explicit approval, the credential was permanently deleted through the
   keyboard-operated confirmation dialog. NVDA spoke `Credential deleted.`.
7. Repeated metadata-only `cmdkey` queries returned `* NONE *` for the exact
   target. The canary is absent and was not recreated.

The clipboard was cleared after each protected-field entry. No canary or value
appears in this report, logs retained for the PR, commits, or screenshots.

## Complete WCAG 2.2 Level A/AA ledger reconciliation

The criterion list follows the W3C [WCAG 2.2 Recommendation](https://www.w3.org/TR/WCAG22/).
`Observed pass` means this Windows session supplied direct manual evidence for
the tested content. `Automated pass` is limited to the repository fixtures and
rules exercised by the named checks. `Not applicable` describes the tested
content, not every possible future Symphony extension. `Not assessed` is an
explicit gap and prevents a conformance claim.

| Criterion | Level | Disposition for this Windows slice | Evidence or limitation |
| --- | --- | --- | --- |
| 1.1.1 Non-text Content | A | Automated pass | HTML, axe, and source checks found no exposed non-text-content failure in covered documents; the exercised operator UI contains no meaningful image-only content. |
| 1.2.1 Audio-only and Video-only (Prerecorded) | A | Not applicable | No prerecorded media. |
| 1.2.2 Captions (Prerecorded) | A | Not applicable | No prerecorded media. |
| 1.2.3 Audio Description or Media Alternative (Prerecorded) | A | Not applicable | No prerecorded media. |
| 1.2.4 Captions (Live) | AA | Not applicable | No live media. |
| 1.2.5 Audio Description (Prerecorded) | AA | Not applicable | No prerecorded media. |
| 1.3.1 Info and Relationships | A | Observed pass | Landmarks, headings, lists, tables, groups, descriptions, validation, and dialog relationships were exposed to NVDA and inspection. |
| 1.3.2 Meaningful Sequence | A | Observed pass | NVDA browse order and keyboard focus order remained meaningful on all exercised routes. |
| 1.3.3 Sensory Characteristics | A | Observed pass | Instructions and states did not depend only on visual position, shape, or sound. |
| 1.3.4 Orientation | AA | Not applicable | The desktop UI does not restrict display orientation. |
| 1.3.5 Identify Input Purpose | AA | Observed pass | The credential input exposed protected password semantics and `autocomplete="new-password"`; no other user-data purpose field was present. |
| 1.4.1 Use of Color | A | Observed pass | State, errors, controls, and focus retained text, semantics, or non-color indicators, including in forced colors. |
| 1.4.2 Audio Control | A | Not applicable | Symphony emitted no automatic audio. NVDA speech is user-agent output. |
| 1.4.3 Contrast (Minimum) | AA | Automated and sampled visual pass | Axe evaluated covered rendered colors; stable-Chrome normal and Windows forced-colors states remained readable. This was not a manual measurement of every color pair. |
| 1.4.4 Resize Text | AA | Observed pass | Exercised routes remained usable at 200% stable-Chrome zoom. |
| 1.4.5 Images of Text | AA | Not applicable | No images of text were present in the exercised UI. |
| 1.4.10 Reflow | AA | Observed pass | No two-dimensional page scrolling or content loss at 200% zoom; automated fixtures also test 320 CSS pixels. |
| 1.4.11 Non-text Contrast | AA | Automated and sampled visual pass | Controls, boundaries, state, and focus remained distinguishable in normal and forced-colors modes; not every possible state was manually measured. |
| 1.4.12 Text Spacing | AA | Observed pass | The documented spacing overrides caused no loss, overlap, or horizontal overflow in exercised content. |
| 1.4.13 Content on Hover or Focus | AA | Not applicable | Exercised controls did not reveal additional hover- or focus-triggered content. |
| 2.1.1 Keyboard | A | Observed pass | Every manual product interaction used the keyboard. |
| 2.1.2 No Keyboard Trap | A | Observed pass | Routes, controls, and the modal dialog remained escapable; dialog focus containment behaved as intended. |
| 2.1.4 Character Key Shortcuts | A | Not applicable | No single-character product shortcuts were present. |
| 2.2.1 Timing Adjustable | A | Not applicable | The exercised UI imposed no user-response time limit. |
| 2.2.2 Pause, Stop, Hide | A | Not applicable | No automatically moving, blinking, scrolling, or auto-updating content met the criterion's conditions. |
| 2.3.1 Three Flashes or Below Threshold | A | Not applicable | No flashing content. |
| 2.4.1 Bypass Blocks | A | Observed pass | NVDA announced the skip link and activation moved focus into main content. |
| 2.4.2 Page Titled | A | Observed pass | NVDA announced unique, descriptive titles on exercised documents. |
| 2.4.3 Focus Order | A | Observed pass | Keyboard order was logical; redirects, validation, dialog cancellation, and actions restored focus to the intended context. |
| 2.4.4 Link Purpose (In Context) | A | Observed pass | Navigation, issue, validation-summary, skip, and dialog links were understandable with their programmatic context. |
| 2.4.5 Multiple Ways | AA | Observed pass for tested fixtures | Primary navigation, issue tables/search, and contextual issue links provided multiple location paths where the criterion applied. |
| 2.4.6 Headings and Labels | AA | Observed pass | NVDA announced descriptive page headings, section headings, table captions, fields, groups, and action labels. |
| 2.4.7 Focus Visible | AA | Observed pass | Keyboard focus was visible on exercised controls in normal and forced-colors modes. |
| 2.4.11 Focus Not Obscured (Minimum) | AA | Observed pass | Focused controls remained at least partially visible during the exercised keyboard workflows. |
| 2.5.1 Pointer Gestures | A | Not applicable | No multipoint or path-based gestures. |
| 2.5.2 Pointer Cancellation | A | Not assessed | This session was intentionally keyboard-only and did not perform a systematic pointer-event review. |
| 2.5.3 Label in Name | A | Observed pass | Inspected interactive names contained their visible labels. |
| 2.5.4 Motion Actuation | A | Not applicable | No motion-operated functionality. |
| 2.5.7 Dragging Movements | AA | Not applicable | No dragging interaction. |
| 2.5.8 Target Size (Minimum) | AA | Not assessed | Target geometry was not systematically measured in stable Chrome. |
| 3.1.1 Language of Page | A | Automated pass | Generated documents expose the page language and pass HTML validation. |
| 3.1.2 Language of Parts | AA | Not applicable | No content changed natural language within the exercised fixtures. |
| 3.2.1 On Focus | A | Observed pass | Keyboard focus alone did not trigger an unexpected context change. |
| 3.2.2 On Input | A | Observed pass | Provider selection changed its explicitly associated fields while preserving values; saves and destructive changes required activation. |
| 3.2.3 Consistent Navigation | AA | Observed pass | Primary navigation remained consistently ordered and named across routes and the error document. |
| 3.2.4 Consistent Identification | AA | Observed pass | Repeated controls and statuses retained consistent names and purposes. |
| 3.2.6 Consistent Help | A | Not applicable | No repeated help mechanism was present. |
| 3.3.1 Error Identification | A | Observed pass | The error summary identified `Project slug: is required` in text and through actual NVDA speech. |
| 3.3.2 Labels or Instructions | A | Observed pass | NVDA and inspection exposed labels, descriptions, required state, and protected-field purpose. |
| 3.3.3 Error Suggestion | AA | Observed pass | The required-field error stated the correction and linked/focused the affected field. |
| 3.3.4 Error Prevention (Legal, Financial, Data) | AA | Observed pass for credential deletion | The irreversible credential deletion required a named confirmation dialog and supported Cancel and Escape before commitment. No legal or financial transaction was present. |
| 3.3.7 Redundant Entry | A | Not applicable | The exercised process did not require previously entered information to be entered again; replacement intentionally accepts a new value. |
| 3.3.8 Accessible Authentication (Minimum) | AA | Not applicable | The tested local status surface has no user-authentication process. Credential storage configures a tracker adapter; it does not authenticate the operator to Symphony. |
| 4.1.2 Name, Role, Value | A | Observed pass | NVDA and separate inspection exposed names, roles, values, selected/expanded/current states, descriptions, dialog semantics, and status regions. |
| 4.1.3 Status Messages | AA | Observed pass | NVDA spoke validation and configuration statuses plus refresh, scheduler, credential, and operator-response results without moving focus to the status. |

The two explicit `Not assessed` rows, sampled rather than exhaustive contrast
review, fixture-based automation, and limited browser/AT combination mean this
ledger is a reconciliation of available evidence, not a WCAG conformance claim.

## Defects and separate fixes

The first Windows configuration report found two product defects. They were
fixed together only after explicit maintainer direction in pull request 13:

- issues 8 and 9: invalid-field context/focus and redirect-result delivery to
  NVDA, fixed by `5120332`;
- issue 10: Windows WebKit no-JavaScript log filtering, fixed by `96e26cf` in
  pull request 14; and
- issue 11: local-time accessibility assertions on non-UTC Windows systems,
  fixed by `018cb2f` and `99b4c5b` in pull request 15.

The wider manual pass then found one additional product defect: Overview action
results were present as DOM statuses but were not delivered as actual NVDA
speech after redirects. Issue 16 was fixed separately in pull request 17 by
`783ea1f`. This evidence-only change contains no product, CI, refactor, or
generated update.

## Automated verification

Commands ran from `go/` unless otherwise shown. These results verify the final
merged source and this evidence documentation; they do not replace the manual
results above.

| Command | Result |
| --- | --- |
| `go build ./cmd/symphony` | Passed with Go 1.26.5. |
| `go test ./...` | Passed. |
| `go vet ./...` | Passed. |
| Disabled GitHub and Linear live-profile tests | Passed with each profile reporting its exact disabled sentinel; no provider credential was supplied. |
| `npm ci` | Passed with Node.js 24.18.0. |
| `npm run html:validate` | Passed: 24 tests across Chromium and WebKit. |
| `npm run test:a11y` | Passed: 222 tests, with two intentionally skipped. |
| Direct pinned `a11y-check-web` 0.3.1 scan using the repository wrapper's exact root, authorization, review-only, and output arguments | Passed: 15 files scanned, zero skipped files, and zero actionable findings; a before/after SHA-256 comparison confirmed the reviewed baseline remained unchanged. |
| `npm run verify` | Host-limited: 67 of 68 wrapper tests passed before the test that creates a symbolic link failed with Windows `EPERM`; the verifier stopped at that first gate as designed. |
| `git diff --check` (repository root) | Passed. |

The canonical `npm run verify` could not complete on this workstation because
the Windows account cannot create the wrapper-test symbolic link (`EPERM`). The
installed scanner's npm-generated command shim also was not directly executable
by Node's Windows `spawnSync`, so the pinned CLI entry point was invoked directly
with the wrapper's exact arguments and a separate baseline-integrity check. The
remaining applicable gates were run individually as listed above. GNU Make and
Mix are not installed, so `make -C elixir all` is not claimed as a local result;
the evidence PR's Windows and Elixir CI jobs are the authoritative coverage for
those host-independent gates.

## Final limitations and state restoration

This session used one Windows build, one stable Chrome build, one NVDA build and
synthesizer, generated fixtures, and a loopback-only local server. It did not
exercise a real tracker network, other browsers or screen readers, every
possible data/state combination, systematic pointer cancellation, systematic
target-size measurement, or a formal contrast measurement of every color pair.

The Windows Credential Manager canary is permanently absent. Windows contrast
mode is off, the prior dark mode and 100% browser zoom are restored, and the
clipboard, disposable processes, browser profile, logs, test workflow, caches,
and downloaded toolchains are removed after CI completes. No complete WCAG 2.2
conformance or release-readiness claim is made.
