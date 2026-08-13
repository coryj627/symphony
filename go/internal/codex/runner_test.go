package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/tracker"
	"github.com/coryj627/symphony/go/internal/workflow"
)

func TestRunnerLaunchesInitializesStartsOneThreadAndClosesProcess(t *testing.T) {
	process, transport := newRunnerTestProcess(t)
	var launched LaunchOptions
	runner := ProcessRunner{
		Launch: func(_ context.Context, options LaunchOptions) (Process, error) {
			launched = options
			return process, nil
		},
		Broker: NewRequestBroker(RequestBrokerOptions{}),
	}
	request := runnerRequestForTest(t)
	started := make(chan struct {
		session AgentSession
		err     error
	}, 1)
	go func() {
		session, err := runner.Start(t.Context(), request)
		started <- struct {
			session AgentSession
			err     error
		}{session: session, err: err}
	}()

	initialize := transport.readRequest(t)
	if methodOf(t, initialize) != "initialize" {
		t.Fatalf("first method = %s", initialize["method"])
	}
	respondResult(t, transport, initialize, compatibleInitializeResponse(request.Workspace.Path))
	initialized := transport.readRequest(t)
	if methodOf(t, initialized) != "initialized" {
		t.Fatalf("second method = %s", initialized["method"])
	}
	threadStart := transport.readRequest(t)
	if methodOf(t, threadStart) != "thread/start" {
		t.Fatalf("third method = %s", threadStart["method"])
	}
	respondThreadStarted(t, transport, threadStart, request.Workspace.Path, "thread-1")
	got := <-started
	if got.err != nil {
		t.Fatal(got.err)
	}
	if launched.Cwd != request.Workspace.Path || launched.WorkspaceRoot != request.Workspace.Root || launched.Command != request.Codex.Command || !slices.Equal(launched.SecretNames, []string{"GITHUB_TOKEN"}) {
		t.Fatalf("launch = %+v", launched)
	}

	turnDone := make(chan TurnResult, 1)
	go func() {
		result, err := got.session.Turn(t.Context(), "Original task")
		if err != nil {
			t.Errorf("turn: %v", err)
		}
		turnDone <- result
	}()
	turnStart := transport.readRequest(t)
	if methodOf(t, turnStart) != "turn/start" {
		t.Fatalf("turn method = %s", turnStart["method"])
	}
	respondTurnStarted(t, transport, turnStart, "turn-1")
	transport.sendJSON(t, map[string]any{
		"id": "unsupported-1", "method": "item/future/requestApproval",
		"params": map[string]any{"itemId": "item-1", "threadId": "thread-1", "turnId": "turn-1"},
	})
	rejected := transport.readRequest(t)
	var rejection RPCError
	if string(rejected["id"]) != `"unsupported-1"` || json.Unmarshal(rejected["error"], &rejection) != nil || rejection.Code != rpcMethodNotFound {
		t.Fatalf("unsupported rejection = %#v", rejected)
	}
	transport.sendJSON(t, map[string]any{
		"method": "turn/completed",
		"params": map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "completed", "items": []any{}}},
	})
	if result := <-turnDone; result.ThreadID != "thread-1" || result.TurnID != "turn-1" || result.Status != TurnCompleted {
		t.Fatalf("result = %+v", result)
	}
	if err := got.session.Close(); err != nil {
		t.Fatal(err)
	}
	if !process.wasStopped() {
		t.Fatal("process tree was not stopped before Close returned")
	}
}

func TestRunnerExecutesAdvertisedDynamicToolAndReturnsOneInputTextItem(t *testing.T) {
	process, transport := newRunnerTestProcess(t)
	runner := ProcessRunner{Launch: func(context.Context, LaunchOptions) (Process, error) { return process, nil }}
	request := runnerRequestForTest(t)
	request.DynamicTools = []DynamicToolSpec{{
		Type: "function", Name: "linear_graphql", Description: "Captured Linear GraphQL",
		InputSchema: json.RawMessage(`{"oneOf":[{"type":"string"},{"type":"object"}]}`),
	}}
	called := make(chan domain.ToolCall, 2)
	request.ExecuteTool = func(_ context.Context, call domain.ToolCall, session tracker.Session) domain.ToolResult {
		if session.Issue.ID != request.Issue.ID {
			t.Errorf("tracker session issue=%q", session.Issue.ID)
		}
		called <- call
		if _, ok := call.Arguments.(map[string]any); !ok {
			return domain.ToolFailure("invalid_arguments", "The linear_graphql arguments are invalid.")
		}
		result, err := domain.ToolSuccess(map[string]any{"viewer": map[string]any{"id": "viewer-1"}})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	session := startRunnerForTest(t, runner, request, process, transport)
	turnDone := startRunnerTurnForTest(t, session, transport, "turn-1")
	transport.sendJSON(t, map[string]any{
		"id": 17, "method": "item/tool/call",
		"params": map[string]any{
			"arguments": map[string]any{"query": "{ viewer { id } }"}, "callId": "call-1",
			"threadId": "thread-1", "tool": "linear_graphql", "turnId": "turn-1",
		},
	})
	response := transport.readRequest(t)
	if string(response["id"]) != "17" {
		t.Fatalf("response id=%s", response["id"])
	}
	var result DynamicToolCallResponse
	mustUnmarshalRaw(t, response["result"], &result)
	if !result.Success || len(result.ContentItems) != 1 || result.ContentItems[0].Type != "inputText" || !strings.Contains(result.ContentItems[0].Text, `"viewer"`) {
		t.Fatalf("result=%+v", result)
	}
	select {
	case call := <-called:
		if call.Name != "linear_graphql" {
			t.Fatalf("call=%+v", call)
		}
	case <-time.After(time.Second):
		t.Fatal("tool executor was not called")
	}
	transport.sendJSON(t, map[string]any{
		"id": "tool-failure", "method": "item/tool/call",
		"params": map[string]any{
			"arguments": 42, "callId": "call-2", "threadId": "thread-1", "tool": "linear_graphql", "turnId": "turn-1",
		},
	})
	failureResponse := transport.readRequest(t)
	mustUnmarshalRaw(t, failureResponse["result"], &result)
	if result.Success || len(result.ContentItems) != 1 || !strings.Contains(result.ContentItems[0].Text, `"code":"invalid_arguments"`) {
		t.Fatalf("failure result=%+v", result)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("invalid tool call was not returned as a normal tool failure")
	}
	completeRunnerTurnForTest(t, transport, turnDone, "turn-1")
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerRejectsMalformedOrUnadvertisedDynamicToolWithoutCallingExecutor(t *testing.T) {
	process, transport := newRunnerTestProcess(t)
	runner := ProcessRunner{Launch: func(context.Context, LaunchOptions) (Process, error) { return process, nil }}
	request := runnerRequestForTest(t)
	request.DynamicTools = []DynamicToolSpec{{Type: "function", Name: "linear_graphql", Description: "Captured Linear GraphQL", InputSchema: json.RawMessage(`{"type":"string"}`)}}
	called := 0
	request.ExecuteTool = func(context.Context, domain.ToolCall, tracker.Session) domain.ToolResult {
		called++
		return domain.ToolUnavailableResult()
	}
	session := startRunnerForTest(t, runner, request, process, transport)
	turnDone := startRunnerTurnForTest(t, session, transport, "turn-1")
	for index, params := range []map[string]any{
		{"arguments": "{ viewer { id } }", "callId": "call-1", "threadId": "wrong-thread", "tool": "linear_graphql", "turnId": "turn-1"},
		{"arguments": "{ viewer { id } }", "callId": "call-2", "threadId": "thread-1", "tool": "future_tool", "turnId": "turn-1"},
	} {
		transport.sendJSON(t, map[string]any{"id": index + 1, "method": "item/tool/call", "params": params})
		response := transport.readRequest(t)
		var rejection RPCError
		if json.Unmarshal(response["error"], &rejection) != nil || rejection.Code != rpcInvalidParams {
			t.Fatalf("response=%+v", response)
		}
	}
	if called != 0 {
		t.Fatalf("executor calls=%d", called)
	}
	completeRunnerTurnForTest(t, transport, turnDone, "turn-1")
	_ = session.Close()
}

func TestRunnerRejectsIncompatibleHandshakeAndStopsProcessBeforeThread(t *testing.T) {
	process, transport := newRunnerTestProcess(t)
	runner := ProcessRunner{Launch: func(context.Context, LaunchOptions) (Process, error) { return process, nil }}
	result := make(chan error, 1)
	go func() {
		_, err := runner.Start(t.Context(), runnerRequestForTest(t))
		result <- err
	}()
	initialize := transport.readRequest(t)
	respondResult(t, transport, initialize, map[string]any{
		"userAgent": "codex_cli_rs/0.145.0", "codexHome": canonicalTestDirectory(t), "platformFamily": "unix", "platformOs": "macos",
	})
	if err := <-result; err == nil || !process.wasStopped() {
		t.Fatalf("err=%v stopped=%v", err, process.wasStopped())
	}
}

func TestRunnerPreflightInitializesWithoutStartingThreadAndAlwaysCloses(t *testing.T) {
	process, transport := newRunnerTestProcess(t)
	runner := ProcessRunner{Launch: func(context.Context, LaunchOptions) (Process, error) { return process, nil }}
	request := runnerRequestForTest(t)
	done := make(chan error, 1)
	go func() { done <- runner.Preflight(t.Context(), request) }()
	initialize := transport.readRequest(t)
	respondResult(t, transport, initialize, compatibleInitializeResponse(request.Workspace.Path))
	if initialized := transport.readRequest(t); methodOf(t, initialized) != "initialized" {
		t.Fatalf("method = %s", initialized["method"])
	}
	if err := <-done; err != nil || !process.wasStopped() {
		t.Fatalf("err=%v stopped=%v", err, process.wasStopped())
	}
}

func TestRunnerUsesSafeApprovalAndSandboxDefaults(t *testing.T) {
	process, transport := newRunnerTestProcess(t)
	runner := ProcessRunner{Launch: func(context.Context, LaunchOptions) (Process, error) { return process, nil }}
	request := runnerRequestForTest(t)
	request.Codex.ApprovalPolicy = nil
	request.Codex.ThreadSandbox = ""
	done := make(chan struct {
		session AgentSession
		err     error
	}, 1)
	go func() {
		session, err := runner.Start(t.Context(), request)
		done <- struct {
			session AgentSession
			err     error
		}{session, err}
	}()
	initialize := transport.readRequest(t)
	respondResult(t, transport, initialize, compatibleInitializeResponse(request.Workspace.Path))
	_ = transport.readRequest(t)
	threadStart := transport.readRequest(t)
	var params ThreadStartParams
	mustUnmarshalRaw(t, threadStart["params"], &params)
	policy, err := params.ApprovalPolicy.MarshalJSON()
	if err != nil || string(policy) != `"on-request"` || params.Sandbox != "workspace-write" {
		t.Fatalf("params=%+v policy=%s err=%v", params, policy, err)
	}
	respondThreadStarted(t, transport, threadStart, request.Workspace.Path, "thread-1")
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	_ = got.session.Close()
}

func TestRunnerRejectsUnsupportedSandboxBeforeLaunch(t *testing.T) {
	launched := false
	runner := ProcessRunner{Launch: func(context.Context, LaunchOptions) (Process, error) {
		launched = true
		return nil, errors.New("unexpected launch")
	}}
	request := runnerRequestForTest(t)
	request.Codex.ThreadSandbox = "danger-full-access"
	if _, err := runner.Start(t.Context(), request); err == nil || launched {
		t.Fatalf("err=%v launched=%v", err, launched)
	}
	request = runnerRequestForTest(t)
	request.Codex.TurnSandboxPolicy = map[string]any{"type": "workspaceWrite", "networkAccess": true}
	if _, err := runner.Start(t.Context(), request); err == nil || launched {
		t.Fatalf("network policy err=%v launched=%v", err, launched)
	}
}

func runnerRequestForTest(t *testing.T) RunnerRequest {
	t.Helper()
	root := canonicalTestDirectory(t)
	workspacePath := root + "/GH-42"
	if err := mkdirPrivate(workspacePath); err != nil {
		t.Fatal(err)
	}
	issue := attemptIssue("open", true, "ready")
	provider := tracker.GitHubConfig{Owner: "coryj627", Repository: "symphony", ActiveStates: []string{"open"}, TerminalStates: []string{"closed"}}
	session, err := tracker.NewSession(issue, provider)
	if err != nil {
		t.Fatal(err)
	}
	return RunnerRequest{
		Issue: issue, Workspace: domain.Workspace{Path: workspacePath, Root: root, IssueID: issue.ID, IssueIdentifier: issue.Identifier}, TrackerSession: session,
		Codex:    workflow.CodexConfig{Command: "codex app-server", ApprovalPolicy: "on-request", ThreadSandbox: "workspace-write", TurnTimeout: time.Minute, ReadTimeout: time.Second, StallTimeout: time.Minute},
		MaxTurns: 2, RequiredLabels: []string{"ready"}, SecretNames: []string{"GITHUB_TOKEN"},
	}
}

func compatibleInitializeResponse(workspace string) map[string]any {
	return map[string]any{"userAgent": "codex_cli_rs/0.144.1", "codexHome": workspace, "platformFamily": "unix", "platformOs": "macos"}
}

func startRunnerForTest(t *testing.T, runner ProcessRunner, request RunnerRequest, _ Process, transport *pipeTransport) AgentSession {
	t.Helper()
	started := make(chan struct {
		session AgentSession
		err     error
	}, 1)
	go func() {
		session, err := runner.Start(t.Context(), request)
		started <- struct {
			session AgentSession
			err     error
		}{session, err}
	}()
	initialize := transport.readRequest(t)
	respondResult(t, transport, initialize, compatibleInitializeResponse(request.Workspace.Path))
	if initialized := transport.readRequest(t); methodOf(t, initialized) != "initialized" {
		t.Fatalf("method=%s", methodOf(t, initialized))
	}
	threadStart := transport.readRequest(t)
	respondThreadStarted(t, transport, threadStart, request.Workspace.Path, "thread-1")
	got := <-started
	if got.err != nil {
		t.Fatal(got.err)
	}
	return got.session
}

func startRunnerTurnForTest(t *testing.T, session AgentSession, transport *pipeTransport, turnID string) <-chan TurnResult {
	t.Helper()
	done := make(chan TurnResult, 1)
	go func() {
		result, err := session.Turn(t.Context(), "Original task")
		if err != nil {
			t.Errorf("turn: %v", err)
		}
		done <- result
	}()
	turnStart := transport.readRequest(t)
	if methodOf(t, turnStart) != "turn/start" {
		t.Fatalf("method=%s", methodOf(t, turnStart))
	}
	respondTurnStarted(t, transport, turnStart, turnID)
	return done
}

func completeRunnerTurnForTest(t *testing.T, transport *pipeTransport, done <-chan TurnResult, turnID string) {
	t.Helper()
	transport.sendJSON(t, map[string]any{
		"method": "turn/completed",
		"params": map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": turnID, "status": "completed", "items": []any{}}},
	})
	result := <-done
	if result.ThreadID != "thread-1" || result.TurnID != turnID || result.Status != TurnCompleted {
		t.Fatalf("result=%+v", result)
	}
}

func mkdirPrivate(path string) error { return os.Mkdir(path, 0o700) }

type runnerTestProcess struct {
	*pipeTransport
	done     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	stopped  bool
}

func newRunnerTestProcess(t *testing.T) (*runnerTestProcess, *pipeTransport) {
	t.Helper()
	stdoutReader, serverOutput := io.Pipe()
	serverInputRaw, stdinWriter := io.Pipe()
	transport := &pipeTransport{
		stdoutReader: stdoutReader, stdinWriter: stdinWriter,
		serverInput: bufio.NewReader(serverInputRaw), serverOutput: serverOutput,
	}
	process := &runnerTestProcess{pipeTransport: transport, done: make(chan struct{})}
	t.Cleanup(func() { _ = process.Close(); _ = process.Stop(context.Background()) })
	return process, transport
}

func (*runnerTestProcess) PID() int                      { return 4242 }
func (process *runnerTestProcess) Done() <-chan struct{} { return process.done }
func (process *runnerTestProcess) Wait(ctx context.Context) error {
	select {
	case <-process.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (process *runnerTestProcess) Stop(context.Context) error {
	process.stopOnce.Do(func() {
		process.mu.Lock()
		process.stopped = true
		process.mu.Unlock()
		close(process.done)
	})
	return nil
}
func (*runnerTestProcess) Diagnostic() string { return "" }
func (process *runnerTestProcess) wasStopped() bool {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.stopped
}
