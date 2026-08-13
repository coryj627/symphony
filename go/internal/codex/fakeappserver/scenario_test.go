package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestHappyScenarioCompletesHandshakeToolCallAndTurn(t *testing.T) {
	input := strings.Join([]string{
		`{"id":1,"method":"initialize","params":{}}`,
		`{"method":"initialized"}`,
		`{"id":2,"method":"thread/start","params":{"cwd":"/workspace","dynamicTools":[{"name":"github_api"}]}}`,
		`{"id":3,"method":"turn/start","params":{"threadId":"thread-1"}}`,
		`{"id":"tool-1","result":{"success":true,"contentItems":[{"type":"inputText","text":"{\"success\":true}"}]}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := runScenario("happy", strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	messages := decodeScenarioOutput(t, output.String())
	if len(messages) != 5 || messages[0]["id"] != float64(1) || messages[1]["id"] != float64(2) || messages[2]["id"] != float64(3) {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[3]["method"] != "item/tool/call" || messages[4]["method"] != "turn/completed" {
		t.Fatalf("messages = %#v", messages)
	}
}

func TestScenarioFailuresAreDeterministic(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "incompatible", input: `{"id":1,"method":"initialize","params":{}}` + "\n", want: "0.145.0"},
		{name: "malformed", input: `{"id":1,"method":"initialize","params":{}}` + "\n", want: `{not-json`},
		{name: "turn-failed", input: strings.Join([]string{
			`{"id":1,"method":"initialize","params":{}}`,
			`{"method":"initialized"}`,
			`{"id":2,"method":"thread/start","params":{"cwd":"/workspace"}}`,
			`{"id":3,"method":"turn/start","params":{"threadId":"thread-1"}}`,
		}, "\n") + "\n", want: `"status":"failed"`},
		{name: "turn-interrupted", input: strings.Join([]string{
			`{"id":1,"method":"initialize","params":{}}`,
			`{"method":"initialized"}`,
			`{"id":2,"method":"thread/start","params":{"cwd":"/workspace"}}`,
			`{"id":3,"method":"turn/start","params":{"threadId":"thread-1"}}`,
		}, "\n") + "\n", want: `"status":"interrupted"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := runScenario(test.name, strings.NewReader(test.input), &output); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), test.want) {
				t.Fatalf("output = %q, want %q", output.String(), test.want)
			}
		})
	}
}

func TestOversizeAndChildExitScenariosFailAtTheirExactBoundaries(t *testing.T) {
	var oversize bytes.Buffer
	if err := runScenario("oversize", strings.NewReader(`{"id":1,"method":"initialize","params":{}}`+"\n"), &oversize); err != nil {
		t.Fatal(err)
	}
	if oversize.Len() != (10<<20)+2 {
		t.Fatalf("oversize line bytes = %d", oversize.Len())
	}
	input := strings.Join([]string{
		`{"id":1,"method":"initialize","params":{}}`,
		`{"method":"initialized"}`,
		`{"id":2,"method":"thread/start","params":{"cwd":"/workspace"}}`,
		`{"id":3,"method":"turn/start","params":{"threadId":"thread-1"}}`,
	}, "\n") + "\n"
	if err := runScenario("child-exit", strings.NewReader(input), io.Discard); err == nil || err.Error() != "deterministic child exit" {
		t.Fatalf("child exit error = %v", err)
	}
}

func TestFullScenarioRequiresOperatorResponsesToolResultAndTwoTurns(t *testing.T) {
	input := strings.Join([]string{
		`{"id":1,"method":"initialize","params":{}}`,
		`{"method":"initialized"}`,
		`{"id":2,"method":"thread/start","params":{"cwd":"/workspace","dynamicTools":[{"name":"github_api"}]}}`,
		`{"id":3,"method":"turn/start","params":{"threadId":"thread-1"}}`,
		`{"id":"approval-1","result":{"decision":"accept"}}`,
		`{"id":"input-1","result":{"answers":{"platform":{"answers":["Windows"]},"detail":{"answers":["integration detail"]},"token":{"answers":["temporary-answer"]}}}}`,
		`{"id":"tool-1","result":{"success":true,"contentItems":[{"type":"inputText","text":"{\"success\":true,\"data\":{\"fake_tool\":\"executed\"}}"}]}}`,
		`{"id":4,"method":"turn/start","params":{"threadId":"thread-1"}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := runScenario("full", strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	messages := decodeScenarioOutput(t, output.String())
	if len(messages) != 9 {
		t.Fatalf("message count = %d: %#v", len(messages), messages)
	}
	methods := []any{messages[3]["method"], messages[4]["method"], messages[5]["method"], messages[6]["method"], messages[8]["method"]}
	want := []any{"item/commandExecution/requestApproval", "item/tool/requestUserInput", "item/tool/call", "turn/completed", "turn/completed"}
	for index := range want {
		if methods[index] != want[index] {
			t.Fatalf("methods = %#v", methods)
		}
	}
}

func decodeScenarioOutput(t *testing.T, raw string) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	messages := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var message map[string]any
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		messages = append(messages, message)
	}
	return messages
}
