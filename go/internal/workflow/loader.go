package workflow

import (
	"crypto/sha256"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Parse reads only a leading YAML front-matter block. Markdown that contains a
// later separator remains prompt text.
func Parse(path string, source []byte) (Definition, error) {
	text := string(source)
	if !startsFrontMatter(text) {
		return Definition{FrontMatter: emptyMapNode(), Prompt: strings.TrimSpace(text)}, nil
	}

	frontMatter, prompt, ok := splitFrontMatter(text)
	if !ok {
		return Definition{}, workflowError(ErrWorkflowParse, path, 1, 1, "front matter is missing its closing ---")
	}

	var node yaml.Node
	if err := yaml.Unmarshal([]byte(frontMatter), &node); err != nil {
		line, column := yamlErrorLocation(err)
		return Definition{}, workflowError(ErrWorkflowParse, path, line, column, err.Error())
	}
	if len(node.Content) == 0 {
		node = *emptyMapNode()
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		line, column := node.Line, node.Column
		if len(node.Content) > 0 {
			line, column = node.Content[0].Line, node.Content[0].Column
		}
		return Definition{}, workflowError(ErrFrontMatterNotMap, path, line+1, column, "front matter must be a YAML mapping")
	}

	return Definition{FrontMatter: &node, Prompt: strings.TrimSpace(prompt)}, nil
}

var yamlLineColumn = regexp.MustCompile(`line ([0-9]+)(?:, column ([0-9]+))?`)

func yamlErrorLocation(err error) (line, column int) {
	match := yamlLineColumn.FindStringSubmatch(err.Error())
	if len(match) == 0 {
		return 1, 1
	}
	line, _ = strconv.Atoi(match[1])
	if len(match) > 2 && match[2] != "" {
		column, _ = strconv.Atoi(match[2])
	}
	if column == 0 {
		column = 1
	}
	// The YAML parser sees only the content after the opening delimiter.
	return line + 1, column
}

// Load reads, parses, and resolves a workflow into a point-in-time snapshot.
func Load(path string, lookup LookupEnv) (Snapshot, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: %s: %v", ErrMissingWorkflow, path, err)
	}
	definition, err := Parse(path, source)
	if err != nil {
		return Snapshot{}, err
	}
	config, err := Resolve(path, definition, lookup)
	if err != nil {
		return Snapshot{}, err
	}
	digest := sha256.Sum256(source)
	return Snapshot{
		Path:       path,
		Source:     string(source),
		Digest:     fmt.Sprintf("%x", digest),
		Definition: definition,
		Config:     config,
		LoadedAt:   time.Now(),
	}, nil
}

func startsFrontMatter(source string) bool {
	firstLine, _, _ := strings.Cut(source, "\n")
	return strings.TrimSuffix(firstLine, "\r") == "---"
}

func splitFrontMatter(source string) (frontMatter, prompt string, ok bool) {
	lines := strings.SplitAfter(source, "\n")
	for index := 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) == "---" {
			return strings.Join(lines[1:index], ""), strings.Join(lines[index+1:], ""), true
		}
	}
	return "", "", false
}

func emptyMapNode() *yaml.Node {
	return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
}

func workflowError(class error, path string, line, column int, detail string) error {
	if line < 1 {
		line = 1
	}
	if column < 1 {
		column = 1
	}
	return fmt.Errorf("%w: %s:%d:%d: %s", class, path, line, column, detail)
}
