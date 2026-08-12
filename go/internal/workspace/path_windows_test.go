//go:build windows

package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureRejectsWindowsReparseEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	path := filepath.Join(root, "GH-42")
	mustSymlinkOrSkip(t, outside, path)
	manager := testManager(t, root, nil)

	_, err := manager.Ensure(contextForTest(t), testIssue("42", "GH-42"), withoutHooks(testConfig(root)))
	if !errors.Is(err, ErrOutsideRoot) && !errors.Is(err, ErrAmbiguousPath) {
		t.Fatalf("Ensure error = %v", err)
	}
	assertExists(t, outside)
}

func TestEnsureRejectsCaseInsensitiveKeyCollision(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, nil)
	config := withoutHooks(testConfig(root))
	if _, err := manager.Ensure(contextForTest(t), testIssue("first", "SYM-A"), config); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "sym-a")); err != nil {
		t.Skip("test volume is case-sensitive")
	}

	_, err := manager.Ensure(contextForTest(t), testIssue("second", "sym-a"), config)
	if !errors.Is(err, ErrWorkspaceKeyCollision) {
		t.Fatalf("collision error = %v", err)
	}
}
