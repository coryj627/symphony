package codex

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestSessionStartupUsesPinnedProtocolOrderAndWorkspace(t *testing.T) {
	router, transport := newPipeTransport(t, RouterOptions{})
	options := testSessionOptions(t)
	session := NewSession(router, options)
	result := make(chan struct {
		threadID string
		err      error
	}, 1)
	go func() {
		threadID, err := session.Start(t.Context())
		result <- struct {
			threadID string
			err      error
		}{threadID: threadID, err: err}
	}()

	initialize := transport.readRequest(t)
	if methodOf(t, initialize) != "initialize" {
		t.Fatalf("%s", initialize["method"])
	}
	var initializeParams InitializeParams
	mustUnmarshalRaw(t, initialize["params"], &initializeParams)
	if initializeParams.ClientInfo.Name != "symphony-go" || initializeParams.ClientInfo.Version != options.ClientVersion || !initializeParams.Capabilities.ExperimentalAPI {
		t.Fatalf("%+v", initializeParams)
	}
	respondResult(t, transport, initialize, map[string]any{
		"userAgent": "codex_cli_rs/0.144.1", "codexHome": options.Workspace, "platformFamily": "unix", "platformOs": "macos",
	})

	initialized := transport.readRequest(t)
	if methodOf(t, initialized) != "initialized" {
		t.Fatalf("%s", initialized["method"])
	}
	if _, exists := initialized["params"]; exists {
		t.Fatalf("initialized unexpectedly has params: %s", initialized["params"])
	}

	threadStart := transport.readRequest(t)
	if methodOf(t, threadStart) != "thread/start" {
		t.Fatalf("%s", threadStart["method"])
	}
	var params ThreadStartParams
	mustUnmarshalRaw(t, threadStart["params"], &params)
	if params.Cwd != options.Workspace || params.Sandbox != "workspace-write" || !slices.Equal(params.RuntimeWorkspaceRoots, []string{options.Workspace}) {
		t.Fatalf("%+v", params)
	}
	if len(params.DynamicTools) != 1 || params.DynamicTools[0].Name != "github_api" {
		t.Fatalf("%+v", params.DynamicTools)
	}
	respondThreadStarted(t, transport, threadStart, options.Workspace, "thread-1")

	got := <-result
	if got.err != nil || got.threadID != "thread-1" {
		t.Fatalf("thread=%q err=%v", got.threadID, got.err)
	}
}

func TestSessionRejectsIncompatibleInitializeBeforeInitializedNotification(t *testing.T) {
	router, transport := newPipeTransport(t, RouterOptions{})
	options := testSessionOptions(t)
	session := NewSession(router, options)
	result := make(chan error, 1)
	go func() { result <- session.Initialize(t.Context()) }()
	initialize := transport.readRequest(t)
	respondResult(t, transport, initialize, map[string]any{
		"userAgent": "codex_cli_rs/0.145.0", "codexHome": options.Workspace, "platformFamily": "unix", "platformOs": "macos",
	})
	if err := <-result; err == nil {
		t.Fatal("incompatible app-server was accepted")
	}
}

func TestSessionRejectsIncompleteInitializeResponse(t *testing.T) {
	router, transport := newPipeTransport(t, RouterOptions{})
	session := NewSession(router, testSessionOptions(t))
	result := make(chan error, 1)
	go func() { result <- session.Initialize(t.Context()) }()
	initialize := transport.readRequest(t)
	respondResult(t, transport, initialize, map[string]any{"userAgent": "codex_cli_rs/0.144.1"})
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("incomplete initialize response was accepted")
		}
	case <-time.After(20 * time.Millisecond):
		if initialized := transport.readRequest(t); methodOf(t, initialized) != "initialized" {
			t.Fatalf("%s", initialized["method"])
		}
		if err := <-result; err == nil {
			t.Fatal("incomplete initialize response was accepted")
		}
	}
}

func TestSessionEventsUseUTCProcessAndCapturedToolSnapshot(t *testing.T) {
	events := make(chan SessionEvent, 8)
	options := testSessionOptions(t)
	options.ProcessID = 42
	options.Now = func() time.Time { return time.Date(2026, 8, 12, 20, 0, 0, 0, time.FixedZone("local", -4*60*60)) }
	options.OnEvent = func(event SessionEvent) { events <- event }
	router, _ := newPipeTransport(t, RouterOptions{})
	session := NewSession(router, options)
	options.DynamicTools[0].Name = "mutated"
	session.emit(SessionEvent{Type: SessionEventInitialized})
	event := <-events
	if event.At.Location() != time.UTC || event.ProcessID != 42 {
		t.Fatalf("%+v", event)
	}
	if session.options.DynamicTools[0].Name != "github_api" {
		t.Fatalf("options were not captured: %+v", session.options.DynamicTools)
	}
}

func TestSessionRequiresSemanticClientVersion(t *testing.T) {
	for _, version := range []string{"", "1", "1.2", "01.2.3", "1.2.3-01", "v1.2.3"} {
		t.Run(version, func(t *testing.T) {
			options := testSessionOptions(t)
			options.ClientVersion = version
			if err := validateSessionOptions(options); err == nil {
				t.Fatalf("accepted non-semantic client version %q", version)
			}
		})
	}
	options := testSessionOptions(t)
	options.ClientVersion = "1.2.3-rc.1+build.7"
	if err := validateSessionOptions(options); err != nil {
		t.Fatalf("rejected semantic client version: %v", err)
	}
}

func TestSessionRejectsNilLifecycleContext(t *testing.T) {
	router, _ := newPipeTransport(t, RouterOptions{})
	session := NewSession(router, testSessionOptions(t))
	if err := session.Initialize(nil); err == nil {
		t.Fatalf("Initialize(nil) = %v", err)
	}
}

func TestSessionRespondRequestPreservesServerOwnedNumericID(t *testing.T) {
	session, transport := startTestSession(t, nil)
	transport.sendJSON(t, map[string]any{
		"id": 17, "method": "item/tool/requestUserInput", "params": map[string]any{},
	})
	var request ServerRequest
	select {
	case request = <-session.ServerRequests():
	case <-time.After(time.Second):
		t.Fatal("server request was not delivered")
	}
	responseDone := make(chan error, 1)
	go func() {
		responseDone <- session.RespondRequest(request.ID, map[string]any{"answers": map[string]any{}})
	}()
	response := transport.readRequest(t)
	if err := <-responseDone; err != nil {
		t.Fatal(err)
	}
	if string(response["id"]) != "17" {
		t.Fatalf("response id = %s", response["id"])
	}
}

func testSessionOptions(t *testing.T) SessionOptions {
	t.Helper()
	policy, err := ParseApprovalPolicy(`"on-request"`)
	if err != nil {
		t.Fatal(err)
	}
	manifest := testCompatibilityManifest()
	return SessionOptions{
		Workspace:      t.TempDir(),
		ApprovalPolicy: policy,
		ThreadSandbox:  "workspace-write",
		ClientVersion:  "0.1.0-test",
		Manifest:       &manifest,
		SilenceTimeout: 250000000,
		RequestTimeout: 1000000000,
		DynamicTools: []DynamicToolSpec{{
			Type: "function", Name: "github_api", Description: "Issue-scoped GitHub API", InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	}
}

func methodOf(t *testing.T, fields map[string]json.RawMessage) string {
	t.Helper()
	var method string
	mustUnmarshalRaw(t, fields["method"], &method)
	return method
}

func mustUnmarshalRaw(t *testing.T, raw json.RawMessage, target any) {
	t.Helper()
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

func respondResult(t *testing.T, transport *pipeTransport, request map[string]json.RawMessage, result any) {
	t.Helper()
	var id any
	if err := json.Unmarshal(request["id"], &id); err != nil {
		t.Fatal(err)
	}
	transport.sendJSON(t, map[string]any{"id": id, "result": result})
}

func respondThreadStarted(t *testing.T, transport *pipeTransport, request map[string]json.RawMessage, workspace, threadID string) {
	t.Helper()
	respondResult(t, transport, request, map[string]any{
		"approvalPolicy": "on-request", "approvalsReviewer": "user", "cwd": workspace,
		"model": "gpt-5.6-terra", "modelProvider": "openai",
		"sandbox": map[string]any{
			"type": "workspaceWrite", "writableRoots": []string{workspace}, "networkAccess": false,
		},
		"thread": map[string]any{
			"cliVersion": "0.144.1", "createdAt": 1, "cwd": workspace, "ephemeral": true,
			"id": threadID, "modelProvider": "openai", "preview": "", "sessionId": "session-1",
			"source": "appServer", "status": map[string]any{"type": "idle"}, "turns": []any{}, "updatedAt": 1,
		},
	})
}
