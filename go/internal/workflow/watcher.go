package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

const workflowWatchDebounce = 100 * time.Millisecond

func (store *FileStore) watchLoop(ctx context.Context) {
	defer store.watchWG.Done()
	var timer *time.Timer
	var timerChannel <-chan time.Time
	stopTimer := func() {
		if timer == nil || !timer.Stop() {
			return
		}
	}
	defer stopTimer()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-store.watcher.Events:
			if !ok {
				return
			}
			if filepath.Clean(event.Name) != store.path || event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			if timer == nil {
				timer = time.NewTimer(workflowWatchDebounce)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(workflowWatchDebounce)
			}
			timerChannel = timer.C
		case <-timerChannel:
			timerChannel = nil
			store.reloadFromWatcher(ctx)
		case _, ok := <-store.watcher.Errors:
			if !ok {
				return
			}
			store.publish(Change{Validation: ValidationResult{Valid: false, FieldErrors: []FieldError{}, GlobalErrors: []SafeError{{Code: "workflow_watch_error", Message: "The workflow watcher encountered an error."}}}})
		}
	}
}

func (store *FileStore) reloadFromWatcher(ctx context.Context) {
	if err := store.pathMu.acquire(ctx, store.stopping); err != nil {
		return
	}
	defer store.pathMu.release()
	for attempts := 0; attempts < 8; attempts++ {
		source, err := os.ReadFile(store.path)
		if err != nil {
			validation := safeValidation(err)
			if errors.Is(err, os.ErrNotExist) {
				validation = safeValidation(ErrMissingWorkflow)
			}
			store.publish(Change{Validation: validation})
			return
		}
		digest := digestSource(source)
		type validationOutcome struct {
			snapshot   Snapshot
			validation ValidationResult
			err        error
		}
		result := make(chan validationOutcome, 1)
		go func() {
			snapshot, validation, candidateErr := store.snapshotFromSource(source)
			result <- validationOutcome{snapshot, validation, candidateErr}
		}()
		var snapshot Snapshot
		var validation ValidationResult
		var candidateErr error
		select {
		case <-ctx.Done():
			return
		case <-store.stopping:
			return
		case outcome := <-result:
			snapshot, validation, candidateErr = outcome.snapshot, outcome.validation, outcome.err
		}
		latest, err := os.ReadFile(store.path)
		if err != nil || digestSource(latest) != digest {
			continue
		}
		if candidateErr != nil || !validation.Valid {
			if ctx.Err() != nil {
				return
			}
			store.publish(Change{Digest: digest, Validation: validation})
			return
		}
		if !store.installSnapshotIfOpen(snapshot, true) {
			return
		}
		return
	}
}
