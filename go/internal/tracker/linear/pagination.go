package linear

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/coryj627/symphony/go/internal/tracker"
)

const (
	linearLogicalPageSize = 50
	// The fixed query costs ceil(1.2 + first*233.6) under Linear's
	// documented complexity weights. Forty costs 9,346, below the hard
	// 10,000-point ceiling while leaving the nested connections at 50.
	linearRequestPageSize = 40
	maxPages              = 100
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
		nodes := make([]json.RawMessage, 0, linearLogicalPageSize)
		remaining := linearLogicalPageSize
		for remaining > 0 {
			first := min(linearRequestPageSize, remaining)
			page, err := adapter.request(ctx, SymphonyIssuesByStates, map[string]any{
				"projectSlug":   adapter.config.ProjectSlug,
				"stateNames":    append([]string(nil), stateNames...),
				"first":         first,
				"relationFirst": linearLogicalPageSize,
				"after":         after,
			})
			if err != nil {
				return nil, err
			}
			nodes = append(nodes, page.nodes...)
			if !page.hasNextPage {
				pages = append(pages, statePage{number: pageNumber, nodes: nodes})
				return pages, nil
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
			remaining -= first
		}
		pages = append(pages, statePage{number: pageNumber, nodes: nodes})
		if pageNumber == maxPages {
			return nil, paginationError("Linear pagination exceeded the page limit")
		}
	}
	return nil, paginationError("Linear pagination exceeded the page limit")
}

func (adapter *Adapter) fetchIDBatches(ctx context.Context, ids []string) ([]idBatch, error) {
	batches := make([]idBatch, 0, (len(ids)+linearRequestPageSize-1)/linearRequestPageSize)
	for logicalStart := 0; logicalStart < len(ids); logicalStart += linearLogicalPageSize {
		logicalEnd := min(logicalStart+linearLogicalPageSize, len(ids))
		for start := logicalStart; start < logicalEnd; start += linearRequestPageSize {
			end := min(start+linearRequestPageSize, logicalEnd)
			batchIDs := append([]string(nil), ids[start:end]...)
			page, err := adapter.request(ctx, SymphonyIssuesByIDs, map[string]any{
				"ids":           batchIDs,
				"projectSlug":   adapter.config.ProjectSlug,
				"first":         len(batchIDs),
				"relationFirst": linearLogicalPageSize,
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
			for _, raw := range page.nodes {
				id, ok := rawIssueID(raw)
				if !ok {
					return nil, payloadError("Linear ID response contained a malformed issue ID")
				}
				if _, requestedByThisQuery := requested[id]; !requestedByThisQuery {
					return nil, payloadError("Linear ID response contained an unexpected issue ID")
				}
			}
			batches = append(batches, idBatch{
				number: len(batches) + 1, requested: requested,
				nodes: append([]json.RawMessage(nil), page.nodes...),
			})
		}
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
