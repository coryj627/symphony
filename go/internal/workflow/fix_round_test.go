package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSaveConflictWhenFileChangesDuringValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	os.WriteFile(path, []byte(validWorkflowSource), 0o600)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	store, _ := NewStore(context.Background(), path, os.LookupEnv, func(EffectiveConfig) []FieldError {
		if calls.Add(1) == 2 {
			close(entered)
			<-release
		}
		return nil
	})
	defer store.Close()
	base, _ := store.Load(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := store.Save(context.Background(), SaveCommand{BaseDigest: base.Digest, RawSource: []byte(base.Source)})
		done <- err
	}()
	<-entered
	external := []byte(workflowWithInterval(9000))
	os.WriteFile(path, external, 0o600)
	close(release)
	if err := <-done; !errors.Is(err, ErrSaveConflict) {
		t.Fatalf("got %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(external) {
		t.Fatal("overwrote external edit")
	}
}

func TestPathTransactionSerializesStoresAndLoadBeforeSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	os.WriteFile(path, []byte(workflowWithInterval(1000)), 0o600)
	loadEntered, release, saveEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var block atomic.Bool
	validator := func(c EffectiveConfig) []FieldError {
		if block.Load() && c.Polling.Interval.Milliseconds() == 1000 {
			if block.CompareAndSwap(true, false) {
				close(loadEntered)
				<-release
			}
		}
		if c.Polling.Interval.Milliseconds() == 5000 {
			select {
			case <-saveEntered:
			default:
				close(saveEntered)
			}
		}
		return nil
	}
	a, _ := NewStore(context.Background(), path, os.LookupEnv, validator)
	b, _ := NewStore(context.Background(), path, os.LookupEnv, validator)
	defer a.Close()
	defer b.Close()
	base, _ := a.Load(context.Background())
	b.Load(context.Background())
	block.Store(true)
	loadDone := make(chan struct{}, 1)
	go func() { a.Load(context.Background()); loadDone <- struct{}{} }()
	<-loadEntered
	saveDone := make(chan error, 1)
	go func() {
		_, e := b.Save(context.Background(), SaveCommand{BaseDigest: base.Digest, RawSource: []byte(workflowWithInterval(5000))})
		saveDone <- e
	}()
	select {
	case <-saveEntered:
		t.Fatal("same-path save entered validation before load completed")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-loadDone
	if e := <-saveDone; e != nil {
		t.Fatal(e)
	}
	current, _ := b.Current()
	if current.Config.Polling.Interval.Milliseconds() != 5000 {
		t.Fatal("older load regressed save")
	}
	if _, err := a.Save(context.Background(), SaveCommand{BaseDigest: base.Digest, RawSource: []byte(workflowWithInterval(6000))}); !errors.Is(err, ErrSaveConflict) {
		t.Fatalf("second store with shared base did not conflict: %v", err)
	}
}

func TestWatcherRetriesCandidateChangedDuringValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	os.WriteFile(path, []byte(workflowWithInterval(1000)), 0o600)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	store, _ := NewStore(context.Background(), path, os.LookupEnv, func(c EffectiveConfig) []FieldError {
		if c.Polling.Interval.Milliseconds() == 2000 {
			once.Do(func() { close(entered); <-release })
		}
		return nil
	})
	defer store.Close()
	store.Load(context.Background())
	os.WriteFile(path, []byte(workflowWithInterval(2000)), 0o600)
	<-entered
	os.WriteFile(path, []byte(workflowWithInterval(3000)), 0o600)
	close(release)
	change := awaitChange(t, store.Changes())
	if change.Snapshot.Config.Polling.Interval.Milliseconds() != 3000 {
		t.Fatal("watcher published stale candidate")
	}
}

func TestLoadRetriesCandidateChangedDuringValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	os.WriteFile(path, []byte(workflowWithInterval(1000)), 0o600)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	store, _ := NewStore(context.Background(), path, os.LookupEnv, func(c EffectiveConfig) []FieldError {
		if c.Polling.Interval.Milliseconds() == 1000 {
			once.Do(func() { close(entered); <-release })
		}
		return nil
	})
	defer store.Close()
	done := make(chan Snapshot, 1)
	go func() { s, _ := store.Load(context.Background()); done <- s }()
	<-entered
	os.WriteFile(path, []byte(workflowWithInterval(4000)), 0o600)
	close(release)
	loaded := <-done
	current, _ := store.Current()
	if loaded.Config.Polling.Interval.Milliseconds() != 4000 || current.Digest != loaded.Digest {
		t.Fatal("installed stale candidate")
	}
}

func TestStructuredPatchPreservesMetadataAliasesAndDelimiterEnding(t *testing.T) {
	source := []byte("---\r\ndefaults: &interval 30000\r\nfuture: *interval\r\ntracker:\r\n  required_labels:\r\n    - &first !label \"alpha\" # keep alpha\r\n    - &second 'beta' # keep beta\r\nfuture_label: *second\r\npolling:\r\n  interval_ms: *interval\r\nworkspace:\r\n  root: !path \"./old\"\r\n---\r\nPrompt\r\n")
	labels := []string{"alpha", "changed"}
	got, err := patchStructuredSource("WORKFLOW.md", source, &StructuredPatch{PollingIntervalMS: intPointer(45000), WorkspaceRoot: stringPointer("./new"), TrackerRequiredLabels: &labels})
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{"---\r\n", "future: *interval", "future_label: 'beta'", "interval_ms: 45000", "root: !path \"./new\"", "&first !label \"alpha\" # keep alpha", "'changed' # keep beta"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if !strings.HasSuffix(text, "Prompt\r\n") {
		t.Fatal("prompt changed")
	}
}

type retryCloseFile struct {
	atomicFile
	closes int
	open   bool
}

func (f *retryCloseFile) Close() error {
	f.closes++
	if f.closes == 1 {
		return injectedAtomicFailure
	}
	f.open = false
	return f.atomicFile.Close()
}

func TestAtomicCloseFailureRetriesCleanup(t *testing.T) {
	path, original, replacement := atomicFixture(t)
	ops := defaultAtomicOperations()
	create, remove := ops.createTemp, ops.remove
	var wrapped *retryCloseFile
	ops.createTemp = func(d, p string) (atomicFile, error) {
		f, e := create(d, p)
		if e != nil {
			return nil, e
		}
		wrapped = &retryCloseFile{atomicFile: f, open: true}
		return wrapped, nil
	}
	ops.remove = func(name string) error {
		if wrapped.open {
			return errors.New("open handle")
		}
		return remove(name)
	}
	err := atomicReplace(path, replacement, ops)
	if !errors.Is(err, injectedAtomicFailure) {
		t.Fatalf("got %v", err)
	}
	if wrapped.closes != 2 || wrapped.open {
		t.Fatal("close not retried")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(original) {
		t.Fatal("destination changed")
	}
	if _, e := os.Stat(wrapped.Name()); !os.IsNotExist(e) {
		t.Fatal("temp leaked")
	}
}
