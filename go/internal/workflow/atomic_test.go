package workflow

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var injectedAtomicFailure = errors.New("injected atomic failure")

type atomicFaultFile struct {
	file       atomicFile
	writeErr   bool
	syncErr    bool
	closeErr   bool
	shortWrite bool
}

func (file *atomicFaultFile) Write(data []byte) (int, error) {
	if file.writeErr {
		return 0, injectedAtomicFailure
	}
	if file.shortWrite && len(data) > 2 {
		data = data[:2]
	}
	return file.file.Write(data)
}
func (file *atomicFaultFile) Sync() error {
	if file.syncErr {
		return injectedAtomicFailure
	}
	return file.file.Sync()
}
func (file *atomicFaultFile) Chmod(mode os.FileMode) error { return file.file.Chmod(mode) }
func (file *atomicFaultFile) Close() error {
	underlying := file.file.Close()
	if file.closeErr {
		return injectedAtomicFailure
	}
	return underlying
}
func (file *atomicFaultFile) Name() string { return file.file.Name() }

func atomicFixture(t *testing.T) (string, []byte, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	original := []byte("original complete workflow\n")
	replacement := []byte(strings.Repeat("validated replacement bytes\n", 20))
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}
	return path, original, replacement
}

func TestAtomicPreReplaceFailuresPreserveOriginalCompletely(t *testing.T) {
	for _, point := range []string{"create", "write", "sync", "close", "replace"} {
		t.Run(point, func(t *testing.T) {
			path, original, replacement := atomicFixture(t)
			ops := defaultAtomicOperations()
			switch point {
			case "create":
				ops.createTemp = func(string, string) (atomicFile, error) { return nil, injectedAtomicFailure }
			case "write", "sync", "close":
				create := ops.createTemp
				ops.createTemp = func(directory, pattern string) (atomicFile, error) {
					file, err := create(directory, pattern)
					if err != nil {
						return nil, err
					}
					return &atomicFaultFile{file: file, writeErr: point == "write", syncErr: point == "sync", closeErr: point == "close"}, nil
				}
			case "replace":
				ops.replace = func(string, string) error { return injectedAtomicFailure }
			}
			err := atomicReplace(path, replacement, ops)
			if !errors.Is(err, injectedAtomicFailure) {
				t.Fatalf("expected injected failure, got %v", err)
			}
			got, readErr := os.ReadFile(path)
			if readErr != nil || string(got) != string(original) {
				t.Fatalf("%s failure lost or partially replaced original: %v", point, readErr)
			}
			matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".WORKFLOW.md.*.tmp"))
			if len(matches) != 0 {
				t.Fatalf("%s failure left temp files", point)
			}
		})
	}
}

func TestAtomicDirectorySyncFailureReturnsDurabilityUncertainAfterCompleteReplace(t *testing.T) {
	path, _, replacement := atomicFixture(t)
	ops := defaultAtomicOperations()
	ops.syncDir = func(string) error { return injectedAtomicFailure }
	err := atomicReplace(path, replacement, ops)
	if !errors.Is(err, ErrDurabilityUncertain) || !errors.Is(err, injectedAtomicFailure) {
		t.Fatalf("expected durability uncertainty wrapping fault, got %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != string(replacement) {
		t.Fatalf("directory sync failure did not leave complete new file visible: %v", readErr)
	}
}

func TestAtomicCompleteWriteLoopHandlesShortWrites(t *testing.T) {
	path, _, replacement := atomicFixture(t)
	ops := defaultAtomicOperations()
	create := ops.createTemp
	ops.createTemp = func(directory, pattern string) (atomicFile, error) {
		file, err := create(directory, pattern)
		if err != nil {
			return nil, err
		}
		return &atomicFaultFile{file: file, shortWrite: true}, nil
	}
	if err := atomicReplace(path, replacement, ops); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(replacement) {
		t.Fatalf("short writes produced partial destination: %v", err)
	}
}

func TestAtomicPreservesExistingPermissionsAndRestrictsNewFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows ACLs are not represented by os.FileMode permission bits")
	}
	t.Run("existing", func(t *testing.T) {
		path, _, replacement := atomicFixture(t)
		if err := atomicReplace(path, replacement, defaultAtomicOperations()); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o640 {
			t.Fatalf("permissions changed: %o", got)
		}
	})
	t.Run("new", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "WORKFLOW.md")
		if err := atomicReplace(path, []byte(validWorkflowSource), defaultAtomicOperations()); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("new permissions are not restrictive: %o", got)
		}
	})
}

func TestAtomicWindowsSourceUsesReplaceExistingWithoutDeleteGap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("source structural check is for non-Windows cross-build hosts")
	}
	source, err := os.ReadFile("atomic_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if !strings.Contains(text, "MOVEFILE_REPLACE_EXISTING") || strings.Contains(text, "os.Remove") || strings.Contains(text, "windows.DeleteFile") {
		t.Fatal("Windows replacement must replace existing atomically without deleting first")
	}
}

var _ io.Writer = (*atomicFaultFile)(nil)
