package instance

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveIsStableAcrossSymlinkedWorkflowPath(t *testing.T) {
	root := t.TempDir()
	realPath := filepath.Join(root, "repo", "WORKFLOW.md")
	mustWriteWorkflow(t, realPath)
	link := filepath.Join(root, "workflow-link.md")
	mustSymlinkOrSkip(t, realPath, link)

	a, err := Resolve(realPath, "github:coryj627/symphony", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Resolve(link, "github:coryj627/symphony", "")
	if err != nil {
		t.Fatal(err)
	}

	if a.ID != b.ID || a.WorkflowID != b.WorkflowID || a.WorkflowPath != b.WorkflowPath {
		t.Fatalf("identity mismatch: %#v %#v", a, b)
	}
	wantPath, err := filepath.EvalSymlinks(realPath)
	if err != nil {
		t.Fatal(err)
	}
	if a.WorkflowPath != wantPath {
		t.Fatalf("WorkflowPath = %q, want %q", a.WorkflowPath, wantPath)
	}
}

func TestResolveIsStableAcrossCaseAliasedWorkflowPath(t *testing.T) {
	root := t.TempDir()
	realPath := filepath.Join(root, "Repository", "WORKFLOW.md")
	mustWriteWorkflow(t, realPath)
	aliasPath := filepath.Join(root, "repository", "workflow.md")
	if _, err := os.Stat(aliasPath); err != nil {
		if os.IsNotExist(err) {
			t.Skip("test volume is case-sensitive")
		}
		t.Fatal(err)
	}

	real, err := Resolve(realPath, "github:coryj627/symphony", "")
	if err != nil {
		t.Fatal(err)
	}
	alias, err := Resolve(aliasPath, "github:coryj627/symphony", "")
	if err != nil {
		t.Fatal(err)
	}

	if real.WorkflowPath != alias.WorkflowPath || real.WorkflowID != alias.WorkflowID || real.ID != alias.ID {
		t.Fatalf("case aliases produced different identities: real %#v alias %#v", real, alias)
	}
}

func TestResolveMissingLeafIsStableAcrossCaseAliasedParent(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "Repository")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(root, "repository")
	if _, err := os.Stat(aliasParent); err != nil {
		if os.IsNotExist(err) {
			t.Skip("test volume is case-sensitive")
		}
		t.Fatal(err)
	}

	real, err := Resolve(filepath.Join(realParent, "WORKFLOW.md"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	alias, err := Resolve(filepath.Join(aliasParent, "WORKFLOW.md"), "", "")
	if err != nil {
		t.Fatal(err)
	}

	if real.WorkflowPath != alias.WorkflowPath || real.WorkflowID != alias.WorkflowID {
		t.Fatalf("case-aliased parents produced different missing-leaf identities: real %#v alias %#v", real, alias)
	}
}

func TestResolveCanonicalizesMissingLeafThroughSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "repo")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "linked-repo")
	mustSymlinkOrSkip(t, realParent, linkedParent)

	info, err := Resolve(filepath.Join(linkedParent, "WORKFLOW.md"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	canonicalParent, err := filepath.EvalSymlinks(realParent)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(canonicalParent, "WORKFLOW.md")
	if info.WorkflowPath != want {
		t.Fatalf("WorkflowPath = %q, want %q", info.WorkflowPath, want)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("Resolve created missing workflow: %v", err)
	}
}

func TestResolveRejectsMissingIntermediateParent(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "missing", "WORKFLOW.md")

	_, err := Resolve(workflowPath, "", "")
	if err == nil {
		t.Fatal("Resolve succeeded with a missing intermediate parent")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error = %v, want a not-exist error", err)
	}
	if _, statErr := os.Stat(filepath.Dir(workflowPath)); !os.IsNotExist(statErr) {
		t.Fatalf("Resolve created intermediate parent: %v", statErr)
	}
}

func TestResolveNormalizesTrackerScopeAndKeepsWorkflowResourcesIndependent(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	mustWriteWorkflow(t, workflowPath)

	canonical, err := Resolve(workflowPath, "github:coryj627/symphony", "")
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := Resolve(workflowPath, "  GitHub:CoryJ627/Symphony\t", "")
	if err != nil {
		t.Fatal(err)
	}
	otherScope, err := Resolve(workflowPath, "linear:symphony", filepath.Join(t.TempDir(), "custom", "..", "data"))
	if err != nil {
		t.Fatal(err)
	}

	if canonical.ID != normalized.ID {
		t.Fatalf("normalized scope changed ID: %q != %q", canonical.ID, normalized.ID)
	}
	if canonical.ID == otherScope.ID {
		t.Fatalf("different tracker scopes share ID %q", canonical.ID)
	}
	if canonical.WorkflowID != otherScope.WorkflowID || canonical.LockPath != otherScope.LockPath {
		t.Fatalf("workflow resources depend on instance settings: %#v %#v", canonical, otherScope)
	}
	if strings.Contains(canonical.WorkflowID, "github") || strings.Contains(canonical.LockPath, "github") {
		t.Fatalf("tracker scope leaked into workflow resources: %#v", canonical)
	}
}

func TestIdentityHashesExactCanonicalPathBytes(t *testing.T) {
	const canonicalPath = "/canonical/repo/WORKFLOW.md"
	if got := workflowID(canonicalPath); got != "24095ec6e91f672c6f21f6b10a0b1c1e" {
		t.Fatalf("workflowID = %q", got)
	}
	if got := instanceID(canonicalPath, "github:coryj627/symphony"); got != "c2624cca35b3d06846160a0bd273fbc8dba2fe45dbb0c3353f0219f0a5b8146e" {
		t.Fatalf("instanceID = %q", got)
	}
}

func TestResolveSelectsDefaultAndExplicitDataDirectories(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	mustWriteWorkflow(t, workflowPath)

	defaultInfo, err := Resolve(workflowPath, "github:coryj627/symphony", "")
	if err != nil {
		t.Fatal(err)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(configDir, "Symphony", "instances", defaultInfo.ID); defaultInfo.DataDir != want {
		t.Fatalf("DataDir = %q, want %q", defaultInfo.DataDir, want)
	}
	if want := filepath.Join(configDir, "Symphony", "locks", defaultInfo.WorkflowID+".lock"); defaultInfo.LockPath != want {
		t.Fatalf("LockPath = %q, want %q", defaultInfo.LockPath, want)
	}

	explicit := filepath.Join(t.TempDir(), "nested", "..", "instance-data")
	explicitInfo, err := Resolve(workflowPath, "github:coryj627/symphony", explicit)
	if err != nil {
		t.Fatal(err)
	}
	wantDataDir, err := filepath.Abs(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if explicitInfo.DataDir != filepath.Clean(wantDataDir) {
		t.Fatalf("explicit DataDir = %q, want %q", explicitInfo.DataDir, filepath.Clean(wantDataDir))
	}
	if explicitInfo.ID != defaultInfo.ID || explicitInfo.LockPath != defaultInfo.LockPath {
		t.Fatalf("explicit data directory changed identity or lock: %#v %#v", defaultInfo, explicitInfo)
	}
}

func mustWriteWorkflow(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ntracker:\n  kind: github\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustSymlinkOrSkip(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
}
