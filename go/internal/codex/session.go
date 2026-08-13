package codex

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coryj627/symphony/go/internal/buildinfo"
	"golang.org/x/mod/semver"
)

const (
	SessionEventInitialized             = "session_initialized"
	SessionEventThreadStarted           = "thread_started"
	SessionEventTurnStarted             = "turn_started"
	SessionEventTurnCompleted           = "turn_completed"
	SessionEventTurnNotificationIgnored = "turn_notification_ignored"
	SessionEventTelemetryUpdated        = "telemetry_updated"
	SessionEventTelemetryIgnored        = "telemetry_ignored"
	SessionEventNotification            = "notification"
)

const (
	defaultSessionRequestTimeout = 30 * time.Second
	defaultTurnSilenceTimeout    = 5 * time.Minute
)

// SessionOptions is an immutable capture of the workspace and app-server policy.
type SessionOptions struct {
	Workspace      string
	ApprovalPolicy ApprovalPolicy
	ThreadSandbox  string
	DynamicTools   []DynamicToolSpec
	ClientVersion  string
	Manifest       *buildinfo.CodexSchemaManifest
	ProcessID      int
	RequestTimeout time.Duration
	SilenceTimeout time.Duration
	Now            func() time.Time
	OnEvent        func(SessionEvent)
	Process        Process
}

// SessionEvent is a browser-safe state transition or telemetry summary.
type SessionEvent struct {
	Type       string
	At         time.Time
	ProcessID  int
	ThreadID   string
	TurnID     string
	SessionID  string
	Tokens     TokenUsageBreakdown
	RateLimits map[string]any
	Summary    string
}

// Session owns one initialized app-server connection and one live thread.
type Session struct {
	router  *Router
	options SessionOptions

	lifecycleMu sync.Mutex
	mu          sync.Mutex
	initialized bool
	threadID    string
	active      *activeTurn
	lastThread  string
	lastTurn    string
	closed      bool
	telemetry   TelemetrySnapshot
	closeOnce   sync.Once
	closeErr    error

	pumpDone chan struct{}
}

// NewSession captures options and starts the notification/event pump.
func NewSession(router *Router, options SessionOptions) *Session {
	options = cloneSessionOptions(options)
	session := &Session{router: router, options: options, pumpDone: make(chan struct{})}
	go session.pump()
	return session
}

// Start performs initialize, initialized, and thread/start in the pinned order.
func (session *Session) Start(ctx context.Context) (string, error) {
	if err := session.Initialize(ctx); err != nil {
		return "", err
	}
	return session.StartThread(ctx)
}

// Initialize performs the one allowed app-server handshake.
func (session *Session) Initialize(ctx context.Context) error {
	if ctx == nil {
		return newProtocolError("missing_context", "Codex session context is missing.", false, nil)
	}
	session.lifecycleMu.Lock()
	defer session.lifecycleMu.Unlock()
	if err := validateSessionOptions(session.options); err != nil {
		return newProtocolError("invalid_session_options", "Codex session configuration is invalid.", false, err)
	}
	session.mu.Lock()
	if session.initialized {
		session.mu.Unlock()
		return nil
	}
	if session.closed {
		session.mu.Unlock()
		return newProtocolError(ProtocolErrorRouterClosed, "Codex session is closed.", false, nil)
	}
	session.mu.Unlock()

	callCtx, cancel := session.requestContext(ctx)
	defer cancel()
	params := InitializeParams{
		ClientInfo:   ClientInfo{Name: "symphony-go", Title: "Symphony", Version: session.options.ClientVersion},
		Capabilities: InitializeCapabilities{ExperimentalAPI: true},
	}
	var response InitializeResponse
	if err := session.router.Call(callCtx, "initialize", params, &response); err != nil {
		return err
	}
	if !filepath.IsAbs(response.CodexHome) || response.PlatformFamily == "" || response.PlatformOS == "" {
		return newProtocolError(ProtocolErrorMalformedMessage, "Codex initialize response is incomplete.", false, nil)
	}
	manifest, err := session.manifest()
	if err != nil {
		return newProtocolError(string(CompatibilityCodeSchemaIntegrity), "The bundled Codex schema failed its integrity check.", false, err)
	}
	compatibility := CheckCompatibility(response, manifest)
	if !compatibility.DispatchAllowed {
		return newProtocolError(string(compatibility.Code), compatibility.Message, false, nil)
	}
	if err := session.router.Notify("initialized", nil); err != nil {
		return err
	}
	session.mu.Lock()
	session.initialized = true
	session.mu.Unlock()
	session.emit(SessionEvent{Type: SessionEventInitialized, Summary: "Codex app-server initialization completed."})
	return nil
}

// StartThread creates the one live thread used by all turns in this session.
func (session *Session) StartThread(ctx context.Context) (string, error) {
	if ctx == nil {
		return "", newProtocolError("missing_context", "Codex session context is missing.", false, nil)
	}
	session.lifecycleMu.Lock()
	defer session.lifecycleMu.Unlock()
	session.mu.Lock()
	if !session.initialized {
		session.mu.Unlock()
		return "", newProtocolError("session_not_initialized", "Codex session is not initialized.", false, nil)
	}
	if session.threadID != "" {
		threadID := session.threadID
		session.mu.Unlock()
		return threadID, nil
	}
	session.mu.Unlock()

	params := ThreadStartParams{
		ApprovalPolicy:        session.options.ApprovalPolicy,
		Cwd:                   session.options.Workspace,
		DynamicTools:          cloneDynamicTools(session.options.DynamicTools),
		RuntimeWorkspaceRoots: []string{session.options.Workspace},
		Sandbox:               session.options.ThreadSandbox,
	}
	callCtx, cancel := session.requestContext(ctx)
	defer cancel()
	var response ThreadStartResponse
	if err := session.router.Call(callCtx, "thread/start", params, &response); err != nil {
		return "", err
	}
	if response.Thread.ID == "" {
		return "", newProtocolError(ProtocolErrorMalformedMessage, "Codex thread/start response has no thread ID.", false, nil)
	}
	session.mu.Lock()
	session.threadID = response.Thread.ID
	session.mu.Unlock()
	session.emit(SessionEvent{Type: SessionEventThreadStarted, ThreadID: response.Thread.ID, Summary: "Codex thread started."})
	return response.Thread.ID, nil
}

// ServerRequests exposes bounded app-server-owned requests to the broker.
func (session *Session) ServerRequests() <-chan ServerRequest { return session.router.ServerRequests() }

func (session *Session) waitForActiveTurn(ctx context.Context, threadID, turnID string) bool {
	if ctx == nil || threadID == "" || turnID == "" {
		return false
	}
	session.mu.Lock()
	active := session.active
	if session.threadID != threadID || active == nil || active.threadID != threadID {
		session.mu.Unlock()
		return false
	}
	if active.turnID != "" {
		matches := active.turnID == turnID
		session.mu.Unlock()
		return matches
	}
	ready := active.ready
	session.mu.Unlock()
	if ready == nil {
		return false
	}
	select {
	case <-ready:
	case <-ctx.Done():
		return false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.active == active && active.turnID == turnID
}

// Respond answers one string-ID server request and resumes silence accounting.
func (session *Session) Respond(id string, result any) error {
	encoded, err := json.Marshal(id)
	if err != nil {
		return err
	}
	requestID, err := ParseRequestID(encoded)
	if err != nil {
		return err
	}
	return session.RespondRequest(requestID, result)
}

// RespondRequest answers one canonical string- or numeric-ID server request.
func (session *Session) RespondRequest(id RequestID, result any) error {
	return session.router.Respond(id, result)
}

// RejectRequest completes one app-server-owned request with a bounded error.
func (session *Session) RejectRequest(id RequestID, code int64, message string) error {
	return session.router.Reject(id, code, message)
}

// Close interrupts an active turn when possible and closes the router.
func (session *Session) Close() error {
	session.closeOnce.Do(func() {
		session.mu.Lock()
		session.closed = true
		active := session.active
		session.mu.Unlock()
		if active != nil {
			ctx, cancel := context.WithTimeout(context.Background(), session.options.RequestTimeout)
			_ = session.InterruptTurn(ctx)
			cancel()
		}
		routerErr := session.router.Close()
		var processErr error
		if session.options.Process != nil {
			processErr = session.options.Process.Stop(context.Background())
		}
		session.closeErr = errors.Join(routerErr, processErr)
	})
	return session.closeErr
}

func (session *Session) pump() {
	defer close(session.pumpDone)
	for {
		select {
		case event := <-session.router.Events():
			session.handleProtocolEvent(event)
		case notification := <-session.router.Notifications():
			session.handleNotification(notification)
		case <-session.router.Done():
			<-session.router.ReadDone()
			session.drainNotifications()
			session.failActive(session.router.Err())
			return
		}
	}
}

func (session *Session) drainNotifications() {
	for {
		select {
		case notification := <-session.router.Notifications():
			session.handleNotification(notification)
		default:
			return
		}
	}
}

func (session *Session) handleProtocolEvent(event ProtocolEvent) {
	switch event.Code {
	case ProtocolEventActivity:
		session.signalActive(turnSignalActivity)
	case ProtocolEventServerRequest:
		session.signalActive(turnSignalPause)
	case ProtocolEventServerRequestResolved:
		session.signalActive(turnSignalResume)
	}
}

func (session *Session) handleNotification(notification Notification) {
	switch notification.Method {
	case "turn/completed":
		session.handleTurnCompleted(notification.Params)
	case "thread/tokenUsage/updated":
		session.handleTokenUsage(notification.Params)
	case "account/rateLimits/updated":
		session.handleRateLimits(notification.Params)
	default:
		session.emit(SessionEvent{
			Type: SessionEventNotification, Summary: "A Codex notification was received.",
		})
	}
}

func (session *Session) manifest() (buildinfo.CodexSchemaManifest, error) {
	if session.options.Manifest != nil {
		return *session.options.Manifest, nil
	}
	manifest, _, err := buildinfo.LoadCodexSchema()
	return manifest, err
}

func (session *Session) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if session.options.RequestTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, session.options.RequestTimeout)
}

func (session *Session) emit(event SessionEvent) {
	if event.At.IsZero() {
		event.At = session.options.Now().UTC()
	} else {
		event.At = event.At.UTC()
	}
	if event.ProcessID == 0 {
		event.ProcessID = session.options.ProcessID
	}
	if event.ThreadID == "" || event.TurnID == "" {
		session.mu.Lock()
		if event.ThreadID == "" {
			event.ThreadID = session.threadID
		}
		if event.TurnID == "" && session.active != nil {
			event.TurnID = session.active.turnID
		}
		session.mu.Unlock()
	}
	if event.SessionID == "" && event.ThreadID != "" && event.TurnID != "" {
		event.SessionID = event.ThreadID + "-" + event.TurnID
	}
	if session.options.OnEvent != nil {
		session.options.OnEvent(event)
	}
}

func cloneSessionOptions(options SessionOptions) SessionOptions {
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = defaultSessionRequestTimeout
	}
	if options.SilenceTimeout <= 0 {
		options.SilenceTimeout = defaultTurnSilenceTimeout
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	options.Workspace = filepath.Clean(options.Workspace)
	options.DynamicTools = cloneDynamicTools(options.DynamicTools)
	if options.Manifest != nil {
		manifest := *options.Manifest
		manifest.Files = append([]string(nil), manifest.Files...)
		manifest.Compatible = append([]buildinfo.CodexSchemaCompatibility(nil), manifest.Compatible...)
		options.Manifest = &manifest
	}
	return options
}

func cloneDynamicTools(tools []DynamicToolSpec) []DynamicToolSpec {
	clone := append([]DynamicToolSpec(nil), tools...)
	for index := range clone {
		clone[index].InputSchema = append(json.RawMessage(nil), tools[index].InputSchema...)
	}
	return clone
}

func validateSessionOptions(options SessionOptions) error {
	if !filepath.IsAbs(options.Workspace) {
		return errors.New("workspace is not absolute")
	}
	if options.ThreadSandbox != "workspace-write" {
		return errors.New("thread sandbox is not workspace-write")
	}
	if _, err := options.ApprovalPolicy.MarshalJSON(); err != nil {
		return err
	}
	if !validClientVersion(options.ClientVersion) {
		return errors.New("client version is not semantic")
	}
	seen := make(map[string]struct{}, len(options.DynamicTools))
	for _, tool := range options.DynamicTools {
		if err := validateDynamicTool(tool); err != nil {
			return err
		}
		if _, exists := seen[tool.Name]; exists {
			return errors.New("dynamic tool name is duplicated")
		}
		seen[tool.Name] = struct{}{}
	}
	return nil
}

func validClientVersion(version string) bool {
	core := strings.SplitN(strings.SplitN(version, "+", 2)[0], "-", 2)[0]
	return len(strings.Split(core, ".")) == 3 && !strings.HasPrefix(version, "v") && semver.IsValid("v"+version)
}
