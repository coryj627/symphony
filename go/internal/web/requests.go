package web

import (
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/coryj627/symphony/go/internal/codex"
	"github.com/coryj627/symphony/go/internal/domain"
)

const maximumOperatorFormBytes = 256 << 10

func (handler *PageHandler) respondOperatorRequest(w http.ResponseWriter, request *http.Request) {
	if !operatorForm(request) {
		handler.writeAPIError(w, "unsupported_media_type")
		return
	}
	if request.PostForm == nil {
		request.Body = http.MaxBytesReader(w, request.Body, maximumOperatorFormBytes)
		if err := request.ParseForm(); err != nil {
			handler.writeOperatorFormError(w, request, "operator_request_invalid")
			return
		}
	}
	defer clearOperatorAnswerFields(request.PostForm)
	requestID := request.PathValue("request_id")
	sessionID, sessionOK := exactFormValue(request.PostForm, "session_id")
	choiceID, choiceOK := optionalExactFormValue(request.PostForm, "choice_id")
	returnTo, returnOK := exactFormValue(request.PostForm, "return_to")
	if !validOperatorRequestID(requestID) || !sessionOK || sessionID == "" || !choiceOK || !returnOK || !validOperatorReturnURL(returnTo) {
		handler.writeOperatorFormError(w, request, "operator_request_invalid")
		return
	}
	answers, ok := operatorAnswers(request.PostForm)
	if !ok {
		handler.writeOperatorFormError(w, request, "operator_request_invalid")
		return
	}
	response := domain.OperatorResponse{RequestID: requestID, SessionID: sessionID, ChoiceID: choiceID, Answers: answers}
	dependencies := handler.dependencies(request)
	err := dependencies.commands.Respond(request.Context(), response)
	if err != nil {
		handler.writeOperatorRequestError(w, request, err)
		return
	}
	handler.redirectOperatorResult(w, request, returnTo, "request-responded", dependencies.scenario)
}

func (handler *PageHandler) extendOperatorRequest(w http.ResponseWriter, request *http.Request) {
	if !operatorForm(request) {
		handler.writeAPIError(w, "unsupported_media_type")
		return
	}
	if request.PostForm == nil {
		request.Body = http.MaxBytesReader(w, request.Body, maximumOperatorFormBytes)
		if err := request.ParseForm(); err != nil {
			handler.writeOperatorFormError(w, request, "operator_request_invalid")
			return
		}
	}
	requestID := request.PathValue("request_id")
	returnTo, returnOK := exactFormValue(request.PostForm, "return_to")
	if !validOperatorRequestID(requestID) || !returnOK || !validOperatorReturnURL(returnTo) || hasUnexpectedFields(request.PostForm, map[string]bool{"csrf_token": true, "return_to": true}) {
		handler.writeOperatorFormError(w, request, "operator_request_invalid")
		return
	}
	dependencies := handler.dependencies(request)
	if err := dependencies.commands.ExtendOperatorRequest(request.Context(), requestID); err != nil {
		handler.writeOperatorRequestError(w, request, err)
		return
	}
	handler.redirectOperatorResult(w, request, returnTo, "request-extended", dependencies.scenario)
}

func (handler *PageHandler) writeOperatorRequestError(w http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, codex.ErrStaleRequest), errors.Is(err, codex.ErrUnknownRequest):
		handler.writeOperatorFormError(w, request, "operator_request_stale")
	case errors.Is(err, codex.ErrInvalidOperatorResponse), errors.Is(err, codex.ErrExtensionLimit):
		handler.writeOperatorFormError(w, request, "operator_request_invalid")
	default:
		handler.writeOperatorFormError(w, request, "operator_request_failed")
	}
}

func (handler *PageHandler) writeOperatorFormError(w http.ResponseWriter, request *http.Request, key string) {
	spec, ok := apiErrorSpecs[key]
	if !ok {
		spec = apiErrorSpecs["operator_request_failed"]
	}
	page := Page{
		Title:       "Operator request — Symphony",
		Heading:     "Operator request",
		Mode:        handler.mode,
		Status:      "The operator request was not changed.",
		FocusTarget: "error-summary",
		ErrorSummary: []PageError{{
			ControlID: "main-content",
			Message:   spec.message,
		}},
		Content: errorContent{Instruction: spec.message},
	}
	w.WriteHeader(spec.status)
	if err := handler.renderHTML(w, "error", page); err != nil {
		handler.logger.ErrorContext(request.Context(), "render operator request error", "error", err)
	}
}

func (handler *PageHandler) redirectOperatorResult(w http.ResponseWriter, request *http.Request, returnTo, result, scenario string) {
	parsed, _ := url.Parse(returnTo)
	values := parsed.Query()
	values.Set("result", result)
	values.Set("focus", "requests-heading")
	parsed.RawQuery = values.Encode()
	parsed.Fragment = "requests-heading"
	setSecurityHeaders(w.Header())
	http.Redirect(w, request, internalURL(parsed.String(), scenario), http.StatusSeeOther)
}

func operatorForm(request *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/x-www-form-urlencoded"
}

func operatorAnswers(form url.Values) (map[string][]string, bool) {
	answers := make(map[string][]string)
	allowed := map[string]bool{"csrf_token": true, "session_id": true, "choice_id": true, "return_to": true}
	for key, values := range form {
		if allowed[key] {
			if len(values) != 1 {
				return nil, false
			}
			continue
		}
		if !strings.HasPrefix(key, "answer.") || len(values) == 0 || len(values) > 32 {
			if strings.HasPrefix(key, "other.") && len(values) == 1 {
				continue
			}
			return nil, false
		}
		questionID := strings.TrimPrefix(key, "answer.")
		if !validQuestionID(questionID) {
			return nil, false
		}
		answers[questionID] = append([]string(nil), values...)
	}
	for questionID, values := range answers {
		for index, value := range values {
			if value != "__other__" {
				continue
			}
			other, ok := exactFormValue(form, "other."+questionID)
			if !ok || other == "" || !utf8.ValidString(other) || len(other) > 64<<10 {
				return nil, false
			}
			answers[questionID][index] = other
		}
	}
	for key, values := range form {
		if !strings.HasPrefix(key, "other.") {
			continue
		}
		questionID := strings.TrimPrefix(key, "other.")
		if !validQuestionID(questionID) || len(values) != 1 || !utf8.ValidString(values[0]) || len(values[0]) > 64<<10 {
			return nil, false
		}
		if _, selected := answers[questionID]; !selected && values[0] != "" {
			answers[questionID] = []string{values[0]}
		}
	}
	return answers, true
}

func clearOperatorAnswerFields(form url.Values) {
	for key := range form {
		if strings.HasPrefix(key, "answer.") || strings.HasPrefix(key, "other.") {
			form.Del(key)
		}
	}
}

func optionalExactFormValue(form url.Values, key string) (string, bool) {
	values, exists := form[key]
	if !exists {
		return "", true
	}
	return firstValue(values), len(values) == 1
}

func validOperatorRequestID(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("-._:", character) {
			continue
		}
		return false
	}
	return true
}

func validQuestionID(value string) bool { return validOperatorRequestID(value) }

func validOperatorReturnURL(value string) bool {
	if value == "/" || value == "/issues" {
		return true
	}
	if !strings.HasPrefix(value, "/issues/") || strings.ContainsAny(value, "?#\\") {
		return false
	}
	identifier, err := url.PathUnescape(strings.TrimPrefix(value, "/issues/"))
	return err == nil && validIssueIdentifier(identifier)
}

func canonicalOperatorRequestAction(request *http.Request) bool {
	escaped := request.URL.EscapedPath()
	const prefix = "/api/v1/requests/"
	if !strings.HasPrefix(escaped, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(escaped, prefix), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "respond" && parts[1] != "extend" {
		return false
	}
	id, err := url.PathUnescape(parts[0])
	return err == nil && validOperatorRequestID(id) && url.PathEscape(id) == parts[0]
}
