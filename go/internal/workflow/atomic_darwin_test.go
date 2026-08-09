//go:build darwin

package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicDarwinSyncsParentDirectory(t *testing.T) {
	directory := t.TempDir()
	if err := syncParentDirectory(directory); err != nil {
		t.Fatalf("macOS parent directory sync failed: %v", err)
	}
	path := filepath.Join(directory, "WORKFLOW.md")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(filepath.Join(directory, "missing"), path); err == nil {
		t.Fatal("replace unexpectedly accepted a missing source")
	}
}
