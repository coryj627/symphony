package domain

import "time"

type OperatorChoice struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type OperatorQuestion struct {
	ID             string           `json:"id"`
	Label          string           `json:"label"`
	Description    string           `json:"description"`
	Required       bool             `json:"required"`
	AllowsMultiple bool             `json:"allows_multiple"`
	AllowsOther    bool             `json:"allows_other"`
	IsSecret       bool             `json:"is_secret"`
	Choices        []OperatorChoice `json:"choices"`
}

type OperatorDetail struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type OperatorRequest struct {
	ID                  string             `json:"id"`
	SessionID           string             `json:"session_id"`
	IssueID             string             `json:"issue_id"`
	IssueIdentifier     string             `json:"issue_identifier"`
	Kind                string             `json:"kind"`
	Title               string             `json:"title"`
	Summary             string             `json:"summary"`
	Details             []OperatorDetail   `json:"details"`
	OpenedAt            time.Time          `json:"opened_at"`
	WarningAt           time.Time          `json:"warning_at"`
	DeadlineAt          time.Time          `json:"deadline_at"`
	ExtensionsUsed      int                `json:"extensions_used"`
	ExtensionsRemaining int                `json:"extensions_remaining"`
	Choices             []OperatorChoice   `json:"choices"`
	Questions           []OperatorQuestion `json:"questions"`
}

type OperatorResponse struct {
	RequestID string              `json:"request_id"`
	SessionID string              `json:"session_id"`
	ChoiceID  string              `json:"choice_id"`
	Answers   map[string][]string `json:"answers"`
}

func (request OperatorRequest) Clone() OperatorRequest {
	clone := request
	if request.Details != nil {
		clone.Details = append(make([]OperatorDetail, 0, len(request.Details)), request.Details...)
	}
	clone.Choices = cloneOperatorChoices(request.Choices)
	if request.Questions != nil {
		clone.Questions = make([]OperatorQuestion, len(request.Questions))
		for index, question := range request.Questions {
			clone.Questions[index] = question
			clone.Questions[index].Choices = cloneOperatorChoices(question.Choices)
		}
	}
	return clone
}

func (response OperatorResponse) Clone() OperatorResponse {
	clone := response
	if response.Answers != nil {
		clone.Answers = make(map[string][]string, len(response.Answers))
		for question, answers := range response.Answers {
			clone.Answers[question] = append(make([]string, 0, len(answers)), answers...)
		}
	}
	return clone
}

func cloneOperatorChoices(choices []OperatorChoice) []OperatorChoice {
	if choices == nil {
		return nil
	}
	return append(make([]OperatorChoice, 0, len(choices)), choices...)
}
