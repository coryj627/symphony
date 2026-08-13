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

func TestCompatibilityAcceptsReviewedAdditionalVersionWithSameSchema(t *testing.T) {
	manifest := testCompatibilityManifest()
	manifest.Compatible = append(manifest.Compatible, buildinfo.CodexSchemaCompatibility{
		Version:      "0.144.2",
		SchemaSHA256: manifest.SchemaSHA256,
	})

	got := CheckCompatibility(InitializeResponse{UserAgent: "codex_cli_rs/0.144.2 (test build)"}, manifest)
	if !got.DispatchAllowed || got.Code != CompatibilityCodeCompatible || got.ObservedVersion != "0.144.2" {
		t.Fatalf("%+v", got)
	}
}

func TestCompatibilityRejectsUnreviewedVersion(t *testing.T) {
	manifest := testCompatibilityManifest()
	got := CheckCompatibility(InitializeResponse{UserAgent: "codex_cli_rs/0.145.0"}, manifest)
	if got.DispatchAllowed || got.Code != CompatibilityCodeVersionMismatch {
		t.Fatalf("%+v", got)
	}
	if got.ExpectedVersion != "0.144.1" || got.ObservedVersion != "0.145.0" {
		t.Fatalf("unsafe or missing version summary: %+v", got)
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
