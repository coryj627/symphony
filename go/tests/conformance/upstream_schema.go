package conformance

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type upstreamManifest struct {
	SchemaVersion int                   `json:"schema_version"`
	SpecPath      string                `json:"spec_path"`
	Rows          []upstreamManifestRow `json:"rows"`
}

type upstreamManifestRow struct {
	ID               string   `json:"id"`
	Section          string   `json:"section"`
	Profile          string   `json:"profile"`
	SourceText       string   `json:"source_text"`
	SourceTextSHA256 string   `json:"source_text_sha256"`
	Status           string   `json:"status"`
	Evidence         []string `json:"evidence"`
}

type upstreamRequirement struct {
	Section          string
	Depth            int
	SourceText       string
	SourceTextSHA256 string
	Profile          string
}

var (
	governedHeading = regexp.MustCompile(`^###\s+(17\.[1-8]|18\.[1-3])(?:\s|$)`)
	anyHeading      = regexp.MustCompile(`^#{1,3}\s`)
	bulletLine      = regexp.MustCompile(`^(\s*)-\s+(.*)$`)
)

func loadUpstreamManifest(t *testing.T) upstreamManifest {
	t.Helper()
	path := filepath.Join(repositoryRoot(t), "tests", "conformance", "upstream-requirements.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest upstreamManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return manifest
}

func extractUpstreamRequirements(t *testing.T) []upstreamRequirement {
	t.Helper()
	path := filepath.Join(repositoryRoot(t), "..", "SPEC.md")
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var requirements []upstreamRequirement
	var ancestors []string
	section := ""
	type pendingRequirement struct {
		section string
		depth   int
		parts   []string
	}
	var pending *pendingRequirement
	finish := func() {
		if pending == nil {
			return
		}
		text := normalizeRequirementText(strings.Join(pending.parts, " "))
		parentTexts := ancestors
		if len(parentTexts) > pending.depth {
			parentTexts = parentTexts[:pending.depth]
		}
		requirements = append(requirements, upstreamRequirement{
			Section:          pending.section,
			Depth:            pending.depth,
			SourceText:       text,
			SourceTextSHA256: requirementTextSHA256(text),
			Profile:          requirementProfile(pending.section, text, parentTexts),
		})
		for len(ancestors) <= pending.depth {
			ancestors = append(ancestors, "")
		}
		ancestors[pending.depth] = text
		ancestors = ancestors[:pending.depth+1]
		pending = nil
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if match := governedHeading.FindStringSubmatch(line); match != nil {
			finish()
			section = match[1]
			ancestors = nil
			continue
		}
		if anyHeading.MatchString(line) {
			finish()
			section = ""
			ancestors = nil
			continue
		}
		if !isGovernedSection(section) {
			continue
		}
		if match := bulletLine.FindStringSubmatch(line); match != nil {
			finish()
			pending = &pendingRequirement{
				section: section,
				depth:   len(match[1]) / 2,
				parts:   []string{match[2]},
			}
			continue
		}
		if pending != nil && len(line) > 0 && (line[0] == ' ' || line[0] == '\t') && strings.TrimSpace(line) != "" {
			pending.parts = append(pending.parts, strings.TrimSpace(line))
			continue
		}
		if strings.TrimSpace(line) == "" {
			finish()
		}
	}
	finish()
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return requirements
}

func normalizeRequirementText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func requirementTextSHA256(value string) string {
	digest := sha256.Sum256([]byte(normalizeRequirementText(value)))
	return hex.EncodeToString(digest[:])
}

func requirementProfile(section, text string, ancestors []string) string {
	if section == "17.8" || section == "18.3" {
		return "real_integration"
	}
	if section == "18.2" || strings.HasPrefix(text, "OPTIONAL ") || strings.HasPrefix(text, "If ") {
		return "extension"
	}
	for _, ancestor := range ancestors {
		if strings.HasPrefix(ancestor, "If ") {
			return "extension"
		}
	}
	return "core"
}

func isGovernedSection(section string) bool {
	for _, candidate := range []string{"17.1", "17.2", "17.3", "17.4", "17.5", "17.6", "17.7", "17.8", "18.1", "18.2", "18.3"} {
		if section == candidate {
			return true
		}
	}
	return false
}

func describeRequirement(requirement upstreamRequirement) string {
	return fmt.Sprintf("%s %q", requirement.Section, requirement.SourceText)
}
