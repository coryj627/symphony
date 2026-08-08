package linear

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
	"github.com/coryj627/symphony/go/internal/tracker"
)

var (
	errMalformedIssueRecord = errors.New("malformed Linear issue record")
	errOutOfProjectScope    = errors.New("Linear issue is outside the configured project")
)

type issueRecord struct {
	ID               json.RawMessage `json:"id"`
	Identifier       json.RawMessage `json:"identifier"`
	Title            json.RawMessage `json:"title"`
	Description      json.RawMessage `json:"description"`
	Priority         json.RawMessage `json:"priority"`
	State            json.RawMessage `json:"state"`
	BranchName       json.RawMessage `json:"branchName"`
	URL              json.RawMessage `json:"url"`
	Assignee         json.RawMessage `json:"assignee"`
	Labels           json.RawMessage `json:"labels"`
	InverseRelations json.RawMessage `json:"inverseRelations"`
	Project          json.RawMessage `json:"project"`
	Team             json.RawMessage `json:"team"`
	CreatedAt        json.RawMessage `json:"createdAt"`
	UpdatedAt        json.RawMessage `json:"updatedAt"`
}

type rawPageInfo struct {
	HasNextPage json.RawMessage `json:"hasNextPage"`
	EndCursor   json.RawMessage `json:"endCursor"`
}

func normalizeIssueRecord(raw json.RawMessage, projectSlug string, terminalStates map[string]struct{}) (domain.Issue, error) {
	var record issueRecord
	if !decodeOneJSON(raw, &record) {
		return domain.Issue{}, errMalformedIssueRecord
	}
	id, ok := requiredString(record.ID)
	if !ok {
		return domain.Issue{}, errMalformedIssueRecord
	}
	identifier, ok := requiredString(record.Identifier)
	if !ok {
		return domain.Issue{}, errMalformedIssueRecord
	}
	title, ok := requiredString(record.Title)
	if !ok {
		return domain.Issue{}, errMalformedIssueRecord
	}
	state, ok := requiredNestedString(record.State, "name")
	if !ok {
		return domain.Issue{}, errMalformedIssueRecord
	}
	projectID, ok := requiredNestedString(record.Project, "id")
	if !ok {
		return domain.Issue{}, errMalformedIssueRecord
	}
	returnedProjectSlug, ok := requiredNestedString(record.Project, "slugId")
	if !ok {
		return domain.Issue{}, errMalformedIssueRecord
	}
	if returnedProjectSlug != projectSlug {
		return domain.Issue{}, errOutOfProjectScope
	}
	teamID, ok := requiredNestedString(record.Team, "id")
	if !ok {
		return domain.Issue{}, errMalformedIssueRecord
	}
	labels, err := normalizeLabelConnection(record.Labels)
	if err != nil {
		return domain.Issue{}, errMalformedIssueRecord
	}
	blockedBy, relationComplete := normalizeRelationConnection(record.InverseRelations)

	dispatchable := true
	if tracker.NormalizeState(state) == "todo" {
		dispatchable = relationComplete
		if dispatchable {
			for _, blocker := range blockedBy {
				if blocker.State == nil {
					dispatchable = false
					break
				}
				if _, terminal := terminalStates[tracker.NormalizeState(*blocker.State)]; !terminal {
					dispatchable = false
					break
				}
			}
		}
	}

	issue := domain.Issue{
		ID:          id,
		Identifier:  identifier,
		Title:       title,
		Description: optionalString(record.Description),
		Priority:    optionalPriority(record.Priority),
		State:       state,
		BranchName:  optionalString(record.BranchName),
		URL:         optionalURL(record.URL),
		AssigneeID:  optionalNestedString(record.Assignee, "id"),
		Labels:      labels,
		BlockedBy:   blockedBy,
		NativeRef: map[string]any{
			"issue_id": id, "identifier": identifier, "project_id": projectID,
			"project_slug": returnedProjectSlug, "team_id": teamID,
		},
		Dispatchable: dispatchable,
		CreatedAt:    optionalTimestamp(record.CreatedAt),
		UpdatedAt:    optionalTimestamp(record.UpdatedAt),
	}
	normalized, err := tracker.NormalizeIssue(issue)
	if err != nil {
		return domain.Issue{}, errMalformedIssueRecord
	}
	return normalized, nil
}

func normalizeLabelConnection(raw json.RawMessage) ([]string, error) {
	var connection struct {
		Nodes    json.RawMessage `json:"nodes"`
		PageInfo json.RawMessage `json:"pageInfo"`
	}
	if !decodeOneJSON(raw, &connection) {
		return nil, errMalformedIssueRecord
	}
	var nodes []json.RawMessage
	if !decodeOneJSON(connection.Nodes, &nodes) || nodes == nil {
		return nil, errMalformedIssueRecord
	}
	hasNext, ok := requiredNestedBool(connection.PageInfo, "hasNextPage")
	if !ok || hasNext {
		return nil, errMalformedIssueRecord
	}
	labels := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if name := optionalNestedString(node, "name"); name != nil {
			labels = append(labels, *name)
		}
	}
	return labels, nil
}

func normalizeRelationConnection(raw json.RawMessage) ([]domain.BlockerRef, bool) {
	blockers := make([]domain.BlockerRef, 0)
	var connection struct {
		Nodes    json.RawMessage `json:"nodes"`
		PageInfo json.RawMessage `json:"pageInfo"`
	}
	if !decodeOneJSON(raw, &connection) {
		return blockers, false
	}
	var nodes []json.RawMessage
	if !decodeOneJSON(connection.Nodes, &nodes) || nodes == nil {
		return blockers, false
	}
	hasNext, pageInfoValid := requiredNestedBool(connection.PageInfo, "hasNextPage")
	complete := pageInfoValid && !hasNext
	for _, node := range nodes {
		var relation struct {
			Type  json.RawMessage `json:"type"`
			Issue json.RawMessage `json:"issue"`
		}
		if !decodeOneJSON(node, &relation) {
			complete = false
			continue
		}
		relationType, ok := requiredString(relation.Type)
		if !ok {
			complete = false
			continue
		}
		if tracker.NormalizeState(relationType) != "blocks" {
			continue
		}
		var related struct {
			ID         json.RawMessage `json:"id"`
			Identifier json.RawMessage `json:"identifier"`
			State      json.RawMessage `json:"state"`
		}
		if !decodeOneJSON(relation.Issue, &related) {
			complete = false
			continue
		}
		blocker := domain.BlockerRef{
			ID:         optionalString(related.ID),
			Identifier: optionalString(related.Identifier),
			State:      optionalNestedString(related.State, "name"),
		}
		if blocker.ID == nil && blocker.Identifier == nil && blocker.State == nil {
			complete = false
			continue
		}
		blockers = append(blockers, blocker)
	}
	return blockers, complete
}

func normalizedStateSet(states []string) map[string]struct{} {
	set := make(map[string]struct{}, len(states))
	for _, state := range states {
		if normalized := tracker.NormalizeState(state); normalized != "" {
			set[normalized] = struct{}{}
		}
	}
	return set
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

func requiredString(raw json.RawMessage) (string, bool) {
	var value string
	if !decodeOneJSON(raw, &value) || strings.TrimSpace(value) == "" {
		return "", false
	}
	return value, true
}

func optionalString(raw json.RawMessage) *string {
	value, ok := requiredString(raw)
	if !ok {
		return nil
	}
	return stringPointer(value)
}

func requiredNestedString(raw json.RawMessage, field string) (string, bool) {
	var object map[string]json.RawMessage
	if !decodeOneJSON(raw, &object) {
		return "", false
	}
	return requiredString(object[field])
}

func optionalNestedString(raw json.RawMessage, field string) *string {
	value, ok := requiredNestedString(raw, field)
	if !ok {
		return nil
	}
	return stringPointer(value)
}

func requiredNestedBool(raw json.RawMessage, field string) (bool, bool) {
	var object map[string]json.RawMessage
	if !decodeOneJSON(raw, &object) {
		return false, false
	}
	var value bool
	if !decodeOneJSON(object[field], &value) {
		return false, false
	}
	return value, true
}

func optionalPriority(raw json.RawMessage) *int {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return nil
	}
	integer, err := strconv.ParseInt(text, 10, strconv.IntSize)
	if err != nil {
		return nil
	}
	value := int(integer)
	return &value
}

func optionalURL(raw json.RawMessage) *string {
	value := optionalString(raw)
	if value == nil {
		return nil
	}
	parsed, err := url.Parse(*value)
	if err != nil || parsed.User != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil
	}
	return value
}

func optionalTimestamp(raw json.RawMessage) *time.Time {
	value := optionalString(raw)
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
