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

func TestSaveConflictWhenWorkflowDeletedDuringValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	os.WriteFile(path, []byte(validWorkflowSource), 0o600)
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	store, _ := NewStore(context.Background(), path, os.LookupEnv, func(EffectiveConfig) []FieldError {
		calls++
		if calls == 2 {
			close(entered)
			<-release
			return []FieldError{{Field: "tracker.kind", Code: "invalid", Message: "Invalid tracker."}}
		}
		return nil
	})
	defer store.Close()
	base, _ := store.Load(context.Background())
	done := make(chan error, 1)
	go func() {
		_, e := store.Save(context.Background(), SaveCommand{BaseDigest: base.Digest, RawSource: []byte(base.Source)})
		done <- e
	}()
	<-entered
	os.Remove(path)
	close(release)
	if e := <-done; !errors.Is(e, ErrSaveConflict) {
		t.Fatalf("got %v", e)
	}
}

func TestSavePropagatesNonMissingFinalReadFailure(t *testing.T) {
	store, _ := newWorkflowStore(t, validWorkflowSource)
	base, _ := store.Load(context.Background())
	store.atomic.readFile = func(string) ([]byte, error) { return nil, os.ErrPermission }
	_, err := store.Save(context.Background(), SaveCommand{BaseDigest: base.Digest, RawSource: []byte(base.Source)})
	if err == nil || errors.Is(err, ErrInvalidWorkflow) || errors.Is(err, ErrSaveConflict) {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestMissingWorkflowCanonicalizesSymlinkedParent(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	realPath := filepath.Join(real, "WORKFLOW.md")
	linkedPath := filepath.Join(link, "WORKFLOW.md")
	a, _ := NewStore(context.Background(), realPath, os.LookupEnv, nil)
	b, _ := NewStore(context.Background(), linkedPath, os.LookupEnv, nil)
	defer a.Close()
	defer b.Close()
	if a.path != b.path || a.pathMu != b.pathMu {
		t.Fatal("missing symlink-parent paths use different transaction gates")
	}
	first, e := a.Save(context.Background(), SaveCommand{BaseDigest: "", RawSource: []byte(validWorkflowSource)})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = b.Save(context.Background(), SaveCommand{BaseDigest: "", RawSource: []byte(first.Source)}); !errors.Is(e, ErrSaveConflict) {
		t.Fatalf("stale empty base got %v", e)
	}
}

func TestStructuredPatchMaterializesAliasMergeAndMatchesSequenceMetadataByValue(t *testing.T) {
	source := []byte("---\nprovider_defaults: &provider\n  owner: openai\n  repository: symphony\n  unknown: keep\ntracker_defaults: &tracker\n  kind: github\n  provider: *provider\ntracker:\n  <<: *tracker\n  required_labels: !labels\n    - !label first # first note\n    - !label second # second note\nconsumer: *provider\n---\nPrompt\n")
	labels := []string{"second", "first"}
	got, e := patchStructuredSource("WORKFLOW.md", source, &StructuredPatch{ProviderRepository: stringPointer("changed"), TrackerRequiredLabels: &labels})
	if e != nil {
		t.Fatal(e)
	}
	text := string(got)
	for _, want := range []string{"repository: changed", "unknown: keep", "consumer: *provider", "required_labels: !labels", "second # second note", "first # first note"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q", want)
		}
	}
	def, e := Parse("WORKFLOW.md", got)
	if e != nil {
		t.Fatal(e)
	}
	cfg, e := Resolve("WORKFLOW.md", def, os.LookupEnv)
	if e != nil {
		t.Fatal(e)
	}
	if cfg.Tracker.Provider["repository"] != "changed" || cfg.Tracker.Provider["unknown"] != "keep" {
		t.Fatal("effective merged provider lost data")
	}
}

func TestCanceledGateWaiterReturnsAndCloseDoesNotWaitForOtherStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	os.WriteFile(path, []byte(validWorkflowSource), 0o600)
	entered, release := make(chan struct{}), make(chan struct{})
	block := true
	a, _ := NewStore(context.Background(), path, os.LookupEnv, func(EffectiveConfig) []FieldError {
		if block {
			block = false
			close(entered)
			<-release
		}
		return nil
	})
	b, _ := NewStore(context.Background(), path, os.LookupEnv, nil)
	defer a.Close()
	go a.Load(context.Background())
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, e := b.Load(ctx); done <- e }()
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("got %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled gate waiter blocked")
	}
	saveCtx, saveCancel := context.WithCancel(context.Background())
	saveDone := make(chan error, 1)
	go func() {
		_, e := b.Save(saveCtx, SaveCommand{BaseDigest: "", RawSource: []byte(validWorkflowSource)})
		saveDone <- e
	}()
	saveCancel()
	select {
	case e := <-saveDone:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("save got %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled Save gate waiter blocked")
	}
	os.WriteFile(path, []byte(validWorkflowSource+"\n"), 0o600)
	time.Sleep(150 * time.Millisecond)
	closed := make(chan struct{})
	go func() { b.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close waited behind another store gate")
	}
	close(release)
}

func TestPathGateRegistryReclaimsClosedStores(t *testing.T) {
	start := pathTransactionRegistrySize()
	for i := 0; i < 20; i++ {
		path := filepath.Join(t.TempDir(), "WORKFLOW.md")
		store, e := NewStore(context.Background(), path, os.LookupEnv, nil)
		if e != nil {
			t.Fatal(e)
		}
		store.Close()
	}
	if got := pathTransactionRegistrySize(); got > start {
		t.Fatalf("registry grew from %d to %d", start, got)
	}
}

func TestCloseReturnsWhileOwnValidationIsBlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	os.WriteFile(path, []byte(validWorkflowSource), 0o600)
	entered, release := make(chan struct{}), make(chan struct{})
	store, _ := NewStore(context.Background(), path, os.LookupEnv, func(EffectiveConfig) []FieldError { close(entered); <-release; return nil })
	done := make(chan error, 1)
	go func() { _, e := store.Load(context.Background()); done <- e }()
	<-entered
	closed := make(chan struct{})
	go func() { store.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close waited for blocked validation")
	}
	close(release)
	if e := <-done; !errors.Is(e, ErrStoreClosed) {
		t.Fatalf("blocked load got %v", e)
	}
}
