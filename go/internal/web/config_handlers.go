package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/coryj627/symphony/go/internal/app"
	"github.com/coryj627/symphony/go/internal/workflow"
)

var configurationControlIDs = map[string]string{
	"tracker.kind":                        "tracker-kind",
	"tracker.provider":                    "tracker-kind",
	"tracker.provider.owner":              "github-owner",
	"tracker.provider.repository":         "github-repository",
	"tracker.provider.project_slug":       "linear-project-slug",
	"tracker.provider.endpoint":           "tracker-endpoint",
	"tracker.provider.credential_ref":     "credential-reference",
	"tracker.provider.assignee":           "github-assignee",
	"tracker.required_labels":             "tracker-required-labels",
	"tracker.active_states":               "tracker-active-states",
	"tracker.terminal_states":             "tracker-terminal-states",
	"polling.interval_ms":                 "polling-interval-ms",
	"workspace.root":                      "workspace-root",
	"hooks.after_create":                  "hook-after-create",
	"hooks.before_run":                    "hook-before-run",
	"hooks.after_run":                     "hook-after-run",
	"hooks.before_remove":                 "hook-before-remove",
	"hooks.timeout_ms":                    "hook-timeout-ms",
	"agent.max_concurrent_agents":         "agent-max-concurrent",
	"agent.max_turns":                     "agent-max-turns",
	"agent.max_retry_backoff_ms":          "agent-max-retry-backoff-ms",
	"codex.command":                       "codex-command",
	"codex.approval_policy":               "codex-approval-policy",
	"codex.thread_sandbox":                "codex-thread-sandbox",
	"codex.turn_timeout_ms":               "codex-turn-timeout-ms",
	"codex.read_timeout_ms":               "codex-read-timeout-ms",
	"codex.stall_timeout_ms":              "codex-stall-timeout-ms",
	"server.port":                         "server-port",
	"server.operator_response_timeout_ms": "server-operator-response-timeout-ms",
	"raw_source":                          "raw-source",
	"credential":                          "credential",
	"request_delete":                      "delete-credential",
	"confirm_delete":                      "credential-delete-confirm",
}

var structuredFormFields = map[string]string{
	"tracker_kind": "tracker.kind", "provider_owner": "tracker.provider.owner",
	"provider_repository": "tracker.provider.repository", "provider_project_slug": "tracker.provider.project_slug",
	"provider_endpoint": "tracker.provider.endpoint", "provider_credential_ref": "tracker.provider.credential_ref",
	"provider_assignee": "tracker.provider.assignee", "tracker_required_labels": "tracker.required_labels",
	"tracker_active_states": "tracker.active_states", "tracker_terminal_states": "tracker.terminal_states",
	"polling_interval_ms": "polling.interval_ms", "workspace_root": "workspace.root",
	"hook_after_create": "hooks.after_create", "hook_before_run": "hooks.before_run",
	"hook_after_run": "hooks.after_run", "hook_before_remove": "hooks.before_remove", "hook_timeout_ms": "hooks.timeout_ms",
	"agent_max_concurrent": "agent.max_concurrent_agents", "agent_max_turns": "agent.max_turns",
	"agent_max_retry_backoff_ms": "agent.max_retry_backoff_ms", "codex_command": "codex.command",
	"codex_approval_policy": "codex.approval_policy", "codex_thread_sandbox": "codex.thread_sandbox",
	"codex_turn_timeout_ms": "codex.turn_timeout_ms", "codex_read_timeout_ms": "codex.read_timeout_ms",
	"codex_stall_timeout_ms": "codex.stall_timeout_ms", "server_port": "server.port",
	"server_operator_response_timeout_ms": "server.operator_response_timeout_ms",
}

func registerConfigurationRoutes(handler *PageHandler, service *app.ConfigService, mode string) {
	handler.mux.HandleFunc("GET /configuration", handler.configurationGET(service, mode))
	routes := map[string]http.HandlerFunc{
		"/api/v1/config/validate":          handler.configurationValidate(service, mode),
		"/api/v1/config/save":              handler.configurationSave(service, mode),
		"/api/v1/config/credential":        handler.credentialReplace(service, mode),
		"/api/v1/config/credential/delete": handler.credentialDelete(service, mode),
	}
	for route, mutation := range routes {
		handler.mux.HandleFunc("POST "+route, mutation)
		handler.mux.HandleFunc(route, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Allow", http.MethodPost)
			handler.RespondError(w, http.StatusMethodNotAllowed)
		})
	}
}

func (handler *PageHandler) configurationGET(service *app.ConfigService, mode string) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		view, err := service.View(request.Context())
		if err != nil {
			handler.RespondError(w, http.StatusInternalServerError)
			return
		}
		page := configurationPage(request, mode, view)
		applyResultCode(&page, request.URL.Query())
		handler.renderConfiguration(request.Context(), w, http.StatusOK, page)
	}
}

func (handler *PageHandler) configurationValidate(service *app.ConfigService, mode string) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if !isFormRequest(request) {
			handler.RespondError(w, http.StatusUnsupportedMediaType)
			return
		}
		raw, ok := exactFormValue(request.PostForm, "raw_source")
		if !ok || hasUnexpectedFields(request.PostForm, map[string]bool{
			"csrf_token": true, "mode": true, "base_digest": true, "raw_source": true, "submit_action": true,
		}) {
			handler.renderSubmittedErrors(w, http.StatusUnprocessableEntity, request, service, mode, app.ConfigValues{}, raw, raw, []workflow.FieldError{{Field: "raw_source", Code: "invalid_payload", Message: "Submit only the complete workflow source for validation."}}, nil, false)
			return
		}
		validation := service.Validate(request.Context(), []byte(raw))
		if !validation.Valid {
			handler.renderSubmittedErrors(w, http.StatusUnprocessableEntity, request, service, mode, app.ConfigValues{}, raw, raw, validation.FieldErrors, validation.GlobalErrors, false)
			return
		}
		view, err := service.View(request.Context())
		if err != nil {
			handler.RespondError(w, http.StatusInternalServerError)
			return
		}
		page := configurationPage(request, mode, view)
		content := page.Content.(configurationContent)
		content.RawSource = raw
		page.Content = content
		page.Flash = "Workflow source is valid. No changes were saved."
		page.FocusTarget = normalizedFocus("validate-raw")
		handler.renderConfiguration(request.Context(), w, http.StatusOK, page)
	}
}

func (handler *PageHandler) configurationSave(service *app.ConfigService, mode string) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if !isFormRequest(request) {
			handler.RespondError(w, http.StatusUnsupportedMediaType)
			return
		}
		modeValue, modeOK := exactFormValue(request.PostForm, "mode")
		baseDigest, digestOK := exactFormValue(request.PostForm, "base_digest")
		if !modeOK || !digestOK {
			handler.renderSubmittedErrors(w, http.StatusUnprocessableEntity, request, service, mode, app.ConfigValues{}, "", "", nil, []workflow.SafeError{{Code: "invalid_payload", Message: "Choose one configuration editor and submit it again."}}, false)
			return
		}

		var command workflow.SaveCommand
		var values app.ConfigValues
		var rawSource string
		var fieldErrors []workflow.FieldError
		switch modeValue {
		case "structured":
			values, command.Patch, fieldErrors = structuredCommand(request.PostForm)
			command.BaseDigest = baseDigest
			if hasUnexpectedStructuredFields(request.PostForm) {
				fieldErrors = append(fieldErrors, workflow.FieldError{Field: "tracker.kind", Code: "invalid_payload", Message: "Structured settings cannot include complete workflow source fields."})
			}
		case "raw":
			rawSource, _ = exactFormValue(request.PostForm, "raw_source")
			if hasUnexpectedFields(request.PostForm, map[string]bool{"csrf_token": true, "mode": true, "base_digest": true, "raw_source": true, "submit_action": true}) {
				fieldErrors = append(fieldErrors, workflow.FieldError{Field: "raw_source", Code: "invalid_payload", Message: "Complete workflow saves cannot include structured settings."})
			}
			command.BaseDigest = baseDigest
			command.RawSource = []byte(rawSource)
		default:
			fieldErrors = append(fieldErrors, workflow.FieldError{Field: "raw_source", Code: "invalid_payload", Message: "Choose the structured or complete workflow editor."})
		}
		if len(fieldErrors) > 0 {
			handler.renderSubmittedErrors(w, http.StatusUnprocessableEntity, request, service, mode, values, rawSource, "", fieldErrors, nil, false)
			return
		}

		result, err := service.Save(request.Context(), command)
		if errors.Is(err, workflow.ErrSaveConflict) {
			fresh, viewErr := service.View(request.Context())
			if viewErr != nil {
				handler.RespondError(w, http.StatusInternalServerError)
				return
			}
			if modeValue == "structured" {
				rawSource = fresh.Source
			}
			handler.renderSubmittedErrors(w, http.StatusConflict, request, service, mode, values, rawSource, fresh.Source,
				[]workflow.FieldError{{Field: conflictField(modeValue), Code: "workflow_save_conflict", Message: "The workflow changed on disk. Review the current source and your unsaved values before trying again."}}, nil, false)
			return
		}
		var invalid *workflow.InvalidWorkflowError
		if errors.As(err, &invalid) {
			handler.renderSubmittedErrors(w, http.StatusUnprocessableEntity, request, service, mode, values, rawSource, "", invalid.Validation.FieldErrors, invalid.Validation.GlobalErrors, false)
			return
		}
		if err != nil {
			handler.renderSubmittedErrors(w, http.StatusUnprocessableEntity, request, service, mode, values, rawSource, "", nil,
				[]workflow.SafeError{{Code: "configuration_save_failed", Message: "Configuration could not be saved. The existing workflow was preserved."}}, false)
			return
		}
		resultCode := "configuration-saved"
		if result.RestartRequired {
			resultCode = "configuration-saved-restart"
		}
		if result.Warning.Code != "" {
			resultCode = "configuration-saved-warning"
		}
		focus := "save-raw"
		if modeValue == "structured" {
			focus = "save-structured"
		}
		redirectConfiguration(w, request, resultCode, focus)
	}
}

func (handler *PageHandler) credentialReplace(service *app.ConfigService, mode string) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if !isFormRequest(request) {
			handler.RespondError(w, http.StatusUnsupportedMediaType)
			return
		}
		credential, ok := exactFormValue(request.PostForm, "credential")
		binding, bindingOK := credentialBinding(request.PostForm)
		malformed := !ok || !bindingOK || hasUnexpectedFields(request.PostForm, map[string]bool{
			"csrf_token": true, "credential": true, "credential_tracker_kind": true, "credential_base_digest": true, "submit_action": true,
		})
		buffer := []byte(credential)
		request.PostForm.Del("credential")
		request.Form.Del("credential")
		if malformed {
			clear(buffer)
			credential = ""
			handler.renderSubmittedErrors(w, http.StatusUnprocessableEntity, request, service, mode, app.ConfigValues{}, "", "", []workflow.FieldError{{Field: "credential", Code: "invalid_payload", Message: "Reload the page before entering a credential."}}, nil, false)
			return
		}
		err := service.ReplaceCredential(request.Context(), binding, buffer)
		clear(buffer)
		credential = ""
		if err != nil {
			if errors.Is(err, app.ErrCredentialConflict) {
				handler.renderSubmittedErrors(w, http.StatusConflict, request, service, mode, app.ConfigValues{}, "", "", []workflow.FieldError{{Field: "credential", Code: "credential_conflict", Message: "The workflow changed on disk. Review the selected tracker before entering the credential again."}}, nil, false)
				return
			}
			message := "Credential could not be stored. Enter a new credential and try again."
			if errors.Is(err, app.ErrCredentialRequired) {
				message = "Enter a credential to store."
			}
			if errors.Is(err, app.ErrEnvironmentManagedCredential) {
				message = "This credential is environment managed and cannot be replaced here."
			}
			handler.renderSubmittedErrors(w, http.StatusUnprocessableEntity, request, service, mode, app.ConfigValues{}, "", "", []workflow.FieldError{{Field: "credential", Code: "credential_store_failed", Message: message}}, nil, false)
			return
		}
		redirectConfiguration(w, request, "credential-stored", "replace-credential")
	}
}

func (handler *PageHandler) credentialDelete(service *app.ConfigService, mode string) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if !isFormRequest(request) {
			handler.RespondError(w, http.StatusUnsupportedMediaType)
			return
		}
		request.PostForm.Del("credential")
		request.Form.Del("credential")
		binding, bindingOK := credentialBinding(request.PostForm)
		if !bindingOK || hasUnexpectedFields(request.PostForm, map[string]bool{
			"csrf_token": true, "credential_tracker_kind": true, "credential_base_digest": true, "submit_action": true,
			"request_delete": true, "confirm_delete": true,
		}) {
			handler.renderSubmittedErrors(w, http.StatusUnprocessableEntity, request, service, mode, app.ConfigValues{}, "", "", []workflow.FieldError{{Field: "request_delete", Code: "invalid_payload", Message: "Reload the page before managing this credential."}}, nil, false)
			return
		}
		confirmation, confirmed := exactFormValue(request.PostForm, "confirm_delete")
		if !confirmed || confirmation != "Delete credential" {
			view, err := service.CredentialView(request.Context(), binding)
			if err != nil {
				if errors.Is(err, app.ErrCredentialConflict) {
					handler.renderSubmittedErrors(w, http.StatusConflict, request, service, mode, app.ConfigValues{}, "", "", []workflow.FieldError{{Field: "request_delete", Code: "credential_conflict", Message: "The workflow changed on disk. Review the selected tracker before requesting deletion again."}}, nil, false)
					return
				}
				handler.RespondError(w, http.StatusInternalServerError)
				return
			}
			page := configurationPage(request, mode, view)
			content := page.Content.(configurationContent)
			content.DeleteConfirmation = true
			page.Content = content
			page.FocusTarget = "credential-delete-cancel"
			handler.renderConfiguration(request.Context(), w, http.StatusOK, page)
			return
		}
		if err := service.DeleteCredential(request.Context(), binding); err != nil {
			if errors.Is(err, app.ErrCredentialConflict) {
				handler.renderSubmittedErrors(w, http.StatusConflict, request, service, mode, app.ConfigValues{}, "", "", []workflow.FieldError{{Field: "request_delete", Code: "credential_conflict", Message: "The workflow changed on disk. Review the selected tracker and request deletion again."}}, nil, false)
				return
			}
			message := "Credential could not be deleted. No secret value was read or displayed."
			if errors.Is(err, app.ErrEnvironmentManagedCredential) {
				message = "This credential is environment managed and cannot be deleted here."
			}
			handler.renderSubmittedErrors(w, http.StatusUnprocessableEntity, request, service, mode, app.ConfigValues{}, "", "", []workflow.FieldError{{Field: "confirm_delete", Code: "credential_delete_failed", Message: message}}, nil, true)
			return
		}
		redirectConfiguration(w, request, "credential-deleted", "delete-credential")
	}
}

func configurationPage(request *http.Request, mode string, view app.ConfigView) Page {
	csrf, _ := CSRFToken(request.Context())
	status := "Configuration is ready."
	if view.FileState == app.FileMissing {
		status = "WORKFLOW.md is missing. Use the complete workflow editor to create it."
	} else if view.FileState == app.FileInvalid {
		status = "WORKFLOW.md is invalid. Use the complete workflow editor to repair it."
	}
	content := configurationContent{View: view, Values: view.Config, RawSource: view.Source, Errors: map[string]string{}}
	return Page{
		Title: "Configuration — Symphony", Route: "/configuration", Heading: "Configuration", Mode: mode,
		Status: status, CSRFToken: csrf, Content: content,
	}
}

func (handler *PageHandler) renderSubmittedErrors(w http.ResponseWriter, status int, request *http.Request, service *app.ConfigService, mode string, values app.ConfigValues, rawSource, currentSource string, fieldErrors []workflow.FieldError, globalErrors []workflow.SafeError, deleteConfirmation bool) {
	view, err := service.View(request.Context())
	if err != nil {
		handler.RespondError(w, http.StatusInternalServerError)
		return
	}
	page := configurationPage(request, mode, view)
	content := page.Content.(configurationContent)
	if values != (app.ConfigValues{}) {
		content.Values = values
	}
	if rawSource != "" || view.FileState != app.FileMissing {
		content.RawSource = rawSource
	}
	content.CurrentSource = currentSource
	content.DeleteConfirmation = deleteConfirmation
	content.Errors, page.ErrorSummary = pageErrors(fieldErrors, globalErrors)
	page.ErrorSummaryInDialog = deleteConfirmation && len(page.ErrorSummary) > 0
	page.Content = content
	page.FocusTarget = "error-summary"
	handler.renderConfiguration(request.Context(), w, status, page)
}

func (handler *PageHandler) renderConfiguration(ctx context.Context, w http.ResponseWriter, status int, page Page) {
	content := page.Content.(configurationContent)
	if content.View.FileState == app.FileValid && handler.configService != nil {
		state, err := handler.configService.CredentialStatus(ctx)
		if err == nil {
			content.Credential = state
		}
	}
	page.Content = content
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	if err := handler.renderer.Render(w, "configuration", page); err != nil && status == http.StatusOK {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

func pageErrors(fieldErrors []workflow.FieldError, globalErrors []workflow.SafeError) (map[string]string, []PageError) {
	errorsByControl := make(map[string]string)
	summary := make([]PageError, 0, len(fieldErrors)+len(globalErrors))
	for _, problem := range fieldErrors {
		control := configurationControlIDs[problem.Field]
		if control == "" {
			control = "raw-source"
		}
		message := strings.TrimSpace(problem.Message)
		if message == "" {
			message = "Review this setting."
		}
		if _, exists := errorsByControl[control]; !exists {
			errorsByControl[control] = message
			summary = append(summary, PageError{ControlID: control, Message: message})
		}
	}
	for _, problem := range globalErrors {
		message := strings.TrimSpace(problem.Message)
		if message == "" {
			message = "Review the complete workflow source."
		}
		if _, exists := errorsByControl["raw-source"]; !exists {
			errorsByControl["raw-source"] = message
			summary = append(summary, PageError{ControlID: "raw-source", Message: message})
		}
	}
	return errorsByControl, summary
}

func structuredCommand(form url.Values) (app.ConfigValues, *workflow.StructuredPatch, []workflow.FieldError) {
	values := app.ConfigValues{}
	get := func(name string) string { value, _ := exactFormValue(form, name); return value }
	values.TrackerKind = get("tracker_kind")
	values.ProviderOwner = get("provider_owner")
	values.ProviderRepository = get("provider_repository")
	values.ProviderProjectSlug = get("provider_project_slug")
	values.ProviderEndpoint = get("provider_endpoint")
	values.CredentialRef = get("provider_credential_ref")
	values.ProviderAssignee = get("provider_assignee")
	values.RequiredLabels = get("tracker_required_labels")
	values.ActiveStates = get("tracker_active_states")
	values.TerminalStates = get("tracker_terminal_states")
	values.PollingIntervalMS = get("polling_interval_ms")
	values.WorkspaceRoot = get("workspace_root")
	values.HookAfterCreate = get("hook_after_create")
	values.HookBeforeRun = get("hook_before_run")
	values.HookAfterRun = get("hook_after_run")
	values.HookBeforeRemove = get("hook_before_remove")
	values.HookTimeoutMS = get("hook_timeout_ms")
	values.AgentMaxConcurrent = get("agent_max_concurrent")
	values.AgentMaxTurns = get("agent_max_turns")
	values.AgentMaxRetryBackoffMS = get("agent_max_retry_backoff_ms")
	values.CodexCommand = get("codex_command")
	values.CodexApprovalPolicy = get("codex_approval_policy")
	values.CodexThreadSandbox = get("codex_thread_sandbox")
	values.CodexTurnTimeoutMS = get("codex_turn_timeout_ms")
	values.CodexReadTimeoutMS = get("codex_read_timeout_ms")
	values.CodexStallTimeoutMS = get("codex_stall_timeout_ms")
	values.ServerPort = get("server_port")
	values.ServerOperatorResponseTimeoutMS = get("server_operator_response_timeout_ms")

	var fieldErrors []workflow.FieldError
	parseInt := func(field, value string) *int {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			fieldErrors = append(fieldErrors, workflow.FieldError{Field: field, Code: "invalid_integer", Message: "Enter a whole number in the documented range."})
			return nil
		}
		return &parsed
	}
	if values.TrackerKind != "github" && values.TrackerKind != "linear" {
		fieldErrors = append(fieldErrors, workflow.FieldError{Field: "tracker.kind", Code: "invalid_tracker_config", Message: "Choose GitHub or Linear."})
	}
	approvalPolicy, err := parseApprovalPolicy(values.CodexApprovalPolicy)
	if err != nil {
		fieldErrors = append(fieldErrors, workflow.FieldError{Field: "codex.approval_policy", Code: "invalid_json", Message: "Enter a policy name or a valid JSON value."})
	}
	patch := &workflow.StructuredPatch{
		TrackerKind:           &values.TrackerKind,
		TrackerRequiredLabels: listPointer(values.RequiredLabels), TrackerActiveStates: listPointer(values.ActiveStates), TrackerTerminalStates: listPointer(values.TerminalStates),
		PollingIntervalMS: parseInt("polling.interval_ms", values.PollingIntervalMS), WorkspaceRoot: &values.WorkspaceRoot,
		HookAfterCreate: &values.HookAfterCreate, HookBeforeRun: &values.HookBeforeRun, HookAfterRun: &values.HookAfterRun, HookBeforeRemove: &values.HookBeforeRemove,
		HookTimeoutMS: parseInt("hooks.timeout_ms", values.HookTimeoutMS), AgentMaxConcurrent: parseInt("agent.max_concurrent_agents", values.AgentMaxConcurrent),
		AgentMaxTurns: parseInt("agent.max_turns", values.AgentMaxTurns), AgentMaxRetryBackoffMS: parseInt("agent.max_retry_backoff_ms", values.AgentMaxRetryBackoffMS),
		CodexCommand: &values.CodexCommand, CodexApprovalPolicy: approvalPolicy, CodexThreadSandbox: &values.CodexThreadSandbox,
		CodexTurnTimeoutMS: parseInt("codex.turn_timeout_ms", values.CodexTurnTimeoutMS), CodexReadTimeoutMS: parseInt("codex.read_timeout_ms", values.CodexReadTimeoutMS),
		CodexStallTimeoutMS: parseInt("codex.stall_timeout_ms", values.CodexStallTimeoutMS), ServerPort: parseInt("server.port", values.ServerPort),
		ServerOperatorResponseTimeoutMS: parseInt("server.operator_response_timeout_ms", values.ServerOperatorResponseTimeoutMS),
	}
	if values.TrackerKind == "github" {
		patch.ProviderOwner, patch.ProviderRepository, patch.ProviderEndpoint = &values.ProviderOwner, &values.ProviderRepository, &values.ProviderEndpoint
		patch.ProviderCredentialRef, patch.ProviderAssignee = &values.CredentialRef, &values.ProviderAssignee
	} else if values.TrackerKind == "linear" {
		patch.ProviderProjectSlug, patch.ProviderEndpoint = &values.ProviderProjectSlug, &values.ProviderEndpoint
		patch.ProviderCredentialRef = &values.CredentialRef
	}
	return values, patch, fieldErrors
}

func parseApprovalPolicy(input string) (*any, error) {
	trimmed := strings.TrimSpace(input)
	if json.Valid([]byte(trimmed)) {
		decoder := json.NewDecoder(strings.NewReader(trimmed))
		decoder.UseNumber()
		var decoded any
		if err := decoder.Decode(&decoded); err != nil {
			return nil, err
		}
		normalized, err := normalizeJSONValue(decoded)
		if err != nil {
			return nil, err
		}
		return &normalized, nil
	}
	if looksLikeJSON(trimmed) {
		return nil, errors.New("invalid_json_value")
	}
	value := any(input)
	return &value, nil
}

func looksLikeJSON(value string) bool {
	if value == "" {
		return false
	}
	switch value[0] {
	case '{', '[', '"', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return true
	default:
		return false
	}
}

func normalizeJSONValue(value any) (any, error) {
	switch value := value.(type) {
	case json.Number:
		if strings.ContainsAny(value.String(), ".eE") {
			return strconv.ParseFloat(value.String(), 64)
		}
		if integer, err := strconv.Atoi(value.String()); err == nil {
			return integer, nil
		}
		unsigned, err := strconv.ParseUint(value.String(), 10, 64)
		if err != nil {
			return nil, errors.New("json_integer_out_of_range")
		}
		return unsigned, nil
	case []any:
		for index, item := range value {
			normalized, err := normalizeJSONValue(item)
			if err != nil {
				return nil, err
			}
			value[index] = normalized
		}
		return value, nil
	case map[string]any:
		for key, item := range value {
			normalized, err := normalizeJSONValue(item)
			if err != nil {
				return nil, err
			}
			value[key] = normalized
		}
		return value, nil
	default:
		return value, nil
	}
}

func listPointer(value string) *[]string {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	items := make([]string, 0, len(lines))
	for _, line := range lines {
		if item := strings.TrimSpace(line); item != "" {
			items = append(items, item)
		}
	}
	return &items
}

func hasUnexpectedStructuredFields(form url.Values) bool {
	allowed := map[string]bool{"csrf_token": true, "mode": true, "base_digest": true, "submit_action": true}
	for field := range structuredFormFields {
		allowed[field] = true
	}
	return hasUnexpectedFields(form, allowed)
}

func hasUnexpectedFields(form url.Values, allowed map[string]bool) bool {
	for key, values := range form {
		if !allowed[key] || len(values) != 1 {
			return true
		}
	}
	return false
}

func exactFormValue(form url.Values, key string) (string, bool) {
	values, ok := form[key]
	return firstValue(values), ok && len(values) == 1
}

func credentialBinding(form url.Values) (app.CredentialBinding, bool) {
	trackerKind, trackerOK := exactFormValue(form, "credential_tracker_kind")
	baseDigest, digestOK := exactFormValue(form, "credential_base_digest")
	return app.CredentialBinding{TrackerKind: trackerKind, BaseDigest: baseDigest}, trackerOK && digestOK && trackerKind != "" && baseDigest != ""
}

func firstValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func isFormRequest(request *http.Request) bool {
	return strings.HasPrefix(strings.ToLower(request.Header.Get("Content-Type")), "application/x-www-form-urlencoded")
}

func conflictField(mode string) string {
	if mode == "raw" {
		return "raw_source"
	}
	return "tracker.kind"
}

func redirectConfiguration(w http.ResponseWriter, request *http.Request, result, focus string) {
	query := url.Values{"result": {result}, "focus": {focus}}
	http.Redirect(w, request, "/configuration?"+query.Encode(), http.StatusSeeOther)
}

func applyResultCode(page *Page, query url.Values) {
	switch query.Get("result") {
	case "configuration-saved":
		page.Flash = "Configuration saved."
	case "configuration-saved-restart":
		page.Flash = "Configuration saved. Restart Symphony to use the new server port."
	case "configuration-saved-warning":
		page.Flash = "Configuration saved, but filesystem durability could not be confirmed. Review the workflow before restarting."
		page.FlashKind = "warning"
	case "credential-stored":
		page.Flash = "Credential stored."
	case "credential-deleted":
		page.Flash = "Credential deleted."
	}
	page.FocusTarget = normalizedFocus(query.Get("focus"))
}

func normalizedFocus(value string) string {
	switch value {
	case "save-structured", "save-raw", "validate-raw", "replace-credential", "delete-credential":
		return value
	default:
		return ""
	}
}
