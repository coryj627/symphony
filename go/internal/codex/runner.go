package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coryj627/symphony/go/internal/buildinfo"
	"github.com/coryj627/symphony/go/internal/observability"
	"github.com/coryj627/symphony/go/internal/tracker"
)

const runnerCloseWait = 2 * time.Second

type LaunchProcess func(context.Context, LaunchOptions) (Process, error)

// ProcessRunner launches and owns a contained native app-server process for
// each attempt. Broker may be shared across concurrent sessions so the local
// UI can present all pending operator requests.
type ProcessRunner struct {
	Launch   LaunchProcess
	Broker   *RequestBroker
	BashPath string
	Redactor *observability.Redactor
	Logger   *slog.Logger
}

func (runner ProcessRunner) Start(ctx context.Context, request RunnerRequest) (AgentSession, error) {
	prepared, err := runner.prepare(request)
	if err != nil {
		return nil, err
	}
	session, err := runner.open(ctx, prepared)
	if err != nil {
		return nil, err
	}
	if _, err := session.protocol.Start(ctx); err != nil {
		_ = session.protocol.Close()
		return nil, err
	}
	session.startBroker(ctx)
	return session, nil
}

// Preflight performs a real launch and compatibility handshake without
// allocating a thread. Real attempts repeat the handshake.
func (runner ProcessRunner) Preflight(ctx context.Context, request RunnerRequest) error {
	prepared, err := runner.prepare(request)
	if err != nil {
		return err
	}
	session, err := runner.open(ctx, prepared)
	if err != nil {
		return err
	}
	if err := session.protocol.Initialize(ctx); err != nil {
		_ = session.protocol.Close()
		return err
	}
	return session.protocol.Close()
}

type preparedRunnerRequest struct {
	RunnerRequest
	approvalPolicy ApprovalPolicy
	threadSandbox  string
}

func (runner ProcessRunner) prepare(request RunnerRequest) (preparedRunnerRequest, error) {
	if runner.Launch == nil {
		return preparedRunnerRequest{}, errors.New("Codex process launcher is unavailable")
	}
	if err := request.Issue.ValidateRequired(); err != nil || request.Workspace.Path == "" || request.Workspace.Root == "" || request.MaxTurns < 1 {
		return preparedRunnerRequest{}, errors.New("Codex runner request is incomplete")
	}
	if request.Workspace.IssueID != request.Issue.ID || request.Workspace.IssueIdentifier != request.Issue.Identifier || request.TrackerSession.Issue.ID != request.Issue.ID {
		return preparedRunnerRequest{}, errors.New("Codex runner identities do not match")
	}
	approval, err := runnerApprovalPolicy(request.Codex.ApprovalPolicy)
	if err != nil {
		return preparedRunnerRequest{}, fmt.Errorf("Codex approval policy is invalid: %w", err)
	}
	sandbox := strings.TrimSpace(request.Codex.ThreadSandbox)
	if sandbox == "" {
		sandbox = "workspace-write"
	}
	if sandbox != "workspace-write" {
		return preparedRunnerRequest{}, errors.New("Codex thread sandbox must be workspace-write")
	}
	if err := validateTurnSandbox(request.Codex.TurnSandboxPolicy, request.Workspace.Path); err != nil {
		return preparedRunnerRequest{}, err
	}
	for _, tool := range request.DynamicTools {
		if err := validateDynamicTool(tool); err != nil {
			return preparedRunnerRequest{}, err
		}
	}
	if (len(request.DynamicTools) > 0) != (request.ExecuteTool != nil) {
		return preparedRunnerRequest{}, errors.New("Codex dynamic tools and executor must be configured together")
	}
	request.Issue, err = request.Issue.Clone()
	if err != nil {
		return preparedRunnerRequest{}, err
	}
	request.TrackerSession, err = request.TrackerSession.Clone()
	if err != nil {
		return preparedRunnerRequest{}, err
	}
	request.RequiredLabels = append([]string(nil), request.RequiredLabels...)
	request.SecretNames = append([]string(nil), request.SecretNames...)
	request.DynamicTools = cloneDynamicTools(request.DynamicTools)
	request.Codex = cloneCodexConfig(request.Codex)
	return preparedRunnerRequest{RunnerRequest: request, approvalPolicy: approval, threadSandbox: sandbox}, nil
}

func (runner ProcessRunner) open(ctx context.Context, request preparedRunnerRequest) (*liveAgentSession, error) {
	if ctx == nil {
		return nil, errors.New("Codex runner context is missing")
	}
	redactor := runner.Redactor
	if redactor == nil {
		redactor = observability.NewRedactor(nil, nil)
	}
	logger := runner.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	process, err := runner.Launch(ctx, LaunchOptions{
		Cwd: request.Workspace.Path, WorkspaceRoot: request.Workspace.Root,
		Command: request.Codex.Command, BashPath: runner.BashPath, SecretNames: request.SecretNames,
		Redactor: redactor, Logger: logger,
	})
	if err != nil {
		return nil, err
	}
	broker := runner.Broker
	if broker == nil {
		broker = NewRequestBroker(RequestBrokerOptions{Redactor: redactor})
	}
	router := NewRouter(process, RouterOptions{ReadTimeout: request.Codex.ReadTimeout})
	protocol := NewSession(router, SessionOptions{
		Workspace: request.Workspace.Path, ApprovalPolicy: request.approvalPolicy,
		ThreadSandbox: request.threadSandbox, DynamicTools: request.DynamicTools,
		ClientVersion: buildinfo.Version, ProcessID: process.PID(),
		RequestTimeout: request.Codex.ReadTimeout, SilenceTimeout: request.Codex.StallTimeout,
		OnEvent: request.OnSessionEvent, Process: process,
	})
	return &liveAgentSession{
		protocol: protocol, broker: broker, issueID: request.Issue.ID,
		issueIdentifier: request.Issue.Identifier, process: process,
		trackerSession: request.TrackerSession, executeTool: request.ExecuteTool,
		dynamicTools: dynamicToolNameSet(request.DynamicTools), sessionIDs: make(map[string]struct{}),
	}, nil
}

type liveAgentSession struct {
	protocol        *Session
	broker          *RequestBroker
	issueID         string
	issueIdentifier string
	process         Process
	trackerSession  tracker.Session
	executeTool     AgentToolExecutor
	dynamicTools    map[string]struct{}

	mu         sync.Mutex
	sessionIDs map[string]struct{}
	brokerDone chan struct{}
	cancel     context.CancelFunc
	closeOnce  sync.Once
	closeErr   error
}

func (session *liveAgentSession) Turn(ctx context.Context, prompt string) (TurnResult, error) {
	return session.protocol.StartTurn(ctx, prompt)
}

func (session *liveAgentSession) startBroker(parent context.Context) {
	brokerCtx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	session.mu.Lock()
	session.cancel = cancel
	session.brokerDone = done
	session.mu.Unlock()
	go func() {
		defer close(done)
		_ = session.broker.RunWithHandler(brokerCtx, session.protocol, session.requestContext, session.handleServerRequest)
	}()
}

func (session *liveAgentSession) requestContext(request ServerRequest) ServerRequestContext {
	sessionID := serverRequestSessionID(request)
	if sessionID != "" {
		session.mu.Lock()
		session.sessionIDs[sessionID] = struct{}{}
		session.mu.Unlock()
	}
	return ServerRequestContext{
		SessionID: sessionID, IssueID: session.issueID, IssueIdentifier: session.issueIdentifier,
		Respond: session.protocol.RespondRequest, Reject: session.protocol.RejectRequest,
	}
}

func (session *liveAgentSession) Close() error {
	session.closeOnce.Do(func() {
		session.mu.Lock()
		cancel := session.cancel
		done := session.brokerDone
		ids := make([]string, 0, len(session.sessionIDs))
		for id := range session.sessionIDs {
			ids = append(ids, id)
		}
		session.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		for _, id := range ids {
			session.broker.CancelSession(id)
		}
		protocolErr := session.protocol.Close()
		if errors.Is(protocolErr, ErrProcessStopTimeout) && session.process != nil {
			<-session.process.Done()
		}
		var brokerErr error
		if done != nil {
			select {
			case <-done:
			case <-time.After(runnerCloseWait):
				brokerErr = errors.New("Codex request broker did not stop")
			}
		}
		session.closeErr = errors.Join(protocolErr, brokerErr)
	})
	return session.closeErr
}

func runnerApprovalPolicy(value any) (ApprovalPolicy, error) {
	if value == nil {
		value = "on-request"
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ApprovalPolicy{}, err
	}
	return ParseApprovalPolicy(string(encoded))
}

func validateTurnSandbox(policy map[string]any, workspace string) error {
	if len(policy) == 0 {
		return nil
	}
	for key := range policy {
		switch key {
		case "type", "writableRoots", "networkAccess", "excludeSlashTmp", "excludeTmpdirEnvVar":
		default:
			return fmt.Errorf("Codex turn sandbox contains unsupported field %q", key)
		}
	}
	if kind, ok := policy["type"]; !ok || kind != "workspaceWrite" {
		return errors.New("Codex turn sandbox must use workspaceWrite")
	}
	if network, ok := policy["networkAccess"]; ok && network != false {
		return errors.New("Codex turn sandbox network access must be disabled")
	}
	if rootsValue, ok := policy["writableRoots"]; ok {
		roots, ok := rootsValue.([]any)
		if !ok || len(roots) != 1 || roots[0] != workspace {
			if stringsValue, stringsOK := rootsValue.([]string); !stringsOK || !slices.Equal(stringsValue, []string{workspace}) {
				return errors.New("Codex turn sandbox writable roots must contain only the issue workspace")
			}
		}
	}
	for _, key := range []string{"excludeSlashTmp", "excludeTmpdirEnvVar"} {
		if value, ok := policy[key]; ok && value != false {
			return fmt.Errorf("Codex turn sandbox %s is not supported", key)
		}
	}
	return nil
}

func serverRequestSessionID(request ServerRequest) string {
	var identity requestIdentity
	if decodeRequestParams(request.Params, &identity) != nil || identity.ThreadID == "" || identity.TurnID == "" {
		return ""
	}
	return identity.ThreadID + "-" + identity.TurnID
}

var _ AgentRunner = ProcessRunner{}
var _ AgentSession = (*liveAgentSession)(nil)
