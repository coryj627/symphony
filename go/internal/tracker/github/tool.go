package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
)

const (
	githubAPIToolName             = "github_api"
	maxGitHubToolResponseBodySize = 1 << 20
	maxGitHubGETAttempts          = 2
)

func githubToolSpec() domain.ToolSpec {
	variants := make([]any, 0, len(githubToolOperations))
	for _, operation := range githubToolOperations {
		properties := map[string]any{
			"operation":    map[string]any{"const": operation},
			"issue_number": map[string]any{"type": "integer", "minimum": 1},
		}
		required := []any{"operation"}
		switch operation {
		case "update_issue":
			properties["input"] = map[string]any{
				"type": "object", "minProperties": 1, "additionalProperties": false,
				"properties": map[string]any{
					"title":        map[string]any{"type": "string", "minLength": 1, "maxLength": 4096},
					"body":         map[string]any{"type": []any{"string", "null"}, "maxLength": 1 << 20},
					"state":        map[string]any{"enum": []any{"open", "closed"}},
					"state_reason": map[string]any{"enum": []any{"completed", "not_planned", "reopened"}},
					"milestone":    map[string]any{"type": []any{"integer", "null"}, "minimum": 1},
				},
			}
			required = append(required, "input")
		case "create_comment":
			properties["input"] = map[string]any{"type": "object", "properties": map[string]any{"body": map[string]any{"type": "string", "minLength": 1, "maxLength": 1 << 20}}, "required": []any{"body"}, "additionalProperties": false}
			properties["idempotency_key"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 256}
			required = append(required, "input", "idempotency_key")
		case "set_labels":
			properties["input"] = map[string]any{"type": "object", "properties": map[string]any{"labels": map[string]any{"type": "array", "maxItems": 100, "items": map[string]any{"type": "string", "minLength": 1, "maxLength": 100}}}, "required": []any{"labels"}, "additionalProperties": false}
			required = append(required, "input")
		case "add_assignees", "remove_assignees":
			properties["input"] = map[string]any{"type": "object", "properties": map[string]any{"assignees": map[string]any{"type": "array", "minItems": 1, "maxItems": 100, "items": map[string]any{"type": "string", "minLength": 1, "maxLength": 100}}}, "required": []any{"assignees"}, "additionalProperties": false}
			required = append(required, "input")
		}
		variants = append(variants, map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false})
	}
	return domain.ToolSpec{
		Name: githubAPIToolName, Description: "Read or mutate only the captured GitHub repository's current issue using an allowlisted operation.",
		InputSchema: map[string]any{"oneOf": variants},
	}
}

func (adapter *Adapter) executeGitHubToolRequest(ctx context.Context, call parsedGitHubToolCall) domain.ToolResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return githubToolFailure("canceled")
	}
	credential := adapter.credential()
	if strings.TrimSpace(credential) == "" {
		return githubToolFailure("missing_credential")
	}
	redactor := observability.NewRedactor(adapter.config.SecretEnvironmentNames(), nil)
	redactor.RegisterSecret([]byte(credential))
	method, requestURL, body, mutation, err := adapter.githubToolRequest(call)
	if err != nil {
		return githubToolTrackerFailure(err, mutation)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return githubToolFailure("invalid_arguments")
	}
	if method == http.MethodGet {
		payload = nil
	}
	for attempt := 1; attempt <= maxGitHubGETAttempts; attempt++ {
		request, requestErr := http.NewRequestWithContext(ctx, method, requestURL.String(), bytes.NewReader(payload))
		if requestErr != nil {
			return githubToolTransportFailure(requestErr, mutation)
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		request.Header.Set("Authorization", "Bearer "+credential)
		request.Header.Set("User-Agent", githubUserAgent)
		if method != http.MethodGet {
			request.Header.Set("Content-Type", "application/json")
		}
		response, requestErr := adapter.githubToolClient(mutation).Do(request)
		if requestErr != nil {
			if !mutation && attempt < maxGitHubGETAttempts && ctx.Err() == nil && !errors.Is(requestErr, errRedirectRejected) {
				continue
			}
			if ctx.Err() != nil {
				requestErr = ctx.Err()
			}
			return githubToolTransportFailure(requestErr, mutation)
		}
		result, retry := readGitHubToolResponse(response, call.operation, call.issueNumber, mutation, redactor)
		if retry && !mutation && attempt < maxGitHubGETAttempts && ctx.Err() == nil {
			continue
		}
		return result
	}
	return githubToolFailure("tool_error")
}

func (adapter *Adapter) githubToolRequest(call parsedGitHubToolCall) (string, *url.URL, map[string]any, bool, error) {
	base, err := appendEscapedPath(adapter.collectionURL, call.issueNumber)
	if err != nil {
		return "", nil, nil, false, err
	}
	switch call.operation {
	case "get_issue":
		return http.MethodGet, base, nil, false, nil
	case "update_issue":
		return http.MethodPatch, base, call.body, true, nil
	case "list_comments":
		requestURL, pathErr := appendEscapedPath(base, "comments")
		if pathErr == nil {
			requestURL.RawQuery = "per_page=100&page=1"
		}
		return http.MethodGet, requestURL, nil, false, pathErr
	case "create_comment":
		requestURL, pathErr := appendEscapedPath(base, "comments")
		return http.MethodPost, requestURL, call.body, true, pathErr
	case "set_labels":
		requestURL, pathErr := appendEscapedPath(base, "labels")
		return http.MethodPut, requestURL, call.body, true, pathErr
	case "add_assignees":
		requestURL, pathErr := appendEscapedPath(base, "assignees")
		return http.MethodPost, requestURL, call.body, true, pathErr
	case "remove_assignees":
		requestURL, pathErr := appendEscapedPath(base, "assignees")
		return http.MethodDelete, requestURL, call.body, true, pathErr
	default:
		return "", nil, nil, false, configError("GitHub tool operation was invalid")
	}
}

func (adapter *Adapter) githubToolClient(mutation bool) *http.Client {
	clone := *adapter.client
	if mutation {
		clone.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirectRejected }
	}
	return &clone
}

func readGitHubToolResponse(response *http.Response, operation, issueNumber string, mutation bool, redactor *observability.Redactor) (domain.ToolResult, bool) {
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		retry := !mutation && (response.StatusCode == http.StatusRequestTimeout || response.StatusCode >= 500)
		return githubToolHTTPFailure(response.StatusCode, response.Header, mutation, redactor), retry
	}
	if response.ContentLength > maxGitHubToolResponseBodySize {
		result := githubToolFailure("response_too_large")
		result.Status = response.StatusCode
		result.RequestID = safeGitHubRequestID(response.Header.Get("X-GitHub-Request-Id"), redactor)
		return result, false
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxGitHubToolResponseBodySize+1))
	if err != nil {
		return githubToolTransportFailure(err, mutation), false
	}
	if len(body) > maxGitHubToolResponseBodySize {
		result := githubToolFailure("response_too_large")
		result.Status = response.StatusCode
		result.RequestID = safeGitHubRequestID(response.Header.Get("X-GitHub-Request-Id"), redactor)
		return result, false
	}
	data, code := decodeGitHubToolData(operation, issueNumber, body)
	if code != "" {
		result := githubToolFailure(code)
		result.Status = response.StatusCode
		result.RequestID = safeGitHubRequestID(response.Header.Get("X-GitHub-Request-Id"), redactor)
		return result, false
	}
	return githubToolSuccess(response.StatusCode, response.Header, data, redactor), false
}

func decodeGitHubToolData(operation, issueNumber string, body []byte) (any, string) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, "malformed_response"
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, "malformed_response"
	}
	if operation == "list_comments" || operation == "set_labels" {
		items, ok := value.([]any)
		if !ok {
			return nil, "malformed_response"
		}
		key := "comments"
		if operation == "set_labels" {
			key = "labels"
		}
		return map[string]any{key: items}, ""
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, "malformed_response"
	}
	switch operation {
	case "get_issue", "update_issue", "add_assignees", "remove_assignees":
		returned, valid := positiveToolIssueNumber(object["number"])
		if !valid {
			return nil, "malformed_response"
		}
		if returned != issueNumber {
			return nil, "response_scope_mismatch"
		}
	}
	return object, ""
}
