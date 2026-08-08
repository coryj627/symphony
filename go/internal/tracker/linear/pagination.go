package linear

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/coryj627/symphony/go/internal/tracker"
)

const (
	linearPageSize = 50
	maxPages       = 100
)

type statePage struct {
	number int
	nodes  []json.RawMessage
}

type idBatch struct {
	number    int
	requested map[string]struct{}
	nodes     []json.RawMessage
}

func (adapter *Adapter) fetchStatePages(ctx context.Context, stateNames []string) ([]statePage, error) {
	pages := make([]statePage, 0, 1)
	seenCursors := make(map[string]struct{})
	var after any
	for pageNumber := 1; pageNumber <= maxPages; pageNumber++ {
		page, err := adapter.request(ctx, SymphonyIssuesByStates, map[string]any{
			"projectSlug":   adapter.config.ProjectSlug,
			"stateNames":    append([]string(nil), stateNames...),
			"first":         linearPageSize,
			"relationFirst": linearPageSize,
			"after":         after,
		})
		if err != nil {
			return nil, err
		}
		pages = append(pages, statePage{number: pageNumber, nodes: append([]json.RawMessage(nil), page.nodes...)})
		if !page.hasNextPage {
			return pages, nil
		}
		if pageNumber == maxPages {
			return nil, paginationError("Linear pagination exceeded the page limit")
		}
		if page.endCursor == nil || strings.TrimSpace(*page.endCursor) == "" {
			return nil, paginationError("Linear pagination was missing an end cursor")
		}
		cursor := *page.endCursor
		if _, repeated := seenCursors[cursor]; repeated {
			return nil, paginationError("Linear pagination repeated a cursor")
		}
		seenCursors[cursor] = struct{}{}
		after = cursor
	}
	return nil, paginationError("Linear pagination exceeded the page limit")
}

func (adapter *Adapter) fetchIDBatches(ctx context.Context, ids []string) ([]idBatch, error) {
	batches := make([]idBatch, 0, (len(ids)+linearPageSize-1)/linearPageSize)
	for start := 0; start < len(ids); start += linearPageSize {
		end := min(start+linearPageSize, len(ids))
		batchIDs := append([]string(nil), ids[start:end]...)
		page, err := adapter.request(ctx, SymphonyIssuesByIDs, map[string]any{
			"ids":           batchIDs,
			"projectSlug":   adapter.config.ProjectSlug,
			"first":         len(batchIDs),
			"relationFirst": linearPageSize,
		})
		if err != nil {
			return nil, err
		}
		if page.hasNextPage {
			return nil, paginationError("Linear ID batch unexpectedly required pagination")
		}
		requested := make(map[string]struct{}, len(batchIDs))
		for _, id := range batchIDs {
			requested[id] = struct{}{}
		}
		batches = append(batches, idBatch{
			number: len(batches) + 1, requested: requested,
			nodes: append([]json.RawMessage(nil), page.nodes...),
		})
	}
	return batches, nil
}

func normalizedStateNames(states []string) []string {
	seen := make(map[string]struct{}, len(states))
	names := make([]string, 0, len(states))
	for _, state := range states {
		trimmed := strings.TrimSpace(state)
		normalized := tracker.NormalizeState(trimmed)
		if normalized == "" {
			continue
		}
		if _, found := seen[normalized]; found {
			continue
		}
		seen[normalized] = struct{}{}
		names = append(names, trimmed)
	}
	return names
}

func uniqueOpaqueIDs(ids []string) ([]string, error) {
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return nil, configError("Linear issue IDs must be nonblank opaque strings")
		}
		if _, found := seen[id]; found {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return unique, nil
}
