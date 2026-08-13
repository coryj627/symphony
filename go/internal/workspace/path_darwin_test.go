//go:build darwin

package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureRejectsSymlinkEscapeAndPreservesTarget(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	path := filepath.Join(root, "GH-42")
	mustSymlinkOrSkip(t, outside, path)
	manager := testManager(t, root, nil)

	_, err := manager.Ensure(contextForTest(t), testIssue("42", "GH-42"), withoutHooks(testConfig(root)))
	if !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("Ensure error = %v", err)
	}
	assertExists(t, outside)
}

func TestNewCanonicalizesSymlinkedRootAndSecuresCreatedPaths(t *testing.T) {
	parent := t.TempDir()
	realRoot := filepath.Join(parent, "real", "workspaces")
	if err := os.MkdirAll(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(parent, "linked")
	mustSymlinkOrSkip(t, realRoot, linkedRoot)
	manager := testManager(t, linkedRoot, nil)
	workspace, err := manager.Ensure(contextForTest(t), testIssue("43", "GH-43"), withoutHooks(testConfig(linkedRoot)))
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	if manager.root != canonicalRoot || workspace.Root != canonicalRoot || filepath.Dir(workspace.Path) != canonicalRoot {
		t.Fatalf("root was not canonicalized: manager=%q workspace=%#v", manager.root, workspace)
	}
	for path, want := range map[string]os.FileMode{
		workspace.Path: 0o700,
		filepath.Join(workspace.Path, markerFilename): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode = %o, want %o", path, got, want)
		}
	}
}
