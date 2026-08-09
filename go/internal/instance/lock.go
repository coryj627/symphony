package instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	filelock "github.com/gofrs/flock"
)

var ErrAlreadyRunning = errors.New("workflow is already running")

type Lock struct {
	mu       sync.Mutex
	fileLock *filelock.Flock
	metadata *os.File
	released bool
}

type ownerMetadata struct {
	PID          int       `json:"pid"`
	StartedAt    time.Time `json:"started_at"`
	WorkflowID   string    `json:"workflow_id"`
	WorkflowPath string    `json:"workflow_path"`
}

type metadataOperations struct {
	encode  func(ownerMetadata) ([]byte, error)
	create  func(string) (*os.File, string, error)
	write   func(*os.File, []byte) (int, error)
	sync    func(*os.File) error
	close   func(*os.File) error
	replace func(string, string) error
}

func defaultMetadataOperations() metadataOperations {
	return metadataOperations{
		encode: func(metadata ownerMetadata) ([]byte, error) {
			contents, err := json.Marshal(metadata)
			if err != nil {
				return nil, err
			}
			return append(contents, '\n'), nil
		},
		create:  createMetadataTemp,
		write:   (*os.File).Write,
		sync:    (*os.File).Sync,
		close:   (*os.File).Close,
		replace: replaceMetadata,
	}
}

func Acquire(info Info) (*Lock, error) {
	return acquire(info, defaultMetadataOperations())
}

func acquire(info Info, operations metadataOperations) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(info.LockPath), 0o700); err != nil {
		return nil, fmt.Errorf("create workflow lock directory: %w", err)
	}

	workflowLock := filelock.New(info.LockPath, filelock.SetPermissions(0o600))
	acquired, err := workflowLock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire workflow lock: %w", err)
	}
	if !acquired {
		return nil, fmt.Errorf("%w: %s", ErrAlreadyRunning, info.WorkflowPath)
	}

	metadata, err := publishOwnerMetadata(info, operations)
	if err != nil {
		unlockErr := workflowLock.Unlock()
		return nil, errors.Join(err, unlockErr)
	}
	return &Lock{fileLock: workflowLock, metadata: metadata}, nil
}

func publishOwnerMetadata(info Info, operations metadataOperations) (*os.File, error) {
	if err := os.MkdirAll(info.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create instance data directory: %w", err)
	}
	owner := ownerMetadata{
		PID:          os.Getpid(),
		StartedAt:    time.Now().UTC(),
		WorkflowID:   info.WorkflowID,
		WorkflowPath: info.WorkflowPath,
	}
	contents, err := operations.encode(owner)
	if err != nil {
		return nil, fmt.Errorf("encode instance metadata: %w", err)
	}

	metadata, temporaryPath, err := operations.create(info.DataDir)
	if err != nil {
		return nil, fmt.Errorf("create temporary instance metadata: %w", err)
	}
	cleanup := func(cause error) error {
		closeErr := operations.close(metadata)
		removeErr := os.Remove(temporaryPath)
		return errors.Join(cause, closeErr, removeErr)
	}

	written, err := operations.write(metadata, contents)
	if err != nil {
		return nil, cleanup(fmt.Errorf("write instance metadata: %w", err))
	}
	if written != len(contents) {
		return nil, cleanup(fmt.Errorf("write instance metadata: %w", io.ErrShortWrite))
	}
	if err := operations.sync(metadata); err != nil {
		return nil, cleanup(fmt.Errorf("sync instance metadata: %w", err))
	}
	metadataPath := filepath.Join(info.DataDir, "instance.json")
	if err := operations.replace(temporaryPath, metadataPath); err != nil {
		return nil, cleanup(fmt.Errorf("publish instance metadata: %w", err))
	}
	return metadata, nil
}

func (lock *Lock) Release() error {
	if lock == nil {
		return nil
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.released {
		return nil
	}

	var metadataErr error
	if lock.metadata != nil {
		truncateErr := lock.metadata.Truncate(0)
		closeErr := lock.metadata.Close()
		metadataErr = errors.Join(truncateErr, closeErr)
		lock.metadata = nil
	}
	unlockErr := lock.fileLock.Unlock()
	if unlockErr == nil {
		lock.released = true
	}
	return errors.Join(metadataErr, unlockErr)
}
