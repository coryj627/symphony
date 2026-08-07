package workflow

import (
	"fmt"
	"strings"
	"time"

	"github.com/osteele/liquid"
)

const fallbackPrompt = "You are working on an issue from the configured tracker."

// TemplateIssue is the temporary template-facing issue shape. Phase 2 adapts
// its provider-neutral domain Issue into this boundary.
type TemplateIssue struct {
	ID           string
	NativeRef    map[string]any
	Identifier   string
	Title        string
	Description  *string
	Priority     *int
	State        string
	BranchName   *string
	URL          *string
	AssigneeID   *string
	Labels       []string
	BlockedBy    []map[string]any
	Dispatchable bool
	CreatedAt    *time.Time
	UpdatedAt    *time.Time
}

func (issue TemplateIssue) Bindings() map[string]any {
	return map[string]any{
		"id":           issue.ID,
		"native_ref":   issue.NativeRef,
		"identifier":   issue.Identifier,
		"title":        issue.Title,
		"description":  issue.Description,
		"priority":     issue.Priority,
		"state":        issue.State,
		"branch_name":  issue.BranchName,
		"url":          issue.URL,
		"assignee_id":  issue.AssigneeID,
		"labels":       issue.Labels,
		"blocked_by":   issue.BlockedBy,
		"dispatchable": issue.Dispatchable,
		"created_at":   issue.CreatedAt,
		"updated_at":   issue.UpdatedAt,
	}
}

// Render renders one issue prompt with strict Liquid variable and filter
// handling. A blank Markdown body receives the configured-tracker fallback.
func Render(definition Definition, issue TemplateIssue, attempt *int) (string, error) {
	prompt := definition.Prompt
	if strings.TrimSpace(prompt) == "" {
		prompt = fallbackPrompt
	}

	engine := liquid.NewEngine()
	engine.StrictVariables()
	template, err := engine.ParseString(prompt)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrTemplateParse, err)
	}
	out, err := template.Render(liquid.Bindings{"issue": issue.Bindings(), "attempt": attempt})
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrTemplateRender, err)
	}
	return string(out), nil
}
