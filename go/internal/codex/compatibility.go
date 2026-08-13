// Package codex implements the bounded Codex app-server client.
package codex

import (
	"regexp"

	"github.com/coryj627/symphony/go/internal/buildinfo"
)

// CompatibilityCode is a stable runtime compatibility result code.
type CompatibilityCode string

const (
	CompatibilityCodeCompatible       CompatibilityCode = "compatible"
	CompatibilityCodeVersionMismatch  CompatibilityCode = "version_mismatch"
	CompatibilityCodeUnknownUserAgent CompatibilityCode = "unknown_user_agent"
	CompatibilityCodeSchemaIntegrity  CompatibilityCode = "schema_integrity"
)

var codexUserAgentPattern = regexp.MustCompile(`^(?:codex_cli_rs|Codex Desktop)/([0-9]+\.[0-9]+\.[0-9]+)(?:[ \t]+\S.*)?$`)

// InitializeResponse contains the initialization fields used by preflight.
type InitializeResponse struct {
	UserAgent string `json:"userAgent"`
}

// Compatibility is a safe summary of a Codex app-server preflight result.
type Compatibility struct {
	DispatchAllowed bool              `json:"dispatch_allowed"`
	Code            CompatibilityCode `json:"code"`
	ExpectedVersion string            `json:"expected_version"`
	ObservedVersion string            `json:"observed_version,omitempty"`
	Message         string            `json:"message"`
}

// CheckCompatibility allows dispatch only for a reviewed version and exact schema digest.
func CheckCompatibility(response InitializeResponse, manifest buildinfo.CodexSchemaManifest) Compatibility {
	result := Compatibility{
		ExpectedVersion: manifest.TargetVersion,
	}
	if err := buildinfo.ValidateCodexSchemaMetadata(manifest); err != nil {
		result.Code = CompatibilityCodeSchemaIntegrity
		result.Message = "The bundled Codex schema failed its integrity check. Rebuild Symphony with a reviewed schema snapshot."
		return result
	}

	matches := codexUserAgentPattern.FindStringSubmatch(response.UserAgent)
	if len(matches) != 2 {
		result.Code = CompatibilityCodeUnknownUserAgent
		result.Message = "Codex did not report a recognized version. Update or reinstall the reviewed Codex CLI version."
		return result
	}
	result.ObservedVersion = matches[1]
	if !manifest.Supports(result.ObservedVersion, manifest.SchemaSHA256) {
		result.Code = CompatibilityCodeVersionMismatch
		result.Message = "The installed Codex CLI does not match a reviewed app-server schema. Install the expected version or regenerate and review the schema."
		return result
	}

	result.DispatchAllowed = true
	result.Code = CompatibilityCodeCompatible
	result.Message = "The Codex app-server schema is compatible."
	return result
}
