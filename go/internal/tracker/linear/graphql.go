package linear

import (
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

const (
	maxLinearToolQueryBytes  = 64 << 10
	maxLinearToolQueryTokens = 32768
)

type linearOperation string

const (
	linearQuery    linearOperation = "query"
	linearMutation linearOperation = "mutation"
)

func parseLinearGraphQLDocument(query string) (linearOperation, string) {
	if strings.TrimSpace(query) == "" || len(query) > maxLinearToolQueryBytes {
		return "", "invalid_graphql"
	}
	document, err := parser.ParseQueryWithTokenLimit(&ast.Source{Input: query}, maxLinearToolQueryTokens)
	if err != nil {
		return "", "invalid_graphql"
	}
	if len(document.Operations) != 1 {
		return "", "invalid_operation_count"
	}
	switch document.Operations[0].Operation {
	case ast.Query:
		return linearQuery, ""
	case ast.Mutation:
		return linearMutation, ""
	default:
		return "", "unsupported_operation"
	}
}
