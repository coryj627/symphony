# Linear tracker

The Linear adapter reads issues from one project slug per Symphony process. It
does not create, edit, move, assign, label, or comment on issues, and it does
not expose a Linear mutation tool to Codex. See the [main README](../README.md)
for running separate projects concurrently.

## Configuration

Configure the tracker in the `WORKFLOW.md` front matter:

```yaml
---
tracker:
  kind: linear
  provider:
    project_slug: example-project
    endpoint: https://api.linear.app/graphql
    credential_ref: os-vault
  required_labels: [symphony]
  active_states: [Todo, In Progress]
  terminal_states: [Closed, Cancelled, Canceled, Duplicate, Done]
---
Work on {{ issue.identifier }}.
```

The fields have these contracts:

| Field | Requirement or default |
| --- | --- |
| `tracker.kind` | Required; must be `linear`. |
| `provider.project_slug` | Required; one exact Linear project slug. |
| `provider.endpoint` | Optional; defaults to `https://api.linear.app/graphql`. It must be HTTPS, include a host, and contain no user information, query, or fragment. |
| `provider.credential_ref` | Optional; an omitted value or `os-vault` uses the native credential vault. `$NAME` reads exactly that environment variable. |
| `tracker.required_labels` | Optional; defaults to no required labels. Every configured label must be nonblank. |
| `tracker.active_states` | Optional; defaults to `[Todo, In Progress]`. State names are matched without case after trimming. |
| `tracker.terminal_states` | Optional; defaults to `[Closed, Cancelled, Canceled, Duplicate, Done]`. These names determine whether blockers are terminal. |

Use `os-vault` to store a credential through Symphony's Configuration page in
macOS Keychain or Windows Credential Manager. An environment reference such as
`$LINEAR_API_KEY` is managed outside Symphony, so the Configuration page cannot
replace or delete it. Never put an API key directly in `WORKFLOW.md`.

Replacing or deleting a native-vault credential retires the running adapter
generation and starts a rebuild. Use a dedicated, least-privilege credential
that can read the configured project; Symphony's Phase 2 adapter requires no
Linear write operation.

For the protected `integration_live` workflow, use a dedicated seeded test
project rather than an operator or production project. Keep at least one stable
issue in an active state (`Todo` or `In Progress` by default) as the sentinel
and grant the live-test credential read access only. The credentialed profile
deliberately fails when it cannot read an active sentinel; it never creates
one.

## Scope and identity

Before every non-empty public issue fetch, Symphony runs a fixed project-scope
probe for `project_slug`. The probe requests two possible matches and must
return exactly one visible project with the exact slug and a valid native
project ID. This distinguishes a valid project with zero matching issues from
a missing or inaccessible project. No issue query runs when the probe fails.
Duplicate, mismatched, paginated, or malformed scope results fail closed as
payload errors; zero visible matches produce `tracker_scope`.

Linear identity is preserved rather than synthesized:

- `issue.id` is Linear's opaque native issue ID;
- `issue.identifier` is the human-facing value such as `ENG-42`;
- `native_ref` records the native issue ID, identifier, project ID, exact
  project slug, and team ID.

A returned issue must include valid issue, project, state, and team identity and
must match the configured project slug exactly. Dispatch-ID fetches accept only
valid opaque native IDs: valid UTF-8, 1 through 256 bytes, no control characters
or surrounding whitespace. Duplicate requested IDs are read once. Visible
out-of-project results are omitted; unexpected or duplicate in-scope identities
fail closed.

## Labels, priority, and blockers

Symphony preserves Linear's priority, branch name, assignee ID, labels,
timestamps, and `blocks` inverse relations when valid. A labels connection must
be complete. The runtime separately applies every `tracker.required_labels`
entry using trimmed, case-insensitive comparison.

Provider dispatchability is blocker-sensitive for the `Todo` state:

- the inverse-relation connection must be complete;
- each `blocks` relation represents an issue blocking the current issue;
- every blocker must expose a state included in `terminal_states`.

If those facts are incomplete or a blocker is not terminal, a `Todo` issue is
not provider-dispatchable. Other states retain provider dispatchability even
when blocker relations are incomplete. Runtime required-label routing still
applies to every state.

## Paging and query limits

A state read has a logical page size of 50. To stay below Linear's query
complexity ceiling, Symphony obtains a full logical page with requests of at
most 40 issues followed by the remaining 10 when another request is required.
It permits at most 100 logical pages, requires forward cursors, and rejects
missing or repeated cursors.

Each issue query caps labels at 50 and inverse relations at 50. A paginated
labels connection is invalid because routing would be incomplete. A paginated
relation connection makes blocker knowledge incomplete and therefore prevents
`Todo` dispatch.

ID reads are grouped into logical batches of 50 and sent as provider-safe
subrequests of at most 40 and then 10. An ID response must not require further
pagination, and it may return only IDs requested by that exact subrequest.

## Failure behavior

Provider failures use bounded, redacted categories rather than GraphQL error
text, response bodies, headers, endpoint URLs, or credentials:

| Category | Meaning |
| --- | --- |
| `tracker_config` | Invalid tracker configuration or dispatch IDs. |
| `tracker_auth` | Missing credential, HTTP 401/403, or a recognized GraphQL authentication/authorization code. |
| `tracker_scope` | The configured project is missing or inaccessible. |
| `tracker_transport` | A request could not be made or read before a usable response. |
| `tracker_response` | An unexpected HTTP status. HTTP 408 and 5xx responses are retryable. |
| `tracker_payload` | A malformed GraphQL envelope, operation error, or inconsistent issue/scope payload. |
| `tracker_pagination` | Invalid, repeated, incomplete, or excessive paging state. |
| `tracker_rate_limited` | HTTP 429 or GraphQL `RATELIMITED`, with bounded retry metadata. |

Linear reset headers are interpreted as epoch milliseconds. Retry delays are
capped at 24 hours and fall back to one minute when no valid reset is present.
Redirects are rejected, responses are bounded to 4 MiB, and requests time out
after 30 seconds.

These controls describe the current read-only provider boundary. They do not
claim that a credentialed live profile has passed in a particular workspace.
