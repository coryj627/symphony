// Package webassets exposes Symphony's local browser assets.
package webassets

import "embed"

// Files contains all server-rendered templates and static assets.
//
//go:embed templates/*.html templates/partials/*.html static/*
var Files embed.FS
