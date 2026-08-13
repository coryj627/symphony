package orchestrator

import (
	"sort"

	"github.com/coryj627/symphony/go/internal/domain"
)

func SortForDispatch(issues []domain.Issue) []domain.Issue {
	ordered := append([]domain.Issue(nil), issues...)
	sort.SliceStable(ordered, func(leftIndex, rightIndex int) bool {
		left := ordered[leftIndex]
		right := ordered[rightIndex]

		leftKnown, leftPriority := rankedPriority(left.Priority)
		rightKnown, rightPriority := rankedPriority(right.Priority)
		if leftKnown != rightKnown {
			return leftKnown
		}
		if leftKnown && leftPriority != rightPriority {
			return leftPriority < rightPriority
		}

		if left.CreatedAt == nil && right.CreatedAt != nil {
			return false
		}
		if left.CreatedAt != nil && right.CreatedAt == nil {
			return true
		}
		if left.CreatedAt != nil && right.CreatedAt != nil && !left.CreatedAt.Equal(*right.CreatedAt) {
			return left.CreatedAt.Before(*right.CreatedAt)
		}
		return left.Identifier < right.Identifier
	})
	return ordered
}

func rankedPriority(priority *int) (bool, int) {
	if priority == nil || *priority < 1 || *priority > 4 {
		return false, 0
	}
	return true, *priority
}
