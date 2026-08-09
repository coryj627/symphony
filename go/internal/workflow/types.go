package workflow

import (
	"time"

	"go.yaml.in/yaml/v3"
)

// Definition is the lossless front matter AST together with the prompt that is
// ready for template rendering.
type Definition struct {
	FrontMatter *yaml.Node
	Prompt      string
}

// Snapshot is one complete, resolved read of a workflow file.
type Snapshot struct {
	Path       string
	Source     string
	Digest     string
	Definition Definition
	Config     EffectiveConfig
	LoadedAt   time.Time
}

type EffectiveConfig struct {
	Tracker   TrackerConfig
	Polling   PollingConfig
	Workspace WorkspaceConfig
	Hooks     HooksConfig
	Agent     AgentConfig
	Codex     CodexConfig
	Server    ServerConfig
}

type TrackerConfig struct {
	Kind           string
	Provider       map[string]any
	RequiredLabels []string
	ActiveStates   []string
	TerminalStates []string
}

type PollingConfig struct{ Interval time.Duration }

type WorkspaceConfig struct{ Root string }

type HooksConfig struct {
	AfterCreate  string
	BeforeRun    string
	AfterRun     string
	BeforeRemove string
	Timeout      time.Duration
}

type AgentConfig struct {
	MaxConcurrent        int
	MaxTurns             int
	MaxRetryBackoff      time.Duration
	MaxConcurrentByState map[string]int
}

type CodexConfig struct {
	Command           string
	ApprovalPolicy    any
	ThreadSandbox     string
	TurnSandboxPolicy map[string]any
	TurnTimeout       time.Duration
	ReadTimeout       time.Duration
	StallTimeout      time.Duration
}

type ServerConfig struct {
	Port                   int
	OperatorResponseWindow time.Duration
}

// LookupEnv is intentionally injected so config resolution is deterministic in
// callers and tests.
type LookupEnv func(string) (string, bool)
