package observability

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

const (
	defaultActiveLogSize = int64(10 << 20)
	defaultLogArchives   = 5
)

var errLogLineTooLarge = errors.New("log line exceeds active file size limit")

type lineSink interface {
	WriteLine([]byte) error
	Close() error
}

type writableLogFile interface {
	io.Writer
	io.Seeker
	Truncate(int64) error
	Close() error
	Stat() (fs.FileInfo, error)
}

type rotatingFileOperations struct {
	lstat    func(string) (fs.FileInfo, error)
	openFile func(string, int, fs.FileMode) (writableLogFile, error)
	rename   func(string, string) error
	remove   func(string) error
}

func systemRotatingFileOperations() rotatingFileOperations {
	return rotatingFileOperations{
		lstat: os.Lstat,
		openFile: func(name string, flag int, permission fs.FileMode) (writableLogFile, error) {
			return os.OpenFile(name, flag, permission)
		},
		rename: os.Rename,
		remove: os.Remove,
	}
}

type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxSize  int64
	archives int
	ops      rotatingFileOperations
	file     writableLogFile
	size     int64
	closed   bool
	closeErr error
}

func newRotatingWriter(path string, maxSize int64, archives int) (*rotatingWriter, error) {
	return newRotatingWriterWithOperations(path, maxSize, archives, systemRotatingFileOperations())
}

func newRotatingWriterWithOperations(path string, maxSize int64, archives int, operations rotatingFileOperations) (*rotatingWriter, error) {
	if path == "" || maxSize <= 0 || archives < 0 {
		return nil, errors.New("invalid rotating writer configuration")
	}
	if operations.lstat == nil || operations.openFile == nil || operations.rename == nil || operations.remove == nil {
		return nil, errors.New("incomplete rotating file operations")
	}
	writer := &rotatingWriter{
		path:     path,
		maxSize:  maxSize,
		archives: archives,
		ops:      operations,
	}
	if err := writer.openActive(); err != nil {
		return nil, err
	}
	return writer, nil
}

func (w *rotatingWriter) WriteLine(line []byte) error {
	if w == nil {
		return errors.New("nil rotating writer")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.file == nil {
		return errors.New("rotating writer is closed")
	}
	if uint64(len(line)) > uint64(w.maxSize) {
		return errLogLineTooLarge
	}
	if int64(len(line)) > w.maxSize-w.size {
		if err := w.rotate(); err != nil {
			return err
		}
	}
	originalOffset := w.size
	if err := writeComplete(w.file, line, originalOffset); err != nil {
		return err
	}
	w.size += int64(len(line))
	return nil
}

func (w *rotatingWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return w.closeErr
	}
	w.closed = true
	if w.file != nil {
		w.closeErr = w.file.Close()
		w.file = nil
	}
	return w.closeErr
}

func (w *rotatingWriter) openActive() error {
	info, err := w.ops.lstat(w.path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("active log is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect active log: %w", err)
	}

	file, err := w.ops.openFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open active log: %w", err)
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("inspect opened active log: %w", err)
	}
	if !openedInfo.Mode().IsRegular() {
		_ = file.Close()
		return errors.New("opened active log is not a regular file")
	}
	if openedInfo.Size() > w.maxSize {
		_ = file.Close()
		return errLogLineTooLarge
	}
	w.file = file
	w.size = openedInfo.Size()
	return nil
}

func (w *rotatingWriter) rotate() error {
	if w.file == nil {
		return errors.New("active log is unavailable")
	}
	if err := w.file.Close(); err != nil {
		w.file = nil
		return fmt.Errorf("close active log before rotation: %w", err)
	}
	w.file = nil

	rotationErr := w.rotateClosedFiles()
	reopenErr := w.openActive()
	if rotationErr != nil && reopenErr != nil {
		return errors.Join(rotationErr, reopenErr)
	}
	if rotationErr != nil {
		return rotationErr
	}
	if reopenErr != nil {
		return reopenErr
	}
	return nil
}

func (w *rotatingWriter) rotateClosedFiles() error {
	if w.archives == 0 {
		if err := w.removeSafe(w.path); err != nil {
			return fmt.Errorf("remove active log: %w", err)
		}
		return nil
	}

	oldest := archivePath(w.path, w.archives)
	if err := w.removeSafe(oldest); err != nil {
		return fmt.Errorf("remove oldest archive: %w", err)
	}
	for index := w.archives - 1; index >= 1; index-- {
		source := archivePath(w.path, index)
		destination := archivePath(w.path, index+1)
		if err := w.renameIfPresent(source, destination); err != nil {
			return fmt.Errorf("rotate archive %d: %w", index, err)
		}
	}
	if err := w.renameIfPresent(w.path, archivePath(w.path, 1)); err != nil {
		return fmt.Errorf("archive active log: %w", err)
	}
	return nil
}

func (w *rotatingWriter) renameIfPresent(source, destination string) error {
	present, err := w.regularOrMissing(source)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if err := w.removeSafe(destination); err != nil {
		return err
	}
	if err := w.ops.rename(source, destination); err != nil {
		return err
	}
	return nil
}

func (w *rotatingWriter) removeSafe(path string) error {
	present, err := w.regularOrMissing(path)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if err := w.ops.remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (w *rotatingWriter) regularOrMissing(path string) (bool, error) {
	info, err := w.ops.lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, errors.New("log rotation path is not a regular file")
	}
	return true, nil
}

func archivePath(path string, archive int) string {
	return fmt.Sprintf("%s.%d", path, archive)
}

type rollbackWriter interface {
	io.Writer
	io.Seeker
	Truncate(int64) error
}

func writeComplete(file rollbackWriter, data []byte, originalOffset int64) error {
	written := 0
	for written < len(data) {
		count, err := file.Write(data[written:])
		if count < 0 || count > len(data)-written {
			err = errors.New("invalid write count")
			count = 0
		}
		written += count
		if err != nil || count == 0 {
			if err == nil {
				err = io.ErrShortWrite
			}
			rollbackErr := rollbackPartialWrite(file, originalOffset)
			if rollbackErr != nil {
				return errors.Join(err, rollbackErr)
			}
			return err
		}
	}
	return nil
}

func rollbackPartialWrite(file rollbackWriter, originalOffset int64) error {
	truncateErr := file.Truncate(originalOffset)
	_, seekErr := file.Seek(originalOffset, io.SeekStart)
	return errors.Join(truncateErr, seekErr)
}

func ensureSecureDirectory(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("log directory is not a regular directory")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err = os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("created log directory is not a regular directory")
	}
	return nil
}
