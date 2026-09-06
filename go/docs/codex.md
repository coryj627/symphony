# Codex runtime

Symphony run mode launches one local Codex app-server process for each issue
attempt. Configure mode does not start the scheduler or Codex. The reviewed
protocol snapshot establishes Codex CLI `0.144.1` as the minimum supported
version. Newer versions are accepted without an allowlist update or schema
regeneration. Version ordering follows semantic versioning, including prerelease
ordering and build metadata. A version below the minimum, an unrecognized
version, or a failed initialization leaves the browser UI available with an
explicit Unavailable state. For an older CLI, update Codex, confirm
`codex --version`, and restart that Symphony instance.

The committed schema and digest verify Symphony's bundled protocol baseline;
they are not a hash or certification of the installed server. Additional
response fields and unknown notifications are tolerated. Required fields,
message types, request validation, approval decisions, and sandbox restrictions
remain enforced. A successful preflight proves initialization, not every later
operation: a breaking change in a thread, turn, or server request is reported
when that operation is exercised. Future releases are not assumed to be
compatible merely because their version is higher.

This minimum-version policy supersedes the exact-version runtime restriction
in the original Phase 4 design. Deterministic fixtures and the baseline CI
smoke may still pin `0.144.1` to keep coverage of the minimum reproducible.

## Workflow configuration

The `codex` front-matter section controls the local child process:

```yaml
codex:
  command: codex app-server
  approval_policy: on-request
  thread_sandbox: workspace-write
  turn_timeout_ms: 3600000
  read_timeout_ms: 5000
  stall_timeout_ms: 300000
```

The command is executed by a native Bash in the issue workspace. Symphony
accepts only the `workspace-write` thread sandbox and supplies exactly the
current issue workspace as the runtime writable root. Network access in the
app-server sandbox request is disabled. Approval policy and timeout values are
validated from the workflow before dispatch.

At startup Symphony validates the committed schema manifest and performs a
real initialize handshake in a private preflight directory. Each real attempt
then launches a new contained process, repeats initialize, creates one thread,
and runs no more than `agent.max_turns` turns. After every completed turn,
Symphony refreshes the exact opaque tracker issue ID. Terminal, inactive,
unroutable, missing, timed-out, stalled, and failed outcomes stop the attempt
with distinct safe statuses.

## Operator requests

Command approvals, file approvals, permission approvals, and user-input
questions are held only in memory and displayed in the local browser UI. Each
request has a named group, visible deadline, keyboard-operable response, and a
ten-minute default response window. The operator may extend a pending request
up to ten times. Secret answers use password controls, are excluded from
diagnostics, and are cleared after use. Expired or already-resolved requests
return a visible stale-request error rather than accepting a late response.

## Testing profiles

`internal/codex/fakeappserver` is a deterministic test executable. Production
composition never imports or selects it; integration tests choose it only by
putting its path in a test workflow's `codex.command`.

The real app-server smoke is disabled by default and reports exactly
`SKIPPED: real Codex smoke`. To opt in, set
`SYMPHONY_REAL_CODEX_SMOKE=1` and provide an absolute, isolated workflow path
in `SYMPHONY_REAL_CODEX_WORKFLOW`. The selected CLI must already be
authenticated: an installed but unauthenticated CLI reports the same exact
`SKIPPED` sentinel, while a missing CLI or enabled handshake failure fails the
test. There is no synthetic-pass mode.

```bash
SYMPHONY_REAL_CODEX_SMOKE=1 \
SYMPHONY_REAL_CODEX_WORKFLOW=/absolute/path/to/isolated/WORKFLOW.md \
go test -v -count=1 -run '^TestRealCodexAppServerSmoke$' ./internal/codex
```

Set `SYMPHONY_REAL_CODEX_COMMAND` only when the selected CLI is intentionally
at a non-default absolute path. If that path is not available as `codex` on
`PATH`, also set `SYMPHONY_REAL_CODEX_LOGIN_COMMAND` to its quoted absolute path
followed by `login status`. The smoke launches and handshakes with the real
app-server; it never substitutes the deterministic fixture.

## Reviewing a Codex protocol baseline update

Ordinary compatible CLI updates do not require changing the bundled schema.
Update the baseline when Symphony adopts new protocol requirements or needs to
adapt to a breaking change. The snapshot's `target_version` also establishes
the new minimum, so raise it only when older CLIs can no longer be supported.

Codex can [generate the protocol schema for the installed CLI](https://learn.chatgpt.com/docs/app-server).
Review the protocol actually consumed by Symphony; a changed whole-schema
digest alone does not establish a breaking change.

Install the exact proposed Codex CLI version, update `TARGET_VERSION` in
`scripts/update-codex-schema.mjs`, and run:

```bash
node scripts/update-codex-schema.mjs
```

Review every generated schema diff and the new manifest rather than accepting
the digest alone. Confirm the initialize identity, thread and turn parameters,
notifications, dynamic-tool request and response types, operator-request
methods, and failure enums against the new CLI. Then update the minimum version
where necessary, protocol fixtures, compatibility tests, docs, and opt-in real
smoke together. Keep tests proving that newer compatible versions still work
and that incompatible required protocol messages are rejected.

See [Security](security.md) for process containment and credential handling and
[Provider tools](provider-tools.md) for the adapter-owned tools passed to a
Codex thread.
