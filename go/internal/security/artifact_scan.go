package security

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	minimumCanaryBytes          = 16
	defaultMaxArtifactNameBytes = 1024
	defaultMaxEntries           = 4096
	defaultMaxFileBytes         = int64(64 << 20)
	defaultMaxTotalBytes        = int64(512 << 20)
)

type artifactKind uint8

const (
	artifactMemory artifactKind = iota + 1
	artifactPath
)

// Artifact is an explicitly named in-memory value or filesystem boundary.
// Construct artifacts with BytesArtifact, TextArtifact, StringsArtifact, or
// PathArtifact so the scanner can fail closed on unsupported inputs.
type Artifact struct {
	kind artifactKind
	name string
	data []byte
	path string
}

// Finding identifies a canary representation without retaining or exposing
// the matching value.
type Finding struct {
	Artifact       string
	Location       string
	Representation string
}

type encodedCanary struct {
	name  string
	value []byte
}

// ArtifactScanner scans bounded artifact sets for raw and commonly encoded
// forms of one disposable canary.
type ArtifactScanner struct {
	needles       []encodedCanary
	maxEntries    int
	maxFileBytes  int64
	maxTotalBytes int64
}

type scanState struct {
	entries int
	bytes   int64
}

// BytesArtifact copies an in-memory artifact for later scanning.
func BytesArtifact(name string, data []byte) Artifact {
	return Artifact{kind: artifactMemory, name: name, data: append([]byte(nil), data...)}
}

// TextArtifact copies a UTF-8 text artifact for later scanning.
func TextArtifact(name, value string) Artifact {
	return BytesArtifact(name, []byte(value))
}

// StringsArtifact serializes a string collection with a delimiter that cannot
// occur in an operating-system environment entry.
func StringsArtifact(name string, values []string) Artifact {
	return TextArtifact(name, strings.Join(append([]string(nil), values...), "\x00"))
}

// PathArtifact identifies a regular file or directory tree to scan without
// following symbolic links.
func PathArtifact(name, path string) Artifact {
	return Artifact{kind: artifactPath, name: name, path: string(append([]byte(nil), path...))}
}

// NewArtifactScanner creates a bounded scanner for one exact canary and its
// common transport encodings.
func NewArtifactScanner(canary []byte) (*ArtifactScanner, error) {
	if len(canary) < minimumCanaryBytes {
		return nil, fmt.Errorf("artifact scanner canary must contain at least %d bytes", minimumCanaryBytes)
	}
	exact := append([]byte(nil), canary...)
	scanner := &ArtifactScanner{
		maxEntries:    defaultMaxEntries,
		maxFileBytes:  defaultMaxFileBytes,
		maxTotalBytes: defaultMaxTotalBytes,
	}
	seen := make(map[string]struct{})
	add := func(name, value string) {
		if value == "" {
			return
		}
		if _, exists := seen[value]; exists {
			return
		}
		seen[value] = struct{}{}
		scanner.needles = append(scanner.needles, encodedCanary{name: name, value: []byte(value)})
	}
	add("raw", string(exact))
	add("base64", base64.StdEncoding.EncodeToString(exact))
	add("base64-unpadded", base64.RawStdEncoding.EncodeToString(exact))
	add("base64url", base64.URLEncoding.EncodeToString(exact))
	add("base64url-unpadded", base64.RawURLEncoding.EncodeToString(exact))
	add("hex", hex.EncodeToString(exact))
	add("hex-uppercase", strings.ToUpper(hex.EncodeToString(exact)))
	add("url-encoded", url.QueryEscape(string(exact)))
	if encoded, err := json.Marshal(string(exact)); err == nil && len(encoded) >= 2 {
		add("json-escaped", string(encoded[1:len(encoded)-1]))
	}
	return scanner, nil
}

// Scan inspects every supplied artifact completely or returns an error. It
// never silently skips oversized, unreadable, symbolic-link, or special files.
func (scanner *ArtifactScanner) Scan(artifacts ...Artifact) ([]Finding, error) {
	if scanner == nil || len(scanner.needles) == 0 {
		return nil, errors.New("artifact scanner is not initialized")
	}
	if len(artifacts) == 0 {
		return nil, errors.New("at least one artifact is required")
	}
	state := &scanState{}
	findings := make([]Finding, 0)
	for _, artifact := range artifacts {
		if strings.TrimSpace(artifact.name) == "" {
			return nil, errors.New("artifact name is required")
		}
		if len(artifact.name) > defaultMaxArtifactNameBytes {
			return nil, errors.New("artifact name exceeded the scan limit")
		}
		label := scanner.safeLabel(artifact.name)
		findings = append(findings, scanner.find(label, "name", []byte(artifact.name))...)
		switch artifact.kind {
		case artifactMemory:
			memoryFindings, err := scanner.scanMemory(label, artifact.data, state)
			if err != nil {
				return nil, err
			}
			findings = append(findings, memoryFindings...)
		case artifactPath:
			if strings.TrimSpace(artifact.path) == "" {
				return nil, fmt.Errorf("artifact %q path is required", label)
			}
			findings = append(findings, scanner.find(label, "path", []byte(artifact.path))...)
			pathFindings, err := scanner.scanPath(label, artifact.path, state)
			if err != nil {
				return nil, err
			}
			findings = append(findings, pathFindings...)
		default:
			return nil, fmt.Errorf("artifact %q has an unsupported kind", label)
		}
	}
	return findings, nil
}

func (scanner *ArtifactScanner) scanMemory(label string, data []byte, state *scanState) ([]Finding, error) {
	if state.entries >= scanner.maxEntries {
		return nil, fmt.Errorf("artifact %q exceeded the %d-entry scan limit", label, scanner.maxEntries)
	}
	if int64(len(data)) > scanner.maxFileBytes {
		return nil, fmt.Errorf("artifact %q content exceeded the per-entry scan limit", label)
	}
	if state.bytes+int64(len(data)) > scanner.maxTotalBytes {
		return nil, fmt.Errorf("artifact %q exceeded the total byte scan limit", label)
	}
	state.entries++
	state.bytes += int64(len(data))
	return scanner.find(label, "content", data), nil
}

func (scanner *ArtifactScanner) scanPath(label, root string, state *scanState) ([]Finding, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("artifact %q could not be inspected", label)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("artifact %q contains an unsupported symbolic link", label)
	}
	if info.Mode().IsRegular() {
		return scanner.scanFile(label, root, "file", info, state)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("artifact %q is not a regular file or directory", label)
	}

	findings := make([]Finding, 0)
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("entry could not be inspected")
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return errors.New("entry path could not be normalized")
		}
		location := scanner.safeLocation(filepath.ToSlash(relative))
		findings = append(findings, scanner.find(label, location+" path", []byte(relative))...)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic link at %s", location)
		}
		if entry.IsDir() {
			return nil
		}
		entryInfo, infoErr := entry.Info()
		if infoErr != nil {
			return fmt.Errorf("entry at %s could not be inspected", location)
		}
		if !entryInfo.Mode().IsRegular() {
			return fmt.Errorf("unsupported file at %s", location)
		}
		fileFindings, scanErr := scanner.scanFile(label, path, location, entryInfo, state)
		if scanErr != nil {
			return scanErr
		}
		findings = append(findings, fileFindings...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("artifact %q directory scan failed: %s", label, scanner.safeError(err))
	}
	return findings, nil
}

func (scanner *ArtifactScanner) scanFile(label, path, location string, before os.FileInfo, state *scanState) ([]Finding, error) {
	if state.entries >= scanner.maxEntries {
		return nil, fmt.Errorf("artifact %q exceeded the %d-entry scan limit", label, scanner.maxEntries)
	}
	if before.Size() < 0 || before.Size() > scanner.maxFileBytes {
		return nil, fmt.Errorf("artifact %q file at %s exceeded the per-file scan limit", label, scanner.safeLocation(location))
	}
	if state.bytes+before.Size() > scanner.maxTotalBytes {
		return nil, fmt.Errorf("artifact %q exceeded the total byte scan limit", label)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("artifact %q file at %s could not be opened", label, scanner.safeLocation(location))
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, fmt.Errorf("artifact %q file at %s changed during inspection", label, scanner.safeLocation(location))
	}
	data, err := io.ReadAll(io.LimitReader(file, scanner.maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("artifact %q file at %s could not be read", label, scanner.safeLocation(location))
	}
	if int64(len(data)) > scanner.maxFileBytes {
		return nil, fmt.Errorf("artifact %q file at %s exceeded the per-file scan limit", label, scanner.safeLocation(location))
	}
	if state.bytes+int64(len(data)) > scanner.maxTotalBytes {
		return nil, fmt.Errorf("artifact %q exceeded the total byte scan limit", label)
	}
	state.entries++
	state.bytes += int64(len(data))
	return scanner.find(label, scanner.safeLocation(location), data), nil
}

func (scanner *ArtifactScanner) find(label, location string, value []byte) []Finding {
	findings := make([]Finding, 0)
	for _, needle := range scanner.needles {
		if bytes.Contains(value, needle.value) {
			findings = append(findings, Finding{
				Artifact:       label,
				Location:       scanner.safeLocation(location),
				Representation: needle.name,
			})
		}
	}
	return findings
}

func (scanner *ArtifactScanner) safeLabel(value string) string {
	if scanner.contains([]byte(value)) {
		return "[redacted artifact name]"
	}
	return value
}

func (scanner *ArtifactScanner) safeLocation(value string) string {
	if scanner.contains([]byte(value)) {
		return "[redacted artifact path]"
	}
	return value
}

func (scanner *ArtifactScanner) safeError(err error) string {
	if err == nil {
		return "unknown error"
	}
	return scanner.safeLocation(err.Error())
}

func (scanner *ArtifactScanner) contains(value []byte) bool {
	for _, needle := range scanner.needles {
		if bytes.Contains(value, needle.value) {
			return true
		}
	}
	return false
}
