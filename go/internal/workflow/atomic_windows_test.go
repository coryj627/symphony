//go:build windows

package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWindowsReplacesExistingDestination(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "WORKFLOW.md")
	source := filepath.Join(directory, "replacement.tmp")
	if err := os.WriteFile(destination, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("complete-new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(source, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != "complete-new" {
		t.Fatalf("replacement failed: %v", err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source still exists: %v", err)
	}
}
