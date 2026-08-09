package workflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func workflowWithInterval(interval int) string {
	return fmt.Sprintf("---\ntracker:\n  kind: github\n  provider:\n    owner: openai\n    repository: symphony\npolling:\n  interval_ms: %d\n---\nPrompt\n", interval)
}

func TestWatcherDetectsSameInodeWriteAndAtomicReplacement(t *testing.T) {
	for _, test := range []struct {
		name  string
		write func(*testing.T, string, []byte)
	}{
		{"same inode write", func(t *testing.T, path string, source []byte) {
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(source); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{"atomic replacement", func(t *testing.T, path string, source []byte) {
			temporary := filepath.Join(filepath.Dir(path), "replacement.tmp")
			if err := os.WriteFile(temporary, source, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(temporary, path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, path := newWorkflowStore(t, workflowWithInterval(1000))
			if _, err := store.Load(context.Background()); err != nil {
				t.Fatal(err)
			}
			candidate := []byte(workflowWithInterval(2500))
			test.write(t, path, candidate)
			change := awaitChange(t, store.Changes())
			if !change.Validation.Valid || change.Snapshot.Source != string(candidate) || change.Snapshot.Config.Polling.Interval.Milliseconds() != 2500 {
				t.Fatalf("watcher did not publish exact valid replacement: valid=%v interval=%d digest=%q", change.Validation.Valid, change.Snapshot.Config.Polling.Interval.Milliseconds(), change.Digest)
			}
		})
	}
}

func TestWatcherCoalescesBursts(t *testing.T) {
	store, path := newWorkflowStore(t, workflowWithInterval(1000))
	if _, err := store.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	for interval := 2000; interval <= 4000; interval += 1000 {
		if err := os.WriteFile(path, []byte(workflowWithInterval(interval)), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	change := awaitChange(t, store.Changes())
	if !change.Validation.Valid || change.Snapshot.Config.Polling.Interval.Milliseconds() != 4000 {
		t.Fatalf("burst did not coalesce to final workflow: valid=%v interval=%d", change.Validation.Valid, change.Snapshot.Config.Polling.Interval.Milliseconds())
	}
	select {
	case extra := <-store.Changes():
		t.Fatalf("burst emitted extra change: valid=%v digest=%q", extra.Validation.Valid, extra.Digest)
	case <-time.After(250 * time.Millisecond):
	}
}

func TestInvalidReloadRetainsCurrentAndRecoversOnLaterValidReload(t *testing.T) {
	store, path := newWorkflowStore(t, workflowWithInterval(1000))
	initial, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	canary := "INVALID-PROMPT-CANARY-MUST-NOT-LEAK"
	if err := os.WriteFile(path, []byte("---\npolling:\n  interval_ms: -1\n---\n"+canary), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := awaitChange(t, store.Changes())
	if invalid.Validation.Valid || len(invalid.Validation.GlobalErrors) == 0 {
		t.Fatalf("invalid reload not reported safely: valid=%v global-errors=%d", invalid.Validation.Valid, len(invalid.Validation.GlobalErrors))
	}
	if strings.Contains(fmt.Sprintf("%+v", invalid.Validation), canary) {
		t.Fatal("invalid reload exposed workflow contents")
	}
	current, ok := store.Current()
	if !ok || current.Digest != initial.Digest {
		t.Fatalf("invalid reload replaced last known good: present=%v digest=%q", ok, current.Digest)
	}
	valid := []byte(workflowWithInterval(7000))
	if err := os.WriteFile(path, valid, 0o600); err != nil {
		t.Fatal(err)
	}
	recovered := awaitChange(t, store.Changes())
	if !recovered.Validation.Valid || recovered.Snapshot.Config.Polling.Interval.Milliseconds() != 7000 {
		t.Fatalf("watcher did not recover: valid=%v interval=%d", recovered.Validation.Valid, recovered.Snapshot.Config.Polling.Interval.Milliseconds())
	}
}

func TestWatcherShutdownIsIdempotentAndClosesChanges(t *testing.T) {
	store, _ := newWorkflowStore(t, validWorkflowSource)
	if _, err := store.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := 0; index < 10; index++ {
			_ = store.Close()
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("repeated close deadlocked")
	}
	select {
	case _, ok := <-store.Changes():
		if ok {
			t.Fatal("changes channel remained open after close")
		}
	case <-time.After(time.Second):
		t.Fatal("changes channel was not closed")
	}
}

func TestWatcherContextCancellationShutsDownCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(validWorkflowSource), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(ctx, path, os.LookupEnv, func(EffectiveConfig) []FieldError { return nil })
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case _, ok := <-store.Changes():
		if ok {
			t.Fatal("changes channel remained open after cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("context cancellation did not stop watcher")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}
