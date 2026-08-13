# Provider tools

Symphony advertises only the active provider adapter's tool and captures the
tracker scope and issue identity before the Codex process starts. Tool calls
cannot supply a different endpoint, repository, project, or credential. A tool
failure is returned to Codex as one bounded `inputText` result; it does not
become an unbounded protocol error or silently change scope.

The text item contains one JSON object. Successful results use `success: true`
and an adapter-owned `data` value, with optional bounded HTTP `status` and
provider `request_id`. Failures use `success: false` and an `error` object with
a stable `code`, safe `message`, and optional `retryable`, `retry_after_ms`, and
`status` metadata. No result contains a credential, raw authorization header,
unbounded response body, or operator secret answer.

## GitHub `github_api`

`github_api` is restricted to the captured repository and the current regular
issue. Pull requests are rejected. The allowlisted operations are:

| Operation | Effect |
| --- | --- |
| `get_issue` | Read the current issue. |
| `update_issue` | Update allowlisted title, body, state, state reason, or milestone fields. |
| `list_comments` | Read the first 100 comments. |
| `create_comment` | Create one comment with a required session-local idempotency key. |
| `set_labels` | Replace labels with the bounded supplied list. |
| `add_assignees` | Add the bounded supplied login list. |
| `remove_assignees` | Remove the bounded supplied login list. |

Every call is an object with required string `operation`. Optional
`issue_number` is an integer of at least 1. The remaining exact fields are:

| Operation | Required fields | Optional fields and limits |
| --- | --- | --- |
| `get_issue`, `list_comments` | none | `issue_number` only. |
| `update_issue` | nonempty `input` | `input.title` is 1–4096 characters; `body` is string or null up to 1 MiB; `state` is `open` or `closed`; `state_reason` is `completed`, `not_planned`, or `reopened`; `milestone` is a positive integer or null. |
| `create_comment` | `input.body`, `idempotency_key` | Body is 1–1,048,576 characters; key is 1–256 characters; `issue_number` is optional. |
| `set_labels` | `input.labels` | Zero to 100 labels, each 1–100 characters; `issue_number` is optional. |
| `add_assignees`, `remove_assignees` | `input.assignees` | One to 100 logins, each 1–100 characters; `issue_number` is optional. |

No operation accepts an endpoint, owner, repository, credential, or other
top-level field. Each `input` object rejects fields not listed above.

An omitted `issue_number` means the captured current issue; a supplied number
must match it. Unknown fields and operations fail closed. GET operations may
retry once after a retryable transport, 408, or 5xx failure. Mutations are
never retried automatically and reject every redirect. `create_comment`
caches its result by opaque tracker session plus idempotency key, including an
ambiguous failure, so a repeated call cannot create a second comment in the
same adapter generation. Tool responses are limited to 1 MiB and returned
issue identities must match the request.

## Linear `linear_graphql`

`linear_graphql` accepts either one GraphQL document string or an object with
`query` and optional `variables`. The document must parse to exactly one query
or mutation; subscriptions, multiple operations, fragment-only documents, and
unknown wrapper fields fail closed. Arguments and responses are each limited
to 1 MiB.

The object form is exactly `{query: string, variables?: object}` and rejects
all other members. The string form is the query itself. In either form the
query must be nonempty and no larger than 1 MiB.

The request always uses the captured Linear endpoint and credential. Linear's
GraphQL schema ultimately controls which objects a credential can access, so
use a dedicated least-privilege credential and author workflow instructions
that keep operations in the configured project. Queries may carry retryable
metadata from a provider failure. Mutations are never marked retryable and are
never automatically replayed.

GraphQL error messages and extension values are redacted before they are
returned. Partial `data` may accompany a sanitized errors list. Raw response
bodies, credentials, request headers, and endpoint URLs are not included in
tool failures.
