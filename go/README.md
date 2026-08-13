# Symphony accessible Go implementation

This directory contains Symphony's local browser application for macOS 14+ and
Windows 11. It is developed alongside, and does not modify, the upstream Elixir
reference implementation in [`../elixir`](../elixir). Linux is not supported.

The Phase 4 runtime launches the reviewed Codex app-server locally and exposes
only the selected provider adapter's scoped tool contract. Automated
accessibility checks are in place, but the planned stable-browser screen-reader
sessions are still pending; see [Accessibility testing](docs/accessibility-testing.md)
for the evidence boundary.

## Prerequisites

- Go 1.26.5
- Node.js 24.18.0
- Codex CLI 0.144.1 for run mode
- Bash (`/bin/bash` on macOS; a native Git for Windows Bash on Windows)
- A local Chrome-compatible browser for ordinary use

The pinned Go and Node.js versions are declared in [`mise.toml`](mise.toml);
the reviewed Codex version is enforced by the embedded protocol manifest and
the runtime compatibility preflight.

## CLI contract

```text
symphony [path-to-WORKFLOW.md] [--port N] [--data-dir PATH] [--open]
symphony configure [path-to-WORKFLOW.md] [--port N] [--data-dir PATH] [--open]
```

The workflow path defaults to `./WORKFLOW.md`. `configure` starts the
configuration experience without starting orchestration. An explicit `--port
0` requests an ephemeral loopback port; without a CLI override, the workflow's
`server.port` value is used. When `--data-dir` is omitted, Symphony selects a
per-workflow, per-tracker directory below the operating system's user
configuration directory. `--open` is opt-in.

## Build and configure

Run these commands from `go/`. The application does not need to be installed.

On macOS:

```bash
mkdir -p bin workflows/github state/github
go build -o ./bin/symphony ./cmd/symphony
./bin/symphony configure ./workflows/github/WORKFLOW.md \
  --port 0 --data-dir ./state/github --open
```

On Windows PowerShell:

```powershell
New-Item -ItemType Directory -Force -Path .\bin, .\workflows\github, .\state\github | Out-Null
go build -o .\bin\symphony.exe .\cmd\symphony
.\bin\symphony.exe configure .\workflows\github\WORKFLOW.md `
  --port 0 --data-dir .\state\github --open
```

The Configuration page creates or repairs the workflow after validating its
front matter and prompt template. Stop configure mode when finished, then run
the same workflow without the `configure` word:

```bash
./bin/symphony ./workflows/github/WORKFLOW.md \
  --port 0 --data-dir ./state/github --open
```

```powershell
.\bin\symphony.exe .\workflows\github\WORKFLOW.md `
  --port 0 --data-dir .\state\github --open
```

If `--open` does not select the browser you want, omit it and open the printed
protected URL in a local browser. The URL contains a one-time bootstrap
capability. Do not share, retain, publish, or paste it into logs or chat.

Provider configuration and least-privilege credential guidance are in:

- [GitHub tracker](docs/github.md)
- [Linear tracker](docs/linear.md)

Codex compatibility, provider tool boundaries, and local security controls are
documented in [Codex runtime](docs/codex.md), [Provider tools](docs/provider-tools.md),
and [Security](docs/security.md).

An omitted `credential_ref` or `os-vault` uses macOS Keychain or Windows
Credential Manager. A `$NAME` reference reads that environment variable and
cannot be replaced through the UI. Never store a raw credential in
`WORKFLOW.md`. Replacing or deleting a native-vault credential causes a running
queue to retire its current adapter and rebuild with the new credential state.

## Multiple instances

Each process configures one GitHub repository or one Linear project. To run two
scopes at once, use two distinct workflow files, ephemeral ports, and isolated
data directories.

On macOS, launch each command in its own terminal:

```bash
./bin/symphony ./workflows/github/WORKFLOW.md \
  --port 0 --data-dir ./state/github --open
./bin/symphony ./workflows/linear/WORKFLOW.md \
  --port 0 --data-dir ./state/linear --open
```

On Windows PowerShell:

```powershell
.\bin\symphony.exe .\workflows\github\WORKFLOW.md `
  --port 0 --data-dir .\state\github --open
.\bin\symphony.exe .\workflows\linear\WORKFLOW.md `
  --port 0 --data-dir .\state\linear --open
```

Before using the Linear command, create its parent directories and configure
that workflow as shown above. A workflow lock is based on the canonical
workflow path. Starting the same workflow twice is rejected even if a symlink,
different tracker scope, port, or data directory is supplied. Distinct
canonical workflow files can run concurrently.

## Network boundary

The UI accepts inbound connections only on IPv4 `127.0.0.1` and, when
available, IPv6 `::1`; it is not exposed to the LAN or internet. All CSS and
JavaScript assets are served locally.

Outbound network access remains necessary for the selected tracker:

- GitHub mode makes HTTPS requests to the configured GitHub API endpoint.
- Linear mode makes HTTPS requests to the configured Linear GraphQL endpoint.

Run mode launches the configured local Codex app-server after an exact-version
compatibility preflight. Codex may require outbound OpenAI service access under
the operator's local Codex configuration. Symphony does not expose the local UI
or app-server transport as a remote network service.

Remote inbound access is neither required nor enabled. Restrict outbound rules
to the endpoints your tracker and Codex configuration actually use.

## Local diagnostics

Structured logs, rotation limits, redaction, the Activity journal, and live
event-stream behavior are documented in [Local observability](docs/observability.md).

## Development checks

```bash
npm ci
npx playwright install chromium webkit
npm run verify
gofmt -w cmd internal
```

`npm run verify` is the deterministic gate for the supported macOS and Windows
implementations. It includes the Go build, tests and vet; disabled live-provider
profile checks; rendered HTML and browser accessibility checks; wrapper tests;
and the review-only source scan. macOS also runs the Go race detector. See
[`docs/accessibility-testing.md`](docs/accessibility-testing.md) for scanner
installation, pre-commit hook activation, evidence boundaries, and manual test
requirements.
