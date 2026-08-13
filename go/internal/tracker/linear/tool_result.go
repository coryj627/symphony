package linear

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/tracker"
)

func linearGraphQLErrorResult(data any, errorsList []any, redactor *observability.Redactor) domain.ToolResult {
	result := domain.ToolResult{
		Success: false,
		Data:    redactor.Value(data),
		Error: &domain.ToolError{
			Code: "graphql_errors", Message: "Linear GraphQL returned operation errors.",
		},
	}
	if sanitized, ok := redactor.Value(errorsList).([]any); ok {
		result.Errors = sanitized
	}
	return result
}

func linearTrackerFailureResult(err error, operation linearOperation) domain.ToolResult {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return domain.ToolFailure("canceled", "The Linear GraphQL call was canceled.")
	}
	var portable *tracker.Error
	if !errors.As(err, &portable) {
		return domain.ToolFailure("transport_error", "The Linear GraphQL request failed.")
	}
	code := "tool_error"
	message := "The Linear GraphQL call failed."
	switch portable.Category {
	case tracker.CategoryAuth:
		code, message = "authentication_failed", "Linear authentication failed."
	case tracker.CategoryTransport:
		code, message = "transport_error", "The Linear GraphQL request failed."
	case tracker.CategoryResponse:
		code, message = "http_error", "Linear returned an unsuccessful HTTP status."
	case tracker.CategoryRateLimited:
		code, message = "rate_limited", "Linear rate limited the GraphQL call."
	case tracker.CategoryPayload:
		code, message = "malformed_response", "Linear returned a malformed GraphQL response."
	}
	retryable := portable.Retryable && operation != linearMutation
	result := domain.ToolFailure(code, message)
	result.Error.Retryable = retryable
	result.Error.Status = portable.Status
	if portable.RetryAfter > 0 {
		result.Error.RetryAfterMS = portable.RetryAfter.Milliseconds()
	}
	return result
}

func linearToolFailure(code string) domain.ToolResult {
	message := map[string]string{
		"invalid_arguments":       "The linear_graphql arguments are invalid.",
		"invalid_graphql":         "The GraphQL document is invalid.",
		"invalid_operation_count": "The GraphQL document must contain exactly one operation.",
		"unsupported_operation":   "The GraphQL operation type is not supported.",
		"missing_credential":      "The captured Linear credential is unavailable.",
		"malformed_response":      "Linear returned a malformed GraphQL response.",
		"response_too_large":      "Linear returned a GraphQL response larger than 1 MiB.",
	}[strings.TrimSpace(code)]
	return domain.ToolFailure(code, message)
}

func toolRetryAfter(headerValue string) time.Duration {
	if seconds, err := time.ParseDuration(strings.TrimSpace(headerValue) + "s"); err == nil && seconds > 0 {
		if seconds > maxRateLimitRetryAfter {
			return maxRateLimitRetryAfter
		}
		return seconds
	}
	return 0
}
