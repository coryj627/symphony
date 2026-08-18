package conformance

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

var upstreamIDPattern = regexp.MustCompile(`^S(?:17\.[1-8]|18\.[1-3])-[a-z0-9]+(?:-[a-z0-9]+)*$`)

func TestEveryUpstreamRequirementHasExactEvidence(t *testing.T) {
	manifest := loadUpstreamManifest(t)
	requirements := extractUpstreamRequirements(t)
	if manifest.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", manifest.SchemaVersion)
	}
	if manifest.SpecPath != "../SPEC.md" {
		t.Fatalf("spec_path = %q, want ../SPEC.md", manifest.SpecPath)
	}
	if len(manifest.Rows) != len(requirements) {
		t.Fatalf("manifest rows = %d, extracted requirements = %d", len(manifest.Rows), len(requirements))
	}

	wantSectionCounts := map[string]int{
		"17.1": 17, "17.2": 12, "17.3": 12, "17.4": 15,
		"17.5": 22, "17.6": 6, "17.7": 6, "17.8": 4,
		"18.1": 18, "18.2": 5, "18.3": 3,
	}
	gotSectionCounts := make(map[string]int)
	seenIDs := make(map[string]struct{}, len(manifest.Rows))
	for index, row := range manifest.Rows {
		requirement := requirements[index]
		gotSectionCounts[row.Section]++
		if !upstreamIDPattern.MatchString(row.ID) {
			t.Errorf("row %d has invalid ID %q", index+1, row.ID)
		}
		if _, exists := seenIDs[row.ID]; exists {
			t.Errorf("duplicate ID %q", row.ID)
		}
		seenIDs[row.ID] = struct{}{}
		if row.Section != requirement.Section || normalizeRequirementText(row.SourceText) != requirement.SourceText || row.SourceTextSHA256 != requirement.SourceTextSHA256 || row.Profile != requirement.Profile {
			t.Errorf("%s is stale; extracted %s", row.ID, describeRequirement(requirement))
		}
		if len(row.Evidence) == 0 {
			t.Errorf("%s has no evidence", row.ID)
		}
		switch row.Profile {
		case "core":
			if row.Status != "pass" {
				t.Errorf("%s core status = %q, want pass", row.ID, row.Status)
			}
		case "extension":
			if row.Status != "pass" && row.Status != "not_implemented_optional" {
				t.Errorf("%s extension status = %q", row.ID, row.Status)
			}
		case "real_integration":
			if row.Status != "pass" && row.Status != "skipped_real_profile" {
				t.Errorf("%s real-integration status = %q", row.ID, row.Status)
			}
		default:
			t.Errorf("%s has unknown profile %q", row.ID, row.Profile)
		}
		if row.Status == "not_implemented_optional" && !slices.ContainsFunc(row.Evidence, func(value string) bool {
			return strings.HasPrefix(value, "report:")
		}) {
			t.Errorf("%s optional deferral lacks a report citation", row.ID)
		}
		if row.Status == "not_implemented_optional" && !slices.Contains(row.Evidence, "go:./tests/conformance::TestDeferredExtensionsRemainUnclaimed") {
			t.Errorf("%s optional deferral lacks the exact deferral-boundary test", row.ID)
		}
		if row.Status == "skipped_real_profile" && row.Section != "17.8" && row.Section != "18.3" {
			t.Errorf("%s uses skipped_real_profile outside Sections 17.8/18.3", row.ID)
		}
	}
	for section, want := range wantSectionCounts {
		if got := gotSectionCounts[section]; got != want {
			t.Errorf("section %s rows = %d, want %d", section, got, want)
		}
	}
}

func TestDeferredExtensionsRemainUnclaimed(t *testing.T) {
	manifest := loadUpstreamManifest(t)
	want := map[string]struct{}{
		"S17.2-05-optional-workspace-population-synchronization-errors-are-surfaced": {},
		"S18.2-03-todo-persist-retry-queue-and-session-metadata":                     {},
		"S18.2-04-todo-make-observability-settings-configurable-in-workflow":         {},
		"S18.2-05-todo-extract-common-semantic-helper-tools-only":                    {},
	}
	for _, row := range manifest.Rows {
		_, explicitlyDeferred := want[row.ID]
		if explicitlyDeferred {
			if row.Status != "not_implemented_optional" {
				t.Errorf("%s status = %q, want not_implemented_optional", row.ID, row.Status)
			}
			delete(want, row.ID)
			continue
		}
		if row.Status == "not_implemented_optional" {
			t.Errorf("unexpected optional deferral %s", row.ID)
		}
	}
	for missing := range want {
		t.Errorf("missing explicit optional deferral %s", missing)
	}
}
