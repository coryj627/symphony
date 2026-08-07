package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestParseFrontMatterAndPrompt(t *testing.T) {
	// Break caught: treating the YAML document itself as a map loses the AST
	// needed by later lossless workflow edits.
	source := []byte("---\npolling:\n  interval_ms: 15000\n---\nWork on {{ issue.identifier }}.\n")
	got, err := Parse("/repo/WORKFLOW.md", source)
	if err != nil {
		t.Fatal(err)
	}
	if got.Prompt != "Work on {{ issue.identifier }}." || got.FrontMatter.Kind != yaml.DocumentNode {
		t.Fatalf("unexpected definition: %#v", got)
	}
}

func TestParseRejectsNonMapFrontMatter(t *testing.T) {
	// Break caught: accepting a YAML list lets invalid workflow configuration
	// reach typed resolution.
	_, err := Parse("WORKFLOW.md", []byte("---\n- bad\n---\nprompt"))
	if !errors.Is(err, ErrFrontMatterNotMap) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "WORKFLOW.md:2:1") {
		t.Fatalf("error lacks front-matter location: %v", err)
	}
}

func TestParseErrorCarriesWorkflowPathAndLocation(t *testing.T) {
	// Break caught: a bare YAML parser error leaves an operator unable to find
	// the malformed line in a repository-owned workflow.
	_, err := Parse("WORKFLOW.md", []byte("---\ntracker: [\n---\nprompt"))
	if !errors.Is(err, ErrWorkflowParse) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "WORKFLOW.md:2:1") {
		t.Fatalf("error lacks path and YAML location: %v", err)
	}
}

func TestParseKeepsIndentedDelimiterLikeHookLineInFrontMatter(t *testing.T) {
	// Break caught: trimming a potential closing delimiter treats an indented
	// shell line in a block scalar as the end of YAML front matter.
	source := []byte("---\nhooks:\n  before_run: |\n    echo begin\n    ---\n    echo end\npolling:\n  interval_ms: 1\n---\nPrompt")
	definition, err := Parse("WORKFLOW.md", source)
	if err != nil {
		t.Fatal(err)
	}
	config, err := Resolve("WORKFLOW.md", definition, nil)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Prompt != "Prompt" || config.Hooks.BeforeRun != "echo begin\n---\necho end\n" {
		t.Fatalf("definition=%#v hooks=%#v", definition, config.Hooks)
	}
}

func TestLoadMissingFileIsTyped(t *testing.T) {
	// Break caught: exposing an OS-specific read error prevents callers from
	// presenting a stable missing-workflow diagnosis.
	_, err := Load(filepath.Join(t.TempDir(), "WORKFLOW.md"), os.LookupEnv)
	if !errors.Is(err, ErrMissingWorkflow) {
		t.Fatalf("got %v", err)
	}
}
