# Symphony Go implementation

- Support macOS 14+ and Windows 11; do not add Linux-specific behavior or release claims.
- Keep the protected UI loopback-only and package all browser assets locally.
- Use Go 1.26.5 and Node 24.18.0 from `mise.toml`.
- Preserve `WORKFLOW.md` as repository-owned policy and never persist or render tracker credentials.
- Run `go test ./...` and `gofmt` on changed Go packages before committing.
