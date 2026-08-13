package linear

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/tracker"
)

func TestLinearToolGraphQLErrorsRetainBoundedRedactedData(t *testing.T) {
	redactor := observability.NewRedactor(nil, nil)
	redactor.RegisterSecret([]byte(tokenCanary))
	result := linearGraphQLErrorResult(
		map[string]any{"viewer": map[string]any{"name": "before-" + tokenCanary}},
		[]any{map[string]any{"message": "failed " + tokenCanary, "extensions": map[string]any{"code": "BAD_USER_INPUT"}}},
		redactor,
	)
	if result.Success || result.Error == nil || result.Error.Code != "graphql_errors" || len(result.Errors) != 1 {
		t.Fatalf("result=%+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), tokenCanary) || !strings.Contains(string(encoded), "[REDACTED]") {
		t.Fatalf("unsafe result=%s", encoded)
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLinearToolFailureCarriesPortableRateLimitMetadata(t *testing.T) {
	result := linearTrackerFailureResult(&tracker.Error{
		Category: tracker.CategoryRateLimited, Message: "Linear rate limit was reached",
		Retryable: true, RetryAfter: 90 * time.Second, Status: 429,
	}, linearQuery)
	if result.Error == nil || result.Error.Code != "rate_limited" || !result.Error.Retryable || result.Error.Status != 429 || result.Error.RetryAfterMS != 90000 {
		t.Fatalf("result=%+v", result)
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLinearMutationAmbiguousFailureIsNeverMarkedRetryable(t *testing.T) {
	result := linearTrackerFailureResult(&tracker.Error{
		Category: tracker.CategoryTransport, Message: "Linear request failed", Retryable: true,
	}, linearMutation)
	if result.Error == nil || result.Error.Retryable {
		t.Fatalf("result=%+v", result)
	}
}

func TestToolResultRejectsUnsafeErrors(t *testing.T) {
	result := domain.ToolResult{
		Success: false,
		Errors:  []any{func() {}},
		Error:   &domain.ToolError{Code: "graphql_errors", Message: "Linear GraphQL returned errors."},
	}
	if result.Validate() == nil {
		t.Fatal("unsafe errors were accepted")
	}
}

func TestLinearToolEnvelopeRequiresObjectDataAndObjectErrors(t *testing.T) {
	for _, body := range []string{
		`{"data":[]}`,
		`{"errors":["unsafe shape"]}`,
		`{"errors":[]}`,
		`{"errors":null}`,
		`{"data":{"viewer":null}} {"data":{}}`,
	} {
		if _, _, ok := decodeLinearToolEnvelope([]byte(body)); ok {
			t.Fatalf("accepted malformed envelope %s", body)
		}
	}
}
