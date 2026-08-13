package github

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/tracker"
)

func githubToolFailure(code string) domain.ToolResult {
	message := map[string]string{
		"invalid_arguments":         "The github_api arguments are invalid.",
		"unsupported_operation":     "The GitHub tool operation is not supported.",
		"invalid_issue_number":      "The GitHub issue number is invalid.",
		"issue_scope_mismatch":      "The GitHub tool call is outside the captured issue.",
		"invalid_session_scope":     "The captured GitHub tool session is invalid.",
		"pull_request_unsupported":  "The GitHub issue tool does not operate on pull requests.",
		"idempotency_key_required":  "create_comment requires a nonempty idempotency key.",
		"idempotency_key_reused":    "The idempotency key was already used with different comment input.",
		"idempotency_cache_full":    "The bounded GitHub idempotency cache is full.",
		"idempotency_cache_invalid": "The cached GitHub tool result is invalid.",
		"missing_credential":        "The captured GitHub credential is unavailable.",
		"canceled":                  "The GitHub tool call was canceled.",
		"response_too_large":        "GitHub returned a response larger than 1 MiB.",
		"malformed_response":        "GitHub returned a malformed tool response.",
		"response_scope_mismatch":   "GitHub returned a response outside the captured issue.",
	}[strings.TrimSpace(code)]
	return domain.ToolFailure(code, message)
}

func githubToolTransportFailure(err error, mutation bool) domain.ToolResult {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return githubToolFailure("canceled")
	}
	result := domain.ToolFailure("transport_error", "The GitHub tool request failed.")
	result.Error.Retryable = !mutation
	return result
}

func githubToolHTTPFailure(status int, header http.Header, mutation bool, redactor *observability.Redactor) domain.ToolResult {
	code := "http_error"
	message := "GitHub returned an unsuccessful HTTP status."
	retryable := !mutation && (status == http.StatusRequestTimeout || status >= 500)
	switch status {
	case http.StatusUnauthorized:
		code, message, retryable = "authentication_failed", "GitHub authentication failed.", false
	case http.StatusForbidden:
		if isRateLimited(header) {
			code, message, retryable = "rate_limited", "GitHub rate limited the tool call.", !mutation
		} else {
			code, message, retryable = "authorization_failed", "GitHub authorization failed.", false
		}
	case http.StatusNotFound:
		code, message, retryable = "not_found", "The captured GitHub issue resource was not found.", false
	case http.StatusUnprocessableEntity:
		code, message, retryable = "validation_failed", "GitHub rejected the tool input.", false
	case http.StatusTooManyRequests:
		code, message, retryable = "rate_limited", "GitHub rate limited the tool call.", !mutation
	}
	result := domain.ToolFailure(code, message)
	result.Status = status
	result.RequestID = safeGitHubRequestID(header.Get("X-GitHub-Request-Id"), redactor)
	result.Error.Status = status
	result.Error.Retryable = retryable
	if code == "rate_limited" {
		result.Error.RetryAfterMS = retryAfter(header, time.Now()).Milliseconds()
	}
	return result
}

func safeGitHubRequestID(value string, redactor *observability.Redactor) string {
	sanitized, _ := redactor.Value(value).(string)
	sanitized = strings.TrimSpace(strings.ToValidUTF8(sanitized, ""))
	sanitized = strings.Map(func(value rune) rune {
		if unicode.IsControl(value) {
			return -1
		}
		return value
	}, sanitized)
	if len(sanitized) > 512 {
		sanitized = sanitized[:512]
		for len(sanitized) > 0 && !utf8.ValidString(sanitized) {
			sanitized = sanitized[:len(sanitized)-1]
		}
	}
	return sanitized
}

func githubToolSuccess(status int, header http.Header, data any, redactor *observability.Redactor) domain.ToolResult {
	return domain.ToolResult{
		Success: true, Status: status, RequestID: safeGitHubRequestID(header.Get("X-GitHub-Request-Id"), redactor), Data: redactor.Value(data),
	}
}

func githubToolTrackerFailure(err error, mutation bool) domain.ToolResult {
	var portable *tracker.Error
	if !errors.As(err, &portable) {
		return githubToolTransportFailure(err, mutation)
	}
	result := domain.ToolFailure("tool_error", "The GitHub tool call failed.")
	result.Error.Retryable = portable.Retryable && !mutation
	result.Error.Status = portable.Status
	return result
}
