package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coryj627/symphony/go/internal/domain"
)

func TestEnsureCreatesOwnedWorkspaceOnceAndRunsAfterCreateOnce(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspaces")
	hooks := &fakeHookRunner{results: map[domain.Hook]HookResult{}}
	manager := testManager(t, root, hooks)
	issue := testIssue("opaque-1", "SYM-1")
	config := testConfig(root)

	first, err := manager.Ensure(contextForTest(t), issue, config)
	if err != nil {
		t.Fatal(err)
	}
	if !first.CreatedNow || !first.Owned || first.Key != "SYM-1" || first.Path != filepath.Join(manager.root, "SYM-1") {
		t.Fatalf("first workspace = %#v", first)
	}
	if hooks.count(domain.HookAfterCreate) != 1 {
		t.Fatalf("after_create calls = %d", hooks.count(domain.HookAfterCreate))
	}
	marker, err := readOwnershipMarker(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	if marker.IssueID != issue.ID || marker.Identifier != issue.Identifier || marker.Key != first.Key || marker.RootIdentity != first.RootIdentity {
		t.Fatalf("ownership marker = %#v", marker)
	}

	second, err := manager.Ensure(contextForTest(t), issue, config)
	if err != nil {
		t.Fatal(err)
	}
	if second.CreatedNow || !second.Owned || second.Path != first.Path {
		t.Fatalf("reused workspace = %#v", second)
	}
	if hooks.count(domain.HookAfterCreate) != 1 {
		t.Fatalf("after_create reran: %d", hooks.count(domain.HookAfterCreate))
	}
}

func TestEnsureRejectsExistingFileAndPreservesIt(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "SYM-1")
	mustWriteFile(t, path, "preserve")
	manager := testManager(t, root, nil)

	_, err := manager.Ensure(contextForTest(t), testIssue("1", "SYM-1"), withoutHooks(testConfig(root)))
	if !errors.Is(err, ErrExistingNonDirectory) {
		t.Fatalf("Ensure error = %v", err)
	}
	contents, readErr := os.ReadFile(path)
	if readErr != nil || string(contents) != "preserve" {
		t.Fatalf("existing file changed: %q, %v", contents, readErr)
	}
}

func TestRunHookRevalidatesWorkspaceAndReturnsFailure(t *testing.T) {
	root := t.TempDir()
	hooks := &fakeHookRunner{results: map[domain.Hook]HookResult{
		domain.HookBeforeRun: {ExitCode: 2, Err: errors.New("hook failed")},
	}}
	manager := testManager(t, root, hooks)
	issue := testIssue("run-hook", "SYM-RUN-HOOK")
	workspace, err := manager.Ensure(contextForTest(t), issue, withoutHooks(testConfig(root)))
	if err != nil {
		t.Fatal(err)
	}
	err = manager.RunHook(contextForTest(t), domain.HookBeforeRun.WithScript("before"), workspace, time.Second)
	if err == nil || !strings.Contains(err.Error(), "before_run") {
		t.Fatalf("RunHook error = %v", err)
	}
	workspace.PathIdentity = "changed"
	if err := manager.RunHook(contextForTest(t), domain.HookAfterRun.WithScript("after"), workspace, time.Second); !errors.Is(err, ErrAmbiguousPath) {
		t.Fatalf("changed workspace error = %v", err)
	}
}

func TestRunHookBlankScriptNeedsNoProcessRunner(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, nil)
	workspace, err := manager.Ensure(contextForTest(t), testIssue("blank-hook", "SYM-BLANK"), withoutHooks(testConfig(root)))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RunHook(contextForTest(t), domain.HookBeforeRun, workspace, 0); err != nil {
		t.Fatalf("blank hook failed: %v", err)
	}
}

func TestEnsureReusesUnmarkedDirectoryWithoutTakingOwnership(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "SYM-2")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(path, "user-data"), "preserve")
	manager := testManager(t, root, nil)

	workspace, err := manager.Ensure(contextForTest(t), testIssue("2", "SYM-2"), withoutHooks(testConfig(root)))
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Owned || workspace.CreatedNow {
		t.Fatalf("unmarked workspace was adopted: %#v", workspace)
	}
	assertMissing(t, filepath.Join(path, markerFilename))
}

func TestEnsureRejectsOwnershipCollision(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, nil)
	config := withoutHooks(testConfig(root))
	if _, err := manager.Ensure(contextForTest(t), testIssue("first", "SYM-3"), config); err != nil {
		t.Fatal(err)
	}

	_, err := manager.Ensure(contextForTest(t), testIssue("second", "SYM-3"), config)
	if !errors.Is(err, ErrWorkspaceKeyCollision) {
		t.Fatalf("collision error = %v", err)
	}
	assertExists(t, filepath.Join(root, "SYM-3"))
}

func TestAfterCreateFailureRemovesOnlyStillEmptyOwnedWorkspace(t *testing.T) {
	for _, test := range []struct {
		name         string
		populate     bool
		wantPreserve bool
	}{
		{name: "empty", wantPreserve: false},
		{name: "populated", populate: true, wantPreserve: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			hooks := &fakeHookRunner{
				results: map[domain.Hook]HookResult{domain.HookAfterCreate: {ExitCode: 2, Err: errors.New("hook failed")}},
			}
			if test.populate {
				hooks.before = func(hook domain.Hook, workspace domain.Workspace) {
					if hook.Name == domain.HookNameAfterCreate {
						mustWriteFile(t, filepath.Join(workspace.Path, "created-by-hook"), "preserve")
					}
				}
			}
			manager := testManager(t, root, hooks)
			path := filepath.Join(root, "SYM-4")

			_, err := manager.Ensure(contextForTest(t), testIssue("4", "SYM-4"), testConfig(root))
			if err == nil {
				t.Fatal("Ensure succeeded after hook failure")
			}
			if test.wantPreserve {
				assertExists(t, path)
				assertExists(t, filepath.Join(path, "created-by-hook"))
			} else {
				assertMissing(t, path)
			}
		})
	}
}
