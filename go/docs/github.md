# GitHub tracker

The GitHub adapter reads issues from one repository per Symphony process. It
does not create, edit, label, assign, close, or comment on issues, and it does
not expose a GitHub mutation tool to Codex. See the [main README](../README.md)
for running more than one repository at the same time.

## Configuration

Configure the tracker in the `WORKFLOW.md` front matter:

```yaml
---
tracker:
  kind: github
  provider:
    owner: example-owner
    repository: example-repository
    endpoint: https://api.github.com
    credential_ref: os-vault
    assignee: example-login
  required_labels: [symphony]
  active_states: [open]
  terminal_states: [closed]
---
Work on {{ issue.identifier }}.
```

The fields have these contracts:

| Field | Requirement or default |
| --- | --- |
| `tracker.kind` | Required; must be `github`. |
| `provider.owner` | Required GitHub owner or organization name. |
| `provider.repository` | Required repository name. |
| `provider.endpoint` | Optional; defaults to `https://api.github.com`. It must be HTTPS, include a host, and contain no user information, query, or fragment. A path prefix is allowed for a compatible API endpoint. |
| `provider.credential_ref` | Optional; an omitted value or `os-vault` uses the native credential vault. `$NAME` reads exactly that environment variable. |
| `provider.assignee` | Optional. When present, an issue must contain a matching assignee, compared without case, to be provider-dispatchable. |
| `tracker.required_labels` | Optional; defaults to no required labels. Every configured label must be nonblank. |
| `tracker.active_states` | Optional; defaults to `[open]` and may contain only `open`. |
| `tracker.terminal_states` | Optional; defaults to `[closed]` and may contain only `closed`. |

Use `os-vault` to store a credential through Symphony's Configuration page in
macOS Keychain or Windows Credential Manager. An environment reference such as
`$GITHUB_TOKEN` is managed outside Symphony, so the Configuration page cannot
replace or delete it. Never put a token value directly in `WORKFLOW.md`.

Replacing or deleting a native-vault credential retires the running adapter
generation and starts a rebuild. The old credential is not kept active while
the replacement is tested.

## Credential permissions

Prefer a dedicated fine-grained personal access token restricted to the one
configured repository. The minimum repository permissions are:

- Metadata: read-only (GitHub includes this permission for fine-grained
  tokens).
- Issues: read-only.

No repository write permission is needed for the Phase 2 adapter. Organization
policy or single sign-on may impose additional authorization requirements, but
do not add write permissions for Symphony.

For the protected `integration_live` workflow, use a dedicated test repository
rather than an operator or production repository. Keep at least one stable open
issue in that repository as the active sentinel and give the live-test token
only the read permissions above. The credentialed profile deliberately fails
when it cannot read an active sentinel; it never creates one.

## Identity and routing

For GitHub issue number `42` in `Example-Owner/Example-Repository`, Symphony
uses:

- human-facing identifier `#42`;
- internal ID `github:example-owner/example-repository#42`;
- native reference fields for owner, repository, and number, with provider IDs
  and state reason retained only when they are valid.

The internal owner and repository are lowercased so dispatch IDs are canonical.
IDs passed back to the adapter must match the configured repository exactly.
Duplicate IDs are fetched once, and a missing individual issue is omitted
rather than treated as proof that the repository is missing.

GitHub's issues endpoint also returns pull requests. Symphony excludes every
record containing GitHub's pull-request marker. A regular issue is
provider-dispatchable unless the optional assignee filter rejects it. The
runtime then applies `tracker.required_labels`, comparing trimmed labels
without case. An issue is routable only when both checks pass.

## Paging and cache behavior

State reads request `state=all`, `per_page=100`, and sequential page numbers.
Symphony follows at most 100 pages. Each `rel="next"` link must:

- parse as valid Link metadata;
- remain on the configured HTTPS origin;
- contain exactly the next page number;
- avoid duplicates and cycles.

Each exact page URL may use its stored ETag. A `304 Not Modified` response is
accepted only when it matches a valid cached page. New bodies, ETags, and Link
metadata are staged and replace the cache only after the complete traversal and
normalization succeeds. A failed or malformed later page therefore cannot
partially update the previous cache.

## Failure behavior

Provider failures use bounded, redacted categories rather than response bodies,
headers, request URLs, or credentials:

| Category | Meaning |
| --- | --- |
| `tracker_config` | Invalid tracker configuration or dispatch ID. |
| `tracker_auth` | Missing credential or failed authentication/authorization. |
| `tracker_scope` | The configured repository collection is missing or inaccessible. |
| `tracker_transport` | A request could not be made or read before a usable response. |
| `tracker_response` | An unexpected HTTP response. HTTP 408 and 5xx responses are retryable. |
| `tracker_payload` | An oversized, malformed, or semantically inconsistent payload. |
| `tracker_pagination` | Invalid, repeated, incomplete, or excessive paging metadata. |
| `tracker_rate_limited` | HTTP 429, or a rate-limited HTTP 403, with bounded retry metadata. |

A collection `404` is `tracker_scope`; an individual issue `404` remains a
normal omission. Rate-limit delay parsing accepts `Retry-After` and GitHub's
reset timestamp and caps the represented delay at 24 hours. Redirects are
limited to the same HTTPS origin. Responses are bounded to 16 MiB and requests
time out after 30 seconds.

These controls describe the current read-only provider boundary. They do not
claim that a credentialed live profile has passed in a particular repository.
