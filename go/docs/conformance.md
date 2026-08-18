# Upstream conformance traceability

Symphony maintains a machine-readable ledger for every bullet in `SPEC.md`
Sections 17.1 through 17.8 and 18.1 through 18.3. The ledger records the exact
normalized source text and its SHA-256, a stable row ID, a profile, a status,
and exact evidence references. It intentionally does not claim full WCAG 2.2
conformance or production readiness.

Run the deterministic trace gate from the `go` directory:

```powershell
npm run conformance:upstream
```

The gate independently extracts the governed SPEC bullets, checks order and
hashes, enumerates referenced Go tests and Playwright titles, and verifies
report anchors. Missing, duplicated, stale, malformed, or nonexistent evidence
fails the command.

The reviewed source pins for this ledger revision are:

- `SPEC.md`: `adb93eb2349ccbf39a8ca389ac29c0f2034f1204776319b4535a2f9424f4322d`
- approved cross-platform design: `801a8ab935e78faaa6a89794d623674684bff3a7f237699a251be5c802c09c00`

A source change intentionally breaks the pin and affected row hashes until a
reviewer re-evaluates the requirement-to-evidence mapping.

## Status meanings

- `pass` means the referenced deterministic evidence exists and the implemented
  behavior is claimed for that individual row.
- `not_implemented_optional` is limited to approved optional capabilities that
  Symphony does not ship. It is not a pass.
- `skipped_real_profile` is limited to Sections 17.8 and 18.3 when credentialed
  or target-host evidence has not been run. It is not a pass and does not
  establish production readiness.

## Deferred optional capabilities

The approved design does not ship these four optional capabilities, so the
ledger keeps them visible as `not_implemented_optional`:

- workspace population or synchronization;
- retry-queue and session-metadata restoration after process restart;
- workflow-front-matter configuration of observability settings; and
- a generic semantic CRUD tool layer that replaces provider-native tools.

`TestDeferredExtensionsRemainUnclaimed` fails if another row is deferred or if
one of these rows is accidentally presented as implemented.

## Real integration profile

Credentialed GitHub and Linear smoke tests remain opt-in and use the providers'
documented secret mechanisms. Run them only against dedicated, isolated test
scope and follow the provider cleanup rules:

```powershell
go test -v -tags=integration_live -count=1 -timeout=2m ./internal/tracker/github ./internal/tracker/linear
```

When disabled, the live suites must report their exact `SKIPPED` sentinel. When
explicitly enabled, missing prerequisites or test failures fail the job. The
target-host checks in Section 18.3 likewise remain `skipped_real_profile` until
they are recorded on every supported host required by that row.
