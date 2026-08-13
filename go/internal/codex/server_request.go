package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/coryj627/symphony/go/internal/domain"
)

const (
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

var (
	ErrMalformedServerRequest   = errors.New("codex_malformed_server_request")
	ErrUnsupportedServerRequest = errors.New("codex_unsupported_server_request")
	ErrInvalidOperatorResponse  = errors.New("codex_invalid_operator_response")
)

type protocolResponder func(RequestID, any) error
type protocolRejecter func(RequestID, int64, string) error

// ServerRequestContext binds one protocol request to its immutable run identity.
type ServerRequestContext struct {
	Request         ServerRequest
	SessionID       string
	IssueID         string
	IssueIdentifier string
	Respond         protocolResponder
	Reject          protocolRejecter
}

type mappedServerRequest struct {
	request        domain.OperatorRequest
	response       func(domain.OperatorResponse) (any, [][]byte, error)
	cancelResponse any
}

type requestIdentity struct {
	ItemID   string `json:"itemId"`
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

type commandApprovalParams struct {
	requestIdentity
	StartedAtMS           *int64            `json:"startedAtMs"`
	Command               *string           `json:"command"`
	Cwd                   *string           `json:"cwd"`
	Reason                *string           `json:"reason"`
	AdditionalPermissions json.RawMessage   `json:"additionalPermissions"`
	AvailableDecisions    []json.RawMessage `json:"availableDecisions"`
}

type fileApprovalParams struct {
	requestIdentity
	StartedAtMS *int64  `json:"startedAtMs"`
	Reason      *string `json:"reason"`
	GrantRoot   *string `json:"grantRoot"`
}

type permissionApprovalParams struct {
	requestIdentity
	StartedAtMS *int64          `json:"startedAtMs"`
	Cwd         string          `json:"cwd"`
	Reason      *string         `json:"reason"`
	Permissions json.RawMessage `json:"permissions"`
}

type userInputParams struct {
	requestIdentity
	Questions []userInputQuestion `json:"questions"`
}

type userInputQuestion struct {
	ID       string            `json:"id"`
	Header   string            `json:"header"`
	Question string            `json:"question"`
	IsOther  bool              `json:"isOther"`
	IsSecret bool              `json:"isSecret"`
	Options  []userInputOption `json:"options"`
}

type userInputOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type answerRule struct {
	choices     map[string]string
	allowsOther bool
	secret      bool
}

func mapServerRequest(context ServerRequestContext) (mappedServerRequest, error) {
	if context.Request.ID.Token() == "" || context.Respond == nil || context.Reject == nil ||
		context.SessionID == "" || context.IssueID == "" || context.IssueIdentifier == "" {
		return mappedServerRequest{}, ErrMalformedServerRequest
	}
	base := domain.OperatorRequest{
		SessionID: context.SessionID, IssueID: boundedRequestText(context.IssueID, 512),
		IssueIdentifier: boundedRequestText(context.IssueIdentifier, 512),
	}
	switch context.Request.Method {
	case "item/commandExecution/requestApproval":
		return mapCommandApproval(context, base)
	case "item/fileChange/requestApproval":
		return mapFileApproval(context, base)
	case "item/permissions/requestApproval":
		return mapPermissionApproval(context, base)
	case "item/tool/requestUserInput":
		return mapUserInput(context, base)
	default:
		return mappedServerRequest{}, ErrUnsupportedServerRequest
	}
}

func mapCommandApproval(context ServerRequestContext, base domain.OperatorRequest) (mappedServerRequest, error) {
	var params commandApprovalParams
	if err := validatePinnedRequestParams("CommandExecutionRequestApprovalParams.json", context.Request.Params); err != nil {
		return mappedServerRequest{}, ErrMalformedServerRequest
	}
	if err := decodeRequestParams(context.Request.Params, &params); err != nil || !validRequestIdentity(params.requestIdentity, context.SessionID) || params.StartedAtMS == nil {
		return mappedServerRequest{}, ErrMalformedServerRequest
	}
	base.Kind = "command_approval"
	base.Title = "Approve command execution"
	base.Summary = firstNonempty(params.Reason, params.Command, "Codex requested permission to run a command.")
	var detailOK bool
	if base.Details, detailOK = appendExactStringDetail(base.Details, "Command", params.Command, 8<<10); !detailOK {
		return mappedServerRequest{}, ErrMalformedServerRequest
	}
	if base.Details, detailOK = appendExactStringDetail(base.Details, "Working directory", params.Cwd, 8<<10); !detailOK {
		return mappedServerRequest{}, ErrMalformedServerRequest
	}
	if base.Details, detailOK = appendExactJSONDetail(base.Details, "Additional permission profile", params.AdditionalPermissions, 16<<10); !detailOK {
		return mappedServerRequest{}, ErrMalformedServerRequest
	}
	decisions, choices, err := commandDecisions(params.AvailableDecisions)
	if err != nil {
		return mappedServerRequest{}, ErrMalformedServerRequest
	}
	base.Choices = choices
	return mappedServerRequest{
		request: base, cancelResponse: map[string]any{"decision": "cancel"},
		response: func(response domain.OperatorResponse) (any, [][]byte, error) {
			decision, ok := decisions[response.ChoiceID]
			if !ok || len(response.Answers) != 0 {
				return nil, nil, ErrInvalidOperatorResponse
			}
			return map[string]any{"decision": cloneJSONValue(decision)}, nil, nil
		},
	}, nil
}

func mapFileApproval(context ServerRequestContext, base domain.OperatorRequest) (mappedServerRequest, error) {
	var params fileApprovalParams
	if err := validatePinnedRequestParams("FileChangeRequestApprovalParams.json", context.Request.Params); err != nil {
		return mappedServerRequest{}, ErrMalformedServerRequest
	}
	if err := decodeRequestParams(context.Request.Params, &params); err != nil || !validRequestIdentity(params.requestIdentity, context.SessionID) || params.StartedAtMS == nil {
		return mappedServerRequest{}, ErrMalformedServerRequest
	}
	base.Kind = "file_approval"
	base.Title = "Approve file changes"
	base.Summary = firstNonempty(params.Reason, params.GrantRoot, "Codex requested permission to change files.")
	var detailOK bool
	if base.Details, detailOK = appendExactStringDetail(base.Details, "Grant root", params.GrantRoot, 8<<10); !detailOK {
		return mappedServerRequest{}, ErrMalformedServerRequest
	}
	decisions := map[string]any{
		"accept": "accept", "accept_for_session": "acceptForSession", "decline": "decline", "cancel": "cancel",
	}
	base.Choices = simpleApprovalChoices(true)
	return mappedServerRequest{
		request: base, cancelResponse: map[string]any{"decision": "cancel"},
		response: func(response domain.OperatorResponse) (any, [][]byte, error) {
			decision, ok := decisions[response.ChoiceID]
			if !ok || len(response.Answers) != 0 {
				return nil, nil, ErrInvalidOperatorResponse
			}
			return map[string]any{"decision": decision}, nil, nil
		},
	}, nil
}

func mapPermissionApproval(context ServerRequestContext, base domain.OperatorRequest) (mappedServerRequest, error) {
	var params permissionApprovalParams
	if err := validatePinnedRequestParams("PermissionsRequestApprovalParams.json", context.Request.Params); err != nil {
		return mappedServerRequest{}, ErrMalformedServerRequest
	}
	if err := decodeRequestParams(context.Request.Params, &params); err != nil || !validRequestIdentity(params.requestIdentity, context.SessionID) || params.StartedAtMS == nil || params.Cwd == "" || len(params.Permissions) == 0 {
		return mappedServerRequest{}, ErrMalformedServerRequest
	}
	permissions, err := decodeJSONObjectValue(params.Permissions)
	if err != nil {
		return mappedServerRequest{}, ErrMalformedServerRequest
	}
	for key := range permissions {
		if key != "fileSystem" && key != "network" {
			return mappedServerRequest{}, ErrMalformedServerRequest
		}
	}
	base.Kind = "permission_approval"
	base.Title = "Approve additional permissions"
	base.Summary = firstNonempty(params.Reason, nil, "Codex requested additional sandbox permissions.")
	permissionProfile, ok := exactJSONValue(params.Permissions, 16<<10)
	if !ok || len(params.Cwd) > 8<<10 {
		return mappedServerRequest{}, ErrMalformedServerRequest
	}
	base.Details = append(base.Details,
		domain.OperatorDetail{Label: "Working directory", Value: params.Cwd},
		domain.OperatorDetail{Label: "Requested permission profile", Value: permissionProfile},
	)
	base.Choices = []domain.OperatorChoice{
		{ID: "grant_turn", Label: "Allow for this turn", Description: "Grant exactly the displayed permissions until this turn ends."},
		{ID: "grant_session", Label: "Allow for this session", Description: "Grant exactly the displayed permissions until this Codex session ends."},
		{ID: "decline", Label: "Deny", Description: "Do not grant the requested permissions."},
	}
	return mappedServerRequest{
		request: base, cancelResponse: map[string]any{"permissions": map[string]any{}, "scope": "turn"},
		response: func(response domain.OperatorResponse) (any, [][]byte, error) {
			if len(response.Answers) != 0 {
				return nil, nil, ErrInvalidOperatorResponse
			}
			switch response.ChoiceID {
			case "grant_turn":
				return map[string]any{"permissions": cloneJSONValue(permissions), "scope": "turn"}, nil, nil
			case "grant_session":
				return map[string]any{"permissions": cloneJSONValue(permissions), "scope": "session"}, nil, nil
			case "decline":
				return map[string]any{"permissions": map[string]any{}, "scope": "turn"}, nil, nil
			default:
				return nil, nil, ErrInvalidOperatorResponse
			}
		},
	}, nil
}

func mapUserInput(context ServerRequestContext, base domain.OperatorRequest) (mappedServerRequest, error) {
	var params userInputParams
	if err := validatePinnedRequestParams("ToolRequestUserInputParams.json", context.Request.Params); err != nil {
		return mappedServerRequest{}, ErrMalformedServerRequest
	}
	if err := decodeRequestParams(context.Request.Params, &params); err != nil || !validRequestIdentity(params.requestIdentity, context.SessionID) || len(params.Questions) == 0 || len(params.Questions) > 32 {
		return mappedServerRequest{}, ErrMalformedServerRequest
	}
	base.Kind = "user_input"
	base.Title = "Codex needs your input"
	base.Summary = "Answer the questions to continue this turn."
	rules := make(map[string]answerRule, len(params.Questions))
	for _, question := range params.Questions {
		if !validOperatorToken(question.ID) || question.Header == "" || question.Question == "" || len(question.Options) > 32 {
			return mappedServerRequest{}, ErrMalformedServerRequest
		}
		if _, duplicate := rules[question.ID]; duplicate {
			return mappedServerRequest{}, ErrMalformedServerRequest
		}
		domainQuestion := domain.OperatorQuestion{
			ID: question.ID, Label: boundedRequestText(question.Header, 512),
			Description: boundedRequestText(question.Question, 4<<10), Required: true,
			AllowsOther: question.IsOther || len(question.Options) == 0, IsSecret: question.IsSecret,
			Choices: []domain.OperatorChoice{},
		}
		rule := answerRule{choices: make(map[string]string, len(question.Options)), allowsOther: domainQuestion.AllowsOther, secret: question.IsSecret}
		for index, option := range question.Options {
			if option.Label == "" {
				return mappedServerRequest{}, ErrMalformedServerRequest
			}
			choiceID := fmt.Sprintf("option-%d", index+1)
			domainQuestion.Choices = append(domainQuestion.Choices, domain.OperatorChoice{
				ID: choiceID, Label: boundedRequestText(option.Label, 512), Description: boundedRequestText(option.Description, 2<<10),
			})
			rule.choices[choiceID] = option.Label
		}
		base.Questions = append(base.Questions, domainQuestion)
		rules[question.ID] = rule
	}
	return mappedServerRequest{
		request: base, cancelResponse: map[string]any{"answers": map[string]any{}},
		response: func(response domain.OperatorResponse) (any, [][]byte, error) {
			if response.ChoiceID != "" || len(response.Answers) != len(rules) {
				return nil, nil, ErrInvalidOperatorResponse
			}
			answers := make(map[string]any, len(rules))
			secrets := make([][]byte, 0)
			for questionID, rule := range rules {
				supplied, ok := response.Answers[questionID]
				if !ok || len(supplied) == 0 || len(supplied) > 32 {
					return nil, nil, ErrInvalidOperatorResponse
				}
				mapped := make([]string, len(supplied))
				for index, answer := range supplied {
					if answer == "" || len(answer) > 64<<10 {
						return nil, nil, ErrInvalidOperatorResponse
					}
					if label, exists := rule.choices[answer]; exists {
						mapped[index] = label
					} else if rule.allowsOther {
						mapped[index] = answer
					} else {
						return nil, nil, ErrInvalidOperatorResponse
					}
					if rule.secret {
						secrets = append(secrets, []byte(mapped[index]))
					}
				}
				answers[questionID] = map[string]any{"answers": mapped}
			}
			return map[string]any{"answers": answers}, secrets, nil
		},
	}, nil
}

func commandDecisions(raw []json.RawMessage) (map[string]any, []domain.OperatorChoice, error) {
	if raw == nil {
		raw = []json.RawMessage{json.RawMessage(`"accept"`), json.RawMessage(`"acceptForSession"`), json.RawMessage(`"decline"`), json.RawMessage(`"cancel"`)}
	}
	decisions := make(map[string]any, len(raw))
	choices := make([]domain.OperatorChoice, 0, len(raw))
	for index, encoded := range raw {
		var decision any
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		if err := decoder.Decode(&decision); err != nil {
			return nil, nil, err
		}
		choiceID, label, description, ok := commandDecisionPresentation(decision, index)
		if !ok {
			return nil, nil, errors.New("unsupported command decision")
		}
		if _, duplicate := decisions[choiceID]; duplicate {
			return nil, nil, errors.New("duplicate command decision")
		}
		decisions[choiceID] = decision
		choices = append(choices, domain.OperatorChoice{ID: choiceID, Label: label, Description: description})
	}
	if len(decisions) == 0 {
		return nil, nil, errors.New("no command decisions")
	}
	return decisions, choices, nil
}

func commandDecisionPresentation(decision any, index int) (string, string, string, bool) {
	if value, ok := decision.(string); ok {
		switch value {
		case "accept":
			return "accept", "Allow once", "Run this command once.", true
		case "acceptForSession":
			return "accept_for_session", "Allow for session", "Allow matching commands for this Codex session.", true
		case "decline":
			return "decline", "Deny", "Do not run this command; let the turn continue.", true
		case "cancel":
			return "cancel", "Deny and stop turn", "Do not run this command and interrupt the turn.", true
		}
	}
	object, ok := decision.(map[string]any)
	if !ok || len(object) != 1 {
		return "", "", "", false
	}
	if _, exists := object["acceptWithExecpolicyAmendment"]; exists {
		detail, ok := exactJSONValueFromAny(object, 4<<10)
		return "accept_with_execpolicy_amendment", "Allow and remember command policy", "Apply this exact command policy amendment: " + detail, ok
	}
	if _, exists := object["applyNetworkPolicyAmendment"]; exists {
		detail, ok := exactJSONValueFromAny(object, 4<<10)
		return fmt.Sprintf("apply_network_policy_amendment_%d", index+1), "Apply network policy", "Apply this exact network policy amendment: " + detail, ok
	}
	return "", "", "", false
}

func simpleApprovalChoices(includeSession bool) []domain.OperatorChoice {
	choices := []domain.OperatorChoice{{ID: "accept", Label: "Allow once", Description: "Approve this request once."}}
	if includeSession {
		choices = append(choices, domain.OperatorChoice{ID: "accept_for_session", Label: "Allow for session", Description: "Approve matching changes for this Codex session."})
	}
	return append(choices,
		domain.OperatorChoice{ID: "decline", Label: "Deny", Description: "Deny this request and continue the turn."},
		domain.OperatorChoice{ID: "cancel", Label: "Deny and stop turn", Description: "Deny this request and interrupt the turn."},
	)
}

func validRequestIdentity(identity requestIdentity, sessionID string) bool {
	return identity.ItemID != "" && identity.ThreadID != "" && identity.TurnID != "" && identity.ThreadID+"-"+identity.TurnID == sessionID
}

func decodeRequestParams(raw json.RawMessage, target any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ErrMalformedServerRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrMalformedServerRequest
	}
	return nil
}

func decodeJSONObjectValue(raw json.RawMessage) (map[string]any, error) {
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil || value == nil {
		return nil, ErrMalformedServerRequest
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrMalformedServerRequest
	}
	return value, nil
}

func validOperatorToken(value string) bool {
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

func cloneJSONValue(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var clone any
	if err := decoder.Decode(&clone); err != nil {
		return nil
	}
	return clone
}

func firstNonempty(first, second *string, fallback string) string {
	for _, candidate := range []*string{first, second} {
		if candidate != nil && strings.TrimSpace(*candidate) != "" {
			return boundedRequestText(*candidate, 8<<10)
		}
	}
	return fallback
}

func boundedRequestText(value string, maximum int) string {
	value = strings.ToValidUTF8(value, "")
	if len(value) <= maximum {
		return value
	}
	value = value[:maximum]
	for len(value) > 0 && value[len(value)-1]&0xc0 == 0x80 {
		value = value[:len(value)-1]
	}
	return value
}

func appendExactStringDetail(details []domain.OperatorDetail, label string, value *string, maximum int) ([]domain.OperatorDetail, bool) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return details, true
	}
	if len(*value) > maximum {
		return nil, false
	}
	return append(details, domain.OperatorDetail{Label: label, Value: *value}), true
}

func appendExactJSONDetail(details []domain.OperatorDetail, label string, raw json.RawMessage, maximum int) ([]domain.OperatorDetail, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return details, true
	}
	value, ok := exactJSONValue(trimmed, maximum)
	if !ok {
		return nil, false
	}
	return append(details, domain.OperatorDetail{Label: label, Value: value}), true
}

func exactJSONValue(raw json.RawMessage, maximum int) (string, bool) {
	var formatted bytes.Buffer
	if err := json.Compact(&formatted, raw); err != nil {
		return "", false
	}
	if formatted.Len() > maximum {
		return "", false
	}
	return formatted.String(), true
}

func exactJSONValueFromAny(value any, maximum int) (string, bool) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", false
	}
	return exactJSONValue(raw, maximum)
}
