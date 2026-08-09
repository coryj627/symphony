package workflow

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrDurabilityUncertain means the replacement completed but the operating
// system could not confirm the parent-directory metadata was durable. The
// complete new file is visible and must not be rolled back.
var ErrDurabilityUncertain = errors.New("workflow_durability_uncertain")

type atomicFile interface {
	io.Writer
	Sync() error
	Chmod(os.FileMode) error
	Close() error
	Name() string
}

type atomicOperations struct {
	createTemp func(directory, pattern string) (atomicFile, error)
	replace    func(temporary, destination string) error
	syncDir    func(directory string) error
	remove     func(path string) error
	stat       func(path string) (os.FileInfo, error)
	readFile   func(path string) ([]byte, error)
}

func defaultAtomicOperations() atomicOperations {
	return atomicOperations{
		createTemp: func(directory, pattern string) (atomicFile, error) {
			return os.CreateTemp(directory, pattern)
		},
		replace:  replaceFile,
		syncDir:  syncParentDirectory,
		remove:   os.Remove,
		stat:     os.Stat,
		readFile: os.ReadFile,
	}
}

func atomicReplace(destination string, source []byte, operations atomicOperations) error {
	return atomicReplaceConditional(destination, source, nil, operations)
}

func atomicReplaceChecked(destination string, source []byte, expectedDigest string, operations atomicOperations) error {
	return atomicReplaceConditional(destination, source, &expectedDigest, operations)
}

func atomicReplaceConditional(destination string, source []byte, expectedDigest *string, operations atomicOperations) (resultErr error) {
	directory := filepath.Dir(destination)
	permissions := os.FileMode(0o600)
	if info, err := operations.stat(destination); err == nil {
		permissions = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect workflow destination: %w", err)
	}

	name := "." + filepath.Base(destination) + ".*.tmp"
	temporary, err := operations.createTemp(directory, name)
	if err != nil {
		return fmt.Errorf("create workflow temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	replaced := false
	closed := false
	defer func() {
		if !closed {
			resultErr = errors.Join(resultErr, temporary.Close())
		}
		if !replaced {
			if removeErr := operations.remove(temporaryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, removeErr)
			}
		}
	}()

	// CreateTemp starts at 0600. Applying the destination's existing bits here
	// preserves them exactly and never makes the destination more permissive
	// than it was before this transaction.
	if err := temporary.Chmod(permissions); err != nil {
		return fmt.Errorf("set workflow temporary permissions: %w", err)
	}
	remaining := source
	for len(remaining) > 0 {
		written, writeErr := temporary.Write(remaining)
		if writeErr != nil {
			return fmt.Errorf("write workflow temporary file: %w", writeErr)
		}
		if written <= 0 || written > len(remaining) {
			return fmt.Errorf("write workflow temporary file: %w", io.ErrShortWrite)
		}
		remaining = remaining[written:]
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync workflow temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close workflow temporary file: %w", err)
	}
	closed = true
	if expectedDigest != nil {
		current, readErr := operations.readFile(destination)
		currentDigest := ""
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return fmt.Errorf("recheck workflow destination: %w", readErr)
		}
		if readErr == nil {
			currentDigest = digestSource(current)
		}
		if currentDigest != *expectedDigest {
			return ErrSaveConflict
		}
	}
	if err := operations.replace(temporaryPath, destination); err != nil {
		return fmt.Errorf("replace workflow destination: %w", err)
	}
	replaced = true
	if err := operations.syncDir(directory); err != nil {
		return fmt.Errorf("%w: %w", ErrDurabilityUncertain, err)
	}
	return nil
}
