package tracker

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTrackerErrorCategoriesMatchPortableConcepts(t *testing.T) {
	// Break caught: changing these values breaks adapter profiles, structured
	// logs, and runtime handling shared across providers.
	got := []Category{
		CategoryConfig,
		CategoryAuth,
		CategoryTransport,
		CategoryResponse,
		CategoryPayload,
		CategoryPagination,
		CategoryRateLimited,
		CategoryScope,
	}
	want := []Category{
		"tracker_config",
		"tracker_auth",
		"tracker_transport",
		"tracker_response",
		"tracker_payload",
		"tracker_pagination",
		"tracker_rate_limited",
		"tracker_scope",
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("category[%d] = %q, want %q", index, got[index], want[index])
		}
	}
}

func TestTrackerErrorRendersOnlyBoundedOperatorText(t *testing.T) {
	// Break caught: rendering status metadata, retry internals, or unbounded
	// diagnostics from Error() creates a second path for sensitive HTTP data.
	err := Error{
		Category:   CategoryTransport,
		Message:    "request failed",
		Retryable:  true,
		RetryAfter: 15 * time.Second,
		Status:     503,
	}
	if got := err.Error(); got != "tracker_transport: request failed" {
		t.Fatalf("error = %q, want bounded category and operator text", got)
	}
	long := Error{Category: CategoryPayload, Message: strings.Repeat("é", 5000)}.Error()
	if len(long) > maxPortableErrorBytes || !strings.HasPrefix(long, "tracker_payload: ") || !strings.HasSuffix(long, "...") {
		t.Fatalf("bounded error length/prefix/suffix = %d %q", len(long), long[:min(len(long), 40)])
	}
	if strings.ToValidUTF8(long, "?") != long {
		t.Fatal("error truncation produced invalid UTF-8")
	}
}

func TestTrackerErrorWithNoMessageStillHasStableCategory(t *testing.T) {
	// Break caught: an empty operator message must remain classifiable without
	// inventing provider response detail.
	if got := (Error{Category: CategoryRateLimited}).Error(); got != "tracker_rate_limited" {
		t.Fatalf("error = %q, want tracker_rate_limited", got)
	}
}

func TestTrackerErrorSupportsTypedInspectionAndRetryMetadata(t *testing.T) {
	// Break caught: flattening the portable error to text prevents the runtime
	// from inspecting status and retry metadata without parsing Error().
	source := &Error{
		Category:   CategoryRateLimited,
		Message:    "rate limited",
		Retryable:  true,
		RetryAfter: 30 * time.Second,
		Status:     429,
	}
	var target *Error
	if !errors.As(source, &target) {
		t.Fatalf("errors.As(%T) failed", source)
	}
	if target.Category != CategoryRateLimited || !target.Retryable || target.RetryAfter != 30*time.Second || target.Status != 429 {
		t.Fatalf("typed metadata = %#v", target)
	}
}
