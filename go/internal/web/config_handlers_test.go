package web

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coryj627/symphony/go/internal/app"
	"github.com/coryj627/symphony/go/internal/secrets"
	"github.com/coryj627/symphony/go/internal/workflow"
)

func TestConfigurationRendersGitHubAndLinearStructuredState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		source string
		wants  []string
	}{
		{name: "github", source: validGitHubSource, wants: []string{`option value="github" selected`, `value="coryj627"`, `value="symphony"`, "github:coryj627/symphony"}},
		{name: "linear", source: validLinearSource, wants: []string{`option value="linear" selected`, `value="symphony-project"`, "linear:symphony-project"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, _, _ := configuredHTTPApp(t, test.source, secrets.Status{})
			cookie := exchange(t, server)
			response := authenticatedRequest(t, server, cookie, http.MethodGet, "/configuration", nil, nil)
			body := readResponse(t, response)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("GET configuration = %d", response.StatusCode)
			}
			for _, want := range test.wants {
				assertContains(t, body, want)
			}
			assertContains(t, body, `<label for="raw-source">Complete WORKFLOW.md</label>`)
			assertContains(t, body, `<textarea id="raw-source" name="raw_source"`)
			assertContains(t, body, `spellcheck="false"`)
			assertContains(t, body, `type="password"`)
			assertContains(t, body, `name="credential_tracker_kind" value="`+test.name+`"`)
			assertContains(t, body, `name="credential_base_digest" value="`+digestOf(test.source)+`"`)
			if strings.Contains(body, `name="credential" value=`) {
				t.Fatal("credential input rendered a value")
			}
			if count := strings.Count(body, "<form "); count != 3 {
				t.Fatalf("configuration rendered %d forms, want 3", count)
			}
		})
	}
}

func TestConfiguredOverviewRendersLiveModeAndTrackerScope(t *testing.T) {
	t.Parallel()
	server, _, _ := configuredHTTPApp(t, validGitHubSource, secrets.Status{})
	cookie := exchange(t, server)
	response := authenticatedRequest(t, server, cookie, http.MethodGet, "/", nil, nil)
	body := readResponse(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET overview = %d", response.StatusCode)
	}
	for _, want := range []string{"github:coryj627/symphony", "Configure", "No scheduler is running"} {
		assertContains(t, body, want)
	}
}

func TestInvalidSaveReturns422LinkedSummaryRetainedRawAndNoMutation(t *testing.T) {
	t.Parallel()
	server, path, _ := configuredHTTPApp(t, validGitHubSource, secrets.Status{})
	cookie := exchange(t, server)
	csrf := csrfForCookie(t, server, cookie)
	before := mustReadFile(t, path)
	invalid := "---\ntracker: []\n---\nRetained operator input"
	response := postForm(t, server, cookie, "/api/v1/config/save", url.Values{
		"csrf_token": {csrf}, "mode": {"raw"}, "base_digest": {digestOf(before)}, "raw_source": {invalid}, "submit_action": {"save-raw"},
	})
	body := readResponse(t, response)
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid save = %d, body %s", response.StatusCode, body)
	}
	assertContainsOnce(t, body, `id="error-summary" role="alert" tabindex="-1"`)
	assertContains(t, body, `href="#raw-source"`)
	assertContains(t, body, invalid)
	assertContains(t, body, `data-focus-target="error-summary"`)
	if got := mustReadFile(t, path); got != before {
		t.Fatal("invalid save mutated workflow")
	}
}

func TestInvalidStructuredSaveRetainsSubmittedValuesWhenTrackerKindIsBlank(t *testing.T) {
	t.Parallel()
	server, _, _ := configuredHTTPApp(t, validGitHubSource, secrets.Status{})
	cookie := exchange(t, server)
	csrf := csrfForCookie(t, server, cookie)
	response := postForm(t, server, cookie, "/api/v1/config/save", completeStructuredForm(csrf, digestOf(validGitHubSource), map[string]string{
		"tracker_kind": "", "workspace_root": "/tmp/operator-retained",
	}))
	body := readResponse(t, response)
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid structured save = %d, body %s", response.StatusCode, body)
	}
	assertContains(t, body, `value="/tmp/operator-retained"`)
}

func TestStructuredAndRawSavePayloadsAreIsolated(t *testing.T) {
	t.Parallel()
	t.Run("structured rejects raw payload", func(t *testing.T) {
		server, path, _ := configuredHTTPApp(t, validGitHubSource, secrets.Status{})
		cookie := exchange(t, server)
		csrf := csrfForCookie(t, server, cookie)
		before := mustReadFile(t, path)
		response := postForm(t, server, cookie, "/api/v1/config/save", url.Values{
			"csrf_token": {csrf}, "mode": {"structured"}, "base_digest": {digestOf(before)},
			"tracker_kind": {"github"}, "raw_source": {validLinearSource}, "submit_action": {"save-structured"},
		})
		if response.StatusCode != http.StatusUnprocessableEntity || mustReadFile(t, path) != before {
			t.Fatalf("cross-form structured response = %d or mutated file", response.StatusCode)
		}
	})

	t.Run("raw rejects structured payload", func(t *testing.T) {
		server, path, _ := configuredHTTPApp(t, validGitHubSource, secrets.Status{})
		cookie := exchange(t, server)
		csrf := csrfForCookie(t, server, cookie)
		before := mustReadFile(t, path)
		response := postForm(t, server, cookie, "/api/v1/config/save", url.Values{
			"csrf_token": {csrf}, "mode": {"raw"}, "base_digest": {digestOf(before)},
			"raw_source": {validLinearSource}, "tracker_kind": {"linear"}, "submit_action": {"save-raw"},
		})
		if response.StatusCode != http.StatusUnprocessableEntity || mustReadFile(t, path) != before {
			t.Fatalf("cross-form raw response = %d or mutated file", response.StatusCode)
		}
	})
}

func TestStructuredSaveMapsKnownFieldsPreservesUnknownAndUsesPRG(t *testing.T) {
	t.Parallel()
	source := strings.Replace(validGitHubSource, "tracker:\n", "extension_root: keep-me\ntracker:\n", 1)
	server, path, _ := configuredHTTPApp(t, source, secrets.Status{})
	cookie := exchange(t, server)
	csrf := csrfForCookie(t, server, cookie)
	response := postForm(t, server, cookie, "/api/v1/config/save", completeStructuredForm(csrf, digestOf(source), map[string]string{
		"provider_owner": "openai", "provider_repository": "symphony-next", "server_port": "0",
	}))
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/configuration?focus=save-structured&result=configuration-saved-restart" {
		t.Fatalf("structured save = %d redirect %q", response.StatusCode, response.Header.Get("Location"))
	}
	got := mustReadFile(t, path)
	for _, want := range []string{"extension_root: keep-me", "owner: openai", "repository: symphony-next", "Work on {{ issue.identifier }}."} {
		assertContains(t, got, want)
	}
}

func TestRawSavePreservesExactBytesAndUsesPRG(t *testing.T) {
	t.Parallel()
	server, path, _ := configuredHTTPApp(t, validGitHubSource, secrets.Status{})
	cookie := exchange(t, server)
	csrf := csrfForCookie(t, server, cookie)
	raw := strings.Replace(validGitHubSource, "port: 43127", "port: 0", 1) + "\n"
	response := postForm(t, server, cookie, "/api/v1/config/save", url.Values{
		"csrf_token": {csrf}, "mode": {"raw"}, "base_digest": {digestOf(validGitHubSource)}, "raw_source": {raw}, "submit_action": {"save-raw"},
	})
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/configuration?focus=save-raw&result=configuration-saved-restart" {
		t.Fatalf("raw save = %d redirect %q", response.StatusCode, response.Header.Get("Location"))
	}
	if got := mustReadFile(t, path); got != raw {
		t.Fatalf("raw bytes changed: %q", got)
	}
}

func TestSuccessfulValidationRetainsUnsavedRawSourceWithoutMutation(t *testing.T) {
	t.Parallel()
	server, path, _ := configuredHTTPApp(t, validGitHubSource, secrets.Status{})
	cookie := exchange(t, server)
	csrf := csrfForCookie(t, server, cookie)
	unsaved := strings.Replace(validGitHubSource, "repository: symphony", "repository: validated-unsaved", 1)
	response := postForm(t, server, cookie, "/api/v1/config/validate", url.Values{
		"csrf_token": {csrf}, "mode": {"raw"}, "base_digest": {digestOf(validGitHubSource)},
		"raw_source": {unsaved}, "submit_action": {"validate-raw"},
	})
	body := readResponse(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("validate = %d, body %s", response.StatusCode, body)
	}
	assertContains(t, body, "validated-unsaved")
	if got := mustReadFile(t, path); got != validGitHubSource {
		t.Fatal("successful validation mutated workflow")
	}
}

func TestSaveConflictReturns409FreshSourceAndUnsavedValues(t *testing.T) {
	t.Parallel()
	server, path, _ := configuredHTTPApp(t, validGitHubSource, secrets.Status{})
	cookie := exchange(t, server)
	csrf := csrfForCookie(t, server, cookie)
	external := strings.Replace(validGitHubSource, "repository: symphony", "repository: externally-edited", 1)
	if err := os.WriteFile(path, []byte(external), 0o600); err != nil {
		t.Fatal(err)
	}
	unsaved := strings.Replace(validGitHubSource, "repository: symphony", "repository: operator-unsaved", 1)
	response := postForm(t, server, cookie, "/api/v1/config/save", url.Values{
		"csrf_token": {csrf}, "mode": {"raw"}, "base_digest": {digestOf(validGitHubSource)}, "raw_source": {unsaved}, "submit_action": {"save-raw"},
	})
	body := readResponse(t, response)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("conflict = %d", response.StatusCode)
	}
	for _, want := range []string{"externally-edited", "operator-unsaved", `href="#raw-source"`, `id="error-summary"`} {
		assertContains(t, body, want)
	}
	if mustReadFile(t, path) != external {
		t.Fatal("conflict overwrote external edit")
	}
}

func TestCredentialReplaceDeleteAndNamedConfirmationNeverExposeValue(t *testing.T) {
	t.Parallel()
	server, _, vault := configuredHTTPApp(t, validGitHubSource, secrets.Status{})
	cookie := exchange(t, server)
	csrf := csrfForCookie(t, server, cookie)
	canary := "credential-http-canary"
	replaceValues := credentialForm(csrf, "github", digestOf(validGitHubSource))
	replaceValues.Set("credential", canary)
	replace := postForm(t, server, cookie, "/api/v1/config/credential", replaceValues)
	if replace.StatusCode != http.StatusSeeOther || strings.Contains(replace.Header.Get("Location"), canary) {
		t.Fatalf("replace response = %d/%q", replace.StatusCode, replace.Header.Get("Location"))
	}
	if string(vault.value) != canary {
		t.Fatal("vault did not receive credential")
	}

	confirmValues := credentialForm(csrf, "github", digestOf(validGitHubSource))
	confirmValues.Set("request_delete", "1")
	confirm := postForm(t, server, cookie, "/api/v1/config/credential/delete", confirmValues)
	body := readResponse(t, confirm)
	if confirm.StatusCode != http.StatusOK || vault.deleteCalls != 0 {
		t.Fatalf("confirmation response = %d or deleted early", confirm.StatusCode)
	}
	for _, want := range []string{`<dialog id="credential-delete-dialog"`, `aria-labelledby="credential-delete-title"`, `aria-describedby="credential-delete-description"`, `open`, `value="Delete credential"`} {
		assertContains(t, body, want)
	}
	if strings.Contains(body, canary) {
		t.Fatal("credential leaked in confirmation body")
	}

	deleteValues := credentialForm(csrf, "github", digestOf(validGitHubSource))
	deleteValues.Set("confirm_delete", "Delete credential")
	deleted := postForm(t, server, cookie, "/api/v1/config/credential/delete", deleteValues)
	if deleted.StatusCode != http.StatusSeeOther || vault.deleteCalls != 1 {
		t.Fatalf("confirmed delete = %d, calls %d", deleted.StatusCode, vault.deleteCalls)
	}
}

func TestCredentialMutationsRejectDisplayedBindingAfterProviderSwitch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		initial       string
		external      string
		kind          string
		path          string
		confirmation  bool
		finalDeletion bool
	}{
		{name: "replace GitHub page after Linear switch", initial: validGitHubSource, external: validLinearSource, kind: "github", path: "/api/v1/config/credential"},
		{name: "replace Linear page after GitHub switch", initial: validLinearSource, external: validGitHubSource, kind: "linear", path: "/api/v1/config/credential"},
		{name: "initial delete GitHub page after Linear switch", initial: validGitHubSource, external: validLinearSource, kind: "github", path: "/api/v1/config/credential/delete", confirmation: true},
		{name: "initial delete Linear page after GitHub switch", initial: validLinearSource, external: validGitHubSource, kind: "linear", path: "/api/v1/config/credential/delete", confirmation: true},
		{name: "confirmed delete after GitHub to Linear switch", initial: validGitHubSource, external: validLinearSource, kind: "github", path: "/api/v1/config/credential/delete", finalDeletion: true},
		{name: "confirmed delete after Linear to GitHub switch", initial: validLinearSource, external: validGitHubSource, kind: "linear", path: "/api/v1/config/credential/delete", finalDeletion: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, path, vault := configuredHTTPApp(t, test.initial, secrets.Status{Present: true})
			cookie := exchange(t, server)
			csrf := csrfForCookie(t, server, cookie)
			binding := credentialForm(csrf, test.kind, digestOf(test.initial))
			if test.finalDeletion {
				binding.Set("request_delete", "1")
				response := postForm(t, server, cookie, test.path, binding)
				if response.StatusCode != http.StatusOK {
					t.Fatalf("open confirmation = %d", response.StatusCode)
				}
				binding.Del("request_delete")
				binding.Set("confirm_delete", "Delete credential")
			} else if test.confirmation {
				binding.Set("request_delete", "1")
			} else {
				binding.Set("credential", "must-not-be-stored")
			}
			if err := os.WriteFile(path, []byte(test.external), 0o600); err != nil {
				t.Fatal(err)
			}
			response := postForm(t, server, cookie, test.path, binding)
			if response.StatusCode != http.StatusConflict {
				t.Fatalf("stale credential mutation = %d, want 409", response.StatusCode)
			}
			if vault.putCalls != 0 || vault.deleteCalls != 0 {
				t.Fatalf("stale credential mutation touched vault: put %d delete %d", vault.putCalls, vault.deleteCalls)
			}
		})
	}
}

func TestCredentialReplaceRejectsMalformedBindingBeforeVault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(url.Values)
	}{
		{name: "absent tracker kind", mutate: func(values url.Values) { values.Del("credential_tracker_kind") }},
		{name: "absent digest", mutate: func(values url.Values) { values.Del("credential_base_digest") }},
		{name: "duplicate tracker kind", mutate: func(values url.Values) { values.Add("credential_tracker_kind", "linear") }},
		{name: "duplicate digest", mutate: func(values url.Values) { values.Add("credential_base_digest", "duplicate-digest") }},
		{name: "blank tracker kind", mutate: func(values url.Values) { values.Set("credential_tracker_kind", "") }},
		{name: "blank digest", mutate: func(values url.Values) { values.Set("credential_base_digest", "") }},
		{name: "unexpected binding field", mutate: func(values url.Values) { values.Set("credential_binding_extra", "binding-field-canary") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, path, vault := configuredHTTPApp(t, validGitHubSource, secrets.Status{})
			cookie := exchange(t, server)
			values := credentialForm(csrfForCookie(t, server, cookie), "github", digestOf(validGitHubSource))
			values.Set("credential", "malformed-credential-canary")
			test.mutate(values)
			response := postForm(t, server, cookie, "/api/v1/config/credential", values)
			body := readResponse(t, response)
			if response.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("malformed binding = %d, want 422", response.StatusCode)
			}
			if vault.putCalls != 0 || vault.deleteCalls != 0 {
				t.Fatalf("malformed binding touched vault: put %d delete %d", vault.putCalls, vault.deleteCalls)
			}
			if mustReadFile(t, path) != validGitHubSource {
				t.Fatal("malformed binding mutated workflow")
			}
			if strings.Contains(body, "malformed-credential-canary") || strings.Contains(body, "binding-field-canary") {
				t.Fatal("malformed binding response disclosed submitted values")
			}
			assertContains(t, body, "Reload the page before entering a credential.")
		})
	}
}

func TestCredentialDeleteFailureKeepsLinkedSummaryInsideConfirmation(t *testing.T) {
	t.Parallel()
	server, _, vault := configuredHTTPApp(t, validGitHubSource, secrets.Status{Present: true})
	vault.deleteErr = errors.New("delete canary must not be rendered")
	cookie := exchange(t, server)
	csrf := csrfForCookie(t, server, cookie)
	values := credentialForm(csrf, "github", digestOf(validGitHubSource))
	values.Set("confirm_delete", "Delete credential")
	response := postForm(t, server, cookie, "/api/v1/config/credential/delete", values)
	body := readResponse(t, response)
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("delete failure = %d, body %s", response.StatusCode, body)
	}
	dialogStart := strings.Index(body, `<dialog id="credential-delete-dialog"`)
	summaryStart := strings.Index(body, `id="error-summary"`)
	dialogEnd := strings.Index(body[dialogStart:], `</dialog>`)
	if dialogStart < 0 || summaryStart < dialogStart || dialogEnd < 0 || summaryStart > dialogStart+dialogEnd {
		t.Fatal("delete error summary is not inside the open confirmation dialog")
	}
	assertContainsOnce(t, body, `id="error-summary"`)
	assertContains(t, body, `href="#credential-delete-confirm"`)
	assertContains(t, body, `data-focus-target="error-summary"`)
	assertContains(t, body, `id="credential-delete-cancel" href="/configuration#delete-credential"`)
	if strings.Contains(body, "delete canary") {
		t.Fatal("delete cause leaked into response")
	}
}

func TestMutationSecurityFailuresCauseZeroConfigurationMutation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		method  string
		path    string
		body    io.Reader
		headers http.Header
		cookie  bool
		status  int
	}{
		{name: "missing session", method: http.MethodPost, path: "/api/v1/config/credential", body: strings.NewReader("credential=x"), headers: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, status: http.StatusUnauthorized},
		{name: "wrong csrf", method: http.MethodPost, path: "/api/v1/config/credential", body: strings.NewReader("csrf_token=wrong&credential=x"), headers: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, cookie: true, status: http.StatusForbidden},
		{name: "wrong origin", method: http.MethodPost, path: "/api/v1/config/credential", body: strings.NewReader("csrf_token=placeholder&credential=x"), headers: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Origin": {"http://example.invalid"}}, cookie: true, status: http.StatusForbidden},
		{name: "unsupported type", method: http.MethodPost, path: "/api/v1/config/credential", body: strings.NewReader("{}"), headers: http.Header{"Content-Type": {"text/plain"}}, cookie: true, status: http.StatusUnsupportedMediaType},
		{name: "unsupported method", method: http.MethodPut, path: "/api/v1/config/credential", cookie: true, status: http.StatusMethodNotAllowed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, path, vault := configuredHTTPApp(t, validGitHubSource, secrets.Status{})
			cookie := exchange(t, server)
			before := mustReadFile(t, path)
			var response *http.Response
			if test.cookie {
				if test.name == "wrong origin" {
					test.body = strings.NewReader("csrf_token=" + url.QueryEscape(csrfForCookie(t, server, cookie)) + "&credential=x")
				}
				response = authenticatedRequest(t, server, cookie, test.method, test.path, test.body, test.headers)
			} else {
				base := strings.Split(server.bound.URL, "?")[0]
				response = request(t, server.client, test.method, base+strings.TrimPrefix(test.path, "/"), test.body, test.headers)
			}
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.status)
			}
			if vault.putCalls != 0 || vault.deleteCalls != 0 || mustReadFile(t, path) != before {
				t.Fatal("rejected request mutated configuration state")
			}
		})
	}
}

func TestConfigurationAPIRoutesRejectAuthenticatedGETAsMethodNotAllowed(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"/api/v1/config/validate",
		"/api/v1/config/save",
		"/api/v1/config/credential",
		"/api/v1/config/credential/delete",
	} {
		t.Run(path, func(t *testing.T) {
			server, _, vault := configuredHTTPApp(t, validGitHubSource, secrets.Status{})
			cookie := exchange(t, server)
			response := authenticatedRequest(t, server, cookie, http.MethodGet, path, nil, nil)
			if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != "POST" {
				t.Fatalf("GET %s = %d Allow %q", path, response.StatusCode, response.Header.Get("Allow"))
			}
			if vault.putCalls != 0 || vault.deleteCalls != 0 {
				t.Fatal("GET API request mutated credentials")
			}
		})
	}
}

const validGitHubSource = `---
tracker:
  kind: github
  provider:
    owner: coryj627
    repository: symphony
    credential_ref: os-vault
  required_labels: [symphony]
  active_states: [open]
  terminal_states: [closed]
workspace:
  root: .symphony/workspaces
server:
  port: 43127
---
Work on {{ issue.identifier }}.
`

const validLinearSource = `---
tracker:
  kind: linear
  provider:
    project_slug: symphony-project
    credential_ref: os-vault
  required_labels: [symphony]
  active_states: [Todo]
  terminal_states: [Done]
---
Work on {{ issue.identifier }}.
`

func configuredHTTPApp(t *testing.T, source string, status secrets.Status) (*runningTestServer, string, *httpVault) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := workflow.NewStore(ctx, path, func(string) (string, bool) { return "", false }, app.ValidateTracker)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	vault := &httpVault{status: status}
	service := app.NewConfigService(app.ConfigServiceOptions{
		Path: path, Store: store, Vault: vault, WorkflowID: "stable-workflow-id", Platform: "darwin", RequestedPort: 43127,
	})
	handler, err := NewConfiguredPageHandler(service, "configure")
	if err != nil {
		t.Fatal(err)
	}
	return startTestServerWithErrorResponder(t, bootstrapFromValue("configured-http-capability"), handler, handler), path, vault
}

func postForm(t *testing.T, server *runningTestServer, cookie *http.Cookie, path string, values url.Values) *http.Response {
	t.Helper()
	return authenticatedRequest(t, server, cookie, http.MethodPost, path, strings.NewReader(values.Encode()), http.Header{
		"Content-Type": {"application/x-www-form-urlencoded"},
	})
}

func csrfForCookie(t *testing.T, server *runningTestServer, cookie *http.Cookie) string {
	t.Helper()
	session, ok := server.server.sessions.authenticate(cookie.Value)
	if !ok {
		t.Fatal("test session unavailable")
	}
	return session.csrf
}

func readResponse(t *testing.T, response *http.Response) string {
	t.Helper()
	contents, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func digestOf(source string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(source)))
}

func completeStructuredForm(csrf, digest string, overrides map[string]string) url.Values {
	values := url.Values{
		"csrf_token": {csrf}, "mode": {"structured"}, "base_digest": {digest}, "submit_action": {"save-structured"},
		"tracker_kind": {"github"}, "provider_owner": {"coryj627"}, "provider_repository": {"symphony"},
		"provider_project_slug": {""}, "provider_endpoint": {""}, "provider_credential_ref": {"os-vault"}, "provider_assignee": {""},
		"tracker_required_labels": {"symphony"}, "tracker_active_states": {"open"}, "tracker_terminal_states": {"closed"},
		"polling_interval_ms": {"30000"}, "workspace_root": {".symphony/workspaces"},
		"hook_after_create": {""}, "hook_before_run": {""}, "hook_after_run": {""}, "hook_before_remove": {""}, "hook_timeout_ms": {"60000"},
		"agent_max_concurrent": {"10"}, "agent_max_turns": {"20"}, "agent_max_retry_backoff_ms": {"300000"},
		"codex_command": {"codex app-server"}, "codex_approval_policy": {""}, "codex_thread_sandbox": {""},
		"codex_turn_timeout_ms": {"3600000"}, "codex_read_timeout_ms": {"5000"}, "codex_stall_timeout_ms": {"300000"},
		"server_port": {"43127"}, "server_operator_response_timeout_ms": {"600000"},
	}
	for key, value := range overrides {
		values.Set(key, value)
	}
	return values
}

func credentialForm(csrf, trackerKind, digest string) url.Values {
	return url.Values{
		"csrf_token": {csrf}, "credential_tracker_kind": {trackerKind}, "credential_base_digest": {digest},
		"submit_action": {"replace-credential"},
	}
}

type httpVault struct {
	status      secrets.Status
	value       []byte
	putCalls    int
	deleteCalls int
	deleteErr   error
}

func (vault *httpVault) Put(_ context.Context, _ secrets.Ref, value []byte) error {
	vault.value = append([]byte(nil), value...)
	vault.putCalls++
	return nil
}
func (*httpVault) Get(context.Context, secrets.Ref) ([]byte, error) { panic("Get must not be called") }
func (vault *httpVault) Delete(context.Context, secrets.Ref) error {
	vault.deleteCalls++
	if vault.deleteErr != nil {
		return vault.deleteErr
	}
	vault.value = nil
	return nil
}
func (vault *httpVault) Status(context.Context, secrets.Ref) secrets.Status {
	if vault.putCalls > 0 {
		return secrets.Status{Present: true, Backend: "native-keyring"}
	}
	return vault.status
}
