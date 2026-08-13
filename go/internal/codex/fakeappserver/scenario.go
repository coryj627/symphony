package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	maximumFixtureLineBytes = 8 << 20
	fixtureTraceEnvironment = "SYMPHONY_FAKE_CODEX_TRACE_PATH"
)

var fixtureScenarios = map[string]struct{}{
	"happy": {}, "full": {}, "tool-failure": {}, "turn-failed": {}, "turn-interrupted": {},
	"incompatible": {}, "malformed": {}, "oversize": {}, "stalled": {}, "child-exit": {},
	"unsupported-tool": {}, "request-timeout": {}, "shutdown-request": {}, "stderr-noise": {},
}

type fixtureEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type fixtureServer struct {
	scenario    string
	encoder     *json.Encoder
	trace       io.Writer
	workspace   string
	dynamicTool string
	pendingTool bool
	pendingKind string
	turnID      string
	turnCount   int
}

func runScenario(name string, input io.Reader, output io.Writer) (runErr error) {
	if _, ok := fixtureScenarios[name]; !ok {
		return fmt.Errorf("unknown fake app-server scenario %q", name)
	}
	trace, closeTrace, err := openFixtureTrace()
	if err != nil {
		return err
	}
	defer func() {
		event := "complete"
		if runErr != nil {
			event = "failed"
		}
		if traceErr := writeFixtureTrace(trace, name, "event", event); runErr == nil && traceErr != nil {
			runErr = traceErr
		}
		if closeErr := closeTrace(); runErr == nil && closeErr != nil {
			runErr = closeErr
		}
	}()
	if err := writeFixtureTrace(trace, name, "event", "start"); err != nil {
		return err
	}
	server := &fixtureServer{scenario: name, encoder: json.NewEncoder(output), trace: trace}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), maximumFixtureLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		var message fixtureEnvelope
		if err := json.Unmarshal(line, &message); err != nil {
			return fmt.Errorf("decode client message: %w", err)
		}
		if err := server.traceMessage(message); err != nil {
			return err
		}
		if err := server.handle(message, output); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func openFixtureTrace() (io.Writer, func() error, error) {
	path := strings.TrimSpace(os.Getenv(fixtureTraceEnvironment))
	if path == "" {
		return io.Discard, func() error { return nil }, nil
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open fake app-server trace: %w", err)
	}
	return file, file.Close, nil
}

func writeFixtureTrace(target io.Writer, scenario, key, value string) error {
	_, err := fmt.Fprintf(target, "scenario=%s %s=%s\n", scenario, key, value)
	return err
}

func (server *fixtureServer) traceMessage(message fixtureEnvelope) error {
	stage := "other"
	if message.Method != "" {
		switch message.Method {
		case "initialize", "initialized", "thread/start", "turn/start", "turn/interrupt":
			stage = message.Method
		default:
			stage = "method_other"
		}
	} else if server.pendingKind == "command" && len(message.Result) > 0 {
		var response struct {
			Decision string `json:"decision"`
		}
		if err := json.Unmarshal(message.Result, &response); err != nil {
			stage = "command_response_other"
		} else {
			switch response.Decision {
			case "accept":
				stage = "command_response_accept"
			case "cancel":
				stage = "command_response_cancel"
			default:
				stage = "command_response_other"
			}
		}
	} else if server.pendingKind == "input" && len(message.Result) > 0 {
		stage = "input_response"
	} else if server.pendingTool && len(message.ID) > 0 {
		if len(message.Error) > 0 {
			stage = "tool_response_error"
		} else {
			var response struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal(message.Result, &response); err == nil && response.Success {
				stage = "tool_response_success"
			} else {
				stage = "tool_response_failure"
			}
		}
	}
	return writeFixtureTrace(server.trace, server.scenario, "receive", stage)
}

func (server *fixtureServer) handle(message fixtureEnvelope, rawOutput io.Writer) error {
	if server.pendingKind != "" && len(message.ID) > 0 && len(message.Result) > 0 {
		return server.handleOperatorResponse(message.Result)
	}
	if server.pendingTool && len(message.ID) > 0 && (len(message.Result) > 0 || len(message.Error) > 0) {
		server.pendingTool = false
		if len(message.Error) > 0 {
			if server.scenario != "unsupported-tool" || !strings.Contains(string(message.Error), `"code":-32602`) {
				return errors.New("fake app-server received an unexpected tool rejection")
			}
			return server.completeTurn("completed", nil)
		}
		var response struct {
			Success      bool `json:"success"`
			ContentItems []struct {
				Text string `json:"text"`
			} `json:"contentItems"`
		}
		if err := json.Unmarshal(message.Result, &response); err != nil || len(response.ContentItems) != 1 {
			return errors.New("fake app-server received a malformed tool response")
		}
		if server.scenario == "tool-failure" {
			if response.Success || !strings.Contains(response.ContentItems[0].Text, `"code":"fixture_tool_failure"`) {
				return errors.New("fake app-server expected the deterministic tool failure")
			}
		} else if server.scenario == "full" {
			if !response.Success || !strings.Contains(response.ContentItems[0].Text, `"fake_tool":"executed"`) {
				return errors.New("fake app-server expected the deterministic provider tool result")
			}
		} else if server.scenario == "unsupported-tool" {
			if response.Success || !strings.Contains(response.ContentItems[0].Text, `"code":"tool_unavailable"`) {
				return errors.New("fake app-server expected an unsupported-tool failure")
			}
		} else if !response.Success {
			return errors.New("fake app-server expected a successful tool response")
		}
		return server.completeTurn("completed", nil)
	}
	switch message.Method {
	case "initialize":
		if server.scenario == "malformed" {
			_, err := io.WriteString(rawOutput, "{not-json\n")
			return err
		}
		if server.scenario == "oversize" {
			_, err := io.WriteString(rawOutput, strings.Repeat("x", (10<<20)+1)+"\n")
			return err
		}
		version := "0.144.1"
		if server.scenario == "incompatible" {
			version = "0.145.0"
		}
		codexHome, err := os.Getwd()
		if err != nil {
			return err
		}
		return server.respond(message.ID, map[string]any{
			"userAgent": "codex_cli_rs/" + version, "codexHome": codexHome, "platformFamily": "unix", "platformOs": "macos",
		})
	case "initialized":
		return nil
	case "thread/start":
		var params struct {
			Cwd          string `json:"cwd"`
			DynamicTools []struct {
				Name string `json:"name"`
			} `json:"dynamicTools"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil {
			return err
		}
		server.workspace = params.Cwd
		if len(params.DynamicTools) > 0 {
			server.dynamicTool = params.DynamicTools[0].Name
		}
		if server.scenario == "unsupported-tool" {
			server.dynamicTool = "unsupported_fixture"
		}
		return server.respond(message.ID, server.threadStarted())
	case "turn/start":
		server.turnCount++
		server.turnID = fmt.Sprintf("turn-%d", server.turnCount)
		if err := server.respond(message.ID, map[string]any{
			"turn": map[string]any{"id": server.turnID, "status": "inProgress", "items": []any{}},
		}); err != nil {
			return err
		}
		switch server.scenario {
		case "turn-failed":
			return server.completeTurn("failed", map[string]any{"message": "Deterministic fixture turn failure."})
		case "turn-interrupted":
			return server.completeTurn("interrupted", nil)
		case "child-exit":
			return errors.New("deterministic child exit")
		case "stalled":
			return nil
		case "request-timeout", "shutdown-request":
			return server.sendCommandApproval()
		case "full":
			if server.turnCount == 1 {
				return server.sendCommandApproval()
			}
			return server.completeTurn("completed", nil)
		}
		if server.dynamicTool == "" {
			return server.completeTurn("completed", nil)
		}
		server.pendingTool = true
		return server.encoder.Encode(map[string]any{
			"id": "tool-1", "method": "item/tool/call",
			"params": map[string]any{
				"arguments": map[string]any{"operation": "get_issue"}, "callId": "fixture-call-1",
				"threadId": "thread-1", "tool": server.dynamicTool, "turnId": server.turnID,
			},
		})
	case "turn/interrupt":
		return server.respond(message.ID, map[string]any{})
	default:
		if len(message.ID) > 0 {
			return server.encoder.Encode(map[string]any{
				"id": json.RawMessage(message.ID), "error": map[string]any{"code": -32601, "message": "Method not found"},
			})
		}
	}
	return nil
}

func (server *fixtureServer) sendCommandApproval() error {
	server.pendingKind = "command"
	return server.encoder.Encode(map[string]any{
		"id": "approval-1", "method": "item/commandExecution/requestApproval",
		"params": map[string]any{
			"itemId": "command-1", "threadId": "thread-1", "turnId": server.turnID, "startedAtMs": 1,
			"command": "printf phase4-secret-canary", "reason": "Verify the contained command approval path",
			"availableDecisions": []any{"accept", "decline", "cancel"},
		},
	})
}

func (server *fixtureServer) sendUserInput() error {
	server.pendingKind = "input"
	return server.encoder.Encode(map[string]any{
		"id": "input-1", "method": "item/tool/requestUserInput",
		"params": map[string]any{
			"itemId": "input-item-1", "threadId": "thread-1", "turnId": server.turnID,
			"questions": []any{
				map[string]any{"id": "platform", "header": "Platform", "question": "Choose a platform", "options": []any{
					map[string]any{"label": "Windows", "description": "Use Windows"}, map[string]any{"label": "macOS", "description": "Use macOS"},
				}},
				map[string]any{"id": "detail", "header": "Detail", "question": "Add integration detail", "isOther": true},
				map[string]any{"id": "token", "header": "Token", "question": "Enter a temporary secret", "isSecret": true},
			},
		},
	})
}

func (server *fixtureServer) sendToolCall() error {
	if server.dynamicTool == "" {
		return errors.New("full fake app-server scenario requires one dynamic tool")
	}
	server.pendingTool = true
	return server.encoder.Encode(map[string]any{
		"id": "tool-1", "method": "item/tool/call",
		"params": map[string]any{
			"arguments": map[string]any{"operation": "get_issue"}, "callId": "fixture-call-1",
			"threadId": "thread-1", "tool": server.dynamicTool, "turnId": server.turnID,
		},
	})
}

func (server *fixtureServer) handleOperatorResponse(raw json.RawMessage) error {
	kind := server.pendingKind
	server.pendingKind = ""
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return errors.New("fake app-server received a malformed operator response")
	}
	switch kind {
	case "command":
		if result["decision"] != "accept" {
			return errors.New("fake app-server expected the command approval")
		}
		return server.sendUserInput()
	case "input":
		encoded, err := json.Marshal(result)
		if err != nil || !bytesContainsAll(encoded, `"platform"`, `"Windows"`, `"detail"`, `"integration detail"`, `"token"`, `"temporary-answer"`) {
			return errors.New("fake app-server expected all deterministic user answers")
		}
		return server.sendToolCall()
	default:
		return errors.New("fake app-server operator response had no pending request")
	}
}

func bytesContainsAll(value []byte, needles ...string) bool {
	text := string(value)
	for _, needle := range needles {
		if !strings.Contains(text, needle) {
			return false
		}
	}
	return true
}

func (server *fixtureServer) respond(id json.RawMessage, result any) error {
	if len(id) == 0 {
		return errors.New("fake app-server request ID is missing")
	}
	return server.encoder.Encode(map[string]any{"id": json.RawMessage(id), "result": result})
}

func (server *fixtureServer) threadStarted() map[string]any {
	workspace := server.workspace
	if strings.TrimSpace(workspace) == "" {
		workspace = "."
	}
	return map[string]any{
		"approvalPolicy": "on-request", "approvalsReviewer": "user", "cwd": workspace,
		"model": "fixture", "modelProvider": "fixture",
		"sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": []string{workspace}, "networkAccess": false},
		"thread": map[string]any{
			"cliVersion": "0.144.1", "createdAt": 1, "cwd": workspace, "ephemeral": true,
			"id": "thread-1", "modelProvider": "fixture", "preview": "", "sessionId": "fixture-session-1",
			"source": "appServer", "status": map[string]any{"type": "idle"}, "turns": []any{}, "updatedAt": 1,
		},
	}
}

func (server *fixtureServer) completeTurn(status string, turnError any) error {
	return server.encoder.Encode(map[string]any{
		"method": "turn/completed",
		"params": map[string]any{
			"threadId": "thread-1",
			"turn":     map[string]any{"id": server.turnID, "status": status, "items": []any{}, "error": turnError},
		},
	})
}
