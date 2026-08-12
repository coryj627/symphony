package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/coryj627/symphony/go/internal/domain"
)

func TestRemoveDeletesOnlyValidatedOwnedWorkspace(t *testing.T) {
	root := t.TempDir()
	hooks := &fakeHookRunner{results: map[domain.Hook]HookResult{domain.HookBeforeRemove: {ExitCode: 3, Err: errors.New("warning only")}}}
	manager := testManager(t, root, hooks)
	config := testConfig(root)
	issue := testIssue("5", "SYM-5")
	workspace, err := manager.Ensure(contextForTest(t), issue, config)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(workspace.Path, "agent-output"), "remove with owned workspace")

	if err := manager.Remove(contextForTest(t), issue, config); err != nil {
		t.Fatal(err)
	}
	assertMissing(t, workspace.Path)
	if hooks.count(domain.HookBeforeRemove) != 1 {
		t.Fatalf("before_remove calls = %d", hooks.count(domain.HookBeforeRemove))
	}
	if err := manager.Remove(contextForTest(t), issue, config); err != nil {
		t.Fatalf("idempotent remove = %v", err)
	}
	assertExists(t, root)
}

func TestRemovePreservesUnownedWorkspace(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "SYM-6")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(path, "user-data"), "preserve")
	hooks := &fakeHookRunner{results: map[domain.Hook]HookResult{}}
	manager := testManager(t, root, hooks)
	issue := testIssue("6", "SYM-6")
	config := testConfig(root)
	if _, err := manager.Ensure(contextForTest(t), issue, config); err != nil {
		t.Fatal(err)
	}

	if err := manager.Remove(contextForTest(t), issue, config); err != nil {
		t.Fatal(err)
	}
	assertExists(t, path)
	assertExists(t, filepath.Join(path, "user-data"))
	if hooks.count(domain.HookBeforeRemove) != 0 {
		t.Fatal("before_remove ran for unowned workspace")
	}
}

func TestRemoveRevalidatesAfterHookAndPreservesSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	managerHooks := &fakeHookRunner{results: map[domain.Hook]HookResult{}}
	manager := testManager(t, root, managerHooks)
	issue := testIssue("7", "SYM-7")
	config := testConfig(root)
	workspace, err := manager.Ensure(contextForTest(t), issue, config)
	if err != nil {
		t.Fatal(err)
	}
	moved := workspace.Path + "-owned"
	managerHooks.before = func(hook domain.Hook, _ domain.Workspace) {
		if hook.Name != domain.HookNameBeforeRemove {
			return
		}
		mustRename(t, workspace.Path, moved)
		mustSymlinkOrSkip(t, outside, workspace.Path)
	}

	err = manager.Remove(contextForTest(t), issue, config)
	if !errors.Is(err, ErrAmbiguousPath) && !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("Remove error = %v", err)
	}
	assertExists(t, outside)
	assertExists(t, moved)
}

func TestRemoveRejectsChangedRootIdentity(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspaces")
	manager := testManager(t, root, nil)
	config := withoutHooks(testConfig(root))
	issue := testIssue("8", "SYM-8")
	workspace, err := manager.Ensure(contextForTest(t), issue, config)
	if err != nil {
		t.Fatal(err)
	}
	movedRoot := root + "-original"
	mustRename(t, root, movedRoot)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}

	err = manager.Remove(contextForTest(t), issue, config)
	if !errors.Is(err, ErrRootIdentity) {
		t.Fatalf("Remove error = %v", err)
	}
	assertExists(t, filepath.Join(movedRoot, filepath.Base(workspace.Path)))
	assertExists(t, root)
}

func TestRemovePreservesAmbiguousOwnershipMarker(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string) string
	}{
		{
			name: "malformed",
			mutate: func(t *testing.T, markerPath string) string {
				t.Helper()
				mustWriteFile(t, markerPath, "not JSON")
				return ""
			},
		},
		{
			name: "symlink",
			mutate: func(t *testing.T, markerPath string) string {
				t.Helper()
				target := filepath.Join(t.TempDir(), "outside-marker")
				mustWriteFile(t, target, `{"version":1}`)
				mustSymlinkOrSkip(t, target, markerPath)
				return target
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			manager := testManager(t, root, nil)
			config := withoutHooks(testConfig(root))
			issue := testIssue("9", "SYM-9")
			workspace, err := manager.Ensure(contextForTest(t), issue, config)
			if err != nil {
				t.Fatal(err)
			}
			markerPath := filepath.Join(workspace.Path, markerFilename)
			if err := os.Remove(markerPath); err != nil {
				t.Fatal(err)
			}
			target := test.mutate(t, markerPath)

			err = manager.Remove(contextForTest(t), issue, config)
			if !errors.Is(err, ErrAmbiguousPath) {
				t.Fatalf("Remove error = %v", err)
			}
			assertExists(t, workspace.Path)
			if target != "" {
				assertExists(t, target)
			}
		})
	}
}
