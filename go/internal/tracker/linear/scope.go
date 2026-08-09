package linear

import (
	"context"
	"encoding/json"

	"github.com/coryj627/symphony/go/internal/tracker"
)

func (adapter *Adapter) verifyProjectScope(ctx context.Context) error {
	data, err := adapter.request(ctx, SymphonyProjectScope, map[string]any{
		"projectSlug": adapter.config.ProjectSlug,
		"first":       2,
	})
	if err != nil {
		return err
	}
	projects, ok := decodeProjectScope(data)
	if !ok || projects.hasNextPage || len(projects.nodes) > 1 {
		return payloadError("Linear returned a malformed project scope payload")
	}
	if len(projects.nodes) == 0 {
		return &tracker.Error{Category: tracker.CategoryScope, Message: "Linear project is missing or inaccessible"}
	}
	node := projects.nodes[0]
	if node.id == "" || node.slug != adapter.config.ProjectSlug {
		return payloadError("Linear returned a malformed project scope payload")
	}
	return nil
}

type projectScopePage struct {
	nodes       []projectScopeNode
	hasNextPage bool
}

type projectScopeNode struct {
	id   string
	slug string
}

func decodeProjectScope(raw json.RawMessage) (projectScopePage, bool) {
	var data struct {
		Projects json.RawMessage `json:"projects"`
	}
	if !decodeOneJSON(raw, &data) {
		return projectScopePage{}, false
	}
	var connection struct {
		Nodes    json.RawMessage `json:"nodes"`
		PageInfo json.RawMessage `json:"pageInfo"`
	}
	if !decodeOneJSON(data.Projects, &connection) {
		return projectScopePage{}, false
	}
	var rawNodes []json.RawMessage
	if !decodeOneJSON(connection.Nodes, &rawNodes) || rawNodes == nil {
		return projectScopePage{}, false
	}
	hasNextPage, ok := requiredNestedBool(connection.PageInfo, "hasNextPage")
	if !ok {
		return projectScopePage{}, false
	}
	nodes := make([]projectScopeNode, 0, len(rawNodes))
	for _, rawNode := range rawNodes {
		var node struct {
			ID     json.RawMessage `json:"id"`
			SlugID json.RawMessage `json:"slugId"`
		}
		if !decodeOneJSON(rawNode, &node) {
			return projectScopePage{}, false
		}
		id, idOK := requiredString(node.ID)
		slug, slugOK := requiredString(node.SlugID)
		if !idOK || !slugOK {
			return projectScopePage{}, false
		}
		nodes = append(nodes, projectScopeNode{id: id, slug: slug})
	}
	return projectScopePage{nodes: nodes, hasNextPage: hasNextPage}, true
}
