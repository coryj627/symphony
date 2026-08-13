package domain

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

const ToolUnavailableCode = "tool_unavailable"

var ErrToolUnavailable = errors.New(ToolUnavailableCode)

var ErrInvalidToolContract = errors.New("invalid_tool_contract")

// ToolSpec is an adapter-owned dynamic tool declaration. InputSchema must be a
// JSON-safe schema object and must not contain credentials.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// ToolCall carries one JSON-safe adapter-owned argument value. Arguments is
// intentionally not restricted to an object because an adapter may document a
// scalar shorthand.
type ToolCall struct {
	Name      string `json:"name"`
	Arguments any    `json:"arguments"`
}

// ToolResult is translated to the targeted app-server protocol. A failure is
// data, not a returned Go error, so unsupported calls cannot stall a session.
type ToolResult struct {
	Success bool       `json:"success"`
	Data    any        `json:"data,omitempty"`
	Errors  []any      `json:"errors,omitempty"`
	Error   *ToolError `json:"error,omitempty"`
}

type ToolError struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	Retryable    bool   `json:"retryable,omitempty"`
	RetryAfterMS int64  `json:"retry_after_ms,omitempty"`
	Status       int    `json:"status,omitempty"`
}

func (spec ToolSpec) Validate() error {
	if strings.TrimSpace(spec.Name) == "" {
		return fmt.Errorf("%w: tool name is required", ErrInvalidToolContract)
	}
	if spec.InputSchema == nil {
		return fmt.Errorf("%w: input schema is required", ErrInvalidToolContract)
	}
	if _, err := cloneNativeRef(spec.InputSchema); err != nil {
		return fmt.Errorf("%w: input schema: %v", ErrInvalidToolContract, err)
	}
	return nil
}

func (call ToolCall) Validate() error {
	if strings.TrimSpace(call.Name) == "" {
		return fmt.Errorf("%w: tool name is required", ErrInvalidToolContract)
	}
	if _, err := cloneToolValue(call.Arguments); err != nil {
		return fmt.Errorf("%w: arguments: %v", ErrInvalidToolContract, err)
	}
	return nil
}

func (result ToolResult) Validate() error {
	if result.Success && (result.Error != nil || len(result.Errors) != 0) {
		return fmt.Errorf("%w: successful result contains errors", ErrInvalidToolContract)
	}
	if !result.Success {
		if result.Error == nil {
			return fmt.Errorf("%w: failed result requires an error", ErrInvalidToolContract)
		}
		if strings.TrimSpace(result.Error.Code) == "" || strings.TrimSpace(result.Error.Message) == "" {
			return fmt.Errorf("%w: failed result requires an error code and message", ErrInvalidToolContract)
		}
	}
	if _, err := cloneToolValue(result.Data); err != nil {
		return fmt.Errorf("%w: result data: %v", ErrInvalidToolContract, err)
	}
	if _, err := cloneToolValue(result.Errors); err != nil {
		return fmt.Errorf("%w: result errors: %v", ErrInvalidToolContract, err)
	}
	if result.Error != nil && (result.Error.RetryAfterMS < 0 || result.Error.Status < 0) {
		return fmt.Errorf("%w: result error metadata is invalid", ErrInvalidToolContract)
	}
	return nil
}

func ToolSuccess(data any) (ToolResult, error) {
	owned, err := cloneToolValue(data)
	if err != nil {
		return ToolResult{}, fmt.Errorf("%w: result data: %v", ErrInvalidToolContract, err)
	}
	return ToolResult{Success: true, Data: owned}, nil
}

func ToolFailure(code, safeMessage string) ToolResult {
	code = strings.TrimSpace(code)
	if code == "" {
		code = "tool_error"
	}
	safeMessage = strings.TrimSpace(safeMessage)
	if safeMessage == "" {
		safeMessage = "The tracker tool failed."
	}
	return ToolResult{
		Success: false,
		Error:   &ToolError{Code: code, Message: safeMessage},
	}
}

func ToolUnavailableResult() ToolResult {
	return ToolFailure(ToolUnavailableCode, "The requested tracker tool is unavailable.")
}

func cloneToolValue(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	clone, err := cloneJSONValue(reflect.ValueOf(value), make(map[jsonVisit]struct{}), 0)
	if err != nil {
		return nil, err
	}
	return clone.Interface(), nil
}
