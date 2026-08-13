package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

var dynamicToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ApprovalPolicy preserves one schema-reviewed approval policy value.
type ApprovalPolicy struct {
	raw json.RawMessage
}

// ParseApprovalPolicy validates a policy against the pinned string or granular shapes.
func ParseApprovalPolicy(source string) (ApprovalPolicy, error) {
	raw := json.RawMessage(source)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return ApprovalPolicy{}, errors.New("approval policy is not valid JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ApprovalPolicy{}, errors.New("approval policy has trailing JSON")
	}
	if !validApprovalPolicyValue(value) {
		return ApprovalPolicy{}, errors.New("approval policy is not supported by the pinned schema")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return ApprovalPolicy{}, errors.New("approval policy cannot be encoded")
	}
	return ApprovalPolicy{raw: canonical}, nil
}

func (policy ApprovalPolicy) MarshalJSON() ([]byte, error) {
	if len(policy.raw) == 0 {
		return nil, errors.New("approval policy is unset")
	}
	return bytes.Clone(policy.raw), nil
}

func (policy *ApprovalPolicy) UnmarshalJSON(source []byte) error {
	parsed, err := ParseApprovalPolicy(string(source))
	if err != nil {
		return err
	}
	*policy = parsed
	return nil
}

func validApprovalPolicyValue(value any) bool {
	if name, ok := value.(string); ok {
		return name == "untrusted" || name == "on-request" || name == "never"
	}
	root, ok := value.(map[string]any)
	if !ok || len(root) != 1 {
		return false
	}
	granular, ok := root["granular"].(map[string]any)
	if !ok {
		return false
	}
	required := map[string]bool{
		"mcp_elicitations": false,
		"rules":            false,
		"sandbox_approval": false,
	}
	for name, setting := range granular {
		if _, ok := setting.(bool); !ok {
			return false
		}
		switch name {
		case "mcp_elicitations", "rules", "sandbox_approval":
			required[name] = true
		case "request_permissions", "skill_approval":
		default:
			return false
		}
	}
	for _, present := range required {
		if !present {
			return false
		}
	}
	return true
}

type ClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type InitializeCapabilities struct {
	ExperimentalAPI bool `json:"experimentalApi"`
}

type InitializeParams struct {
	ClientInfo   ClientInfo             `json:"clientInfo"`
	Capabilities InitializeCapabilities `json:"capabilities"`
}

type DynamicToolSpec struct {
	Type         string          `json:"type"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	DeferLoading bool            `json:"deferLoading,omitempty"`
}

type ThreadStartParams struct {
	ApprovalPolicy        ApprovalPolicy    `json:"approvalPolicy"`
	Cwd                   string            `json:"cwd"`
	DynamicTools          []DynamicToolSpec `json:"dynamicTools"`
	RuntimeWorkspaceRoots []string          `json:"runtimeWorkspaceRoots"`
	Sandbox               string            `json:"sandbox"`
}

type ThreadStartResponse struct {
	Thread Thread `json:"thread"`
}

type Thread struct {
	ID string `json:"id"`
}

type UserInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type WorkspaceWriteSandboxPolicy struct {
	Type            string   `json:"type"`
	WritableRoots   []string `json:"writableRoots"`
	NetworkAccess   bool     `json:"networkAccess"`
	ExcludeSlashTmp bool     `json:"excludeSlashTmp,omitempty"`
	ExcludeTmpdir   bool     `json:"excludeTmpdirEnvVar,omitempty"`
}

type TurnStartParams struct {
	ThreadID              string                      `json:"threadId"`
	Input                 []UserInput                 `json:"input"`
	Cwd                   string                      `json:"cwd"`
	RuntimeWorkspaceRoots []string                    `json:"runtimeWorkspaceRoots"`
	SandboxPolicy         WorkspaceWriteSandboxPolicy `json:"sandboxPolicy"`
}

type TurnStartResponse struct {
	Turn Turn `json:"turn"`
}

type Turn struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Error  *TurnError      `json:"error,omitempty"`
	Items  json.RawMessage `json:"items,omitempty"`
}

type TurnError struct {
	Message string `json:"message"`
}

type TurnCompletedNotification struct {
	ThreadID string `json:"threadId"`
	Turn     Turn   `json:"turn"`
}

type TurnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

type TokenUsageBreakdown struct {
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	InputTokens           int64 `json:"inputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
	TotalTokens           int64 `json:"totalTokens"`
}

type ThreadTokenUsage struct {
	Last               TokenUsageBreakdown `json:"last"`
	Total              TokenUsageBreakdown `json:"total"`
	ModelContextWindow *int64              `json:"modelContextWindow,omitempty"`
}

type ThreadTokenUsageUpdatedNotification struct {
	ThreadID   string           `json:"threadId"`
	TurnID     string           `json:"turnId"`
	TokenUsage ThreadTokenUsage `json:"tokenUsage"`
}

func validateDynamicTool(tool DynamicToolSpec) error {
	if tool.Type != "function" || !dynamicToolNamePattern.MatchString(tool.Name) {
		return fmt.Errorf("dynamic tool %q has an invalid type or name", tool.Name)
	}
	if tool.Description == "" || len(tool.Description) > 2048 {
		return fmt.Errorf("dynamic tool %q has an invalid description", tool.Name)
	}
	var schema map[string]any
	if len(tool.InputSchema) == 0 || json.Unmarshal(tool.InputSchema, &schema) != nil || schema == nil {
		return fmt.Errorf("dynamic tool %q has an invalid input schema", tool.Name)
	}
	return nil
}
