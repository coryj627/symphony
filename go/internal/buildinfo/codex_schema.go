package buildinfo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	codexschema "github.com/coryj627/symphony/go/schema/codex"
)

const codexSchemaManifestPath = CodexVersion + "/manifest.json"

// CodexSchemaCompatibility records a reviewed CLI version and schema digest.
type CodexSchemaCompatibility struct {
	Version      string `json:"version"`
	SchemaSHA256 string `json:"schema_sha256"`
}

// CodexSchemaManifest describes the checked-in Codex app-server schema.
type CodexSchemaManifest struct {
	TargetVersion     string                     `json:"target_version"`
	SchemaSHA256      string                     `json:"schema_sha256"`
	GenerationCommand string                     `json:"generation_command"`
	Files             []string                   `json:"files"`
	Compatible        []CodexSchemaCompatibility `json:"compatible"`
}

// Supports reports whether a version and exact schema digest were reviewed.
func (m CodexSchemaManifest) Supports(version, digest string) bool {
	for _, compatible := range m.Compatible {
		if compatible.Version == version && compatible.SchemaSHA256 == digest {
			return true
		}
	}
	return false
}

// LoadCodexSchema loads and verifies the embedded Codex schema snapshot.
func LoadCodexSchema() (CodexSchemaManifest, map[string][]byte, error) {
	manifestBytes, err := fs.ReadFile(codexschema.Files, codexSchemaManifestPath)
	if err != nil {
		return CodexSchemaManifest{}, nil, fmt.Errorf("read Codex schema manifest: %w", err)
	}
	var manifest CodexSchemaManifest
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return CodexSchemaManifest{}, nil, fmt.Errorf("decode Codex schema manifest: %w", err)
	}
	if decoder.More() {
		return CodexSchemaManifest{}, nil, errors.New("decode Codex schema manifest: trailing JSON value")
	}

	root := manifest.TargetVersion
	files := make(map[string][]byte, len(manifest.Files))
	err = fs.WalkDir(codexschema.Files, root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative := strings.TrimPrefix(name, root+"/")
		if relative == name {
			return fmt.Errorf("embedded schema file %q is outside %q", name, root)
		}
		if relative == "manifest.json" {
			return nil
		}
		contents, err := fs.ReadFile(codexschema.Files, name)
		if err != nil {
			return err
		}
		files[relative] = contents
		return nil
	})
	if err != nil {
		return CodexSchemaManifest{}, nil, fmt.Errorf("read embedded Codex schema: %w", err)
	}
	if err := ValidateCodexSchemaManifest(manifest, files); err != nil {
		return CodexSchemaManifest{}, nil, err
	}
	return manifest, files, nil
}

type fatalTestingT interface {
	Helper()
	Fatalf(format string, args ...any)
}

// MustCodexSchema loads the schema or fails the calling test.
func MustCodexSchema(t fatalTestingT) (CodexSchemaManifest, map[string][]byte) {
	t.Helper()
	manifest, files, err := LoadCodexSchema()
	if err != nil {
		t.Fatalf("load Codex schema: %v", err)
	}
	return manifest, files
}

// TestManifest returns a minimal reviewed manifest for compatibility tests.
func TestManifest(version, digest string) CodexSchemaManifest {
	return CodexSchemaManifest{
		TargetVersion:     version,
		SchemaSHA256:      digest,
		GenerationCommand: "test fixture",
		Compatible: []CodexSchemaCompatibility{{
			Version:      version,
			SchemaSHA256: digest,
		}},
	}
}

// AggregateSchemaDigest hashes normalized schema bytes in bytewise path order.
func AggregateSchemaDigest(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	hash := sha256.New()
	for _, name := range names {
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(normalizeFinalNewline(files[name]))
		_, _ = hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

// ValidateCodexSchemaManifest verifies manifest metadata, file inventory, and digest.
func ValidateCodexSchemaManifest(manifest CodexSchemaManifest, files map[string][]byte) error {
	if err := ValidateCodexSchemaMetadata(manifest); err != nil {
		return err
	}
	if len(manifest.Files) == 0 {
		return errors.New("Codex schema manifest has no files")
	}
	previous := ""
	for index, name := range manifest.Files {
		if !fs.ValidPath(name) || name == "manifest.json" || !strings.HasSuffix(name, ".json") {
			return fmt.Errorf("Codex schema manifest has invalid file path %q", name)
		}
		if index > 0 && name <= previous {
			return fmt.Errorf("Codex schema manifest files are not unique and bytewise sorted at %q", name)
		}
		previous = name
		if _, ok := files[name]; !ok {
			return fmt.Errorf("Codex schema manifest file %q is missing", name)
		}
	}
	if len(files) != len(manifest.Files) {
		return fmt.Errorf("Codex schema file inventory differs: manifest=%d embedded=%d", len(manifest.Files), len(files))
	}
	if got := AggregateSchemaDigest(files); got != manifest.SchemaSHA256 {
		return fmt.Errorf("Codex schema digest mismatch: got %s, manifest %s", got, manifest.SchemaSHA256)
	}
	return nil
}

// ValidateCodexSchemaMetadata verifies fields needed by runtime compatibility checks.
func ValidateCodexSchemaMetadata(manifest CodexSchemaManifest) error {
	if manifest.TargetVersion == "" {
		return errors.New("Codex schema manifest target version is empty")
	}
	if !validSHA256(manifest.SchemaSHA256) {
		return errors.New("Codex schema manifest digest is invalid")
	}
	if manifest.GenerationCommand == "" {
		return errors.New("Codex schema manifest generation command is empty")
	}
	if len(manifest.Compatible) == 0 {
		return errors.New("Codex schema manifest compatibility set is empty")
	}
	seen := make(map[string]struct{}, len(manifest.Compatible))
	for _, compatible := range manifest.Compatible {
		if compatible.Version == "" || !validSHA256(compatible.SchemaSHA256) {
			return errors.New("Codex schema manifest compatibility entry is invalid")
		}
		if _, exists := seen[compatible.Version]; exists {
			return fmt.Errorf("duplicate compatibility entry for Codex %s", compatible.Version)
		}
		seen[compatible.Version] = struct{}{}
	}
	if !manifest.Supports(manifest.TargetVersion, manifest.SchemaSHA256) {
		return errors.New("Codex schema manifest target is not in the reviewed compatibility set")
	}
	return nil
}

func validSHA256(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func normalizeFinalNewline(value []byte) []byte {
	end := len(value)
	for end > 0 && (value[end-1] == '\r' || value[end-1] == '\n') {
		end--
	}
	normalized := make([]byte, end+1)
	copy(normalized, value[:end])
	normalized[end] = '\n'
	return normalized
}
