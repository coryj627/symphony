package buildinfo

import (
	"strings"
	"testing"
)

func TestVendoredCodexSchemaMatchesManifest(t *testing.T) {
	manifest, files := MustCodexSchema(t)
	if manifest.TargetVersion != "0.144.1" {
		t.Fatalf("target %q", manifest.TargetVersion)
	}
	got := AggregateSchemaDigest(files)
	if got != manifest.SchemaSHA256 {
		t.Fatalf("schema digest %s, manifest %s", got, manifest.SchemaSHA256)
	}
	if !manifest.Supports("0.144.1", got) {
		t.Fatal("target schema is not in reviewed compatibility set")
	}
	if err := ValidateCodexSchemaManifest(manifest, files); err != nil {
		t.Fatalf("validate manifest: %v", err)
	}
}

func TestVendoredCodexSchemaContainsRequiredProtocolSurface(t *testing.T) {
	_, files := MustCodexSchema(t)
	combined := string(files["codex_app_server_protocol.schemas.json"])
	for _, required := range []string{
		`"initialize"`,
		`"thread/start"`,
		`"turn/start"`,
		`"turn/completed"`,
		`"item/tool/call"`,
		`"item/tool/requestUserInput"`,
		`"item/commandExecution/requestApproval"`,
		`"item/fileChange/requestApproval"`,
		`"item/permissions/requestApproval"`,
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("combined schema does not contain %s", required)
		}
	}
}

func TestSchemaDigestUsesBytewisePathOrderAndNormalizesFinalNewline(t *testing.T) {
	forward := map[string][]byte{
		"z.json":   []byte("{\"z\":true}\r\n\r\n"),
		"a/b.json": []byte("{\"b\":true}\n"),
	}
	reverse := map[string][]byte{
		"a/b.json": []byte("{\"b\":true}"),
		"z.json":   []byte("{\"z\":true}\n"),
	}

	got := AggregateSchemaDigest(forward)
	if got != AggregateSchemaDigest(reverse) {
		t.Fatalf("digest depends on map order or final newline style: %s vs %s", got, AggregateSchemaDigest(reverse))
	}
	if !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+64 {
		t.Fatalf("unexpected digest format %q", got)
	}
}

func TestSchemaManifestRejectsDuplicateCompatibilityEntries(t *testing.T) {
	digest := AggregateSchemaDigest(map[string][]byte{"schema.json": []byte("{}\n")})
	manifest := TestManifest("0.144.1", digest)
	manifest.Files = []string{"schema.json"}
	manifest.Compatible = append(manifest.Compatible, manifest.Compatible[0])

	err := ValidateCodexSchemaManifest(manifest, map[string][]byte{"schema.json": []byte("{}\n")})
	if err == nil || !strings.Contains(err.Error(), "duplicate compatibility") {
		t.Fatalf("expected duplicate compatibility error, got %v", err)
	}
}

func TestSchemaManifestSupportsReviewedAdditionalVersionAndDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	manifest := TestManifest("0.144.1", digest)
	manifest.Compatible = append(manifest.Compatible, CodexSchemaCompatibility{
		Version:      "0.144.2",
		SchemaSHA256: digest,
	})

	if !manifest.Supports("0.144.2", digest) {
		t.Fatal("reviewed additional version and digest were rejected")
	}
	if manifest.Supports("0.144.2", "sha256:"+strings.Repeat("b", 64)) {
		t.Fatal("unreviewed digest was accepted")
	}
}
