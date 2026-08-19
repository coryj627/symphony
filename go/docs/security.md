# Security boundaries

Symphony is a local operator application, not a remote service. The browser UI
binds only to loopback and uses a one-time bootstrap capability to establish a
protected local session. The Codex app-server transport is private JSONL over
the child process's standard streams.

The bootstrap exchange accepts only the exact generated root URL from a native
browser launch: the request must use the bound numeric loopback host and port,
contain only the one canonical capability query parameter, carry no `Origin`,
and have either no `Sec-Fetch-Site` header or `Sec-Fetch-Site: none`. A rejected
request does not consume the one-time capability. Authenticated mutations then
reject foreign-origin and cross-site browser signals and require both the
session cookie and its session-bound CSRF token before application handlers
run.

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

## Packaged browser assets

`npm run security:assets` checks every HTML template, CSS file, and JavaScript
file selected by `web/embed.go`. It rejects remote or protocol-relative resource
references, external source maps, known analytics and telemetry markers, inline
HTML event handlers, dynamic script and style resources, unsafe resource paths,
and local resources that are not covered by the embed manifest. Ordinary links
to external tracker pages remain allowed because they are navigation, not
executable, style, or media resources.

The gate is deterministic and does not make network requests. Findings contain
only the source path, line number, and policy class; rejected source values are
not copied into logs. Both supported native CI runners execute the gate.

## Disposable-canary artifact tests

Security-boundary tests create a cryptographically random, test-only canary and
register it with the process redactor before exercising secret-bearing paths.
The shared artifact scanner checks the raw canary and common base64, base64url,
hexadecimal, URL, and JSON encodings in explicitly named memory artifacts,
child environments, regular files, and directory trees. Scans are bounded and
fail closed when an input is absent, unreadable, oversized, a special file, or
a symbolic link; findings identify only the artifact and encoding class and do
not print the canary or matching bytes.

The focused integration scenario covers serialized HTTP headers and bodies,
SSE data, snapshots, the activity journal, structured in-memory and on-disk
logs, sanitized Codex stderr, child-process environment output, the data
directory, and captured test artifacts. It also retains a safe issue identifier
as a control so redaction does not erase ordinary observable content. These
tests are regression evidence for the covered boundaries, not a claim that an
arbitrary local artifact is credential-free or that the full security review is
complete.
