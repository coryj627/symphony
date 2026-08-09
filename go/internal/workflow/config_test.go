package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestResolveAppliesCoreDefaultsAndTargetedExpansion(t *testing.T) {
	// Break caught: expanding every dollar expression corrupts the user-supplied
	// Codex shell command, while failing to expand workspace.root escapes its
	// documented environment-indirection contract.
	root := t.TempDir()
	workflowPath := filepath.Join(root, "repo", "WORKFLOW.md")
	workRoot := filepath.Join(root, "safe", "work")
	def, err := Parse(workflowPath, []byte("---\ntracker:\n  kind: github\n  provider:\n    repo: coryj627/symphony\nworkspace:\n  root: $WORK_ROOT\ncodex:\n  command: 'codex app-server --config $UNCHANGED'\n---\nPrompt"))
	if err != nil {
		t.Fatal(err)
	}
	env := func(key string) (string, bool) {
		if key == "WORK_ROOT" {
			return workRoot, true
		}
		return "", false
	}
	got, err := Resolve(workflowPath, def, env)
	if err != nil {
		t.Fatal(err)
	}
	if got.Polling.Interval != 30*time.Second || got.Agent.MaxConcurrent != 10 || got.Agent.MaxTurns != 20 {
		t.Fatalf("defaults not applied: %#v", got)
	}
	if got.Workspace.Root != filepath.Clean(workRoot) || got.Codex.Command != "codex app-server --config $UNCHANGED" {
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
	workflowPath := filepath.Join(t.TempDir(), "repo", "config", "WORKFLOW.md")
	cases := []struct {
		name     string
		yamlRoot string
		want     string
	}{
		{name: "relative", yamlRoot: "workspaces", want: filepath.Join(filepath.Dir(workflowPath), "workspaces")},
		{name: "home", yamlRoot: "~/workspaces", want: filepath.Join(home, "workspaces")},
		{name: "bare home", yamlRoot: "'~'", want: home},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			def, err := Parse(workflowPath, []byte("---\nworkspace:\n  root: "+test.yamlRoot+"\n---\nPrompt"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := Resolve(workflowPath, def, nil)
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

func TestResolvePreservesLargeJSONSafeIntegers(t *testing.T) {
	// Break caught: JSON validation that round-trips through interface{} turns
	// integers larger than 2^53 into lossy float64 values.
	const want = 9007199254740993
	def, err := Parse("WORKFLOW.md", []byte("---\ntracker:\n  provider:\n    large_id: "+"9007199254740993"+"\ncodex:\n  turn_sandbox_policy:\n    maximum_bytes: "+"9007199254740993"+"\n---\nPrompt"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Resolve("WORKFLOW.md", def, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tracker.Provider["large_id"] != want || got.Codex.TurnSandboxPolicy["maximum_bytes"] != want {
		t.Fatalf("large values changed: provider=%#v policy=%#v", got.Tracker.Provider, got.Codex.TurnSandboxPolicy)
	}
}

func TestResolveAllowsNonPositiveStallTimeout(t *testing.T) {
	// Break caught: rejecting a non-positive stall timeout makes the documented
	// explicit stall-detection disable setting unusable.
	for _, test := range []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "negative disables", value: "-1", want: -time.Millisecond},
		{name: "zero disables", value: "0", want: 0},
		{name: "positive enables", value: "1500", want: 1500 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			def, err := Parse("WORKFLOW.md", []byte("---\ncodex:\n  stall_timeout_ms: "+test.value+"\n---\nPrompt"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := Resolve("WORKFLOW.md", def, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.Codex.StallTimeout != test.want {
				t.Fatalf("stall timeout = %v, want %v", got.Codex.StallTimeout, test.want)
			}
		})
	}
}

func TestResolveReportsInvalidFieldLocation(t *testing.T) {
	// Break caught: reporting every typed validation error at 1:1 sends the
	// operator to the opening delimiter instead of the invalid setting.
	def, err := Parse("WORKFLOW.md", []byte("---\npolling:\n  interval_ms: 0\n---\nPrompt"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Resolve("WORKFLOW.md", def, nil)
	if !errors.Is(err, ErrWorkflowParse) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "WORKFLOW.md:3:") {
		t.Fatalf("error lacks invalid-field location: %v", err)
	}
}
