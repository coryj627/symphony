// Package codex implements the bounded Codex app-server client.
package codex

import (
	"regexp"
	"strings"

	"github.com/coryj627/symphony/go/internal/buildinfo"
	"golang.org/x/mod/semver"
)

// CompatibilityCode is a stable runtime compatibility result code.
type CompatibilityCode string

const (
	CompatibilityCodeCompatible       CompatibilityCode = "compatible"
	CompatibilityCodeVersionMismatch  CompatibilityCode = "version_mismatch"
	CompatibilityCodeUnknownUserAgent CompatibilityCode = "unknown_user_agent"
	CompatibilityCodeSchemaIntegrity  CompatibilityCode = "schema_integrity"
)

var codexUserAgentPattern = regexp.MustCompile(`^(?:codex_cli_rs|Codex Desktop)/([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)(?:[ \t]+\S.*)?$`)

// InitializeResponse contains the initialization fields used by preflight.
type InitializeResponse struct {
	UserAgent      string `json:"userAgent"`
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOS     string `json:"platformOs"`
}

// Compatibility is a safe summary of a Codex app-server preflight result.
type Compatibility struct {
	DispatchAllowed bool              `json:"dispatch_allowed"`
	Code            CompatibilityCode `json:"code"`
	ExpectedVersion string            `json:"expected_version"` // Minimum supported version; retain the existing JSON field.
	ObservedVersion string            `json:"observed_version,omitempty"`
	Message         string            `json:"message"`
}

// CheckCompatibility enforces the minimum CLI version and bundled schema integrity.
// Newer servers may add protocol fields and methods. Session and request validation
// still reject incompatible messages when the corresponding protocol is exercised.
func CheckCompatibility(response InitializeResponse, manifest buildinfo.CodexSchemaManifest) Compatibility {
	result := Compatibility{
		ExpectedVersion: manifest.TargetVersion,
	}
	minimum := "v" + manifest.TargetVersion
	if err := buildinfo.ValidateCodexSchemaMetadata(manifest); err != nil ||
		!semver.IsValid(minimum) || semver.Canonical(minimum) != strings.SplitN(minimum, "+", 2)[0] {
		result.Code = CompatibilityCodeSchemaIntegrity
		result.Message = "The bundled Codex schema failed its integrity check. Rebuild Symphony with a reviewed schema snapshot."
		return result
	}

	matches := codexUserAgentPattern.FindStringSubmatch(response.UserAgent)
	if len(matches) != 2 || !semver.IsValid("v"+matches[1]) {
		result.Code = CompatibilityCodeUnknownUserAgent
		result.Message = "Codex did not report a recognized semantic version. Update or reinstall the Codex CLI."
		return result
	}
	result.ObservedVersion = matches[1]
	if semver.Compare("v"+result.ObservedVersion, minimum) < 0 {
		result.Code = CompatibilityCodeVersionMismatch
		result.Message = "The installed Codex CLI is older than Symphony's minimum supported version. Install Codex CLI " + manifest.TargetVersion + " or newer."
		return result
	}

	result.DispatchAllowed = true
	result.Code = CompatibilityCodeCompatible
	result.Message = "The Codex CLI meets Symphony's minimum version requirement."
	return result
}
