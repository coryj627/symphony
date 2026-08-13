package tracker

import (
	"context"
	"strconv"
	"sync/atomic"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/secrets"
	"github.com/coryj627/symphony/go/internal/workflow"
)

type Adapter interface {
	Kind() string
	FetchIssuesByStates(context.Context, []string) ([]domain.Issue, error)
	FetchIssuesByIDs(context.Context, []string) ([]domain.Issue, error)
	AgentTools(Session) []domain.ToolSpec
	ExecuteAgentTool(context.Context, domain.ToolCall, Session) domain.ToolResult
	SecretEnvironmentNames() []string
}

// Factory builds an adapter from workflow tracker configuration. A future
// implementation may capture workflow identity for vault resolution, call
// DecodeConfig for the provider profile, and preserve the host-side
// secrets.Resolver boundary without importing concrete provider packages here.
type Factory interface {
	Build(context.Context, workflow.TrackerConfig, secrets.Resolver) (Adapter, error)
}

// Session is the provider/tool context captured for one agent session. Use
// NewSession (and Clone at handoff boundaries) so nested values cannot alias a
// workflow reload or provider refresh.
type Session struct {
	Issue          domain.Issue
	ProviderConfig ProviderConfig
	toolScopeID    string
}

var toolScopeSequence atomic.Uint64

func NewSession(issue domain.Issue, providerConfig ProviderConfig) (Session, error) {
	issueSnapshot, err := NormalizeIssue(issue)
	if err != nil {
		return Session{}, err
	}
	return Session{
		Issue:          issueSnapshot,
		ProviderConfig: cloneProviderConfig(providerConfig),
		toolScopeID:    "tracker-session-" + strconv.FormatUint(toolScopeSequence.Add(1), 36),
	}, nil
}

func (session Session) Clone() (Session, error) {
	issueSnapshot, err := NormalizeIssue(session.Issue)
	if err != nil {
		return Session{}, err
	}
	return Session{
		Issue: issueSnapshot, ProviderConfig: cloneProviderConfig(session.ProviderConfig), toolScopeID: session.toolScopeID,
	}, nil
}

// ToolScopeID is an opaque process-local identity used only for bounded tool
// state such as mutation idempotency. It is never serialized or sent to a
// provider or child process.
func (session Session) ToolScopeID() string { return session.toolScopeID }

func cloneProviderConfig(providerConfig ProviderConfig) ProviderConfig {
	switch config := providerConfig.(type) {
	case GitHubConfig:
		config.ActiveStates = append([]string(nil), config.ActiveStates...)
		config.TerminalStates = append([]string(nil), config.TerminalStates...)
		return config
	case *GitHubConfig:
		if config == nil {
			return nil
		}
		clone := *config
		clone.ActiveStates = append([]string(nil), config.ActiveStates...)
		clone.TerminalStates = append([]string(nil), config.TerminalStates...)
		return &clone
	case LinearConfig:
		config.ActiveStates = append([]string(nil), config.ActiveStates...)
		config.TerminalStates = append([]string(nil), config.TerminalStates...)
		return config
	case *LinearConfig:
		if config == nil {
			return nil
		}
		clone := *config
		clone.ActiveStates = append([]string(nil), config.ActiveStates...)
		clone.TerminalStates = append([]string(nil), config.TerminalStates...)
		return &clone
	default:
		return providerConfig
	}
}
