# Workflow save recovery

The structured and raw Configuration editors share one save transaction. It
checks the editor's base digest, validates the candidate, writes a temporary
file beside the workflow, flushes it, and replaces the destination. An external
edit produces a conflict so the operator can review both versions.

## Failure outcomes

| Failure point | File and active state | Recovery |
| --- | --- | --- |
| Before replacement: inspect, create, permissions, write, flush, close, digest recheck, or replace | The previous complete workflow and last-known-good snapshot remain. A missing destination remains absent. | Resolve the filesystem error and retry. Review a conflict before resubmitting. |
| Parent-directory flush after replacement | The complete new workflow is visible and its snapshot is installed. The save reports durability uncertainty. | Review the saved workflow before restarting. Use the new version as the basis for subsequent edits. |
| Temporary-file cleanup after a failed save | The destination remains intact. The operation reports both the save error and any cleanup error. | Resolve the filesystem problem. If the OS refused deletion, the exact temporary file may remain; inspect it before removing it. |

The Configuration page retains submitted values after a failed save. A
durability warning means replacement completed; it does not mean the old file
was restored. A visible file and confirmed persistence through a sudden power
loss are different guarantees.

Temporary files are created with restrictive permissions. Replacement preserves
the destination's existing permission bits where supported. These checks do not
claim that Unix permission bits describe Windows ACLs. Tracker credentials are
saved separately through the credential controls and must not be placed in
`WORKFLOW.md`.

## Verification boundary

The workflow package tests inject filesystem failures while using real
temporary directories and files. Save-level assertions cover exact file bytes,
active snapshots, change notifications, cleanup, and retry after the fault is
removed. Fault operations are package-private constructor inputs; the production
constructor always uses the real filesystem, and no CLI option enables faults.

Run the workflow tests from `go/`:

```bash
go test ./internal/workflow
```

On macOS, the workflow tests also run with the race detector. The native
replacement adapters use rename plus a parent-directory flush on macOS and
`MoveFileEx` with replace-existing and write-through flags on Windows. A
synthetic directory-flush failure tests store behavior on either host; it does
not simulate a Windows filesystem or prove storage hardware durability. Native
Windows execution remains a separate CI result.
