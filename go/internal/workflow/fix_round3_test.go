package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStructuredPatchDealiasesConsumersOfRemovedSequenceAnchors(t *testing.T) {
	source := []byte("---\ntracker:\n  required_labels:\n    - &first !label first # first note\n    - &second !label second # second note\nfuture_one: *second\nfuture_two:\n  nested: *second\n---\nPrompt\n")
	labels := []string{"first"}
	got, e := patchStructuredSource("WORKFLOW.md", source, &StructuredPatch{TrackerRequiredLabels: &labels})
	if e != nil {
		t.Fatal(e)
	}
	text := string(got)
	if strings.Contains(text, "*second") || !strings.Contains(text, "future_one: !label second") || !strings.Contains(text, "nested: !label second") {
		t.Fatal("removed anchor left dangling or changed consumers")
	}
	if _, e = Parse("WORKFLOW.md", got); e != nil {
		t.Fatal(e)
	}
	def, _ := Parse("WORKFLOW.md", got)
	if _, e = Resolve("WORKFLOW.md", def, os.LookupEnv); e != nil {
		t.Fatal(e)
	}
}

func TestWatcherCloseAbandonsBlockedProviderValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	os.WriteFile(path, []byte(workflowWithInterval(1000)), 0o600)
	entered, release, callbackDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	store, _ := NewStore(context.Background(), path, os.LookupEnv, func(c EffectiveConfig) []FieldError {
		if c.Polling.Interval.Milliseconds() == 2000 {
			defer close(callbackDone)
			close(entered)
			<-release
		}
		return nil
	})
	base, e := store.Load(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(path, []byte(workflowWithInterval(2000)), 0o600)
	<-entered
	closed := make(chan struct{})
	go func() { store.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(300 * time.Millisecond):
		close(release)
		<-closed
		t.Fatal("Close waited for blocked watcher validation")
	}
	close(release)
	select {
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("released callback worker did not exit")
	}
	current, _ := store.Current()
	if current.Digest != base.Digest {
		t.Fatal("late watcher validation changed closed store")
	}
	select {
	case _, ok := <-store.Changes():
		if ok {
			t.Fatal("published after close")
		}
	case <-time.After(time.Second):
		t.Fatal("changes not closed")
	}
}

func TestSaveAndLoadCannotInstallOrPublishAfterClose(t *testing.T) {
	for _, mode := range []string{"save", "load"} {
		t.Run(mode, func(t *testing.T) {
			store, path := newWorkflowStore(t, workflowWithInterval(1000))
			base, e := store.Load(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			store.beforeInstall = func() { close(entered); <-release }
			result := make(chan error, 1)
			panicResult := make(chan any, 1)
			go func() {
				defer func() { panicResult <- recover() }()
				if mode == "save" {
					_, e = store.Save(context.Background(), SaveCommand{BaseDigest: base.Digest, RawSource: []byte(workflowWithInterval(5000))})
				} else {
					os.WriteFile(path, []byte(workflowWithInterval(5000)), 0o600)
					_, e = store.Load(context.Background())
				}
				result <- e
			}()
			<-entered
			closed := make(chan struct{})
			go func() { store.Close(); close(closed) }()
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("Close blocked at final install fence")
			}
			close(release)
			err := <-result
			if recovered := <-panicResult; recovered != nil {
				t.Fatalf("publication panic: %v", recovered)
			}
			if !errors.Is(err, ErrStoreClosed) {
				t.Fatalf("got %v", err)
			}
			visible, _ := os.ReadFile(path)
			if !strings.Contains(string(visible), "interval_ms: 5000") {
				t.Fatal("visible file does not reflect completed write")
			}
			current, _ := store.Current()
			if current.Digest != base.Digest {
				t.Fatal("installed state after close")
			}
			if _, ok := <-store.Changes(); ok {
				t.Fatal("changes channel not closed")
			}
		})
	}
}
