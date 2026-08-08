package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/tracker"
)

var errMalformedIssueRecord = errors.New("malformed GitHub issue record")

type issueRecord struct {
	DatabaseID  json.RawMessage `json:"id"`
	NodeID      json.RawMessage `json:"node_id"`
	Number      json.RawMessage `json:"number"`
	Title       json.RawMessage `json:"title"`
	Body        json.RawMessage `json:"body"`
	State       json.RawMessage `json:"state"`
	StateReason json.RawMessage `json:"state_reason"`
	HTMLURL     json.RawMessage `json:"html_url"`
	Labels      json.RawMessage `json:"labels"`
	Assignees   json.RawMessage `json:"assignees"`
	CreatedAt   json.RawMessage `json:"created_at"`
	UpdatedAt   json.RawMessage `json:"updated_at"`
	PullRequest json.RawMessage `json:"pull_request"`
}

func normalizeIssueRecord(raw json.RawMessage, config tracker.GitHubConfig) (domain.Issue, bool, error) {
	var record issueRecord
	if !decodeOneJSON(raw, &record) {
		return domain.Issue{}, false, errMalformedIssueRecord
	}
	pullRequest := record.PullRequest != nil
	numberText, ok := requiredPositiveNumber(record.Number)
	if !ok {
		return domain.Issue{}, pullRequest, errMalformedIssueRecord
	}
	title, ok := requiredString(record.Title)
	if !ok {
		return domain.Issue{}, pullRequest, errMalformedIssueRecord
	}
	state, ok := requiredString(record.State)
	if !ok {
		return domain.Issue{}, pullRequest, errMalformedIssueRecord
	}

	assignees := optionalAssignees(record.Assignees)
	dispatchable := true
	if configured := strings.TrimSpace(config.Assignee); configured != "" {
		dispatchable = false
		for _, login := range assignees {
			if strings.EqualFold(login, configured) {
				dispatchable = true
				break
			}
		}
	}
	if pullRequest {
		dispatchable = false
	}

	nativeRef := map[string]any{
		"owner":      config.Owner,
		"repository": config.Repository,
		"number":     json.Number(numberText),
	}
	if databaseID, ok := optionalPositiveJSONNumber(record.DatabaseID); ok {
		nativeRef["database_id"] = databaseID
	}
	if nodeID := optionalTrimmedString(record.NodeID); nodeID != nil {
		nativeRef["node_id"] = *nodeID
	}
	if stateReason := optionalTrimmedString(record.StateReason); stateReason != nil {
		nativeRef["state_reason"] = *stateReason
	}

	var assigneeID *string
	if len(assignees) > 0 {
		assigneeID = stringPointer(assignees[0])
	}
	issue := domain.Issue{
		ID:           dispatchID(config, numberText),
		NativeRef:    nativeRef,
		Identifier:   "#" + numberText,
		Title:        title,
		Description:  optionalString(record.Body),
		Priority:     nil,
		State:        state,
		BranchName:   nil,
		URL:          optionalURL(record.HTMLURL),
		AssigneeID:   assigneeID,
		Labels:       optionalLabels(record.Labels),
		BlockedBy:    []domain.BlockerRef{},
		Dispatchable: dispatchable,
		CreatedAt:    optionalTimestamp(record.CreatedAt),
		UpdatedAt:    optionalTimestamp(record.UpdatedAt),
	}
	normalized, err := tracker.NormalizeIssue(issue)
	if err != nil {
		return domain.Issue{}, pullRequest, errMalformedIssueRecord
	}
	return normalized, pullRequest, nil
}

func dispatchID(config tracker.GitHubConfig, number string) string {
	return "github:" + strings.ToLower(config.Owner) + "/" + strings.ToLower(config.Repository) + "#" + number
}

func decodeOneJSON(raw []byte, destination any) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return false
	}
	var trailing any
	return errors.Is(decoder.Decode(&trailing), io.EOF)
}

func requiredPositiveNumber(raw json.RawMessage) (string, bool) {
	text := string(bytes.TrimSpace(raw))
	if text == "" || strings.HasPrefix(text, "+") || (len(text) > 1 && text[0] == '0') {
		return "", false
	}
	number, err := strconv.ParseUint(text, 10, 64)
	if err != nil || number == 0 {
		return "", false
	}
	return text, true
}

func requiredString(raw json.RawMessage) (string, bool) {
	var value string
	if !decodeOneJSON(raw, &value) || strings.TrimSpace(value) == "" {
		return "", false
	}
	return value, true
}

func optionalString(raw json.RawMessage) *string {
	var value string
	if !decodeOneJSON(raw, &value) || strings.TrimSpace(value) == "" {
		return nil
	}
	return stringPointer(value)
}

func optionalTrimmedString(raw json.RawMessage) *string {
	value := optionalString(raw)
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	return stringPointer(trimmed)
}

func optionalPositiveJSONNumber(raw json.RawMessage) (json.Number, bool) {
	text := string(bytes.TrimSpace(raw))
	if text == "" || strings.HasPrefix(text, "+") || (len(text) > 1 && text[0] == '0') {
		return "", false
	}
	integer := new(big.Int)
	if _, ok := integer.SetString(text, 10); !ok || integer.Sign() <= 0 {
		return "", false
	}
	return json.Number(text), true
}

func optionalURL(raw json.RawMessage) *string {
	value := optionalTrimmedString(raw)
	if value == nil {
		return nil
	}
	parsed, err := url.Parse(*value)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
		return nil
	}
	return value
}

func optionalLabels(raw json.RawMessage) []string {
	var entries []json.RawMessage
	if !decodeOneJSON(raw, &entries) || entries == nil {
		return []string{}
	}
	labels := make([]string, 0, len(entries))
	for _, entry := range entries {
		var label struct {
			Name json.RawMessage `json:"name"`
		}
		if !decodeOneJSON(entry, &label) {
			continue
		}
		if name := optionalString(label.Name); name != nil {
			labels = append(labels, *name)
		}
	}
	return labels
}

func optionalAssignees(raw json.RawMessage) []string {
	var entries []json.RawMessage
	if !decodeOneJSON(raw, &entries) || entries == nil {
		return []string{}
	}
	assignees := make([]string, 0, len(entries))
	for _, entry := range entries {
		var assignee struct {
			Login json.RawMessage `json:"login"`
		}
		if !decodeOneJSON(entry, &assignee) {
			continue
		}
		if login := optionalTrimmedString(assignee.Login); login != nil {
			assignees = append(assignees, *login)
		}
	}
	return assignees
}

func optionalTimestamp(raw json.RawMessage) *time.Time {
	value := optionalTrimmedString(raw)
	if value == nil {
		return nil
	}
	timestamp, err := time.Parse(time.RFC3339, *value)
	if err != nil {
		return nil
	}
	return &timestamp
}

func stringPointer(value string) *string {
	copy := value
	return &copy
}
