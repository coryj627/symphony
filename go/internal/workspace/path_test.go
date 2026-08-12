package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPathWithinRejectsRootAndPrefixConfusion(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if !pathWithin(root, filepath.Join(root, "child")) {
		t.Fatal("direct child was rejected")
	}
	for _, candidate := range []string{root, filepath.Join(parent, "root-other"), parent} {
		if pathWithin(root, candidate) {
			t.Fatalf("pathWithin accepted %q", candidate)
		}
	}
}

func TestEnsureRejectsConfiguredWorkspaceRootChange(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	manager := testManager(t, root, nil)
	config := withoutHooks(testConfig(other))

	_, err := manager.Ensure(contextForTest(t), testIssue("10", "SYM-10"), config)
	if !errors.Is(err, ErrRootIdentity) {
		t.Fatalf("Ensure error = %v", err)
	}
	assertMissing(t, filepath.Join(root, "SYM-10"))
}

func TestNewCreatesNestedRootWithoutBroadPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "workspaces")
	manager := testManager(t, root, nil)
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	managerInfo, err := os.Stat(manager.root)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, managerInfo) {
		t.Fatalf("manager root %q is not configured root %q", manager.root, root)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("root mode = %v", info.Mode())
	}
}
