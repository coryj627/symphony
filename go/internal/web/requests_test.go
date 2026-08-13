package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/codex"
	"github.com/coryj627/symphony/go/internal/domain"
)

func TestRespondRejectsStaleRequestWithoutMutatingBroker(t *testing.T) {
	runtime := &pageRuntimeFake{respondErr: codex.ErrStaleRequest}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	form := url.Values{"session_id": {"session-1"}, "choice_id": {"decline"}, "return_to": {"/"}}
	request := requestForm(t, "/api/v1/requests/stale/respond", form)
	request.SetPathValue("request_id", "stale")
	recorder := httptest.NewRecorder()
	handler.respondOperatorRequest(recorder, request)
	if recorder.Code != http.StatusConflict || runtime.respondCalls.Load() != 1 || runtime.extendCalls.Load() != 0 {
		t.Fatalf("status/respond/extend = %d/%d/%d body=%s", recorder.Code, runtime.respondCalls.Load(), runtime.extendCalls.Load(), recorder.Body.String())
	}
}

func TestRespondRejectsMalformedFormBeforeRuntimeMutation(t *testing.T) {
	runtime := &pageRuntimeFake{}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	tests := []url.Values{
		{"session_id": {"session-1"}, "choice_id": {"decline"}, "return_to": {"https://attacker.invalid"}},
		{"session_id": {"session-1", "duplicate"}, "choice_id": {"decline"}, "return_to": {"/"}},
		{"session_id": {"session-1"}, "choice_id": {"decline"}, "return_to": {"/"}, "unexpected": {"value"}},
		{"session_id": {"session-1"}, "return_to": {"/"}, "answer.question": {"__other__"}},
	}
	for index, form := range tests {
		request := requestForm(t, "/api/v1/requests/request-1/respond", form)
		request.SetPathValue("request_id", "request-1")
		recorder := httptest.NewRecorder()
		handler.respondOperatorRequest(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("case %d status=%d body=%s", index, recorder.Code, recorder.Body.String())
		}
	}
	if runtime.respondCalls.Load() != 0 {
		t.Fatalf("invalid forms invoked runtime %d times", runtime.respondCalls.Load())
	}
}

func TestRespondInvalidFormRendersFocusedErrorSummaryAndClearsSecretFields(t *testing.T) {
	runtime := &pageRuntimeFake{}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	request := requestForm(t, "/api/v1/requests/request-1/respond", url.Values{
		"session_id": {"session-1"}, "return_to": {"/"}, "answer.secret": {"__other__"}, "other.secret": {"secret-canary"},
		"unexpected": {"value"},
	})
	request.SetPathValue("request_id", "request-1")
	recorder := httptest.NewRecorder()
	handler.respondOperatorRequest(recorder, request)
	if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("response = %d content-type=%q body=%s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{`data-focus-target="error-summary"`, `id="error-summary" role="alert"`, "Review the operator response and try again."} {
		if !strings.Contains(body, want) {
			t.Fatalf("error page omitted %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "secret-canary") {
		t.Fatal("error page exposed secret answer")
	}
	for key := range request.PostForm {
		if strings.HasPrefix(key, "answer.") || strings.HasPrefix(key, "other.") {
			t.Fatalf("secret answer remained in parsed form: %q", key)
		}
	}
}

func TestRespondMapsQuestionAnswersClearsFormAndRestoresRequestRegionFocus(t *testing.T) {
	runtime := &pageRuntimeFake{}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	form := url.Values{
		"session_id": {"session-1"}, "return_to": {"/issues/GH-42"},
		"answer.platform": {"option-2"}, "answer.detail": {"__other__"}, "other.detail": {"typed answer"},
	}
	request := requestForm(t, "/api/v1/requests/request-1/respond", form)
	request.SetPathValue("request_id", "request-1")
	recorder := httptest.NewRecorder()
	handler.respondOperatorRequest(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/issues/GH-42?focus=requests-heading&result=request-responded" {
		t.Fatalf("response = %d location=%q body=%s", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	runtime.mu.Lock()
	response := runtime.lastResponse.Clone()
	runtime.mu.Unlock()
	if response.RequestID != "request-1" || response.SessionID != "session-1" || response.Answers["platform"][0] != "option-2" || response.Answers["detail"][0] != "typed answer" {
		t.Fatalf("runtime response = %#v", response)
	}
	for key := range request.PostForm {
		if strings.HasPrefix(key, "answer.") || strings.HasPrefix(key, "other.") {
			t.Fatalf("answer remained in parsed form: %q", key)
		}
	}
}

func TestExtendUsesExactRequestIDAndMapsLimitToBadRequest(t *testing.T) {
	runtime := &pageRuntimeFake{extendErr: codex.ErrExtensionLimit}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	request := requestForm(t, "/api/v1/requests/request-1/extend", url.Values{"return_to": {"/"}})
	request.SetPathValue("request_id", "request-1")
	recorder := httptest.NewRecorder()
	handler.extendOperatorRequest(recorder, request)
	if recorder.Code != http.StatusBadRequest || runtime.extendCalls.Load() != 1 || runtime.lastExtended != "request-1" {
		t.Fatalf("extend = %d calls=%d id=%q body=%s", recorder.Code, runtime.extendCalls.Load(), runtime.lastExtended, recorder.Body.String())
	}
}

func TestOperatorRequestsRenderNamedControlsDeadlinesAndSecretPasteField(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	request := domain.OperatorRequest{
		ID: "request-1", SessionID: "thread-1-turn-1", IssueIdentifier: "GH-42", Kind: "user_input",
		Title: "Codex needs your input", Summary: "Answer the questions.", OpenedAt: now,
		WarningAt: now.Add(40 * time.Second), DeadlineAt: now.Add(time.Minute), ExtensionsRemaining: 10,
		Questions: []domain.OperatorQuestion{{
			ID: "token", Label: "Token", Description: "Enter the secret", Required: true, AllowsOther: true, IsSecret: true,
			Choices: []domain.OperatorChoice{},
		}},
	}
	runtime := &pageRuntimeFake{snapshot: domain.Snapshot{Requests: []domain.OperatorRequest{request}}}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	recorder := serveDirect(t, handler, http.MethodGet, "/", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	html := recorder.Body.String()
	for _, want := range []string{
		`<h2 id="requests-heading" tabindex="-1">Operator requests</h2>`,
		`<fieldset><legend>Token</legend>`,
		`type="password"`, `name="other.token"`, `autocomplete="new-password"`,
		`data-request-deadline aria-live="off"`, `role="status" aria-live="polite"`,
		`Extend response time`, `thread-1-turn-1`, `GH-42`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("operator UI omitted %q\n%s", want, html)
		}
	}
	if strings.Contains(html, `onpaste=`) {
		t.Fatal("secret field blocks paste")
	}
}

func TestIssueOperatorRequestFormIncludesCSRFAndRestoresRegionFocus(t *testing.T) {
	request := domain.OperatorRequest{
		ID: "request-1", SessionID: "thread-1-turn-1", IssueIdentifier: "GH-42", Kind: "choice",
		Title: "Codex needs approval", Summary: "Choose a response.", Choices: []domain.OperatorChoice{{ID: "decline", Label: "Decline"}},
		Details: []domain.OperatorDetail{{Label: "Requested permission profile", Value: `{"network":{"enabled":true}}`}},
	}
	runtime := &pageRuntimeFake{
		details:  map[string]domain.IssueDetail{"GH-42": {Issue: domain.Issue{ID: "42", Identifier: "GH-42", Title: "Issue", State: "Open"}}},
		snapshot: domain.Snapshot{Requests: []domain.OperatorRequest{request}},
	}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	recorder := serveDirect(t, handler, http.MethodGet, "/issues/GH-42?result=request-responded&focus=requests-heading", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{
		`name="csrf_token" value="` + testCSRFToken + `"`,
		`name="return_to" value="/issues/GH-42"`,
		`data-focus-target="requests-heading"`,
		`Operator response submitted.`,
		`<dt>Requested permission profile</dt><dd><code>{&#34;network&#34;:{&#34;enabled&#34;:true}}</code></dd>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("issue request page omitted %q\n%s", want, body)
		}
	}
}

func TestOperatorRequestRoutesArePostOnlyWithoutApplicationDispatch(t *testing.T) {
	runtime := &pageRuntimeFake{}
	handler := newTestPageHandler(t, PageOptions{Queries: runtime, Commands: runtime})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/requests/request-1/respond", nil)
	allowed, defined := handler.AllowedMethods(request)
	if !defined || len(allowed) != 1 || allowed[0] != http.MethodPost {
		t.Fatalf("method policy = %v, %t", allowed, defined)
	}
	malformed := httptest.NewRequest(http.MethodPost, "/api/v1/requests/request%2F1/respond", nil)
	if _, defined := handler.AllowedMethods(malformed); defined {
		t.Fatal("encoded slash request route was accepted")
	}
	if runtime.respondCalls.Load() != 0 || runtime.extendCalls.Load() != 0 {
		t.Fatal("method discovery invoked application commands")
	}
}

func requestForm(t *testing.T, target string, form url.Values) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return request
}
