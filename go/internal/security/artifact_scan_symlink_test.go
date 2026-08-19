//go:build !windows

package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanaryArtifactScannerDoesNotFollowSymbolicLinks(t *testing.T) {
	canary := NewCanary(t)
	scanner, err := NewArtifactScanner([]byte(canary.Value))
	if err != nil {
		t.Fatalf("NewArtifactScanner() failed: %v", err)
	}

	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(target, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(nested, "linked.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create symbolic-link fixture: %v", err)
	}
	if _, err := scanner.Scan(PathArtifact("linked artifacts", root)); err == nil {
		t.Fatal("Scan() followed or ignored a symbolic link")
	} else if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Scan() did not explicitly reject the nested symbolic link: %v", err)
	}
}
