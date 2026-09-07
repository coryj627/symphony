package workflow

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

var (
	errInjectedSaveFailure    = errors.New("injected save failure")
	errInjectedCleanupFailure = errors.New("injected cleanup failure")
)

const (
	rawSaveFaultCandidate = "---\ntracker:\n  kind: github\n  provider:\n    owner: openai\n    repository: symphony\npolling:\n  interval_ms: 41000\n---\nRaw save fault candidate.\n"
	rawSaveRecovery       = "---\ntracker:\n  kind: github\n  provider:\n    owner: openai\n    repository: symphony\npolling:\n  interval_ms: 42000\n---\nRaw save recovery.\n"
	structuredCandidate   = "---\ntracker:\n  kind: github\n  provider:\n    owner: openai\n    repository: symphony\npolling:\n  interval_ms: 45000\n---\nWork on {{ issue.identifier }}.\n"
	structuredRecovery    = "---\ntracker:\n  kind: github\n  provider:\n    owner: openai\n    repository: symphony\npolling:\n  interval_ms: 46000\n---\nWork on {{ issue.identifier }}.\n"
)

type saveFaultMode struct {
	name              string
	candidate         string
	recoveryCandidate string
	command           func(string) SaveCommand
	recoveryCommand   func(string) SaveCommand
}

func saveFaultModes() []saveFaultMode {
	return []saveFaultMode{
		{
			name:              "raw",
			candidate:         rawSaveFaultCandidate,
			recoveryCandidate: rawSaveRecovery,
			command: func(baseDigest string) SaveCommand {
				return SaveCommand{BaseDigest: baseDigest, RawSource: []byte(rawSaveFaultCandidate)}
			},
			recoveryCommand: func(baseDigest string) SaveCommand {
				return SaveCommand{BaseDigest: baseDigest, RawSource: []byte(rawSaveRecovery)}
			},
		},
		{
			name:              "structured",
			candidate:         structuredCandidate,
			recoveryCandidate: structuredRecovery,
			command: func(baseDigest string) SaveCommand {
				return SaveCommand{BaseDigest: baseDigest, Patch: &StructuredPatch{PollingIntervalMS: intPointer(45000)}}
			},
			recoveryCommand: func(baseDigest string) SaveCommand {
				return SaveCommand{BaseDigest: baseDigest, Patch: &StructuredPatch{PollingIntervalMS: intPointer(46000)}}
			},
		},
	}
}

type saveFaultOperations struct {
	point          string
	enabled        bool
	hits           int
	cleanupHits    int
	temporaryPaths []string
}

func (fault *saveFaultOperations) operations() atomicOperations {
	base := defaultAtomicOperations()
	operations := base
	operations.stat = func(path string) (os.FileInfo, error) {
		if fault.enabled && fault.point == "stat" {
			fault.hits++
			return nil, errInjectedSaveFailure
		}
		return base.stat(path)
	}
	operations.createTemp = func(directory, pattern string) (atomicFile, error) {
		if fault.enabled && fault.point == "create" {
			fault.hits++
			return nil, errInjectedSaveFailure
		}
		file, err := base.createTemp(directory, pattern)
		if err != nil {
			return nil, err
		}
		fault.temporaryPaths = append(fault.temporaryPaths, file.Name())
		return &saveFaultFile{atomicFile: file, fault: fault}, nil
	}
	operations.readFile = func(path string) ([]byte, error) {
		if fault.enabled && fault.point == "checked-digest-read" {
			fault.hits++
			return nil, errInjectedSaveFailure
		}
		return base.readFile(path)
	}
	operations.replace = func(temporary, destination string) error {
		if fault.enabled && fault.point == "replace" {
			fault.hits++
			return errInjectedSaveFailure
		}
		return base.replace(temporary, destination)
	}
	operations.syncDir = func(directory string) error {
		if fault.enabled && fault.point == "directory-sync" {
			fault.hits++
			return errInjectedSaveFailure
		}
		return base.syncDir(directory)
	}
	operations.remove = func(path string) error {
		if fault.enabled && fault.point == "cleanup-remove" {
			fault.cleanupHits++
			return errInjectedCleanupFailure
		}
		return base.remove(path)
	}
	return operations
}

type saveFaultFile struct {
	atomicFile
	fault *saveFaultOperations
}

func (file *saveFaultFile) Write(data []byte) (int, error) {
	if !file.fault.enabled {
		return file.atomicFile.Write(data)
	}
	switch file.fault.point {
	case "partial-write", "cleanup-remove":
		file.fault.hits++
		limit := 7
		if len(data) < limit {
			limit = len(data)
		}
		written, err := file.atomicFile.Write(data[:limit])
		return written, errors.Join(errInjectedSaveFailure, err)
	case "zero-write":
		file.fault.hits++
		return 0, nil
	default:
		return file.atomicFile.Write(data)
	}
}

func (file *saveFaultFile) Sync() error {
	if file.fault.enabled && file.fault.point == "sync" {
		file.fault.hits++
		return errInjectedSaveFailure
	}
	return file.atomicFile.Sync()
}

func (file *saveFaultFile) Chmod(mode os.FileMode) error {
	if file.fault.enabled && file.fault.point == "chmod" {
		file.fault.hits++
		return errInjectedSaveFailure
	}
	return file.atomicFile.Chmod(mode)
}

func (file *saveFaultFile) Close() error {
	err := file.atomicFile.Close()
	if file.fault.enabled && file.fault.point == "close" {
		file.fault.hits++
		return errors.Join(errInjectedSaveFailure, err)
	}
	return err
}

func TestSavePreReplacementFailuresPreserveStateAndRecover(t *testing.T) {
	points := []string{"stat", "create", "chmod", "partial-write", "zero-write", "sync", "close", "checked-digest-read", "replace"}
	for _, mode := range saveFaultModes() {
		for _, point := range points {
			t.Run(mode.name+"/existing/"+point, func(t *testing.T) {
				assertSavePreReplacementFailure(t, mode, point, true)
			})
		}
	}
	for _, point := range points {
		t.Run("raw/absent/"+point, func(t *testing.T) {
			assertSavePreReplacementFailure(t, saveFaultModes()[0], point, false)
		})
	}
}

func assertSavePreReplacementFailure(t *testing.T, mode saveFaultMode, point string, existing bool) {
	t.Helper()
	initial := ""
	if existing {
		initial = validWorkflowSource
	}
	fault := &saveFaultOperations{point: point, enabled: true}
	store, path := newSaveFaultStore(t, initial, fault.operations())
	baseDigest := ""
	var previous Snapshot
	if existing {
		var err error
		previous, err = store.Load(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		baseDigest = previous.Digest
	}
	got, err := store.Save(context.Background(), mode.command(baseDigest))
	if err == nil {
		t.Fatalf("%s failure returned success", point)
	}
	if point == "zero-write" {
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("expected short-write error, got %v", err)
		}
	} else if !errors.Is(err, errInjectedSaveFailure) {
		t.Fatalf("expected injected failure, got %v", err)
	}
	if fault.hits == 0 {
		t.Fatalf("%s injection was not reached", point)
	}
	if got.Digest != "" {
		t.Fatalf("failed save returned candidate snapshot %q", got.Digest)
	}
	assertSaveDestination(t, path, initial, existing)
	assertSaveCurrent(t, store, previous, existing)
	assertNoSaveChange(t, store.Changes(), mode.candidate)
	assertNoSaveTemporaryFiles(t, path)

	fault.enabled = false
	recovered, recoveryErr := store.Save(context.Background(), mode.command(baseDigest))
	if recoveryErr != nil {
		t.Fatalf("save did not recover after removing %s fault: %v", point, recoveryErr)
	}
	assertInstalledSave(t, store, path, recovered, mode.candidate)
}

func TestSaveDirectorySyncFailureInstallsCompletedSaveAndAllowsNextSave(t *testing.T) {
	cases := []struct {
		mode     saveFaultMode
		existing bool
	}{
		{mode: saveFaultModes()[0], existing: true},
		{mode: saveFaultModes()[1], existing: true},
		{mode: saveFaultModes()[0], existing: false},
	}
	for _, test := range cases {
		destination := "absent"
		if test.existing {
			destination = "existing"
		}
		t.Run(test.mode.name+"/"+destination, func(t *testing.T) {
			initial := ""
			if test.existing {
				initial = validWorkflowSource
			}
			fault := &saveFaultOperations{point: "directory-sync", enabled: true}
			store, path := newSaveFaultStore(t, initial, fault.operations())
			baseDigest := ""
			if test.existing {
				loaded, err := store.Load(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				baseDigest = loaded.Digest
			}
			completed, err := store.Save(context.Background(), test.mode.command(baseDigest))
			if !errors.Is(err, ErrDurabilityUncertain) || !errors.Is(err, errInjectedSaveFailure) {
				t.Fatalf("expected durability uncertainty wrapping injected failure, got %v", err)
			}
			if fault.hits == 0 {
				t.Fatal("directory-sync injection was not reached")
			}
			assertInstalledSave(t, store, path, completed, test.mode.candidate)

			fault.enabled = false
			next, nextErr := store.Save(context.Background(), test.mode.recoveryCommand(completed.Digest))
			if nextErr != nil {
				t.Fatalf("save using completed digest failed: %v", nextErr)
			}
			assertInstalledSave(t, store, path, next, test.mode.recoveryCandidate)
		})
	}
}

func TestSaveCleanupRemovalFailurePreservesPrimaryErrorAndOldDestination(t *testing.T) {
	for _, mode := range saveFaultModes() {
		t.Run(mode.name, func(t *testing.T) {
			fault := &saveFaultOperations{point: "cleanup-remove", enabled: true}
			store, path := newSaveFaultStore(t, validWorkflowSource, fault.operations())
			previous, err := store.Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				for _, temporaryPath := range fault.temporaryPaths {
					_ = os.Remove(temporaryPath)
				}
			})

			got, saveErr := store.Save(context.Background(), mode.command(previous.Digest))
			if !errors.Is(saveErr, errInjectedSaveFailure) || !errors.Is(saveErr, errInjectedCleanupFailure) {
				t.Fatalf("expected primary and cleanup failures, got %v", saveErr)
			}
			if got.Digest != "" {
				t.Fatalf("failed save returned candidate snapshot %q", got.Digest)
			}
			if fault.hits == 0 {
				t.Fatal("primary write injection was not reached")
			}
			if fault.cleanupHits != 1 {
				t.Fatalf("cleanup attempts = %d, want 1", fault.cleanupHits)
			}
			assertSaveDestination(t, path, validWorkflowSource, true)
			assertSaveCurrent(t, store, previous, true)
			assertNoSaveChange(t, store.Changes(), mode.candidate)
			matches := saveTemporaryFiles(t, path)
			if len(matches) != 1 || !sameSaveFile(t, matches[0], fault.temporaryPaths[0]) {
				t.Fatalf("remaining temporary files = %v, want exact injected file %q", matches, fault.temporaryPaths[0])
			}

			fault.enabled = false
			recovered, recoveryErr := store.Save(context.Background(), mode.command(previous.Digest))
			if recoveryErr != nil {
				t.Fatalf("save did not recover after removing cleanup fault: %v", recoveryErr)
			}
			assertInstalledSave(t, store, path, recovered, mode.candidate)
		})
	}
}

func newSaveFaultStore(t *testing.T, source string, operations atomicOperations) (*FileStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if source != "" {
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := newStoreWithAtomicOperations(
		context.Background(),
		path,
		os.LookupEnv,
		func(EffectiveConfig) []FieldError { return nil },
		operations,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

func assertSaveDestination(t *testing.T, path, expected string, exists bool) {
	t.Helper()
	got, err := os.ReadFile(path)
	if !exists {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed save created absent destination: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("read destination after failed save: %v", err)
	}
	if string(got) != expected {
		t.Fatalf("failed save changed destination bytes:\n%s", got)
	}
}

func assertSaveCurrent(t *testing.T, store *FileStore, expected Snapshot, exists bool) {
	t.Helper()
	current, ok := store.Current()
	if ok != exists {
		t.Fatalf("current snapshot presence = %v, want %v", ok, exists)
	}
	if exists && (current.Digest != expected.Digest || current.Source != expected.Source) {
		t.Fatalf("failed save replaced current snapshot: digest=%q", current.Digest)
	}
}

func assertNoSaveChange(t *testing.T, changes <-chan Change, candidate string) {
	t.Helper()
	select {
	case change := <-changes:
		if change.Digest == digestSource([]byte(candidate)) {
			t.Fatalf("failed save published candidate change %q", change.Digest)
		}
		t.Fatalf("failed save unexpectedly published change %q", change.Digest)
	default:
	}
}

func assertNoSaveTemporaryFiles(t *testing.T, path string) {
	t.Helper()
	if matches := saveTemporaryFiles(t, path); len(matches) != 0 {
		t.Fatalf("failed save left temporary files: %v", matches)
	}
}

func saveTemporaryFiles(t *testing.T, path string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func sameSaveFile(t *testing.T, first, second string) bool {
	t.Helper()
	firstInfo, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(firstInfo, secondInfo)
}

func assertInstalledSave(t *testing.T, store *FileStore, path string, snapshot Snapshot, expected string) {
	t.Helper()
	if snapshot.Source != expected || snapshot.Digest != digestSource([]byte(expected)) {
		t.Fatalf("save returned unexpected snapshot: digest=%q source=%q", snapshot.Digest, snapshot.Source)
	}
	assertSaveDestination(t, path, expected, true)
	current, ok := store.Current()
	if !ok || current.Digest != snapshot.Digest || current.Source != expected {
		t.Fatalf("save did not install completed snapshot: present=%v digest=%q", ok, current.Digest)
	}
	change := awaitChange(t, store.Changes())
	if change.Digest != snapshot.Digest || change.Snapshot.Source != expected || !change.Validation.Valid {
		t.Fatalf("save published unexpected change: digest=%q valid=%v", change.Digest, change.Validation.Valid)
	}
}
