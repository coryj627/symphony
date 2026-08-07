package instance

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireRejectsSecondOwnerWithoutOverwritingMetadata(t *testing.T) {
	root := t.TempDir()
	info := testInfo(root, "data")
	first, err := Acquire(info)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Release() })
	metadataPath := filepath.Join(info.DataDir, "instance.json")
	before, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}

	secondInfo := info
	secondInfo.DataDir = filepath.Join(root, "other-data")
	if _, err := Acquire(secondInfo); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Acquire error = %v, want ErrAlreadyRunning", err)
	}
	after, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("failed acquisition overwrote owner metadata:\nbefore: %s\nafter: %s", before, after)
	}
	if _, err := os.Stat(filepath.Join(secondInfo.DataDir, "instance.json")); !os.IsNotExist(err) {
		t.Fatalf("failed acquisition wrote second metadata: %v", err)
	}
}

func TestAcquireWritesNonSecretOwnerMetadataAfterLocking(t *testing.T) {
	root := t.TempDir()
	info := testInfo(root, "data")
	before := time.Now().Add(-time.Second)

	lock, err := Acquire(info)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	contents, err := os.ReadFile(filepath.Join(info.DataDir, "instance.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		PID          int       `json:"pid"`
		StartedAt    time.Time `json:"started_at"`
		WorkflowID   string    `json:"workflow_id"`
		WorkflowPath string    `json:"workflow_path"`
	}
	if err := json.Unmarshal(contents, &metadata); err != nil {
		t.Fatalf("metadata is not valid JSON: %v\n%s", err, contents)
	}
	var fields map[string]any
	if err := json.Unmarshal(contents, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 4 {
		t.Fatalf("metadata fields = %#v, want only non-secret owner fields", fields)
	}
	if metadata.PID != os.Getpid() || metadata.WorkflowID != info.WorkflowID || metadata.WorkflowPath != info.WorkflowPath {
		t.Fatalf("metadata = %#v", metadata)
	}
	if metadata.StartedAt.Before(before) || metadata.StartedAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("started_at = %v, want acquisition time", metadata.StartedAt)
	}
	if len(contents) == 0 || contents[len(contents)-1] != '\n' {
		t.Fatalf("metadata should be a complete newline-terminated JSON document: %q", contents)
	}
}

func TestReleaseTruncatesOnlyOwnedMetadataAndPermitsNextOwner(t *testing.T) {
	root := t.TempDir()
	firstInfo := testInfo(root, "first-data")
	first, err := Acquire(firstInfo)
	if err != nil {
		t.Fatal(err)
	}
	firstMetadata := filepath.Join(firstInfo.DataDir, "instance.json")
	unrelated := filepath.Join(root, "unrelated.json")
	if err := os.WriteFile(unrelated, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(firstMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Size() != 0 {
		t.Fatalf("released metadata size = %d, want 0", stat.Size())
	}
	if contents, err := os.ReadFile(unrelated); err != nil || string(contents) != "keep me" {
		t.Fatalf("unrelated file changed: contents %q error %v", contents, err)
	}

	secondInfo := firstInfo
	secondInfo.DataDir = filepath.Join(root, "second-data")
	second, err := Acquire(secondInfo)
	if err != nil {
		t.Fatalf("next owner could not acquire released lock: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("releasing old owner twice: %v", err)
	}
	secondMetadata := filepath.Join(secondInfo.DataDir, "instance.json")
	if stat, err := os.Stat(secondMetadata); err != nil || stat.Size() == 0 {
		t.Fatalf("old owner truncated current owner metadata: stat %#v error %v", stat, err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireReleasesWorkflowLockWhenMetadataSetupFails(t *testing.T) {
	root := t.TempDir()
	info := testInfo(root, "blocked-data")
	if err := os.WriteFile(info.DataDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Acquire(info); err == nil {
		t.Fatal("Acquire succeeded when metadata directory could not be created")
	}
	if err := os.Remove(info.DataDir); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(info)
	if err != nil {
		t.Fatalf("metadata failure leaked workflow lock: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireRejectsMetadataSymlinkWithoutTruncatingTarget(t *testing.T) {
	root := t.TempDir()
	info := testInfo(root, "data")
	if err := os.MkdirAll(info.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "do-not-truncate")
	if err := os.WriteFile(target, []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(info.DataDir, "instance.json")
	mustSymlinkOrSkip(t, target, metadataPath)

	if _, err := Acquire(info); err == nil {
		t.Fatal("Acquire succeeded with symlinked metadata")
	}
	if contents, err := os.ReadFile(target); err != nil || string(contents) != "preserve me" {
		t.Fatalf("metadata target changed: contents %q error %v", contents, err)
	}
	if err := os.Remove(metadataPath); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(info)
	if err != nil {
		t.Fatalf("metadata validation failure leaked workflow lock: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseTruncatesAcquiredMetadataFileNotPathReplacement(t *testing.T) {
	root := t.TempDir()
	info := testInfo(root, "first-data")
	lock, err := Acquire(info)
	if err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(info.DataDir, "instance.json")
	ownedMetadata := filepath.Join(info.DataDir, "owned-instance.json")
	if err := os.Rename(metadataPath, ownedMetadata); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if stat, err := os.Stat(ownedMetadata); err != nil || stat.Size() != 0 {
		t.Fatalf("owned metadata was not truncated: stat %#v error %v", stat, err)
	}
	if contents, err := os.ReadFile(metadataPath); err != nil || string(contents) != "replacement" {
		t.Fatalf("replacement metadata changed: contents %q error %v", contents, err)
	}
	secondInfo := info
	secondInfo.DataDir = filepath.Join(root, "second-data")
	second, err := Acquire(secondInfo)
	if err != nil {
		t.Fatalf("metadata truncation failure leaked workflow lock: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func testInfo(root, dataLeaf string) Info {
	return Info{
		ID:           "same-instance",
		WorkflowID:   "same-workflow",
		WorkflowPath: filepath.Join(root, "repo", "WORKFLOW.md"),
		DataDir:      filepath.Join(root, dataLeaf),
		LockPath:     filepath.Join(root, "locks", "same-workflow.lock"),
	}
}
