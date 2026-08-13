package orchestrator

import (
	"math"
	"testing"
	"time"
)

func TestFailureDelayExactAndSaturating(t *testing.T) {
	cap := 5 * time.Minute
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: 10 * time.Second},
		{attempt: 2, want: 20 * time.Second},
		{attempt: 3, want: 40 * time.Second},
		{attempt: 5, want: 160 * time.Second},
		{attempt: 6, want: cap},
		{attempt: 63, want: cap},
		{attempt: math.MaxInt, want: cap},
		{attempt: 0, want: cap},
	}
	for _, test := range tests {
		if got := FailureDelay(test.attempt, cap); got != test.want {
			t.Fatalf("attempt %d: FailureDelay() = %s, want %s", test.attempt, got, test.want)
		}
	}
	if got := FailureDelay(1, 5*time.Second); got != 5*time.Second {
		t.Fatalf("small cap = %s, want 5s", got)
	}
	if got := FailureDelay(1, 0); got != 0 {
		t.Fatalf("zero cap = %s, want 0", got)
	}
}

func TestContinuationDelayIsExactlyOneSecond(t *testing.T) {
	if ContinuationDelay != time.Second {
		t.Fatalf("ContinuationDelay = %s, want 1s", ContinuationDelay)
	}
}
