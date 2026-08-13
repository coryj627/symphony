package tracker

import (
	"context"
	"testing"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/secrets"
	"github.com/coryj627/symphony/go/internal/workflow"
)

type adapterContractStub struct{}

func (adapterContractStub) Kind() string { return "contract" }

func (adapterContractStub) FetchIssuesByStates(context.Context, []string) ([]domain.Issue, error) {
	return []domain.Issue{}, nil
}

func (adapterContractStub) FetchIssuesByIDs(context.Context, []string) ([]domain.Issue, error) {
	return []domain.Issue{}, nil
}

func (adapterContractStub) AgentTools(Session) []domain.ToolSpec { return []domain.ToolSpec{} }

func (adapterContractStub) ExecuteAgentTool(context.Context, domain.ToolCall, Session) domain.ToolResult {
	return domain.ToolUnavailableResult()
}

func (adapterContractStub) SecretEnvironmentNames() []string { return []string{} }

type factoryContractStub struct{}

func (factoryContractStub) Build(context.Context, workflow.TrackerConfig, secrets.Resolver) (Adapter, error) {
	return adapterContractStub{}, nil
}

var (
	_ Adapter = adapterContractStub{}
	_ Factory = factoryContractStub{}
)

func assertAdapterContract(t *testing.T, adapter Adapter) {
	t.Helper()
	if adapter.Kind() == "" {
		t.Fatal("adapter kind is blank")
	}
	if adapter.AgentTools(Session{}) == nil {
		t.Fatal("adapter tools must be a non-nil collection")
	}
	if adapter.SecretEnvironmentNames() == nil {
		t.Fatal("secret environment names must be a non-nil collection")
	}
	byStates, err := adapter.FetchIssuesByStates(t.Context(), nil)
	if err != nil || byStates == nil || len(byStates) != 0 {
		t.Fatalf("empty state fetch = %#v, %v; want non-nil empty result", byStates, err)
	}
	byIDs, err := adapter.FetchIssuesByIDs(t.Context(), nil)
	if err != nil || byIDs == nil || len(byIDs) != 0 {
		t.Fatalf("empty ID fetch = %#v, %v; want non-nil empty result", byIDs, err)
	}
}

func TestAdapterContractTestkitExercisesProviderNeutralSurface(t *testing.T) {
	// Break caught: drift in the shared adapter signature or nil collection
	// semantics would otherwise first appear in a provider implementation.
	assertAdapterContract(t, adapterContractStub{})
}

func TestSessionSnapshotDoesNotAliasIssueOrConcreteProviderConfig(t *testing.T) {
	// Break caught: a workflow reload or provider refresh can mutate the issue,
	// scope state lists, and tool context of an already-running session.
	providerID := "provider-42"
	issue := domain.Issue{
		ID: "42", Identifier: "GH-42", Title: "Title", State: "open",
		NativeRef: map[string]any{"id": providerID}, Labels: []string{"bug"},
	}
	config := GitHubConfig{Owner: "coryj627", Repository: "symphony", ActiveStates: []string{"open"}, TerminalStates: []string{"closed"}}

	session, err := NewSession(issue, config)
	if err != nil {
		t.Fatal(err)
	}
	issue.NativeRef["id"] = "changed"
	issue.Labels[0] = "changed"
	config.ActiveStates[0] = "changed"
	config.TerminalStates[0] = "changed"

	gotConfig := session.ProviderConfig.(GitHubConfig)
	if session.Issue.NativeRef["id"] != providerID || session.Issue.Labels[0] != "bug" {
		t.Fatalf("session issue aliases caller: %#v", session.Issue)
	}
	if gotConfig.ActiveStates[0] != "open" || gotConfig.TerminalStates[0] != "closed" {
		t.Fatalf("session config aliases caller: %#v", gotConfig)
	}

	clone, err := session.Clone()
	if err != nil {
		t.Fatal(err)
	}
	session.Issue.NativeRef["id"] = "session-changed"
	session.Issue.Labels[0] = "session-changed"
	sessionConfig := session.ProviderConfig.(GitHubConfig)
	sessionConfig.ActiveStates[0] = "session-changed"
	if clone.Issue.NativeRef["id"] != providerID || clone.Issue.Labels[0] != "bug" || clone.ProviderConfig.(GitHubConfig).ActiveStates[0] != "open" {
		t.Fatalf("cloned session aliases source: %#v", clone)
	}
	if session.ToolScopeID() == "" || clone.ToolScopeID() != session.ToolScopeID() {
		t.Fatalf("tool scope identity did not survive clone: %q %q", session.ToolScopeID(), clone.ToolScopeID())
	}
	later, err := NewSession(clone.Issue, clone.ProviderConfig)
	if err != nil {
		t.Fatal(err)
	}
	if later.ToolScopeID() == session.ToolScopeID() {
		t.Fatalf("later session reused tool scope identity %q", later.ToolScopeID())
	}
}

func TestSessionSnapshotClonesPointerLinearConfig(t *testing.T) {
	// Break caught: only cloning the value form leaves callers using the equally
	// valid pointer form able to mutate provider state slices in flight.
	config := &LinearConfig{ProjectSlug: "symphony", ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}}
	session, err := NewSession(domain.Issue{ID: "1", Identifier: "LIN-1", Title: "Title", State: "Todo"}, config)
	if err != nil {
		t.Fatal(err)
	}
	config.ActiveStates[0] = "changed"
	got := session.ProviderConfig.(*LinearConfig)
	if got.ActiveStates[0] != "Todo" {
		t.Fatalf("pointer config aliases caller: %#v", got)
	}
}
