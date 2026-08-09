package workflow

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type fieldLocator struct {
	path string
	root *yaml.Node
}

func (locator fieldLocator) error(field, detail string) error {
	line, column := 1, 1
	if node := mappingFieldNode(locator.root, field); node != nil {
		line, column = node.Line+1, node.Column
	}
	if field != "" {
		detail = field + " " + detail
	}
	return workflowError(ErrWorkflowParse, locator.path, line, column, detail)
}

func mappingFieldNode(root *yaml.Node, field string) *yaml.Node {
	if root == nil || len(root.Content) != 1 {
		return nil
	}
	node := root.Content[0]
	for _, key := range strings.Split(field, ".") {
		if node.Kind != yaml.MappingNode {
			return nil
		}
		var value *yaml.Node
		for index := 0; index+1 < len(node.Content); index += 2 {
			if node.Content[index].Value == key {
				value = node.Content[index+1]
				break
			}
		}
		if value == nil {
			return nil
		}
		node = value
	}
	return node
}

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
	locator := fieldLocator{path: path, root: definition.FrontMatter}
	raw, err := rawMapping(locator, definition)
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

	if err := resolveTracker(locator, raw, &config); err != nil {
		return EffectiveConfig{}, err
	}
	if err := resolvePolling(locator, raw, &config); err != nil {
		return EffectiveConfig{}, err
	}
	if err := resolveWorkspace(locator, raw, lookup, &config); err != nil {
		return EffectiveConfig{}, err
	}
	if err := resolveHooks(locator, raw, &config); err != nil {
		return EffectiveConfig{}, err
	}
	if err := resolveAgent(locator, raw, &config); err != nil {
		return EffectiveConfig{}, err
	}
	if err := resolveCodex(locator, raw, &config); err != nil {
		return EffectiveConfig{}, err
	}
	if err := resolveServer(locator, raw, &config); err != nil {
		return EffectiveConfig{}, err
	}
	return config, nil
}

func rawMapping(locator fieldLocator, definition Definition) (map[string]any, error) {
	if definition.FrontMatter == nil {
		return nil, locator.error("", "front matter is missing")
	}
	var raw map[string]any
	if err := definition.FrontMatter.Decode(&raw); err != nil {
		return nil, locator.error("", err.Error())
	}
	if raw == nil {
		return map[string]any{}, nil
	}
	return raw, nil
}

func resolveTracker(locator fieldLocator, raw map[string]any, config *EffectiveConfig) error {
	section, err := object(locator, raw, "tracker")
	if err != nil || section == nil {
		return err
	}
	if value, ok := section["kind"]; ok {
		kind, ok := value.(string)
		if !ok {
			return fieldError(locator, "tracker.kind", "must be a string")
		}
		config.Tracker.Kind = kind
	}
	if value, ok := section["provider"]; ok {
		provider, ok := value.(map[string]any)
		if !ok {
			return fieldError(locator, "tracker.provider", "must be an object")
		}
		normalized, err := jsonSafe(locator, "tracker.provider", provider)
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
				return fieldError(locator, "tracker."+key, "must be a list of strings")
			}
			*destination = strings
		}
	}
	return nil
}

func resolvePolling(locator fieldLocator, raw map[string]any, config *EffectiveConfig) error {
	section, err := object(locator, raw, "polling")
	if err != nil || section == nil {
		return err
	}
	if value, ok := section["interval_ms"]; ok {
		duration, err := milliseconds(locator, "polling.interval_ms", value, 1)
		if err != nil {
			return err
		}
		config.Polling.Interval = duration
	}
	return nil
}

func resolveWorkspace(locator fieldLocator, raw map[string]any, lookup LookupEnv, config *EffectiveConfig) error {
	section, err := object(locator, raw, "workspace")
	if err != nil {
		return err
	}
	root := config.Workspace.Root
	if section != nil {
		if value, ok := section["root"]; ok {
			var valid bool
			root, valid = value.(string)
			if !valid {
				return fieldError(locator, "workspace.root", "must be a string")
			}
		}
	}
	resolved, err := resolveWorkspaceRoot(locator, root, lookup)
	if err != nil {
		return err
	}
	config.Workspace.Root = resolved
	return nil
}

func resolveHooks(locator fieldLocator, raw map[string]any, config *EffectiveConfig) error {
	section, err := object(locator, raw, "hooks")
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
				return fieldError(locator, "hooks."+key, "must be a string")
			}
			*destination = text
		}
	}
	if value, ok := section["timeout_ms"]; ok {
		duration, err := milliseconds(locator, "hooks.timeout_ms", value, 1)
		if err != nil {
			return err
		}
		config.Hooks.Timeout = duration
	}
	return nil
}

func resolveAgent(locator fieldLocator, raw map[string]any, config *EffectiveConfig) error {
	section, err := object(locator, raw, "agent")
	if err != nil || section == nil {
		return err
	}
	if value, ok := section["max_concurrent_agents"]; ok {
		limit, ok := integer(value)
		if !ok || limit < 1 {
			return fieldError(locator, "agent.max_concurrent_agents", "must be a positive integer")
		}
		config.Agent.MaxConcurrent = limit
	}
	if value, ok := section["max_turns"]; ok {
		turns, ok := integer(value)
		if !ok || turns < 1 {
			return fieldError(locator, "agent.max_turns", "must be a positive integer")
		}
		config.Agent.MaxTurns = turns
	}
	if value, ok := section["max_retry_backoff_ms"]; ok {
		duration, err := milliseconds(locator, "agent.max_retry_backoff_ms", value, 0)
		if err != nil {
			return err
		}
		config.Agent.MaxRetryBackoff = duration
	}
	if value, ok := section["max_concurrent_agents_by_state"]; ok {
		limits, ok := value.(map[string]any)
		if !ok {
			return fieldError(locator, "agent.max_concurrent_agents_by_state", "must be an object")
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

func resolveCodex(locator fieldLocator, raw map[string]any, config *EffectiveConfig) error {
	section, err := object(locator, raw, "codex")
	if err != nil || section == nil {
		return err
	}
	if value, ok := section["command"]; ok {
		command, ok := value.(string)
		if !ok || strings.TrimSpace(command) == "" {
			return fieldError(locator, "codex.command", "must be a non-empty string")
		}
		config.Codex.Command = command
	}
	if value, ok := section["approval_policy"]; ok {
		normalized, err := jsonSafe(locator, "codex.approval_policy", value)
		if err != nil {
			return err
		}
		config.Codex.ApprovalPolicy = normalized
	}
	if value, ok := section["thread_sandbox"]; ok {
		sandbox, ok := value.(string)
		if !ok {
			return fieldError(locator, "codex.thread_sandbox", "must be a string")
		}
		config.Codex.ThreadSandbox = sandbox
	}
	if value, ok := section["turn_sandbox_policy"]; ok {
		policy, ok := value.(map[string]any)
		if !ok {
			return fieldError(locator, "codex.turn_sandbox_policy", "must be an object")
		}
		normalized, err := jsonSafe(locator, "codex.turn_sandbox_policy", policy)
		if err != nil {
			return err
		}
		config.Codex.TurnSandboxPolicy = normalized.(map[string]any)
	}
	for key, setting := range map[string]struct {
		destination *time.Duration
		minimum     int
	}{
		"turn_timeout_ms":  {&config.Codex.TurnTimeout, 1},
		"read_timeout_ms":  {&config.Codex.ReadTimeout, 1},
		"stall_timeout_ms": {&config.Codex.StallTimeout, math.MinInt},
	} {
		if value, ok := section[key]; ok {
			duration, err := milliseconds(locator, "codex."+key, value, setting.minimum)
			if err != nil {
				return err
			}
			*setting.destination = duration
		}
	}
	return nil
}

func resolveServer(locator fieldLocator, raw map[string]any, config *EffectiveConfig) error {
	section, err := object(locator, raw, "server")
	if err != nil || section == nil {
		return err
	}
	if value, ok := section["port"]; ok {
		port, ok := integer(value)
		if !ok || port < 0 || port > 65535 {
			return fieldError(locator, "server.port", "must be an integer from 0 through 65535")
		}
		config.Server.Port = port
	}
	if value, ok := section["operator_response_timeout_ms"]; ok {
		duration, err := milliseconds(locator, "server.operator_response_timeout_ms", value, 1)
		if err != nil {
			return err
		}
		if duration < minimumOperatorResponseWindow {
			return fieldError(locator, "server.operator_response_timeout_ms", "must be at least 30000ms")
		}
		config.Server.OperatorResponseWindow = duration
	}
	return nil
}

func object(locator fieldLocator, raw map[string]any, key string) (map[string]any, error) {
	value, ok := raw[key]
	if !ok || value == nil {
		return nil, nil
	}
	section, ok := value.(map[string]any)
	if !ok {
		return nil, fieldError(locator, key, "must be an object")
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

func milliseconds(locator fieldLocator, field string, value any, minimum int) (time.Duration, error) {
	amount, ok := integer(value)
	if !ok || amount < minimum {
		return 0, fieldError(locator, field, "must be a valid millisecond integer")
	}
	if int64(amount) > math.MaxInt64/int64(time.Millisecond) || int64(amount) < math.MinInt64/int64(time.Millisecond) {
		return 0, fieldError(locator, field, "is too large")
	}
	return time.Duration(amount) * time.Millisecond, nil
}

func resolveWorkspaceRoot(locator fieldLocator, root string, lookup LookupEnv) (string, error) {
	if variable, ok := exactEnvironmentReference(root); ok {
		if lookup == nil {
			lookup = os.LookupEnv
		}
		value, found := lookup(variable)
		if !found || value == "" {
			return "", fieldError(locator, "workspace.root", "references an unset environment variable")
		}
		root = value
	}
	if root == "~" || strings.HasPrefix(root, "~/") || strings.HasPrefix(root, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fieldError(locator, "workspace.root", err.Error())
		}
		if root == "~" {
			root = home
		} else {
			root = filepath.Join(home, root[2:])
		}
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(filepath.Dir(locator.path), root)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fieldError(locator, "workspace.root", err.Error())
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

func jsonSafe(locator fieldLocator, field string, value any) (any, error) {
	if _, err := json.Marshal(value); err != nil {
		return nil, fieldError(locator, field, "must contain JSON-safe values")
	}
	return value, nil
}

func fieldError(locator fieldLocator, field, detail string) error {
	return locator.error(field, detail)
}
