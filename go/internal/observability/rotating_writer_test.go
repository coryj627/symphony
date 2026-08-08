package observability

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRotatingWriterKeepsExactBoundaryActive(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "symphony.jsonl")
	writer, err := newRotatingWriter(path, 10, 5)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteLine([]byte("12345")); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteLine([]byte("67890")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact-boundary write rotated: %v", err)
	}
	if err := writer.WriteLine([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, path+".1", "1234567890")
	assertFileContents(t, path, "x")
}

func TestRotatingWriterKeepsExactTenMiBBoundaryActive(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "symphony.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, defaultActiveLogSize-1); err != nil {
		t.Fatal(err)
	}
	writer, err := newRotatingWriter(path, defaultActiveLogSize, defaultLogArchives)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteLine([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact 10 MiB boundary rotated: %v", err)
	}
	if err := writer.WriteLine([]byte("y")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != defaultActiveLogSize {
		t.Fatalf("archive size = %d, want %d", info.Size(), defaultActiveLogSize)
	}
}

func TestRotatingWriterRetainsFiveArchivesNewestFirst(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "symphony.jsonl")
	writer, err := newRotatingWriter(path, 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		if err := writer.WriteLine([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, path, "g")
	for archive, want := range map[int]string{1: "f", 2: "e", 3: "d", 4: "c", 5: "b"} {
		assertFileContents(t, path+"."+string(rune('0'+archive)), want)
	}
}

func TestRotatingWriterRejectsSymlinkActiveFile(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "symphony.jsonl")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := newRotatingWriter(path, 10, 5); err == nil {
		t.Fatal("symlink active log was accepted")
	}
}

type shortThenFailFile struct {
	data       []byte
	offset     int64
	truncateAt int64
	writes     int
}

func (file *shortThenFailFile) Write(p []byte) (int, error) {
	file.writes++
	if file.writes == 1 {
		count := len(p) / 2
		file.data = append(file.data, p[:count]...)
		file.offset += int64(count)
		return count, nil
	}
	return 0, io.ErrUnexpectedEOF
}

func (file *shortThenFailFile) Close() error { return nil }

func (file *shortThenFailFile) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekStart {
		file.offset = offset
	}
	return file.offset, nil
}

func (file *shortThenFailFile) Truncate(size int64) error {
	file.truncateAt = size
	if int64(len(file.data)) > size {
		file.data = file.data[:size]
	}
	return nil
}

func TestWriteCompleteRollsBackPartialFailure(t *testing.T) {
	t.Parallel()

	file := &shortThenFailFile{data: []byte("old"), offset: 3, truncateAt: -1}
	if err := writeComplete(file, []byte("new-line"), 3); err == nil {
		t.Fatal("partial failure returned nil")
	}
	if file.truncateAt != 3 || string(file.data) != "old" {
		t.Fatalf("rollback = truncate %d data %q, want 3 and old", file.truncateAt, file.data)
	}
}

type closeTrackingFile struct {
	*os.File
	closed   *bool
	closeErr error
}

func (file *closeTrackingFile) Close() error {
	*file.closed = true
	underlyingErr := file.File.Close()
	return errors.Join(underlyingErr, file.closeErr)
}

func TestRotatingWriterClosesActiveFileBeforeRename(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "symphony.jsonl")
	closed := false
	renameSawClosed := false
	operations := systemRotatingFileOperations()
	operations.openFile = func(name string, flag int, mode fs.FileMode) (writableLogFile, error) {
		file, err := os.OpenFile(name, flag, mode)
		if err != nil {
			return nil, err
		}
		closed = false
		return &closeTrackingFile{File: file, closed: &closed}, nil
	}
	operations.rename = func(oldPath, newPath string) error {
		renameSawClosed = closed
		return os.Rename(oldPath, newPath)
	}
	writer, err := newRotatingWriterWithOperations(path, 1, 5, operations)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteLine([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteLine([]byte("b")); err != nil {
		t.Fatal(err)
	}
	if !renameSawClosed {
		t.Fatal("rename occurred before the active file was closed")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRotatingWriterReportsOpenRenameRemoveAndCloseFailures(t *testing.T) {
	t.Run("initial open", func(t *testing.T) {
		operations := systemRotatingFileOperations()
		operations.openFile = func(string, int, fs.FileMode) (writableLogFile, error) {
			return nil, errors.New("open failed")
		}
		if _, err := newRotatingWriterWithOperations(filepath.Join(t.TempDir(), "log"), 1, 5, operations); err == nil {
			t.Fatal("open failure was hidden")
		}
	})

	t.Run("reopen", func(t *testing.T) {
		writer := mustTestRotatingWriter(t)
		if err := writer.WriteLine([]byte("a")); err != nil {
			t.Fatal(err)
		}
		writer.ops.openFile = func(string, int, fs.FileMode) (writableLogFile, error) {
			return nil, errors.New("reopen failed")
		}
		if err := writer.WriteLine([]byte("b")); err == nil {
			t.Fatal("reopen failure was hidden")
		}
	})

	t.Run("rename", func(t *testing.T) {
		writer := mustTestRotatingWriter(t)
		if err := writer.WriteLine([]byte("a")); err != nil {
			t.Fatal(err)
		}
		writer.ops.rename = func(string, string) error { return errors.New("rename failed") }
		if err := writer.WriteLine([]byte("b")); err == nil {
			t.Fatal("rename failure was hidden")
		}
		_ = writer.Close()
	})

	t.Run("remove", func(t *testing.T) {
		writer := mustTestRotatingWriter(t)
		if err := writer.WriteLine([]byte("a")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(archivePath(writer.path, writer.archives), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		writer.ops.remove = func(string) error { return errors.New("remove failed") }
		if err := writer.WriteLine([]byte("b")); err == nil {
			t.Fatal("remove failure was hidden")
		}
		_ = writer.Close()
	})

	t.Run("close before rename", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "log")
		closed := false
		renameCalls := 0
		operations := systemRotatingFileOperations()
		operations.openFile = func(name string, flag int, mode fs.FileMode) (writableLogFile, error) {
			file, err := os.OpenFile(name, flag, mode)
			if err != nil {
				return nil, err
			}
			return &closeTrackingFile{File: file, closed: &closed, closeErr: errors.New("close failed")}, nil
		}
		operations.rename = func(string, string) error {
			renameCalls++
			return nil
		}
		writer, err := newRotatingWriterWithOperations(path, 1, 5, operations)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteLine([]byte("a")); err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteLine([]byte("b")); err == nil {
			t.Fatal("close failure was hidden")
		}
		if renameCalls != 0 {
			t.Fatalf("rename calls = %d, want 0 after close failure", renameCalls)
		}
	})
}

func mustTestRotatingWriter(t *testing.T) *rotatingWriter {
	t.Helper()
	writer, err := newRotatingWriter(filepath.Join(t.TempDir(), "log"), 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	return writer
}

func TestRotatingWriterSerializesConcurrentCompleteLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "symphony.jsonl")
	writer, err := newRotatingWriter(path, 1<<20, 5)
	if err != nil {
		t.Fatal(err)
	}
	const count = 100
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := writer.WriteLine([]byte("{\"valid\":true}\n")); err != nil {
				t.Errorf("WriteLine: %v", err)
			}
		}()
	}
	wait.Wait()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "{\"valid\":true}\n") != count {
		t.Fatalf("complete-line count = %d, want %d", strings.Count(string(data), "{\"valid\":true}\n"), count)
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", filepath.Base(path), data, want)
	}
}
