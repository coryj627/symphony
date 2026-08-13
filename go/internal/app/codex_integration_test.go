package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/codex"
	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/orchestrator"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

func TestProductionCodexCompositionRunsContainedProcessToolAndTerminalRefresh(t *testing.T) {
	command := buildFakeCodexCommand(t, "happy")
	root := privateTempDir(t)
	snapshot := codexIntegrationSnapshot(root, command)
	issue := validIssue("CODEX-1")
	adapter := &codexIntegrationAdapter{refreshed: terminalIssue(issue)}
	build, err := ProductionAgentBuilder(observability.NewRedactor(nil, nil), slog.Default())(
		t.Context(), snapshot, adapter, codex.NewRequestBroker(codex.RequestBrokerOptions{}),
	)
	if err != nil {
		t.Fatalf("build: %v (direct preflight: %v)", err, diagnoseCodexPreflight(t, snapshot))
	}
	result := build.Worker.Run(t.Context(), orchestrator.RunRequest{Issue: issue, Workflow: snapshot}, nil)
	if result.Reason != domain.StopReasonTerminal || result.ErrorCode != "issue_terminal" {
		t.Fatalf("result = %+v", result)
	}
	if adapter.toolCallCount() != 1 {
		t.Fatalf("tool calls = %d", adapter.toolCallCount())
	}
}

type codexIntegrationAdapter struct {
	mu        sync.Mutex
	refreshed domain.Issue
	toolCalls int
	toolFail  bool
}

func (*codexIntegrationAdapter) Kind() string { return "github" }
func (*codexIntegrationAdapter) FetchIssuesByStates(context.Context, []string) ([]domain.Issue, error) {
	return []domain.Issue{}, nil
}
func (adapter *codexIntegrationAdapter) FetchIssuesByIDs(context.Context, []string) ([]domain.Issue, error) {
	clone, err := adapter.refreshed.Clone()
	if err != nil {
		return nil, err
	}
	return []domain.Issue{clone}, nil
}
func (*codexIntegrationAdapter) AgentTools(tracker.Session) []domain.ToolSpec {
	return []domain.ToolSpec{{
		Name: "github_api", Description: "Issue-scoped deterministic integration tool.",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false},
	}}
}
func (adapter *codexIntegrationAdapter) ExecuteAgentTool(_ context.Context, call domain.ToolCall, _ tracker.Session) domain.ToolResult {
	adapter.mu.Lock()
	adapter.toolCalls++
	fail := adapter.toolFail
	adapter.mu.Unlock()
	if call.Name != "github_api" {
		return domain.ToolUnavailableResult()
	}
	if fail {
		return domain.ToolFailure("fixture_tool_failure", "The deterministic fixture tool failed.")
	}
	result, err := domain.ToolSuccess(map[string]any{"identifier": "CODEX-1"})
	if err != nil {
		panic(err)
	}
	return result
}
func (*codexIntegrationAdapter) SecretEnvironmentNames() []string {
	return []string{"SYMPHONY_TEST_TOKEN"}
}
func (adapter *codexIntegrationAdapter) toolCallCount() int {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.toolCalls
}

func buildFakeCodexCommand(t *testing.T, scenario string) string {
	t.Helper()
	return shellQuote(filepath.ToSlash(buildFakeCodexBinary(t))) + " --scenario " + shellQuote(scenario)
}

func buildFakeCodexBinary(t *testing.T) string {
	t.Helper()
	binaryName := "fake-codex-app-server"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binary := filepath.Join(t.TempDir(), binaryName)
	goTool := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOOS == "windows" {
		goTool += ".exe"
	}
	command := exec.CommandContext(t.Context(), goTool, "build", "-o", binary, "./internal/codex/fakeappserver")
	command.Dir = filepath.Clean(filepath.Join("..", ".."))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fake app-server: %v\n%s", err, output)
	}
	return binary
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }

func privateTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func codexIntegrationSnapshot(root, command string) workflow.Snapshot {
	snapshot := validQueueSnapshot("github", "", "codex-integration")
	snapshot.Definition = workflow.Definition{Prompt: "Work on {{ issue.identifier }}."}
	snapshot.Config.Workspace.Root = root
	snapshot.Config.Agent = workflow.AgentConfig{MaxConcurrent: 1, MaxTurns: 1, MaxRetryBackoff: time.Second, MaxConcurrentByState: map[string]int{}}
	snapshot.Config.Codex = workflow.CodexConfig{
		Command: command, ApprovalPolicy: "on-request", ThreadSandbox: "workspace-write",
		TurnTimeout: 10 * time.Second, ReadTimeout: 5 * time.Second, StallTimeout: 5 * time.Second,
	}
	return snapshot
}

func terminalIssue(issue domain.Issue) domain.Issue {
	issue.State = "closed"
	return issue
}

func diagnoseCodexPreflight(t *testing.T, snapshot workflow.Snapshot) error {
	t.Helper()
	workspacePath, err := os.MkdirTemp(snapshot.Config.Workspace.Root, ".diagnostic-preflight-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workspacePath)
	root, err := filepath.EvalSymlinks(snapshot.Config.Workspace.Root)
	if err != nil {
		return err
	}
	workspacePath, err = filepath.EvalSymlinks(workspacePath)
	if err != nil {
		return err
	}
	issue := domain.Issue{ID: "diagnostic", Identifier: "DIAGNOSTIC", Title: "Diagnostic", State: "open", Labels: []string{}, BlockedBy: []domain.BlockerRef{}, Dispatchable: true, NativeRef: map[string]any{"kind": "diagnostic"}}
	provider, err := tracker.DecodeConfig(snapshot.Config.Tracker)
	if err != nil {
		return err
	}
	session, err := tracker.NewSession(issue, provider)
	if err != nil {
		return err
	}
	bashPath, err := codex.FindBash()
	if err != nil {
		return err
	}
	runner := codex.ProcessRunner{Launch: codex.Launch, BashPath: bashPath}
	return runner.Preflight(t.Context(), codex.RunnerRequest{
		Issue: issue, Workspace: domain.Workspace{Path: workspacePath, Root: root, IssueID: issue.ID, IssueIdentifier: issue.Identifier},
		TrackerSession: session, Codex: snapshot.Config.Codex, MaxTurns: 1,
	})
}

func requirePrerequisiteCode(t *testing.T, err error, code string) {
	t.Helper()
	var prerequisite *AgentPrerequisiteError
	if !errors.As(err, &prerequisite) || prerequisite.Code != code {
		t.Fatalf("error = %v, want prerequisite %q", err, code)
	}
}
