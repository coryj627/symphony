//go:build darwin

package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOwnershipMarkerRejectsSamePathReplacementAfterOpen(t *testing.T) {
	workspace := t.TempDir()
	original := ownershipMarker{
		Version: markerVersion, IssueID: "issue-1", Identifier: "SYM-1", Key: "SYM-1",
		RootIdentity: "root-1", WorkspaceIdentity: "workspace-1",
	}
	if err := writeOwnershipMarker(workspace, original); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(workspace, markerFilename)
	openedPath := markerPath + ".opened"
	operations := defaultOwnershipMarkerReadOperations()
	operations.open = func(path string) (*os.File, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		if err := os.Rename(path, openedPath); err != nil {
			_ = file.Close()
			return nil, err
		}
		replacement := original
		replacement.IssueID = "attacker-controlled"
		if err := writeOwnershipMarker(workspace, replacement); err != nil {
			_ = file.Close()
			return nil, err
		}
		return file, nil
	}

	_, err := readOwnershipMarkerWithOperations(workspace, operations)
	if !errors.Is(err, ErrAmbiguousPath) {
		t.Fatalf("readOwnershipMarker error = %v, want ambiguous path", err)
	}
	assertExists(t, openedPath)
	assertExists(t, markerPath)
}
