package codex

import (
	"strings"
	"testing"
)

func TestContinuationGuidanceUsesOnlyBoundedTurnMetadata(t *testing.T) {
	got, err := ContinuationGuidance(2, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Continuation guidance:",
		"continuation turn #2 of 20",
		"Resume from the current workspace and workpad state",
		"do not restate them before acting",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("guidance does not contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Original task") {
		t.Fatalf("guidance repeated task text: %s", got)
	}
}

func TestContinuationGuidanceRejectsFirstOrOutOfRangeTurn(t *testing.T) {
	for _, test := range []struct {
		turn int
		max  int
	}{{1, 20}, {2, 1}, {21, 20}, {0, 0}} {
		if got, err := ContinuationGuidance(test.turn, test.max); err == nil || got != "" {
			t.Fatalf("ContinuationGuidance(%d, %d) = %q, %v", test.turn, test.max, got, err)
		}
	}
}
