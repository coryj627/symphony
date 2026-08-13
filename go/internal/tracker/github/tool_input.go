package github

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/coryj627/symphony/go/internal/tracker"
)

const maxGitHubToolArgumentsBytes = 1 << 20

var githubToolOperations = []string{
	"get_issue", "update_issue", "list_comments", "create_comment", "set_labels", "add_assignees", "remove_assignees",
}

type parsedGitHubToolCall struct {
	operation      string
	issueNumber    string
	body           map[string]any
	idempotencyKey string
	digest         [sha256.Size]byte
}

type githubToolEnvelope struct {
	Operation      string          `json:"operation"`
	IssueNumber    json.RawMessage `json:"issue_number"`
	Input          json.RawMessage `json:"input"`
	IdempotencyKey *string         `json:"idempotency_key"`
}

func parseGitHubToolCall(arguments any, session tracker.Session, adapterConfig tracker.GitHubConfig) (parsedGitHubToolCall, string) {
	number, code := githubToolSessionScope(session, adapterConfig)
	if code != "" {
		return parsedGitHubToolCall{}, code
	}
	encoded, err := encodeGitHubToolArguments(arguments)
	if err != nil || len(encoded) > maxGitHubToolArgumentsBytes {
		return parsedGitHubToolCall{}, "invalid_arguments"
	}
	var envelope githubToolEnvelope
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return parsedGitHubToolCall{}, "invalid_arguments"
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return parsedGitHubToolCall{}, "invalid_arguments"
	}
	envelope.Operation = strings.TrimSpace(envelope.Operation)
	if !containsGitHubToolOperation(envelope.Operation) {
		return parsedGitHubToolCall{}, "unsupported_operation"
	}
	if len(envelope.IssueNumber) > 0 {
		requested, ok := requiredPositiveNumber(envelope.IssueNumber)
		if !ok {
			return parsedGitHubToolCall{}, "invalid_issue_number"
		}
		if requested != number {
			return parsedGitHubToolCall{}, "issue_scope_mismatch"
		}
	}
	parsed := parsedGitHubToolCall{operation: envelope.Operation, issueNumber: number}
	switch envelope.Operation {
	case "get_issue", "list_comments":
		if len(envelope.Input) != 0 || envelope.IdempotencyKey != nil {
			return parsedGitHubToolCall{}, "invalid_arguments"
		}
	case "update_issue":
		if envelope.IdempotencyKey != nil {
			return parsedGitHubToolCall{}, "invalid_arguments"
		}
		parsed.body, code = parseGitHubUpdateInput(envelope.Input)
	case "create_comment":
		key := ""
		if envelope.IdempotencyKey != nil {
			key = *envelope.IdempotencyKey
		}
		if strings.TrimSpace(key) == "" || strings.TrimSpace(key) != key || len(key) > 256 || !utf8.ValidString(key) {
			return parsedGitHubToolCall{}, "idempotency_key_required"
		}
		parsed.body, code = parseGitHubStringInput(envelope.Input, "body", false)
		parsed.idempotencyKey = key
	case "set_labels":
		if envelope.IdempotencyKey != nil {
			return parsedGitHubToolCall{}, "invalid_arguments"
		}
		parsed.body, code = parseGitHubStringListInput(envelope.Input, "labels", true)
	case "add_assignees", "remove_assignees":
		if envelope.IdempotencyKey != nil {
			return parsedGitHubToolCall{}, "invalid_arguments"
		}
		parsed.body, code = parseGitHubStringListInput(envelope.Input, "assignees", false)
	}
	if code != "" {
		return parsedGitHubToolCall{}, code
	}
	if parsed.operation == "create_comment" {
		normalized, _ := json.Marshal(map[string]any{"issue_number": parsed.issueNumber, "input": parsed.body})
		parsed.digest = sha256.Sum256(normalized)
	}
	return parsed, ""
}

func encodeGitHubToolArguments(arguments any) ([]byte, error) {
	if raw, ok := arguments.(json.RawMessage); ok {
		return bytes.Clone(raw), nil
	}
	return json.Marshal(arguments)
}

func githubToolSessionScope(session tracker.Session, adapterConfig tracker.GitHubConfig) (string, string) {
	config, ok := githubConfigForToolSession(session.ProviderConfig)
	if !ok || session.ToolScopeID() == "" || config.Owner != adapterConfig.Owner || config.Repository != adapterConfig.Repository || config.Endpoint != adapterConfig.Endpoint {
		return "", "invalid_session_scope"
	}
	for _, key := range []string{"pull_request", "pull_request_url", "is_pull_request"} {
		if _, found := session.Issue.NativeRef[key]; found {
			return "", "pull_request_unsupported"
		}
	}
	owner, ownerOK := session.Issue.NativeRef["owner"].(string)
	repository, repositoryOK := session.Issue.NativeRef["repository"].(string)
	number, numberOK := positiveToolIssueNumber(session.Issue.NativeRef["number"])
	if !ownerOK || !repositoryOK || !numberOK || owner != config.Owner || repository != config.Repository ||
		session.Issue.ID != dispatchID(config, number) || session.Issue.Identifier != "#"+number {
		return "", "invalid_session_scope"
	}
	return number, ""
}

func githubConfigForToolSession(provider tracker.ProviderConfig) (tracker.GitHubConfig, bool) {
	switch config := provider.(type) {
	case tracker.GitHubConfig:
		return config, true
	case *tracker.GitHubConfig:
		if config != nil {
			return *config, true
		}
	}
	return tracker.GitHubConfig{}, false
}

func positiveToolIssueNumber(value any) (string, bool) {
	switch number := value.(type) {
	case json.Number:
		return requiredPositiveNumber(json.RawMessage(number.String()))
	case uint64:
		encoded, _ := json.Marshal(number)
		return requiredPositiveNumber(encoded)
	case int:
		encoded, _ := json.Marshal(number)
		return requiredPositiveNumber(encoded)
	default:
		return "", false
	}
}

func parseGitHubUpdateInput(raw json.RawMessage) (map[string]any, string) {
	fields, ok := decodeStrictGitHubInput(raw, map[string]struct{}{
		"title": {}, "body": {}, "state": {}, "state_reason": {}, "milestone": {},
	})
	if !ok || len(fields) == 0 {
		return nil, "invalid_arguments"
	}
	body := make(map[string]any, len(fields))
	for name, rawValue := range fields {
		switch name {
		case "title":
			var value string
			if !decodeOneJSON(rawValue, &value) || strings.TrimSpace(value) == "" || len(value) > 4096 {
				return nil, "invalid_arguments"
			}
			body[name] = value
		case "body":
			value, valid := nullableBoundedString(rawValue, 1<<20)
			if !valid {
				return nil, "invalid_arguments"
			}
			body[name] = value
		case "state":
			var value string
			if !decodeOneJSON(rawValue, &value) || (value != "open" && value != "closed") {
				return nil, "invalid_arguments"
			}
			body[name] = value
		case "state_reason":
			var value string
			if !decodeOneJSON(rawValue, &value) || (value != "completed" && value != "not_planned" && value != "reopened") {
				return nil, "invalid_arguments"
			}
			body[name] = value
		case "milestone":
			if bytes.Equal(bytes.TrimSpace(rawValue), []byte("null")) {
				body[name] = nil
				continue
			}
			number, valid := requiredPositiveNumber(rawValue)
			if !valid {
				return nil, "invalid_arguments"
			}
			body[name] = json.Number(number)
		}
	}
	return body, ""
}

func parseGitHubStringInput(raw json.RawMessage, field string, allowEmpty bool) (map[string]any, string) {
	fields, ok := decodeStrictGitHubInput(raw, map[string]struct{}{field: {}})
	if !ok || len(fields) != 1 {
		return nil, "invalid_arguments"
	}
	var value string
	if !decodeOneJSON(fields[field], &value) || len(value) > 1<<20 || (!allowEmpty && strings.TrimSpace(value) == "") {
		return nil, "invalid_arguments"
	}
	return map[string]any{field: value}, ""
}

func parseGitHubStringListInput(raw json.RawMessage, field string, allowEmpty bool) (map[string]any, string) {
	fields, ok := decodeStrictGitHubInput(raw, map[string]struct{}{field: {}})
	if !ok || len(fields) != 1 {
		return nil, "invalid_arguments"
	}
	var values []string
	if !decodeOneJSON(fields[field], &values) || values == nil || len(values) > 100 || (!allowEmpty && len(values) == 0) {
		return nil, "invalid_arguments"
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value || len(value) > 100 || !utf8.ValidString(value) {
			return nil, "invalid_arguments"
		}
	}
	return map[string]any{field: values}, ""
}

func decodeStrictGitHubInput(raw json.RawMessage, allowed map[string]struct{}) (map[string]json.RawMessage, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if !decodeOneJSON(raw, &fields) || fields == nil {
		return nil, false
	}
	for name := range fields {
		if _, ok := allowed[name]; !ok {
			return nil, false
		}
	}
	return fields, true
}

func nullableBoundedString(raw json.RawMessage, maximum int) (any, bool) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, true
	}
	var value string
	if !decodeOneJSON(raw, &value) || len(value) > maximum || !utf8.ValidString(value) {
		return nil, false
	}
	return value, true
}

func containsGitHubToolOperation(value string) bool {
	for _, operation := range githubToolOperations {
		if value == operation {
			return true
		}
	}
	return false
}
