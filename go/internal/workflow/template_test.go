package workflow

import (
	"errors"
	"testing"
)

func TestRenderRejectsUnknownVariableAndFilter(t *testing.T) {
	// Break caught: lax template rendering silently drops misspelled issue fields
	// or unsupported filters and sends an incomplete instruction to Codex.
	def := Definition{Prompt: "{{ issue.missing | no_such_filter }}"}
	_, err := Render(def, TemplateIssue{Identifier: "GH-1", Title: "Test"}, nil)
	if !errors.Is(err, ErrTemplateRender) && !errors.Is(err, ErrTemplateParse) {
		t.Fatalf("got %v", err)
	}
}

func TestRenderBindsIssueAndAttempt(t *testing.T) {
	// Break caught: passing an issue struct directly leaves Liquid unable to
	// resolve its documented string-keyed variables or a retry attempt.
	attempt := 2
	got, err := Render(
		Definition{Prompt: "{{ issue.identifier }}: {{ issue.title }} (attempt {{ attempt }})"},
		TemplateIssue{Identifier: "GH-1", Title: "Fix parser"},
		&attempt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != "GH-1: Fix parser (attempt 2)" {
		t.Fatalf("rendered prompt = %q", got)
	}
}

func TestRenderUsesFallbackForBlankPrompt(t *testing.T) {
	// Break caught: a valid workflow with no Markdown body otherwise launches an
	// agent with an empty instruction instead of the configured-tracker fallback.
	got, err := Render(Definition{}, TemplateIssue{Identifier: "GH-1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "You are working on an issue from the configured tracker." {
		t.Fatalf("rendered prompt = %q", got)
	}
}
