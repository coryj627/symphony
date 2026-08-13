package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/tracker"
)

const (
	linearGraphQLToolName       = "linear_graphql"
	maxLinearToolArgumentsBytes = 1 << 20
	maxLinearToolResponseBytes  = 1 << 20
)

type linearToolInput struct {
	Query     string
	Variables map[string]any
}

func linearGraphQLToolSpec() domain.ToolSpec {
	return domain.ToolSpec{
		Name:        linearGraphQLToolName,
		Description: "Run exactly one query or mutation against the captured Linear GraphQL endpoint using the captured project credential.",
		InputSchema: map[string]any{
			"oneOf": []any{
				map[string]any{"type": "string", "minLength": 1, "maxLength": maxLinearToolQueryBytes},
				map[string]any{
					"type": "object", "additionalProperties": false,
					"required": []any{"query"},
					"properties": map[string]any{
						"query":     map[string]any{"type": "string", "minLength": 1, "maxLength": maxLinearToolQueryBytes},
						"variables": map[string]any{"type": "object"},
					},
				},
			},
		},
	}
}

func parseLinearToolArguments(arguments any) (linearToolInput, string) {
	if query, ok := arguments.(string); ok {
		return linearToolInput{Query: query, Variables: map[string]any{}}, ""
	}
	var encoded []byte
	var err error
	if raw, ok := arguments.(json.RawMessage); ok {
		encoded = bytes.Clone(raw)
	} else {
		encoded, err = json.Marshal(arguments)
		if err != nil {
			return linearToolInput{}, "invalid_arguments"
		}
	}
	if len(encoded) > maxLinearToolArgumentsBytes {
		return linearToolInput{}, "invalid_arguments"
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return linearToolInput{}, "invalid_arguments"
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return linearToolInput{}, "invalid_arguments"
	}
	if query, ok := root.(string); ok {
		return linearToolInput{Query: query, Variables: map[string]any{}}, ""
	}
	object, ok := root.(map[string]any)
	if !ok || len(object) < 1 || len(object) > 2 {
		return linearToolInput{}, "invalid_arguments"
	}
	query, ok := object["query"].(string)
	if !ok {
		return linearToolInput{}, "invalid_arguments"
	}
	variables := map[string]any{}
	if rawVariables, found := object["variables"]; found {
		variables, ok = rawVariables.(map[string]any)
		if !ok {
			return linearToolInput{}, "invalid_arguments"
		}
	}
	for key := range object {
		if key != "query" && key != "variables" {
			return linearToolInput{}, "invalid_arguments"
		}
	}
	return linearToolInput{Query: query, Variables: variables}, ""
}

func (adapter *Adapter) executeLinearGraphQL(ctx context.Context, input linearToolInput, operation linearOperation) domain.ToolResult {
	if ctx == nil {
		ctx = context.Background()
	}
	credential := adapter.authorization()
	if strings.TrimSpace(credential) == "" {
		return linearToolFailure("missing_credential")
	}
	redactor := observability.NewRedactor(adapter.config.SecretEnvironmentNames(), nil)
	redactor.RegisterSecret([]byte(credential))
	payload, err := json.Marshal(graphQLRequest{Query: input.Query, Variables: input.Variables})
	if err != nil {
		return linearToolFailure("invalid_arguments")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, adapter.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return linearTrackerFailureResult(transportError(ctx, "Linear request could not be created"), operation)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", credential)
	request.Header.Set("User-Agent", linearUserAgent)

	response, err := adapter.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return linearTrackerFailureResult(ctx.Err(), operation)
		}
		return linearTrackerFailureResult(transportError(ctx, "Linear request failed"), operation)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure error
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			failure = &tracker.Error{Category: tracker.CategoryAuth, Message: "Linear authentication failed", Status: response.StatusCode}
		case http.StatusTooManyRequests:
			retryAfter := toolRetryAfter(response.Header.Get("Retry-After"))
			if retryAfter == 0 {
				retryAfter = linearRetryAfter(response.Header, time.Now())
			}
			failure = &tracker.Error{Category: tracker.CategoryRateLimited, Message: "Linear rate limit was reached", Retryable: true, RetryAfter: retryAfter, Status: response.StatusCode}
		default:
			failure = statusError(response.StatusCode)
		}
		return linearTrackerFailureResult(failure, operation)
	}
	if response.ContentLength > maxLinearToolResponseBytes {
		return linearToolFailure("response_too_large")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxLinearToolResponseBytes+1))
	if err != nil {
		return linearTrackerFailureResult(transportError(ctx, "Linear response could not be read"), operation)
	}
	if len(body) > maxLinearToolResponseBytes {
		return linearToolFailure("response_too_large")
	}
	data, errorsList, ok := decodeLinearToolEnvelope(body)
	if !ok {
		return linearToolFailure("malformed_response")
	}
	if len(errorsList) > 0 {
		return linearGraphQLErrorResult(data, errorsList, redactor)
	}
	result, err := domain.ToolSuccess(redactor.Value(data))
	if err != nil {
		return linearToolFailure("malformed_response")
	}
	return result
}

func decodeLinearToolEnvelope(body []byte) (any, []any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil || root == nil {
		return nil, nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, nil, false
	}
	data, hasData := root["data"]
	if hasData && data != nil {
		if _, ok := data.(map[string]any); !ok {
			return nil, nil, false
		}
	}
	errorsValue, hasErrors := root["errors"]
	if !hasData && !hasErrors {
		return nil, nil, false
	}
	errorsList := []any{}
	if hasErrors && errorsValue != nil {
		var ok bool
		errorsList, ok = errorsValue.([]any)
		if !ok || len(errorsList) == 0 {
			return nil, nil, false
		}
		for _, item := range errorsList {
			if _, ok := item.(map[string]any); !ok {
				return nil, nil, false
			}
		}
	}
	if !hasData && len(errorsList) == 0 {
		return nil, nil, false
	}
	return data, errorsList, true
}
