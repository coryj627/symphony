// Package codexschema embeds the reviewed Codex app-server protocol snapshot.
package codexschema

import "embed"

// Files contains the generated schema directory and its integrity manifest.
//
//go:embed 0.144.1
var Files embed.FS
