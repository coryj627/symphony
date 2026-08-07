package instance

import (
	"encoding/json"
	"errors"
	"fmt"
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

func Acquire(info Info) (*Lock, error) {
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

	metadata, err := writeOwnerMetadata(info)
	if err != nil {
		unlockErr := workflowLock.Unlock()
		return nil, errors.Join(err, unlockErr)
	}
	return &Lock{fileLock: workflowLock, metadata: metadata}, nil
}

func writeOwnerMetadata(info Info) (*os.File, error) {
	if err := os.MkdirAll(info.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create instance data directory: %w", err)
	}
	metadataPath := filepath.Join(info.DataDir, "instance.json")
	metadata, err := os.OpenFile(metadataPath, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open instance metadata: %w", err)
	}
	openedInfo, err := metadata.Stat()
	if err != nil {
		_ = metadata.Close()
		return nil, fmt.Errorf("inspect opened instance metadata: %w", err)
	}
	pathInfo, err := os.Lstat(metadataPath)
	if err != nil {
		_ = metadata.Close()
		return nil, fmt.Errorf("inspect instance metadata path: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !pathInfo.Mode().IsRegular() || !os.SameFile(openedInfo, pathInfo) {
		_ = metadata.Close()
		return nil, fmt.Errorf("instance metadata path %q is not a regular file", metadataPath)
	}
	if err := metadata.Chmod(0o600); err != nil {
		_ = metadata.Close()
		return nil, fmt.Errorf("secure instance metadata: %w", err)
	}
	if err := metadata.Truncate(0); err != nil {
		_ = metadata.Close()
		return nil, fmt.Errorf("truncate instance metadata: %w", err)
	}
	owner := ownerMetadata{
		PID:          os.Getpid(),
		StartedAt:    time.Now().UTC(),
		WorkflowID:   info.WorkflowID,
		WorkflowPath: info.WorkflowPath,
	}
	if err := json.NewEncoder(metadata).Encode(owner); err != nil {
		_ = metadata.Close()
		return nil, fmt.Errorf("write instance metadata: %w", err)
	}
	if err := metadata.Sync(); err != nil {
		_ = metadata.Close()
		return nil, fmt.Errorf("sync instance metadata: %w", err)
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
