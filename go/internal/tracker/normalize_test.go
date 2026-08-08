package tracker

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
)

func TestNormalizeLabelsTrimsLowercasesDeduplicatesAndDropsBlank(t *testing.T) {
	// Break caught: provider casing, whitespace, or duplicates make the core
	// required-label comparison provider-dependent.
	got := NormalizeLabels([]string{" Symphony ", "BUG", "bug", " "})
	if !slices.Equal(got, []string{"symphony", "bug"}) {
		t.Fatalf("labels = %q, want [symphony bug]", got)
	}
}

func TestNormalizeLabelsReturnsIndependentNonNilSlice(t *testing.T) {
	// Break caught: aliasing lets a provider reuse its decode buffer and mutate
	// a published issue; nil violates the normalized collection contract.
	input := []string{"Bug"}
	got := NormalizeLabels(input)
	input[0] = "changed"
	if !slices.Equal(got, []string{"bug"}) {
		t.Fatalf("labels aliased input: %q", got)
	}
	if empty := NormalizeLabels(nil); empty == nil || len(empty) != 0 {
		t.Fatalf("nil input normalized to %#v, want non-nil empty slice", empty)
	}
}

func TestNormalizeStateReturnsComparisonValueWithoutChangingDisplayState(t *testing.T) {
	// Break caught: storing the comparison form destroys provider spelling in
	// UI and prompt output.
	display := "  In Progress  "
	if got := NormalizeState(display); got != "in progress" {
		t.Fatalf("state = %q, want in progress", got)
	}
	if display != "  In Progress  " {
		t.Fatalf("display state changed to %q", display)
	}
}

func TestNormalizeIssueCopiesMetadataUsesFallbacksAndUTC(t *testing.T) {
	// Break caught: retaining caller-owned nested data, blank blocker fields,
	// nil collections, or provider-local timestamp locations makes snapshots
	// unstable or violates the normalized wire contract.
	blank := "  "
	blockerIdentifier := "LIN-2"
	blockerState := "In Progress"
	createdAt := time.Date(2026, time.August, 8, 10, 30, 0, 0, time.FixedZone("provider", -4*60*60))
	nested := map[string]any{"id": uint64(9007199254740993)}
	issue := domain.Issue{
		ID: "issue-1", NativeRef: map[string]any{"nested": nested}, Identifier: "LIN-1", Title: "Title",
		Description: &blank, BranchName: &blank, URL: &blank, AssigneeID: &blank,
		State: " Todo ", Labels: []string{" Symphony ", "BUG", "bug"},
		BlockedBy: []domain.BlockerRef{
			{ID: &blank},
			{Identifier: &blockerIdentifier, State: &blockerState},
		},
		CreatedAt: &createdAt,
	}

	got, err := NormalizeIssue(issue)
	if err != nil {
		t.Fatal(err)
	}
	nested["id"] = uint64(1)
	issue.Labels[0] = "changed"
	blockerIdentifier = "changed"
	createdAt = time.Time{}

	if got.State != " Todo " || !slices.Equal(got.Labels, []string{"symphony", "bug"}) {
		t.Fatalf("display state or labels = state %q labels %q", got.State, got.Labels)
	}
	if len(got.BlockedBy) != 1 || got.BlockedBy[0].ID != nil || got.BlockedBy[0].Identifier == nil || *got.BlockedBy[0].Identifier != "LIN-2" {
		t.Fatalf("blockers = %#v, want one usable nullable blocker", got.BlockedBy)
	}
	if got.CreatedAt == nil || got.CreatedAt.Location() != time.UTC || !got.CreatedAt.Equal(time.Date(2026, time.August, 8, 14, 30, 0, 0, time.UTC)) {
		t.Fatalf("created_at = %#v, want equivalent UTC instant", got.CreatedAt)
	}
	if value := got.NativeRef["nested"].(map[string]any)["id"]; value != uint64(9007199254740993) {
		t.Fatalf("native_ref ID = %#v, want lossless copied uint64", value)
	}
	if got.Description != nil || got.BranchName != nil || got.URL != nil || got.AssigneeID != nil {
		t.Fatalf("blank optional strings did not fall back to null: %#v", got)
	}
	if got.UpdatedAt != nil || got.BlockedBy == nil || got.Labels == nil {
		t.Fatalf("normalized nullable/collection fields = %#v", got)
	}
}

func TestNormalizeIssueFallsBackFromUnsafeOptionalNativeRef(t *testing.T) {
	// Break caught: optional provider metadata must not discard an otherwise
	// valid issue, and a cycle must not recurse forever.
	cycle := map[string]any{}
	cycle["self"] = cycle
	got, err := NormalizeIssue(domain.Issue{
		ID: "42", Identifier: "GH-42", Title: "Title", State: "open", NativeRef: cycle,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.NativeRef != nil {
		t.Fatalf("native_ref = %#v, want safe null fallback", got.NativeRef)
	}
}

func TestNormalizeIssueValidatesProviderNeutralRequiredFields(t *testing.T) {
	// Break caught: provider-specific identifier regexes would reject valid
	// opaque provider values, while whitespace-only values must still fail.
	if _, err := NormalizeIssue(domain.Issue{ID: "opaque/id", Identifier: "TEAM/#42", Title: "Title", State: "Open"}); err != nil {
		t.Fatalf("provider-neutral identifiers rejected: %v", err)
	}
	if _, err := NormalizeIssue(domain.Issue{ID: "opaque/id", Identifier: " ", Title: "Title", State: "Open"}); !errors.Is(err, domain.ErrInvalidIssue) {
		t.Fatalf("blank identifier error = %v, want ErrInvalidIssue", err)
	}
}
