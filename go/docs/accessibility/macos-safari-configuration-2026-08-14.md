# macOS Safari configuration accessibility slice — 2026-08-14

This report records a focused configuration and native-credential validation
slice. It is not a complete Safari/VoiceOver acceptance report and does not
establish WCAG 2.2 conformance for Symphony as a whole.

## Evidence boundary

The session used macOS accessibility APIs to inspect Safari's exposed tree and
to drive keyboard and VoiceOver interaction commands. VoiceOver was confirmed
enabled in macOS Accessibility settings, and `VO-Shift-Down Arrow` plus
`VO-Shift-Up Arrow` were used to enter and leave interaction with editable
controls. The session did not capture or transcribe VoiceOver speech, exercise
the rotor, or obtain a human tester's announcement-by-announcement results.
Those checks remain pending.

No bootstrap URL, session cookie, credential value, or Keychain password is
included in this report.

## Environment

| Item | Tested value |
| --- | --- |
| Date | 2026-08-14 |
| Source commit | `f16c98e8fe1ebcb371b5ca98f4ecfc9380812396` |
| macOS | 26.5.2, build 25F84 |
| Safari | 26.5.2, build 21624.2.5.11.8 |
| VoiceOver | 10, build 993 |
| Symphony mode | `configure` on an ephemeral loopback port |
| Credential backend | Native macOS Keychain |

The workflow and data directory were disposable files below `/private/tmp`.
The test instance used a test-specific workflow path, so its Keychain
service did not overlap another Symphony workflow.

## Results

| Area | Relevant WCAG 2.2 A/AA criteria | Result and observed evidence |
| --- | --- | --- |
| Page structure | 1.3.1, 2.4.2, 2.4.6, 4.1.2 | Pass for this route. Safari exposed the unique `Configuration — Symphony` title, primary navigation, one level-one Configuration heading, labelled field groups, provider subsections, labelled inputs, descriptions, a secure credential field, and named buttons. |
| Skip navigation | 2.1.1, 2.4.1, 2.4.3 | Pass. First Tab focused `Skip to main content`; Return moved focus to the main content container; the next Tab reached Provider. |
| Keyboard and VoiceOver form interaction | 2.1.1, 2.1.2, 2.4.3, 3.3.2, 4.1.2 | Pass for exercised controls. Provider, Owner, Project slug, structured-save, credential, and delete controls were reachable. VoiceOver interaction commands permitted text entry without losing the associated accessible name or value. |
| Validation error | 3.3.1, 3.3.2, 3.3.3, 4.1.3 | Pass. Submitting a blank GitHub owner exposed a focused, visibly outlined `There is a problem` summary. Its `is required` link moved focus to Owner, the entered correction was retained, and a successful resubmission exposed the `Configuration saved.` status and returned focus to `Save structured settings`. |
| Provider switch | 1.3.1, 3.2.2, 4.1.2 | Pass. The unsaved provider choice did not retarget credential controls: they remained bound to the current on-disk provider and digest. After Save, the selected scope, credential label, and credential state changed together to Linear; saving GitHub again restored the GitHub scope and Keychain state. This fail-closed behavior matched the credential-binding contract. |
| Credential replacement | 1.3.1, 2.1.1, 3.3.2, 4.1.2, 4.1.3 | Pass. A canary was entered only in the secure field. Safari's password-save prompt was declined. Symphony returned focus to `Replace credential`, exposed `Credential stored.`, cleared the field, and reported `Stored in macOS Keychain` after reload. A metadata-only `security find-generic-password` query confirmed the exact test service and account existed without reading its value. |
| Delete dialog | 1.3.1, 2.1.1, 2.1.2, 2.4.3, 2.4.7, 2.4.11, 4.1.2 | Pass. Safari exposed only the named `Delete credential?` dialog subtree, description, Cancel link, and Delete button. Initial focus was Cancel; Shift-Tab and Tab wrapped between the two controls; Escape closed the dialog and restored focus to the invoking Delete button. |
| Confirmed deletion | 2.4.3, 4.1.3 | Pass. After explicit operator confirmation, deletion reported `Credential deleted.`, restored focus to `Delete credential`, and changed the state to `Not configured`. The metadata-only Keychain query then exited 44 with item-not-found. |
| Zoom and reflow | 1.4.4, 1.4.10 | Partial interactive evidence. At Safari's maximum page zoom, Zoom In became unavailable, the structured fields remained readable and operable, and Safari exposed only the vertical page scrollbar. The restored-width window also retained a single-column form. Safari capped this session at 300%, so this is not a literal 400% manual result; the existing automated 320-CSS-pixel WebKit coverage remains separate. |
| Visible focus | 2.4.7, 2.4.11 | Pass for exercised controls. Skip navigation, error summary, linked invalid field, save buttons, credential controls, and dialog controls retained a visible, unobscured focus indicator in Safari. |

## Defects and follow-up

No product defect was confirmed in this focused slice. The following work is
still required before changing the macOS matrix to complete:

- a human-recorded VoiceOver speech and rotor session;
- the remaining routes and run-mode workflows listed in the Phase 5 manual
  matrix;
- literal 400% or equivalent 320-CSS-pixel manual reflow evidence where the
  stable browser and display configuration permit it; and
- reconciliation against the complete WCAG 2.2 A/AA ledger.

The Keychain canary was removed before the session ended. This report makes no
claim about Windows Credential Manager, Chrome/NVDA, provider-network access,
or release readiness.
