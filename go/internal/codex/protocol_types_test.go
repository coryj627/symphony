package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestProtocolTypesSerializePinnedStartupAndTurnShapes(t *testing.T) {
	workspace := filepath.Clean(t.TempDir())
	policy, err := ParseApprovalPolicy(`"on-request"`)
	if err != nil {
		t.Fatal(err)
	}
	thread := ThreadStartParams{
		ApprovalPolicy:        policy,
		Cwd:                   workspace,
		DynamicTools:          []DynamicToolSpec{{Type: "function", Name: "github_api", Description: "Issue-scoped GitHub API", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		RuntimeWorkspaceRoots: []string{workspace},
		Sandbox:               "workspace-write",
	}
	encoded, err := json.Marshal(thread)
	if err != nil {
		t.Fatal(err)
	}
	var threadFields map[string]any
	if err := json.Unmarshal(encoded, &threadFields); err != nil {
		t.Fatal(err)
	}
	if threadFields["approvalPolicy"] != "on-request" || threadFields["sandbox"] != "workspace-write" || threadFields["cwd"] != workspace {
		t.Fatalf("%s", encoded)
	}

	turn := TurnStartParams{
		ThreadID:              "thread-1",
		Input:                 []UserInput{{Type: "text", Text: "Do the work"}},
		Cwd:                   workspace,
		RuntimeWorkspaceRoots: []string{workspace},
		SandboxPolicy: WorkspaceWriteSandboxPolicy{
			Type: "workspaceWrite", WritableRoots: []string{workspace}, NetworkAccess: false,
		},
	}
	encoded, err = json.Marshal(turn)
	if err != nil {
		t.Fatal(err)
	}
	var turnFields map[string]any
	if err := json.Unmarshal(encoded, &turnFields); err != nil {
		t.Fatal(err)
	}
	input := turnFields["input"].([]any)[0].(map[string]any)
	sandbox := turnFields["sandboxPolicy"].(map[string]any)
	if input["type"] != "text" || input["text"] != "Do the work" || sandbox["type"] != "workspaceWrite" || sandbox["networkAccess"] != false {
		t.Fatalf("%s", encoded)
	}
}

func TestProtocolTypesRejectUnsupportedPolicySandboxAndToolShapes(t *testing.T) {
	for _, source := range []string{`"always"`, `null`, `{"granular":{"rules":true}}`, `"on-request" trailing`} {
		if _, err := ParseApprovalPolicy(source); err == nil {
			t.Fatalf("accepted approval policy %s", source)
		}
	}
	options := SessionOptions{
		Workspace:     t.TempDir(),
		ThreadSandbox: "danger-full-access",
		DynamicTools: []DynamicToolSpec{{
			Type: "function", Name: "bad name", Description: "bad", InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	}
	if err := validateSessionOptions(options); err == nil {
		t.Fatal("accepted unsafe session options")
	}
}

func TestProtocolTypesFixturesConformToPinnedSchemas(t *testing.T) {
	type fixture struct {
		name    string
		schemas []string
		fields  []string
	}
	fixtures := []fixture{
		{
			name: "happy.jsonl",
			schemas: []string{
				"v1/InitializeParams.json",
				"v1/InitializeResponse.json",
				"ClientNotification.json",
				"v2/ThreadStartParams.json",
				"v2/ThreadStartResponse.json",
				"v2/TurnStartParams.json",
				"v2/TurnStartResponse.json",
				"v2/TurnCompletedNotification.json",
			},
			fields: []string{"params", "result", "", "params", "result", "params", "result", "params"},
		},
		{
			name:    "failed-turn.jsonl",
			schemas: []string{"v2/TurnCompletedNotification.json"},
			fields:  []string{"params"},
		},
		{
			name: "usage-rate-limit.jsonl",
			schemas: []string{
				"v2/ThreadTokenUsageUpdatedNotification.json",
				"v2/ThreadTokenUsageUpdatedNotification.json",
				"v2/AccountRateLimitsUpdatedNotification.json",
			},
			fields: []string{"params", "params", "params"},
		},
	}

	compiled := make(map[string]*jsonschema.Schema)
	for _, transcript := range fixtures {
		t.Run(transcript.name, func(t *testing.T) {
			path := filepath.Join("..", "..", "testdata", "codex", "session", transcript.name)
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()

			scanner := bufio.NewScanner(file)
			scanner.Buffer(make([]byte, 64*1024), defaultMaxLineBytes)
			line := 0
			for scanner.Scan() {
				if line >= len(transcript.schemas) {
					t.Fatalf("unexpected fixture line %d", line+1)
				}
				fields, err := decodeJSONObject(scanner.Bytes())
				if err != nil {
					t.Fatalf("line %d envelope: %v", line+1, err)
				}
				field := transcript.fields[line]
				source := scanner.Bytes()
				if field != "" {
					source = fields[field]
				}
				instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(source))
				if err != nil {
					t.Fatalf("line %d %s: %v", line+1, field, err)
				}
				schemaPath := filepath.Join("..", "..", "schema", "codex", "0.144.1", transcript.schemas[line])
				schema := compiled[schemaPath]
				if schema == nil {
					schema = compilePinnedSchema(t, schemaPath)
					compiled[schemaPath] = schema
				}
				if err := schema.Validate(instance); err != nil {
					t.Fatalf("line %d does not match %s: %v", line+1, transcript.schemas[line], err)
				}
				line++
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if line != len(transcript.schemas) {
				t.Fatalf("got %d lines want %d", line, len(transcript.schemas))
			}
		})
	}
}

func compilePinnedSchema(t *testing.T, path string) *jsonschema.Schema {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	document, err := jsonschema.UnmarshalJSON(file)
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	const resource = "https://symphony.local/schema.json"
	if err := compiler.AddResource(resource, document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(resource)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}
