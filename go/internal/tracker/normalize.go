package tracker

import (
	"strings"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
)

// NormalizeState returns the scheduler comparison form. Callers retain the
// provider-native State on Issue for display and tool context.
func NormalizeState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

func NormalizeLabels(labels []string) []string {
	normalized := make([]string, 0, len(labels))
	seen := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		label = strings.ToLower(strings.TrimSpace(label))
		if label == "" {
			continue
		}
		if _, found := seen[label]; found {
			continue
		}
		seen[label] = struct{}{}
		normalized = append(normalized, label)
	}
	return normalized
}

// NormalizeIssue applies provider-neutral optional-field fallbacks, owns every
// reference-bearing field, and validates the required normalized record. Wire
// decoders remain responsible for proving dispatchability was explicitly
// present before constructing Issue.Dispatchable.
func NormalizeIssue(issue domain.Issue) (domain.Issue, error) {
	normalized, err := issue.Clone()
	if err != nil {
		// native_ref is optional. A provider value that cannot safely cross the
		// JSON boundary falls back to null without hiding valid required fields.
		issue.NativeRef = nil
		normalized, err = issue.Clone()
		if err != nil {
			return domain.Issue{}, err
		}
	}
	normalized.Labels = NormalizeLabels(normalized.Labels)
	normalized.BlockedBy = normalizeBlockers(normalized.BlockedBy)
	normalized.Description = usableOptionalString(normalized.Description)
	normalized.BranchName = usableOptionalString(normalized.BranchName)
	normalized.URL = usableOptionalString(normalized.URL)
	normalized.AssigneeID = usableOptionalString(normalized.AssigneeID)
	normalized.CreatedAt = normalizeTime(normalized.CreatedAt)
	normalized.UpdatedAt = normalizeTime(normalized.UpdatedAt)
	if err := normalized.ValidateRequired(); err != nil {
		return domain.Issue{}, err
	}
	return normalized, nil
}

func normalizeBlockers(blockers []domain.BlockerRef) []domain.BlockerRef {
	normalized := make([]domain.BlockerRef, 0, len(blockers))
	for _, blocker := range blockers {
		blocker.ID = usableOptionalString(blocker.ID)
		blocker.Identifier = usableOptionalString(blocker.Identifier)
		blocker.State = usableOptionalString(blocker.State)
		if blocker.ID == nil && blocker.Identifier == nil && blocker.State == nil {
			continue
		}
		normalized = append(normalized, blocker)
	}
	return normalized
}

func usableOptionalString(value *string) *string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	clone := *value
	return &clone
}

func normalizeTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}
