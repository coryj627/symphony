# Security boundaries

Symphony is a local operator application, not a remote service. The browser UI
binds only to loopback and uses a one-time bootstrap capability to establish a
protected local session. The Codex app-server transport is private JSONL over
the child process's standard streams.

## Child-process containment

Before every launch, Symphony revalidates that the issue workspace remains
inside the configured workspace root. On macOS the app-server is placed in its
own process group; orderly stop escalates to terminating that group if needed.
On Windows the process is assigned to a Job Object with kill-on-close. Only the
intended standard handles are inherited. Shutdown reports a stopping-failed
state when tree termination cannot be confirmed.

The workflow command is bounded and executed through a discovered native Bash.
The child receives a sanitized environment: declared provider secret names are
removed without case sensitivity on Windows. Provider credentials remain in
the host-side adapter and are never serialized into app-server initialize,
thread, turn, tool declaration, event, or browser payloads.

This containment is not a virtual-machine or operating-system account
boundary. Workflow hooks are trusted host commands, and a permissive local
Codex approval or sandbox policy may allow code to read or change data
available to the operator's host account. Review workflows, hooks, Codex
configuration, and approval requests as trusted local automation; use a
separate OS account or VM when an isolation boundary is required.

## Protocol and diagnostics

Each JSONL message, method name, pending-call collection, server-request
collection, stderr tail, and diagnostic line is bounded. Incoming messages are
validated against the committed app-server schema before use. Unknown methods,
malformed identities, mismatched thread or turn IDs, oversized messages, and
silence past the configured deadline fail closed.

Structured logs and the local activity journal receive redacted, operator-safe
summaries. App-server stderr is retained only as a bounded redacted tail.
Secret operator answers are memory-only and cleared after a response, expiry,
or session shutdown. Native-vault credential changes retire the entire active
adapter generation before rebuilding.

## Provider network boundary

Tracker traffic uses only the configured HTTPS endpoint. GitHub queue reads
allow only same-origin HTTPS redirects; GitHub mutations reject all redirects.
Linear rejects redirects. Provider bodies and tool results have independent
size limits, and errors expose stable categories rather than credentials or raw
provider text. See [Provider tools](provider-tools.md) for the exact operation
allowlists and retry rules.

The deterministic fake app-server is a test-only executable selected by test
workflow data. Production run mode always selects the contained native Codex
launcher. The optional real-Codex CI profile is explicit, main-branch-only, and
must use an isolated workflow path.
