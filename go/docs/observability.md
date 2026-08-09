# Local observability

Symphony keeps operational diagnostics on the machine running the process. It
does not send telemetry, load remote browser assets, or upload logs or activity
events. The browser UI and its event stream are served only on loopback; see the
[main README](../README.md#network-boundary) for the separate outbound network
requirements of trackers and Codex.

## Structured logs

The active JSON Lines log is:

```text
<data-dir>/logs/symphony.jsonl
```

The data directory must resolve to an absolute path. Symphony creates the log
directory with mode `0700` and the active file with mode `0600` where the
platform filesystem supports POSIX permissions. It refuses symlink or
non-regular log and rotation paths.

The active file rotates at 10 MiB. Five archives are retained:
`symphony.jsonl.1` is newest and `symphony.jsonl.5` is oldest. Rotation closes
the active file before renaming and writes only complete JSONL records.

Independently of those files, each process retains a restart-scoped in-memory
ring of 2,000 sanitized log records. Log queries are newest first, return 100
records by default, and are capped at 200. They support an exact
case-insensitive level filter, a case-insensitive search of sanitized canonical
JSON, and an exclusive `before` sequence for older pages.

If initialization, writing, rotation, or close of the file sink fails, file
logging becomes permanently degraded for that process. The in-memory ring
continues to accept and serve recent records. Symphony writes this warning to
standard error exactly once:

```text
Symphony logging degraded; recent logs remain available in memory.
```

The Logs page displays the same bounded, sanitized diagnostic records and a
persistent degradation notice when applicable. It is searchable and pageable;
it is not an ARIA live log and is not continuously announced by a screen
reader.

## Redaction and content boundary

Messages, field names, field values, nested structures, and untrusted local
content pass through the centralized sanitizer before entering either the file
or the in-memory ring. Values are converted to valid UTF-8 and bounded by depth,
element count, and encoded size. Registered credentials and credential-shaped
fields are redacted, and unsafe composite values are replaced with safe
markers.

The logging contract excludes:

- tracker credentials and credential environment values;
- bootstrap capabilities, session identifiers, cookies, and CSRF values;
- raw HTTP request/response headers, bodies, and provider payloads;
- raw Codex requests, responses, prompts, and tool payloads.

Code should log allowlisted operational facts and stable error codes instead of
those values. Provider errors exposed to the runtime are separately bounded and
redacted. A local log file is still operational data: restrict access to the
account running Symphony and do not publish it without review.

## Activity journal

Activity is a separate, in-memory transition journal. Its default retention is
the first limit reached of 4,096 complete events or 8 MiB. Each encoded event is
capped at 64 KiB. This journal is not the 2,000-record log ring and is not
written to the JSONL archives.

Every process start creates a random restart epoch and starts a monotonic event
sequence. Retained events are served in chronological order. A valid cursor
from another epoch, a future cursor, or a cursor behind an evicted event causes
a reset response requiring a fresh snapshot. Malformed cursors are rejected.

The Activity page exposes only high-level, allowlisted summaries for:

- `queue.refreshed`;
- `queue.failed`;
- `configuration.changed`.

It does not display raw log records, provider responses, or Codex payloads.
Activity and Logs therefore answer different questions: Activity summarizes
state transitions, while Logs provides bounded diagnostic detail.

## Server-sent events

The same-origin `/api/v1/events` stream carries complete retained activity
events to the browser. Its fixed limits are:

| Control | Limit |
| --- | --- |
| Heartbeat interval | 20 seconds |
| Encoded event payload | 64 KiB |
| Concurrent clients | 32 |
| Per-write deadline | 2 seconds |
| Cursor text | 149 bytes |

`Last-Event-ID` is authoritative when present. A history gap or epoch change
produces one `reset` event with reason `snapshot_required` and then closes the
stream so the client can reconcile from a fresh snapshot. Responses are marked
`no-store`, and each event or heartbeat is written and flushed as a complete
record.

Routine queue and activity refreshes do not use an ARIA live region. Structural
updates wait when they would disturb focus and expose an explicit Apply control
at a safe focus boundary. Bounded status feedback is reserved for deliberate
user actions and failures such as an unsuccessful live-update resume; the UI
does not announce every poll or event.
