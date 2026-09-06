package codex

import (
	"strings"
	"testing"

	"github.com/coryj627/symphony/go/internal/buildinfo"
)

func TestCompatibilityAcceptsExactReviewedVersion(t *testing.T) {
	manifest := testCompatibilityManifest()
	got := CheckCompatibility(InitializeResponse{UserAgent: "codex_cli_rs/0.144.1"}, manifest)
	if !got.DispatchAllowed || got.Code != CompatibilityCodeCompatible {
		t.Fatalf("%+v", got)
	}
}

func TestCompatibilityAcceptsReviewedDesktopUserAgent(t *testing.T) {
	manifest := testCompatibilityManifest()
	got := CheckCompatibility(InitializeResponse{
		UserAgent: "Codex Desktop/0.144.1 (Mac OS 26.5.2; arm64) dumb (symphony; 0.1.0)",
	}, manifest)
	if !got.DispatchAllowed || got.Code != CompatibilityCodeCompatible || got.ObservedVersion != "0.144.1" {
		t.Fatalf("%+v", got)
	}
}

func TestCompatibilityAcceptsNewerVersionsWithoutAllowlistEntries(t *testing.T) {
	manifest := testCompatibilityManifest()
	for _, version := range []string{"0.144.1+build.7", "0.144.2", "0.145.0-rc.1", "0.153.4", "0.1000.0", "1.0.0", "2.0.0"} {
		t.Run(version, func(t *testing.T) {
			got := CheckCompatibility(InitializeResponse{UserAgent: "codex_cli_rs/" + version + " (test build)"}, manifest)
			if !got.DispatchAllowed || got.Code != CompatibilityCodeCompatible || got.ObservedVersion != version || got.ExpectedVersion != "0.144.1" {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestCompatibilityRejectsVersionsBelowMinimum(t *testing.T) {
	manifest := testCompatibilityManifest()
	for _, version := range []string{"0.99.99", "0.143.99", "0.144.0", "0.144.1-rc.1"} {
		t.Run(version, func(t *testing.T) {
			got := CheckCompatibility(InitializeResponse{UserAgent: "codex_cli_rs/" + version}, manifest)
			if got.DispatchAllowed || got.Code != CompatibilityCodeVersionMismatch {
				t.Fatalf("%+v", got)
			}
			if got.ExpectedVersion != "0.144.1" || got.ObservedVersion != version {
				t.Fatalf("unsafe or missing version summary: %+v", got)
			}
		})
	}
}

func TestCompatibilityRejectsMissingOrMalformedUserAgent(t *testing.T) {
	manifest := testCompatibilityManifest()
	for _, userAgent := range []string{
		"",
		"codex_cli_rs",
		"codex_cli_rs/latest",
		"other/0.144.1",
		"codex_cli_rs/0.144.1/extra",
		"codex_cli_rs/0.144",
		"codex_cli_rs/v0.153.4",
		"codex_cli_rs/00.153.4",
		"codex_cli_rs/0.153.4-01",
		"codex_cli_rs/0.153.4+",
	} {
		t.Run(userAgent, func(t *testing.T) {
			got := CheckCompatibility(InitializeResponse{UserAgent: userAgent}, manifest)
			if got.DispatchAllowed || got.Code != CompatibilityCodeUnknownUserAgent {
				t.Fatalf("%+v", got)
			}
			if got.ObservedVersion != "" {
				t.Fatalf("malformed user agent leaked into observed version: %+v", got)
			}
		})
	}
}

func TestCompatibilityRejectsMalformedMinimumVersion(t *testing.T) {
	for _, version := range []string{"latest", "0.144", "00.144.1", "0.144.1-01"} {
		t.Run(version, func(t *testing.T) {
			digest := "sha256:" + strings.Repeat("a", 64)
			manifest := buildinfo.TestManifest(version, digest)
			got := CheckCompatibility(InitializeResponse{UserAgent: "codex_cli_rs/0.153.4"}, manifest)
			if got.DispatchAllowed || got.Code != CompatibilityCodeSchemaIntegrity {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestCompatibilityRejectsManifestDigestTampering(t *testing.T) {
	manifest := testCompatibilityManifest()
	manifest.SchemaSHA256 = "sha256:" + strings.Repeat("b", 64)

	got := CheckCompatibility(InitializeResponse{UserAgent: "codex_cli_rs/0.144.1"}, manifest)
	if got.DispatchAllowed || got.Code != CompatibilityCodeSchemaIntegrity {
		t.Fatalf("%+v", got)
	}
}

func TestCompatibilityRejectsDuplicateCompatibilityEntries(t *testing.T) {
	manifest := testCompatibilityManifest()
	manifest.Compatible = append(manifest.Compatible, manifest.Compatible[0])

	got := CheckCompatibility(InitializeResponse{UserAgent: "codex_cli_rs/0.144.1"}, manifest)
	if got.DispatchAllowed || got.Code != CompatibilityCodeSchemaIntegrity {
		t.Fatalf("%+v", got)
	}
}

func testCompatibilityManifest() buildinfo.CodexSchemaManifest {
	digest := "sha256:" + strings.Repeat("a", 64)
	return buildinfo.TestManifest("0.144.1", digest)
}
