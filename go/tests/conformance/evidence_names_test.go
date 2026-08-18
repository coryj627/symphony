package conformance

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

var (
	goEvidencePattern         = regexp.MustCompile(`^go:(\./[A-Za-z0-9_./-]+)::(Test[A-Za-z0-9_]+)$`)
	playwrightEvidencePattern = regexp.MustCompile(`^playwright:([A-Za-z0-9_./-]+\.(?:mjs|js))::(.+)$`)
	reportEvidencePattern     = regexp.MustCompile(`^report:([^#]+)#([a-z0-9][a-z0-9-]*)$`)
)

func TestEvidenceReferencesResolveExactly(t *testing.T) {
	root := repositoryRoot(t)
	manifest := loadUpstreamManifest(t)
	goTests := make(map[string]map[string]struct{})
	for _, row := range manifest.Rows {
		seen := make(map[string]struct{}, len(row.Evidence))
		for _, evidence := range row.Evidence {
			if _, duplicate := seen[evidence]; duplicate {
				t.Errorf("%s repeats evidence %q", row.ID, evidence)
			}
			seen[evidence] = struct{}{}
			switch {
			case goEvidencePattern.MatchString(evidence):
				match := goEvidencePattern.FindStringSubmatch(evidence)
				tests, ok := goTests[match[1]]
				if !ok {
					tests = packageTestNames(t, filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(match[1], "./"))))
					goTests[match[1]] = tests
				}
				if _, exists := tests[match[2]]; !exists {
					t.Errorf("%s references missing Go test %s", row.ID, evidence)
				}
			case playwrightEvidencePattern.MatchString(evidence):
				match := playwrightEvidencePattern.FindStringSubmatch(evidence)
				path := filepath.Join(root, filepath.FromSlash(match[1]))
				data, err := os.ReadFile(path)
				if err != nil {
					t.Errorf("%s references unreadable Playwright file %s: %v", row.ID, path, err)
				} else if !strings.Contains(string(data), match[2]) {
					t.Errorf("%s references absent Playwright title %q", row.ID, match[2])
				}
			case reportEvidencePattern.MatchString(evidence):
				match := reportEvidencePattern.FindStringSubmatch(evidence)
				path := filepath.Clean(filepath.Join(root, filepath.FromSlash(match[1])))
				if !markdownFileHasAnchor(t, path, match[2]) {
					t.Errorf("%s references missing report anchor %s", row.ID, evidence)
				}
			case strings.HasPrefix(evidence, "command:") && strings.TrimSpace(strings.TrimPrefix(evidence, "command:")) != "":
				if row.Profile != "real_integration" {
					t.Errorf("%s uses command-only evidence outside real integration", row.ID)
				}
			default:
				t.Errorf("%s has invalid evidence %q", row.ID, evidence)
			}
		}
	}
}

func packageTestNames(t *testing.T, directory string) map[string]struct{} {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]struct{})
	files := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(files, filepath.Join(directory, entry.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && strings.HasPrefix(function.Name.Name, "Test") {
				names[function.Name.Name] = struct{}{}
			}
		}
	}
	return names
}

func markdownFileHasAnchor(t *testing.T, path, want string) bool {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Errorf("open report %s: %v", path, err)
		return false
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "#") {
			continue
		}
		heading := strings.TrimSpace(strings.TrimLeft(line, "#"))
		if markdownAnchor(heading) == want {
			return true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Errorf("read report %s: %v", path, err)
	}
	return false
}

func markdownAnchor(value string) string {
	var anchor strings.Builder
	lastHyphen := false
	for _, character := range strings.ToLower(value) {
		switch {
		case unicode.IsLetter(character) || unicode.IsDigit(character):
			anchor.WriteRune(character)
			lastHyphen = false
		case unicode.IsSpace(character) || character == '-':
			if anchor.Len() > 0 && !lastHyphen {
				anchor.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	return strings.TrimSuffix(anchor.String(), "-")
}
