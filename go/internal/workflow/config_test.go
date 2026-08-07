package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestResolveAppliesCoreDefaultsAndTargetedExpansion(t *testing.T) {
	// Break caught: expanding every dollar expression corrupts the user-supplied
	// Codex shell command, while failing to expand workspace.root escapes its
	// documented environment-indirection contract.
	def, err := Parse("/repo/WORKFLOW.md", []byte("---\ntracker:\n  kind: github\n  provider:\n    repo: coryj627/symphony\nworkspace:\n  root: $WORK_ROOT\ncodex:\n  command: 'codex app-server --config $UNCHANGED'\n---\nPrompt"))
	if err != nil {
		t.Fatal(err)
	}
	env := func(key string) (string, bool) {
		if key == "WORK_ROOT" {
			return "/safe/work", true
		}
		return "", false
	}
	got, err := Resolve("/repo/WORKFLOW.md", def, env)
	if err != nil {
		t.Fatal(err)
	}
	if got.Polling.Interval != 30*time.Second || got.Agent.MaxConcurrent != 10 || got.Agent.MaxTurns != 20 {
		t.Fatalf("defaults not applied: %#v", got)
	}
	if got.Workspace.Root != filepath.Clean("/safe/work") || got.Codex.Command != "codex app-server --config $UNCHANGED" {
		t.Fatalf("bad expansion: %#v", got)
	}
}

func TestResolveMakesRelativeAndHomeWorkspaceRootsAbsolute(t *testing.T) {
	// Break caught: resolving relative roots from the process cwd makes one
	// checked-in workflow behave differently depending on how Symphony starts.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		root string
		want string
	}{
		{name: "relative", root: "workspaces", want: "/repo/config/workspaces"},
		{name: "home", root: "~/workspaces", want: filepath.Join(home, "workspaces")},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			def, err := Parse("/repo/config/WORKFLOW.md", []byte("---\nworkspace:\n  root: "+test.root+"\n---\nPrompt"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := Resolve("/repo/config/WORKFLOW.md", def, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.Workspace.Root != filepath.Clean(test.want) {
				t.Fatalf("workspace root = %q, want %q", got.Workspace.Root, filepath.Clean(test.want))
			}
		})
	}
}

func TestResolveRejectsInvalidNumericSettings(t *testing.T) {
	// Break caught: accepting invalid bounds lets the scheduler spin, disable
	// required time limits, or attempt an invalid listener bind.
	for name, source := range map[string]string{
		"zero poll interval":        "polling:\n  interval_ms: 0",
		"zero max turns":            "agent:\n  max_turns: 0",
		"negative hook timeout":     "hooks:\n  timeout_ms: -1",
		"negative server port":      "server:\n  port: -1",
		"too large server port":     "server:\n  port: 65536",
		"short operator response":   "server:\n  operator_response_timeout_ms: 29999",
		"non-numeric retry backoff": "agent:\n  max_retry_backoff_ms: fast",
	} {
		t.Run(name, func(t *testing.T) {
			def, err := Parse("WORKFLOW.md", []byte("---\n"+source+"\n---\nPrompt"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = Resolve("WORKFLOW.md", def, nil)
			if !errors.Is(err, ErrWorkflowParse) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestResolveNormalizesStateLimitsAndPreservesUnknownData(t *testing.T) {
	// Break caught: unnormalized state names bypass their intended concurrency
	// cap, and rejecting extension keys prevents forward-compatible workflows.
	def, err := Parse("WORKFLOW.md", []byte("---\nagent:\n  max_concurrent_agents_by_state:\n    ' Ready ': 2\n    blocked: 0\n    malformed: no\nextension:\n  retained: true\n---\nPrompt"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Resolve("WORKFLOW.md", def, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Agent.MaxConcurrentByState) != 1 || got.Agent.MaxConcurrentByState["ready"] != 2 {
		t.Fatalf("state limits = %#v", got.Agent.MaxConcurrentByState)
	}
	if !mappingHasKey(def.FrontMatter.Content[0], "extension") {
		t.Fatal("unknown extension was not retained in the workflow AST")
	}
}

func mappingHasKey(node *yaml.Node, key string) bool {
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return true
		}
	}
	return false
}

func TestResolvePreservesCodexPolicyValues(t *testing.T) {
	// Break caught: decoding policy fields into a local enum/map shape loses
	// schema-owned values before the pinned Codex protocol can validate them.
	def, err := Parse("WORKFLOW.md", []byte("---\ncodex:\n  approval_policy: on-request\n  thread_sandbox: workspace-write\n  turn_sandbox_policy:\n    network_access: false\n    writable_roots:\n      - /safe/work\n---\nPrompt"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Resolve("WORKFLOW.md", def, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Codex.ApprovalPolicy != "on-request" || got.Codex.ThreadSandbox != "workspace-write" {
		t.Fatalf("policy scalars = %#v", got.Codex)
	}
	if got.Codex.TurnSandboxPolicy["network_access"] != false {
		t.Fatalf("policy map = %#v", got.Codex.TurnSandboxPolicy)
	}
}
