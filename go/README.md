# Symphony accessible Go implementation

This directory contains Symphony's accessible Go implementation for macOS 14+
and Windows 11. It is developed alongside, and does not modify, the upstream
Elixir reference implementation in [`../elixir`](../elixir).

## Prerequisites

- Go 1.26.5
- Node.js 24.18.0

The pinned versions are declared in [`mise.toml`](mise.toml).

## CLI contract

```text
symphony [path-to-WORKFLOW.md] [--port N] [--data-dir PATH] [--open]
symphony configure [path-to-WORKFLOW.md] [--port N] [--data-dir PATH] [--open]
```

The workflow path defaults to `./WORKFLOW.md`. `configure` starts the
configuration experience without starting orchestration. An explicit `--port
0` requests an ephemeral loopback port.

## Development checks

```bash
go test ./...
gofmt -w cmd internal
```
