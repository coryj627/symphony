package workflow

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultPollingInterval        = 30 * time.Second
	defaultHookTimeout            = time.Minute
	defaultMaxConcurrent          = 10
	defaultMaxTurns               = 20
	defaultMaxRetryBackoff        = 5 * time.Minute
	defaultCodexCommand           = "codex app-server"
	defaultTurnTimeout            = time.Hour
	defaultReadTimeout            = 5 * time.Second
	defaultStallTimeout           = 5 * time.Minute
	defaultOperatorResponseWindow = 10 * time.Minute
	minimumOperatorResponseWindow = 30 * time.Second
)

// Resolve applies workflow defaults and validates the typed settings Symphony
// owns. Provider fields remain adapter-owned and are retained unchanged.
func Resolve(path string, definition Definition, lookup LookupEnv) (EffectiveConfig, error) {
	raw, err := rawMapping(path, definition)
	if err != nil {
		return EffectiveConfig{}, err
	}

	config := EffectiveConfig{
		Tracker:   TrackerConfig{Provider: map[string]any{}, RequiredLabels: []string{}, ActiveStates: []string{}, TerminalStates: []string{}},
		Polling:   PollingConfig{Interval: defaultPollingInterval},
		Workspace: WorkspaceConfig{Root: filepath.Join(os.TempDir(), "symphony_workspaces")},
		Hooks:     HooksConfig{Timeout: defaultHookTimeout},
		Agent:     AgentConfig{MaxConcurrent: defaultMaxConcurrent, MaxTurns: defaultMaxTurns, MaxRetryBackoff: defaultMaxRetryBackoff, MaxConcurrentByState: map[string]int{}},
		Codex:     CodexConfig{Command: defaultCodexCommand, TurnTimeout: defaultTurnTimeout, ReadTimeout: defaultReadTimeout, StallTimeout: defaultStallTimeout},
		Server:    ServerConfig{Port: 0, OperatorResponseWindow: defaultOperatorResponseWindow},
	}

	if err := resolveTracker(path, raw, &config); err != nil {
		return EffectiveConfig{}, err
	}
	if err := resolvePolling(path, raw, &config); err != nil {
		return EffectiveConfig{}, err
	}
	if err := resolveWorkspace(path, raw, lookup, &config); err != nil {
		return EffectiveConfig{}, err
	}
	if err := resolveHooks(path, raw, &config); err != nil {
		return EffectiveConfig{}, err
	}
	if err := resolveAgent(path, raw, &config); err != nil {
		return EffectiveConfig{}, err
	}
	if err := resolveCodex(path, raw, &config); err != nil {
		return EffectiveConfig{}, err
	}
	if err := resolveServer(path, raw, &config); err != nil {
		return EffectiveConfig{}, err
	}
	return config, nil
}

func rawMapping(path string, definition Definition) (map[string]any, error) {
	if definition.FrontMatter == nil {
		return nil, workflowError(ErrWorkflowParse, path, 1, 1, "front matter is missing")
	}
	var raw map[string]any
	if err := definition.FrontMatter.Decode(&raw); err != nil {
		return nil, workflowError(ErrWorkflowParse, path, definition.FrontMatter.Line, definition.FrontMatter.Column, err.Error())
	}
	if raw == nil {
		return map[string]any{}, nil
	}
	return raw, nil
}

func resolveTracker(path string, raw map[string]any, config *EffectiveConfig) error {
	section, err := object(path, raw, "tracker")
	if err != nil || section == nil {
		return err
	}
	if value, ok := section["kind"]; ok {
		kind, ok := value.(string)
		if !ok {
			return fieldError(path, "tracker.kind", "must be a string")
		}
		config.Tracker.Kind = kind
	}
	if value, ok := section["provider"]; ok {
		provider, ok := value.(map[string]any)
		if !ok {
			return fieldError(path, "tracker.provider", "must be an object")
		}
		normalized, err := jsonSafe(path, "tracker.provider", provider)
		if err != nil {
			return err
		}
		config.Tracker.Provider = normalized.(map[string]any)
	}
	for key, destination := range map[string]*[]string{
		"required_labels": &config.Tracker.RequiredLabels,
		"active_states":   &config.Tracker.ActiveStates,
		"terminal_states": &config.Tracker.TerminalStates,
	} {
		if value, ok := section[key]; ok {
			strings, ok := stringSlice(value)
			if !ok {
				return fieldError(path, "tracker."+key, "must be a list of strings")
			}
			*destination = strings
		}
	}
	return nil
}

func resolvePolling(path string, raw map[string]any, config *EffectiveConfig) error {
	section, err := object(path, raw, "polling")
	if err != nil || section == nil {
		return err
	}
	if value, ok := section["interval_ms"]; ok {
		duration, err := milliseconds(path, "polling.interval_ms", value, true)
		if err != nil {
			return err
		}
		config.Polling.Interval = duration
	}
	return nil
}

func resolveWorkspace(path string, raw map[string]any, lookup LookupEnv, config *EffectiveConfig) error {
	section, err := object(path, raw, "workspace")
	if err != nil {
		return err
	}
	root := config.Workspace.Root
	if section != nil {
		if value, ok := section["root"]; ok {
			var valid bool
			root, valid = value.(string)
			if !valid {
				return fieldError(path, "workspace.root", "must be a string")
			}
		}
	}
	resolved, err := resolveWorkspaceRoot(path, root, lookup)
	if err != nil {
		return err
	}
	config.Workspace.Root = resolved
	return nil
}

func resolveHooks(path string, raw map[string]any, config *EffectiveConfig) error {
	section, err := object(path, raw, "hooks")
	if err != nil || section == nil {
		return err
	}
	for key, destination := range map[string]*string{
		"after_create":  &config.Hooks.AfterCreate,
		"before_run":    &config.Hooks.BeforeRun,
		"after_run":     &config.Hooks.AfterRun,
		"before_remove": &config.Hooks.BeforeRemove,
	} {
		if value, ok := section[key]; ok {
			text, ok := value.(string)
			if !ok {
				return fieldError(path, "hooks."+key, "must be a string")
			}
			*destination = text
		}
	}
	if value, ok := section["timeout_ms"]; ok {
		duration, err := milliseconds(path, "hooks.timeout_ms", value, true)
		if err != nil {
			return err
		}
		config.Hooks.Timeout = duration
	}
	return nil
}

func resolveAgent(path string, raw map[string]any, config *EffectiveConfig) error {
	section, err := object(path, raw, "agent")
	if err != nil || section == nil {
		return err
	}
	if value, ok := section["max_concurrent_agents"]; ok {
		limit, ok := integer(value)
		if !ok || limit < 1 {
			return fieldError(path, "agent.max_concurrent_agents", "must be a positive integer")
		}
		config.Agent.MaxConcurrent = limit
	}
	if value, ok := section["max_turns"]; ok {
		turns, ok := integer(value)
		if !ok || turns < 1 {
			return fieldError(path, "agent.max_turns", "must be a positive integer")
		}
		config.Agent.MaxTurns = turns
	}
	if value, ok := section["max_retry_backoff_ms"]; ok {
		duration, err := milliseconds(path, "agent.max_retry_backoff_ms", value, false)
		if err != nil {
			return err
		}
		config.Agent.MaxRetryBackoff = duration
	}
	if value, ok := section["max_concurrent_agents_by_state"]; ok {
		limits, ok := value.(map[string]any)
		if !ok {
			return fieldError(path, "agent.max_concurrent_agents_by_state", "must be an object")
		}
		for state, rawLimit := range limits {
			limit, valid := integer(rawLimit)
			state = strings.ToLower(strings.TrimSpace(state))
			if !valid || limit < 1 || state == "" {
				continue
			}
			config.Agent.MaxConcurrentByState[state] = limit
		}
	}
	return nil
}

func resolveCodex(path string, raw map[string]any, config *EffectiveConfig) error {
	section, err := object(path, raw, "codex")
	if err != nil || section == nil {
		return err
	}
	if value, ok := section["command"]; ok {
		command, ok := value.(string)
		if !ok || strings.TrimSpace(command) == "" {
			return fieldError(path, "codex.command", "must be a non-empty string")
		}
		config.Codex.Command = command
	}
	if value, ok := section["approval_policy"]; ok {
		normalized, err := jsonSafe(path, "codex.approval_policy", value)
		if err != nil {
			return err
		}
		config.Codex.ApprovalPolicy = normalized
	}
	if value, ok := section["thread_sandbox"]; ok {
		sandbox, ok := value.(string)
		if !ok {
			return fieldError(path, "codex.thread_sandbox", "must be a string")
		}
		config.Codex.ThreadSandbox = sandbox
	}
	if value, ok := section["turn_sandbox_policy"]; ok {
		policy, ok := value.(map[string]any)
		if !ok {
			return fieldError(path, "codex.turn_sandbox_policy", "must be an object")
		}
		normalized, err := jsonSafe(path, "codex.turn_sandbox_policy", policy)
		if err != nil {
			return err
		}
		config.Codex.TurnSandboxPolicy = normalized.(map[string]any)
	}
	for key, setting := range map[string]struct {
		destination *time.Duration
		positive    bool
	}{
		"turn_timeout_ms":  {&config.Codex.TurnTimeout, true},
		"read_timeout_ms":  {&config.Codex.ReadTimeout, true},
		"stall_timeout_ms": {&config.Codex.StallTimeout, false},
	} {
		if value, ok := section[key]; ok {
			duration, err := milliseconds(path, "codex."+key, value, setting.positive)
			if err != nil {
				return err
			}
			*setting.destination = duration
		}
	}
	return nil
}

func resolveServer(path string, raw map[string]any, config *EffectiveConfig) error {
	section, err := object(path, raw, "server")
	if err != nil || section == nil {
		return err
	}
	if value, ok := section["port"]; ok {
		port, ok := integer(value)
		if !ok || port < 0 || port > 65535 {
			return fieldError(path, "server.port", "must be an integer from 0 through 65535")
		}
		config.Server.Port = port
	}
	if value, ok := section["operator_response_timeout_ms"]; ok {
		duration, err := milliseconds(path, "server.operator_response_timeout_ms", value, true)
		if err != nil {
			return err
		}
		if duration < minimumOperatorResponseWindow {
			return fieldError(path, "server.operator_response_timeout_ms", "must be at least 30000ms")
		}
		config.Server.OperatorResponseWindow = duration
	}
	return nil
}

func object(path string, raw map[string]any, key string) (map[string]any, error) {
	value, ok := raw[key]
	if !ok || value == nil {
		return nil, nil
	}
	section, ok := value.(map[string]any)
	if !ok {
		return nil, fieldError(path, key, "must be an object")
	}
	return section, nil
}

func stringSlice(value any) ([]string, bool) {
	values, ok := value.([]any)
	if !ok {
		return nil, false
	}
	result := make([]string, len(values))
	for index, value := range values {
		text, ok := value.(string)
		if !ok {
			return nil, false
		}
		result[index] = text
	}
	return result, true
}

func integer(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case int64:
		if number < math.MinInt || number > math.MaxInt {
			return 0, false
		}
		return int(number), true
	case uint64:
		if number > math.MaxInt {
			return 0, false
		}
		return int(number), true
	default:
		return 0, false
	}
}

func milliseconds(path, field string, value any, positive bool) (time.Duration, error) {
	amount, ok := integer(value)
	if !ok || (positive && amount < 1) || (!positive && amount < 0) {
		return 0, fieldError(path, field, "must be a valid millisecond integer")
	}
	if int64(amount) > int64(math.MaxInt64/int64(time.Millisecond)) {
		return 0, fieldError(path, field, "is too large")
	}
	return time.Duration(amount) * time.Millisecond, nil
}

func resolveWorkspaceRoot(path, root string, lookup LookupEnv) (string, error) {
	if variable, ok := exactEnvironmentReference(root); ok {
		if lookup == nil {
			lookup = os.LookupEnv
		}
		value, found := lookup(variable)
		if !found || value == "" {
			return "", fieldError(path, "workspace.root", "references an unset environment variable")
		}
		root = value
	}
	if root == "~" || strings.HasPrefix(root, "~/") || strings.HasPrefix(root, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fieldError(path, "workspace.root", err.Error())
		}
		root = filepath.Join(home, root[2:])
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(filepath.Dir(path), root)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fieldError(path, "workspace.root", err.Error())
	}
	return filepath.Clean(abs), nil
}

func exactEnvironmentReference(value string) (string, bool) {
	if len(value) < 2 || value[0] != '$' {
		return "", false
	}
	for index, runeValue := range value[1:] {
		if !(runeValue == '_' || runeValue >= 'A' && runeValue <= 'Z' || runeValue >= 'a' && runeValue <= 'z' || index > 0 && runeValue >= '0' && runeValue <= '9') {
			return "", false
		}
	}
	return value[1:], true
}

func jsonSafe(path, field string, value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fieldError(path, field, "must contain JSON-safe values")
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWorkflowParse, err)
	}
	return normalized, nil
}

func fieldError(path, field, detail string) error {
	return workflowError(ErrWorkflowParse, path, 1, 1, field+" "+detail)
}
