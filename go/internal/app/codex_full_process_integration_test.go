//go:build darwin || windows

package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	fullProcessCanary         = "phase4-secret-canary"
	fullProcessBootstrapToken = "phase4-deterministic-bootstrap-capability"
)

type fullProcessState struct {
	Candidates []json.RawMessage `json:"candidates"`
	Running    []json.RawMessage `json:"running"`
	Retrying   []json.RawMessage `json:"retrying"`
	Requests   []struct {
		RequestID string `json:"request_id"`
		Kind      string `json:"kind"`
	} `json:"requests"`
	Scheduler struct {
		State string `json:"state"`
	} `json:"scheduler"`
}

type synchronizedText struct {
	mu      sync.Mutex
	builder strings.Builder
}

func (output *synchronizedText) addLine(line string) {
	output.mu.Lock()
	defer output.mu.Unlock()
	output.builder.WriteString(line)
	output.builder.WriteByte('\n')
}

func (output *synchronizedText) add(value string) {
	output.mu.Lock()
	defer output.mu.Unlock()
	output.builder.WriteString(value)
}

func (output *synchronizedText) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.builder.String()
}

func TestBuiltSymphonyCompletesTwoTurnsOperatorRequestsAndProviderToolWithoutLeaks(t *testing.T) {
	root := privateTempDir(t)
	fakeBinary := buildFakeCodexBinary(t)
	symphonyBinary := buildE2ESymphonyBinary(t, root)
	workspaceRoot := filepath.Join(root, "workspaces")
	dataRoot := filepath.Join(root, "data")
	diagnosticsRoot := filepath.Join(root, "diagnostics")
	if err := os.MkdirAll(diagnosticsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	workflowPath := writeFullProcessWorkflow(t, root, workspaceRoot, fakeBinary)

	command := exec.Command(symphonyBinary, workflowPath, "--port", "0", "--data-dir", dataRoot)
	configureFullProcessCommand(command)
	command.Env = fullProcessEnvironment(root)
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatalf("start built Symphony: %v", err)
	}
	var output synchronizedText
	ready := make(chan string, 1)
	readPipe := func(reader io.Reader, findURL bool) {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if findURL && strings.HasPrefix(line, "http://127.0.0.1:") && strings.Contains(line, "?access_token=") {
				select {
				case ready <- line:
				default:
				}
			}
			output.addLine(fullProcessBootstrapPattern.ReplaceAllString(line, "access_token=[REDACTED]"))
		}
	}
	go readPipe(stderr, true)
	go readPipe(stdout, false)
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	t.Cleanup(func() {
		select {
		case <-exited:
			return
		default:
		}
		_ = command.Process.Kill()
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
		}
	})

	protectedURL := awaitProtectedURL(t, ready, exited, &output)
	parsed, err := url.Parse(protectedURL)
	if err != nil {
		t.Fatal(err)
	}
	baseURL := parsed.Scheme + "://" + parsed.Host
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	cookie := exchangeFullProcessCapability(t, client, protectedURL)
	var captured synchronizedText
	csrf := fullProcessCSRFToken(t, client, baseURL, cookie, &captured)
	startFullProcessRuntime(t, client, baseURL, cookie, csrf, &captured, diagnosticsRoot)

	approval := awaitFullProcessState(t, client, baseURL, cookie, &captured, dataRoot, func(state fullProcessState) bool {
		return len(state.Requests) == 1 && state.Requests[0].Kind == "command_approval"
	})
	csrf, sessionID := fullProcessFormTokens(t, client, baseURL, cookie, &captured)
	postFullProcessResponse(t, client, baseURL, cookie, approval.Requests[0].RequestID, url.Values{
		"csrf_token": {csrf}, "session_id": {sessionID}, "choice_id": {"accept"}, "return_to": {"/"},
	}, &captured)

	input := awaitFullProcessState(t, client, baseURL, cookie, &captured, dataRoot, func(state fullProcessState) bool {
		return len(state.Requests) == 1 && state.Requests[0].Kind == "user_input"
	})
	csrf, sessionID = fullProcessFormTokens(t, client, baseURL, cookie, &captured)
	postFullProcessResponse(t, client, baseURL, cookie, input.Requests[0].RequestID, url.Values{
		"csrf_token": {csrf}, "session_id": {sessionID}, "return_to": {"/"},
		"answer.platform": {"option-1"}, "other.detail": {"integration detail"}, "other.token": {"temporary-answer"},
	}, &captured)

	final := awaitFullProcessState(t, client, baseURL, cookie, &captured, dataRoot, func(state fullProcessState) bool {
		return state.Scheduler.State == "running" && len(state.Requests) == 0 && len(state.Candidates) == 0 && len(state.Running) == 0 && len(state.Retrying) == 0
	})
	if final.Scheduler.State != "running" {
		t.Fatalf("final scheduler state = %q", final.Scheduler.State)
	}

	stopFullProcess(t, command)
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("built Symphony shutdown: %v; output=%s", err, output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("built Symphony did not stop after the supported interrupt")
	}

	for name, value := range map[string]string{"HTTP responses": captured.String(), "process output": output.String()} {
		assertFullProcessSecretsAbsent(t, name, value)
	}
	for _, path := range []string{dataRoot, workspaceRoot} {
		assertFullProcessTreeHasNoSecrets(t, path)
	}
}

func buildE2ESymphonyBinary(t *testing.T, root string) string {
	t.Helper()
	name := "symphony-e2e"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(root, name)
	goTool := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOOS == "windows" {
		goTool += ".exe"
	}
	command := exec.CommandContext(t.Context(), goTool, "build", "-tags=codex_e2e", "-o", binary, "./cmd/symphony")
	command.Dir = filepath.Clean(filepath.Join("..", ".."))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build e2e Symphony: %v\n%s", err, output)
	}
	return binary
}

func writeFullProcessWorkflow(t *testing.T, root, workspaceRoot, fakeBinary string) string {
	t.Helper()
	if err := os.MkdirAll(workspaceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "WORKFLOW.md")
	source := fmt.Sprintf(`---
tracker:
  kind: github
  provider:
    owner: fixture
    repository: fixture
    credential_ref: "$SYMPHONY_GITHUB_TEST_TOKEN"
  required_labels: [ready]
  active_states: [open]
  terminal_states: [closed]
polling:
  interval_ms: 25
workspace:
  root: %s
agent:
  max_concurrent_agents: 1
  max_turns: 2
  max_retry_backoff_ms: 100
codex:
  command: %s
  approval_policy: on-request
  thread_sandbox: workspace-write
  turn_timeout_ms: 10000
  read_timeout_ms: 5000
  stall_timeout_ms: 5000
server:
  port: 0
  operator_response_timeout_ms: 30000
---
Work on {{ issue.identifier }} through the deterministic full-process fixture.
`, strconv.Quote(filepath.ToSlash(workspaceRoot)), strconv.Quote(strconv.Quote(filepath.ToSlash(fakeBinary))))
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fullProcessEnvironment(root string) []string {
	blocked := map[string]bool{
		"SYMPHONY_E2E_CODEX_TRACKER": true, "SYMPHONY_FAKE_CODEX_SCENARIO": true,
		"SYMPHONY_E2E_BOOTSTRAP_TOKEN": true, "SYMPHONY_FAKE_CODEX_TRACE_PATH": true,
		"SYMPHONY_E2E_SECRET_CANARY": true, "SYMPHONY_GITHUB_TEST_TOKEN": true, "SYMPHONY_LINEAR_TEST_TOKEN": true,
	}
	environment := make([]string, 0, len(os.Environ())+8)
	for _, entry := range os.Environ() {
		name := strings.SplitN(entry, "=", 2)[0]
		if !blocked[name] {
			environment = append(environment, entry)
		}
	}
	environment = append(environment,
		"SYMPHONY_E2E_CODEX_TRACKER=1", "SYMPHONY_FAKE_CODEX_SCENARIO=full",
		"SYMPHONY_FAKE_CODEX_TRACE_PATH="+filepath.Join(root, "diagnostics", "fake-codex-trace.log"),
		"SYMPHONY_E2E_BOOTSTRAP_TOKEN="+fullProcessBootstrapToken,
		"SYMPHONY_E2E_SECRET_CANARY="+fullProcessCanary,
		"SYMPHONY_GITHUB_TEST_TOKEN="+fullProcessCanary,
		"SYMPHONY_LINEAR_TEST_TOKEN="+fullProcessCanary,
	)
	if runtime.GOOS == "darwin" {
		environment = append(environment, "HOME="+root)
	} else {
		environment = append(environment, "APPDATA="+filepath.Join(root, "AppData", "Roaming"))
	}
	return environment
}

func TestFullProcessEnvironmentPinsPrivateTracePath(t *testing.T) {
	t.Setenv("SYMPHONY_FAKE_CODEX_TRACE_PATH", "attacker-controlled-trace.log")
	root := privateTempDir(t)
	want := filepath.Join(root, "diagnostics", "fake-codex-trace.log")
	var got string
	count := 0
	for _, entry := range fullProcessEnvironment(root) {
		name, value, found := strings.Cut(entry, "=")
		if found && name == "SYMPHONY_FAKE_CODEX_TRACE_PATH" {
			count++
			got = value
		}
	}
	if count != 1 || got != want {
		t.Fatalf("trace environment count = %d, value = %q, want one %q", count, got, want)
	}
}

func awaitProtectedURL(t *testing.T, ready <-chan string, exited <-chan error, output *synchronizedText) string {
	t.Helper()
	select {
	case protected := <-ready:
		return protected
	case err := <-exited:
		t.Fatalf("built Symphony exited before ready: %v; output=%s", err, output.String())
	case <-time.After(15 * time.Second):
		t.Fatalf("built Symphony did not become ready; output=%s", output.String())
	}
	return ""
}

func exchangeFullProcessCapability(t *testing.T, client *http.Client, protectedURL string) *http.Cookie {
	t.Helper()
	response, err := client.Get(protectedURL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/" {
		t.Fatalf("bootstrap exchange = %d location=%q", response.StatusCode, response.Header.Get("Location"))
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == "symphony_session" {
			return cookie
		}
	}
	t.Fatal("bootstrap exchange omitted the session cookie")
	return nil
}

func awaitFullProcessState(t *testing.T, client *http.Client, baseURL string, cookie *http.Cookie, captured *synchronizedText, debugRoot string, accept func(fullProcessState) bool) fullProcessState {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last fullProcessState
	var lastBody string
	for time.Now().Before(deadline) {
		body, status := fullProcessGet(t, client, baseURL+"/api/v1/state", cookie)
		lastBody = body
		captured.add(body)
		if status != http.StatusOK {
			t.Fatalf("state API = %d body=%s", status, body)
		}
		if err := json.Unmarshal([]byte(body), &last); err != nil {
			t.Fatalf("decode state: %v body=%s", err, body)
		}
		if accept(last) {
			return last
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out awaiting full-process state: %+v body=%s diagnostics=%s", last, lastBody, fullProcessDiagnostics(debugRoot))
	return fullProcessState{}
}

func fullProcessDiagnostics(root string) string {
	var diagnostics strings.Builder
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		value, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		text := string(bytes.ToValidUTF8(value, nil))
		text = strings.ReplaceAll(text, fullProcessCanary, "[REDACTED]")
		text = strings.ReplaceAll(text, "temporary-answer", "[REDACTED]")
		if len(text) > 16<<10 {
			text = text[len(text)-(16<<10):]
		}
		diagnostics.WriteString(path)
		diagnostics.WriteByte('\n')
		diagnostics.WriteString(text)
		return nil
	})
	return diagnostics.String()
}

var (
	fullProcessBootstrapPattern = regexp.MustCompile(`access_token=[^&[:space:]]+`)
	csrfFieldPattern            = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)
	sessionFieldPattern         = regexp.MustCompile(`name="session_id" value="([^"]+)"`)
)

func fullProcessFormTokens(t *testing.T, client *http.Client, baseURL string, cookie *http.Cookie, captured *synchronizedText) (string, string) {
	t.Helper()
	body, status := fullProcessGet(t, client, baseURL+"/", cookie)
	captured.add(body)
	if status != http.StatusOK {
		t.Fatalf("overview = %d body=%s", status, body)
	}
	csrf := csrfFieldPattern.FindStringSubmatch(body)
	session := sessionFieldPattern.FindStringSubmatch(body)
	if len(csrf) != 2 || len(session) != 2 {
		t.Fatalf("operator form omitted CSRF or session binding")
	}
	return csrf[1], session[1]
}

func fullProcessCSRFToken(t *testing.T, client *http.Client, baseURL string, cookie *http.Cookie, captured *synchronizedText) string {
	t.Helper()
	body, status := fullProcessGet(t, client, baseURL+"/", cookie)
	captured.add(body)
	if status != http.StatusOK {
		t.Fatalf("overview = %d body=%s", status, body)
	}
	csrf := csrfFieldPattern.FindStringSubmatch(body)
	if len(csrf) != 2 {
		t.Fatal("overview omitted the CSRF token")
	}
	return csrf[1]
}

func startFullProcessRuntime(t *testing.T, client *http.Client, baseURL string, cookie *http.Cookie, csrf string, captured *synchronizedText, debugRoot string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/runtime/start", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(cookie)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	request.Header.Set("Origin", baseURL)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	captured.add(string(body))
	if response.StatusCode != http.StatusAccepted {
		state, _ := fullProcessGet(t, client, baseURL+"/api/v1/state", cookie)
		t.Fatalf("runtime start = %d body=%s state=%s diagnostics=%s", response.StatusCode, body, state, fullProcessDiagnostics(debugRoot))
	}
}

func postFullProcessResponse(t *testing.T, client *http.Client, baseURL string, cookie *http.Cookie, requestID string, form url.Values, captured *synchronizedText) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/requests/"+url.PathEscape(requestID)+"/respond", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(cookie)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", baseURL)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	captured.add(string(body))
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("operator response = %d body=%s", response.StatusCode, body)
	}
}

func fullProcessGet(t *testing.T, client *http.Client, rawURL string, cookie *http.Cookie) (string, int) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(cookie)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(body), response.StatusCode
}

func stopFullProcess(t *testing.T, command *exec.Cmd) {
	t.Helper()
	if err := interruptFullProcess(command.Process); err != nil {
		t.Fatal(err)
	}
}

func assertFullProcessSecretsAbsent(t *testing.T, source, value string) {
	t.Helper()
	for _, forbidden := range []string{fullProcessCanary, fullProcessBootstrapToken, "temporary-answer"} {
		if strings.Contains(value, forbidden) {
			t.Fatalf("%s retained forbidden secret material", source)
		}
	}
}

func assertFullProcessTreeHasNoSecrets(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		value, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		assertFullProcessSecretsAbsent(t, path, string(bytes.ToValidUTF8(value, nil)))
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
