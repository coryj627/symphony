package orchestrator

import (
	"slices"
	"testing"

	"github.com/coryj627/symphony/go/internal/domain"
)

func TestSortForDispatchExactBuckets(t *testing.T) {
	old := mustTime(t, "2026-01-01T00:00:00Z")
	newer := mustTime(t, "2026-02-01T00:00:00Z")
	input := []domain.Issue{
		issueWith("Z", intPointer(9), timePointer(old)),
		issueWith("N", nil, nil),
		issueWith("B", intPointer(1), timePointer(newer)),
		issueWith("A", intPointer(1), timePointer(newer)),
		issueWith("C", intPointer(4), timePointer(old)),
	}

	got := SortForDispatch(input)
	gotIdentifiers := make([]string, len(got))
	inputIdentifiers := make([]string, len(input))
	for index := range got {
		gotIdentifiers[index] = got[index].Identifier
		inputIdentifiers[index] = input[index].Identifier
	}
	if !slices.Equal(gotIdentifiers, []string{"A", "B", "C", "Z", "N"}) {
		t.Fatalf("dispatch order = %v", gotIdentifiers)
	}
	if !slices.Equal(inputIdentifiers, []string{"Z", "N", "B", "A", "C"}) {
		t.Fatalf("input mutated = %v", inputIdentifiers)
	}
}

func TestSortForDispatchTreatsAllUnknownPrioritiesAsOneBucket(t *testing.T) {
	old := mustTime(t, "2026-01-01T00:00:00Z")
	newer := mustTime(t, "2026-02-01T00:00:00Z")
	input := []domain.Issue{
		issueWith("null", nil, timePointer(old)),
		issueWith("zero", intPointer(0), timePointer(newer)),
		issueWith("negative", intPointer(-1), timePointer(old)),
		issueWith("five", intPointer(5), nil),
	}

	got := SortForDispatch(input)
	identifiers := make([]string, len(got))
	for index := range got {
		identifiers[index] = got[index].Identifier
	}
	if !slices.Equal(identifiers, []string{"negative", "null", "zero", "five"}) {
		t.Fatalf("unknown-priority order = %v", identifiers)
	}
}
