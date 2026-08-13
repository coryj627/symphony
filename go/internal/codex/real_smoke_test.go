package codex

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

func TestRealCodexAppServerSmoke(t *testing.T) {
	if os.Getenv("SYMPHONY_REAL_CODEX_SMOKE") != "1" {
		t.Skip("SKIPPED: real Codex smoke")
	}
	workflowPath := os.Getenv("SYMPHONY_REAL_CODEX_WORKFLOW")
	if !filepath.IsAbs(workflowPath) {
		t.Fatal("SYMPHONY_REAL_CODEX_WORKFLOW must be an absolute isolated workflow path")
	}
	info, err := os.Stat(workflowPath)
	if err != nil || info.IsDir() {
		t.Fatalf("isolated workflow path is unavailable: %v", err)
	}
	command := os.Getenv("SYMPHONY_REAL_CODEX_COMMAND")
	if command == "" {
		command = "codex app-server"
	}
	if command == "codex app-server" {
		if _, err := exec.LookPath("codex"); err != nil {
			t.Fatalf("reviewed Codex CLI is unavailable: %v", err)
		}
	}
	root := t.TempDir()
	workspacePath, err := os.MkdirTemp(root, "real-codex-smoke-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	workspacePath, err = filepath.EvalSymlinks(workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	issue := domain.Issue{
		ID: "real-codex-smoke", Identifier: "CODEX-SMOKE", Title: "Real Codex compatibility smoke", State: "open",
		Labels: []string{}, BlockedBy: []domain.BlockerRef{}, Dispatchable: true, NativeRef: map[string]any{"kind": "smoke"},
	}
	provider, err := tracker.DecodeConfig(workflow.TrackerConfig{
		Kind: "github", Provider: map[string]any{"owner": "symphony-smoke", "repository": "symphony-smoke", "credential_ref": "os-vault"},
		ActiveStates: []string{"open"}, TerminalStates: []string{"closed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	trackerSession, err := tracker.NewSession(issue, provider)
	if err != nil {
		t.Fatal(err)
	}
	bashPath, err := FindBash()
	if err != nil {
		t.Fatal(err)
	}
	loginCommand := os.Getenv("SYMPHONY_REAL_CODEX_LOGIN_COMMAND")
	if loginCommand == "" {
		loginCommand = "codex login status"
	}
	loginSpec := BashCommand(bashPath, loginCommand)
	login := exec.CommandContext(t.Context(), loginSpec.Path, loginSpec.Args...)
	login.Dir = workspacePath
	login.Stdout = io.Discard
	login.Stderr = io.Discard
	if err := login.Run(); err != nil {
		t.Skip("SKIPPED: real Codex smoke")
	}
	runner := ProcessRunner{Launch: Launch, BashPath: bashPath}
	if err := runner.Preflight(t.Context(), RunnerRequest{
		Issue: issue,
		Workspace: domain.Workspace{
			Path: workspacePath, Root: root, IssueID: issue.ID, IssueIdentifier: issue.Identifier,
		},
		TrackerSession: trackerSession,
		Codex: workflow.CodexConfig{
			Command: command, ApprovalPolicy: "on-request", ThreadSandbox: "workspace-write",
			ReadTimeout: 10 * time.Second, StallTimeout: 30 * time.Second,
		},
		MaxTurns: 1,
	}); err != nil {
		t.Fatalf("real Codex app-server smoke failed: %v", err)
	}
}
